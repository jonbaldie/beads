package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CheckProjectGitignore checks if the project-root .gitignore contains patterns
// to prevent accidentally committing Dolt database files and credential keys.
// repoPath is the project root directory.
func CheckProjectGitignore(repoPath string) DoctorCheck {
	gitignorePath := filepath.Join(repoPath, ".gitignore")

	content, err := os.ReadFile(gitignorePath) // #nosec G304 -- path is hardcoded
	if err != nil {
		if os.IsNotExist(err) {
			return DoctorCheck{
				Name:    "Project Gitignore",
				Status:  StatusWarning,
				Message: "No project .gitignore found — Dolt/credential files may be committed accidentally",
				Fix:     "Run: bd init (safe to re-run) or bd doctor --fix",
			}
		}
		return DoctorCheck{
			Name:    "Project Gitignore",
			Status:  StatusWarning,
			Message: fmt.Sprintf("Cannot read project .gitignore: %v", err),
		}
	}

	contentStr := string(content)
	var missing []string
	for _, pattern := range ProjectGitignorePatterns {
		if !containsGitignorePattern(contentStr, pattern) {
			missing = append(missing, pattern)
		}
	}

	if len(missing) > 0 {
		return DoctorCheck{
			Name:    "Project Gitignore",
			Status:  StatusWarning,
			Message: "Project .gitignore missing required exclusion patterns",
			Detail:  "Missing: " + strings.Join(missing, ", "),
			Fix:     "Run: bd doctor --fix or bd init (safe to re-run)",
		}
	}

	return DoctorCheck{
		Name:    "Project Gitignore",
		Status:  StatusOK,
		Message: "Dolt and credential files excluded",
	}
}

// EnsureProjectGitignore adds .dolt/, *.db, and .beads-credential-key patterns
// to the project-root .gitignore if they are not already present. Creates the
// file if it doesn't exist. This prevents users from accidentally committing
// Dolt database files or the credential encryption key.
// repoPath is the project root directory.
func EnsureProjectGitignore(repoPath string) error {
	gitignorePath := filepath.Join(repoPath, ".gitignore")
	existingContent, err := readProjectGitignore(gitignorePath)
	if err != nil {
		return err
	}
	toAdd := missingProjectGitignorePatterns(existingContent)
	if len(toAdd) == 0 {
		return nil
	}
	return writeProjectGitignorePatterns(gitignorePath, existingContent, toAdd)
}

func readProjectGitignore(gitignorePath string) (string, error) {
	content, err := os.ReadFile(gitignorePath) // #nosec G304 -- path is hardcoded
	if err == nil {
		return string(content), nil
	}
	if os.IsNotExist(err) {
		return "", nil
	}
	return "", fmt.Errorf("failed to read .gitignore: %w", err)
}

func missingProjectGitignorePatterns(existingContent string) []string {
	var toAdd []string
	for _, pattern := range ProjectGitignorePatterns {
		if !containsGitignorePattern(existingContent, pattern) {
			toAdd = append(toAdd, pattern)
		}
	}
	return toAdd
}

func writeProjectGitignorePatterns(gitignorePath, existingContent string, toAdd []string) error {
	newContent := existingContent
	if len(newContent) > 0 && !strings.HasSuffix(newContent, "\n") {
		newContent += "\n"
	}
	newContent += "\n" + ProjectGitignoreHeader + "\n"
	for _, pattern := range toAdd {
		newContent += pattern + "\n"
	}
	// #nosec G306 -- gitignore needs to be readable by git and collaborators
	if err := os.WriteFile(gitignorePath, []byte(newContent), 0644); err != nil {
		return fmt.Errorf("failed to write .gitignore: %w", err)
	}
	return nil
}

// FixProjectGitignore is an alias for EnsureProjectGitignore, used by bd doctor --fix.
// repoPath is the project root directory.
func FixProjectGitignore(repoPath string) error {
	return EnsureProjectGitignore(repoPath)
}

// containsGitignorePattern checks if a gitignore file content contains the given pattern.
// It checks for the pattern as a standalone line (ignoring leading/trailing whitespace).
func containsGitignorePattern(content, pattern string) bool {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == pattern {
			return true
		}
	}
	return false
}
