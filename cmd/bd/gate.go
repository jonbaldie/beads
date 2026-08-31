package main

import (
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/spf13/cobra"
)

// gateCmd is the parent command for gate operations
var gateCmd = &cobra.Command{
	Use:     "gate",
	GroupID: "issues",
	Short:   "Manage async coordination gates",
	Long: `Gates are async wait conditions that block workflow steps.

Gates are created automatically when a formula step has a gate field.
They must be closed (manually or via watchers) for the blocked step to proceed.

Gate types:
  human   - Requires manual bd close (Phase 1)
  timer   - Expires after timeout (Phase 2)
  gh:run  - Waits for GitHub workflow (Phase 3)
  gh:pr   - Waits for PR merge (Phase 3)
  bead    - Waits for another bead to close (Phase 4)

For bead gates, await_id is a bead ID in this rig's database (e.g., "bd-abc123").
The historical cross-rig form <rig>:<bead-id> can no longer be evaluated
(multi-rig routing removed) and stays pending until resolved manually.

Examples:
  bd gate list           # Show all open gates
  bd gate list --all     # Show all gates including closed
  bd gate check          # Evaluate all open gates
  bd gate check --type=bead  # Evaluate only bead gates
  bd gate resolve <id>   # Close a gate manually`,
}

// gateListCmd lists gate issues
var gateListCmd = &cobra.Command{
	Use:   "list [issue-id]",
	Short: "List gate issues",
	Long: `List gate issues.

With no argument, lists all gate issues in the current beads database.
With an [issue-id] argument, lists ONLY the gates that block that issue
(its own dependency gates) — not every gate in the database.

By default, shows only open gates. Use --all to include closed gates.`,
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("gate-list")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if usesProxiedServer() {
			return runGateListProxiedServer(cmd, getRootContext(), args)
		}

		allFlag, _ := cmd.Flags().GetBool("all")
		limit, _ := cmd.Flags().GetInt("limit")

		ctx := getRootContext()

		// Bead-scoped: list only the gates that block this specific issue
		// (its dependency gates), never the whole database. Without this an
		// issue-id argument was silently ignored and the DB-wide list was
		// returned, which could lead a caller to act on unrelated gates.
		if len(args) == 1 {
			target, err := getStore().GetIssue(ctx, args[0])
			if err != nil {
				return HandleErrorRespectJSON("issue not found: %s", args[0])
			}
			deps, err := getStore().GetDependencies(ctx, target.ID)
			if err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
			gates := filterIssueGates(deps, allFlag, limit)
			if isJSONOutput() {
				return outputJSON(gates)
			}
			displayGates(gates, allFlag)
			return nil
		}

		gateType := types.IssueType("gate")
		filter := types.IssueFilter{
			IssueFilterCore: types.IssueFilterCore{
				IssueType: &gateType,
				Limit:     limit,
			},
		}

		if !allFlag {
			filter.ExcludeStatus = []types.Status{types.StatusClosed}
		}

		issues, err := getStore().SearchIssues(ctx, "", filter)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}

		if isJSONOutput() {
			return outputJSON(issues)
		}

		displayGates(issues, allFlag)
		return nil
	},
}
