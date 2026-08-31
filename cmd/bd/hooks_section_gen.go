package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/git"
)

// CheckGitHooks checks the status of bd git hooks in .git/hooks/
func CheckGitHooks() []HookStatus {
	hooks := []string{"pre-commit", "post-merge", "pre-push", "post-checkout", "prepare-commit-msg"}
	statuses := make([]HookStatus, 0, len(hooks))

	// Get hooks directory from common git dir (hooks are shared across worktrees)
	hooksDir, err := git.GetGitHooksDir()
	if err != nil {
		// Not a git repo - return all hooks as not installed
		for _, hookName := range hooks {
			statuses = append(statuses, HookStatus{Name: hookName, Installed: false})
		}
		return statuses
	}

	for _, hookName := range hooks {
		status := HookStatus{
			Name: hookName,
		}

		// Check if hook exists
		hookPath := filepath.Join(hooksDir, hookName)
		versionInfo, err := getHookVersion(hookPath)
		if err != nil {
			// Hook doesn't exist or couldn't be read
			status.Installed = false
		} else {
			status.Installed = true
			status.Version = versionInfo.Version
			status.IsShim = versionInfo.IsShim

			// Thin shims are never outdated (they delegate to bd)
			// bd hooks are outdated if version is missing (legacy inline) or differs
			if !versionInfo.IsShim && versionInfo.IsBdHook && versionInfo.Version != Version {
				status.Outdated = true
			}
		}

		statuses = append(statuses, status)
	}

	return statuses
}

// hookVersionInfo contains version information extracted from a hook file
type hookVersionInfo struct {
	Version  string // bd version (for legacy hooks) or shim version
	IsShim   bool   // true if this is a thin shim
	IsBdHook bool   // true if this is any type of bd hook (shim or inline)
}

// getHookVersion extracts the version from a hook file
func getHookVersion(path string) (hookVersionInfo, error) {
	// #nosec G304 -- hook path constrained to .git/hooks directory
	file, err := os.Open(path)
	if err != nil {
		return hookVersionInfo{}, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	// Read the entire file to support section markers anywhere (GH#1380)
	var content strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		content.WriteString(line)
		content.WriteString("\n")
		// Check for section marker (GH#1380) — can appear anywhere in the file
		if strings.HasPrefix(line, hookSectionBeginPrefix) {
			// Extract version from "# --- BEGIN BEADS INTEGRATION v0.56.1 ---"
			after := strings.TrimPrefix(line, hookSectionBeginPrefix)
			after = strings.TrimSpace(after)
			after = strings.TrimPrefix(after, "v")
			after = strings.TrimSuffix(after, "---")
			version := strings.TrimSpace(after)
			return hookVersionInfo{Version: version, IsShim: true, IsBdHook: true}, nil
		}
		// Check for thin shim marker first
		if strings.HasPrefix(line, shimVersionPrefix) {
			version := strings.TrimSpace(strings.TrimPrefix(line, shimVersionPrefix))
			return hookVersionInfo{Version: version, IsShim: true, IsBdHook: true}, nil
		}
		// Check for legacy version marker
		if strings.HasPrefix(line, hookVersionPrefix) {
			version := strings.TrimSpace(strings.TrimPrefix(line, hookVersionPrefix))
			return hookVersionInfo{Version: version, IsShim: false, IsBdHook: true}, nil
		}
	}

	if err := scanner.Err(); err != nil {
		return hookVersionInfo{}, fmt.Errorf("reading hook file: %w", err)
	}

	// Check if it's an inline bd hook (from bd init) - GH#1120
	// These don't have version markers but have "# bd (beads)" comment
	if strings.Contains(content.String(), inlineHookMarker) {
		return hookVersionInfo{IsBdHook: true}, nil
	}

	// No version found and not a bd hook
	return hookVersionInfo{}, nil
}

// FormatHookWarnings returns a formatted warning message if hooks are outdated
func FormatHookWarnings(statuses []HookStatus) string {
	var warnings []string

	missingCount := 0
	outdatedCount := 0

	for _, status := range statuses {
		if !status.Installed {
			missingCount++
		} else if status.Outdated {
			outdatedCount++
		}
	}

	if missingCount > 0 {
		warnings = append(warnings, fmt.Sprintf("⚠️  Git hooks not installed (%d missing)", missingCount))
		warnings = append(warnings, "   Run: bd hooks install")
	}

	if outdatedCount > 0 {
		warnings = append(warnings, fmt.Sprintf("⚠️  Git hooks are outdated (%d hooks)", outdatedCount))
		warnings = append(warnings, "   Run: bd hooks install")
	}

	if len(warnings) > 0 {
		return strings.Join(warnings, "\n")
	}

	return ""
}
