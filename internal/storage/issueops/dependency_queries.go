package issueops

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/jonbaldie/beads/internal/types"
)

// GetAllDependencyRecordsInTx returns all dependency records from permanent and
// wisp dependency tables.
func GetAllDependencyRecordsInTx(ctx context.Context, tx DBTX) (map[string][]*types.Dependency, error) {
	result := make(map[string][]*types.Dependency)
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		if err := getAllDependencyRecordsIntoFromTable(ctx, tx, depTable, result); err != nil {
			if optionalBlockedTable(depTable) && isTableNotExistError(err) {
				continue
			}
			return nil, err
		}
	}
	return result, nil
}

//nolint:gosec // G201: depTable is "dependencies" or "wisp_dependencies" (hardcoded by caller).
func getAllDependencyRecordsIntoFromTable(ctx context.Context, tx DBTX, depTable string, result map[string][]*types.Dependency) error {
	// Total order: issue_id alone is only a grouping key; without a tiebreaker the
	// intra-issue dependency slice is plan-dependent (export churn, unstable --json).
	// Mirrors labels bulk-load (issue_id, label). The separate typed-target unique
	// keys don't make (issue_id, depends_on_id, type) total, so `id` closes it.
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
			SELECT issue_id, %s AS depends_on_id, type, created_at, created_by, metadata, thread_id
			FROM %s
			ORDER BY issue_id, depends_on_id, type, id
		`, DepTargetExpr, depTable))
	if err != nil {
		return fmt.Errorf("get all dependency records from %s: %w", depTable, err)
	}
	defer rows.Close()

	for rows.Next() {
		dep, scanErr := scanDependencyRow(rows)
		if scanErr != nil {
			return fmt.Errorf("get all dependency records from %s: %w", depTable, scanErr)
		}
		result[dep.IssueID] = append(result[dep.IssueID], dep)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("get all dependency records from %s: %w", depTable, err)
	}
	return nil
}

// GetDependencyRecordsForIssuesInTx returns dependency records for specific issues,
// routing each ID to dependencies or wisp_dependencies based on wisp status.
// Uses a single batched wisp-partition query + batched IN clauses, so cost is
// O(1 + N/queryBatchSize) round-trips rather than O(N) — important on remote
// backends (see GH#3414).
func GetDependencyRecordsForIssuesInTx(ctx context.Context, tx DBTX, issueIDs []string) (map[string][]*types.Dependency, error) {
	if len(issueIDs) == 0 {
		return make(map[string][]*types.Dependency), nil
	}

	wispIDs, permIDs, err := PartitionWispIDsInTx(ctx, tx, issueIDs)
	if err != nil {
		return nil, err
	}

	result := make(map[string][]*types.Dependency)
	if len(wispIDs) > 0 {
		if err := getDependencyRecordsIntoFromTable(ctx, tx, "wisp_dependencies", wispIDs, result); err != nil {
			return nil, err
		}
	}
	if len(permIDs) > 0 {
		if err := getDependencyRecordsIntoFromTable(ctx, tx, "dependencies", permIDs, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// GetDependencyRecordsForIssuesFromTableInTx is a fast-path variant used by
// callers that already know every ID belongs to a single dep table (e.g.
// searchTableInTx). Skips the wisp-partition round-trip.
func GetDependencyRecordsForIssuesFromTableInTx(ctx context.Context, tx DBTX, depTable string, issueIDs []string) (map[string][]*types.Dependency, error) {
	if len(issueIDs) == 0 {
		return make(map[string][]*types.Dependency), nil
	}
	result := make(map[string][]*types.Dependency)
	if err := getDependencyRecordsIntoFromTable(ctx, tx, depTable, issueIDs, result); err != nil {
		return nil, err
	}
	return result, nil
}

//nolint:gosec // G201: depTable is "dependencies" or "wisp_dependencies" (hardcoded by callers).
func getDependencyRecordsIntoFromTable(ctx context.Context, tx DBTX, depTable string, ids []string, result map[string][]*types.Dependency) error {
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
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(
			`SELECT issue_id, %s AS depends_on_id, type, created_at, created_by, metadata, thread_id
			 FROM %s WHERE issue_id IN (%s) ORDER BY issue_id, depends_on_id, type, id`,
			DepTargetExpr, depTable, strings.Join(placeholders, ",")), args...)
		if err != nil {
			return fmt.Errorf("get dependency records from %s: %w", depTable, err)
		}
		for rows.Next() {
			dep, scanErr := scanDependencyRow(rows)
			if scanErr != nil {
				_ = rows.Close()
				return fmt.Errorf("get dependency records: scan: %w", scanErr)
			}
			result[dep.IssueID] = append(result[dep.IssueID], dep)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("get dependency records: rows: %w", err)
		}
	}
	return nil
}

// GetDependentRecordsForIssuesInTx returns raw dependency rows keyed by TARGET
// id: for each id in targetIDs, the rows whose target is that id — its INCOMING
// edges, i.e. its dependents — spanning BOTH the durable and wisp dependency
// tables and applying NO type filter or visibility policy (the caller filters
// at hydration). It is the batched, target-keyed mirror of the source-keyed
// GetDependencyRecordsForIssuesInTx: one query per table per batch of
// queryBatchSize target ids, so cost is O(1 + N/queryBatchSize) round-trips per
// table rather than O(N) — the whole-page read that lets a caller render every
// id's inbound `blocks` edges without a per-id fan-out.
//
// A target is matched by the coalesced target expression (DepTargetExpr) — the
// same predicate the batched source-keyed blocks/counts reads use — so an id
// that appears in any of the three typed target columns resolves; each returned
// row's DependsOnID is that resolved target, which is the map key. Rows are
// de-duped by their primary id across the two tables exactly as
// GetDependentRecordsInTx does: a wisp promoted to durable carries ONE depid in
// both tables, so the durable table is scanned first and a repeat id from the
// wisp table is skipped — the edge is returned once, as its authoritative
// durable row.
func GetDependentRecordsForIssuesInTx(ctx context.Context, tx DBTX, targetIDs []string) (map[string][]*types.Dependency, error) {
	result := make(map[string][]*types.Dependency)
	if len(targetIDs) == 0 {
		return result, nil
	}
	// De-dup by row id across the two tables, preferring the durable copy scanned
	// first — same cross-table collision handling as GetDependentRecordsInTx.
	seen := make(map[string]bool)
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		if err := getDependentRecordsIntoFromTable(ctx, tx, depTable, targetIDs, seen, result); err != nil {
			if optionalBlockedTable(depTable) && isTableNotExistError(err) {
				continue
			}
			return nil, err
		}
	}
	return result, nil
}

//nolint:gosec // G201: depTable is "dependencies" or "wisp_dependencies" (hardcoded by caller); placeholders are ? only.
func getDependentRecordsIntoFromTable(ctx context.Context, tx DBTX, depTable string, targetIDs []string, seen map[string]bool, result map[string][]*types.Dependency) error {
	total := len(targetIDs)
	for start := 0; start < total; start += queryBatchSize {
		end := start + queryBatchSize
		if end > len(targetIDs) {
			end = len(targetIDs)
		}
		batch := targetIDs[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(
			`SELECT id, issue_id, %s AS depends_on_id, type, created_at, created_by, metadata, thread_id
			 FROM %s WHERE %s ORDER BY %s`,
			DepTargetExpr, depTable, depTargetIn("", strings.Join(placeholders, ",")), DepTargetExpr), args...)
		if err != nil {
			return fmt.Errorf("get dependent records from %s: %w", depTable, err)
		}
		for rows.Next() {
			dep, scanErr := scanDependentRow(rows)
			if scanErr != nil {
				_ = rows.Close()
				return fmt.Errorf("get dependent records: scan: %w", scanErr)
			}
			// De-dup by row id (depid): the wisp copy of a promoted edge carries
			// the same id as the durable copy scanned first, so skip the repeat.
			if seen[dep.ID] {
				continue
			}
			seen[dep.ID] = true
			result[dep.DependsOnID] = append(result[dep.DependsOnID], dep)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("get dependent records: rows: %w", err)
		}
	}
	return nil
}

// Target-keyed dependents-read bounds. A raw read has no consumer to apply a
// page size, so it clamps its own (default when limit <= 0, hard cap otherwise).
const (
	defaultDependentRecordsLimit = 100
	maxDependentRecordsLimit     = 500
)

// GetDependentRecordsInTx returns raw dependency rows whose TARGET is targetID
// — the edges pointing AT targetID — from both the permanent and wisp
// dependency tables. Unlike GetDependents/GetDependentsWithMetadata it does
// NOT join or hydrate the source issues, so edges from dangling, cross-project,
// or wisp sources are returned as raw rows rather than dropped. RAW READ: it
// spans BOTH the `dependencies` and `wisp_dependencies` tables and applies no
// visibility policy — filtering (e.g. a group-membership visibility rule) is
// the caller's job, applied at hydration.
//
// When depType is non-empty only rows of that dependency type are returned
// ("" = all types). Results are ordered by the dependency row's primary id ASC
// and bounded by limit (see the clamp constants). afterID is a keyset
// continuation over that id order: "" starts at the beginning, otherwise only
// rows with id > afterID are returned.
//
// CURSOR KEY: the dependency row's own id (depid.New(issue_id, target), a
// UUIDv5 that is stable and globally unique across both tables) — NOT the source
// issue_id. issue_id is not a total key for a fixed target: a source can appear
// across the two scanned tables, so paging on it could drop or duplicate rows.
// Paging on the unique row id is total, so each source's inbound edge is
// returned exactly once. The target match is a sargable per-column OR over the
// three typed columns (seeks idx_dep_*_target / the type composites) rather than
// a COALESCE wrapper.
func GetDependentRecordsInTx(ctx context.Context, tx DBTX, targetID, depType string, limit int, afterID string) ([]*types.Dependency, error) {
	limit = clampDependentRecordsLimit(limit)
	all, err := loadDependentRecordsFromTables(ctx, tx, targetID, depType, limit, afterID)
	if err != nil {
		return nil, err
	}

	// Merge the de-duped per-table pages into one total order by row id. Each
	// table returned its first `limit` rows with id > afterID, so the global
	// first `limit` DISTINCT ids > afterID are all present; sorting by the
	// globally unique id and truncating yields exactly that page — no drop, no
	// dup, and stable across a cross-table id collision.
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func clampDependentRecordsLimit(limit int) int {
	if limit <= 0 {
		return defaultDependentRecordsLimit
	}
	if limit > maxDependentRecordsLimit {
		return maxDependentRecordsLimit
	}
	return limit
}

func loadDependentRecordsFromTables(ctx context.Context, tx DBTX, targetID, depType string, limit int, afterID string) ([]*types.Dependency, error) {
	var all []*types.Dependency
	seen := make(map[string]bool)
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		rows, err := queryDependentRecordsFromTable(ctx, tx, depTable, targetID, depType, limit, afterID)
		if err != nil {
			if optionalBlockedTable(depTable) && isTableNotExistError(err) {
				continue
			}
			return nil, err
		}
		all = appendUniqueDependentRecords(all, seen, rows)
	}
	return all, nil
}

// appendUniqueDependentRecords de-duplicates by row id across the two tables.
// depid.New keys the id on (issue_id, target) and deliberately omits the table,
// so a promoted edge carries one id in both tables. The durable copy is kept
// because loadDependentRecordsFromTables scans it first.
func appendUniqueDependentRecords(all []*types.Dependency, seen map[string]bool, rows []*types.Dependency) []*types.Dependency {
	for _, row := range rows {
		if seen[row.ID] {
			continue
		}
		seen[row.ID] = true
		all = append(all, row)
	}
	return all
}

//nolint:gosec // G201: depTable is a hardcoded constant; targetID/depType/afterID are bound as parameters.
func queryDependentRecordsFromTable(ctx context.Context, tx DBTX, depTable, targetID, depType string, limit int, afterID string) ([]*types.Dependency, error) {
	query := fmt.Sprintf(`
		SELECT id, issue_id, %s AS depends_on_id, type, created_at, created_by, metadata, thread_id
		FROM %s
		WHERE %s`, DepTargetExpr, depTable, depTargetEqualsOr())
	args := []any{targetID, targetID, targetID}
	if depType != "" {
		query += " AND type = ?"
		args = append(args, depType)
	}
	if afterID != "" {
		query += " AND id > ?"
		args = append(args, afterID)
	}
	query += fmt.Sprintf(" ORDER BY id ASC LIMIT %d", limit)

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get dependent records from %s: %w", depTable, err)
	}
	defer rows.Close()

	var deps []*types.Dependency
	for rows.Next() {
		dep, scanErr := scanDependentRow(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("get dependent records: scan: %w", scanErr)
		}
		deps = append(deps, dep)
	}
	return deps, rows.Err()
}

// scanDependentRow scans a dependents row that INCLUDES the row id (the keyset
// cursor). The shared scanDependencyRow does not select id, and adding it there
// would ripple through every source-keyed read, so the target-keyed read owns
// this variant.
func scanDependentRow(rows *sql.Rows) (*types.Dependency, error) {
	var dep types.Dependency
	var createdAt sql.NullTime
	var metadata, threadID sql.NullString

	if err := rows.Scan(&dep.ID, &dep.IssueID, &dep.DependsOnID, &dep.Type, &createdAt, &dep.CreatedBy, &metadata, &threadID); err != nil {
		return nil, fmt.Errorf("scan dependent: %w", err)
	}
	if createdAt.Valid {
		dep.CreatedAt = createdAt.Time
	}
	if metadata.Valid {
		dep.Metadata = metadata.String
	}
	if threadID.Valid {
		dep.ThreadID = threadID.String
	}
	return &dep, nil
}

// CountDependentRecordsInTx returns the number of DISTINCT inbound edges of
// targetID, applying the same sargable target predicate and optional depType
// filter as GetDependentRecordsInTx but no keyset/limit. Callers that want a
// true total membership count need it without paging to exhaustion. Like the
// paged read it is a RAW count spanning both tables; the caller applies any
// visibility policy separately.
//
// It must agree with GetDependentRecordsInTx's durable-preferred de-dup: an edge
// present in BOTH tables (same depid) is ONE row in the page, so the count is
// every durable row PLUS every wisp row whose depid is not already a durable row
// for the same target/type. Summing two raw COUNT(*)s would over-count that edge
// and exceed the distinct keyset row count.
func CountDependentRecordsInTx(ctx context.Context, tx DBTX, targetID, depType string) (int, error) {
	durable, err := countDependentRecordsFromTable(ctx, tx, "dependencies", targetID, depType)
	if err != nil {
		return 0, err
	}
	wispExtra, err := countWispDependentsNotInDurableInTx(ctx, tx, targetID, depType)
	if err != nil {
		// wisp_dependencies absent: the durable count is the whole answer.
		if isTableNotExistError(err) {
			return durable, nil
		}
		return 0, err
	}
	return durable + wispExtra, nil
}

// countWispDependentsNotInDurableInTx counts wisp_dependencies rows whose target
// is targetID (optional depType) but whose depid is NOT present in the durable
// dependencies table for the same target/type — the wisp-ONLY inbound edges.
// Colliding edges (present in both tables) are counted on the durable side, so
// this is the exact complement that makes the total distinct-by-id. The NOT IN
// subquery is uncorrelated and bounded by the target's durable inbound-edge
// count; both arms use the sargable per-column OR target predicate.
//
//nolint:gosec // G201: table names are hardcoded constants; targetID/depType are bound as parameters.
func countWispDependentsNotInDurableInTx(ctx context.Context, tx DBTX, targetID, depType string) (int, error) {
	wispWhere := depTargetEqualsOr()
	durableWhere := depTargetEqualsOr()
	args := []any{targetID, targetID, targetID}
	if depType != "" {
		wispWhere += " AND type = ?"
		args = append(args, depType)
	}
	args = append(args, targetID, targetID, targetID)
	if depType != "" {
		durableWhere += " AND type = ?"
		args = append(args, depType)
	}
	query := fmt.Sprintf(
		"SELECT COUNT(*) FROM wisp_dependencies WHERE %s AND id NOT IN (SELECT id FROM dependencies WHERE %s)",
		wispWhere, durableWhere)
	var n int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count wisp-only dependent records: %w", err)
	}
	return n, nil
}

//nolint:gosec // G201: depTable is a hardcoded constant; targetID/depType are bound as parameters.
func countDependentRecordsFromTable(ctx context.Context, tx DBTX, depTable, targetID, depType string) (int, error) {
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s", depTable, depTargetEqualsOr())
	args := []any{targetID, targetID, targetID}
	if depType != "" {
		query += " AND type = ?"
		args = append(args, depType)
	}
	var n int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count dependent records from %s: %w", depTable, err)
	}
	return n, nil
}

func GetDependencyCountsInTx(ctx context.Context, tx DBTX, issueIDs []string) (map[string]*types.DependencyCounts, error) {
	if len(issueIDs) == 0 {
		return make(map[string]*types.DependencyCounts), nil
	}

	result := emptyDependencyCounts(issueIDs)
	depTables, err := dependencyCountTables(ctx, tx)
	if err != nil {
		return nil, err
	}

	total := len(issueIDs)
	for start := 0; start < total; start += queryBatchSize {
		end := start + queryBatchSize
		if end > len(issueIDs) {
			end = len(issueIDs)
		}
		batch := issueIDs[start:end]

		if err := dependencyCountsBatch(ctx, tx, batch, depTables, result); err != nil {
			return nil, err
		}
	}

	return result, nil
}

func emptyDependencyCounts(issueIDs []string) map[string]*types.DependencyCounts {
	result := make(map[string]*types.DependencyCounts, len(issueIDs))
	for _, id := range issueIDs {
		result[id] = &types.DependencyCounts{}
	}
	return result
}

func dependencyCountTables(ctx context.Context, tx DBTX) ([]string, error) {
	if empty, err := wispsTableEmptyOrMissingInTx(ctx, tx); err != nil {
		return nil, fmt.Errorf("get dependency counts: probe: %w", err)
	} else if empty {
		return []string{"dependencies"}, nil
	}
	return []string{"dependencies", "wisp_dependencies"}, nil
}

func dependencyCountsBatch(ctx context.Context, tx DBTX, batch, depTables []string, result map[string]*types.DependencyCounts) error {
	inClause, args := buildSQLInClause(batch)
	for _, depTable := range depTables {
		if err := dependencyCountsForTable(ctx, tx, depTable, inClause, args, result); err != nil {
			return err
		}
	}
	return nil
}

func dependencyCountsForTable(ctx context.Context, tx DBTX, depTable, inClause string, args []any, result map[string]*types.DependencyCounts) error {
	//nolint:gosec // G201: depTable is hardcoded and inClause contains only ? placeholders.
	blockerQuery := fmt.Sprintf(`
		SELECT issue_id, COUNT(*) as cnt
		FROM %s
		WHERE issue_id IN (%s) AND type = 'blocks'
		GROUP BY issue_id
	`, depTable, inClause)
	if err := readDependencyCountRows(ctx, tx, blockerQuery, args, result, false); err != nil {
		if optionalBlockedTable(depTable) && isTableNotExistError(err) {
			return nil
		}
		return fmt.Errorf("get dependency counts (blockers from %s): %w", depTable, err)
	}

	//nolint:gosec // G201: depTable is hardcoded and inClause contains only ? placeholders.
	dependentQuery := fmt.Sprintf(`
		SELECT %s AS depends_on_id, COUNT(*) as cnt
		FROM %s
		WHERE %s AND type = 'blocks'
		GROUP BY %s
	`, DepTargetExpr, depTable, depTargetIn("", inClause), DepTargetExpr)
	if err := readDependencyCountRows(ctx, tx, dependentQuery, args, result, true); err != nil {
		if optionalBlockedTable(depTable) && isTableNotExistError(err) {
			return nil
		}
		return fmt.Errorf("get dependency counts (dependents from %s): %w", depTable, err)
	}
	return nil
}

func readDependencyCountRows(ctx context.Context, tx DBTX, query string, args []any, result map[string]*types.DependencyCounts, dependent bool) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return fmt.Errorf("get dependency counts: scan %s: %w", dependencyCountKind(dependent), err)
		}
		if counts, ok := result[id]; ok {
			addDependencyCount(counts, count, dependent)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("get dependency counts: %s rows: %w", dependencyCountKind(dependent), err)
	}
	return nil
}

func dependencyCountKind(dependent bool) string {
	if dependent {
		return "dependent"
	}
	return "blocker"
}

func addDependencyCount(counts *types.DependencyCounts, count int, dependent bool) {
	if dependent {
		counts.DependentCount += count
		return
	}
	counts.DependencyCount += count
}
