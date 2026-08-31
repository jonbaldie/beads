package doctor

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strings"
	"time"

	// Import MySQL driver for server mode connections
	_ "github.com/go-sql-driver/mysql"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage/doltutil"
)

// checkStaleDatabases identifies leftover test/polecat databases on the shared server.
// These waste memory and can degrade performance under concurrent load.
func checkStaleDatabases(db *sql.DB) DoctorCheck {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return DoctorCheck{
			Name:     "Stale Databases",
			Status:   StatusError,
			Message:  "Failed to list databases",
			Detail:   err.Error(),
			Category: CategoryMaintenance,
		}
	}
	defer rows.Close()

	stale, total := collectStaleDatabases(rows)
	if err := rows.Err(); err != nil {
		return DoctorCheck{
			Name:     "Stale Databases",
			Status:   StatusWarning,
			Message:  "Row iteration error",
			Detail:   err.Error(),
			Category: CategoryMaintenance,
		}
	}

	if len(stale) == 0 {
		return DoctorCheck{
			Name:     "Stale Databases",
			Status:   StatusOK,
			Message:  fmt.Sprintf("%d databases, no stale test/polecat databases found", total),
			Category: CategoryMaintenance,
		}
	}

	return DoctorCheck{
		Name:     "Stale Databases",
		Status:   StatusWarning,
		Message:  fmt.Sprintf("%d stale test/polecat databases found", len(stale)),
		Detail:   formatStaleDatabaseDetail(stale, total),
		Fix:      "Run 'bd dolt clean-databases' to drop stale databases",
		Category: CategoryMaintenance,
	}
}

func collectStaleDatabases(rows *sql.Rows) ([]string, int) {
	var stale []string
	var total int
	for rows.Next() {
		var dbName string
		if err := rows.Scan(&dbName); err != nil {
			continue
		}
		total++
		if isStaleDatabaseName(dbName) {
			stale = append(stale, dbName)
		}
	}
	return stale, total
}

func isStaleDatabaseName(dbName string) bool {
	if knownProductionDatabases[dbName] {
		return false
	}
	for _, prefix := range staleDatabasePrefixes {
		if strings.HasPrefix(dbName, prefix) {
			return true
		}
	}
	return false
}

func formatStaleDatabaseDetail(stale []string, total int) string {
	detail := fmt.Sprintf("Found %d stale databases (of %d total):\n", len(stale), total)
	shown := len(stale)
	if shown > 10 {
		shown = 10
	}
	for _, name := range stale[:shown] {
		detail += fmt.Sprintf("  %s\n", name)
	}
	if len(stale) > 10 {
		detail += fmt.Sprintf("  ... and %d more\n", len(stale)-10)
	}
	return strings.TrimSpace(detail)
}

// checkServerReachable checks if the server is reachable via TCP
func checkServerReachable(host string, port int) DoctorCheck {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	_, err := doltserver.ProbeSQLServer("tcp", addr, 5*time.Second)
	if err != nil {
		return DoctorCheck{
			Name:     "Server Reachable",
			Status:   StatusError,
			Message:  fmt.Sprintf("Cannot connect to %s", addr),
			Detail:   err.Error(),
			Fix:      "Ensure dolt sql-server is running and accessible",
			Category: CategoryFederation,
		}
	}

	return DoctorCheck{
		Name:     "Server Reachable",
		Status:   StatusOK,
		Message:  fmt.Sprintf("Connected to %s", addr),
		Category: CategoryFederation,
	}
}

// checkDoltVersion connects to the server and checks if it's a Dolt server
// Returns the DoctorCheck and an open database connection (caller must close)
func checkDoltVersion(cfg *configfile.Config, beadsDir string) (DoctorCheck, *sql.DB) {
	host := cfg.GetDoltServerHost()
	port := doltserver.DefaultConfig(beadsDir).Port
	user := cfg.GetDoltServerUser()

	// Resolve password the same way the CRUD path does: BEADS_DOLT_PASSWORD env
	// takes precedence (checked inside GetDoltServerPasswordForPort), with a
	// fallback to ~/.config/beads/credentials keyed by [host:port]. Using the
	// resolved runtime port is required because the port file may differ from
	// the metadata port (bd-h5k7).
	password := cfg.GetDoltServerPasswordForPort(port)

	// Build DSN without database (just to test server connectivity)
	connStr := doltutil.ServerDSN{
		Host:     host,
		Port:     port,
		User:     user,
		Password: password,
		TLS:      cfg.GetDoltServerTLS(),
	}.String()

	db, err := sql.Open("mysql", connStr)
	if err != nil {
		return DoctorCheck{
			Name:     "Dolt Version",
			Status:   StatusError,
			Message:  "Failed to open connection",
			Detail:   err.Error(),
			Fix:      "Check MySQL driver and connection settings",
			Category: CategoryFederation,
		}, nil
	}

	// Set connection pool limits
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(30 * time.Second)

	// Test connectivity
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close() // Best effort cleanup
		return DoctorCheck{
			Name:     "Dolt Version",
			Status:   StatusError,
			Message:  "Server not responding",
			Detail:   err.Error(),
			Fix:      "Ensure dolt sql-server is running",
			Category: CategoryFederation,
		}, nil
	}

	// Query Dolt version
	var version string
	err = db.QueryRowContext(ctx, "SELECT dolt_version()").Scan(&version)
	if err != nil {
		// If dolt_version() doesn't exist, it's not a Dolt server
		if strings.Contains(err.Error(), "Unknown") || strings.Contains(err.Error(), "doesn't exist") {
			_ = db.Close() // Best effort cleanup
			return DoctorCheck{
				Name:     "Dolt Version",
				Status:   StatusError,
				Message:  "Server is not Dolt",
				Detail:   "dolt_version() function not found - this may be a MySQL server, not Dolt",
				Fix:      "Ensure you're connecting to a Dolt sql-server, not vanilla MySQL",
				Category: CategoryFederation,
			}, nil
		}
		_ = db.Close() // Best effort cleanup
		return DoctorCheck{
			Name:     "Dolt Version",
			Status:   StatusError,
			Message:  "Failed to query version",
			Detail:   err.Error(),
			Category: CategoryFederation,
		}, nil
	}

	return DoctorCheck{
		Name:     "Dolt Version",
		Status:   StatusOK,
		Message:  fmt.Sprintf("Dolt %s", version),
		Category: CategoryFederation,
	}, db
}

// checkDatabaseExists checks if the beads database exists
func checkDatabaseExists(db *sql.DB, database string) DoctorCheck {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	legacyName, err := validateDoctorDatabaseName(database)
	if err != nil {
		return DoctorCheck{
			Name:     "Database Exists",
			Status:   StatusError,
			Message:  fmt.Sprintf("Invalid database name '%s'", database),
			Detail:   "Database name must be alphanumeric with underscores only",
			Category: CategoryFederation,
		}
	}

	// Use SHOW DATABASES instead of INFORMATION_SCHEMA.SCHEMATA to avoid
	// crashing on phantom catalog entries (R-006, GH#2051, GH#2091).
	lookup := findDatabase(ctx, db, database)
	if lookup.queryErr != nil {
		return DoctorCheck{
			Name:     "Database Exists",
			Status:   StatusError,
			Message:  "Failed to query databases",
			Detail:   lookup.queryErr.Error(),
			Category: CategoryFederation,
		}
	}
	if lookup.rowsErr != nil {
		return DoctorCheck{
			Name:     "Database Exists",
			Status:   StatusWarning,
			Message:  "Row iteration error",
			Detail:   lookup.rowsErr.Error(),
			Category: CategoryFederation,
		}
	}

	if !lookup.found {
		return DoctorCheck{
			Name:     "Database Exists",
			Status:   StatusError,
			Message:  fmt.Sprintf("Database '%s' not found", database),
			Fix:      fmt.Sprintf("Run 'bd bootstrap' to recover the existing '%s' database safely. Use 'bd init' only for brand-new projects.", database),
			Category: CategoryFederation,
		}
	}

	// Switch to the database
	// Note: USE cannot use parameterized queries, but we validated the identifier above.
	// Backtick-quote to support hyphenated legacy names (GH#2142).
	if _, err := db.ExecContext(ctx, "USE `"+database+"`"); err != nil { // #nosec G201 - database validated by validateDoctorDatabaseName
		return DoctorCheck{
			Name:     "Database Exists",
			Status:   StatusError,
			Message:  fmt.Sprintf("Cannot access database '%s'", database),
			Detail:   err.Error(),
			Category: CategoryFederation,
		}
	}

	// Warn about hyphenated names — functional but new projects use underscores
	if legacyName {
		return DoctorCheck{
			Name:     "Database Exists",
			Status:   StatusWarning,
			Message:  fmt.Sprintf("Database '%s' uses hyphens (legacy naming)", database),
			Detail:   "New projects use underscores. To migrate: export data, run 'bd init --force', re-import.",
			Category: CategoryFederation,
		}
	}

	return DoctorCheck{
		Name:     "Database Exists",
		Status:   StatusOK,
		Message:  fmt.Sprintf("Database '%s' accessible", database),
		Category: CategoryFederation,
	}
}

func validateDoctorDatabaseName(database string) (bool, error) {
	if isValidIdentifier(database) {
		return false, nil
	}
	if strings.ContainsRune(database, '-') && isValidIdentifier(strings.ReplaceAll(database, "-", "_")) {
		return true, nil
	}
	return false, fmt.Errorf("invalid database name")
}

type databaseLookup struct {
	found    bool
	queryErr error
	rowsErr  error
}

func findDatabase(ctx context.Context, db *sql.DB, database string) databaseLookup {
	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return databaseLookup{queryErr: err}
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var dbName string
		if err := rows.Scan(&dbName); err != nil {
			continue
		}
		if dbName == database {
			found = true
			break
		}
	}
	return databaseLookup{found: found, rowsErr: rows.Err()}
}

// isValidIdentifier checks if a string is a valid SQL identifier
// (alphanumeric and underscore only, doesn't start with a number)
func isValidIdentifier(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i, c := range s {
		if i == 0 && isASCIIDigit(c) {
			return false
		}
		if !isIdentifierCharacter(c) {
			return false
		}
	}
	return true
}

func isASCIIDigit(c rune) bool {
	return c >= '0' && c <= '9'
}

func isIdentifierCharacter(c rune) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '_'
}

// checkSchemaCompatible checks if the beads tables are queryable
func checkSchemaCompatible(db *sql.DB, database string) DoctorCheck {
	// Note: database parameter reserved for future use (e.g., multi-database support)
	_ = database
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Try to query the issues table
	var count int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM issues").Scan(&count)
	if err != nil {
		if strings.Contains(err.Error(), "doesn't exist") || strings.Contains(err.Error(), "Unknown table") {
			return DoctorCheck{
				Name:     "Schema Compatible",
				Status:   StatusError,
				Message:  "Issues table not found",
				Fix:      "Run 'bd init' to create schema",
				Category: CategoryFederation,
			}
		}
		return DoctorCheck{
			Name:     "Schema Compatible",
			Status:   StatusError,
			Message:  "Cannot query issues table",
			Detail:   err.Error(),
			Category: CategoryFederation,
		}
	}

	// Query metadata table for bd_version
	var bdVersion string
	err = db.QueryRowContext(ctx, "SELECT value FROM local_metadata WHERE `key` = 'bd_version'").Scan(&bdVersion)
	if err != nil && err != sql.ErrNoRows {
		if strings.Contains(err.Error(), "doesn't exist") || strings.Contains(err.Error(), "Unknown table") {
			return DoctorCheck{
				Name:     "Schema Compatible",
				Status:   StatusWarning,
				Message:  fmt.Sprintf("%d issues found (no metadata table)", count),
				Fix:      "Run 'bd migrate' to update schema",
				Category: CategoryFederation,
			}
		}
	}

	detail := fmt.Sprintf("%d issues", count)
	if bdVersion != "" {
		detail = fmt.Sprintf("%d issues (bd %s)", count, bdVersion)
	}

	return DoctorCheck{
		Name:     "Schema Compatible",
		Status:   StatusOK,
		Message:  detail,
		Category: CategoryFederation,
	}
}

// checkConnectionPool checks the connection pool health
func checkConnectionPool(db *sql.DB) DoctorCheck {
	stats := db.Stats()

	// Report pool statistics
	detail := fmt.Sprintf("open: %d, in_use: %d, idle: %d",
		stats.OpenConnections,
		stats.InUse,
		stats.Idle,
	)

	// Check for connection errors
	if stats.MaxIdleClosed > 0 || stats.MaxLifetimeClosed > 0 {
		detail += fmt.Sprintf("\nclosed: idle=%d, lifetime=%d",
			stats.MaxIdleClosed,
			stats.MaxLifetimeClosed,
		)
	}

	return DoctorCheck{
		Name:     "Connection Pool",
		Status:   StatusOK,
		Message:  "Pool healthy",
		Detail:   detail,
		Category: CategoryFederation,
	}
}
