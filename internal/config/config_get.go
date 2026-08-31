package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// GetString retrieves a string configuration value
func GetString(key string) string {
	if v == nil {
		return ""
	}
	return v.GetString(key)
}

// GetStringFromDir reads a single string configuration value directly from
// <beadsDir>/config.yaml without using or modifying global viper state.
// This is intended for library consumers that call NewFromConfigWithOptions
// without first invoking config.Initialize().
//
// The key uses dotted notation (e.g. "dolt.auto-start"). YAML booleans and
// numbers are coerced to their string representations ("true", "false", etc.).
// Returns "" if the file is absent, the key is not found, or any error occurs.
func GetStringFromDir(beadsDir, key string) string {
	configPath := filepath.Join(beadsDir, "config.yaml")
	data, err := os.ReadFile(configPath) //nolint:gosec // G304: path is the selected workspace's fixed config.yaml.
	if err != nil {
		return ""
	}
	var root map[string]interface{}
	if err := yaml.Unmarshal(data, &root); err != nil {
		return ""
	}
	val, ok := getConfigValueFromMap(root, key)
	if !ok {
		return ""
	}
	switch s := val.(type) {
	case string:
		return s
	default:
		return fmt.Sprintf("%v", s)
	}
}

func getConfigValueFromMap(root map[string]interface{}, key string) (interface{}, bool) {
	node := root
	parts := strings.Split(key, ".")
	for index, part := range parts {
		value, ok := node[part]
		if !ok {
			return nil, false
		}
		if index == len(parts)-1 {
			return value, true
		}
		var okMap bool
		node, okMap = value.(map[string]interface{})
		if !okMap {
			return nil, false
		}
	}
	return nil, false
}

// GetBool retrieves a boolean configuration value
func GetBool(key string) bool {
	if v == nil {
		return false
	}
	return v.GetBool(key)
}

// GetInt retrieves an integer configuration value
func GetInt(key string) int {
	if v == nil {
		return 0
	}
	return v.GetInt(key)
}

// GetDuration retrieves a duration configuration value
func GetDuration(key string) time.Duration {
	if v == nil {
		return 0
	}
	return v.GetDuration(key)
}

// Set sets a configuration value
func Set(key string, value interface{}) {
	if v != nil {
		v.Set(key, value)
		markOverrideKey(overriddenKeys, key)
	}
}

func markOverrideKey(keys map[string]bool, key string) {
	keys[key] = true
}

// BindPFlag is reserved for future use if we want to bind Cobra flags directly to Viper
// For now, we handle flag precedence manually in PersistentPreRun
// Uncomment and implement if needed:
//
// func BindPFlag(key string, flag *pflag.Flag) error {
// 	if v == nil {
// 		return fmt.Errorf("viper not initialized")
// 	}
// 	return v.BindPFlag(key, flag)
// }

// DefaultAIModel returns the configured AI model identifier.
// Override via: bd config set ai.model "model-name" or BD_AI_MODEL=model-name
func DefaultAIModel() string {
	return GetString("ai.model")
}

// DefaultAIModelFor returns the model for Anthropic-compatible AI calls,
// accounting for which provider the resolved key selects: an explicitly
// configured ai.model always wins; otherwise a MiniMax-selected key gets a
// MiniMax-served default (MINIMAX_MODEL env > MiniMaxDefaultModel), since
// MiniMax does not serve the Claude default model.
func DefaultAIModelFor(keySource AIAPIKeySource) string {
	if GetValueSource("ai.model") != SourceDefault {
		return GetString("ai.model")
	}
	if keySource == AIAPIKeySourceMiniMaxEnv {
		if m := os.Getenv("MINIMAX_MODEL"); m != "" {
			return m
		}
		return MiniMaxDefaultModel
	}
	return GetString("ai.model")
}

// AIAPIKeySource identifies where the active Anthropic-compatible API key came from.
type AIAPIKeySource string

const (
	AIAPIKeySourceNone         AIAPIKeySource = ""
	AIAPIKeySourceAnthropicEnv AIAPIKeySource = "ANTHROPIC_API_KEY" //nolint:gosec // Environment variable name, not a credential.
	AIAPIKeySourceMiniMaxEnv   AIAPIKeySource = "MINIMAX_API_KEY"   //nolint:gosec // Environment variable name, not a credential.
	AIAPIKeySourceConfig       AIAPIKeySource = "ai.api_key"
	AIAPIKeySourceExplicit     AIAPIKeySource = "explicit"

	MiniMaxDefaultBaseURL = "https://api.minimax.io/anthropic"

	// MiniMaxDefaultModel is used when MINIMAX_API_KEY selected the key and
	// the user did not configure ai.model: MiniMax's Anthropic-compatible
	// endpoint does not serve the Claude default model, so key-only setup
	// must route to a model MiniMax actually hosts. Override with
	// MINIMAX_MODEL or ai.model.
	MiniMaxDefaultModel = "MiniMax-M2"
)

// ResolveAIAPIKey returns the API key for Anthropic-compatible AI calls.
//
// Precedence: ANTHROPIC_API_KEY > MINIMAX_API_KEY > ai.api_key > explicit.
func ResolveAIAPIKey(explicit string) (string, AIAPIKeySource) {
	if envKey := os.Getenv("ANTHROPIC_API_KEY"); envKey != "" {
		return envKey, AIAPIKeySourceAnthropicEnv
	}
	if envKey := os.Getenv("MINIMAX_API_KEY"); envKey != "" {
		return envKey, AIAPIKeySourceMiniMaxEnv
	}
	if configKey := GetString("ai.api_key"); configKey != "" {
		return configKey, AIAPIKeySourceConfig
	}
	if explicit != "" {
		return explicit, AIAPIKeySourceExplicit
	}
	return "", AIAPIKeySourceNone
}

// DefaultAIBaseURL returns the configured base URL for Anthropic-compatible AI calls.
//
// Precedence: ai.base_url (or BD_AI_BASE_URL) > MINIMAX_BASE_URL > MiniMax default
// when MINIMAX_API_KEY selected the key. Empty means use the SDK's Anthropic default.
func DefaultAIBaseURL(keySource AIAPIKeySource) string {
	if baseURL := GetString("ai.base_url"); baseURL != "" {
		return baseURL
	}
	if keySource == AIAPIKeySourceMiniMaxEnv {
		if baseURL := os.Getenv("MINIMAX_BASE_URL"); baseURL != "" {
			return baseURL
		}
		return MiniMaxDefaultBaseURL
	}
	return ""
}

// AllSettings returns all configuration settings as a map
func AllSettings() map[string]interface{} {
	if v == nil {
		return map[string]interface{}{}
	}
	return v.AllSettings()
}

// AllKeys returns all keys in the viper registry (defaults + config file + env).
// Keys are returned in lowercase dot-notation (e.g., "federation.remote").
func AllKeys() []string {
	if v == nil {
		return nil
	}
	return v.AllKeys()
}

// ConfigFileUsed returns the path to the config file that was loaded.
// Returns empty string if no config file was found or viper is not initialized.
// This is useful for resolving relative paths from the config file's directory.
func ConfigFileUsed() string {
	if v == nil {
		return ""
	}
	return v.ConfigFileUsed()
}

// GetStringSlice retrieves a string slice configuration value
func GetStringSlice(key string) []string {
	if v == nil {
		return []string{}
	}
	return v.GetStringSlice(key)
}

// GetStringMapString retrieves a map[string]string configuration value
func GetStringMapString(key string) map[string]string {
	if v == nil {
		return map[string]string{}
	}
	return v.GetStringMapString(key)
}

// GetDirectoryLabels returns labels for the current working directory based on config.
// It checks directory.labels config for matching patterns.
// Returns nil if no labels are configured for the current directory.
func GetDirectoryLabels() []string {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}

	dirLabels := GetStringMapString("directory.labels")
	if len(dirLabels) == 0 {
		return nil
	}

	// Check each configured directory pattern
	for pattern, label := range dirLabels {
		// Support both exact match and suffix match
		// e.g., "packages/maverick" matches "/path/to/repo/packages/maverick"
		if strings.HasSuffix(cwd, pattern) || strings.HasSuffix(cwd, filepath.Clean(pattern)) {
			return []string{label}
		}
		// Also try as a path prefix (user might be in a subdirectory)
		if strings.Contains(cwd, "/"+pattern+"/") || strings.Contains(cwd, "/"+pattern) {
			return []string{label}
		}
	}

	return nil
}

// MultiRepoConfig contains configuration for multi-repo support
type MultiRepoConfig struct {
	Primary    string   // Primary repo path (where canonical issues live)
	Additional []string // Additional repos to hydrate from
}

// GetMultiRepoConfig retrieves multi-repo configuration
// Returns nil if multi-repo is not configured (single-repo mode)
func GetMultiRepoConfig() *MultiRepoConfig {
	if v == nil {
		return nil
	}

	// Check if repos.primary is set (indicates multi-repo mode)
	primary := v.GetString("repos.primary")
	if primary == "" {
		return nil // Single-repo mode
	}

	return &MultiRepoConfig{
		Primary:    primary,
		Additional: v.GetStringSlice("repos.additional"),
	}
}

// GetExternalProjects returns the external_projects configuration.
// Maps project names to paths for cross-project dependency resolution.
// Example config.yaml:
//
//	external_projects:
//	  beads: ../beads
//	  other-project: /absolute/path/to/other-project
func GetExternalProjects() map[string]string {
	return GetStringMapString("external_projects")
}

// ResolveExternalProjectPath resolves a project name to its absolute path.
// Returns empty string if project not configured or path doesn't exist.
func ResolveExternalProjectPath(projectName string) string {
	projects := GetExternalProjects()
	path, ok := projects[projectName]
	if !ok {
		return ""
	}

	// Resolve relative paths from repo root (parent of .beads/), NOT CWD.
	// This ensures paths like "../beads" in config resolve correctly
	// when running from different directories.
	if !filepath.IsAbs(path) {
		// Config is at .beads/config.yaml, so go up twice to get repo root
		configFile := ConfigFileUsed()
		if configFile != "" {
			repoRoot := filepath.Dir(filepath.Dir(configFile)) // .beads/config.yaml -> repo/
			path = filepath.Join(repoRoot, path)
		} else {
			// Fallback: resolve from CWD (legacy behavior)
			cwd, err := os.Getwd()
			if err != nil {
				return ""
			}
			path = filepath.Join(cwd, path)
		}
	}

	// Verify path exists
	if _, err := os.Stat(path); err != nil {
		return ""
	}

	return path
}
