// Package beads provides a minimal public API for extending bd with custom orchestration.
//
// Most extensions should use direct SQL queries against bd's database.
// This package exports only the essential types and functions needed for
// Go-based extensions that want to use bd's storage layer programmatically.
//
// For a working extension example, see examples/bd-example-extension-go.
package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/git"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/backends"
	"github.com/jonbaldie/beads/internal/utils"
)

// CanonicalDatabaseName is the required database filename for all beads repositories
const CanonicalDatabaseName = "beads.db"

// RedirectFileName is the name of the file that redirects to another .beads directory
const RedirectFileName = "redirect"

// SourceDatabaseInfo contains the dolt_database name from a source .beads/metadata.json,
// preserved across a redirect so that the source directory's database identity is not
// lost when the redirect target has a different dolt_database.
//
// When a .beads/redirect points to a shared .beads directory that serves multiple
// databases, the source's metadata.json may specify a different dolt_database than
// the target's. This struct captures the source database name so callers can
// restore it after redirect resolution.
type SourceDatabaseInfo struct {
	// SourceDir is the original .beads directory (before redirect)
	SourceDir string
	// TargetDir is the resolved .beads directory (after redirect)
	TargetDir string
	// WasRedirected is true if a redirect was followed
	WasRedirected bool
	// SourceDatabase is dolt_database from the source metadata.json (raw field,
	// NOT the env-var-aware GetDoltDatabase()). Empty if no source metadata exists
	// or the source has no dolt_database configured.
	SourceDatabase string
}

// ResolveRedirect follows a .beads/redirect file and captures the source directory's
// dolt_database from metadata.json BEFORE following the redirect. This preserves
// the source database identity across redirects.
//
// The env var BEADS_DOLT_SERVER_DATABASE still takes highest priority (handled by
// GetDoltDatabase() in callers). This function only captures the raw config field
// so callers can use it as an override when the env var is not set.
//
// Returns SourceDatabaseInfo with WasRedirected=true if a redirect was followed,
// and SourceDatabase set to the source's dolt_database (if any).
func ResolveRedirect(beadsDir string) SourceDatabaseInfo {
	info := SourceDatabaseInfo{
		SourceDir: beadsDir,
		TargetDir: beadsDir,
	}

	// Read source metadata.json directly (NOT via configfile.Load which may trigger
	// Dolt connections or recursive FollowRedirect calls causing deadlocks).
	// We only need the raw dolt_database field.
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	if data, err := os.ReadFile(metadataPath); err == nil {
		var raw struct {
			DoltDatabase string `json:"dolt_database"`
		}
		if json.Unmarshal(data, &raw) == nil {
			info.SourceDatabase = raw.DoltDatabase
		}
	}

	// Follow redirect
	resolved := FollowRedirect(beadsDir)
	if resolved != beadsDir {
		info.WasRedirected = true
		info.TargetDir = resolved
	}

	return info
}

// FollowRedirect checks if a .beads directory contains a redirect file and follows it.
// If a redirect file exists, it returns the target .beads directory path.
// If no redirect exists or there's an error, it returns the original path unchanged.
//
// The redirect file should contain a single path (relative or absolute) to the target
// .beads directory. Relative paths are resolved from the parent directory of the
// original .beads directory (i.e., the project root).
//
// Redirect chains are not followed - only one level of redirection is supported.
// This prevents infinite loops and keeps the behavior predictable.
func FollowRedirect(beadsDir string) string {
	target := readRedirectTarget(beadsDir)
	if target == "" {
		return beadsDir
	}
	target = validateRedirectTarget(beadsDir, target)
	if target == "" {
		return beadsDir
	}
	warnRedirectChain(target)
	debugRedirect(beadsDir, target)
	return target
}

func readRedirectTarget(beadsDir string) string {
	data, err := os.ReadFile(filepath.Join(beadsDir, RedirectFileName))
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}

func validateRedirectTarget(beadsDir, target string) string {

	// Resolve relative paths from the parent of the .beads directory (project root)
	if !filepath.IsAbs(target) {
		projectRoot := filepath.Dir(beadsDir)
		target = filepath.Join(projectRoot, target)
	}

	// Canonicalize the target path and prefer a stable branch worktree when the
	// redirect points at a detached snapshot checkout.
	target = canonicalizeBeadsDirPath(target)

	// Verify the target exists and is a directory
	if !isBeadsDirectory(target) {
		// Invalid redirect target - fall back to original
		fmt.Fprintf(os.Stderr, "Warning: redirect target does not exist or is not a directory: %s\n", target)
		return ""
	}

	// Defense-in-depth (gastownhall/beads#4692): a redirect can end up
	// pointing at a directory with no database at all, e.g. a stray
	// worktree-depth redirect written by the "graceful server-to-embedded
	// fallback" path (related to the "bd worktree create" write-site removed
	// in #3051). Following such a redirect silently lands bd on an empty,
	// unrelated location and `bd list`/`bd show` report no issues even
	// though the real data is untouched elsewhere. docs/reference/advanced.md
	// ("Database Redirects") already documents the contract: "The target
	// directory must exist and contain a valid database" -- enforce that
	// here instead of trusting any redirect file blindly.
	//
	// This intentionally does NOT look at the source directory's own mode:
	// a server-mode source rig redirecting to a shared Gas Town root (each
	// supplying its own dolt_database via ResolveRedirect/fb51196f7) is a
	// documented, supported topology, not a staleness signal.
	//
	// hasBeadsProjectFiles treats bare presence of metadata.json in the
	// target as sufficient, even if it later fails to parse: a
	// present-but-corrupt metadata.json is a config problem, not a
	// missing-database problem, and store_factory.go's
	// newDoltStoreFromConfig already hard-errors loudly on an unloadable
	// metadata.json rather than silently falling back to the embedded store.
	if !hasBeadsProjectFiles(target) {
		warnInvalidRedirectTargetOnce(beadsDir, target)
		return ""
	}
	return target
}

func warnRedirectChain(target string) {
	targetRedirect := filepath.Join(target, RedirectFileName)
	if _, err := os.Stat(targetRedirect); err == nil {
		fmt.Fprintf(os.Stderr, "Warning: redirect chains not allowed, ignoring redirect in %s\n", target)
	}
}

func debugRedirect(beadsDir, target string) {
	if os.Getenv("BD_DEBUG_ROUTING") != "" {
		fmt.Fprintf(os.Stderr, "[routing] Followed redirect from %s -> %s\n", beadsDir, target)
	}
}

// invalidRedirectTargetWarned tracks source beadsDir paths that have already
// received the "ignoring redirect: invalid target" warning in this process,
// so warnInvalidRedirectTargetOnce doesn't spam stderr. FollowRedirect is
// called many times per bd invocation (CWD walk, routing, worktree
// fallbacks), all for the same source directory.
var invalidRedirectTargetWarned sync.Map

// warnInvalidRedirectTargetOnce emits the invalid-redirect-target warning at
// most once per source beadsDir per process.
func warnInvalidRedirectTargetOnce(beadsDir, target string) {
	if _, alreadyWarned := invalidRedirectTargetWarned.LoadOrStore(beadsDir, struct{}{}); alreadyWarned {
		return
	}
	fmt.Fprintf(os.Stderr, "Warning: ignoring redirect from %s to %s because the target has no database or metadata.json; fix or delete the redirect file\n", beadsDir, target)
}

func canonicalizeBeadsDirPath(beadsDir string) string {
	canonical := utils.CanonicalizePath(beadsDir)
	if stable := preferStableBranchWorktreeBeadsDir(canonical); stable != "" {
		return stable
	}
	return canonical
}

type worktreeInfo struct {
	Path     string
	Head     string
	Branch   string
	Detached bool
	Bare     bool
}

func preferStableBranchWorktreeBeadsDir(beadsDir string) string {
	if filepath.Base(beadsDir) != ".beads" {
		return ""
	}

	repoRoot := filepath.Dir(beadsDir)
	if !isDetachedCommitWorktreePath(repoRoot) {
		return ""
	}

	branch, err := gitOutput(repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch != "HEAD" {
		return ""
	}

	head, err := gitOutput(repoRoot, "rev-parse", "HEAD")
	if err != nil || head == "" {
		return ""
	}

	worktrees, err := listWorktrees(repoRoot)
	if err != nil {
		return ""
	}

	candidates := stableBranchWorktreeCandidates(worktrees, head, repoRoot)
	if len(candidates) == 0 {
		return ""
	}
	return stableBranchWorktreeBeadsDir(candidates)
}

func stableBranchWorktreeCandidates(worktrees []worktreeInfo, head, repoRoot string) []worktreeInfo {
	var candidates []worktreeInfo
	for _, wt := range worktrees {
		if isStableBranchWorktreeCandidate(wt, head, repoRoot) {
			candidates = append(candidates, wt)
		}
	}
	return candidates
}

func isStableBranchWorktreeCandidate(wt worktreeInfo, head, repoRoot string) bool {
	return !wt.Bare && !wt.Detached && wt.Branch != "" && wt.Head == head && !utils.PathsEqual(wt.Path, repoRoot)
}

func stableBranchWorktreeBeadsDir(candidates []worktreeInfo) string {
	sort.Slice(candidates, func(i, j int) bool {
		iStable := !isDetachedCommitWorktreePath(candidates[i].Path)
		jStable := !isDetachedCommitWorktreePath(candidates[j].Path)
		if iStable != jStable {
			return iStable
		}
		return candidates[i].Path < candidates[j].Path
	})

	stableBeadsDir := filepath.Join(candidates[0].Path, ".beads")
	if isBeadsDirectory(stableBeadsDir) {
		return utils.CanonicalizePath(stableBeadsDir)
	}

	return ""
}

// isDetachedCommitWorktreePath checks if a path follows the megarepo convention
// of placing detached worktrees under refs/commits/<sha>.
func isDetachedCommitWorktreePath(path string) bool {
	return strings.Contains(filepath.ToSlash(path), "/refs/commits/")
}

func gitOutput(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // args are internal, not user-supplied
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func listWorktrees(repoRoot string) ([]worktreeInfo, error) {
	output, err := gitOutput(repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktrees(output), nil
}

func parseWorktrees(output string) []worktreeInfo {
	var worktrees []worktreeInfo
	var current *worktreeInfo

	for _, line := range strings.Split(output, "\n") {
		current = parseWorktreeLine(line, current, &worktrees)
	}

	if current != nil {
		worktrees = append(worktrees, *current)
	}

	return worktrees
}

func parseWorktreeLine(line string, current *worktreeInfo, worktrees *[]worktreeInfo) *worktreeInfo {
	switch {
	case strings.HasPrefix(line, "worktree "):
		if current != nil {
			*worktrees = append(*worktrees, *current)
		}
		return &worktreeInfo{Path: strings.TrimPrefix(line, "worktree ")}
	case current == nil:
		return nil
	case strings.HasPrefix(line, "HEAD "):
		current.Head = strings.TrimPrefix(line, "HEAD ")
	case strings.HasPrefix(line, "branch refs/heads/"):
		current.Branch = strings.TrimPrefix(line, "branch refs/heads/")
	case line == "detached":
		current.Detached = true
	case line == "bare":
		current.Bare = true
	}
	return current
}

// RedirectInfo contains information about a beads directory redirect.
type RedirectInfo struct {
	// IsRedirected is true if the local .beads has a redirect file
	IsRedirected bool
	// LocalDir is the local .beads directory (the one with the redirect file)
	LocalDir string
	// TargetDir is the actual .beads directory being used (after following redirect)
	TargetDir string
}

// GetRedirectInfo checks if the current beads directory is redirected.
// It searches for the local .beads/ directory and checks if it contains a redirect file.
// Returns RedirectInfo with IsRedirected=true if a redirect is active.
//
// bd-wayc3: This function now also checks the git repo's local .beads directory even when
// BEADS_DIR is set. This handles the case where BEADS_DIR is pre-set to the redirect target
// (e.g., by shell environment or tooling), but we still need to detect that a redirect exists.
func GetRedirectInfo() RedirectInfo {
	// First, always check the git repo's local .beads directory for redirects
	// This handles the case where BEADS_DIR is pre-set to the redirect target
	if localBeadsDir := findLocalBdsDirInRepo(); localBeadsDir != "" {
		if info := checkRedirectInDir(localBeadsDir); info.IsRedirected {
			return info
		}
	}

	// Fall back to original logic for non-git-repo cases
	if localBeadsDir := findLocalBeadsDir(); localBeadsDir != "" {
		return checkRedirectInDir(localBeadsDir)
	}

	return RedirectInfo{}
}

// checkRedirectInDir checks if a beads directory has a redirect file and returns redirect info.
// Returns RedirectInfo with IsRedirected=true if a valid redirect exists.
func checkRedirectInDir(beadsDir string) RedirectInfo {
	info := RedirectInfo{LocalDir: beadsDir}

	// Check if this directory has a redirect file
	redirectFile := filepath.Join(beadsDir, RedirectFileName)
	if _, err := os.Stat(redirectFile); err != nil {
		// No redirect file
		return info
	}

	// There's a redirect - find the target
	targetDir := FollowRedirect(beadsDir)
	if targetDir == beadsDir {
		// Redirect file exists but failed to resolve (invalid target)
		return info
	}

	info.IsRedirected = true
	info.TargetDir = targetDir
	return info
}

// findLocalBdsDirInRepo finds the .beads directory relative to the git repo root.
// This ignores BEADS_DIR to find the "true local" .beads for redirect detection.
// bd-wayc3: Added to detect redirects even when BEADS_DIR is pre-set.
func findLocalBdsDirInRepo() string {
	// Get git repo root
	repoRoot := git.GetRepoRoot()
	if repoRoot == "" {
		return ""
	}

	beadsDir := filepath.Join(repoRoot, ".beads")
	if info, err := os.Stat(beadsDir); err == nil && info.IsDir() {
		return beadsDir
	}

	return ""
}

// findLocalBeadsDir finds the local .beads directory without following redirects.
// This is used to detect if a redirect is configured.
func findLocalBeadsDir() string {
	if beadsDir := os.Getenv("BEADS_DIR"); beadsDir != "" {
		return canonicalizeBeadsDirPath(beadsDir)
	}
	if beadsDir := findLocalWorktreeBeadsDir(); beadsDir != "" {
		return beadsDir
	}
	if beadsDir := findLocalFallbackBeadsDir(); beadsDir != "" {
		return beadsDir
	}
	return findLocalBeadsDirByWalking()
}

// For worktrees, check worktree-local redirect first (per-worktree override).
// Returns the raw worktree .beads dir (not the resolved target) since
// findLocalBeadsDir doesn't follow redirects — callers use FollowRedirect.
func findLocalWorktreeBeadsDir() string {
	if !git.IsWorktree() {
		return ""
	}
	root := git.GetRepoRoot()
	if root == "" {
		return ""
	}
	worktreeBeadsDir := filepath.Join(root, ".beads")
	if _, err := os.Stat(filepath.Join(worktreeBeadsDir, "redirect")); err == nil {
		return worktreeBeadsDir
	}
	if isBeadsDirectory(worktreeBeadsDir) && hasBeadsProjectFiles(worktreeBeadsDir) {
		return worktreeBeadsDir
	}
	return ""
}

func findLocalFallbackBeadsDir() string {
	beadsDir := GetWorktreeFallbackBeadsDir()
	if beadsDir != "" && isBeadsDirectory(beadsDir) {
		return beadsDir
	}
	return ""
}

func findLocalBeadsDirByWalking() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	for dir := cwd; dir != "/" && dir != "."; {
		beadsDir := filepath.Join(dir, ".beads")
		if isBeadsDirectory(beadsDir) {
			return beadsDir
		}

		// Move up one directory
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached filesystem root (works on both Unix and Windows)
			// On Unix: filepath.Dir("/") returns "/"
			// On Windows: filepath.Dir("C:\\") returns "C:\\"
			break
		}
		dir = parent
	}

	return ""
}

// findDatabaseInBeadsDir searches for a database within a .beads directory.
// Checks metadata.json for the selected implementation's database path. For
// server mode, no local path needs to exist yet: authoritative
// metadata is enough to route the caller without falling through to Dolt.
// Embedded Dolt checks both embeddeddolt/ and the legacy dolt/ path. Returns
// empty string if no database is configured or found.
func findDatabaseInBeadsDir(beadsDir string, _ bool) string {
	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		// A present but unreadable/invalid metadata file is authoritative. Do
		// not bypass it by discovering a leftover local Dolt directory; callers
		// that need the detailed error should use OpenBestAvailable.
		return ""
	}
	if cfg != nil {
		return findConfiguredDatabase(beadsDir, cfg)
	}
	return findUnconfiguredDatabase(beadsDir)
}

func findConfiguredDatabase(beadsDir string, cfg *configfile.Config) string {
	if !configfile.IsSupportedBackend(cfg.Backend) {
		return ""
	}
	if backends.WorkspaceIsBeadsDir(cfg.GetBackend()) {
		return beadsDir
	}
	if cfg.IsDoltServerMode() {
		return cfg.DatabasePath(beadsDir)
	}
	if databasePath := existingDatabaseDirectory(filepath.Join(beadsDir, "embeddeddolt")); databasePath != "" {
		return databasePath
	}
	return existingDatabaseDirectory(cfg.DatabasePath(beadsDir))
}

func findUnconfiguredDatabase(beadsDir string) string {
	embeddedPath := filepath.Join(beadsDir, "embeddeddolt")
	if databasePath := existingDatabaseDirectory(embeddedPath); databasePath != "" {
		return databasePath
	}
	return existingDatabaseDirectory(filepath.Join(beadsDir, "dolt"))
}

func existingDatabaseDirectory(path string) string {
	if isBeadsDirectory(path) {
		return path
	}
	return ""
}

// Storage provides the minimal interface for extension orchestration
type Storage = storage.Storage

// Transaction provides atomic multi-operation support within a database transaction.
// Use Storage.RunInTransaction() to obtain a Transaction instance.
type Transaction = storage.Transaction
