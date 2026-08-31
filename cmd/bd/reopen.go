package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/issueops"
	"github.com/spf13/cobra"
)

func newReopenCmd() *cobra.Command {
	reopenCmd := &cobra.Command{
		Use:     "reopen [id...]",
		GroupID: "issues",
		Short:   "Reopen one or more closed issues",
		Long: `Reopen closed issues by setting status to 'open' and clearing the closed_at timestamp.
This is more explicit than 'bd update --status open' and emits a Reopened event.`,
		Args:          cobra.MinimumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runReopen,
	}
	reopenCmd.Flags().StringP("reason", "r", "", "Reason for reopening")
	reopenCmd.ValidArgsFunction = issueIDCompletion
	return reopenCmd
}

type reopenState struct {
	reason       string
	reopened     []*types.Issue
	mutated      map[storage.DoltStorage][]string
	pendingClose []*RoutedResult
	hasError     bool
}

func runReopen(cmd *cobra.Command, args []string) error {
	if err := CheckReadonly("reopen"); err != nil {
		return err
	}
	evt := metrics.NewCommandEvent("reopen")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	if usesProxiedServer() {
		return runReopenProxiedServer(cmd, getRootContext(), args)
	}
	reason, _ := cmd.Flags().GetString("reason")
	return runReopenDirect(args, reason)
}

func runReopenDirect(ids []string, reason string) error {
	if getStore() == nil {
		return HandleErrorWithHint("database not initialized", diagHint())
	}
	opsCtx, err := issueOpsContext(getRootContext())
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	state := reopenState{reason: reason, mutated: map[storage.DoltStorage][]string{}}
	for _, id := range ids {
		if err := reopenOne(opsCtx, id, &state); err != nil {
			fmt.Fprintf(os.Stderr, "Error %v\n", err)
			state.hasError = true
		}
	}
	if err := commitReopenedIssues(&state); err != nil {
		return err
	}
	return reportReopenedIssues(&state)
}

func reportReopenedIssues(state *reopenState) error {
	if isJSONOutput() && len(state.reopened) > 0 {
		if err := outputJSON(state.reopened); err != nil {
			return err
		}
	}
	if state.hasError {
		return SilentExit()
	}
	return nil
}

func reopenOne(opsCtx context.Context, id string, state *reopenState) error {
	result, err := resolveAndGetIssueForMutation(getRootContext(), getStore(), id)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", id, err)
	}
	if result.Issue.Status == types.StatusOpen {
		fmt.Fprintln(os.Stderr, reopenNoOpMessage(result.ResolvedID, types.StatusOpen))
		result.Close()
		return nil
	}
	ops, err := writeOps(result.Store)
	if err != nil {
		result.Close()
		return fmt.Errorf("reopening %s: %w", result.ResolvedID, err)
	}
	reopened, err := ops.Reopen(opsCtx, issueops.ReopenRequest{
		Actor: getActor(), IssueID: result.ResolvedID, Reason: state.reason,
		Provenance: "bd: reopen " + result.ResolvedID,
	})
	if err != nil {
		result.Close()
		return fmt.Errorf("reopening %s: %w", result.ResolvedID, err)
	}
	if !reopened.Changed {
		fmt.Fprintln(os.Stderr, reopenNoOpMessage(result.ResolvedID, reopenStatusOf(reopened.Issue, result.Issue)))
		result.Close()
		return nil
	}
	recordReopenedIssue(result, reopened.Issue, state)
	return nil
}

func recordReopenedIssue(result *RoutedResult, issue *types.Issue, state *reopenState) {
	state.mutated[result.Store] = append(state.mutated[result.Store], result.ResolvedID)
	state.pendingClose = append(state.pendingClose, result)
	if isJSONOutput() {
		if issue != nil {
			issue.Dependencies = nil
			state.reopened = append(state.reopened, issue)
		}
		return
	}
	reasonMessage := ""
	if state.reason != "" {
		reasonMessage = ": " + state.reason
	}
	fmt.Printf("%s Reopened %s%s\n", ui.RenderAccent("↻"), result.ResolvedID, reasonMessage)
}

func commitReopenedIssues(state *reopenState) error {
	defer func() {
		for _, result := range state.pendingClose {
			result.Close()
		}
	}()
	for issueStore, ids := range state.mutated {
		if err := commitPendingIfEmbedded(getRootContext(), issueStore, getActor(), doltAutoCommitParams{
			Command: "reopen", IssueIDs: ids,
		}); err != nil {
			return HandleErrorRespectJSON("failed to commit: %v", err)
		}
	}
	return nil
}

// reopenNoOpMessage is what either route says when the role reported no change.
// Both lines live here so the two front doors cannot drift apart on the way a
// reopen that did nothing reads: the proxied route has no pre-read to
// short-circuit on, so it reaches the already-open case through the same result
// the direct route reaches the non-done case through.
func reopenNoOpMessage(id string, status types.Status) string {
	if status == types.StatusOpen {
		return fmt.Sprintf("%s is already open", id)
	}
	return fmt.Sprintf("%s is not closed (status: %s); nothing to do", id, status)
}

// reopenStatusOf reports the status a no-op reopen left in place, preferring
// the operation's post-state snapshot over the pre-read it was based on.
func reopenStatusOf(post, pre *types.Issue) types.Status {
	if post != nil {
		return post.Status
	}
	if pre != nil {
		return pre.Status
	}
	return ""
}

func init() {
	rootCmd.AddCommand(newReopenCmd())
}
