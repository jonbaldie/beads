package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/git"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/spf13/cobra"
)

// DriftItem represents a single drift check result.
type DriftItem struct {
	Check    string `json:"check"`
	Status   string `json:"status"` // "ok", "drift", "info", "skipped"
	Message  string `json:"message"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
}

const (
	driftStatusOK      = "ok"
	driftStatusDrift   = "drift"
	driftStatusInfo    = "info"
	driftStatusSkipped = "skipped"
)

func newConfigDriftCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "drift",
		Short: "Detect config-vs-reality inconsistencies",
		Long: `Detect drift between declared configuration and actual system state.

This is a read-only diagnostic that answers "is my environment consistent
with my config?" — no mutations are performed.

Checks:
  - hooks     Git hooks installed and up-to-date
  - remote    Dolt remote matches federation.remote config
  - server    Server state matches dolt.shared-server config

Exit codes:
  0  No drift detected (all checks ok/info/skipped)
  1  Drift detected (at least one check has status "drift")

Examples:
  bd config drift
  bd config drift --json`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runConfigDrift,
	}
}

func init() {
	configCmd.AddCommand(newConfigDriftCmd())
}

func runConfigDrift(_ *cobra.Command, _ []string) error {
	evt := metrics.NewCommandEvent("config-drift")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	items := runDriftChecks()
	if isJSONOutput() {
		if err := outputJSON(items); err != nil {
			return err
		}
	} else {
		printDriftItems(items)
	}
	for _, item := range items {
		if item.Status == driftStatusDrift {
			return SilentExit()
		}
	}
	return nil
}

// runDriftChecks runs all drift checks and returns the results.
func runDriftChecks() []DriftItem {
	var items []DriftItem
	items = append(items, checkHooksDrift()...)
	items = append(items, checkRemoteDrift()...)
	items = append(items, checkServerDrift()...)
	return items
}

// checkHooksDrift checks whether git hooks are installed and current.
func checkHooksDrift() []DriftItem {
	// Verify we're in a git repo first
	_, err := git.GetGitHooksDir()
	if err != nil {
		return []DriftItem{{
			Check:   "hooks",
			Status:  driftStatusSkipped,
			Message: "Not a git repository",
		}}
	}

	statuses := CheckGitHooks()

	var items []DriftItem
	var missing, outdated []string

	for _, s := range statuses {
		if !s.Installed {
			missing = append(missing, s.Name)
		} else if s.Outdated {
			outdated = append(outdated, fmt.Sprintf("%s (have %s, want %s)", s.Name, s.Version, Version))
		}
	}

	if len(missing) > 0 {
		items = append(items, DriftItem{
			Check:    "hooks.missing",
			Status:   driftStatusDrift,
			Message:  fmt.Sprintf("Git hooks not installed: %s", strings.Join(missing, ", ")),
			Expected: "installed",
			Actual:   "missing",
		})
	}

	if len(outdated) > 0 {
		items = append(items, DriftItem{
			Check:    "hooks.outdated",
			Status:   driftStatusDrift,
			Message:  fmt.Sprintf("Git hooks outdated: %s", strings.Join(outdated, ", ")),
			Expected: "v" + Version,
			Actual:   "outdated",
		})
	}

	if len(missing) == 0 && len(outdated) == 0 {
		items = append(items, DriftItem{
			Check:   "hooks",
			Status:  driftStatusOK,
			Message: "Git hooks installed and current",
		})
	}

	return items
}

// checkRemoteDrift compares federation.remote config against actual Dolt remotes.
func checkRemoteDrift() []DriftItem {
	federationRemote, originURL, remoteNames, failure := loadRemoteDriftState()
	if failure != nil {
		return []DriftItem{*failure}
	}
	return classifyRemoteDrift(federationRemote, originURL, remoteNames)
}

func loadRemoteDriftState() (string, string, []string, *DriftItem) {
	federationRemote := config.GetString("federation.remote")
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return "", "", nil, &DriftItem{
			Check:   "remote",
			Status:  driftStatusSkipped,
			Message: "No active beads workspace found",
		}
	}
	ctx := context.Background()
	st, err := dolt.NewFromConfigWithOptions(ctx, beadsDir, &dolt.Config{
		ReadOnly: true,
		ServerOptions: dolt.ServerOptions{
			DisableAutoStart: true,
		},
	})
	if err != nil {
		return "", "", nil, &DriftItem{
			Check:   "remote",
			Status:  driftStatusSkipped,
			Message: fmt.Sprintf("Cannot open Dolt store: %v", err),
		}
	}
	defer func() { _ = st.Close() }()
	remotes, err := st.ListRemotes(ctx)
	if err != nil {
		return "", "", nil, &DriftItem{
			Check:   "remote",
			Status:  driftStatusSkipped,
			Message: fmt.Sprintf("Cannot list remotes: %v", err),
		}
	}
	var originURL string
	names := make([]string, len(remotes))
	for i, r := range remotes {
		names[i] = r.Name
		if r.Name == "origin" {
			originURL = r.URL
		}
	}
	return federationRemote, originURL, names, nil
}

func classifyRemoteDrift(federationRemote, originURL string, remoteNames []string) []DriftItem {
	if federationRemote != "" {
		return classifyConfiguredRemote(federationRemote, originURL)
	}
	if len(remoteNames) > 0 {
		return []DriftItem{{
			Check:   "remote",
			Status:  driftStatusInfo,
			Message: fmt.Sprintf("Dolt remotes configured (%s) but federation.remote is not set", strings.Join(remoteNames, ", ")),
		}}
	}
	return []DriftItem{{
		Check:   "remote",
		Status:  driftStatusInfo,
		Message: "No federation.remote configured and no Dolt remotes found",
	}}
}

func classifyConfiguredRemote(federationRemote, originURL string) []DriftItem {
	if originURL == "" {
		return []DriftItem{{
			Check:    "remote",
			Status:   driftStatusDrift,
			Message:  "federation.remote is configured but no Dolt 'origin' remote exists",
			Expected: federationRemote,
			Actual:   "(no origin remote)",
		}}
	}

	if !remoteURLMatchesConfig(originURL, federationRemote) {
		return []DriftItem{{
			Check:    "remote",
			Status:   driftStatusDrift,
			Message:  "Dolt origin remote URL does not match federation.remote",
			Expected: federationRemote,
			Actual:   originURL,
		}}
	}

	return []DriftItem{{
		Check:   "remote",
		Status:  driftStatusOK,
		Message: "Dolt origin remote matches federation.remote",
	}}
}

// checkServerDrift compares dolt.shared-server config against running server state.
// Uses non-mutating PID file check instead of doltserver.IsRunning() which can
// delete stale state files.
func checkServerDrift() []DriftItem {
	serverDir, wantServer, failure := serverDriftState()
	if failure != nil {
		return []DriftItem{*failure}
	}
	return classifyServerDrift(wantServer, isServerProbablyRunning(serverDir))
}

func serverDriftState() (string, bool, *DriftItem) {
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return "", false, &DriftItem{Check: "server", Status: driftStatusSkipped, Message: "No active beads workspace found"}
	}
	wantServer := doltserver.IsSharedServerMode()
	if !wantServer {
		return beadsDir, false, nil
	}
	serverDir, err := doltserver.SharedServerPath()
	if err != nil {
		return "", true, &DriftItem{
			Check: "server", Status: driftStatusDrift,
			Message:  fmt.Sprintf("dolt.shared-server is enabled but its state directory cannot be resolved: %v", err),
			Expected: "resolvable shared server state directory", Actual: "unavailable",
		}
	}
	return serverDir, true, nil
}

func classifyServerDrift(wantServer, serverRunning bool) []DriftItem {
	if wantServer && !serverRunning {
		return []DriftItem{{
			Check:    "server",
			Status:   driftStatusDrift,
			Message:  "dolt.shared-server is enabled but no server appears to be running",
			Expected: "running",
			Actual:   "not running",
		}}
	}

	if !wantServer && serverRunning {
		return []DriftItem{{
			Check:   "server",
			Status:  driftStatusInfo,
			Message: "Dolt server is running but dolt.shared-server is not enabled in config",
		}}
	}

	if wantServer && serverRunning {
		return []DriftItem{{
			Check:   "server",
			Status:  driftStatusOK,
			Message: "Server running, consistent with dolt.shared-server config",
		}}
	}

	return []DriftItem{{
		Check:   "server",
		Status:  driftStatusOK,
		Message: "No shared server configured or running",
	}}
}

// isServerProbablyRunning performs a non-mutating check for a running Dolt server.
// Reads the PID file and checks if the process exists, without deleting stale files.
func isServerProbablyRunning(beadsDir string) bool {
	pidFile := filepath.Join(beadsDir, doltserver.PIDFileName)
	data, err := os.ReadFile(pidFile) // #nosec G304 -- controlled path
	if err != nil {
		return false
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false
	}

	return pidAlive(pid)
}

// printDriftItems renders drift results in human-readable format.
func printDriftItems(items []DriftItem) {
	if len(items) == 0 {
		fmt.Println("No drift checks available")
		return
	}

	statusIcon := map[string]string{
		driftStatusOK:      "✓",
		driftStatusDrift:   "✗",
		driftStatusInfo:    "ℹ",
		driftStatusSkipped: "–",
	}

	for _, item := range items {
		icon := statusIcon[item.Status]
		if icon == "" {
			icon = "?"
		}
		fmt.Fprintf(os.Stdout, "  %s %s: %s\n", icon, item.Check, item.Message)
		if item.Expected != "" || item.Actual != "" {
			fmt.Fprintf(os.Stdout, "      expected: %s\n", item.Expected)
			fmt.Fprintf(os.Stdout, "      actual:   %s\n", item.Actual)
		}
	}
}
