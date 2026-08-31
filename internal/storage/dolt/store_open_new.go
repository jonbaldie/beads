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
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/doltserver"
)

// New creates a new Dolt storage backend.
// Connects to a running dolt sql-server via MySQL protocol (pure Go).
func New(ctx context.Context, cfg *Config) (*DoltStore, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("database path is required")
	}

	applyConfigDefaults(cfg)

	// Hard guard: tests must NEVER connect to the production Dolt server.
	// applyConfigDefaults rewrites a production port to 1 in BEADS_TEST_MODE=1
	// for fail-loud-but-continue behavior; this panic is defense-in-depth for
	// any path that bypasses or post-edits the rewrite. Generalized via
	// isProductionPort so non-3307 production deployments are covered.
	if os.Getenv("BEADS_TEST_MODE") == "1" && isProductionPort(cfg) {
		panic(buildTestModeProductionPortPanic(cfg))
	}

	// Database-name firewall: refuse to open a test-named database on any
	// server unless the operator opted in via BEADS_TEST_SERVER=1. This is
	// the second of two AD-01 defenses (the first is the production-port
	// guard above). Returning an error (not panic) lets tests assert on it.
	if isTestDatabaseName(cfg.Database) && os.Getenv("BEADS_TEST_SERVER") != "1" {
		addr := net.JoinHostPort(cfg.ServerHost, strconv.Itoa(cfg.ServerPort))
		if cfg.ServerSocket != "" {
			addr = cfg.ServerSocket
		}
		return nil, fmt.Errorf(
			"refusing to connect test database %q to server %s: "+
				"set BEADS_TEST_SERVER=1 on a dedicated test server, "+
				"or use test helpers in internal/storage/dolt/testserver",
			cfg.Database, addr)
	}

	return newServerMode(ctx, cfg)
}

// resolveLocalActiveDatabaseDir returns an authoritative local path only for
// server configurations whose storage ownership is known. It deliberately
// does not infer locality from Path, CLIDir, or filesystem existence: external
// servers may leave unrelated client-local directories at those locations.
func resolveLocalActiveDatabaseDir(cfg *Config) string {
	if !localActiveDatabaseDirConfigEligible(cfg) {
		return ""
	}

	// An endpoint supplied directly by the environment may be any server,
	// including a container or tunnel on localhost. It is not proof that this
	// process can inspect the server's data directory.
	if hasExplicitServerPort() {
		return ""
	}

	if doltserver.IsSharedServerMode() {
		return filepath.Join(doltserver.ResolveDoltDir(cfg.BeadsDir), cfg.Database)
	}

	// Owned mode plus effective auto-start authority is the affirmative proof
	// that the configured data root belongs to this local beads instance.
	if !cfg.AutoStart || doltserver.ResolveServerMode(cfg.BeadsDir) != doltserver.ServerModeOwned {
		return ""
	}
	return filepath.Join(cfg.Path, cfg.Database)
}

func localActiveDatabaseDirConfigEligible(cfg *Config) bool {
	if cfg == nil || cfg.BeadsDir == "" || cfg.Database == "" {
		return false
	}
	if cfg.Gateway || cfg.ProxiedServer || cfg.ServerSocket != "" || cfg.ServerTLS {
		return false
	}
	return isLocalHost(cfg.ServerHost)
}

func hasExplicitServerPort() bool {
	return os.Getenv("BEADS_DOLT_SERVER_PORT") != "" || os.Getenv("BEADS_DOLT_PORT") != ""
}

// buildTestModeProductionPortPanic returns the multi-line panic message for
// the BEADS_TEST_MODE=1 + production-port hard-guard. Format follows
// AD-01 Wireframe 1: scannable header + database/path/server fields,
// list of detection rules that matched, and a fix block naming each
// supported escape hatch.
func buildTestModeProductionPortPanic(cfg *Config) string {
	addr := net.JoinHostPort(cfg.ServerHost, strconv.Itoa(cfg.ServerPort))
	if cfg.ServerSocket != "" {
		addr = cfg.ServerSocket
	}
	reasons := productionPortReasons(cfg)
	if len(reasons) == 0 {
		// Should be unreachable (caller checks isProductionPort first), but
		// keep the message coherent if it ever hits.
		reasons = []string{"production-port heuristic matched"}
	}
	var rules strings.Builder
	for _, r := range reasons {
		rules.WriteString("    - ")
		rules.WriteString(r)
		rules.WriteString("\n")
	}
	var fixLines strings.Builder
	fixLines.WriteString("    - point BEADS_DOLT_SERVER_PORT at a non-production port (test server)\n")
	fixLines.WriteString("    - or use test helpers in internal/storage/dolt/testserver\n")
	// BEADS_TEST_SERVER=1 does not suppress Rule 1 (port == DefaultSQLPort,
	// see productionPortReasons) — only list it as a fix when the port
	// itself isn't the reason this fired, so the message never claims an
	// opt-in that would not actually resolve this panic.
	if cfg.ServerPort != DefaultSQLPort {
		fixLines.WriteString("    - or set BEADS_TEST_SERVER=1 on the spawned test server's env\n")
	}
	return fmt.Sprintf(
		"refusing to connect: BEADS_TEST_MODE=1 but resolved server port is production\n\n"+
			"  database: %s\n"+
			"  path:     %s\n"+
			"  server:   %s\n"+
			"  detected as production via:\n"+
			"%s"+
			"  fix:\n"+
			"%s",
		cfg.Database,
		cfg.Path,
		addr,
		rules.String(),
		fixLines.String(),
	)
}

// dialProbe reports whether an address accepts a connection within timeout.
// Declared as a var (not a plain call) so unit tests can stub connectivity
// without a live Dolt server. Returns nil when the endpoint is reachable.
//
// Delegates to doltserver.ProbeSQLServer so the probe drains the MySQL
// handshake before closing (Close() then sends FIN, not RST) — applies to
// unix sockets too, since the protocol spoken over them is still MySQL.
// See gastownhall/beads#4132, #4133.
var dialProbe = func(network, addr string, timeout time.Duration) error {
	_, err := doltserver.ProbeSQLServer(network, addr, timeout)
	return err
}

// ResolveSocketTransport applies a socket-first / TCP-fallback policy and
// returns the effective unix socket path to use ("" means use TCP).
//
// A configured unix socket is a preference, not a hard requirement. Dolt's
// /tmp/mysql.sock is created only on some server start paths and is frequently
// absent while the server is fully reachable on its TCP port — when that
// happens, every socket-mode bd operation (and `gt mq submit`, cross-rig bead
// reads that route through bd) fails hard with no fallback (gt-28itz). This
// mirrors the conservative socket-first/TCP-fallback semantics already used on
// the gt-CLI side (internal/cmd/dolt_dsn.go localDoltSocketPath/buildDoltDSN).
//
// Returns the socket unchanged when: no socket is configured, the socket is
// connectable, or neither the socket nor TCP is reachable (the latter is left
// to the normal error path so its socket-specific hint still surfaces a true
// outage rather than masking it behind a TCP error).
//
// Exported because the store is no longer the only consumer: `bd serve` builds
// a unit-of-work provider against the same server from the same connection
// settings, and a transport policy that only one of them applies is a workspace
// where CLI commands work and the HTTP server cannot connect.
func ResolveSocketTransport(socket, host string, port int, timeout time.Duration) string {
	if socket == "" {
		return ""
	}
	if dialProbe("unix", socket, timeout) == nil {
		return socket // socket is live — keep using it
	}
	if port > 0 && dialProbe("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout) == nil {
		debug.Logf("dolt: socket %s unreachable, falling back to TCP %s\n", socket, net.JoinHostPort(host, strconv.Itoa(port)))
		return "" // socket down but TCP up — transparently fall back to TCP
	}
	return socket // both down (or no TCP port) — keep socket for the error path
}

// ensureRunningDetailed starts (or reuses) the repo-local auto-started dolt
// sql-server. Declared as a var (not a plain call) so unit tests can stub
// auto-start outcomes — including a retargeted port — without spawning a
// real dolt sql-server process.
var ensureRunningDetailed = doltserver.EnsureRunningDetailed

// stopRejectedAutoStartedServer stops a repo-local dolt sql-server that
// newServerMode's fail-closed checks (GH#4052) decided not to use. Declared
// as a var (matching ensureRunningDetailed above) so unit tests can stub it
// and assert whether it was invoked, without spawning or killing a real
// dolt sql-server process.
var stopRejectedAutoStartedServer = doltserver.Stop
