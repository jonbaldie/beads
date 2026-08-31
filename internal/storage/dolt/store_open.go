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
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
)

func applyConfigDefaults(cfg *Config) {
	applyDatabaseDefault(cfg)
	applyCommitterDefaults(cfg)
	if cfg.Remote == "" {
		cfg.Remote = "origin"
	}
	applyServerDefaults(cfg)
	applyRemoteCredentialDefaults(cfg)
}

func applyDatabaseDefault(cfg *Config) {
	if cfg.Database == "" {
		// Check env var first — this is the highest-priority override and
		// must be consulted even when no config file was loaded.
		if d := os.Getenv("BEADS_DOLT_SERVER_DATABASE"); d != "" {
			cfg.Database = d
			return
		}
		if os.Getenv("BEADS_TEST_MODE") == "1" && cfg.Path != "" {
			cfg.Database = testDatabaseName(cfg.Path)
			return
		}
		fmt.Fprintf(os.Stderr, "warning: no database name configured; falling back to default %q\n", configfile.DefaultDoltDatabase)
		cfg.Database = configfile.DefaultDoltDatabase
	}
}

func testDatabaseName(path string) string {
	// Test mode: derive unique database name from path for isolation.
	// Each test creates a unique temp directory, so hashing the path
	// gives each test its own database on the shared test server.
	h := fnv.New64a()
	_, _ = h.Write([]byte(path)) // hash.Hash.Write never returns an error
	return fmt.Sprintf("testdb_%x", h.Sum64())
}

func applyCommitterDefaults(cfg *Config) {
	if cfg.CommitterName == "" {
		cfg.CommitterName = os.Getenv("GIT_AUTHOR_NAME")
		if cfg.CommitterName == "" {
			cfg.CommitterName = "beads"
		}
	}
	if cfg.CommitterEmail == "" {
		cfg.CommitterEmail = os.Getenv("GIT_AUTHOR_EMAIL")
		if cfg.CommitterEmail == "" {
			cfg.CommitterEmail = "beads@local"
		}
	}
}

func applyServerDefaults(cfg *Config) {
	// Server connection defaults (applied in server mode; embedded mode bypasses TCP)
	if cfg.ServerSocket == "" {
		cfg.ServerSocket = os.Getenv("BEADS_DOLT_SERVER_SOCKET")
	}
	if cfg.ServerHost == "" {
		cfg.ServerHost = configuredServerHost()
	}
	applyConfiguredServerPort(cfg)
	enforceTestServerPort(cfg)
	if cfg.ServerUser == "" {
		cfg.ServerUser = "root"
	}
	// Check environment variable for password (more secure than command-line)
	if cfg.ServerPassword == "" {
		cfg.ServerPassword = os.Getenv("BEADS_DOLT_PASSWORD")
	}
}

func configuredServerHost() string {
	// Host resolution: BEADS_DOLT_SERVER_HOST env > default 127.0.0.1.
	if host := os.Getenv("BEADS_DOLT_SERVER_HOST"); host != "" {
		return host
	}
	return "127.0.0.1"
}

func applyConfiguredServerPort(cfg *Config) {
	// Port resolution: BEADS_DOLT_SERVER_PORT env (or legacy BEADS_DOLT_PORT) >
	// metadata config > default. The test-mode guard is applied by the caller
	// after all resolution sources have been considered.
	// CRITICAL: BEADS_TEST_MODE=1 forces port 1 (immediate fail) if the resolved port
	// is the production port (DefaultSQLPort). This prevents test databases from leaking
	// onto production even when the port env var is set to 3307 by the orchestrator's beads module.
	// Only an explicit non-production port (e.g., 43211 for a test server)
	// overrides test mode — that's a deliberate test server assignment.
	envPort := os.Getenv("BEADS_DOLT_SERVER_PORT")
	if envPort == "" {
		envPort = os.Getenv("BEADS_DOLT_PORT") // legacy fallback
	}
	if envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil && p > 0 {
			cfg.ServerPort = p
			// This env read happens before doltserver.DefaultConfig is
			// consulted below, but it is the same authoritative source
			// (BEADS_DOLT_SERVER_PORT / legacy BEADS_DOLT_PORT) — record it
			// so the auto-start fail-closed check in newServerMode sees it.
			cfg.ServerPortSource = doltserver.PortSourceEnv
		}
	}
	applyMetadataServerPort(cfg)
}

func applyMetadataServerPort(cfg *Config) {
	// If env var didn't provide a port, consult the full resolution chain:
	// port file > config.yaml > metadata.json (GH#2590).
	// Resolve from the owning .beads dir when available; cfg.Path is the Dolt
	// data path, not the config directory, and using it directly can miss the
	// repo-local port file or metadata.
	if cfg.ServerPort == 0 {
		resolveDir := cfg.BeadsDir
		if resolveDir == "" && cfg.Path != "" {
			resolveDir = filepath.Dir(cfg.Path)
		}
		if resolveDir != "" {
			if resolved := doltserver.DefaultConfig(resolveDir); resolved.Port > 0 {
				cfg.ServerPort = resolved.Port
				cfg.ServerPortSource = resolved.PortSource
				cfg.ServerPortSharedServer = resolved.PortSharedServer
			}
		}
	}
}

func enforceTestServerPort(cfg *Config) {
	// Port 0 means "not yet resolved" — auto-start (EnsureRunning) will
	// allocate an ephemeral port. Don't default to 3307 as that caused
	// cross-project data leakage (GH#2098, GH#2372).
	//
	// Test mode guard: force port 1 (immediate fail) if we'd hit production
	// or have no port, to prevent test databases leaking onto production.
	// Production-port detection is generalized via isProductionPort so cities
	// using non-3307 ports (BEADS_PRODUCTION_PORT or dolt-server.port) are
	// covered too.
	if os.Getenv("BEADS_TEST_MODE") == "1" {
		if cfg.ServerPort == 0 || isProductionPort(cfg) {
			cfg.ServerPort = 1
		}
	}
}

func applyRemoteCredentialDefaults(cfg *Config) {
	// Remote credentials for Hosted Dolt push/pull (env vars take precedence)
	if cfg.RemoteUser == "" {
		cfg.RemoteUser = os.Getenv("DOLT_REMOTE_USER")
	}
	if cfg.RemotePassword == "" {
		cfg.RemotePassword = os.Getenv("DOLT_REMOTE_PASSWORD")
	}
}
