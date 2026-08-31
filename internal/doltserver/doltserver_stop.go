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
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/fdhygiene"
)

func Stop(beadsDir string) error {
	return StopWithForce(beadsDir, false)
}

// StopWithForce is like Stop but with an optional force flag.
func StopWithForce(beadsDir string, _ bool) error {
	state, err := IsRunning(beadsDir)
	if err != nil {
		return err
	}
	if !state.Running {
		// Server not running — still clean up any leftover state files
		// so bd dolt status won't report stale state (GH#2670).
		// Join cleanup errors with the sentinel so callers can still use
		// errors.Is(err, ErrServerNotRunning) while operators see filesystem issues.
		cleanupErr := cleanupStateFiles(beadsDir)
		return errors.Join(ErrServerNotRunning, cleanupErr)
	}

	// Flush uncommitted working set changes before stopping the server.
	// This prevents data loss when changes have been written but not yet committed.
	cfg := DefaultConfig(beadsDir)
	if flushErr := FlushWorkingSet(cfg.Host, state.Port); flushErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not flush working set before stop: %v\n", flushErr)
	}

	if err := gracefulStop(state.PID, 5*time.Second); err != nil {
		return errors.Join(err, cleanupStateFiles(beadsDir))
	}

	// In debug mode, rotate cpu.pprof → cpu-<timestamp>.pprof so the next
	// server start does not overwrite this run's profile. Only meaningful
	// after a graceful (SIGTERM) exit — SIGKILL skips pkg/profile's
	// deferred flush, leaving nothing to rotate. Best-effort.
	if IsDebugMode() {
		rotateDebugProfile(beadsDir)
	}

	return cleanupStateFiles(beadsDir)
}

// sanitizeInheritedFDs marks descriptors bd inherited but did not open
// close-on-exec, so the detached sql-server does not pin them for its whole
// lifetime (GH#4634). It is a free function rather than an inline call because
// the spawn path shadows the debug package with a local `debug bool`.
func sanitizeInheritedFDs() {
	if leaked := fdhygiene.MarkInheritedCloexec(); len(leaked) > 0 {
		debug.Logf("marked %d inherited fd(s) close-on-exec before starting dolt sql-server: %v", len(leaked), leaked)
	}
}

// cleanupStateFiles removes all server state files (PID and port).
// Returns a joined error for non-NotExist removal failures so callers
// can surface filesystem problems while still treating "already clean"
// as success. Logs non-NotExist errors at debug level (GH#2670).
func cleanupStateFiles(beadsDir string) error {
	var errs []error
	for _, path := range []string{pidPath(beadsDir), portPath(beadsDir)} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			debug.Logf("failed to remove server state file %s: %v", path, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func StateFilePaths(beadsDir string) []string {
	return []string{
		pidPath(beadsDir),
		portPath(beadsDir),
		lockPath(beadsDir),
		logPath(beadsDir),
		logPath(beadsDir) + ".1",
		DebugProfileDir(beadsDir),
		doltServerConfigPath(beadsDir),
	}
}

func RemoveStateFiles(beadsDir string) []error {
	var errs []error
	for _, path := range StateFilePaths(beadsDir) {
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// LogPath returns the path to the server log file.
func LogPath(beadsDir string) string {
	return logPath(beadsDir)
}

func LockPath(beadsDir string) string {
	return lockPath(beadsDir)
}

// killStaleServersForDir finds and kills orphan dolt sql-server processes for
// the current repo's Dolt data directory that are not tracked by the canonical
// PID file. Only processes that beads started (tracked via the PID file) are
// eligible for cleanup. Externally-managed servers are never killed.
//
// A process is considered "external" (never kill) when any of:
//   - ResolveServerMode() returns ServerModeExternal (explicit port, shared server, etc.)
//   - No PID file exists (beads has no record of starting a server)
func killStaleServersForDir(beadsDir string, allPIDs []int, inDir func(int, string) bool, kill func(int) error) ([]int, error) {
	if !staleServerCleanupAllowed(beadsDir, allPIDs) {
		return nil, nil
	}

	serverDir := resolveServerDir(beadsDir)
	canonicalPID := readCanonicalPID(serverDir)
	if canonicalPID == 0 {
		return nil, nil
	}

	ownedDoltDir := ResolveDoltDir(serverDir)
	return killStaleServerPIDs(allPIDs, canonicalPID, ownedDoltDir, inDir, kill)
}

func staleServerCleanupAllowed(beadsDir string, allPIDs []int) bool {
	if len(allPIDs) == 0 {
		return false
	}
	// If auto-start is disabled the server is externally managed (e.g., by
	// systemd or a manual bd dolt start), so we must not kill any processes.
	// IsAutoStartDisabled covers the env/config setting; ResolveServerMode also
	// covers explicit port, shared-server, and embedded configurations.
	return !IsAutoStartDisabled() && ResolveServerMode(beadsDir) != ServerModeExternal
}

func readCanonicalPID(serverDir string) int {
	data, err := os.ReadFile(pidPath(serverDir))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func killStaleServerPIDs(allPIDs []int, canonicalPID int, ownedDoltDir string, inDir func(int, string) bool, kill func(int) error) ([]int, error) {
	var killed []int
	for _, pid := range allPIDs {
		if pid == os.Getpid() {
			continue
		}
		if pid == canonicalPID {
			continue // preserve canonical server
		}
		if !inDir(pid, ownedDoltDir) {
			continue // preserve other repos' Dolt servers
		}
		if err := kill(pid); err == nil {
			killed = append(killed, pid)
		}
	}
	return killed, nil
}

// KillStaleServers finds and kills orphan dolt sql-server processes for the
// current repo's Dolt data directory that are not tracked by the canonical PID
// file. Returns the PIDs of killed processes.
//
// When auto-start is disabled (BEADS_DOLT_AUTO_START=0 or dolt.auto-start:
// false), this function is a no-op — the dolt server is externally managed
// and must not be killed by bd (GH#2641).
func KillStaleServers(beadsDir string) ([]int, error) {
	if IsAutoStartDisabled() {
		return nil, nil
	}
	allPIDs := listDoltProcessPIDs()
	return killStaleServersForDir(
		beadsDir,
		allPIDs,
		isProcessInDir,
		func(pid int) error {
			proc, err := os.FindProcess(pid)
			if err != nil {
				return err
			}
			return proc.Kill()
		},
	)
}

// waitForReady polls until the server accepts TCP connections AND greets
// with a MySQL handshake. Draining the handshake before closing the probe
// connection makes Close() send TCP FIN instead of RST, which prevents the
// dolt sql-server process from interpreting probe closes as aborted MySQL
// handshakes and crashing (see gastownhall/beads#4132, #4133).
//
// A dial that succeeds but never greets (TCP listener accepting, MySQL
// engine not yet writing) is not treated as ready: this function keeps
// polling until either a greeting arrives or the deadline is reached.
func waitForReady(host string, port int, timeout time.Duration) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		greeted, err := ProbeSQLServer("tcp", addr, 500*time.Millisecond) //nolint:gosec // G704: addr is built from internal host+port, not user input
		if err == nil && greeted {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("timeout after %s waiting for server at %s", timeout, addr)
}

// ensureDoltIdentity sets dolt global user identity from git config if not already set.
func ensureDoltIdentity() error {
	// Check if dolt identity is already configured
	nameCmd := exec.Command("dolt", "config", "--global", "--get", "user.name")
	if out, err := nameCmd.Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		return nil // Already configured
	}

	// Try to get identity from git
	gitName := "beads"
	gitEmail := "beads@localhost"

	if out, err := exec.Command("git", "config", "user.name").Output(); err == nil {
		if name := strings.TrimSpace(string(out)); name != "" {
			gitName = name
		}
	}
	if out, err := exec.Command("git", "config", "user.email").Output(); err == nil {
		if email := strings.TrimSpace(string(out)); email != "" {
			gitEmail = email
		}
	}

	if out, err := exec.Command("dolt", "config", "--global", "--add", "user.name", gitName).CombinedOutput(); err != nil {
		return fmt.Errorf("setting dolt user.name: %w\n%s", err, out)
	}
	if out, err := exec.Command("dolt", "config", "--global", "--add", "user.email", gitEmail).CombinedOutput(); err != nil {
		return fmt.Errorf("setting dolt user.email: %w\n%s", err, out)
	}

	return nil
}

// bdDoltMarker is written after a current bd process creates or acknowledges a
// local Dolt repository. Its absence in an existing .dolt/ directory indicates
// the database was created by a pre-0.56 bd version (which used embedded mode).
// Those databases are incompatible with the current server-only architecture.
const bdDoltMarker = ".bd-dolt-ok"

// MarkDoltDirCompatible writes the canonical bd compatibility marker when
// doltDir contains a local Dolt repository. It no-ops when there is no .dolt/
// directory, which lets server and repair paths call it defensively.
func MarkDoltDirCompatible(doltDir string) error {
	if doltDir == "" {
		return errors.New("dolt directory is required")
	}
	dotDolt := filepath.Join(doltDir, ".dolt")
	if info, err := os.Stat(dotDolt); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("checking dolt metadata directory %s: %w", dotDolt, err)
	} else if !info.IsDir() {
		return fmt.Errorf("dolt metadata path %s is not a directory", dotDolt)
	}
	markerPath := filepath.Join(doltDir, bdDoltMarker)
	if _, err := os.Stat(markerPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking dolt compatibility marker %s: %w", markerPath, err)
	}
	if err := os.WriteFile(markerPath, []byte("ok\n"), 0600); err != nil {
		return fmt.Errorf("writing dolt compatibility marker %s: %w", markerPath, err)
	}
	return nil
}

// ensureDoltInit initializes a dolt database directory if .dolt/ doesn't exist.
// If .dolt/ exists, seeds the .bd-dolt-ok marker for existing working databases.
// See GH#2137 for background on pre-0.56 database compatibility.
func ensureDoltInit(doltDir string) error {
	if err := os.MkdirAll(doltDir, config.BeadsDirPerm); err != nil {
		return fmt.Errorf("creating dolt directory: %w", err)
	}

	dotDolt := filepath.Join(doltDir, ".dolt")

	if _, err := os.Stat(dotDolt); err == nil {
		// .dolt/ exists — seed the marker if missing.
		// This is the non-destructive path: we just mark existing databases
		// as known. The destructive recovery path (RecoverPreV56DoltDir) is
		// triggered separately during version upgrades.
		_ = MarkDoltDirCompatible(doltDir)
		return nil // Already initialized
	}

	cmd := exec.Command("dolt", "init")
	cmd.Dir = doltDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("dolt init: %w\n%s", err, out)
	}

	// Write version marker so future runs know this database is compatible.
	_ = MarkDoltDirCompatible(doltDir)

	return nil
}

// RecoverPreV56DoltDir removes and reinitializes a dolt database that was
// created by a pre-0.56 bd version. Call this during version upgrade detection
// (e.g., from autoMigrateOnVersionBump when previousVersion < 0.56).
//
// Pre-0.56 databases used embedded Dolt mode with a different Dolt library
// version that may produce nil DoltDB values, causing panics (GH#2137).
// The data is unrecoverable — the fix is to start fresh.
//
// Returns true if recovery was performed, false if not needed.
func RecoverPreV56DoltDir(doltDir string) (bool, error) {
	dotDolt := filepath.Join(doltDir, ".dolt")
	if _, err := os.Stat(dotDolt); os.IsNotExist(err) {
		return false, nil // No .dolt/ directory — nothing to recover
	}

	markerPath := filepath.Join(doltDir, bdDoltMarker)
	if _, err := os.Stat(markerPath); err == nil {
		return false, nil // Marker exists — database is from 0.56+
	}

	fmt.Fprintf(os.Stderr, "Detected dolt database from an older bd version (pre-0.56).\n")
	fmt.Fprintf(os.Stderr, "Rebuilding dolt database at %s ...\n", doltDir)

	if err := os.RemoveAll(dotDolt); err != nil {
		return false, fmt.Errorf("cannot remove old dolt database at %s: %w\n\n"+
			"Manually delete %s and retry", dotDolt, err, dotDolt)
	}

	// Reinitialize
	if err := ensureDoltInit(doltDir); err != nil {
		return true, fmt.Errorf("recovery: %w", err)
	}

	return true, nil
}

// IsPreV56DoltDir returns true if doltDir contains a .dolt/ directory that
// was NOT created by bd 0.56+ (missing .bd-dolt-ok marker). These databases
// were created by the old embedded Dolt mode and may be incompatible.
// Used by doctor checks to detect potentially problematic dolt databases.
func IsPreV56DoltDir(doltDir string) bool {
	dotDolt := filepath.Join(doltDir, ".dolt")
	if _, err := os.Stat(dotDolt); os.IsNotExist(err) {
		return false // No .dolt/ at all
	}
	markerPath := filepath.Join(doltDir, bdDoltMarker)
	_, err := os.Stat(markerPath)
	return os.IsNotExist(err)
}
