package fix

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// ExternalHookManager represents a detected external hook management tool.
type ExternalHookManager struct {
	Name       string // e.g., "lefthook", "husky", "pre-commit"
	ConfigFile string // Path to the config file that was detected
}

// hookManagerConfig pairs a manager name with its possible config files.
type hookManagerConfig struct {
	name        string
	configFiles []string
}

// hookManagerConfigs defines external hook managers in priority order.
// See https://lefthook.dev/configuration/ for lefthook config options.
// Note: prek (https://prek.j178.dev) uses the same config files as pre-commit
// but is a faster Rust-based alternative. We detect both from the same config.
var hookManagerConfigs = []hookManagerConfig{
	{"lefthook", []string{
		// YAML variants
		"lefthook.yml", ".lefthook.yml", ".config/lefthook.yml",
		"lefthook.yaml", ".lefthook.yaml", ".config/lefthook.yaml",
		// TOML variants
		"lefthook.toml", ".lefthook.toml", ".config/lefthook.toml",
		// JSON variants
		"lefthook.json", ".lefthook.json", ".config/lefthook.json",
	}},
	{"husky", []string{".husky"}},
	// pre-commit and prek share the same config files; we detect which is active from git hooks
	{"pre-commit", []string{".pre-commit-config.yaml", ".pre-commit-config.yml"}},
	{"overcommit", []string{".overcommit.yml"}},
	{"yorkie", []string{".yorkie"}},
	{"hk", []string{
		"hk.pkl", ".config/hk.pkl",
		"hk.local.pkl", ".config/hk.local.pkl",
	}},
	{"simple-git-hooks", []string{
		".simple-git-hooks.cjs", ".simple-git-hooks.js",
		"simple-git-hooks.cjs", "simple-git-hooks.js",
	}},
}

// DetectExternalHookManagers checks for presence of external hook management tools.
// Returns a list of detected managers along with their config file paths.
func DetectExternalHookManagers(path string) []ExternalHookManager {
	var managers []ExternalHookManager

	for _, mgr := range hookManagerConfigs {
		for _, configFile := range mgr.configFiles {
			configPath := filepath.Join(path, configFile)
			if info, err := os.Stat(configPath); err == nil {
				// For directories like .husky, check if it exists
				// For files, check if it's a regular file
				if info.IsDir() || info.Mode().IsRegular() {
					managers = append(managers, ExternalHookManager{
						Name:       mgr.name,
						ConfigFile: configFile,
					})
					break // Only report each manager once
				}
			}
		}
	}

	return managers
}

// HookIntegrationStatus represents the status of bd integration in an external hook manager.
type HookIntegrationStatus struct {
	Manager          string   // Hook manager name
	HooksWithBd      []string // Hooks that have bd integration (bd hooks run)
	HooksWithoutBd   []string // Hooks configured but without bd integration
	HooksNotInConfig []string // Recommended hooks not in config at all
	Configured       bool     // Whether any bd integration was found
	DetectionOnly    bool     // True if we detected the manager but can't verify its config
}

// bdHookPattern matches the recommended bd hooks run pattern with word boundaries
var bdHookPattern = regexp.MustCompile(`\bbd\s+hooks\s+run\b`)

// hookManagerPattern pairs a manager name with its detection pattern.
type hookManagerPattern struct {
	name    string
	pattern *regexp.Regexp
}

// hookManagerPatterns identifies which hook manager installed a git hook (in priority order).
// Note: prek must come before pre-commit since prek hooks may also contain "pre-commit" in paths.
var hookManagerPatterns = []hookManagerPattern{
	{"hk", regexp.MustCompile(`(?i)\bhk\s+run\b`)},
	{"lefthook", regexp.MustCompile(`(?i)lefthook`)},
	{"husky", regexp.MustCompile(`(?i)(\.husky|husky\.sh)`)},
	// prek (https://prek.j178.dev) - faster Rust-based pre-commit alternative
	{"prek", regexp.MustCompile(`(?i)(prek\s+run|prek\s+hook-impl)`)},
	{"pre-commit", regexp.MustCompile(`(?i)(pre-commit\s+run|\.pre-commit-config|INSTALL_PYTHON|PRE_COMMIT)`)},
	{"simple-git-hooks", regexp.MustCompile(`(?i)simple-git-hooks`)},
}

// DetectActiveHookManager reads the git hooks to determine which manager installed them.
// This is more reliable than just checking for config files when multiple managers exist.
func DetectActiveHookManager(path string) string {
	hooksDir, ok := resolveActiveHooksDir(path)
	if !ok {
		return ""
	}
	return matchHookManagerInDir(hooksDir)
}

func resolveActiveHooksDir(path string) (string, bool) {
	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = path
	output, err := cmd.Output()
	if err != nil {
		return "", false
	}
	gitCommonDir := strings.TrimSpace(string(output))
	if !filepath.IsAbs(gitCommonDir) {
		gitCommonDir = filepath.Join(path, gitCommonDir)
	}
	return applyCustomHooksPath(path, filepath.Join(gitCommonDir, "hooks")), true
}

func applyCustomHooksPath(path, hooksDir string) string {
	hooksPathCmd := exec.Command("git", "config", "--get", "core.hooksPath")
	hooksPathCmd.Dir = path
	hooksPathOutput, err := hooksPathCmd.Output()
	if err != nil {
		return hooksDir
	}
	customPath := strings.TrimSpace(string(hooksPathOutput))
	if customPath == "" {
		return hooksDir
	}
	if !filepath.IsAbs(customPath) {
		customPath = filepath.Join(path, customPath)
	}
	return customPath
}

func matchHookManagerInDir(hooksDir string) string {
	for _, hookName := range []string{"pre-commit", "pre-push", "post-merge"} {
		content, err := os.ReadFile(filepath.Join(hooksDir, hookName)) // #nosec G304 - path is validated
		if err != nil {
			continue
		}
		if name := matchHookManagerContent(string(content)); name != "" {
			return name
		}
	}
	return ""
}

func matchHookManagerContent(content string) string {
	for _, mp := range hookManagerPatterns {
		if mp.pattern.MatchString(content) {
			return mp.name
		}
	}
	return ""
}

// recommendedBdHooks are the hooks that should have bd integration
var recommendedBdHooks = []string{"pre-commit", "post-merge", "pre-push"}

// lefthookConfigFiles lists lefthook config files (derived from hookManagerConfigs).
// Format is inferred from extension.
var lefthookConfigFiles = hookManagerConfigs[0].configFiles // lefthook is first
