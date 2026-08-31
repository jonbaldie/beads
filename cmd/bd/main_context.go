package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/ui"
)

// commandOptsOutOfStore reports whether cmd or any of its ancestors carries the
// skipStoreAnnotation set to "1". The whole ancestor chain is walked, so
// annotating a command exempts that command and every subcommand beneath it.
// (This is broader than the noDbCommands list, which only matches a command
// name or its direct parent — annotate deliberately, on the specific command
// you want to skip the store.)
func commandOptsOutOfStore(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Annotations[skipStoreAnnotation] == "1" {
			return true
		}
	}
	return false
}

// readOnlyCommands lists commands that only read from the database.
// These commands open the store in read-only mode. See GH#804.
var readOnlyCommands = map[string]bool{
	"list":       true,
	"ready":      true,
	"show":       true,
	"stats":      true,
	"blocked":    true,
	"count":      true,
	"search":     true,
	"query":      true,
	"graph":      true,
	"duplicates": true,
	"comments":   true, // list comments (not add)
	"current":    true, // bd sync mode current
	"ping":       true,
	"backup":     true, // reads from Dolt, writes only to .beads/backup/
	"export":     true, // reads from Dolt, writes JSONL to file/stdout
	"tail":       true, // bd events tail: reads bd_events_journal, writes nothing
	"context":    true,
}

// isReadOnlyCommand returns true if the command only reads from the database.
// This is used to open the store in read-only mode, preventing file modifications
// that would trigger file watchers. See GH#804.
func isReadOnlyCommand(cmdName string) bool {
	return readOnlyCommands[cmdName]
}

// isPreviewCommand reports whether cmd was explicitly invoked in a
// non-mutating preview mode. Preview flags are command-local rather than
// persistent, so checking them here after Cobra has parsed the selected
// command is the earliest reliable point to keep the store open read-only.
func isPreviewCommand(cmd *cobra.Command) bool {
	for _, name := range []string{"dry-run", "inspect"} {
		if flag := cmd.Flags().Lookup(name); flag != nil {
			enabled, err := cmd.Flags().GetBool(name)
			if err == nil && enabled {
				return true
			}
		}
	}
	return false
}

type rootStorePolicy struct {
	readOnly         bool
	disableAutoStart bool
	runMaintenance   bool
}

// effectiveRootStorePolicy separates strict --readonly/config policy from
// command classification. Classified reads retain their compatibility
// maintenance and auto-start behavior; strict readonly is mutation-free.
func effectiveRootStorePolicy(cmdName string, strictReadonly bool) rootStorePolicy {
	return rootStorePolicy{
		readOnly:         strictReadonly || isReadOnlyCommand(cmdName),
		disableAutoStart: strictReadonly,
		runMaintenance:   !strictReadonly,
	}
}

// backendSupportsStrictReadonly reports whether the live backend path can open
// without provisioning or lifecycle changes. Unsupported SQL backends are
// rejected earlier by validateConfiguredBackend; proxied Dolt remains writable-only.
func backendSupportsStrictReadonly(cfg *configfile.Config) bool {
	return cfg == nil || !cfg.IsDoltProxiedServerMode()
}

// runsPostCommandMaintenance reports whether PersistentPostRunE should run the
// post-command maintenance net — Dolt auto-commit, the tip-metadata commit,
// auto-backup, auto-export and auto-push.
//
// `bd serve` is excluded, and not as an optimization. Those steps are per-
// COMMAND bookkeeping, and a server is not a command: it is a process that ran
// for hours and committed each mutation inside its own transaction as it
// happened. Running them when the operator finally sends SIGTERM would push and
// export on the way out of a signal handler — the worst possible moment — and
// attribute a whole process lifetime of requests to the shutdown. Proxied-mode
// serve never reached this branch at all (PersistentPostRunE only closes the
// provider there); server and shared-server mode do, so the exclusion has to be
// stated rather than inherited.
func runsPostCommandMaintenance(cmdName string, strictReadonly bool) bool {
	if cmdName == serveCmdName {
		return false
	}
	return effectiveRootStorePolicy(cmdName, strictReadonly).runMaintenance
}

// resolveDoltServerConnection fills in how to reach the workspace's Dolt SQL
// server — host, port, socket, user, password, TLS — on doltCfg.
//
// Both consumers of a SQL server in this process go through here: the CLI's own
// store open, and the unit-of-work provider `bd serve` builds for a server-mode
// workspace. That matters more than the deduplication: an HTTP request and a
// CLI command in the same workspace must reach the same server as the same
// identity, and the only way to guarantee that is to resolve it once.
//
// It mirrors dolt.applyResolvedConfig, which this hand-built doltCfg path
// bypasses.
func resolveDoltServerConnection(ctx context.Context, beadsDir string, fileCfg *configfile.Config, doltCfg *dolt.Config) error {
	doltCfg.ServerHost = fileCfg.GetDoltServerHost()
	// Use doltserver.DefaultConfig for port resolution (env > port file >
	// config.yaml). Port 0 is fine here — auto-start will resolve it.
	doltCfg.ServerPort = doltserver.DefaultConfig(beadsDir).Port
	doltCfg.ServerSocket = fileCfg.GetDoltServerSocket()
	// A configured credential command targets an authenticating gateway server:
	// run it for a short-lived token used as the connection username. Fail closed
	// — never fall back to the static/root user when a command was configured but
	// failed. Server mode only: embedded stores never present a username, so the
	// command must not run (or fail) embedded opens even when the env var is set.
	// Dolt-only: the gateway credential command mints a Dolt server
	// username. IsSharedServerMode() forces ServerMode true with no backend
	// guard, so non-Dolt metadata must not try to resolve a server username.
	if doltCfg.ServerMode && fileCfg.GetBackend() == configfile.BackendDolt {
		if _, err := dolt.ApplyGatewayCredential(ctx, fileCfg, doltCfg); err != nil {
			return fmt.Errorf("resolving dolt credential command: %w", err)
		}
	}
	if doltCfg.ServerUser == "" {
		doltCfg.ServerUser = fileCfg.GetDoltServerUser()
	}
	// Use the resolved port for credential lookup — metadata.json port
	// and runtime port can diverge (e.g., tunnel on 3308 vs local on 3307).
	doltCfg.ServerPassword = fileCfg.GetDoltServerPasswordForPort(doltCfg.ServerPort)
	doltCfg.ServerTLS = fileCfg.GetDoltServerTLS()
	return nil
}

var (
	runPostRunAutoCommit = maybeAutoCommit
	runPostRunAutoBackup = maybeAutoBackup
	runPostRunAutoExport = maybeAutoExport
	runPostRunAutoPush   = maybeAutoPush
)

// isWorkingSetReconcileCommand reports whether cmd's whole purpose is to
// reconcile the Dolt working set: "bd dolt commit" or "bd vc commit". These
// commands are the documented recovery from a pending-migration dirty-table
// refusal, but they also open the store, and an open runs the migration -
// hitting that same refusal before the commit that would clear the dirty
// state ever runs. Opening leniently (embeddeddolt.OpenForWorkingSetReconcile)
// breaks that deadlock by skipping the migration instead of failing the open
// (gastownhall/beads#4566).
func isWorkingSetReconcileCommand(cmd *cobra.Command) bool {
	if cmd.Name() != "commit" {
		return false
	}
	parent := cmd.Parent()
	if parent == nil {
		return false
	}
	return parent.Name() == "dolt" || parent.Name() == "vc"
}

// isForcedMigrate reports whether cmd is `bd migrate` or `bd migrate schema`
// invoked with --force: the operator confirming they are the single designated
// migrator, so the remote-migrate gate (#4259) must not block this run's store
// opens. Consulted in the root PersistentPreRunE because the gate fires during
// store open (and during autoMigrateOnVersionBump), long before the migrate
// command's own RunE.
func isForcedMigrate(cmd *cobra.Command) bool {
	if cmd != migrateCmd && cmd != migrateSchemaCmd {
		return false
	}
	force, _ := cmd.Flags().GetBool("force")
	return force
}

// forcedMigratePreviewFlag returns the name of a preview flag (--dry-run,
// --inspect) that conflicts with --force on a forced migrate invocation, or ""
// when there is no conflict. The combination must be rejected BEFORE the store
// opens: with the gate override set, the open itself applies pending schema
// migrations, so the preview flag would be honored only after the destructive
// work it exists to prevent had already happened.
func forcedMigratePreviewFlag(cmd *cobra.Command) string {
	for _, name := range []string{"dry-run", "inspect"} {
		if v, err := cmd.Flags().GetBool(name); err == nil && v {
			return name
		}
	}
	return ""
}

// applyNoColorFlag disables colorized output when --no-color is set.
// Complements the NO_COLOR / CLICOLOR=0 env detection in package ui,
// giving callers a per-invocation override.
func applyNoColorFlag() {
	if isNoColorFlag() {
		ui.DisableColors()
	}
}
