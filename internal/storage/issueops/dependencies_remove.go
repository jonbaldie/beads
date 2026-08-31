package issueops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage/sqlbuild"
	"github.com/jonbaldie/beads/internal/types"
)

// RemoveDependencyInTx removes a dependency between two issues within an
// existing transaction. Automatically routes to wisp_dependencies if the
// source issue is an active wisp. When emitEvent is set and a row is actually
// removed it records a dependency_removed event (attributed to actor) on the
// source's event table; a no-op remove of a missing edge, or a structural remove
// with emitEvent unset, records nothing. Only the explicit bd dep remove verb
// sets emitEvent; structural removals (issue delete, reparent, batch, duplicate
// cleanup) leave it unset so they wire edges away silently, mirroring the
// proxied repository's DepInsertOpts.EmitEvent gate so both backends record
// identical history.
//
// It returns whether a dependency_removed event was actually written, so callers
// that stage tables for a Dolt commit stage the events table only when an event
// row exists (avoiding the sweep-unrelated-rows hazard doltAddAndCommit guards
// against, GH#2455).
//
//nolint:gosec // G201: depTable from WispTableRouting (hardcoded constants)
func RemoveDependencyInTx(ctx context.Context, tx *sql.Tx, issueID, dependsOnID, actor string, emitEvent bool) (bool, error) {
	return removeDependencyInTx(ctx, tx, issueID, dependsOnID, actor, emitEvent, nil)
}

func removeDependencyInTx(ctx context.Context, tx DBTX, issueID, dependsOnID, actor string, emitEvent bool, recomputeResult *RecomputeIsBlockedResult) (bool, error) {
	isWisp := IsActiveWispInTx(ctx, tx, issueID)
	_, _, eventTable, depTable := WispTableRouting(isWisp)

	// Capture the row's type before deleting so we can dispatch the right
	// affected-set helper. If no row matches, treat as a no-op.
	var depType, depMetadata string
	row := tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT type, metadata FROM %s WHERE issue_id = ? AND %s = ?`, depTable, DepTargetExpr),
		issueID, dependsOnID)
	if err := row.Scan(&depType, &depMetadata); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("lookup dependency type for %s -> %s: %w", issueID, dependsOnID, err)
	}

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM %s WHERE issue_id = ? AND %s = ?`, depTable, DepTargetExpr),
		issueID, dependsOnID); err != nil {
		return false, fmt.Errorf("remove dependency: %w", err)
	}

	// The lookup above returned early when no row matched, so reaching here means
	// an edge was actually deleted. Record the dependency_removed event on the
	// source issue's event table for bd CLI / library history observers — but only
	// when emitEvent is set, so structural removes stay silent (parity with the
	// proxied repo and with the symmetric AddDependencyInTx EmitEvent gate).
	eventWritten := false
	if emitEvent {
		if err := RecordEventInTable(ctx, tx, eventTable, issueID, types.EventDependencyRemoved, actor,
			fmt.Sprintf("Removed dependency on %s", dependsOnID)); err != nil {
			return false, fmt.Errorf("record dependency_removed event: %w", err)
		}
		eventWritten = true
	}

	var affectedIssues, affectedWisps []string
	var aerr error
	if isWisp {
		affectedIssues, affectedWisps, aerr = AffectedByDepChangeForWispInTx(ctx, tx, issueID, dependsOnID, types.DependencyType(depType))
	} else {
		affectedIssues, affectedWisps, aerr = AffectedByDepChangeInTx(ctx, tx, issueID, dependsOnID, types.DependencyType(depType))
	}
	if aerr != nil {
		return false, fmt.Errorf("affected by remove dependency %s -> %s: %w", issueID, dependsOnID, aerr)
	}
	recomputed, err := RecomputeIsBlockedInTxWithResult(ctx, tx, affectedIssues, affectedWisps)
	if err != nil {
		return false, fmt.Errorf("recompute is_blocked after remove dependency %s -> %s: %w", issueID, dependsOnID, err)
	}
	mergeRecomputeIsBlockedResult(recomputeResult, recomputed)
	// Snapshot only after all derived blocked-state maintenance has completed.
	// Never gated on emitEvent — a structural removal is as real to a replaying
	// consumer as one from an explicit dep verb.
	return eventWritten, RecordDepEventInTx(ctx, tx, EventDepRemove, issueID, depType, dependsOnID, depMetadata)
}

func mergeRecomputeIsBlockedResult(target *RecomputeIsBlockedResult, source RecomputeIsBlockedResult) {
	if target == nil {
		return
	}
	target.IssueRowsChanged = target.IssueRowsChanged || source.IssueRowsChanged
	target.WispRowsChanged = target.WispRowsChanged || source.WispRowsChanged
}

// GetIssuesByIDsInTx retrieves multiple issues by ID within an existing
// transaction, including labels. Automatically routes each ID to the correct
// table (issues/wisps). Uses batched IN clauses.
//
// wispSet is an optional pre-built set of active wisp IDs scoped to
// cover ids (see WispIDSetInTx). Pass nil to have the helper build
// a scoped set internally; callers hydrating multiple batches inside
// one tx can build the set once over the union of their IDs and
// reuse it across calls.
//
//nolint:gosec // G201: table names come from WispTableRouting (hardcoded constants)
func GetIssuesByIDsInTx(ctx context.Context, tx DBTX, ids []string, wispSet map[string]struct{}) ([]*types.Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	if wispSet == nil {
		var err error
		wispSet, err = WispIDSetInTx(ctx, tx, ids)
		if err != nil {
			return nil, fmt.Errorf("get issues by IDs: build wisp set: %w", err)
		}
	}

	// Partition IDs by wisp status.
	wispIDs, permIDs := partitionByWispSet(ids, wispSet)

	permIssues, err := getIssuesByTableIDsInTx(ctx, tx, "issues", "labels", permIDs)
	if err != nil {
		return nil, err
	}
	wispIssues, err := getIssuesByTableIDsInTx(ctx, tx, "wisps", "wisp_labels", wispIDs)
	if err != nil {
		return nil, err
	}

	allIssues := make([]*types.Issue, 0, len(permIssues)+len(wispIssues))
	allIssues = append(allIssues, permIssues...)
	allIssues = append(allIssues, wispIssues...)
	return allIssues, nil
}

//nolint:gosec // G201: table and label table are selected from fixed constants.
func getIssuesByTableIDsInTx(ctx context.Context, tx DBTX, table, labelTable string, ids []string) ([]*types.Issue, error) {
	var allIssues []*types.Issue
	total := len(ids)
	for start := 0; start < total; start += queryBatchSize {
		end := start + queryBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}
		inClause := strings.Join(placeholders, ",")
		issues, issueMap, err := readIssuesByTableBatch(ctx, tx, table, inClause, args)
		if err != nil {
			return nil, err
		}
		if err := hydrateIssueLabelsInTx(ctx, tx, labelTable, inClause, args, issueMap); err != nil {
			return nil, err
		}
		allIssues = append(allIssues, issues...)
	}
	return allIssues, nil
}

//nolint:gosec // G201: table is selected from fixed constants and inClause has only placeholders.
func readIssuesByTableBatch(ctx context.Context, tx DBTX, table, inClause string, args []any) ([]*types.Issue, map[string]*types.Issue, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(
		`SELECT %s FROM %s %s WHERE id IN (%s)`,
		IssueSelectColumns, table, sqlbuild.LeaseJoin(table), inClause), args...)
	if err != nil {
		return nil, nil, fmt.Errorf("get issues by IDs from %s: %w", table, err)
	}
	issues := make([]*types.Issue, 0)
	issueMap := make(map[string]*types.Issue)
	for rows.Next() {
		issue, scanErr := ScanIssueFrom(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("get issues by IDs: scan: %w", scanErr)
		}
		issues = append(issues, issue)
		issueMap[issue.ID] = issue
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("get issues by IDs: rows: %w", err)
	}
	return issues, issueMap, nil
}

//nolint:gosec // G201: labelTable is selected from fixed constants and inClause has only placeholders.
func hydrateIssueLabelsInTx(ctx context.Context, tx DBTX, labelTable, inClause string, args []any, issueMap map[string]*types.Issue) error {
	if len(issueMap) == 0 {
		return nil
	}
	labelRows, err := tx.QueryContext(ctx, fmt.Sprintf(
		`SELECT issue_id, label FROM %s WHERE issue_id IN (%s) ORDER BY issue_id, label`,
		labelTable, inClause), args...)
	if err != nil {
		return fmt.Errorf("get issues by IDs: labels from %s: %w", labelTable, err)
	}
	for labelRows.Next() {
		var issueID, label string
		if scanErr := labelRows.Scan(&issueID, &label); scanErr != nil {
			_ = labelRows.Close()
			return fmt.Errorf("get issues by IDs: scan label: %w", scanErr)
		}
		if issue, ok := issueMap[issueID]; ok {
			issue.Labels = append(issue.Labels, label)
		}
	}
	_ = labelRows.Close()
	if err := labelRows.Err(); err != nil {
		return fmt.Errorf("get issues by IDs: label rows: %w", err)
	}
	return nil
}

// GetDependenciesWithMetadataInTx returns issues that the given issueID depends on,
// along with the dependency type. Works within an existing transaction.
// Queries both dependency tables to handle cross-table dependencies.
//
//nolint:gosec // G201: table names come from hardcoded constants
func GetDependenciesWithMetadataInTx(ctx context.Context, tx DBTX, issueID string) ([]*types.IssueWithDependencyMetadata, error) {
	return getIssuesWithDependencyMetadataInTx(ctx, tx, issueID, false)
}

// GetDependentsWithMetadataInTx returns issues that depend on the given issueID
// along with the dependency type. Works within an existing transaction.
//
//nolint:gosec // G201: table names come from WispTableRouting (hardcoded constants)
func GetDependentsWithMetadataInTx(ctx context.Context, tx DBTX, issueID string) ([]*types.IssueWithDependencyMetadata, error) {
	return getIssuesWithDependencyMetadataInTx(ctx, tx, issueID, true)
}

type dependencyMetadata struct {
	depID, depType string
}

func getIssuesWithDependencyMetadataInTx(ctx context.Context, tx DBTX, issueID string, dependents bool) ([]*types.IssueWithDependencyMetadata, error) {
	deps, err := queryDependencyMetadataInTx(ctx, tx, issueID, dependents)
	if err != nil {
		return nil, err
	}
	if len(deps) == 0 {
		return nil, nil
	}

	ids := make([]string, len(deps))
	for i, dep := range deps {
		ids[i] = dep.depID
	}
	issues, err := GetIssuesByIDsInTx(ctx, tx, ids, nil)
	if err != nil {
		kind := "dependencies"
		if dependents {
			kind = "dependents"
		}
		return nil, fmt.Errorf("get %s: fetch issues: %w", kind, err)
	}
	issueMap := make(map[string]*types.Issue, len(issues))
	for _, issue := range issues {
		issueMap[issue.ID] = issue
	}

	results := make([]*types.IssueWithDependencyMetadata, 0, len(deps))
	for _, dep := range deps {
		issue, ok := issueMap[dep.depID]
		if !ok {
			continue
		}
		results = append(results, &types.IssueWithDependencyMetadata{
			Issue:          *issue,
			DependencyType: types.DependencyType(dep.depType),
		})
	}
	return results, nil
}

//nolint:gosec // G201: dependency tables are fixed and the target expression is static.
func queryDependencyMetadataInTx(ctx context.Context, tx DBTX, issueID string, dependents bool) ([]dependencyMetadata, error) {
	kind := "dependencies"
	selectSQL := fmt.Sprintf("%s AS depends_on_id", DepTargetExpr)
	whereSQL := "issue_id = ?"
	if dependents {
		kind = "dependents"
		selectSQL = "issue_id"
		whereSQL = DepTargetExpr + " = ?"
	}

	var deps []dependencyMetadata
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(
			`SELECT %s, type FROM %s WHERE %s`, selectSQL, depTable, whereSQL), issueID)
		if err != nil {
			return nil, fmt.Errorf("get %s from %s: %w", kind, depTable, err)
		}
		for rows.Next() {
			var dep dependencyMetadata
			if scanErr := rows.Scan(&dep.depID, &dep.depType); scanErr != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("get %s: scan: %w", kind, scanErr)
			}
			deps = append(deps, dep)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get %s: rows from %s: %w", kind, depTable, err)
		}
	}
	return deps, nil
}

// GetDependenciesInTx returns issues that the given issueID depends on.
// Queries both dependencies and wisp_dependencies tables.
//
//nolint:gosec // G201: table names come from hardcoded constants
func GetDependenciesInTx(ctx context.Context, tx *sql.Tx, issueID string) ([]*types.Issue, error) {
	var ids []string
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(
			`SELECT %s AS depends_on_id FROM %s WHERE issue_id = ?`, DepTargetExpr, depTable), issueID)
		if err != nil {
			return nil, fmt.Errorf("get dependencies from %s: %w", depTable, err)
		}
		for rows.Next() {
			var id string
			if scanErr := rows.Scan(&id); scanErr != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("get dependencies: scan: %w", scanErr)
			}
			ids = append(ids, id)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get dependencies: rows from %s: %w", depTable, err)
		}
	}

	if len(ids) == 0 {
		return nil, nil
	}

	return GetIssuesByIDsInTx(ctx, tx, ids, nil)
}

// GetDependentsInTx returns issues that depend on the given issueID.
// Queries both dependencies and wisp_dependencies tables.
//
//nolint:gosec // G201: table names come from hardcoded constants
func GetDependentsInTx(ctx context.Context, tx *sql.Tx, issueID string) ([]*types.Issue, error) {
	var ids []string
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(
			`SELECT issue_id FROM %s WHERE %s = ?`, depTable, DepTargetExpr), issueID)
		if err != nil {
			return nil, fmt.Errorf("get dependents from %s: %w", depTable, err)
		}
		for rows.Next() {
			var id string
			if scanErr := rows.Scan(&id); scanErr != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("get dependents: scan: %w", scanErr)
			}
			ids = append(ids, id)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get dependents: rows from %s: %w", depTable, err)
		}
	}

	if len(ids) == 0 {
		return nil, nil
	}

	return GetIssuesByIDsInTx(ctx, tx, ids, nil)
}
