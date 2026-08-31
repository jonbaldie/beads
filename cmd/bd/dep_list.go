// Package main implements the bd CLI dependency management commands.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/issueops"
	"github.com/spf13/cobra"
)

type depListAnchor struct {
	fullID string
	store  storage.DoltStorage
	result *RoutedResult
}

// readDepListEdges asks each anchor's OWN store for its stored edges and
// reassembles the answers into the order the arguments named.
//
// The grouping is what keeps the answer on ONE shape: a failure is a failure, a
// split batch is N role calls merged back into one answer, and the caller's
// argument count picks the shape (see batchMode at the call site). `bd dep list
// a b c --json` documents an array of dependency records, not of issues.
func readDepListEdges(ctx context.Context, anchors []depListAnchor, typeFilter string) ([]issueops.AnchorEdges, error) {
	depTypes := depListTypes(typeFilter)
	byStore, order := groupDepListAnchors(anchors)
	answered, err := readDepListStoreGroups(ctx, byStore, order, depTypes)
	if err != nil {
		return nil, err
	}
	return orderDepListAnswers(anchors, answered), nil
}

func depListTypes(typeFilter string) []types.DependencyType {
	if typeFilter == "" {
		return nil
	}
	return []types.DependencyType{types.DependencyType(typeFilter)}
}

func groupDepListAnchors(anchors []depListAnchor) (map[storage.DoltStorage][]string, []storage.DoltStorage) {
	// Grouped by store IDENTITY, not by the store's workspace path: two handles
	// onto the same database are still two connections, and asking one of them
	// for the other's ids would answer that every one of them is missing.
	byStore := map[storage.DoltStorage][]string{}
	var order []storage.DoltStorage
	for _, anchor := range anchors {
		if _, seen := byStore[anchor.store]; !seen {
			order = append(order, anchor.store)
		}
		byStore[anchor.store] = append(byStore[anchor.store], anchor.fullID)
	}
	return byStore, order
}

func readDepListStoreGroups(ctx context.Context, byStore map[storage.DoltStorage][]string, order []storage.DoltStorage, depTypes []types.DependencyType) (map[string]issueops.AnchorEdges, error) {
	answered := make(map[string]issueops.AnchorEdges)
	for _, st := range order {
		reader, err := st.EdgeReader()
		if err != nil {
			return nil, err
		}
		result, err := reader.ReadEdges(ctx, issueops.EdgeReadRequest{IDs: byStore[st], Types: depTypes})
		if err != nil {
			return nil, err
		}
		for _, anchor := range result.Anchors {
			answered[anchor.ID] = anchor
		}
	}
	return answered, nil
}

func orderDepListAnswers(anchors []depListAnchor, answered map[string]issueops.AnchorEdges) []issueops.AnchorEdges {
	out := make([]issueops.AnchorEdges, 0, len(anchors))
	seen := make(map[string]struct{}, len(anchors))
	for _, anchor := range anchors {
		if _, dup := seen[anchor.fullID]; dup {
			continue
		}
		seen[anchor.fullID] = struct{}{}
		out = append(out, answered[anchor.fullID])
	}
	return out
}

// printDepListEdges renders the role's per-anchor answer, and is shared by both
// routes so the two cannot drift apart in what they print.
//
// A GHOST ANCHOR goes to stderr in both modes. Keeping it off stdout is what
// leaves `--json` a flat array of dependency records, which is the shape the
// command documents.
func printDepListEdges(anchors []issueops.AnchorEdges) error {
	for _, anchor := range anchors {
		if anchor.Missing {
			fmt.Fprintf(os.Stderr, "warning: no issue found: %s (skipped)\n", anchor.ID)
		}
	}
	if isJSONOutput() {
		out := []*types.Dependency{}
		for _, anchor := range anchors {
			out = append(out, anchor.Edges...)
		}
		return outputJSON(out)
	}
	for _, anchor := range anchors {
		if anchor.Missing {
			continue
		}
		if len(anchor.Edges) == 0 {
			fmt.Printf("\n%s has no dependencies\n", anchor.ID)
			continue
		}
		fmt.Printf("\n%s Dependencies of %s:\n\n", ui.RenderAccent("📋"), anchor.ID)
		for _, dep := range anchor.Edges {
			fmt.Printf("  %s via %s\n", dep.DependsOnID, dep.Type)
		}
	}
	fmt.Println()
	return nil
}

var depListCmd = &cobra.Command{
	Use:   "list [issue-id...]",
	Short: "List dependencies or dependents of one or more issues",
	Long: `List dependencies or dependents of one or more issues with optional type filtering.

By default shows dependencies (what issues depend on). Use --direction to control:
  - down: Show dependencies (what this issue depends on) - default
  - up:   Show dependents (what depends on this issue)

Multiple IDs can be provided for batch dep listing. With --json, the output
is a flat array of dependency records across all requested issues.

Use --type to filter by dependency type (e.g., tracks, blocks, parent-child).

Examples:
  bd dep list gt-abc                     # Show what gt-abc depends on
  bd dep list gt-abc gt-def              # Batch: deps for both issues
  bd dep list gt-abc --direction=up      # Show what depends on gt-abc
  bd dep list gt-abc --direction=up -t tracks  # Show what tracks gt-abc (convoy tracking)`,
	Args:          cobra.MinimumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("dep-list")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if usesProxiedServer() {
			return runDepListProxiedServer(cmd, getRootContext(), args)
		}

		ctx := getRootContext()
		direction, _ := cmd.Flags().GetString("direction")
		typeFilter, _ := cmd.Flags().GetString("type")
		if direction == "" {
			direction = "down"
		}

		var resolved []depListAnchor
		batchMode := len(args) > 1
		for _, arg := range args {
			routedResult, err := resolveAndGetIssueWithRouting(ctx, getStore(), arg)
			if err != nil {
				if batchMode {
					fmt.Fprintf(os.Stderr, "warning: resolving %s: %v (skipped)\n", arg, err)
					continue
				}
				return HandleErrorRespectJSON("resolving %s: %v", arg, err)
			}
			if routedResult == nil || routedResult.Issue == nil {
				if batchMode {
					fmt.Fprintf(os.Stderr, "warning: no issue found: %s (skipped)\n", arg)
					continue
				}
				return HandleErrorRespectJSON("no issue found: %s", arg)
			}
			depStore := getStore()
			if routedResult.Routed && routedResult.Store != nil {
				depStore = routedResult.Store
			}
			resolved = append(resolved, depListAnchor{
				fullID: routedResult.ResolvedID,
				store:  depStore,
				result: routedResult,
			})
		}
		if batchMode && len(resolved) == 0 {
			if isJSONOutput() {
				return outputJSON([]*types.Dependency{})
			}
			fmt.Fprintln(os.Stderr, "no resolvable issues in batch")
			return nil
		}
		defer func() {
			for _, r := range resolved {
				if r.result != nil {
					r.result.Close()
				}
			}
		}()

		// The multi-id edge listing is on the EdgeReader role, not Relations:
		// Relations is anchored on ONE issue, answers with the issues on the far
		// end of its edges rather than the edges themselves, and drops every
		// edge whose target this database has no row for.
		//
		// The accessor is taken PER STORE rather than once for the command: a
		// routed anchor answers from its own store, carrying its own decorator
		// stack.
		//
		// The shape is chosen on batchMode — the count the CALLER TYPED — and
		// not on len(resolved). Those differ exactly when an anchor did not
		// resolve, and the help text promises the records shape "with --json
		// ... across all requested issues", so a skipped anchor must not
		// silently change what a script is parsing.
		if batchMode && direction == "down" {
			anchors, err := readDepListEdges(ctx, resolved, typeFilter)
			if err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
			return printDepListEdges(anchors)
		}

		// The neighbor query is on the Relations role: one call per anchor, each
		// with an explicit direction, because the role refuses to guess one. The
		// accessor is taken per resolved anchor rather than once for the command
		// — a routed anchor answers from its own store, carrying its own
		// decorator stack.
		request := issueops.RelatedRequest{Direction: issueops.RelationOut}
		if direction == "up" {
			request.Direction = issueops.RelationIn
		}
		if typeFilter != "" {
			request.Types = []types.DependencyType{types.DependencyType(typeFilter)}
		}

		var allIssues []*issueops.RelatedIssue
		for _, r := range resolved {
			rel, err := r.store.IssueRelations()
			if err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
			request.ID = r.fullID
			issues, err := rel.Related(ctx, request)
			if err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
			allIssues = append(allIssues, issues...)
		}

		if isJSONOutput() {
			if allIssues == nil {
				allIssues = []*issueops.RelatedIssue{}
			}
			return outputJSON(allIssues)
		}

		if len(allIssues) == 0 {
			if len(resolved) == 1 {
				if direction == "up" {
					fmt.Printf("\nNo issues depend on %s\n", resolved[0].fullID)
				} else {
					fmt.Printf("\n%s has no dependencies\n", resolved[0].fullID)
				}
			} else {
				fmt.Println("\nNo dependencies found")
			}
			return nil
		}

		for _, iss := range allIssues {
			var idStr string
			switch iss.Status {
			case types.StatusOpen:
				idStr = ui.StatusOpenStyle().Render(iss.ID)
			case types.StatusInProgress:
				idStr = ui.StatusInProgressStyle().Render(iss.ID)
			case types.StatusBlocked:
				idStr = ui.StatusBlockedStyle().Render(iss.ID)
			case types.StatusClosed:
				idStr = ui.StatusClosedStyle().Render(iss.ID)
			default:
				idStr = iss.ID
			}
			fmt.Printf("  %s: %s [P%d] (%s) via %s\n",
				idStr, iss.Title, iss.Priority, iss.Status, iss.DependencyType)
		}
		fmt.Println()
		return nil
	},
}

var depRemoveCmd = &cobra.Command{
	Use:           "remove [issue-id] [depends-on-id]",
	Aliases:       []string{"rm"},
	Short:         "Remove a dependency",
	Args:          cobra.ExactArgs(2),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := CheckReadonly("dep remove"); err != nil {
			return err
		}

		evt := metrics.NewCommandEvent("dep-remove")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if usesProxiedServer() {
			return runDepRemoveProxiedServer(cmd, getRootContext(), args)
		}

		ctx := getRootContext()

		// Resolve partial IDs with routing support. The source issue's store is
		// mutated by RemoveDependency below, so resolve it write-intent (#4141);
		// the depends-on target is only resolved by ID and stays read-only
		// (bd-6dnrw.32, GH#3231).
		var fromID, toID string
		fromID, fromStore, fromCleanup, err := resolveIDForMutation(ctx, getStore(), args[0])
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		defer fromCleanup()

		isExternalRef := strings.HasPrefix(args[1], "external:")

		if isExternalRef {
			toID = args[1]
			if err := validateExternalRef(toID); err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
		} else if exact, ok := exactDependencyTarget(ctx, fromStore, fromID, args[1]); ok {
			// Prefer an exact depends_on_id match against raw edge records
			// before partial-ID resolution (GH#5005). Otherwise
			// `bd dep remove X 8vezf` resolves to the qualified good edge and
			// deletes it while leaving a dangling bare-id row behind.
			toID = exact
		} else {
			var toCleanup func()
			toID, _, toCleanup, err = resolveIDWithRouting(ctx, getStore(), args[1])
			if err != nil {
				srcPrefix := types.ExtractPrefix(fromID)
				tgtPrefix := types.ExtractPrefix(args[1])
				if srcPrefix != "" && tgtPrefix != "" && srcPrefix != tgtPrefix {
					toID = args[1]
				} else {
					return HandleErrorRespectJSON("resolving dependency ID %s: %v", args[1], err)
				}
			} else {
				defer toCleanup()
			}
		}

		fullFromID := fromID
		fullToID := toID

		// Explicit dep verb: the role records a dependency_removed history entry
		// for a genuine removal, matching bd dep add's edge event and the
		// proxied bd dep remove path.
		//
		// The role's Removed verdict is not printed, for the reason the proxied
		// route gives: `bd dep remove` has always confirmed the same way whether
		// or not an edge was there, and reporting the difference now would
		// change what every existing script reads.
		editor, err := fromStore.DependencyEditor()
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		opsCtx, err := issueOpsContext(ctx)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		if _, err := editor.RemoveDependency(opsCtx, issueops.RemoveDependencyRequest{
			Actor:       getActor(),
			IssueID:     fullFromID,
			DependsOnID: fullToID,
		}); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}

		if err := commitPendingIfEmbedded(ctx, fromStore, getActor(), doltAutoCommitParams{
			Command:  "dep remove",
			IssueIDs: []string{fullFromID, fullToID},
		}); err != nil {
			return HandleErrorRespectJSON("failed to commit: %v", err)
		}

		if isJSONOutput() {
			return outputJSON(map[string]interface{}{
				"status":        "removed",
				"issue_id":      fullFromID,
				"depends_on_id": fullToID,
			})
		}

		fmt.Printf("%s Removed dependency: %s → %s\n",
			ui.RenderPass("✓"), formatFeedbackIDParen(fullFromID, lookupTitle(fullFromID)), formatFeedbackIDParen(fullToID, lookupTitle(fullToID)))
		return nil
	},
}
