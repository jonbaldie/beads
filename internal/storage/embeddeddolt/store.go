//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/storage/schema"
)

// Compile-time interface checks.
var _ storage.DoltStorage = (*EmbeddedDoltStore)(nil)
var _ storage.StoreLocator = (*EmbeddedDoltStore)(nil)
var _ storage.ActiveDatabaseSizer = (*EmbeddedDoltStore)(nil)
var _ storage.GarbageCollector = (*EmbeddedDoltStore)(nil)
var _ storage.Flattener = (*EmbeddedDoltStore)(nil)
var _ storage.Compactor = (*EmbeddedDoltStore)(nil)
var _ storage.SchemaMigrator = (*EmbeddedDoltStore)(nil)
var _ storage.EventsJournalConfigurer = (*EmbeddedDoltStore)(nil)
var _ storage.ExternalRefHistoryQuerier = (*EmbeddedDoltStore)(nil)
var _ = (*EmbeddedDoltStore).withConn

// EmbeddedDoltStore implements storage.DoltStorage backed by the embedded Dolt engine.
// Each method call opens a short-lived connection, executes within an explicit
// SQL transaction, and closes the connection immediately. This minimizes the
// time the embedded engine's write lock is held, reducing contention when
// multiple processes access the same database concurrently.
//
// The dolthub/driver/v2 handles its own concurrency internally. File-level locking
// is only used during bd init to protect one-time initialization steps.
type EmbeddedDoltStore struct {
	dataDir       string
	beadsDir      string
	database      string
	branch        string
	credentialKey []byte
	closed        atomic.Bool
	// eventsJournalEnabled activates the durable events journal for THIS store
	// instance only (storage.EventsJournalConfigurer); never process-global.
	eventsJournalEnabled atomic.Bool
	// readOnly marks a store opened via OpenReadOnly or
	// OpenForPreviewCommand: open-time mutations (CREATE DATABASE, schema
	// migrations) were skipped and write transactions are refused
	// (bd-6dnrw.32).
	readOnly bool
	// intent records why this store was opened, controlling how lenient
	// initSchema is about pending-migration refusals it would otherwise treat
	// as fatal. Unlike readOnly, a non-strict intent still allows writes
	// (e.g. the post-command autocommit net, or the commit itself) - only the
	// migration step is skipped.
	intent openIntent
}

// embeddedCredentialKeyAvailable keeps the store's credential-key state
// visible alongside its declaration while the federation implementation
// remains in its focused file.
func embeddedCredentialKeyAvailable(store *EmbeddedDoltStore) bool {
	return store.credentialKey != nil
}

// openIntent classifies why a store is being opened. openStrict fails the
// open on any pending-migration refusal; the other two intents relax both
// the #4259 remote-migrate gate refusal and the #4566 dirty-table refusal,
// each with its own warning text (see initSchema).
type openIntent int

const (
	// openStrict is the default: any pending-migration refusal fails the
	// open. Used by Open.
	openStrict openIntent = iota
	// openReadOnlyCommand relaxes both refusals for read-only commands: they
	// must keep working on the current schema until the operator makes the
	// migrate-or-adopt decision (bd-578h9.5), and must not be bricked by
	// dirty tables either. Used by OpenForReadOnlyCommand.
	openReadOnlyCommand
	// openWorkingSetReconcile relaxes both refusals for working-set-reconcile
	// commands (bd dolt commit, bd vc commit): their entire purpose is to
	// clear the dirty working set that a migration would otherwise refuse to
	// touch, so failing the open here would deadlock the documented recovery
	// (#4566). Used by OpenForWorkingSetReconcile.
	openWorkingSetReconcile
)

// errClosed is returned when a method is called after Close.
var errClosed = errors.New("embeddeddolt: store is closed")

// IsClosed reports whether the store has been closed. Implements
// storage.LifecycleManager so that callers (e.g., maybeAutoCommit) can
// skip operations on a closed store without triggering errClosed.
func (s *EmbeddedDoltStore) IsClosed() bool {
	return s.closed.Load()
}

// newStore creates an EmbeddedDoltStore using the embedded Dolt engine.
// beadsDir is the .beads/ root; the data directory is derived as <beadsDir>/embeddeddolt/.
// The database is created automatically if it doesn't exist (initSchema handles this).
//
// The dolthub/driver/v2 handles its own concurrency internally. File-level locking
// is only used during bd init (via util.TryLock in the init command) to protect
// one-time initialization steps — the store itself does not hold any lock.
func newStore(ctx context.Context, beadsDir, database, branch string, intent openIntent) (*EmbeddedDoltStore, error) {
	if database == "" {
		return nil, fmt.Errorf("embeddeddolt: database name must not be empty (caller should default to %q)", "beads")
	}

	// Resolve to absolute path — the embedded dolt driver resolves file://
	// DSN paths relative to its data directory, so relative paths cause
	// doubled-path errors on subsequent opens.
	absBeadsDir, err := filepath.Abs(beadsDir)
	if err != nil {
		return nil, fmt.Errorf("embeddeddolt: resolving beads dir: %w", err)
	}
	dataDir := filepath.Join(absBeadsDir, "embeddeddolt")
	if err := os.MkdirAll(dataDir, config.BeadsDirPerm); err != nil {
		return nil, fmt.Errorf("embeddeddolt: creating data directory: %w", err)
	}

	s := &EmbeddedDoltStore{
		dataDir:  dataDir,
		beadsDir: absBeadsDir,
		database: database,
		branch:   branch,
		intent:   intent,
	}

	if err := s.initSchema(ctx); err != nil {
		return nil, fmt.Errorf("embeddeddolt: init schema: %w", err)
	}

	return s, nil
}

// OpenReadOnly opens an existing embedded database for read-only access,
// skipping every mutating open-time step: no data-directory creation, no
// CREATE DATABASE, no remote-migrate gate, and no schema migrations
// (bd-6dnrw.32). It is the embedded equivalent of server mode's
// Config.ReadOnly open, used for cross-repo hydration of foreign projects
// (GH#3231) where opening must not write anything — not even a one-time
// migration backfill commit — into the target's history. Drift in either
// direction is checked at open: forward (the database AHEAD of this binary)
// because stale-binary reads fail cryptically, and behind (the database
// BEHIND this binary) because these paths used to auto-migrate and would
// otherwise fail at query time with unknown-column errors (bd-578h9.12).
//
// Read-only stores bypass the Open cache in both directions: they must not be
// handed a future writable Open (which would skip migrations), and writable
// opens of the same directory keep their own lifecycle. Write transactions on
// the returned store are refused.
func OpenReadOnly(ctx context.Context, beadsDir, database, branch string) (*EmbeddedDoltStore, error) {
	return openReadOnly(ctx, beadsDir, database, branch, true)
}

// OpenForPreviewCommand opens an existing embedded database without any
// open-time mutation and refuses all write transactions. Unlike
// OpenReadOnly, it permits a behind schema cursor so --dry-run/--inspect can
// still validate state that is query-compatible with the current binary. A
// missing column or other genuine incompatibility is reported by the preview
// query itself; it is never repaired implicitly.
func OpenForPreviewCommand(ctx context.Context, beadsDir, database, branch string) (*EmbeddedDoltStore, error) {
	return openReadOnly(ctx, beadsDir, database, branch, false)
}

func openReadOnly(ctx context.Context, beadsDir, database, branch string, checkBehind bool) (*EmbeddedDoltStore, error) {
	if database == "" {
		return nil, fmt.Errorf("embeddeddolt: database name must not be empty (caller should default to %q)", "beads")
	}
	if !validIdentifier.MatchString(database) {
		return nil, fmt.Errorf("embeddeddolt: invalid database name: %q", database)
	}
	absBeadsDir, err := filepath.Abs(beadsDir)
	if err != nil {
		return nil, fmt.Errorf("embeddeddolt: resolving beads dir: %w", err)
	}
	dataDir := filepath.Join(absBeadsDir, "embeddeddolt")
	if _, err := os.Stat(dataDir); err != nil {
		return nil, fmt.Errorf("embeddeddolt: no embedded database at %s: %w", dataDir, err)
	}

	s := &EmbeddedDoltStore{
		dataDir:  dataDir,
		beadsDir: absBeadsDir,
		database: database,
		branch:   branch,
		readOnly: true,
	}

	db, cleanup, err := OpenSQL(ctx, dataDir, database, branch)
	if err != nil {
		return nil, fmt.Errorf("embeddeddolt: open db: %w", err)
	}
	defer func() { _ = cleanup() }()
	if err := schema.CheckForwardDrift(ctx, db); err != nil {
		return nil, err
	}
	if checkBehind {
		if err := schema.CheckBehindDrift(ctx, db); err != nil {
			return nil, err
		}
	}

	return s, nil
}

// withConn opens a short-lived database connection configured for the store's
// database and branch, begins an explicit SQL transaction, and passes it to
// fn. If commit is true and fn returns nil, the transaction is committed;
// otherwise it is rolled back. The connection is closed before withConn
// returns regardless of outcome.
//
// The database must already exist (created during initSchema).
func (s *EmbeddedDoltStore) withConn(ctx context.Context, commit bool, fn func(tx *sql.Tx) error) (err error) {
	if s.closed.Load() {
		err = errClosed
		return
	}
	if commit && s.readOnly {
		err = ErrReadOnly
		return
	}

	var db *sql.DB
	var cleanup func() error
	db, cleanup, err = OpenSQL(ctx, s.dataDir, s.database, s.branch)
	if err != nil {
		return
	}

	committed := false
	defer func() {
		err = joinTransactionCleanupError(err, cleanup(), committed)
	}()

	var tx *sql.Tx
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		err = fmt.Errorf("embeddeddolt: begin tx: %w", err)
		return
	}
	clearJournalScope := issueops.ScopeEventsJournalTransaction(tx, s.eventsJournalEnabled.Load())
	defer clearJournalScope()

	if fnErr := fn(tx); fnErr != nil {
		err = errors.Join(fnErr, tx.Rollback())
		return
	}

	if !commit {
		err = tx.Rollback()
		return
	}

	if cErr := commitEmbeddedTx(tx); cErr != nil {
		err = cErr
		return
	}
	committed = true
	return
}

// SetEventsJournalEnabled activates the journal for this store instance only.
func (s *EmbeddedDoltStore) SetEventsJournalEnabled(enabled bool) {
	s.eventsJournalEnabled.Store(enabled)
}

// commitEmbeddedTx classifies an unconfirmed SQL commit response as
// indeterminate: the engine may have applied it before the connection failed.
func commitEmbeddedTx(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return wrapCommitIndeterminate("embeddeddolt: commit tx", err)
	}
	return nil
}

func joinTransactionCleanupError(operationErr, cleanupErr error, committed bool) error {
	if committed && cleanupErr != nil {
		cleanupErr = wrapCommitIndeterminate("embeddeddolt: cleanup after SQL commit", cleanupErr)
	}
	return errors.Join(operationErr, cleanupErr)
}

func (s *EmbeddedDoltStore) ApplySchemaMigrations(ctx context.Context) (int, error) {
	if s.closed.Load() {
		return 0, errClosed
	}
	if s.readOnly {
		return 0, ErrReadOnly
	}
	db, cleanup, err := OpenSQL(ctx, s.dataDir, s.database, s.branch)
	if err != nil {
		return 0, fmt.Errorf("embeddeddolt: open db: %w", err)
	}
	defer func() { _ = cleanup() }()

	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("embeddeddolt: pin connection: %w", err)
	}
	defer conn.Close()

	return schema.MigrateUp(ctx, conn)
}

// ---------------------------------------------------------------------------
// storage.RemoteStore
// ---------------------------------------------------------------------------

// RemoveRemote, ListRemotes, Push, Pull, ForcePush, Fetch, PushTo, PullFrom
// are implemented in version_control.go via versioncontrolops.

// ---------------------------------------------------------------------------
// storage.SyncStore
// ---------------------------------------------------------------------------

// Sync and SyncStatus are implemented in federation.go.

// ---------------------------------------------------------------------------
// storage.FederationStore
// ---------------------------------------------------------------------------

// AddFederationPeer, GetFederationPeer, ListFederationPeers, RemoveFederationPeer
// are implemented in federation.go via issueops.
