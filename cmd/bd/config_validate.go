package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/cmd/bd/doctor"
	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/git"
	"github.com/jonbaldie/beads/internal/remotecache"
	"github.com/spf13/viper"
)

// validateSyncConfig performs additional sync-related config validation
// beyond what doctor.CheckConfigValues covers.
func validateSyncConfig(repoPath string) []string {
	// Load config.yaml from the resolved workspace so shared worktrees validate
	// the same config file they actually run with.
	configPath := filepath.Join(doctor.ResolveBeadsDirForRepo(repoPath), "config.yaml")
	v := viper.New()
	v.SetConfigType("yaml")
	v.SetConfigFile(configPath)

	// Try to read config, but don't error if it doesn't exist
	if err := v.ReadInConfig(); err != nil {
		// Config file doesn't exist or is unreadable - nothing to validate
		return nil
	}
	return validateSyncConfigValues(v)
}

// Keep the validation order stable: sovereignty first, then the required
// remote, its URL shape, and finally any configured allow-list patterns.
func validateSyncConfigValues(v *viper.Viper) []string {
	issues := validateFederationSovereignty(v.GetString("federation.sovereignty"))
	return append(issues, validateFederationRemote(v, v.GetString("federation.remote"))...)
}

func validateFederationSovereignty(value string) []string {
	if value == "" || config.IsValidSovereignty(value) {
		return nil
	}
	return []string{fmt.Sprintf("federation.sovereignty: %q is invalid (valid values: %s, or empty for no restriction)", value, strings.Join(config.ValidSovereigntyTiers(), ", "))}
}

func validateFederationRemote(v *viper.Viper, remote string) []string {
	if remote == "" {
		return []string{"federation.remote: required for Dolt sync"}
	}

	var issues []string
	if err := remotecache.ValidateRemoteURL(remote); err != nil {
		issues = append(issues, fmt.Sprintf("federation.remote: %s", err))
	}
	patterns := v.GetStringSlice("federation.allowed-remote-patterns")
	if len(patterns) > 0 {
		if err := remotecache.ValidateRemoteURLWithPatterns(remote, patterns); err != nil {
			issues = append(issues, fmt.Sprintf("federation.remote: %s", err))
		}
	}
	return issues
}

// isValidRemoteURL validates remote URL formats for sync configuration.
// Uses strict security validation that checks structural correctness,
// rejects control characters, and validates per-scheme requirements.
func isValidRemoteURL(rawURL string) bool {
	return remotecache.ValidateRemoteURL(rawURL) == nil
}

// findBeadsRepoRoot walks up from the given path to find the repo root (containing .beads)
func findBeadsRepoRoot(startPath string) string {
	path := startPath
	for {
		beadsDir := filepath.Join(path, ".beads")
		if info, err := os.Stat(beadsDir); err == nil && info.IsDir() {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}

	if isGitRepo() && git.IsWorktree() {
		if fallbackDir := beads.GetWorktreeFallbackBeadsDir(); fallbackDir != "" {
			return filepath.Dir(fallbackDir)
		}
	}

	return ""
}

// resolvedConfigRepoRoot returns the repository root for the active beads
// workspace. It follows FindBeadsDir semantics, including BEADS_DIR and
// worktree/shared fallback resolution.
func resolvedConfigRepoRoot() (string, error) {
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return "", fmt.Errorf("%s", activeWorkspaceNotFoundError())
	}
	return filepath.Dir(beadsDir), nil
}
