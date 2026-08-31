package main

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/validation"
	"github.com/spf13/cobra"
)

func newPriorityCmd() *cobra.Command {
	priorityCmd := &cobra.Command{
		Use:     "priority <id> <n>",
		GroupID: "issues",
		Short:   "Set the priority of an issue",
		Long: `Set the priority of an issue.

Shorthand for 'bd update <id> --priority <n>'.

Priority levels:
  0 - Critical (security, data loss, broken builds)
  1 - High (major features, important bugs)
  2 - Medium (default)
  3 - Low (polish, optimization)
  4 - Backlog (future ideas)

Examples:
  bd priority bd-123 0    # Critical
  bd priority bd-123 2    # Medium`,
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runPriority,
	}
	priorityCmd.ValidArgsFunction = issueIDCompletion
	return priorityCmd
}

func runPriority(_ *cobra.Command, args []string) error {
	if err := CheckReadonly("priority"); err != nil {
		return err
	}
	evt := metrics.NewCommandEvent("priority")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	if usesProxiedServer() {
		return runPriorityProxiedServer(getRootContext(), args)
	}
	priority, err := validation.ValidatePriority(args[1])
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	return withMutationIssue(getRootContext(), getStore(), args[0], func(result *RoutedResult) error {
		return applyPriority(getRootContext(), args[0], priority, result)
	})
}

func applyPriority(ctx context.Context, id string, priority int, result *RoutedResult) error {
	if err := validateIssueUpdatable(id, result.Issue); err != nil {
		return HandleErrorRespectJSON("%s", err)
	}
	if err := result.Store.UpdateIssue(ctx, result.ResolvedID, map[string]interface{}{"priority": priority}, getActor()); err != nil {
		return HandleErrorRespectJSON("updating %s: %v", id, err)
	}
	if err := commitPendingIfEmbedded(ctx, result.Store, getActor(), doltAutoCommitParams{
		Command: "priority", IssueIDs: []string{result.ResolvedID},
	}); err != nil {
		return HandleErrorRespectJSON("failed to commit: %v", err)
	}
	SetLastTouchedID(result.ResolvedID)
	updated, _ := result.Store.GetIssue(ctx, result.ResolvedID)
	if isJSONOutput() {
		if updated != nil {
			return outputJSON(updated)
		}
		return nil
	}
	fmt.Printf("%s Set priority of %s to P%d\n", ui.RenderPass("✓"), formatFeedbackID(result.ResolvedID, issueTitleOrEmpty(updated)), priority)
	return nil
}

func init() {
	rootCmd.AddCommand(newPriorityCmd())
}
