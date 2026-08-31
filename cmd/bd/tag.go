package main

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

func newTagCmd() *cobra.Command {
	tagCmd := &cobra.Command{
		Use:     "tag <id> <label>",
		GroupID: "issues",
		Short:   "Add a label to an issue",
		Long: `Add a label to an issue.

Shorthand for 'bd update <id> --add-label <label>'.

Examples:
  bd tag bd-123 bug
  bd tag bd-123 needs-review`,
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runTag,
	}
	tagCmd.ValidArgsFunction = issueIDCompletion
	return tagCmd
}

func runTag(_ *cobra.Command, args []string) error {
	if err := CheckReadonly("tag"); err != nil {
		return err
	}
	evt := metrics.NewCommandEvent("tag")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	if usesProxiedServer() {
		return runTagProxiedServer(getRootContext(), args)
	}
	return withMutationIssue(getRootContext(), getStore(), args[0], func(result *RoutedResult) error {
		return applyTag(getRootContext(), args[0], args[1], result)
	})
}

func applyTag(ctx context.Context, id, label string, result *RoutedResult) error {
	if err := validateIssueUpdatable(id, result.Issue); err != nil {
		return HandleErrorRespectJSON("%s", err)
	}
	if err := result.Store.AddLabel(ctx, result.ResolvedID, label, getActor()); err != nil {
		return HandleErrorRespectJSON("adding label to %s: %v", id, err)
	}
	if err := commitPendingIfEmbedded(ctx, result.Store, getActor(), doltAutoCommitParams{
		Command: "tag", IssueIDs: []string{result.ResolvedID},
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
	fmt.Printf("%s Added label %q to %s\n", ui.RenderPass("✓"), label, formatFeedbackID(result.ResolvedID, issueTitleOrEmpty(updated)))
	return nil
}

func init() {
	rootCmd.AddCommand(newTagCmd())
}
