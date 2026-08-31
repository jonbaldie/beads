package main

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/jonbaldie/beads/cmd/bd/doctor"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/issueops"
	"github.com/spf13/cobra"
)

// openWorkspaceConfig hands back the workspace-settings role for whichever
// route this invocation is on, each through its OWN capability accessor — the
// store's for the direct route and the provider's for the proxied one.
//
// directRequirement is the message `ensureDirectMode` reports when a workspace
// is reachable by neither route. It is per-verb because the shipped text names
// the verb.
func openWorkspaceConfig(directRequirement string) (issueops.WorkspaceConfig, error) {
	if usesProxiedServer() {
		return proxiedWorkspaceConfig()
	}
	if err := ensureDirectMode(directRequirement); err != nil {
		return nil, err
	}
	return getStore().WorkspaceConfig()
}

// noteDirectConfigWrite marks the invocation as having written, which is what
// the auto-commit epilogue in main.go keys on.
//
// It is DIRECT-ROUTE ONLY: a proxied write already committed inside the role's
// own unit of work, so flagging it here would ask the epilogue to commit a
// second time on a route that has nothing outstanding.
func noteDirectConfigWrite() {
	if !usesProxiedServer() {
		commandDidWrite.Store(true)
	}
}

var configGetCmd = &cobra.Command{
	Use:           "get <key>",
	Short:         "Get a configuration value",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("config-get")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		key := args[0]

		if key == "backup.enabled" {
			// backup.enabled has an auto-detected effective value that
			// differs from the stored value: when unset it auto-enables
			// in embedded mode with a git remote, and is forced OFF in
			// sql-server mode (see isBackupAutoEnabled). Reporting the raw
			// stored "false"/"not set" here misled operators during the
			// 2026-07 shared-dolt incident, so show the EFFECTIVE value
			// and its source.
			return runConfigGetBackupEnabled()
		}

		if config.IsYamlOnlyKey(key) {
			// User-global keys (e.g. metrics.*) must be read from the user-global
			// config.yaml only — the same source the runtime uses for metrics
			// consent and endpoint. Reading the merged value here would let a
			// project's .beads/config.yaml shadow the effective value and report the
			// opposite of what `bd metrics` actually honors.
			if config.IsUserGlobalKey(key) {
				value := config.GetUserYamlConfig(key)
				location := config.UserConfigYamlDisplayPath()
				if isJSONOutput() {
					return outputJSON(map[string]interface{}{
						"key":      key,
						"value":    value,
						"location": location,
					})
				}
				if value == "" {
					fmt.Printf("%s (not set in %s)\n", key, location)
				} else {
					fmt.Printf("%s\n", value)
				}
				return nil
			}

			value := config.GetYamlConfig(key)

			if isJSONOutput() {
				return outputJSON(map[string]interface{}{
					"key":      key,
					"value":    value,
					"location": "config.yaml",
				})
			}
			if value == "" {
				fmt.Printf("%s (not set in config.yaml)\n", key)
			} else {
				fmt.Printf("%s\n", value)
			}
			return nil
		}

		if key == "beads.role" {
			cmd := exec.Command("git", "config", "--get", "beads.role")
			output, err := cmd.Output()
			value := strings.TrimSpace(string(output))
			if err != nil {
				value = ""
			}
			if isJSONOutput() {
				return outputJSON(map[string]interface{}{
					"key":      key,
					"value":    value,
					"location": "git config",
				})
			}
			if value == "" {
				fmt.Printf("%s (not set in git config)\n", key)
			} else {
				fmt.Printf("%s\n", value)
			}
			return nil
		}

		settings, err := openWorkspaceConfig("config get requires direct database access")
		if err != nil {
			return HandleError("%v", err)
		}
		result, err := settings.GetSetting(getRootContext(), issueops.GetSettingRequest{Key: key})
		if err != nil {
			return HandleError("getting config: %v", err)
		}

		if isJSONOutput() {
			return outputJSON(map[string]string{
				"key":   result.Key,
				"value": result.Value,
			})
		}
		// "(not set)" also prints for a key stored as the empty string: the
		// role answers "" for both, and issueops.SettingResult.Value says why.
		if result.Value == "" {
			fmt.Printf("%s (not set)\n", result.Key)
		} else {
			fmt.Printf("%s\n", result.Value)
		}
		return nil
	},
}

// runConfigGetBackupEnabled reports the EFFECTIVE value of
// backup.enabled together with its source, rather than the raw stored
// value. The stored value is misleading because isBackupAutoEnabled()
// derives the runtime value: unset → auto-enabled in embedded mode
// when a git remote exists, and forced OFF in sql-server mode.
func runConfigGetBackupEnabled() error {
	const key = "backup.enabled"
	source := config.GetValueSource(key)
	effective := isBackupAutoEnabled()

	var sourceDesc string
	switch source {
	case config.SourceEnvVar:
		sourceDesc = "env var"
	case config.SourceConfigFile:
		sourceDesc = "config.yaml"
	default: // SourceDefault — value came from auto-detection
		switch {
		case usesSQLServer():
			sourceDesc = "default (auto: off in sql-server mode)"
		case effective:
			sourceDesc = "default (auto: on — git remote detected)"
		default:
			sourceDesc = "default (auto: off — no git remote)"
		}
	}

	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"key":       key,
			"value":     effective,
			"effective": effective,
			"source":    string(source),
		})
	}
	fmt.Printf("%t (%s)\n", effective, sourceDesc)
	return nil
}

var configListCmd = &cobra.Command{
	Use:           "list",
	Short:         "List all configuration",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("config-list")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		settings, err := openWorkspaceConfig("config list requires direct database access")
		if err != nil {
			return HandleError("%v", err)
		}
		result, err := settings.ListSettings(getRootContext(), issueops.ListSettingsRequest{})
		if err != nil {
			return HandleError("listing config: %v", err)
		}
		stored := result.Settings

		if isJSONOutput() {
			return outputJSON(stored)
		}

		if len(stored) == 0 {
			fmt.Println("No configuration set")
			return nil
		}

		keys := make([]string, 0, len(stored))
		for k := range stored {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		fmt.Println("\nConfiguration:")
		for _, k := range keys {
			fmt.Printf("  %s = %s\n", k, stored[k])
		}

		// The OTHER sources, reported beside the stored ones rather than merged
		// into them: the role answers for the database, and config.yaml and the
		// environment are files and variables of THIS process.
		showConfigYAMLOverrides(stored)
		return nil
	},
}

// showConfigYAMLOverrides warns when config.yaml or env vars override database settings.
// This addresses the confusion when `bd config list` shows one value but the effective
// value used by commands is different due to higher-priority config sources.
func showConfigYAMLOverrides(dbConfig map[string]string) {
	// Discover yaml-only keys dynamically via AllKeys() instead of a hardcoded list.
	// This stays in sync as new yaml-only keys are added to the config system.
	allKeys := config.AllKeys()
	sort.Strings(allKeys)
	envWarnings := collectConfigDBEnvWarnings(dbConfig)
	yamlOverrides := collectConfigYAMLOverrides(dbConfig, allKeys)
	envWarnings = append(envWarnings, collectConfigYAMLEnvWarnings(dbConfig, allKeys)...)
	printConfigOverrides(yamlOverrides, envWarnings)
}

func collectConfigDBEnvWarnings(dbConfig map[string]string) []string {
	var warnings []string
	for key, dbValue := range dbConfig {
		envName := config.EnvVarName(key)
		if envName == "" {
			continue
		}
		envValue := os.Getenv(envName)
		if envValue != dbValue {
			warnings = append(warnings, fmt.Sprintf("  %s: DB has %q, but env %s=%q takes precedence", key, dbValue, envName, envValue))
		}
	}
	return warnings
}

func collectConfigYAMLOverrides(dbConfig map[string]string, allKeys []string) []string {
	var overrides []string
	for _, key := range allKeys {
		// Skip keys already shown in the DB config section
		if _, inDB := dbConfig[key]; inDB {
			continue
		}
		// Only show yaml-only keys that are explicitly set in config.yaml
		if !config.IsYamlOnlyKey(key) {
			continue
		}
		if config.GetValueSource(key) != config.SourceConfigFile {
			continue
		}
		val := config.GetString(key)
		if val != "" {
			overrides = append(overrides, fmt.Sprintf("  %s = %s", key, val))
		}
	}
	return overrides
}

func collectConfigYAMLEnvWarnings(dbConfig map[string]string, allKeys []string) []string {
	var warnings []string
	for _, key := range allKeys {
		if _, inDB := dbConfig[key]; inDB {
			continue // already checked above
		}
		if envName := config.EnvVarName(key); envName != "" {
			src := config.GetValueSource(key)
			if src == config.SourceEnvVar {
				warnings = append(warnings, fmt.Sprintf("  %s: env %s=%q overrides config", key, envName, os.Getenv(envName)))
			}
		}
	}
	return warnings
}

func printConfigOverrides(yamlOverrides, envWarnings []string) {
	if len(yamlOverrides) > 0 {
		fmt.Println("\nAlso set in config.yaml (not shown above):")
		for _, line := range yamlOverrides {
			fmt.Println(line)
		}
	}

	if len(envWarnings) > 0 {
		sort.Strings(envWarnings)
		fmt.Println("\n⚠ Environment variable overrides detected:")
		for _, w := range envWarnings {
			fmt.Println(w)
		}
	}

	fmt.Println("\nTip: Run 'bd config show' for all effective config with provenance.")
}

var configUnsetCmd = &cobra.Command{
	Use:           "unset <key>",
	Short:         "Delete a configuration value",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("config-unset")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		key := args[0]

		if config.IsYamlOnlyKey(key) {
			location := "config.yaml"
			var unsetErr error
			if config.IsUserGlobalKey(key) {
				unsetErr = config.UnsetUserYamlConfig(key)
				location = config.UserConfigYamlDisplayPath()
			} else {
				unsetErr = config.UnsetYamlConfig(key)
			}
			if unsetErr != nil {
				return HandleError("unsetting config: %v", unsetErr)
			}

			if isJSONOutput() {
				if err := outputJSON(map[string]interface{}{
					"key":      key,
					"location": location,
				}); err != nil {
					return err
				}
			} else {
				fmt.Printf("Unset %s (in %s)\n", key, location)
			}
			printConfigSideEffects(checkConfigUnsetSideEffects(key))
			return nil
		}

		if key == "beads.role" {
			gitCmd := exec.Command("git", "config", "--unset", "beads.role")
			if err := gitCmd.Run(); err != nil {
				return HandleError("unsetting beads.role in git config: %v", err)
			}
			if isJSONOutput() {
				if err := outputJSON(map[string]interface{}{
					"key":      key,
					"location": "git config",
				}); err != nil {
					return err
				}
			} else {
				fmt.Printf("Unset %s (in git config)\n", key)
			}
			return nil
		}

		settings, err := openWorkspaceConfig("config unset requires direct database access")
		if err != nil {
			return HandleError("%v", err)
		}
		result, err := settings.UnsetSetting(getRootContext(), issueops.UnsetSettingRequest{Key: key})
		if err != nil {
			return HandleError("deleting config: %v", err)
		}
		noteDirectConfigWrite()

		if isJSONOutput() {
			if err := outputJSON(map[string]string{
				"key": result.Key,
			}); err != nil {
				return err
			}
		} else {
			fmt.Printf("Unset %s\n", result.Key)
		}
		printConfigSideEffects(checkConfigUnsetSideEffects(result.Key))
		return nil
	},
}

var configValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate sync-related configuration",
	Long: `Validate sync-related configuration settings.

Checks:
  - federation.sovereignty is valid (T1, T2, T3, T4, or empty)
  - federation.remote is set for Dolt sync
  - Remote URL format is valid (dolthub://, gs://, s3://, az://, file://)
  - routing.mode is valid (auto, maintainer, contributor, explicit)

	Examples:
	  bd config validate
	  bd config validate --json`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("config-validate")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		repoPath, err := resolvedConfigRepoRoot()
		if err != nil {
			return HandleErrorWithHintRespectJSON(activeWorkspaceNotFoundError(), diagHint())
		}

		doctorCheck := doctor.CheckConfigValues(repoPath)

		syncIssues := validateSyncConfig(repoPath)

		allIssues := []string{}
		if doctorCheck.Detail != "" {
			allIssues = append(allIssues, strings.Split(doctorCheck.Detail, "\n")...)
		}
		allIssues = append(allIssues, syncIssues...)

		if isJSONOutput() {
			return outputJSON(map[string]interface{}{
				"valid":  len(allIssues) == 0,
				"issues": allIssues,
			})
		}

		if len(allIssues) == 0 {
			fmt.Println("✓ All sync-related configuration is valid")
			return nil
		}

		fmt.Println("Configuration validation found issues:")
		for _, issue := range allIssues {
			if issue != "" {
				fmt.Printf("  • %s\n", issue)
			}
		}
		fmt.Println("\nRun 'bd config set <key> <value>' to fix configuration issues.")
		return SilentExit()
	},
}
