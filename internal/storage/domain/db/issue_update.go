package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

type issueUpdatePlan struct {
	id               string
	actor            string
	opts             domain.IssueTableOpts
	table            string
	oldIssue         *types.Issue
	updates          map[string]any
	setClauses       []string
	args             []any
	clearLease       bool
	statusChanging   bool
	forceClosePolicy bool
	eventType        types.EventType
}

func (r *issueWriteRepository) prepareUpdate(ctx context.Context, id string, updates map[string]any, opts domain.IssueTableOpts) (*issueUpdatePlan, error) {
	if id == "" {
		return nil, errors.New("db: Update: id must not be empty")
	}
	if len(updates) == 0 {
		return nil, nil
	}

	updates = cloneUpdateFields(updates)
	forceClosePolicy := issueops.PopForceClosePolicy(updates)
	if err := validateUpdateFieldLengths(updates); err != nil {
		return nil, err
	}

	oldIssue, updates, err := r.loadUpdateState(ctx, id, updates, opts)
	if err != nil {
		return nil, err
	}
	if len(updates) == 0 {
		return nil, nil
	}

	statusChanging := hasStatusUpdate(updates)
	if err := r.enforceUpdateClosePolicy(ctx, id, oldIssue, updates, statusChanging, forceClosePolicy); err != nil {
		return nil, err
	}

	setClauses, args, clearLease, err := buildIssueUpdateSQL(oldIssue, updates, statusChanging)
	if err != nil {
		return nil, fmt.Errorf("db: Update: %w", err)
	}
	return &issueUpdatePlan{
		id:               id,
		actor:            "",
		opts:             opts,
		table:            pickIssueTable(opts.UseWispsTable),
		oldIssue:         oldIssue,
		updates:          updates,
		setClauses:       setClauses,
		args:             args,
		clearLease:       clearLease,
		statusChanging:   statusChanging,
		forceClosePolicy: forceClosePolicy,
		eventType:        updateEventType(oldIssue, updates, statusChanging),
	}, nil
}

func validateUpdateFieldLengths(updates map[string]any) error {
	for _, field := range []string{"assignee", "owner"} {
		if raw, ok := updates[field]; ok {
			if value, ok := raw.(string); ok {
				if err := types.CheckFieldLen(field, value); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (r *issueWriteRepository) loadUpdateState(ctx context.Context, id string, updates map[string]any, opts domain.IssueTableOpts) (*types.Issue, map[string]any, error) {
	oldIssue, err := r.lookup.Get(ctx, id, opts)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("db: Update %s: %w", id, sql.ErrNoRows)
		}
		return nil, nil, fmt.Errorf("db: Update %s: read old issue: %w", id, err)
	}

	if issueops.HasMergeOps(updates) {
		resolved, resolveErr := issueops.ResolveMergeOps(oldIssue, updates)
		if resolveErr != nil {
			return nil, nil, fmt.Errorf("db: Update %s: %w", id, resolveErr)
		}
		updates = resolved
	}
	if err := issueops.ValidateClosedAtCoherence(oldIssue, updates); err != nil {
		return nil, nil, fmt.Errorf("db: Update %s: %w", id, err)
	}
	filtered, err := issueops.DiscardNoopIssueUpdates(oldIssue, updates)
	if err != nil {
		return nil, nil, fmt.Errorf("db: Update %s: compare updates: %w", id, err)
	}
	return oldIssue, filtered, nil
}

func hasStatusUpdate(updates map[string]any) bool {
	_, ok := updates["status"]
	return ok
}

func (r *issueWriteRepository) enforceUpdateClosePolicy(ctx context.Context, id string, oldIssue *types.Issue, updates map[string]any, statusChanging, forceClosePolicy bool) error {
	if !statusChanging {
		return nil
	}
	crossing, err := issueops.CrossesIntoDoneCategoryInTx(ctx, r.runner, oldIssue.Status, updates)
	if err != nil {
		return fmt.Errorf("db: Update %s: %w", id, err)
	}
	if !crossing {
		return nil
	}
	if _, err := issueops.EnforceClosePolicyInTx(ctx, r.runner, id, forceClosePolicy); err != nil {
		return fmt.Errorf("db: Update %s: %w", id, err)
	}
	return nil
}

func buildIssueUpdateSQL(oldIssue *types.Issue, updates map[string]any, statusChanging bool) ([]string, []any, bool, error) {
	setClauses := make([]string, 0, len(updates)+3)
	args := make([]any, 0, len(updates)+4)
	for key, value := range updates {
		if _, ok := allowedUpdateFields[key]; !ok {
			return nil, nil, false, fmt.Errorf("field %q is not allowed", key)
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
	if statusChanging {
		setClauses, args = issueops.ManageClosedAt(oldIssue, updates, setClauses, args)
		setClauses, args = issueops.ManageStartedAt(oldIssue, updates, setClauses, args)
	}
	clearLease := issueops.ManageLeaseOnUpdate(oldIssue, updates)
	rowLockClause, rowLockArgs := issueops.RowLockClause()
	setClauses = append(setClauses, rowLockClause)
	args = append(args, rowLockArgs...)
	return setClauses, args, clearLease, nil
}

func updateEventType(oldIssue *types.Issue, updates map[string]any, statusChanging bool) types.EventType {
	if statusChanging {
		return issueops.DetermineEventType(oldIssue, updates)
	}
	return types.EventUpdated
}

func (r *issueWriteRepository) applyUpdate(ctx context.Context, plan *issueUpdatePlan) error {
	args := append(append([]any{}, plan.args...), plan.id)
	//nolint:gosec // G201: table is one of two hardcoded constants
	q := fmt.Sprintf("UPDATE %s SET %s WHERE id = ?", plan.table, strings.Join(plan.setClauses, ", "))
	res, err := r.runner.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("db: Update %s: %w", plan.id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("db: Update %s: rows affected: %w", plan.id, err)
	}
	if rows == 0 {
		return fmt.Errorf("db: Update %s: %w", plan.id, sql.ErrNoRows)
	}
	return r.clearUpdateLease(ctx, plan)
}

func (r *issueWriteRepository) clearUpdateLease(ctx context.Context, plan *issueUpdatePlan) error {
	if !plan.clearLease || plan.opts.UseWispsTable {
		return nil
	}
	if err := issueops.DeleteLeaseInTx(ctx, r.runner, plan.id); err != nil {
		return fmt.Errorf("db: Update %s: clear lease: %w", plan.id, err)
	}
	return nil
}

func (r *issueWriteRepository) finishUpdate(ctx context.Context, plan *issueUpdatePlan) error {
	if err := r.events.Record(ctx, domain.Event{
		IssueID: plan.id,
		Type:    plan.eventType,
		Actor:   plan.actor,
	}, domain.RecordEventOpts{UseWispsTable: plan.opts.UseWispsTable}); err != nil {
		return err
	}
	if err := r.refreshBlockedAfterStatusChange(ctx, plan); err != nil {
		return err
	}
	return issueops.RecordEventInTx(ctx, r.runner, issueops.EventUpdate, plan.id)
}

func (r *issueWriteRepository) refreshBlockedAfterStatusChange(ctx context.Context, plan *issueUpdatePlan) error {
	if !plan.statusChanging {
		return nil
	}
	newStatus := coerceStatus(plan.updates["status"])
	oldActive := plan.oldIssue.Status != types.StatusClosed && plan.oldIssue.Status != types.StatusPinned
	newActive := newStatus != types.StatusClosed && newStatus != types.StatusPinned
	if oldActive == newActive {
		return nil
	}
	var affectedIssues, affectedWisps []string
	var err error
	if plan.opts.UseWispsTable {
		affectedIssues, affectedWisps, err = issueops.AffectedByStatusChangeForWispInTx(ctx, r.runner, plan.id)
	} else {
		affectedIssues, affectedWisps, err = issueops.AffectedByStatusChangeInTx(ctx, r.runner, plan.id)
	}
	if err != nil {
		return fmt.Errorf("db: Update %s: affected by status change: %w", plan.id, err)
	}
	if err := issueops.RecomputeIsBlockedInTx(ctx, r.runner, affectedIssues, affectedWisps); err != nil {
		return fmt.Errorf("db: Update %s: recompute is_blocked: %w", plan.id, err)
	}
	return nil
}
