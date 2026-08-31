package main

import (
	"fmt"
	"os"
	"slices"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/spf13/cobra"
)

func classifyPersistentStoreInit(cmd *cobra.Command) bool {
	cmdName := cmd.Name()
	isSubcommand := cmd.Parent() != nil && cmd.Parent().Name() != "bd"
	skipsStoreInit := classifyPersistentParentSkip(cmd, cmdName)
	if slices.Contains(persistentNoDBCommands, cmdName) && !isSubcommand {
		skipsStoreInit = true
	}
	if cmd.Parent() == nil && cmdName == cmd.Use {
		skipsStoreInit = true
	}
	if v, _ := cmd.Flags().GetBool("version"); v {
		skipsStoreInit = true
	}
	if commandOptsOutOfStore(cmd) {
		skipsStoreInit = true
	}
	return skipsStoreInit
}

// GH#1093: Check noDbCommands BEFORE expensive operations to avoid spawning
// git subprocesses for simple commands like "bd version" that don't need
// database access. A command can also opt out via skipStoreAnnotation
// (see commandOptsOutOfStore). "doctor" uses that seam and is absent here.
var persistentNoDBCommands = []string{
	"__complete",       // Cobra's internal completion command (shell completions work without db)
	"__completeNoDesc", // Cobra's completion without descriptions (used by fish)
	"bash",
	"bootstrap",
	"completion",
	"context", // reads config files directly, does not need DB open
	"codex-hook",
	"cursor-hook", // shells out to `bd prime`; never opens the store itself
	// "doctor" opts out via skipStoreAnnotation on its Command literal.
	"dolt", // bare "bd dolt" shows help only; subcommands handled below
	"fish",
	"formula", // parser-only subcommands; add a store-needed guard before adding DB-backed formula subcommands
	"help",
	"hook", // manages its own store lifecycle (#1719)
	"hooks",
	"human",
	"init",
	"merge",
	"metrics", // config-only: status/on/off/example never touch the DB
	"onboard",
	"powershell",
	"prime",
	"quickstart",
	metrics.SendMetricsSubcommand,
	"setup",
	"version",
	"where",
	"zsh",
}

var needsStoreDoltSubcommands = []string{"push", "pull", "commit"}

var needsStoreDoltGrandchildren = []string{"remote"}

var needsStoreHumanSubcommands = []string{"list", "respond", "dismiss", "stats"}

var skipStoreMigrateSubcommands = []string{
	"from-server-to-proxied-server",
	"from-proxied-server-to-server",
	"from-shared-server-to-proxied-server",
	"from-proxied-server-to-shared-server",
}

func classifyPersistentParentSkip(cmd *cobra.Command, cmdName string) bool {
	if cmd.Parent() == nil {
		return false
	}
	parentName := cmd.Parent().Name()
	if parentName == "dolt" && slices.Contains(needsStoreDoltSubcommands, cmdName) {
		return false
	}
	if slices.Contains(needsStoreDoltGrandchildren, parentName) {
		return false
	}
	if parentName == "human" && slices.Contains(needsStoreHumanSubcommands, cmdName) {
		return false
	}
	if parentName == "migrate" && slices.Contains(skipStoreMigrateSubcommands, cmdName) {
		return true
	}
	return slices.Contains(persistentNoDBCommands, parentName)
}

func preparePersistentNoDB(cmd *cobra.Command) error {
	beadsDir := selectedNoDBBeadsDir(cmd)
	prepareSelectedNoDBContext(beadsDir)
	refreshBoundCommandConfig(cmd)
	if os.Getenv("BEADS_DIR") == "" {
		loadEnvironment()
		if err := loadServerModeFromConfig(); err != nil {
			// Warn, don't fatal: skipsStoreInit commands (doctor,
			// init, bootstrap, version, ...) never select a store,
			// and several of them are the repair path for the very
			// corruption being reported.
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		}
	}
	if beadsDir == "" {
		beadsDir = beads.FindBeadsDir()
	}
	if err := guardLegacyNoStoreCommand(cmd, beadsDir); err != nil {
		return HandleError("%v", err)
	}
	if _, err := getDoltAutoCommitMode(); err != nil {
		return HandleError("%v", err)
	}
	return nil
}
