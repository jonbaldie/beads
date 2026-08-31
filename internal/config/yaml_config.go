package config

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// YamlOnlyKeys are configuration keys that must be stored in config.yaml
// rather than the database. These are "startup" settings that are
// read before the database is opened.
//
// This fixes GH#536: users were confused when `bd config set no-db true`
// appeared to succeed but had no effect (because no-db is read from yaml
// at startup, not from the database).
var YamlOnlyKeys = map[string]bool{
	// Bootstrap flags (affect how bd starts)
	"no-db": true,
	"json":  true,
	// Events journal: read through viper during root pre-run, before the store
	// is open, so a DB-backed write would be silently unread — the GH#536 class
	// this map exists to prevent. Without these four entries
	// `bd config set events-journal true` reports success and changes nothing.
	"events-journal":             true,
	"events-journal-retain-days": true,
	"events-journal-retain-rows": true,
	"events-journal-auto-prune":  true,

	// Database and identity
	"db":       true,
	"actor":    true,
	"identity": true,
	// Replica identity: config.NodeID() reads this through viper (yaml/env)
	// only, so a DB-backed write would be silently unread — exactly the
	// GH#536 class this map exists to prevent.
	"node_id": true,

	// Git settings
	"git.author":      true,
	"git.no-gpg-sign": true,
	"no-push":         true,
	"no-git-ops":      true, // Disable git ops in bd prime session close protocol (GH#593)
	"agent.profile":   true, // Explicit policy profile for bd prime's close protocol (GH#3423)

	// Sync settings
	"sync.remote":     true, // Primary: any Dolt-compatible remote URL
	"sync.git-remote": true, // Deprecated: falls back from sync.remote
	"sync.require_confirmation_on_mass_delete": true,

	// Routing settings
	"routing.mode":        true,
	"routing.default":     true,
	"routing.maintainer":  true,
	"routing.contributor": true,

	// Create command settings
	"create.require-description": true,

	// Prime memory-injection caps (read at session start, possibly before
	// the database is reachable, so they must live in yaml)
	"prime.max-memories":     true,
	"prime.max-memory-chars": true,

	// Validation settings (bd-t7jq)
	// Values: "warn" | "error" | "none"
	"validation.on-create": true,
	"validation.on-close":  true,
	"validation.on-sync":   true,

	// Hierarchy settings (GH#995)
	"hierarchy.max-depth": true,

	// Backup settings (must be in yaml so GetValueSource can detect overrides)
	"backup.enabled":  true,
	"backup.interval": true,
	"backup.git-push": true,
	"backup.git-repo": true,

	// Import settings
	"import.auto": true,
	"import.path": true,

	// Dolt server settings
	"dolt.shared-server":      true, // Shared Dolt server at ~/.beads/shared-server/ (GH#2377)
	"dolt.max-conns":          true, // Connection pool size override (default 10, GH#3140)
	"dolt.pool-read-timeout":  true, // Pool per-I/O read deadline override (default 10s, bd-vz0y9)
	"dolt.pool-write-timeout": true, // Pool per-I/O write deadline override (default 10s, bd-vz0y9)
	"dolt.debug":              true, // Debug-mode dolt sql-server: --loglevel=debug + --prof cpu

	// Secrets: tokens and API keys must NOT be stored in the Dolt database
	// because that data is pushed to remotes, triggering secret-scanning
	// blocks on GitHub. Store them in local config.yaml instead.
	"github.token":               true,
	"linear.api_key":             true,
	"linear.oauth_client_id":     true,
	"linear.oauth_client_secret": true,
	"jira.api_token":             true,
	"gitlab.token":               true,
	"ado.pat":                    true,
}

// IsYamlOnlyKey returns true if the given key should be stored in config.yaml
// rather than the Dolt database.
func IsYamlOnlyKey(key string) bool {
	// Check exact match
	if YamlOnlyKeys[key] {
		return true
	}

	// Check prefix matches for nested keys
	prefixes := []string{"routing.", "sync.", "git.", "directory.", "repos.", "external_projects.", "validation.", "hierarchy.", "ai.", "backup.", "export.", "dolt.", "federation.", "metrics.", "list.", "audit.", "storage-class."}
	for _, prefix := range prefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}

	return false
}

// secretKeyPatterns are substrings that identify a key as carrying sensitive
// material. Matched anywhere in the key, so they must be long enough that a
// substring hit is never an accident.
var secretKeyPatterns = []string{
	"api_key", "api-key", "apikey", "secret", "token", "password", "passwd",
	"credential", "private_key", "privatekey", "privkey",
}

// secretKeySegments are whole segments — split on `.`, `_` and `-` — that mark
// a key as sensitive.
//
// They are matched as SEGMENTS rather than as substrings because every one of
// them is a prefix of an ordinary word: as a substring, "pat" would redact
// `issue.path` and `export.pattern`, "auth" would redact `commit.author`, and
// "key" would redact `sort.keyword`. As a segment, `github.pat` and
// `commit.author` are told apart correctly.
var secretKeySegments = map[string]bool{
	"key": true, "keys": true, "apikey": true, "pwd": true, "pat": true,
	"auth": true, "bearer": true, "cert": true, "credential": true,
	"credentials": true, "secret": true, "token": true, "password": true,
}

// IsSecretKey reports whether a config key holds sensitive material.
//
// IT IS A SECURITY CONTROL, not only a lint. Two callers depend on it: the
// `bd config set` guard that refuses to write a credential into a git-tracked
// file, and — since the settings surface went on the wire — the redaction in
// internal/httpapi that decides whether GET /v0/beads/config publishes a
// value. Redaction is the whole control there: a `bd serve` bearer is optional
// and, where configured, shared and surface-wide, so it cannot withhold one
// value from one caller — and there is no TLS either. A spelling missing from
// this predicate is a credential served in cleartext.
//
// It errs toward over-redacting for that reason: a key wrongly withheld is an
// operator asking why, and a key wrongly published cannot be recalled. The
// decision is about the KEY alone; no value is ever inspected.
func IsSecretKey(key string) bool {
	lower := strings.ToLower(key)
	for _, pattern := range secretKeyPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	for _, segment := range strings.FieldsFunc(lower, func(r rune) bool {
		return r == '.' || r == '_' || r == '-'
	}) {
		if secretKeySegments[segment] {
			return true
		}
	}
	return false
}

// isGitTracked returns true if the file at path is tracked by git
// (i.e., has been git-added). Uses `git ls-files --error-unmatch`.
func isGitTracked(path string) bool {
	cmd := exec.Command("git", "ls-files", "--error-unmatch", path)
	cmd.Dir = filepath.Dir(path)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

var secretKeyEnvVarHints = map[string]string{ //nolint:gosec // Values are environment variable names, not credentials.
	// Single var name only: this value is interpolated into an
	// `export %s="..."` shell template, where "A or B" would silently
	// assign to B alone. ANTHROPIC_API_KEY is the primary; MiniMax users
	// can export MINIMAX_API_KEY instead (same resolution chain).
	"ai.api_key":     "ANTHROPIC_API_KEY",
	"github.token":   "GITHUB_TOKEN",
	"linear.api_key": "LINEAR_API_KEY",
}

// secretKeyEnvVarHint returns a suggested environment variable name for a
// secret config key, e.g. "linear.api_key" -> "LINEAR_API_KEY".
func secretKeyEnvVarHint(key string) string {
	if envVar, ok := secretKeyEnvVarHints[key]; ok {
		return envVar
	}
	return "BD_" + strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), ".", "_"))
}

// CheckSecretKeyGitSafety checks whether writing key to the project's
// config.yaml would expose a secret in git history. Returns a descriptive
// error with remediation steps if so; nil otherwise. Non-secret keys always
// return nil.
func CheckSecretKeyGitSafety(key string) error {
	configPath, err := findProjectConfigYaml()
	if err != nil {
		return nil // can't resolve path; let the write fail with its own error
	}
	return checkSecretGitTracked(configPath, key)
}

func checkSecretGitTracked(configPath, key string) error {
	if !IsYamlOnlyKey(key) {
		return nil
	}
	if !IsSecretKey(key) {
		return nil
	}
	if !isGitTracked(configPath) {
		return nil
	}
	envVar := secretKeyEnvVarHint(key)
	return fmt.Errorf(
		"refusing to write secret key %q to git-tracked config file %s\n\n"+
			"This would expose your secret in git history. Instead:\n"+
			"  export %s=\"your-key-here\"    # add to ~/.secrets or ~/.zshrc\n\n"+
			"Or move config.yaml out of git tracking:\n"+
			"  git rm --cached %s\n"+
			"  echo \"config.yaml\" >> %s/.gitignore\n\n"+
			"To override this check (e.g., for testing):\n"+
			"  bd config set --force-git-tracked %s \"value\"",
		key, configPath,
		envVar,
		configPath,
		filepath.Dir(configPath),
		key,
	)
}

// keyAliases maps alternative key names to their canonical yaml form.
// This ensures consistency when users use different formats (dot vs hyphen).
var keyAliases = map[string]string{}

// normalizeYamlKey converts a key to its canonical yaml format.
// Some keys have aliases (e.g., sync.branch -> sync-branch) to handle
// different input formats consistently.
func normalizeYamlKey(key string) string {
	if canonical, ok := keyAliases[key]; ok {
		return canonical
	}
	return key
}
