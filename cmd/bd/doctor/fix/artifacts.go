package fix

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ClassicArtifacts removes beads classic artifacts found by scanning the path.
// Only removes artifacts that are safe to delete:
// - JSONL export artifacts (not issues.jsonl itself)
// - SQLite WAL/SHM files and backup databases
// - Extra files in redirect-only .beads directories
func ClassicArtifacts(path string) error {
	removed, skipped, errCount, err := walkClassicArtifacts(path)
	if err != nil {
		return err
	}
	fmt.Printf("  Artifact cleanup: %d removed, %d skipped, %d errors\n", removed, skipped, errCount)
	if skipped > 0 {
		fmt.Println("  Skipped items may need manual review (e.g., issues.jsonl in dolt dirs, beads.db files)")
	}
	if errCount > 0 {
		return fmt.Errorf("%d artifact(s) could not be removed", errCount)
	}
	return nil
}

func walkClassicArtifacts(path string) (removed, skipped, errCount int, err error) {
	err = filepath.Walk(path, func(walkPath string, info os.FileInfo, walkErr error) error {
		r, s, e, skip := visitClassicArtifactPath(walkPath, info, walkErr)
		removed += r
		skipped += s
		errCount += e
		return skip
	})
	if err != nil {
		return removed, skipped, errCount, fmt.Errorf("failed to walk directory tree: %w", err)
	}
	return removed, skipped, errCount, nil
}

func visitClassicArtifactPath(walkPath string, info os.FileInfo, err error) (removed, skipped, errCount int, walkErr error) {
	if err != nil {
		return 0, 0, 0, nil // Skip directories we can't read
	}
	base := filepath.Base(walkPath)
	if info.IsDir() && (base == "node_modules" || base == "vendor" || base == "__pycache__") {
		return 0, 0, 0, filepath.SkipDir
	}
	if !info.IsDir() || base != ".beads" {
		return 0, 0, 0, nil
	}
	r, s, e := cleanBeadsDirArtifacts(walkPath)
	return r, s, e, filepath.SkipDir
}

// cleanBeadsDirArtifacts cleans artifacts from a single .beads directory.
// Returns counts of removed, skipped, and errored items.
func cleanBeadsDirArtifacts(beadsDir string) (removed, skipped, errCount int) {
	hasDolt := hasDoltDir(beadsDir)
	isRedirectExpected := isRedirectExpectedLocation(beadsDir)

	// 1. Clean JSONL artifacts in dolt-native directories
	if hasDolt {
		r, s, e := cleanJSONLArtifacts(beadsDir)
		removed += r
		skipped += s
		errCount += e
	}

	// 2. Clean SQLite artifacts
	r, s, e := cleanSQLiteArtifacts(beadsDir)
	removed += r
	skipped += s
	errCount += e

	// 3. Clean cruft .beads directories (if redirect is expected)
	// Clean even when the redirect file is missing — stale cruft files
	// (config.yaml, metadata.json, README.md, issues.jsonl, etc.) prevent
	// the redirect from being created and should be removed regardless.
	if isRedirectExpected {
		r, e := cleanCruftBeadsDirFiles(beadsDir)
		removed += r
		errCount += e
	}

	return
}

// hasDoltDir returns true if the .beads directory contains a dolt/ subdirectory.
func hasDoltDir(beadsDir string) bool {
	info, err := os.Stat(getDatabasePath(beadsDir))
	return err == nil && info.IsDir()
}

// isRedirectExpectedLocation returns true if this .beads directory should contain
// only a redirect file.
func isRedirectExpectedLocation(beadsDir string) bool {
	parent := filepath.Dir(beadsDir)
	grandparent := filepath.Dir(parent)
	if isOrchestratorRedirectLocation(filepath.Base(parent), filepath.Base(grandparent)) {
		return true
	}
	return isRigRootRedirectLocation(parent)
}

func isOrchestratorRedirectLocation(parentName, grandparentName string) bool {
	switch grandparentName {
	case "polecats", "crew", "beads-worktrees":
		return true
	}
	return parentName == "rig" && grandparentName == "refinery"
}

func isRigRootRedirectLocation(parent string) bool {
	canonicalDir := filepath.Join(parent, "mayor", "rig", ".beads")
	if _, err := os.Stat(canonicalDir); err != nil {
		return false
	}
	for _, sibling := range []string{"mayor", "polecats"} {
		if info, err := os.Stat(filepath.Join(parent, sibling)); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// cleanJSONLArtifacts removes stale JSONL files from a dolt-native .beads directory.
func cleanJSONLArtifacts(beadsDir string) (removed, skipped, errCount int) {
	// Safe to delete (not the primary data source)
	safeFiles := []string{
		"issues.jsonl.new",
	}

	for _, name := range safeFiles {
		path := filepath.Join(beadsDir, name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := os.Remove(path); err != nil {
			fmt.Printf("  Error removing %s: %v\n", path, err)
			errCount++
			continue
		}
		fmt.Printf("  Removed: %s (JSONL artifact)\n", path)
		removed++
	}

	// interactions.jsonl - only remove if empty
	interPath := filepath.Join(beadsDir, "interactions.jsonl")
	if info, err := os.Stat(interPath); err == nil {
		if info.Size() == 0 {
			if err := os.Remove(interPath); err != nil {
				fmt.Printf("  Error removing %s: %v\n", interPath, err)
				errCount++
			} else {
				fmt.Printf("  Removed: %s (empty interactions log)\n", interPath)
				removed++
			}
		} else {
			fmt.Printf("  Skip (not empty): %s\n", interPath)
			skipped++
		}
	}

	// issues.jsonl in dolt-native directory - skip (needs manual review)
	issuesPath := filepath.Join(beadsDir, "issues.jsonl")
	if _, err := os.Stat(issuesPath); err == nil {
		fmt.Printf("  Skip (needs review): %s (issues.jsonl in dolt-native dir)\n", issuesPath)
		skipped++
	}

	return
}

// cleanSQLiteArtifacts removes leftover SQLite database files.
func cleanSQLiteArtifacts(beadsDir string) (removed, skipped, errCount int) {
	entries, err := os.ReadDir(beadsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		r, s, e := cleanOneSQLiteArtifact(beadsDir, entry)
		removed += r
		skipped += s
		errCount += e
	}
	return
}

func cleanOneSQLiteArtifact(beadsDir string, entry os.DirEntry) (removed, skipped, errCount int) {
	if entry.IsDir() {
		return
	}
	name := entry.Name()
	path := filepath.Join(beadsDir, name)
	switch {
	case name == "beads.db-shm" || name == "beads.db-wal":
		return removeSQLiteArtifact(path, "SQLite WAL/SHM")
	case name == "beads.db":
		fmt.Printf("  Skip (needs review): %s\n", path)
		return 0, 1, 0
	case strings.HasPrefix(name, "beads.backup-") && strings.HasSuffix(name, ".db"):
		return removeSQLiteArtifact(path, "pre-migration backup")
	}
	return
}

func removeSQLiteArtifact(path, kind string) (removed, skipped, errCount int) {
	if err := os.Remove(path); err != nil {
		fmt.Printf("  Error removing %s: %v\n", path, err)
		return 0, 0, 1
	}
	fmt.Printf("  Removed: %s (%s)\n", path, kind)
	return 1, 0, 0
}

// cleanCruftBeadsDirFiles removes everything from a .beads directory except
// the redirect file, .gitkeep, and (when a redirect file is present)
// metadata.json.
func cleanCruftBeadsDirFiles(beadsDir string) (removed, errCount int) {
	entries, err := os.ReadDir(beadsDir)
	if err != nil {
		return 0, 1
	}
	hasRedirect := cruftDirHasRedirect(entries)
	for _, entry := range entries {
		r, e := removeCruftBeadsEntry(beadsDir, entry, hasRedirect)
		removed += r
		errCount += e
	}
	return
}

func cruftDirHasRedirect(entries []os.DirEntry) bool {
	// gastownhall/beads#4692: a co-located redirect and metadata.json is a
	// documented, supported topology (see fb51196f7 / docs/reference/
	// advanced.md "Database Redirects" -- a server-mode source rig's own
	// metadata.json supplies its dolt_database while the redirect points at
	// the shared Gas Town root). Deleting metadata.json here would corrupt
	// that source rig's identity while leaving the redirect itself intact,
	// so never delete it when a redirect is present in the same directory.
	for _, entry := range entries {
		if entry.Name() == "redirect" {
			return true
		}
	}
	return false
}

func keepCruftBeadsEntry(name string, hasRedirect bool) bool {
	if name == "redirect" || name == ".gitkeep" {
		return true
	}
	return name == "metadata.json" && hasRedirect
}

func cruftEntryEscapes(beadsDir, entryPath string) bool {
	rel, err := filepath.Rel(beadsDir, entryPath)
	return err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func removeCruftBeadsEntry(beadsDir string, entry os.DirEntry, hasRedirect bool) (removed, errCount int) {
	name := entry.Name()
	if keepCruftBeadsEntry(name, hasRedirect) {
		return 0, 0
	}
	entryPath := filepath.Join(beadsDir, name)
	if cruftEntryEscapes(beadsDir, entryPath) {
		return 0, 0
	}
	var err error
	if entry.IsDir() {
		err = os.RemoveAll(entryPath)
	} else {
		err = os.Remove(entryPath)
	}
	if err != nil {
		fmt.Printf("  Error removing %s: %v\n", entryPath, err)
		return 0, 1
	}
	fmt.Printf("  Removed: %s (cruft in redirect-only dir)\n", entryPath)
	return 1, 0
}
