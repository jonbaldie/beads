package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jonbaldie/beads/cmd/bd/doctor/fix"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/git"
	"github.com/jonbaldie/beads/internal/types"
)

// CheckStaleLegacyHooks detects *.legacy sidecar hooks (created by Python's pre-commit
// framework) that still call the removed "bd hook" command. These cause "unknown command"
// errors at runtime even though "bd hooks list" and "bd doctor" show green. (GH#2398)
func CheckStaleLegacyHooks() DoctorCheck {
	hooksDir, err := git.GetGitHooksDir()
	if err != nil {
		return DoctorCheck{
			Name:    "Stale Legacy Hooks",
			Status:  StatusOK,
			Message: "N/A (not a git repository)",
		}
	}

	entries, err := os.ReadDir(hooksDir)
	if err != nil {
		return DoctorCheck{
			Name:    "Stale Legacy Hooks",
			Status:  StatusOK,
			Message: "N/A (cannot read hooks directory)",
		}
	}

	var staleFiles []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".legacy") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(hooksDir, name)) //nolint:gosec // G304: name comes from ReadDir of the resolved Git hooks directory.
		if err != nil {
			continue
		}
		if staleBdHookPattern.Match(content) {
			staleFiles = append(staleFiles, name)
		}
	}

	if len(staleFiles) == 0 {
		return DoctorCheck{
			Name:    "Stale Legacy Hooks",
			Status:  StatusOK,
			Message: "No stale legacy hook sidecars",
		}
	}

	return DoctorCheck{
		Name:    "Stale Legacy Hooks",
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d stale .legacy hook(s) calling removed 'bd hook' command", len(staleFiles)),
		Detail:  fmt.Sprintf("Files: %s", strings.Join(staleFiles, ", ")),
		Fix:     "Remove or update these files: rm " + strings.Join(staleFilePaths(hooksDir, staleFiles), " "),
	}
}

func staleFilePaths(hooksDir string, names []string) []string {
	paths := make([]string, len(names))
	for i, name := range names {
		paths[i] = filepath.Join(hooksDir, name)
	}
	return paths
}

// CheckHooksPath detects a dangling core.hooksPath: a git config value that
// still points at a hooks directory which no longer exists. This happens
// when a user manually deletes .beads/ (e.g. `rm -rf .beads/`) without
// running `bd hooks uninstall` first — git keeps looking for the missing
// directory, and beads' post-checkout import can recreate a stray .beads/
// workspace as a side effect (GH#4440).
func CheckHooksPath() DoctorCheck {
	const name = "Hooks Path"

	repoRoot, _ := git.GetMainRepoRoot()
	if repoRoot == "" {
		repoRoot = git.GetRepoRoot()
	}
	if repoRoot == "" {
		return DoctorCheck{
			Name:    name,
			Status:  StatusOK,
			Message: "N/A (not a git repository)",
		}
	}

	hooksPath, ok := getConfiguredHooksPath(repoRoot)
	if !ok || hooksPath == "" {
		return DoctorCheck{
			Name:    name,
			Status:  StatusOK,
			Message: "core.hooksPath is not set",
		}
	}

	resolved := expandHookPathTilde(hooksPath)
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(repoRoot, resolved)
	}

	if fileExists(resolved) {
		return DoctorCheck{
			Name:    name,
			Status:  StatusOK,
			Message: fmt.Sprintf("core.hooksPath is set to %s", hooksPath),
		}
	}

	managed := IsBeadsManagedHooksPath(repoRoot, hooksPath)
	var fix string
	if managed {
		fix = "Run 'bd doctor --fix' to unset core.hooksPath, or: git config --unset core.hooksPath"
	} else {
		fix = "core.hooksPath is not beads-managed; bd will not change it. Restore the missing directory, or unset it yourself: git config --unset core.hooksPath"
	}

	return DoctorCheck{
		Name:    name,
		Status:  StatusWarning,
		Message: "core.hooksPath points at a missing directory",
		Detail:  fmt.Sprintf("core.hooksPath=%s (resolved: %s)", hooksPath, resolved),
		Fix:     fix,
	}
}

// FixHooksPath unsets core.hooksPath ONLY when it is beads-managed and its
// target is missing. It re-reads the current value and re-checks both
// conditions immediately before acting, so it never mutates a third-party
// hooksPath (e.g. husky) even if the check that triggered the fix pass is
// stale.
func FixHooksPath() error {
	repoRoot, _ := git.GetMainRepoRoot()
	if repoRoot == "" {
		repoRoot = git.GetRepoRoot()
	}
	if repoRoot == "" {
		return nil // not in a git repo
	}

	hooksPath, ok := getConfiguredHooksPath(repoRoot)
	if !ok || hooksPath == "" {
		return nil // nothing set
	}

	if !IsBeadsManagedHooksPath(repoRoot, hooksPath) {
		return nil // never touch a third-party hooksPath
	}

	resolved := expandHookPathTilde(hooksPath)
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(repoRoot, resolved)
	}
	if fileExists(resolved) {
		return nil // target exists — nothing dangling to fix
	}

	cmd := exec.Command("git", "config", "--unset", "core.hooksPath")
	cmd.Dir = repoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git config --unset core.hooksPath failed: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// getConfiguredHooksPath reads the raw core.hooksPath value from repoRoot.
// The bool return is false when core.hooksPath is not set at all.
func getConfiguredHooksPath(repoRoot string) (string, bool) {
	cmd := exec.Command("git", "config", "--get", "core.hooksPath")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// IsBeadsManagedHooksPath reports whether hooksPath is one of the values bd
// itself writes to core.hooksPath (.beads/hooks or .beads-hooks, relative or
// absolute under repoRoot). It is the single matcher for that question;
// resetHooksPathIfBeadsManaged (cmd/bd/hooks.go) calls it too, so the two
// paths cannot drift apart.
//
// Absolute values are compared after symlink resolution. repoRoot comes from
// git.GetMainRepoRoot, which canonicalizes the path, while the configured
// core.hooksPath is whatever string the user or an older bd wrote. A raw
// string compare therefore misses a genuinely beads-managed path any time the
// repo is reached through a symlink — on macOS every repo under /var (which
// is a symlink to /private/var, including all of t.TempDir()) hits this. The
// false negative is not cosmetic: CheckHooksPath then reports the dangling
// path as third-party and FixHooksPath deliberately refuses to unset it.
func IsBeadsManagedHooksPath(repoRoot, hooksPath string) bool {
	if hooksPath == ".beads/hooks" || hooksPath == ".beads-hooks" {
		return true
	}
	if !filepath.IsAbs(hooksPath) {
		return false
	}
	root := resolveExistingPrefix(repoRoot)
	candidate := resolveExistingPrefix(hooksPath)
	return candidate == filepath.Join(root, ".beads", "hooks") ||
		candidate == filepath.Join(root, ".beads-hooks")
}

// resolveExistingPrefix returns path with its longest existing ancestor
// replaced by that ancestor's symlink-resolved form, leaving any missing
// trailing components untouched.
//
// filepath.EvalSymlinks alone is not enough here: the whole point of the
// dangling-hooksPath check is that the leaf does not exist, so EvalSymlinks
// on the full path fails and returns nothing usable. Walking up to the
// deepest component that does exist still canonicalizes the symlinked prefix
// (/var -> /private/var), which is where the divergence lives.
func resolveExistingPrefix(path string) string {
	if path == "" || !filepath.IsAbs(path) {
		return path
	}
	current := filepath.Clean(path)
	var trailing string
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(resolved, trailing)
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Reached the filesystem root without finding an existing
			// component; nothing to canonicalize against.
			return filepath.Clean(path)
		}
		trailing = filepath.Join(filepath.Base(current), trailing)
		current = parent
	}
}

// CheckGitHooksDoltCompatibility checks if installed git hooks are compatible with Dolt backend.
// Hooks installed before Dolt support was added don't have the backend check and will
// fail with confusing errors on git pull/commit.
func CheckGitHooksDoltCompatibility(path string) DoctorCheck {
	backend, _ := getBackendAndBeadsDir(path)

	// Only relevant for Dolt backend
	if backend != configfile.BackendDolt {
		return DoctorCheck{
			Name:    "Git Hooks Dolt Compatibility",
			Status:  StatusOK,
			Message: "N/A (not using Dolt backend)",
		}
	}
	return inspectDoltHookCompatibility()
}

func inspectDoltHookCompatibility() DoctorCheck {
	// Check if we're in a git repository
	hooksDir, err := git.GetGitHooksDir()
	if err != nil {
		return DoctorCheck{
			Name:    "Git Hooks Dolt Compatibility",
			Status:  StatusOK,
			Message: "N/A (not a git repository)",
		}
	}

	// Check post-merge hook (most likely to cause issues with Dolt)
	postMergePath := filepath.Join(hooksDir, "post-merge")
	content, err := os.ReadFile(postMergePath) //nolint:gosec // G304: fixed hook name beneath Git's resolved hooks directory.
	if err != nil {
		// No hook installed - that's fine
		return DoctorCheck{
			Name:    "Git Hooks Dolt Compatibility",
			Status:  StatusOK,
			Message: "N/A (no post-merge hook installed)",
		}
	}

	return classifyDoltHookCompatibility(string(content))
}

func classifyDoltHookCompatibility(contentStr string) DoctorCheck {
	// Section-marker hooks (GH#1380) delegate to 'bd hooks run' which handles Dolt correctly
	if strings.Contains(contentStr, bdSectionMarkerPrefix) {
		return DoctorCheck{
			Name:    "Git Hooks Dolt Compatibility",
			Status:  StatusOK,
			Message: "Marker-managed hooks (Dolt handled by bd hooks run)",
		}
	}

	// Shim hooks (bd-shim) delegate to 'bd hook' which handles Dolt correctly
	if strings.Contains(contentStr, bdShimMarker) {
		return DoctorCheck{
			Name:    "Git Hooks Dolt Compatibility",
			Status:  StatusOK,
			Message: "Shim hooks (Dolt handled by bd hook command)",
		}
	}

	// Check if it's a bd inline hook
	if !strings.Contains(contentStr, bdInlineHookMarker) && !strings.Contains(contentStr, "bd") {
		return DoctorCheck{
			Name:    "Git Hooks Dolt Compatibility",
			Status:  StatusOK,
			Message: "N/A (not a bd hook)",
		}
	}

	// Check if inline hook has the Dolt backend skip logic
	if strings.Contains(contentStr, `"backend"`) && strings.Contains(contentStr, `"dolt"`) {
		return DoctorCheck{
			Name:    "Git Hooks Dolt Compatibility",
			Status:  StatusOK,
			Message: "Inline hooks have Dolt backend check",
		}
	}

	// Hook exists but lacks Dolt check - this will cause errors
	return DoctorCheck{
		Name:    "Git Hooks Dolt Compatibility",
		Status:  StatusError,
		Message: "Git hooks incompatible with Dolt backend",
		Detail:  "Installed hooks are outdated and incompatible with the Dolt backend.",
		Fix:     "Run 'bd hooks install --force' to update hooks for Dolt compatibility",
	}
}

// fixGitHooks fixes missing or broken git hooks by calling bd hooks install.
func fixGitHooks(path string) error {
	return fix.GitHooks(path)
}

// FindOrphanedIssues identifies issues referenced in git commits but still open in the database.
// This is the shared core logic used by both 'bd orphans' and 'bd doctor' commands.
// Returns empty slice if not a git repo, no issues from provider, or no orphans found (no error).
//
// Parameters:
//   - gitPath: The directory to scan for git commits
//   - provider: The issue provider to get open issues and prefix from
func FindOrphanedIssues(gitPath string, provider types.IssueProvider) ([]OrphanIssue, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
	defer cancel()

	// Skip if not in a git repo
	if !isGitRepository(ctx, gitPath) {
		return []OrphanIssue{}, nil // Not a git repo, return empty list
	}

	// Get issue prefix from provider
	issuePrefix := provider.GetIssuePrefix()

	// Get all open/in_progress issues from provider
	issues, err := provider.GetOpenIssues(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting open issues: %w", err)
	}

	openIssues := buildOpenOrphanIssues(issues)

	if len(openIssues) == 0 {
		return []OrphanIssue{}, nil
	}

	output, err := readGitLog(ctx, gitPath)
	if err != nil {
		return nil, fmt.Errorf("reading git log: %w", err)
	}
	return orphanedIssuesFromGitLog(openIssues, issuePrefix, output), nil
}

func buildOpenOrphanIssues(issues []*types.Issue) map[string]*OrphanIssue {
	openIssues := make(map[string]*OrphanIssue)
	for _, issue := range issues {
		openIssues[issue.ID] = &OrphanIssue{
			IssueID: issue.ID,
			Title:   issue.Title,
			Status:  string(issue.Status),
		}
	}
	return openIssues
}

func readGitLog(ctx context.Context, gitPath string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "log", "--oneline", "--all")
	cmd.Dir = gitPath
	return cmd.Output()
}

func orphanedIssuesFromGitLog(openIssues map[string]*OrphanIssue, issuePrefix string, output []byte) []OrphanIssue {
	// Match pattern like (bd-xxx) or (bd-xxx.1) including hierarchical IDs
	pattern := fmt.Sprintf(`\(%s-[a-z0-9.]+\)`, regexp.QuoteMeta(issuePrefix))
	re := regexp.MustCompile(pattern)
	var orphanedIssues []OrphanIssue
	lines := strings.Split(string(output), "\n")

	for _, line := range lines {
		recordOrphanedCommit(line, re, openIssues)
	}

	// Collect issues with commit references
	for _, orphan := range openIssues {
		if orphan.LatestCommit != "" {
			orphanedIssues = append(orphanedIssues, *orphan)
		}
	}

	return orphanedIssues
}

func recordOrphanedCommit(line string, re *regexp.Regexp, openIssues map[string]*OrphanIssue) {
	if line == "" {
		return
	}
	parts := strings.SplitN(line, " ", 2)
	if len(parts) < 1 {
		return
	}
	commitHash := parts[0]
	commitMsg := ""
	if len(parts) > 1 {
		commitMsg = parts[1]
	}
	for _, match := range re.FindAllString(line, -1) {
		issueID := strings.Trim(match, "()")
		orphan, exists := openIssues[issueID]
		if exists && orphan.LatestCommit == "" {
			// Only record first (most recent) commit per issue.
			orphan.LatestCommit = commitHash
			orphan.LatestCommitMessage = commitMsg
		}
	}
}

// CheckOrphanedIssues detects issues referenced in git commits but still open.
// This catches cases where someone implemented a fix with "(bd-xxx)" in the commit
// message but forgot to run "bd close".
func CheckOrphanedIssues(_ string) DoctorCheck {
	// Orphaned issue detection requires a local database provider which was removed
	// during the Dolt-only migration. This check is disabled until reimplemented
	// against the Dolt store.
	return DoctorCheck{
		Name:     "Orphaned Issues",
		Status:   StatusOK,
		Message:  "N/A (not yet implemented for Dolt backend)",
		Category: CategoryGit,
	}
}
