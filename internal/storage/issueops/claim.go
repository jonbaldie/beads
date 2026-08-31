package issueops

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	publicops "github.com/jonbaldie/beads/issueops"
)

// ClaimResult holds the result of a ClaimIssueInTx call.
type ClaimResult struct {
	OldIssue *types.Issue
	IsWisp   bool
}

// ClaimIssueInTx atomically claims an issue using compare-and-swap semantics.
// It sets the assignee to actor and status to "in_progress" only if the issue
// is currently open and unassigned, already assigned to the same actor, or
// assigned to a pool alias listed in the claim.pools config (see
// ClaimPoolAliasesInTx).
// Returns storage.ErrAlreadyClaimed if already claimed by a different user.
// Idempotent: re-claiming an in_progress issue by the same actor is a no-op
// success (supports agent retry workflows).
// Routes to the correct table (issues/wisps) automatically.
// The caller is responsible for Dolt versioning (DOLT_ADD/COMMIT) if needed.
//
//nolint:gosec // G201: table names come from WispTableRouting (hardcoded constants)
func ClaimIssueInTx(ctx context.Context, tx DBTX, id string, actor string) (*ClaimResult, error) {
	if err := types.CheckFieldLen("actor", actor); err != nil {
		return nil, err
	}
	claim, err := prepareClaimInTx(ctx, tx, id, actor)
	if err != nil {
		return nil, err
	}
	rowsAffected, err := executeClaimUpdate(ctx, tx, claim)
	if err != nil {
		return nil, err
	}
	if rowsAffected == 0 {
		return resolveClaimConflict(ctx, tx, claim)
	}
	if err := grantClaimLease(ctx, tx, claim); err != nil {
		return nil, err
	}
	if err := recordClaimInTx(ctx, tx, claim); err != nil {
		return nil, err
	}
	return claim.result(), nil
}

type claimPreparation struct {
	id                 string
	actor              string
	isWisp             bool
	issueTable         string
	eventTable         string
	oldIssue           *types.Issue
	now                time.Time
	statusPlaceholders string
	statusArgs         []interface{}
	pools              []string
}

func (c claimPreparation) result() *ClaimResult {
	return &ClaimResult{OldIssue: c.oldIssue, IsWisp: c.isWisp}
}

func prepareClaimInTx(ctx context.Context, tx DBTX, id, actor string) (claimPreparation, error) {
	isWisp := IsActiveWispInTx(ctx, tx, id)
	issueTable, _, eventTable, _ := WispTableRouting(isWisp)
	oldIssue, err := GetIssueInTx(ctx, tx, id)
	if err != nil {
		return claimPreparation{}, fmt.Errorf("failed to get issue for claim: %w", err)
	}
	statusPlaceholders, statusArgs, err := claimStatusArgsInTx(ctx, tx)
	if err != nil {
		return claimPreparation{}, err
	}
	pools, err := ClaimPoolAliasesInTx(ctx, tx)
	if err != nil {
		return claimPreparation{}, fmt.Errorf("failed to resolve claim pools: %w", err)
	}
	return claimPreparation{
		id: id, actor: actor, isWisp: isWisp, issueTable: issueTable, eventTable: eventTable,
		oldIssue: oldIssue, now: time.Now().UTC(), statusPlaceholders: statusPlaceholders,
		statusArgs: statusArgs, pools: pools,
	}, nil
}

func claimStatusArgsInTx(ctx context.Context, tx DBTX) (string, []interface{}, error) {
	claimableStatuses, err := ClaimableSourceStatusesInTx(ctx, tx)
	if err != nil {
		return "", nil, fmt.Errorf("failed to resolve claimable statuses: %w", err)
	}
	placeholders, args := buildSQLInClause(claimableStatuses)
	return placeholders, args, nil
}

func executeClaimUpdate(ctx context.Context, tx DBTX, claim claimPreparation) (int64, error) {
	if !claimableAssignee(claim.oldIssue, claim.actor, claim.pools) {
		return 0, nil
	}
	rowLockClause, rowLockArgs := RowLockClause()
	args := []interface{}{claim.actor, claim.now}
	startedClause := ""
	if claim.oldIssue.StartedAt == nil {
		args = append(args, claim.now)
		startedClause = "started_at = ?, "
	}
	args = append(args, rowLockArgs...)
	args = append(args, claim.id, claim.oldIssue.RowVersion)
	args = append(args, claim.statusArgs...)
	query := fmt.Sprintf(`
		UPDATE %s
		SET assignee = ?, status = 'in_progress', updated_at = ?, %s%s
		WHERE id = ? AND row_lock = ? AND status IN (%s)
	`, claim.issueTable, startedClause, rowLockClause, claim.statusPlaceholders)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to claim issue: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get rows affected: %w", err)
	}
	return rowsAffected, nil
}

func claimableAssignee(issue *types.Issue, actor string, pools []string) bool {
	return issue.Assignee == "" || actorMatches(issue.Assignee, actor) || slices.Contains(pools, issue.Assignee)
}

func resolveClaimConflict(ctx context.Context, tx DBTX, claim claimPreparation) (*ClaimResult, error) {
	assignee, currentStatus, err := readClaimStateInTx(ctx, tx, claim.issueTable, claim.id)
	if err != nil {
		return nil, fmt.Errorf("failed to get current claim state: %w", err)
	}
	if actorMatches(assignee, claim.actor) && currentStatus == types.StatusInProgress {
		return claim.result(), nil
	}
	refusal := fmt.Errorf("%w%s%s", storage.ErrNotClaimable, storage.NotClaimableStatusFragment, currentStatus)
	if assignee != "" && !actorMatches(assignee, claim.actor) {
		refusal = claimRefusal(claim, assignee, currentStatus, refusal)
	}
	return nil, &publicops.ClaimConflictError{
		IssueID: claim.id, Assignee: assignee, Status: currentStatus, Err: refusal,
	}
}

func claimRefusal(claim claimPreparation, assignee string, currentStatus types.Status, refusal error) error {
	if slices.Contains(claim.pools, assignee) {
		return refusal
	}
	if currentStatus == types.StatusOpen {
		return fmt.Errorf("%w: already assigned to %q — coordinate with the holder; if their claim is abandoned (crashed agent), lease expiry will surface it for bd reclaim", storage.ErrAlreadyClaimed, assignee)
	}
	return fmt.Errorf("%w%s%s", storage.ErrAlreadyClaimed, storage.ClaimedByFragment, assignee)
}

func grantClaimLease(ctx context.Context, tx DBTX, claim claimPreparation) error {
	if claim.isWisp {
		return nil
	}
	return UpsertLeaseInTx(ctx, tx, claim.id, claim.actor, claim.now, leaseTTL(ctx))
}

func recordClaimInTx(ctx context.Context, tx DBTX, claim claimPreparation) error {
	oldData, _ := json.Marshal(claim.oldIssue)
	newData, _ := json.Marshal(map[string]interface{}{"assignee": claim.actor, "status": "in_progress"})
	if err := RecordFullEventInTable(ctx, tx, claim.eventTable, claim.id, types.EventClaimed, claim.actor, string(oldData), string(newData)); err != nil {
		return fmt.Errorf("failed to record claim event: %w", err)
	}
	return RecordEventInTx(ctx, tx, EventUpdate, claim.id)
}

// readClaimStateInTx reads one row's coordination columns inside tx — the
// state a lost compare-and-set reports. Shared by the CAS itself and by the
// public claim role, so the two cannot disagree about what "the state that
// refused this claim" means.
//
//nolint:gosec // G201: issueTable comes from WispTableRouting (hardcoded constants)
func readClaimStateInTx(ctx context.Context, tx DBTX, issueTable, id string) (string, types.Status, error) {
	var assignee sql.NullString
	var status types.Status
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT assignee, status FROM %s WHERE id = ?`, issueTable), id).Scan(&assignee, &status); err != nil {
		return "", "", err
	}
	return assignee.String, status, nil
}

// ClaimReadyIssueInTx claims the first currently ready issue matching filter in
// the same transaction that computes readiness. It returns nil when no matching
// ready issue can be claimed.
func ClaimReadyIssueInTx(
	ctx context.Context,
	tx DBTX,
	filter types.WorkFilter,
	actor string,
) (*types.Issue, error) {
	claimFilter := filter
	claimFilter.Status = types.StatusOpen
	claimFilter.Unassigned = true
	claimFilter.Assignee = nil
	// Claim only ever delivers the one issue it successfully claims below —
	// the breaker's job is to bound delivered payloads, and a claim's
	// payload is always exactly one row regardless of how large the ready
	// pool it scanned was. So a rig-wide BEADS_MAX_ROWS/--max-rows cap
	// (sized for bulk list/ready reads) must not fire here, and the scan
	// itself must stay unbounded (Limit=0): the loop below walks
	// readyIssues in order and continues past any that are transiently
	// unclaimable (already claimed by a racing agent, etc), so bounding
	// the scan to Limit=MaxRows (e.g. BEADS_MAX_ROWS=1 → scan only the
	// single top-of-queue row) would make claim spuriously return "nothing
	// to claim" whenever that narrow window is unclaimable, even with
	// plenty of other ready work available. Clear the cap fields so
	// GetReadyWorkInTx never returns ErrTooManyRows either.
	//
	// This is parity with pre-PR main (which never bounded the claim scan)
	// and correctness-first: an unbounded scan preserves claim's existing
	// fairness/ordering guarantee across the whole ready set. A paged scan
	// that stays bounded while still walking past unclaimable rows is a
	// reasonable follow-up, but it's a genuine behavior change (not a
	// MaxRows-cap fix) and is deliberately deferred rather than folded in
	// here.
	claimFilter.Limit = 0
	claimFilter.MaxRows = 0
	claimFilter.MaxRowsSource = ""
	// Claim the leaf, not the parent epic, so `bd ready --claim` takes
	// ep-7j4.2 rather than the still-open container.
	claimFilter.LeavesOnly = true

	readyIssues, err := GetReadyWorkInTx(ctx, tx, claimFilter)
	if err != nil {
		return nil, err
	}
	for _, issue := range readyIssues {
		if _, err := ClaimIssueInTx(ctx, tx, issue.ID, actor); err != nil {
			if errors.Is(err, storage.ErrAlreadyClaimed) || errors.Is(err, storage.ErrNotClaimable) {
				continue
			}
			return nil, err
		}
		claimed, err := GetIssueInTx(ctx, tx, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("get claimed issue: %w", err)
		}
		return claimed, nil
	}
	return nil, nil
}

// ClaimPoolAliasesInTx returns the pool pseudo-assignee aliases from the
// claim.pools config key (comma-separated, whitespace-trimmed). An issue
// assigned to one of these aliases is claimable by ANY actor through the
// normal claim CAS — the pattern where a dispatcher pre-assigns work to a
// group alias (e.g. "fable-crew") and members take items from the pool.
// Issues assigned to a real actor are unaffected. Missing/empty config (the
// default) disables pool-aware claiming entirely.
func ClaimPoolAliasesInTx(ctx context.Context, tx DBTX) ([]string, error) {
	raw, err := GetConfigInTx(ctx, tx, "claim.pools")
	if err != nil {
		return nil, err
	}
	return ParseClaimPools(raw), nil
}

// ParseClaimPools parses a raw claim.pools config value (comma-separated,
// whitespace-trimmed) into the pool alias list. Shared by the claim CAS
// (ClaimPoolAliasesInTx, and its domain/db dual) and the cmd-layer reassign
// fence (bd-98s5c), so the alias set can never drift between --claim and
// -a/--assignee — the two verbs must agree on which holders are pools.
func ParseClaimPools(raw string) []string {
	var pools []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			pools = append(pools, p)
		}
	}
	return pools
}

// ClaimableSourceStatusesInTx returns the set of statuses an issue may be
// claimed FROM: the built-in "open" and "blocked" statuses plus any configured
// custom status whose category is "active" (the same category that surfaces
// issues in bd ready). Stored blocked is claimable so an agent can resume after
// a manual hold (`bd update --status blocked` then later `--claim`). Custom
// statuses in the wip/done/frozen categories are intentionally excluded so
// claim retains its anti-steal protection (GH-3570) — an in_progress issue, or
// a custom alias for one, is never silently re-claimable. Unspecified-category
// customs are also excluded, matching their absence from bd ready.
func ClaimableSourceStatusesInTx(ctx context.Context, tx DBTX) ([]string, error) {
	statuses := []string{string(types.StatusOpen), string(types.StatusBlocked)}
	customs, err := ResolveCustomStatusesDetailedInTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, s := range customs {
		if s.Category == types.CategoryActive {
			statuses = append(statuses, s.Name)
		}
	}
	return statuses, nil
}
