package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
)

func newAssignCmd() *cobra.Command {
	assignCmd := &cobra.Command{
		Use:     "assign <id> <name>",
		GroupID: "issues",
		Short:   "Assign an issue to someone",
		Long: `Assign an issue to someone.

Shorthand for 'bd update <id> --assignee <name>'.

Refuses to overwrite another actor's live in_progress claim without --force
(bd-98s5c); issues assigned to a claim.pools alias are exempt, matching
--claim. For a holder-aware transfer prefer
'bd update <id> --if-assignee <holder> -a <new>'.

Examples:
  bd assign bd-123 alice
  bd assign bd-123 ""      # unassign`,
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runAssign,
	}
	assignCmd.Flags().Bool("force", false, "Allow overwriting another actor's live in_progress claim (use only for abandoned claims — crashed agent, expired lease; prefer bd reclaim)")
	assignCmd.ValidArgsFunction = issueIDCompletion
	return assignCmd
}

func runAssign(cmd *cobra.Command, args []string) error {
	if err := CheckReadonly("assign"); err != nil {
		return err
	}
	evt := metrics.NewCommandEvent("assign")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	force, _ := cmd.Flags().GetBool("force")
	if usesProxiedServer() {
		return runAssignProxiedServer(getRootContext(), args, force)
	}
	return withMutationIssue(getRootContext(), getStore(), args[0], func(result *RoutedResult) error {
		return applyAssign(getRootContext(), args[0], args[1], force, result)
	})
}

func applyAssign(ctx context.Context, id, assignee string, force bool, result *RoutedResult) error {
	if err := validateIssueUpdatable(id, result.Issue); err != nil {
		return HandleErrorRespectJSON("%s", err)
	}
	if err := validateIssueReassignable(id, result.Issue, getActor(), assignee,
		storeClaimPoolAliases(ctx, result.Store), force); err != nil {
		return HandleErrorRespectJSON("%s", err)
	}
	if err := result.Store.UpdateIssue(ctx, result.ResolvedID, map[string]interface{}{"assignee": assignee}, getActor()); err != nil {
		return HandleErrorRespectJSON("updating %s: %v", id, err)
	}
	if err := commitPendingIfEmbedded(ctx, result.Store, getActor(), doltAutoCommitParams{
		Command: "assign", IssueIDs: []string{result.ResolvedID},
	}); err != nil {
		return HandleErrorRespectJSON("failed to commit: %v", err)
	}
	SetLastTouchedID(result.ResolvedID)
	updated, _ := result.Store.GetIssue(ctx, result.ResolvedID)
	return renderAssignSuccess(result.ResolvedID, assignee, updated)
}

func renderAssignSuccess(id, assignee string, updated *types.Issue) error {
	if isJSONOutput() {
		if updated != nil {
			return outputJSON(updated)
		}
		return nil
	}
	title := issueTitleOrEmpty(updated)
	if assignee == "" {
		fmt.Printf("%s Unassigned %s\n", ui.RenderPass("✓"), formatFeedbackID(id, title))
		return nil
	}
	fmt.Printf("%s Assigned %s to %s\n", ui.RenderPass("✓"), formatFeedbackID(id, title), assignee)
	return nil
}

func init() {
	rootCmd.AddCommand(newAssignCmd())
}
