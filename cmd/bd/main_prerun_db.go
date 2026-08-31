package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"runtime/trace"
	"time"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/remotecache"
	"github.com/jonbaldie/beads/internal/routing"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

var errPersistentPreRunComplete = errors.New("persistent prerun complete")

func resolvePersistentDatabasePath(cmd *cobra.Command, args []string) error {
	startPersistentProfiling(cmd)
	detectPersistentSandbox(cmd)
	if getDBPath() == "" {
		preserveRedirectSourceDatabase(beads.GetRedirectInfo().LocalDir)
	}
	if err := discoverPersistentBeadsDatabase(cmd); err != nil {
		return err
	}
	if getDBPath() != "" {
		return nil
	}
	return resolveMissingPersistentDatabase(cmd, args)
}

func startPersistentProfiling(cmd *cobra.Command) {
	if !isCPUProfileEnabled() {
		return
	}
	timestamp := time.Now().Format("20060102-150405")
	if f, _ := os.Create(fmt.Sprintf("bd-profile-%s-%s.prof", cmd.Name(), timestamp)); f != nil {
		setProfileFile(f)
		_ = pprof.StartCPUProfile(f)
	}
	if f, _ := os.Create(fmt.Sprintf("bd-trace-%s-%s.out", cmd.Name(), timestamp)); f != nil {
		setTraceFile(f)
		_ = trace.Start(f)
	}
}

func detectPersistentSandbox(cmd *cobra.Command) {
	if cmd.Root().PersistentFlags().Changed("sandbox") {
		return
	}
	if isSandboxed() {
		setSandboxMode(true)
		fmt.Fprintf(os.Stderr, "ℹ️  Sandbox detected, using direct mode\n")
	}
}

func discoverPersistentBeadsDatabase(cmd *cobra.Command) error {
	if getDBPath() != "" {
		return nil
	}
	bd := beads.FindBeadsDir()
	if bd == "" {
		if guardErr := guardUndiscoveredLegacyWorkspace(); guardErr != nil {
			return HandleError("%v", guardErr)
		}
		return nil
	}
	return applyDiscoveredBeadsDir(cmd, bd)
}

func applyDiscoveredBeadsDir(cmd *cobra.Command, bd string) error {
	// Bind the discovered target before admission so the legacy guard
	// honors its config.yaml (including dolt.shared-server), not the
	// caller's. This setup is read-only: metadata discovery below still
	// uses LoadForDiscovery and cannot migrate config.json.
	prepareSelectedCommandContext(bd, true)
	refreshBoundCommandConfig(cmd)
	if guardErr := guardLegacyUpgradeWorkspace(bd); guardErr != nil {
		return HandleError("%v", guardErr)
	}
	cfg, cfgErr := configfile.LoadForDiscovery(bd)
	if discoveredBeadsDirIsStoreRoot(cfg, cfgErr) {
		setDBPath(bd)
	}
	return nil
}

func discoveredBeadsDirIsStoreRoot(cfg *configfile.Config, cfgErr error) bool {
	if cfgErr != nil || cfg != nil && (cfg.IsDoltProxiedServerMode() ||
		registeredBackendWorkspaceIsBeadsDir(cfg) ||
		!configfile.IsSupportedBackend(cfg.Backend)) {
		return true
	}
	return cfg == nil && cfgErr == nil && configfile.DefaultConfig().HostImpliesServerMode()
}

func resolveMissingPersistentDatabase(cmd *cobra.Command, args []string) error {
	if foundDB := beads.FindDatabasePath(); foundDB != "" {
		setDBPath(foundDB)
		return nil
	}
	if done, err := maybeSkipMissingPersistentDatabase(cmd, args); done {
		return err
	}
	if err := applyCreateRepoDatabasePath(cmd); err != nil {
		return err
	}
	return applyDefaultMissingDatabasePath(cmd)
}

func maybeSkipMissingPersistentDatabase(cmd *cobra.Command, args []string) (bool, error) {
	if !configCommandCanRunWithoutStore(cmd, args) {
		return false, nil
	}
	if getDBPath() != "" {
		if beadsDir := resolveCommandBeadsDir(getDBPath()); beadsDir != "" {
			prepareSelectedCommandContext(beadsDir, false)
		}
	}
	return true, errPersistentPreRunComplete
}

func applyCreateRepoDatabasePath(cmd *cobra.Command) error {
	if cmd.Name() != "create" || !cmd.Flags().Changed("repo") {
		return nil
	}
	repoVal, _ := cmd.Flags().GetString("repo")
	if repoVal == "" {
		return nil
	}
	if remotecache.IsRemoteURL(repoVal) {
		return errPersistentPreRunComplete
	}
	targetBeadsDir := filepath.Join(routing.ExpandPath(repoVal), ".beads")
	setDBPath(utils.CanonicalizePath(filepath.Join(targetBeadsDir, beads.CanonicalDatabaseName)))
	return nil
}

func applyDefaultMissingDatabasePath(cmd *cobra.Command) error {
	if getDBPath() != "" {
		return nil
	}
	if cmd.Name() != "import" && cmd.Name() != "setup" {
		fmt.Fprintf(os.Stderr, "Error: no beads database found\n")
		fmt.Fprintf(os.Stderr, "Hint: %s\n", diagHint())
		fmt.Fprintf(os.Stderr, "      or set BEADS_DIR to point to your .beads directory\n")
		return SilentExit()
	}
	targetBeadsDir := beads.FindBeadsDir()
	if targetBeadsDir == "" {
		targetBeadsDir = ".beads"
	}
	setDBPath(utils.CanonicalizePath(filepath.Join(targetBeadsDir, beads.CanonicalDatabaseName)))
	return nil
}
