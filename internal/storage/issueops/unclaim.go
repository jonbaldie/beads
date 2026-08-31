package issueops

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// UnclaimIssueInTx atomically releases a claimed issue: it clears the assignee,
// resets status to "open", clears started_at, deletes the issue's lease row
// (see UpsertLeaseInTx) and rewrites row_lock so a concurrent reclaim or close
// on the same row conflicts rather than silently cell-merging (see the
// row_lock invariant in lease.go). Records an "unclaimed" event.
//
// Ownership: only the current assignee may release its own claim. A mismatched
// actor is rejected with storage.ErrNotOwner rather than a silent no-op, so a
// second agent cannot yank a claim it does not hold. Ownership is compared
// under actorMatches (ga-5ksp5), so two spellings of the same Gas Town
// identity (e.g. a dotted alias vs its session-name form) both count as the
// owner — see canonicalActor. Pass force=true to bypass the ownership check
// (admin/reaper use, threaded from `bd unclaim --force`).
//
// Only works on issues that have an assignee and status is "open" or
// "in_progress". Returns error if:
//   - Issue is closed (cannot unclaim closed issues)
//   - Issue has no assignee (nothing to unclaim)
//   - Issue is claimed by a different actor and force is false (ErrNotOwner)
//
//nolint:gosec // G201: table names come from WispTableRouting (hardcoded constants)
func UnclaimIssueInTx(ctx context.Context, tx DBTX, id string, actor string, force bool) error {
	target, err := loadUnclaimTarget(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := validateUnclaimTarget(target.issue, id, actor, force); err != nil {
		return err
	}
	updated, err := updateUnclaimRow(ctx, tx, target.issueTable, id, target.issue.RowVersion)
	if err != nil {
		return err
	}
	if !updated {
		return resolveUnclaimNoMatch(ctx, tx, id, actor, force, "")
	}
	return finishUnclaimInTx(ctx, tx, target.eventTable, id, actor, target.issue)
}

type unclaimTarget struct {
	issueTable string
	eventTable string
	issue      *types.Issue
}

func loadUnclaimTarget(ctx context.Context, tx DBTX, id string) (unclaimTarget, error) {
	isWisp := IsActiveWispInTx(ctx, tx, id)
	issueTable, _, eventTable, _ := WispTableRouting(isWisp)
	issue, err := GetIssueInTx(ctx, tx, id)
	if err != nil {
		return unclaimTarget{}, fmt.Errorf("failed to get issue for unclaim: %w", err)
	}
	return unclaimTarget{issueTable: issueTable, eventTable: eventTable, issue: issue}, nil
}

func validateUnclaimTarget(issue *types.Issue, id, actor string, force bool) error {
	if issue.Status == types.StatusClosed {
		return fmt.Errorf("cannot unclaim closed issue %s", id)
	}
	if issue.Assignee == "" {
		return fmt.Errorf("issue %s is not assigned", id)
	}
	if !force && !actorMatches(issue.Assignee, actor) {
		return fmt.Errorf("%w: %s is held by %s; coordinate with the holder — pass --force only if their claim is abandoned (crashed agent, expired lease)",
			storage.ErrNotOwner, id, issue.Assignee)
	}
	return nil
}

func updateUnclaimRow(ctx context.Context, tx DBTX, issueTable, id string, rowVersion int64) (bool, error) {
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %s
		SET assignee = '', status = 'open', updated_at = ?,
		    started_at = NULL, row_lock = ?
		WHERE id = ? AND status IN ('open', 'in_progress') AND row_lock = ?
	`, issueTable), time.Now().UTC(), freshRowLock(), id, rowVersion)
	if err != nil {
		return false, fmt.Errorf("failed to unclaim issue: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to get rows affected: %w", err)
	}
	return rowsAffected > 0, nil
}

func resolveUnclaimNoMatch(ctx context.Context, tx DBTX, id, actor string, force bool, expectedAssignee string) error {
	current, err := GetIssueInTx(ctx, tx, id)
	if err != nil {
		return fmt.Errorf("failed to unclaim issue %s: no matching row", id)
	}
	if expectedAssignee == "" {
		if !force && !actorMatches(current.Assignee, actor) {
			return fmt.Errorf("%w: %s is held by %s; coordinate with the holder — pass --force only if their claim is abandoned (crashed agent, expired lease)",
				storage.ErrNotOwner, id, current.Assignee)
		}
	} else if !actorMatches(current.Assignee, expectedAssignee) {
		return fmt.Errorf("%w: %s is held by %q, expected %q", storage.ErrAssigneeMismatch, id, current.Assignee, expectedAssignee)
	}
	return fmt.Errorf("failed to unclaim issue %s: no matching row", id)
}

// finishUnclaimInTx applies the post-UPDATE half of a release shared by
// UnclaimIssueInTx and UnclaimIssueIfAssigneeInTx: it drops the lease row (a
// no-op when none exists, e.g. a wisp or an open-but-assigned issue that was
// never leased) and records the "unclaimed" event. The row mutation
// (assignee/status/started_at/row_lock) must already have been applied in tx.
func finishUnclaimInTx(ctx context.Context, tx DBTX, eventTable string, id string, actor string, oldIssue *types.Issue) error {
	if err := DeleteLeaseInTx(ctx, tx, id); err != nil {
		return err
	}

	oldData, _ := json.Marshal(oldIssue)
	newData, _ := json.Marshal(map[string]interface{}{
		"assignee": "",
		"status":   "open",
	})
	if err := RecordFullEventInTable(ctx, tx, eventTable, id, "unclaimed", actor, string(oldData), string(newData)); err != nil {
		return fmt.Errorf("failed to record unclaim event: %w", err)
	}
	// A release changes assignee and status, so it journals as an update. Both
	// unclaim entry points funnel through here after their CAS succeeded, so
	// this covers the conditional release too.
	return RecordEventInTx(ctx, tx, EventUpdate, id)
}

// UnclaimIssueIfAssigneeInTx atomically releases a claim only while the issue is
// still assigned to expectedAssignee — the compare-and-swap inverse of
// ClaimIssueInTx: a Go-side actorMatches precheck (ga-5ksp5) plus a conditional
// UPDATE CASed on row_lock, with RowsAffected as the verdict, so a stale
// releaser can never clobber a claim that has since moved to (or been
// re-taken by) someone else. "Still assigned to expectedAssignee" is judged
// under actorMatches, not verbatim equality, so a caller naming the current
// holder under a different layer's spelling of the same identity is a match,
// not a mismatch — see canonicalActor. On success it applies the same
// transition as UnclaimIssueInTx (assignee cleared, status reopened,
// started_at cleared, lease dropped, row_lock rewritten, "unclaimed" event
// recorded). When the current assignee does not match expectedAssignee —
// including when the issue is no longer assigned at all — it returns
// storage.ErrAssigneeMismatch naming the current holder and leaves the row
// untouched. actor is recorded as the event author.
//
//nolint:gosec // G201: table names come from WispTableRouting (hardcoded constants)
func UnclaimIssueIfAssigneeInTx(ctx context.Context, tx DBTX, id string, actor string, expectedAssignee string) error {
	if expectedAssignee == "" {
		return fmt.Errorf("conditional unclaim of %s: expected assignee must not be empty (use UnclaimIssueInTx for an unconditional release)", id)
	}
	target, err := loadUnclaimTarget(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := validateConditionalUnclaimTarget(target.issue, id, expectedAssignee); err != nil {
		return err
	}
	updated, err := updateUnclaimRow(ctx, tx, target.issueTable, id, target.issue.RowVersion)
	if err != nil {
		return err
	}
	if !updated {
		return resolveUnclaimNoMatch(ctx, tx, id, actor, false, expectedAssignee)
	}
	return finishUnclaimInTx(ctx, tx, target.eventTable, id, actor, target.issue)
}

func validateConditionalUnclaimTarget(issue *types.Issue, id, expectedAssignee string) error {
	if issue.Status == types.StatusClosed {
		return fmt.Errorf("cannot unclaim closed issue %s", id)
	}
	if !actorMatches(issue.Assignee, expectedAssignee) {
		return fmt.Errorf("%w: %s is held by %q, expected %q", storage.ErrAssigneeMismatch, id, issue.Assignee, expectedAssignee)
	}
	return nil
}
