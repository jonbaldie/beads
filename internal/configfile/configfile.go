package configfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jonbaldie/beads/internal/storage/backendnames"
)

const ConfigFileName = "metadata.json"

// ConfigCompatibilityFields keeps rarely used compatibility metadata together
// while the anonymous embedding preserves Config's flat JSON representation and
// promoted selectors for callers.
type ConfigCompatibilityFields struct {
	DoltTeamServer     bool   `json:"dolt_team_server,omitempty"` // Schema is managed by beads-team-server (bts); bd never runs migrations (proxied-server mode only)
	PostgresDSN        string `json:"postgres_dsn,omitempty"`
	PostgresSchema     string `json:"postgres_schema,omitempty"`
	MySQLDSN           string `json:"mysql_dsn,omitempty"`
	MySQLDatabase      string `json:"mysql_database,omitempty"`
	SQLitePath         string `json:"sqlite_path,omitempty"` // database file, relative to the beads dir (default beads.db)
	GlobalDoltDatabase string `json:"global_dolt_database,omitempty"`
	GlobalProjectID    string `json:"global_project_id,omitempty"`
	LastBdVersion      string `json:"last_bd_version,omitempty"`
}

type Config struct {
	Database string `json:"database"`
	Backend  string `json:"backend,omitempty"` // Storage backend: "dolt" (default), a registered extension, or a legacy rejection tombstone. Read via GetBackend().

	// Deletions configuration
	DeletionsRetentionDays int `json:"deletions_retention_days,omitempty"` // 0 means use default (3 days)

	// Dolt connection mode configuration (bd-dolt.2.2)
	// "embedded" (default for standalone) runs Dolt in-process.
	// "server" connects to an external dolt sql-server (required for orchestrator / multi-writer).
	DoltMode           string `json:"dolt_mode,omitempty"`            // "embedded" (default) or "server"
	DoltServerHost     string `json:"dolt_server_host,omitempty"`     // Server host (default: 127.0.0.1)
	DoltServerPort     int    `json:"dolt_server_port,omitempty"`     // Server port (default: 3307)
	DoltServerSocket   string `json:"dolt_server_socket,omitempty"`   // Unix domain socket path (overrides host/port)
	DoltServerUser     string `json:"dolt_server_user,omitempty"`     // MySQL user (default: root)
	DoltDatabase       string `json:"dolt_database,omitempty"`        // SQL database name (default: beads)
	DoltServerTLS      bool   `json:"dolt_server_tls,omitempty"`      // Enable TLS for server connections (required for Hosted Dolt)
	DoltDataDir        string `json:"dolt_data_dir,omitempty"`        // Custom dolt data directory (absolute path; default: .beads/dolt)
	DoltRemotesAPIPort int    `json:"dolt_remotesapi_port,omitempty"` // Dolt remotesapi port for federation (default: 8080)
	// Note: Password should be set via BEADS_DOLT_PASSWORD env var for security

	ConfigCompatibilityFields

	// Project identity — unique ID generated at bd init time.
	// Used to detect cross-project data leakage when a client connects
	// to the wrong Dolt server (GH#2372).
	ProjectID string `json:"project_id,omitempty"`

	// Stale closed issues check configuration
	// 0 = disabled (default), positive = threshold in days
	StaleClosedIssuesDays int `json:"stale_closed_issues_days,omitempty"`
}

func DefaultConfig() *Config {
	return &Config{
		Database: "beads.db",
	}
}

func ConfigPath(beadsDir string) string {
	return filepath.Join(beadsDir, ConfigFileName)
}

func Load(beadsDir string) (*Config, error) {
	configPath := ConfigPath(beadsDir)

	data, err := os.ReadFile(configPath) // #nosec G304 - controlled path from config
	if os.IsNotExist(err) {
		// Try legacy config.json location (migration path)
		legacyPath := filepath.Join(beadsDir, "config.json")
		data, err = os.ReadFile(legacyPath) // #nosec G304 - controlled path from config
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading legacy config: %w", err)
		}

		// Migrate: parse legacy config, save as metadata.json, remove old file
		var cfg Config
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parsing legacy config: %w", err)
		}

		// Save to new location
		if err := cfg.Save(beadsDir); err != nil {
			return nil, fmt.Errorf("migrating config to metadata.json: %w", err)
		}

		// Remove legacy file (best effort: migration already saved to new location)
		_ = os.Remove(legacyPath)

		return &cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	return &cfg, nil
}

// LoadForDiscovery reads workspace metadata without migrating or rewriting it.
//
// Store admission uses this only to classify a workspace before a command can
// open storage. In particular, legacy config.json remains in place so a failed
// admission cannot turn a recoverable old workspace into a partially migrated
// one. Unknown JSON fields remain tolerated here because callers use the
// result only to decide whether to issue a conservative refusal; normal store
// selection keeps its existing validation.
func LoadForDiscovery(beadsDir string) (*Config, error) {
	data, err := os.ReadFile(ConfigPath(beadsDir)) // #nosec G304 -- beadsDir is caller-selected workspace state
	if os.IsNotExist(err) {
		data, err = os.ReadFile(filepath.Join(beadsDir, "config.json")) // #nosec G304 -- legacy workspace state
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading legacy config: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return &cfg, nil
}

// writeFileAtomic writes data to a temp file in path's directory and renames
// it over path, so concurrent readers never observe a truncated or partial
// file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// DefaultDeletionsRetentionDays is the default retention period for deletion records.
const DefaultDeletionsRetentionDays = 3

// GetDeletionsRetentionDays returns the configured retention days, or the default if not set.

// GetStaleClosedIssuesDays returns the configured threshold for stale closed issues.
// Returns 0 if disabled (the default), or a positive value if enabled.

// Backend constants
const (
	BackendDolt     = "dolt"
	BackendPostgres = "postgres"
	BackendMySQL    = "mysql"
	BackendSQLite   = "sqlite"
)

// BackendCapabilities describes behavioral constraints for a storage backend.
//
// This is intentionally small and stable: callers should use these flags to decide
// whether to enable features like RPC and process spawning.
//
// NOTE: Multiple processes opening the same Dolt directory concurrently can
// cause lock contention and transient failures. Dolt is treated as
// single-process-only unless using server mode.
type BackendCapabilities struct {
	// SingleProcessOnly indicates the backend must not be accessed from multiple
	// Beads OS processes concurrently.
	SingleProcessOnly bool
}

// CapabilitiesForBackend returns capabilities for a backend string. Embedded Dolt
// is single-process-only; use Config.GetCapabilities() to account for
// Dolt server and proxied-server modes.
func CapabilitiesForBackend(_ string) BackendCapabilities {
	return BackendCapabilities{SingleProcessOnly: true}
}

// GetCapabilities returns the backend capabilities for this config.
// Unlike CapabilitiesForBackend(string), this considers Dolt server mode
// (and proxied-server mode) which support multi-process access.

// IsSupportedBackend reports whether backend selects Dolt or a backend
// registered by this binary. The empty value is the legacy/default spelling
// of Dolt. OSS registers no alternate backends.
func IsSupportedBackend(backend string) bool {
	return backend == "" || backend == BackendDolt || backendnames.Has(backend)
}

// GetBackend returns the configured storage backend. PostgreSQL, MySQL, and
// SQLite remain recognizable here so workspaces created by earlier builds can
// fail loudly at store selection instead of silently falling back to an empty
// Dolt database.
// Registered extension names are returned unchanged. Empty and explicit Dolt
// retain the established Dolt behavior. GetBackend keeps the historical Dolt
// fallback for unknown values, so storage-selection callers must check
// IsSupportedBackend(c.Backend) before opening or creating storage.

// GetSQLitePath returns the SQLite database file path (relative to the beads dir, or
// absolute). Empty means the default (beads.db).

// Dolt mode constants
const (
	DoltModeEmbedded      = "embedded"
	DoltModeServer        = "server"
	DoltModeProxiedServer = "proxied-server"
)

// Default Dolt server settings
const (
	DefaultDoltServerHost     = "127.0.0.1"
	DefaultDoltServerPort     = 3307 // Use 3307 to avoid conflict with MySQL on 3306
	DefaultDoltServerUser     = "root"
	DefaultDoltDatabase       = "beads"
	DefaultDoltRemotesAPIPort = 8080 // Default dolt remotesapi port for federation
)
