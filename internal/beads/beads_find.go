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
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/git"
	"github.com/jonbaldie/beads/internal/utils"
)

// FindDatabasePath discovers the bd database path using bd's standard search order:
//  1. $BEADS_DIR environment variable (points to .beads directory)
//  2. $BEADS_DB environment variable (points directly to database file, deprecated)
//  3. .beads/*.db in current directory or ancestors
//
// Redirect files are supported: if a .beads/redirect file exists, its contents
// are used as the actual .beads directory path.
//
// Returns empty string if no database is found.
func FindDatabasePath() string {
	// 1. Check BEADS_DIR environment variable (preferred)
	if beadsDir := os.Getenv("BEADS_DIR"); beadsDir != "" {
		// Canonicalize the path to prevent nested .beads directories
		absBeadsDir := canonicalizeBeadsDirPath(beadsDir)

		// Follow redirect if present
		absBeadsDir = FollowRedirect(absBeadsDir)

		// Use helper to find database (no warnings for BEADS_DIR - user explicitly set it)
		if dbPath := findDatabaseInBeadsDir(absBeadsDir, false); dbPath != "" {
			return dbPath
		}

		// BEADS_DIR is set but no database found - this is OK for --no-db mode
		// Return empty string and let the caller handle it
		return ""
	}

	// 2. Check BEADS_DB environment variable (deprecated but still supported)
	if envDB := os.Getenv("BEADS_DB"); envDB != "" {
		absDB := utils.CanonicalizePath(envDB)
		// If BEADS_DB points to a directory rather than a file, treat it
		// like BEADS_DIR to avoid filepath.Dir() resolving one level too
		// high in the caller (cmd/bd/main.go). See GH#2548.
		if info, err := os.Stat(absDB); err == nil && info.IsDir() {
			if dbPath := findDatabaseInBeadsDir(absDB, false); dbPath != "" {
				return dbPath
			}
		}
		return absDB
	}

	// 3. Search for .beads/*.db in current directory and ancestors
	if foundDB := findDatabaseInTree(); foundDB != "" {
		return utils.CanonicalizePath(foundDB)
	}

	// No fallback to ~/.beads - return empty string
	return ""
}

// FindBeadsDirFrom finds the effective .beads/ directory as if discovery
// started from startDir, without changing the process working directory.
func FindBeadsDirFrom(startDir string) string {
	startDir, ok := canonicalDirectory(startDir)
	if !ok {
		return ""
	}
	repoRoot := repoRootFrom(startDir)
	jjSecondaryRoot, jjPrimaryBeadsDir, jjPrimaryHasDB := jjBeadsDirFrom(startDir)
	fallbackBeadsDir, fallbackHasDB := fallbackBeadsDirFrom(repoRoot)
	if beadsDir := walkBeadsDirFrom(startDir, repoRoot, jjSecondaryRoot, fallbackHasDB, jjPrimaryHasDB); beadsDir != "" {
		return beadsDir
	}
	if beadsDir := validFallbackBeadsDir(fallbackBeadsDir); beadsDir != "" {
		return beadsDir
	}
	return jjPrimaryBeadsDir
}

func canonicalDirectory(path string) (string, bool) {
	if path == "" || !isBeadsDirectory(path) {
		return "", false
	}
	return utils.CanonicalizePath(path), true
}

func repoRootFrom(startDir string) string {
	out, err := gitOutput(startDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return utils.CanonicalizePath(out)
}

func jjBeadsDirFrom(startDir string) (string, string, bool) {
	root, ok := git.JJSecondaryWorkspaceRootFrom(startDir)
	if !ok {
		return "", "", false
	}
	secondaryRoot := utils.CanonicalizePath(root)
	primaryRoot, err := git.GetJJPrimaryWorkspaceRootFrom(startDir)
	if err != nil || primaryRoot == "" {
		return secondaryRoot, "", false
	}
	primaryBeadsDir := filepath.Join(primaryRoot, ".beads")
	if !isBeadsDirectory(primaryBeadsDir) {
		return secondaryRoot, "", false
	}
	resolved := FollowRedirect(primaryBeadsDir)
	if !hasBeadsProjectFiles(resolved) {
		return secondaryRoot, "", false
	}
	return secondaryRoot, resolved, hasBeadsDatabase(resolved)
}

func fallbackBeadsDirFrom(repoRoot string) (string, bool) {
	if repoRoot == "" {
		return "", false
	}
	fallbackBeadsDir := worktreeFallbackBeadsDirForRepo(repoRoot)
	if fallbackBeadsDir == "" || !isBeadsDirectory(fallbackBeadsDir) {
		return fallbackBeadsDir, false
	}
	return fallbackBeadsDir, hasBeadsDatabase(FollowRedirect(fallbackBeadsDir))
}

func walkBeadsDirFrom(startDir, repoRoot, jjSecondaryRoot string, fallbackHasDB, jjPrimaryHasDB bool) string {
	for dir := startDir; dir != "/" && dir != "."; {
		if beadsDir := findBeadsDirFromAt(dir, repoRoot, jjSecondaryRoot, fallbackHasDB, jjPrimaryHasDB); beadsDir != "" {
			return beadsDir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func findBeadsDirFromAt(dir, repoRoot, jjSecondaryRoot string, fallbackHasDB, jjPrimaryHasDB bool) string {
	beadsDir := filepath.Join(dir, ".beads")
	if !isBeadsDirectory(beadsDir) {
		return ""
	}
	resolved := FollowRedirect(beadsDir)
	if inheritedBeadsDirAtRoot(dir, repoRoot, jjSecondaryRoot, resolved, fallbackHasDB, jjPrimaryHasDB) {
		return ""
	}
	if hasBeadsProjectFiles(resolved) {
		return resolved
	}
	return ""
}

func inheritedBeadsDirAtRoot(dir, repoRoot, jjSecondaryRoot, resolved string, fallbackHasDB, jjPrimaryHasDB bool) bool {
	worktreeRoot := repoRoot != "" && utils.PathsEqual(dir, repoRoot)
	jjSecondaryRootMatch := jjSecondaryRoot != "" && utils.PathsEqual(dir, jjSecondaryRoot)
	return (worktreeRoot && fallbackHasDB && !hasBeadsDatabase(resolved)) ||
		(jjSecondaryRootMatch && jjPrimaryHasDB && !hasBeadsDatabase(resolved))
}

func validFallbackBeadsDir(fallbackBeadsDir string) string {
	if !isBeadsDirectory(fallbackBeadsDir) {
		return ""
	}
	resolved := FollowRedirect(fallbackBeadsDir)
	if hasBeadsProjectFiles(resolved) {
		return resolved
	}
	return ""
}

// hasBeadsProjectFiles checks if a .beads directory contains actual project files.
// Returns true if the directory contains any of:
// - metadata.json or config.yaml (project configuration)
// - Any *.db file (excluding backups and vc.db)
// - A dolt/ directory (Dolt database)
//
// Returns false for directories that only contain legacy registry files.
// This prevents FindBeadsDir from returning ~/.beads/ which only has registry.json.
func hasBeadsProjectFiles(beadsDir string) bool {
	return hasBeadsConfiguration(beadsDir) || hasBeadsStorageDirectory(beadsDir) || hasBeadsDatabaseFile(beadsDir)
}

func hasBeadsConfiguration(beadsDir string) bool {
	if _, err := os.Stat(filepath.Join(beadsDir, "metadata.json")); err == nil {
		return true
	}
	return fileExists(filepath.Join(beadsDir, "config.yaml"))
}

func hasBeadsStorageDirectory(beadsDir string) bool {
	return isBeadsDirectory(filepath.Join(beadsDir, "dolt")) || isBeadsDirectory(filepath.Join(beadsDir, "embeddeddolt"))
}

func hasBeadsDatabaseFile(beadsDir string) bool {
	dbMatches, _ := filepath.Glob(filepath.Join(beadsDir, "*.db"))
	for _, match := range dbMatches {
		baseName := filepath.Base(match)
		if !strings.Contains(baseName, ".backup") && baseName != "vc.db" {
			return true
		}
	}

	return false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// hasBeadsDatabase is the strict counterpart to hasBeadsProjectFiles: it
// returns true only when beadsDir contains an actual database — a dolt/
// directory, an embeddeddolt/ directory, or a non-backup *.db file. Mere
// presence of metadata.json / config.yaml / issues.jsonl does not count.
//
// Used by FindBeadsDir's worktree-separate-DB branch to distinguish a
// genuine separate-database worktree (which owns its own Dolt data) from
// a worktree that has inherited tracked .beads/ artifacts through a git
// checkout of the parent repo's working-tree snapshot. Without this strict
// check, the separate-DB branch would match on inherited metadata.json and
// return a broken directory, short-circuiting the shared-DB fallback.
func hasBeadsDatabase(beadsDir string) bool {
	if info, err := os.Stat(filepath.Join(beadsDir, "dolt")); err == nil && info.IsDir() {
		return true
	}
	if info, err := os.Stat(filepath.Join(beadsDir, "embeddeddolt")); err == nil && info.IsDir() {
		return true
	}
	dbMatches, _ := filepath.Glob(filepath.Join(beadsDir, "*.db"))
	for _, match := range dbMatches {
		baseName := filepath.Base(match)
		if !strings.Contains(baseName, ".backup") && baseName != "vc.db" {
			return true
		}
	}
	return false
}

// FindBeadsDir finds the .beads/ directory in the current directory tree.
// Returns empty string if not found.
//
// Resolution order:
//  1. BEADS_DIR environment variable (highest priority)
//  2. Walk up from CWD toward repo root boundary, checking each directory
//     for .beads/ with valid project files. For worktrees, stops at the
//     worktree root; for non-worktrees, stops at the git root.
//  3. Worktree-specific fallback: per-worktree redirect, worktree's own
//     .beads (separate-DB mode), shared .beads via git-common-dir.
//  4. Extended walk from the boundary to the main repo root (worktrees)
//     or checks the git root itself (non-worktrees).
//
// Validates that directories contain actual project files (metadata.json,
// config.yaml, dolt/, embeddeddolt/, or *.db).
// Redirect files are supported: if a .beads/redirect file exists, its
// contents are used as the actual .beads directory path.
func FindBeadsDir() string {
	if beadsDir := findBeadsDirFromEnv(); beadsDir != "" {
		return beadsDir
	}

	search, ok := newBeadsDirSearchContext()
	if !ok {
		return ""
	}
	if beadsDir := findBeadsDirBeforeBoundary(search); beadsDir != "" {
		return beadsDir
	}
	if beadsDir := findBeadsDirFallback(&search); beadsDir != "" {
		return beadsDir
	}
	return findBeadsDirAfterBoundary(search)
}

func findBeadsDirFromEnv() string {
	beadsDir := os.Getenv("BEADS_DIR")
	if beadsDir == "" {
		return ""
	}
	return validBeadsDir(FollowRedirect(canonicalizeBeadsDirPath(beadsDir)))
}

func validBeadsDir(beadsDir string) string {
	info, err := os.Stat(beadsDir)
	if err == nil && info.IsDir() && hasBeadsProjectFiles(beadsDir) {
		return beadsDir
	}
	return ""
}

type beadsDirSearchContext struct {
	cwdCanonical          string
	gitRoot               string
	walkBoundary          string
	walkBoundaryCanonical string
	isWorktree            bool
	isJJSecondary         bool
	jjSecondaryRoot       string
	mainRepoRoot          string
}

func newBeadsDirSearchContext() (beadsDirSearchContext, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return beadsDirSearchContext{}, false
	}

	search := beadsDirSearchContext{
		gitRoot:    findGitRoot(),
		isWorktree: git.IsWorktree(),
	}
	if !search.isWorktree {
		search.jjSecondaryRoot, search.isJJSecondary = git.JJSecondaryWorkspaceRoot()
	}

	search.walkBoundary = search.gitRoot
	if search.isWorktree {
		search.walkBoundary = git.GetRepoRoot()
	} else if search.isJJSecondary {
		search.walkBoundary = search.jjSecondaryRoot
	}

	search.cwdCanonical = utils.CanonicalizePath(cwd)
	if search.walkBoundary != "" {
		search.walkBoundaryCanonical = utils.CanonicalizePath(search.walkBoundary)
	}
	return search, true
}

func findBeadsDirBeforeBoundary(search beadsDirSearchContext) string {
	for dir := search.cwdCanonical; dir != "/" && dir != "."; {
		if search.walkBoundaryCanonical != "" && dir == search.walkBoundaryCanonical {
			break
		}
		if beadsDir := findBeadsDirAt(dir); beadsDir != "" {
			return beadsDir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func findBeadsDirAt(dir string) string {
	beadsDir := filepath.Join(dir, ".beads")
	info, err := os.Stat(beadsDir)
	if err != nil || !info.IsDir() {
		return ""
	}
	return validBeadsDir(FollowRedirect(beadsDir))
}

func findBeadsDirFallback(search *beadsDirSearchContext) string {
	if search.isWorktree {
		return findWorktreeBeadsDirFallback(search)
	}
	if search.isJJSecondary {
		return findJJSecondaryBeadsDirFallback(search)
	}
	return ""
}

func findWorktreeBeadsDirFallback(search *beadsDirSearchContext) string {
	if beadsDir := findWorktreeRedirectBeadsDir(); beadsDir != "" {
		return beadsDir
	}
	if beadsDir := findWorktreeOwnBeadsDir(); beadsDir != "" {
		return beadsDir
	}
	if beadsDir := findWorktreeSharedBeadsDir(); beadsDir != "" {
		return beadsDir
	}

	mainRepoRoot, err := git.GetMainRepoRoot()
	if err != nil {
		mainRepoRoot = ""
	}
	search.mainRepoRoot = mainRepoRoot
	return ""
}

func findWorktreeRedirectBeadsDir() string {
	target := worktreeRedirectTarget()
	if target == "" {
		return ""
	}
	return validBeadsDir(target)
}

func findWorktreeOwnBeadsDir() string {
	worktreeRoot := git.GetRepoRoot()
	if worktreeRoot == "" {
		return ""
	}
	worktreeBeadsDir := filepath.Join(worktreeRoot, ".beads")
	info, err := os.Stat(worktreeBeadsDir)
	if err != nil || !info.IsDir() {
		return ""
	}
	if hasBeadsDatabase(worktreeBeadsDir) {
		return worktreeBeadsDir
	}
	if worktreeFallbackHasDatabase() {
		return ""
	}
	if hasBeadsProjectFiles(worktreeBeadsDir) {
		return worktreeBeadsDir
	}
	return ""
}

func worktreeFallbackHasDatabase() bool {
	fallback := GetWorktreeFallbackBeadsDir()
	if fallback == "" {
		return false
	}
	info, err := os.Stat(fallback)
	if err != nil || !info.IsDir() {
		return false
	}
	return hasBeadsDatabase(FollowRedirect(fallback))
}

func findWorktreeSharedBeadsDir() string {
	fallback := GetWorktreeFallbackBeadsDir()
	if fallback == "" {
		return ""
	}
	return validBeadsDir(FollowRedirect(fallback))
}

func findJJSecondaryBeadsDirFallback(search *beadsDirSearchContext) string {
	primaryRoot, primaryErr := git.GetJJPrimaryWorkspaceRoot()
	if beadsDir := findJJSecondaryOwnBeadsDir(search.jjSecondaryRoot, primaryRoot, primaryErr); beadsDir != "" {
		return beadsDir
	}
	if beadsDir := findJJPrimaryBeadsDir(search, primaryRoot, primaryErr); beadsDir != "" {
		return beadsDir
	}
	return ""
}

func findJJSecondaryOwnBeadsDir(secondaryRoot, primaryRoot string, primaryErr error) string {
	if secondaryRoot == "" {
		return ""
	}
	secondaryBeadsDir := filepath.Join(secondaryRoot, ".beads")
	if !isBeadsDirectory(secondaryBeadsDir) {
		return ""
	}
	if hasBeadsDatabase(secondaryBeadsDir) {
		return secondaryBeadsDir
	}
	if jjPrimaryFallbackHasDatabase(primaryRoot, primaryErr) {
		return ""
	}
	if hasBeadsProjectFiles(secondaryBeadsDir) {
		return secondaryBeadsDir
	}
	return ""
}

func isBeadsDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func jjPrimaryFallbackHasDatabase(primaryRoot string, primaryErr error) bool {
	if primaryErr != nil || primaryRoot == "" {
		return false
	}
	primaryBeadsDir := filepath.Join(primaryRoot, ".beads")
	if !isBeadsDirectory(primaryBeadsDir) {
		return false
	}
	return hasBeadsDatabase(FollowRedirect(primaryBeadsDir))
}

func findJJPrimaryBeadsDir(search *beadsDirSearchContext, primaryRoot string, primaryErr error) string {
	if primaryErr != nil || primaryRoot == "" {
		return ""
	}
	primaryBeadsDir := filepath.Join(primaryRoot, ".beads")
	if info, err := os.Stat(primaryBeadsDir); err == nil && info.IsDir() {
		resolved := FollowRedirect(primaryBeadsDir)
		if beadsDir := validBeadsDir(resolved); beadsDir != "" {
			return beadsDir
		}
	}
	search.mainRepoRoot = primaryRoot
	return ""
}

func findBeadsDirAfterBoundary(search beadsDirSearchContext) string {
	if search.walkBoundary == "" {
		return ""
	}
	return findBeadsDirThroughBoundary(
		search.walkBoundaryCanonical,
		beadsDirSearchExtendedRoot(search),
	)
}

func beadsDirSearchExtendedRoot(search beadsDirSearchContext) string {
	extendedRoot := search.gitRoot
	if (search.isWorktree || search.isJJSecondary) && search.mainRepoRoot != "" {
		extendedRoot = search.mainRepoRoot
	}
	if extendedRoot == "" {
		return ""
	}
	return utils.CanonicalizePath(extendedRoot)
}

func findBeadsDirThroughBoundary(start, root string) string {
	for dir := start; dir != "/" && dir != "."; {
		if beadsDir := findBeadsDirAt(dir); beadsDir != "" {
			return beadsDir
		}
		if root != "" && dir == root {
			break
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}
