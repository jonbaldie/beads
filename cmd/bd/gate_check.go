package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

type gateCheckResult struct {
	gate      *types.Issue
	resolved  bool
	escalated bool
	reason    string
	err       error
}

func filterCheckableGates(gates []*types.Issue, typeFilter string) []*types.Issue {
	var out []*types.Issue
	for _, gate := range gates {
		if shouldCheckGate(gate, typeFilter) {
			out = append(out, gate)
		}
	}
	return out
}

func printNoOpenGates(typeFilter string) {
	if typeFilter != "" {
		fmt.Printf("No open gates of type '%s' found.\n", typeFilter)
	} else {
		fmt.Println("No open gates found.")
	}
}

func evaluateGates(ctx context.Context, gates []*types.Issue, now time.Time, getter issueGetter, persistAwaitID func(gateID, runID string) error) []gateCheckResult {
	results := make([]gateCheckResult, 0, len(gates))
	for _, gate := range gates {
		r := gateCheckResult{gate: gate}
		switch {
		case strings.HasPrefix(gate.AwaitType, "gh:run"):
			r.resolved, r.escalated, r.reason, r.err = checkGHRun(gate, persistAwaitID)
		case strings.HasPrefix(gate.AwaitType, "gh:pr"):
			r.resolved, r.escalated, r.reason, r.err = checkGHPR(gate)
		case gate.AwaitType == "timer":
			r.resolved, r.escalated, r.reason, r.err = checkTimer(gate, now)
		case gate.AwaitType == "bead":
			r.resolved, r.reason = checkBeadGate(ctx, getter, gate.AwaitID)
		default:
			continue
		}
		results = append(results, r)
	}
	return results
}

func applyGateCheckResults(results []gateCheckResult, dryRun, escalate bool, closeResolved func(gate *types.Issue, reason string) error) (resolvedCount, escalatedCount, errorCount int) {
	for _, r := range results {
		if r.err != nil {
			errorCount++
			fmt.Fprintf(os.Stderr, "%s %s: error checking - %v\n",
				ui.RenderFail("✗"), r.gate.ID, r.err)
			continue
		}

		switch {
		case r.resolved:
			resolvedCount++
			if dryRun {
				fmt.Printf("%s %s: would resolve - %s\n",
					ui.RenderPass("✓"), r.gate.ID, r.reason)
				continue
			}
			if closeErr := closeResolved(r.gate, r.reason); closeErr != nil {
				fmt.Fprintf(os.Stderr, "%s %s: error closing - %v\n",
					ui.RenderFail("✗"), r.gate.ID, closeErr)
				errorCount++
			} else {
				fmt.Printf("%s %s: resolved - %s\n",
					ui.RenderPass("✓"), r.gate.ID, r.reason)
			}
		case r.escalated:
			escalatedCount++
			if dryRun {
				fmt.Printf("%s %s: would escalate - %s\n",
					ui.RenderWarn("⚠"), r.gate.ID, r.reason)
				continue
			}
			fmt.Printf("%s %s: ESCALATE - %s\n",
				ui.RenderWarn("⚠"), r.gate.ID, r.reason)
			if escalate {
				escalateGate(r.gate, r.reason)
			}
		default:
			fmt.Printf("%s %s: pending - %s\n",
				ui.RenderAccent("○"), r.gate.ID, r.reason)
		}
	}
	return resolvedCount, escalatedCount, errorCount
}

func printGateCheckSummary(checked, resolvedCount, escalatedCount, errorCount int, dryRun bool) error {
	fmt.Println()
	fmt.Printf("Checked %d gates: %d resolved, %d escalated, %d errors\n",
		checked, resolvedCount, escalatedCount, errorCount)

	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"checked":   checked,
			"resolved":  resolvedCount,
			"escalated": escalatedCount,
			"errors":    errorCount,
			"dry_run":   dryRun,
		})
	}
	return nil
}

// shouldCheckGate returns true if the gate matches the type filter
func shouldCheckGate(gate *types.Issue, typeFilter string) bool {
	if typeFilter == "" || typeFilter == "all" {
		return true
	}
	if typeFilter == "gh" {
		return strings.HasPrefix(gate.AwaitType, "gh:")
	}
	return gate.AwaitType == typeFilter
}

func init() {
	// gate list flags
	gateListCmd.Flags().BoolP("all", "a", false, "Show all gates including closed")
	gateListCmd.Flags().IntP("limit", "n", 50, "Limit results (default 50)")

	// gate resolve flags
	gateResolveCmd.Flags().StringP("reason", "r", "", "Reason for resolving the gate")

	// gate check flags
	gateCheckCmd.Flags().StringP("type", "t", "", "Gate type to check (gh, gh:run, gh:pr, timer, bead, all)")
	gateCheckCmd.Flags().Bool("dry-run", false, "Show what would happen without making changes")
	gateCheckCmd.Flags().BoolP("escalate", "e", false, "Escalate failed/expired gates")
	gateCheckCmd.Flags().IntP("limit", "l", 100, "Limit results (default 100)")

	// gate create flags
	gateCreateCmd.Flags().String("blocks", "", "Issue ID to block (required)")
	gateCreateCmd.Flags().StringP("type", "t", "human", "Gate type (human, timer, gh:run, gh:pr)")
	gateCreateCmd.Flags().StringP("reason", "r", "", "Reason for the gate")
	gateCreateCmd.Flags().String("await-id", "", "Condition identifier (run ID, PR number, etc.)")
	gateCreateCmd.Flags().String("timeout", "", "Timeout duration (e.g., 2h, 30m)")
	gateCreateCmd.Flags().String("title", "", "Custom gate title (default: \"Gate: <type>\")")
	_ = gateCreateCmd.MarkFlagRequired("blocks")

	// Issue ID completions
	configureGateIssueIDCompletions(gateShowCmd, gateResolveCmd, gateAddWaiterCmd, gateCreateCmd)

	// Add subcommands
	gateCmd.AddCommand(gateListCmd)
	gateCmd.AddCommand(gateCreateCmd)
	gateCmd.AddCommand(gateShowCmd)
	gateCmd.AddCommand(gateResolveCmd)
	gateCmd.AddCommand(gateCheckCmd)
	gateCmd.AddCommand(gateAddWaiterCmd)

	rootCmd.AddCommand(gateCmd)
}

func configureGateIssueIDCompletions(commands ...*cobra.Command) {
	for _, command := range commands {
		command.ValidArgsFunction = issueIDCompletion
	}
}
