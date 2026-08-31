package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/beads"
)

func ensureCreatedWorktreeClean(ctx context.Context, worktreePath string) error {
	gitCmd := gitCmdInDir(ctx, worktreePath, "status", "--porcelain=v1", "--untracked-files=all")
	output, err := gitCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to inspect created worktree cleanliness: %w\n%s", err, string(output))
	}

	if status := strings.TrimSpace(string(output)); status != "" {
		return fmt.Errorf("created worktree is dirty after checkout; refusing to continue: %s\n%s", worktreePath, status)
	}

	return nil
}

// gitCmdInDir creates a git command that runs in the specified directory.
// This is used for worktree operations that need to run in a specific location
// (either the CWD repo root or a specific worktree path).
//
// Security: Sets core.hooksPath and GIT_TEMPLATE_DIR to disable hooks/templates
// for defense-in-depth, matching the pattern in RepoContext.GitCmd().
func gitCmdInDir(ctx context.Context, dir string, args ...string) *exec.Cmd {
	gitArgs := append([]string{"-c", "core.hooksPath="}, args...)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	cmd.Dir = dir
	// Security: Disable git hooks and templates (SEC-001, SEC-002)
	cmd.Env = append(os.Environ(),
		"GIT_TEMPLATE_DIR=",
	)
	return cmd
}

// listWorktreesWithoutBeads lists worktrees when no .beads directory exists.
// This fallback allows the command to work in repos that haven't been initialized.
func listWorktreesWithoutBeads(ctx context.Context, repoRoot string) error {
	worktrees, err := readWorktreeList(ctx, repoRoot)
	if err != nil {
		return err
	}
	for i := range worktrees {
		worktrees[i].BeadsState = "none"
	}
	return renderWorktreeList(worktrees)
}

func parseWorktreeList(output string) []WorktreeInfo {
	var worktrees []WorktreeInfo
	var current WorktreeInfo

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "worktree ") {
			if current.Path != "" {
				worktrees = append(worktrees, current)
			}
			path := strings.TrimPrefix(line, "worktree ")
			current = WorktreeInfo{
				Path: path,
				Name: filepath.Base(path),
			}
			continue
		}
		parseWorktreeListField(line, &current)
	}
	if current.Path != "" {
		worktrees = append(worktrees, current)
	}

	// Mark the first non-bare worktree as main
	if len(worktrees) > 0 && worktrees[0].Branch != "(bare)" {
		worktrees[0].IsMain = true
	}

	return worktrees
}

func parseWorktreeListField(line string, current *WorktreeInfo) {
	switch {
	case strings.HasPrefix(line, "HEAD "):
		// Skip HEAD hash.
	case strings.HasPrefix(line, "branch "):
		current.Branch = strings.TrimPrefix(line, "branch refs/heads/")
	case line == "bare":
		current.IsMain = true
		current.Branch = "(bare)"
	}
}

func getBeadsState(worktreePath, mainBeadsDir string) string {
	beadsDir := filepath.Join(worktreePath, ".beads")
	redirectFile := filepath.Join(beadsDir, beads.RedirectFileName)

	if _, err := os.Stat(redirectFile); err == nil {
		return "redirect"
	}
	if _, err := os.Stat(beadsDir); err == nil {
		// Check if this is the main beads dir
		absBeadsDir, _ := filepath.Abs(beadsDir)
		absMainBeadsDir, _ := filepath.Abs(mainBeadsDir)
		if absBeadsDir == absMainBeadsDir {
			return "shared"
		}
		return "local"
	}
	return "none"
}

func getRedirectTarget(worktreePath string) string {
	redirectFile := filepath.Join(worktreePath, ".beads", beads.RedirectFileName)
	// #nosec G304 - path is constructed from worktreePath which comes from git worktree list
	data, err := os.ReadFile(redirectFile)
	if err != nil {
		return ""
	}
	target := strings.TrimSpace(string(data))
	// Resolve relative paths from the worktree root (matching FollowRedirect behavior)
	if !filepath.IsAbs(target) {
		target = filepath.Join(worktreePath, target)
	}
	target, _ = filepath.Abs(target)
	return target
}

func resolveWorktreePath(ctx context.Context, repoRoot, name string) (string, error) {
	if path, ok := existingWorktreePath(repoRoot, name); ok {
		return path, nil
	}
	return resolveWorktreeFromRegistry(ctx, repoRoot, name)
}

func existingWorktreePath(repoRoot, name string) (string, bool) {
	if filepath.IsAbs(name) {
		if _, err := os.Stat(name); err == nil {
			return name, true
		}
	}

	absPath, _ := filepath.Abs(name)
	if _, err := os.Stat(absPath); err == nil {
		return absPath, true
	}

	repoPath := filepath.Join(repoRoot, name)
	if _, err := os.Stat(repoPath); err == nil {
		return repoPath, true
	}
	return "", false
}

func resolveWorktreeFromRegistry(ctx context.Context, repoRoot, name string) (string, error) {
	gitCmd := gitCmdInDir(ctx, repoRoot, "worktree", "list", "--porcelain")
	output, err := gitCmd.CombinedOutput()
	if err == nil {
		worktrees := parseWorktreeList(string(output))
		for _, wt := range worktrees {
			if worktreeMatchesName(wt, name) {
				return wt.Path, nil
			}
		}
	}

	return "", fmt.Errorf("worktree not found: %s", name)
}

func worktreeMatchesName(worktree WorktreeInfo, name string) bool {
	if worktree.Name != name && worktree.Path != name {
		return false
	}
	_, err := os.Stat(worktree.Path)
	return err == nil
}
