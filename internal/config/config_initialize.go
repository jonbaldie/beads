package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/debug"
)

type configPathDiscovery struct {
	paths                  []string
	primary                string
	beadsEnvConfigPath     string
	ignoreRepoConfig       bool
	ignoredRepoConfigPaths map[string]bool
}

func discoverConfigPaths() configPathDiscovery {
	discovery := newConfigPathDiscovery()
	userPaths := currentUserConfigYamlCandidates()
	discovery.paths = appendExistingUserConfig(discovery.paths, userPaths.legacy)
	discovery.paths = appendExistingUserConfig(discovery.paths, userPaths.native)
	discovery.paths = appendExistingUserConfig(discovery.paths, userPaths.documented)

	cwd, err := os.Getwd()
	if err == nil {
		discovery = discoverCWDConfigPaths(discovery, cwd)
	}
	return appendBeadsDirConfig(discovery)
}

func newConfigPathDiscovery() configPathDiscovery {
	beadsDirEnv := strings.TrimSpace(os.Getenv("BEADS_DIR"))
	beadsEnvConfigPath := ""
	if beadsDirEnv != "" {
		beadsEnvConfigPath = filepath.Clean(filepath.Join(beadsDirEnv, "config.yaml"))
	}
	return configPathDiscovery{
		beadsEnvConfigPath:     beadsEnvConfigPath,
		ignoreRepoConfig:       os.Getenv("BEADS_TEST_IGNORE_REPO_CONFIG") != "",
		ignoredRepoConfigPaths: map[string]bool{},
	}
}

func appendExistingUserConfig(paths []string, path string) []string {
	if !userConfigPathExists(path) {
		return paths
	}
	for _, existing := range paths {
		if filepath.Clean(existing) == path {
			return paths
		}
	}
	return append(paths, path)
}

func discoverCWDConfigPaths(discovery configPathDiscovery, cwd string) configPathDiscovery {
	moduleRoot := ""
	if discovery.ignoreRepoConfig {
		moduleRoot = findConfigModuleRoot(cwd)
		addIgnoredRepoConfigPaths(&discovery, moduleRoot, cwd)
	}

	discovery = walkProjectConfigPaths(discovery, cwd, moduleRoot)
	if discovery.primary == "" && discovery.beadsEnvConfigPath == "" {
		discovery.primary = tryProjectConfigPath(&discovery, worktreeFallbackConfigPath(cwd))
	}
	return discovery
}

func findConfigModuleRoot(cwd string) string {
	for dir := cwd; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
	}
	return ""
}

func addIgnoredRepoConfigPaths(discovery *configPathDiscovery, moduleRoot, cwd string) {
	if moduleRoot != "" {
		path := filepath.Join(moduleRoot, ".beads", "config.yaml")
		discovery.ignoredRepoConfigPaths[ignoredConfigKey(path)] = true
	}
	if fallbackPath := worktreeFallbackConfigPath(cwd); fallbackPath != "" {
		discovery.ignoredRepoConfigPaths[ignoredConfigKey(fallbackPath)] = true
	}
}

func walkProjectConfigPaths(discovery configPathDiscovery, cwd, moduleRoot string) configPathDiscovery {
	for dir := cwd; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		path := filepath.Join(dir, ".beads", "config.yaml")
		if _, err := os.Stat(path); err == nil {
			if discovery.beadsEnvConfigPath != "" && filepath.Clean(path) != discovery.beadsEnvConfigPath {
				break
			}
			if tryProjectConfigPath(&discovery, path) != "" {
				break
			}
		}
		if discovery.ignoreRepoConfig && moduleRoot != "" && dir == moduleRoot {
			break
		}
	}
	return discovery
}

func tryProjectConfigPath(discovery *configPathDiscovery, path string) string {
	if path == "" {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	if discovery.ignoreRepoConfig && discovery.ignoredRepoConfigPaths[ignoredConfigKey(path)] {
		return ""
	}
	discovery.paths = append(discovery.paths, path)
	discovery.primary = path
	return path
}

func appendBeadsDirConfig(discovery configPathDiscovery) configPathDiscovery {
	beadsDir := os.Getenv("BEADS_DIR")
	if beadsDir == "" {
		return discovery
	}

	path := filepath.Join(beadsDir, "config.yaml")
	ignored := discovery.ignoreRepoConfig && discovery.ignoredRepoConfigPaths[ignoredConfigKey(path)]
	if _, err := os.Stat(path); err != nil || ignored {
		return discovery
	}
	if discovery.primary == "" || filepath.Clean(path) != filepath.Clean(discovery.primary) {
		discovery.paths = append(discovery.paths, path)
	}
	discovery.primary = path
	return discovery
}

func configureViper() {
	// Set config type to yaml (we only load config.yaml, not config.json).
	v.SetConfigType("yaml")
	v.SetEnvPrefix("BD")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	v.AutomaticEnv()
	_ = v.BindEnv("identity", "BEADS_IDENTITY")
	_ = v.BindEnv("node_id", "BEADS_NODE_ID", "BD_NODE_ID")
}

func setConfigDefaults() {
	v.SetDefault("json", false)
	v.SetDefault("events-export", false)
	v.SetDefault("events-journal", false)
	v.SetDefault("events-journal-retain-days", 7)
	v.SetDefault("events-journal-retain-rows", 100000)
	v.SetDefault("events-journal-auto-prune", true)
	v.SetDefault("audit.enabled", false)
	v.SetDefault("no-db", false)
	v.SetDefault("no-hooks", false)
	v.SetDefault("db", "")
	v.SetDefault("actor", "")
	v.SetDefault("issue-prefix", "")
	v.SetDefault("identity", "")
	v.SetDefault("node_id", "")

	v.SetDefault("dolt.auto-commit", "on")
	v.SetDefault("routing.mode", "")
	v.SetDefault("routing.default", ".")
	v.SetDefault("routing.maintainer", ".")
	v.SetDefault("routing.contributor", "~/.beads-planning")
	v.SetDefault("sync.require_confirmation_on_mass_delete", false)
	v.SetDefault("metrics.disabled", false)
	v.SetDefault("metrics.endpoint", "https://gastownhall-eventsapi.com/mp/collect")

	v.SetDefault("federation.remote", "")
	v.SetDefault("federation.sovereignty", "")
	v.SetDefault("federation.allowed-remote-patterns", []string{})
	v.SetDefault("federation.exclude_types", []string{"wisp"})
	v.SetDefault("no-push", false)

	v.SetDefault("agent.profile", string(ProfileConservative))
	v.SetDefault("create.require-description", false)
	v.SetDefault("validation.on-create", "none")
	v.SetDefault("validation.on-close", "none")
	v.SetDefault("validation.on-sync", "none")
	v.SetDefault("validation.metadata.mode", "none")
	v.SetDefault("hierarchy.max-depth", 3)
	v.SetDefault("git.author", "")
	v.SetDefault("git.no-gpg-sign", false)
	v.SetDefault("directory.labels", map[string]string{})

	v.SetDefault("backup.enabled", false)
	v.SetDefault("backup.interval", "15m")
	v.SetDefault("backup.git-push", false)
	v.SetDefault("backup.git-repo", "")
	v.SetDefault("export.auto", false)
	v.SetDefault("export.interval", "60s")
	v.SetDefault("export.path", "issues.jsonl")
	v.SetDefault("export.git-add", false)
	v.SetDefault("import.auto", true)
	v.SetDefault("import.path", "issues.jsonl")

	v.SetDefault("ai.model", "claude-haiku-4-5-20251001")
	v.SetDefault("ai.base_url", "")
	v.SetDefault("list.limit", 50)
	v.SetDefault("output.title-length", 255)
	v.SetDefault("external_projects", map[string]string{})
}

func loadConfigFiles(discovery configPathDiscovery) error {
	if len(discovery.paths) == 0 {
		debug.Logf("Debug: no config.yaml found; using defaults and environment variables\n")
		return nil
	}

	v.SetConfigFile(discovery.paths[0])
	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("error reading config file: %w", err)
	}
	debug.Logf("Debug: loaded config from %s\n", discovery.paths[0])
	if err := mergeConfigPaths(discovery.paths[1:]); err != nil {
		return err
	}

	v.SetConfigFile(discovery.primary)
	return mergeLocalConfig(discovery.primary)
}

func mergeConfigPaths(paths []string) error {
	for _, path := range paths {
		v.SetConfigFile(path)
		if err := v.MergeInConfig(); err != nil {
			return fmt.Errorf("error merging config file %s: %w", path, err)
		}
		debug.Logf("Debug: merged config from %s\n", path)
	}
	return nil
}

func mergeLocalConfig(primaryConfigPath string) error {
	localConfigPath := filepath.Join(filepath.Dir(primaryConfigPath), "config.local.yaml")
	if _, err := os.Stat(localConfigPath); err != nil {
		return nil
	}
	v.SetConfigFile(localConfigPath)
	if err := v.MergeInConfig(); err != nil {
		return fmt.Errorf("error merging local config file: %w", err)
	}
	debug.Logf("Debug: merged local config from %s\n", localConfigPath)
	v.SetConfigFile(primaryConfigPath)
	return nil
}
