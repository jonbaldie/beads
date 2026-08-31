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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage/doltutil"
	"github.com/jonbaldie/beads/internal/storage/schema"
)

type doltStoreLifecycleState struct {
	closed               atomic.Bool
	eventsJournalEnabled atomic.Bool
	mu                   sync.RWMutex
}

type doltStoreVersionControlState struct {
	committerName  string
	committerEmail string
	remote         string // Default remote for push/pull
	branch         string // Current branch
	remoteUser     string // Remote auth user for Hosted Dolt push/pull (optional)
	remotePassword string // Remote auth password for Hosted Dolt push/pull (optional)
	serverMode     bool   // true when connected to external dolt sql-server (not embedded)

	// autoStartedServerDir is set when this store triggered a dolt sql-server
	// auto-start. Close() uses it to stop the server when the last store
	// referencing it is closed (tracked via autoStartRefs).
	autoStartedServerDir string
}

// DoltStore implements the Storage interface using Dolt.
type DoltStore struct {
	db             *sql.DB
	dbPath         string // Path to Dolt data directory (server root, e.g. .beads/dolt/)
	beadsDir       string // Path to .beads directory (parent of dbPath)
	database       string // Database name (subdirectory under dbPath)
	connStr        string // Connection string for reconnection
	serverEndpoint string // Exact endpoint bound to bootstrap reset authority
	readOnly       bool   // True if opened in read-only mode

	// localActiveDatabaseDir is the exact active database directory when this
	// store instance has authoritative local filesystem access. It is resolved
	// once at construction; empty means sizing is unsupported for this instance.
	localActiveDatabaseDir string

	doltStoreLifecycleState
	doltStoreConfigCacheState
	doltStoreIdentityState

	// OTel span attribute cache (avoids per-call allocation)
	spanAttrs spanAttributeCache

	// Circuit breaker for Dolt server connections
	breaker *circuitBreaker

	doltStoreVersionControlState
}

// newServerMode creates a DoltStore connected to a running dolt sql-server.
// This path is pure Go and does not require CGO.
func newServerMode(ctx context.Context, cfg *Config) (*DoltStore, error) {
	_, _, autoStartedDir, breaker, err := establishServerEndpoint(ctx, cfg)
	if err != nil {
		return nil, err
	}
	// Server mode: connect via MySQL protocol to dolt sql-server
	db, connStr, dbFacts, err := openServerConnection(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return initializeOpenedServer(ctx, cfg, db, connStr, dbFacts, breaker, autoStartedDir)
}

func initializeOpenedServer(
	ctx context.Context,
	cfg *Config,
	db *sql.DB,
	connStr string,
	dbFacts serverConnFacts,
	breaker *circuitBreaker,
	autoStartedDir string,
) (*DoltStore, error) {
	storeReady := false
	defer func() {
		if !storeReady {
			_ = db.Close()
		}
	}()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping Dolt database: %w", err)
	}
	store := newOpenedServerStore(cfg, db, connStr, breaker, autoStartedDir)
	if err := schema.CheckForwardDrift(ctx, db); err != nil {
		return nil, err
	}
	if err := verifyOpenedServer(ctx, cfg, store, dbFacts); err != nil {
		return nil, err
	}
	if err := initializeOpenedServerSchema(ctx, cfg, store, dbFacts); err != nil {
		return nil, err
	}
	persistOpenedServerPort(cfg)
	store.branch = "main"
	store.registerPoolGauges()
	storeReady = true
	return store, nil
}

func newOpenedServerStore(
	cfg *Config,
	db *sql.DB,
	connStr string,
	breaker *circuitBreaker,
	autoStartedDir string,
) *DoltStore {
	beadsDir := cfg.BeadsDir
	if beadsDir == "" && cfg.Path != "" {
		beadsDir = filepath.Dir(cfg.Path)
	}
	store := &DoltStore{
		db:                     db,
		dbPath:                 cfg.Path,
		beadsDir:               beadsDir,
		database:               cfg.Database,
		localActiveDatabaseDir: resolveLocalActiveDatabaseDir(cfg),
		connStr:                connStr,
		serverEndpoint:         serverEndpointIdentity(cfg),
		breaker:                breaker,
		readOnly:               cfg.ReadOnly,
		doltStoreLifecycleState: doltStoreLifecycleState{
			closed:               atomic.Bool{},
			eventsJournalEnabled: atomic.Bool{},
			mu:                   sync.RWMutex{},
		},
		doltStoreConfigCacheState: doltStoreConfigCacheState{},
		doltStoreIdentityState:    doltStoreIdentityState{},
		spanAttrs:                 spanAttributeCache{},
		doltStoreVersionControlState: doltStoreVersionControlState{
			committerName:        cfg.CommitterName,
			committerEmail:       cfg.CommitterEmail,
			remote:               cfg.Remote,
			branch:               "main",
			remoteUser:           cfg.RemoteUser,
			remotePassword:       cfg.RemotePassword,
			serverMode:           true,
			autoStartedServerDir: autoStartedDir,
		},
	}
	store.doltSpanAttrs()
	return store
}

func verifyOpenedServer(ctx context.Context, cfg *Config, store *DoltStore, facts serverConnFacts) error {
	// Existing databases must be checked before migrations. Gateway databases
	// deliberately leave this gate closed because their server owns identity.
	if cfg.CreateIfMissing && !facts.alreadyExisted {
		return nil
	}
	if cfg.Database == doltserver.GlobalDatabaseName {
		return store.verifyGlobalProjectIdentity(ctx, cfg.BeadsDir)
	}
	return store.verifyProjectIdentity(ctx, cfg.BeadsDir)
}

func initializeOpenedServerSchema(
	ctx context.Context,
	cfg *Config,
	store *DoltStore,
	facts serverConnFacts,
) error {
	if cfg.ReadOnly || cfg.Gateway {
		return nil
	}
	if err := store.initSchema(ctx, facts.bootstrapHeal); err != nil {
		return fmt.Errorf("failed to initialize schema: %w", err)
	}
	return nil
}

func persistOpenedServerPort(cfg *Config) {
	if !isLocalHost(cfg.ServerHost) {
		return
	}
	beadsDir := cfg.BeadsDir
	if beadsDir == "" && cfg.Path != "" {
		beadsDir = filepath.Dir(cfg.Path)
	}
	_ = persistResolvedPortFile(cfg, beadsDir)
}

var (
	cleanServerCircuitState = CleanStaleCircuitBreakerFiles
	newServerCircuitBreaker = maybeNewCircuitBreaker
	ensureResolvedPortFile  = doltserver.EnsurePortFile
)

func initializeServerCircuitBreaker(cfg *Config) *circuitBreaker {
	if cfg.DisableAutoStart || os.Getenv("BEADS_TEST_MODE") == "1" {
		return nil
	}
	// Clean stale circuit breaker files before checking — prevents leftover
	// state from previous sessions poisoning fresh writable opens (GH#2598).
	cleanServerCircuitState()
	return newServerCircuitBreaker(cfg.ServerHost, cfg.ServerPort, cfg.Database)
}

// serverOpenCanAutoStart reports whether a stopped managed dolt server may be
// auto-started for this open. This is keyed off DisableAutoStart (the strict
// --readonly signal threaded from policy.disableAutoStart in cmd/bd/main.go),
// not cfg.ReadOnly: ordinary classified-read commands (bd show, bd list, ...)
// also set cfg.ReadOnly but must still be able to auto-start a stopped
// managed server, per dolt_autostart_lifecycle_integration_test.go.
func serverOpenCanAutoStart(cfg *Config) bool {
	return !cfg.DisableAutoStart && cfg.AutoStart && cfg.Path != "" &&
		cfg.ServerSocket == "" && isLocalHost(cfg.ServerHost)
}

func persistResolvedPortFile(cfg *Config, beadsDir string) error {
	if cfg.DisableAutoStart || !shouldPersistResolvedPortFile() {
		return nil
	}
	return ensureResolvedPortFile(beadsDir, cfg.ServerPort)
}

func shouldPersistResolvedPortFile() bool {
	return os.Getenv("BEADS_DOLT_SERVER_PORT") == "" && os.Getenv("BEADS_DOLT_PORT") == ""
}

// isLocalHost returns true if the host refers to the local machine.
func isLocalHost(host string) bool {
	switch host {
	case "", "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
}

// isExternalServerHost reports whether host names a remote server for the
// purposes of connect-failure hints (GH#3518). Unlike isLocalHost it
// normalizes case/whitespace and treats 0.0.0.0 as local, matching the
// mode-inference classification in internal/configfile — the two must
// agree or an unreachable local server gets external-server advice with
// no "bd dolt start" recovery hint.
func isExternalServerHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "", "localhost", "127.0.0.1", "::1", "[::1]", "0.0.0.0":
		return false
	}
	return true
}

// buildServerDSN constructs a MySQL DSN for connecting to a Dolt server.
// If database is empty, connects without selecting a database (for init operations).
// Adds ReadTimeout/WriteTimeout for long-lived connection pools.
func buildServerDSN(cfg *Config, database string) string {
	base := doltutil.ServerDSN{
		Socket:   cfg.ServerSocket,
		Host:     cfg.ServerHost,
		Port:     cfg.ServerPort,
		User:     cfg.ServerUser,
		Password: cfg.ServerPassword,
		Database: database,
		TLS:      cfg.ServerTLS,
	}
	// Parse the base DSN and add pool-specific timeouts.
	parsed, err := mysql.ParseDSN(base.String())
	if err != nil {
		return base.String()
	}
	parsed.ReadTimeout = defaultPoolReadTimeout
	if cfg.PoolReadTimeout > 0 {
		parsed.ReadTimeout = cfg.PoolReadTimeout
	}
	parsed.WriteTimeout = defaultPoolWriteTimeout
	if cfg.PoolWriteTimeout > 0 {
		parsed.WriteTimeout = cfg.PoolWriteTimeout
	}
	return parsed.FormatDSN()
}

// applyPoolLimits configures the pool on db using the sensible-default
// connection pool limits, overridden by any non-zero Config fields.
//
// These limits are deliberately oriented at long-lived daemons: a 1h
// connection lifetime lets the same physical MySQL connection be reused
// for thousands of queries, so dolt-server.log no longer shows a
// NewConnection/ConnectionClosed pair every few queries.
func applyPoolLimits(db *sql.DB, cfg *Config) {
	maxOpen := defaultMaxOpenConns
	if cfg.MaxOpenConns > 0 {
		maxOpen = cfg.MaxOpenConns
	}

	maxIdle := defaultMaxIdleConns
	if cfg.MaxIdleConns > 0 {
		maxIdle = cfg.MaxIdleConns
	}
	// MaxIdleConns must never exceed MaxOpenConns or database/sql silently
	// clamps it and we end up with a different pool shape than requested.
	if maxIdle > maxOpen {
		maxIdle = maxOpen
	}

	lifetime := defaultConnMaxLifetime
	if cfg.ConnMaxLifetime > 0 {
		lifetime = cfg.ConnMaxLifetime
	}

	idle := defaultConnMaxIdleTime
	if cfg.ConnMaxIdleTime > 0 {
		idle = cfg.ConnMaxIdleTime
	}

	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(lifetime)
	db.SetConnMaxIdleTime(idle)
}

// serverConnFacts reports what openServerConnection established about the
// target database while connecting. Creation and prior existence are
// deliberately NOT inverses: for a gateway database, existence is never
// probed, so neither is proven and each caller's gate fails closed.
type serverConnFacts struct {
	// bootstrapHeal carries one-shot reset authority bound to the exact
	// endpoint, server UUID, database, and initial HEAD captured after this
	// call created and successfully connected to a pristine database. A nil
	// capability always fails closed.
	bootstrapHeal *schema.FreshBootstrapHealCapability

	// alreadyExisted reports whether the database was proven to exist on the
	// server before this call: either the SHOW DATABASES probe found it, or
	// our CREATE DATABASE was refused with "database exists" (1007). Callers
	// use it to decide whether project-identity verification applies even
	// when CreateIfMissing is true (see the newServerMode gate around
	// verifyProjectIdentity, GH#4637).
	alreadyExisted bool
}
