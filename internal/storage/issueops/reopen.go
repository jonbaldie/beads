package issueops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

type ReopenResult struct {
	IsWisp      bool
	AlreadyOpen bool
	Changed     bool
	// IssueRowsChanged is the concrete SQL fact that at least one issues row
	// changed in this transaction, either the reopened row or a recomputed row.
	IssueRowsChanged bool
}

// ReopenIssueInTx reopens only statuses in the done category. It leaves all
// other statuses unchanged.
func ReopenIssueInTx(ctx context.Context, tx DBTX, id, reason, actor string) (*ReopenResult, error) {
	return reopenIssueInTx(ctx, tx, id, reason, actor, true)
}

//nolint:gosec // G201: table names come from WispTableRouting (hardcoded constants)
func reopenIssueInTx(ctx context.Context, tx DBTX, id, reason, actor string, retryOnConditionalMiss bool) (*ReopenResult, error) {
	target, err := prepareReopenTarget(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if target.category != types.CategoryDone {
		return &ReopenResult{IsWisp: target.isWisp, AlreadyOpen: target.status == types.StatusOpen}, nil
	}
	target.affectedIssues, target.affectedWisps, err = reopenAffectedIDs(ctx, tx, target.isWisp, id)
	if err != nil {
		return nil, fmt.Errorf("affected by reopen for %s: %w", id, err)
	}
	return reopenDoneIssue(ctx, tx, id, reason, actor, retryOnConditionalMiss, target)
}

type reopenTarget struct {
	isWisp         bool
	issueTable     string
	eventTable     string
	status         types.Status
	category       types.StatusCategory
	affectedIssues []string
	affectedWisps  []string
}

func prepareReopenTarget(ctx context.Context, tx DBTX, id string) (reopenTarget, error) {
	isWisp := IsActiveWispInTx(ctx, tx, id)
	issueTable, _, eventTable, _ := WispTableRouting(isWisp)

	var current string
	err := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT status FROM %s WHERE id = ?", issueTable), id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return reopenTarget{}, fmt.Errorf("%w: issue %s", storage.ErrNotFound, id)
	}
	if err != nil {
		return reopenTarget{}, fmt.Errorf("read reopen status for %s: %w", id, err)
	}

	status := types.Status(current)
	category, err := ReopenCategoryInTx(ctx, tx, status)
	if err != nil {
		return reopenTarget{}, fmt.Errorf("resolve reopen category for %s: %w", id, err)
	}
	return reopenTarget{
		isWisp:     isWisp,
		issueTable: issueTable,
		eventTable: eventTable,
		status:     status,
		category:   category,
	}, nil
}

func reopenAffectedIDs(ctx context.Context, tx DBTX, isWisp bool, id string) ([]string, []string, error) {
	if isWisp {
		return AffectedByStatusChangeForWispInTx(ctx, tx, id)
	}
	return AffectedByStatusChangeInTx(ctx, tx, id)
}

func reopenDoneIssue(ctx context.Context, tx DBTX, id, reason, actor string, retryOnConditionalMiss bool, target reopenTarget) (*ReopenResult, error) {
	now := time.Now().UTC()
	rows, err := reopenStatusRow(ctx, tx, target.issueTable, id, target.status, now)
	if err != nil {
		return nil, err
	}
	if rows == 0 {
		return resolveReopenConditionalMiss(ctx, tx, id, reason, actor, retryOnConditionalMiss, target.isWisp, target.issueTable)
	}
	if err := completeReopenEffects(ctx, tx, id, reason, actor, target); err != nil {
		return nil, err
	}
	recompute, err := RecomputeIsBlockedInTxWithResult(ctx, tx, target.affectedIssues, target.affectedWisps)
	if err != nil {
		return nil, fmt.Errorf("recompute is_blocked after reopen for %s: %w", id, err)
	}

	// Snapshot only after all derived blocked-state maintenance has completed.
	// A reopen is a status change, so it journals as an update.
	if err := RecordEventInTx(ctx, tx, EventUpdate, id); err != nil {
		return nil, err
	}

	return &ReopenResult{
		IsWisp:           target.isWisp,
		Changed:          true,
		IssueRowsChanged: !target.isWisp || recompute.IssueRowsChanged,
	}, nil
}

func reopenStatusRow(ctx context.Context, tx DBTX, table, id string, status types.Status, now time.Time) (int64, error) {
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %s
		SET status = ?, closed_at = NULL, close_reason = '', closed_by_session = '', defer_until = NULL,
			updated_at = ?, row_lock = ?
		WHERE id = ? AND status = ?
	`, table), types.StatusOpen, now, freshRowLock(), id, status)
	if err != nil {
		return 0, fmt.Errorf("reopen issue: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("get reopen rows affected: %w", err)
	}
	return rows, nil
}

func resolveReopenConditionalMiss(ctx context.Context, tx DBTX, id, reason, actor string, retryOnConditionalMiss, isWisp bool, issueTable string) (*ReopenResult, error) {
	var latest string
	err := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT status FROM %s WHERE id = ?", issueTable), id).Scan(&latest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: issue %s", storage.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("check reopen status for %s: %w", id, err)
	}
	latestStatus := types.Status(latest)
	latestCategory, err := ReopenCategoryInTx(ctx, tx, latestStatus)
	if err != nil {
		return nil, fmt.Errorf("resolve latest reopen category for %s: %w", id, err)
	}
	if latestCategory != types.CategoryDone {
		return &ReopenResult{IsWisp: isWisp, AlreadyOpen: latestStatus == types.StatusOpen}, nil
	}
	if retryOnConditionalMiss {
		return reopenIssueInTx(ctx, tx, id, reason, actor, false)
	}
	return nil, fmt.Errorf("reopen issue %s: status changed concurrently", id)
}

func completeReopenEffects(ctx context.Context, tx DBTX, id, reason, actor string, target reopenTarget) error {

	if err := DeleteLeaseInTx(ctx, tx, id); err != nil {
		return err
	}

	if err := RecordEventInTable(ctx, tx, target.eventTable, id, types.EventReopened, actor, reason); err != nil {
		return fmt.Errorf("record reopen event: %w", err)
	}

	if reason != "" {
		if err := AddCommentEventInTx(ctx, tx, id, actor, reason); err != nil {
			return fmt.Errorf("add reopen comment: %w", err)
		}
	}
	return nil
}
