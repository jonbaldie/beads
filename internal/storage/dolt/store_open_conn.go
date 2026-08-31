// Package dolt implements the storage interface using Dolt (versioned MySQL-compatible database).
//
// Dolt provides native version control for SQL data with cell-level merge, history queries,
// and federation via Dolt remotes. The database itself is version-controlled.
//
// Dolt capabilities:
//   - Native version control (commit, push, pull, branch, merge)
//   - Time-travel queries via AS OF and dolt_history_* tables
//   - Cell-level merge for conflict resolution
//   - Multi-writer via dolt sql-server (federation, pure Go)
//
// All operations require a running dolt sql-server. Connect via MySQL protocol (pure Go).
package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"

	"github.com/jonbaldie/beads/internal/storage/schema"
)

// openServerConnection connects to (and if needed creates) the target database
// on a dolt sql-server via MySQL protocol. See serverConnFacts for what the
// returned facts mean and why they are not a single bool.
func openServerConnection(ctx context.Context, cfg *Config) (*sql.DB, string, serverConnFacts, error) {
	connStr := buildServerDSN(cfg, cfg.Database)

	db, err := sql.Open("mysql", connStr)
	if err != nil {
		return nil, "", serverConnFacts{}, fmt.Errorf("failed to open Dolt server connection: %w", err)
	}

	// Configure the pool. *sql.DB is safe for concurrent use and manages its
	// own pool — the same Store reuses these connections across every query
	// for the lifetime of the daemon, rather than opening a fresh one each
	// time (which used to show up as endless NewConnection/ConnectionClosed
	// pairs in dolt-server.log).
	applyPoolLimits(db, cfg)

	// Close the pool on any failure path below; cleared at the success return.
	connReady := false
	defer func() {
		if !connReady {
			_ = db.Close()
		}
	}()

	if cfg.Gateway {
		if err := pingGatewayDatabase(ctx, db, cfg); err != nil {
			return nil, "", serverConnFacts{}, fmt.Errorf("failed to connect to gateway server %s:%d (database %q): %w",
				cfg.ServerHost, cfg.ServerPort, cfg.Database, err)
		}
		connReady = true
		return db, connStr, serverConnFacts{}, nil
	}

	facts, err := openOrCreateServerDatabase(ctx, cfg, db)
	if err != nil {
		return nil, "", serverConnFacts{}, err
	}
	connReady = true
	return db, connStr, facts, nil
}

func pingGatewayDatabase(ctx context.Context, db *sql.DB, _ *Config) error {
	return db.PingContext(ctx)
}

func openOrCreateServerDatabase(ctx context.Context, cfg *Config, db *sql.DB) (serverConnFacts, error) {
	initDB, err := sql.Open("mysql", buildServerDSN(cfg, ""))
	if err != nil {
		return serverConnFacts{}, fmt.Errorf("failed to open init connection: %w", err)
	}
	defer func() { _ = initDB.Close() }()

	if err := validateServerDatabaseTarget(cfg); err != nil {
		return serverConnFacts{}, err
	}
	dbExists, err := databaseExistsOnServer(ctx, initDB, cfg.Database)
	if err != nil {
		return serverConnFacts{}, fmt.Errorf("failed to check if database %q exists on server %s:%d: %w",
			cfg.Database, cfg.ServerHost, cfg.ServerPort, err)
	}
	created, dbExists, err := createMissingServerDatabase(ctx, cfg, initDB, dbExists)
	if err != nil {
		return serverConnFacts{}, err
	}
	if err := waitForServerDatabase(ctx, cfg, db); err != nil {
		return serverConnFacts{}, err
	}
	bootstrapHeal, err := captureFreshBootstrapHeal(ctx, cfg, db, created)
	if err != nil {
		return serverConnFacts{}, err
	}
	return serverConnFacts{bootstrapHeal: bootstrapHeal, alreadyExisted: dbExists}, nil
}

func validateServerDatabaseTarget(cfg *Config) error {
	if err := ValidateDatabaseName(cfg.Database); err != nil {
		return fmt.Errorf("invalid database name %q: %w", cfg.Database, err)
	}
	if isTestDatabaseName(cfg.Database) && isProductionPort(cfg) {
		return fmt.Errorf(
			"REFUSED: will not CREATE DATABASE %q on production port %d — "+
				"this is a test database name on the production server (see DOLT-WAR-ROOM.md)",
			cfg.Database, cfg.ServerPort)
	}
	return nil
}

func createMissingServerDatabase(ctx context.Context, cfg *Config, initDB *sql.DB, dbExists bool) (bool, bool, error) {
	if dbExists {
		return false, true, nil
	}
	if !cfg.CreateIfMissing {
		return false, false, databaseNotFoundError(cfg)
	}

	_, err := initDB.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE `%s`", cfg.Database)) //nolint:gosec // G201: cfg.Database validated by ValidateDatabaseName above
	if err == nil {
		return true, false, nil
	}
	errLower := strings.ToLower(err.Error())
	if strings.Contains(errLower, "database exists") || strings.Contains(errLower, "1007") {
		return false, true, nil
	}
	if strings.Contains(errLower, "connection refused") || strings.Contains(errLower, "connect: connection refused") {
		return false, false, fmt.Errorf("failed to connect to Dolt server at %s:%d: %w\n\nThe Dolt server may not be running. Try:\n  bd dolt start    # Start a local server\n  gt dolt start    # If using an orchestrator",
			cfg.ServerHost, cfg.ServerPort, err)
	}
	return false, false, fmt.Errorf("failed to create database: %w", err)
}

func waitForServerDatabase(ctx context.Context, cfg *Config, db *sql.DB) error {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 100 * time.Millisecond
	bo.MaxElapsedTime = 10 * time.Second
	if err := backoff.Retry(func() error {
		pingErr := db.PingContext(ctx)
		if pingErr == nil {
			return nil
		}
		if isRetryableError(pingErr) {
			return pingErr
		}
		return backoff.Permanent(pingErr)
	}, backoff.WithContext(bo, ctx)); err != nil {
		return fmt.Errorf("database %q not available after CREATE DATABASE: %w", cfg.Database, err)
	}
	return nil
}

func captureFreshBootstrapHeal(
	ctx context.Context,
	cfg *Config,
	db *sql.DB,
	created bool,
) (*schema.FreshBootstrapHealCapability, error) {
	if !created {
		return nil, nil
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("capture fresh database identity: pin connection: %w", err)
	}
	bootstrapHeal, captureErr := schema.CaptureFreshBootstrapHealCapability(
		ctx, conn, serverEndpointIdentity(cfg), cfg.Database,
	)
	_ = conn.Close()
	if captureErr != nil {
		return nil, fmt.Errorf("capture fresh database identity: %w", captureErr)
	}
	return bootstrapHeal, nil
}

// serverEndpointIdentity returns the exact configured transport endpoint. It
// is captured with fresh-bootstrap authority and must match the migration
// connection's endpoint before that authority can be consumed.
func serverEndpointIdentity(cfg *Config) string {
	if cfg.ServerSocket != "" {
		return "unix:" + cfg.ServerSocket
	}
	return "tcp:" + net.JoinHostPort(cfg.ServerHost, strconv.Itoa(cfg.ServerPort))
}

// databaseExistsOnServer checks if a database with the exact given name exists
// on the Dolt server. Uses SHOW DATABASES + iterate instead of SHOW DATABASES LIKE
// to avoid LIKE wildcard issues with underscores in database names.
func databaseExistsOnServer(ctx context.Context, db *sql.DB, name string) (bool, error) {
	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var dbName string
		if err := rows.Scan(&dbName); err != nil {
			return false, err
		}
		if dbName == name {
			return true, nil
		}
	}
	return false, rows.Err()
}

// initSchemaOnDB applies pending schema migrations. schema.MigrateUp tracks
// applied versions in schema_migrations and backfills legacy config-driven
// tables. Returns the number of migrations applied.
func initSchemaOnDB(ctx context.Context, db *sql.DB) (int, error) {
	return initSchemaOnDBWithBootstrapHeal(ctx, db, nil, "")
}

// initSchemaOnDBWithBootstrapHeal threads one-shot, incarnation-bound reset
// authority into the migration lock. A nil capability always fails closed.
func initSchemaOnDBWithBootstrapHeal(
	ctx context.Context,
	db *sql.DB,
	bootstrapHeal *schema.FreshBootstrapHealCapability,
	endpoint string,
) (int, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("schema: pin connection: %w", err)
	}
	defer conn.Close()

	var dbName string
	if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&dbName); err != nil {
		return 0, fmt.Errorf("schema: read database name: %w", err)
	}

	var opts []schema.MigrateLockOption
	if bootstrapHeal != nil {
		opts = append(opts, schema.WithFreshBootstrapHeal(bootstrapHeal, endpoint))
	}
	applied, err := schema.MigrateUpWithLock(ctx, conn, dbName, opts...)
	if err != nil {
		return applied, fmt.Errorf("schema migration: %w", err)
	}
	return applied, nil
}

func initSchemaOnDBWithRetry(ctx context.Context, db *sql.DB) (int, error) {
	return initSchemaOnDBWithRetryAndGate(ctx, db, nil)
}

// initSchemaOnDBWithRetryAndGate is initSchemaOnDBWithRetry with an optional
// pre-migration gate run INSIDE the retry loop. The gate's own reads
// (schema_migrations, dolt_remotes) can hit the same transient Dolt
// startup/catalog races the migration retry absorbs, so gate probe errors are
// retried with them instead of failing the open fast (bd-6dnrw.30); a
// *schema.RemoteMigrateGateError refusal stays permanent.
func initSchemaOnDBWithRetryAndGate(ctx context.Context, db *sql.DB, gate func(context.Context, *sql.DB) error) (int, error) {
	return initSchemaOnDBWithRetryAndGateBootstrapHeal(ctx, db, gate, nil, "")
}

// initSchemaOnDBWithRetryAndGateBootstrapHeal shares one capability across the
// outer retry loop. Once consumed, no later retry can issue another reset.
func initSchemaOnDBWithRetryAndGateBootstrapHeal(
	ctx context.Context,
	db *sql.DB,
	gate func(context.Context, *sql.DB) error,
	bootstrapHeal *schema.FreshBootstrapHealCapability,
	endpoint string,
) (int, error) {
	// Schema initialization for server mode is idempotent. Retry transient
	// Dolt startup/catalog races and contended migration-lock attempts so
	// concurrent bd processes converge instead of failing one unlucky waiter.
	schemaBO := backoff.NewExponentialBackOff()
	schemaBO.InitialInterval = 100 * time.Millisecond
	// Must exceed schema.MigrateUpWithLock's 5s GET_LOCK wait so a contended
	// schema migration can time out once and still retry.
	schemaBO.MaxElapsedTime = serverRetryMaxElapsed
	var applied int
	err := backoff.Retry(func() error {
		if gate != nil {
			if gateErr := gate(ctx, db); gateErr != nil {
				if !schema.IsRemoteMigrateGateError(gateErr) && isRetryableError(gateErr) {
					return gateErr
				}
				return backoff.Permanent(gateErr)
			}
		}
		var schemaErr error
		applied, schemaErr = initSchemaOnDBWithBootstrapHeal(ctx, db, bootstrapHeal, endpoint)
		if schemaErr != nil && isRetryableError(schemaErr) {
			return schemaErr
		}
		if schemaErr != nil {
			return backoff.Permanent(schemaErr)
		}
		return nil
	}, backoff.WithContext(schemaBO, ctx))
	return applied, err
}

// configCommitMode controls how commitWorkingSet treats the config table, which
// holds both internal keys (issue_prefix) and synced user data (kv.* keys,
// including kv.memory.* persistent memories).
type configCommitMode int

const (
	// configExclude skips config entirely (GH#2455): a plain Commit must not
	// sweep a concurrent writer's half-applied issue_prefix change into an
	// unrelated commit.
	configExclude configCommitMode = iota
	// configIncludeUserKVOnly stages config for the pre-pull auto-commit, but
	// only when every dirty config row is this clone's own user KV data (the
	// kv.* namespace, which includes kv.memory.* memories). Any other dirty
	// config key — an internal key such as issue_prefix above all — aborts the
	// commit with operator guidance so the pull never auto-commits unsafe
	// config (GH#2455 + GH#2474).
	configIncludeUserKVOnly
	// configIncludeAll stages every dirty config row. Used only to conclude a
	// merge whose conflicts the operator resolved explicitly (bd federation
	// sync --strategy): that resolution is intentional, so a resolved
	// issue_prefix (or any config row) must be committed, not dropped.
	configIncludeAll
)

// DoltStatus is an alias for storage.Status.
