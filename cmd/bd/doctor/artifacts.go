package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ArtifactFinding represents a single detected artifact that may need cleanup.
type ArtifactFinding struct {
	Path        string // Absolute path to the artifact
	Type        string // "jsonl", "sqlite", "cruft-beads", "redirect"
	Description string // Human-readable description
	SafeDelete  bool   // Whether this is safe to delete without data loss
}

// ArtifactReport contains all findings from an artifact scan.
type ArtifactReport struct {
	SQLiteArtifacts []ArtifactFinding
	CruftBeadsDirs  []ArtifactFinding
	RedirectIssues  []ArtifactFinding
	TotalCount      int
	SafeDeleteCount int
}

// CheckClassicArtifacts scans for beads classic artifacts that should be cleaned up
// after the Dolt migration. This includes stale JSONL files in dolt-native directories,
// leftover SQLite database files, cruft .beads directories that should be redirect-only,
// and invalid redirect files.
//
// The scan is rooted at the given path and looks for .beads/ directories recursively,
// checking each for artifacts that indicate incomplete migration cleanup.
func CheckClassicArtifacts(path string) DoctorCheck {
	report, err := ScanForArtifacts(path)
	if err != nil {
		return DoctorCheck{
			Name:     "Classic Artifacts",
			Status:   StatusWarning,
			Message:  "Artifact scan failed",
			Detail:   err.Error(),
			Category: CategoryMaintenance,
		}
	}

	if report.TotalCount == 0 {
		return DoctorCheck{
			Name:     "Classic Artifacts",
			Status:   StatusOK,
			Message:  "No classic artifacts detected",
			Category: CategoryMaintenance,
		}
	}

	// Build summary message
	var parts []string
	if len(report.SQLiteArtifacts) > 0 {
		parts = append(parts, fmt.Sprintf("%d SQLite artifact(s)", len(report.SQLiteArtifacts)))
	}
	if len(report.CruftBeadsDirs) > 0 {
		parts = append(parts, fmt.Sprintf("%d cruft .beads dir(s)", len(report.CruftBeadsDirs)))
	}
	if len(report.RedirectIssues) > 0 {
		parts = append(parts, fmt.Sprintf("%d redirect issue(s)", len(report.RedirectIssues)))
	}

	msg := strings.Join(parts, ", ")

	// Build detail showing examples
	var details []string
	for _, findings := range [][]ArtifactFinding{
		report.SQLiteArtifacts,
		report.CruftBeadsDirs, report.RedirectIssues,
	} {
		for i, f := range findings {
			if i >= 3 {
				details = append(details, fmt.Sprintf("  ... and %d more %s artifact(s)", len(findings)-3, f.Type))
				break
			}
			details = append(details, fmt.Sprintf("  %s: %s", f.Path, f.Description))
		}
	}

	return DoctorCheck{
		Name:     "Classic Artifacts",
		Status:   StatusWarning,
		Message:  msg,
		Detail:   strings.Join(details, "\n"),
		Fix:      "Run 'bd doctor --fix' to clean up, or 'bd doctor --check=artifacts' for details",
		Category: CategoryMaintenance,
	}
}

// ScanForArtifacts performs a recursive scan of the given path for classic beads artifacts.
// Returns an error if the walk itself fails (e.g., root path doesn't exist or is inaccessible).
// Individual unreadable subdirectories are skipped without error.
func ScanForArtifacts(rootPath string) (ArtifactReport, error) {
	var report ArtifactReport
	walkErr := filepath.Walk(rootPath, walkArtifactPath(&report))
	if walkErr != nil {
		return report, fmt.Errorf("scanning artifacts at %s: %w", rootPath, walkErr)
	}
	tallyArtifactReport(&report)
	return report, nil
}

func walkArtifactPath(report *ArtifactReport) filepath.WalkFunc {
	return func(path string, info os.FileInfo, err error) error {
		return visitArtifactPath(report, path, info, err)
	}
}

func visitArtifactPath(report *ArtifactReport, path string, info os.FileInfo, err error) error {
	if err != nil {
		return nil // Skip directories we can't read
	}
	base := filepath.Base(path)
	if base == ".git" && info.IsDir() {
		scanGitWorktreeArtifacts(path, report)
		return filepath.SkipDir
	}
	if skipArtifactWalkDir(info, base) {
		return filepath.SkipDir
	}
	if !info.IsDir() || base != ".beads" {
		return nil
	}
	scanBeadsDir(path, report)
	return filepath.SkipDir
}

func skipArtifactWalkDir(info os.FileInfo, base string) bool {
	return info.IsDir() && (base == "node_modules" || base == "vendor" || base == "__pycache__")
}

func tallyArtifactReport(report *ArtifactReport) {
	report.TotalCount = len(report.SQLiteArtifacts) +
		len(report.CruftBeadsDirs) + len(report.RedirectIssues)
	for _, findings := range [][]ArtifactFinding{
		report.SQLiteArtifacts,
		report.CruftBeadsDirs, report.RedirectIssues,
	} {
		for _, f := range findings {
			if f.SafeDelete {
				report.SafeDeleteCount++
			}
		}
	}
}

// scanGitWorktreeArtifacts scans the git-managed worktree area only.
// This avoids traversing the entire .git directory tree, which can be large and
// is unrelated to classic beads artifact cleanup.
func scanGitWorktreeArtifacts(gitDir string, report *ArtifactReport) {
	worktreesDir := filepath.Join(gitDir, "beads-worktrees")
	info, err := os.Stat(worktreesDir)
	if err != nil || !info.IsDir() {
		return
	}

	_ = filepath.Walk(worktreesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() || filepath.Base(path) != ".beads" {
			return nil
		}
		scanBeadsDir(path, report)
		return filepath.SkipDir
	})
}

// scanBeadsDir checks a single .beads directory for artifacts.
func scanBeadsDir(beadsDir string, report *ArtifactReport) {
	// Check if this should be a redirect-only directory
	isRedirectExpected := isRedirectExpectedDir(beadsDir)

	// Check if it has a redirect file
	hasRedirect := hasRedirectFile(beadsDir)

	// 1. Check for SQLite artifacts
	scanSQLiteArtifacts(beadsDir, report)

	// 2. Check for cruft .beads directories (should be redirect-only)
	if isRedirectExpected {
		scanCruftBeadsDir(beadsDir, report)
	}

	// 3. Validate redirect files
	if hasRedirect {
		validateRedirect(beadsDir, report)
	}
}

// isRedirectExpectedDir returns true if this .beads directory should contain
// only a redirect file (i.e., it's in a worktree or orchestrator subdirectory).
// NOTE: The polecats/crew/refinery patterns are retained for backwards compatibility
// with existing orchestrator installations.
func isRedirectExpectedDir(beadsDir string) bool {
	// The parent of .beads is the project dir
	// We need to determine if this is a "leaf" .beads that should redirect
	// to a "canonical" .beads (typically in the main rig or main worktree)

	parent := filepath.Dir(beadsDir)
	parentName := filepath.Base(parent)
	grandparent := filepath.Dir(parent)
	grandparentName := filepath.Base(grandparent)

	// Pattern: */polecats/*/.beads/ (orchestrator worker worktree — backwards compat)
	if grandparentName == "polecats" {
		return true
	}

	// Pattern: */crew/*/.beads/ (orchestrator assistant workspace — backwards compat)
	if grandparentName == "crew" {
		return true
	}

	// Pattern: */refinery/rig/.beads/ (orchestrator processor — backwards compat)
	if parentName == "rig" && grandparentName == "refinery" {
		return true
	}

	// Pattern: .git/beads-worktrees/*/.beads/
	if grandparentName == "beads-worktrees" {
		return true
	}

	// Check if this is a rig-root .beads/ (e.g., my-project/.beads/)
	// that should redirect to mayor/rig/.beads/
	// A rig-root .beads has a sibling "mayor/" or "polecats/" directory
	if hasSibling(parent, "mayor") || hasSibling(parent, "polecats") {
		// This looks like a rig root, check if there's a canonical location
		canonicalDir := filepath.Join(parent, "mayor", "rig", ".beads")
		if _, err := os.Stat(canonicalDir); err == nil {
			return true
		}
	}

	return false
}

// hasSibling returns true if the directory has a sibling with the given name.
func hasSibling(dir string, siblingName string) bool {
	sibling := filepath.Join(dir, siblingName)
	info, err := os.Stat(sibling)
	return err == nil && info.IsDir()
}

// hasRedirectFile returns true if the .beads directory has a redirect file.
func hasRedirectFile(beadsDir string) bool {
	_, err := os.Stat(filepath.Join(beadsDir, "redirect"))
	return err == nil
}

// scanSQLiteArtifacts checks for leftover SQLite database files.
// Only flags SQLite files as artifacts if Dolt is the active backend.
// If SQLite is still the active backend, beads.db is the live database.
func scanSQLiteArtifacts(beadsDir string, report *ArtifactReport) {
	if !IsDoltBackend(beadsDir) {
		return
	}
	entries, err := os.ReadDir(beadsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if finding, ok := sqliteArtifactFinding(beadsDir, entry); ok {
			report.SQLiteArtifacts = append(report.SQLiteArtifacts, finding)
		}
	}
}

func sqliteArtifactFinding(beadsDir string, entry os.DirEntry) (ArtifactFinding, bool) {
	if entry.IsDir() {
		return ArtifactFinding{}, false
	}
	name := entry.Name()
	if name == "beads.db" || name == "beads.db-shm" || name == "beads.db-wal" {
		return ArtifactFinding{
			Path:        filepath.Join(beadsDir, name),
			Type:        "sqlite",
			Description: "SQLite database file (Dolt is active backend)",
			SafeDelete:  name == "beads.db-shm" || name == "beads.db-wal",
		}, true
	}
	if strings.HasPrefix(name, "beads.backup-") && strings.HasSuffix(name, ".db") {
		return ArtifactFinding{
			Path:        filepath.Join(beadsDir, name),
			Type:        "sqlite",
			Description: "pre-migration backup",
			SafeDelete:  true,
		}, true
	}
	return ArtifactFinding{}, false
}

// scanCruftBeadsDir checks if a .beads directory that should be redirect-only
// contains extra files beyond the redirect file.
func scanCruftBeadsDir(beadsDir string, report *ArtifactReport) {
	entries, err := os.ReadDir(beadsDir)
	if err != nil {
		return
	}

	// Count non-redirect entries
	var extraFiles []string
	for _, entry := range entries {
		name := entry.Name()
		// redirect file is expected
		if name == "redirect" {
			continue
		}
		// .gitkeep is harmless
		if name == ".gitkeep" {
			continue
		}
		extraFiles = append(extraFiles, name)
	}

	if len(extraFiles) == 0 {
		return
	}

	desc := fmt.Sprintf("should be redirect-only but contains: %s", strings.Join(extraFiles, ", "))
	if len(extraFiles) > 5 {
		desc = fmt.Sprintf("should be redirect-only but contains %d extra files", len(extraFiles))
	}

	report.CruftBeadsDirs = append(report.CruftBeadsDirs, ArtifactFinding{
		Path:        beadsDir,
		Type:        "cruft-beads",
		Description: desc,
		SafeDelete:  true, // Safe: location is redirect-expected, extra files are cruft
	})
}

// validateRedirect checks that a redirect file points to a valid target.
func validateRedirect(beadsDir string, report *ArtifactReport) {
	redirectPath := filepath.Join(beadsDir, "redirect")
	target, ok := readRedirectTarget(redirectPath, report)
	if !ok {
		return
	}
	checkRedirectTarget(redirectPath, beadsDir, target, report)
}

func appendRedirectIssue(report *ArtifactReport, redirectPath, description string) {
	report.RedirectIssues = append(report.RedirectIssues, ArtifactFinding{
		Path:        redirectPath,
		Type:        "redirect",
		Description: description,
		SafeDelete:  false,
	})
}

func readRedirectTarget(redirectPath string, report *ArtifactReport) (string, bool) {
	data, err := os.ReadFile(redirectPath) // #nosec G304 - path constructed from walked dir
	if err != nil {
		appendRedirectIssue(report, redirectPath, "redirect file unreadable")
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line, true
		}
	}
	appendRedirectIssue(report, redirectPath, "redirect file is empty")
	return "", false
}

func checkRedirectTarget(redirectPath, beadsDir, target string, report *ArtifactReport) {
	resolvedTarget := target
	if !filepath.IsAbs(target) {
		resolvedTarget = filepath.Join(filepath.Dir(beadsDir), target)
	}
	info, err := os.Stat(resolvedTarget)
	if err != nil {
		appendRedirectIssue(report, redirectPath, fmt.Sprintf("redirect target does not exist: %s", target))
		return
	}
	if !info.IsDir() {
		appendRedirectIssue(report, redirectPath, fmt.Sprintf("redirect target is not a directory: %s", target))
		return
	}
	// gastownhall/beads#4692: FollowRedirect ignores a redirect whose target
	// has no metadata.json and no recognizable database (a stray
	// worktree-depth redirect, e.g. a relic of the "bd worktree create"
	// write-site removed in #3051, can point past the real .beads dir into
	// an empty location). Flag that here too as an actionable warning, not
	// SafeDelete cruft -- the fix is to correct or remove the redirect file,
	// not to delete anything automatically.
	if !hasDatabaseOrMetadata(resolvedTarget) {
		appendRedirectIssue(report, redirectPath, fmt.Sprintf("redirect target has no database or metadata.json (ignored by bd): %s", target))
	}
}

// hasDatabaseOrMetadata reports whether dir contains a metadata.json or a
// recognizable Dolt/SQLite database. Mirrors the target-validity check
// FollowRedirect (internal/beads) applies before honoring a redirect;
// duplicated locally (rather than importing internal/beads) to keep this
// package's existing pattern of small, self-contained filesystem checks
// (see isRedirectExpectedDir/hasRedirectFile above).
func hasDatabaseOrMetadata(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "metadata.json")); err == nil {
		return true
	}
	if info, err := os.Stat(filepath.Join(dir, "dolt")); err == nil && info.IsDir() {
		return true
	}
	if info, err := os.Stat(filepath.Join(dir, "embeddeddolt")); err == nil && info.IsDir() {
		return true
	}
	dbMatches, _ := filepath.Glob(filepath.Join(dir, "*.db"))
	for _, match := range dbMatches {
		baseName := filepath.Base(match)
		if !strings.Contains(baseName, ".backup") && baseName != "vc.db" {
			return true
		}
	}
	return false
}
