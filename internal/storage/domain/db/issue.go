package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/storage/sqlbuild"
	"github.com/jonbaldie/beads/internal/types"
	publicops "github.com/jonbaldie/beads/issueops"
)

// issueRepositoryCore owns the SQL runner and event sink shared by the
// focused issue-repository roles.
type issueRepositoryCore struct {
	runner Runner
	events domain.EventsSQLRepository
}

// issueSQLRepositoryImpl is the composition root. Its role implementations
// are anonymous so their methods remain promoted through the single public
// IssueSQLRepository contract.
type issueSQLRepositoryImpl struct {
	*issueRepositoryCore
	*issueWriteRepository
	*issueLookupRepository
	*issueReportRepository
	*issueSearchRepository
	*issueSearchDescendantRepository
	*issueCountSearchRepository
	*issueReadyRepository
	*issueDependencyRepository
	*issueDeletionRepository
	*issueLifecycleRepository
	*issueClaimRepository
}

type issueWriteRepository struct {
	*issueRepositoryCore
	lookup *issueLookupRepository
}

type issueLookupRepository struct {
	*issueRepositoryCore
}

type issueReportRepository struct {
	*issueRepositoryCore
}

type issueDependencyRepository struct {
	*issueRepositoryCore
}

type issueDeletionRepository struct {
	*issueRepositoryCore
}

type issueLifecycleRepository struct {
	*issueRepositoryCore
}

var _ domain.IssueSQLRepository = (*issueSQLRepositoryImpl)(nil)

func NewIssueSQLRepository(runner Runner) domain.IssueSQLRepository {
	core := &issueRepositoryCore{
		runner: runner,
		events: NewEventsSQLRepository(runner),
	}
	lookup := &issueLookupRepository{issueRepositoryCore: core}
	hydrator := &issueSearchHydrator{issueRepositoryCore: core}
	descendants := &issueSearchDescendantRepository{
		issueRepositoryCore: core,
		hydrator:            hydrator,
	}
	search := &issueSearchRepository{
		issueRepositoryCore: core,
		hydrator:            hydrator,
	}
	counts := &issueCountSearchRepository{issueRepositoryCore: core, search: search}
	repo := &issueSQLRepositoryImpl{
		issueRepositoryCore:             core,
		issueWriteRepository:            &issueWriteRepository{issueRepositoryCore: core, lookup: lookup},
		issueLookupRepository:           lookup,
		issueReportRepository:           &issueReportRepository{issueRepositoryCore: core},
		issueSearchRepository:           search,
		issueSearchDescendantRepository: descendants,
		issueCountSearchRepository:      counts,
		issueReadyRepository:            &issueReadyRepository{issueRepositoryCore: core, search: search, counts: counts},
		issueDependencyRepository:       &issueDependencyRepository{issueRepositoryCore: core},
		issueDeletionRepository:         &issueDeletionRepository{issueRepositoryCore: core},
		issueLifecycleRepository:        &issueLifecycleRepository{issueRepositoryCore: core},
		issueClaimRepository:            &issueClaimRepository{issueRepositoryCore: core, lookup: lookup},
	}
	// The role fields are anonymous on purpose: method promotion is the seam
	// that keeps this implementation source-compatible with the interface.
	// Read them at the composition boundary so static analyzers recognize that
	// the fields are used to assemble that promoted method set.
	_ = repo.issueRepositoryCore
	_ = repo.issueWriteRepository
	_ = repo.issueLookupRepository
	_ = repo.issueReportRepository
	_ = repo.issueSearchRepository
	_ = repo.issueSearchDescendantRepository
	_ = repo.issueCountSearchRepository
	_ = repo.issueReadyRepository
	_ = repo.issueDependencyRepository
	_ = repo.issueDeletionRepository
	_ = repo.issueLifecycleRepository
	_ = repo.issueClaimRepository
	return repo
}

// issueSelectColumns aliases the shared canonical column list; the scan side
// delegates to issueops.ScanIssueFrom, which scans it positionally.
const issueSelectColumns = sqlbuild.IssueSelectColumns

var allowedUpdateFields = map[string]struct{}{
	"status": {}, "priority": {}, "title": {}, "assignee": {}, "owner": {},
	"description": {}, "design": {}, "acceptance_criteria": {}, "notes": {},
	"issue_type": {}, "estimated_minutes": {}, "external_ref": {}, "spec_id": {},
	"started_at": {}, "closed_at": {}, "close_reason": {}, "closed_by_session": {},
	"source_repo": {}, "sender": {}, "wisp": {}, "wisp_type": {}, "no_history": {}, "pinned": {},
	"mol_type": {}, "event_kind": {}, "actor": {}, "target": {}, "payload": {},
	"due_at": {}, "defer_until": {}, "await_id": {}, "waiters": {},
	"metadata": {},
}

var updateFieldColumnRename = map[string]string{
	"wisp": "ephemeral",
}

func (r *issueWriteRepository) Insert(ctx context.Context, issue *types.Issue, actor string, opts domain.InsertIssueOpts) error {
	if issue == nil {
		return errors.New("db: Insert: issue must not be nil")
	}

	normalizeIssueTimestamps(issue)
	if issue.ContentHash == "" {
		issue.ContentHash = issue.ComputeContentHash()
	}

	if issue.ID == "" {
		return errors.New("db: Insert: explicit ID required (ID generation belongs to CreateIssueUseCase)")
	}

	table := pickIssueTable(opts.UseWispsTable)
	if opts.CreateOnly {
		return r.insertCreateOnly(ctx, issue, actor, opts, table)
	}
	return r.insertOrUpdate(ctx, issue, actor, opts, table)
}

func (r *issueWriteRepository) insertCreateOnly(ctx context.Context, issue *types.Issue, actor string, opts domain.InsertIssueOpts, table string) error {
	if err := issueops.EnsureIssueIDAvailableInTx(ctx, r.runner, issue.ID); err != nil {
		return err
	}
	if err := issueops.InsertIssueStrictInTx(ctx, r.runner, table, issue); err != nil {
		return err
	}
	return r.recordIssueCreated(ctx, issue.ID, actor, opts)
}

func (r *issueWriteRepository) insertOrUpdate(ctx context.Context, issue *types.Issue, actor string, opts domain.InsertIssueOpts, table string) error {
	if err := insertIssueRow(ctx, r.runner, table, issue); err != nil {
		return err
	}
	return r.recordIssueCreated(ctx, issue.ID, actor, opts)
}

func (r *issueWriteRepository) recordIssueCreated(ctx context.Context, issueID, actor string, opts domain.InsertIssueOpts) error {
	if err := r.events.Record(ctx, domain.Event{
		IssueID: issueID,
		Type:    types.EventCreated,
		Actor:   actor,
	}, domain.RecordEventOpts{UseWispsTable: opts.UseWispsTable}); err != nil {
		return err
	}
	return issueops.RecordEventInTx(ctx, r.runner, issueops.EventCreate, issueID)
}

func (r *issueWriteRepository) InsertBatch(ctx context.Context, issues []*types.Issue, actor string, opts domain.InsertIssueOpts) error {
	for _, issue := range issues {
		if err := r.Insert(ctx, issue, actor, opts); err != nil {
			return err
		}
	}
	return nil
}

// PromoteFromEphemeral promotes an active wisp into the Dolt-versioned issues
// plane in place: same id, wisp_type retained, labels/dependencies/events/
// comments carried across to the permanent tables, inbound wisp-targeted
// dependency edges retargeted, and blocked state recomputed. It delegates to
// the exact issueops implementation the classic (direct/embedded) route runs,
// so the two modes cannot drift. The issueops error is returned unwrapped on
// purpose: the CLI surfaces it verbatim ("wisp <id> not found"), and that
// text is part of the classic error contract.
func (r *issueWriteRepository) PromoteFromEphemeral(ctx context.Context, id, actor string) error {
	if id == "" {
		return errors.New("db: PromoteFromEphemeral: id must not be empty")
	}
	return issueops.PromoteFromEphemeralInTx(ctx, r.runner, id, actor)
}

func (r *issueWriteRepository) MovePersistence(ctx context.Context, id string, mode types.PersistenceMode) (bool, error) {
	issue, err := issueops.GetIssueInTx(ctx, r.runner, id)
	if err != nil {
		return false, fmt.Errorf("db: MovePersistence %s: get issue: %w", id, err)
	}
	result, err := issueops.MoveIssuePersistenceInTx(ctx, r.runner, issue, mode)
	if err != nil {
		return false, fmt.Errorf("db: MovePersistence %s: %w", id, err)
	}
	return result.Changed, nil
}

func (r *issueWriteRepository) Update(ctx context.Context, id string, updates map[string]any, actor string, opts domain.IssueTableOpts) error {
	plan, err := r.prepareUpdate(ctx, id, updates, opts)
	if err != nil || plan == nil {
		return err
	}
	plan.actor = actor
	if err := r.applyUpdate(ctx, plan); err != nil {
		return err
	}
	return r.finishUpdate(ctx, plan)
}

/*
	updates = cloneUpdateFields(updates)
	// Pop the close-policy override before anything reads the map as a set of
	// columns, mirroring issueops.updateIssueInTx. The no-op filter below keeps
	// unrecognized keys, so a surviving override would reach the field
	// allowlist and be refused by name.
	forceClosePolicy := issueops.PopForceClosePolicy(updates)

	// Bound the VARCHAR(255) assignment columns before touching SQL, mirroring
	// issueops.updateIssueInTx: an over-length assignee/owner aborts with a typed
	// ErrFieldTooLong instead of a raw backend "data too long" error.
	for _, field := range []string{"assignee", "owner"} {
		if raw, ok := updates[field]; ok {
			if val, ok := raw.(string); ok {
				if err := types.CheckFieldLen(field, val); err != nil {
					return err
				}
			}
		}
	}

	table := pickIssueTable(opts.UseWispsTable)

	mergeOps := issueops.HasMergeOps(updates)

	// Read the prior row once. Status and merge updates need it for their
	// transaction-local resolution, and every update uses it to suppress true
	// no-ops before changing row_lock or recording an event.
	oldIssue, err := r.lookup.Get(ctx, id, opts)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("db: Update %s: %w", id, sql.ErrNoRows)
		}
		return fmt.Errorf("db: Update %s: read old issue: %w", id, err)
	}

	// Resolve read-merge-write operation keys (issueops.OpMergeMetadata,
	// OpSetMetadata, OpUnsetMetadata, OpAppendNotes) into concrete column
	// values inside the mutation transaction, mirroring the embedded path
	// (issueops.updateIssueInTx). Callers must pass the OPERATION, never a
	// value pre-merged from an earlier read: this runner is a Dolt sql-server
	// session where FOR UPDATE is a parse-only no-op, so a stale-snapshot merge
	// is only made safe by Dolt's commit-time conflict detection plus the
	// caller redoing the whole unit of work on a serialization failure — and
	// that redo re-runs this in-transaction resolution against the winner's
	// committed row.
	if mergeOps {
		resolved, err := issueops.ResolveMergeOps(oldIssue, updates)
		if err != nil {
			return fmt.Errorf("db: Update %s: %w", id, err)
		}
		updates = resolved
	}

	// closed_at coherence parity with issueops.updateIssueInTx: an explicit
	// closed_at must agree with the status this update lands, checked against
	// the row this unit of work already read and ahead of the close-policy gate
	// so a refusal writes nothing at all. It runs on the merge-resolved map
	// BEFORE the no-op filter for the same reason it does there — the guard
	// reads the caller's intent, and a closed_at equal to the stored value is
	// still a request to keep the column, not an absent key.
	if err := issueops.ValidateClosedAtCoherence(oldIssue, updates); err != nil {
		return fmt.Errorf("db: Update %s: %w", id, err)
	}

	filteredUpdates, err := issueops.DiscardNoopIssueUpdates(oldIssue, updates)
	if err != nil {
		return fmt.Errorf("db: Update %s: compare updates: %w", id, err)
	}
	updates = filteredUpdates
	if len(updates) == 0 {
		return nil
	}
	// A status that matched the row was already dropped as a no-op, so the
	// lifecycle side effects below only fire on a real transition.
	_, statusChanging := updates["status"]

	// Close-policy parity with issueops.updateIssueInTx: a status that crosses
	// into the done category is a close by another name and answers to close
	// policy. A refusal returns before any write and aborts the caller's unit of
	// work. The wrap keeps the sentinels matchable, so a caller distinguishes
	// these refusals here exactly as it does on the close path.
	if statusChanging {
		crossing, err := issueops.CrossesIntoDoneCategoryInTx(ctx, r.runner, oldIssue.Status, updates)
		if err != nil {
			return fmt.Errorf("db: Update %s: %w", id, err)
		}
		if crossing {
			if _, err := issueops.EnforceClosePolicyInTx(ctx, r.runner, id, forceClosePolicy); err != nil {
				return fmt.Errorf("db: Update %s: %w", id, err)
			}
		}
	}

	setClauses := make([]string, 0, len(updates)+3)
	args := make([]any, 0, len(updates)+4)
	for key, value := range updates {
		if _, ok := allowedUpdateFields[key]; !ok {
			return fmt.Errorf("db: Update: field %q is not allowed", key)
		}
		column := key
		if renamed, ok := updateFieldColumnRename[key]; ok {
			column = renamed
		}
		setClauses = append(setClauses, fmt.Sprintf("`%s` = ?", column))
		args = append(args, normalizeUpdateValue(key, value))
	}
	setClauses = append(setClauses, "updated_at = ?")
	args = append(args, time.Now().UTC())

	// Lifecycle parity with issueops.updateIssueInTx: auto-manage closed_at and
	// started_at from the status transition unless the caller set them
	// explicitly.
	if statusChanging {
		setClauses, args = issueops.ManageClosedAt(oldIssue, updates, setClauses, args)
		setClauses, args = issueops.ManageStartedAt(oldIssue, updates, setClauses, args)
	}
	clearLease := issueops.ManageLeaseOnUpdate(oldIssue, updates)

	// Rewrite row_lock on every generic update, mirroring the classic
	// issueops.updateIssueInTx invariant (update.go): a concurrent
	// status/ownership mutation collides on this shared cell instead of
	// silently cell-merging, and the row's RowVersion CAS token advances so
	// the "generic update path changes RowVersion" contract
	// (types.Issue.RowVersion) holds on the proxied backend too.
	rowLockClause, rowLockArgs := issueops.RowLockClause()
	setClauses = append(setClauses, rowLockClause)
	args = append(args, rowLockArgs...)

	args = append(args, id)

	//nolint:gosec // G201: table is one of two hardcoded constants
	q := fmt.Sprintf("UPDATE %s SET %s WHERE id = ?", table, strings.Join(setClauses, ", "))
	res, err := r.runner.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("db: Update %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("db: Update %s: rows affected: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("db: Update %s: %w", id, sql.ErrNoRows)
	}
	if clearLease && !opts.UseWispsTable {
		if err := issueops.DeleteLeaseInTx(ctx, r.runner, id); err != nil {
			return fmt.Errorf("db: Update %s: clear lease: %w", id, err)
		}
	}

	// Event-type parity: embedded records EventClosed / EventReopened /
	// EventStatusChanged for status transitions (issueops.DetermineEventType),
	// EventUpdated otherwise.
	eventType := types.EventUpdated
	if statusChanging {
		eventType = issueops.DetermineEventType(oldIssue, updates)
	}
	if err := r.events.Record(ctx, domain.Event{
		IssueID: id,
		Type:    eventType,
		Actor:   actor,
	}, domain.RecordEventOpts{UseWispsTable: opts.UseWispsTable}); err != nil {
		return err
	}

	if statusChanging {
		newStatus := coerceStatus(updates["status"])
		oldActive := oldIssue.Status != types.StatusClosed && oldIssue.Status != types.StatusPinned
		newActive := newStatus != types.StatusClosed && newStatus != types.StatusPinned
		if oldActive != newActive {
			var (
				affectedIssues, affectedWisps []string
				aerr                          error
			)
			if opts.UseWispsTable {
				affectedIssues, affectedWisps, aerr = issueops.AffectedByStatusChangeForWispInTx(ctx, r.runner, id)
			} else {
				affectedIssues, affectedWisps, aerr = issueops.AffectedByStatusChangeInTx(ctx, r.runner, id)
			}
			if aerr != nil {
				return fmt.Errorf("db: Update %s: affected by status change: %w", id, aerr)
			}
			if err := issueops.RecomputeIsBlockedInTx(ctx, r.runner, affectedIssues, affectedWisps); err != nil {
				return fmt.Errorf("db: Update %s: recompute is_blocked: %w", id, err)
			}
		}
	}
	// Snapshot only after all derived blocked-state maintenance has completed.
	// The no-op early returns above wrote nothing and journal nothing.
	return issueops.RecordEventInTx(ctx, r.runner, issueops.EventUpdate, id)
}
*/

// CompareAndSetMetadataKey runs the SHARED compare-and-set body, unwrapped.
//
// It is the whole of this leg's implementation, and that is the point: the two
// store backends wrap the same function in their own transaction, so the third
// leg is a wrapper check rather than an independent vote — which is what the
// conformance contract's header says.
//
// It does NOT wrap the error the way its siblings above do, for the reason
// WalkDependencyTree gives: the body publishes storage.ErrNotFound and
// storage.ErrValidation as the role's own vocabulary, both classified by
// errors.Is at every front door, and a "db: ..." prefix would put this
// repository's name into a message the direct route never shows a user.
func (r *issueWriteRepository) CompareAndSetMetadataKey(ctx context.Context, plan storage.CompareAndSetKeyPlan) (publicops.CompareAndSetKeyResult, bool, error) {
	result, write, err := issueops.CompareAndSetMetadataKeyInTx(ctx, r.runner, plan)
	return result, write.Wrote, err
}

// ReleaseIssue runs the SHARED claim-release body, unwrapped.
//
// It is the whole of this leg's implementation, and that is the point: the two
// store backends wrap the same function in their own transaction, so the third
// leg is a wrapper check rather than an independent vote — which is what the
// conformance contract's header says.
//
// It reports the body's Wrote — "a row was written" — and NOT the durable table
// set beside it, which is the half of ReleaseWrite the two STORE legs want. The
// difference is load-bearing on this leg and it cost a red test to find: an
// ephemeral release writes a wisp row and changes no versioned table, and this
// leg's commit message is what commits the SQL transaction as well as what
// versions it, so reporting the table set here composes no message and rolls the
// release back — the wisp comes out still claimed. The version-control layer
// below already demotes a commit with nothing pending to a plain SQL COMMIT, so
// answering the row fact costs no spurious history entry.
//
// It does NOT wrap the error, for the reason CompareAndSetMetadataKey gives.
func (r *issueWriteRepository) ReleaseIssue(ctx context.Context, req publicops.ReleaseRequest) (publicops.ReleaseResult, bool, error) {
	result, write, err := issueops.ReleaseIssueInTx(ctx, r.runner, req)
	return result, write.Wrote, err
}

func cloneUpdateFields(updates map[string]any) map[string]any {
	cloned := make(map[string]any, len(updates))
	for key, value := range updates {
		cloned[key] = value
	}
	return cloned
}

func coerceStatus(v any) types.Status {
	switch s := v.(type) {
	case string:
		return types.Status(s)
	case types.Status:
		return s
	default:
		return ""
	}
}
