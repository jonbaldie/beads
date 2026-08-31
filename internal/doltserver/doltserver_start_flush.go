// Package doltserver manages the lifecycle of a local dolt sql-server process.
// It provides transparent auto-start so that `bd init` and `bd <command>` work
// without manual server management.
//
// Port assignment uses OS-assigned ephemeral ports by default. When no explicit
// port is configured (env var, config.yaml, metadata.json), Start() asks the OS
// for a free port via net.Listen(":0"), passes it to dolt sql-server, and writes
// the actual port to dolt-server.port. This eliminates the birthday-problem
// collisions that plagued the old hash-derived port scheme (GH#2098, GH#2372).
//
// Users with explicit port config via BEADS_DOLT_SERVER_PORT env var or
// config.yaml always use that port instead, with conflict detection via
// reclaimPort.
//
// Server state files (PID, port, log, lock) live in the .beads/ directory.
package doltserver

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/jonbaldie/beads/internal/storage/doltutil"
)

// EnsureGlobalDatabase connects to the shared Dolt server and creates the
// beads_global database if it doesn't already exist. This is idempotent and
// safe to call on every shared server init. Schema initialization and config
// seeding (issue prefix, project ID) are handled by the store layer when the
// global database is first opened with CreateIfMissing=true.
//
// Returns nil if the database already exists or was successfully created.
func EnsureGlobalDatabase(host string, port int, user, password string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dsn := doltutil.ServerDSN{
		Host:     host,
		Port:     port,
		User:     user,
		Password: password,
	}.String()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("ensure global db: failed to open connection: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(10 * time.Second)

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ensure global db: server not reachable: %w", err)
	}

	// CREATE DATABASE IF NOT EXISTS is idempotent — safe on every call.
	// GlobalDatabaseName is a constant ("beads_global"), not user input.
	_, err = db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`", GlobalDatabaseName)) //nolint:gosec // G201: constant database name
	if err != nil {
		errLower := strings.ToLower(err.Error())
		if !strings.Contains(errLower, "database exists") && !strings.Contains(errLower, "1007") {
			return fmt.Errorf("ensure global db: failed to create %s: %w", GlobalDatabaseName, err)
		}
	}

	return nil
}

// FlushWorkingSet connects to the running Dolt server and commits any uncommitted
// working set changes across all databases. This prevents data loss when the server
// is about to be stopped or restarted. Returns nil if there's nothing to flush or
// if the server is not reachable (best-effort).
func FlushWorkingSet(host string, port int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dsn := doltutil.ServerDSN{
		Host: host,
		Port: port,
		User: "root",
	}.String()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("flush: failed to open connection: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(10 * time.Second)

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("flush: server not reachable: %w", err)
	}

	databases, err := listFlushDatabases(ctx, db)
	if err != nil {
		return err
	}

	if len(databases) == 0 {
		return nil
	}

	flushed := flushWorkingSetDatabases(ctx, db, databases)

	if flushed > 0 {
		fmt.Fprintf(os.Stderr, "Flushed working set for %d database(s) before server stop\n", flushed)
	}
	return nil
}

func listFlushDatabases(ctx context.Context, db *sql.DB) ([]string, error) {
	// List all databases, skipping system databases.
	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, fmt.Errorf("flush: failed to list databases: %w", err)
	}
	defer rows.Close()
	var databases []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		if isDoltSystemDatabase(name) {
			continue
		}
		databases = append(databases, name)
	}
	return databases, nil
}

func isDoltSystemDatabase(name string) bool {
	return name == "information_schema" || name == "mysql" || name == "performance_schema"
}

func flushWorkingSetDatabases(ctx context.Context, db *sql.DB, databases []string) int {
	var flushed int
	for _, dbName := range databases {
		if flushWorkingSetDatabase(ctx, db, dbName) {
			flushed++
		}
	}
	return flushed
}

func flushWorkingSetDatabase(ctx context.Context, db *sql.DB, dbName string) bool {
	// Check for uncommitted changes via dolt_status.
	var hasChanges bool
	row := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) > 0 FROM `%s`.dolt_status", dbName))
	if err := row.Scan(&hasChanges); err != nil {
		// dolt_status may not exist for non-beads databases; skip.
		return false
	}
	if !hasChanges {
		return false
	}

	// Commit all uncommitted changes.
	if _, err := db.ExecContext(ctx, fmt.Sprintf("USE `%s`", dbName)); err != nil {
		fmt.Fprintf(os.Stderr, "flush: failed to USE %s: %v\n", dbName, err)
		return false
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'auto-flush: commit working set before server stop')"); err != nil {
		errStr := strings.ToLower(err.Error())
		if strings.Contains(errStr, "nothing to commit") || strings.Contains(errStr, "no changes") {
			return false
		}
		fmt.Fprintf(os.Stderr, "flush: failed to commit %s: %v\n", dbName, err)
		return false
	}
	return true
}

// Stop is idempotent: when the server is already stopped it returns
// ErrServerNotRunning after cleaning up any leftover state files.
// Callers should use errors.Is(err, ErrServerNotRunning) to distinguish
// this expected condition from real failures.
