package setup

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

func InstallClaude(global bool, stealth bool) error {
	env, err := claudeEnvProvider()
	if err != nil {
		return HandleError("%v", err)
	}
	return installClaude(env, global, stealth)
}

// InstallClaudeProject installs project-local Claude hooks, returning an error
// instead of exiting. Used by bd init to integrate Claude setup automatically.
func InstallClaudeProject(stealth bool) error {
	env, err := claudeEnvProvider()
	if err != nil {
		return err
	}
	return installClaude(env, false, stealth)
}

func installClaude(env claudeEnv, global bool, stealth bool) error {
	settingsPath := claudeInstallSettingsPath(env, global)

	if err := env.ensureDir(filepath.Dir(settingsPath), 0o755); err != nil {
		_, _ = fmt.Fprintf(env.stderr, "Error: %v\n", err)
		return err
	}

	settings, err := loadClaudeSettings(env, settingsPath)
	if err != nil {
		return err
	}

	hooks := claudeSettingsHooks(settings)
	removeObsoleteClaudeHooks(hooks, stealth)
	installClaudeHook(env, hooks, stealth)
	if err := writeClaudeSettings(env, settingsPath, settings); err != nil {
		return err
	}

	// Migrate legacy hooks: remove beads hooks from settings.local.json if present
	if !global {
		migrateLegacyClaudeSettings(env)
	}

	agentsSkipped := installClaudeInstructions(env)
	reportClaudeInstall(env, settingsPath, agentsSkipped)
	return nil
}

func claudeInstallSettingsPath(env claudeEnv, global bool) string {
	if global {
		_, _ = fmt.Fprintln(env.stdout, "Installing Claude hooks globally...")
		return globalSettingsPath(env.homeDir)
	}
	_, _ = fmt.Fprintln(env.stdout, "Installing Claude hooks for this project...")
	return projectSettingsPath(env.projectDir)
}

func loadClaudeSettings(env claudeEnv, path string) (map[string]interface{}, error) {
	settings := make(map[string]interface{})
	data, err := env.readFile(path)
	if err != nil {
		return settings, nil
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		_, _ = fmt.Fprintf(env.stderr, "Error: failed to parse settings.json: %v\n", err)
		return nil, err
	}
	return settings, nil
}

func claudeSettingsHooks(settings map[string]interface{}) map[string]interface{} {
	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		hooks = make(map[string]interface{})
		settings["hooks"] = hooks
	}
	for key, value := range hooks {
		if value == nil {
			delete(hooks, key)
		}
	}
	return hooks
}

func removeObsoleteClaudeHooks(hooks map[string]interface{}, stealth bool) {
	command := claudeHookCommand(stealth)
	for _, legacy := range []string{"bd prime", "bd prime --stealth"} {
		if legacy == command {
			continue
		}
		removeHookCommand(hooks, "SessionStart", legacy)
		removeHookCommand(hooks, "PreCompact", legacy)
	}
	removeHookCommand(hooks, "PreCompact", "bd prime --hook-json")
	removeHookCommand(hooks, "PreCompact", "bd prime --stealth --hook-json")
}

func claudeHookCommand(stealth bool) string {
	if stealth {
		return "bd prime --stealth --hook-json"
	}
	return "bd prime --hook-json"
}

func installClaudeHook(env claudeEnv, hooks map[string]interface{}, stealth bool) {
	if hasBeadsPlugin(env) {
		_, _ = fmt.Fprintln(env.stdout, "✓ Beads plugin detected — hooks are plugin-managed, skipping")
		return
	}
	if addHookCommand(hooks, "SessionStart", claudeHookCommand(stealth)) {
		_, _ = fmt.Fprintln(env.stdout, "✓ Registered SessionStart hook")
	}
}

func writeClaudeSettings(env claudeEnv, path string, settings map[string]interface{}) error {
	data, err := json.MarshalIndent(settings, "", "  ")
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

func migrateLegacyClaudeSettings(env claudeEnv) {
	legacyPath := legacyProjectSettingsPath(env.projectDir)
	if !hasBeadsHooks(legacyPath) {
		return
	}
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
	if err != nil {
		return
	}
	if err := env.writeFile(legacyPath, migrated); err == nil {
		_, _ = fmt.Fprintf(env.stdout, "✓ Migrated hooks from %s\n", legacyPath)
	}
}

func removeAllClaudeHooks(hooks map[string]interface{}) {
	for _, command := range []string{"bd prime", "bd prime --stealth", "bd prime --hook-json", "bd prime --stealth --hook-json"} {
		removeHookCommand(hooks, "SessionStart", command)
		removeHookCommand(hooks, "PreCompact", command)
	}
}

func installClaudeInstructions(env claudeEnv) bool {
	agentsEnv, redirected := claudeAgentsEnvRedirect(env)
	agentsSkipped := false
	agentsEnv.skipped = &agentsSkipped
	if err := installAgents(agentsEnv, claudeAgentsIntegration); err != nil {
		_, _ = fmt.Fprintf(env.stderr, "Warning: failed to update %s: %v\n", claudeInstructionsFile, err)
	}
	if redirected && !agentsSkipped {
		if err := stripStaleClaudeBlock(env); err != nil {
			_, _ = fmt.Fprintf(env.stderr, "Warning: failed to clean stale beads block from %s: %v\n", claudeInstructionsFile, err)
		}
	}
	return agentsSkipped
}

func reportClaudeInstall(env claudeEnv, settingsPath string, agentsSkipped bool) {
	if agentsSkipped {
		_, _ = fmt.Fprintln(env.stdout, "\n✓ Claude Code hooks installed")
		_, _ = fmt.Fprintf(env.stdout, "  Agent instructions skipped: %s is a symlink\n", claudeInstructionsFile)
	} else {
		_, _ = fmt.Fprintln(env.stdout, "\n✓ Claude Code integration installed")
	}
	_, _ = fmt.Fprintf(env.stdout, "  Settings: %s\n", settingsPath)
	_, _ = fmt.Fprintln(env.stdout, "\nRestart Claude Code for changes to take effect.")
}

// claudeSettingsUsesRemovedSyncCommand reports whether any hook command references
// bd sync (removed as a real command; GH#3546).
func claudeSettingsUsesRemovedSyncCommand(data []byte) bool {
	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return false
	}
	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		return false
	}
	return claudeHooksUseRemovedSyncCommand(hooks)
}

func claudeHooksUseRemovedSyncCommand(hooks map[string]interface{}) bool {
	for _, raw := range hooks {
		eventHooks, ok := raw.([]interface{})
		if ok && claudeEventUsesRemovedSyncCommand(eventHooks) {
			return true
		}
	}
	return false
}

func claudeEventUsesRemovedSyncCommand(eventHooks []interface{}) bool {
	for _, hook := range eventHooks {
		hookMap, ok := hook.(map[string]interface{})
		if !ok {
			continue
		}
		commands, ok := hookMap["hooks"].([]interface{})
		if ok && claudeCommandsUseRemovedSyncCommand(commands) {
			return true
		}
	}
	return false
}

func claudeCommandsUseRemovedSyncCommand(commands []interface{}) bool {
	for _, command := range commands {
		commandMap, ok := command.(map[string]interface{})
		if !ok {
			continue
		}
		value, _ := commandMap["command"].(string)
		if strings.Contains(value, "bd sync") {
			return true
		}
	}
	return false
}

func warnIfClaudeHooksUseRemovedSync(env claudeEnv) {
	paths := []string{
		projectSettingsPath(env.projectDir),
		globalSettingsPath(env.homeDir),
		legacyProjectSettingsPath(env.projectDir),
	}
	for _, p := range paths {
		data, err := env.readFile(p)
		if err != nil {
			continue
		}
		if !claudeSettingsUsesRemovedSyncCommand(data) {
			continue
		}
		_, _ = fmt.Fprintf(env.stderr, "Warning: %s contains a hook using removed \"bd sync\". Run bd setup claude to refresh hooks (bd prime / bd dolt push), or edit settings manually.\n", p)
	}
}
