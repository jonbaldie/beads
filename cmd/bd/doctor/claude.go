package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

var latestPyPIVersionFetcher = fetchLatestPyPIVersion

// CheckClaude returns Claude integration verification as a DoctorCheck.
// repoPath is the project root directory.
func CheckClaude(repoPath string) DoctorCheck {
	return reportClaudeIntegration(claudeInstallState{
		hasPlugin:    isBeadsPluginInstalled(repoPath),
		hasMCP:       isMCPServerInstalled(repoPath),
		hasHooks:     hasClaudeHooks(repoPath),
		inClaudeCode: os.Getenv("CLAUDECODE") == "1",
	})
}

type claudeInstallState struct {
	hasPlugin    bool
	hasMCP       bool
	hasHooks     bool
	inClaudeCode bool
}

func claudeIntegrationMode(st claudeInstallState) string {
	if st.hasPlugin {
		return "plugin"
	}
	if st.hasMCP && st.hasHooks {
		return "mcp-hooks"
	}
	if st.hasHooks {
		return "hooks"
	}
	if st.hasMCP {
		return "mcp-only"
	}
	if !st.inClaudeCode || !isClaudePresent() {
		return "cli"
	}
	return "unconfigured"
}

func reportClaudeIntegration(st claudeInstallState) DoctorCheck {
	switch claudeIntegrationMode(st) {
	case "plugin":
		return DoctorCheck{
			Name:    "Claude Integration",
			Status:  "ok",
			Message: "Plugin installed",
			Detail:  "Slash commands and workflow hooks enabled via plugin",
		}
	case "mcp-hooks":
		return DoctorCheck{
			Name:    "Claude Integration",
			Status:  "ok",
			Message: "MCP server and hooks installed",
			Detail:  "Workflow reminders enabled (legacy MCP mode)",
		}
	case "hooks":
		return DoctorCheck{
			Name:    "Claude Integration",
			Status:  "ok",
			Message: "Hooks installed (CLI mode)",
			Detail:  "Plugin not detected - install for slash commands",
		}
	case "mcp-only":
		return DoctorCheck{
			Name:    "Claude Integration",
			Status:  "warning",
			Message: "MCP server installed but hooks missing",
			Detail: "MCP-only mode: relies on tools for every query (~10.5k tokens)\n" +
				"  bd prime hooks provide much better token efficiency",
			Fix: "Add bd prime hooks for better token efficiency:\n" +
				"  1. Run 'bd setup claude' to add SessionStart hooks\n" +
				"\n" +
				"Benefits:\n" +
				"  • MCP mode: ~50 tokens vs ~10.5k for full tool scan (99% reduction)\n" +
				"  • Automatic context refresh on session start and compaction\n" +
				"  • Works alongside MCP tools for when you need them\n" +
				"\n" +
				"See: bd setup claude --help",
		}
	case "cli":
		// Not in Claude Code, or CLAUDECODE=1 was set by another AI tool but
		// Claude CLI/~/.claude/ are absent — skip plugin suggestion.
		return DoctorCheck{
			Name:    "Claude Integration",
			Status:  "ok",
			Message: "CLI-only mode",
			Detail:  "To enable Claude integration, run bd setup claude",
		}
	default:
		return DoctorCheck{
			Name:    "Claude Integration",
			Status:  "warning",
			Message: "Not configured",
			Detail:  "Claude can use bd more effectively with the beads plugin",
			Fix: "Set up Claude integration:\n" +
				"  Option 1: Install the beads plugin (recommended)\n" +
				"    • Provides hooks, slash commands, and MCP tools automatically\n" +
				"    • See: https://github.com/gastownhall/beads/blob/main/docs/integrations/claude-code-plugin.md\n" +
				"\n" +
				"  Option 2: CLI-only mode\n" +
				"    • Run 'bd setup claude' to add SessionStart hooks\n" +
				"    • No slash commands, but hooks provide workflow context\n" +
				"\n" +
				"Benefits:\n" +
				"  • Auto-inject workflow context on session start (~50-2k tokens)\n" +
				"  • Automatic context recovery before compaction",
		}
	}
}

// isBeadsPluginInstalled checks if beads plugin is enabled in Claude Code.
// It checks user-level (~/.claude/settings.json) and project-level settings
// (.claude/settings.json and .claude/settings.local.json).
// repoPath is the project root directory.
func isBeadsPluginInstalled(repoPath string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}

	// Check user-level settings
	userSettings := filepath.Join(home, ".claude", "settings.json")
	if checkPluginInSettings(userSettings) {
		return true
	}

	// Check project-level settings
	projectSettings := filepath.Join(repoPath, ".claude", "settings.json")
	if checkPluginInSettings(projectSettings) {
		return true
	}

	// Check project-level local settings (gitignored)
	projectLocalSettings := filepath.Join(repoPath, ".claude", "settings.local.json")
	if checkPluginInSettings(projectLocalSettings) {
		return true
	}

	return false
}

// checkPluginInSettings checks if beads plugin is enabled in a settings file
func checkPluginInSettings(settingsPath string) bool {
	data, err := os.ReadFile(settingsPath) // #nosec G304 -- settingsPath is constructed from known safe locations, not user input
	if err != nil {
		return false
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return false
	}

	// Check enabledPlugins section for beads
	enabledPlugins, ok := settings["enabledPlugins"].(map[string]interface{})
	if !ok {
		return false
	}

	// Look for beads@beads-marketplace plugin
	for key, value := range enabledPlugins {
		if strings.Contains(strings.ToLower(key), "beads") {
			// Check if it's enabled (value should be true)
			if enabled, ok := value.(bool); ok && enabled {
				return true
			}
		}
	}

	return false
}

// isMCPServerInstalled checks if MCP server is configured.
// It checks user-level (~/.claude/settings.json) and project-level settings
// (.claude/settings.json and .claude/settings.local.json).
// repoPath is the project root directory.
func isMCPServerInstalled(repoPath string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}

	// Check user-level settings
	userSettings := filepath.Join(home, ".claude", "settings.json")
	if checkMCPInSettings(userSettings) {
		return true
	}

	// Check project-level settings
	projectSettings := filepath.Join(repoPath, ".claude", "settings.json")
	if checkMCPInSettings(projectSettings) {
		return true
	}

	// Check project-level local settings (gitignored)
	projectLocalSettings := filepath.Join(repoPath, ".claude", "settings.local.json")
	if checkMCPInSettings(projectLocalSettings) {
		return true
	}

	return false
}

// checkMCPInSettings checks if beads MCP server is configured in a settings file
func checkMCPInSettings(settingsPath string) bool {
	data, err := os.ReadFile(settingsPath) // #nosec G304 -- settingsPath is constructed from known safe locations, not user input
	if err != nil {
		return false
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return false
	}

	// Check mcpServers section for beads
	mcpServers, ok := settings["mcpServers"].(map[string]interface{})
	if !ok {
		return false
	}

	// Look for beads server (any key containing "beads")
	for key := range mcpServers {
		if strings.Contains(strings.ToLower(key), "beads") {
			return true
		}
	}

	return false
}

// hasClaudeHooks checks if Claude hooks are installed.
// repoPath is the project root directory.
func hasClaudeHooks(repoPath string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}

	globalSettings := filepath.Join(home, ".claude", "settings.json")
	projectSettings := filepath.Join(repoPath, ".claude", "settings.json")
	projectLocalSettings := filepath.Join(repoPath, ".claude", "settings.local.json")

	return hasBeadsHooks(globalSettings) || hasBeadsHooks(projectSettings) || hasBeadsHooks(projectLocalSettings)
}

// hasBeadsHooks checks if a settings file has bd prime hooks
func hasBeadsHooks(settingsPath string) bool {
	hooks, ok := loadClaudeHooksMap(settingsPath)
	if !ok {
		return false
	}
	for _, event := range []string{"SessionStart", "PreCompact"} {
		if eventHasBeadsPrime(hooks, event) {
			return true
		}
	}
	return false
}

func loadClaudeHooksMap(settingsPath string) (map[string]interface{}, bool) {
	data, err := os.ReadFile(settingsPath) // #nosec G304 -- settingsPath is constructed from known safe locations (user home/.claude), not user input
	if err != nil {
		return nil, false
	}
	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, false
	}
	hooks, ok := settings["hooks"].(map[string]interface{})
	return hooks, ok
}

func eventHasBeadsPrime(hooks map[string]interface{}, event string) bool {
	eventHooks, ok := hooks[event].([]interface{})
	if !ok {
		return false
	}
	for _, hook := range eventHooks {
		if hookHasBeadsPrime(hook) {
			return true
		}
	}
	return false
}

func hookHasBeadsPrime(hook interface{}) bool {
	hookMap, ok := hook.(map[string]interface{})
	if !ok {
		return false
	}
	commands, ok := hookMap["hooks"].([]interface{})
	if !ok {
		return false
	}
	for _, cmd := range commands {
		if isBeadsPrimeCommand(cmd) {
			return true
		}
	}
	return false
}

func isBeadsPrimeCommand(cmd interface{}) bool {
	cmdMap, ok := cmd.(map[string]interface{})
	if !ok {
		return false
	}
	cmdStr, _ := cmdMap["command"].(string)
	switch cmdStr {
	case "bd prime", "bd prime --stealth",
		"bd prime --hook-json", "bd prime --stealth --hook-json":
		return true
	}
	return false
}
