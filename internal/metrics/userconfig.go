package metrics

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jonbaldie/beads/internal/config"
)

var commentedMetricsRe = regexp.MustCompile(`(?m)^\s*#\s*metrics\s*:`)

func EnsureUserConfigDefaults() error {
	path, err := config.UserConfigYamlPath()
	if err != nil {
		return fmt.Errorf("ensure user config: %w", err)
	}

	data, err := readUserConfig(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return writeUserConfigBootstrap(path)
		}
		return err
	}
	if commentedMetricsRe.Match(data) {
		return nil
	}
	root, err := parseUserConfig(path, data)
	if err != nil {
		return err
	}
	return applyUserConfigDefaults(root)
}

func readUserConfig(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is a validated absolute user config path
	if err != nil {
		return nil, fmt.Errorf("ensure user config: read %s: %w", path, err)
	}
	return data, nil
}

func parseUserConfig(path string, data []byte) (*yaml.Node, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("ensure user config: parse %s: %w", path, err)
	}
	return &root, nil
}

func applyUserConfigDefaults(root *yaml.Node) error {
	needDisabled := !userConfigHasLeaf(root, "metrics", "disabled")
	needEndpoint := !userConfigHasLeaf(root, "metrics", "endpoint")
	if !needDisabled && !needEndpoint {
		return nil
	}
	if needDisabled {
		if err := config.SetUserYamlConfig("metrics.disabled", "false"); err != nil {
			return fmt.Errorf("ensure user config: set metrics.disabled: %w", err)
		}
	}
	if needEndpoint {
		if err := config.SetUserYamlConfig("metrics.endpoint", DefaultEndpoint); err != nil {
			return fmt.Errorf("ensure user config: set metrics.endpoint: %w", err)
		}
	}
	return nil
}

func writeUserConfigBootstrap(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("ensure user config: mkdir %s: %w", filepath.Dir(path), err)
	}
	body := []byte("metrics:\n  disabled: false\n  endpoint: " + DefaultEndpoint + "\n")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // path is from config.UserConfigYamlPath
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return EnsureUserConfigDefaults()
		}
		return fmt.Errorf("ensure user config: create %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("ensure user config: write %s: %w", path, err)
	}
	return nil
}

func userConfigHasLeaf(root *yaml.Node, parts ...string) bool {
	if root == nil || len(root.Content) == 0 {
		return false
	}
	mapping := root.Content[0]
	if mapping.Kind != yaml.MappingNode {
		return false
	}

	if found, ok := userConfigFlatLeaf(mapping, strings.Join(parts, ".")); ok {
		return found
	}

	current := mapping
	for _, part := range parts {
		var ok bool
		current, ok = userConfigChild(current, part)
		if !ok {
			return false
		}
	}
	return current.Kind == yaml.ScalarNode && current.Value != ""
}

func userConfigFlatLeaf(mapping *yaml.Node, key string) (bool, bool) {
	n := len(mapping.Content)
	for i := 0; i+1 < n; i += 2 {
		k, v := mapping.Content[i], mapping.Content[i+1]
		if k.Kind == yaml.ScalarNode && k.Value == key {
			return v.Kind == yaml.ScalarNode && v.Value != "", true
		}
	}
	return false, false
}

func userConfigChild(mapping *yaml.Node, key string) (*yaml.Node, bool) {
	if mapping.Kind != yaml.MappingNode {
		return nil, false
	}
	n := len(mapping.Content)
	for i := 0; i+1 < n; i += 2 {
		k, v := mapping.Content[i], mapping.Content[i+1]
		if k.Kind == yaml.ScalarNode && k.Value == key {
			return v, true
		}
	}
	return nil, false
}
