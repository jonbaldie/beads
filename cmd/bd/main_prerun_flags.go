package main

import (
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/metrics"
	dbidentifier "github.com/jonbaldie/beads/internal/storage/domain/db"
	"github.com/spf13/cobra"
)

func bootstrapPersistentPreRun(cmd *cobra.Command) bool {
	applyNoColorFlag()

	// Initialize CommandContext to hold runtime state (replaces scattered globals)
	initCommandContext()

	// Reset per-command write tracking (used by Dolt auto-commit).
	commandDidWrite.Store(false)
	commandMayEmptyJSONLExport.Store(false)
	setCommandDidExplicitDoltCommit(false)
	setCommandDidWriteTipMetadata(false)
	setCommandTipIDsShown(make(map[string]struct{}))

	// Set up signal-aware context with batch commit flush on shutdown.
	// Unlike signal.NotifyContext, this also handles SIGHUP and flushes
	// pending batch commits before canceling the context.
	//
	// Publish through setRootContext, not a bare assignment to the
	// globals: cmdCtx exists by now (initCommandContext above), so
	// getRootContext() reads cmdCtx.RootCtx, and the commands that
	// return early from this hook -- every skipsStoreInit command,
	// migrate among them -- never reach syncCommandContext to have it
	// backfilled. A bare assignment leaves those commands reading a nil
	// per-command context and losing Ctrl-C entirely.
	shutdownCtx, shutdownCancel := setupGracefulShutdown(os.Exit)
	setRootContext(shutdownCtx, shutdownCancel)

	// Initialize OTel. Telemetry is opt-in — initTelemetry is a noop
	// unless BD_OTEL_ENABLED=true or a legacy BD_OTEL_* selector is set.
	// Must run before any DB access so SQL spans nest under the command
	// span.
	initTelemetry(getRootContext(), Version)

	// Materialize the user-level metrics config only when metrics are
	// actually enabled. When metrics are disabled (BD_DISABLE_METRICS or a
	// user-global metrics.disabled), there is nothing to bootstrap. The
	// send-metrics flusher is exempt so it never recurses into bootstrap.
	// This mirrors the resolveMetricsEnabled() gate on the first-run notice
	// below. (~/.config/bd/ lives outside the repo, so this write is not a
	// stealth/per-repository trace; stealth init is handled by suppressing
	// the first-run notice, not by skipping this user-global bootstrap.)
	if cmd.Name() != metrics.SendMetricsSubcommand && resolveMetricsEnabled() {
		if err := metrics.EnsureUserConfigDefaults(); err != nil {
			debug.Logf("warning: ensure user config defaults failed: %v", err)
		}
	}

	if _, err := metrics.Init(Version, resolveMetricsEnabled(), resolveMetricsEndpoint()); err != nil {
		debug.Logf("warning: metrics init failed: %v", err)
	}

	if cmd.Name() == metrics.SendMetricsSubcommand {
		return true
	}
	return false
}

func applyPersistentPreRunFlags(cmd *cobra.Command) (string, error) {
	// Start root span for this command. rootCtx now carries the span, so
	// all downstream DB and AI calls become child spans automatically.
	spanCtx, span := startCommandSpan(getRootContext(), cmd.Name(), Version, os.Args[1:], secretFlagTokens(cmd))
	setCommandSpan(span)
	setRootContext(spanCtx, getRootCancel())

	// Apply verbosity flags early (before any output)
	debug.SetVerbose(isVerbose())
	debug.SetQuiet(isQuiet())

	if err := applyChangeDirSelection(cmd); err != nil {
		return "", err
	}

	// Block dangerous env var overrides that could cause data fragmentation (bd-hevyw).
	if err := checkBlockedEnvVars(); err != nil {
		return "", HandleError("%v", err)
	}

	loadSelectionEnvironment()

	flagOverrides := make(map[string]persistentFlagOverride)
	applyPersistentJSONFlags(cmd, flagOverrides)
	applyPersistentReadonlyFlag(cmd, flagOverrides)
	dbNameFromDBFlag, err := applyPersistentDBFlags(cmd, flagOverrides)
	if err != nil {
		return "", err
	}
	applyPersistentActorAndCommitFlags(cmd, flagOverrides)
	if isIgnoreSchemaSkew() {
		_ = os.Setenv("BD_IGNORE_SCHEMA_SKEW", "1")
	}
	if isVerbose() {
		for _, override := range config.CheckOverrides(flagOverrides) {
			config.LogOverride(override)
		}
	}
	return dbNameFromDBFlag, nil
}

type persistentFlagOverride = struct {
	Value  interface{}
	WasSet bool
}

func applyPersistentJSONFlags(cmd *cobra.Command, flagOverrides map[string]persistentFlagOverride) {
	if cmd.Root().PersistentFlags().Changed("format") {
		format, _ := cmd.Root().PersistentFlags().GetString("format")
		if strings.EqualFold(format, "json") {
			setJSONOutput(true)
		}
	}
	if !cmd.Root().PersistentFlags().Changed("json") && !cmd.Root().PersistentFlags().Changed("format") {
		setJSONOutput(config.GetBool("json"))
	} else {
		flagOverrides["json"] = persistentFlagOverride{isJSONOutput(), true}
	}
}

func applyPersistentReadonlyFlag(cmd *cobra.Command, flagOverrides map[string]persistentFlagOverride) {
	if !cmd.Root().PersistentFlags().Changed("readonly") {
		setReadonlyMode(config.GetBool("readonly"))
	} else {
		flagOverrides["readonly"] = persistentFlagOverride{isReadonlyMode(), true}
	}
}

func applyPersistentDBFlags(cmd *cobra.Command, flagOverrides map[string]persistentFlagOverride) (string, error) {
	dbNameFromDBFlag, err := interpretDBFlagAsName(cmd)
	if err != nil {
		return "", err
	}
	applyPersistentDBPathDefault(cmd, flagOverrides)
	return dbNameFromDBFlag, nil
}

func interpretDBFlagAsName(cmd *cobra.Command) (string, error) {
	if cmd.Name() == "init" || !cmd.Root().PersistentFlags().Changed("db") || getDBPath() == "" {
		return "", nil
	}
	if dbidentifier.ValidateIdentifier(getDBPath()) != nil {
		return "", nil
	}
	_, statErr := os.Stat(getDBPath())
	if statErr == nil {
		return "", nil
	}
	if !os.IsNotExist(statErr) {
		return "", HandleError("--db %q: %v", getDBPath(), statErr)
	}
	name := getDBPath()
	setDBPath("")
	return name, nil
}

func applyPersistentDBPathDefault(cmd *cobra.Command, flagOverrides map[string]persistentFlagOverride) {
	if !cmd.Root().PersistentFlags().Changed("db") && getDBPath() == "" &&
		os.Getenv("BEADS_DB") == "" && os.Getenv("BD_DB") == "" && os.Getenv("BEADS_DIR") == "" {
		setDBPath(config.GetString("db"))
		return
	}
	if cmd.Root().PersistentFlags().Changed("db") {
		flagOverrides["db"] = persistentFlagOverride{getDBPath(), true}
	}
}

func applyPersistentActorAndCommitFlags(cmd *cobra.Command, flagOverrides map[string]persistentFlagOverride) {
	if !cmd.Root().PersistentFlags().Changed("actor") && getActor() == "" {
		setActor(resolveConfiguredActor())
	} else if cmd.Root().PersistentFlags().Changed("actor") {
		flagOverrides["actor"] = persistentFlagOverride{getActor(), true}
	}
	if !cmd.Root().PersistentFlags().Changed("dolt-auto-commit") && strings.TrimSpace(getDoltAutoCommit()) == "" {
		setDoltAutoCommit(config.GetString("dolt.auto-commit"))
	} else if cmd.Root().PersistentFlags().Changed("dolt-auto-commit") {
		flagOverrides["dolt-auto-commit"] = persistentFlagOverride{getDoltAutoCommit(), true}
	}
}
