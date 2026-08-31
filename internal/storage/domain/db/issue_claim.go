package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

type issueClaimRepository struct {
	*issueRepositoryCore
	lookup *issueLookupRepository
}

type issueClaimPlan struct {
	id               string
	actor            string
	opts             domain.IssueTableOpts
	table            string
	now              time.Time
	oldIssue         *types.Issue
	startedWasZero   bool
	pools            []string
	statusPredicate  string
	statusArgs       []any
	rowLockClause    string
	rowLockArgs      []any
	assigneeEligible bool
}

func (r *issueClaimRepository) Claim(ctx context.Context, id, actor string, opts domain.IssueTableOpts) (domain.ClaimRowResult, error) {
	plan, err := r.prepareClaim(ctx, id, actor, opts)
	if err != nil {
		return domain.ClaimRowResult{}, err
	}
	rows, err := r.tryClaim(ctx, plan)
	if err != nil {
		return domain.ClaimRowResult{}, err
	}
	if rows == 0 {
		return r.claimConflict(ctx, plan)
	}
	return r.recordClaim(ctx, plan)
}

func (r *issueClaimRepository) prepareClaim(ctx context.Context, id, actor string, opts domain.IssueTableOpts) (*issueClaimPlan, error) {
	if id == "" {
		return nil, errors.New("db: Claim: id must not be empty")
	}
	if err := types.CheckFieldLen("actor", actor); err != nil {
		return nil, err
	}

	oldIssue, err := r.lookup.Get(ctx, id, opts)
	if err != nil {
		return nil, fmt.Errorf("db: Claim %s: read old issue: %w", id, err)
	}
	pools, err := issueops.ClaimPoolAliasesInTx(ctx, r.issueRepositoryCore.runner)
	if err != nil {
		return nil, fmt.Errorf("db: Claim %s: resolve claim pools: %w", id, err)
	}
	claimableStatuses, err := issueops.ClaimableSourceStatusesInTx(ctx, r.runner)
	if err != nil {
		return nil, fmt.Errorf("db: Claim %s: resolve claimable statuses: %w", id, err)
	}
	statusPredicate, statusArgs := buildClaimStatusPredicate(claimableStatuses)
	rowLockClause, rowLockArgs := issueops.RowLockClause()
	return &issueClaimPlan{
		id:               id,
		actor:            actor,
		opts:             opts,
		table:            pickIssueTable(opts.UseWispsTable),
		now:              time.Now().UTC(),
		oldIssue:         oldIssue,
		startedWasZero:   oldIssue.StartedAt == nil,
		pools:            pools,
		statusPredicate:  statusPredicate,
		statusArgs:       statusArgs,
		rowLockClause:    rowLockClause,
		rowLockArgs:      rowLockArgs,
		assigneeEligible: claimAssigneeEligible(oldIssue, actor, pools),
	}, nil
}

func buildClaimStatusPredicate(statuses []string) (string, []any) {
	predicate := "status = ?"
	args := []any{statuses[0]}
	for _, status := range statuses[1:] {
		predicate += " OR status = ?"
		args = append(args, status)
	}
	return predicate, args
}

func claimAssigneeEligible(issue *types.Issue, actor string, pools []string) bool {
	return issue.Assignee == "" || issueops.ActorMatches(issue.Assignee, actor) || slices.Contains(pools, issue.Assignee)
}

func (r *issueClaimRepository) tryClaim(ctx context.Context, plan *issueClaimPlan) (int64, error) {
	if !plan.assigneeEligible {
		return 0, nil
	}
	var (
		res sql.Result
		err error
	)
	if plan.startedWasZero {
		args := append([]any{plan.actor, plan.now, plan.now}, plan.rowLockArgs...)
		args = append(args, plan.id, plan.oldIssue.RowVersion)
		args = append(args, plan.statusArgs...)
		//nolint:gosec // G201: table is one of two hardcoded constants
		res, err = r.runner.ExecContext(ctx, fmt.Sprintf(`
			UPDATE %s
			SET assignee = ?, status = 'in_progress', updated_at = ?, started_at = ?, %s
			WHERE id = ? AND row_lock = ? AND (%s)
		`, plan.table, plan.rowLockClause, plan.statusPredicate), args...)
	} else {
		args := append([]any{plan.actor, plan.now}, plan.rowLockArgs...)
		args = append(args, plan.id, plan.oldIssue.RowVersion)
		args = append(args, plan.statusArgs...)
		//nolint:gosec // G201: table is one of two hardcoded constants
		res, err = r.runner.ExecContext(ctx, fmt.Sprintf(`
			UPDATE %s
			SET assignee = ?, status = 'in_progress', updated_at = ?, %s
			WHERE id = ? AND row_lock = ? AND (%s)
		`, plan.table, plan.rowLockClause, plan.statusPredicate), args...)
	}
	if err != nil {
		return 0, fmt.Errorf("db: Claim %s: %w", plan.id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("db: Claim %s: rows affected: %w", plan.id, err)
	}
	return rows, nil
}

func (r *issueClaimRepository) claimConflict(ctx context.Context, plan *issueClaimPlan) (domain.ClaimRowResult, error) {
	var currentAssignee sql.NullString
	var currentStatus types.Status
	//nolint:gosec // G201: table is one of two hardcoded constants
	if err := r.runner.QueryRowContext(ctx,
		fmt.Sprintf("SELECT assignee, status FROM %s WHERE id = ?", plan.table), plan.id,
	).Scan(&currentAssignee, &currentStatus); err != nil {
		return domain.ClaimRowResult{}, fmt.Errorf("db: Claim %s: read current state: %w", plan.id, err)
	}
	assignee := ""
	if currentAssignee.Valid {
		assignee = currentAssignee.String
	}
	return domain.ClaimRowResult{
		Updated:               false,
		CurrentAssignee:       assignee,
		CurrentAssigneeIsPool: slices.Contains(plan.pools, assignee),
		CurrentStatus:         currentStatus,
		StartedAtWasZero:      plan.startedWasZero,
		OldIssue:              plan.oldIssue,
	}, nil
}

func (r *issueClaimRepository) recordClaim(ctx context.Context, plan *issueClaimPlan) (domain.ClaimRowResult, error) {
	if !plan.opts.UseWispsTable {
		if err := issueops.UpsertLeaseInTx(ctx, r.runner, plan.id, plan.actor, plan.now, issueops.LeaseTTL(ctx)); err != nil {
			return domain.ClaimRowResult{}, fmt.Errorf("db: Claim %s: %w", plan.id, err)
		}
	}
	oldData, _ := json.Marshal(plan.oldIssue)
	newData, _ := json.Marshal(map[string]any{"assignee": plan.actor, "status": "in_progress"})
	if err := r.events.Record(ctx, domain.Event{
		IssueID:  plan.id,
		Type:     types.EventType("claimed"),
		Actor:    plan.actor,
		OldValue: string(oldData),
		NewValue: string(newData),
	}, domain.RecordEventOpts{UseWispsTable: plan.opts.UseWispsTable}); err != nil {
		return domain.ClaimRowResult{}, fmt.Errorf("db: Claim %s: record event: %w", plan.id, err)
	}
	if err := issueops.RecordEventInTx(ctx, r.runner, issueops.EventUpdate, plan.id); err != nil {
		return domain.ClaimRowResult{}, err
	}
	return domain.ClaimRowResult{
		Updated:          true,
		CurrentAssignee:  plan.actor,
		CurrentStatus:    types.StatusInProgress,
		StartedAtWasZero: plan.startedWasZero,
		OldIssue:         plan.oldIssue,
	}, nil
}
