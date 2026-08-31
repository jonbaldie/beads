package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

func (r *dependencyDeleteRepository) Delete(ctx context.Context, issueID, dependsOnID, actor string, opts domain.DepInsertOpts) (domain.DepDeleteResult, error) {
	if issueID == "" || dependsOnID == "" {
		return domain.DepDeleteResult{}, errors.New("db: DependencySQLRepository.Delete: issueID and dependsOnID must not be empty")
	}
	table := pickDepTable(opts.UseWispsTable)

	depType, depMetadata, err := lookupDependencyForDelete(ctx, r.runner, table, issueID, dependsOnID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.DepDeleteResult{Found: false}, nil
	case err != nil:
		return domain.DepDeleteResult{}, fmt.Errorf("db: DependencySQLRepository.Delete: lookup type %s -> %s: %w", issueID, dependsOnID, err)
	}

	if err := deleteDependencyRow(ctx, r.runner, table, issueID, dependsOnID); err != nil {
		return domain.DepDeleteResult{}, fmt.Errorf("db: DependencySQLRepository.Delete: %s -> %s: %w", issueID, dependsOnID, err)
	}

	// The type lookup above returned Found:false when no edge existed, so reaching
	// here means a row was deleted — record the dependency_removed event on the
	// source's event table, matching the embedded/issueops RemoveDependencyInTx path.
	// Gated on EmitEvent so only the explicit `bd dep remove` verb emits.
	if opts.EmitEvent {
		if err := r.events.Record(ctx, domain.Event{
			IssueID:  issueID,
			Type:     types.EventDependencyRemoved,
			Actor:    actor,
			NewValue: fmt.Sprintf("Removed dependency on %s", dependsOnID),
		}, domain.RecordEventOpts{UseWispsTable: opts.UseWispsTable}); err != nil {
			return domain.DepDeleteResult{}, fmt.Errorf("db: DependencySQLRepository.Delete: record dependency_removed event: %w", err)
		}
	}

	dt := types.DependencyType(depType)
	if err := maintainDeletedDependencyState(ctx, r.runner, issueID, dependsOnID, dt, depType, depMetadata, opts.UseWispsTable); err != nil {
		return domain.DepDeleteResult{}, err
	}

	return domain.DepDeleteResult{Found: true, Type: dt, DependsOnID: dependsOnID}, nil
}

func lookupDependencyForDelete(ctx context.Context, runner Runner, table, issueID, dependsOnID string) (string, string, error) {
	var depType, depMetadata string
	//nolint:gosec // G201: table and depTargetExpr are hardcoded constants
	err := runner.QueryRowContext(ctx,
		fmt.Sprintf("SELECT type, metadata FROM %s WHERE issue_id = ? AND %s = ?", table, depTargetExpr),
		issueID, dependsOnID,
	).Scan(&depType, &depMetadata)
	return depType, depMetadata, err
}

func deleteDependencyRow(ctx context.Context, runner Runner, table, issueID, dependsOnID string) error {
	//nolint:gosec // G201: table and depTargetExpr are hardcoded constants
	_, err := runner.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM %s WHERE issue_id = ? AND %s = ?", table, depTargetExpr),
		issueID, dependsOnID,
	)
	return err
}

func maintainDeletedDependencyState(ctx context.Context, runner Runner, issueID, dependsOnID string, depType types.DependencyType, depTypeName, depMetadata string, useWisps bool) error {
	var affectedIssues, affectedWisps []string
	var err error
	if useWisps {
		affectedIssues, affectedWisps, err = issueops.AffectedByDepChangeForWispInTx(ctx, runner, issueID, dependsOnID, depType)
	} else {
		affectedIssues, affectedWisps, err = issueops.AffectedByDepChangeInTx(ctx, runner, issueID, dependsOnID, depType)
	}
	if err != nil {
		return fmt.Errorf("db: DependencySQLRepository.Delete: affected set: %w", err)
	}
	if err := issueops.RecomputeIsBlockedInTx(ctx, runner, affectedIssues, affectedWisps); err != nil {
		return fmt.Errorf("db: DependencySQLRepository.Delete: recompute is_blocked: %w", err)
	}
	// Snapshot only after all derived blocked-state maintenance has completed.
	// Never gated on EmitEvent — a structural removal is as real to a replaying
	// consumer as one from an explicit dep verb.
	return issueops.RecordDepEventInTx(ctx, runner, issueops.EventDepRemove, issueID, depTypeName, dependsOnID, depMetadata)
}

func (r *dependencyCycleRepository) HasCycle(ctx context.Context, issueID, dependsOnID string) (bool, error) {
	if issueID == "" || dependsOnID == "" {
		return false, errors.New("db: DependencySQLRepository.HasCycle: issueID and dependsOnID must not be empty")
	}

	cycle, err := issueops.WouldCreateSchedulingCycleInTx(ctx, r.runner, issueID, dependsOnID, nil)
	if err != nil {
		return false, fmt.Errorf("db: DependencySQLRepository.HasCycle: %w", err)
	}
	return cycle, nil
}

func (r *dependencyListRepository) ListByIssueIDs(ctx context.Context, issueIDs []string, opts domain.DepListOpts) (domain.DepBulkResult, error) {
	result := domain.DepBulkResult{
		Outgoing: make(map[string][]*types.Dependency),
		Incoming: make(map[string][]*types.Dependency),
	}
	if len(issueIDs) == 0 {
		return result, nil
	}

	idPlaceholders, idArgs := buildInPlaceholders(issueIDs)
	typeWhere, typeArgs := buildTypeFilter(opts.Types)
	table := pickDepTable(opts.UseWispsTable)

	if opts.Direction == domain.DepDirectionBoth || opts.Direction == domain.DepDirectionOut {
		//nolint:gosec // G201: table and depSelectColumns are hardcoded
		q := fmt.Sprintf(
			`SELECT %s FROM %s WHERE issue_id IN (%s)%s ORDER BY issue_id`,
			depSelectColumns, table, idPlaceholders, typeWhere,
		)
		args := combineArgs(idArgs, typeArgs)
		if err := r.queryDeps(ctx, q, args, result.Outgoing, true); err != nil {
			return domain.DepBulkResult{}, fmt.Errorf("db: DependencySQLRepository.ListByIssueIDs (out): %w", err)
		}
	}

	if opts.Direction == domain.DepDirectionBoth || opts.Direction == domain.DepDirectionIn {
		//nolint:gosec // G201: table, depSelectColumns, depTargetExpr are hardcoded
		q := fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s IN (%s)%s ORDER BY issue_id`,
			depSelectColumns, table, depTargetExpr, idPlaceholders, typeWhere,
		)
		args := combineArgs(idArgs, typeArgs)
		if err := r.queryDeps(ctx, q, args, result.Incoming, false); err != nil {
			return domain.DepBulkResult{}, fmt.Errorf("db: DependencySQLRepository.ListByIssueIDs (in): %w", err)
		}
	}

	return result, nil
}

func (r *dependencyListRepository) CountsByIssueIDs(ctx context.Context, issueIDs []string, opts domain.DepCountsOpts) (map[string]*types.DependencyCounts, error) {
	result := make(map[string]*types.DependencyCounts)
	if len(issueIDs) == 0 {
		return result, nil
	}
	for _, id := range issueIDs {
		result[id] = &types.DependencyCounts{}
	}

	idPlaceholders, idArgs := buildInPlaceholders(issueIDs)
	table := pickDepTable(opts.UseWispsTable)

	//nolint:gosec // G201: table is one of two hardcoded constants
	outQ := fmt.Sprintf(
		`SELECT issue_id, COUNT(*) FROM %s WHERE issue_id IN (%s) AND type = 'blocks' GROUP BY issue_id`,
		table, idPlaceholders,
	)
	if err := scanCounts(ctx, r.runner, outQ, idArgs, result, func(c *types.DependencyCounts, n int) { c.DependencyCount = n }); err != nil {
		return nil, fmt.Errorf("db: DependencySQLRepository.CountsByIssueIDs (out): %w", err)
	}

	//nolint:gosec // G201: table and depTargetExpr are hardcoded
	inQ := fmt.Sprintf(
		`SELECT %s AS depends_on_id, COUNT(*) FROM %s WHERE %s IN (%s) AND type = 'blocks' GROUP BY %s`,
		depTargetExpr, table, depTargetExpr, idPlaceholders, depTargetExpr,
	)
	if err := scanCounts(ctx, r.runner, inQ, idArgs, result, func(c *types.DependencyCounts, n int) { c.DependentCount = n }); err != nil {
		return nil, fmt.Errorf("db: DependencySQLRepository.CountsByIssueIDs (in): %w", err)
	}

	return result, nil
}
