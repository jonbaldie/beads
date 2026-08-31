package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/issueops"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:     "config",
	GroupID: "setup",
	Short:   "Manage configuration settings",
	Long: `Manage configuration settings for external integrations and preferences.

Configuration is stored per-project in the beads database and is version-control-friendly.

Common namespaces:
  - export.*          Auto-export settings (stored in config.yaml)
  - import.*          JSONL import settings (stored in config.yaml)
  - jira.*            Jira integration settings
  - linear.*          Linear integration settings
  - github.*          GitHub integration settings
  - gitlab.*          GitLab integration settings
  - ado.*             Azure DevOps integration settings
  - notion.*          Notion integration settings
  - custom.*          Custom integration settings
  - status.*          Issue status configuration
  - claim.*           Claim arbitration settings (pool-aware claiming)
  - doctor.suppress.* Suppress specific bd doctor warnings (GH#1095)

Auto-Export (config.yaml):
  Optional JSONL export to .beads/issues.jsonl after write commands (throttled).
  Useful for viewers (bv), interchange, and issue-level migration; not a backup.
  It is not cross-machine sync; use bd dolt push/pull with a Dolt remote.
  Disabled by default. Enable only for integrations that need fresh JSONL.
  Auto-staging is separate and disabled by default.

  Keys:
    export.auto       Enable/disable auto-export (default: false)
    export.path       Output filename relative to .beads/ (default: issues.jsonl)
    export.interval   Minimum time between exports (default: 60s)
    export.git-add    Auto-stage the export file (default: false)

Auto-Import (config.yaml):
  Reads .beads/issues.jsonl by default when a JSONL import path is implied.
  Use a relative filename/path so the import stays within the project .beads/
  directory and remains portable across machines.

  Keys:
    import.path       Input filename relative to .beads/ (default: issues.jsonl)

Custom Status States:
  You can define custom status states for multi-step pipelines using the
  status.custom config key. Statuses should be comma-separated.

  Example:
    bd config set status.custom "awaiting_review,awaiting_testing,awaiting_docs"

  This enables issues to use statuses like 'awaiting_review' in addition to
  the built-in statuses (open, in_progress, blocked, deferred, closed).

Claim Pools:
  A dispatcher can pre-assign issues to a pool pseudo-assignee (e.g.
  "fable-crew") and let any actor take them with --claim. List the pool
  aliases in the claim.pools config key, comma-separated:

    bd config set claim.pools "fable-crew,night-crew"

  Issues assigned to a real actor (or to an alias not in the list) keep
  their anti-steal protection. Pool takes carry the normal lease; note
  that if a taker's lease expires, bd reclaim returns the issue to the
  unassigned pool, not to the pool alias it was dispatched to.

Suppressing Doctor Warnings:
  Suppress specific bd doctor warnings by check name slug:
    bd config set doctor.suppress.pending-migrations true
    bd config set doctor.suppress.git-hooks true
  Check names are converted to slugs: "Git Hooks" → "git-hooks".
  Only warnings are suppressed (errors and passing checks always show).
  To unsuppress: bd config unset doctor.suppress.<slug>

Examples:
  bd config set export.auto true                       # Enable auto-export for viewer integrations
  bd config set export.path "beads.jsonl"              # Custom export filename
  bd config set import.path "beads.jsonl"              # Custom import filename
  bd config set export.git-add true                    # Also stage the export file
  bd config set jira.url "https://company.atlassian.net"
  bd config set jira.project "PROJ"
  bd config set status.custom "awaiting_review,awaiting_testing"
  bd config set claim.pools "fable-crew,night-crew"    # Pool aliases claimable by any actor
  bd config set doctor.suppress.pending-migrations true
  bd config set dolt.debug true                        # Enable Dolt sql-server debug mode (loglevel=debug, --prof cpu)
  bd config set dolt.local-only true                   # Skip wiring a Dolt sync remote during bd init
  bd config get export.auto
  bd config list
  bd config unset jira.url`,
}

var configSetCmd = &cobra.Command{
	Use:           "set <key> <value>",
	Short:         "Set a configuration value",
	Args:          cobra.ExactArgs(2),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("config-set")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		key := args[0]
		value := args[1]

		if msg, rejected := rejectProtectedConfigKey(key); rejected {
			fmt.Fprintln(os.Stderr, msg)
			return SilentExit()
		}

		if key == "dolt.debug" && !usesSQLServer() {
			fmt.Fprintln(os.Stderr, "Error: dolt.debug requires a sql-server-backed project (embedded mode has no managed server).")
			fmt.Fprintln(os.Stderr, "  To migrate: re-init with 'bd init --server' or 'bd init --shared-server'.")
			return SilentExit()
		}

		if strings.HasPrefix(key, "storage-class.") {
			if err := validateStorageClassConfig(key, value); err != nil {
				return HandleError("%v", err)
			}
		}

		if !isRecognizedConfigKey(key) {
			suggestion := suggestConfigKey(key)
			if suggestion != "" {
				fmt.Fprintf(os.Stderr, "Warning: %q is not a recognized config key. Did you mean %q?\n", key, suggestion)
			} else {
				fmt.Fprintf(os.Stderr, "Warning: %q is not a recognized config key. Use 'custom.*' for user-defined keys.\n", key)
			}
			fmt.Fprintf(os.Stderr, "Run 'bd config --help' for valid namespaces.\n")
		}

		if !forceGitTrackedEnabled(cmd) {
			if err := config.CheckSecretKeyGitSafety(key); err != nil {
				return HandleError("%v", err)
			}
		}

		if config.IsYamlOnlyKey(key) {
			var setErr error
			location := "config.yaml"
			if config.IsUserGlobalKey(key) {
				setErr = config.SetUserYamlConfig(key, value)
				location = config.UserConfigYamlDisplayPath()
			} else {
				setErr = config.SetYamlConfig(key, value)
			}
			if setErr != nil {
				return HandleError("setting config: %v", setErr)
			}

			if isJSONOutput() {
				if err := outputJSON(map[string]interface{}{
					"key":      key,
					"value":    value,
					"location": location,
				}); err != nil {
					return err
				}
			} else {
				fmt.Printf("Set %s = %s (in %s)\n", key, value, location)
			}
			printConfigSideEffects(checkConfigSetSideEffects(key, value))
			return nil
		}

		if key == "beads.role" {
			validRoles := map[string]bool{"maintainer": true, "contributor": true}
			if !validRoles[value] {
				return HandleError("invalid role %q (valid values: maintainer, contributor)", value)
			}
			cmd := exec.Command("git", "config", "beads.role", value) //nolint:gosec // value is validated against allowlist above
			if err := cmd.Run(); err != nil {
				return HandleError("setting beads.role in git config: %v", err)
			}
			if isJSONOutput() {
				if err := outputJSON(map[string]interface{}{
					"key":      key,
					"value":    value,
					"location": "git config",
				}); err != nil {
					return err
				}
			} else {
				fmt.Printf("Set %s = %s (in git config)\n", key, value)
			}
			return nil
		}

		// Everything above this line is FRONT-DOOR routing: which source owns
		// the key, and whether writing it to a file on this machine would leak
		// a secret into git. From here the key is known to belong to the
		// workspace database, and the write is the role's.
		settings, err := openWorkspaceConfig("config set requires direct database access")
		if err != nil {
			return HandleError("%v", err)
		}
		result, err := settings.SetSetting(getRootContext(), issueops.SetSettingRequest{Key: key, Value: value})
		if err != nil {
			return HandleError("setting config: %v", err)
		}
		noteDirectConfigWrite()

		if isJSONOutput() {
			if err := outputJSON(map[string]string{
				"key":   result.Key,
				"value": result.Value,
			}); err != nil {
				return err
			}
		} else {
			fmt.Printf("Set %s = %s\n", result.Key, result.Value)
		}
		printConfigSideEffects(checkConfigSetSideEffects(result.Key, result.Value))
		return nil
	},
}
