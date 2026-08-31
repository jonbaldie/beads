package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/git"
)

// CheckHooksQuick does a fast check for outdated git hooks.
// Checks all beads hooks: pre-commit, post-merge, pre-push, post-checkout.
// cliVersion is the current CLI version to compare against.
func CheckHooksQuick(cliVersion string) string {
	hooksDir, err := git.GetGitHooksDir()
	if err != nil {
		return ""
	}
	if _, err := os.Stat(hooksDir); os.IsNotExist(err) {
		return ""
	}
	outdatedHooks, oldestVersion := collectOutdatedBdHooks(hooksDir, cliVersion)
	return formatOutdatedBdHooks(outdatedHooks, oldestVersion, cliVersion)
}

func collectOutdatedBdHooks(hooksDir, cliVersion string) (outdatedHooks []string, oldestVersion string) {
	for _, hookName := range []string{"pre-commit", "post-merge", "pre-push", "post-checkout"} {
		outdated, hookVersion := inspectBdHookVersion(filepath.Join(hooksDir, hookName), cliVersion)
		if !outdated {
			continue
		}
		outdatedHooks = append(outdatedHooks, hookName)
		if oldestVersion == "" || CompareVersions(hookVersion, oldestVersion) < 0 {
			oldestVersion = hookVersion
		}
	}
	return outdatedHooks, oldestVersion
}

func inspectBdHookVersion(hookPath, cliVersion string) (outdated bool, hookVersion string) {
	content, err := os.ReadFile(hookPath) // #nosec G304 - path is controlled
	if err != nil {
		return false, ""
	}
	hookContent := string(content)
	if !strings.Contains(hookContent, "bd-hooks-version:") {
		return false, ""
	}
	hookVersion = parseBdHookVersion(hookContent)
	if hookVersion == "" || hookVersion == cliVersion {
		return false, hookVersion
	}
	return true, hookVersion
}

func parseBdHookVersion(hookContent string) string {
	for _, line := range strings.Split(hookContent, "\n") {
		if !strings.Contains(line, "bd-hooks-version:") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return ""
		}
		return strings.TrimSpace(parts[1])
	}
	return ""
}

func formatOutdatedBdHooks(outdatedHooks []string, oldestVersion, cliVersion string) string {
	switch len(outdatedHooks) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("Git hook %s outdated (%s → %s)", outdatedHooks[0], oldestVersion, cliVersion)
	default:
		return fmt.Sprintf("Git hooks outdated: %s (%s → %s)", strings.Join(outdatedHooks, ", "), oldestVersion, cliVersion)
	}
}
