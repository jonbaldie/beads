package beads

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
)

// GitCmd creates an exec.Cmd configured to run git in the beads repository.
//
// This method sets cmd.Dir to RepoRoot, ensuring git commands operate on
// the correct repository regardless of CWD.
//
// Security: Git hooks and templates are disabled to prevent code execution
// in potentially malicious repositories (SEC-001, SEC-002).
//
// Pattern:
//
//	cmd := rc.GitCmd(ctx, "add", ".beads/")
//	output, err := cmd.Output()
//
// Equivalent to running: cd $RepoRoot && git add .beads/
//
// GH#2538: When running from a git worktree, git may inherit environment
// variables that point to the worktree's .git instead of the main repo.
// We explicitly set GIT_DIR and GIT_WORK_TREE to ensure git operates on the
// correct repository (the one containing .beads/).
func (rc *RepoContext) GitCmd(ctx context.Context, args ...string) *exec.Cmd {
	gitArgs := append([]string{"-c", "core.hooksPath="}, args...)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	cmd.Dir = rc.RepoRoot

	// GH#2538: Ensure git uses the target repository, not the worktree we may be running from.
	// This fixes "pathspec outside repository" errors when bd sync runs from a worktree.
	gitDir := filepath.Join(rc.RepoRoot, ".git")

	// Security: Disable git hooks and templates to prevent code execution
	// in potentially malicious repositories (SEC-001, SEC-002)
	cmd.Env = append(os.Environ(),
		"GIT_TEMPLATE_DIR=",          // Disable templates
		"GIT_DIR="+gitDir,            // Ensure git uses the correct .git directory
		"GIT_WORK_TREE="+rc.RepoRoot, // Ensure git uses the correct work tree
	)
	return cmd
}

// GitCmdCWD creates an exec.Cmd configured to run git in the user's working repository.
//
// Use this for git commands that should reflect the user's current context,
// such as showing status or checking for uncommitted changes in their working repo.
//
// If CWD is not in a git repository, cmd.Dir is left unset (uses process CWD).
func (rc *RepoContext) GitCmdCWD(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	if rc.CWDRepoRoot != "" {
		cmd.Dir = rc.CWDRepoRoot
	}
	return cmd
}

// RelPath returns the given absolute path relative to the beads repository root.
//
// Useful for displaying paths to users in a consistent, repo-relative format.
// Returns an error if the path is not within the repository.
func (rc *RepoContext) RelPath(absPath string) (string, error) {
	return filepath.Rel(rc.RepoRoot, absPath)
}
