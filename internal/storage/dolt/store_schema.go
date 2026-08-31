package dolt

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/go-sql-driver/mysql"

	"github.com/jonbaldie/beads/internal/storage/schema"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

func (s *DoltStore) initSchema(ctx context.Context, bootstrapHeal *schema.FreshBootstrapHealCapability) error {
	// Schema migrations can run arbitrarily long (e.g. full-table recomputes
	// such as the is_blocked backfill in migration 0047). The main connection
	// pool sets a 10s ReadTimeout (see buildServerDSN); a slow migration over
	// that pool aborts mid-flight with "i/o timeout" and leaves tables dirty,
	// which then blocks every subsequent migration attempt. Run the migration
	// pass over a dedicated connection with no read/write timeout. Cancellation
	// is governed by the caller's context, not a fixed deadline.
	migDB, err := s.openMigrationDB()
	if err != nil {
		return err
	}
	defer migDB.Close()
	// #4259: refuse to silently apply pending migrations to a remote-backed,
	// already-initialized database — that is how two clones fork the schema.
	// The gate runs inside the retry loop, before each migration attempt: its
	// reads can hit transient startup/catalog races (retryable) while a gate
	// refusal is permanent and never retried into a migration.
	// Use the on-disk fallback: a freshly (auto-)started server can report an
	// empty dolt_remotes table even though remotes are persisted in .dolt/config
	// (GH#2315), so an SQL-only check would miss the remote on the first write
	// open after an upgrade.
	//
	// adopt injects the driver-side fast-forward ancestry primitives
	// (mybd-ae1i) so the smart gate can distinguish a losslessly
	// fast-forwardable remote-ahead case (smartAdoptFastForward) from the
	// plain destructive adopt, and auto-execute it: CheckRemoteMigrateGate*
	// calls FastForward and returns nil (proceed, nothing pending) once HEAD
	// has actually advanced; any execution failure (dirty working set raced
	// in, non-fast-forward, concurrent writer) falls back to the plain
	// destructive adopt directive instead of forcing the write.
	adopt := &schema.FastForwardAdopter{
		IsStrictAncestor: func(ctx context.Context, db schema.DBConn, ref string) (bool, error) {
			return versioncontrolops.LocalIsStrictAncestorOf(ctx, db, ref)
		},
		WorkingSetClean: func(ctx context.Context, db schema.DBConn) (bool, error) {
			return versioncontrolops.WorkingSetClean(ctx, db)
		},
		FastForward: func(ctx context.Context, db schema.DBConn, ref string) error {
			return versioncontrolops.FastForwardAdopt(ctx, db, ref)
		},
		// s.initSchema is only ever invoked from the writable-open path (the
		// caller guards it on !cfg.ReadOnly), so this is always false in
		// practice today — wired explicitly anyway so the adopter's safety
		// invariant (ReadOnly means "cannot write here") does not silently
		// depend on that external guard alone.
		ReadOnly: s.readOnly,
	}
	gate := func(ctx context.Context, db *sql.DB) error {
		return schema.CheckRemoteMigrateGateForRemoteWithRemoteCheckAndAdopt(ctx, db, s.remote, s.hasPersistedCLIRemote, adopt)
	}
	_, err = initSchemaOnDBWithRetryAndGateBootstrapHeal(ctx, migDB, gate, bootstrapHeal, s.serverEndpoint)
	return err
}

// ApplySchemaMigrations runs idempotent schema migrations under the
// per-database advisory lock, with retry for transient lock contention.
// Implements storage.SchemaMigrator.
func (s *DoltStore) ApplySchemaMigrations(ctx context.Context) (int, error) {
	migDB, err := s.openMigrationDB()
	if err != nil {
		return 0, err
	}
	defer migDB.Close()
	return initSchemaOnDBWithRetry(ctx, migDB)
}

// openMigrationDB opens a one-off connection pool for schema migrations with no
// read/write timeout. Migrations may run far longer than the default 10s pool
// timeout, and timing out part-way leaves the database in a dirty, half-migrated
// state. The single connection is closed by the caller once migration completes.
func (s *DoltStore) openMigrationDB() (*sql.DB, error) {
	cfg, err := mysql.ParseDSN(s.connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse DSN for migration connection: %w", err)
	}
	cfg.ReadTimeout = 0
	cfg.WriteTimeout = 0
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("failed to open migration connection: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
