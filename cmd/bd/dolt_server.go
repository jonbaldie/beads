package main

import (
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/proxy"
	"github.com/spf13/cobra"
)

var doltStartCmd = &cobra.Command{
	Use:           "start",
	SilenceUsage:  true,
	SilenceErrors: true,
	Short:         "Start the Dolt SQL server for this project",
	Long: `Start a dolt sql-server for the current beads project.

The server runs in the background on a per-project port derived from the
project path. PID and logs are stored in .beads/.

The server auto-starts transparently when needed, so manual start is rarely
required. Use this command for explicit control or diagnostics.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		beadsDir := selectedDoltBeadsDir()
		if beadsDir == "" {
			return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
		}
		fileCfg, err := loadDoltBackendConfig(beadsDir)
		if err != nil {
			return HandleError("%v", err)
		}
		if !usesSQLServer() {
			return HandleError("'bd dolt start' is not supported in embedded mode (no Dolt server)")
		}
		// A remote (non-localhost) server host means bd does not own the
		// server lifecycle (GH#3545/GH#3518): starting a repo-local
		// server here would write local PID/port state that shadows the
		// configured remote endpoint.
		if host := fileCfg.GetDoltServerHost(); !usesProxiedServer() && !configfile.IsLocalHostString(host) {
			return HandleError("the configured Dolt server host is remote (%s); 'bd dolt start' only manages a local server.\nStart the server on that host, or clear dolt_server_host / dolt.host / BEADS_DOLT_SERVER_HOST to run one locally", host)
		}
		serverDir := doltserver.ResolveServerDir(beadsDir)

		state, err := doltserver.Start(serverDir)
		if err != nil {
			if strings.Contains(err.Error(), "already running") {
				fmt.Println(err)
				return nil
			}
			return HandleError("%v", err)
		}

		fmt.Printf("Dolt server started (PID %d, port %d)\n", state.PID, state.Port)
		fmt.Printf("  Data: %s\n", state.DataDir)
		fmt.Printf("  Logs: %s\n", doltserver.LogPath(serverDir))
		if doltserver.IsSharedServerMode() {
			fmt.Println("  Mode: shared server")
		}
		if doltserver.IsDebugMode() {
			fmt.Println("  Debug: on (loglevel=debug, --prof cpu)")
			fmt.Printf("  Profile dir: %s\n", doltserver.DebugProfileDir(beadsDir))
			fmt.Println("  Note: cpu.pprof is written when the server exits cleanly (bd dolt stop).")
		}
		return nil
	},
}

var doltStopCmd = &cobra.Command{
	Use:           "stop",
	SilenceUsage:  true,
	SilenceErrors: true,
	Short:         "Stop the Dolt SQL server for this project",
	Long: `Stop the dolt sql-server managed by beads for the current project.

This sends a graceful shutdown signal. The server will restart automatically
on the next bd command unless auto-start is disabled.

For a managed proxied server, --force can recover unverifiable or legacy
process records (both the proxy and its backend) only after each live process
executable is matched to bd or dolt and its command line ties it to this
workspace. In that recovery path, force still refuses to signal a process
whose executable identity cannot be matched to bd or dolt, or whose workspace
scope cannot be established.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		beadsDir := selectedDoltBeadsDir()
		if beadsDir == "" {
			return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
		}
		fileCfg, err := loadDoltBackendConfig(beadsDir)
		if err != nil {
			return HandleError("%v", err)
		}
		if !usesSQLServer() {
			return HandleError("'bd dolt stop' is not supported in embedded mode (no Dolt server)")
		}
		// Same remote-host ownership guard as 'bd dolt start': with a
		// remote server host, the repo-local PID state (if any) is a
		// leftover, and stopping it would report success while the
		// configured external server keeps running (GH#3545/GH#3518).
		if host := fileCfg.GetDoltServerHost(); !usesProxiedServer() && !configfile.IsLocalHostString(host) {
			return HandleError("the configured Dolt server host is remote (%s); 'bd dolt stop' only manages a local server.\nStop the server on that host, or clear dolt_server_host / dolt.host / BEADS_DOLT_SERVER_HOST to manage one locally", host)
		}
		force, _ := cmd.Flags().GetBool("force")

		if usesProxiedServer() {
			rootDir, err := resolveProxiedServerRootPath(beadsDir)
			if err != nil {
				return HandleError("%v", err)
			}
			shutdownErr := proxy.Shutdown(rootDir)
			if shutdownErr == nil {
				return renderDoltStopResult(doltStopResult{
					Stopped:  true,
					Force:    force,
					Verified: boolPointer(true),
				})
			}
			if !force || !proxy.CanForceStopUnverified(shutdownErr) {
				return HandleErrorRespectJSON("%v", shutdownErr)
			}

			report, forceErr := proxy.ForceStopUnverified(rootDir)
			return renderDoltStopResult(newForcedDoltStopResult(shutdownErr, report, forceErr))
		}

		serverDir := doltserver.ResolveServerDir(beadsDir)

		if err := doltserver.StopWithForce(serverDir, force); err != nil {
			return HandleError("%v", err)
		}
		return renderDoltStopResult(doltStopResult{
			Stopped: true,
			Force:   force,
		})
	},
}
