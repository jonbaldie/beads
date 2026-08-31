package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

func newUndeferCmd() *cobra.Command {
	undeferCmd := &cobra.Command{
		Use:   "undefer [id...]",
		Short: "Undefer one or more issues (restore to open)",
		Long: `Undefer issues to restore them to open status.

This brings issues back from the icebox so they can be worked on again.
Issues will appear in 'bd ready' if they have no blockers.

Examples:
  bd undefer bd-abc        # Undefer a single issue
  bd undefer bd-abc bd-def # Undefer multiple issues`,
		Args:          cobra.MinimumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runUndefer,
	}
	undeferCmd.ValidArgsFunction = issueIDCompletion
	return undeferCmd
}

func runUndefer(_ *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("undefer")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	if err := CheckReadonly("undefer"); err != nil {
		return err
	}
	if usesProxiedServer() {
		return runUndeferProxiedServer(getRootContext(), args)
	}
	return runDirectUndefer(getRootContext(), args)
}

func runDirectUndefer(ctx context.Context, args []string) error {
	if _, err := utils.ResolvePartialIDs(ctx, getStore(), args); err != nil {
		return HandleError("%v", err)
	}
	if getStore() == nil {
		return HandleErrorWithHint("database not initialized", diagHint())
	}

	undeferredIssues := make([]*types.Issue, 0, len(args))
	for _, id := range args {
		issue, ok := applyUndefer(ctx, id)
		if ok && issue != nil {
			undeferredIssues = append(undeferredIssues, issue)
		}
	}
	if len(args) > 0 {
		commandDidWrite.Store(true)
	}
	if isJSONOutput() && len(undeferredIssues) > 0 {
		return outputJSON(undeferredIssues)
	}
	return nil
}

func applyUndefer(ctx context.Context, id string) (*types.Issue, bool) {
	fullID, err := utils.ResolvePartialID(ctx, getStore(), id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving %s: %v\n", id, err)
		return nil, false
	}
	issue, err := getStore().GetIssue(ctx, fullID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting %s: %v\n", fullID, err)
		return nil, false
	}
	if issue == nil || issue.Status != types.StatusDeferred {
		if issue == nil {
			fmt.Fprintf(os.Stderr, "Issue %s not found\n", fullID)
		} else {
			fmt.Fprintf(os.Stderr, "%s is not deferred (status: %s)\n", fullID, string(issue.Status))
		}
		return nil, false
	}
	if err := getStore().UpdateIssue(ctx, fullID, map[string]interface{}{
		"status":      string(types.StatusOpen),
		"defer_until": nil,
	}, getActor()); err != nil {
		fmt.Fprintf(os.Stderr, "Error undeferring %s: %v\n", fullID, err)
		return nil, false
	}
	if isJSONOutput() {
		updated, _ := getStore().GetIssue(ctx, fullID)
		return updated, true
	}
	fmt.Printf("%s Undeferred %s (now open)\n", ui.RenderPass("*"), fullID)
	return nil, true
}

func init() {
	rootCmd.AddCommand(newUndeferCmd())
}
