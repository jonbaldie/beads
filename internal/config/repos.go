package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ReposConfig represents the repos section of config.yaml
type ReposConfig struct {
	Primary    string   `yaml:"primary,omitempty"`
	Additional []string `yaml:"additional,omitempty,flow"`
}

// FindConfigYAMLPath finds the config.yaml file in .beads directory
// Walks up from CWD to find .beads/config.yaml
func FindConfigYAMLPath() (string, error) {
	configPath, err := findProjectConfigYaml()
	if err != nil {
		return "", fmt.Errorf("no .beads/config.yaml found in current directory or parents")
	}
	return configPath, nil
}

// GetReposFromYAML reads the repos configuration from config.yaml
// Returns an empty ReposConfig if repos section doesn't exist
func GetReposFromYAML(configPath string) (*ReposConfig, error) {
	data, err := readReposConfig(configPath)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return &ReposConfig{}, nil
	}
	return parseReposConfig(data)
}

func readReposConfig(configPath string) ([]byte, error) {
	data, err := os.ReadFile(configPath) // #nosec G304 - config file path from caller
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read config.yaml: %w", err)
	}
	return data, nil
}

func parseReposConfig(data []byte) (*ReposConfig, error) {
	var cfg map[string]interface{}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config.yaml: %w", err)
	}
	reposRaw := cfg["repos"]
	if reposRaw == nil {
		return &ReposConfig{}, nil
	}
	reposMap, ok := reposRaw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("repos section is not a map")
	}
	return reposConfigFromMap(reposMap), nil
}

func reposConfigFromMap(reposMap map[string]interface{}) *ReposConfig {
	repos := &ReposConfig{}
	if primary, ok := reposMap["primary"].(string); ok {
		repos.Primary = primary
	}
	if additional, ok := reposMap["additional"].([]interface{}); ok {
		for _, item := range additional {
			if str, ok := item.(string); ok {
				repos.Additional = append(repos.Additional, str)
			}
		}
	}
	return repos
}

// SetReposInYAML writes the repos configuration to config.yaml
// It preserves other config sections and comments where possible
func SetReposInYAML(configPath string, repos *ReposConfig) error {
	data, err := readReposConfig(configPath)
	if err != nil {
		return err
	}
	root, err := parseReposYAMLDocument(data)
	if err != nil {
		return err
	}
	updateReposDocument(root, repos)
	encoded, err := encodeReposDocument(root)
	if err != nil {
		return err
	}
	if err := os.WriteFile(configPath, encoded, 0600); err != nil {
		return fmt.Errorf("failed to write config.yaml: %w", err)
	}
	reloadReposConfig()
	return nil
}

func parseReposYAMLDocument(data []byte) (*yaml.Node, error) {
	root := &yaml.Node{}
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, root); err != nil {
			return nil, fmt.Errorf("failed to parse config.yaml: %w", err)
		}
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		root.Kind = yaml.DocumentNode
		root.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}
	if root.Content[0].Kind != yaml.MappingNode {
		root.Content[0] = &yaml.Node{Kind: yaml.MappingNode}
	}
	return root, nil
}

func updateReposDocument(root *yaml.Node, repos *ReposConfig) {
	mapping := root.Content[0]
	index := reposNodeIndex(mapping)
	reposNode := buildReposNode(repos)
	switch {
	case index >= 0 && reposNode == nil:
		mapping.Content = append(mapping.Content[:index], mapping.Content[index+2:]...)
	case index >= 0:
		mapping.Content[index+1] = reposNode
	case reposNode != nil:
		mapping.Content = append(mapping.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "repos"}, reposNode)
	}
}

func reposNodeIndex(mapping *yaml.Node) int {
	count := len(mapping.Content)
	for i := 0; i < count; i += 2 {
		if mapping.Content[i].Value == "repos" {
			return i
		}
	}
	return -1
}

func encodeReposDocument(root *yaml.Node) ([]byte, error) {
	var buf strings.Builder
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(root); err != nil {
		return nil, fmt.Errorf("failed to encode config.yaml: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("failed to close encoder: %w", err)
	}
	return []byte(buf.String()), nil
}

func reloadReposConfig() {
	if v != nil {
		_ = v.ReadInConfig()
	}
}

// buildReposNode creates a yaml.Node for the repos configuration
// Returns nil if repos is empty (no primary and no additional)
func buildReposNode(repos *ReposConfig) *yaml.Node {
	if repos == nil || (repos.Primary == "" && len(repos.Additional) == 0) {
		return nil
	}

	node := &yaml.Node{Kind: yaml.MappingNode}

	if repos.Primary != "" {
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "primary"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: repos.Primary, Style: yaml.DoubleQuotedStyle},
		)
	}

	if len(repos.Additional) > 0 {
		additionalNode := &yaml.Node{Kind: yaml.SequenceNode}
		for _, path := range repos.Additional {
			additionalNode.Content = append(additionalNode.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: path, Style: yaml.DoubleQuotedStyle},
			)
		}
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "additional"},
			additionalNode,
		)
	}

	return node
}

// AddRepo adds a repository to the repos.additional list in config.yaml
// If primary is not set, it defaults to "."
func AddRepo(configPath, repoPath string) error {
	repos, err := GetReposFromYAML(configPath)
	if err != nil {
		return fmt.Errorf("failed to get repos config: %w", err)
	}

	// Set primary to "." if not already set (standard multi-repo convention)
	if repos.Primary == "" {
		repos.Primary = "."
	}

	// Check if repo already exists
	for _, existing := range repos.Additional {
		if existing == repoPath {
			return fmt.Errorf("repository already configured: %s", repoPath)
		}
	}

	// Add the new repo
	repos.Additional = append(repos.Additional, repoPath)

	return SetReposInYAML(configPath, repos)
}

// RemoveRepo removes a repository from the repos.additional list in config.yaml
func RemoveRepo(configPath, repoPath string) error {
	repos, err := GetReposFromYAML(configPath)
	if err != nil {
		return fmt.Errorf("failed to get repos config: %w", err)
	}

	// Find and remove the repo
	found := false
	newAdditional := make([]string, 0, len(repos.Additional))
	for _, existing := range repos.Additional {
		if existing == repoPath {
			found = true
			continue
		}
		newAdditional = append(newAdditional, existing)
	}

	if !found {
		return fmt.Errorf("repository not found: %s", repoPath)
	}

	repos.Additional = newAdditional

	// If no repos left, clear primary too
	if len(repos.Additional) == 0 {
		repos.Primary = ""
	}

	return SetReposInYAML(configPath, repos)
}

// ListRepos returns the current repos configuration from YAML
func ListRepos(configPath string) (*ReposConfig, error) {
	return GetReposFromYAML(configPath)
}
