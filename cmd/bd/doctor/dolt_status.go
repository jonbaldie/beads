package doctor

import (
	"context"
	"fmt"
	"strings"

	// MySQL driver for connecting to dolt sql-server
	_ "github.com/go-sql-driver/mysql"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
)

// checkStatusWithDB reports uncommitted changes in Dolt using an existing connection.
// Separated from CheckDoltStatus to allow connection reuse across checks.
func checkStatusWithDB(conn *doltConn) DoctorCheck {
	ctx := context.Background()

	// Check dolt_status for uncommitted changes
	rows, err := conn.db.QueryContext(ctx, "SELECT table_name, staged, status FROM dolt_status")
	if err != nil {
		return DoctorCheck{
			Name:     "Dolt Status",
			Status:   StatusWarning,
			Message:  "Could not query dolt_status",
			Detail:   err.Error(),
			Category: CategoryData,
		}
	}
	defer rows.Close()

	scanned, err := scanDoltStatus(rows)
	if err != nil {
		return DoctorCheck{
			Name:     "Dolt Status",
			Status:   StatusWarning,
			Message:  "Row iteration error",
			Detail:   err.Error(),
			Category: CategoryData,
		}
	}
	changes := describeUncommittedTables(scanned)

	if len(changes) > 0 {
		return DoctorCheck{
			Name:     "Dolt Status",
			Status:   StatusWarning,
			Message:  fmt.Sprintf("%d uncommitted change(s)", len(changes)),
			Detail:   fmt.Sprintf("Changes: %v", changes),
			Fix:      "Run 'bd vc commit -m \"commit changes\"' to commit, or changes will auto-commit on next bd command",
			Category: CategoryData,
		}
	}

	return DoctorCheck{
		Name:     "Dolt Status",
		Status:   StatusOK,
		Message:  "Clean working set",
		Category: CategoryData,
	}
}

// CheckDoltStatus reports uncommitted changes in Dolt.
// This is the standalone entry point; RunDoltHealthChecks is preferred
// for coordinated access.
func CheckDoltStatus(path string) DoctorCheck {
	beadsDir := ResolveBeadsDirForRepo(path)

	// Only run for Dolt backend
	if !IsDoltBackend(beadsDir) {
		return DoctorCheck{
			Name:     "Dolt Status",
			Status:   StatusOK,
			Message:  "N/A (not using Dolt backend)",
			Category: CategoryData,
		}
	}

	conn, err := openDoltConn(beadsDir)
	if err != nil {
		return DoctorCheck{
			Name:     "Dolt Status",
			Status:   StatusWarning,
			Message:  "Could not check Dolt status",
			Detail:   err.Error(),
			Category: CategoryData,
		}
	}
	defer conn.Close()

	return checkStatusWithDB(conn)
}

// checkPhantomDatabases detects phantom catalog entries from naming convention
// changes (beads_* prefix or *_beads suffix) that don't match the configured
// database. These phantom entries can cause INFORMATION_SCHEMA queries to crash
// (GH#2051). Complementary to checkStaleDatabases in server.go, which targets
// test/polecat leftovers with different prefixes.
func checkPhantomDatabases(conn *doltConn) DoctorCheck {
	phantoms, failMsg, err := listPhantomDatabases(conn)
	if err != nil {
		return DoctorCheck{
			Name:     "Phantom Databases",
			Status:   StatusWarning,
			Message:  failMsg,
			Detail:   err.Error(),
			Category: CategoryData,
		}
	}
	return reportPhantomDatabases(phantoms)
}

func listPhantomDatabases(conn *doltConn) ([]string, string, error) {
	rows, err := conn.db.Query("SHOW DATABASES")
	if err != nil {
		return nil, "Could not query databases", err
	}
	defer rows.Close()
	configuredDB := configfile.DefaultDoltDatabase
	if conn.cfg != nil {
		configuredDB = conn.cfg.GetDoltDatabase()
	}
	var phantoms []string
	for rows.Next() {
		var dbName string
		if err := rows.Scan(&dbName); err != nil {
			continue
		}
		if isPhantomDatabaseName(dbName, configuredDB) {
			phantoms = append(phantoms, dbName)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "Row iteration error", err
	}
	return phantoms, "", nil
}

func isPhantomDatabaseName(dbName, configuredDB string) bool {
	if dbName == "information_schema" || dbName == "mysql" || dbName == configuredDB {
		return false
	}
	return strings.HasPrefix(dbName, "beads_") || strings.HasSuffix(dbName, "_beads")
}

func reportPhantomDatabases(phantoms []string) DoctorCheck {
	if len(phantoms) > 0 {
		return DoctorCheck{
			Name:     "Phantom Databases",
			Status:   StatusWarning,
			Message:  fmt.Sprintf("%d phantom database(s) detected: %s", len(phantoms), strings.Join(phantoms, ", ")),
			Detail:   fmt.Sprintf("Phantom entries: %v", phantoms),
			Fix:      "Restart Dolt server to flush phantom entries. See GH#2051.",
			Category: CategoryData,
		}
	}
	return DoctorCheck{
		Name:     "Phantom Databases",
		Status:   StatusOK,
		Message:  "No phantom databases detected",
		Category: CategoryData,
	}
}

// probeForCorrectDatabase checks if another database on the same server has the
// expected beads tables. Returns the database name if found, empty string otherwise.
// Used by checkSchemaWithDB to detect pre-#2142 migrations where dolt_database
// was not written to metadata.json (GH#2160).
func probeForCorrectDatabase(conn *doltConn) string {
	ctx := context.Background()
	return firstDatabaseWithIssues(conn, ctx, listProbeDatabases(conn, ctx))
}

func listProbeDatabases(conn *doltConn, ctx context.Context) []string {
	rows, err := conn.db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil
	}
	defer rows.Close()
	configuredDB := configfile.DefaultDoltDatabase
	if conn.cfg != nil {
		configuredDB = conn.cfg.GetDoltDatabase()
	}
	skip := map[string]bool{
		"information_schema": true,
		"mysql":              true,
		configuredDB:         true, // Already checked this one
	}
	var candidates []string
	for rows.Next() {
		var dbName string
		if err := rows.Scan(&dbName); err != nil {
			continue
		}
		if skip[dbName] || isProbeSkipDatabase(dbName) {
			continue
		}
		candidates = append(candidates, dbName)
	}
	return candidates
}

func isProbeSkipDatabase(dbName string) bool {
	return strings.HasPrefix(dbName, "testdb_") ||
		strings.HasPrefix(dbName, "doctest_") ||
		strings.HasPrefix(dbName, "doctortest_")
}

func firstDatabaseWithIssues(conn *doltConn, ctx context.Context, candidates []string) string {
	for _, dbName := range candidates {
		var count int
		//nolint:gosec // G201: dbName is from SHOW DATABASES, not user input
		err := conn.db.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM `%s`.issues LIMIT 1", dbName)).Scan(&count)
		if err == nil {
			return dbName
		}
	}
	return ""
}

// checkSharedServerHealth verifies shared server configuration and health.
func checkSharedServerHealth(beadsDir string) DoctorCheck {
	if !doltserver.IsSharedServerMode() {
		return DoctorCheck{
			Name:     "Shared Server",
			Status:   StatusOK,
			Message:  "N/A (per-project mode)",
			Category: CategoryRuntime,
		}
	}

	sharedDir, err := doltserver.SharedServerDir()
	if err != nil {
		return DoctorCheck{
			Name:     "Shared Server",
			Status:   StatusError,
			Message:  "Cannot access shared server directory",
			Detail:   err.Error(),
			Fix:      "Ensure ~/.beads/shared-server/ is writable",
			Category: CategoryRuntime,
		}
	}

	state, err := doltserver.IsRunning(sharedDir)
	if err != nil {
		return DoctorCheck{
			Name:     "Shared Server",
			Status:   StatusWarning,
			Message:  "Cannot check shared server status",
			Detail:   err.Error(),
			Category: CategoryRuntime,
		}
	}

	if state == nil || !state.Running {
		return DoctorCheck{
			Name:     "Shared Server",
			Status:   StatusWarning,
			Message:  "Shared server not running (will auto-start on next bd command)",
			Detail:   fmt.Sprintf("Server directory: %s", sharedDir),
			Fix:      "Run 'bd dolt start' to start the shared server",
			Category: CategoryRuntime,
		}
	}

	cfg, _ := configfile.Load(beadsDir)
	dbName := configfile.DefaultDoltDatabase
	if cfg != nil {
		dbName = cfg.GetDoltDatabase()
	}

	return DoctorCheck{
		Name:     "Shared Server",
		Status:   StatusOK,
		Message:  fmt.Sprintf("Running (PID %d, port %d), database: %s", state.PID, state.Port, dbName),
		Detail:   fmt.Sprintf("Server directory: %s", sharedDir),
		Category: CategoryRuntime,
	}
}

// CheckCorruptManifest reports the GH#3290 corrupt-manifest condition: the
// dolt server log tail shows "root hash doesn't exist" and the affected
// databases hold no recoverable data (empty journal, empty oldgen). The
// repair (backup + reinitialize) is destructive, so it only runs via
// bd doctor --fix with explicit confirmation — never automatically
// (bd-6dnrw.6).
func CheckCorruptManifest(path string) DoctorCheck {
	beadsDir := ResolveBeadsDirForRepo(path)
	if !IsDoltBackend(beadsDir) {
		return DoctorCheck{
			Name:     "Corrupt Manifest",
			Status:   StatusOK,
			Message:  "N/A (not using Dolt backend)",
			Category: CategoryRuntime,
		}
	}

	dirs, err := doltserver.DetectCorruptManifest(beadsDir)
	if err != nil {
		return DoctorCheck{
			Name:     "Corrupt Manifest",
			Status:   StatusWarning,
			Message:  "Could not scan for corrupt-manifest state",
			Detail:   err.Error(),
			Category: CategoryRuntime,
		}
	}
	if len(dirs) == 0 {
		return DoctorCheck{
			Name:     "Corrupt Manifest",
			Status:   StatusOK,
			Message:  "No corrupt-manifest state detected",
			Category: CategoryRuntime,
		}
	}
	return DoctorCheck{
		Name:     "Corrupt Manifest",
		Status:   StatusError,
		Message:  fmt.Sprintf("%d dolt database(s) have a corrupt manifest with no recoverable data (GH#3290)", len(dirs)),
		Detail:   strings.Join(dirs, "\n"),
		Fix:      "Run 'bd doctor --fix' to back up the corrupt database(s) and reinitialize",
		Category: CategoryRuntime,
	}
}
