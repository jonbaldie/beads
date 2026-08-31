package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

// promotionComment is the audit comment both routes record on the promoted
// bead. Shared by the classic and proxied-server paths so the text cannot
// drift.
func promotionComment(reason string) string {
	comment := "Promoted from Level 0"
	if reason != "" {
		comment += ": " + reason
	}
	return comment
}

// printPromoteResult renders the `bd promote` success output. updated is the
// best-effort post-promote re-read used only by --json (it may be nil, in
// which case the JSON path prints nothing, matching the classic behavior).
// Shared by the classic and proxied-server paths so the output shape cannot
// drift.
func printPromoteResult(fullID string, updated *types.Issue) error {
	if isJSONOutput() {
		if updated != nil {
			return outputJSON(updated)
		}
		return nil
	}
	fmt.Printf("%s Promoted %s to permanent bead\n", ui.RenderPass("✓"), fullID)
	return nil
}

func newPromoteCmd() *cobra.Command {
	promoteCmd := &cobra.Command{
		Use:     "promote <wisp-id>",
		GroupID: "issues",
		Short:   "Promote a wisp to a permanent bead",
		Long: `Promote a wisp (ephemeral issue) to a permanent bead.

This copies the issue from the wisps table (dolt_ignored) to the permanent
issues table (Dolt-versioned), preserving labels, dependencies, events, and
comments. The original ID is preserved so all links keep working.

A comment is added recording the promotion and optional reason.

Examples:
  bd promote bd-wisp-abc123
  bd promote bd-wisp-abc123 --reason "Worth tracking long-term"`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runPromote,
	}
	promoteCmd.Flags().StringP("reason", "r", "", "Reason for promotion")
	promoteCmd.ValidArgsFunction = issueIDCompletion
	return promoteCmd
}

func runPromote(cmd *cobra.Command, args []string) error {
	if err := CheckReadonly("promote"); err != nil {
		return err
	}
	evt := metrics.NewCommandEvent("promote")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	reason, _ := cmd.Flags().GetString("reason")
	if usesProxiedServer() {
		return runPromoteProxiedServer(getRootContext(), args[0], reason)
	}
	return runDirectPromote(getRootContext(), args[0], reason)
}

func runDirectPromote(ctx context.Context, id, reason string) error {
	if getStore() == nil {
		return HandleErrorWithHint("database not initialized", diagHint())
	}
	fullID, err := utils.ResolvePartialID(ctx, getStore(), id)
	if err != nil {
		return HandleErrorRespectJSON("resolving %s: %v", id, err)
	}
	issue, err := loadPromotableIssue(ctx, fullID)
	if err != nil {
		return err
	}
	if !issue.Ephemeral {
		return HandleErrorRespectJSON("%s is not a wisp (already persistent)", fullID)
	}
	if err := getStore().PromoteFromEphemeral(ctx, fullID, getActor()); err != nil {
		return HandleErrorRespectJSON("promoting %s: %v", fullID, err)
	}
	if err := getStore().AddComment(ctx, fullID, getActor(), promotionComment(reason)); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to add promotion comment to %s: %v\n", fullID, err)
	}
	commandDidWrite.Store(true)
	return renderDirectPromote(ctx, fullID)
}

func loadPromotableIssue(ctx context.Context, fullID string) (*types.Issue, error) {
	issue, err := getStore().GetIssue(ctx, fullID)
	if err == nil {
		return issue, nil
	}
	if errors.Is(err, storage.ErrNotFound) {
		return nil, HandleErrorRespectJSON("issue %s not found", fullID)
	}
	return nil, HandleErrorRespectJSON("getting issue %s: %v", fullID, err)
}

func renderDirectPromote(ctx context.Context, fullID string) error {
	var updated *types.Issue
	if isJSONOutput() {
		updated, _ = getStore().GetIssue(ctx, fullID)
	}
	return printPromoteResult(fullID, updated)
}

func init() {
	rootCmd.AddCommand(newPromoteCmd())
}
