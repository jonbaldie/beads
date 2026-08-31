package doctor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
)

// CheckAgentDocDivergence detects when AGENTS.md and CLAUDE.md exist as
// independent regular files whose user-authored regions (everything outside
// the BEGIN/END BEADS INTEGRATION markers) have drifted apart. Hand-edits to
// only one of the pair are a common source of inconsistency; the warning
// recommends symlinking, regenerating via bd setup, or reconciling manually.
//
// Skipped when:
//   - Either file is missing
//   - The files share an inode (hardlink) or one is a symlink to the other
//   - The user-authored content matches after normalization
func CheckAgentDocDivergence(repoPath string) DoctorCheck {
	pair, check, skip := inspectAgentDocPair(repoPath)
	if skip {
		return check
	}
	return compareAgentDocPair(pair)
}

type agentDocPair struct {
	file   string
	agents string
	claude string
}

func inspectAgentDocPair(repoPath string) (agentDocPair, DoctorCheck, bool) {
	agentsFile := config.SafeAgentsFile()
	agentsPath := filepath.Join(repoPath, agentsFile)
	claudePath := filepath.Join(repoPath, "CLAUDE.md")
	if check, skip := skipLinkedAgentDocs(agentsFile, agentsPath, claudePath); skip {
		return agentDocPair{}, check, true
	}
	agentsContent, err := os.ReadFile(agentsPath) // #nosec G304 - path under repoPath
	if err != nil {
		return agentDocPair{}, DoctorCheck{
			Name:    "Agent Doc Divergence",
			Status:  StatusOK,
			Message: fmt.Sprintf("Cannot read %s: %v", agentsFile, err),
		}, true
	}
	claudeContent, err := os.ReadFile(claudePath) // #nosec G304 - path under repoPath
	if err != nil {
		return agentDocPair{}, DoctorCheck{
			Name:    "Agent Doc Divergence",
			Status:  StatusOK,
			Message: fmt.Sprintf("Cannot read CLAUDE.md: %v", err),
		}, true
	}
	return agentDocPair{file: agentsFile, agents: string(agentsContent), claude: string(claudeContent)}, DoctorCheck{}, false
}

func skipLinkedAgentDocs(agentsFile, agentsPath, claudePath string) (DoctorCheck, bool) {
	agentsInfo, errA := os.Lstat(agentsPath)
	claudeInfo, errB := os.Lstat(claudePath)
	if errA != nil || errB != nil {
		return DoctorCheck{
			Name:    "Agent Doc Divergence",
			Status:  StatusOK,
			Message: "N/A (one or both files missing)",
		}, true
	}
	if agentsInfo.Mode()&os.ModeSymlink != 0 || claudeInfo.Mode()&os.ModeSymlink != 0 {
		return DoctorCheck{
			Name:    "Agent Doc Divergence",
			Status:  StatusOK,
			Message: fmt.Sprintf("%s and CLAUDE.md are linked", agentsFile),
		}, true
	}
	if same, err := sameInode(agentsPath, claudePath); err == nil && same {
		return DoctorCheck{
			Name:    "Agent Doc Divergence",
			Status:  StatusOK,
			Message: fmt.Sprintf("%s and CLAUDE.md share an inode", agentsFile),
		}, true
	}
	return DoctorCheck{}, false
}

func compareAgentDocPair(pair agentDocPair) DoctorCheck {
	const optOutMarker = "<!-- bd-doctor-divergence: ok -->"
	if strings.Contains(pair.agents, optOutMarker) || strings.Contains(pair.claude, optOutMarker) {
		return DoctorCheck{
			Name:    "Agent Doc Divergence",
			Status:  StatusOK,
			Message: "Divergence check opted out via marker",
		}
	}
	if normalizeUserAuthored(pair.agents) == normalizeUserAuthored(pair.claude) {
		return DoctorCheck{
			Name:    "Agent Doc Divergence",
			Status:  StatusOK,
			Message: fmt.Sprintf("%s and CLAUDE.md user-authored content matches", pair.file),
		}
	}
	return DoctorCheck{
		Name:    "Agent Doc Divergence",
		Status:  StatusWarning,
		Message: fmt.Sprintf("%s and CLAUDE.md user-authored content has diverged", pair.file),
		Detail: "Both files exist as independent regular files (not symlinked, different inodes),\n" +
			"  but their content outside the <!-- BEGIN/END BEADS INTEGRATION --> markers differs.\n" +
			"  Hand-edits to only one of the pair are a common cause.",
		Fix: "Reconcile the two files using one of:\n" +
			"\n" +
			"  (a) Symlink one to the other so future edits stay in sync:\n" +
			"      ln -sf " + pair.file + " CLAUDE.md\n" +
			"\n" +
			"  (b) Regenerate the managed sections (preserves user-authored content\n" +
			"      from AGENTS.md as the source of truth):\n" +
			"      bd setup claude && bd setup codex\n" +
			"\n" +
			"  (c) Reconcile manually — diff the files and copy the intended\n" +
			"      user-authored content into both:\n" +
			"      diff " + pair.file + " CLAUDE.md\n" +
			"\n" +
			"  (d) If the divergence is intentional (e.g. distinct audiences for\n" +
			"      each file), opt out by adding this HTML comment anywhere in\n" +
			"      either file:\n" +
			"      <!-- bd-doctor-divergence: ok -->",
	}
}

// CheckDatabaseConfig verifies that the configured database path matches what
// actually exists on disk. For Dolt backends, data is on the server. For legacy
// backends, this checks that .db files match the configuration.
func CheckDatabaseConfig(repoPath string) DoctorCheck {
	beadsDir := ResolveBeadsDirForRepo(repoPath)
	cfg, check, skip := loadDoctorDatabaseConfig(beadsDir)
	if skip {
		return check
	}
	return reportDatabaseConfig(findDatabaseConfigMismatches(beadsDir, cfg))
}

func loadDoctorDatabaseConfig(beadsDir string) (*configfile.Config, DoctorCheck, bool) {
	cfg, err := configfile.Load(beadsDir)
	if err != nil || cfg == nil {
		return nil, DoctorCheck{
			Name:    "Database Config",
			Status:  StatusOK,
			Message: "Using default configuration",
		}, true
	}
	if cfg.GetBackend() == configfile.BackendDolt {
		return nil, DoctorCheck{
			Name:    "Database Config",
			Status:  StatusOK,
			Message: "Dolt backend (data on server)",
		}, true
	}
	return cfg, DoctorCheck{}, false
}

func findDatabaseConfigMismatches(beadsDir string, cfg *configfile.Config) []string {
	if cfg.Database == "" {
		return nil
	}
	if _, err := os.Stat(cfg.DatabasePath(beadsDir)); !os.IsNotExist(err) {
		return nil
	}
	otherDBs := listLegacyDBFiles(beadsDir)
	if len(otherDBs) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("Configured database '%s' not found, but found: %s", cfg.Database, strings.Join(otherDBs, ", "))}
}

func listLegacyDBFiles(beadsDir string) []string {
	entries, _ := os.ReadDir(beadsDir) // Best effort: nil entries means no legacy files to check
	var otherDBs []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".db") {
			otherDBs = append(otherDBs, entry.Name())
		}
	}
	return otherDBs
}

func reportDatabaseConfig(issues []string) DoctorCheck {
	if len(issues) == 0 {
		return DoctorCheck{
			Name:    "Database Config",
			Status:  StatusOK,
			Message: "Configuration matches existing files",
		}
	}
	return DoctorCheck{
		Name:    "Database Config",
		Status:  StatusWarning,
		Message: "Configuration mismatch detected",
		Detail:  strings.Join(issues, "\n  "),
		Fix: "Run 'bd doctor --fix' to auto-detect and fix mismatches, or manually:\n" +
			"  1. Check which files are actually being used\n" +
			"  2. Update metadata.json to match the actual filenames\n" +
			"  3. Or rename the files to match the configuration",
	}
}

// CheckFreshClone detects if this is a fresh clone that needs 'bd init'.
// A fresh clone has legacy JSONL with issues but no database (Dolt or SQLite).
func CheckFreshClone(repoPath string) DoctorCheck {
	backend, beadsDir := getBackendAndBeadsDir(repoPath)

	// Check if .beads/ exists
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		return DoctorCheck{
			Name:    "Fresh Clone",
			Status:  StatusOK,
			Message: "N/A (no .beads directory)",
		}
	}

	// Find the JSONL file
	var jsonlPath string
	var jsonlName string
	for _, name := range []string{"issues.jsonl", "beads.jsonl"} {
		testPath := filepath.Join(beadsDir, name)
		if _, err := os.Stat(testPath); err == nil {
			jsonlPath = testPath
			jsonlName = name
			break
		}
	}

	// No JSONL file - not a fresh clone situation
	if jsonlPath == "" {
		return DoctorCheck{
			Name:    "Fresh Clone",
			Status:  StatusOK,
			Message: "N/A (no JSONL file)",
		}
	}

	if result, handled := checkFreshCloneDatabase(backend, beadsDir); handled {
		return result
	}

	// Check if JSONL has any issues (empty JSONL = not really a fresh clone)
	issueCount, _ := countJSONLIssuesAndPrefix(jsonlPath)
	if issueCount == 0 {
		return DoctorCheck{
			Name:    "Fresh Clone",
			Status:  StatusOK,
			Message: fmt.Sprintf("JSONL exists but is empty (%s)", jsonlName),
		}
	}

	return freshCloneJSONLWithoutDatabase(issueCount, jsonlName)
}

func checkFreshCloneDatabase(backend, beadsDir string) (DoctorCheck, bool) {
	switch backend {
	case configfile.BackendDolt:
		return checkFreshCloneDoltDatabase(beadsDir)
	default:
		return checkFreshCloneLegacyDatabase(beadsDir)
	}
}

func checkFreshCloneDoltDatabase(beadsDir string) (DoctorCheck, bool) {
	if info, err := os.Stat(getDatabasePath(beadsDir)); err == nil && info.IsDir() {
		return DoctorCheck{
			Name:    "Fresh Clone",
			Status:  StatusOK,
			Message: "Database exists",
		}, true
	}
	cfg, err := configfile.Load(beadsDir)
	if err != nil || cfg == nil || !cfg.IsDoltServerMode() {
		return DoctorCheck{}, false
	}
	host := cfg.GetDoltServerHost()
	port := doltserver.DefaultConfig(beadsDir).Port
	user := cfg.GetDoltServerUser()
	password := cfg.GetDoltServerPasswordForPort(port)
	dbName := cfg.GetDoltDatabase()
	result := checkFreshCloneDB(host, port, user, password, dbName, cfg.GetDoltServerTLS())
	if result.Reachable {
		syncRemote := config.GetStringFromDir(beadsDir, "sync.remote")
		if syncRemote == "" {
			syncRemote = config.GetStringFromDir(beadsDir, "sync.git-remote")
		}
		return freshCloneServerResult(result.Exists, dbName, host, port, syncRemote), true
	}
	return freshCloneServerUnreachableResult(dbName, host, port, result.Err), true
}

func checkFreshCloneLegacyDatabase(beadsDir string) (DoctorCheck, bool) {
	var dbPath string
	if cfg, err := configfile.Load(beadsDir); err == nil && cfg != nil && cfg.Database != "" {
		dbPath = cfg.DatabasePath(beadsDir)
	} else {
		dbPath = filepath.Join(beadsDir, beads.CanonicalDatabaseName)
	}
	if _, err := os.Stat(dbPath); err == nil {
		return DoctorCheck{
			Name:    "Fresh Clone",
			Status:  StatusOK,
			Message: "Database exists",
		}, true
	}
	return DoctorCheck{}, false
}

func freshCloneJSONLWithoutDatabase(issueCount int, jsonlName string) DoctorCheck {
	fixCmd := "bd bootstrap"
	return DoctorCheck{
		Name:    "Fresh Clone",
		Status:  StatusWarning,
		Message: fmt.Sprintf("Fresh clone detected (%d issues in %s, no database)", issueCount, jsonlName),
		Detail: "This appears to be a freshly cloned repository.\n" +
			"  The JSONL file contains issues but no local database exists.\n" +
			"  Run 'bd bootstrap' as the safe entry point for recovering existing state.\n" +
			"  Use '--dry-run' first if you need to inspect whether bootstrap will recover or initialize.\n" +
			"  Use 'bd init' only when creating a brand-new project with no existing .beads data.",
		Fix: fmt.Sprintf("Run '%s' to recover the existing database and import tracked issues", fixCmd),
	}
}

// countJSONLIssuesAndPrefix counts issues in a legacy JSONL file and detects the most common prefix.
func countJSONLIssuesAndPrefix(jsonlPath string) (int, string) {
	file, err := os.Open(jsonlPath) //nolint:gosec
	if err != nil {
		return 0, ""
	}
	defer file.Close()

	count := 0
	prefixCounts := make(map[string]int)

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1024), 2*1024*1024) // 2MB buffer for large lines
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var issue struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(line, &issue); err != nil {
			continue
		}

		if issue.ID != "" {
			count++
			// Extract prefix (everything before the last dash)
			if lastDash := strings.LastIndex(issue.ID, "-"); lastDash > 0 {
				prefix := issue.ID[:lastDash]
				prefixCounts[prefix]++
			}
		}
	}

	// Find most common prefix
	var mostCommonPrefix string
	maxCount := 0
	for prefix, cnt := range prefixCounts {
		if cnt > maxCount {
			maxCount = cnt
			mostCommonPrefix = prefix
		}
	}

	return count, mostCommonPrefix
}
