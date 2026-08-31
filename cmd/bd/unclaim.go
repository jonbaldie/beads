package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

func newUnclaimCmd() *cobra.Command {
	unclaimCmd := &cobra.Command{
		Use:           "unclaim [id...]",
		GroupID:       "issues",
		Short:         "Release a claimed issue",
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `Release a claimed issue by clearing the assignee and resetting status to 'open'.

Use this when an agent crashes mid-work or you need to abandon a claimed task.
The issue becomes available for re-claiming by other agents.

Only the current assignee can release its own claim. Releasing another
actor's claim requires --force and should be coordinated with the holder
first — their claim may be live even if the issue looks idle. Prefer
letting lease expiry reclaim genuinely abandoned work.

With --if-assignee, the release is an atomic compare-and-swap (the inverse of
claim): the issue is released only while it is still assigned to the given
assignee. If the holder differs — e.g. the claim was already reclaimed and
re-taken by another worker — nothing is changed and bd exits nonzero with an
error naming the current holder. Use this from supervisors that must return a
specific worker's issue without ever clobbering someone else's live claim.
--if-assignee requires a non-empty assignee and cannot be combined with --force
(they encode contradictory intent).

Exit status: 0 when every issue was released; 1 when any release failed
(including an --if-assignee mismatch).

Examples:
  bd unclaim bd-123
  bd unclaim bd-123 --reason "Agent crashed"
  bd unclaim bd-123 bd-456
  bd unclaim bd-123 --if-assignee worker-7   # only if still held by worker-7`,
		Args: cobra.MinimumNArgs(1),
		RunE: runUnclaim,
	}
	unclaimCmd.Flags().StringP("reason", "r", "", "Reason for unclaiming")
	unclaimCmd.Flags().Bool("force", false, "Release the claim even if held by a different actor (admin/reaper use)")
	unclaimCmd.Flags().String("if-assignee", "", "Only release if still assigned to this assignee (atomic compare-and-swap; exits nonzero without changing the issue when the holder differs)")
	// --force (unconditional bypass of the ownership check) and --if-assignee
	// (release only while a specific assignee still holds it) encode
	// contradictory intent. Rejecting the combination stops a reaper script that
	// habitually passes --force from silently dropping it when it also passes
	// --if-assignee for one case.
	unclaimCmd.MarkFlagsMutuallyExclusive("force", "if-assignee")
	unclaimCmd.ValidArgsFunction = issueIDCompletion
	return unclaimCmd
}

type unclaimFlags struct {
	reason      string
	force       bool
	ifAssignee  string
	conditional bool
}

func readUnclaimFlags(cmd *cobra.Command) (unclaimFlags, error) {
	flags := unclaimFlags{}
	flags.reason, _ = cmd.Flags().GetString("reason")
	flags.force, _ = cmd.Flags().GetBool("force")
	flags.ifAssignee, _ = cmd.Flags().GetString("if-assignee")
	flags.conditional = cmd.Flags().Changed("if-assignee")
	if flags.conditional && flags.ifAssignee == "" {
		return flags, HandleErrorRespectJSON("--if-assignee requires a non-empty assignee; it releases the issue only while that assignee still holds it")
	}
	return flags, nil
}

func runUnclaim(cmd *cobra.Command, args []string) error {
	flags, err := readUnclaimFlags(cmd)
	if err != nil {
		return err
	}
	if err := CheckReadonly("unclaim"); err != nil {
		return err
	}
	if usesProxiedServer() {
		expectedAssignee := ""
		if flags.conditional {
			expectedAssignee = flags.ifAssignee
		}
		return runUnclaimProxiedServer(getRootContext(), args, flags.reason, flags.force, expectedAssignee)
	}
	return runUnclaimDirect(args, flags)
}

func runUnclaimDirect(ids []string, flags unclaimFlags) error {
	if getStore() == nil {
		return HandleErrorWithHint("database not initialized", diagHint())
	}
	unclaimedIssues := []*types.Issue{}
	hasError := false
	for _, id := range ids {
		issue, err := unclaimOne(id, flags)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error %v\n", err)
			hasError = true
			continue
		}
		if issue != nil {
			unclaimedIssues = append(unclaimedIssues, issue)
		}
	}
	commandDidWrite.Store(true)
	if isJSONOutput() && len(unclaimedIssues) > 0 {
		if err := outputJSON(unclaimedIssues); err != nil {
			return HandleError("%v", err)
		}
	}
	if hasError {
		return SilentExit()
	}
	return nil
}

func unclaimOne(id string, flags unclaimFlags) (*types.Issue, error) {
	result, err := resolveAndGetIssueWithRouting(getRootContext(), getStore(), id)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", id, err)
	}
	defer result.Close()
	if err := releaseClaim(result.Store, result.ResolvedID, flags); err != nil {
		return nil, fmt.Errorf("unclaiming %s: %w", result.ResolvedID, err)
	}
	if flags.reason != "" {
		if _, err := result.Store.AddIssueComment(getRootContext(), result.ResolvedID, getActor(), flags.reason); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to add reason comment: %v\n", err)
		}
	}
	if isJSONOutput() {
		issue, _ := result.Store.GetIssue(getRootContext(), result.ResolvedID)
		return issue, nil
	}
	reasonMessage := ""
	if flags.reason != "" {
		reasonMessage = ": " + flags.reason
	}
	fmt.Printf("%s Unclaimed %s%s\n", ui.RenderPass("✓"), result.ResolvedID, reasonMessage)
	return nil, nil
}

func releaseClaim(issueStore interface {
	UnclaimIssue(ctx context.Context, id, actor string, force bool) error
	UnclaimIssueIfAssignee(ctx context.Context, id, actor, expectedAssignee string) error
}, id string, flags unclaimFlags) error {
	if flags.conditional {
		return issueStore.UnclaimIssueIfAssignee(getRootContext(), id, getActor(), flags.ifAssignee)
	}
	return issueStore.UnclaimIssue(getRootContext(), id, getActor(), flags.force)
}

func init() {
	rootCmd.AddCommand(newUnclaimCmd())
}
