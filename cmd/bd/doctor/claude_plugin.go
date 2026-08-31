package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// VerifyPrimeOutput checks if bd prime command works and adapts correctly.
// repoPath is the project root directory.
func VerifyPrimeOutput(repoPath string) DoctorCheck {
	cmd := exec.Command("bd", "prime")
	output, err := cmd.CombinedOutput()

	if err != nil {
		return DoctorCheck{
			Name:    "bd prime Command",
			Status:  "error",
			Message: "Command failed to execute",
			Fix:     "Ensure bd is installed and in PATH",
		}
	}

	if len(output) == 0 {
		return DoctorCheck{
			Name:    "bd prime Command",
			Status:  "error",
			Message: "No output produced",
			Detail:  "Expected workflow context markdown",
		}
	}

	// Check if output adapts to MCP mode
	hasMCP := isMCPServerInstalled(repoPath)
	outputStr := string(output)

	if hasMCP && strings.Contains(outputStr, "mcp__plugin_beads_beads__") {
		return DoctorCheck{
			Name:    "bd prime Output",
			Status:  "ok",
			Message: "MCP mode detected",
			Detail:  "Outputting workflow reminders",
		}
	} else if !hasMCP && strings.Contains(outputStr, "bd ready") {
		return DoctorCheck{
			Name:    "bd prime Output",
			Status:  "ok",
			Message: "CLI mode detected",
			Detail:  "Outputting full command reference",
		}
	} else {
		return DoctorCheck{
			Name:    "bd prime Output",
			Status:  "warning",
			Message: "Output may not be adapting to environment",
		}
	}
}

// CheckBdInPath verifies that 'bd' command is available in PATH.
// This is important because Claude hooks rely on executing 'bd prime'.
func CheckBdInPath() DoctorCheck {
	_, err := exec.LookPath("bd")
	if err != nil {
		return DoctorCheck{
			Name:    "CLI Availability",
			Status:  "warning",
			Message: "'bd' command not found in PATH",
			Detail:  "Claude hooks execute 'bd prime' and won't work without bd in PATH",
			Fix: "Install bd globally:\n" +
				"  • Homebrew: brew install beads\n" +
				"  • Script: " + installScriptCommand + "\n" +
				"  • Or add bd to your PATH",
		}
	}

	return DoctorCheck{
		Name:    "CLI Availability",
		Status:  "ok",
		Message: "'bd' command available in PATH",
	}
}

// CheckDocumentationBdPrimeReference checks if the agents file or CLAUDE.md reference 'bd prime'
// and verifies the command exists. This helps catch version mismatches where docs
// reference features not available in the installed version.
// Also supports local-only variants (claude.local.md) that are gitignored.
func CheckDocumentationBdPrimeReference(repoPath string) DoctorCheck {
	docFiles := agentDocFiles(repoPath)

	var filesWithBdPrime []string
	for _, docFile := range docFiles {
		content, err := os.ReadFile(docFile) // #nosec G304 - controlled paths from repoPath
		if err != nil {
			continue
		}

		if strings.Contains(string(content), "bd prime") {
			filesWithBdPrime = append(filesWithBdPrime, filepath.Base(docFile))
		}
	}

	// If no docs reference bd prime, that's fine - not everyone uses it
	if len(filesWithBdPrime) == 0 {
		return DoctorCheck{
			Name:    "Prime Documentation",
			Status:  "ok",
			Message: "No bd prime references in documentation",
		}
	}

	// Docs reference bd prime - verify the command works
	cmd := exec.Command("bd", "prime", "--help")
	if err := cmd.Run(); err != nil {
		return DoctorCheck{
			Name:    "Prime Documentation",
			Status:  "warning",
			Message: "Documentation references 'bd prime' but command not found",
			Detail:  "Files: " + strings.Join(filesWithBdPrime, ", "),
			Fix: "Upgrade bd to get the 'bd prime' command:\n" +
				"  • Homebrew: brew upgrade beads\n" +
				"  • Script: " + installScriptCommand + "\n" +
				"  Or remove 'bd prime' references from documentation if using older version",
		}
	}

	return DoctorCheck{
		Name:    "Prime Documentation",
		Status:  "ok",
		Message: "Documentation references match installed features",
		Detail:  "Files: " + strings.Join(filesWithBdPrime, ", "),
	}
}

// isClaudePresent returns true when the Claude CLI binary exists in PATH or the
// ~/.claude/ directory is present.  CLAUDECODE=1 can be set by AI coding tools
// other than Claude Code itself, so checking for actual Claude artifacts prevents
// spurious warnings for users who never installed Claude Code.
func isClaudePresent() bool {
	if _, err := exec.LookPath("claude"); err == nil {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	info, err := os.Stat(filepath.Join(home, ".claude"))
	return err == nil && info.IsDir()
}

// CheckClaudePlugin checks if the beads Claude Code plugin is installed and up to date.
func CheckClaudePlugin() DoctorCheck {
	// Check if running in Claude Code.
	// CLAUDECODE=1 may be set by AI tools other than Claude Code, so also verify
	// that the claude CLI or ~/.claude/ directory actually exists.
	if os.Getenv("CLAUDECODE") != "1" || !isClaudePresent() {
		return DoctorCheck{
			Name:    "Claude Plugin",
			Status:  StatusOK,
			Message: "N/A (not running in Claude Code)",
		}
	}

	// Get plugin version from installed_plugins.json
	pluginVersion, pluginInstalled, err := GetClaudePluginVersion()
	if err != nil {
		return DoctorCheck{
			Name:    "Claude Plugin",
			Status:  StatusWarning,
			Message: "Unable to check plugin version",
			Detail:  err.Error(),
		}
	}

	if !pluginInstalled {
		return DoctorCheck{
			Name:    "Claude Plugin",
			Status:  StatusWarning,
			Message: "beads plugin not installed",
			Fix:     "Install plugin: /plugin marketplace add steveyegge/beads && /plugin install beads (see docs/integrations/claude-code-plugin.md)",
		}
	}

	// Query PyPI for latest MCP version
	latestMCPVersion, err := latestPyPIVersionFetcher("beads-mcp")
	if err != nil {
		// Network error - don't fail
		return DoctorCheck{
			Name:    "Claude Plugin",
			Status:  StatusOK,
			Message: fmt.Sprintf("version %s (unable to check for updates)", pluginVersion),
		}
	}

	// Compare versions
	if latestMCPVersion == "" || pluginVersion == latestMCPVersion {
		return DoctorCheck{
			Name:    "Claude Plugin",
			Status:  StatusOK,
			Message: fmt.Sprintf("version %s (latest)", pluginVersion),
		}
	}

	if CompareVersions(latestMCPVersion, pluginVersion) > 0 {
		return DoctorCheck{
			Name:    "Claude Plugin",
			Status:  StatusWarning,
			Message: fmt.Sprintf("version %s (latest: %s)", pluginVersion, latestMCPVersion),
			Fix:     "Update plugin: /plugin update beads@beads-marketplace\nRestart Claude Code after update",
		}
	}

	return DoctorCheck{
		Name:    "Claude Plugin",
		Status:  StatusOK,
		Message: fmt.Sprintf("version %s", pluginVersion),
	}
}

// CheckClaudePluginLocalOnly validates local Claude plugin presence/version
// without contacting PyPI.
func CheckClaudePluginLocalOnly() DoctorCheck {
	if os.Getenv("CLAUDECODE") != "1" || !isClaudePresent() {
		return DoctorCheck{
			Name:    "Claude Plugin",
			Status:  StatusOK,
			Message: "N/A (not running in Claude Code)",
		}
	}

	pluginVersion, pluginInstalled, err := GetClaudePluginVersion()
	if err != nil {
		return DoctorCheck{
			Name:    "Claude Plugin",
			Status:  StatusWarning,
			Message: "Unable to check plugin version",
			Detail:  err.Error(),
		}
	}

	if !pluginInstalled {
		return DoctorCheck{
			Name:    "Claude Plugin",
			Status:  StatusWarning,
			Message: "beads plugin not installed",
			Fix:     "Install plugin: /plugin marketplace add steveyegge/beads && /plugin install beads (see docs/integrations/claude-code-plugin.md)",
		}
	}

	return DoctorCheck{
		Name:    "Claude Plugin",
		Status:  StatusOK,
		Message: fmt.Sprintf("version %s (update check skipped in non-interactive mode)", pluginVersion),
	}
}

// GetClaudePluginVersion returns the installed beads Claude plugin version.
func GetClaudePluginVersion() (version string, installed bool, err error) {
	data, err := readInstalledPluginsJSON()
	if err != nil || data == nil {
		return "", false, err
	}
	return parseInstalledBeadsPluginVersion(data)
}

func readInstalledPluginsJSON() ([]byte, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("unable to determine home directory: %w", err)
	}
	pluginPath := filepath.Join(homeDir, ".claude", "plugins", "installed_plugins.json")
	data, err := os.ReadFile(pluginPath) // #nosec G304 - path is controlled
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("unable to read plugin file: %w", err)
	}
	return data, nil
}

func parseInstalledBeadsPluginVersion(data []byte) (string, bool, error) {
	var versionCheck struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &versionCheck); err != nil {
		return "", false, fmt.Errorf("unable to parse plugin file: %w", err)
	}
	if versionCheck.Version == 2 {
		return parseBeadsPluginV2(data)
	}
	return parseBeadsPluginV1(data)
}

func parseBeadsPluginV2(data []byte) (string, bool, error) {
	var pluginDataV2 struct {
		Plugins map[string][]struct {
			Version string `json:"version"`
			Scope   string `json:"scope"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(data, &pluginDataV2); err != nil {
		return "", false, fmt.Errorf("unable to parse plugin file v2: %w", err)
	}
	if entries, ok := pluginDataV2.Plugins["beads@beads-marketplace"]; ok && len(entries) > 0 {
		return entries[0].Version, true, nil
	}
	return "", false, nil
}

func parseBeadsPluginV1(data []byte) (string, bool, error) {
	var pluginDataV1 struct {
		Plugins map[string]struct {
			Version string `json:"version"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(data, &pluginDataV1); err != nil {
		return "", false, fmt.Errorf("unable to parse plugin file: %w", err)
	}
	if plugin, ok := pluginDataV1.Plugins["beads@beads-marketplace"]; ok {
		return plugin.Version, true, nil
	}
	return "", false, nil
}

func fetchLatestPyPIVersion(packageName string) (string, error) {
	url := fmt.Sprintf("https://pypi.org/pypi/%s/json", packageName)

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}

	// Set User-Agent
	req.Header.Set("User-Agent", "beads-cli-doctor")

	resp, err := client.Do(req) //nolint:gosec // G704: URL is the fixed official plugin registry endpoint assembled above.
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("pypi api returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var data struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
	}

	if err := json.Unmarshal(body, &data); err != nil {
		return "", err
	}

	return data.Info.Version, nil
}
