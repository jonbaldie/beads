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
	"sync"
	"time"

	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// DefaultSQLPort is the default port for dolt sql-server.
const DefaultSQLPort = 3307

// testDatabasePrefixes are name prefixes that indicate a test database.
// Used by isTestDatabaseName to prevent test databases from being created
// on the production Dolt server (Clown Shows #12-#18).
//
// Origin of each prefix:
//   - testdb_     : applyConfigDefaults derives this for BEADS_TEST_MODE=1
//     without an explicit Database (FNV hash of cfg.Path).
//   - beads_test  : convention for hand-written integration tests.
//   - beads_pt    : property-test fixtures.
//   - beads_vr    : version-roundtrip / migration fixtures.
//   - doctest_    : `bd doctor` self-check fixtures.
//   - doctortest_ : older `bd doctor` fixture name (kept for back-compat).
//   - benchdb_    : per-bench scratch DBs (cmd/bd/template_test.go
//     newTemplateBenchmarkStore, format `benchdb_<unixnano>`). Added by
//     AD-01 (be-c5p).
//
// This list is the firewall side of the test/prod split. Two sibling lists
// must converge with it (be-avn): cmd/bd/dolt.go:staleDatabasePrefixes (used
// by `bd dolt clean-databases`) and the formula-side `gc dolt cleanup`
// stale-prefix list. Any prefix added here must be mirrored to those lists,
// or stale fixtures will leak past clean-up.
// Compile-time interface checks.
var _ storage.DoltStorage = (*DoltStore)(nil)
var _ storage.RawDBAccessor = (*DoltStore)(nil)
var _ storage.StoreLocator = (*DoltStore)(nil)
var _ storage.ActiveDatabaseSizer = (*DoltStore)(nil)
var _ storage.LifecycleManager = (*DoltStore)(nil)
var _ storage.PendingCommitter = (*DoltStore)(nil)
var _ storage.GarbageCollector = (*DoltStore)(nil)
var _ storage.Flattener = (*DoltStore)(nil)
var _ storage.Compactor = (*DoltStore)(nil)
var _ storage.SchemaMigrator = (*DoltStore)(nil)
var _ storage.ExternalRefHistoryQuerier = (*DoltStore)(nil)
var _ storage.EventsJournalConfigurer = (*DoltStore)(nil)

type doltStoreConfigCacheState struct {
	customStatusDetailedCache []types.CustomStatus
	customStatusCache         []string
	customStatusCached        bool
	customTypeCache           []string
	customTypeCached          bool
	infraTypeCache            map[string]bool
	infraTypeCached           bool
	cacheMu                   sync.Mutex
}

type doltStoreIdentityState struct {
	credentialKey []byte
}

// doltCredentialKeyAvailable keeps the credential-key state alongside the
// store declaration while the encryption implementation remains in its own
// file.
func doltCredentialKeyAvailable(store *DoltStore) bool {
	return store.doltStoreIdentityState.credentialKey != nil
}

func invalidateDoltConfigCaches(store *DoltStore, key string) {
	store.doltStoreConfigCacheState.cacheMu.Lock()
	defer store.doltStoreConfigCacheState.cacheMu.Unlock()
	switch key {
	case "status.custom":
		store.customStatusCached = false
		store.customStatusCache = nil
		store.customStatusDetailedCache = nil
	case "types.custom":
		store.customTypeCached = false
		store.customTypeCache = nil
	case "types.infra":
		store.infraTypeCached = false
		store.infraTypeCache = nil
	}
}

func doltCustomConfigCacheReady(store *DoltStore) bool {
	store.doltStoreConfigCacheState.cacheMu.Lock()
	defer store.doltStoreConfigCacheState.cacheMu.Unlock()
	return store.doltStoreConfigCacheState.customStatusCached && store.doltStoreConfigCacheState.customTypeCached
}

func setDoltCustomConfigCache(store *DoltStore, statuses []types.CustomStatus, customTypes []string) {
	store.cacheMu.Lock()
	defer store.cacheMu.Unlock()
	if !store.customStatusCached {
		store.customStatusDetailedCache = statuses
		store.customStatusCache = types.CustomStatusNames(statuses)
		store.customStatusCached = true
	}
	if !store.customTypeCached {
		store.customTypeCache = customTypes
		store.customTypeCached = true
	}
}

func doltCachedCustomStatuses(store *DoltStore) []string {
	store.cacheMu.Lock()
	defer store.cacheMu.Unlock()
	return store.customStatusCache
}

func doltCachedCustomStatusesDetailed(store *DoltStore) []types.CustomStatus {
	store.cacheMu.Lock()
	defer store.cacheMu.Unlock()
	return store.customStatusDetailedCache
}

func doltCachedCustomTypes(store *DoltStore) []string {
	store.cacheMu.Lock()
	defer store.cacheMu.Unlock()
	return store.customTypeCache
}

func doltCachedInfraTypes(store *DoltStore) (map[string]bool, bool) {
	store.cacheMu.Lock()
	defer store.cacheMu.Unlock()
	return store.infraTypeCache, store.infraTypeCached
}

func setDoltInfraTypesCache(store *DoltStore, result map[string]bool) {
	store.cacheMu.Lock()
	defer store.cacheMu.Unlock()
	store.infraTypeCache = result
	store.infraTypeCached = true
}

// IsClosed returns true if the store has been closed.
func (s *DoltStore) IsClosed() bool {
	return s.doltStoreLifecycleState.closed.Load()
}

// Config holds Dolt database configuration.
//
// The option groups keep the public configuration surface organized by
// concern while preserving the promoted field selectors used by callers.
type Config struct {
	Path           string // Path to Dolt database directory
	BeadsDir       string // Path to .beads directory (for server auto-start when Path is custom)
	CommitterName  string // Git-style committer name
	CommitterEmail string // Git-style committer email
	Remote         string // Default remote name (e.g., "origin")
	Database       string // Database name within Dolt (default: "beads")
	ReadOnly       bool   // Open in read-only mode (skip schema init)
	Preview        bool   // Non-mutating preview: embedded opens skip schema init and refuse writes

	// LenientOpen opens the store leniently: embedded mode only. A migration
	// gate refusal (#4259) or a dirty-working-set refusal (#4566) skips the
	// migration instead of failing the open. Set for working-set-reconcile
	// commands (bd dolt commit, bd vc commit; #4566), whose entire purpose is
	// to clear the working set that the migration would otherwise refuse to
	// touch. Ignored in server mode.
	LenientOpen bool

	ServerOptions
	RemoteOptions
	PoolOptions
}

// ServerOptions holds connection, routing, and server lifecycle options.
type ServerOptions struct {
	// Server connection options
	ServerSocket   string // Unix domain socket path (overrides Host/Port when set)
	ServerHost     string // Server host (default: 127.0.0.1)
	ServerPort     int    // Server port (default: 3307)
	ServerUser     string // MySQL user (default: root)
	ServerPassword string // MySQL password (default: empty, can be set via BEADS_DOLT_PASSWORD)
	ServerTLS      bool   // Enable TLS for server connections (required for Hosted Dolt)

	// ServerPortSource records which step of doltserver's port-resolution
	// chain (or the env-var read in applyConfigDefaults) produced ServerPort.
	// Zero value (doltserver.PortSourceUnset) when ServerPort was never
	// resolved from a source (e.g. left 0, or set directly by a caller that
	// bypassed applyConfigDefaults). Consulted by newServerMode's auto-start
	// path to decide whether silently retargeting to a different port is
	// safe (GH#4052).
	ServerPortSource doltserver.PortSource

	// ServerPortSharedServer mirrors doltserver.Config.PortSharedServer:
	// true when ServerPort was resolved via shared-server mode
	// (BEADS_DOLT_SHARED_SERVER=1). In shared-server mode, auto-start's
	// EnsureRunningDetailed(resolvedBeadsDir) always spins up a repo-local
	// server (a different database than the shared one), so a port change
	// here is never a benign refresh regardless of ServerPortSource —
	// consulted by newServerMode's auto-start path alongside
	// ServerPortSource.IsAuthoritative() (GH#4052).
	ServerPortSharedServer bool

	// ServerMode indicates this config targets an external dolt sql-server
	// rather than the embedded Dolt engine. Set by the store factory based
	// on metadata.json dolt_mode or BEADS_DOLT_SERVER_MODE env var.
	ServerMode bool

	// ProxiedServer indicates this config targets a per-workspace proxied
	// dolt sql-server (a parent proxy + a child dolt sql-server, both rooted
	// at <BeadsDir>/dolt). Mutually exclusive with ServerMode: the
	// proxied path owns its own connection details and does not consult
	// ServerHost/Port/Socket/User. Set by the store factory based on
	// metadata.json dolt_mode=proxied-server.
	ProxiedServer bool

	// Gateway indicates the server is an authenticating gateway server: a credential
	// command supplies a short-lived token as the connection username. bd treats such a
	// server as owning database routing and schema, so it connects with the project
	// database, skips the no-database admin probe, and never issues SHOW DATABASES /
	// CREATE DATABASE or schema DDL (drift check only, like ReadOnly). Set by
	// ApplyGatewayCredential, never by hand.
	Gateway bool

	// AutoStart enables transparent server auto-start when connection fails.
	// When true and the host is localhost, bd will start a dolt sql-server
	// automatically if one isn't running. Disabled under orchestrator (GT_ROOT set).
	AutoStart bool

	// DisableAutoStart suppresses implicit server startup even when standalone
	// defaults would enable it. Diagnostic paths use this to stay read-only.
	DisableAutoStart bool
}

// RemoteOptions holds remote authentication and database-creation options.
type RemoteOptions struct {
	// Remote auth for Hosted Dolt push/pull (optional)
	// When set, Push/Pull use the --user flag and set DOLT_REMOTE_PASSWORD env var.
	RemoteUser     string // Hosted Dolt remote user (set via DOLT_REMOTE_USER env var)
	RemotePassword string // Hosted Dolt remote password (set via DOLT_REMOTE_PASSWORD env var)

	// SyncRemote holds the effective sync remote URL (from sync.remote
	// or deprecated sync.git-remote). Used for context-aware error hints.
	SyncRemote string

	// CreateIfMissing allows CREATE DATABASE when the target database does not
	// exist on the server. Only explicit initialization, migration, or new-board
	// creation paths should set this to true. Normal open paths leave it false,
	// which causes an error if the database is missing — preventing silent
	// creation of shadow databases on the wrong server.
	CreateIfMissing bool
}

// PoolOptions holds the optional database connection-pool limits.
type PoolOptions struct {
	// MaxOpenConns overrides the connection pool size (0 = default 10).
	// Set to 1 for branch isolation in tests (DOLT_CHECKOUT is session-level).
	MaxOpenConns int

	// MaxIdleConns overrides the maximum number of idle pooled connections
	// (0 = default min(5, MaxOpenConns)). Higher values keep more connections
	// warm between queries, reducing NewConnection/ConnectionClosed churn.
	MaxIdleConns int

	// ConnMaxLifetime overrides how long a pooled connection may be reused
	// before the pool retires it (0 = default 1 hour). Long-lived daemons
	// should not use a short lifetime — every retire+reopen shows up as a
	// NewConnection event in dolt-server.log and churns the pool for no
	// benefit when the server is local and stable.
	ConnMaxLifetime time.Duration

	// ConnMaxIdleTime overrides how long a connection may sit idle in the pool
	// before the pool retires it (0 = default 20s). This must stay below the
	// dolt sql-server wait_timeout (currently 30s) so the pool retires an idle
	// connection before the server reaps it server-side; otherwise the next
	// query handed a server-reaped connection fails with "invalid connection".
	ConnMaxIdleTime time.Duration

	// PoolReadTimeout / PoolWriteTimeout override the per-I/O read/write
	// deadlines on shared-pool connections (0 = default 10s each; see
	// buildServerDSN). The default's fast-fail is right for a healthy local
	// server, but on an overloaded shared server it kills ordinary queries
	// mid-flight ("client connection went away", wy-b72dj/bd-vz0y9); raising
	// it is the intended relief valve for such deployments. Known-long
	// operations should not lean on this — route them through
	// execWithLongTimeout/openLongTimeoutConn instead.
	PoolReadTimeout  time.Duration
	PoolWriteTimeout time.Duration
}

type DoltStatus = storage.Status

// StatusEntry is an alias for storage.StatusEntry.
type StatusEntry = storage.StatusEntry
