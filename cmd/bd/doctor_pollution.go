package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
)

// runPollutionCheck runs detailed test pollution detection
// This integrates the detect-pollution command functionality into doctor.
//
//nolint:unparam // path reserved for future use
func runPollutionCheck(_ string, clean bool, yes bool) error {
	if err := ensureDirectMode("pollution check requires direct mode"); err != nil {
		return HandleError("%v", err)
	}

	ctx := getRootContext()

	// Get all issues (env-only cap via BEADS_MAX_ROWS; designer §4: doctor
	// family is env opt-in, no operator-facing flag).
	maxRows, maxRowsSource := resolveMaxRowsEnvOnly()
	allIssues, err := getStore().SearchIssues(ctx, "", types.IssueFilter{
		IssueFilterPage: types.IssueFilterPage{
			MaxRows:       maxRows,
			MaxRowsSource: maxRowsSource,
		},
	})
	if err != nil {
		if capErr := handleMaxRowsError(err); capErr != nil {
			return capErr
		}
		return HandleError("fetching issues: %v", err)
	}

	polluted := detectTestPollution(allIssues)

	if len(polluted) == 0 {
		if isJSONOutput() {
			return outputJSON(map[string]interface{}{
				"polluted_count": 0,
				"issues":         []interface{}{},
			})
		}
		fmt.Println("No test pollution detected!")
		return nil
	}

	highConfidence, mediumConfidence := categorizePollutionResults(polluted)

	if isJSONOutput() {
		return outputPollutionJSON(polluted, highConfidence, mediumConfidence)
	}

	printPollutionResults(polluted, highConfidence, mediumConfidence)

	if !clean {
		fmt.Printf("Run 'bd doctor --check=pollution --clean' to delete these issues (with confirmation).\n")
		return nil
	}

	return cleanPollutedIssues(ctx, polluted, yes)
}

func categorizePollutionResults(polluted []pollutionResult) (high, medium []pollutionResult) {
	for _, p := range polluted {
		if p.score >= 0.9 {
			high = append(high, p)
		} else {
			medium = append(medium, p)
		}
	}
	return high, medium
}

func outputPollutionJSON(polluted, high, medium []pollutionResult) error {
	issues := make([]map[string]interface{}, 0, len(polluted))
	for _, p := range polluted {
		issues = append(issues, map[string]interface{}{
			"id":         p.issue.ID,
			"title":      p.issue.Title,
			"score":      p.score,
			"reasons":    p.reasons,
			"created_at": p.issue.CreatedAt,
		})
	}
	return outputJSON(map[string]interface{}{
		"polluted_count":    len(polluted),
		"high_confidence":   len(high),
		"medium_confidence": len(medium),
		"issues":            issues,
	})
}

func printPollutionResults(polluted, high, medium []pollutionResult) {
	fmt.Printf("Found %d potential test issues:\n\n", len(polluted))
	printPollutionCategory("High Confidence (score ≥ 0.9):", high)
	printPollutionCategory("Medium Confidence (score 0.7-0.9):", medium)
}

func printPollutionCategory(title string, issues []pollutionResult) {
	if len(issues) == 0 {
		return
	}
	fmt.Printf("%s\n", title)
	for _, p := range issues {
		fmt.Printf("  %s: %q (score: %.2f)\n", p.issue.ID, p.issue.Title, p.score)
		for _, reason := range p.reasons {
			fmt.Printf("    - %s\n", reason)
		}
	}
	fmt.Printf("  (Total: %d issues)\n\n", len(issues))
}

func cleanPollutedIssues(ctx context.Context, polluted []pollutionResult, yes bool) error {
	if !yes && !confirmPollutionCleanup(len(polluted)) {
		return nil
	}

	backupPath := ".beads/pollution-backup.jsonl"
	if err := backupPollutedIssues(polluted, backupPath); err != nil {
		return HandleError("backing up issues: %v", err)
	}
	fmt.Printf("Backed up %d issues to %s\n", len(polluted), backupPath)

	deleted := deletePollutedIssues(ctx, polluted)
	fmt.Printf("%s Deleted %d test issues\n", ui.RenderPass("✓"), deleted)
	fmt.Printf("\nCleanup complete. To restore, run: bd init --from-jsonl %s\n", backupPath)
	return nil
}

func confirmPollutionCleanup(count int) bool {
	fmt.Printf("\nDelete %d test issues? [y/N] ", count)
	var response string
	_, _ = fmt.Scanln(&response)
	if strings.ToLower(response) != "y" {
		fmt.Println("Canceled.")
		return false
	}
	return true
}

func deletePollutedIssues(ctx context.Context, polluted []pollutionResult) int {
	fmt.Printf("\nDeleting %d issues...\n", len(polluted))
	deleted := 0
	for _, p := range polluted {
		if err := deleteIssue(ctx, p.issue.ID); err != nil {
			fmt.Fprintf(os.Stderr, "Error deleting %s: %v\n", p.issue.ID, err)
			continue
		}
		deleted++
	}
	return deleted
}

func init() {
	rootCmd.AddCommand(doctorCmd)
	doctorCmd.Flags().Bool("perf", false, "Run performance diagnostics and generate CPU profile")
	doctorCmd.Flags().Bool("check-health", false, "Quick health check for git hooks (silent on success)")
	doctorCmd.Flags().StringP("output", "o", "", "Export diagnostics to JSON file")
	doctorCmd.Flags().String("check", "", "Run specific check in detail (e.g., 'pollution')")
	doctorCmd.Flags().Bool("clean", false, "For pollution check: delete detected test issues")
	doctorCmd.Flags().Bool("deep", false, "Validate full graph integrity")
}
