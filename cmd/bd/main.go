package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/hooks"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/uow"
	oteltrace "go.opentelemetry.io/otel/trace"
)

var (
	changeDir    string
	dbPath       string
	databaseFlag string
	uowProvider  uow.UnitOfWorkProvider
	jsonOutput   bool

	// Signal-aware context for graceful cancellation
	rootCtx    context.Context
	rootCancel context.CancelFunc

	// Hook runner for extensibility
	hookRunner *hooks.Runner

	// Store concurrency protection
	storeMutex  sync.Mutex // Protects store access from background goroutine
	storeActive = false    // Tracks if store is available

	// Version upgrade tracking
	versionUpgradeDetected = false // Set to true if bd version changed since last run
	previousVersion        = ""    // The last bd version user had (empty = first run or unknown)
	upgradeAcknowledged    = false // Set to true after showing upgrade notification once per session
)

type envSnapshotValue struct {
	value string
	ok    bool
}

var changeDirEnvSnapshot map[string]envSnapshotValue
var changeDirOrigWD string

var (
	noColorFlag       bool
	sandboxMode       bool
	globalFlag        bool
	serverMode        bool
	proxiedServerMode bool
	readonlyMode      bool               // Read-only mode: block write operations (for worker sandboxes)
	storeIsReadOnly   atomic.Bool        // Track if store was opened read-only (for staleness checks)
	ignoreSchemaSkew  bool               // Proceed despite forward schema drift
	lockTimeout       = 30 * time.Second // Dolt open timeout (fixed default)
	cpuProfileEnabled bool
	profileFile       *os.File
	traceFile         *os.File
	memProfilePath    string
	verboseFlag       bool // Enable verbose/debug output
	quietFlag         bool // Suppress non-essential output

	// Dolt auto-commit policy (flag/config). Values: off | on
	doltAutoCommit string

	// commandDidWrite is set when a command performs a write that should trigger
	// auto-flush. Used to decide whether to auto-commit Dolt after the command completes.
	// Thread-safe via atomic.Bool to avoid data races in concurrent flush operations.
	commandDidWrite atomic.Bool

	// commandMayEmptyJSONLExport is set by destructive maintenance commands
	// after they actually delete rows, allowing post-run auto-export to record
	// an intentional empty JSONL artifact instead of treating it as ambiguous.
	commandMayEmptyJSONLExport atomic.Bool

	// commandDidExplicitDoltCommit is set when a command already created a Dolt commit
	// explicitly (e.g., bd sync in dolt-native mode, hook flows, bd vc commit).
	// This prevents a redundant auto-commit attempt in PersistentPostRun.
	commandDidExplicitDoltCommit bool

	// commandDidWriteTipMetadata is set when a command records a tip as "shown" by writing
	// metadata (tip_*_last_shown). This will be used to create a separate Dolt commit for
	// tip writes, even when the main command is read-only.
	commandDidWriteTipMetadata bool

	// commandTipIDsShown tracks which tip IDs were shown in this command (deduped).
	// This is used for tip-commit message formatting.
	commandTipIDsShown map[string]struct{}

	// commandSpan is the root OTel span for the current command execution.
	// All storage and AI spans are nested as children of this span.
	commandSpan atomic.Pointer[commandSpanHolder]
)

type commandSpanHolder struct{ span oteltrace.Span }

func currentCommandSpan() oteltrace.Span {
	holder := commandSpan.Load()
	if holder == nil {
		return nil
	}
	return holder.span
}

func setCommandSpan(span oteltrace.Span) {
	if span == nil {
		commandSpan.Store(nil)
		return
	}
	commandSpan.Store(&commandSpanHolder{span: span})
}

// skipStoreAnnotation, when set to "1" on a command (or any of its ancestors),
// makes bd skip database/store initialization for that command — the
// annotation-based equivalent of listing the command name in noDbCommands. It
// lets commands defined in other files or build-tagged variants opt out of the
// store gate locally, without editing the central noDbCommands list.
const skipStoreAnnotation = "bd:skip_store"

func init() {
	// Initialize viper configuration
	if err := config.Initialize(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to initialize config: %v\n", err)
	}

	// Register persistent flags
	flags := persistentFlags()
	rootCmd.PersistentFlags().StringVarP(&flags.ChangeDir, "directory", "C", "", "Change to this directory before running the command (like git -C)")
	rootCmd.PersistentFlags().StringVar(&flags.DBPath, "db", "", "Database path (default: auto-discover .beads/*.db). In proxied-server mode, a value that isn't an existing path is treated as a database name override (see --database)")
	rootCmd.PersistentFlags().StringVar(&flags.DatabaseFlag, "database", "", "Run against a different server database for this invocation, without changing the project's configured database (proxied-server mode only)")
	rootCmd.PersistentFlags().StringVar(&flags.Actor, "actor", "", "Actor name for audit trail (default: $BEADS_ACTOR, git user.name, $USER)")
	rootCmd.PersistentFlags().BoolVar(&flags.JSONOutput, "json", false, "Output in JSON format")
	rootCmd.PersistentFlags().String("format", "", "Output format (json). Alias for --json")
	_ = rootCmd.PersistentFlags().MarkHidden("format") // Hidden alias for CLI ergonomics
	rootCmd.PersistentFlags().BoolVar(&flags.SandboxMode, "sandbox", false, "Sandbox mode: disables Dolt auto-push")
	rootCmd.PersistentFlags().BoolVar(&flags.ReadonlyMode, "readonly", false, "Read-only mode: block write operations (for worker sandboxes)")
	rootCmd.PersistentFlags().BoolVar(&flags.GlobalFlag, "global", false, "Use the global shared-server database (beads_global)")
	rootCmd.PersistentFlags().StringVar(&flags.DoltAutoCommit, "dolt-auto-commit", "", "Dolt auto-commit policy (off|on|batch). 'on': commit after each write. 'batch': defer commits to bd dolt commit; uncommitted changes persist in the working set until then (a live batch-mode bd process also flushes on SIGTERM/SIGHUP). Applies to embedded and direct SQL-server modes; proxied-server routes are unaffected. Default: on. Override via config key dolt.auto-commit")
	rootCmd.PersistentFlags().BoolVar(&flags.CPUProfileEnabled, "cpu-profile", false, "Generate CPU profile for performance analysis")
	rootCmd.PersistentFlags().StringVar(&flags.MemProfilePath, "mem-profile", "", "Write heap profile to FILE on exit (also respects BEADS_MEM_PROFILE)")
	rootCmd.PersistentFlags().BoolVarP(&flags.Verbose, "verbose", "v", false, "Enable verbose/debug output")
	rootCmd.PersistentFlags().BoolVarP(&flags.Quiet, "quiet", "q", false, "Suppress non-essential output (errors only)")
	rootCmd.PersistentFlags().BoolVar(&flags.IgnoreSchemaSkew, "ignore-schema-skew", false, "Proceed despite forward schema drift (some queries may fail)")
	rootCmd.PersistentFlags().BoolVar(&flags.NoColorFlag, "no-color", false, "Disable color output (also: NO_COLOR=1 or CLICOLOR=0)")

	// Add --version flag to root command (same behavior as version subcommand)
	rootCmd.Flags().BoolP("version", "V", false, "Print version information")

	// Command groups for organized help output (Tufte-inspired)
	rootCmd.AddGroup(&cobra.Group{ID: "issues", Title: "Working With Issues:"})
	rootCmd.AddGroup(&cobra.Group{ID: "views", Title: "Views & Reports:"})
	rootCmd.AddGroup(&cobra.Group{ID: "deps", Title: "Dependencies & Structure:"})
	rootCmd.AddGroup(&cobra.Group{ID: "sync", Title: "Sync & Data:"})
	rootCmd.AddGroup(&cobra.Group{ID: "setup", Title: "Setup & Configuration:"})
	// NOTE: Many maintenance commands (clean, cleanup, compact, validate, repair-deps)
	// should eventually be consolidated into 'bd doctor' and 'bd doctor --fix' to simplify
	// the user experience. The doctor command can detect issues and offer fixes interactively.
	rootCmd.AddGroup(&cobra.Group{ID: "maint", Title: "Maintenance:"})
	rootCmd.AddGroup(&cobra.Group{ID: "advanced", Title: "Integrations & Advanced:"})

	// Custom help function with semantic coloring (Tufte-inspired)
	// Note: Usage output (shown on errors) is not styled to avoid recursion issues
	rootCmd.SetHelpFunc(colorizedHelpFunc)
}

var errNoBeadsProject = errors.New("no beads project found")

func resolveChangeDirAbs(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("cannot resolve -C directory %q: %w", path, err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("cannot use -C directory %q: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("cannot use -C directory %q: not a directory", path)
	}
	return absPath, nil
}

func resolveChangeDirBeadsDir(path string) (string, error) {
	absPath, err := resolveChangeDirAbs(path)
	if err != nil {
		return "", err
	}
	if absPath == "" {
		return "", nil
	}
	beadsDir := beads.FindBeadsDirFrom(absPath)
	if beadsDir == "" {
		return "", fmt.Errorf("cannot use -C directory %q: %w", path, errNoBeadsProject)
	}
	return beadsDir, nil
}

// allowsChangeDirWithoutProject reports whether -C may target a directory
// that does not yet contain a beads project. Identify init by command
// pointer, not by the name "init": bd notion init is a different command.
func allowsChangeDirWithoutProject(cmd *cobra.Command) bool {
	for current := cmd; current != nil; current = current.Parent() {
		if current == initCmd {
			return true
		}
	}
	return false
}

func chdirForChangeDir(absPath string) error {
	if getChangeDirOrigWD() == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("cannot remember working directory for -C: %w", err)
		}
		setChangeDirOrigWD(wd)
	}
	if err := os.Chdir(absPath); err != nil {
		_ = os.Chdir(getChangeDirOrigWD())
		setChangeDirOrigWD("")
		return fmt.Errorf("cannot change to -C directory %q: %w", absPath, err)
	}
	return nil
}

func applyChangeDirSelection(cmd *cobra.Command) error {
	selectedDir := getChangeDir()
	if strings.TrimSpace(selectedDir) == "" {
		return nil
	}
	absPath, err := resolveChangeDirAbs(selectedDir)
	if err != nil {
		return HandleError("%v", err)
	}
	beadsDir, err := resolveChangeDirBeadsDir(selectedDir)
	if err != nil {
		if !errors.Is(err, errNoBeadsProject) || !allowsChangeDirWithoutProject(cmd) {
			return HandleError("%v", err)
		}
		if err := chdirForChangeDir(absPath); err != nil {
			return HandleError("%v", err)
		}
		return nil
	}
	if err := chdirForChangeDir(absPath); err != nil {
		return HandleError("%v", err)
	}
	snapshot := make(map[string]envSnapshotValue, 3)
	for _, key := range []string{"BEADS_DIR", "BEADS_DB", "BD_DB"} {
		value, ok := os.LookupEnv(key)
		snapshot[key] = envSnapshotValue{value: value, ok: ok}
	}
	setChangeDirEnvSnapshot(snapshot)
	_ = os.Setenv("BEADS_DIR", beadsDir)
	return nil
}

func restoreChangeDirSelection() {
	if snapshot := getChangeDirEnvSnapshot(); snapshot != nil {
		for key, value := range snapshot {
			if value.ok {
				_ = os.Setenv(key, value.value)
			} else {
				_ = os.Unsetenv(key)
			}
		}
		setChangeDirEnvSnapshot(nil)
	}
	if origWD := getChangeDirOrigWD(); origWD != "" {
		_ = os.Chdir(origWD)
		setChangeDirOrigWD("")
	}
}

func guardLegacyNoStoreCommand(cmd *cobra.Command, beadsDir string) error {
	if !shouldGuardLegacyNoStoreCommand(cmd) || legacyNoStoreCommandExempt(cmd) {
		return nil
	}
	if beadsDir == "" {
		return guardUndiscoveredLegacyWorkspace()
	}
	return guardLegacyUpgradeWorkspace(beadsDir)
}

func shouldGuardLegacyNoStoreCommand(cmd *cobra.Command) bool {
	return cmd != nil && cmd.Runnable() && cmd.Parent() != nil
}

func legacyNoStoreCommandExempt(cmd *cobra.Command) bool {
	return legacyNoStoreCommandExemptByPointer(cmd) ||
		legacyNoStoreCommandExemptBySchema(cmd) ||
		legacyNoStoreCommandExemptByParent(cmd) ||
		legacyNoStoreCommandExemptByName(cmd)
}

func legacyNoStoreCommandExemptByPointer(cmd *cobra.Command) bool {
	return cmd == versionCmd || cmd == doctorCmd || cmd == initCmd || cmd == bootstrapCmd || cmd == legacySQLiteCmd
}

func legacyNoStoreCommandExemptBySchema(cmd *cobra.Command) bool {
	return cmd == schemaCmd && cmd.Parent() != nil && cmd.Parent().Parent() == nil
}

func legacyNoStoreCommandExemptByParent(cmd *cobra.Command) bool {
	for current := cmd; current != nil; current = current.Parent() {
		if current == metricsCmd {
			return true
		}
	}
	return false
}

func legacyNoStoreCommandExemptByName(cmd *cobra.Command) bool {
	switch cmd.Name() {
	case "__complete", "__completeNoDesc", "bash", "completion", "fish", "help", "powershell", "zsh":
		return true
	default:
		return false
	}
}

var rootCmd = &cobra.Command{
	Use:   "bd",
	Short: "bd - Dependency-aware issue tracker",
	Long:  `Issues chained together like beads. A lightweight issue tracker with first-class dependency support.`,
	Run: func(cmd *cobra.Command, args []string) {
		// Handle --version flag on root command
		if v, _ := cmd.Flags().GetBool("version"); v {
			fmt.Printf("bd version %s (%s)\n", Version, Build)
			return
		}
		// No subcommand - show help
		_ = cmd.Help() // Help() always returns nil for cobra commands
	},
	PersistentPreRunE:  runPersistentPreRun,
	PersistentPostRunE: runPersistentPostRun,
}

func shouldRunPostCommandAutoExport(cmd *cobra.Command) bool {
	if cmd == nil {
		return true
	}
	return !isReadOnlyCommand(cmd.Name())
}

func shouldRunAutoImportJSONL(cmd *cobra.Command, s storage.DoltStorage, useReadOnly, globalFlag, serverMode bool) bool {
	if cmd == nil || s == nil || useReadOnly || globalFlag || serverMode {
		return false
	}
	// import.auto=false (or BD_IMPORT_AUTO=false) must disable ALL auto-import
	// behavior, not just the git-hook sync path (importJSONLForSync). Without
	// this check, a fresh/empty database would silently auto-import stale
	// issues.jsonl on every write command regardless of the config setting
	// (GH#4304).
	if !config.GetBool("import.auto") {
		return false
	}
	return cmd.Name() != "import"
}

// isDisablingImportAutoViaConfigCommand reports whether the command about to
// run is "bd config set import.auto false" (or an equivalent
// "bd config set-many ... import.auto=false" pair). shouldRunAutoImportJSONL
// runs in PersistentPreRun before configSetCmd/configSetManyCmd write the new
// value to config.yaml, so without this exemption the master switch would
// trigger the very auto-import it is meant to disable on its own invocation
// when a stale .beads/issues.jsonl sits next to an empty database (GH#4304).
func isDisablingImportAutoViaConfigCommand(cmd *cobra.Command, args []string) bool {
	if cmd == nil || cmd.Parent() == nil || cmd.Parent().Name() != "config" {
		return false
	}
	switch cmd.Name() {
	case "set":
		return isDisablingImportAutoSet(args)
	case "set-many":
		return isDisablingImportAutoSetMany(args)
	default:
		return false
	}
}

func isDisablingImportAutoSet(args []string) bool {
	return len(args) >= 2 && args[0] == "import.auto" && isFalsyConfigValue(args[1])
}

func isDisablingImportAutoSetMany(args []string) bool {
	for _, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		if ok && key == "import.auto" && isFalsyConfigValue(value) {
			return true
		}
	}
	return false
}

// isFalsyConfigValue reports whether a config value string parses as a
// boolean false (e.g. "false", "0", "f").
func isFalsyConfigValue(value string) bool {
	parsed, err := strconv.ParseBool(value)
	return err == nil && !parsed
}

func commandAllowsEmptyAutoExport(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	switch cmd.Name() {
	case "prune", "purge":
		return commandMayEmptyJSONLExport.Load()
	default:
		return false
	}
}

// blockedEnvVars lists environment variables that must not be set because they
// could silently override the storage backend via viper's AutomaticEnv, causing
// data fragmentation (bd-hevyw).
var blockedEnvVars = []string{"BD_BACKEND", "BD_DATABASE_BACKEND"}

// checkBlockedEnvVars returns an error if any blocked env vars are set.
func checkBlockedEnvVars() error {
	for _, name := range blockedEnvVars {
		if os.Getenv(name) != "" {
			return fmt.Errorf("%s env var is not supported and has been removed to prevent data fragmentation.\n"+
				"Unset %s; storage selection comes from .beads/metadata.json. To choose a different supported backend, follow 'bd help init-safety'; do not edit metadata.json by hand", name, name)
		}
	}
	return nil
}

// setupGracefulShutdown creates a context that cancels on SIGINT/SIGTERM/SIGHUP.
// Before cancellation, it flushes pending batch commits so that accumulated
// changes in the Dolt working set are not lost on graceful shutdown.
func setupGracefulShutdown(exit func(int)) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec // G118: cancel is returned and called by caller

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		select {
		case <-sigCh:
			flushBatchCommitOnShutdown()
			cancel()
			// On second signal, force exit
			<-sigCh
			exit(1)
		case <-ctx.Done():
			signal.Stop(sigCh)
		}
	}()

	return ctx, cancel
}

// flushBatchCommitOnShutdown commits any pending batch changes before process exit.
// This prevents data loss when SIGTERM/SIGHUP kills a process with uncommitted
// batch writes sitting in the Dolt working set.
func flushBatchCommitOnShutdown() {
	mode, err := getDoltAutoCommitMode()
	if err != nil || mode != doltAutoCommitBatch {
		return
	}

	storeMutex.Lock()
	active := isStoreActive()
	st := getStore()
	storeMutex.Unlock()

	if !active || st == nil {
		return
	}

	// Use a fresh context with timeout — rootCtx is about to be canceled.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// CommitPending reports atomically whether a commit actually landed, so a
	// clean shutdown stays quiet without spending the 5s flush budget on
	// HEAD-reporting probes before the commit itself (and without racing a
	// concurrent writer's HEAD movement the way a before/after compare would).
	committed, err := st.CommitPending(ctx, getActorWithGit())
	if err != nil {
		if !isDoltNothingToCommit(err) {
			fmt.Fprintf(os.Stderr, "\nWarning: failed to flush batch commit on shutdown: %v\n", err)
		}
		return
	}
	if !committed {
		return
	}

	fmt.Fprintf(os.Stderr, "\nFlushed pending batch commit on shutdown\n")
}

// validateWorkspaceIdentity checks that the project identity from metadata.json
// matches the database's stored project_id. A mismatch indicates configuration
// drift — the CLI may be pointing at the wrong database (GH#2438, GH#2372).
//
// This check only runs for write commands because:
// 1. Read commands are safe even against wrong databases (no data mutation)
// 2. The check requires an open store connection
// 3. New databases won't have _project_id yet (bootstrap case)
func validateWorkspaceIdentity(ctx context.Context, beadsDir string) error {
	if getStore() == nil {
		return nil // No store connection, nothing to validate
	}

	// Load project_id from metadata.json
	cfg, err := configfile.Load(beadsDir)
	if err != nil || cfg == nil {
		return nil // No config, skip validation (fresh init)
	}
	configProjectID := cfg.ProjectID
	if configProjectID == "" {
		return nil // No project_id in config (pre-identity era)
	}

	// Get project_id from database
	dbProjectID, err := getStore().GetMetadata(ctx, "_project_id")
	if err != nil || dbProjectID == "" {
		return nil // No project_id in DB (new or pre-identity database)
	}

	// Compare: mismatch means drift
	if configProjectID != dbProjectID {
		fmt.Fprintf(os.Stderr, "Error: workspace identity mismatch detected\n\n")
		fmt.Fprintf(os.Stderr, "  metadata.json project_id: %s\n", configProjectID)
		fmt.Fprintf(os.Stderr, "  database _project_id:     %s\n\n", dbProjectID)
		fmt.Fprintf(os.Stderr, "This means the CLI config and database belong to different projects.\n")
		fmt.Fprintf(os.Stderr, "Possible causes:\n")
		fmt.Fprintf(os.Stderr, "  • BEADS_DIR points to a different project's .beads/\n")
		fmt.Fprintf(os.Stderr, "  • Dolt server endpoint changed and now serves a different database\n")
		fmt.Fprintf(os.Stderr, "  • metadata.json was copied from another project\n\n")
		fmt.Fprintf(os.Stderr, "Recovery: run 'bd doctor --fix' or 'bd bootstrap' to reconcile workspace metadata with the authoritative database when shared-server metadata drifted.\n")
		fmt.Fprintf(os.Stderr, "To diagnose: bd context --json\n")
		fmt.Fprintf(os.Stderr, "To override: set BEADS_SKIP_IDENTITY_CHECK=1\n")
		return SilentExit()
	}
	return nil
}

func main() {
	runMain(os.Exit)
}

func runMain(exit func(int)) {
	// BD_NAME overrides the binary name in help text (e.g. BD_NAME=ops makes
	// "ops --help" show "ops" instead of "bd"). Useful for multi-instance
	// setups where wrapper scripts set BEADS_DIR for routing.
	if name := os.Getenv("BD_NAME"); name != "" {
		setRootCommandName(rootCmd, name)
	}

	// Register --all flag on Cobra's auto-generated help command.
	// Must be called after init() so all subcommands are registered and
	// Cobra has created its default help command.
	rootCmd.InitDefaultHelpCmd()
	registerHelpAllFlag()

	executedCmd, err := rootCmd.ExecuteC()

	// Let this command's fire-and-forget hooks finish, for the same
	// every-exit-path reason the metrics flush below is here rather than in
	// PersistentPostRunE: cobra SKIPS PostRunE when RunE returns an error, so a
	// partial batch — `bd close A B` where A commits and B refuses — would exit
	// with A's committed mutation never reaching its hook script. Idempotent, so
	// the PostRunE call on the clean path makes this one free.
	waitForCommandHooks()

	// Finalize queued metrics and detach the uploader. Shared with the os.Exit
	// guards (CheckReadonly and the pre-run gates) so every exit path flushes the
	// same way instead of only the clean RunE/ExecuteC return.
	metrics.CloseAndFlush()

	if err != nil {
		if code, ok := exitCodeFromError(err); ok {
			exit(code)
		}
		if executedCmd != nil && executedCmd.SilenceErrors {
			fmt.Fprintf(os.Stderr, "Error: %s\n", err.Error())
		}
		exit(1)
	}
}

func setRootCommandName(cmd *cobra.Command, name string) {
	cmd.Use = name
}

func resolveMetricsEnabled() bool {
	if v, ok := os.LookupEnv(metrics.EnvDisableMetrics); ok {
		return !envTruthyValue(v)
	}
	// DO_NOT_TRACK is a disable-only alias: a truthy value opts out, but a
	// falsey or empty value (DO_NOT_TRACK=0/false/"") must fall through to the
	// user's saved preference instead of forcing metrics back on over a saved
	// `bd metrics off`. Only BD_DISABLE_METRICS (checked first) is a
	// bidirectional override.
	if v, ok := os.LookupEnv(metrics.EnvDoNotTrack); ok && envTruthyValue(v) {
		return false
	}
	// Consent is the user's own global choice: resolve it from the user-global
	// config only, never merged project/BEADS_DIR config. Otherwise a
	// repository's .beads/config.yaml (highest viper precedence) could re-enable
	// metrics for a user who ran `bd metrics off`.
	return !config.MetricsDisabledByUserConfig()
}

func resolveMetricsEndpoint() string {
	if v := os.Getenv(metrics.EnvEndpoint); v != "" {
		return v
	}
	// Like enablement, the endpoint is resolved from env + user-global config
	// only so a repository can never redirect where a user's metrics are sent.
	if ep := config.UserMetricsEndpoint(); ep != "" {
		return ep
	}
	return metrics.DefaultEndpoint
}

func envTruthyValue(v string) bool {
	if v == "" {
		return false
	}
	switch strings.ToLower(v) {
	case "0", "false":
		return false
	}
	return true
}
