package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage/doltutil"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

var doltStatusCmd = &cobra.Command{
	Use:           "status",
	SilenceUsage:  true,
	SilenceErrors: true,
	Short:         "Show Dolt engine status",
	Long: `Show the status of the Dolt engine for the current project.

In embedded mode, reports that the Dolt engine runs in-process and shows
the on-disk data directory. For beads-managed (local) servers, displays
PID, port, and data directory from the local PID file. For externally-
managed servers — a shared server (dolt.shared-server: true), a remote
dolt_server_host, or a local server managed outside bd (dolt.auto-start:
false, e.g. an orchestrator-shared sql-server) — pings the configured
endpoint via SQL and reports reachability, server version, and database.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		beadsDir := selectedDoltBeadsDir()
		if beadsDir == "" {
			return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
		}
		cfg, cfgErr := configfile.Load(beadsDir)
		if cfgErr != nil {
			return HandleError("loading config: %v", cfgErr)
		}
		if cfg == nil {
			cfg = configfile.DefaultConfig()
		}
		if err := validateConfiguredBackend(cfg); err != nil {
			return HandleError("%v", err)
		}
		// A non-Dolt backend (SQLite or a removed-backend tombstone) has no Dolt engine;
		// report the backend rather than misdescribing an embedded Dolt server
		// (parity with `bd dolt show`, which already special-cases this).
		if cfg.GetBackend() != configfile.BackendDolt {
			fmt.Printf("Backend: %s (no Dolt engine)\n", cfg.GetBackend())
			return nil
		}
		if !usesSQLServer() {
			showEmbeddedDoltStatus(beadsDir)
			return nil
		}

		// For externally-managed Dolt servers, the local PID file is
		// meaningless or absent — ping the configured endpoint via SQL
		// instead. Two flavors qualify:
		//   - non-local host (Hosted Dolt, remote shared sql-server, bd-q35w)
		//   - local host with auto-start disabled (an orchestrator or
		//     systemd manages the server lifecycle, be-0eyj)
		//
		// IsAutoStartDisabled reads the active (globally-bound) config and
		// BEADS_DOLT_AUTO_START env, not the per-beadsDir cfg loaded above.
		// That coupling is intentional and consistent with every
		// other call site of IsAutoStartDisabled in this package — both
		// resolve against the same active workspace at command time.
		if cfg != nil && shouldUseExternalDoltStatus(cfg, doltserver.IsAutoStartDisabled(), doltserver.IsSharedServerMode()) {
			runExternalDoltStatus(beadsDir, cfg)
			return nil
		}

		serverDir := doltserver.ResolveServerDir(beadsDir)

		state, err := doltserver.IsRunning(serverDir)
		if err != nil {
			return HandleError("%v", err)
		}
		renderLocalDoltStatus(state, serverDir)
		return nil
	},
}

// renderLocalDoltStatus writes the bd-managed (local PID-file) status of
// the Dolt server to stdout, honoring jsonOutput. Extracted from the
// doltStatusCmd Run closure so the bd-managed output path is unit-testable
// without requiring a live dolt sql-server (the externally-managed path
// is exercised by TestRunExternalDoltStatus_Unreachable).
func renderLocalDoltStatus(state *doltserver.State, serverDir string) {
	if isJSONOutput() {
		if err := outputJSON(state); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return
	}
	if state == nil || !state.Running {
		cfg := doltserver.DefaultConfig(serverDir)
		fmt.Println("Dolt server: not running")
		fmt.Printf("  Expected port: %d\n", cfg.Port)
		return
	}
	fmt.Println("Dolt server: running")
	fmt.Printf("  PID:  %d\n", state.PID)
	fmt.Printf("  Port: %d\n", state.Port)
	fmt.Printf("  Data: %s\n", state.DataDir)
	fmt.Printf("  Logs: %s\n", doltserver.LogPath(serverDir))
	if doltserver.IsSharedServerMode() {
		fmt.Println("  Mode: shared server")
	}
	if doltserver.IsDebugMode() {
		fmt.Println("  Debug: on (loglevel=debug, --prof cpu)")
		fmt.Printf("  Profile dir: %s\n", doltserver.DebugProfileDir(serverDir))
	}
	if isDoltLocalOnly() {
		fmt.Println("  Remote sync: disabled (dolt.local-only=true)")
	}
}

// shouldUseExternalDoltStatus reports whether bd dolt status should treat
// the server as externally-managed and probe via SQL instead of consulting
// the local PID file. Returns true when:
//   - shared-server mode is enabled (and not proxied) — a shared server's
//     lifecycle is owned by something other than bd (a Homebrew service,
//     systemd/launchd unit, or a sibling clone), so bd has no PID file for
//     it even when the host is local and auto-start is enabled. Without this
//     branch, status reports "not running" while bd CRUD commands, bd dolt
//     test, and bd dolt show all connect to the server fine (GH#3218). This
//     is checked BEFORE the server-mode guard because shared-server mode
//     wins over a stale metadata.json that still pins dolt_mode="embedded"
//     — mirroring the loadServerMode override in main.go (GH#2946). That
//     stale-metadata case (dolt.shared-server: true in config.yaml, which
//     IsDoltServerMode does not consult) is exactly where the residual
//     GH#3218 bug lived.
//   - dolt_mode=server with a non-local host (Hosted Dolt, remote shared
//     sql-server) — the PID file is on a different machine.
//   - dolt_mode=server with a local host but bd auto-start is disabled —
//     the server lifecycle is owned by something outside bd (e.g. an
//     orchestrator or systemd unit), so no bd PID file exists. Without
//     this branch, status reports "not running" even when bd CRUD
//     commands successfully connect to the server (be-0eyj).
//
// When false, the caller falls back to the PID-file path that reports
// PID, port, log path, and data directory for bd-managed servers.
//
// autoStartDisabled and sharedServerMode are passed in (rather than read
// here) so the predicate is pure and unit-testable without manipulating
// package-level config or process env.
func shouldUseExternalDoltStatus(cfg *configfile.Config, autoStartDisabled, sharedServerMode bool) bool {
	if cfg == nil {
		return false
	}
	// Shared-server mode wins even over an explicit metadata.json
	// dolt_mode="embedded" (loadServerMode override, main.go, GH#2946), so
	// this must precede the IsDoltServerMode guard — otherwise a workspace
	// with dolt.shared-server: true in config.yaml and stale embedded
	// metadata still falls through to the PID-file "not running" path.
	// Proxied-server mode is excluded, matching that override's !psm guard.
	if sharedServerMode && !cfg.IsDoltProxiedServerMode() {
		return true
	}
	if !cfg.IsDoltServerMode() {
		return false
	}
	if !isLocalHost(cfg.GetDoltServerHost()) {
		return true
	}
	return autoStartDisabled
}

// isLocalHost reports whether host refers to this machine. Used to
// distinguish beads-managed local servers from externally-hosted ones.
func isLocalHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return true // empty defaults to local
	}
	switch h {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}

// runExternalDoltStatus queries an externally-hosted Dolt server and prints
// (or returns, for --json) status. Unlike the local path, there is no PID or
// log file — reachability, version, host/port/database, and TLS mode are the
// user-relevant signals.
func runExternalDoltStatus(beadsDir string, cfg *configfile.Config) {
	status := newExternalDoltStatus(beadsDir, cfg)
	status.probe()
	if isJSONOutput() {
		if err := outputJSON(status.jsonResult()); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return
	}
	status.print()
}

type externalDoltStatus struct {
	host     string
	port     int
	user     string
	password string
	database string
	tls      bool
	running  bool
	version  string
	connErr  error
}

func newExternalDoltStatus(beadsDir string, cfg *configfile.Config) externalDoltStatus {
	port := doltserver.DefaultConfig(beadsDir).Port
	return externalDoltStatus{
		host:     cfg.GetDoltServerHost(),
		port:     port,
		user:     cfg.GetDoltServerUser(),
		password: cfg.GetDoltServerPasswordForPort(port),
		database: cfg.GetDoltDatabase(),
		tls:      cfg.GetDoltServerTLS(),
	}
}

func (status *externalDoltStatus) probe() {
	dsn := doltutil.ServerDSN{
		Host:     status.host,
		Port:     status.port,
		User:     status.user,
		Password: status.password,
		TLS:      status.tls,
		Timeout:  5 * time.Second,
	}.String()
	status.probeWithPassword(dsn)
}

func (status *externalDoltStatus) probeWithPassword(dsn string) {
	db, openErr := sql.Open("mysql", dsn)
	if openErr != nil {
		status.connErr = openErr
		return
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if pingErr := db.PingContext(ctx); pingErr != nil {
		status.connErr = pingErr
		return
	}
	status.running = true
	// Best-effort version lookup; don't treat errors as fatal.
	_ = db.QueryRowContext(ctx, "SELECT @@version").Scan(&status.version)
}

func (status externalDoltStatus) jsonResult() map[string]interface{} {
	result := map[string]interface{}{
		"mode":     "external",
		"host":     status.host,
		"port":     status.port,
		"user":     status.user,
		"database": status.database,
		"tls":      status.tls,
		"running":  status.running,
	}
	if status.version != "" {
		result["version"] = status.version
	}
	if status.connErr != nil {
		result["error"] = status.connErr.Error()
	}
	return result
}

func (status externalDoltStatus) print() {
	if status.running {
		fmt.Println("Dolt server: running (external)")
	} else {
		fmt.Println("Dolt server: not reachable (external)")
	}
	fmt.Printf("  Host:     %s\n", status.host)
	fmt.Printf("  Port:     %d\n", status.port)
	fmt.Printf("  Database: %s\n", status.database)
	fmt.Printf("  User:     %s\n", status.user)
	fmt.Printf("  TLS:      %t\n", status.tls)
	if status.version != "" {
		fmt.Printf("  Version:  %s\n", status.version)
	}
	if status.connErr != nil {
		fmt.Printf("  Error:    %v\n", status.connErr)
	}
}

// showEmbeddedDoltStatus reports Dolt engine status when running in
// embedded mode. There is no separate server process; the engine runs
// in-process and data lives at .beads/embeddeddolt/.
func showEmbeddedDoltStatus(beadsDir string) {
	dataDir := filepath.Join(beadsDir, "embeddeddolt")
	dataDirExists := false
	if info, err := os.Stat(dataDir); err == nil && info.IsDir() {
		dataDirExists = true
	}

	if isJSONOutput() {
		if err := outputJSON(map[string]interface{}{
			"mode": "embedded",
			// Embedded mode has an active in-process engine, but no
			// separate server process. Use a server-specific field so
			// clients do not read running=false as "Dolt is unavailable".
			"server_running":  false,
			"data_dir":        dataDir,
			"data_dir_exists": dataDirExists,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return
	}

	fmt.Println("Dolt engine: embedded (in-process, no server)")
	fmt.Printf("  Data: %s\n", dataDir)
	if !dataDirExists {
		fmt.Printf("  %s\n", ui.RenderWarn("Data directory does not exist — run 'bd init' to create it"))
	}
	if isDoltLocalOnly() {
		fmt.Println("  Remote sync: disabled (dolt.local-only=true)")
	}
}

var doltKillallCmd = &cobra.Command{
	Use:   "killall",
	Short: "Kill all orphan Dolt server processes",
	Long: `Find and kill orphan dolt sql-server processes not tracked by the
canonical PID file for the current repo's Dolt data directory.

Under an orchestrator, the canonical server lives at $GT_ROOT/.beads/. Any other
dolt sql-server processes using that shared data directory are considered
orphans and will be killed.

In standalone mode, only dolt sql-server processes using the current
project's Dolt data directory are eligible for cleanup. Other projects'
servers are preserved.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		beadsDir := selectedDoltBeadsDir()
		if beadsDir != "" {
			if _, err := loadDoltBackendConfig(beadsDir); err != nil {
				return HandleError("%v", err)
			}
		}
		if !usesSQLServer() {
			return HandleError("'bd dolt killall' is not supported in embedded mode (no Dolt server)")
		}
		if beadsDir == "" {
			beadsDir = "." // best effort
		}

		killed, err := doltserver.KillStaleServers(beadsDir)
		if err != nil {
			return HandleError("%v", err)
		}

		if len(killed) == 0 {
			fmt.Println("No orphan dolt servers found.")
		} else {
			fmt.Printf("Killed %d orphan dolt server(s): %v\n", len(killed), killed)
		}
		return nil
	},
}
