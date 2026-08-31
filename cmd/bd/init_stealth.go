package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/cmd/bd/doctor"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/ui"
)

// setupStealthMode configures git settings for stealth operation.
// Only configures git-level invisibility (.git/info/exclude).
// Tool-specific setup (Claude, Cursor, etc.) is handled by `bd setup <tool>`.
// Uses .git/info/exclude (per-repository) instead of global gitignore because:
// - Global gitignore doesn't support absolute paths (GitHub #704)
// - .git/info/exclude is designed for user-specific, repo-local ignores
// - Patterns are relative to repo root, so ".beads/" works correctly
func setupStealthMode(verbose bool) error {
	// Setup per-repository git exclude file (skip if not in a git repo)
	if err := setupGitExclude(verbose); err != nil {
		if strings.Contains(err.Error(), "not a git repository") {
			if verbose {
				fmt.Printf("Not in a git repository — skipping git exclude setup\n")
			}
		} else {
			return fmt.Errorf("failed to setup git exclude: %w", err)
		}
	}

	if verbose {
		fmt.Printf("\n%s Stealth mode configured successfully!\n\n", ui.RenderPass("✓"))
		fmt.Printf("  Git exclude: %s\n", ui.RenderAccent(".git/info/exclude configured"))
		fmt.Printf("\nYour beads setup is now %s - other repo collaborators won't see any beads-related files.\n", ui.RenderAccent("invisible"))
		fmt.Printf("To set up a specific AI tool, run: %s\n\n", ui.RenderAccent("bd setup <claude|cursor|aider|...> --stealth"))
	}

	return nil
}

// setupGitExclude configures .git/info/exclude to ignore beads and claude files
// This is the correct approach for per-repository user-specific ignores (GitHub #704).
// Unlike global gitignore, patterns here are relative to the repo root.
func setupGitExclude(verbose bool) error {
	added, excludePath, err := addExcludePatterns("",
		"# Beads stealth mode (added by bd init --stealth)",
		[]string{".beads/", ".claude/settings.local.json"})
	if err != nil {
		return err
	}
	if verbose {
		if len(added) == 0 {
			fmt.Printf("Git exclude already configured for stealth mode\n")
		} else {
			fmt.Printf("Configured git exclude for stealth mode: %s\n", excludePath)
		}
	}
	return nil
}

// resolveGitExcludePath returns the path to .git/info/exclude for repoPath, using --git-common-dir
// so worktrees resolve to the main repo's exclude file (GH#1053). An empty repoPath resolves
// against the current directory.
func resolveGitExcludePath(repoPath string) (string, error) {
	args := make([]string, 0, 3)
	if repoPath != "" {
		args = append(args, "-C", repoPath)
	}
	args = append(args, "rev-parse", "--git-common-dir")
	// #nosec G702 - fixed "git" command; args are constant subcommands plus an internal repoPath,
	// never attacker-controlled input.
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return "", fmt.Errorf("not a git repository")
	}
	gitDir := strings.TrimSpace(string(out))
	// git prints --git-common-dir relative to its working directory (repoPath when -C is used), so
	// anchor relative results back to repoPath.
	if !filepath.IsAbs(gitDir) {
		base := repoPath
		if base == "" {
			base = "."
		}
		gitDir = filepath.Join(base, gitDir)
	}
	return filepath.Join(gitDir, "info", "exclude"), nil
}

// addExcludePatterns ensures each pattern exists as an exact line in repoPath's .git/info/exclude,
// appending any missing ones under header. It returns the patterns it actually added (empty when all
// were already present) and the resolved exclude path. repoPath "" resolves against the current
// directory. This is the shared core for stealth, fork, and project-pattern exclude setup.
func addExcludePatterns(repoPath, header string, patterns []string) (added []string, excludePath string, err error) {
	excludePath, err = resolveGitExcludePath(repoPath)
	if err != nil {
		return nil, "", err
	}

	if err = os.MkdirAll(filepath.Dir(excludePath), 0755); err != nil {
		return nil, "", fmt.Errorf("failed to create git info directory: %w", err)
	}

	existing := readOptionalExcludeContent(excludePath)
	added = missingExcludePatterns(existing, patterns)
	if len(added) == 0 {
		return nil, excludePath, nil
	}

	newContent := appendExcludePatterns(existing, header, added)

	// #nosec G306 - config file needs 0644
	if err = os.WriteFile(excludePath, []byte(newContent), 0644); err != nil {
		return nil, excludePath, fmt.Errorf("failed to write git exclude file: %w", err)
	}
	return added, excludePath, nil
}

func readOptionalExcludeContent(path string) string {
	// #nosec G304 - git config path
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(content)
}

func missingExcludePatterns(existing string, patterns []string) []string {
	var missing []string
	for _, pattern := range patterns {
		// Exact line match avoids false positives (e.g. ".beads/issues.jsonl" matching ".beads/").
		if !containsExactPattern(existing, pattern) {
			missing = append(missing, pattern)
		}
	}
	return missing
}

func appendExcludePatterns(existing, header string, patterns []string) string {
	newContent := existing
	if len(newContent) > 0 && !strings.HasSuffix(newContent, "\n") {
		newContent += "\n"
	}
	newContent += "\n" + header + "\n"
	for _, pattern := range patterns {
		newContent += pattern + "\n"
	}
	return newContent
}

// projectExcludeHeader labels the project-root ignore patterns (.dolt/, *.db, …) that beads routes
// into .git/info/exclude instead of a tracked .gitignore when git ops are disabled. It is kept
// neutral (not "added by bd init --stealth") because both bd init --stealth and bd doctor --fix
// write it.
const projectExcludeHeader = "# Beads: Dolt files kept local via .git/info/exclude (stealth / no-git-ops)"

// addProjectPatternsToGitExclude appends project-root ignore patterns (.dolt/, *.db, etc.) to
// .git/info/exclude rather than a tracked .gitignore. Stealth / no-git-ops repos use this so beads
// never creates or modifies a visible .gitignore that would expose its presence to repo
// collaborators. repoPath is the repository root ("" resolves against the current directory).
func addProjectPatternsToGitExclude(repoPath string, patterns []string, verbose bool) error {
	added, excludePath, err := addExcludePatterns(repoPath, projectExcludeHeader, patterns)
	if err != nil {
		return err
	}
	if verbose {
		if len(added) == 0 {
			fmt.Printf("Git exclude already has Dolt file patterns\n")
		} else {
			fmt.Printf("Configured git exclude for Dolt files: %s\n", excludePath)
		}
	}
	return nil
}

// removeBeadsProjectGitignoreSection strips the bd-managed section from the tracked project-root
// .gitignore at repoPath, reversing doctor.EnsureProjectGitignore. It removes only the header beads
// writes (doctor.ProjectGitignoreHeader) plus the Dolt pattern lines beads added directly beneath
// it, so unrelated user patterns are preserved. If beads was the .gitignore's only content the file
// is removed entirely, restoring true stealth. Returns true when it changed (or removed) the file;
// a repo with no beads section (or no .gitignore) is left untouched.
func removeBeadsProjectGitignoreSection(repoPath string) (bool, error) {
	gitignorePath := filepath.Join(repoPath, ".gitignore")
	// #nosec G304 - path is the repo-root .gitignore
	content, err := os.ReadFile(gitignorePath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to read .gitignore: %w", err)
	}

	managed := projectGitignoreManagedPatterns()
	lines := strings.Split(string(content), "\n")
	out, changed := removeManagedGitignoreSection(lines, managed)
	if !changed {
		return false, nil
	}

	newContent := strings.Join(out, "\n")
	if strings.TrimSpace(newContent) == "" {
		// beads was the only reason this .gitignore existed — remove it for true stealth.
		if err := os.Remove(gitignorePath); err != nil {
			return false, fmt.Errorf("failed to remove emptied .gitignore: %w", err)
		}
		return true, nil
	}

	// #nosec G306 - gitignore needs to be readable by git and collaborators
	if err := os.WriteFile(gitignorePath, []byte(newContent), 0644); err != nil {
		return false, fmt.Errorf("failed to write .gitignore: %w", err)
	}
	return true, nil
}

func projectGitignoreManagedPatterns() map[string]bool {
	managed := make(map[string]bool, len(doctor.ProjectGitignorePatterns))
	for _, pattern := range doctor.ProjectGitignorePatterns {
		managed[pattern] = true
	}
	return managed
}

func removeManagedGitignoreSection(lines []string, managed map[string]bool) ([]string, bool) {
	out := make([]string, 0, len(lines))
	changed := false
	lineCount := len(lines)
	for i := 0; i < lineCount; i++ {
		if strings.TrimSpace(lines[i]) != doctor.ProjectGitignoreHeader {
			out = append(out, lines[i])
			continue
		}
		changed = true
		out = dropGitignoreSeparator(out)
		i = skipManagedGitignoreLines(lines, i+1, lineCount, managed) - 1
	}
	return out, changed
}

func dropGitignoreSeparator(lines []string) []string {
	lineCount := len(lines)
	if lineCount > 0 && strings.TrimSpace(lines[lineCount-1]) == "" {
		return lines[:lineCount-1]
	}
	return lines
}

func skipManagedGitignoreLines(lines []string, index, lineCount int, managed map[string]bool) int {
	for index < lineCount && managed[strings.TrimSpace(lines[index])] {
		index++
	}
	return index
}

// isStealthRepo reports whether beads must keep its footprint out of tracked git files for the
// workspace at repoPath. It keys off the persisted no-git-ops flag — the same signal bd prime uses
// for the stealth session-close protocol (GH#593). bd init --stealth sets it, and a user may also
// set it directly; either way beads routes ignores into .git/info/exclude rather than a tracked
// .gitignore.
func isStealthRepo(repoPath string) bool {
	beadsDir := doctor.ResolveBeadsDirForRepo(repoPath)
	return config.GetStringFromDir(beadsDir, "no-git-ops") == "true"
}

// trackedGitignoreHasBeadsSection reports whether the tracked project-root .gitignore at repoPath
// still carries the bd-managed section header — i.e. a previous run leaked Dolt patterns into a
// git-visible file. Used by the stealth doctor check to flag the leak for --fix to clean up.
func trackedGitignoreHasBeadsSection(repoPath string) bool {
	// #nosec G304 - path is the repo-root .gitignore
	content, err := os.ReadFile(filepath.Join(repoPath, ".gitignore"))
	if err != nil {
		return false
	}
	return containsExactPattern(string(content), doctor.ProjectGitignoreHeader)
}

// checkProjectExcludeStealth is the stealth-mode counterpart to doctor.CheckProjectGitignore: it
// verifies the project-root ignore patterns live in .git/info/exclude instead of a tracked
// .gitignore. Reusing the "Project Gitignore" name keeps the --fix dispatch and ordering stable.
func checkProjectExcludeStealth(repoPath string) doctor.DoctorCheck {
	excludePath, err := resolveGitExcludePath(repoPath)
	if err != nil {
		return doctor.DoctorCheck{
			Name:    "Project Gitignore",
			Status:  doctor.StatusOK,
			Message: "N/A (not a git repository)",
		}
	}

	var content string
	// #nosec G304 - git config path
	if data, err := os.ReadFile(excludePath); err == nil {
		content = string(data)
	}

	var missing []string
	for _, p := range doctor.ProjectGitignorePatterns {
		if !containsExactPattern(content, p) {
			missing = append(missing, p)
		}
	}

	leaked := trackedGitignoreHasBeadsSection(repoPath)

	if len(missing) > 0 || leaked {
		var details []string
		message := "Stealth mode: .git/info/exclude missing Dolt exclusion patterns"
		if len(missing) > 0 {
			details = append(details, "Missing from .git/info/exclude: "+strings.Join(missing, ", "))
		}
		if leaked {
			// A previous run leaked the patterns into the tracked .gitignore; --fix moves them out.
			message = "Stealth mode: Dolt patterns are exposed in the tracked .gitignore"
			details = append(details, "Tracked .gitignore contains the beads section; bd doctor --fix will move it into .git/info/exclude")
		}
		return doctor.DoctorCheck{
			Name:    "Project Gitignore",
			Status:  doctor.StatusWarning,
			Message: message,
			Detail:  strings.Join(details, "; "),
			Fix:     "Run: bd doctor --fix",
		}
	}
	return doctor.DoctorCheck{
		Name:    "Project Gitignore",
		Status:  doctor.StatusOK,
		Message: "Dolt and credential files excluded via .git/info/exclude (stealth)",
	}
}

// setupForkExclude configures .git/info/exclude for fork workflows (GH#742)
// Adds beads files and Claude artifacts to keep PRs to upstream clean.
// This is separate from stealth mode - fork protection is specifically about
// preventing beads/Claude files from appearing in upstream PRs.
func setupForkExclude(verbose bool) error {
	added, _, err := addExcludePatterns("",
		"# Beads fork protection (bd init)",
		[]string{".beads/", "**/RECOVERY*.md", "**/SESSION*.md"})
	if err != nil {
		return err
	}
	if verbose {
		if len(added) == 0 {
			fmt.Printf("%s Git exclude already configured\n", ui.RenderPass("✓"))
		} else {
			fmt.Printf("\n%s Added to .git/info/exclude:\n", ui.RenderPass("✓"))
			for _, p := range added {
				fmt.Printf("  %s\n", p)
			}
			fmt.Println("\nNote: .git/info/exclude is local-only and won't affect upstream.")
		}
	}
	return nil
}

// containsExactPattern checks if content contains the pattern as an exact line
// This avoids false positives like ".beads/issues.jsonl" matching ".beads/"
func containsExactPattern(content, pattern string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == pattern {
			return true
		}
	}
	return false
}

// promptForkExclude asks if user wants to configure .git/info/exclude for fork workflow (GH#742)
func promptForkExclude(upstreamURL string, quiet bool) (bool, error) {
	if quiet {
		return false, nil // Don't prompt in quiet mode
	}

	fmt.Printf("\n%s Detected fork (upstream: %s)\n\n", ui.RenderAccent("▶"), upstreamURL)
	fmt.Println("Would you like to configure .git/info/exclude to keep beads files local?")
	fmt.Println("This prevents beads from appearing in PRs to upstream.")
	fmt.Print("\n[Y/n]: ")

	reader := bufio.NewReader(os.Stdin)
	response, err := readLineWithContext(getRootContext(), reader, os.Stdin)
	if err != nil {
		if isCanceled(err) {
			return false, err
		}
		response = ""
	}
	response = strings.TrimSpace(strings.ToLower(response))

	// Default to yes (empty or "y" or "yes")
	return response == "" || response == "y" || response == "yes", nil
}

// setupGlobalGitIgnore configures global gitignore to ignore beads and claude files for a specific project
// DEPRECATED: This function uses absolute paths which don't work in gitignore (GitHub #704).
// Use setupGitExclude instead for new code.
func setupGlobalGitIgnore(homeDir string, projectPath string, verbose bool) error {
	ignorePath, err := resolveGlobalGitIgnorePath(homeDir, verbose)
	if err != nil {
		return err
	}
	existingContent := readOptionalExcludeContent(ignorePath)
	beadsPattern, claudePattern := globalStealthPatterns(projectPath)

	hasBeads := strings.Contains(existingContent, beadsPattern)
	hasClaude := strings.Contains(existingContent, claudePattern)

	if hasBeads && hasClaude {
		if verbose {
			fmt.Printf("Global gitignore already configured for stealth mode in %s\n", projectPath)
		}
		return nil
	}

	newContent := appendGlobalStealthPatterns(existingContent, projectPath, beadsPattern, claudePattern, hasBeads, hasClaude)

	// Write the updated ignore file
	// #nosec G306 - config file needs 0644
	if err := os.WriteFile(ignorePath, []byte(newContent), 0644); err != nil {
		printGlobalStealthWriteFailure(ignorePath, projectPath, beadsPattern, claudePattern, hasBeads, hasClaude)
		return nil
	}

	if verbose {
		fmt.Printf("Configured global gitignore for stealth mode in %s\n", projectPath)
	}

	return nil
}

func resolveGlobalGitIgnorePath(homeDir string, verbose bool) (string, error) {
	// Check if user already has a global gitignore file configured.
	cmd := exec.Command("git", "config", "--global", "core.excludesfile")
	output, err := cmd.Output()
	if configured, ok := configuredGlobalGitIgnorePath(string(output), err, homeDir); ok {
		if verbose {
			fmt.Printf("Using existing configured global gitignore file: %s\n", configured)
		}
		return configured, nil
	}

	configDir := filepath.Join(homeDir, ".config", "git")
	standardIgnorePath := filepath.Join(configDir, "ignore")
	if _, err := os.Stat(standardIgnorePath); err == nil {
		if verbose {
			fmt.Printf("Using existing global gitignore file: %s\n", standardIgnorePath)
		}
		return standardIgnorePath, nil
	}

	if err := os.MkdirAll(configDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create git config directory: %w", err)
	}
	if verbose {
		fmt.Printf("Creating new global gitignore file: %s\n", standardIgnorePath)
	}
	return standardIgnorePath, nil
}

func configuredGlobalGitIgnorePath(output string, commandErr error, homeDir string) (string, bool) {
	if commandErr != nil || output == "" {
		return "", false
	}
	path := strings.TrimSpace(output)
	if strings.HasPrefix(path, "~/") {
		path = filepath.Join(homeDir, path[2:])
	} else if path == "~" {
		path = homeDir
	}
	return path, true
}

func globalStealthPatterns(projectPath string) (string, string) {
	return projectPath + "/.beads/", projectPath + "/.claude/settings.local.json"
}

func appendGlobalStealthPatterns(existing, projectPath, beadsPattern, claudePattern string, hasBeads, hasClaude bool) string {
	newContent := existing
	if len(newContent) > 0 && !strings.HasSuffix(newContent, "\n") {
		newContent += "\n"
	}
	if !hasBeads || !hasClaude {
		newContent += fmt.Sprintf("\n# Beads stealth mode: %s (added by bd init --stealth)\n", projectPath)
	}
	if !hasBeads {
		newContent += beadsPattern + "\n"
	}
	if !hasClaude {
		newContent += claudePattern + "\n"
	}
	return newContent
}

func printGlobalStealthWriteFailure(ignorePath, projectPath, beadsPattern, claudePattern string, hasBeads, hasClaude bool) {
	fmt.Printf("\nUnable to write to %s (file is read-only)\n\n", ignorePath)
	fmt.Printf("To enable stealth mode, add these lines to your global gitignore:\n\n")
	if !hasBeads || !hasClaude {
		fmt.Printf("# Beads stealth mode: %s\n", projectPath)
	}
	if !hasBeads {
		fmt.Printf("%s\n", beadsPattern)
	}
	if !hasClaude {
		fmt.Printf("%s\n", claudePattern)
	}
	fmt.Println()
}
