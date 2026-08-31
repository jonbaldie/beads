package main

import (
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

var readyCmd = &cobra.Command{
	Use:   "ready",
	Short: "Show ready work (open, no active blockers)",
	Long: `Show ready work (open issues with no active blockers).

Excludes in_progress, blocked, deferred, and hooked issues. This uses the
GetReadyWork API which applies blocker-aware semantics to find truly claimable work.

Note: 'bd list --ready' uses the same blocker-aware ready-work semantics.

Use --mol to filter to a specific molecule's steps:
  bd ready --mol bd-patrol   # Show ready steps within molecule

Use --gated to find molecules ready for gate-resume dispatch:
  bd ready --gated           # Find molecules where a gate closed

Use --claim to atomically claim the first ready issue matching the filters:
  bd ready --claim --json

This is useful for agents executing molecules to see which steps can run next.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("ready")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		claimReady, _ := cmd.Flags().GetBool("claim")

		// ABOVE THE MODE DISPATCH, and above the proxied branch, so it is the
		// one place a --brief conflict is decided for this command on either
		// route. The branches below return before gatherReadyInput runs, so a
		// check written into each of them would be three untested copies; this
		// is one call into the body the gatherer also uses.
		if err := briefModeConflictFromFlags(cmd); err != nil {
			return err
		}

		if usesProxiedServer() {
			// --claim consumes exactly one row, same reasoning as the
			// direct-path fix in issueops/claim.go: a rig-wide cap sized
			// for bulk list/ready reads must not block a single-row claim.
			// Only the bulk (non-claim) proxied ready listing rejects an
			// active cap.
			if !claimReady {
				if err := rejectMaxRowsUnderProxiedServer(cmd); err != nil {
					return err
				}
			} else {
				// Still validate --max-rows/BEADS_MAX_ROWS here even though
				// the resolved cap is ignored below: resolveMaxRows is also
				// where a malformed value (e.g. --max-rows -1) is rejected
				// with exit 1, and skipping it entirely for the claim-exempt
				// branch would silently accept a usage error that every
				// other command (direct or proxied) rejects.
				if _, _, err := resolveMaxRows(cmd); err != nil {
					return err
				}
			}
			return runReadyProxiedServer(cmd, getRootContext())
		}

		if offset, _ := cmd.Flags().GetInt("offset"); offset > 0 {
			return HandleErrorRespectJSON("--offset is only supported under --proxied-server")
		}

		gated, _ := cmd.Flags().GetBool("gated")
		if gated {
			if claimReady {
				return HandleErrorRespectJSON("--claim cannot be combined with --gated")
			}
			// Delegate to the non-emitting core so `bd ready --gated` records
			// exactly one cli_command event ("ready"), not also "mol-ready-gated".
			return runMolReadyGatedCore(cmd, args)
		}

		molID, _ := cmd.Flags().GetString("mol")
		if molID != "" {
			if claimReady {
				return HandleErrorRespectJSON("--claim cannot be combined with --mol")
			}
			return runMoleculeReady(cmd, molID)
		}

		explain, _ := cmd.Flags().GetBool("explain")
		if explain {
			if claimReady {
				return HandleErrorRespectJSON("--claim cannot be combined with --explain")
			}
			return runReadyExplain(cmd)
		}

		// The row cap is meaningful on this route alone - the proxied one
		// rejects a live cap outright - so this is the only caller that hands
		// the gatherer a resolver for it.
		in, err := gatherReadyInput(cmd, resolveMaxRows)
		if err != nil {
			return err
		}
		filter := in.filter

		ctx := getRootContext()

		activeStore := getStore()
		if claimReady {
			if err := CheckReadonly("ready --claim"); err != nil {
				return err
			}
		} else {
			routedStore, routed, routingRule, err := openRoutedReadStore(ctx, activeStore)
			if err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
			if routed {
				defer func() { _ = routedStore.Close() }()
				printContributorRoutingNotice(ctx, activeStore, routingRule)
				activeStore = routedStore
			}
		}

		if claimReady {
			// The claim is on the ReadyClaimer role, through the store's own
			// accessor, so selection, the compare-and-set and the hydration
			// that feeds --json all share one transaction. The listing below
			// is not on a role and still builds the filter, for the reasons
			// issueops.Reader's doc comment gives.
			claimer, err := activeStore.ReadyClaimer()
			if err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
			res, err := claimer.ClaimNext(ctx, claimNextRequest(in))
			if err != nil {
				// No handleMaxRowsError here, unlike the listing below: the
				// request carries no cap, so ErrTooManyRows cannot come back.
				// See claimNextRequest for why the cap never applied.
				return HandleErrorRespectJSON("%v", err)
			}
			if res.Claimed == nil {
				if isJSONOutput() {
					return outputJSON([]*types.IssueWithCounts{})
				}
				fmt.Printf("\n%s No ready work to claim\n\n", ui.RenderWarn("○"))
				return nil
			}
			claimed := res.Claimed
			if err := commitPendingIfEmbedded(ctx, activeStore, getActor(), doltAutoCommitParams{
				Command:  "ready",
				IssueIDs: []string{claimed.ID},
			}); err != nil {
				return HandleErrorRespectJSON("failed to commit: %v", err)
			}
			SetLastTouchedID(claimed.ID)
			if isJSONOutput() {
				return outputJSON([]*types.IssueWithCounts{claimed})
			}
			fmt.Printf("%s Claimed issue: %s\n", ui.RenderPass("✓"), formatFeedbackID(claimed.ID, claimed.Title))
			return nil
		}

		if isJSONOutput() {
			results, err := activeStore.GetReadyWorkWithCounts(ctx, filter)
			if err != nil {
				if capErr := handleMaxRowsError(err); capErr != nil {
					return capErr
				}
				return HandleErrorRespectJSON("%v", err)
			}
			totalReady := len(results)
			truncated := false
			if filter.Limit > 0 && len(results) == filter.Limit {
				// The page is full, so there may be more ready work. The
				// ReadyCounter role promises its answer equals
				// len(Reader.Ready(Limit=0).Items), which is what makes this
				// total describe the page above it.
				if n, countErr := readyTotal(ctx, activeStore, in); countErr == nil && n > len(results) {
					totalReady = n
					truncated = true
				}
			}
			if results == nil {
				results = []*types.IssueWithCounts{}
			}
			var pag *PaginationMeta
			if truncated {
				pag = &PaginationMeta{
					Returned:  len(results),
					Total:     totalReady,
					Truncated: true,
				}
			}
			if jerr := outputJSONWithPagination(results, pag); jerr != nil {
				return jerr
			}
			if truncated {
				fmt.Fprintf(os.Stderr, "Showing %d of %d ready issues. Use --limit 0 for all, or --limit N to raise the cap.\n", len(results), totalReady)
			}
			return nil
		}

		issues, err := activeStore.GetReadyWork(ctx, filter)
		if err != nil {
			if capErr := handleMaxRowsError(err); capErr != nil {
				return capErr
			}
			return HandleErrorRespectJSON("%v", err)
		}

		totalReady := len(issues)
		truncated := false
		if filter.Limit > 0 && len(issues) == filter.Limit {
			// The same question the --json branch asks, through the same role,
			// so the "Showing X of N" a human reads and the total a script
			// parses are one number.
			if n, countErr := readyTotal(ctx, activeStore, in); countErr == nil && n > len(issues) {
				totalReady = n
				truncated = true
			}
		}
		maybeShowUpgradeNotification()

		if len(issues) == 0 {
			hasOpenIssues := false
			if stats, statsErr := activeStore.GetStatistics(ctx); statsErr == nil {
				hasOpenIssues = stats.OpenIssues > 0 || stats.InProgressIssues > 0
			}
			printReadyEmptyHuman(hasOpenIssues, hasStoredBlockedStatus(ctx, activeStore.SearchIssues))
			maybeShowTip(getStore())
			return nil
		}
		parentEpicMap := buildParentEpicMap(ctx, activeStore, issues)

		usePlain := in.plainFormat || !in.prettyFormat
		if usePlain {
			fmt.Printf("\n%s Ready work (%d issues with no active blockers):\n\n", ui.RenderAccent("▸"), len(issues))
			for i, issue := range issues {
				fmt.Printf("%d. [%s] [%s] %s: %s\n", i+1,
					ui.RenderPriority(issue.Priority),
					ui.RenderType(string(issue.IssueType)),
					ui.RenderID(issue.ID), issue.Title)
				if issue.EstimatedMinutes != nil {
					fmt.Printf("   Estimate: %d min\n", *issue.EstimatedMinutes)
				}
				if issue.Assignee != "" {
					fmt.Printf("   Assignee: %s\n", issue.Assignee)
				}
			}
			fmt.Println()
		} else {
			displayReadyList(issues, parentEpicMap)
		}

		if truncated {
			fmt.Printf("%s\n\n", ui.RenderMuted(fmt.Sprintf("Showing %d of %d ready issues. Use -n to show more.", len(issues), totalReady)))
		}

		maybeShowTip(getStore())
		return nil
	},
}
var blockedCmd = &cobra.Command{
	Use:           "blocked",
	Short:         "Show blocked issues",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("blocked")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if usesProxiedServer() {
			return runBlockedProxiedServer(cmd, getRootContext())
		}
		// Use global jsonOutput set by PersistentPreRun (respects config.yaml + env vars)
		// Use factory to respect backend configuration (bd-m2jr: SQLite fallback fix)
		ctx := getRootContext()
		parentID, _ := cmd.Flags().GetString("parent")
		var blockedFilter types.WorkFilter
		if parentID != "" {
			blockedFilter.ParentID = &parentID
		}
		blocked, err := getStore().GetBlockedIssues(ctx, blockedFilter)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		if isJSONOutput() {
			if blocked == nil {
				blocked = []*types.BlockedIssue{}
			}
			return outputJSON(blocked)
		}
		printBlockedHuman(blocked)
		return nil
	},
}
