package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CheckRedirectNotTracked verifies that .beads/redirect is NOT tracked by git.
// Redirect files contain relative paths that only work in the original worktree.
// If committed, they cause warnings in other clones where the path is invalid.
// repoPath is the project root directory.
func CheckRedirectNotTracked(repoPath string) DoctorCheck {
	redirectPath := filepath.Join(repoPath, ".beads", "redirect")

	// First check if the file exists
	if _, err := os.Stat(redirectPath); os.IsNotExist(err) {
		// File doesn't exist - nothing to check
		return DoctorCheck{
			Name:    "Redirect Tracking",
			Status:  StatusOK,
			Message: "No redirect file present",
		}
	}

	// Check if git considers this file tracked
	// git ls-files exits 0 and outputs the filename if tracked, empty if untracked
	cmd := exec.Command("git", "ls-files", redirectPath) // #nosec G204 - args are hardcoded paths
	output, err := cmd.Output()
	if err != nil {
		// Not in a git repo or git error - skip check
		return DoctorCheck{
			Name:    "Redirect Tracking",
			Status:  StatusOK,
			Message: "N/A (not a git repository)",
		}
	}

	trackedPath := strings.TrimSpace(string(output))
	if trackedPath == "" {
		// File exists but is not tracked - this is correct
		return DoctorCheck{
			Name:    "Redirect Tracking",
			Status:  StatusOK,
			Message: "redirect file not tracked (correct)",
		}
	}

	// File is tracked - this is a problem
	return DoctorCheck{
		Name:    "Redirect Tracking",
		Status:  StatusWarning,
		Message: "redirect file is tracked by git",
		Detail:  "The .beads/redirect file contains a relative path that only works in this worktree. When committed, it causes warnings in other clones.",
		Fix:     "Run 'bd doctor --fix' to untrack, or manually: git rm --cached .beads/redirect",
	}
}

// FixRedirectTracking untracks the .beads/redirect file from git.
// repoPath is the project root directory.
func FixRedirectTracking(repoPath string) error {
	redirectPath := filepath.Join(repoPath, ".beads", "redirect")

	// Check if file is actually tracked first
	cmd := exec.Command("git", "ls-files", redirectPath) // #nosec G204 - args are hardcoded paths
	output, err := cmd.Output()
	if err != nil {
		return nil // Not a git repo, nothing to do
	}

	trackedPath := strings.TrimSpace(string(output))
	if trackedPath == "" {
		return nil // Not tracked, nothing to do
	}

	// Untrack the file (keeps the local copy)
	cmd = exec.Command("git", "rm", "--cached", redirectPath) // #nosec G204 - args are hardcoded paths
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to untrack redirect file: %w", err)
	}

	return nil
}

// parseRedirectTarget extracts the first non-comment, non-empty redirect target.
// It also strips a UTF-8 BOM if present.
func parseRedirectTarget(data []byte) string {
	content := strings.TrimSpace(string(data))
	if content == "" {
		return ""
	}

	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "\ufeff")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}

	return ""
}

// resolveRedirectTarget resolves a redirect target relative to the .beads parent.
// Absolute targets are cleaned and returned as-is.
func resolveRedirectTarget(beadsDir string, target string) string {
	if target == "" {
		return ""
	}

	resolvedTarget := target
	if !filepath.IsAbs(target) {
		projectRoot := filepath.Dir(beadsDir)
		resolvedTarget = filepath.Join(projectRoot, target)
	}
	resolvedTarget = filepath.Clean(resolvedTarget)
	if absPath, err := filepath.Abs(resolvedTarget); err == nil {
		resolvedTarget = absPath
	}

	return resolvedTarget
}

// CheckRedirectTargetValid verifies that the redirect target exists and has a valid beads database.
// This catches cases where the redirect points to a non-existent directory or one without a database.
// repoPath is the project root directory.
func CheckRedirectTargetValid(repoPath string) DoctorCheck {
	resolvedTarget, check, ok := loadRedirectTargetDir(repoPath)
	if !ok {
		return check
	}
	if check, ok := statRedirectTargetDir(resolvedTarget); !ok {
		return check
	}
	return checkRedirectHasDatabase(resolvedTarget)
}

func loadRedirectTargetDir(repoPath string) (string, DoctorCheck, bool) {
	redirectPath := filepath.Join(repoPath, ".beads", "redirect")
	data, err := os.ReadFile(redirectPath) // #nosec G304 - path is hardcoded
	if os.IsNotExist(err) {
		return "", DoctorCheck{
			Name:    "Redirect Target Valid",
			Status:  StatusOK,
			Message: "No redirect configured",
		}, false
	}
	if err != nil {
		return "", DoctorCheck{
			Name:    "Redirect Target Valid",
			Status:  StatusWarning,
			Message: "Cannot read redirect file",
			Detail:  err.Error(),
		}, false
	}
	target := parseRedirectTarget(data)
	if target == "" {
		return "", DoctorCheck{
			Name:    "Redirect Target Valid",
			Status:  StatusWarning,
			Message: "Redirect file is empty",
			Fix:     "Remove the empty redirect file or add a valid path",
		}, false
	}
	beadsDir := filepath.Dir(redirectPath)
	return resolveRedirectTarget(beadsDir, target), DoctorCheck{}, true
}

func statRedirectTargetDir(resolvedTarget string) (DoctorCheck, bool) {
	info, err := os.Stat(resolvedTarget)
	if os.IsNotExist(err) {
		return DoctorCheck{
			Name:    "Redirect Target Valid",
			Status:  StatusError,
			Message: "Redirect target does not exist",
			Detail:  fmt.Sprintf("Target: %s", resolvedTarget),
			Fix:     "Fix the redirect path or create the target directory",
		}, false
	}
	if err != nil {
		return DoctorCheck{
			Name:    "Redirect Target Valid",
			Status:  StatusWarning,
			Message: "Cannot access redirect target",
			Detail:  err.Error(),
		}, false
	}
	if !info.IsDir() {
		return DoctorCheck{
			Name:    "Redirect Target Valid",
			Status:  StatusError,
			Message: "Redirect target is not a directory",
			Detail:  fmt.Sprintf("Target: %s", resolvedTarget),
		}, false
	}
	return DoctorCheck{}, true
}

func checkRedirectHasDatabase(resolvedTarget string) DoctorCheck {
	// First check for Dolt backend via metadata.json — Dolt server mode has no local .db file
	metadataPath := filepath.Join(resolvedTarget, "metadata.json")
	metadataData, metaErr := os.ReadFile(metadataPath) // #nosec G304 -- constructed from known path
	if metaErr == nil && strings.Contains(string(metadataData), `"backend"`) &&
		strings.Contains(string(metadataData), `"dolt"`) {
		return DoctorCheck{
			Name:    "Redirect Target Valid",
			Status:  StatusOK,
			Message: fmt.Sprintf("Redirect target valid (dolt backend): %s", resolvedTarget),
		}
	}
	// Legacy: check for Dolt database directory or SQLite .db file
	doltDir := filepath.Join(resolvedTarget, "dolt")
	if info, statErr := os.Stat(doltDir); statErr == nil && info.IsDir() {
		return DoctorCheck{
			Name:    "Redirect Target Valid",
			Status:  StatusOK,
			Message: fmt.Sprintf("Redirect target valid (dolt directory): %s", resolvedTarget),
		}
	}
	dbPath := filepath.Join(resolvedTarget, "beads.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		matches, _ := filepath.Glob(filepath.Join(resolvedTarget, "*.db"))
		if len(matches) == 0 {
			return DoctorCheck{
				Name:    "Redirect Target Valid",
				Status:  StatusWarning,
				Message: "Redirect target has no beads database",
				Detail:  fmt.Sprintf("Target: %s (no dolt directory or .db file found)", resolvedTarget),
				Fix:     "Run 'bd init' in the target directory or check redirect path",
			}
		}
	}
	return DoctorCheck{
		Name:    "Redirect Target Valid",
		Status:  StatusOK,
		Message: fmt.Sprintf("Redirect target valid (legacy): %s", resolvedTarget),
	}
}

// CheckRedirectTargetSyncWorktree verifies that the redirect target has a working beads-sync worktree.
// This is important for repos using sync-branch mode with redirects.
// repoPath is the project root directory.
func CheckRedirectTargetSyncWorktree(repoPath string) DoctorCheck {
	redirectPath := filepath.Join(repoPath, ".beads", "redirect")

	// Check if redirect file exists
	data, err := os.ReadFile(redirectPath) // #nosec G304 - path is hardcoded
	if os.IsNotExist(err) {
		return DoctorCheck{
			Name:    "Redirect Target Sync",
			Status:  StatusOK,
			Message: "No redirect configured",
		}
	}
	if err != nil {
		return DoctorCheck{
			Name:    "Redirect Target Sync",
			Status:  StatusOK, // Don't warn if we can't read - other check handles that
			Message: "N/A (cannot read redirect)",
		}
	}

	target := parseRedirectTarget(data)
	if target == "" {
		return DoctorCheck{
			Name:    "Redirect Target Sync",
			Status:  StatusOK,
			Message: "N/A (empty redirect)",
		}
	}

	// Resolve the target path
	beadsDir := filepath.Dir(redirectPath)
	resolvedTarget := resolveRedirectTarget(beadsDir, target)

	// Check if the target has a sync-branch configured in config.yaml
	configPath := filepath.Join(resolvedTarget, "config.yaml")
	configData, err := os.ReadFile(configPath) // #nosec G304 - constructed from known path
	if err != nil {
		// No config.yaml means no sync-branch, which is fine
		return DoctorCheck{
			Name:    "Redirect Target Sync",
			Status:  StatusOK,
			Message: "N/A (target not using sync-branch mode)",
		}
	}

	// Simple check for sync-branch in config
	if !strings.Contains(string(configData), "sync-branch:") {
		return DoctorCheck{
			Name:    "Redirect Target Sync",
			Status:  StatusOK,
			Message: "N/A (target not using sync-branch mode)",
		}
	}

	// Target uses sync-branch - check for beads-sync worktree in the repo containing the target
	// The target is inside a .beads dir, so the repo is the parent of .beads
	targetRepoRoot := filepath.Dir(resolvedTarget)

	// Check for beads-sync worktree
	worktreePath := filepath.Join(targetRepoRoot, ".beads-sync")
	if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
		return DoctorCheck{
			Name:    "Redirect Target Sync",
			Status:  StatusWarning,
			Message: "Redirect target missing beads-sync worktree",
			Detail:  fmt.Sprintf("Expected worktree at: %s", worktreePath),
			Fix:     fmt.Sprintf("Run 'bd init' in %s to set up beads", targetRepoRoot),
		}
	}

	return DoctorCheck{
		Name:    "Redirect Target Sync",
		Status:  StatusOK,
		Message: "Redirect target has beads-sync worktree",
	}
}

// CheckNoVestigialSyncWorktrees detects beads-sync worktrees in redirected repos that are unused.
// When a repo uses .beads/redirect, it doesn't need its own beads-sync worktree since
// sync operations happen in the redirect target. These vestigial worktrees waste space.
// repoPath is the project root directory.
func CheckNoVestigialSyncWorktrees(repoPath string) DoctorCheck {
	redirectPath := filepath.Join(repoPath, ".beads", "redirect")

	// Check if redirect file exists
	if _, err := os.Stat(redirectPath); os.IsNotExist(err) {
		// No redirect - this check doesn't apply
		return DoctorCheck{
			Name:    "Vestigial Sync Worktrees",
			Status:  StatusOK,
			Message: "N/A (no redirect configured)",
		}
	}

	// Use repoPath to find git root instead of walking up from cwd
	gitRoot := repoPath
	for {
		if _, err := os.Stat(filepath.Join(gitRoot, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(gitRoot)
		if parent == gitRoot {
			// Reached filesystem root, not in a git repo
			return DoctorCheck{
				Name:    "Vestigial Sync Worktrees",
				Status:  StatusOK,
				Message: "N/A (not in git repository)",
			}
		}
		gitRoot = parent
	}

	// Check for .beads-sync worktree
	syncWorktreePath := filepath.Join(gitRoot, ".beads-sync")
	if _, err := os.Stat(syncWorktreePath); os.IsNotExist(err) {
		// No local worktree - good
		return DoctorCheck{
			Name:    "Vestigial Sync Worktrees",
			Status:  StatusOK,
			Message: "No vestigial sync worktrees found",
		}
	}

	// Found a local .beads-sync but we have a redirect - this is vestigial
	return DoctorCheck{
		Name:    "Vestigial Sync Worktrees",
		Status:  StatusWarning,
		Message: "Vestigial .beads-sync worktree found",
		Detail:  fmt.Sprintf("This repo uses redirect but has unused worktree at: %s", syncWorktreePath),
		Fix:     fmt.Sprintf("Remove with: rm -rf %s", syncWorktreePath),
	}
}

// CheckLastTouchedNotTracked verifies that .beads/last-touched is NOT tracked by git.
// The last-touched file is local runtime state that should never be committed.
// If committed, it causes spurious diffs in other clones.
// repoPath is the project root directory.
func CheckLastTouchedNotTracked(repoPath string) DoctorCheck {
	lastTouchedPath := filepath.Join(repoPath, ".beads", "last-touched")

	// First check if the file exists
	if _, err := os.Stat(lastTouchedPath); os.IsNotExist(err) {
		// File doesn't exist - nothing to check
		return DoctorCheck{
			Name:    "Last-Touched Tracking",
			Status:  StatusOK,
			Message: "No last-touched file present",
		}
	}

	// Check if git considers this file tracked
	// git ls-files exits 0 and outputs the filename if tracked, empty if untracked
	cmd := exec.Command("git", "ls-files", lastTouchedPath) // #nosec G204 - args are hardcoded paths
	output, err := cmd.Output()
	if err != nil {
		// Not in a git repo or git error - skip check
		return DoctorCheck{
			Name:    "Last-Touched Tracking",
			Status:  StatusOK,
			Message: "N/A (not a git repository)",
		}
	}

	trackedPath := strings.TrimSpace(string(output))
	if trackedPath == "" {
		// File exists but is not tracked - this is correct
		return DoctorCheck{
			Name:    "Last-Touched Tracking",
			Status:  StatusOK,
			Message: "last-touched file not tracked (correct)",
		}
	}

	// File is tracked - this is a problem
	return DoctorCheck{
		Name:    "Last-Touched Tracking",
		Status:  StatusWarning,
		Message: "last-touched file is tracked by git",
		Detail:  "The .beads/last-touched file is local runtime state that should never be committed.",
		Fix:     "Run 'bd doctor --fix' to untrack, or manually: git rm --cached .beads/last-touched",
	}
}

// FixLastTouchedTracking untracks the .beads/last-touched file from git.
// repoPath is the project root directory.
func FixLastTouchedTracking(repoPath string) error {
	lastTouchedPath := filepath.Join(repoPath, ".beads", "last-touched")

	// Check if file is actually tracked first
	cmd := exec.Command("git", "ls-files", lastTouchedPath) // #nosec G204 - args are hardcoded paths
	output, err := cmd.Output()
	if err != nil {
		return nil // Not a git repo, nothing to do
	}

	trackedPath := strings.TrimSpace(string(output))
	if trackedPath == "" {
		return nil // Not tracked, nothing to do
	}

	// Untrack the file (keeps the local copy)
	cmd = exec.Command("git", "rm", "--cached", lastTouchedPath) // #nosec G204 - args are hardcoded paths
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to untrack last-touched file: %w", err)
	}

	return nil
}
