package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
)

func CheckClaude() error {
	env, err := claudeEnvProvider()
	if err != nil {
		return HandleError("%v", err)
	}
	return checkClaude(env)
}

func checkClaude(env claudeEnv) error {
	warnIfClaudeHooksUseRemovedSync(env)

	projectSettings := projectSettingsPath(env.projectDir)
	globalSettings := globalSettingsPath(env.homeDir)
	legacySettings := legacyProjectSettingsPath(env.projectDir)

	switch {
	case hasBeadsHooks(projectSettings):
		_, _ = fmt.Fprintf(env.stdout, "✓ Project hooks installed: %s\n", projectSettings)
	case hasBeadsHooks(globalSettings):
		_, _ = fmt.Fprintf(env.stdout, "✓ Global hooks installed: %s\n", globalSettings)
	case hasBeadsHooks(legacySettings):
		_, _ = fmt.Fprintf(env.stdout, "✓ Project hooks installed (legacy): %s\n", legacySettings)
		_, _ = fmt.Fprintf(env.stdout, "  Consider running 'bd setup claude' to migrate to .claude/settings.json\n")
	case hasBeadsPlugin(env):
		// GH#3192: Plugin provides hooks via plugin.json — no project-level hooks needed
		_, _ = fmt.Fprintln(env.stdout, "✓ Hooks provided by beads plugin (plugin-managed)")
	default:
		_, _ = fmt.Fprintln(env.stdout, "✗ No hooks installed")
		_, _ = fmt.Fprintln(env.stdout, "  Run: bd setup claude")
		return errClaudeHooksMissing
	}

	return checkAgents(claudeAgentsEnv(env), claudeAgentsIntegration)
}

func RemoveClaude(global bool) error {
	env, err := claudeEnvProvider()
	if err != nil {
		return HandleError("%v", err)
	}
	return removeClaude(env, global)
}

func removeClaude(env claudeEnv, global bool) error {
	settingsPath := claudeRemovalSettingsPath(env, global)
	if err := removeClaudeSettings(env, settingsPath); err != nil {
		return err
	}

	// Also clean legacy settings.local.json when removing project hooks
	if !global {
		removeLegacyClaudeSettings(env)
	}

	removeClaudeInstructions(env)

	_, _ = fmt.Fprintln(env.stdout, "✓ Claude hooks removed")
	return nil
}

func claudeRemovalSettingsPath(env claudeEnv, global bool) string {
	if global {
		_, _ = fmt.Fprintln(env.stdout, "Removing Claude hooks globally...")
		return globalSettingsPath(env.homeDir)
	}
	_, _ = fmt.Fprintln(env.stdout, "Removing Claude hooks from project...")
	return projectSettingsPath(env.projectDir)
}

func removeClaudeSettings(env claudeEnv, path string) error {
	data, err := env.readFile(path)
	if err != nil {
		_, _ = fmt.Fprintln(env.stdout, "No settings file found")
		return nil
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		_, _ = fmt.Fprintf(env.stderr, "Error: failed to parse settings.json: %v\n", err)
		return err
	}
	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		_, _ = fmt.Fprintln(env.stdout, "No hooks found")
		return nil
	}

	removeAllClaudeHooks(hooks)
	data, err = json.MarshalIndent(settings, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintf(env.stderr, "Error: marshal settings: %v\n", err)
		return err
	}
	if err := env.writeFile(path, data); err != nil {
		_, _ = fmt.Fprintf(env.stderr, "Error: write settings: %v\n", err)
		return err
	}
	return nil
}

func removeLegacyClaudeSettings(env claudeEnv) {
	legacyPath := legacyProjectSettingsPath(env.projectDir)
	legacyData, err := env.readFile(legacyPath)
	if err != nil {
		return
	}
	var legacySettings map[string]interface{}
	if json.Unmarshal(legacyData, &legacySettings) != nil {
		return
	}
	legacyHooks, ok := legacySettings["hooks"].(map[string]interface{})
	if !ok {
		return
	}
	removeAllClaudeHooks(legacyHooks)
	migrated, err := json.MarshalIndent(legacySettings, "", "  ")
	if err == nil {
		_ = env.writeFile(legacyPath, migrated)
	}
}

func removeClaudeInstructions(env claudeEnv) {
	agentsEnv, redirected := claudeAgentsEnvRedirect(env)
	if redirected {
		_, _ = fmt.Fprintf(env.stdout, "  Leaving shared beads section in %s untouched (project-authoritative, not Claude-specific)\n", config.SafeAgentsFile())
		if err := stripStaleClaudeBlock(env); err != nil {
			_, _ = fmt.Fprintf(env.stderr, "Warning: failed to clean stale beads block from %s: %v\n", claudeInstructionsFile, err)
		}
		return
	}
	if err := removeAgents(agentsEnv, claudeAgentsIntegration); err != nil {
		_, _ = fmt.Fprintf(env.stderr, "Warning: failed to update %s: %v\n", claudeInstructionsFile, err)
	}
}

// addHookCommand adds a hook command to an event if not already present
// Returns true if hook was added, false if already exists
func addHookCommand(hooks map[string]interface{}, event, command string) bool {
	// Get or create event array
	eventHooks, ok := hooks[event].([]interface{})
	if !ok {
		eventHooks = []interface{}{}
	}

	// Check if bd hook already registered
	for _, hook := range eventHooks {
		hookMap, ok := hook.(map[string]interface{})
		if !ok {
			continue
		}
		commands, ok := hookMap["hooks"].([]interface{})
		if !ok {
			continue
		}
		for _, cmd := range commands {
			cmdMap, ok := cmd.(map[string]interface{})
			if !ok {
				continue
			}
			if cmdMap["command"] == command {
				fmt.Printf("✓ Hook already registered: %s\n", event)
				return false
			}
		}
	}

	// Add bd hook to array
	newHook := map[string]interface{}{
		"matcher": "",
		"hooks": []interface{}{
			map[string]interface{}{
				"type":    "command",
				"command": command,
			},
		},
	}

	eventHooks = append(eventHooks, newHook)
	hooks[event] = eventHooks
	return true
}

// removeHookCommand removes a specific command from an event's hook entries.
// Only the matching command object is removed; sibling commands in the same
// hook entry are preserved. A hook entry is dropped only when its command list
// becomes empty after filtering.
func removeHookCommand(hooks map[string]interface{}, event, command string) {
	eventHooks, ok := hooks[event].([]interface{})
	if !ok {
		return
	}

	// Initialize as empty slice (not nil) to avoid JSON null serialization.
	filtered := make([]interface{}, 0, len(eventHooks))
	for _, hook := range eventHooks {
		filteredHook, keep := filterClaudeHook(hook, event, command)
		if keep {
			filtered = append(filtered, filteredHook)
		}
	}

	// GH#955: Delete the key entirely if no hooks remain, rather than
	// leaving an empty array. This is cleaner and avoids potential
	// issues with empty arrays in settings.
	if len(filtered) == 0 {
		delete(hooks, event)
	} else {
		hooks[event] = filtered
	}
}

func filterClaudeHook(hook interface{}, event, command string) (interface{}, bool) {
	hookMap, ok := hook.(map[string]interface{})
	if !ok {
		return hook, true
	}
	commands, ok := hookMap["hooks"].([]interface{})
	if !ok {
		return hook, true
	}

	remaining, removed := filterClaudeHookCommands(commands, command)
	if removed {
		fmt.Printf("✓ Removed %s hook\n", event)
	}
	if len(remaining) == 0 {
		return nil, false
	}
	hookMap["hooks"] = remaining
	return hookMap, true
}

func filterClaudeHookCommands(commands []interface{}, command string) ([]interface{}, bool) {
	remaining := make([]interface{}, 0, len(commands))
	removed := false
	for _, candidate := range commands {
		commandMap, ok := candidate.(map[string]interface{})
		if ok && commandMap["command"] == command {
			removed = true
			continue
		}
		remaining = append(remaining, candidate)
	}
	return remaining, removed
}

// hasBeadsPlugin checks if the beads Claude Code plugin is enabled in any
// settings file. The plugin declares its own SessionStart hooks in plugin.json,
// so project-level hooks from bd setup claude would duplicate them.
func hasBeadsPlugin(env claudeEnv) bool {
	paths := []string{
		projectSettingsPath(env.projectDir),
		globalSettingsPath(env.homeDir),
		legacyProjectSettingsPath(env.projectDir),
	}
	for _, p := range paths {
		if checkBeadsPluginInFile(env.readFile, p) {
			return true
		}
	}
	return false
}

// checkBeadsPluginInFile checks if the beads plugin is enabled in a single settings file.
func checkBeadsPluginInFile(readFile func(string) ([]byte, error), path string) bool {
	data, err := readFile(path)
	if err != nil {
		return false
	}
	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return false
	}
	enabledPlugins, ok := settings["enabledPlugins"].(map[string]interface{})
	if !ok {
		return false
	}
	for key, value := range enabledPlugins {
		// enabledPlugins keys are "<pluginName>@<marketplace>". Match the
		// plugin-name segment exactly: a substring test (GH#4244) mistakes any
		// "*beads*" plugin (e.g. design-to-beads) for the beads hook plugin and
		// wrongly skips the SessionStart hook write.
		name, _, _ := strings.Cut(strings.ToLower(key), "@")
		if name == "beads" {
			if enabled, ok := value.(bool); ok && enabled {
				return true
			}
		}
	}
	return false
}

// hasBeadsHooks checks if a settings file has bd prime hooks
func hasBeadsHooks(settingsPath string) bool {
	data, err := os.ReadFile(settingsPath) // #nosec G304 -- settingsPath is constructed from known safe locations (user home/.claude), not user input
	if err != nil {
		return false
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return false
	}

	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		return false
	}

	return hasBeadsHookEvents(hooks)
}

func hasBeadsHookEvents(hooks map[string]interface{}) bool {
	for _, event := range []string{"SessionStart", "PreCompact"} {
		eventHooks, ok := hooks[event].([]interface{})
		if ok && hasBeadsHookInEvent(eventHooks) {
			return true
		}
	}
	return false
}

func hasBeadsHookInEvent(eventHooks []interface{}) bool {
	for _, hook := range eventHooks {
		hookMap, ok := hook.(map[string]interface{})
		if !ok {
			continue
		}
		commands, ok := hookMap["hooks"].([]interface{})
		if ok && hasBeadsHookCommand(commands) {
			return true
		}
	}
	return false
}

func hasBeadsHookCommand(commands []interface{}) bool {
	for _, command := range commands {
		commandMap, ok := command.(map[string]interface{})
		if !ok {
			continue
		}
		switch commandMap["command"] {
		case "bd prime", "bd prime --stealth",
			"bd prime --hook-json", "bd prime --stealth --hook-json":
			return true
		}
	}
	return false
}
