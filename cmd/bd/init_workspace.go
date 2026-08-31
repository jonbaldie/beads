package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/git"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

// migrateOldDatabases detects and migrates old database files to beads.db
func migrateOldDatabases(targetPath string, quiet bool) error {
	targetDir := filepath.Dir(targetPath)
	targetName := filepath.Base(targetPath)

	// If target already exists, no migration needed
	if _, err := os.Stat(targetPath); err == nil {
		return nil
	}

	// Create .beads directory if it doesn't exist
	if err := os.MkdirAll(targetDir, 0750); err != nil {
		return fmt.Errorf("failed to create .beads directory: %w", err)
	}

	oldDBs, err := findOldDatabaseFiles(targetDir, targetName)
	if err != nil {
		return err
	}

	if len(oldDBs) == 0 {
		// No old databases to migrate
		return nil
	}

	if len(oldDBs) > 1 {
		// Multiple databases found - ambiguous, require manual intervention
		return fmt.Errorf("multiple database files found in %s: %v\nPlease manually rename the correct database to %s and remove others",
			targetDir, oldDBs, targetName)
	}

	return migrateOldDatabaseFile(oldDBs[0], targetPath, targetName, quiet)
}

func findOldDatabaseFiles(targetDir, targetName string) ([]string, error) {
	// Look for existing .db files in the .beads directory.
	pattern := filepath.Join(targetDir, "*.db")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to search for existing databases: %w", err)
	}

	// Filter out the target file name and any backup files.
	var oldDBs []string
	for _, match := range matches {
		baseName := filepath.Base(match)
		if baseName != targetName && !strings.HasSuffix(baseName, ".backup.db") {
			oldDBs = append(oldDBs, match)
		}
	}
	return oldDBs, nil
}

func migrateOldDatabaseFile(oldDB, targetPath, targetName string, quiet bool) error {
	if !quiet {
		fmt.Fprintf(os.Stderr, "→ Migrating database: %s → %s\n", filepath.Base(oldDB), targetName)
	}

	// Rename the old database to the new canonical name
	if err := os.Rename(oldDB, targetPath); err != nil {
		return fmt.Errorf("failed to migrate database %s to %s: %w", oldDB, targetPath, err)
	}

	if !quiet {
		fmt.Fprintf(os.Stderr, "✓ Database migration complete\n\n")
	}

	return nil
}

// errWorkspaceAlreadyInitialized marks the benign "this workspace already has a
// database" outcome from checkExistingBeadsData. --init-if-missing treats only
// this case as an idempotent skip; any other error from the check is operational
// (e.g. an unreadable .beads/embeddeddolt directory) and must still abort rather
// than be silently masked as success.
var errWorkspaceAlreadyInitialized = errors.New("workspace already initialized")

// workspaceExistsError carries a user-facing "already initialized" message while
// still matching errWorkspaceAlreadyInitialized via errors.Is, so callers can
// distinguish the benign case without the sentinel text leaking into the message.
type workspaceExistsError struct{ msg string }

func (e *workspaceExistsError) Error() string { return e.msg }
func (e *workspaceExistsError) Is(target error) bool {
	return target == errWorkspaceAlreadyInitialized
}

// alreadyInitialized builds a workspaceExistsError from a formatted message.
func alreadyInitialized(format string, args ...any) error {
	return &workspaceExistsError{msg: fmt.Sprintf(format, args...)}
}

// checkExistingBeadsDataAt checks for existing database at a specific beadsDir path.
// This is extracted to support both BEADS_DIR and CWD-based resolution.
//
// A returned error that matches errWorkspaceAlreadyInitialized means a database
// already exists (the benign, idempotent-skip case); any other error is
// operational and must not be treated as success.

func checkExistingBeadsDataAt(beadsDir string, prefix string) error {
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		return nil // No .beads directory, safe to init
	}

	cfg, err := loadExistingWorkspaceConfig(beadsDir)
	if err != nil {
		return err
	}
	if cfg != nil {
		if err := validateConfiguredBackend(cfg); err != nil {
			return err
		}
	}
	if cfg != nil && cfg.GetBackend() == configfile.BackendDolt {
		return checkExistingDoltData(beadsDir, prefix, cfg)
	}
	return checkExistingLegacyData(beadsDir, prefix)
}

func loadExistingWorkspaceConfig(beadsDir string) (*configfile.Config, error) {
	// metadata.json is authoritative for the configured backend, so resolve it once
	// and dispatch. Removed-backend tombstones are marked by metadata
	// alone — there is no local Dolt directory to inspect — so a plain
	// `bd init` (which defaults to Dolt) must not silently repoint a live SQL
	// workspace to a fresh embedded Dolt DB and orphan its issues. --reinit-local
	// /--force bypass this (handled by the caller). Invalid metadata must fail closed:
	// without an explicit reinitialization request, init may not overwrite the only
	// marker for an external or otherwise nonlocal database.
	cfg, err := configfile.LoadForDiscovery(beadsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to load %s: %w; refusing to reinitialize automatically (restore the metadata or use --reinit-local after safeguarding existing data)", configfile.ConfigPath(beadsDir), err)
	}
	return cfg, nil
}

func checkExistingDoltData(beadsDir, prefix string, cfg *configfile.Config) error {
	if cfg.IsDoltProxiedServerMode() {
		return checkExistingProxiedDoltData(beadsDir)
	}
	if !cfg.IsDoltServerMode() {
		return checkExistingEmbeddedDoltData(beadsDir, prefix)
	}
	return checkExistingConfiguredDoltData(beadsDir, prefix, cfg)
}

func checkExistingProxiedDoltData(beadsDir string) error {
	proxiedRoot, err := resolveProxiedServerRootPath(beadsDir)
	if err != nil {
		return fmt.Errorf("resolve proxied server root: %w", err)
	}
	if directoryExists(proxiedRoot) {
		return alreadyInitialized(`
%s Found existing Dolt database: %s

This workspace is already initialized.

To use the existing database:
  Just run bd commands normally (e.g., %s)

Aborting.`, ui.RenderWarn("⚠"), proxiedRoot, ui.RenderAccent("bd list"))
	}
	return nil
}

func checkExistingEmbeddedDoltData(beadsDir, prefix string) error {
	embeddedRoot := filepath.Join(beadsDir, "embeddeddolt")
	location, err := findEmbeddedDoltDatabase(embeddedRoot)
	if err != nil {
		return err
	}
	if location == "" {
		return nil
	}
	return existingDoltDatabaseError(location, prefix)
}

func findEmbeddedDoltDatabase(embeddedRoot string) (string, error) {
	entries, err := os.ReadDir(embeddedRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // No embedded root -> fresh clone, safe to init
		}
		return "", fmt.Errorf("failed to read embedded dolt directory %s: %w", embeddedRoot, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		location := filepath.Join(embeddedRoot, entry.Name())
		if directoryExists(filepath.Join(location, ".dolt")) {
			return location, nil
		}
	}
	return "", nil
}

func checkExistingConfiguredDoltData(beadsDir, prefix string, cfg *configfile.Config) error {
	doltPath := doltserver.ResolveDoltDir(beadsDir)
	if !directoryExists(doltPath) {
		if !cfg.IsDoltServerMode() || !serverDoltDatabaseExists(beadsDir, cfg) {
			// A server-mode workspace with no local directory is safe to
			// initialize when its configured database is missing or unreachable.
			return nil
		}
	}
	return existingDoltDatabaseError(doltDatabaseLocation(beadsDir, doltPath, cfg), prefix)
}

func serverDoltDatabaseExists(beadsDir string, cfg *configfile.Config) bool {
	// For server mode, distinguish "DB exists" from "DB missing" (FR-010).
	result := checkDatabaseOnServer(
		cfg.GetDoltServerHost(),
		doltserver.DefaultConfig(beadsDir).Port,
		cfg.GetDoltServerUser(),
		cfg.GetDoltServerPassword(),
		cfg.GetDoltDatabase(),
		cfg.GetDoltServerTLS(),
	)
	// An unreachable server or a failed lookup is treated like a fresh clone:
	// init can still bootstrap the database (e.g., via --from-jsonl). (GH#2433)
	return result.Reachable && result.Exists
}

func doltDatabaseLocation(beadsDir, doltPath string, cfg *configfile.Config) string {
	if cfg.IsDoltServerMode() {
		return fmt.Sprintf("dolt server at %s:%d", cfg.GetDoltServerHost(), doltserver.DefaultConfig(beadsDir).Port)
	}
	return doltPath
}

func directoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func existingDoltDatabaseError(location, prefix string) error {
	return alreadyInitialized(`
%s Found existing Dolt database: %s

This workspace is already initialized.

To use the existing database:
  Just run bd commands normally (e.g., %s)

If the database is genuinely corrupt and unrecoverable:
  bd export > issue-export.jsonl        # Export issue records first
  bd init --reinit-local --prefix %s    # Then reinitialize

Aborting.`, ui.RenderWarn("⚠"), location, ui.RenderAccent("bd list"), prefix)
}

func checkExistingLegacyData(beadsDir, prefix string) error {
	redirectTarget := beads.FollowRedirect(beadsDir)
	if redirectTarget != beadsDir {
		return checkExistingRedirectTarget(redirectTarget, prefix)
	}
	return checkExistingCanonicalDatabase(beadsDir, prefix)
}

func checkExistingRedirectTarget(redirectTarget, prefix string) error {
	targetDBPath := filepath.Join(redirectTarget, beads.CanonicalDatabaseName)
	if _, err := os.Stat(targetDBPath); err != nil {
		return nil
	}
	return alreadyInitialized(`
%s Cannot init: redirect target already has database

Local .beads redirects to: %s
That location already has: %s

The redirect target is already initialized. Running init here would overwrite it.

To use the existing database:
  Just run bd commands normally (e.g., %s)
  The redirect will route to the canonical database.

If the database is genuinely corrupt and unrecoverable:
  bd export > issue-export.jsonl        # Export issue records first
  bd init --reinit-local --prefix %s    # Then reinitialize

Aborting.`, ui.RenderWarn("⚠"), redirectTarget, targetDBPath, ui.RenderAccent("bd list"), prefix)
}

func checkExistingCanonicalDatabase(beadsDir, prefix string) error {
	dbPath := filepath.Join(beadsDir, beads.CanonicalDatabaseName)
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	return alreadyInitialized(`
%s Found existing database: %s

This workspace is already initialized.

To use the existing database:
  Just run bd commands normally (e.g., %s)

If the database is genuinely corrupt and unrecoverable:
  bd export > issue-export.jsonl        # Export issue records first
  bd init --reinit-local --prefix %s    # Then reinitialize

Aborting.`, ui.RenderWarn("⚠"), dbPath, ui.RenderAccent("bd list"), prefix)
}

// countExistingIssues attempts to connect to the existing database and count
// issues. Returns 0 if the database is unreachable or empty. Used by --force
// safeguard to show users what they're about to destroy.
func countExistingIssues(_ string) (int, error) {
	var beadsDir string
	if envBeadsDir := os.Getenv("BEADS_DIR"); envBeadsDir != "" {
		beadsDir = utils.CanonicalizePath(envBeadsDir)
	} else {
		beadsDir = beads.FindBeadsDir()
		if beadsDir == "" {
			return 0, nil
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	store, err := newDoltStoreFromConfig(ctx, beadsDir)
	if err != nil {
		return 0, err
	}
	defer func() { _ = store.Close() }()

	stats, err := store.GetStatistics(ctx)
	if err != nil {
		return 0, err
	}
	if stats == nil {
		return 0, nil
	}
	return stats.TotalIssues, nil
}

// checkExistingBeadsData checks for existing database files
// and returns an error if found (safety guard for bd-emg)
//
// Note: This only blocks when a database already exists (workspace is initialized).
// Fresh clones without a database are allowed — init will create the database.
//
// For worktrees, checks the main repository root instead of current directory
// since worktrees should share the database with the main repository.
//
// For redirects, checks the redirect target and errors if it already has a database.
// This prevents accidentally overwriting an existing canonical database (GH#bd-0qel).
// normalizeIssuePrefix applies the prefix normalization rules shared by the
// init path and the --init-if-missing mismatch guard: strip leading dots, drop
// the trailing hyphen, convert dots to underscores, and prepend "bd_" when the
// result would not start with a SQL-identifier-safe character. Keeping this in
// one place ensures the mismatch check derives exactly the same name init does.
func normalizeIssuePrefix(prefix string) string {
	prefix = strings.TrimLeft(prefix, ".")
	prefix = strings.TrimRight(prefix, "-")
	prefix = strings.ReplaceAll(prefix, ".", "_")
	if len(prefix) > 0 && !((prefix[0] >= 'a' && prefix[0] <= 'z') || (prefix[0] >= 'A' && prefix[0] <= 'Z') || prefix[0] == '_') {
		prefix = "bd_" + prefix
	}
	return prefix
}

// dbNameFromPrefix derives the Dolt database name from a normalized issue
// prefix (hyphens become underscores), matching how init records DoltDatabase.
func dbNameFromPrefix(prefix string) string {
	return strings.ReplaceAll(prefix, "-", "_")
}

// initIfMissingPrefixMismatch reports whether an explicit --prefix request
// conflicts with the existing workspace, in which case --init-if-missing must
// abort rather than silently skip (otherwise the requested prefix is ignored).
// existingDBName is the existing workspace's recorded Dolt database name;
// requestedPrefix is the raw, un-normalized --prefix value. Returns false when
// the existing name is unknown so an undeterminable state falls through to the
// benign skip.
func initIfMissingPrefixMismatch(existingDBName, requestedPrefix string) bool {
	if existingDBName == "" {
		return false
	}
	requested := dbNameFromPrefix(normalizeIssuePrefix(requestedPrefix))
	return requested != "" && !strings.EqualFold(existingDBName, requested)
}

// initIfMissingDatabaseMismatch reports whether an explicit --database request
// conflicts with the existing workspace. --database is the authoritative database
// selector (it overrides prefix-based naming later in init), so an explicit
// mismatch must abort --init-if-missing rather than silently reuse a different
// database. existingDBName is the existing workspace's recorded Dolt database
// name; requestedDatabase is the raw --database value. Returns false when either
// is unknown so an undeterminable state falls through to the benign skip.
func initIfMissingDatabaseMismatch(existingDBName, requestedDatabase string) bool {
	if existingDBName == "" || requestedDatabase == "" {
		return false
	}
	return !strings.EqualFold(existingDBName, requestedDatabase)
}

// resolveInitBeadsDir resolves the .beads directory that init would target,
// using the same precedence as checkExistingBeadsData (BEADS_DIR > worktree
// fallback > CWD). Returns "" when it cannot be determined.
func resolveInitBeadsDir() string {
	if envBeadsDir := os.Getenv("BEADS_DIR"); envBeadsDir != "" {
		return utils.CanonicalizePath(envBeadsDir)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if isGitRepo() && git.IsWorktree() {
		return beads.GetWorktreeFallbackBeadsDir()
	}
	return filepath.Join(cwd, ".beads")
}

// existingWorkspaceDBName returns the Dolt database name explicitly recorded
// for the already-initialized workspace at the init target, or "" if it cannot
// be determined. It reads the raw DoltDatabase field rather than
// GetDoltDatabase() on purpose: the getter falls back to a default name (and an
// env override), which would manufacture a phantom value and trigger false
// mismatches. An unset value safely falls through to the benign skip.
func existingWorkspaceDBName() string {
	beadsDir := resolveInitBeadsDir()
	if beadsDir == "" {
		return ""
	}
	cfg, err := configfile.LoadForDiscovery(beadsDir)
	if err != nil || cfg == nil {
		return ""
	}
	return cfg.DoltDatabase
}

// existingWorkspaceDoltMode returns the connection mode recorded in the
// already-initialized workspace's metadata.json, or "" if there is none.
//
// Like existingWorkspaceDBName it reads the raw field rather than a getter: a
// getter that defaults to embedded would make "no recorded mode" and
// "deliberately embedded" indistinguishable, and this value is used to decide
// whether a re-init is about to change modes.
func existingWorkspaceDoltMode() string {
	beadsDir := resolveInitBeadsDir()
	if beadsDir == "" {
		return ""
	}
	cfg, err := configfile.LoadForDiscovery(beadsDir)
	if err != nil || cfg == nil {
		return ""
	}
	// Matched case-insensitively by callers, mirroring configfile's own
	// IsServerMode/IsProxiedServerMode comparisons.
	return strings.ToLower(strings.TrimSpace(cfg.DoltMode))
}

// inheritWorkspaceDoltMode is the decision half of #3885's fix: what a bare
// re-init adopts from the workspace it is re-initializing. It returns
// inheritServer=true when the workspace records server mode.
//
// Proxied-server is deliberately an error, not an inheritance: the dedicated
// proxied init path (runInitProxiedServer) dispatches on the explicit flag
// BEFORE the inheritance point in RunE, so inheriting by mutating only the
// process-mode globals would build the database embedded while metadata.json
// kept claiming proxied-server — the exact silent mismatch inheritance exists
// to prevent. The mode is experimental and dark-launched; a re-init of such a
// workspace must name it explicitly so it routes through the real init path.
func inheritWorkspaceDoltMode() (bool, error) {
	switch existingWorkspaceDoltMode() {
	case configfile.DoltModeServer:
		return true, nil
	case configfile.DoltModeProxiedServer:
		return false, fmt.Errorf("this workspace is recorded as proxied-server in .beads/metadata.json; " +
			"re-run with --proxied-server to keep it, or name another mode explicitly to change it")
	default:
		return false, nil
	}
}

// initModeExplicitlyRequested reports whether this invocation names a
// connection mode of its own, as opposed to falling back to the build default.
//
// Only an explicit request may change an existing workspace's mode. Without
// this distinction a bare `bd init --from-jsonl --reinit-local` on a
// server-mode project runs embedded and rewrites dolt_mode to embedded, which
// is #3885: the project comes back half-configured and the user is told
// nothing.
func initModeExplicitlyRequested(cmd *cobra.Command) bool {
	for _, name := range []string{"server", "shared-server", "proxied-server"} {
		if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
			return true
		}
	}
	for _, env := range []string{
		"BEADS_DOLT_SERVER_MODE",
		"BEADS_DOLT_SHARED_SERVER",
		"BEADS_DOLT_PROXIED_SERVER",
	} {
		if os.Getenv(env) != "" {
			return true
		}
	}
	// A global config.yaml `dolt.mode` is a deliberate statement about this
	// machine, so it counts as explicit too — the seeding block above already
	// treats it like --server.
	return config.GetYamlConfig("dolt.mode") != ""
}

func checkExistingBeadsData(prefix string) error {
	beadsDir := resolveInitBeadsDir()
	if beadsDir == "" {
		return nil // Can't determine target, allow init to proceed
	}
	return checkExistingBeadsDataAt(beadsDir, prefix)
}

// isNonInteractiveInit returns true if init should run without interactive prompts.
// Precedence: explicit flag > BD_NON_INTERACTIVE env > CI env > terminal detection.
// Setting BD_NON_INTERACTIVE=0 or BD_NON_INTERACTIVE=false explicitly forces
// interactive mode, overriding CI detection and terminal checks.
