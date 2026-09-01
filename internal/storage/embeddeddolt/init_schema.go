//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/storage/schema"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

func (s *EmbeddedDoltStore) initSchema(ctx context.Context) error {
	conn, cleanup, err := s.openSchemaConnection(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cleanup() }()
	defer conn.Close()

	if err := s.configureSchemaConnection(ctx, conn); err != nil {
		return err
	}
	if err := schema.CheckForwardDrift(ctx, conn); err != nil {
		return err
	}
	skipMigration, err := s.checkRemoteMigrateGate(ctx, conn)
	if err != nil {
		return err
	}
	if skipMigration {
		return nil
	}
	return s.migrateSchema(ctx, conn)
}

func (s *EmbeddedDoltStore) openSchemaConnection(ctx context.Context) (*sql.Conn, func() error, error) {
	db, cleanup, err := OpenSQL(ctx, s.dataDir, "", "")
	if err != nil {
		return nil, nil, fmt.Errorf("embeddeddolt: open db: %w", err)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("embeddeddolt: pin connection: %w", err)
	}
	return conn, cleanup, nil
}

func (s *EmbeddedDoltStore) configureSchemaConnection(ctx context.Context, conn *sql.Conn) error {
	if s.database == "" {
		return nil
	}
	if err := validateEmbeddedDatabaseName(s.database); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS `"+s.database+"`"); err != nil {
		return fmt.Errorf("embeddeddolt: creating database: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "USE `"+s.database+"`"); err != nil {
		return fmt.Errorf("embeddeddolt: switching to database: %w", err)
	}
	if s.branch == "" {
		return nil
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET @@%s_head_ref = %s", s.database, sqlStringLiteral(s.branch))); err != nil {
		return fmt.Errorf("embeddeddolt: setting branch: %w", err)
	}
	return nil
}

func validateEmbeddedDatabaseName(database string) error {
	if validIdentifier.MatchString(database) {
		return nil
	}
	msg := fmt.Sprintf("embeddeddolt: invalid database name: %q", database)
	if strings.ContainsRune(database, '-') {
		msg += "; hyphens are not allowed in embedded mode — replace with underscores in .beads/metadata.json dolt_database field, or run 'bd doctor'"
	}
	return errors.New(msg)
}

func (s *EmbeddedDoltStore) checkRemoteMigrateGate(ctx context.Context, conn *sql.Conn) (bool, error) {
	// #4259: refuse to silently apply pending migrations to a remote-backed,
	// already-initialized database — independently migrating each clone forks the
	// schema. Embedded mode syncs via Dolt remotes too, so it needs the same gate
	// as server mode.
	//
	// The adopter injects the driver-side fast-forward ancestry primitives so the
	// smart gate can distinguish a lossless remote-ahead case from destructive
	// adoption. A failed fast-forward falls back to the normal gate directive.
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
		// ReadOnly is deliberately left unset: this path is only reached via
		// newStore, whose intents all perform a writable open. OpenReadOnly skips
		// initSchema and this gate entirely.
	}
	if err := schema.CheckRemoteMigrateGateWithAdopt(ctx, conn, adopt); err != nil {
		return s.handleRemoteMigrateGateError(err)
	}
	return false, nil
}

func (s *EmbeddedDoltStore) handleRemoteMigrateGateError(err error) (bool, error) {
	var gateErr *schema.RemoteMigrateGateError
	if s.intent == openStrict || !errors.As(err, &gateErr) {
		return false, err
	}

	const sharedGuidance = "  This is\n" +
		"  coordination decision, not an auto-fix - do NOT run a migration unless\n" +
		"  you are the single designated migrator (only ONE clone may migrate a\n" +
		"  shared remote, else the schema forks; #4259):\n" +
		"    • designated migrator (only ONE machine): bd migrate --force && bd dolt push\n" +
		"    • every other clone (another already migrated): bd bootstrap\n" +
		"    • several machines: only ONE migrates; sync each other clone and run\n" +
		"      bd dolt pull after the migrator pushes, before upgrading it\n"
	if s.intent == openWorkingSetReconcile {
		fmt.Fprintf(os.Stderr,
			"Warning: %v\n"+
				"  Working-set reconcile command: continuing on schema v%d without\n"+
				"  migrating; the commit applies to the working set at the current\n"+
				"  schema."+sharedGuidance,
			gateErr, gateErr.CurrentVersion)
		return true, nil
	}
	fmt.Fprintf(os.Stderr,
		"Warning: %v\n"+
			"  Read-only command: continuing on schema v%d without migrating.\n"+
			"  Writes are blocked until the schema is reconciled."+sharedGuidance,
		gateErr, gateErr.CurrentVersion)
	return true, nil
}

func (s *EmbeddedDoltStore) migrateSchema(ctx context.Context, conn *sql.Conn) error {
	// Embedded mode relies on the driver's local file/concurrency controls;
	// schema.MigrateUpWithLock requires a SQL-server session lock.
	if _, err := schema.MigrateUp(ctx, conn); err != nil {
		return s.handleMigrationError(err)
	}
	return nil
}

func (s *EmbeddedDoltStore) handleMigrationError(err error) error {
	var dirtyErr *schema.DirtyTablesError
	if s.intent == openStrict || !errors.As(err, &dirtyErr) {
		return fmt.Errorf("embeddeddolt: migrate: %w", err)
	}
	if s.intent == openWorkingSetReconcile {
		fmt.Fprintf(os.Stderr,
			"Warning: %v\n"+
				"  Committing the working set at the current schema; when it completes,\n"+
				"  re-run 'bd migrate'.\n",
			dirtyErr)
		return nil
	}
	fmt.Fprintf(os.Stderr,
		"Warning: %v\n"+
			"  Continuing without migrating. Run 'bd dolt commit' to commit the\n"+
			"  working set at the current schema, then re-run 'bd migrate'.\n",
		dirtyErr)
	return nil
}
