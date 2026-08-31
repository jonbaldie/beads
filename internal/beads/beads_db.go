// Package beads provides a minimal public API for extending bd with custom orchestration.
//
// Most extensions should use direct SQL queries against bd's database.
// This package exports only the essential types and functions needed for
// Go-based extensions that want to use bd's storage layer programmatically.
//
// For a working extension example, see examples/bd-example-extension-go.
package beads

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/git"
	"github.com/jonbaldie/beads/internal/utils"
)

// DatabaseInfo contains information about a discovered beads database
type DatabaseInfo struct {
	Path       string // Full path to the .db file
	BeadsDir   string // Parent .beads directory
	IssueCount int    // Number of issues (-1 if unknown)
}

// findGitRoot returns the root directory of the current git repository,
// or empty string if not in a git repository. Used to limit directory
// tree walking to within the current git repo.
//
// This function delegates to git.GetRepoRoot() which is worktree-aware
// and handles Windows path normalization.
func findGitRoot() string {
	return git.GetRepoRoot()
}

// GetWorktreeFallbackBeadsDir returns the canonical shared .beads location for
// the current git worktree when no local redirect or worktree-local .beads is present.
func GetWorktreeFallbackBeadsDir() string {
	if !git.IsWorktree() {
		return ""
	}

	commonDir, err := git.GetGitCommonDir()
	if err != nil || commonDir == "" {
		return ""
	}

	commonDir = utils.CanonicalizePath(commonDir)
	if filepath.Base(commonDir) == ".git" {
		return filepath.Join(filepath.Dir(commonDir), ".beads")
	}

	return filepath.Join(commonDir, ".beads")
}

// ResolveBeadsDirForRepo returns the effective .beads directory for a repo path.
// It prefers a local .beads directory and otherwise falls back to the shared
// worktree location derived from git-common-dir.
//
// Unlike FindBeadsDir, this helper does not use BEADS_DIR and does not walk up
// from CWD. Callers that care about nested rig directories should resolve those
// before falling back to this repo-scoped helper.
func ResolveBeadsDirForRepo(repoPath string) string {
	repoPath = utils.CanonicalizePath(repoPath)
	localBeadsDir := filepath.Join(repoPath, ".beads")
	if info, err := os.Stat(localBeadsDir); err == nil && info.IsDir() {
		return FollowRedirect(localBeadsDir)
	}

	if fallback := worktreeFallbackBeadsDirForRepo(repoPath); fallback != "" {
		return FollowRedirect(fallback)
	}

	return FollowRedirect(localBeadsDir)
}

func worktreeFallbackBeadsDirForRepo(repoPath string) string {
	cmd := exec.Command("git", "-C", repoPath, "rev-parse", "--git-dir", "--git-common-dir")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 {
		return ""
	}

	gitDir := gitPathForRepo(repoPath, strings.TrimSpace(lines[0]))
	commonDir := gitPathForRepo(repoPath, strings.TrimSpace(lines[1]))
	if gitDir == "" || commonDir == "" || utils.PathsEqual(gitDir, commonDir) {
		return ""
	}

	if filepath.Base(commonDir) == ".git" {
		return filepath.Join(filepath.Dir(commonDir), ".beads")
	}

	return filepath.Join(commonDir, ".beads")
}

func gitPathForRepo(repoPath, path string) string {
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoPath, path)
	}
	return utils.CanonicalizePath(path)
}

// worktreeRedirectTarget returns the resolved redirect target for the current
// worktree's .beads/redirect file, or empty string if not in a worktree or no
// redirect exists. This centralizes the per-worktree redirect override logic
// used by findLocalBeadsDir, FindBeadsDir, and findDatabaseInTree.
func worktreeRedirectTarget() string {
	if !git.IsWorktree() {
		return ""
	}
	worktreeRoot := git.GetRepoRoot()
	if worktreeRoot == "" {
		return ""
	}
	worktreeBeadsDir := filepath.Join(worktreeRoot, ".beads")
	redirectFile := filepath.Join(worktreeBeadsDir, "redirect")
	if _, err := os.Stat(redirectFile); err != nil {
		return ""
	}
	target := FollowRedirect(worktreeBeadsDir)
	if target == worktreeBeadsDir {
		// Redirect file exists but FollowRedirect returned the original path
		// (empty/invalid content). Return the raw .beads dir so callers that
		// only need to know a redirect *exists* (findLocalBeadsDir) still work.
		return worktreeBeadsDir
	}
	return target
}

// findDatabaseInTree walks up the directory tree looking for .beads/*.db
// Stops at the git repository root to avoid finding unrelated databases.
// For worktrees, searches the main repository root first, then falls back to worktree.
// Prefers config.json, falls back to beads.db, and warns if multiple .db files exist.
// Redirect files are supported: if a .beads/redirect file exists, its contents
// are used as the actual .beads directory path.
func findDatabaseInTree() string {
	dir, ok := currentCanonicalDirectory()
	if !ok {
		return ""
	}
	isWorktree, jjSecondaryRoot, isJJSecondary := databaseWorkspaceInfo()
	if dbPath := findDatabaseInCwd(dir, isWorktree, jjSecondaryRoot, isJJSecondary); dbPath != "" {
		return dbPath
	}
	dbPath, mainRepoRoot := findWorkspaceDatabase(isWorktree, jjSecondaryRoot, isJJSecondary)
	if dbPath != "" {
		return dbPath
	}
	return walkDatabaseTree(dir, databaseSearchRoot(isWorktree, isJJSecondary, mainRepoRoot))
}

func currentCanonicalDirectory() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	return utils.CanonicalizePath(dir), true
}

func databaseWorkspaceInfo() (bool, string, bool) {
	isWorktree := git.IsWorktree()
	if isWorktree {
		return true, "", false
	}
	jjSecondaryRoot, isJJSecondary := git.JJSecondaryWorkspaceRoot()
	return false, jjSecondaryRoot, isJJSecondary
}

func findDatabaseInCwd(dir string, isWorktree bool, jjSecondaryRoot string, isJJSecondary bool) string {
	if skipDatabaseCwdCheck(dir, isWorktree, jjSecondaryRoot, isJJSecondary) {
		return ""
	}
	return findDatabaseAt(dir)
}

func skipDatabaseCwdCheck(dir string, isWorktree bool, jjSecondaryRoot string, isJJSecondary bool) bool {
	if isWorktree && dir == utils.CanonicalizePath(git.GetRepoRoot()) {
		return true
	}
	return isJJSecondary && dir == utils.CanonicalizePath(jjSecondaryRoot)
}

func findDatabaseAt(dir string) string {
	beadsDir := filepath.Join(dir, ".beads")
	if !isBeadsDirectory(beadsDir) {
		return ""
	}
	return findDatabaseInBeadsDir(FollowRedirect(beadsDir), true)
}

func findWorkspaceDatabase(isWorktree bool, jjSecondaryRoot string, isJJSecondary bool) (string, string) {
	if isWorktree {
		return findWorktreeDatabase()
	}
	if isJJSecondary {
		return findJJSecondaryDatabase(jjSecondaryRoot)
	}
	return "", ""
}

func findWorktreeDatabase() (string, string) {
	if dbPath := findWorktreeRedirectDatabase(); dbPath != "" {
		return dbPath, ""
	}
	if dbPath := findWorktreeOwnDatabase(); dbPath != "" {
		return dbPath, ""
	}
	if dbPath := findWorktreeFallbackDatabase(); dbPath != "" {
		return dbPath, ""
	}
	return "", mainRepoRootForDatabaseSearch()
}

func findWorktreeRedirectDatabase() string {
	target := worktreeRedirectTarget()
	if target == "" {
		return ""
	}
	return findDatabaseInBeadsDir(target, true)
}

func findWorktreeOwnDatabase() string {
	worktreeRoot := git.GetRepoRoot()
	if worktreeRoot == "" {
		return ""
	}
	worktreeBeadsDir := filepath.Join(worktreeRoot, ".beads")
	if !isBeadsDirectory(worktreeBeadsDir) || !hasBeadsDatabase(worktreeBeadsDir) {
		return ""
	}
	return findDatabaseInBeadsDir(worktreeBeadsDir, true)
}

func findWorktreeFallbackDatabase() string {
	fallbackBeadsDir := GetWorktreeFallbackBeadsDir()
	if fallbackBeadsDir == "" || !isBeadsDirectory(fallbackBeadsDir) {
		return ""
	}
	return findDatabaseInBeadsDir(FollowRedirect(fallbackBeadsDir), true)
}

func mainRepoRootForDatabaseSearch() string {
	mainRepoRoot, err := git.GetMainRepoRoot()
	if err != nil {
		return ""
	}
	return mainRepoRoot
}

func findJJSecondaryDatabase(jjSecondaryRoot string) (string, string) {
	if dbPath := findJJSecondaryOwnDatabase(jjSecondaryRoot); dbPath != "" {
		return dbPath, ""
	}
	jjPrimaryRoot, jjPrimaryErr := git.GetJJPrimaryWorkspaceRoot()
	if jjPrimaryErr != nil || jjPrimaryRoot == "" {
		return "", ""
	}
	primaryBeadsDir := filepath.Join(jjPrimaryRoot, ".beads")
	if isBeadsDirectory(primaryBeadsDir) {
		if dbPath := findDatabaseInBeadsDir(FollowRedirect(primaryBeadsDir), true); dbPath != "" {
			return dbPath, ""
		}
	}
	return "", jjPrimaryRoot
}

func findJJSecondaryOwnDatabase(jjSecondaryRoot string) string {
	if jjSecondaryRoot == "" {
		return ""
	}
	secondaryBeadsDir := filepath.Join(jjSecondaryRoot, ".beads")
	if !isBeadsDirectory(secondaryBeadsDir) || !hasBeadsDatabase(secondaryBeadsDir) {
		return ""
	}
	return findDatabaseInBeadsDir(secondaryBeadsDir, true)
}

func databaseSearchRoot(isWorktree, isJJSecondary bool, mainRepoRoot string) string {
	gitRoot := findGitRoot()
	if (isWorktree || isJJSecondary) && mainRepoRoot != "" {
		gitRoot = mainRepoRoot
	}
	if gitRoot == "" {
		return ""
	}
	return utils.CanonicalizePath(gitRoot)
}

func walkDatabaseTree(dir, gitRoot string) string {
	for {
		if dbPath := findDatabaseAt(dir); dbPath != "" {
			return dbPath
		}
		parent := filepath.Dir(dir)
		if parent == dir || (gitRoot != "" && dir == gitRoot) {
			break
		}
		dir = parent
	}
	return ""
}

// FindAllDatabases scans the directory hierarchy for the closest .beads directory.
// Returns a slice with at most one DatabaseInfo - the closest database to CWD.
// Stops searching upward as soon as a .beads directory is found,
// because in multi-workspace setups, nested .beads directories
// are intentional and separate - parent directories are out of scope.
// Redirect files are supported: if a .beads/redirect file exists, its contents
// are used as the actual .beads directory path.
func FindAllDatabases() []DatabaseInfo {
	databases := []DatabaseInfo{} // Initialize to empty slice, never return nil
	seen := make(map[string]bool) // Track canonical paths to avoid duplicates

	dir, err := os.Getwd()
	if err != nil {
		return databases
	}
	return findAllDatabasesFrom(dir, findGitRoot(), seen, databases)
}

func findAllDatabasesFrom(dir, gitRoot string, seen map[string]bool, databases []DatabaseInfo) []DatabaseInfo {
	for {
		if databaseInfo, ok := findDatabaseInfoAt(dir); ok {
			canonicalPath := canonicalDatabasePath(databaseInfo.Path)
			if seen[canonicalPath] {
				nextDir, ok := nextDatabaseDirectory(dir, "", false)
				if !ok {
					break
				}
				dir = nextDir
				continue
			}
			seen[canonicalPath] = true
			databases = append(databases, databaseInfo)
			break
		}

		nextDir, ok := nextDatabaseDirectory(dir, gitRoot, true)
		if !ok {
			break
		}
		dir = nextDir
	}

	return databases
}

func findDatabaseInfoAt(dir string) (DatabaseInfo, bool) {
	beadsDir := filepath.Join(dir, ".beads")
	if !isBeadsDirectory(beadsDir) {
		return DatabaseInfo{}, false
	}
	beadsDir = FollowRedirect(beadsDir)
	dbPath := databasePathInBeadsDir(beadsDir)
	if dbPath == "" {
		return DatabaseInfo{}, false
	}
	return DatabaseInfo{Path: dbPath, BeadsDir: beadsDir, IssueCount: -1}, true
}

func databasePathInBeadsDir(beadsDir string) string {
	doltDir := filepath.Join(beadsDir, "dolt")
	if isBeadsDirectory(doltDir) {
		return doltDir
	}
	matches, err := filepath.Glob(filepath.Join(beadsDir, "*.db"))
	if err == nil && len(matches) > 0 {
		return matches[0]
	}
	return ""
}

func canonicalDatabasePath(dbPath string) string {
	if resolved, err := filepath.EvalSymlinks(dbPath); err == nil {
		return resolved
	}
	return dbPath
}

func nextDatabaseDirectory(dir, gitRoot string, stopAtGitRoot bool) (string, bool) {
	parent := filepath.Dir(dir)
	if parent == dir || (stopAtGitRoot && gitRoot != "" && dir == gitRoot) {
		return "", false
	}
	return parent, true
}
