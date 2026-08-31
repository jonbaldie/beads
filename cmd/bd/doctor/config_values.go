package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/spf13/viper"
)

// validRoutingModes are the allowed values for routing.mode
var validRoutingModes = map[string]bool{
	"auto":        true,
	"maintainer":  true,
	"contributor": true,
	"explicit":    true,
}

// validActorRegex validates actor names (alphanumeric with dashes, underscores, dots, and @ for emails)
var validActorRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._@-]*$`)

// validCustomStatusRegex validates custom status names (alphanumeric with underscores)
var validCustomStatusRegex = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// CheckConfigValues validates configuration values in config.yaml and metadata.json
// Returns issues found, or OK if all values are valid
// CheckConfigValues validates configuration values across config.yaml, metadata.json, and the database.
// Opens its own store; prefer CheckConfigValuesWithStore when a shared store is available.
func CheckConfigValues(repoPath string) DoctorCheck {
	var issues []string

	// Check config.yaml values
	yamlIssues := checkYAMLConfigValues(repoPath)
	issues = append(issues, yamlIssues...)

	// Check metadata.json values
	metadataIssues := checkMetadataConfigValues(repoPath)
	issues = append(issues, metadataIssues...)

	// Check database config values (status.custom, etc.)
	dbIssues := checkDatabaseConfigValues(repoPath)
	issues = append(issues, dbIssues...)

	return formatConfigValuesResult(issues)
}

// CheckConfigValuesWithStore validates config values using a shared store (GH#2636).
func CheckConfigValuesWithStore(repoPath string, ss *SharedStore) DoctorCheck {
	var issues []string

	// Check config.yaml values
	yamlIssues := checkYAMLConfigValues(repoPath)
	issues = append(issues, yamlIssues...)

	// Check metadata.json values
	metadataIssues := checkMetadataConfigValues(repoPath)
	issues = append(issues, metadataIssues...)

	// Check database config values using shared store
	store := ss.Store()
	if store != nil {
		dbIssues := checkDatabaseConfigValuesWithStore(store)
		issues = append(issues, dbIssues...)
	}

	return formatConfigValuesResult(issues)
}

func formatConfigValuesResult(issues []string) DoctorCheck {
	if len(issues) == 0 {
		return DoctorCheck{
			Name:    "Config Values",
			Status:  "ok",
			Message: "All configuration values are valid",
		}
	}

	return DoctorCheck{
		Name:    "Config Values",
		Status:  "warning",
		Message: fmt.Sprintf("Found %d configuration issue(s)", len(issues)),
		Detail:  strings.Join(issues, "\n"),
		Fix:     "Edit config files to fix invalid values. Run 'bd config' to view current settings.",
	}
}

// findConfigPath locates config.yaml in standard locations.
func findConfigPath(repoPath string) string {
	configPath := filepath.Join(ResolveBeadsDirForRepo(repoPath), "config.yaml")
	if _, err := os.Stat(configPath); err == nil {
		return configPath
	}
	if configDir, err := os.UserConfigDir(); err == nil {
		userConfigPath := filepath.Join(configDir, "bd", "config.yaml")
		if _, err := os.Stat(userConfigPath); err == nil {
			return userConfigPath
		}
	}
	if homeDir, err := os.UserHomeDir(); err == nil {
		homeConfigPath := filepath.Join(homeDir, ".beads", "config.yaml")
		if _, err := os.Stat(homeConfigPath); err == nil {
			return homeConfigPath
		}
	}
	return ""
}

// validateBooleanConfigs validates boolean config values.
func validateBooleanConfigs(v *viper.Viper, keys []string) []string {
	var issues []string
	for _, key := range keys {
		if v.IsSet(key) {
			strVal := v.GetString(key)
			if strVal != "" && !isValidBoolString(strVal) {
				issues = append(issues, fmt.Sprintf("%s: %q is not a valid boolean value (expected true/false, yes/no, 1/0, on/off)", key, strVal))
			}
		}
	}
	return issues
}

// validateRoutingPaths validates routing path config values.
func validateRoutingPaths(v *viper.Viper) []string {
	var issues []string
	for _, key := range []string{"routing.default", "routing.maintainer", "routing.contributor"} {
		if v.IsSet(key) {
			path := v.GetString(key)
			if path != "" && path != "." {
				expandedPath := expandPath(path)
				if _, err := os.Stat(expandedPath); os.IsNotExist(err) {
					issues = append(issues, fmt.Sprintf("%s: path %q does not exist", key, path))
				}
			}
		}
	}
	return issues
}

// validateRepoPaths validates repos.primary and repos.additional paths.
func validateRepoPaths(v *viper.Viper) []string {
	var issues []string
	issues = append(issues, validatePrimaryRepoPath(v)...)
	issues = append(issues, validateAdditionalRepoPaths(v)...)
	return issues
}

func validatePrimaryRepoPath(v *viper.Viper) []string {
	if !v.IsSet("repos.primary") {
		return nil
	}
	primary := v.GetString("repos.primary")
	if primary == "" {
		return nil
	}
	info, err := os.Stat(expandPath(primary))
	if err == nil {
		if !info.IsDir() {
			return []string{fmt.Sprintf("repos.primary: %q is not a directory", primary)}
		}
		return nil
	}
	if os.IsNotExist(err) {
		return nil
	}
	return []string{fmt.Sprintf("repos.primary: cannot access %q: %v", primary, err)}
}

func validateAdditionalRepoPaths(v *viper.Viper) []string {
	if !v.IsSet("repos.additional") {
		return nil
	}
	var issues []string
	for _, path := range v.GetStringSlice("repos.additional") {
		if path == "" {
			continue
		}
		info, err := os.Stat(expandPath(path))
		if err == nil && !info.IsDir() {
			issues = append(issues, fmt.Sprintf("repos.additional: %q is not a directory", path))
		}
	}
	return issues
}

// checkYAMLConfigValues validates values in config.yaml
func checkYAMLConfigValues(repoPath string) []string {
	configPath := findConfigPath(repoPath)
	if configPath == "" {
		return nil
	}
	v := viper.New()
	v.SetConfigType("yaml")
	v.SetConfigFile(configPath)
	if err := v.ReadInConfig(); err != nil {
		return []string{fmt.Sprintf("config.yaml: failed to parse: %v", err)}
	}
	var issues []string
	issues = append(issues, checkYAMLIssuePrefix(v)...)
	issues = append(issues, checkYAMLRouting(v)...)
	issues = append(issues, validateRoutingPaths(v)...)
	issues = append(issues, checkYAMLActorAndDB(v)...)
	issues = append(issues, validateBooleanConfigs(v, []string{"json", "no-db", "sync.require_confirmation_on_mass_delete"})...)
	issues = append(issues, validateRepoPaths(v)...)
	return issues
}

func checkYAMLIssuePrefix(v *viper.Viper) []string {
	if !v.IsSet("issue-prefix") {
		return nil
	}
	prefix := v.GetString("issue-prefix")
	if prefix == "" {
		return nil
	}
	var issues []string
	if len(prefix) > 20 {
		issues = append(issues, fmt.Sprintf("issue-prefix: %q is too long (max 20 characters)", prefix))
	}
	if !regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`).MatchString(prefix) {
		issues = append(issues, fmt.Sprintf("issue-prefix: %q is invalid (must start with letter, contain only letters, numbers, dashes, underscores)", prefix))
	}
	return issues
}

func checkYAMLRouting(v *viper.Viper) []string {
	if !v.IsSet("routing.mode") {
		return nil
	}
	mode := v.GetString("routing.mode")
	var issues []string
	if mode != "" && !validRoutingModes[mode] {
		validModes := make([]string, 0, len(validRoutingModes))
		for m := range validRoutingModes {
			validModes = append(validModes, m)
		}
		issues = append(issues, fmt.Sprintf("routing.mode: %q is invalid (valid values: %s)", mode, strings.Join(validModes, ", ")))
	}
	if mode == "auto" {
		issues = append(issues, checkYAMLRoutingHydration(v)...)
	}
	return issues
}

func checkYAMLRoutingHydration(v *viper.Viper) []string {
	// When routing.mode=auto with routing targets, those targets should be in repos.additional
	// so routed issues are visible in bd list via multi-repo hydration (bd-fix-routing).
	contributorRepo := v.GetString("routing.contributor")
	maintainerRepo := v.GetString("routing.maintainer")
	hasRoutingTargets := (contributorRepo != "" && contributorRepo != ".") || (maintainerRepo != "" && maintainerRepo != ".")
	if !hasRoutingTargets {
		return nil
	}
	additional := v.GetStringSlice("repos.additional")
	if len(additional) == 0 {
		return []string{
			"routing.mode=auto with routing targets but repos.additional not configured. " +
				"Issues created via routing will not be visible in bd list. " +
				"Run 'bd repo add <routing-target>' to enable hydration.",
		}
	}
	return checkYAMLRoutingTargetsHydrated(contributorRepo, maintainerRepo, additional)
}

func checkYAMLRoutingTargetsHydrated(contributorRepo, maintainerRepo string, additional []string) []string {
	additionalSet := make(map[string]bool)
	for _, path := range additional {
		additionalSet[expandPath(path)] = true
	}
	var issues []string
	if contributorRepo != "" && !additionalSet[expandPath(contributorRepo)] {
		issues = append(issues, fmt.Sprintf(
			"routing.contributor=%q is not in repos.additional. "+
				"Run 'bd repo add %s' to make routed issues visible.",
			contributorRepo, contributorRepo))
	}
	if maintainerRepo != "" && maintainerRepo != "." && !additionalSet[expandPath(maintainerRepo)] {
		issues = append(issues, fmt.Sprintf(
			"routing.maintainer=%q is not in repos.additional. "+
				"Run 'bd repo add %s' to make routed issues visible.",
			maintainerRepo, maintainerRepo))
	}
	return issues
}

func checkYAMLActorAndDB(v *viper.Viper) []string {
	var issues []string
	if v.IsSet("actor") {
		actor := v.GetString("actor")
		if actor != "" && !validActorRegex.MatchString(actor) {
			issues = append(issues, fmt.Sprintf("actor: %q is invalid (must start with letter/number, contain only letters, numbers, dashes, underscores, dots, or @)", actor))
		}
	}
	if v.IsSet("db") {
		dbPath := v.GetString("db")
		if dbPath != "" && strings.ContainsAny(dbPath, "\x00") {
			issues = append(issues, fmt.Sprintf("db: %q contains invalid characters", dbPath))
		}
	}
	return issues
}

// isValidBoolString checks if a string represents a valid boolean value
func isValidBoolString(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	switch lower {
	case "true", "false", "yes", "no", "1", "0", "on", "off", "t", "f", "y", "n":
		return true
	}
	// Also check if it parses as a bool
	_, err := strconv.ParseBool(s)
	return err == nil
}

// expandPath expands ~ to home directory and resolves the path
func expandPath(path string) string {
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[1:])
		}
	}
	return path
}

// checkMetadataConfigValues validates values in metadata.json
func checkMetadataConfigValues(repoPath string) []string {
	beadsDir := ResolveBeadsDirForRepo(repoPath)
	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		return []string{fmt.Sprintf("metadata.json: failed to load: %v", err)}
	}
	if cfg == nil {
		return nil
	}
	var issues []string
	issues = append(issues, checkMetadataDatabaseName(cfg)...)
	issues = append(issues, checkMetadataDoltDatabase(cfg)...)
	if cfg.DeletionsRetentionDays < 0 {
		issues = append(issues, fmt.Sprintf("metadata.json deletions_retention_days: %d is invalid (must be >= 0)", cfg.DeletionsRetentionDays))
	}
	return issues
}

func checkMetadataDatabaseName(cfg *configfile.Config) []string {
	if cfg.Database == "" {
		return nil
	}
	var issues []string
	if strings.Contains(cfg.Database, string(os.PathSeparator)) || strings.Contains(cfg.Database, "/") {
		issues = append(issues, fmt.Sprintf("metadata.json database: %q should be a filename, not a path", cfg.Database))
	}
	if cfg.GetBackend() != configfile.BackendDolt {
		return issues
	}
	return append(issues, checkMetadataDoltDatabaseFilename(cfg.Database)...)
}

func checkMetadataDoltDatabaseFilename(database string) []string {
	var issues []string
	// Dolt is directory-backed; `database` should point to a directory (typically "dolt").
	if strings.HasSuffix(database, ".db") || strings.HasSuffix(database, ".sqlite") || strings.HasSuffix(database, ".sqlite3") {
		issues = append(issues, fmt.Sprintf("metadata.json database: %q looks like a SQLite file, but backend is dolt (expected a directory like %q)", database, "dolt"))
	}
	if database == beads.CanonicalDatabaseName {
		issues = append(issues, fmt.Sprintf("metadata.json database: %q is misleading for dolt backend (expected %q)", database, "dolt"))
	}
	return issues
}

func checkMetadataDoltDatabase(cfg *configfile.Config) []string {
	// Validate dolt_database for embedded-mode compatibility (GH#3231).
	// Hyphens and dots are allowed by server mode but rejected by the
	// embedded Dolt engine because database names are interpolated into
	// system variable identifiers (@@<db>_head_ref) where only
	// [a-zA-Z_][a-zA-Z0-9_]* is valid.
	if cfg.DoltDatabase == "" || cfg.IsDoltServerMode() {
		return nil
	}
	sanitized := strings.ReplaceAll(cfg.DoltDatabase, "-", "_")
	sanitized = strings.ReplaceAll(sanitized, ".", "_")
	if sanitized == cfg.DoltDatabase {
		return nil
	}
	return []string{fmt.Sprintf(
		"metadata.json dolt_database: %q contains characters invalid in embedded mode — "+
			"replace with %q or set dolt_mode to \"server\" (GH#3231)", cfg.DoltDatabase, sanitized)}
}

// checkDatabaseConfigValues validates configuration values stored in the database
func checkDatabaseConfigValues(repoPath string) []string {
	var issues []string

	beadsDir := ResolveBeadsDirForRepo(repoPath)
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		return issues // No .beads directory, nothing to check
	}

	// Check backend
	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		return issues
	}

	backend := configfile.BackendDolt
	if cfg != nil {
		backend = cfg.GetBackend()
	}

	if backend != configfile.BackendDolt {
		return issues // Non-Dolt backend, skip database config validation
	}

	// Check if Dolt directory exists
	doltPath := getDatabasePath(beadsDir)
	if _, err := os.Stat(doltPath); os.IsNotExist(err) {
		return issues // No database, nothing to check
	}

	// Open Dolt store in read-only mode
	ctx := context.Background()
	store, err := dolt.NewFromConfigWithCLIOptions(ctx, beadsDir, configValidationStoreOptions())
	if err != nil {
		issues = append(issues, fmt.Sprintf("database: failed to open Dolt store: %v", err))
		return issues
	}
	defer func() { _ = store.Close() }()

	return checkDatabaseConfigValuesWithStore(store)
}

func configValidationStoreOptions() *dolt.Config {
	return &dolt.Config{
		ReadOnly: true,
		ServerOptions: dolt.ServerOptions{
			DisableAutoStart: true,
		},
	}
}

func checkDatabaseConfigValuesWithStore(store *dolt.DoltStore) []string {
	var issues []string
	ctx := context.Background()

	// Check status.custom - custom status names should be lowercase alphanumeric with underscores
	statusCustom, err := store.GetConfig(ctx, "status.custom")
	if err == nil && statusCustom != "" {
		statuses := strings.Split(statusCustom, ",")
		for _, status := range statuses {
			status = strings.TrimSpace(status)
			if status == "" {
				continue
			}
			if !validCustomStatusRegex.MatchString(status) {
				issues = append(issues, fmt.Sprintf("status.custom: %q is invalid (must start with lowercase letter, contain only lowercase letters, numbers, and underscores)", status))
			}
			// Check for conflicts with built-in statuses
			switch status {
			case "open", "in_progress", "blocked", "closed":
				issues = append(issues, fmt.Sprintf("status.custom: %q conflicts with built-in status", status))
			}
		}
	}

	return issues
}
