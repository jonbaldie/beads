package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
)

// AdvanceToNextStep finds the next ready step in a molecule after closing a step.
// If autoClaim is true, it marks the next step as in_progress using optimistic
// concurrency control: the step's status is re-verified before claiming to
// guard against TOCTOU races where multiple agents identify and try to claim the
// same step concurrently.
// Returns nil if the issue is not part of a molecule.
func AdvanceToNextStep(ctx context.Context, s molWriter, closedStepID string, autoClaim bool, actorName string) (*ContinueResult, error) {
	result, progress, err := loadAdvanceContext(ctx, s, closedStepID)
	if err != nil || result == nil {
		return result, err
	}

	// Check if molecule is complete
	if progress.Completed >= progress.Total {
		result.MolComplete = true
		return result, nil
	}

	// Collect all ready steps (not just the first) so we can fall back
	// if a concurrent agent claims one before us.
	readySteps := readyMoleculeSteps(progress)

	if len(readySteps) == 0 {
		// No ready steps - might be blocked
		return result, nil
	}

	result.NextStep = readySteps[0]

	if autoClaim {
		if candidate, ok := claimNextReadyStep(ctx, s, readySteps, actorName); ok {
			result.NextStep = candidate
			result.AutoAdvanced = true
		}
	}

	return result, nil
}

func loadAdvanceContext(ctx context.Context, s molWriter, closedStepID string) (*ContinueResult, *MoleculeProgress, error) {
	if s == nil {
		return nil, nil, fmt.Errorf("no database connection")
	}

	closedStep, err := s.GetIssue(ctx, closedStepID)
	if err != nil || closedStep == nil {
		return nil, nil, fmt.Errorf("could not get closed step: %w", err)
	}

	result := &ContinueResult{ClosedStep: closedStep}
	moleculeID := findParentMolecule(ctx, s, closedStepID)
	if moleculeID == "" {
		return nil, nil, nil
	}
	result.MoleculeID = moleculeID

	progress, err := getMoleculeProgress(ctx, s, moleculeID)
	if err != nil {
		return nil, nil, fmt.Errorf("could not load molecule: %w", err)
	}
	return result, progress, nil
}

func readyMoleculeSteps(progress *MoleculeProgress) []*types.Issue {
	readySteps := make([]*types.Issue, 0)
	for _, step := range progress.Steps {
		if step.Status == "ready" {
			readySteps = append(readySteps, step.Issue)
		}
	}
	return readySteps
}

func claimNextReadyStep(ctx context.Context, s molWriter, readySteps []*types.Issue, actorName string) (*types.Issue, bool) {
	for _, candidate := range readySteps {
		if err := s.ClaimStepIfOpen(ctx, candidate.ID, actorName); err == nil {
			return candidate, true
		}
		// This candidate was already claimed; try the next ready step
	}
	return nil, false
}

// PrintContinueResult prints the result of advancing to the next step
func PrintContinueResult(result *ContinueResult) {
	if result == nil {
		return
	}

	if result.MolComplete {
		fmt.Printf("\n%s Molecule %s complete! All steps closed.\n", ui.RenderPass("✓"), result.MoleculeID)
		fmt.Println("Consider: bd mol squash " + result.MoleculeID + " --summary '...'")
		return
	}

	if result.NextStep == nil {
		fmt.Println("\nNo ready steps in molecule (may be blocked).")
		return
	}

	fmt.Printf("\nNext ready in molecule:\n")
	fmt.Printf("  %s: %s\n", result.NextStep.ID, result.NextStep.Title)

	if result.AutoAdvanced {
		fmt.Printf("\n%s Marked in_progress (use --no-auto to skip)\n", ui.RenderWarn("→"))
	} else {
		fmt.Printf("\nStart with: bd update %s --claim\n", result.NextStep.ID)
	}
}

// parseRange parses a range string like "1-50" or "100-150" into start and end indices.
// Returns 1-based indices (start=1 means first step).
func parseRange(rangeStr string) (start, end int, err error) {
	parts := strings.Split(rangeStr, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected format 'start-end' (e.g., '1-50')")
	}
	start, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid start: %w", err)
	}
	end, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid end: %w", err)
	}
	if start < 1 {
		return 0, 0, fmt.Errorf("start must be >= 1")
	}
	if end < start {
		return 0, 0, fmt.Errorf("end must be >= start")
	}
	return start, end, nil
}

// filterStepsByRange filters steps to a 1-based range [start, end].
func filterStepsByRange(steps []*StepStatus, start, end int) []*StepStatus {
	// Convert to 0-based indices
	startIdx := start - 1
	endIdx := end

	if startIdx >= len(steps) {
		return nil
	}
	if endIdx > len(steps) {
		endIdx = len(steps)
	}
	return steps[startIdx:endIdx]
}

// printLargeMoleculeSummary prints a summary for molecules with many steps.
func printLargeMoleculeSummary(stats *types.MoleculeProgressStats) {
	fmt.Printf("Molecule: %s\n", ui.RenderAccent(stats.MoleculeID))
	fmt.Printf("  %s\n", stats.MoleculeTitle)
	fmt.Println()

	// Progress summary
	var percent float64
	if stats.Total > 0 {
		percent = float64(stats.Completed) * 100 / float64(stats.Total)
	}
	fmt.Printf("Progress: %d / %d steps (%.1f%%)\n", stats.Completed, stats.Total, percent)

	if stats.CurrentStepID != "" {
		fmt.Printf("Current step: %s\n", stats.CurrentStepID)
	} else if stats.InProgress > 0 {
		fmt.Printf("In progress: %d step(s)\n", stats.InProgress)
	}

	fmt.Println()
	fmt.Printf("%s This molecule has %d steps (threshold: %d).\n",
		ui.RenderWarn("Note:"), stats.Total, LargeMoleculeThreshold)
	fmt.Println("To view steps, use one of:")
	fmt.Printf("  bd mol current %s --limit 50        # First 50 steps\n", stats.MoleculeID)
	fmt.Printf("  bd mol current %s --range 1-50     # Steps 1-50\n", stats.MoleculeID)
	fmt.Printf("  bd mol progress %s                 # Efficient progress summary\n", stats.MoleculeID)

	// Show hint about viewing step instructions
	if stats.CurrentStepID != "" {
		fmt.Printf("\n%s Run `bd show %s` to see detailed instructions.\n", ui.RenderAccent("💡"), stats.CurrentStepID)
	}
}

func init() {
	molCurrentCmd.Flags().String("for", "", "Show molecules for a specific agent/assignee")
	molCurrentCmd.Flags().Int("limit", 0, "Maximum number of steps to display (0 = auto, use 'all' threshold)")
	molCurrentCmd.Flags().String("range", "", "Display specific step range (e.g., '1-50', '100-150')")
	molCmd.AddCommand(molCurrentCmd)
}
