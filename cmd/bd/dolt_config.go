package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/doltutil"
	"github.com/jonbaldie/beads/internal/ui"
)

func selectedDoltBeadsDir() string {
	beadsDir := ""
	if os.Getenv("BEADS_DIR") != "" {
		beadsDir = beads.FindBeadsDir()
	}
	if beadsDir == "" {
		beadsDir = selectedNoDBBeadsDir(nil)
	}
	if beadsDir == "" {
		return ""
	}
	prepareSelectedNoDBContext(beadsDir)
	return beadsDir
}

// resolveDoltShowRemotes returns remotes for `bd dolt show`.
// `show` is a no-store diagnostic command, so getStore() is usually nil and
// ListRemotes is unavailable. Fall back to on-disk repo_state.json (same
// source as the remote-migrate gate) so remotes match `bd dolt remote list`
// (GH#4619).
//
// Only the candidate path(s) for the active mode (embedded vs. server) are
// probed; a repo in one mode must not surface stale remotes persisted under
// the other mode's data directory. Within the mode-appropriate candidates,
// the first repo_state.json found on disk is authoritative: an empty
// remotes list there means "no remotes", not "keep looking" — this stops
// an authoritative-but-empty active database from falling through to a
// stale candidate. A corrupt or unreadable repo_state.json is surfaced as a
// warning rather than silently rendered as "(none)".
func doltShowStoreRemotes() []storage.RemoteInfo {
	st := getStore()
	if st == nil {
		return nil
	}
	remotes, err := st.ListRemotes(context.Background())
	if err != nil || len(remotes) == 0 {
		return nil
	}
	return remotes
}

func doltShowRemoteCandidates(beadsDir string, cfg *configfile.Config, embeddedDataDir string, embedded bool) []string {
	dbName := ""
	if cfg != nil {
		dbName = cfg.GetDoltDatabase()
	}
	if embedded {
		var candidates []string
		if embeddedDataDir != "" {
			candidates = append(candidates, embeddedDataDir)
			if dbName != "" {
				candidates = append(candidates, filepath.Join(embeddedDataDir, dbName))
			}
		}
		return candidates
	}
	if beadsDir == "" {
		return nil
	}
	candidates := []string{filepath.Join(beadsDir, "dolt")}
	if dbName != "" {
		candidates = append(candidates, filepath.Join(beadsDir, "dolt", dbName))
	}
	return candidates
}

func loadDoltShowRemotesFromDisk(candidates []string) []storage.RemoteInfo {
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		statePath := filepath.Join(dir, ".dolt", "repo_state.json")
		if _, err := os.Stat(statePath); err != nil {
			// No dolt repo state at this candidate; try the next
			// mode-appropriate candidate.
			continue
		}
		remotes, err := doltutil.PersistedRemotes(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", ui.RenderWarn(fmt.Sprintf("could not read remotes from %s: %v", statePath, err)))
			return nil
		}
		// repo_state.json exists at this candidate: its remotes (even if
		// empty) are authoritative for the active mode.
		return remotes
	}
	return nil
}

func resolveDoltShowRemotes(beadsDir string, cfg *configfile.Config, embeddedDataDir string, embedded bool) []storage.RemoteInfo {
	if remotes := doltShowStoreRemotes(); len(remotes) > 0 {
		return remotes
	}
	candidates := doltShowRemoteCandidates(beadsDir, cfg, embeddedDataDir, embedded)
	return loadDoltShowRemotesFromDisk(candidates)
}

type doltShowConfigState struct {
	beadsDir        string
	cfg             *configfile.Config
	backend         string
	embedded        bool
	host            string
	port            int
	embeddedDataDir string
}

func loadDoltShowConfig(beadsDir string) (doltShowConfigState, error) {
	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		return doltShowConfigState{}, fmt.Errorf("loading config: %w", err)
	}
	if cfg == nil {
		cfg = configfile.DefaultConfig()
	}
	if err := validateConfiguredBackend(cfg); err != nil {
		return doltShowConfigState{}, err
	}
	return doltShowConfigState{
		beadsDir:        beadsDir,
		cfg:             cfg,
		backend:         cfg.GetBackend(),
		embedded:        !usesSQLServer(),
		host:            cfg.GetDoltServerHost(),
		port:            doltserver.DefaultConfig(beadsDir).Port,
		embeddedDataDir: filepath.Join(beadsDir, "embeddeddolt"),
	}, nil
}

func renderDoltShowConfigJSON(state doltShowConfigState, testConnection bool) error {
	result := map[string]interface{}{
		"backend": state.backend,
	}
	if state.backend == configfile.BackendDolt {
		result["database"] = state.cfg.GetDoltDatabase()
		result["embedded"] = state.embedded
		if state.embedded {
			result["data_dir"] = state.embeddedDataDir
		} else {
			result["host"] = state.host
			result["port"] = state.port
			result["user"] = state.cfg.GetDoltServerUser()
			result["tls"] = state.cfg.GetDoltServerTLS()
			result["shared_server"] = doltserver.IsSharedServerMode()
			if testConnection {
				result["connection_ok"] = testServerConnection(state.host, state.port)
			}
		}
	}
	if err := outputJSON(result); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	}
	return nil
}

func printDoltShowServerConfig(state doltShowConfigState, testConnection bool) {
	fmt.Printf("  Host:     %s\n", state.host)
	fmt.Printf("  Port:     %d\n", state.port)
	fmt.Printf("  User:     %s\n", state.cfg.GetDoltServerUser())
	fmt.Printf("  TLS:      %t\n", state.cfg.GetDoltServerTLS())
	if doltserver.IsSharedServerMode() {
		fmt.Println("  Mode:     shared server")
		if sharedDir, err := doltserver.SharedServerDir(); err == nil {
			fmt.Printf("  Server:   %s\n", sharedDir)
		}
	} else {
		fmt.Println("  Mode:     per-project")
	}

	if !testConnection {
		return
	}
	fmt.Println()
	if testServerConnection(state.host, state.port) {
		fmt.Printf("  %s\n", ui.RenderPass("✓ Server connection OK"))
	} else {
		fmt.Printf("  %s\n", ui.RenderWarn("✗ Server not reachable"))
	}
}

func printDoltShowRemotes(state doltShowConfigState) {
	fmt.Println("\nRemotes:")
	remotes := resolveDoltShowRemotes(state.beadsDir, state.cfg, state.embeddedDataDir, state.embedded)
	if len(remotes) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, remote := range remotes {
			fmt.Printf("  %-16s %s\n", remote.Name, remote.URL)
		}
	}
}

func printDoltShowConfig(state doltShowConfigState, testConnection bool) {
	if state.backend != configfile.BackendDolt {
		fmt.Printf("Backend: %s\n", state.backend)
		return
	}

	fmt.Println("Dolt Configuration")
	fmt.Println("==================")
	fmt.Printf("  Database: %s\n", state.cfg.GetDoltDatabase())
	if state.embedded {
		fmt.Println("  Mode:     embedded (in-process Dolt engine)")
		fmt.Printf("  Data:     %s\n", state.embeddedDataDir)
	} else {
		printDoltShowServerConfig(state, testConnection)
	}
	printDoltShowRemotes(state)
	printDoltShowConfigSources(os.Stdout)
}

func showDoltConfig(testConnection bool) error {
	beadsDir := selectedDoltBeadsDir()
	if beadsDir == "" {
		return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
	}

	state, err := loadDoltShowConfig(beadsDir)
	if err != nil {
		return HandleError("%v", err)
	}
	if isJSONOutput() {
		return renderDoltShowConfigJSON(state, testConnection)
	}
	printDoltShowConfig(state, testConnection)
	return nil
}

// printDoltShowConfigSources renders doltserver.PortSourceLabels(), the same
// slice DefaultConfig resolves against, so this list can't drift from actual
// resolution behavior (GH#4511).
func printDoltShowConfigSources(w io.Writer) {
	fmt.Fprintln(w, "\nConfig sources for server port (priority order):")
	for i, label := range doltserver.PortSourceLabels() {
		fmt.Fprintf(w, "  %d. %s\n", i+1, label)
	}
}
