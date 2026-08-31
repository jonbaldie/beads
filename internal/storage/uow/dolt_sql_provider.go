package uow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	_ "github.com/go-sql-driver/mysql"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/proxy"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/util"
	db "github.com/jonbaldie/beads/internal/storage/domain/db"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/storage/schema"
)

const (
	defaultBranch           = "main"
	defaultProxyIdleTimeout = 30 * time.Second
)

type doltSQLProvider struct {
	defaultBranch string
	db            *sql.DB
	// serverEndpoint is the exact transport endpoint used by the migration
	// connection. Fresh-bootstrap reset authority is bound to it together with
	// the Dolt server UUID, database name, and initial HEAD.
	serverEndpoint string
	// teamServer: schema is owned by beads-team-server (bts) — bd never
	// creates the database or migrates, only verifies the schema version.
	teamServer bool
	// expectedProjectID is the calling workspace's project identity, asserted
	// against the team-server database on open. Empty means "no assertion
	// available" (bd init, which adopts; server-wide maintenance; a workspace
	// predating project identity) and skips the check.
	expectedProjectID string
	// preview: the command that opened this provider is an explicitly
	// non-mutating preview (--dry-run, --inspect). The open creates no
	// database and applies no migration; see providerOptions.preview.
	preview bool
	// eventsJournalEnabled activates the durable events journal for THIS
	// provider instance only. See SetEventsJournalEnabled.
	eventsJournalEnabled atomic.Bool
}

// SetEventsJournalEnabled activates the durable events journal for every unit
// of work this provider begins from now on. Per instance, never process-global:
// a process can hold several providers at once, and enabling one must not
// enable the rest.
//
// Emission itself lives at the issueops seam that the domain/db repositories
// call, but the seam only emits for a transaction activation is BOUND to (see
// BeginTx). Without that binding the uow plumbing writes mutations while
// journaling nothing — the failure is invisible, because the code runs and the
// write lands and the journal is simply empty.
func (p *doltSQLProvider) SetEventsJournalEnabled(enabled bool) {
	p.eventsJournalEnabled.Store(enabled)
}

type bootstrapPreparationError struct {
	err       error
	retryable bool
}

func (e *bootstrapPreparationError) Error() string {
	return e.err.Error()
}

func (e *bootstrapPreparationError) Unwrap() error {
	return e.err
}

func classifyInitSchemaError(err error) error {
	var preparationErr *bootstrapPreparationError
	if errors.As(err, &preparationErr) {
		if preparationErr.retryable {
			return fmt.Errorf("uow: bootstrap preparation: %w", err)
		}
		return backoff.Permanent(err)
	}
	if isSerializationError(err) || schema.IsMigrationLockError(err) {
		return fmt.Errorf("uow: migrate: %w", err)
	}
	return backoff.Permanent(fmt.Errorf("uow: migrate: %w", err))
}

// ProviderOption tunes how a SQL-server unit-of-work provider opens. Options
// are variadic so the existing constructor call sites — every one of which
// wants the ordinary mutating open — stay unchanged.
type ProviderOption func(*providerOptions)

type providerOptions struct {
	// preview opens for a command that promised not to mutate anything
	// (--dry-run, --inspect). Such a command must reach its own RunE before
	// anything writes, so the open may neither CREATE DATABASE nor run
	// MigrateUpWithLock: both happen during root pre-run, before the flag the
	// user passed has had any effect. An absent or behind database is
	// reported by the preview's own query rather than repaired implicitly —
	// the same contract embeddeddolt.OpenForPreviewCommand gives the embedded
	// path.
	preview bool
}

// WithPreview opens the provider for a non-mutating preview command.
func WithPreview() ProviderOption {
	return func(o *providerOptions) { o.preview = true }
}

func applyProviderOptions(opts []ProviderOption) providerOptions {
	var resolved providerOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&resolved)
		}
	}
	return resolved
}

var (
	_ UnitOfWorkProvider              = (*doltSQLProvider)(nil)
	_ TxProvider                      = (*doltSQLProvider)(nil)
	_ storage.EventsJournalConfigurer = (*doltSQLProvider)(nil)
)

func (p *doltSQLProvider) NewUOW(ctx context.Context) (UnitOfWork, error) {
	return NewUOW(ctx, p)
}

func (p *doltSQLProvider) Close(_ context.Context) error {
	if p.db == nil {
		return nil
	}
	db := p.db
	p.db = nil
	return db.Close()
}

func (p *doltSQLProvider) BeginTx(ctx context.Context) (Tx, error) {
	conn, err := p.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("uow: pin connection: %w", err)
	}

	_, err = conn.ExecContext(ctx, "START TRANSACTION;")
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("uow: failed to start transaction: %w", err)
	}

	// Bind journal activation to the connection this unit of work is pinned to,
	// AFTER START TRANSACTION so the seq allocation's UPDATE and the SELECT that
	// must observe it are inside one transaction on one session. The scope is
	// released when the connection is (doltServerTx.releaseConn / poisonConn),
	// so an entry cannot outlive its transaction.
	return &doltServerTx{
		conn:              conn,
		clearJournalScope: issueops.ScopeEventsJournalTransaction(conn, p.eventsJournalEnabled.Load()),
	}, nil
}

func (p *doltSQLProvider) initSchema(ctx context.Context, database string) error {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 25 * time.Millisecond
	// This budget must outwait a peer holding the migration lock through a
	// full cold-start migration pass (every migration + a Dolt commit each),
	// not just a transient blip — it grows as migrations accumulate.
	bo.MaxElapsedTime = 60 * time.Second
	bootstrap := &schemaBootstrap{provider: p, database: database}
	return backoff.Retry(func() error {
		return p.initSchemaAttempt(ctx, bootstrap)
	}, backoff.WithContext(bo, ctx))
}

type schemaBootstrap struct {
	provider      *doltSQLProvider
	database      string
	created       bool
	bootstrapHeal *schema.FreshBootstrapHealCapability
}

func (p *doltSQLProvider) initSchemaAttempt(ctx context.Context, bootstrap *schemaBootstrap) error {
	conn, err := p.db.Conn(ctx)
	if err != nil {
		if isSerializationError(err) {
			return fmt.Errorf("uow: pin connection: %w", err)
		}
		return backoff.Permanent(fmt.Errorf("uow: pin connection: %w", err))
	}
	defer conn.Close()
	ddl := db.NewDDLSQLRepository(conn)
	if p.teamServer {
		return p.initTeamServerSchema(ctx, conn, ddl, bootstrap.database)
	}
	if p.preview {
		return p.initPreviewSchema(ctx, ddl, bootstrap.database)
	}
	if _, err := schema.MigrateUpWithLock(ctx, conn, bootstrap.database,
		schema.WithLockedPreparation(p.serverEndpoint, bootstrap.prepare)); err != nil {
		return classifyInitSchemaError(err)
	}
	return nil
}

func (p *doltSQLProvider) initTeamServerSchema(ctx context.Context, conn *sql.Conn, ddl db.DDLSQLRepository, database string) error {
	if err := ddl.UseDatabase(ctx, database); err != nil {
		if isSerializationError(err) {
			return fmt.Errorf("uow: switching to database: %w", err)
		}
		return backoff.Permanent(fmt.Errorf(
			"uow: database %q not found — the schema is managed by beads-team-server; ask your operator to run 'bts init' first: %w",
			database, err))
	}
	if err := checkTeamServerSchema(ctx, conn, database); err != nil {
		if isSerializationError(err) {
			return fmt.Errorf("uow: team-server schema check: %w", err)
		}
		return backoff.Permanent(err)
	}
	if err := checkTeamServerIdentity(ctx, conn, database, p.expectedProjectID); err != nil {
		if isSerializationError(err) {
			return fmt.Errorf("uow: team-server identity check: %w", err)
		}
		return backoff.Permanent(err)
	}
	return nil
}

func (p *doltSQLProvider) initPreviewSchema(ctx context.Context, ddl db.DDLSQLRepository, database string) error {
	if err := ddl.UseDatabase(ctx, database); err != nil {
		if isSerializationError(err) {
			return fmt.Errorf("uow: switching to database: %w", err)
		}
		return backoff.Permanent(fmt.Errorf(
			"uow: database %q not found — preview commands (--dry-run, --inspect) never create or migrate a database; run the command without the preview flag first: %w",
			database, err))
	}
	return nil
}

func (b *schemaBootstrap) prepare(ctx context.Context, conn *sql.Conn) (*schema.FreshBootstrapHealCapability, error) {
	ddl := db.NewDDLSQLRepository(conn)
	if b.created {
		if err := ddl.CreateDatabaseIfNotExists(ctx, b.database); err != nil {
			return nil, &bootstrapPreparationError{err: fmt.Errorf("uow: creating database: %w", err)}
		}
		if err := ddl.UseDatabase(ctx, b.database); err != nil {
			return nil, &bootstrapPreparationError{err: fmt.Errorf("uow: switching to database: %w", err)}
		}
		return b.bootstrapHeal, nil
	}
	createdNow, err := b.createInitialDatabase(ctx, ddl)
	if err != nil {
		return nil, err
	}
	if err := ddl.UseDatabase(ctx, b.database); err != nil {
		return nil, &bootstrapPreparationError{err: fmt.Errorf("uow: switching to database: %w", err)}
	}
	if !createdNow {
		return b.bootstrapHeal, nil
	}
	b.created = true
	b.bootstrapHeal, err = schema.CaptureFreshBootstrapHealCapability(ctx, conn, b.provider.serverEndpoint, b.database)
	if err != nil {
		return nil, &bootstrapPreparationError{err: fmt.Errorf("uow: capture fresh database identity: %w", err)}
	}
	return b.bootstrapHeal, nil
}

func (b *schemaBootstrap) createInitialDatabase(ctx context.Context, ddl db.DDLSQLRepository) (bool, error) {
	err := ddl.CreateDatabase(ctx, b.database)
	switch {
	case err == nil:
		return true, nil
	case isDatabaseExistsError(err):
		return false, nil
	case isSerializationError(err):
		return false, &bootstrapPreparationError{err: fmt.Errorf("uow: creating database: %w", err), retryable: true}
	default:
		return false, &bootstrapPreparationError{err: fmt.Errorf("uow: creating database: %w", err)}
	}
}

func buildDSN(ep proxy.Endpoint, database, user, password, tlsConfigName string) string {
	return util.DoltServerDSN{
		Host:            ep.Host,
		Port:            ep.Port,
		User:            user,
		Password:        password,
		Database:        database,
		TLSConfigName:   tlsConfigName,
		ClientFoundRows: true,
	}.String()
}

func openDB(ctx context.Context, dsn string) (*sql.DB, error) {
	conn, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("uow: open db: %w", err)
	}
	if err := conn.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("uow: ping db: %w", err), conn.Close())
	}
	return conn, nil
}

func openAndInitSchema(ctx context.Context, ep proxy.Endpoint, database, rootUser, rootPassword, tlsConfigName string, teamServer bool, expectedProjectID string, opts providerOptions) (UnitOfWorkProvider, error) {
	initDB, err := openDB(ctx, buildDSN(ep, "", rootUser, rootPassword, tlsConfigName))
	if err != nil {
		return nil, err
	}

	initProvider := &doltSQLProvider{
		defaultBranch:     defaultBranch,
		db:                initDB,
		serverEndpoint:    "tcp:" + ep.Address(),
		teamServer:        teamServer,
		expectedProjectID: expectedProjectID,
		preview:           opts.preview,
	}

	if err := initProvider.initSchema(ctx, database); err != nil {
		_ = initDB.Close()
		return nil, fmt.Errorf("uow: init schema: %w", err)
	}

	if err := initDB.Close(); err != nil {
		return nil, fmt.Errorf("uow: close init db: %w", err)
	}

	dbConn, err := openDB(ctx, buildDSN(ep, database, rootUser, rootPassword, tlsConfigName))
	if err != nil {
		return nil, err
	}

	return &doltSQLProvider{
		defaultBranch:     defaultBranch,
		db:                dbConn,
		serverEndpoint:    "tcp:" + ep.Address(),
		teamServer:        teamServer,
		expectedProjectID: expectedProjectID,
		preview:           opts.preview,
	}, nil
}
