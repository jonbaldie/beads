package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/schema"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

func handleInspect() error {
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		if isJSONOutput() {
			if jerr := outputJSON(map[string]interface{}{
				"error":   "no_beads_directory",
				"message": activeWorkspaceNotFoundMessage() + " " + diagHint() + ".",
			}); jerr != nil {
				return jerr
			}
			return SilentExit()
		}
		return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
	}

	if getStore() == nil {
		return reportMissingMigrationInspection()
	}

	store := getStore()
	if store == nil {
		return HandleError("no database — run 'bd init' first")
	}
	inspection := collectMigrationInspection(getRootContext(), store)
	return reportMigrationInspection(inspection)
}

type migrationInspection struct {
	registeredMigrations []string
	schemaVersion        string
	issueCount           int
	config               map[string]string
	missingConfig        []string
	warnings             []string
}

func reportMissingMigrationInspection() error {
	result := map[string]interface{}{
		"registered_migrations": listMigrations(),
		"current_state": map[string]interface{}{
			"schema_version": "missing",
			"issue_count":    0,
			"config":         map[string]string{},
			"missing_config": []string{},
			"db_exists":      false,
		},
		"warnings":            []string{"Database does not exist - " + diagHint()},
		"invariants_to_check": []string{},
	}
	if isJSONOutput() {
		return outputJSON(result)
	}
	fmt.Println("\nMigration Inspection")
	fmt.Println("====================")
	fmt.Println("Database: missing")
	fmt.Println("\n⚠ Database does not exist - " + diagHint())
	return nil
}

func collectMigrationInspection(ctx context.Context, store storage.DoltStorage) migrationInspection {
	schemaVersion, err := store.GetLocalMetadata(ctx, "bd_version")
	if err != nil {
		schemaVersion = "unknown"
	}

	issueCount := 0
	if stats, err := store.GetStatistics(ctx); err == nil {
		issueCount = stats.TotalIssues
	}

	configMap := make(map[string]string)
	prefix, _ := store.GetConfig(ctx, "issue_prefix")
	if prefix != "" {
		configMap["issue_prefix"] = prefix
	}

	missingConfig := []string{}
	if issueCount > 0 && prefix == "" {
		missingConfig = append(missingConfig, "issue_prefix")
	}

	warnings := migrationInspectionWarnings(ctx, store, issueCount, prefix, schemaVersion)
	return migrationInspection{
		registeredMigrations: listMigrations(),
		schemaVersion:        schemaVersion,
		issueCount:           issueCount,
		config:               configMap,
		missingConfig:        missingConfig,
		warnings:             warnings,
	}
}

func migrationInspectionWarnings(ctx context.Context, store storage.DoltStorage, issueCount int, prefix string, schemaVersion string) []string {
	warnings := []string{}
	if issueCount > 0 && prefix == "" {
		detectedPrefix := ""
		if issues, err := store.SearchIssues(ctx, "", types.IssueFilter{}); err == nil && len(issues) > 0 {
			detectedPrefix = utils.ExtractIssuePrefix(issues[0].ID)
		}
		warnings = append(warnings, fmt.Sprintf("issue_prefix config not set - may break commands after migration (detected: %s)", detectedPrefix))
	}
	if schemaVersion != Version {
		warnings = append(warnings, fmt.Sprintf("schema version mismatch (current: %s, expected: %s)", schemaVersion, Version))
	}
	return warnings
}

func (i migrationInspection) result() map[string]interface{} {
	return map[string]interface{}{
		"registered_migrations": i.registeredMigrations,
		"current_state": map[string]interface{}{
			"schema_version": i.schemaVersion,
			"issue_count":    i.issueCount,
			"config":         i.config,
			"missing_config": i.missingConfig,
			"db_exists":      true,
		},
		"warnings":            i.warnings,
		"invariants_to_check": []string{},
	}
}

func reportMigrationInspection(inspection migrationInspection) error {
	if isJSONOutput() {
		return outputJSON(inspection.result())
	}
	fmt.Println("\nMigration Inspection")
	fmt.Println("====================")
	fmt.Printf("Schema Version: %s\n", inspection.schemaVersion)
	fmt.Printf("Issue Count: %d\n", inspection.issueCount)
	fmt.Printf("Registered Migrations: %d\n", len(inspection.registeredMigrations))

	if len(inspection.warnings) > 0 {
		fmt.Println("\nWarnings:")
		for _, warning := range inspection.warnings {
			fmt.Printf("  ⚠ %s\n", warning)
		}
	}

	if len(inspection.missingConfig) > 0 {
		fmt.Println("\nMissing Config:")
		for _, key := range inspection.missingConfig {
			fmt.Printf("  - %s\n", key)
		}
	}
	fmt.Println()
	return nil
}

func handleSchemaMigrate() error {
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		if isJSONOutput() {
			if jerr := outputJSON(map[string]interface{}{
				"error":   "no_beads_directory",
				"message": activeWorkspaceNotFoundMessage() + " " + diagHint() + ".",
			}); jerr != nil {
				return jerr
			}
			return SilentExit()
		}
		return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
	}

	store := getStore()
	if store == nil {
		return reportMissingSchemaMigrationDatabase()
	}

	migrator, ok := storage.UnwrapStore(store).(storage.SchemaMigrator)
	if !ok {
		return reportUnsupportedSchemaMigration()
	}

	applied, err := migrator.ApplySchemaMigrations(getRootContext())
	if err != nil {
		return reportSchemaMigrationFailure(err)
	}
	return reportSchemaMigrationResult(applied)
}

func reportMissingSchemaMigrationDatabase() error {
	if isJSONOutput() {
		if jerr := outputJSON(map[string]interface{}{
			"error":   "no_database",
			"message": "No database found. Run 'bd init' to create a new database.",
		}); jerr != nil {
			return jerr
		}
		return SilentExit()
	}
	return HandleErrorWithHint("no database", "Run 'bd init' to create a new database")
}

func reportUnsupportedSchemaMigration() error {
	if isJSONOutput() {
		if jerr := outputJSON(map[string]interface{}{
			"error":   "unsupported_backend",
			"message": "current storage backend does not support schema migration",
		}); jerr != nil {
			return jerr
		}
		return SilentExit()
	}
	return HandleError("current storage backend does not support schema migration")
}

func reportSchemaMigrationFailure(err error) error {
	if isJSONOutput() {
		if jerr := outputJSON(map[string]interface{}{
			"error":   "schema_migration_failed",
			"message": err.Error(),
		}); jerr != nil {
			return jerr
		}
		return SilentExit()
	}
	return HandleError("schema migration failed: %v", err)
}

func reportSchemaMigrationResult(applied int) error {
	latest := schema.LatestVersion()
	status := "current"
	if applied > 0 {
		status = "applied"
		commandDidWrite.Store(true)
	}

	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"status":         status,
			"applied":        applied,
			"latest_version": latest,
		})
	}

	if applied == 0 {
		fmt.Printf("%s\n", ui.RenderPass(fmt.Sprintf("✓ Schema already at v%d", latest)))
		return nil
	}
	fmt.Printf("%s\n", ui.RenderPass(fmt.Sprintf("✓ Applied %d schema migration(s); schema now at v%d", applied, latest)))
	return nil
}

func handleToSeparateBranch(branch string, dryRun bool) error {
	b, err := validateSeparateBranch(branch)
	if err != nil {
		return err
	}

	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		if isJSONOutput() {
			if jerr := outputJSON(map[string]interface{}{
				"error":   "no_beads_directory",
				"message": activeWorkspaceNotFoundMessage() + " " + diagHint() + ".",
			}); jerr != nil {
				return jerr
			}
			return SilentExit()
		}
		return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
	}

	store := getStore()
	if store == nil {
		return HandleError("no database — run 'bd init' first")
	}

	ctx := getRootContext()
	current, _ := store.GetConfig(ctx, "sync.branch")

	if dryRun {
		return reportSeparateBranchDryRun(current, b)
	}

	if current == b {
		return reportSeparateBranchNoop(b)
	}

	if err := store.SetConfig(ctx, "sync.branch", b); err != nil {
		return reportSeparateBranchWriteFailure(err)
	}

	commandDidWrite.Store(true)
	return reportSeparateBranchSuccess(current, b)
}

func validateSeparateBranch(branch string) (string, error) {
	b := strings.TrimSpace(branch)
	if b != "" && !strings.ContainsAny(b, " \t\n") {
		return b, nil
	}
	if isJSONOutput() {
		if jerr := outputJSON(map[string]interface{}{
			"error":   "invalid_branch",
			"message": "Branch name cannot be empty or contain whitespace",
		}); jerr != nil {
			return "", jerr
		}
		return "", SilentExit()
	}
	return "", HandleErrorWithHint(fmt.Sprintf("invalid branch name '%s'", branch), "branch name cannot be empty or contain whitespace")
}

func reportSeparateBranchDryRun(current string, branch string) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"dry_run":  true,
			"previous": current,
			"branch":   branch,
			"changed":  current != branch,
		})
	}
	fmt.Println("Dry run mode - no changes will be made")
	if current == branch {
		fmt.Printf("sync.branch already set to '%s'\n", branch)
	} else {
		fmt.Printf("Would set sync.branch: '%s' → '%s'\n", current, branch)
	}
	return nil
}

func reportSeparateBranchNoop(branch string) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"status":  "noop",
			"branch":  branch,
			"message": "sync.branch already set to this value",
		})
	}
	fmt.Printf("%s\n", ui.RenderPass(fmt.Sprintf("✓ sync.branch already set to '%s'", branch)))
	fmt.Println("No changes needed")
	return nil
}

func reportSeparateBranchWriteFailure(err error) error {
	if isJSONOutput() {
		if jerr := outputJSON(map[string]interface{}{
			"error":   "config_update_failed",
			"message": err.Error(),
		}); jerr != nil {
			return jerr
		}
		return SilentExit()
	}
	return HandleError("failed to set sync.branch: %v", err)
}

func reportSeparateBranchSuccess(previous string, branch string) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"status":   "success",
			"previous": previous,
			"branch":   branch,
			"message":  "Enabled separate branch workflow",
		})
	}
	fmt.Printf("%s\n\n", ui.RenderPass("✓ Enabled separate branch workflow"))
	fmt.Printf("Set sync.branch to '%s'\n\n", branch)
	fmt.Println("Next steps:")
	fmt.Println("  1. No restart required. sync.branch is active immediately.")
	fmt.Printf("     bd dolt push\n\n")
	fmt.Println("  2. Your existing data is preserved - no changes to git history")
	fmt.Println("  3. Future issue updates are stored in Dolt directly")
	return nil
}

// listMigrations returns registered Dolt schema migrations. The compat runner
// was retired once all historical migrations had SQL equivalents; this is
// kept as a stable hook for `bd migrate --inspect` output.
func listMigrations() []string {
	return nil
}

// migrateSyncCmd is the "bd migrate sync <branch>" subcommand that
// configures the separate-branch workflow for multi-clone setups.
// Previously this was documented but never wired as an actual subcommand,
// so bd doctor's recommendation to run "bd migrate sync beads-sync" would fail.
var migrateSyncCmd = &cobra.Command{
	Use:   "sync <branch>",
	Short: "Set up sync.branch workflow for multi-clone setups",
	Long: `Configure separate branch workflow for multi-clone setups.

This sets the sync.branch config value so that issue data is committed
to a dedicated branch, keeping your main branch clean.

Example:
  bd migrate sync beads-sync`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if usesProxiedServer() {
			return HandleErrorRespectJSON("migrate sync is not supported in proxied-server mode")
		}
		evt := metrics.NewCommandEvent("migrate-sync")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		dryRun, _ := cmd.Flags().GetBool("dry-run")
		if !dryRun {
			if err := CheckReadonly("migrate sync"); err != nil {
				return err
			}
		}
		return handleToSeparateBranch(args[0], dryRun)
	},
}

var migrateSchemaCmd = &cobra.Command{
	Use:   "schema",
	Short: "Apply pending schema migrations (idempotent)",
	Long: `Apply pending schema migrations idempotently.

Schema migrations also run automatically on store open, so this subcommand
is typically a no-op. It exists to make migration explicit and observable
in CI, release gates, and recovery scenarios.

Example:
  bd migrate schema
  bd migrate schema --json`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if usesProxiedServer() {
			return HandleErrorRespectJSON("migrate schema is not supported in proxied-server mode")
		}
		if err := CheckReadonly("migrate schema"); err != nil {
			return err
		}

		evt := metrics.NewCommandEvent("migrate-schema")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		return handleSchemaMigrate()
	},
}

func init() {
	flags := persistentFlags()
	migrateCmd.Flags().Bool("yes", false, "Auto-confirm prompts")
	migrateCmd.Flags().Bool("dry-run", false, "Show what would be done without making changes")
	migrateCmd.Flags().Bool("update-repo-id", false, "Update repository ID (use after changing git remote)")
	migrateCmd.Flags().Bool("inspect", false, "Show migration plan and database state for AI agent analysis")
	migrateCmd.Flags().BoolVar(&flags.JSONOutput, "json", false, "Output migration statistics in JSON format")
	// --force bypasses the remote-migrate gate (#4259) as the single designated
	// migrator. No -f shorthand: deliberate typing for a fork-risk bypass.
	migrateCmd.Flags().Bool("force", false, "Bypass the remote-migrate gate as the single designated migrator (equivalent to BD_ALLOW_REMOTE_MIGRATE=1)")

	migrateSyncCmd.Flags().Bool("dry-run", false, "Show what would be done without making changes")
	migrateSyncCmd.Flags().BoolVar(&flags.JSONOutput, "json", false, "Output in JSON format")
	migrateCmd.AddCommand(migrateSyncCmd)

	migrateHooksCmd.Flags().Bool("dry-run", false, "Show what would be done without making changes")
	migrateHooksCmd.Flags().Bool("apply", false, "Apply planned hook migration changes")
	migrateHooksCmd.Flags().Bool("yes", false, "Skip confirmation prompt for --apply")
	migrateHooksCmd.Flags().BoolVar(&flags.JSONOutput, "json", false, "Output in JSON format")
	migrateCmd.AddCommand(migrateHooksCmd)

	migrateSchemaCmd.Flags().BoolVar(&flags.JSONOutput, "json", false, "Output in JSON format")
	// --force on migrate schema mirrors the parent command's flag; both trip the
	// same isForcedMigrate check in main.go's PersistentPreRunE.
	migrateSchemaCmd.Flags().Bool("force", false, "Bypass the remote-migrate gate as the single designated migrator (equivalent to BD_ALLOW_REMOTE_MIGRATE=1)")
	migrateCmd.AddCommand(migrateSchemaCmd)

	migrateToProxiedServerCmd.Flags().Bool("dry-run", false, "Show what would be done without making changes")
	migrateToProxiedServerCmd.Flags().Duration("idle-timeout", 0, "Proxy idle timeout; omit for the 30s default, 0 for indefinite uptime")
	migrateCmd.AddCommand(migrateToProxiedServerCmd)

	migrateSharedToProxiedServerCmd.Flags().Bool("dry-run", false, "Show what would be done without making changes")
	migrateSharedToProxiedServerCmd.Flags().Duration("idle-timeout", 0, "Proxy idle timeout; omit for the 30s default, 0 for indefinite uptime")
	migrateCmd.AddCommand(migrateSharedToProxiedServerCmd)

	migrateToServerCmd.Flags().Bool("dry-run", false, "Show what would be done without making changes")
	migrateCmd.AddCommand(migrateToServerCmd)

	migrateToSharedServerCmd.Flags().Bool("dry-run", false, "Show what would be done without making changes")
	migrateCmd.AddCommand(migrateToSharedServerCmd)

	rootCmd.AddCommand(migrateCmd)
}
