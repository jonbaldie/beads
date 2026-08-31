package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/workapi"
	"github.com/jonbaldie/beads/issueops"
	"github.com/spf13/cobra"
)

func runListCoreProxied(cmd *cobra.Command, in listInput) error {
	out := cmd.OutOrStdout()
	// The cap USED to be rejected here: the proxied query path threaded no
	// MaxRows, so honoring it would have been silence. It threads one now
	// (internal/storage/domain/db sizes its bound and enforces the cap
	// through the same two functions the store seam uses), so this route
	// answers *ErrTooManyRows the same way the direct route below does —
	// same message, same exit code.
	if err := runListProxiedServer(cmd, getRootContext(), out, in); err != nil {
		if capErr := handleMaxRowsError(err); capErr != nil {
			return capErr
		}
		return HandleError("%v", err)
	}
	return nil
}

func runListCoreDirect(cmd *cobra.Command, in listInput) error {
	if in.Offset > 0 {
		return HandleError("--offset is only supported under --proxied-server")
	}
	ctx := getRootContext()
	cfg, err := workapi.LoadStoreListConfig(ctx, getStore())
	if err != nil {
		return HandleError("%v", err)
	}
	filter, err := workapi.BuildListFilter(in.ListRequest, cfg)
	if err != nil {
		return HandleError("%v", err)
	}
	activeStore, cleanup, err := listDirectActiveStore(ctx)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}
	if in.watchMode {
		return runListWatch(ctx, activeStore, filter, in)
	}
	return renderListDirect(ctx, cmd, in, activeStore, filter)
}

func listDirectActiveStore(ctx context.Context) (storage.DoltStorage, func(), error) {
	activeStore := getStore()
	routedStore, routed, routingRule, err := openRoutedReadStore(ctx, activeStore)
	if err != nil {
		return nil, nil, HandleError("%v", err)
	}
	if !routed {
		return activeStore, nil, nil
	}
	printContributorRoutingNotice(ctx, activeStore, routingRule)
	return routedStore, func() { _ = routedStore.Close() }, nil
}

func runListWatch(ctx context.Context, activeStore storage.DoltStorage, filter types.IssueFilter, in listInput) error {
	if err := watchIssues(ctx, activeStore, filter, in.ReadyFlag, in.ParentID, in.SortBy, in.Reverse, in.effectiveLimit); err != nil {
		if capErr := handleMaxRowsError(err); capErr != nil {
			return capErr
		}
		return HandleError("querying issues: %v", err)
	}
	return nil
}

func renderListDirect(ctx context.Context, cmd *cobra.Command, in listInput, activeStore storage.DoltStorage, filter types.IssueFilter) error {
	reader, err := activeStore.IssueReader()
	if err != nil {
		return HandleError("%v", err)
	}
	if isJSONOutput() {
		return renderListDirectJSON(ctx, in, reader)
	}
	return renderListDirectText(ctx, cmd, in, activeStore, filter, reader)
}

func renderListDirectJSON(ctx context.Context, in listInput, reader issueops.Reader) error {
	// --json. The role's List runs the same LoadStoreListConfig, the same
	// BuildListFilter and the same workapi.FinishPage this branch ran longhand,
	// and the --ready arm is its ReadyFlag, so the page, its order, its trim and
	// its has-more verdict are unchanged bytes. The cap still arrives as
	// *ErrTooManyRows, which is why handleMaxRowsError still wraps the call.
	page, err := reader.List(ctx, in.ListRequest)
	if err != nil {
		if capErr := handleMaxRowsError(err); capErr != nil {
			return capErr
		}
		return HandleError("%v", err)
	}
	if in.SkipLabels {
		if err := outputJSON(newSkipLabelsListJSONResponse(page.Items)); err != nil {
			return err
		}
		printTruncationHint(page.HasMore, in.effectiveLimit)
		return nil
	}
	if err := outputJSON(page.Items); err != nil {
		return err
	}
	printTruncationHint(page.HasMore, in.effectiveLimit)
	return nil
}

func renderListDirectText(ctx context.Context, cmd *cobra.Command, in listInput, activeStore storage.DoltStorage, filter types.IssueFilter, reader issueops.Reader) error {
	// The text renderings print no cardinality, so the request carries SkipCounts
	// (issueops.ListRequest.SkipCounts). Without it this would trade a plain scan
	// for three aggregate joins on the most-run command in the tree.
	textRequest := in.ListRequest
	textRequest.SkipCounts = true
	page, err := reader.List(ctx, textRequest)
	if err != nil {
		if capErr := handleMaxRowsError(err); capErr != nil {
			return capErr
		}
		return HandleError("%v", err)
	}
	issues, truncated := listPageIssues(page)
	if in.prettyFormat && !isJSONOutput() {
		return renderListDirectPretty(ctx, in, activeStore, filter, issues, truncated)
	}
	if in.formatStr != "" {
		return renderListDirectFormatted(ctx, cmd, in, activeStore, issues, truncated)
	}
	return renderListDirectDefault(ctx, in, activeStore, issues, truncated)
}

func renderListDirectPretty(ctx context.Context, in listInput, activeStore storage.DoltStorage, filter types.IssueFilter, issues []*types.Issue, truncated bool) error {
	if in.ParentID != "" && !in.ReadyFlag {
		return renderListDirectPrettyTree(ctx, in, activeStore, filter)
	}
	allDeps, depErr := activeStore.GetAllDependencyRecords(ctx)
	if depErr != nil && in.depsMode != "" {
		return HandleError("loading dependencies for --deps: %v", depErr)
	}
	displayPrettyListWithDepsMode(issues, false, allDeps, in.depsMode, truncated)
	printTruncationHint(truncated, in.effectiveLimit)
	printSkipLabelsFooter(in.SkipLabels)
	return nil
}

func renderListDirectPrettyTree(ctx context.Context, in listInput, activeStore storage.DoltStorage, filter types.IssueFilter) error {
	treeIssues, err := getHierarchicalChildren(ctx, activeStore, "", in.ParentID, filter)
	if err != nil {
		return HandleError("%v", err)
	}
	if len(treeIssues) == 0 {
		fmt.Printf("Issue '%s' has no children\n", in.ParentID)
		return nil
	}
	allDeps, depErr := activeStore.GetAllDependencyRecords(ctx)
	if depErr != nil && in.depsMode != "" {
		return HandleError("loading dependencies for --deps: %v", depErr)
	}
	// Hierarchical --parent walks use an unlimited per-level query, so the tree is never page-truncated.
	displayPrettyListWithDepsMode(treeIssues, false, allDeps, in.depsMode, false)
	printSkipLabelsFooter(in.SkipLabels)
	return nil
}

func renderListDirectFormatted(ctx context.Context, cmd *cobra.Command, in listInput, activeStore storage.DoltStorage, issues []*types.Issue, truncated bool) error {
	depsByIssueID, _ := activeStore.GetAllDependencyRecords(ctx)
	if err := outputFormattedList(cmd.OutOrStdout(), issues, depsByIssueID, in.formatStr); err != nil {
		return HandleError("%v", err)
	}
	printTruncationHint(truncated, in.effectiveLimit)
	return nil
}

func renderListDirectDefault(ctx context.Context, in listInput, activeStore storage.DoltStorage, issues []*types.Issue, truncated bool) error {
	maybeShowUpgradeNotification()
	issueIDs, labelsMap := listDirectIssueLabels(issues)
	// The decoration goes through issueops.BlockingAnnotator. Its failure is
	// still swallowed: this route has always rendered the page undecorated
	// rather than failing on it, while the proxied route fails — a difference
	// between the two CALLERS, recorded for the owner in AMBIGUITIES.md
	// (A-blk-1) rather than converged here.
	blocking := annotateListBlocking(ctx, activeStore, issueIDs)
	var buf strings.Builder
	if ui.IsAgentMode() {
		writeListAgentIssues(&buf, issues, blocking)
		fmt.Print(buf.String())
		printTruncationHint(truncated, in.effectiveLimit)
		return nil
	}
	writeListHumanIssues(&buf, in, issues, labelsMap, blocking)
	if err := ui.ToPager(buf.String(), ui.PagerOptions{NoPager: in.noPager}); err != nil {
		if _, writeErr := fmt.Fprint(os.Stdout, buf.String()); writeErr != nil {
			fmt.Fprintf(os.Stderr, "Error writing output: %v\n", writeErr)
		}
	}
	printTruncationHint(truncated, in.effectiveLimit)
	maybeShowTip(getStore())
	return nil
}

func listDirectIssueLabels(issues []*types.Issue) ([]string, map[string][]string) {
	issueIDs := make([]string, len(issues))
	labelsMap := make(map[string][]string, len(issues))
	for i, issue := range issues {
		issueIDs[i] = issue.ID
		if len(issue.Labels) > 0 {
			labelsMap[issue.ID] = issue.Labels
		}
	}
	return issueIDs, labelsMap
}

func writeListAgentIssues(buf *strings.Builder, issues []*types.Issue, blocking listBlocking) {
	for _, issue := range issues {
		formatAgentIssue(buf, issue, blocking.blockedBy[issue.ID], blocking.blocks[issue.ID], blocking.parent[issue.ID])
	}
}

func writeListHumanIssues(buf *strings.Builder, in listInput, issues []*types.Issue, labelsMap map[string][]string, blocking listBlocking) {
	if in.longFormat {
		buf.WriteString(fmt.Sprintf("\nFound %d issues:\n\n", len(issues)))
		for _, issue := range issues {
			formatIssueLong(buf, issue, labelsMap[issue.ID], in.SkipLabels)
		}
	} else {
		for _, issue := range issues {
			formatIssueCompact(buf, issue, labelsMap[issue.ID], blocking.blockedBy[issue.ID], blocking.blocks[issue.ID], blocking.parent[issue.ID])
		}
	}
	if in.SkipLabels && !isQuiet() {
		buf.WriteString(skipLabelsFooterText())
	}
}
