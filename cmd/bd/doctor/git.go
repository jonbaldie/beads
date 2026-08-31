package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jonbaldie/beads/cmd/bd/doctor/fix"
	"github.com/jonbaldie/beads/internal/git"
)

// gitCmdTimeout is the timeout for git subprocess commands in doctor checks.
// Prevents doctor checks from blocking indefinitely if git hangs.
const gitCmdTimeout = 30 * time.Second

const (
	hooksExamplesURL = "https://github.com/jonbaldie/beads/tree/main/examples/git-hooks"
	hooksUpgradeURL  = "https://github.com/jonbaldie/beads/issues/615"
)

// bdShimMarker identifies bd shim hooks (GH#946)
const bdShimMarker = "# bd-shim"

// bdInlineHookMarker identifies inline hooks created by bd init (GH#1120)
// These hooks have the logic embedded directly rather than calling bd hooks run
const bdInlineHookMarker = "# bd (beads)"

// bdSectionMarkerPrefix identifies marker-managed hooks (GH#1380)
// These use "# --- BEGIN BEADS INTEGRATION vX.Y.Z ---" section markers.
const bdSectionMarkerPrefix = "# --- BEGIN BEADS INTEGRATION"

// bdHooksRunPattern matches hooks that call bd hooks run
var bdHooksRunPattern = regexp.MustCompile(`\bbd\s+hooks\s+run\b`)

// CheckGitHooks verifies that recommended git hooks are installed.
func CheckGitHooks(cliVersion string) DoctorCheck {
	// Check if we're in a git repository using worktree-aware detection
	hooksDir, err := git.GetGitHooksDir()
	if err != nil {
		return DoctorCheck{
			Name:    "Git Hooks",
			Status:  StatusOK,
			Message: "N/A (not a git repository)",
		}
	}
	return inspectGitHooks(hooksDir, cliVersion)
}

type hookInventory struct {
	missing   []string
	installed []string
}

func inspectGitHooks(hooksDir, cliVersion string) DoctorCheck {
	inventory := inspectRecommendedHooks(hooksDir)
	repoRoot := git.GetRepoRoot()
	externalManagers := fix.DetectExternalHookManagers(repoRoot)
	if len(externalManagers) > 0 {
		if check, ok := checkInstalledBdShims(hooksDir, cliVersion); ok {
			return check
		}
		if check, ok := checkExternalHookManager(repoRoot, externalManagers); ok {
			return check
		}
	}
	return checkRecommendedHookStatus(hooksDir, cliVersion, inventory)
}

func inspectRecommendedHooks(hooksDir string) hookInventory {
	// Recommended hooks and their purposes
	recommendedHooks := map[string]string{
		"pre-commit": "Syncs pending bd changes before commit",
		"post-merge": "Syncs database after git pull/merge",
		"pre-push":   "Validates database state before push",
	}
	var inventory hookInventory

	for hookName := range recommendedHooks {
		hookPath := filepath.Join(hooksDir, hookName)
		if _, err := os.Stat(hookPath); os.IsNotExist(err) {
			inventory.missing = append(inventory.missing, hookName)
		} else {
			inventory.installed = append(inventory.installed, hookName)
		}
	}
	return inventory
}

func checkInstalledBdShims(hooksDir, cliVersion string) (DoctorCheck, bool) {
	hasBdShims, bdHooks := areBdShimsInstalled(hooksDir)
	if !hasBdShims {
		return DoctorCheck{}, false
	}
	if outdated, oldest := findOutdatedBDHookVersions(hooksDir, bdHooks, cliVersion); len(outdated) > 0 {
		return outdatedHooksCheck(outdated, oldest, cliVersion), true
	}
	return DoctorCheck{
		Name:    "Git Hooks",
		Status:  StatusOK,
		Message: "bd shims installed (ignoring external manager config)",
		Detail:  fmt.Sprintf("bd hooks run: %s", strings.Join(bdHooks, ", ")),
	}, true
}

func checkExternalHookManager(repoRoot string, externalManagers []fix.ExternalHookManager) (DoctorCheck, bool) {
	integration := fix.CheckExternalHookManagerIntegration(repoRoot)
	if integration == nil {
		return DoctorCheck{}, false
	}
	if integration.DetectionOnly {
		return DoctorCheck{
			Name:    "Git Hooks",
			Status:  StatusOK,
			Message: fmt.Sprintf("%s detected (cannot verify bd integration)", integration.Manager),
			Detail:  "Ensure your hook config calls 'bd hooks run <hook>'",
		}, true
	}
	if integration.Configured {
		return configuredExternalHookCheck(integration)
	}
	return DoctorCheck{
		Name:    "Git Hooks",
		Status:  StatusWarning,
		Message: fmt.Sprintf("%s not calling bd", fix.ManagerNames(externalManagers)),
		Detail:  "Configure hooks to call bd commands",
		Fix:     "Add or upgrade to 'bd hooks run <hook>'. See " + hooksUpgradeURL,
	}, true
}

func configuredExternalHookCheck(integration *fix.HookIntegrationStatus) (DoctorCheck, bool) {
	if len(integration.HooksWithoutBd) > 0 {
		return DoctorCheck{
			Name:    "Git Hooks",
			Status:  StatusWarning,
			Message: fmt.Sprintf("%s hooks not calling bd", integration.Manager),
			Detail:  fmt.Sprintf("Missing bd: %s", strings.Join(integration.HooksWithoutBd, ", ")),
			Fix:     "Add or upgrade to 'bd hooks run <hook>'. See " + hooksUpgradeURL,
		}, true
	}
	return DoctorCheck{
		Name:    "Git Hooks",
		Status:  StatusOK,
		Message: fmt.Sprintf("All hooks via %s", integration.Manager),
		Detail:  fmt.Sprintf("bd hooks run: %s", strings.Join(integration.HooksWithBd, ", ")),
	}, true
}

func checkRecommendedHookStatus(hooksDir, cliVersion string, inventory hookInventory) DoctorCheck {
	if len(inventory.missing) == 0 {
		if outdated, oldest := findOutdatedBDHookVersions(hooksDir, inventory.installed, cliVersion); len(outdated) > 0 {
			return outdatedHooksCheck(outdated, oldest, cliVersion)
		}
		return DoctorCheck{
			Name:    "Git Hooks",
			Status:  StatusOK,
			Message: "All recommended hooks installed",
			Detail:  fmt.Sprintf("Installed: %s", strings.Join(inventory.installed, ", ")),
		}
	}
	return missingRecommendedHookCheck(inventory)
}

func outdatedHooksCheck(outdated []string, oldest, cliVersion string) DoctorCheck {
	return DoctorCheck{
		Name:    "Git Hooks",
		Status:  StatusWarning,
		Message: "Installed bd hooks are outdated",
		Detail: fmt.Sprintf(
			"Outdated: %s (oldest: %s, current: %s)",
			strings.Join(outdated, ", "),
			oldest,
			cliVersion,
		),
		Fix: "Run 'bd hooks install --force' to update hooks",
	}
}

func missingRecommendedHookCheck(inventory hookInventory) DoctorCheck {
	hookInstallMsg := "Install hooks with 'bd hooks install'. See " + hooksExamplesURL
	if len(inventory.installed) > 0 {
		return DoctorCheck{
			Name:    "Git Hooks",
			Status:  StatusWarning,
			Message: fmt.Sprintf("Missing %d recommended hook(s)", len(inventory.missing)),
			Detail:  fmt.Sprintf("Missing: %s", strings.Join(inventory.missing, ", ")),
			Fix:     hookInstallMsg,
		}
	}
	return DoctorCheck{
		Name:    "Git Hooks",
		Status:  StatusWarning,
		Message: "No recommended git hooks installed",
		Detail:  fmt.Sprintf("Recommended: %s", strings.Join([]string{"pre-commit", "post-merge", "pre-push"}, ", ")),
		Fix:     hookInstallMsg,
	}
}

func findOutdatedBDHookVersions(
	hooksDir string,
	hookNames []string,
	cliVersion string,
) ([]string, string) {
	if !IsValidSemver(cliVersion) {
		return nil, ""
	}
	var outdated []string
	var oldest string
	for _, hookName := range hookNames {
		hookLabel, hookVersion, ok := outdatedBDHookVersion(hooksDir, hookName, cliVersion)
		if !ok {
			continue
		}
		outdated = append(outdated, hookLabel)
		if hookVersion == "" {
			if oldest == "" {
				oldest = "0.0.0"
			}
		} else if oldest == "" || CompareVersions(hookVersion, oldest) < 0 {
			oldest = hookVersion
		}
	}
	return outdated, oldest
}

func outdatedBDHookVersion(hooksDir, hookName, cliVersion string) (string, string, bool) {
	content, err := os.ReadFile(filepath.Join(hooksDir, hookName))
	if err != nil {
		return "", "", false
	}
	contentStr := string(content)
	hookVersion, ok := parseBDHookVersion(contentStr)
	if !ok || !IsValidSemver(hookVersion) {
		// No version comment found. If this is a bd hook (has shim marker,
		// inline marker, or calls bd hooks run), treat it as outdated since
		// all current hook templates include a version comment. (GH#1466)
		if isBdHookContent(contentStr) {
			return fmt.Sprintf("%s@unknown", hookName), "", true
		}
		return "", "", false
	}
	if CompareVersions(hookVersion, cliVersion) >= 0 {
		return "", "", false
	}
	return fmt.Sprintf("%s@%s", hookName, hookVersion), hookVersion, true
}

// isBdHookContent checks if hook content is a bd hook (shim, inline, section-marker, or calls bd hooks run).
func isBdHookContent(content string) bool {
	return strings.Contains(content, bdShimMarker) ||
		strings.Contains(content, bdInlineHookMarker) ||
		strings.Contains(content, bdSectionMarkerPrefix) ||
		bdHooksRunPattern.MatchString(content)
}

func parseBDHookVersion(content string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		// Check for section marker: "# --- BEGIN BEADS INTEGRATION v0.57.0 ---"
		// This is the current hook format used by marker-managed installs (GH#1380).
		if strings.HasPrefix(line, bdSectionMarkerPrefix) {
			after := strings.TrimPrefix(line, bdSectionMarkerPrefix)
			after = strings.TrimSpace(after)
			after = strings.TrimPrefix(after, "v")
			after = strings.TrimSuffix(after, "---")
			version := strings.TrimSpace(after)
			if version != "" {
				return version, true
			}
		}
		// Check for legacy version comment: "# bd-hooks-version: 0.55.0"
		if strings.Contains(line, "bd-hooks-version:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) != 2 {
				continue
			}
			version := strings.TrimSpace(parts[1])
			if version != "" {
				return version, true
			}
		}
	}
	return "", false
}

// areBdShimsInstalled checks if the installed hooks are bd shims, call bd hooks run,
// or are inline bd hooks created by bd init.
// This helps detect when bd hooks are installed directly but an external manager config exists.
// Returns (true, installedHooks) if bd hooks are detected, (false, nil) otherwise.
// (GH#946, GH#1120)
func areBdShimsInstalled(hooksDir string) (bool, []string) {
	hooks := []string{"pre-commit", "post-merge", "pre-push"}
	var bdHooks []string

	for _, hookName := range hooks {
		hookPath := filepath.Join(hooksDir, hookName)
		content, err := os.ReadFile(hookPath)
		if err != nil {
			continue
		}
		contentStr := string(content)
		// Check for bd-shim marker, bd hooks run call, or inline bd hook marker (from bd init)
		if strings.Contains(contentStr, bdShimMarker) ||
			strings.Contains(contentStr, bdInlineHookMarker) ||
			bdHooksRunPattern.MatchString(contentStr) {
			bdHooks = append(bdHooks, hookName)
		}
	}

	return len(bdHooks) > 0, bdHooks
}
