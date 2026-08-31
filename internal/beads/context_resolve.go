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
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/git"
)

// ResetCaches is retained for compatibility with callers that reset related
// repository caches between tests. RepoContext resolution is request-scoped,
// so there is no package cache to clear here.
//
// Usage in tests:
//
//	t.Cleanup(func() {
//	    beads.ResetCaches()
//	    git.ResetCaches()
//	})
func ResetCaches() {
}

// unsafePrefixes lists system directories that BEADS_DIR should never point to.
// This prevents path traversal attacks (SEC-003).
var unsafePrefixes = []string{
	"/etc", "/usr", "/var", "/root", "/System", "/Library",
	"/bin", "/sbin", "/opt", "/private",
}

// isPathInSafeBoundary validates that a path is not in sensitive system directories.
// Returns false if the path is in an unsafe location (SEC-003).
func isPathInSafeBoundary(path string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}

	if tempRoot, ok := tempBoundaryFor(absPath); ok {
		return resolvedPathWithinRoot(absPath, tempRoot)
	}

	// Allow /var/home as a valid user home directory (Fedora Silverblue, Bluefin, etc.)
	if strings.HasPrefix(absPath, "/var/home/") {
		return true
	}

	// Allow /var/tmp as the FHS-standard secondary temp directory (persists across
	// reboots, unlike /tmp). This is distinct from the os.TempDir() carve-out
	// above: a machine's build tooling can set GOTMPDIR to redirect Go's own
	// test/compile temp dirs under /var/tmp even while os.TempDir() itself still
	// reports /tmp, so t.TempDir() in a test binary can land here without the
	// os.TempDir() check ever seeing it (be-odye4). Like /Users/Shared, /var/tmp is
	// world-writable (drwxrwxrwt), so resolve symlinks before admitting (SEC-003):
	// a symlink planted under it must not be followed into a rejected directory.
	if hasPathPrefixOrRoot(absPath, "/var/tmp") {
		return resolvedPathWithinRoot(absPath, "/var/tmp")
	}

	if hasUnsafePrefix(absPath) {
		return false
	}

	// macOS's /Users/Shared is the OS-designated shared directory, not a peer
	// user's home — allow it (and its subpaths) before the peer-home rejection
	// below. SEC-003 guards against path traversal into system directories; the
	// unsafePrefixes blocklist above stays authoritative, so this carve-out only
	// admits the shared dir, mirroring the /var/home/ allowance. /Users/Shared is
	// world-writable (drwxrwxrwt), so resolve symlinks before admitting: a symlink
	// planted under it whose target escapes the boundary must be rejected, not
	// followed into a system directory (be-vc1 SEC-003 hardening).
	if hasPathPrefixOrRoot(absPath, "/Users/Shared") {
		return resolvedPathWithinRoot(absPath, "/Users/Shared")
	}

	if isOtherUserHomePath(absPath) {
		return false
	}

	return true
}

// tempBoundaryFor returns the logical temp root for a path under either the
// logical or physical spelling of os.TempDir().
func tempBoundaryFor(absPath string) (string, bool) {
	tempDir := strings.TrimSuffix(os.TempDir(), "/")
	physTempDir := strings.TrimSuffix(resolveLongestExistingAncestor(tempDir), "/")
	if hasPathPrefixOrRoot(absPath, tempDir) || hasPathPrefixOrRoot(absPath, physTempDir) {
		return tempDir, true
	}
	return "", false
}

func hasPathPrefixOrRoot(path, root string) bool {
	return path == root || hasPathPrefix(path, root)
}

func hasPathPrefix(path, root string) bool {
	return strings.HasPrefix(path, root+"/")
}

func hasUnsafePrefix(path string) bool {
	for _, prefix := range unsafePrefixes {
		if hasPathPrefixOrRoot(path, prefix) {
			return true
		}
	}
	return false
}

// isOtherUserHomePath reports whether path is below a home-directory root but
// outside the current user's home. The roots intentionally use a trailing
// slash, matching the historical boundary check: the root itself is not
// classified as a peer home.
func isOtherUserHomePath(path string) bool {
	if !hasPathPrefix(path, "/Users") && !hasPathPrefix(path, "/home") && !hasPathPrefix(path, "/var/home") {
		return false
	}

	// Resolve the current user's home from the account database, which is not
	// affected by $HOME manipulation. Fall back to $HOME when that lookup is
	// unavailable (e.g. CGO-free builds where the user is not in /etc/passwd).
	homeDir := ""
	if u, err := user.Current(); err == nil {
		homeDir = u.HomeDir
	}
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}
	if homeDir == "" {
		return false
	}

	// Compare on a path boundary so a sibling like /home/aliceXX is not treated
	// as inside /home/alice.
	home := strings.TrimSuffix(homeDir, "/")
	return path != home && !strings.HasPrefix(path, home+"/")
}

// resolveLongestExistingAncestor canonicalizes path by resolving symlinks on its
// longest existing ancestor and re-appending the trailing segments that do not
// exist yet. Unlike a bare filepath.EvalSymlinks (which fails on a non-existent
// path and leaves it unresolved), this lets a not-yet-created BEADS_DIR still be
// canonicalized against a real, symlink-free root. The upward walk mirrors the
// filepath.Dir loops elsewhere in this package.
func resolveLongestExistingAncestor(path string) string {
	cur := filepath.Clean(path)
	remainder := ""
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			if remainder == "" {
				return resolved
			}
			return filepath.Join(resolved, remainder)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the filesystem root without resolving anything; return the
			// cleaned input unchanged (best effort).
			return filepath.Clean(path)
		}
		remainder = filepath.Join(filepath.Base(cur), remainder)
		cur = parent
	}
}

// resolvedPathWithinRoot reports whether absPath, after symlink resolution, still
// lies within root. Both sides are resolved via resolveLongestExistingAncestor so
// the comparison is symlink-safe and works for not-yet-created paths: a symlink
// under root whose target escapes root resolves outside and returns false, while
// a real (or not-yet-created) subpath of a non-symlinked root returns true.
//
// This hardens the /Users/Shared carve-out (be-vc1, SEC-003): /Users/Shared is
// world-writable, so a co-located user could plant a symlink there pointing at a
// system directory; matching on the unresolved path would admit it. Resolving
// first closes that path-traversal vector. Resolving root too is a no-op for the
// real /Users/Shared but is required for temp-dir-rooted tests on macOS, where
// the temp dir lives under the symlinked /var.
func resolvedPathWithinRoot(absPath, root string) bool {
	resolved := resolveLongestExistingAncestor(absPath)
	resolvedRoot := resolveLongestExistingAncestor(root)
	return resolved == resolvedRoot || strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator))
}

// GetRepoContextForWorkspace returns a fresh RepoContext for a specific workspace.
//
// Unlike GetRepoContext(), this function:
//   - Does NOT cache results (caller may handle multiple workspaces)
//   - Does NOT respect BEADS_DIR (workspace path is explicit)
//   - Resolves worktree relationships correctly
//
// This is designed for processes that need to handle
// multiple workspaces or detect context changes.
//
// The function temporarily changes to the workspace directory to resolve paths,
// then restores the original directory.
func GetRepoContextForWorkspace(workspacePath string) (*RepoContext, error) {
	// Normalize workspace path
	absWorkspace, err := filepath.Abs(workspacePath)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve workspace path %s: %w", workspacePath, err)
	}

	// Change to workspace directory temporarily
	originalDir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Chdir(originalDir) }()

	if err := os.Chdir(absWorkspace); err != nil {
		return nil, fmt.Errorf("cannot access workspace %s: %w", absWorkspace, err)
	}

	// Clear git caches for fresh resolution
	git.ResetCaches()

	// Build context fresh, specifically for this workspace (ignores BEADS_DIR)
	return buildRepoContextForWorkspace(absWorkspace)
}

// buildRepoContextForWorkspace constructs RepoContext for a specific workspace.
// Unlike buildRepoContext(), this ignores BEADS_DIR env var since the workspace
// path is explicitly provided.
func buildRepoContextForWorkspace(workspacePath string) (*RepoContext, error) {
	// 1. Determine if we're in a worktree and find the main repo root
	var repoRoot string
	var isWorktree bool

	if git.IsWorktree() {
		isWorktree = true
		var err error
		repoRoot, err = git.GetMainRepoRoot()
		if err != nil {
			return nil, fmt.Errorf("cannot determine main repository root: %w", err)
		}
	} else {
		isWorktree = false
		repoRoot = git.GetRepoRoot()
		if repoRoot == "" {
			return nil, fmt.Errorf("workspace %s is not in a git repository", workspacePath)
		}
	}

	// 2. Find .beads directory in the appropriate location
	beadsDir := filepath.Join(repoRoot, ".beads")

	// Check if .beads exists
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("no .beads directory found at %s", beadsDir)
	}

	// 3. Follow redirect if present
	beadsDir = FollowRedirect(beadsDir)

	// 4. Security: Validate path boundary (SEC-003)
	if !isPathInSafeBoundary(beadsDir) {
		return nil, fmt.Errorf("beads directory in unsafe location: %s", beadsDir)
	}

	// 5. Validate directory contains actual project files
	if !hasBeadsProjectFiles(beadsDir) {
		return nil, fmt.Errorf("beads directory missing required files: %s", beadsDir)
	}

	// 6. Get CWD's repo root (same as workspace in this case)
	cwdRepoRoot := git.GetRepoRoot()

	return &RepoContext{
		BeadsDir:     beadsDir,
		RepoRoot:     repoRoot,
		CWDRepoRoot:  cwdRepoRoot,
		IsRedirected: false, // Workspace-specific context is never "redirected"
		IsWorktree:   isWorktree,
	}, nil
}

// Validate checks if the cached context is still valid.
//
// Returns an error if BeadsDir or RepoRoot no longer exist. This is useful
// for long-running processes that need to detect when context becomes stale (DMN-002).
func (rc *RepoContext) Validate() error {
	if _, err := os.Stat(rc.BeadsDir); os.IsNotExist(err) {
		return fmt.Errorf("BeadsDir no longer exists: %s", rc.BeadsDir)
	}
	if _, err := os.Stat(rc.RepoRoot); os.IsNotExist(err) {
		return fmt.Errorf("RepoRoot no longer exists: %s", rc.RepoRoot)
	}
	return nil
}

// GitOutput runs a git command in the beads repository and returns its output.
//
// This is a convenience wrapper around GitCmd that captures stdout.
// Returns an error if the command fails or produces no output.
//
// Pattern:
//
//	output, err := rc.GitOutput(ctx, "config", "--get", "beads.role")
//	if err != nil {
//	    // Config key not set or git error
//	}
func (rc *RepoContext) GitOutput(ctx context.Context, args ...string) (string, error) {
	cmd := rc.GitCmd(ctx, args...)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

// Role reads beads.role from git config (fresh each call, ~1ms).
//
// If BEADS_DIR is set (IsRedirected), returns Contributor implicitly
// because external repo mode always indicates a contributor workflow.
//
// Returns ("", false) if role is not configured and not redirected.
// The bool return indicates whether a role was determined.
func (rc *RepoContext) Role() (UserRole, bool) {
	// BEADS_DIR implies contributor (external repo mode)
	if rc.IsRedirected {
		return Contributor, true
	}

	output, err := rc.GitOutput(context.Background(), "config", "--get", "beads.role")
	if err != nil {
		return "", false // Not configured
	}
	return UserRole(strings.TrimSpace(output)), true
}

// IsContributor returns true if user is configured as contributor.
//
// This includes both explicit configuration (git config beads.role contributor)
// and implicit detection (BEADS_DIR redirect active).
func (rc *RepoContext) IsContributor() bool {
	role, ok := rc.Role()
	return ok && role == Contributor
}

// IsMaintainer returns true if user is configured as maintainer.
//
// Only returns true for explicit configuration (git config beads.role maintainer).
// BEADS_DIR redirect always implies contributor, never maintainer.
func (rc *RepoContext) IsMaintainer() bool {
	role, ok := rc.Role()
	return ok && role == Maintainer
}

// RequireRole returns error if role not configured (forces init prompt).
//
// Use this at command entry points that need role-aware behavior.
// If BEADS_DIR is set, role is implicitly determined (contributor),
// so this will not return an error.
func (rc *RepoContext) RequireRole() error {
	if _, ok := rc.Role(); !ok {
		return ErrRoleNotConfigured
	}
	return nil
}
