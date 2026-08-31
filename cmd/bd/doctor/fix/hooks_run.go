package fix

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

// CheckLefthookBdIntegration parses lefthook config (YAML, TOML, or JSON) and checks if bd hooks are integrated.
// See https://lefthook.dev/configuration/ for supported config file locations.
func CheckLefthookBdIntegration(path string) *HookIntegrationStatus {
	configPath := findLefthookConfig(path)
	if configPath == "" {
		return nil
	}
	config, ok := parseLefthookConfig(configPath)
	if !ok {
		return nil
	}
	return inspectLefthookBdHooks(config)
}

func findLefthookConfig(path string) string {
	for _, name := range lefthookConfigFiles {
		p := filepath.Join(path, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func parseLefthookConfig(configPath string) (map[string]interface{}, bool) {
	content, err := os.ReadFile(configPath) // #nosec G304 - path is validated
	if err != nil {
		return nil, false
	}
	var config map[string]interface{}
	switch filepath.Ext(configPath) {
	case ".toml":
		if _, err := toml.Decode(string(content), &config); err != nil {
			return nil, false
		}
	case ".json":
		if err := json.Unmarshal(content, &config); err != nil {
			return nil, false
		}
	default: // .yml, .yaml
		if err := yaml.Unmarshal(content, &config); err != nil {
			return nil, false
		}
	}
	return config, true
}

func inspectLefthookBdHooks(config map[string]interface{}) *HookIntegrationStatus {
	status := &HookIntegrationStatus{
		Manager:    "lefthook",
		Configured: false,
	}
	for _, hookName := range recommendedBdHooks {
		hookSection, ok := config[hookName]
		if !ok {
			status.HooksNotInConfig = append(status.HooksNotInConfig, hookName)
			continue
		}
		if hasBdInCommands(hookSection) {
			status.HooksWithBd = append(status.HooksWithBd, hookName)
			status.Configured = true
			continue
		}
		status.HooksWithoutBd = append(status.HooksWithoutBd, hookName)
	}
	return status
}

// hasBdInCommands checks if any command's "run" field contains bd hooks run.
// Walks the lefthook structure for both syntaxes:
// - commands (map-based, older): hookSection.commands.*.run
// - jobs (array-based, v1.10.0+): hookSection.jobs[*].run
func hasBdInCommands(hookSection interface{}) bool {
	sectionMap, ok := hookSection.(map[string]interface{})
	if !ok {
		return false
	}
	return lefthookCommandsHaveBd(sectionMap) || lefthookJobsHaveBd(sectionMap)
}

func lefthookCommandsHaveBd(sectionMap map[string]interface{}) bool {
	commands, ok := sectionMap["commands"]
	if !ok {
		return false
	}
	commandsMap, ok := commands.(map[string]interface{})
	if !ok {
		return false
	}
	for _, cmdConfig := range commandsMap {
		if hasBdInRunField(cmdConfig) {
			return true
		}
	}
	return false
}

func lefthookJobsHaveBd(sectionMap map[string]interface{}) bool {
	jobs, ok := sectionMap["jobs"]
	if !ok {
		return false
	}
	jobsList, ok := jobs.([]interface{})
	if !ok {
		return false
	}
	for _, job := range jobsList {
		if hasBdInRunField(job) {
			return true
		}
	}
	return false
}

// hasBdInRunField checks if a command/job config has bd hooks run in its "run" field.
func hasBdInRunField(config interface{}) bool {
	configMap, ok := config.(map[string]interface{})
	if !ok {
		return false
	}

	runVal, ok := configMap["run"]
	if !ok {
		return false
	}

	runStr, ok := runVal.(string)
	if !ok {
		return false
	}

	return bdHookPattern.MatchString(runStr)
}

// precommitConfigFiles lists pre-commit config files.
var precommitConfigFiles = []string{".pre-commit-config.yaml", ".pre-commit-config.yml"}

// CheckPrecommitBdIntegration parses pre-commit config and checks if bd hooks are integrated.
// See https://pre-commit.com/ for config file format.
func CheckPrecommitBdIntegration(path string) *HookIntegrationStatus {
	// Find first existing config file
	var configPath string
	for _, name := range precommitConfigFiles {
		p := filepath.Join(path, name)
		if _, err := os.Stat(p); err == nil {
			configPath = p
			break
		}
	}
	if configPath == "" {
		return nil
	}

	content, err := os.ReadFile(configPath) // #nosec G304 - path is validated
	if err != nil {
		return nil
	}

	// Parse YAML config
	var config map[string]interface{}
	if err := yaml.Unmarshal(content, &config); err != nil {
		return nil
	}

	status := &HookIntegrationStatus{
		Manager:    "pre-commit",
		Configured: false,
	}

	// Track which hooks have bd integration
	hooksWithBd := make(map[string]bool)

	// Parse repos list
	repos, ok := config["repos"]
	if !ok {
		// Empty config, all hooks missing
		status.HooksNotInConfig = recommendedBdHooks
		return status
	}

	reposList, ok := repos.([]interface{})
	if !ok {
		status.HooksNotInConfig = recommendedBdHooks
		return status
	}

	collectPrecommitBdHooks(reposList, hooksWithBd)
	finishPrecommitBdStatus(status, hooksWithBd)
	return status
}

func collectPrecommitBdHooks(reposList []interface{}, hooksWithBd map[string]bool) {
	for _, repo := range reposList {
		repoMap, ok := repo.(map[string]interface{})
		if !ok {
			continue
		}
		hooks, ok := repoMap["hooks"]
		if !ok {
			continue
		}
		hooksList, ok := hooks.([]interface{})
		if !ok {
			continue
		}
		for _, hook := range hooksList {
			markPrecommitBdHook(hook, hooksWithBd)
		}
	}
}

func markPrecommitBdHook(hook interface{}, hooksWithBd map[string]bool) {
	hookMap, ok := hook.(map[string]interface{})
	if !ok {
		return
	}
	entry, ok := hookMap["entry"]
	if !ok {
		return
	}
	entryStr, ok := entry.(string)
	if !ok {
		return
	}
	if !bdHookPattern.MatchString(entryStr) {
		return
	}
	for _, stage := range getPrecommitStages(hookMap) {
		hooksWithBd[stage] = true
	}
}

func finishPrecommitBdStatus(status *HookIntegrationStatus, hooksWithBd map[string]bool) {
	for _, hookName := range recommendedBdHooks {
		if hooksWithBd[hookName] {
			status.HooksWithBd = append(status.HooksWithBd, hookName)
			status.Configured = true
			continue
		}
		status.HooksNotInConfig = append(status.HooksNotInConfig, hookName)
	}
}

// getPrecommitStages extracts the stages from a pre-commit hook config.
// Returns the hook stages, defaulting to ["pre-commit"] if not specified.
// Handles both new format (stages: [pre-commit]) and legacy format (stages: [commit]).
func getPrecommitStages(hookMap map[string]interface{}) []string {
	stages, ok := hookMap["stages"]
	if !ok {
		// Default to pre-commit if no stages specified
		return []string{"pre-commit"}
	}

	stagesList, ok := stages.([]interface{})
	if !ok {
		return []string{"pre-commit"}
	}

	var result []string
	for _, s := range stagesList {
		stage, ok := s.(string)
		if !ok {
			continue
		}
		// Normalize legacy stage names (pre-3.2.0)
		switch stage {
		case "commit":
			result = append(result, "pre-commit")
		case "push":
			result = append(result, "pre-push")
		case "merge-commit":
			result = append(result, "pre-merge-commit")
		default:
			result = append(result, stage)
		}
	}

	if len(result) == 0 {
		return []string{"pre-commit"}
	}
	return result
}

// CheckHuskyBdIntegration checks .husky/ scripts for bd integration.
func CheckHuskyBdIntegration(path string) *HookIntegrationStatus {
	huskyDir := filepath.Join(path, ".husky")
	if _, err := os.Stat(huskyDir); os.IsNotExist(err) {
		return nil
	}

	status := &HookIntegrationStatus{
		Manager:    "husky",
		Configured: false,
	}

	for _, hookName := range recommendedBdHooks {
		hookPath := filepath.Join(huskyDir, hookName)
		content, err := os.ReadFile(hookPath) // #nosec G304 - path is validated
		if err != nil {
			// Hook script doesn't exist in .husky/
			status.HooksNotInConfig = append(status.HooksNotInConfig, hookName)
			continue
		}

		contentStr := string(content)

		// Check for bd hooks run pattern
		if bdHookPattern.MatchString(contentStr) {
			status.HooksWithBd = append(status.HooksWithBd, hookName)
			status.Configured = true
		} else {
			status.HooksWithoutBd = append(status.HooksWithoutBd, hookName)
		}
	}

	return status
}

// hkConfigFiles lists hk config files (derived from hookManagerConfigs).
var hkConfigFiles = []string{"hk.pkl", ".config/hk.pkl", "hk.local.pkl", ".config/hk.local.pkl"}

// CheckHkBdIntegration checks hk Pkl config for bd integration.
// hk uses Pkl configuration where hooks are nested under hooks { ["hook-name"] { steps { ... } } }.
// See https://hk.jdx.dev/ for config format.
func CheckHkBdIntegration(path string) *HookIntegrationStatus {
	// Find first existing config file
	var configPath string
	for _, name := range hkConfigFiles {
		p := filepath.Join(path, name)
		if _, err := os.Stat(p); err == nil {
			configPath = p
			break
		}
	}
	if configPath == "" {
		return nil
	}

	content, err := os.ReadFile(configPath) // #nosec G304 - path is validated
	if err != nil {
		return nil
	}

	contentStr := string(content)

	status := &HookIntegrationStatus{
		Manager:    "hk",
		Configured: false,
	}

	// For each recommended hook, check two things:
	// 1. Is the hook section present? (e.g., ["pre-commit"] appears in config)
	// 2. Does "bd hooks run <hookname>" appear in the config?
	// Since bd hooks run commands include the hook name, we can match them directly.
	for _, hookName := range recommendedBdHooks {
		sectionPattern := regexp.MustCompile(`\[\s*"` + regexp.QuoteMeta(hookName) + `"\s*\]`)
		hasBdRun := regexp.MustCompile(`\bbd\s+hooks\s+run\s+` + regexp.QuoteMeta(hookName) + `\b`)

		sectionExists := sectionPattern.MatchString(contentStr)
		bdRunExists := hasBdRun.MatchString(contentStr)

		if bdRunExists {
			status.HooksWithBd = append(status.HooksWithBd, hookName)
			status.Configured = true
		} else if sectionExists {
			status.HooksWithoutBd = append(status.HooksWithoutBd, hookName)
		} else {
			status.HooksNotInConfig = append(status.HooksNotInConfig, hookName)
		}
	}

	return status
}

// checkManagerBdIntegration checks a specific manager for bd integration.
func checkManagerBdIntegration(name, path string) *HookIntegrationStatus {
	switch name {
	case "lefthook":
		return CheckLefthookBdIntegration(path)
	case "husky":
		return CheckHuskyBdIntegration(path)
	case "hk":
		return CheckHkBdIntegration(path)
	case "pre-commit", "prek":
		// prek uses the same config format as pre-commit
		status := CheckPrecommitBdIntegration(path)
		if status != nil {
			status.Manager = name // Use the actual detected manager name
		}
		return status
	default:
		return nil
	}
}

// CheckExternalHookManagerIntegration checks if detected hook managers have bd integration.
func CheckExternalHookManagerIntegration(path string) *HookIntegrationStatus {
	managers := DetectExternalHookManagers(path)
	if len(managers) == 0 {
		return nil
	}

	// First, try to detect which manager is actually active from git hooks
	if activeManager := DetectActiveHookManager(path); activeManager != "" {
		if status := checkManagerBdIntegration(activeManager, path); status != nil {
			return status
		}
	}

	// Fall back to checking detected managers in order
	for _, m := range managers {
		if status := checkManagerBdIntegration(m.Name, path); status != nil {
			return status
		}
	}

	// Return basic status for unsupported managers (detection only, can't verify config)
	return &HookIntegrationStatus{
		Manager:       ManagerNames(managers),
		Configured:    false,
		DetectionOnly: true,
	}
}

// ManagerNames extracts names from a slice of ExternalHookManager as comma-separated string.
func ManagerNames(managers []ExternalHookManager) string {
	names := make([]string, len(managers))
	for i, m := range managers {
		names[i] = m.Name
	}
	return strings.Join(names, ", ")
}

// GitHooks fixes missing or broken git hooks by calling bd hooks install.
// If external hook managers are detected (lefthook, husky, etc.), it uses
// --chain to preserve existing hooks instead of overwriting them.
func GitHooks(path string) error {
	// Validate workspace
	if err := validateBeadsWorkspace(path); err != nil {
		return err
	}

	// Check if we're in a git repository using git rev-parse
	// This handles worktrees where .git is a file, not a directory
	checkCmd := exec.Command("git", "rev-parse", "--git-dir")
	checkCmd.Dir = path
	if err := checkCmd.Run(); err != nil {
		return fmt.Errorf("not a git repository")
	}

	// Detect external hook managers
	externalManagers := DetectExternalHookManagers(path)

	// Get bd binary path
	bdBinary, err := getBdBinary()
	if err != nil {
		return err
	}

	// Build command arguments
	// Use --force to cleanly replace outdated hooks without creating backups (GH#1466)
	args := []string{"hooks", "install", "--force"}

	// If external hook managers detected, use --chain to preserve them
	if len(externalManagers) > 0 {
		args = append(args, "--chain")
	}

	// Run bd hooks install
	cmd := newBdCmd(bdBinary, args...)
	cmd.Dir = path // Set working directory without changing process dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to install hooks: %w", err)
	}

	return nil
}
