package doctor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/types"
)

// CheckIDFormat checks whether issues use hash-based or sequential IDs.
// Opens its own store; prefer CheckIDFormatWithStore when a shared store is available.
func CheckIDFormat(path string) DoctorCheck {
	_, beadsDir := getBackendAndBeadsDir(path)

	doltPath := getDatabasePath(beadsDir)
	if _, err := os.Stat(doltPath); os.IsNotExist(err) {
		return DoctorCheck{
			Name:    "Issue IDs",
			Status:  StatusOK,
			Message: "No issues yet (will use hash-based IDs)",
		}
	}

	ctx := context.Background()
	store, err := dolt.NewFromConfigWithCLIOptions(ctx, beadsDir, &dolt.Config{ReadOnly: true})
	if err != nil {
		return DoctorCheck{
			Name:    "Issue IDs",
			Status:  StatusError,
			Message: "Unable to open database",
			Detail:  err.Error(),
		}
	}
	defer func() { _ = store.Close() }()

	return checkIDFormatWithStore(store)
}

// CheckIDFormatWithStore checks ID format using a shared store (GH#2636).
func CheckIDFormatWithStore(ss *SharedStore) DoctorCheck {
	store := ss.Store()
	if store == nil {
		return DoctorCheck{
			Name:    "Issue IDs",
			Status:  StatusOK,
			Message: "No issues yet (will use hash-based IDs)",
		}
	}
	return checkIDFormatWithStore(store)
}

func checkIDFormatWithStore(store *dolt.DoltStore) DoctorCheck {
	ctx := context.Background()
	db := store.UnderlyingDB()

	// Get sample of issues to check ID format (up to 10 for pattern analysis)
	rows, err := db.QueryContext(ctx, "SELECT id FROM issues ORDER BY created_at LIMIT 10")
	if err != nil {
		return DoctorCheck{
			Name:    "Issue IDs",
			Status:  StatusError,
			Message: "Unable to query issues",
			Detail:  err.Error(),
		}
	}
	defer rows.Close()

	var issueIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			issueIDs = append(issueIDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		return DoctorCheck{
			Name:    "Issue IDs",
			Status:  StatusWarning,
			Message: "Row iteration error",
			Detail:  err.Error(),
		}
	}

	if len(issueIDs) == 0 {
		return DoctorCheck{
			Name:    "Issue IDs",
			Status:  StatusOK,
			Message: "No issues yet (will use hash-based IDs)",
		}
	}

	// Detect ID format using robust heuristic
	if DetectHashBasedIDs(db, issueIDs) {
		return DoctorCheck{
			Name:    "Issue IDs",
			Status:  StatusOK,
			Message: "hash-based ✓",
		}
	}

	return DoctorCheck{
		Name:    "Issue IDs",
		Status:  StatusWarning,
		Message: "sequential IDs detected — consider migrating to hash-based IDs",
	}
}

// CheckDependencyCycles checks for circular dependencies in the issue graph.
// Opens its own store; prefer CheckDependencyCyclesWithStore when a shared store is available.
func CheckDependencyCycles(path string) DoctorCheck {
	_, beadsDir := getBackendAndBeadsDir(path)

	doltPath := getDatabasePath(beadsDir)
	if _, err := os.Stat(doltPath); os.IsNotExist(err) {
		return DoctorCheck{
			Name:    "Dependency Cycles",
			Status:  StatusOK,
			Message: "N/A (no database)",
		}
	}

	ctx := context.Background()
	store, err := dolt.NewFromConfigWithCLIOptions(ctx, beadsDir, &dolt.Config{ReadOnly: true})
	if err != nil {
		return DoctorCheck{
			Name:    "Dependency Cycles",
			Status:  StatusWarning,
			Message: "Unable to open database",
			Detail:  err.Error(),
		}
	}
	defer func() { _ = store.Close() }()

	return checkDependencyCyclesWithStore(store)
}

// CheckDependencyCyclesWithStore checks for cycles using a shared store (GH#2636).
func CheckDependencyCyclesWithStore(ss *SharedStore) DoctorCheck {
	store := ss.Store()
	if store == nil {
		return DoctorCheck{
			Name:    "Dependency Cycles",
			Status:  StatusOK,
			Message: "N/A (no database)",
		}
	}
	return checkDependencyCyclesWithStore(store)
}

// dependencyCyclePageSize bounds the rows fetched per query while loading the
// dependency graph. Shared dolt sql-servers enforce per-query read timeouts
// (read_timeout_millis) that kill long-running streaming queries mid-stream
// ("invalid connection"); keyset-paginated pages keep every query small.
const dependencyCyclePageSize = 1000

// dependencyCycleMaxEdges bounds the in-memory graph. Beyond this the check
// degrades to a warning instead of risking excessive memory use.
const dependencyCycleMaxEdges = 1_000_000

// dependencyCycleTables are the tables cycle detection traverses, matching
// issueops.DetectCyclesInTx and doctorDependencyUnionSQL: wisp edges
// participate in the same blocking graph as durable ones.
var dependencyCycleTables = []string{"dependencies", "wisp_dependencies"}

func checkDependencyCyclesWithStore(store *dolt.DoltStore) DoctorCheck {
	db := store.UnderlyingDB()

	edges, check := loadDependencyEdges(db, dependencyCyclePageSize, dependencyCycleMaxEdges)
	if check != nil {
		return *check
	}

	cycleNodes := dependencyCycleNodes(edges)

	if len(cycleNodes) == 0 {
		return DoctorCheck{
			Name:    "Dependency Cycles",
			Status:  StatusOK,
			Message: "No circular dependencies detected",
		}
	}

	return DoctorCheck{
		Name:    "Dependency Cycles",
		Status:  StatusError,
		Message: fmt.Sprintf("Found %d circular dependency cycle(s)", len(cycleNodes)),
		Detail:  fmt.Sprintf("First cycle involves: %s", cycleNodes[0]),
		Fix:     "Run 'bd dep cycles' to see full cycle paths, then 'bd dep remove' to break cycles",
	}
}

// loadDependencyEdges reads the blocking edges of both dependency tables as
// one adjacency map, one bounded page per query. Cycle detection used to run
// as a single WITH RECURSIVE path enumeration, which is exponential in dense
// graphs and exceeded per-query read timeouts on shared dolt sql-servers.
//
// Only blocking edge types are traversed, matching issueops.DetectCyclesInTx
// and the cycle prevention on 'bd dep add': non-blocking types (tracks,
// related, discovered-from, ...) legitimately form loops and previously made
// this check disagree with the 'bd dep cycles' command its fix hint points at.
//
// All pages of both tables run inside one transaction so the whole graph is
// read from a single snapshot; on a shared server, per-statement implicit
// transactions could interleave with concurrent edge writes and paginate over
// a shifting id sequence, dropping or duplicating edges.
// Returns a non-nil DoctorCheck on failure.
func loadDependencyEdges(db *sql.DB, pageSize, maxEdges int) (map[string][]string, *DoctorCheck) {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, dependencyCycleWarning("Unable to check for cycles", err.Error())
	}
	defer func() { _ = tx.Rollback() }()

	edges := make(map[string][]string)
	edgeCount := 0
	for _, table := range dependencyCycleTables {
		var check *DoctorCheck
		if dependencyIDPaginationUsable(ctx, tx, table) {
			check = loadDependencyEdgePages(ctx, tx, table, pageSize, maxEdges, &edgeCount, edges)
		} else {
			check = loadDependencyEdgeScan(ctx, tx, table, maxEdges, &edgeCount, edges)
		}
		if check != nil {
			return nil, check
		}
	}
	return edges, nil
}

// dependencyIDPaginationUsable reports whether `WHERE id > ? ORDER BY id`
// keyset pagination visits every row of table exactly once. That requires an
// id column with no NULLs, and doctor cannot assume either: supported
// databases exist where dependencies.id never materialized (#4690) or is
// mid-backfill NULL (ensureDependenciesIDColumn), and doctor opens the store
// read-only without running the migration chain that repairs those shapes.
// NULL never satisfies `id > ?`, so paginating over such rows would silently
// drop their edges from the graph.
func dependencyIDPaginationUsable(ctx context.Context, tx *sql.Tx, table string) bool {
	var idColumns int
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = ? AND column_name = 'id'`,
		table).Scan(&idColumns)
	if err != nil || idColumns == 0 {
		return false
	}
	var one int
	//nolint:gosec // G202: table is hardcoded in dependencyCycleTables
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM `+table+` WHERE id IS NULL LIMIT 1`).Scan(&one)
	return errors.Is(err, sql.ErrNoRows)
}

// loadDependencyEdgePages loads table via id keyset pagination, bounding the
// work of every query. dependencies.id is a deterministic primary key on the
// current schema (migration 0050); wisp_dependencies.id has been a primary
// key since the table's creation (migration 0021).
func loadDependencyEdgePages(ctx context.Context, tx *sql.Tx, table string, pageSize, maxEdges int, edgeCount *int, edges map[string][]string) *DoctorCheck {
	lastID := ""
	for {
		rowsRead, check := loadDependencyEdgePage(ctx, tx, table, pageSize, maxEdges, &lastID, edgeCount, edges)
		if check != nil {
			return check
		}
		if rowsRead < pageSize {
			return nil
		}
	}
}

func loadDependencyEdgePage(ctx context.Context, tx *sql.Tx, table string, pageSize, maxEdges int, lastID *string, edgeCount *int, edges map[string][]string) (int, *DoctorCheck) {
	//nolint:gosec // G202: table is hardcoded in dependencyCycleTables
	rows, err := tx.QueryContext(ctx, `
		SELECT id, issue_id, `+doctorDependencyTargetExpr+` AS depends_on_id, type
		FROM `+table+`
		WHERE id > ?
		ORDER BY id
		LIMIT ?`, *lastID, pageSize)
	if err != nil {
		return 0, dependencyCycleWarning("Unable to check for cycles", table+": "+err.Error())
	}
	defer rows.Close()

	rowsRead := 0
	for rows.Next() {
		var id, issueID, depType string
		var dependsOnID sql.NullString
		if err := rows.Scan(&id, &issueID, &dependsOnID, &depType); err != nil {
			// Must fail loudly: a skipped row would leave rowsRead short of
			// pageSize, silently ending pagination with a truncated graph.
			return 0, dependencyCycleWarning("Unable to check for cycles", "scan "+table+": "+err.Error())
		}
		rowsRead++
		*lastID = id
		if check := addBlockingDependencyEdge(edges, issueID, dependsOnID, depType, maxEdges, edgeCount); check != nil {
			return 0, check
		}
	}
	if err := rows.Err(); err != nil {
		return 0, dependencyCycleWarning("Row iteration error", err.Error())
	}
	return rowsRead, nil
}

// loadDependencyEdgeScan streams table in one query, the same single linear
// pass issueops.AppendBlockingGraphInTx makes. It is the fallback for schema
// shapes where id keyset pagination would be unsound; the exponential work
// this check must avoid was the recursive path enumeration, not the plain
// edge scan 'bd dep cycles' itself relies on.
func loadDependencyEdgeScan(ctx context.Context, tx *sql.Tx, table string, maxEdges int, edgeCount *int, edges map[string][]string) *DoctorCheck {
	//nolint:gosec // G202: table is hardcoded in dependencyCycleTables
	rows, err := tx.QueryContext(ctx, `
		SELECT issue_id, `+doctorDependencyTargetExpr+` AS depends_on_id, type
		FROM `+table)
	if err != nil {
		return dependencyCycleWarning("Unable to check for cycles", table+": "+err.Error())
	}
	defer rows.Close()

	for rows.Next() {
		var issueID, depType string
		var dependsOnID sql.NullString
		if err := rows.Scan(&issueID, &dependsOnID, &depType); err != nil {
			return dependencyCycleWarning("Unable to check for cycles", "scan "+table+": "+err.Error())
		}
		if check := addBlockingDependencyEdge(edges, issueID, dependsOnID, depType, maxEdges, edgeCount); check != nil {
			return check
		}
	}
	if err := rows.Err(); err != nil {
		return dependencyCycleWarning("Row iteration error", err.Error())
	}
	return nil
}

// addBlockingDependencyEdge adds one row to the adjacency map if it is a
// blocking edge. The type filter runs client-side like
// issueops.AppendBlockingGraphInTx: a WHERE on type would let one paginated
// page scan an unbounded id range when blocking edges are sparse, defeating
// the bounded-work-per-query guarantee. Returns the graph-too-large warning
// once the in-memory graph would exceed maxEdges.
func addBlockingDependencyEdge(edges map[string][]string, issueID string, dependsOnID sql.NullString, depType string, maxEdges int, edgeCount *int) *DoctorCheck {
	blocking := types.DependencyType(depType) == types.DepBlocks ||
		types.DependencyType(depType) == types.DepConditionalBlocks
	if !blocking || !dependsOnID.Valid {
		return nil
	}
	if *edgeCount >= maxEdges {
		return dependencyCycleWarning("Unable to check for cycles",
			fmt.Sprintf("dependency graph too large (more than %d edges)", maxEdges))
	}
	edges[issueID] = append(edges[issueID], dependsOnID.String)
	*edgeCount++
	return nil
}

func dependencyCycleWarning(message, detail string) *DoctorCheck {
	return &DoctorCheck{
		Name:    "Dependency Cycles",
		Status:  StatusWarning,
		Message: message,
		Detail:  detail,
	}
}
