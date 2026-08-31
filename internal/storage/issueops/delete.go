package issueops

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/workapi"
	publicops "github.com/jonbaldie/beads/issueops"
)

// deleteBatchSize controls the maximum number of IDs per IN-clause query
// for delete operations. Kept small to avoid large IN-clause queries.
const deleteBatchSize = 50

// maxRecursiveResults is the safety limit for the total number of issues
// discovered during recursive dependent traversal.
const maxRecursiveResults = 10000

//nolint:gosec // G201: table names come from WispTableRouting (hardcoded constants)
func DeleteIssueInTx(ctx context.Context, tx *sql.Tx, id string) error {
	isWisp := IsActiveWispInTx(ctx, tx, id)

	var deletedIssues, deletedWisps []string
	if isWisp {
		deletedWisps = []string{id}
	} else {
		deletedIssues = []string{id}
	}
	affectedIssues, affectedWisps, aerr := AffectedByDeletionInTx(ctx, tx, deletedIssues, deletedWisps)
	if aerr != nil {
		return fmt.Errorf("affected by delete for %s: %w", id, aerr)
	}

	// Edges are journaled before the rows go, while their source snapshots can
	// still be read.
	if err := RecordDependencyRemovalsForIssuesInTx(ctx, tx, []string{id}); err != nil {
		return fmt.Errorf("journal dependency removals for %s: %w", id, err)
	}
	if err := deleteIssueRowInTx(ctx, tx, id, isWisp); err != nil {
		return err
	}

	if err := RecomputeIsBlockedInTx(ctx, tx, affectedIssues, affectedWisps); err != nil {
		return fmt.Errorf("recompute is_blocked after delete for %s: %w", id, err)
	}

	return nil
}

//nolint:gosec // G201: table names come from WispTableRouting (hardcoded constants)
func deleteIssueRowInTx(ctx context.Context, tx *sql.Tx, id string, isWisp bool) error {
	issueTable, _, _, _ := WispTableRouting(isWisp)
	result, err := tx.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE id = ?", issueTable), id)
	if err != nil {
		return fmt.Errorf("delete issue from %s: %w", issueTable, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("get rows affected: %w", err)
	}
	if rows == 0 {
		// Wrap the sentinel so callers can errors.Is(..., storage.ErrNotFound),
		// matching GetIssue/UpdateIssue. The storage conformance suite asserts
		// this parity across not-found paths.
		return fmt.Errorf("%w: issue %s", storage.ErrNotFound, id)
	}
	// Journal the delete in the same transaction. This worker backs single
	// deletes (DeleteIssueInTx) and the per-wisp branch of the bulk delete
	// (DeleteResolvedSetInTx); the bulk regular-issue branch journals its own
	// ids directly. The rows==0 return above is what keeps this
	// actually-deleted-only.
	if err := RecordDeleteInTx(ctx, tx, id); err != nil {
		return err
	}
	if isWisp {
		if err := DeleteWispFromDependenciesInTx(ctx, tx, id); err != nil {
			return err
		}
	} else if err := DeleteLeaseInTx(ctx, tx, id); err != nil {
		// A deleted issue holds no lease.
		return err
	}
	return nil
}

// DeletionSet is the EXACT set of rows one delete removes, split by the plane
// each row lives in.
//
// IT EXISTS BECAUSE THE SET USED TO BE COMPUTED TWICE, from different roots, so
// `bd delete <wisp> --cascade` left a durable row reachable only through that
// wisp alive and then rewrote its neighbors' text to say `[deleted:<id>]` about
// it. The neighborhood read, the deletion and the reference rewrite all take
// THIS value.
type DeletionSet struct {
	// WispIDs and RegularIDs partition All by plane. The two tiers are deleted
	// through different tables and their associated rows counted from
	// different ones, so the split is carried rather than recomputed.
	WispIDs    []string
	RegularIDs []string
	// All is the whole set in one slice — what a caller scopes a neighborhood
	// read or a citation rewrite to, and what nothing else may recompute.
	All []string
}

// ResolveDeletionSetInTx decides WHICH rows a delete removes: the named ids,
// plus — under cascade — the transitive closure of everything that depends on
// them, in BOTH planes.
//
// THE CASCADE IS ROOTED AT EVERY NAMED ID, WISPS INCLUDED. Rooting it at the
// durable half is what made `bd wisp gc` (which hardcodes cascade) silently
// under-delete. It does not read the caller's slice destructively either: the
// non-cascade set is a copy, because DeleteRequest promises IDs is never
// sorted in place.
func ResolveDeletionSetInTx(ctx context.Context, tx DBTX, ids []string, cascade bool) (DeletionSet, error) {
	all := append([]string(nil), ids...)
	if cascade {
		closure, err := FindAllDependentsInTx(ctx, tx, ids)
		if err != nil {
			return DeletionSet{}, fmt.Errorf("expand cascade: %w", err)
		}
		all = workapi.SortedDeleteIDs(closure)
	}
	if len(all) == 0 {
		return DeletionSet{}, nil
	}
	wispIDs, regularIDs, err := PartitionWispIDsInTx(ctx, tx, all)
	if err != nil {
		return DeletionSet{}, fmt.Errorf("partition delete ids: %w", err)
	}
	return DeletionSet{WispIDs: wispIDs, RegularIDs: regularIDs, All: all}, nil
}

func DeleteIssuesInTx(ctx context.Context, tx *sql.Tx, ids []string, cascade bool, force bool, dryRun bool) (*types.DeleteIssuesResult, error) {
	if len(ids) == 0 {
		return &types.DeleteIssuesResult{}, nil
	}

	set, err := ResolveDeletionSetInTx(ctx, tx, ids, cascade)
	if err != nil {
		return nil, err
	}

	orphaned, guardResult, err := nonCascadeOrphans(ctx, tx, set, ids, cascade, force)
	if err != nil {
		return guardResult, err
	}
	result, err := DeleteResolvedSetInTx(ctx, tx, set, dryRun)
	if err != nil {
		return nil, err
	}
	result.OrphanedIssues = orphaned
	return result, nil
}

func nonCascadeOrphans(ctx context.Context, tx *sql.Tx, set DeletionSet, ids []string, cascade, force bool) ([]string, *types.DeleteIssuesResult, error) {
	if cascade {
		return nil, nil, nil
	}
	// The guard here is the STORAGE SEAM's, and it stays durable-only: the
	// server-backed store peels wisps off before it ever reaches this
	// function (dolt/issues.go DeleteIssues), so widening it would make the
	// embedded store refuse where the server-backed one cannot. The ROLE's
	// guard, which does cover both planes, is in DeleteInTx.
	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	// One scan of the dependency planes answers both modes: the guard needs
	// to know WHICH id is blocked, and the forced path needs the union of
	// what it orphans.
	external, err := ExternalDependentsBySourceInTx(ctx, tx, set.RegularIDs, idSet)
	if err != nil {
		return nil, nil, fmt.Errorf("get dependents: %w", err)
	}
	if !force {
		for _, id := range set.RegularIDs {
			if deps := external[id]; len(deps) > 0 {
				return nil, &types.DeleteIssuesResult{OrphanedIssues: deps},
					&publicops.DependentsOutsideRequestError{IssueID: id, Dependents: deps}
			}
		}
	}
	orphans := make(map[string]bool)
	for _, deps := range external {
		for _, id := range deps {
			orphans[id] = true
		}
	}
	return workapi.SortedDeleteIDs(orphans), nil, nil
}

// DeleteResolvedSetInTx deletes EXACTLY set — no expansion, no re-partition,
// no guard — and reports the associated rows that went with it.
//
// It is split out of DeleteIssuesInTx so the role body can read the
// neighborhood BEFORE the delete and rewrite it AFTER against the SAME
// DeletionSet the delete was handed.
//
//nolint:gosec // G201: inClause contains only ? placeholders
func DeleteResolvedSetInTx(ctx context.Context, tx *sql.Tx, set DeletionSet, dryRun bool) (*types.DeleteIssuesResult, error) {
	result := &types.DeleteIssuesResult{}
	if len(set.All) == 0 {
		return result, nil
	}

	deletedSet := make(map[string]bool, len(set.All))
	for _, id := range set.All {
		deletedSet[id] = true
	}

	depsCount, labelsCount, eventsCount, err := countDeletionRows(ctx, tx, set, deletedSet)
	if err != nil {
		return nil, err
	}
	result.DependenciesCount = depsCount
	result.LabelsCount = labelsCount
	result.EventsCount = eventsCount
	result.DeletedCount = len(set.RegularIDs) + len(set.WispIDs)

	if dryRun {
		return result, nil
	}
	return executeResolvedDeletion(ctx, tx, set, result)
}

func executeResolvedDeletion(ctx context.Context, tx *sql.Tx, set DeletionSet, result *types.DeleteIssuesResult) (*types.DeleteIssuesResult, error) {
	affectedIssues, affectedWisps, aerr := AffectedByDeletionInTx(ctx, tx, set.RegularIDs, set.WispIDs)
	if aerr != nil {
		return nil, fmt.Errorf("affected by batch delete: %w", aerr)
	}

	// Resolve WHICH regular ids this delete actually removes before the batched
	// DELETE runs: afterwards the rows are gone, and RowsAffected reports a
	// count, not a set. A journal record for an id that was already absent would
	// tell a consumer to drop a bead this transaction never touched.
	journaledDeletes, err := journalableDeletesInTx(ctx, tx, "issues", set.RegularIDs)
	if err != nil {
		return nil, err
	}
	// Edges are journaled before the rows go, while their source snapshots can
	// still be read.
	if err := RecordDependencyRemovalsForIssuesInTx(ctx, tx, set.All); err != nil {
		return nil, fmt.Errorf("journal dependency removals for batch delete: %w", err)
	}

	if err := deleteWispRows(ctx, tx, set.WispIDs); err != nil {
		return nil, err
	}

	totalRegularsDeleted, err := deleteRegularRows(ctx, tx, set.RegularIDs)
	if err != nil {
		return nil, err
	}
	result.DeletedCount = totalRegularsDeleted + len(set.WispIDs)

	// Journal every regular issue this bulk/cascade delete removed. Wisps went
	// through deleteIssueRowInTx above, which journals each itself; set.All is
	// cascade-expanded, so this records cascade deletes too.
	if err := journalDeletedIssues(ctx, tx, journaledDeletes); err != nil {
		return nil, err
	}

	if err := RecomputeIsBlockedInTx(ctx, tx, affectedIssues, affectedWisps); err != nil {
		return nil, fmt.Errorf("recompute is_blocked after batch delete: %w", err)
	}

	return result, nil
}

func countDeletionRows(ctx context.Context, tx *sql.Tx, set DeletionSet, deletedSet map[string]bool) (int, int, int, error) {
	depsCount, err := CountRowsForIssueIDsInTx(ctx, tx, "dependencies", set.RegularIDs)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("count dependencies: %w", err)
	}
	wispDepsCount, err := CountRowsForIssueIDsInTx(ctx, tx, "wisp_dependencies", set.WispIDs)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("count wisp dependencies: %w", err)
	}
	depsCount += wispDepsCount
	inboundCount, err := countInboundDependencies(ctx, tx, set.All, deletedSet)
	if err != nil {
		return 0, 0, 0, err
	}
	depsCount += inboundCount

	labelsCount, err := CountRowsForIssueIDsInTx(ctx, tx, "labels", set.RegularIDs)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("count labels: %w", err)
	}
	wispLabelsCount, err := CountRowsForIssueIDsInTx(ctx, tx, "wisp_labels", set.WispIDs)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("count wisp labels: %w", err)
	}
	labelsCount += wispLabelsCount

	eventsCount, err := CountRowsForIssueIDsInTx(ctx, tx, "events", set.RegularIDs)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("count events: %w", err)
	}
	wispEventsCount, err := CountRowsForIssueIDsInTx(ctx, tx, "wisp_events", set.WispIDs)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("count wisp events: %w", err)
	}
	eventsCount += wispEventsCount
	return depsCount, labelsCount, eventsCount, nil
}

func countInboundDependencies(ctx context.Context, tx *sql.Tx, issueIDs []string, deletedSet map[string]bool) (int, error) {
	count := 0
	total := len(issueIDs)
	for i := 0; i < total; i += deleteBatchSize {
		end := i + deleteBatchSize
		if end > total {
			end = total
		}
		batchCount, err := countInboundDependencyBatch(ctx, tx, issueIDs[i:end], deletedSet)
		if err != nil {
			return 0, err
		}
		count += batchCount
	}
	return count, nil
}

func countInboundDependencyBatch(ctx context.Context, tx *sql.Tx, issueIDs []string, deletedSet map[string]bool) (int, error) {
	batchInClause, batchArgs := buildSQLInClause(issueIDs)
	count := 0
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		inboundIDs, err := readInboundDependencyIDs(ctx, tx, depTable, batchInClause, batchArgs)
		if err != nil {
			if optionalBlockedTable(depTable) && isTableNotExistError(err) {
				continue
			}
			return 0, fmt.Errorf("count inbound dependencies from %s: %w", depTable, err)
		}
		for _, issueID := range inboundIDs {
			if !deletedSet[issueID] {
				count++
			}
		}
	}
	return count, nil
}

//nolint:gosec // G201: depTable is hardcoded and the IN clause contains only ?.
func readInboundDependencyIDs(ctx context.Context, tx *sql.Tx, depTable, inClause string, args []interface{}) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		fmt.Sprintf(`SELECT issue_id FROM %s WHERE %s`, depTable, depTargetIn("", inClause)),
		args...)
	if err != nil {
		return nil, err
	}
	var issueIDs []string
	for rows.Next() {
		var issueID string
		if err := rows.Scan(&issueID); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan inbound dependency: %w", err)
		}
		issueIDs = append(issueIDs, issueID)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate inbound dependencies from %s: %w", depTable, err)
	}
	return issueIDs, nil
}

func deleteWispRows(ctx context.Context, tx *sql.Tx, ids []string) error {
	for _, id := range ids {
		if err := deleteIssueRowInTx(ctx, tx, id, true); err != nil {
			return fmt.Errorf("delete wisp %s: %w", id, err)
		}
	}
	return nil
}

func deleteRegularRows(ctx context.Context, tx *sql.Tx, ids []string) (int, error) {
	deleted := 0
	total := len(ids)
	for i := 0; i < total; i += deleteBatchSize {
		end := i + deleteBatchSize
		if end > total {
			end = total
		}
		batch := ids[i:end]
		batchInClause, batchArgs := buildSQLInClause(batch)
		deleteResult, err := tx.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM issues WHERE id IN (%s)`, batchInClause),
			batchArgs...)
		if err != nil {
			return 0, fmt.Errorf("delete issues: %w", err)
		}
		rowsAffected, _ := deleteResult.RowsAffected()
		deleted += int(rowsAffected)
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM leases WHERE issue_id IN (%s)`, batchInClause),
			batchArgs...); err != nil {
			return 0, fmt.Errorf("delete leases: %w", err)
		}
	}
	return deleted, nil
}

func journalDeletedIssues(ctx context.Context, tx *sql.Tx, ids []string) error {
	for _, id := range ids {
		if err := RecordDeleteInTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

// ExistingIssueIDsInTableInTx returns the requested IDs that currently exist
// in the selected issue table. It preserves caller ordering so delete and
// journal records are deterministic across batches.
func ExistingIssueIDsInTableInTx(ctx context.Context, tx DBTX, table string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	switch table {
	case "issues", "wisps":
	default:
		return nil, fmt.Errorf("unsupported issue table %q", table)
	}
	exists, err := collectExistingIssueIDs(ctx, tx, table, ids)
	if err != nil {
		return nil, err
	}
	actual := make([]string, 0, len(exists))
	for _, id := range ids {
		if _, ok := exists[id]; ok {
			actual = append(actual, id)
		}
	}
	return actual, nil
}

func collectExistingIssueIDs(ctx context.Context, tx DBTX, table string, ids []string) (map[string]struct{}, error) {
	exists := make(map[string]struct{}, len(ids))
	total := len(ids)
	for i := 0; i < total; i += deleteBatchSize {
		end := i + deleteBatchSize
		if end > total {
			end = total
		}
		batchIDs, err := readExistingIssueIDsBatch(ctx, tx, table, ids[i:end])
		if err != nil {
			return nil, err
		}
		for _, id := range batchIDs {
			exists[id] = struct{}{}
		}
	}
	return exists, nil
}

//nolint:gosec // G201: table is validated by the caller and placeholders are ?.
func readExistingIssueIDsBatch(ctx context.Context, tx DBTX, table string, ids []string) ([]string, error) {
	inClause, args := buildSQLInClause(ids)
	rows, err := tx.QueryContext(ctx, "SELECT id FROM "+table+" WHERE id IN ("+inClause+")", args...)
	if err != nil {
		return nil, err
	}
	var existingIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		existingIDs = append(existingIDs, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return existingIDs, nil
}

// findAllDependentsRecursiveInTx finds all issues that depend on the given
// issues, recursively. Uses batched IN-clause queries. Traversal is capped
// at maxRecursiveResults total discovered IDs.
//
//nolint:gosec // G201: inClause contains only ? placeholders
func FindAllDependentsInTx(ctx context.Context, tx DBTX, ids []string) (map[string]bool, error) {
	result := make(map[string]bool)
	for _, id := range ids {
		result[id] = true
	}

	toProcess := make([]string, len(ids))
	copy(toProcess, ids)

	for {
		if len(toProcess) == 0 {
			break
		}
		if len(result) > maxRecursiveResults {
			return nil, fmt.Errorf("cascade traversal discovered over %d issues; aborting to prevent runaway deletion", maxRecursiveResults)
		}
		batchEnd := deleteBatchSize
		if batchEnd > len(toProcess) {
			batchEnd = len(toProcess)
		}
		batch := toProcess[:batchEnd]
		toProcess = toProcess[batchEnd:]

		var err error
		toProcess, err = expandDependentBatch(ctx, tx, batch, result, toProcess)
		if err != nil {
			return nil, err
		}
	}

	return result, nil
}

func expandDependentBatch(ctx context.Context, tx DBTX, batch []string, result map[string]bool, toProcess []string) ([]string, error) {
	inClause, args := buildSQLInClause(batch)
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		dependentIDs, err := readDependentIDsFromTable(ctx, tx, depTable, inClause, args)
		if err != nil {
			if optionalBlockedTable(depTable) && isTableNotExistError(err) {
				continue
			}
			return nil, fmt.Errorf("query dependents for batch from %s: %w", depTable, err)
		}
		for _, depID := range dependentIDs {
			if result[depID] {
				continue
			}
			result[depID] = true
			toProcess = append(toProcess, depID)
		}
	}
	return toProcess, nil
}

//nolint:gosec // G201: depTable is hardcoded and inClause contains only ?.
func readDependentIDsFromTable(ctx context.Context, tx DBTX, depTable, inClause string, args []interface{}) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		fmt.Sprintf(`SELECT issue_id FROM %s WHERE %s`, depTable, depTargetIn("", inClause)),
		args...)
	if err != nil {
		return nil, err
	}
	var dependentIDs []string
	for rows.Next() {
		var depID string
		if err := rows.Scan(&depID); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan dependent: %w", err)
		}
		dependentIDs = append(dependentIDs, depID)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dependents for batch from %s: %w", depTable, err)
	}
	return dependentIDs, nil
}

//nolint:gosec // G201: table is selected by callers from fixed issue/wisp auxiliary tables.
func CountRowsForIssueIDsInTx(ctx context.Context, tx DBTX, table string, ids []string) (int, error) {
	total := 0
	idTotal := len(ids)
	for i := 0; i < idTotal; i += deleteBatchSize {
		end := i + deleteBatchSize
		if end > idTotal {
			end = idTotal
		}
		inClause, args := buildSQLInClause(ids[i:end])
		var count int
		if err := tx.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE issue_id IN (%s)`, table, inClause),
			args...).Scan(&count); err != nil {
			if optionalBlockedTable(table) && isTableNotExistError(err) {
				continue
			}
			return 0, err
		}
		total += count
	}
	return total, nil
}
