package main

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

var molProgressCmd = &cobra.Command{
	Use:   "progress [molecule-id]",
	Short: "Show molecule progress summary",
	Long: `Show efficient progress summary for a molecule.

This command uses indexed queries to count progress without loading all steps,
making it suitable for very large molecules (millions of steps).

If no molecule-id is given, shows progress for any molecule you're working on.

Output includes:
  - Progress: completed / total (percentage)
  - Current step: the in-progress step (if any)
  - Rate: steps/hour based on closure times
  - ETA: estimated time to completion

Example:
  bd mol progress bd-hanoi-xyz`,
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("mol-progress")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if usesProxiedServer() {
			return runMolProgressProxiedServer(getRootContext(), args)
		}

		ctx := getRootContext()

		if getStore() == nil {
			return HandleErrorRespectJSON("no database connection")
		}

		var moleculeID string
		if len(args) == 1 {
			resolved, err := utils.ResolvePartialID(ctx, getStore(), args[0])
			if err != nil {
				return HandleErrorRespectJSON("molecule '%s' not found", args[0])
			}
			moleculeID = resolved
		} else {
			moleculeIDs := findInProgressMoleculeIDs(ctx, getStore(), getActor())
			if len(moleculeIDs) == 0 {
				if isJSONOutput() {
					return outputJSON([]interface{}{})
				}
				fmt.Println("No molecules in progress.")
				fmt.Println("\nUse: bd mol progress <molecule-id>")
				return nil
			}
			moleculeID = moleculeIDs[0]
		}

		stats, err := getStore().GetMoleculeProgress(ctx, moleculeID)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}

		if isJSONOutput() {
			output := map[string]interface{}{
				"molecule_id":     stats.MoleculeID,
				"molecule_title":  stats.MoleculeTitle,
				"total":           stats.Total,
				"completed":       stats.Completed,
				"in_progress":     stats.InProgress,
				"current_step_id": stats.CurrentStepID,
			}
			if stats.Total > 0 {
				output["percent"] = float64(stats.Completed) * 100 / float64(stats.Total)
			}
			if stats.FirstClosed != nil && stats.LastClosed != nil && stats.Completed > 1 {
				duration := stats.LastClosed.Sub(*stats.FirstClosed)
				if duration > 0 {
					rate := float64(stats.Completed-1) / duration.Hours()
					output["rate_per_hour"] = rate
					remaining := stats.Total - stats.Completed
					if rate > 0 {
						etaHours := float64(remaining) / rate
						output["eta_hours"] = etaHours
					}
				}
			}
			return outputJSON(output)
		}

		printMoleculeProgressStats(stats)
		return nil
	},
}

// findInProgressMoleculeIDs finds molecule IDs with in_progress steps for an agent.
// This is a lightweight version that only returns IDs without loading subgraphs.
func findInProgressMoleculeIDs(ctx context.Context, s molReader, agent string) []string {
	// Query for in_progress issues
	status := types.StatusInProgress
	filter := types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Status: &status}}
	if agent != "" {
		filter.Assignee = &agent
	}
	inProgressIssues, err := s.SearchIssues(ctx, "", filter)
	if err != nil || len(inProgressIssues) == 0 {
		return nil
	}

	// Batch-find parent molecules for all in_progress issues (bd-hn4q)
	issueIDs := make([]string, len(inProgressIssues))
	for i, issue := range inProgressIssues {
		issueIDs[i] = issue.ID
	}
	moleculeRoots := findParentMolecules(ctx, s, issueIDs)

	seen := make(map[string]bool)
	var moleculeIDs []string
	for _, issue := range inProgressIssues {
		moleculeID := moleculeRoots[issue.ID]
		if moleculeID != "" && !seen[moleculeID] {
			seen[moleculeID] = true
			moleculeIDs = append(moleculeIDs, moleculeID)
		}
	}

	return moleculeIDs
}

// printMoleculeProgressStats prints molecule progress in human-readable format
func printMoleculeProgressStats(stats *types.MoleculeProgressStats) {
	fmt.Printf("Molecule: %s (%s)\n", ui.RenderAccent(stats.MoleculeID), stats.MoleculeTitle)
	printMoleculeProgressStatsLine(stats)
	printMoleculeCurrentStep(stats)
	printMoleculeRate(stats)
}

func printMoleculeProgressStatsLine(stats *types.MoleculeProgressStats) {
	percent := moleculeProgressPercent(stats)
	fmt.Printf("Progress: %s / %s (%.1f%%)\n",
		formatNumber(stats.Completed), formatNumber(stats.Total), percent)
}

func moleculeProgressPercent(stats *types.MoleculeProgressStats) float64 {
	if stats.Total == 0 {
		return 0
	}
	return float64(stats.Completed) * 100 / float64(stats.Total)
}

func printMoleculeCurrentStep(stats *types.MoleculeProgressStats) {
	if stats.CurrentStepID != "" {
		fmt.Printf("Current step: %s\n", stats.CurrentStepID)
		return
	}
	if stats.InProgress > 0 {
		fmt.Printf("In progress: %d step(s)\n", stats.InProgress)
	}
}

func printMoleculeRate(stats *types.MoleculeProgressStats) {
	if stats.FirstClosed == nil || stats.LastClosed == nil || stats.Completed <= 1 {
		return
	}
	duration := stats.LastClosed.Sub(*stats.FirstClosed)
	if duration <= 0 {
		return
	}
	rate := float64(stats.Completed-1) / duration.Hours()
	fmt.Printf("Rate: ~%.0f steps/hour\n", rate)
	remaining := stats.Total - stats.Completed
	if rate > 0 && remaining > 0 {
		fmt.Printf("ETA: %s remaining\n", formatDuration(float64(remaining)/rate))
	}
}

// formatNumber formats large numbers with commas (handles millions)
func formatNumber(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		return fmt.Sprintf("%d,%03d", n/1000, n%1000)
	}
	millions := n / 1000000
	thousands := (n % 1000000) / 1000
	ones := n % 1000
	return fmt.Sprintf("%d,%03d,%03d", millions, thousands, ones)
}

// formatDuration formats hours as a human-readable duration
func formatDuration(hours float64) string {
	if hours < 1 {
		minutes := hours * 60
		return fmt.Sprintf("~%.0f minutes", minutes)
	}
	if hours < 24 {
		return fmt.Sprintf("~%.1f hours", hours)
	}
	days := hours / 24
	if days < 7 {
		return fmt.Sprintf("~%.1f days", days)
	}
	weeks := days / 7
	return fmt.Sprintf("~%.1f weeks", weeks)
}

func init() {
	molCmd.AddCommand(molProgressCmd)
}
