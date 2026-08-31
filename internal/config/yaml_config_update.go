package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// GetYamlConfig gets a configuration value from config.yaml.
// Returns empty string if key is not found or is commented out.
// Keys are normalized to their canonical yaml format (e.g., sync.branch -> sync-branch).
func GetYamlConfig(key string) string {
	if v == nil {
		return ""
	}
	normalizedKey := normalizeYamlKey(key)
	return v.GetString(normalizedKey)
}

// UnsetYamlConfig removes a configuration value from the project's config.yaml file.
// The key line is commented out (prefixed with "# ") to preserve it as documentation.
func UnsetYamlConfig(key string) error {
	configPath, err := findProjectConfigYaml()
	if err != nil {
		return err
	}

	normalizedKey := normalizeYamlKey(key)

	content, err := os.ReadFile(configPath) //nolint:gosec // configPath is from findProjectConfigYaml
	if err != nil {
		return fmt.Errorf("failed to read config.yaml: %w", err)
	}

	newContent := commentOutYamlKey(string(content), normalizedKey)

	if err := os.WriteFile(configPath, []byte(newContent), 0600); err != nil { //nolint:gosec // configPath is validated
		return fmt.Errorf("failed to write config.yaml: %w", err)
	}

	return nil
}

// findProjectConfigYaml finds the active config.yaml path for YAML-only config writes.
//
// Resolution order:
//  1. BEADS_DIR/config.yaml (when BEADS_DIR is set)
//  2. Walk up from CWD to find .beads/config.yaml
//
// This keeps YAML-only config behavior aligned with runtime resolution when
// BEADS_DIR points to an external runtime directory.
func findProjectConfigYaml() (string, error) {
	return findProjectConfigYamlWithFinder(findProjectBeadsDir)
}

func findProjectConfigYamlWithFinder(findBeadsDir func() string) (string, error) {
	// Respect BEADS_DIR first when set.
	if beadsDir := os.Getenv("BEADS_DIR"); beadsDir != "" {
		configPath := filepath.Join(beadsDir, "config.yaml")
		if _, err := os.Stat(configPath); err == nil {
			return configPath, nil
		}
		return "", fmt.Errorf("no config.yaml found in BEADS_DIR (%s) (run 'bd init' first)", beadsDir)
	}

	if configPath := projectConfigPathFromLoadedState(); configPath != "" {
		return configPath, nil
	}

	if findBeadsDir != nil {
		if beadsDir := findBeadsDir(); beadsDir != "" {
			configPath := filepath.Join(beadsDir, "config.yaml")
			if _, err := os.Stat(configPath); err == nil {
				return configPath, nil
			}
		}
	}

	return "", fmt.Errorf("no .beads/config.yaml found (run 'bd init' first)")
}

func projectConfigPathFromLoadedState() string {
	configPath := ConfigFileUsed()
	if configPath == "" {
		return ""
	}
	if filepath.Base(configPath) != "config.yaml" {
		return ""
	}
	if filepath.Base(filepath.Dir(configPath)) != ".beads" {
		return ""
	}
	if _, err := os.Stat(configPath); err != nil {
		return ""
	}
	return configPath
}

func findProjectBeadsDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	for dir := cwd; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		beadsDir := filepath.Join(dir, ".beads")
		if info, err := os.Stat(beadsDir); err == nil && info.IsDir() {
			return beadsDir
		}
	}

	configPath := worktreeFallbackConfigPath(cwd)
	if configPath == "" {
		return ""
	}

	return filepath.Dir(configPath)
}

// updateYamlKey updates a key in yaml content, handling commented-out keys.
// If the key exists (commented or not), it updates it in place.
// If the key doesn't exist, it appends it at the end.
//
//nolint:unparam // error return kept for future validation
func updateYamlKey(content, key, value string) (string, error) {
	if strings.Contains(key, ".") {
		if updated, ok, err := updateNestedYamlKey(content, key, value); err != nil {
			return "", err
		} else if ok {
			return updated, nil
		}
	}
	return updateFlatYamlKey(content, key, value), nil
}

func updateFlatYamlKey(content, key, value string) string {
	formattedValue := formatYamlValue(value)
	newLine := fmt.Sprintf("%s: %s", key, formattedValue)

	// Build regex to match the key (commented or not)
	// Matches: "key: value" or "# key: value" with optional leading whitespace
	keyPattern := regexp.MustCompile(`^(\s*)(#\s*)?` + regexp.QuoteMeta(key) + `\s*:`)

	found := false
	var result []string

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if keyPattern.MatchString(line) {
			// Found the key - replace with new value (uncommented)
			// Preserve leading whitespace
			matches := keyPattern.FindStringSubmatch(line)
			indent := ""
			if len(matches) > 1 {
				indent = matches[1]
			}
			result = append(result, indent+newLine)
			found = true
		} else {
			result = append(result, line)
		}
	}

	if !found {
		// Key not found - append at end
		// Add blank line before if content doesn't end with one
		if len(result) > 0 && result[len(result)-1] != "" {
			result = append(result, "")
		}
		result = append(result, newLine)
	}

	return strings.Join(result, "\n")
}

func updateNestedYamlKey(content, key, value string) (string, bool, error) {
	parts := strings.Split(key, ".")
	if len(parts) < 2 {
		return "", false, nil
	}

	var root yaml.Node
	if err := yaml.Unmarshal([]byte(content), &root); err != nil {
		return "", false, err
	}
	if len(root.Content) == 0 {
		return "", false, nil
	}
	mapping := root.Content[0]
	if mapping.Kind != yaml.MappingNode {
		return "", false, nil
	}

	if findMappingChild(mapping, key) != -1 {
		return "", false, nil
	}

	leaf, ok := findOrCreateNestedScalar(mapping, parts)
	if !ok {
		return "", false, nil
	}

	leaf.Kind = yaml.ScalarNode
	leaf.Tag = ""
	leaf.Style = scalarStyleFor(value)
	leaf.Value = value

	out, err := yaml.Marshal(&root)
	if err != nil {
		return "", false, err
	}
	return string(out), true, nil
}

func findOrCreateNestedScalar(mapping *yaml.Node, parts []string) (*yaml.Node, bool) {
	current := mapping
	for i, part := range parts {
		if current.Kind != yaml.MappingNode {
			return nil, false
		}
		idx := findMappingChild(current, part)
		isLeaf := i == len(parts)-1
		if idx == -1 {
			keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part}
			var valNode *yaml.Node
			if isLeaf {
				valNode = &yaml.Node{Kind: yaml.ScalarNode}
			} else {
				valNode = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			}
			current.Content = append(current.Content, keyNode, valNode)
			if isLeaf {
				return valNode, true
			}
			current = valNode
			continue
		}
		child := current.Content[idx+1]
		if isLeaf {
			return child, true
		}
		if child.Kind != yaml.MappingNode {
			return nil, false
		}
		current = child
	}
	return nil, false
}

func findMappingChild(mapping *yaml.Node, name string) int {
	n := len(mapping.Content)
	for i := 0; i < n; i += 2 {
		k := mapping.Content[i]
		if k.Kind == yaml.ScalarNode && k.Value == name {
			return i
		}
	}
	return -1
}

func scalarStyleFor(value string) yaml.Style {
	if value == "" {
		return yaml.DoubleQuotedStyle
	}
	if _, err := strconv.ParseBool(value); err == nil {
		return 0
	}
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return 0
	}
	switch value {
	case "null", "Null", "NULL", "~", "yes", "no", "on", "off":
		return yaml.DoubleQuotedStyle
	}
	if strings.ContainsAny(value, ":#\n\"'") || strings.HasPrefix(value, " ") || strings.HasSuffix(value, " ") {
		return yaml.DoubleQuotedStyle
	}
	return 0
}

func commentOutYamlKey(content, key string) string {
	keyPattern := regexp.MustCompile(`^(\s*)` + regexp.QuoteMeta(key) + `\s*:`)

	var result []string
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if keyPattern.MatchString(line) {
			matches := keyPattern.FindStringSubmatch(line)
			indent := ""
			if len(matches) > 1 {
				indent = matches[1]
			}
			// Comment out the line, preserving indentation
			result = append(result, indent+"# "+strings.TrimLeft(line, " \t"))
		} else {
			result = append(result, line)
		}
	}

	return strings.Join(result, "\n")
}

// formatYamlValue formats a value appropriately for YAML.
func formatYamlValue(value string) string {
	// Boolean values
	lower := strings.ToLower(value)
	if lower == "true" || lower == "false" {
		return lower
	}

	// Numeric values - return as-is
	if isNumeric(value) {
		return value
	}

	// Duration values (like "30s", "5m") - return as-is
	if isDuration(value) {
		return value
	}

	// For all other string-like values, quote to preserve YAML string semantics
	return fmt.Sprintf("%q", value)
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if c == '-' && i == 0 {
			continue
		}
		if c == '.' {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isDuration(s string) bool {
	if len(s) < 2 {
		return false
	}
	suffix := s[len(s)-1]
	if suffix != 's' && suffix != 'm' && suffix != 'h' {
		return false
	}
	return isNumeric(s[:len(s)-1])
}

// validateYamlConfigValue validates a configuration value before setting.
// Returns an error if the value is invalid for the given key.
func validateYamlConfigValue(key, value string) error {
	switch key {
	case "hierarchy.max-depth":
		return validateMaxDepth(value)
	case "dolt.shared-server", "dolt.debug":
		return validateBooleanValue(key, value)
	case "dolt.mode":
		return validateDoltMode(value)
	case "prime.max-memories", "prime.max-memory-chars":
		return validateNonNegativeValue(key, value)
	}
	return nil
}

func validateMaxDepth(value string) error {
	depth, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("hierarchy.max-depth must be a positive integer, got %q", value)
	}
	if depth < 1 {
		return fmt.Errorf("hierarchy.max-depth must be at least 1, got %d", depth)
	}
	return nil
}

func validateBooleanValue(key, value string) error {
	if strings.EqualFold(value, "true") || strings.EqualFold(value, "false") {
		return nil
	}
	return fmt.Errorf("%s must be \"true\" or \"false\", got %q", key, value)
}

func validateDoltMode(value string) error {
	lower := strings.ToLower(value)
	if lower == "server" || lower == "embedded" {
		return nil
	}
	return fmt.Errorf("dolt.mode must be \"server\" or \"embedded\", got %q", value)
}

func validateNonNegativeValue(key, value string) error {
	n, err := strconv.Atoi(value)
	if err == nil && n >= 0 {
		return nil
	}
	return fmt.Errorf("%s must be a non-negative integer (0 = unlimited), got %q", key, value)
}
