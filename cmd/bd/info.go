package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/workapi"
	"github.com/spf13/cobra"
)

var infoCmd = &cobra.Command{
	Use:     "info",
	GroupID: "setup",
	Short:   "Show database information",
	Long: `Display information about the current database.

This command helps debug issues where bd is using an unexpected database. It shows:
  - The absolute path to the database file
  - Database statistics (issue count)
  - Schema information (with --schema flag)
  - What's new in recent versions (with --whats-new flag)

Examples:
  bd info
  bd info --json
  bd info --schema --json
  bd info --whats-new
  bd info --whats-new --json
  bd info --thanks`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("info")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		schemaFlag, _ := cmd.Flags().GetBool("schema")
		whatsNewFlag, _ := cmd.Flags().GetBool("whats-new")
		thanksFlag, _ := cmd.Flags().GetBool("thanks")

		if thanksFlag {
			printThanksPage()
			return nil
		}

		if whatsNewFlag {
			return showWhatsNew()
		}

		if usesProxiedServer() {
			return runInfoProxiedServer(getRootContext(), schemaFlag)
		}

		absDBPath := absoluteDBPath()

		info := map[string]interface{}{
			"database_path": absDBPath,
			"mode":          "direct",
		}

		if getStore() != nil {
			ctx := getRootContext()

			issues, err := getStore().SearchIssues(ctx, "", types.IssueFilter{})
			if err == nil {
				info["issue_count"] = len(issues)
			}

			// THE SAME FILTER THE SETTINGS ROLE USES. `bd info --json` serves
			// this map whole, and the beads MCP server's get_schema_info tool
			// runs `bd info --schema --json` and returns the parsed dict —
			// config included — so every memory key AND VALUE landed in the
			// transcript of any agent that asked a SCHEMA question. `bd info`
			// is also the diagnostic people paste into bug reports.
			//
			// Unlike `bd config show`, which an operator asks for by name to
			// see provenance, nothing here says "show me my memories".
			configMap, err := getStore().GetAllConfig(ctx)
			if err == nil {
				if filtered := workapi.FilterSettingsEnumeration(configMap); len(filtered) > 0 {
					info["config"] = filtered
				}
			}

			if schemaFlag {
				schemaVersion, err := getStore().GetLocalMetadata(ctx, "bd_version")
				if err != nil {
					schemaVersion = "unknown"
				}
				prefix, _ := getStore().GetConfig(ctx, "issue_prefix") // Best effort: empty prefix is valid
				info["schema"] = buildInfoSchema(schemaVersion, prefix, issues)
			}
		}

		return renderInfo(info, schemaFlag, absDBPath)
	},
}

func absoluteDBPath() string {
	absDBPath, err := filepath.Abs(getDBPath())
	if err != nil {
		return getDBPath()
	}
	return absDBPath
}

func buildInfoSchema(schemaVersion, prefix string, issues []*types.Issue) map[string]interface{} {
	tables := []string{"issues", "dependencies", "labels", "config", "metadata"}

	configMap := make(map[string]string)
	if prefix != "" {
		configMap["issue_prefix"] = prefix
	}

	sampleIDs := []string{}
	detectedPrefix := ""
	if len(issues) > 0 {
		maxSamples := 3
		if len(issues) < maxSamples {
			maxSamples = len(issues)
		}
		for i := 0; i < maxSamples; i++ {
			sampleIDs = append(sampleIDs, issues[i].ID)
		}
		detectedPrefix = extractPrefix(issues[0].ID)
	}

	return map[string]interface{}{
		"tables":           tables,
		"schema_version":   schemaVersion,
		"config":           configMap,
		"sample_issue_ids": sampleIDs,
		"detected_prefix":  detectedPrefix,
	}
}

func renderInfo(info map[string]interface{}, schemaFlag bool, absDBPath string) error {
	if isJSONOutput() {
		return outputJSON(info)
	}

	mode, _ := info["mode"].(string)

	fmt.Println("\nBeads Database Information")
	fmt.Println("===========================")
	fmt.Printf("Database: %s\n", absDBPath)
	fmt.Printf("Mode: %s\n", mode)

	if count, ok := info["issue_count"].(int); ok {
		fmt.Printf("\nIssue Count: %d\n", count)
	}

	renderInfoSchema(info, schemaFlag)

	hookStatuses := CheckGitHooks()
	if warning := FormatHookWarnings(hookStatuses); warning != "" {
		fmt.Printf("\n%s\n", warning)
	}

	fmt.Println()
	return nil
}

func renderInfoSchema(info map[string]interface{}, schemaFlag bool) {
	if !schemaFlag {
		return
	}
	schemaInfo, ok := info["schema"].(map[string]interface{})
	if !ok {
		return
	}
	fmt.Println("\nSchema Information:")
	fmt.Printf("  Tables: %v\n", schemaInfo["tables"])
	renderInfoSchemaDetails(schemaInfo)
}

func renderInfoSchemaDetails(schemaInfo map[string]interface{}) {
	if version, ok := schemaInfo["schema_version"].(string); ok {
		fmt.Printf("  Schema Version: %s\n", version)
	}
	if prefix, ok := schemaInfo["detected_prefix"].(string); ok && prefix != "" {
		fmt.Printf("  Detected Prefix: %s\n", prefix)
	}
	if samples, ok := schemaInfo["sample_issue_ids"].([]string); ok && len(samples) > 0 {
		fmt.Printf("  Sample Issues: %v\n", samples)
	}
}

// extractPrefix extracts the prefix from an issue ID (e.g., "bd-123" -> "bd")
// Uses the last hyphen before a numeric suffix, so "beads-vscode-1" -> "beads-vscode"
func extractPrefix(issueID string) string {
	// Try last hyphen first (handles multi-part prefixes like "beads-vscode-1")
	lastIdx := strings.LastIndex(issueID, "-")
	if lastIdx <= 0 {
		return ""
	}

	suffix := issueID[lastIdx+1:]
	// Check if suffix is numeric
	if len(suffix) > 0 {
		numPart := suffix
		if dotIdx := strings.Index(suffix, "."); dotIdx > 0 {
			numPart = suffix[:dotIdx]
		}
		var num int
		if _, err := fmt.Sscanf(numPart, "%d", &num); err == nil {
			return issueID[:lastIdx]
		}
	}

	// Suffix is not numeric, fall back to first hyphen
	firstIdx := strings.Index(issueID, "-")
	if firstIdx <= 0 {
		return ""
	}
	return issueID[:firstIdx]
}

// VersionChange represents agent-relevant changes for a specific version
func showWhatsNew() error {
	currentVersion := Version

	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"current_version": currentVersion,
			"recent_changes":  versionChanges,
		})
	}

	// Human-readable output
	fmt.Printf("\n🆕 What's New in bd (Current: v%s)\n", currentVersion)
	fmt.Println("=" + strings.Repeat("=", 60))
	fmt.Println()

	for _, vc := range versionChanges {
		// Highlight if this is the current version
		versionMarker := ""
		if vc.Version == currentVersion {
			versionMarker = " ← current"
		}

		fmt.Printf("## v%s (%s)%s\n\n", vc.Version, vc.Date, versionMarker)

		for _, change := range vc.Changes {
			fmt.Printf("  • %s\n", change)
		}
		fmt.Println()
	}

	fmt.Println("💡 Tip: Use `bd info --whats-new --json` for machine-readable output")
	fmt.Println()
	return nil
}

func init() {
	infoCmd.Flags().Bool("schema", false, "Include schema information in output")
	infoCmd.Flags().Bool("whats-new", false, "Show agent-relevant changes from recent versions")
	infoCmd.Flags().Bool("thanks", false, "Show thank you page for contributors")
	rootCmd.AddCommand(infoCmd)
}
