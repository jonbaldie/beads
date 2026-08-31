// Package beads context.go provides centralized repository context resolution.
//
// Problem: 50+ git commands across the codebase assume CWD is the repository root.
// When BEADS_DIR points to a different repo, or when running from a worktree,
// these commands execute in the wrong directory.
//
// Solution: RepoContext provides a single source of truth for repository paths,
// with methods that ensure git commands run in the correct repository.
//
// Usage:
//
//	rc, err := beads.GetRepoContext()
//	if err != nil {
//	    return err
//	}
//	cmd := rc.GitCmd(ctx, "status")  // Runs in beads repo, not CWD
//
// See engdocs/REPO_CONTEXT.md for detailed documentation.
package beads

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/git"
)

// UserRole represents the user's relationship to a repository.
// Used to determine appropriate behaviors for fork contributors vs maintainers.
type UserRole string

// Role constants for user relationship to repository.
const (
	// Contributor indicates the user is contributing to a fork (not the maintainer).
	// BEADS_DIR redirection implies contributor role automatically.
	Contributor UserRole = "contributor"

	// Maintainer indicates the user owns/maintains the repository.
	Maintainer UserRole = "maintainer"
)

// ErrRoleNotConfigured is returned when beads.role is not set in git config.
// This signals that the init prompt should be shown to configure the role.
var ErrRoleNotConfigured = errors.New("beads.role not configured in git config")

// RepoContext holds resolved repository paths for beads operations.
//
// The struct distinguishes between:
//   - RepoRoot: where .beads/ lives (for git operations on beads data)
//   - CWDRepoRoot: where user is working (for status display, etc.)
//
// These may differ when BEADS_DIR points to a different repository,
// or when running from a git worktree.
type RepoContext struct {
	// BeadsDir is the actual .beads directory path (after following redirects).
	BeadsDir string

	// RepoRoot is the repository root containing BeadsDir.
	// Git commands for beads operations should run here.
	RepoRoot string

	// CWDRepoRoot is the repository root containing the current working directory.
	// May differ from RepoRoot when BEADS_DIR points elsewhere.
	CWDRepoRoot string

	// IsRedirected is true if BeadsDir resolves to a different repository than CWD.
	// This covers explicit BEADS_DIR usage and redirect files.
	IsRedirected bool

	// IsWorktree is true if CWD is in a git worktree.
	IsWorktree bool
}

// GetRepoContext resolves the repository context for the current workspace.
//
// Resolution is intentionally request-scoped: callers that need a stable
// context can retain the returned value, while separate callers and tests do
// not share process-wide state when CWD or BEADS_DIR changes.
func GetRepoContext() (*RepoContext, error) {
	return buildRepoContext()
}

// buildRepoContext constructs the RepoContext by resolving all paths.
func buildRepoContext() (*RepoContext, error) {
	// 1. Find .beads directory (respects BEADS_DIR env var)
	beadsDir := FindBeadsDir()
	if beadsDir == "" {
		return nil, fmt.Errorf("no .beads directory found")
	}

	// 2. Security: Validate path boundary (SEC-003)
	if !isPathInSafeBoundary(beadsDir) {
		return nil, fmt.Errorf("BEADS_DIR points to unsafe location: %s", beadsDir)
	}

	// 3. Check for redirect file in the local repo
	redirectInfo := GetRedirectInfo()

	// 4. Determine RepoRoot based on external/redirect status
	var repoRoot string
	isExternal := redirectInfo.IsRedirected
	if !isExternal {
		if external, err := isExternalBeadsDir(beadsDir); err == nil {
			isExternal = external
		}
	}

	if isExternal {
		// Beads dir is in a different repo - use that repo's root
		repoRoot = repoRootForBeadsDir(beadsDir)
	} else {
		// Normal case - find repo root via git
		var err error
		repoRoot, err = git.GetMainRepoRoot()
		if err != nil {
			return nil, fmt.Errorf("cannot determine repository root: %w", err)
		}
	}

	// 5. Get CWD's repo root (may differ from RepoRoot)
	cwdRepoRoot := git.GetRepoRoot() // Returns "" if not in git repo

	// 6. Check worktree status
	isWorktree := git.IsWorktree()

	return &RepoContext{
		BeadsDir:     beadsDir,
		RepoRoot:     repoRoot,
		CWDRepoRoot:  cwdRepoRoot,
		IsRedirected: isExternal,
		IsWorktree:   isWorktree,
	}, nil
}

// isExternalBeadsDir returns true if beadsDir is in a different git repo than CWD.
// Uses git common dir to correctly handle worktrees and bare repos.
func isExternalBeadsDir(beadsDir string) (bool, error) {
	cwdCommonDir, err := git.GetGitCommonDir()
	if err != nil {
		return false, err
	}

	beadsCommonDir, err := getGitCommonDirForPath(beadsDir)
	if err != nil {
		return false, err
	}

	return cwdCommonDir != beadsCommonDir, nil
}

// getGitCommonDirForPath returns the shared git directory for a path.
// For worktrees, this returns the shared git directory (common to all worktrees).
func getGitCommonDirForPath(path string) (string, error) {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--git-common-dir")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git common dir for %s: %w", path, err)
	}
	result := strings.TrimSpace(string(output))

	if !filepath.IsAbs(result) {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("failed to get absolute path for %s: %w", path, err)
		}
		result = filepath.Join(absPath, result)
	}

	result = filepath.Clean(result)
	if resolved, err := filepath.EvalSymlinks(result); err == nil {
		result = resolved
	}

	return result, nil
}

// repoRootForBeadsDir returns the repository root for a beads directory.
// Falls back to the beadsDir parent if git lookup fails.
func repoRootForBeadsDir(beadsDir string) string {
	repoRoot, err := getRepoRootFromPath(beadsDir)
	if err == nil && repoRoot != "" {
		return repoRoot
	}
	return filepath.Dir(beadsDir)
}

// getRepoRootFromPath returns the git repository root for a given path.
func getRepoRootFromPath(path string) (string, error) {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git root for %s: %w", path, err)
	}
	return strings.TrimSpace(string(output)), nil
}
