package issueops

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage/depid"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/types"
)

// CheckDependencyCycleInTx rejects self-dependencies and cycles across the
// combined blocks, conditional-blocks, and parent-child graph before insert.
// The caller may pass a restricted depTables list for a known storage bucket;
// nil uses all dependency tables.
func CheckDependencyCycleInTx(ctx context.Context, tx DBTX, dep *types.Dependency, depTables []string) error {
	if dep.IssueID == dep.DependsOnID {
		return fmt.Errorf("%w: %s cannot depend on itself", domain.ErrSelfDependency, dep.IssueID)
	}
	if !types.IsSchedulingEdge(dep.Type) {
		return nil
	}
	wouldCycle, err := WouldCreateSchedulingCycleInTx(ctx, tx, dep.IssueID, dep.DependsOnID, depTables)
	if err != nil {
		return fmt.Errorf("failed to check for dependency cycle: %w", err)
	}
	if wouldCycle {
		return domain.ErrDependencyCycle
	}
	return nil
}

// WouldCreateSchedulingCycleInTx reports whether adding issueID -> dependsOnID
// would close a cycle in the combined scheduling graph. It is shared by the
// classic and domain storage stacks so both traverse the same dependency types
// and typed target columns.
func WouldCreateSchedulingCycleInTx(ctx context.Context, tx DBTX, issueID, dependsOnID string, depTables []string) (bool, error) {
	if len(depTables) == 0 {
		depTables = cycleDetectionTables()
	}
	var reachable int
	query := cycleReachabilityQuery(depTables)
	if err := tx.QueryRowContext(ctx, query, dependsOnID, issueID).Scan(&reachable); err != nil {
		return false, err
	}
	return reachable > 0, nil
}

// cycleReachabilityQuery uses UNION distinct recursion so cyclic and diamond
// graphs terminate by unique reachable node instead of enumerating paths.
//
// The walked edge set is the union of scheduling-relevant edges: blocks,
// conditional-blocks, and parent-child. Parent-child is included because a
// blocked parent propagates its blocked state to its children in the
// ready-work computation, so a chain mixing blocks and parent-child edges
// can form a logical livelock that prevents anything from being ready.
func cycleReachabilityQuery(depTables []string) string {
	if len(depTables) == 1 {
		return fmt.Sprintf(`
			WITH RECURSIVE reachable(node) AS (
				SELECT ?
				UNION
				SELECT %s
				FROM reachable r
				JOIN %s d ON d.issue_id = r.node AND d.type IN ('blocks', 'conditional-blocks', 'parent-child')
			)
			SELECT COUNT(*) FROM reachable WHERE node = ?
		`, DepTargetExpr, depTables[0])
	}

	var unions []string
	for _, t := range depTables {
		unions = append(unions, fmt.Sprintf("SELECT issue_id, %s AS depends_on_id FROM %s WHERE type IN ('blocks', 'conditional-blocks', 'parent-child')", DepTargetExpr, t))
	}
	unionQuery := strings.Join(unions, " UNION ")
	return fmt.Sprintf(`
		WITH RECURSIVE reachable(node) AS (
			SELECT ?
			UNION
			SELECT d.depends_on_id
			FROM reachable r
			JOIN (%s) d ON d.issue_id = r.node
		)
		SELECT COUNT(*) FROM reachable WHERE node = ?
	`, unionQuery)
}

func cycleDetectionTables() []string {
	return []string{"dependencies", "wisp_dependencies"}
}

// CheckBlockingHierarchyInTx rejects blocking dependencies between an issue
// and its own ancestor or descendant. Cross-prefix/external targets must be
// filtered by the caller because no local hierarchy can connect them.
func CheckBlockingHierarchyInTx(ctx context.Context, tx DBTX, dep *types.Dependency, depTables []string) error {
	if dep.Type != types.DepBlocks && dep.Type != types.DepConditionalBlocks {
		return nil
	}
	if dep.IssueID == dep.DependsOnID {
		return nil // The dedicated self-dependency check owns this error.
	}
	if len(depTables) == 0 {
		depTables = cycleDetectionTables()
	}
	blockerIsAncestor, err := isAncestorInTx(ctx, tx, dep.IssueID, dep.DependsOnID, depTables)
	if err != nil {
		return fmt.Errorf("failed to check blocker ancestry: %w", err)
	}
	if blockerIsAncestor {
		return &domain.DependencyHierarchyConflictError{
			IssueID: dep.IssueID, BlockerID: dep.DependsOnID, BlockerIsAncestor: true,
		}
	}
	blockerIsDescendant, err := isAncestorInTx(ctx, tx, dep.DependsOnID, dep.IssueID, depTables)
	if err != nil {
		return fmt.Errorf("failed to check blocker ancestry: %w", err)
	}
	if blockerIsDescendant {
		return &domain.DependencyHierarchyConflictError{
			IssueID: dep.IssueID, BlockerID: dep.DependsOnID,
		}
	}
	return nil
}

// isAncestorInTx reports whether candidate is an ancestor of node along
// parent-child dependency edges (walking child -> parent only, so siblings
// and cousins in the same hierarchy do not match). Uses UNION distinct
// recursion so diamond/cyclic parentage terminates by unique reachable node.
func isAncestorInTx(ctx context.Context, tx DBTX, node, candidate string, depTables []string) (bool, error) {
	var unions []string
	for _, t := range depTables {
		unions = append(unions, fmt.Sprintf("SELECT issue_id, %s AS parent_id FROM %s WHERE type = 'parent-child'", DepTargetExpr, t))
	}
	//nolint:gosec // G201: depTables are fixed dependency table names from cycleDetectionTables/opts.
	query := fmt.Sprintf(`
		WITH RECURSIVE ancestors(node) AS (
			SELECT ?
			UNION
			SELECT d.parent_id
			FROM ancestors a
			JOIN (%s) d ON d.issue_id = a.node
		)
		SELECT COUNT(*) FROM ancestors WHERE node = ?
	`, strings.Join(unions, " UNION "))
	var n int
	if err := tx.QueryRowContext(ctx, query, node, candidate).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func DeleteWispFromDependenciesInTx(ctx context.Context, tx *sql.Tx, wispID string) error {
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM dependencies WHERE depends_on_wisp_id = ?", wispID); err != nil {
		return fmt.Errorf("delete wisp %s from dependencies: %w", wispID, err)
	}
	return nil
}

//nolint:gosec // G201: inClause contains only ? placeholders
func DeleteWispsFromDependenciesInTx(ctx context.Context, tx *sql.Tx, wispIDs []string) error {
	if len(wispIDs) == 0 {
		return nil
	}
	inClause, args := buildSQLInClause(wispIDs)
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM dependencies WHERE depends_on_wisp_id IN (%s)", inClause),
		args...); err != nil {
		return fmt.Errorf("delete wisps from dependencies: %w", err)
	}
	return nil
}

// Dependency target rewrites reinsert matching rows because Dolt can leave the
// stored generated depends_on_id column stale after a split target column is
// updated by FK cascade.
func UpdateWispIDInDependenciesInTx(ctx context.Context, tx *sql.Tx, oldID, newID string) error {
	for _, table := range []string{"dependencies", "wisp_dependencies"} {
		if err := replaceDependencyTargetInTx(ctx, tx, table, "depends_on_wisp_id", oldID, newID); err != nil {
			return fmt.Errorf("update wisp %s -> %s in %s: %w", oldID, newID, table, err)
		}
	}
	return nil
}

func UpdateIssueIDInDependenciesInTx(ctx context.Context, tx *sql.Tx, oldID, newID string) error {
	for _, table := range []string{"dependencies", "wisp_dependencies"} {
		if err := replaceDependencyTargetInTx(ctx, tx, table, "depends_on_issue_id", oldID, newID); err != nil {
			return fmt.Errorf("update issue target %s -> %s in %s: %w", oldID, newID, table, err)
		}
	}
	// Re-derive the deterministic primary key for rows whose SOURCE issue was
	// renamed. dependencies.issue_id carries fk_dep_issue ... ON UPDATE CASCADE, so
	// renaming the issues row (updateIssueIDInTx updates issues.id first) cascades
	// issue_id from oldID to newID before we get here — but the cascade leaves the
	// surrogate id at depid.New(oldID, target). A stale id re-forks the primary key
	// across clones (#4259) and breaks the same-PK => same-edge invariant the pull
	// conflict resolver relies on, so recompute it from the post-rename (newID, target).
	if err := rekeyDependencySourceInTx(ctx, tx, oldID, newID); err != nil {
		return fmt.Errorf("rekey dependency sources %s -> %s: %w", oldID, newID, err)
	}
	return nil
}

// rekeyDependencySourceInTx rewrites dependencies.id for every edge whose source
// issue was renamed to newID so the stored id equals depid.New(newID, target). It
// matches rows by both newID (the normal post-FK-cascade state) and oldID (defensive,
// in case a caller reaches here before the cascade) and re-asserts issue_id = newID
// so the row converges either way. Only rows whose id is actually stale are touched.
func rekeyDependencySourceInTx(ctx context.Context, tx *sql.Tx, oldID, newID string) error {
	queryRows, err := tx.QueryContext(ctx, `
		SELECT id, depends_on_issue_id, depends_on_wisp_id, depends_on_external
		FROM dependencies
		WHERE issue_id = ? OR issue_id = ?
	`, newID, oldID)
	if err != nil {
		return fmt.Errorf("query dependency sources: %w", err)
	}
	type rekey struct{ oldRowID, newRowID string }
	var rekeys []rekey
	for queryRows.Next() {
		var id string
		var issueTarget, wispTarget, external sql.NullString
		if err := queryRows.Scan(&id, &issueTarget, &wispTarget, &external); err != nil {
			_ = queryRows.Close()
			return fmt.Errorf("scan dependency source: %w", err)
		}
		target, ok := resolveDependencyTarget(issueTarget, wispTarget, external)
		if !ok {
			continue // ck_dep_one_target guarantees one target; skip defensively
		}
		if want := depid.New(newID, target); want != id {
			rekeys = append(rekeys, rekey{oldRowID: id, newRowID: want})
		}
	}
	_ = queryRows.Close()
	if err := queryRows.Err(); err != nil {
		return fmt.Errorf("iterate dependency sources: %w", err)
	}
	for _, rk := range rekeys {
		if _, err := tx.ExecContext(ctx,
			"UPDATE dependencies SET id = ?, issue_id = ? WHERE id = ?",
			rk.newRowID, newID, rk.oldRowID); err != nil {
			return fmt.Errorf("rekey dependency source id %s -> %s: %w", rk.oldRowID, rk.newRowID, err)
		}
	}
	return nil
}

// resolveDependencyTarget returns the single non-null dependency target — the value
// depid.New and the uk_dep_* unique keys treat as the edge's target — following the
// same precedence as DepTargetExpr (issue, then wisp, then external).
func resolveDependencyTarget(issueTarget, wispTarget, external sql.NullString) (string, bool) {
	switch {
	case issueTarget.Valid:
		return issueTarget.String, true
	case wispTarget.Valid:
		return wispTarget.String, true
	case external.Valid:
		return external.String, true
	default:
		return "", false
	}
}

type dependencyTargetRow struct {
	issueID     string
	issueTarget sql.NullString
	wispTarget  sql.NullString
	external    sql.NullString
	depType     string
	createdAt   sql.NullTime
	createdBy   sql.NullString
	metadata    sql.NullString
	threadID    sql.NullString
}

func replaceDependencyTargetInTx(ctx context.Context, tx *sql.Tx, table, column, oldID, newID string) error {
	// Dolt does not reliably recompute the stored generated depends_on_id when
	// only the split target column changes. Reinsert rows so the generated key
	// is calculated from the new target value.
	if err := checkRenameTargetCollision(ctx, tx, table, column, newID); err != nil {
		return err
	}

	rows, err := loadDependencyTargetRows(ctx, tx, table, column, oldID, newID)
	if err != nil {
		return err
	}

	if err := deleteDependencyTargetRows(ctx, tx, table, column, oldID); err != nil {
		return err
	}
	return insertDependencyTargetRows(ctx, tx, table, newID, rows)
}

//nolint:gosec // G201: table and column are hardcoded by callers.
func loadDependencyTargetRows(ctx context.Context, tx *sql.Tx, table, column, oldID, newID string) ([]dependencyTargetRow, error) {
	rows := make([]dependencyTargetRow, 0)
	queryRows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT issue_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external, type, created_at, created_by, metadata, thread_id
		FROM %s
		WHERE %s = ? OR (%s = ? AND depends_on_external IS NULL)
	`, table, column, DepTargetExpr), oldID, oldID)
	if err != nil {
		return nil, fmt.Errorf("query dependency targets: %w", err)
	}
	for queryRows.Next() {
		var row dependencyTargetRow
		if err := queryRows.Scan(&row.issueID, &row.issueTarget, &row.wispTarget, &row.external, &row.depType, &row.createdAt, &row.createdBy, &row.metadata, &row.threadID); err != nil {
			_ = queryRows.Close()
			return nil, fmt.Errorf("scan dependency target: %w", err)
		}
		if err := retargetDependencyTarget(&row, column, newID); err != nil {
			_ = queryRows.Close()
			return nil, err
		}
		rows = append(rows, row)
	}
	_ = queryRows.Close()
	if err := queryRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dependency targets: %w", err)
	}
	return rows, nil
}

func retargetDependencyTarget(row *dependencyTargetRow, column, newID string) error {
	switch column {
	case "depends_on_issue_id":
		row.issueTarget = sql.NullString{String: newID, Valid: true}
		row.wispTarget = sql.NullString{}
		row.external = sql.NullString{}
	case "depends_on_wisp_id":
		row.issueTarget = sql.NullString{}
		row.wispTarget = sql.NullString{String: newID, Valid: true}
		row.external = sql.NullString{}
	default:
		return fmt.Errorf("replace dependency target: unsupported typed column %q", column)
	}
	return nil
}

//nolint:gosec // G201: table and column are hardcoded by callers.
func deleteDependencyTargetRows(ctx context.Context, tx *sql.Tx, table, column, oldID string) error {
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE %s = ? OR (%s = ? AND depends_on_external IS NULL)`, table, column, DepTargetExpr), oldID, oldID); err != nil {
		return fmt.Errorf("delete old dependency target: %w", err)
	}
	return nil
}

//nolint:gosec // G201: table is hardcoded by callers.
func insertDependencyTargetRows(ctx context.Context, tx *sql.Tx, table, newID string, rows []dependencyTargetRow) error {
	for _, row := range rows {
		// The retargeted edge's natural key is (issue_id, newID): the switch above
		// set exactly one typed target column to newID. Re-derive id from it so the
		// rewritten row stays merge-safe and keeps a clone-stable primary key (#4259).
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO %s (id, issue_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external, type, created_at, created_by, metadata, thread_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, table), depid.New(row.issueID, newID), row.issueID, nullStringValue(row.issueTarget), nullStringValue(row.wispTarget), nullStringValue(row.external), row.depType, nullTimeValue(row.createdAt), nullStringValue(row.createdBy), nullStringValue(row.metadata), nullStringValue(row.threadID)); err != nil {
			return fmt.Errorf("insert replacement dependency target: %w", err)
		}
	}
	return nil
}

func nullStringValue(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

func nullTimeValue(value sql.NullTime) any {
	if !value.Valid {
		return nil
	}
	return value.Time
}

func RetargetInboundDependenciesToWispInTx(ctx context.Context, tx DBTX, id string) error {
	for _, table := range []string{"dependencies", "wisp_dependencies"} {
		if err := checkRetargetTargetCollision(ctx, tx, table, "depends_on_issue_id", "depends_on_wisp_id", id); err != nil {
			return err
		}
		if err := checkRenameTargetCollision(ctx, tx, table, "depends_on_wisp_id", id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE %s
			SET depends_on_wisp_id = ?, depends_on_issue_id = NULL
			WHERE depends_on_issue_id = ?
		`, table), id, id); err != nil {
			return fmt.Errorf("retarget inbound dependencies to wisp in %s for %s: %w", table, id, err)
		}
	}
	return nil
}

func RetargetInboundDependenciesToIssueInTx(ctx context.Context, tx DBTX, id string) error {
	for _, table := range []string{"dependencies", "wisp_dependencies"} {
		if err := checkRetargetTargetCollision(ctx, tx, table, "depends_on_wisp_id", "depends_on_issue_id", id); err != nil {
			return err
		}
		if err := checkRenameTargetCollision(ctx, tx, table, "depends_on_issue_id", id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE %s
			SET depends_on_issue_id = ?, depends_on_wisp_id = NULL
			WHERE depends_on_wisp_id = ?
		`, table), id, id); err != nil {
			return fmt.Errorf("retarget inbound dependencies to issue in %s for %s: %w", table, id, err)
		}
	}
	return nil
}

// UpdateIssueIDInDependencyTargetsInTx is called after the issues PK is updated
// from oldID to newID. FK ON UPDATE CASCADE has already propagated
// depends_on_issue_id from oldID to newID across dependencies and
// wisp_dependencies, so no rewrite is needed.
func UpdateIssueIDInDependencyTargetsInTx(ctx context.Context, tx *sql.Tx, _, newID string) error {
	for _, table := range []string{"dependencies", "wisp_dependencies"} {
		if err := checkRenameTargetCollision(ctx, tx, table, "depends_on_issue_id", newID); err != nil {
			return err
		}
	}
	return nil
}

//nolint:gosec // G201: table and typed columns are hardcoded constants.
func checkRetargetTargetCollision(ctx context.Context, tx DBTX, table, sourceCol, destCol, id string) error {
	var conflictCols []string
	switch destCol {
	case "depends_on_issue_id":
		conflictCols = []string{"depends_on_issue_id", "depends_on_external"}
	case "depends_on_wisp_id":
		conflictCols = []string{"depends_on_wisp_id", "depends_on_external"}
	default:
		return fmt.Errorf("checkRetargetTargetCollision: unsupported destination column %q", destCol)
	}
	if sourceCol != "depends_on_issue_id" && sourceCol != "depends_on_wisp_id" {
		return fmt.Errorf("checkRetargetTargetCollision: unsupported source column %q", sourceCol)
	}

	query := fmt.Sprintf(`
		SELECT 1 FROM %s moving
		JOIN %s existing ON moving.issue_id = existing.issue_id
		WHERE moving.%s = ?
		  AND (existing.%s = ? OR existing.%s = ?)
		LIMIT 1
	`, table, table, sourceCol, conflictCols[0], conflictCols[1])

	var found int
	err := tx.QueryRowContext(ctx, query, id, id, id).Scan(&found)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		if isTableNotExistError(err) {
			return nil
		}
		return fmt.Errorf("check retarget collision in %s: %w", table, err)
	}
	return fmt.Errorf("retarget to %s collides with existing dependency target in %s", id, table)
}

//nolint:gosec // G201: table and typedCol are hardcoded constants.
func checkRenameTargetCollision(ctx context.Context, tx DBTX, table, typedCol, newID string) error {
	var otherCols []string
	switch typedCol {
	case "depends_on_issue_id":
		otherCols = []string{"depends_on_wisp_id", "depends_on_external"}
	case "depends_on_wisp_id":
		otherCols = []string{"depends_on_issue_id", "depends_on_external"}
	default:
		return fmt.Errorf("checkRenameTargetCollision: unsupported typed column %q", typedCol)
	}

	query := fmt.Sprintf(`
		SELECT 1 FROM %s a
		JOIN %s b ON a.issue_id = b.issue_id
		WHERE a.%s = ?
		  AND (b.%s = ? OR b.%s = ?)
		LIMIT 1
	`, table, table, typedCol, otherCols[0], otherCols[1])

	var found int
	err := tx.QueryRowContext(ctx, query, newID, newID, newID).Scan(&found)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		if isTableNotExistError(err) {
			return nil
		}
		return fmt.Errorf("check rename collision in %s: %w", table, err)
	}
	return fmt.Errorf("rename to %s collides with existing dependency target in %s", newID, table)
}
