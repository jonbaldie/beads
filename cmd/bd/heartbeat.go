package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/ui"
)

func newHeartbeatCmd() *cobra.Command {
	heartbeatCmd := &cobra.Command{
		Use:     "heartbeat <id>",
		Aliases: []string{"hb"},
		GroupID: "issues",
		Short:   "Refresh the lease on an issue you hold in_progress",
		Long: `Refresh the lease on an issue you currently hold in_progress.

A claim carries a lease that expires after a TTL. A worker keeps its claim alive
by heartbeating faster than the TTL; once it stops (because it died), the lease
goes stale and 'bd reclaim' reverts the issue to ready so another worker can pick
it up. Heartbeat pushes lease_expires_at forward and stamps heartbeat_at = now.

Only the current owner may heartbeat. If the lease has already been reclaimed or
the issue closed, heartbeat fails so the worker learns to stop.

Leases live in an ephemeral, node-local table: heartbeats write no Dolt commit
and no history, so any cadence comfortably below the TTL is fine. Leases are
only enforceable on the node that granted them; cross-machine claim visibility
rides the issue's status and assignee, which do commit.

Examples:
  bd heartbeat bd-123
  bd hb bd-123`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runHeartbeat,
	}
	heartbeatCmd.ValidArgsFunction = issueIDCompletion
	return heartbeatCmd
}

func runHeartbeat(_ *cobra.Command, args []string) error {
	if err := CheckReadonly("heartbeat"); err != nil {
		return err
	}
	evt := metrics.NewCommandEvent("heartbeat")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	if usesProxiedServer() {
		return runHeartbeatProxiedServer(getRootContext(), args[0])
	}
	return withMutationIssue(getRootContext(), getStore(), args[0], func(result *RoutedResult) error {
		return applyHeartbeat(getRootContext(), result)
	})
}

func applyHeartbeat(ctx context.Context, result *RoutedResult) error {
	if err := result.Store.HeartbeatIssue(ctx, result.ResolvedID, getActor()); err != nil {
		return HandleErrorRespectJSON("heartbeat %s: %v", result.ResolvedID, err)
	}
	if err := commitPendingIfEmbedded(ctx, result.Store, getActor(), doltAutoCommitParams{
		Command: "heartbeat", IssueIDs: []string{result.ResolvedID},
	}); err != nil {
		return HandleErrorRespectJSON("failed to commit: %v", err)
	}
	SetLastTouchedID(result.ResolvedID)
	return renderHeartbeatSuccess(result.ResolvedID, result.Issue.Title)
}

// renderHeartbeatSuccess prints the success shape both storage routes share —
// the classic path and the proxied-server path (heartbeat_proxied_server.go)
// call exactly this code, so the --json contract workers parse
// ({"id","status":"heartbeat","owner"}) cannot drift between modes.
func renderHeartbeatSuccess(id, title string) error {
	if isJSONOutput() {
		return outputJSON(map[string]string{
			"id":     id,
			"status": "heartbeat",
			"owner":  getActor(),
		})
	}
	fmt.Printf("%s Heartbeat %s (lease refreshed)\n", ui.RenderPass("✓"), formatFeedbackID(id, title))
	return nil
}

func init() {
	rootCmd.AddCommand(newHeartbeatCmd())
}
