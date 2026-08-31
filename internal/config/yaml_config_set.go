package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// SetYamlConfig sets a configuration value in the project's config.yaml file.
// It handles both adding new keys and updating existing (possibly commented) keys.
// Keys are normalized to their canonical yaml format (e.g., sync.branch -> sync-branch).
func SetYamlConfig(key, value string) error {
	// Validate specific keys (GH#995)
	if err := validateYamlConfigValue(key, value); err != nil {
		return err
	}

	configPath, err := findProjectConfigYaml()
	if err != nil {
		return err
	}

	return setYamlConfigAtPath(configPath, key, value)
}

// SetYamlConfigInDir sets a configuration value in the config.yaml located in
// the provided beadsDir, bypassing CWD/worktree discovery. Use this when the
// caller has already resolved the authoritative workspace and needs to avoid
// local worktree stubs shadowing the real shared config location.
func SetYamlConfigInDir(beadsDir, key, value string) error {
	// Validate specific keys (GH#995)
	if err := validateYamlConfigValue(key, value); err != nil {
		return err
	}

	configPath := filepath.Join(beadsDir, "config.yaml")
	if _, err := os.Stat(configPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no config.yaml found in %s (run 'bd init' first)", beadsDir)
		}
		return fmt.Errorf("failed to stat config.yaml: %w", err)
	}

	return setYamlConfigAtPath(configPath, key, value)
}

var userGlobalKeyPrefixes = []string{"metrics."}

// userGlobalExactKeys are per-MACHINE settings that must never be written to
// the project .beads/config.yaml, which is a git-TRACKED file (see
// cmd/bd/doctor/gitignore.go: nothing in .beads/.gitignore excludes it). A
// committed value propagates one machine's answer to every clone that pulls
// it, which for these keys is worse than having no value at all.
//
// node_id is the exemplar: it names the beads STORE that grants leases here,
// and the reclaim guard (issueops.ReclaimExpiredLeasesInTx) compares it
// against each lease's granted_node. Commit "node_id: mini" and every replica
// reads "mini", so every comparison matches and the guard is simultaneously
// fully ARMED and fully INERT — laptop reaps mini's leases exactly as if they
// were local, which is the precise hazard the guard exists to close, now
// happening while the operator believes they are protected. Routing the write
// to ~/.config/bd/config.yaml keeps it per-machine; viper still merges that
// file, so config.NodeID() reads it back.
var userGlobalExactKeys = map[string]bool{"node_id": true}

func IsUserGlobalKey(key string) bool {
	if userGlobalExactKeys[key] {
		return true
	}
	for _, prefix := range userGlobalKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// readUserGlobalYamlValue reads a single dotted key from the user-global
// config.yaml ONLY, never project or BEADS_DIR config. It accepts both the
// nested form (metrics:\n  disabled: true) and the flat dotted form
// (metrics.disabled: true). It returns the raw scalar string and whether the
// key was present.
//
// Consent-bearing settings (metrics enablement and endpoint) are resolved
// through this rather than merged viper so a repository's .beads/config.yaml can
// never re-enable metrics for a user who opted out, nor redirect where metrics
// are sent. See MetricsDisabledByUserConfig / UserMetricsEndpoint.
func readUserGlobalYamlValue(key string) (string, bool) {
	configPath, err := UserConfigYamlPath()
	if err != nil {
		return "", false
	}
	return readYamlValueAtPath(configPath, key)
}

// WorkspaceYamlValue reads a single dotted key out of ONE workspace's
// config.yaml, named by its .beads directory, returning ("", false) when the
// file or the key is absent.
//
// It exists for the cross-workspace opens — routed creates, remote-cache
// hydration, `bd serve` against another workspace — where the process-wide
// merged config answers for the directory bd was LAUNCHED from, not for the
// workspace about to be written. A setting that governs what gets recorded in a
// target workspace has to be read from that target.
func WorkspaceYamlValue(beadsDir, key string) (string, bool) {
	if beadsDir == "" {
		return "", false
	}
	return readYamlValueAtPath(filepath.Join(beadsDir, "config.yaml"), key)
}

func readYamlValueAtPath(path, key string) (string, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // path is a resolved config.yaml path, not user input
	if err != nil {
		return "", false
	}
	var root map[string]interface{}
	if err := yaml.Unmarshal(data, &root); err != nil {
		return "", false
	}
	if raw, ok := root[key]; ok { // flat dotted form
		return yamlScalarString(raw)
	}
	var node interface{} = root // nested form
	for _, part := range strings.Split(key, ".") {
		m, ok := node.(map[string]interface{})
		if !ok {
			return "", false
		}
		node, ok = m[part]
		if !ok {
			return "", false
		}
	}
	return yamlScalarString(node)
}

func yamlScalarString(v interface{}) (string, bool) {
	switch s := v.(type) {
	case nil:
		return "", false
	case string:
		return s, true
	default:
		return fmt.Sprintf("%v", s), true
	}
}

// GetUserYamlConfig reads a single dotted key from the user-global config.yaml
// ONLY, never project/BEADS_DIR config, returning "" if unset. It is the read
// counterpart of SetUserYamlConfig/UnsetUserYamlConfig and the generic form of
// the per-key consent helpers below. User-global keys (see IsUserGlobalKey —
// currently metrics.*) must be read through this so `bd config get` reports the
// value that actually governs runtime behavior, not the merged value a project's
// .beads/config.yaml could shadow.
func GetUserYamlConfig(key string) string {
	raw, _ := readUserGlobalYamlValue(key)
	return strings.TrimSpace(raw)
}

// MetricsDisabledByUserConfig reports whether the user-global config.yaml sets
// metrics.disabled: true. Project/BEADS_DIR config is intentionally ignored so a
// repository can never re-enable metrics for a user who opted out globally.
// Absent or unparseable values read as "not disabled" (the default).
func MetricsDisabledByUserConfig() bool {
	raw, ok := readUserGlobalYamlValue("metrics.disabled")
	if !ok {
		return false
	}
	disabled, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return disabled
}

// UserMetricsEndpoint returns the metrics endpoint configured in the user-global
// config.yaml, or "" if unset. Project/BEADS_DIR config is intentionally ignored
// so a repository can never redirect a user's metrics endpoint. Callers fall
// back to the built-in default when this is empty.
func UserMetricsEndpoint() string {
	raw, _ := readUserGlobalYamlValue("metrics.endpoint")
	return strings.TrimSpace(raw)
}

// MetricsNoticeShownByUserConfig reports whether the user-global config.yaml
// records that the first-run metrics disclosure was already shown. Like consent
// and endpoint, it is resolved from the user-global config ONLY: a repository's
// .beads/config.yaml must not be able to set metrics.notice_shown: true and
// suppress the one-time disclosure for a user who has never actually seen it.
// Absent or unparseable values read as "not shown" (the default).
func MetricsNoticeShownByUserConfig() bool {
	raw, ok := readUserGlobalYamlValue("metrics.notice_shown")
	if !ok {
		return false
	}
	shown, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return shown
}

func UnsetUserYamlConfig(key string) error {
	configPath, err := UserConfigYamlPath()
	if err != nil {
		return err
	}
	normalizedKey := normalizeYamlKey(key)

	content, err := os.ReadFile(configPath) //nolint:gosec // configPath is a validated absolute user config path
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read user config.yaml: %w", err)
	}

	newContent := commentOutYamlKey(string(content), normalizedKey)

	// Preserve the owner-private 0600 posture every other user-global writer
	// uses (SetUserYamlConfig, setYamlConfigAtPath, the metrics bootstrap);
	// rewriting at 0644 would relax this shared user config to world-readable.
	if err := os.WriteFile(configPath, []byte(newContent), 0o600); err != nil { //nolint:gosec // configPath is from UserConfigYamlPath
		return fmt.Errorf("failed to write user config.yaml: %w", err)
	}

	return nil
}

func SetUserYamlConfig(key, value string) error {
	if err := validateYamlConfigValue(key, value); err != nil {
		return err
	}
	configPath, err := UserConfigYamlPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return fmt.Errorf("failed to create user config directory: %w", err)
	}
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		if err := os.WriteFile(configPath, []byte{}, 0o600); err != nil {
			return fmt.Errorf("failed to create user config.yaml: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to stat user config.yaml: %w", err)
	}
	return setYamlConfigAtPath(configPath, key, value)
}

func setYamlConfigAtPath(configPath, key, value string) error {

	// Normalize key to canonical yaml format
	normalizedKey := normalizeYamlKey(key)

	// Read existing config
	content, err := os.ReadFile(configPath) //nolint:gosec // configPath is from findProjectConfigYaml
	if err != nil {
		return fmt.Errorf("failed to read config.yaml: %w", err)
	}

	// Update or add the key
	newContent, err := updateYamlKey(string(content), normalizedKey, value)
	if err != nil {
		return err
	}

	// Write back
	if err := os.WriteFile(configPath, []byte(newContent), 0600); err != nil { //nolint:gosec // configPath is validated
		return fmt.Errorf("failed to write config.yaml: %w", err)
	}

	return nil
}
