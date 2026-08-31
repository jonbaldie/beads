package issueops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage/depid"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/types"
)

// AddDependencyInTx validates and inserts a dependency within an existing
// transaction. It handles:
//   - Wisp routing (auto-detected or caller-provided)
//   - Source/target existence validation
//   - Hierarchy deadlock validation for blocking deps (GH#1495, bd-wg7ve)
//   - Cycle detection via recursive CTE across both dependency tables
//   - Idempotent same-type updates (metadata only)
//   - Type conflict detection
//
// The caller is responsible for transaction lifecycle, dolt commits, and
// any cache invalidation.
//
// It returns whether a dependency_added event was actually written — true only
// on the genuine new-edge path with opts.EmitEvent set, false for an idempotent
// same-type re-add or a silent structural add. Callers that stage tables for a
// Dolt commit stage the events table only when an event row exists, so a
// no-event add cannot sweep unrelated pending rows into the commit (GH#2455).
func AddDependencyInTx(ctx context.Context, tx DBTX, dep *types.Dependency, actor string, opts AddDependencyOpts) (bool, error) {
	return addDependencyInTx(ctx, tx, dep, actor, opts, nil)
}

func resolveAddDependencyTables(ctx context.Context, tx DBTX, dep *types.Dependency, opts AddDependencyOpts) (sourceTable, targetTable, writeTable string, depTables []string) {
	sourceTable, writeTable = resolveAddDependencySourceTables(ctx, tx, dep, opts)
	targetTable = resolveAddDependencyTargetTable(ctx, tx, dep, opts)

	depTables = opts.DepTables
	if len(depTables) == 0 {
		depTables = cycleDetectionTables()
	}
	return sourceTable, targetTable, writeTable, depTables
}

func resolveAddDependencySourceTables(ctx context.Context, tx DBTX, dep *types.Dependency, opts AddDependencyOpts) (sourceTable, writeTable string) {
	sourceTable = opts.SourceTable
	writeTable = opts.WriteTable
	if sourceTable != "" && writeTable != "" {
		return sourceTable, writeTable
	}
	sourceIsWisp := IsActiveWispInTx(ctx, tx, dep.IssueID)
	st, _, _, dt := WispTableRouting(sourceIsWisp)
	if sourceTable == "" {
		sourceTable = st
	}
	if writeTable == "" {
		writeTable = dt
	}
	return sourceTable, writeTable
}

func resolveAddDependencyTargetTable(ctx context.Context, tx DBTX, dep *types.Dependency, opts AddDependencyOpts) string {
	if opts.TargetTable != "" {
		return opts.TargetTable
	}
	if strings.HasPrefix(dep.DependsOnID, "external:") || opts.IsCrossPrefix {
		return "issues"
	}
	targetIsWisp := IsActiveWispInTx(ctx, tx, dep.DependsOnID)
	targetTable, _, _, _ := WispTableRouting(targetIsWisp)
	return targetTable
}

func dependencyMetadataValue(dep *types.Dependency) string {
	if dep.Metadata == "" {
		return "{}"
	}
	return dep.Metadata
}

//nolint:gosec // G201: sourceTable is selected from the fixed issue tables.
func validateDependencySource(ctx context.Context, tx DBTX, sourceTable string, dep *types.Dependency) error {
	var sourceType string
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT issue_type FROM %s WHERE id = ?`, sourceTable), dep.IssueID).Scan(&sourceType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MissingDependencySource(dep.IssueID, dep.DependsOnID)
		}
		return fmt.Errorf("failed to check issue existence: %w", err)
	}
	return nil
}

//nolint:gosec // G201: targetTable is selected from the fixed issue tables.
func validateDependencyTarget(ctx context.Context, tx DBTX, targetTable string, dep *types.Dependency, opts AddDependencyOpts) (string, error) {
	if opts.PrecheckedTarget != nil {
		return opts.PrecheckedTarget.IssueType, nil
	}
	if strings.HasPrefix(dep.DependsOnID, "external:") || opts.IsCrossPrefix {
		return "", nil
	}

	var targetType string
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT issue_type FROM %s WHERE id = ?`, targetTable), dep.DependsOnID).Scan(&targetType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", MissingDependencyTarget(dep.IssueID, dep.DependsOnID)
		}
		return "", fmt.Errorf("failed to check target issue existence: %w", err)
	}
	return targetType, nil
}

func validateAndClassifyDependencyAdd(ctx context.Context, tx DBTX, sourceTable, targetTable string, depTables []string, dep *types.Dependency, opts AddDependencyOpts) (DepTargetKind, error) {
	if err := validateDependencySource(ctx, tx, sourceTable, dep); err != nil {
		return DepTargetIssue, err
	}
	targetType, err := validateDependencyTarget(ctx, tx, targetTable, dep, opts)
	if err != nil {
		return DepTargetIssue, err
	}
	if targetType != "" {
		if err := CheckBlockingHierarchyInTx(ctx, tx, dep, depTables); err != nil {
			return DepTargetIssue, err
		}
	}
	if !opts.SkipCycleCheck {
		if err := CheckDependencyCycleInTx(ctx, tx, dep, depTables); err != nil {
			return DepTargetIssue, err
		}
	}
	if opts.TargetKind != nil {
		return *opts.TargetKind, nil
	}
	return ClassifyDepTarget(ctx, tx, dep, opts.IsCrossPrefix), nil
}

//nolint:gosec // G201: writeTable is selected from the fixed dependency tables.
func updateExistingDependencyInTx(ctx context.Context, tx DBTX, writeTable string, dep *types.Dependency, metadata string) (existing bool, err error) {
	var existingType string
	err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT type FROM %s WHERE issue_id = ? AND %s`, writeTable, depTargetEquals("")),
		dep.IssueID, dep.DependsOnID).Scan(&existingType)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to check existing dependency: %w", err)
	}
	if existingType != string(dep.Type) {
		return true, &domain.DependencyTypeConflictError{
			IssueID:       dep.IssueID,
			DependsOnID:   dep.DependsOnID,
			ExistingType:  existingType,
			RequestedType: string(dep.Type),
		}
	}

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET metadata = ? WHERE issue_id = ? AND %s`, writeTable, depTargetEquals("")),
		metadata, dep.IssueID, dep.DependsOnID); err != nil {
		return true, fmt.Errorf("failed to update dependency metadata: %w", err)
	}
	return true, RecordDepEventInTx(ctx, tx, EventDepAdd, dep.IssueID, string(dep.Type), dep.DependsOnID, metadata)
}

func addDependencyInTx(ctx context.Context, tx DBTX, dep *types.Dependency, actor string, opts AddDependencyOpts, recomputeResult *RecomputeIsBlockedResult) (bool, error) {
	sourceTable, targetTable, writeTable, depTables := resolveAddDependencyTables(ctx, tx, dep, opts)
	metadata := dependencyMetadataValue(dep)

	kind, err := validateAndClassifyDependencyAdd(ctx, tx, sourceTable, targetTable, depTables, dep, opts)
	if err != nil {
		return false, err
	}
	targetCol := kind.Column()

	existing, err := updateExistingDependencyInTx(ctx, tx, writeTable, dep, metadata)
	if err != nil {
		return false, err
	}
	if existing {
		return false, nil
	}

	if err := insertDependencyEdgeInTx(ctx, tx, writeTable, targetCol, dep, actor, metadata); err != nil {
		return false, err
	}

	srcIsWisp := writeTable == "wisp_dependencies"
	eventWritten, err := recordDependencyAddedEventInTx(ctx, tx, dep, actor, srcIsWisp, opts.EmitEvent)
	if err != nil {
		return false, err
	}
	return finalizeAddedDependencyInTx(ctx, tx, dep, kind, opts.PrecheckedTarget, srcIsWisp, eventWritten, metadata, recomputeResult)
}

//nolint:gosec // G201: writeTable and targetCol are fixed routing values.
func insertDependencyEdgeInTx(ctx context.Context, tx DBTX, writeTable, targetCol string, dep *types.Dependency, actor, metadata string) error {
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, issue_id, %s, type, created_at, created_by, metadata, thread_id)
		VALUES (?, ?, ?, ?, UTC_TIMESTAMP(), ?, ?, ?)
	`, writeTable, targetCol), depid.New(dep.IssueID, dep.DependsOnID), dep.IssueID, dep.DependsOnID, dep.Type, actor, metadata, dep.ThreadID); err != nil {
		return fmt.Errorf("failed to add dependency: %w", err)
	}
	if dep.Type == types.DepParentChild {
		if err := TouchDependencyCoordinationTableInTx(ctx, tx, dep.DependsOnID, writeTable); err != nil {
			return err
		}
	}
	return nil
}

func recordDependencyAddedEventInTx(ctx context.Context, tx DBTX, dep *types.Dependency, actor string, srcIsWisp, emitEvent bool) (bool, error) {
	if !emitEvent {
		return false, nil
	}
	_, _, eventTable, _ := WispTableRouting(srcIsWisp)
	if err := RecordEventInTable(ctx, tx, eventTable, dep.IssueID, types.EventDependencyAdded, actor,
		fmt.Sprintf("Added dependency: %s %s %s", dep.IssueID, dep.Type, dep.DependsOnID)); err != nil {
		return false, fmt.Errorf("record dependency_added event: %w", err)
	}
	return true, nil
}

func finalizeAddedDependencyInTx(ctx context.Context, tx DBTX, dep *types.Dependency, kind DepTargetKind, precheckedTarget *DepTargetPrecheck, srcIsWisp, eventWritten bool, metadata string, recomputeResult *RecomputeIsBlockedResult) (bool, error) {
	affectedIssues, affectedWisps, err := affectedByDependencyAdd(ctx, tx, dep, srcIsWisp)
	if err != nil {
		return false, err
	}
	if dep.Type == types.DepBlocks || dep.Type == types.DepConditionalBlocks {
		if err := markDirectBlockingDependencySourceInTx(ctx, tx, dep.IssueID, srcIsWisp, dep.DependsOnID, kind, precheckedTarget); err != nil {
			return false, fmt.Errorf("mark direct is_blocked after add dependency %s -> %s: %w", dep.IssueID, dep.DependsOnID, err)
		}
		affectedIssues, affectedWisps = RemoveSourceFromAffected(dep.IssueID, srcIsWisp, affectedIssues, affectedWisps)
	}
	if dep.Type == types.DepParentChild {
		// Parent-child adds are not monotonic: adding an already-closed child can
		// satisfy an any-children waits-for gate and unblock the waiter.
		recomputed, err := RecomputeIsBlockedInTxWithResult(ctx, tx, affectedIssues, affectedWisps)
		if err != nil {
			return false, fmt.Errorf("recompute is_blocked after add dependency %s -> %s: %w", dep.IssueID, dep.DependsOnID, err)
		}
		mergeRecomputeIsBlockedResult(recomputeResult, recomputed)
		return eventWritten, RecordDepEventInTx(ctx, tx, EventDepAdd, dep.IssueID, string(dep.Type), dep.DependsOnID, metadata)
	}
	if err := MarkIsBlockedInTx(ctx, tx, affectedIssues, affectedWisps); err != nil {
		return false, fmt.Errorf("mark is_blocked after add dependency %s -> %s: %w", dep.IssueID, dep.DependsOnID, err)
	}
	return eventWritten, RecordDepEventInTx(ctx, tx, EventDepAdd, dep.IssueID, string(dep.Type), dep.DependsOnID, metadata)
}

func affectedByDependencyAdd(ctx context.Context, tx DBTX, dep *types.Dependency, srcIsWisp bool) ([]string, []string, error) {
	var issueIDs, wispIDs []string
	var err error
	if srcIsWisp {
		issueIDs, wispIDs, err = AffectedByDepChangeForWispInTx(ctx, tx, dep.IssueID, dep.DependsOnID, dep.Type)
	} else {
		issueIDs, wispIDs, err = AffectedByDepChangeInTx(ctx, tx, dep.IssueID, dep.DependsOnID, dep.Type)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("affected by add dependency %s -> %s: %w", dep.IssueID, dep.DependsOnID, err)
	}
	return issueIDs, wispIDs, nil
}

// RemoveSourceFromAffected drops the dep source from the affected-ID sets
// after a direct is_blocked mark, so the follow-up Mark/Recompute pass does
// not redo it. Shared with the domain/db dependency repository.
func RemoveSourceFromAffected(source string, srcIsWisp bool, issueIDs, wispIDs []string) ([]string, []string) {
	if srcIsWisp {
		return issueIDs, removeID(wispIDs, source)
	}
	return removeID(issueIDs, source), wispIDs
}

func removeID(ids []string, remove string) []string {
	if len(ids) == 0 {
		return ids
	}
	out := ids[:0]
	for _, id := range ids {
		if id != remove {
			out = append(out, id)
		}
	}
	return out
}

//nolint:gosec // G201: table names are selected from fixed issue/wisp tables.
func markDirectBlockingDependencySourceInTx(ctx context.Context, tx DBTX, source string, srcIsWisp bool, target string, targetKind DepTargetKind, precheckedTarget *DepTargetPrecheck) error {
	sourceTable := "issues"
	if srcIsWisp {
		sourceTable = "wisps"
	}
	targetTable := ""
	switch targetKind {
	case DepTargetIssue:
		targetTable = "issues"
	case DepTargetWisp:
		targetTable = "wisps"
	default:
		return nil
	}

	if precheckedTarget != nil {
		// The target row lives on another session; the EXISTS gate below
		// would miss it there. Its openness is already known, so gate in Go
		// and mark the source (whose table always matches tx) directly.
		if types.Status(precheckedTarget.Status) == types.StatusClosed || types.Status(precheckedTarget.Status) == types.StatusPinned {
			return nil
		}
		_, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE %s s SET s.is_blocked = 1, s.updated_at = s.updated_at
			WHERE s.id = ?
			  AND s.is_blocked = 0
			  AND s.status <> 'closed' AND s.status <> 'pinned'
		`, sourceTable), source)
		return err
	}

	// MySQL 8.0+ rejects UPDATE <T> ... WHERE EXISTS (SELECT FROM <T> ...)
	// even with distinct aliases (Error 1093: can't specify target table
	// for update in FROM clause), which fires here whenever sourceTable ==
	// targetTable. Wrapping the inner SELECT in a derived table makes MySQL
	// treat it as an independent rowset, satisfying the restriction. Dolt
	// accepts both forms, so this is a no-op there.
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %s s SET s.is_blocked = 1, s.updated_at = s.updated_at
		WHERE s.id = ?
		  AND s.is_blocked = 0
		  AND s.status <> 'closed' AND s.status <> 'pinned'
		  AND EXISTS (
		    SELECT 1 FROM (
		      SELECT id, status FROM %s WHERE id = ?
		    ) AS t
		    WHERE t.status <> 'closed' AND t.status <> 'pinned'
		  )
	`, sourceTable, targetTable), source, target)
	return err
}
