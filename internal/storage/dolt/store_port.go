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
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/jonbaldie/beads/internal/doltserver"
)

var testDatabasePrefixes = []string{
	"testdb_",
	"beads_test",
	"beads_pt",
	"beads_vr",
	"doctest_",
	"doctortest_",
	"benchdb_",
}

// isTestDatabaseName returns true if the database name matches known test patterns.
// This is a pattern-based firewall — it does not rely on environment variables.
func isTestDatabaseName(name string) bool {
	for _, prefix := range testDatabasePrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// productionPortReasons returns human-readable labels for each rule that
// flags cfg.ServerPort as a production port. An empty slice means the port
// is not detected as production.
//
// Detection sources, in order:
//  1. cfg.ServerPort == DefaultSQLPort (legacy default 3307). Unconditional —
//     never suppressed by BEADS_TEST_SERVER=1. The well-known Dolt default
//     port is the single highest-confidence production signal, and a
//     dedicated test server opting out of the other heuristics still must
//     not bind to it.
//  2. BEADS_PRODUCTION_PORT env var, parsed to int, matches cfg.ServerPort.
//  3. cfg.BeadsDir/dolt-server.port file present and contains cfg.ServerPort.
//
// Rules 2 and 3 are suppressed when BEADS_TEST_SERVER=1: they are
// heuristics (an env var or an on-disk port file, either of which can be
// stale or misconfigured) rather than the fixed default port, so an
// operator's explicit opt-in into a dedicated test-server lane is honored
// for those two only.
//
// Rule 3 deliberately does not fall back to filepath.Dir(cfg.Path) when
// BeadsDir is empty — the port-resolution chain in applyConfigDefaults
// already does that fallback for resolution purposes, but using it here
// would treat any cfg.Path under a directory that happens to contain a
// stray dolt-server.port file (e.g. /tmp/dolt-server.port from a leaked
// dev server) as production. Test fixtures commonly set cfg.Path under
// /tmp without a real BeadsDir; only an explicitly set BeadsDir is
// considered authoritative for the production check.
//
// All rules read deterministic state (constant, env, on-disk port file).
// No state is mutated. Multiple rules can match; the panic message lists all.
func productionPortReasons(cfg *Config) []string {
	if cfg == nil || cfg.ServerPort <= 0 {
		return nil
	}
	reasons := productionDefaultPortReasons(cfg.ServerPort)
	// Rules 2 and 3 are the suppressible heuristics: honor the operator's
	// BEADS_TEST_SERVER=1 opt-in for a dedicated test server by skipping
	// them. Rule 1 above is intentionally evaluated before this check and
	// is never suppressed.
	if os.Getenv("BEADS_TEST_SERVER") == "1" {
		return reasons
	}
	return append(reasons, productionHeuristicPortReasons(cfg)...)
}

func productionDefaultPortReasons(port int) []string {
	if port != DefaultSQLPort {
		return nil
	}
	return []string{fmt.Sprintf("port %d == DefaultSQLPort", port)}
}

func productionHeuristicPortReasons(cfg *Config) []string {
	var reasons []string
	if reason := productionEnvironmentPortReason(cfg.ServerPort); reason != "" {
		reasons = append(reasons, reason)
	}
	if reason := productionPortFileReason(cfg); reason != "" {
		reasons = append(reasons, reason)
	}
	return reasons
}

func productionEnvironmentPortReason(port int) string {
	env := os.Getenv("BEADS_PRODUCTION_PORT")
	if env == "" {
		return ""
	}
	p, err := strconv.Atoi(env)
	if err != nil || p <= 0 || p != port {
		return ""
	}
	return fmt.Sprintf("BEADS_PRODUCTION_PORT=%d matches", p)
}

func productionPortFileReason(cfg *Config) string {
	if cfg.BeadsDir == "" {
		return ""
	}
	p := doltserver.ReadPortFile(cfg.BeadsDir)
	if p <= 0 || p != cfg.ServerPort {
		return ""
	}
	return fmt.Sprintf("%s/%s contains %d", cfg.BeadsDir, doltserver.PortFileName, p)
}

// isProductionPort reports whether cfg.ServerPort matches any production-port
// indicator. Pure at call time — port resolution itself happens earlier in
// applyConfigDefaults; this helper only inspects already-resolved state.
//
// BEADS_TEST_SERVER=1 narrows detection to Rule 1 only (port ==
// DefaultSQLPort): the operator has explicitly opted into the dedicated
// test-server lane (e.g. a per-test container, an external test port), which
// suppresses the BEADS_PRODUCTION_PORT and dolt-server.port heuristics
// (Rules 2 and 3, see productionPortReasons). Rule 1 stays unconditional —
// a test server must never bind to the well-known default port 3307,
// opt-in or not. The database-name firewall in New is a separate AD-01
// defense with its own independent BEADS_TEST_SERVER=1 opt-out; it is not
// affected by this function.
//
// See productionPortReasons for the three detection sources and the
// suppression rule.
func isProductionPort(cfg *Config) bool {
	return len(productionPortReasons(cfg)) > 0
}

// autoStartRefs tracks in-process reference counts for auto-started dolt
// sql-server processes, keyed by resolved server directory. When the count
// drops to zero, the server is stopped. This prevents test-started servers
// from leaking (GH#2542) while allowing multiple stores to share one server.
// Normal repo-local auto-starts are intentionally not tracked here: those
// servers should stay up like an explicit `bd dolt start`, rather than being
// torn down at the end of each command.
type autoStartRefState struct {
	mu sync.Mutex
	m  map[string]int
}

var autoStartRefs = &autoStartRefState{}

func autoStartAcquire(serverDir string) {
	autoStartRefs.acquire(serverDir)
}

func (s *autoStartRefState) acquire(serverDir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[string]int)
	}
	s.m[serverDir]++
}

// autoStartAcquireExisting increments the refcount for serverDir only when the
// current process is already tracking that auto-started server. This lets later
// stores share the same test-owned server without taking ownership of servers
// started by other processes.
func autoStartAcquireExisting(serverDir string) bool {
	return autoStartRefs.acquireExisting(serverDir)
}

func (s *autoStartRefState) acquireExisting(serverDir string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil || s.m[serverDir] <= 0 {
		return false
	}
	s.m[serverDir]++
	return true
}

// autoStartRelease decrements the refcount for serverDir and stops the server
// when it reaches zero. Returns any error from stopping the server.
// If the server is already stopped (e.g. killed externally, or never started),
// the ErrServerNotRunning sentinel is silently absorbed to avoid false
// "failed to stop" warnings (GH#2670).
func autoStartRelease(serverDir string) error {
	return autoStartRefs.release(serverDir)
}

func (s *autoStartRefState) release(serverDir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		return nil
	}
	s.m[serverDir]--
	if s.m[serverDir] <= 0 {
		delete(s.m, serverDir)
		// Stop is idempotent: returns ErrServerNotRunning (possibly joined
		// with cleanup errors) when the server is already gone. Strip the
		// sentinel but propagate any real cleanup failures.
		return doltserver.IgnoreNotRunning(doltserver.Stop(serverDir))
	}
	return nil
}

// undoRejectedAutoStart cleans up the side effects of a speculative
// auto-start that newServerMode's fail-closed checks (GH#4052) decided not
// to use.
//
// It restores the port file to the pre-call snapshot: EnsureRunningDetailed
// writes serverDir's port file with the new server's actual port before
// either fail-closed check runs (Start()'s writePortFile, or the
// adopt-existing-server path's EnsurePortFile), and that port file is the
// second-highest-precedence port source. Left in place, it would let a
// second, identical invocation resolve the port file instead of the
// authoritative source that just failed, adopt the server we declined to
// use, and silently succeed — permanently disarming the guard after exactly
// one invocation.
//
// When we spawned the server ourselves (startedByUs), it also stops it: we
// have decided not to use it, so leaving a stray dolt process running is an
// unrequested side effect. When autoStartedDir is set, the server is
// refcount-tracked (the test/test-database path via autoStartAcquire); that
// path already stops the server once the refcount reaches zero, so
// autoStartRelease is used instead of a direct Stop to avoid pulling the rug
// out from under another store instance sharing the same auto-started
// server. An adopted pre-existing server (startedByUs == false) is left
// running — we didn't start it, so we don't stop it, but its port file
// write must still be undone.
//
// Best-effort throughout: cleanup failures are reported on stderr but never
// returned, so they cannot mask the caller's fail-closed error, which is the
// one that matters.
func undoRejectedAutoStart(serverDir string, startedByUs bool, autoStartedDir string, snap doltserver.PortFileSnapshot, snapErr error) {
	if startedByUs {
		if autoStartedDir != "" {
			if err := autoStartRelease(autoStartedDir); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to stop rejected auto-started dolt server: %v\n", err)
			}
		} else if err := doltserver.IgnoreNotRunning(stopRejectedAutoStartedServer(serverDir)); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to stop rejected auto-started dolt server: %v\n", err)
		}
	}
	if snapErr != nil {
		// The pre-call snapshot itself failed (e.g. a permissions error
		// reading an existing file) — restoring a zero-value snapshot here
		// could wrongly delete a port file we never actually read. Leave the
		// port file alone rather than guess.
		return
	}
	if err := doltserver.RestorePortFile(serverDir, snap); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to restore port file after rejected auto-start: %v\n", err)
	}
}

// shouldStopAutoStartedServerOnClose reports whether an auto-started server
// should be treated as test-owned cleanup state instead of a normal repo-local
// server. In real repos, auto-start should behave like a persistent helper
// server, not a single-command subprocess.
func shouldStopAutoStartedServerOnClose(cfg *Config) bool {
	if os.Getenv("BEADS_TEST_MODE") == "1" {
		return true
	}
	return isTestDatabaseName(cfg.Database)
}
