package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jonbaldie/beads/internal/templates/agents"
)

var (
	geminiEnvProvider     = defaultGeminiEnv
	errGeminiHooksMissing = errors.New("gemini hooks not installed")
	errGeminiHooksLegacy  = errors.New("gemini hooks need upgrade")
)

const geminiInstructionsFile = "GEMINI.md"

var geminiAgentsIntegration = agentsIntegration{
	name:         "Gemini CLI",
	setupCommand: "bd setup gemini",
	profile:      agents.ProfileMinimal,
}

type geminiEnv struct {
	stdout     io.Writer
	stderr     io.Writer
	homeDir    string
	projectDir string
	ensureDir  func(string, os.FileMode) error
	readFile   func(string) ([]byte, error)
	writeFile  func(string, []byte) error
}

func defaultGeminiEnv() (geminiEnv, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return geminiEnv{}, fmt.Errorf("home directory: %w", err)
	}
	workDir, err := os.Getwd()
	if err != nil {
		return geminiEnv{}, fmt.Errorf("working directory: %w", err)
	}
	return geminiEnv{
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		homeDir:    home,
		projectDir: workDir,
		ensureDir:  EnsureDir,
		readFile:   os.ReadFile,
		writeFile: func(path string, data []byte) error {
			return atomicWriteFile(path, data)
		},
	}, nil
}

func geminiProjectSettingsPath(base string) string {
	return filepath.Join(base, ".gemini", "settings.json")
}

func geminiGlobalSettingsPath(home string) string {
	return filepath.Join(home, ".gemini", "settings.json")
}

func geminiAgentsEnv(env geminiEnv) agentsEnv {
	return agentsEnv{
		agentsPath: filepath.Join(env.projectDir, geminiInstructionsFile),
		stdout:     env.stdout,
		stderr:     env.stderr,
	}
}

func InstallGemini(project bool, stealth bool) error {
	env, err := geminiEnvProvider()
	if err != nil {
		return HandleError("%v", err)
	}
	return installGemini(env, project, stealth)
}

func installGemini(env geminiEnv, project bool, stealth bool) error {
	settingsPath := announceGeminiInstall(env, project)
	if err := env.ensureDir(filepath.Dir(settingsPath), 0o755); err != nil {
		_, _ = fmt.Fprintf(env.stderr, "Error: %v\n", err)
		return err
	}
	settings, hooks, err := loadGeminiSettings(env, settingsPath)
	if err != nil {
		return err
	}
	command := "bd prime --hook-json"
	if stealth {
		command = "bd prime --stealth --hook-json"
	}
	migrateGeminiHooks(hooks, command)
	if addHookCommand(hooks, "SessionStart", command) {
		_, _ = fmt.Fprintln(env.stdout, "✓ Registered SessionStart hook")
	}
	if err := writeGeminiSettings(env, settingsPath, settings); err != nil {
		return err
	}
	if err := installAgents(geminiAgentsEnv(env), geminiAgentsIntegration); err != nil {
		_, _ = fmt.Fprintf(env.stderr, "Warning: failed to update %s: %v\n", geminiInstructionsFile, err)
	}
	_, _ = fmt.Fprintln(env.stdout, "\n✓ Gemini CLI integration installed")
	_, _ = fmt.Fprintf(env.stdout, "  Settings: %s\n", settingsPath)
	_, _ = fmt.Fprintln(env.stdout, "\nRestart Gemini CLI for changes to take effect.")
	return nil
}

func announceGeminiInstall(env geminiEnv, project bool) string {
	if project {
		_, _ = fmt.Fprintln(env.stdout, "Installing Gemini CLI hooks for this project...")
		return geminiProjectSettingsPath(env.projectDir)
	}
	_, _ = fmt.Fprintln(env.stdout, "Installing Gemini CLI hooks globally...")
	return geminiGlobalSettingsPath(env.homeDir)
}

func loadGeminiSettings(env geminiEnv, path string) (map[string]interface{}, map[string]interface{}, error) {
	settings := make(map[string]interface{})
	if data, err := env.readFile(path); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			_, _ = fmt.Fprintf(env.stderr, "Error: failed to parse settings.json: %v\n", err)
			return nil, nil, err
		}
	}
	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		hooks = make(map[string]interface{})
		settings["hooks"] = hooks
	}
	return settings, hooks, nil
}

func migrateGeminiHooks(hooks map[string]interface{}, command string) {
	for _, legacy := range []string{"bd prime", "bd prime --stealth"} {
		if legacy != command {
			removeHookCommand(hooks, "SessionStart", legacy)
			removeHookCommand(hooks, "PreCompress", legacy)
		}
	}
	removeHookCommand(hooks, "PreCompress", "bd prime --hook-json")
	removeHookCommand(hooks, "PreCompress", "bd prime --stealth --hook-json")
}

func writeGeminiSettings(env geminiEnv, path string, settings map[string]interface{}) error {
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

func CheckGemini() error {
	env, err := geminiEnvProvider()
	if err != nil {
		return HandleError("%v", err)
	}
	return checkGemini(env)
}

func checkGemini(env geminiEnv) error {
	globalSettings := geminiGlobalSettingsPath(env.homeDir)
	projectSettings := geminiProjectSettingsPath(env.projectDir)

	switch {
	case hasCurrentGeminiHooks(globalSettings):
		_, _ = fmt.Fprintf(env.stdout, "✓ Global hooks installed (current): %s\n", globalSettings)
	case hasCurrentGeminiHooks(projectSettings):
		_, _ = fmt.Fprintf(env.stdout, "✓ Project hooks installed (current): %s\n", projectSettings)
	case hasGeminiBeadsHooks(globalSettings):
		_, _ = fmt.Fprintf(env.stdout, "⚠ Global hooks installed (legacy format): %s\n", globalSettings)
		_, _ = fmt.Fprintln(env.stdout, "  Legacy 'bd prime' hooks emit raw markdown; Gemini requires JSON stdout.")
		_, _ = fmt.Fprintln(env.stdout, "  Run: bd setup gemini  (upgrades to 'bd prime --hook-json')")
		return errGeminiHooksLegacy
	case hasGeminiBeadsHooks(projectSettings):
		_, _ = fmt.Fprintf(env.stdout, "⚠ Project hooks installed (legacy format): %s\n", projectSettings)
		_, _ = fmt.Fprintln(env.stdout, "  Legacy 'bd prime' hooks emit raw markdown; Gemini requires JSON stdout.")
		_, _ = fmt.Fprintln(env.stdout, "  Run: bd setup gemini --project  (upgrades to 'bd prime --hook-json')")
		return errGeminiHooksLegacy
	default:
		_, _ = fmt.Fprintln(env.stdout, "✗ No hooks installed")
		_, _ = fmt.Fprintln(env.stdout, "  Run: bd setup gemini")
		return errGeminiHooksMissing
	}

	return checkAgents(geminiAgentsEnv(env), geminiAgentsIntegration)
}

func RemoveGemini(project bool) error {
	env, err := geminiEnvProvider()
	if err != nil {
		return HandleError("%v", err)
	}
	return removeGemini(env, project)
}

func removeGemini(env geminiEnv, project bool) error {
	var settingsPath string
	if project {
		settingsPath = geminiProjectSettingsPath(env.projectDir)
		_, _ = fmt.Fprintln(env.stdout, "Removing Gemini CLI hooks from project...")
	} else {
		settingsPath = geminiGlobalSettingsPath(env.homeDir)
		_, _ = fmt.Fprintln(env.stdout, "Removing Gemini CLI hooks globally...")
	}

	data, err := env.readFile(settingsPath)
	if err != nil {
		_, _ = fmt.Fprintln(env.stdout, "No settings file found")
	} else {
		var settings map[string]interface{}
		if err := json.Unmarshal(data, &settings); err != nil {
			_, _ = fmt.Fprintf(env.stderr, "Error: failed to parse settings.json: %v\n", err)
			return err
		}

		hooks, ok := settings["hooks"].(map[string]interface{})
		if !ok {
			_, _ = fmt.Fprintln(env.stdout, "No hooks found")
		} else {
			// Remove all known variants from both events. PreCompress is
			// included for migration safety: older installations registered
			// bd prime there before we discovered Gemini's PreCompress hook
			// can't inject additionalContext.
			variants := []string{
				"bd prime",
				"bd prime --stealth",
				"bd prime --hook-json",
				"bd prime --stealth --hook-json",
			}
			for _, cmd := range variants {
				removeHookCommand(hooks, "SessionStart", cmd)
				removeHookCommand(hooks, "PreCompress", cmd)
			}

			data, err = json.MarshalIndent(settings, "", "  ")
			if err != nil {
				_, _ = fmt.Fprintf(env.stderr, "Error: marshal settings: %v\n", err)
				return err
			}

			if err := env.writeFile(settingsPath, data); err != nil {
				_, _ = fmt.Fprintf(env.stderr, "Error: write settings: %v\n", err)
				return err
			}
		}
	}

	if err := removeAgents(geminiAgentsEnv(env), geminiAgentsIntegration); err != nil {
		// Non-fatal
		_, _ = fmt.Fprintf(env.stderr, "Warning: failed to update %s: %v\n", geminiInstructionsFile, err)
	}

	_, _ = fmt.Fprintln(env.stdout, "✓ Gemini CLI hooks removed")
	return nil
}

// geminiSessionStartCommands returns all command strings registered under the
// SessionStart hook event in a settings file. Returns nil on any read/parse
// error. Detection scope is SessionStart only — PreCompress is advisory-only
// in Gemini CLI and does not support additionalContext injection.
func geminiSessionStartCommands(settingsPath string) []string {
	data, err := os.ReadFile(settingsPath) // #nosec G304 -- path constructed from known safe locations (user home/.gemini or cwd/.gemini)
	if err != nil {
		return nil
	}
	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil
	}
	return geminiCommandsFromSettings(settings)
}

func geminiCommandsFromSettings(settings map[string]interface{}) []string {
	hooks, _ := settings["hooks"].(map[string]interface{})
	eventHooks, _ := hooks["SessionStart"].([]interface{})
	var cmds []string
	for _, hook := range eventHooks {
		cmds = append(cmds, geminiCommandsFromHook(hook)...)
	}
	return cmds
}

func geminiCommandsFromHook(hook interface{}) []string {
	hookMap, _ := hook.(map[string]interface{})
	hookCommands, _ := hookMap["hooks"].([]interface{})
	var commands []string
	for _, command := range hookCommands {
		commandMap, _ := command.(map[string]interface{})
		if value, ok := commandMap["command"].(string); ok {
			commands = append(commands, value)
		}
	}
	return commands
}

// hasCurrentGeminiHooks reports whether a settings file has a current-format
// bd prime --hook-json command on SessionStart. Returns false for legacy
// "bd prime" installs, which emit raw markdown that violates Gemini's strict
// stdout-must-be-JSON hook contract.
func hasCurrentGeminiHooks(settingsPath string) bool {
	for _, cmd := range geminiSessionStartCommands(settingsPath) {
		if cmd == "bd prime --hook-json" || cmd == "bd prime --stealth --hook-json" {
			return true
		}
	}
	return false
}

// hasGeminiBeadsHooks reports whether a settings file has any bd prime hook
// on SessionStart — current format or legacy. Used to detect pre-fix
// installations that need upgrading via bd setup gemini.
func hasGeminiBeadsHooks(settingsPath string) bool {
	for _, cmd := range geminiSessionStartCommands(settingsPath) {
		switch cmd {
		case "bd prime", "bd prime --stealth",
			"bd prime --hook-json", "bd prime --stealth --hook-json":
			return true
		}
	}
	return false
}
