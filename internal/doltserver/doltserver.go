// Package doltserver manages the lifecycle of a local dolt sql-server process.
// It provides transparent auto-start so that `bd init` and `bd <command>` work
// without manual server management.
//
// Port assignment uses OS-assigned ephemeral ports by default. When no explicit
// port is configured (env var, config.yaml, metadata.json), Start() asks the OS
// for a free port via net.Listen(":0"), passes it to dolt sql-server, and writes
// the actual port to dolt-server.port. This eliminates the birthday-problem
// collisions that plagued the old hash-derived port scheme (GH#2098, GH#2372).
//
// Users with explicit port config via BEADS_DOLT_SERVER_PORT env var or
// config.yaml always use that port instead, with conflict detection via
// reclaimPort.
//
// Server state files (PID, port, log, lock) live in the .beads/ directory.
package doltserver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
)

// ErrServerNotRunning is returned by Stop when the Dolt server is not running.
// Callers can use errors.Is to distinguish this expected condition from real
// failures (GH#2670).
var ErrServerNotRunning = errors.New("dolt server is not running")

// IgnoreNotRunning strips ErrServerNotRunning from err and returns any
// remaining errors (typically cleanup failures). If the only error was the
// sentinel, it returns nil. Handles both errors.Join (multi-unwrap) and
// standard fmt.Errorf wrapping (single-unwrap).
//
// IMPORTANT: call directly on Stop()/StopWithForce() return values only.
// Do not wrap the error before passing it here — wrapping may hide joined
// cleanup errors from the multi-unwrap path.
func IgnoreNotRunning(err error) error {
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrServerNotRunning) {
		return err // unrelated error, pass through
	}
	// Multi-error from errors.Join: filter out the sentinel, keep the rest.
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var remaining []error
		for _, e := range joined.Unwrap() {
			if e != nil && !errors.Is(e, ErrServerNotRunning) {
				remaining = append(remaining, e)
			}
		}
		return errors.Join(remaining...)
	}
	// Single-wrapped error (e.g., fmt.Errorf("%w", ErrServerNotRunning)):
	// the sentinel is the only meaningful content, so treat as pure sentinel.
	return nil
}

// PIDFileName and PortFileName are the canonical state file names used by the
// Dolt server lifecycle. They are exported so cross-package tests can reference
// the same names as the production code.
const (
	PIDFileName  = "dolt-server.pid"
	PortFileName = "dolt-server.port"
)

// maxEphemeralPortAttempts is the number of times Start() retries ephemeral
// port allocation when the TOCTOU race causes a bind failure.
const maxEphemeralPortAttempts = 10

// DefaultSharedServerPort is the default port for shared server mode.
// Uses 3308 to avoid conflict with the orchestrator which uses 3307.
const DefaultSharedServerPort = 3308

// GlobalDatabaseName is the SQL database name for the project-agnostic
// global issue database in shared-server mode.
const GlobalDatabaseName = "beads_global"

// GlobalIssuePrefix is the issue prefix used in the global database.
const GlobalIssuePrefix = "global"

// GlobalProjectID is the well-known sentinel UUID for the global database.
// Used for project identity verification — the global DB doesn't belong to
// any single project, so it uses this fixed value instead of a random UUID.
const GlobalProjectID = "00000000-0000-0000-0000-000000000000"

// IsSharedServerMode returns true if shared server mode is enabled.
// Checks (in priority order):
//  1. BEADS_DOLT_SHARED_SERVER env var ("1" or "true")
//  2. dolt.shared-server in config.yaml
//
// Shared server mode means all projects on this machine share a single
// dolt sql-server process at SharedServerDir(), each using its own
// database (already unique via prefix-based naming in bd init).
func IsSharedServerMode() bool {
	if v := os.Getenv("BEADS_DOLT_SHARED_SERVER"); v == "1" || strings.EqualFold(v, "true") {
		return true
	}
	return config.GetBool("dolt.shared-server")
}

func IsDebugMode() bool {
	if v := os.Getenv("BEADS_DOLT_DEBUG"); v == "1" || strings.EqualFold(v, "true") {
		return true
	}
	return config.GetBool("dolt.debug")
}

func DebugProfileDir(beadsDir string) string {
	p := filepath.Join(resolveServerDir(beadsDir), "dolt-pprof")
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

const debugProfileFilename = "cpu.pprof"

func rotateDebugProfile(beadsDir string) {
	profDir := DebugProfileDir(beadsDir)
	src := filepath.Join(profDir, debugProfileFilename)
	info, err := os.Stat(src)
	if err != nil || info.Size() == 0 {
		// No profile to rotate (server killed before flush, or never started in debug).
		return
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	dst := filepath.Join(profDir, fmt.Sprintf("cpu-%s.pprof", ts))
	if err := os.Rename(src, dst); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not rotate %s → %s: %v\n", src, dst, err)
		return
	}
	fmt.Fprintf(os.Stderr, "Debug: cpu profile rotated to %s\n", dst)
}

// IsAutoStartDisabled returns true if the dolt server should NOT be
// auto-started or managed by bd. When true, KillStaleServers and
// auto-start are suppressed — the server is externally managed (e.g.,
// by systemd).
//
// Either source can disable auto-start independently — there is no way
// to force-enable via env when the config file says disabled. Accepted
// disable values: any value strconv.ParseBool recognizes as false
// ("0", "f", "F", "false", "FALSE", "False") plus "off" (case-insensitive)
// for backward compatibility.
//
// This is used by KillStaleServers and Start to avoid killing or
// interfering with externally-managed dolt processes (GH#2641).
func IsAutoStartDisabled() bool {
	if isFalsyBool(os.Getenv("BEADS_DOLT_AUTO_START")) {
		return true
	}
	return isFalsyBool(config.GetString("dolt.auto-start"))
}

// externalNonLocalhostHost reports the configured Dolt server host when
// it is set to a non-localhost value, indicating bd is talking to a
// server it does not manage. Returns (host, true) when external,
// ("", false) otherwise.
//
// Used to branch error messages in EnsureRunning so operators with a
// non-localhost server are not told to "bd dolt start" — that command
// won't help them and reinforces a wrong mental model (GH#3518).
//
// Falls back to a zero Config when no metadata.json exists in beadsDir
// so env-only configurations (BEADS_DOLT_SERVER_HOST set, no on-disk
// config) are still detected. configfile.Load error returns are treated
// as "not external" — the existing local-flavored error message is the
// safer fallback and the configfile error will surface elsewhere.
func externalNonLocalhostHost(beadsDir string) (string, bool) {
	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		return "", false
	}
	if cfg == nil {
		cfg = &configfile.Config{}
	}
	if !cfg.IsDoltServerMode() {
		return "", false
	}
	host := cfg.GetDoltServerHost()
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "", "localhost", "127.0.0.1", "::1", "[::1]", "0.0.0.0":
		return "", false
	}
	return host, true
}

// isFalsyBool returns true when s is a recognized "false" value:
// anything strconv.ParseBool accepts as false, or "off" (case-insensitive).
// Leading/trailing whitespace is trimmed before parsing.
func isFalsyBool(s string) bool {
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "off") {
		return true
	}
	b, err := strconv.ParseBool(s)
	return err == nil && !b
}

// readyTimeout returns the timeout used by waitForReady when starting the
// dolt sql-server. Defaults to 10 seconds, but can be overridden via the
// BEADS_DOLT_READY_TIMEOUT environment variable (positive integer seconds).
// First-run Dolt SQL engine initialization can take ~60s on slower hardware
// where the privileges.db, stats subrepo, and other bootstrap work must
// happen before the MySQL listener accepts TCP connections. See GH#3142.
func readyTimeout() time.Duration {
	const defaultTimeout = 10 * time.Second
	v := strings.TrimSpace(os.Getenv("BEADS_DOLT_READY_TIMEOUT"))
	if v == "" {
		return defaultTimeout
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 1 {
		fmt.Fprintf(os.Stderr,
			"Warning: BEADS_DOLT_READY_TIMEOUT=%q is not a positive integer; using default %s\n",
			v, defaultTimeout)
		return defaultTimeout
	}
	return time.Duration(secs) * time.Second
}

// SharedServerPath returns the directory for shared server state files without
// creating it. Override with BEADS_SHARED_SERVER_DIR for testing or custom
// layouts; otherwise it resolves to ~/.beads/shared-server/.
func SharedServerPath() (string, error) {
	if d := os.Getenv("BEADS_SHARED_SERVER_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".beads", "shared-server"), nil
}

// SharedServerDir returns the directory for shared server state files.
// Returns ~/.beads/shared-server/ (created on first use).
// Override with BEADS_SHARED_SERVER_DIR env var for testing or custom layouts.
func SharedServerDir() (string, error) {
	dir, err := SharedServerPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, config.BeadsDirPerm); err != nil {
		return "", fmt.Errorf("cannot create shared server directory %s: %w", dir, err)
	}
	return dir, nil
}

// SharedDoltDir returns the dolt data directory for the shared server.
// Returns ~/.beads/shared-server/dolt/ (created on first use).
func SharedDoltDir() (string, error) {
	serverDir, err := SharedServerDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(serverDir, "dolt")
	if err := os.MkdirAll(dir, config.BeadsDirPerm); err != nil {
		return "", fmt.Errorf("cannot create shared dolt directory %s: %w", dir, err)
	}
	return dir, nil
}

// resolveServerDir returns the canonical server directory for dolt state files.
// In shared server mode, returns ~/.beads/shared-server/ instead of the
// project's .beads/ directory.
func resolveServerDir(beadsDir string) string {
	if IsSharedServerMode() {
		dir, err := SharedServerDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: shared server directory unavailable, using per-project mode: %v\n", err)
			return beadsDir
		}
		return dir
	}
	return beadsDir
}

// ResolveServerDir is the exported version of resolveServerDir.
// CLI commands use this to resolve the server directory before calling
// Start, Stop, or IsRunning.
func ResolveServerDir(beadsDir string) string {
	return resolveServerDir(beadsDir)
}

// ResolveDoltDir returns the dolt data directory for the given beadsDir.
// It checks the BEADS_DOLT_DATA_DIR env var and metadata.json for a custom
// dolt_data_dir, falling back to the default .beads/dolt/ path.
//
// Note: we check for metadata.json existence before calling configfile.Load
// to avoid triggering the config.json → metadata.json migration side effect,
// which would create files in the .beads/ directory unexpectedly.
func ResolveDoltDir(beadsDir string) string {
	// Shared server mode: use centralized dolt data directory
	if IsSharedServerMode() {
		dir, err := SharedDoltDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: shared dolt directory unavailable, using per-project mode: %v\n", err)
		} else {
			return dir
		}
	}

	// Env var, metadata.json dolt_data_dir (stat-guarded), then default —
	// shared with the side-effect-free DoltDirPath so the two resolvers
	// cannot drift.
	return projectDoltDirPath(beadsDir)
}
