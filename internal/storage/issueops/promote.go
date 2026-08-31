package issueops

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

//nolint:gosec // G201: table names are hardcoded constants
func PromoteFromEphemeralInTx(ctx context.Context, tx DBTX, id string, _ string) error {
	if !IsActiveWispInTx(ctx, tx, id) {
		return fmt.Errorf("wisp %s not found", id)
	}
	return promoteWispAggregate(ctx, tx, id)
}

func promoteWispAggregate(ctx context.Context, tx DBTX, id string) error {
	issue, err := preparePromotedIssue(ctx, tx, id)
	if err != nil {
		return err
	}
	if _, _, err := InsertIssueIfNew(ctx, tx, "issues", issue, storage.BatchCreateOptions{}); err != nil {
		return fmt.Errorf("promote wisp to issues: %w", err)
	}

	return finishPromotedWisp(ctx, tx, id)
}

func finishPromotedWisp(ctx context.Context, tx DBTX, id string) error {
	if err := copyPromotedWispData(ctx, tx, id); err != nil {
		return err
	}
	if err := RetargetInboundDependenciesToIssueInTx(ctx, tx, id); err != nil {
		return err
	}
	if err := deletePromotedWisp(ctx, tx, id); err != nil {
		return err
	}
	if err := recomputePromotedBlockedState(ctx, tx, id); err != nil {
		return err
	}

	// The bead keeps its ID across promotion; only its plane changes. Journal
	// one update carrying the now-durable snapshot, after derived blocked-state
	// maintenance has settled.
	if err := RecordEventInTx(ctx, tx, EventUpdate, id); err != nil {
		return err
	}
	return nil
}

func preparePromotedIssue(ctx context.Context, tx DBTX, id string) (*types.Issue, error) {
	issue, err := GetIssueInTx(ctx, tx, id)
	if err != nil {
		return nil, fmt.Errorf("get wisp for promote: %w", err)
	}
	if issue == nil {
		return nil, fmt.Errorf("wisp %s not found", id)
	}

	// A promoted wisp is fully durable: clear BOTH wisp-plane flags, not just
	// Ephemeral. A no-history wisp (Ephemeral=false, NoHistory=true) promoted
	// with NoHistory intact lands in the issues table still flag-marked as
	// wisp-plane state, and everything that infers the plane from flags —
	// most damagingly import's table routing — silently re-planes it back
	// into the (default-export-excluded) wisps table, dropping its relations
	// on the way (bd-r9uce). Post-promotion the flag has no meaning.
	issue.Ephemeral = false
	issue.NoHistory = false
	// Promotion clears an explicit ephemeral class marker to select normalized
	// versioned storage (same rule as types.NormalizePersistenceMode); with
	// both plane flags cleared, a lingering explicit ephemeral class would
	// fail validation in PrepareIssueForInsert below.
	if issue.StorageClass == types.StorageClassEphemeral {
		issue.StorageClass = ""
	}

	// Read the custom-status/type config directly (NewBatchContext needs a
	// *sql.Tx; promote only uses these two fields of it, and the DBTX forms
	// let the proxied-server repository share this exact implementation).
	customStatuses, err := ResolveCustomStatusesDetailedInTx(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("failed to get custom statuses: %w", err)
	}
	customTypes, err := ResolveCustomTypesInTx(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("failed to get custom types: %w", err)
	}
	if err := PrepareIssueForInsert(issue, types.CustomStatusNames(customStatuses), customTypes); err != nil {
		return nil, fmt.Errorf("promote wisp to issues: %w", err)
	}
	return issue, nil
}

type promotedCopySpec struct {
	label     string
	copyQuery string
	deleteSQL string
}

func copyPromotedWispData(ctx context.Context, tx DBTX, id string) error {
	if err := copyPromotedRows(ctx, tx, id, promotedCopySpec{
		label: "labels",
		copyQuery: `
			INSERT IGNORE INTO labels (issue_id, label)
			SELECT issue_id, label FROM wisp_labels WHERE issue_id = ?
		`,
		deleteSQL: `DELETE FROM wisp_labels WHERE issue_id = ?`,
	}); err != nil {
		return err
	}

	// Carry id across promotion. Both tables derive id deterministically from the
	// same (issue_id, target) key, so the wisp edge's id is exactly the id a
	// direct dependency on that edge would get; copying it (rather than letting a
	// DEFAULT mint a fresh random one) keeps the promoted edge merge-safe and is
	// required now that dependencies.id has no DEFAULT (#4259).
	if err := copyPromotedRows(ctx, tx, id, promotedCopySpec{
		label: "dependencies",
		copyQuery: `
			INSERT IGNORE INTO dependencies (id, issue_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external, type, created_at, created_by, metadata, thread_id)
			SELECT id, issue_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external, type, created_at, created_by, metadata, thread_id
			FROM wisp_dependencies WHERE issue_id = ?
		`,
		deleteSQL: `DELETE FROM wisp_dependencies WHERE issue_id = ?`,
	}); err != nil {
		return err
	}
	if err := copyPromotedRows(ctx, tx, id, promotedCopySpec{
		label: "events",
		copyQuery: `
			INSERT IGNORE INTO events (id, issue_id, event_type, actor, old_value, new_value, comment, created_at)
			SELECT id, issue_id, event_type, actor, old_value, new_value, comment, created_at
			FROM wisp_events WHERE issue_id = ?
		`,
		deleteSQL: `DELETE FROM wisp_events WHERE issue_id = ?`,
	}); err != nil {
		return err
	}
	return copyPromotedRows(ctx, tx, id, promotedCopySpec{
		label: "comments",
		copyQuery: `
			INSERT IGNORE INTO comments (id, issue_id, author, text, created_at)
			SELECT id, issue_id, author, text, created_at
			FROM wisp_comments WHERE issue_id = ?
		`,
		deleteSQL: `DELETE FROM wisp_comments WHERE issue_id = ?`,
	})
}

func copyPromotedRows(ctx context.Context, tx DBTX, id string, spec promotedCopySpec) error {
	if _, err := tx.ExecContext(ctx, spec.copyQuery, id); err != nil {
		return fmt.Errorf("copy %s for promoted wisp %s: %w", spec.label, id, err)
	}
	if _, err := tx.ExecContext(ctx, spec.deleteSQL, id); err != nil {
		return fmt.Errorf("delete copied wisp %s for promoted wisp %s: %w", spec.label, id, err)
	}
	return nil
}

func deletePromotedWisp(ctx context.Context, tx DBTX, id string) error {
	result, err := tx.ExecContext(ctx, `DELETE FROM wisps WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete promoted wisp row %s: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("get promoted wisp rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("wisp %s not found", id)
	}
	return nil
}

func recomputePromotedBlockedState(ctx context.Context, tx DBTX, id string) error {
	affectedIssues, affectedWisps, err := AffectedByStatusChangeInTx(ctx, tx, id)
	if err != nil {
		return fmt.Errorf("affected by promote for %s: %w", id, err)
	}
	if err := RecomputeIsBlockedInTx(ctx, tx, affectedIssues, affectedWisps); err != nil {
		return fmt.Errorf("recompute is_blocked after promote for %s: %w", id, err)
	}
	return nil
}
