package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/storage/doltutil"
	"github.com/jonbaldie/beads/internal/storage/embeddeddolt"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
	"golang.org/x/term"
)

// cloneFromRemote clones a Dolt database from a remote URL.
// In embedded mode, uses the embedded engine's DOLT_CLONE procedure.
// In external server mode, connects to the running server via MySQL and
// executes DOLT_CLONE so the server places the database in its own data
// directory. In owned-server mode, shells out to dolt clone via
// BootstrapFromRemoteWithDB.
// Shared by bd init and bd bootstrap to keep clone logic in one place.
func cloneFromRemote(ctx context.Context, beadsDir, remoteURL, dbName string, cfg *configfile.Config) error {
	return cloneFromRemoteWithMode(ctx, beadsDir, remoteURL, dbName, cfg, remoteCloneAuto)
}

func cloneFromRemoteWithMode(ctx context.Context, beadsDir, remoteURL, dbName string, cfg *configfile.Config, cloneMode remoteCloneMode) error {
	mode := resolveRemoteCloneMode(beadsDir, cfg, cloneMode)

	switch mode {
	case remoteCloneEmbedded:
		return cloneViaEmbedded(ctx, beadsDir, remoteURL, dbName)

	case remoteCloneExternalServer:
		if cfg == nil {
			// Caller didn't provide config; fall back to loading from disk.
			if loaded, err := configfile.Load(beadsDir); err == nil && loaded != nil {
				cfg = loaded
			}
		}
		if cfg != nil {
			return cloneViaServer(ctx, beadsDir, remoteURL, dbName, cfg)
		}
		// No config available — fall through to CLI clone.
		fmt.Fprintf(os.Stderr, "Warning: server mode detected but no config available, falling back to CLI clone\n")
		return cloneViaCLI(ctx, beadsDir, remoteURL, dbName)

	default:
		return cloneViaCLI(ctx, beadsDir, remoteURL, dbName)
	}
}

func resolveRemoteCloneMode(beadsDir string, cfg *configfile.Config, cloneMode remoteCloneMode) remoteCloneMode {
	if cloneMode != remoteCloneAuto {
		return cloneMode
	}

	if cfg != nil {
		if cfg.IsDoltServerMode() || doltserver.IsSharedServerMode() || os.Getenv("BEADS_DOLT_SERVER_MODE") == "1" {
			return remoteCloneExternalServer
		}
		return remoteCloneEmbedded
	}

	switch doltserver.ResolveServerMode(beadsDir) {
	case doltserver.ServerModeEmbedded:
		return remoteCloneEmbedded
	case doltserver.ServerModeExternal:
		return remoteCloneExternalServer
	default:
		return remoteCloneCLI
	}
}

// cloneViaEmbedded clones using the embedded Dolt engine (CGO required).
func cloneViaEmbedded(ctx context.Context, beadsDir, remoteURL, dbName string) error {
	dataDir := filepath.Join(beadsDir, "embeddeddolt")
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return fmt.Errorf("create embeddeddolt directory: %w", err)
	}
	db, cleanup, err := embeddeddolt.OpenSQL(ctx, dataDir, "", "")
	if err != nil {
		return fmt.Errorf("open embedded engine for clone: %w", err)
	}
	defer func() { _ = cleanup() }()

	if err := versioncontrolops.DoltClone(ctx, db, remoteURL, dbName, os.Getenv("DOLT_REMOTE_USER")); err != nil {
		return fmt.Errorf("clone from remote: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Synced database from %s\n", remoteURL)
	return nil
}

// cloneViaServer clones by connecting to the external Dolt server and
// executing CALL DOLT_CLONE. The server places the database in its own
// data directory, which is the correct behavior for externally managed
// servers where bd does not know the filesystem layout.
func cloneViaServer(ctx context.Context, beadsDir, remoteURL, dbName string, cfg *configfile.Config) error {
	port := serverClonePort(beadsDir, cfg)
	dsn := doltutil.ServerDSN{
		Socket:   cfg.GetDoltServerSocket(),
		Host:     cfg.GetDoltServerHost(),
		Port:     port,
		User:     cfg.GetDoltServerUser(),
		Password: cfg.GetDoltServerPasswordForPort(port),
		TLS:      cfg.GetDoltServerTLS(),
		// No Database — DOLT_CLONE creates the database.
	}.String()

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("connect to dolt server for clone: %w", err)
	}
	defer db.Close()

	cloneCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if err := db.PingContext(cloneCtx); err != nil {
		return fmt.Errorf("dolt server unreachable at %s:%d (is dolt sql-server running?): %w",
			cfg.GetDoltServerHost(), port, err)
	}

	if err := versioncontrolops.DoltClone(cloneCtx, db, remoteURL, dbName, os.Getenv("DOLT_REMOTE_USER")); err != nil {
		return fmt.Errorf("clone from remote via server: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Synced database from %s (via server at %s:%d)\n",
		remoteURL, cfg.GetDoltServerHost(), port)
	return nil
}

func serverClonePort(beadsDir string, cfg *configfile.Config) int {
	if port := configuredCloneServerPort(cfg); port > 0 {
		return port
	}
	if port := positivePortFromEnv("BEADS_DOLT_SERVER_PORT"); port > 0 {
		return port
	}
	if port := positivePortFromEnv("BEADS_DOLT_PORT"); port > 0 {
		return port
	}
	if port := doltserver.DefaultConfig(beadsDir).Port; port > 0 {
		return port
	}
	if cfg != nil {
		return cfg.GetDoltServerPort()
	}
	return configfile.DefaultDoltServerPort
}

func configuredCloneServerPort(cfg *configfile.Config) int {
	if cfg == nil || cfg.DoltServerPort <= 0 {
		return 0
	}
	return cfg.DoltServerPort
}

// cloneViaCLI clones by shelling out to the dolt CLI.
// Used for owned-server mode where bd manages the server lifecycle.
func cloneViaCLI(ctx context.Context, beadsDir, remoteURL, dbName string) error {
	doltDir := doltserver.ResolveDoltDir(beadsDir)
	synced, err := dolt.BootstrapFromRemoteWithDB(ctx, doltDir, remoteURL, dbName)
	if err != nil {
		return fmt.Errorf("sync from remote: %w", err)
	}
	if synced {
		fmt.Fprintf(os.Stderr, "Synced database from %s\n", remoteURL)
	}
	return nil
}

func inferPrefix(cfg *configfile.Config) string {
	db := cfg.GetDoltDatabase()
	if db != "" && db != "beads" {
		return db
	}
	cwd, _ := os.Getwd()
	return filepath.Base(cwd)
}

// isNonInteractiveBootstrap returns true if bootstrap should skip confirmation prompts.
// Precedence: explicit flag > BD_NON_INTERACTIVE env > CI env > terminal detection.
func isNonInteractiveBootstrap(flagValue bool) bool {
	if flagValue {
		return true
	}
	if v := os.Getenv("BD_NON_INTERACTIVE"); v == "1" || v == "true" {
		return true
	}
	if v := os.Getenv("CI"); v == "true" || v == "1" {
		return true
	}
	return !term.IsTerminal(int(os.Stdin.Fd()))
}

// findParentConfig walks up from beadsDir's parent looking for a
// .beads/metadata.json in ancestor directories. This handles the case where a
// rig subdirectory (its own git repo) doesn't have a local .beads but its
// parent workspace does. Returns nil if no parent config is found. A malformed
// or unreadable ancestor metadata file is authoritative and returned as an error;
// bootstrap must not skip it and select a more distant workspace or defaults.
func findParentConfig(beadsDir string) (*configfile.Config, error) {
	// Start from the parent of beadsDir's enclosing directory.
	// beadsDir is typically "<project>/.beads", so we start from <project>'s parent.
	start := filepath.Dir(filepath.Dir(beadsDir))
	homeDir, _ := os.UserHomeDir()

	for dir := start; dir != "/" && dir != "."; {
		candidate := filepath.Join(dir, ".beads")
		cfg, err := configfile.LoadForDiscovery(candidate)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", configfile.ConfigPath(candidate), err)
		}
		if cfg != nil {
			if err := guardLegacyUpgradeWorkspace(candidate); err != nil {
				return nil, err
			}
			return cfg, nil
		}

		// Don't search above $HOME
		if homeDir != "" && dir == homeDir {
			break
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return nil, nil
}

func init() {
	bootstrapCmd.Flags().Bool("dry-run", false, "Show what would be done without doing it")
	bootstrapCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompts (for CI/automation)")
	bootstrapCmd.Flags().Bool("non-interactive", false, "Alias for --yes")
	rootCmd.AddCommand(bootstrapCmd)
}

// isGitCodeRepoURL reports whether rawURL looks like a git code-repository
// remote rather than a Dolt data remote. Used to block accidental DOLT_CLONE
// against forges like github.com.
//
// Rules (in priority order):
//   - Dolt-native schemes (dolthub, s3, gs, az, file) → always false
//   - .git suffix → always true (Dolt DBs never carry a .git suffix)
//   - Well-known code forges / their subdomains → true
//   - Everything else → false (git+ssh:// to self-hosted Dolt remotes is valid)
func isGitCodeRepoURL(rawURL string) bool {
	// Dolt-native schemes are never code repos.
	for _, prefix := range []string{"dolthub://", "s3://", "gs://", "az://", "file://"} {
		if strings.HasPrefix(rawURL, prefix) {
			return false
		}
	}
	// .git suffix is a universal code-repo signal; Dolt DBs never use it.
	if strings.HasSuffix(strings.ToLower(rawURL), ".git") {
		return true
	}
	// Well-known code-hosting forges.
	host := urlHostname(rawURL)
	switch host {
	case "github.com", "gitlab.com", "bitbucket.org", "codeberg.org":
		return true
	}
	return strings.HasSuffix(host, ".github.com") || strings.HasSuffix(host, ".gitlab.com")
}

// urlHostname extracts the lowercase hostname from a URL without importing
// net/url. Handles scheme://[user@]host[:port]/path and SCP git@host:path.
func urlHostname(rawURL string) string {
	if sep := strings.Index(rawURL, "://"); sep >= 0 {
		rest := rawURL[sep+3:]
		if at := strings.Index(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		if slash := strings.Index(rest, "/"); slash >= 0 {
			rest = rest[:slash]
		}
		if colon := strings.Index(rest, ":"); colon >= 0 {
			rest = rest[:colon]
		}
		return strings.ToLower(rest)
	}
	// SCP-style: git@github.com:org/repo
	if at := strings.Index(rawURL, "@"); at >= 0 {
		rest := rawURL[at+1:]
		if colon := strings.Index(rest, ":"); colon >= 0 {
			return strings.ToLower(rest[:colon])
		}
	}
	return ""
}
