package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"golang.org/x/term"
)

func detectBootstrapAction(beadsDir string, cfg *configfile.Config) BootstrapPlan {
	plan := BootstrapPlan{
		BeadsDir: beadsDir,
		Database: cfg.GetDoltDatabase(),
	}

	// When bootstrap synthesized a fallback beadsDir for a fresh clone or
	// worktree recovery, the path may not exist yet. In that case we must let
	// sync.remote / refs/dolt/data detection run before treating an existing
	// shared-server database as "nothing to do", otherwise an unrelated default
	// "beads" database can mask the real recovery path.
	beadsDirExists := isBootstrapDirectory(beadsDir)

	// Check for existing database (path differs between server and embedded mode).
	// Determine server/shared-server mode from the target workspace itself
	// (metadata.json, env vars, and the target config.yaml when present) rather
	// than unrelated global config loaded from the caller's current repo.
	isSharedServer := bootstrapSharedServerMode(beadsDir)
	isServer := cfg.IsDoltServerMode() || isSharedServer

	// Prefer an existing local database over re-clone (GH#5037). Previously
	// sync.remote returned Action=sync unconditionally, so a second
	// `bd bootstrap` on an already-cloned workspace ran DOLT_CLONE into the
	// existing dir and failed with Error 1007 (database exists) — contradicting
	// help text ("If database already exists: validates and reports status")
	// and the multi-clone upgrade guide. If the local beadsDir does not exist
	// yet, still prefer sync recovery first for Action=="none" so a default
	// shared-server "beads" DB from another project cannot mask a real clone.
	if dbAction, ok := existingBootstrapDBPlan(beadsDir, cfg, isServer, isSharedServer); ok {
		if beadsDirExists || dbAction.Action != "none" {
			return dbAction
		}
		plan = dbAction
	}

	if syncPlan, ok := configuredBootstrapSyncPlan(plan); ok {
		return syncPlan
	}
	if syncPlan, ok := autoDetectedBootstrapSyncPlan(plan); ok {
		return syncPlan
	}
	if backupPlan, ok := backupBootstrapPlan(plan); ok {
		return backupPlan
	}
	if jsonlPlan, ok := jsonlBootstrapPlan(plan); ok {
		return jsonlPlan
	}

	return completeBootstrapPlan(plan)
}

func completeBootstrapPlan(plan BootstrapPlan) BootstrapPlan {
	if plan.Action != "none" {
		// Fresh setup
		plan.Action = "init"
		plan.Reason = "No existing database, remote, or backup — will create fresh database"
	}
	return plan
}

func isBootstrapDirectory(beadsDir string) bool {
	info, err := os.Stat(beadsDir)
	return err == nil && info.IsDir()
}

func configuredBootstrapSyncPlan(plan BootstrapPlan) (BootstrapPlan, bool) {
	syncRemote := resolveSyncRemote()
	if syncRemote == "" {
		return plan, false
	}
	if isGitCodeRepoURL(syncRemote) {
		// Cloning from a git code-repo URL via DOLT_CLONE spins dolt to
		// 1000% CPU and requires manual SIGKILL. Reject and surface the
		// misconfiguration rather than attempting the clone.
		fmt.Fprintf(os.Stderr, "error: sync.remote %q looks like a git code-repository URL, not a Dolt remote — skipping clone\n", syncRemote)
		plan.Action = "none"
		plan.Reason = fmt.Sprintf("sync.remote %q rejected: git code-repository URL (not a Dolt remote)", syncRemote)
		return plan, true
	}
	// User-provided sync.remote — trust the URL format as-is.
	// normalizeRemoteURL would convert http:// to git+http://, breaking
	// Dolt remotesapi endpoints (GH#3339).
	plan.SyncRemote = syncRemote
	plan.Action = "sync"
	plan.Reason = "sync.remote configured — will clone from " + syncRemote
	return plan, true
}

func autoDetectedBootstrapSyncPlan(plan BootstrapPlan) (BootstrapPlan, bool) {
	// Auto-detect: probe git origin for Dolt data stored in git (refs/dolt/data).
	// This only applies to git remotes — Dolt-native remotes must be configured
	// via sync.remote.
	if !isGitRepo() || isBareGitRepo() {
		return plan, false
	}
	originURL, err := gitOriginGetURL()
	if err != nil || originURL == "" || !gitOriginHasDoltDataRef() {
		return plan, false
	}
	plan.SyncRemote = normalizeRemoteURL(originURL)
	plan.Action = "sync"
	plan.Reason = "Found Dolt data on git origin (refs/dolt/data) — will clone from " + originURL
	return plan, true
}

func backupBootstrapPlan(plan BootstrapPlan) (BootstrapPlan, bool) {
	backupDir := filepath.Join(plan.BeadsDir, "backup")
	issuesFile := filepath.Join(backupDir, "issues.jsonl")
	info, err := os.Stat(issuesFile)
	if err != nil || info.Size() == 0 {
		return plan, false
	}
	plan.BackupDir = backupDir
	plan.Action = "restore"
	plan.Reason = "Backup files found — will restore from " + backupDir
	return plan, true
}

func jsonlBootstrapPlan(plan BootstrapPlan) (BootstrapPlan, bool) {
	gitJSONL := filepath.Join(plan.BeadsDir, "issues.jsonl")
	if _, err := os.Stat(gitJSONL); err != nil {
		return plan, false
	}
	plan.JSONLFile = gitJSONL
	plan.Action = "jsonl-import"
	plan.Reason = "Git-tracked issues.jsonl found — will import from " + gitJSONL
	return plan, true
}

func existingBootstrapDBPlan(beadsDir string, cfg *configfile.Config, isServer, isSharedServer bool) (BootstrapPlan, bool) {
	plan := BootstrapPlan{
		BeadsDir: beadsDir,
		Database: cfg.GetDoltDatabase(),
	}

	dbPath := bootstrapDatabasePath(beadsDir, cfg, isServer, isSharedServer)
	if !isBootstrapDirectory(dbPath) {
		return BootstrapPlan{}, false
	}

	if !bootstrapDirectoryHasEntries(dbPath) {
		return BootstrapPlan{}, false
	}

	if isServer {
		return existingServerBootstrapDBPlan(plan, beadsDir, cfg, isSharedServer)
	}

	plan.HasExisting = true
	plan.Action = "none"
	plan.Reason = "Database already exists at " + dbPath
	return plan, true
}

func bootstrapDatabasePath(beadsDir string, cfg *configfile.Config, isServer, isSharedServer bool) string {
	if isServer {
		return bootstrapServerDoltDir(beadsDir, cfg, isSharedServer)
	}
	return filepath.Join(beadsDir, "embeddeddolt")
}

func bootstrapDirectoryHasEntries(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) > 0
}

func existingServerBootstrapDBPlan(plan BootstrapPlan, beadsDir string, cfg *configfile.Config, isSharedServer bool) (BootstrapPlan, bool) {
	probeCfg := bootstrapServerProbeConfig{
		host:     cfg.GetDoltServerHost(),
		port:     bootstrapServerPort(beadsDir, cfg, isSharedServer),
		user:     cfg.GetDoltServerUser(),
		pass:     cfg.GetDoltServerPassword(),
		database: cfg.GetDoltDatabase(),
		tls:      cfg.GetDoltServerTLS(),
	}
	result := probeBootstrapServerWithRetry(probeCfg, cfg.GetDoltDatabase())
	if result.Err != nil {
		plan.Action = "none"
		plan.Reason = fmt.Sprintf("Could not verify existing server database %s: %v", cfg.GetDoltDatabase(), result.Err)
		return plan, true
	}
	if result.Exists {
		plan.HasExisting = true
		plan.Action = "none"
		plan.Reason = fmt.Sprintf("Database %s already exists on server at %s:%d", probeCfg.database, probeCfg.host, probeCfg.port)
		return plan, true
	}
	return BootstrapPlan{}, false
}

func probeBootstrapServerWithRetry(probeCfg bootstrapServerProbeConfig, database string) bootstrapServerDBCheck {
	// When the server is reachable but the DB appears absent, retry with
	// exponential backoff before concluding the DB is genuinely missing.
	// A managed Dolt restart completes in <30 s; three retries over 70 s cover
	// all observed restart windows.
	retryDelays := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second}
	for attempt := 0; ; attempt++ {
		result := checkBootstrapServerDB(probeCfg)
		if result.Err != nil || result.Exists || !result.Reachable || attempt >= len(retryDelays) {
			return result
		}
		fmt.Fprintf(os.Stderr, "Database %s not found on reachable server (attempt %d/%d), retrying in %v (possible transient restart)\n",
			database, attempt+1, len(retryDelays), retryDelays[attempt])
		bootstrapRetryDelay(retryDelays[attempt])
	}
}

func bootstrapSharedServerMode(beadsDir string) bool {
	if v := os.Getenv("BEADS_DOLT_SHARED_SERVER"); v == "1" || strings.EqualFold(v, "true") {
		return true
	}
	return strings.EqualFold(config.GetStringFromDir(beadsDir, "dolt.shared-server"), "true")
}

func bootstrapServerDoltDir(beadsDir string, cfg *configfile.Config, isSharedServer bool) string {
	if isSharedServer {
		if dir, err := doltserver.SharedDoltDir(); err == nil {
			return dir
		}
	}

	if d := cfg.GetDoltDataDir(); d != "" {
		if filepath.IsAbs(d) {
			return d
		}
		return filepath.Join(beadsDir, d)
	}

	return filepath.Join(beadsDir, "dolt")
}

func bootstrapServerPort(beadsDir string, cfg *configfile.Config, isSharedServer bool) int {
	if isSharedServer {
		return sharedBootstrapServerPort()
	}
	return localBootstrapServerPort(beadsDir, cfg)
}

func sharedBootstrapServerPort() int {
	sharedDir, err := doltserver.SharedServerDir()
	if err == nil {
		if port := doltserver.ReadPortFile(sharedDir); port > 0 {
			return port
		}
	}
	return doltserver.DefaultSharedServerPort
}

func localBootstrapServerPort(beadsDir string, cfg *configfile.Config) int {
	if port := positivePortFromEnv("BEADS_DOLT_SERVER_PORT"); port > 0 {
		return port
	}
	if port := doltserver.ReadPortFile(beadsDir); port > 0 {
		return port
	}
	if port := positivePortFromConfig(beadsDir, "dolt.port"); port > 0 {
		return port
	}
	if cfg.DoltServerPort > 0 {
		return cfg.DoltServerPort
	}
	return configfile.DefaultDoltServerPort
}

func positivePortFromEnv(name string) int {
	return parsePositivePort(os.Getenv(name))
}

func positivePortFromConfig(beadsDir, key string) int {
	return parsePositivePort(config.GetStringFromDir(beadsDir, key))
}

func parsePositivePort(value string) int {
	port, err := strconv.Atoi(value)
	if err != nil || port <= 0 {
		return 0
	}
	return port
}

func printBootstrapPlan(plan BootstrapPlan) {
	switch plan.Action {
	case "none":
		fmt.Printf("✓ Database already exists: %s\n", plan.BeadsDir)
		if !usesSQLServer() {
			fmt.Printf("  Nothing to do.\n")
		} else {
			fmt.Printf("  Nothing to do. Use 'bd doctor' to check health.\n")
		}
	case "sync":
		fmt.Printf("Bootstrap plan: clone from remote\n")
		fmt.Printf("  Remote: %s\n", plan.SyncRemote)
		fmt.Printf("  Database: %s\n", plan.Database)
	case "restore":
		fmt.Printf("Bootstrap plan: restore from backup\n")
		fmt.Printf("  Backup dir: %s\n", plan.BackupDir)
	case "jsonl-import":
		fmt.Printf("Bootstrap plan: import from git-tracked JSONL\n")
		fmt.Printf("  JSONL file: %s\n", plan.JSONLFile)
		fmt.Printf("  Database: %s\n", plan.Database)
	case "init":
		fmt.Printf("Bootstrap plan: create fresh database\n")
		fmt.Printf("  Database: %s\n", plan.Database)
	}
}

// confirmPrompt asks the user to confirm an action. Returns true if
// nonInteractive is set, stdin is not a terminal, or the user confirms.
func confirmPrompt(message string, nonInteractive bool) bool {
	if nonInteractive {
		return true
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return true
	}
	fmt.Fprintf(os.Stderr, "%s [Y/n] ", message)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "" || line == "y" || line == "yes"
}
