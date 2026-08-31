package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

// defaultCloseEligibleReason is the close reason `bd epic close-eligible` has
// always used; --reason overrides it (GH#4817).
const defaultCloseEligibleReason = "All children completed"

var epicCmd = &cobra.Command{
	Use:     "epic",
	GroupID: "deps",
	Short:   "Epic management commands",
}
var epicStatusCmd = &cobra.Command{
	Use:           "status",
	Short:         "Show epic completion status",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("epic-status")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		eligibleOnly, _ := cmd.Flags().GetBool("eligible-only")

		if usesProxiedServer() {
			return runEpicStatusProxiedServer(getRootContext(), eligibleOnly)
		}

		epics, err := getStore().GetEpicsEligibleForClosure(getRootContext())
		if err != nil {
			return HandleErrorRespectJSON("getting epic status: %v", err)
		}
		return renderEpicStatus(epics, eligibleOnly)
	},
}

func renderEpicStatus(epics []*types.EpicStatus, eligibleOnly bool) error {
	if eligibleOnly {
		epics = filterEligibleEpics(epics)
	}
	if isJSONOutput() {
		if epics == nil {
			epics = []*types.EpicStatus{}
		}
		return outputJSON(epics)
	}
	if len(epics) == 0 {
		fmt.Println("No open epics found")
		return nil
	}
	for _, epicStatus := range epics {
		renderEpicStatusLine(epicStatus)
	}
	return nil
}

func renderEpicStatusLine(epicStatus *types.EpicStatus) {
	epic := epicStatus.Epic
	percentage := epicCompletionPercentage(epicStatus)
	statusIcon := epicStatusIcon(epicStatus.EligibleForClose, percentage)
	fmt.Printf("%s %s %s\n", statusIcon, ui.RenderAccent(epic.ID), ui.RenderBold(epic.Title))
	fmt.Printf("   Progress: %d/%d children closed (%d%%)\n",
		epicStatus.ClosedChildren, epicStatus.TotalChildren, percentage)
	if epicStatus.EligibleForClose {
		fmt.Printf("   %s\n", ui.RenderPass("Eligible for closure"))
	}
	fmt.Println()
}

func epicCompletionPercentage(epicStatus *types.EpicStatus) int {
	if epicStatus.TotalChildren == 0 {
		return 0
	}
	return (epicStatus.ClosedChildren * 100) / epicStatus.TotalChildren
}

func epicStatusIcon(eligible bool, percentage int) string {
	if eligible {
		return ui.RenderPass("✓")
	}
	if percentage > 0 {
		return ui.RenderWarn("○")
	}
	return "○"
}

func filterEligibleEpics(epics []*types.EpicStatus) []*types.EpicStatus {
	filtered := []*types.EpicStatus{}
	for _, epic := range epics {
		if epic.EligibleForClose {
			filtered = append(filtered, epic)
		}
	}
	return filtered
}

func resolveCloseEligibleReason(cmd *cobra.Command) string {
	reason, _ := cmd.Flags().GetString("reason")
	if strings.TrimSpace(reason) == "" {
		return defaultCloseEligibleReason
	}
	return reason
}

var closeEligibleEpicsCmd = &cobra.Command{
	Use:           "close-eligible",
	Short:         "Close epics where all children are complete",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("epic-close-eligible")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		dryRun, _ := cmd.Flags().GetBool("dry-run")
		reason := resolveCloseEligibleReason(cmd)

		if err := validateCloseReasons([]string{reason}); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}

		if usesProxiedServer() {
			return runCloseEligibleEpicsProxiedServer(getRootContext(), dryRun, reason)
		}

		if !dryRun {
			if err := CheckReadonly("epic close-eligible"); err != nil {
				return err
			}
		}
		epics, err := getStore().GetEpicsEligibleForClosure(getRootContext())
		if err != nil {
			return HandleErrorRespectJSON("getting eligible epics: %v", err)
		}
		eligibleEpics := filterEligibleEpics(epics)
		if len(eligibleEpics) == 0 {
			return outputNoEligibleEpics(reason)
		}
		if dryRun {
			return outputCloseEligibleDryRun(eligibleEpics, reason)
		}
		closedIDs := []string{}
		for _, epicStatus := range eligibleEpics {
			err := getStore().CloseIssue(getRootContext(), epicStatus.Epic.ID, reason, "system", "")
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error closing %s: %v\n", epicStatus.Epic.ID, err)
				continue
			}
			closedIDs = append(closedIDs, epicStatus.Epic.ID)
		}
		if len(closedIDs) > 0 {
			commandDidWrite.Store(true)
		}
		return outputCloseEligibleResult(closedIDs, reason)
	},
}

func outputNoEligibleEpics(reason string) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"closed": []string{},
			"count":  0,
			"reason": reason,
		})
	}
	fmt.Println("No epics eligible for closure")
	return nil
}

func outputCloseEligibleDryRun(eligibleEpics []*types.EpicStatus, reason string) error {
	if isJSONOutput() {
		return outputJSON(eligibleEpics)
	}
	fmt.Printf("Would close %d epic(s) with reason %q:\n", len(eligibleEpics), reason)
	for _, epicStatus := range eligibleEpics {
		fmt.Printf("  - %s: %s\n", epicStatus.Epic.ID, epicStatus.Epic.Title)
	}
	return nil
}

func outputCloseEligibleResult(closedIDs []string, reason string) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"closed": closedIDs,
			"count":  len(closedIDs),
			"reason": reason,
		})
	}
	for _, id := range closedIDs {
		fmt.Printf("  - %s\n", id)
	}
	fmt.Printf("✓ Closed %d epic(s) with reason %q\n", len(closedIDs), reason)
	return nil
}

func init() {
	epicCmd.AddCommand(epicStatusCmd)
	epicCmd.AddCommand(closeEligibleEpicsCmd)
	epicStatusCmd.Flags().Bool("eligible-only", false, "Show only epics eligible for closure")
	closeEligibleEpicsCmd.Flags().Bool("dry-run", false, "Preview what would be closed without making changes")
	closeEligibleEpicsCmd.Flags().StringP("reason", "r", defaultCloseEligibleReason, "Close reason applied to every epic closed")
	rootCmd.AddCommand(epicCmd)
}
