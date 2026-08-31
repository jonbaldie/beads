package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

type viperAPI interface {
	AllKeys() []string
	AllSettings() map[string]interface{}
	AutomaticEnv()
	BindEnv(input ...string) error
	ConfigFileUsed() string
	Get(key string) interface{}
	GetBool(key string) bool
	GetDuration(key string) time.Duration
	GetInt(key string) int
	GetString(key string) string
	GetStringMapString(key string) map[string]string
	GetStringSlice(key string) []string
	InConfig(key string) bool
	MergeInConfig() error
	ReadInConfig() error
	Set(key string, value interface{})
	SetConfigFile(in string)
	SetConfigType(in string)
	SetDefault(key string, value interface{})
	SetEnvKeyReplacer(r *strings.Replacer)
	SetEnvPrefix(in string)
}

type viperHolder struct {
	*viper.Viper
}

func newViperHolder() *viperHolder {
	return &viperHolder{Viper: viper.New()}
}

func (h *viperHolder) reset() {
	h.Viper = viper.New()
}

var v viperAPI = newViperHolder()

// overriddenKeys tracks keys explicitly set via Set() at runtime, so
// GetValueSource can distinguish them from Viper defaults.
var overriddenKeys = map[string]bool{}

// ignoredConfigKey normalizes a config path into the single form the
// BEADS_TEST_IGNORE_REPO_CONFIG ignore set is keyed by, so that membership does
// not depend on which alias of a directory the caller happened to hold.
//
// The set is built from os.Getwd(), which honors $PWD and therefore reports the
// path the process was given rather than the one the kernel resolved. The
// BEADS_DIR that is tested against it is written by the CLI's own dispatch from
// beads.FindBeadsDir, which runs utils.CanonicalizePath (filepath.EvalSymlinks).
// Comparing those two strings directly misses whenever the workspace is reached
// through a symlink, the ignore is not applied, and the repo config is merged
// after all — which is exactly what the flag exists to prevent.
//
// On macOS that is not an edge case, it is every temp workspace: $TMPDIR is
// /var/folders/... and /var is a symlink to /private/var, so every t.TempDir()
// has two names and the two sides pick different ones.
//
// Symlinks are resolved on the directory, not the file: callers pass candidate
// config paths that may not exist, and EvalSymlinks fails on a missing leaf.
// When the directory cannot be resolved either, fall back to a lexical clean —
// applied identically on both insert and lookup, so the two still agree.
func ignoredConfigKey(path string) string {
	if path == "" {
		return ""
	}
	dir, base := filepath.Split(filepath.Clean(path))
	resolvedDir, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(resolvedDir, base))
}

// Initialize sets up the viper configuration singleton
// Should be called once at application startup
func Initialize() error {
	resetViper()
	paths := discoverConfigPaths()
	configureViper()
	setConfigDefaults()
	return loadConfigFiles(paths)
}

func resetViper() {
	if holder, ok := v.(*viperHolder); ok {
		holder.reset()
	}
}

// ResetForTesting clears the config state, allowing Initialize() to be called again.
// This is intended for tests that need to change config.yaml between test steps.
// WARNING: Not thread-safe. Only call from single-threaded test contexts.
func ResetForTesting() {
	resetViper()
	resetOverrideKeys(overriddenKeys)
}

func resetOverrideKeys(keys map[string]bool) {
	clear(keys)
}

func worktreeFallbackConfigPath(repoPath string) string {
	gitDir, commonDir, ok := gitDirsForRepo(repoPath)
	if !ok || samePath(gitDir, commonDir) {
		return ""
	}

	if filepath.Base(commonDir) == ".git" {
		return filepath.Join(filepath.Dir(commonDir), ".beads", "config.yaml")
	}

	return filepath.Join(commonDir, ".beads", "config.yaml")
}

func gitDirsForRepo(repoPath string) (gitDir, commonDir string, ok bool) {
	cmd := exec.Command("git", "-C", repoPath, "rev-parse", "--git-dir", "--git-common-dir")
	output, err := cmd.Output()
	if err != nil {
		return "", "", false
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 {
		return "", "", false
	}

	gitDir = gitPathForRepo(repoPath, strings.TrimSpace(lines[0]))
	commonDir = gitPathForRepo(repoPath, strings.TrimSpace(lines[1]))
	if gitDir == "" || commonDir == "" {
		return "", "", false
	}

	return gitDir, commonDir, true
}

func gitPathForRepo(repoPath, path string) string {
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoPath, path)
	}

	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}

	return path
}

func samePath(left, right string) bool {
	if left == "" || right == "" {
		return left == right
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

// ConfigSource represents where a configuration value came from
type ConfigSource string

const (
	SourceDefault    ConfigSource = "default"
	SourceConfigFile ConfigSource = "config_file"
	SourceEnvVar     ConfigSource = "env_var"
	SourceFlag       ConfigSource = "flag"
)

// ConfigOverride represents a detected configuration override
type ConfigOverride struct {
	Key            string
	EffectiveValue interface{}
	OverriddenBy   ConfigSource
	OriginalSource ConfigSource
	OriginalValue  interface{}
}

// GetValueSource returns the source of a configuration value.
// Priority (highest to lowest): env var > config file > default
// Note: Flag overrides are handled separately in main.go since viper doesn't know about cobra flags.
func GetValueSource(key string) ConfigSource {
	if v == nil {
		return SourceDefault
	}

	// Check if value is set from environment variable.
	// Use LookupEnv (not Getenv) so that explicitly-set-but-empty vars like
	// BD_BACKUP_ENABLED= are recognized as "set by the user" rather than
	// falling through to the default/auto-detect path.
	envKey := "BD_" + strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), ".", "_"))
	if _, ok := os.LookupEnv(envKey); ok {
		return SourceEnvVar
	}

	// Check BEADS_ prefixed env vars for legacy compatibility
	beadsEnvKey := "BEADS_" + strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), ".", "_"))
	if _, ok := os.LookupEnv(beadsEnvKey); ok {
		return SourceEnvVar
	}

	// Check if value is set in config file (as opposed to being a default)
	if v.InConfig(key) {
		return SourceConfigFile
	}

	// Check if value was explicitly set via Set() at runtime
	if overriddenKeys[key] {
		return SourceConfigFile
	}

	return SourceDefault
}

// EnvVarName returns the environment variable name that would override the given
// config key, if one is set. Returns the BD_ or BEADS_ prefixed name, or empty
// string if no env var is set for this key.
func EnvVarName(key string) string {
	envKey := "BD_" + strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), ".", "_"))
	if _, ok := os.LookupEnv(envKey); ok {
		return envKey
	}
	beadsEnvKey := "BEADS_" + strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), ".", "_"))
	if _, ok := os.LookupEnv(beadsEnvKey); ok {
		return beadsEnvKey
	}
	return ""
}

// CheckOverrides checks for configuration overrides and returns a list of detected overrides.
// This is useful for informing users when env vars or flags override config file values.
// flagOverrides is a map of key -> (flagValue, flagWasSet) for flags that were explicitly set.
func CheckOverrides(flagOverrides map[string]struct {
	Value  interface{}
	WasSet bool
}) []ConfigOverride {
	var overrides []ConfigOverride

	for key, flagInfo := range flagOverrides {
		if override, ok := flagOverride(key, flagInfo.Value, flagInfo.WasSet); ok {
			overrides = append(overrides, override)
		}
	}

	return append(overrides, environmentOverrides()...)
}

func flagOverride(key string, value interface{}, wasSet bool) (ConfigOverride, bool) {
	if !wasSet {
		return ConfigOverride{}, false
	}
	source := GetValueSource(key)
	if source != SourceConfigFile && source != SourceEnvVar {
		return ConfigOverride{}, false
	}
	return ConfigOverride{
		Key:            key,
		EffectiveValue: value,
		OverriddenBy:   SourceFlag,
		OriginalSource: source,
		OriginalValue:  originalFlagValue(key, value),
	}, true
}

func originalFlagValue(key string, value interface{}) interface{} {
	switch value := value.(type) {
	case bool:
		return GetBool(key)
	case string:
		return GetString(key)
	case int:
		return GetInt(key)
	default:
		return value
	}
}

func environmentOverrides() []ConfigOverride {
	if v == nil {
		return nil
	}
	var overrides []ConfigOverride
	for _, key := range v.AllKeys() {
		if override, ok := environmentOverride(key); ok {
			overrides = append(overrides, override)
		}
	}
	return overrides
}

func environmentOverride(key string) (ConfigOverride, bool) {
	if GetValueSource(key) != SourceEnvVar || !v.InConfig(key) {
		return ConfigOverride{}, false
	}
	if EnvVarName(key) == "" {
		return ConfigOverride{}, false
	}
	return ConfigOverride{
		Key:            key,
		EffectiveValue: v.Get(key),
		OverriddenBy:   SourceEnvVar,
		OriginalSource: SourceConfigFile,
		OriginalValue:  nil, // We can't easily get the config file value separately
	}, true
}

// LogOverride logs a message about a configuration override in verbose mode.
func LogOverride(override ConfigOverride) {
	var sourceDesc string
	switch override.OriginalSource {
	case SourceConfigFile:
		sourceDesc = "config file"
	case SourceEnvVar:
		sourceDesc = "environment variable"
	case SourceDefault:
		sourceDesc = "default"
	default:
		sourceDesc = string(override.OriginalSource)
	}

	var overrideDesc string
	switch override.OverriddenBy {
	case SourceFlag:
		overrideDesc = "command-line flag"
	case SourceEnvVar:
		overrideDesc = "environment variable"
	default:
		overrideDesc = string(override.OverriddenBy)
	}

	// Always emit to stderr when verbose mode is enabled (caller guards on verbose)
	fmt.Fprintf(os.Stderr, "Config: %s overridden by %s (was: %v from %s, now: %v)\n",
		override.Key, overrideDesc, override.OriginalValue, sourceDesc, override.EffectiveValue)
}

// SaveConfigValue sets a key-value pair and writes it to the config file.
// If no config file is currently loaded, it creates config.yaml in the given beadsDir.
// Only the specified key is modified; other file contents are preserved.
func SaveConfigValue(key string, value interface{}, beadsDir string) error {
	if v == nil {
		return fmt.Errorf("config not initialized")
	}
	v.Set(key, value)

	configPath := v.ConfigFileUsed()
	if configPath == "" {
		configPath = filepath.Join(beadsDir, "config.yaml")
		v.SetConfigFile(configPath)
	}

	// Read existing file contents to avoid dumping all merged viper state
	// (defaults, env vars, overrides) into the config file.
	existing := make(map[string]interface{})
	if data, err := os.ReadFile(filepath.Clean(configPath)); err == nil {
		_ = yaml.Unmarshal(data, &existing)
	}

	// Set the single key using dot-path splitting for nested keys (e.g. "routing.mode").
	setNestedKey(existing, key, value)

	out, err := yaml.Marshal(existing)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	return os.WriteFile(configPath, out, 0o600)
}

// setNestedKey sets a value in a nested map using a dot-separated key path.
func setNestedKey(m map[string]interface{}, key string, value interface{}) {
	parts := strings.SplitN(key, ".", 2)
	if len(parts) == 1 {
		m[key] = value
		return
	}
	sub, ok := m[parts[0]].(map[string]interface{})
	if !ok {
		sub = make(map[string]interface{})
		m[parts[0]] = sub
	}
	setNestedKey(sub, parts[1], value)
}
