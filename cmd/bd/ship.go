package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

var shipCmd = &cobra.Command{
	Use:   "ship <capability>",
	Short: "Publish a capability for cross-project dependencies",
	Long: `Ship a capability to satisfy cross-project dependencies.

This command:
  1. Finds issue with export:<capability> label
  2. Validates issue is closed (or --force to override)
  3. Adds provides:<capability> label

External projects can depend on this capability using:
  bd dep add <issue> external:<project>:<capability>

The capability is resolved when the external project has a closed issue
with the provides:<capability> label.

Examples:
  bd ship mol-run-assignee              # Ship the mol-run-assignee capability
  bd ship mol-run-assignee --force      # Ship even if issue is not closed
  bd ship mol-run-assignee --dry-run    # Preview without making changes`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runShip,
}

func runShip(cmd *cobra.Command, args []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("ship is not supported in proxied-server mode")
	}
	if err := CheckReadonly("ship"); err != nil {
		return err
	}
	evt := metrics.NewCommandEvent("ship")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	capability := args[0]
	force, _ := cmd.Flags().GetBool("force")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	issue, err := loadShipIssue(getRootContext(), capability, force)
	if err != nil {
		return err
	}
	providesLabel := "provides:" + capability
	already, err := shipAlreadyProvides(getRootContext(), issue.ID, providesLabel)
	if err != nil {
		return err
	}
	if already {
		return printShipAlready(capability, issue.ID)
	}
	if dryRun {
		return printShipDryRun(capability, issue.ID, providesLabel)
	}
	return shipCapability(getRootContext(), capability, issue.ID, providesLabel)
}

func loadShipIssue(ctx context.Context, capability string, force bool) (*types.Issue, error) {
	exportLabel := "export:" + capability
	issues, err := getStore().GetIssuesByLabel(ctx, exportLabel)
	if err != nil {
		return nil, HandleErrorRespectJSON("listing issues: %v", err)
	}
	if len(issues) == 0 {
		return nil, HandleErrorWithHintRespectJSON(
			fmt.Sprintf("no issue found with label '%s'", exportLabel),
			fmt.Sprintf("add the label first: bd label add <issue-id> %s", exportLabel))
	}
	if len(issues) > 1 {
		fmt.Fprintf(os.Stderr, "Error: multiple issues found with label '%s':\n", exportLabel)
		for _, issue := range issues {
			fmt.Fprintf(os.Stderr, "  %s: %s (%s)\n", issue.ID, issue.Title, issue.Status)
		}
		return nil, HandleErrorRespectJSON("only one issue should have this label")
	}
	issue := issues[0]
	if issue.Status != types.StatusClosed && !force {
		return nil, HandleErrorWithHintRespectJSON(
			fmt.Sprintf("issue %s is not closed (status: %s)", issue.ID, issue.Status),
			"close the issue first, or use --force to override")
	}
	return issue, nil
}

func shipAlreadyProvides(ctx context.Context, issueID, providesLabel string) (bool, error) {
	labels, err := getStore().GetLabels(ctx, issueID)
	if err != nil {
		return false, HandleErrorRespectJSON("getting labels: %v", err)
	}
	for _, l := range labels {
		if l == providesLabel {
			return true, nil
		}
	}
	return false, nil
}

func printShipAlready(capability, issueID string) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"status":     "already_shipped",
			"capability": capability,
			"issue_id":   issueID,
		})
	}
	fmt.Printf("%s Capability '%s' already shipped (%s)\n", ui.RenderPass("✓"), capability, issueID)
	return nil
}

func printShipDryRun(capability, issueID, providesLabel string) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"status":     "dry_run",
			"capability": capability,
			"issue_id":   issueID,
			"would_add":  providesLabel,
		})
	}
	fmt.Printf("%s Would ship '%s' on %s (dry run)\n", ui.RenderAccent("→"), capability, issueID)
	return nil
}

func shipCapability(ctx context.Context, capability, issueID, providesLabel string) error {
	if err := getStore().AddLabel(ctx, issueID, providesLabel, getActor()); err != nil {
		return HandleErrorRespectJSON("adding label: %v", err)
	}
	commandDidWrite.Store(true)
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"status":     "shipped",
			"capability": capability,
			"issue_id":   issueID,
			"label":      providesLabel,
		})
	}
	fmt.Printf("%s Shipped %s (%s)\n", ui.RenderPass("✓"), capability, issueID)
	fmt.Printf("  Added label: %s\n", providesLabel)
	fmt.Printf("\nExternal projects can now depend on: external:%s:%s\n", "<this-project>", capability)
	return nil
}

func init() {
	shipCmd.Flags().Bool("force", false, "Ship even if issue is not closed")
	shipCmd.Flags().Bool("dry-run", false, "Preview without making changes")

	rootCmd.AddCommand(shipCmd)
}
