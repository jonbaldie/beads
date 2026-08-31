package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// CheckGitWorkingTree checks if the git working tree is clean.
// This helps prevent leaving work stranded (AGENTS.md: keep git state clean).
func CheckGitWorkingTree(path string) DoctorCheck {
	ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-dir")
	cmd.Dir = path
	if err := cmd.Run(); err != nil {
		return DoctorCheck{
			Name:    "Git Working Tree",
			Status:  StatusOK,
			Message: "N/A (not a git repository)",
		}
	}

	cmd = exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmd.Dir = path
	out, err := cmd.Output()
	if err != nil {
		return DoctorCheck{
			Name:    "Git Working Tree",
			Status:  StatusWarning,
			Message: "Unable to check git status",
			Detail:  err.Error(),
			Fix:     "Run 'git status' and commit/stash changes before syncing",
		}
	}

	status := strings.TrimSpace(string(out))
	if status == "" {
		return DoctorCheck{
			Name:    "Git Working Tree",
			Status:  StatusOK,
			Message: "Clean",
		}
	}

	// Parse raw porcelain lines preserving leading spaces for correct XY parsing.
	// strings.TrimSpace above strips the leading space from the first " D ..."
	// line, corrupting porcelain format. Use the raw output for line parsing.
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")

	// In redirect worktrees (.beads/redirect exists), deleted .beads/ files
	// are expected — the actual data lives at the redirect target (the rig).
	// Filter these out so they don't trigger a false warning.
	redirectPath := filepath.Join(path, ".beads", "redirect")
	if _, err := os.Stat(redirectPath); err == nil {
		var filtered []string
		for _, line := range lines {
			if isExpectedRedirectChange(line) {
				continue
			}
			filtered = append(filtered, line)
		}
		if len(filtered) == 0 {
			return DoctorCheck{
				Name:    "Git Working Tree",
				Status:  StatusOK,
				Message: "Clean (redirect worktree, .beads/ deletions expected)",
			}
		}
		lines = filtered
	}

	// Show a small sample of paths for quick debugging.
	maxLines := 8
	if len(lines) > maxLines {
		lines = append(lines[:maxLines], "…")
	}

	return DoctorCheck{
		Name:    "Git Working Tree",
		Status:  StatusWarning,
		Message: "Uncommitted changes present",
		Detail:  strings.Join(lines, "\n"),
		Fix:     "Commit or stash changes, then follow AGENTS.md: git pull --rebase && git push",
	}
}

// isExpectedRedirectChange returns true if a git status --porcelain line
// represents an expected change in a redirect worktree: deleted .beads/ files
// or the untracked .beads/redirect file itself.
// Porcelain format: XY PATH where X=index status, Y=worktree status.
// Deletions show as " D .beads/..." (unstaged) or "D  .beads/..." (staged).
func isExpectedRedirectChange(line string) bool {
	if len(line) < 4 {
		return false
	}
	xy := line[:2]
	filePath := line[3:]
	if !strings.HasPrefix(filePath, ".beads/") {
		return false
	}
	// Deleted .beads/ files (expected: data lives at redirect target)
	if xy == " D" || xy == "D " || xy == "DD" {
		return true
	}
	// Untracked .beads/redirect file (expected: the redirect marker itself)
	if xy == "??" && filePath == ".beads/redirect" {
		return true
	}
	return false
}

// CheckGitUpstream checks whether the current branch is up to date with its upstream.
// This catches common "forgot to pull/push" failure modes (AGENTS.md: pull --rebase, push).
func CheckGitUpstream(path string) DoctorCheck {
	ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
	defer cancel()

	if !isGitRepository(ctx, path) {
		return DoctorCheck{
			Name:    "Git Upstream",
			Status:  StatusOK,
			Message: "N/A (not a git repository)",
		}
	}

	branch, ok := currentGitBranch(ctx, path)
	if !ok {
		return DoctorCheck{
			Name:    "Git Upstream",
			Status:  StatusWarning,
			Message: "Detached HEAD (no branch)",
			Fix:     "Check out a branch before syncing",
		}
	}

	// Check if any remotes exist — no point warning about upstream if there's no remote
	if !gitHasRemote(ctx, path) {
		return DoctorCheck{
			Name:    "Git Upstream",
			Status:  StatusOK,
			Message: "N/A — no remotes configured",
		}
	}

	upstream, ok := configuredGitUpstream(ctx, path)
	if !ok {
		return DoctorCheck{
			Name:    "Git Upstream",
			Status:  StatusWarning,
			Message: fmt.Sprintf("No upstream configured for %s", branch),
			Fix:     fmt.Sprintf("Set upstream then push: git push -u origin %s", branch),
		}
	}

	ahead, aheadErr := gitRevListCount(ctx, path, "@{u}..HEAD")
	behind, behindErr := gitRevListCount(ctx, path, "HEAD..@{u}")
	return gitUpstreamStatus(branch, upstream, ahead, behind, aheadErr, behindErr)
}

func isGitRepository(ctx context.Context, path string) bool {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-dir")
	cmd.Dir = path
	return cmd.Run() == nil
}

func currentGitBranch(ctx context.Context, path string) (string, bool) {
	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "--short", "HEAD")
	cmd.Dir = path
	branch, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(branch)), true
}

func gitHasRemote(ctx context.Context, path string) bool {
	cmd := exec.CommandContext(ctx, "git", "remote")
	cmd.Dir = path
	remote, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(remote)) != ""
}

func configuredGitUpstream(ctx context.Context, path string) (string, bool) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	cmd.Dir = path
	upstream, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(upstream)), true
}

func gitUpstreamStatus(branch, upstream string, ahead, behind int, aheadErr, behindErr error) DoctorCheck {
	if aheadErr != nil || behindErr != nil {
		return unableToCompareGitUpstream(upstream, aheadErr, behindErr)
	}
	return gitUpstreamComparisonStatus(branch, upstream, ahead, behind)
}

func unableToCompareGitUpstream(upstream string, aheadErr, behindErr error) DoctorCheck {
	detailParts := []string{}
	if aheadErr != nil {
		detailParts = append(detailParts, "ahead: "+aheadErr.Error())
	}
	if behindErr != nil {
		detailParts = append(detailParts, "behind: "+behindErr.Error())
	}
	return DoctorCheck{
		Name:    "Git Upstream",
		Status:  StatusWarning,
		Message: fmt.Sprintf("Unable to compare with upstream (%s)", upstream),
		Detail:  strings.Join(detailParts, "; "),
		Fix:     "Run 'git fetch' then check: git status -sb",
	}
}

func gitUpstreamComparisonStatus(branch, upstream string, ahead, behind int) DoctorCheck {
	if ahead == 0 && behind == 0 {
		return DoctorCheck{
			Name:    "Git Upstream",
			Status:  StatusOK,
			Message: fmt.Sprintf("Up to date (%s)", upstream),
			Detail:  fmt.Sprintf("Branch: %s", branch),
		}
	}

	if ahead > 0 && behind == 0 {
		return DoctorCheck{
			Name:    "Git Upstream",
			Status:  StatusWarning,
			Message: fmt.Sprintf("Ahead of upstream by %d commit(s)", ahead),
			Detail:  fmt.Sprintf("Branch: %s, upstream: %s", branch, upstream),
			Fix:     "Run 'git push' (AGENTS.md: git pull --rebase && git push)",
		}
	}

	if behind > 0 && ahead == 0 {
		return DoctorCheck{
			Name:    "Git Upstream",
			Status:  StatusWarning,
			Message: fmt.Sprintf("Behind upstream by %d commit(s)", behind),
			Detail:  fmt.Sprintf("Branch: %s, upstream: %s", branch, upstream),
			Fix:     "Run 'git pull --rebase' (then re-run bd doctor)",
		}
	}

	return DoctorCheck{
		Name:    "Git Upstream",
		Status:  StatusWarning,
		Message: fmt.Sprintf("Diverged from upstream (ahead %d, behind %d)", ahead, behind),
		Detail:  fmt.Sprintf("Branch: %s, upstream: %s", branch, upstream),
		Fix:     "Run 'git pull --rebase' then 'git push'",
	}
}

func gitRevListCount(ctx context.Context, path string, rangeExpr string) (int, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-list", "--count", rangeExpr) // #nosec G204 -- fixed args
	cmd.Dir = path
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	countStr := strings.TrimSpace(string(out))
	if countStr == "" {
		return 0, nil
	}

	var n int
	if _, err := fmt.Sscanf(countStr, "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

// staleBdHookPattern matches the removed "bd hook <name>" command (not "bd hooks run").
// This was removed in v0.58.0 and replaced by "bd hooks run".
var staleBdHookPattern = regexp.MustCompile(`\bbd\s+hook\s+(?:pre-commit|post-merge|pre-push|post-checkout|prepare-commit-msg)\b`)
