package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/spf13/cobra"
	"github.com/subosito/gotenv"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/doltserver"
)

// loadBeadsEnvFile loads .beads/.env into process environment for per-project
// Dolt credentials (GH#2520). Uses gotenv.Load which is non-overriding —
// existing shell env vars always take precedence.
// Safe to call with an empty beadsDir (no-op).
func loadBeadsEnvFile(beadsDir string) {
	if beadsDir == "" {
		return
	}
	envFile := filepath.Join(beadsDir, ".env")
	if _, err := os.Stat(envFile); err != nil {
		return
	}
	_ = gotenv.Load(envFile)
}

func logConfigDiscovery(beadsDir, reason string) {
	metadataPath := filepath.Join(beadsDir, configfile.ConfigFileName)
	configYAMLPath := filepath.Join(beadsDir, "config.yaml")
	_, metadataErr := os.Stat(metadataPath)
	_, yamlErr := os.Stat(configYAMLPath)
	debug.Logf("Debug: %s at %s -> metadata=%v (%v), config.yaml=%v (%v)\n",
		reason, beadsDir, metadataErr == nil, metadataErr, yamlErr == nil, yamlErr)
}

func shouldLogDefaultDoltDatabase(cfg *configfile.Config) bool {
	return cfg != nil && cfg.DoltDatabase == "" && os.Getenv("BEADS_DOLT_SERVER_DATABASE") == ""
}

// loadBeadsSelectionEnvFile loads only the selector keys needed for early
// workspace/database discovery. Unlike loadBeadsEnvFile, this intentionally
// limits itself to BEADS_DIR / BEADS_DB / BD_DB so caller credentials and
// runtime knobs do not leak into explicit-target commands before rebinding.
func loadBeadsSelectionEnvFile(beadsDir string) {
	if beadsDir == "" {
		return
	}
	envFile := filepath.Join(beadsDir, ".env")
	pairs, err := gotenv.Read(envFile)
	if err != nil {
		return
	}
	for _, key := range []string{"BEADS_DIR", "BEADS_DB", "BD_DB"} {
		if os.Getenv(key) != "" {
			continue
		}
		if value, ok := pairs[key]; ok && strings.TrimSpace(value) != "" {
			_ = os.Setenv(key, value)
		}
	}
}

// loadSelectionEnvironment loads only the selector keys required to discover
// the target workspace/database before the store-init path runs. This preserves
// historical support for .beads/.env files that route commands via BEADS_DB or
// BEADS_DIR without importing the caller workspace's broader runtime settings.
func loadSelectionEnvironment() {
	if os.Getenv("BEADS_DIR") != "" || os.Getenv("BEADS_DB") != "" || os.Getenv("BD_DB") != "" {
		return
	}
	if beadsDir := beads.FindBeadsDir(); beadsDir != "" {
		loadBeadsSelectionEnvFile(beadsDir)
	}
}

// loadEnvironment runs the lightweight, always-needed environment setup that
// must happen before the noDbCommands early return. This ensures commands like
// "bd doctor --server" pick up per-project Dolt credentials from .beads/.env.
//
// This function intentionally does NOT do any store initialization, auto-migrate,
// or telemetry setup — those belong in the store-init phase that runs after the
// noDbCommands check.
func loadEnvironment() {
	// FindBeadsDir is lightweight (filesystem walk, no git subprocesses)
	// and resolves BEADS_DIR, redirects, and worktree paths.
	if beadsDir := beads.FindBeadsDir(); beadsDir != "" {
		loadBeadsEnvFile(beadsDir)
		// Non-fatal warning if .beads/ directory has overly permissive access.
		config.CheckBeadsDirPermissions(beadsDir)
	}
}

var sharedServerEmbeddedMismatchWarned atomic.Bool

// warnSharedServerEmbeddedMismatch detects the case where shared-server mode
// is active but metadata.json explicitly pins dolt_mode=embedded. The
// shared-server setting wins for this invocation (GH#2946/2949: stale embedded
// metadata must not hide server-backed issue state), but bd never rewrites the
// committed metadata.json — per-machine environment must not leak into shared
// config (bd-6dnrw.5). Print guidance so the user resolves the conflict
// explicitly.
func warnSharedServerEmbeddedMismatch(cfg *configfile.Config) {
	if cfg == nil || sharedServerEmbeddedMismatchWarned.Load() {
		return
	}
	if strings.ToLower(strings.TrimSpace(cfg.DoltMode)) != configfile.DoltModeEmbedded {
		return
	}
	if !doltserver.IsSharedServerMode() {
		return
	}
	sharedServerEmbeddedMismatchWarned.Store(true)
	fmt.Fprintln(os.Stderr, "Notice: shared-server mode is enabled (BEADS_DOLT_SHARED_SERVER or dolt.shared-server in config.yaml) but .beads/metadata.json pins dolt_mode=\"embedded\". Using the shared server for this run.")
	fmt.Fprintln(os.Stderr, "  To persist server mode: set dolt_mode to \"server\" in .beads/metadata.json and commit it.")
	fmt.Fprintln(os.Stderr, "  To stay embedded: unset BEADS_DOLT_SHARED_SERVER (or remove dolt.shared-server from config.yaml).")
}

// loadServerModeFromBeadsDir loads the storage mode (embedded vs server vs
// proxied-server) from the given beads directory's metadata.json so that
// usesSQLServer() and usesProxiedServer() return the correct values.
//
// A metadata.json that exists but cannot be loaded is a hard error: treating
// it like an absent file silently flips server-mode deployments onto the
// embedded store, where every query answers from an empty relic with exit 0
// (false-empty). Absent metadata.json (cfg == nil) keeps the fresh-repo
// embedded default.
func loadServerModeFromBeadsDir(beadsDir string) error {
	if beadsDir == "" {
		return nil
	}
	cfg, err := configfile.LoadForDiscovery(beadsDir)
	if err != nil {
		return fmt.Errorf("load %s: %w; no storage database was opened or modified (storage mode unknown; data commands refuse to fall back to the embedded store)", configfile.ConfigPath(beadsDir), err)
	}
	// Absent metadata.json keeps the fresh-repo embedded default unless
	// env/config.yaml supply a remote host (GH#3545) — inference must not
	// depend on metadata existing.
	cfg = normalizeLoadedConfig(cfg)
	warnSharedServerEmbeddedMismatch(cfg)
	psm := cfg.IsDoltProxiedServerMode()
	sm := cfg.IsDoltServerMode()
	// GH#2946: shared-server override for stale metadata.json (no-db commands)
	if !sm && !psm && doltserver.IsSharedServerMode() {
		sm = true
	}
	setServerMode(sm)
	setProxiedServerMode(psm)
	return nil
}

// loadServerModeFromConfig loads the storage mode (embedded vs server vs
// proxied-server) from metadata.json so that usesSQLServer() and
// usesProxiedServer() return the correct values. Called for commands that
// skip full DB init but still need to know the mode.
func loadServerModeFromConfig() error {
	return loadServerModeFromBeadsDir(beads.FindBeadsDir())
}

func preserveRedirectSourceDatabase(beadsDir string) {
	if beadsDir == "" || os.Getenv("BEADS_DOLT_SERVER_DATABASE") != "" {
		return
	}

	rInfo := beads.ResolveRedirect(beadsDir)
	if rInfo.WasRedirected && rInfo.SourceDatabase != "" {
		_ = os.Setenv("BEADS_DOLT_SERVER_DATABASE", rInfo.SourceDatabase)
		if os.Getenv("BD_DEBUG_ROUTING") != "" {
			fmt.Fprintf(os.Stderr, "[routing] Preserved source dolt_database %q across redirect\n", rInfo.SourceDatabase) //nolint:gosec // G705: CLI stderr, not HTML.
		}
	}
}

func selectedNoDBBeadsDir(cmd *cobra.Command) string {
	if selectedBeadsDir, handled := selectedNoDBBeadsDirFromCommand(cmd); handled {
		if selectedBeadsDir != "" {
			return selectedBeadsDir
		}
	} else if selectedBeadsDir, handled := selectedNoDBBeadsDirFromEnvironment(); handled {
		if selectedBeadsDir != "" {
			return selectedBeadsDir
		}
	}
	if selectedBeadsDir := selectedNoDBBeadsDirFromEnvironmentVariable(); selectedBeadsDir != "" {
		return selectedBeadsDir
	}
	if selectedBeadsDir := selectedNoDBBeadsDirFromDBPath(); selectedBeadsDir != "" {
		return selectedBeadsDir
	}
	return beads.FindBeadsDir()
}

func selectedNoDBBeadsDirFromCommand(cmd *cobra.Command) (string, bool) {
	if cmd != nil && cmd.Root() != nil && cmd.Root().PersistentFlags().Changed("db") && getDBPath() != "" {
		return resolveCommandBeadsDir(getDBPath()), true
	}
	if cmd != nil && cmd.PersistentFlags().Changed("db") && getDBPath() != "" {
		return resolveCommandBeadsDir(getDBPath()), true
	}
	return "", false
}

func selectedNoDBBeadsDirFromEnvironment() (string, bool) {
	if envDB := os.Getenv("BEADS_DB"); envDB != "" {
		return resolveCommandBeadsDir(envDB), true
	}
	if envDB := os.Getenv("BD_DB"); envDB != "" {
		return resolveCommandBeadsDir(envDB), true
	}
	return "", false
}

func selectedNoDBBeadsDirFromEnvironmentVariable() string {
	if os.Getenv("BEADS_DIR") == "" {
		return ""
	}
	return beads.FindBeadsDir()
}

func selectedNoDBBeadsDirFromDBPath() string {
	if getDBPath() == "" {
		return ""
	}
	return resolveCommandBeadsDir(getDBPath())
}

func isSelectedNoDBCommand(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	if cmd.Name() == "context" || cmd.Name() == "where" {
		return true
	}
	if cmd.Parent() == nil || cmd.Parent().Name() != "dolt" {
		return false
	}
	switch cmd.Name() {
	case "push", "pull", "commit":
		return false
	default:
		return true
	}
}

// configCommandCanRunWithoutStore returns true for config subcommands whose Run
// path can execute without an opened Dolt store. This lets no-workspace calls
// fail or degrade in the command itself instead of tripping low-level DB init.
func configCommandCanRunWithoutStore(cmd *cobra.Command, args []string) bool {
	if cmd == nil || cmd.Parent() == nil || cmd.Parent().Name() != "config" {
		return false
	}

	switch cmd.Name() {
	case "show", "validate", "drift", "apply":
		return true
	case "set", "get", "unset":
		return configCommandKeyCanRunWithoutStore(args)
	case "set-many":
		return configSetManyCanRunWithoutStore(args)
	default:
		return false
	}
}

func configCommandKeyCanRunWithoutStore(args []string) bool {
	if len(args) == 0 {
		return true
	}
	key := args[0]
	return config.IsYamlOnlyKey(key) || key == "beads.role"
}

func configSetManyCanRunWithoutStore(args []string) bool {
	if len(args) == 0 {
		return true
	}
	for _, arg := range args {
		key, _, ok := strings.Cut(arg, "=")
		if !ok || key == "" {
			return true
		}
		if !config.IsYamlOnlyKey(key) && key != "beads.role" {
			return false
		}
	}
	return true
}

func prepareSelectedCommandContext(beadsDir string, loadEnv bool) {
	if beadsDir == "" {
		return
	}
	_ = os.Setenv("BEADS_DIR", beadsDir)
	if loadEnv {
		loadBeadsEnvFile(beadsDir)
	}
	preserveRedirectSourceDatabase(beadsDir)
	if err := config.Initialize(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to reinitialize config for selected beads dir: %v\n", err)
	}
	config.CheckBeadsDirPermissions(beadsDir)
	if err := loadServerModeFromBeadsDir(beadsDir); err != nil {
		// Warn, don't fatal: this context also serves no-DB commands —
		// doctor, init, bootstrap, config — which are exactly the repair
		// paths for a corrupt metadata.json. Data commands stay protected
		// by the hard error at store init and in the store factories.
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
}

func prepareSelectedNoDBContext(beadsDir string) {
	prepareSelectedCommandContext(beadsDir, true)
}

// refreshBoundCommandConfig reapplies config-backed defaults after the command
// context has been rebound to a resolved target beads directory. This keeps
// explicit flags authoritative while letting rerouted/explicit-db commands use
// the target repo's config rather than the caller's config.
func refreshBoundCommandConfig(cmd *cobra.Command) {
	if cmd == nil {
		return
	}
	root := cmd.Root()
	if root == nil {
		root = cmd
	}
	if !root.PersistentFlags().Changed("json") && !root.PersistentFlags().Changed("format") {
		setJSONOutput(config.GetBool("json"))
	}
	if !root.PersistentFlags().Changed("readonly") {
		setReadonlyMode(config.GetBool("readonly"))
	}
	if !root.PersistentFlags().Changed("actor") {
		setActor(resolveConfiguredActor())
	}
	if !root.PersistentFlags().Changed("dolt-auto-commit") {
		setDoltAutoCommit(config.GetString("dolt.auto-commit"))
	}
}

// resolveCommandBeadsDir maps a discovered Dolt data path back to the owning
// .beads directory. filepath.Dir(dbPath) only works when the Dolt data lives
// under .beads/dolt; custom dolt_data_dir values can place it elsewhere.
func resolveCommandBeadsDir(dbPath string) string {
	if dbPath == "" {
		return ""
	}

	// Use the same validated candidate logic as the helper/reopen path
	// (GH#2627). This checks filepath.Dir, canonicalized paths, AND
	// FindBeadsDir — but only returns a candidate whose metadata.json
	// actually points to dbPath, preventing CWD discovery from overriding
	// an explicit --db flag.
	if beadsDir := resolveBeadsDirForDBPath(dbPath); beadsDir != "" {
		return beadsDir
	}

	for dir := filepath.Dir(dbPath); dir != "" && dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, ".beads")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}

	// No candidate matched — fall back to parent directory of the db path.
	// This handles bootstrap/init where no metadata.json exists yet.
	return filepath.Dir(dbPath)
}

// resolveConfiguredActor returns the actor implied by env/config when no
// explicit --actor flag was passed, honoring the documented priority
// BEADS_ACTOR > BD_ACTOR (deprecated) > config.yaml `actor`.
//
// viper's AutomaticEnv binds the deprecated BD_ACTOR to the "actor" key (env
// prefix "BD"), and it is consulted ahead of any explicit binding — so
// config.GetString("actor") alone returns BD_ACTOR's value even when
// BEADS_ACTOR is also set, silently letting the deprecated alias win (GH#4645).
// Check BEADS_ACTOR explicitly first so the primary override outranks it.
func resolveConfiguredActor() string {
	if beadsActor := os.Getenv("BEADS_ACTOR"); beadsActor != "" {
		return beadsActor
	}
	return config.GetString("actor")
}

// getActorWithGit returns the actor for audit trails with git config fallback.
// Priority: --actor flag > BEADS_ACTOR env > BD_ACTOR env (deprecated) > git config user.name > $USER > "unknown"
// This provides a sensible default for developers: their git identity is used unless
// explicitly overridden
func getActorWithGit() string {
	// If actor is already set (from --actor flag), use it
	if getActor() != "" {
		return getActor()
	}

	// Check BEADS_ACTOR env var (primary env override)
	if beadsActor := os.Getenv("BEADS_ACTOR"); beadsActor != "" {
		return beadsActor
	}

	// Check BD_ACTOR env var (deprecated alias, kept for backwards compatibility)
	if bdActor := os.Getenv("BD_ACTOR"); bdActor != "" {
		return bdActor
	}

	// Try git config user.name - the natural default for a git-native tool
	if out, err := exec.Command("git", "config", "user.name").Output(); err == nil {
		if gitUser := strings.TrimSpace(string(out)); gitUser != "" {
			return gitUser
		}
	}

	// Fall back to system username
	if user := os.Getenv("USER"); user != "" {
		return user
	}

	return "unknown"
}

// getOwner returns the human owner for CV attribution.
// Priority: GIT_AUTHOR_EMAIL env > git config user.email > "" (empty)
// This is the foundation for HOP CV (curriculum vitae) chains per Decision 008.
// Unlike actor (which tracks who executed), owner tracks the human responsible.
func getOwner() string {
	// Check GIT_AUTHOR_EMAIL first - this is set during git commit operations
	if authorEmail := os.Getenv("GIT_AUTHOR_EMAIL"); authorEmail != "" {
		return authorEmail
	}

	// Fall back to git config user.email - the natural default
	if out, err := exec.Command("git", "config", "user.email").Output(); err == nil {
		if gitEmail := strings.TrimSpace(string(out)); gitEmail != "" {
			return gitEmail
		}
	}

	// Return empty if no email found (owner is optional)
	return ""
}
