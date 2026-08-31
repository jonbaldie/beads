package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/workapi"
	"github.com/jonbaldie/beads/issueops"
)

func runReadyProxiedServer(cmd *cobra.Command, ctx context.Context) error {
	// --offset is supported here and nowhere else, so this is where a negative
	// value is rejected. It stays out of the shared gatherer because the direct
	// route reaches that gatherer too, and `bd ready --offset -1` has always
	// been a no-op there rather than an error. HandleError, not the RespectJSON
	// variant, for the same reason: this message is the proxied route's alone,
	// and it has always gone to stderr as plain text.
	if offset, _ := cmd.Flags().GetInt("offset"); offset < 0 {
		return HandleError("--offset must be >= 0")
	}

	// No cap resolver: the RunE that routed here has already resolved
	// --max-rows / BEADS_MAX_ROWS, either to reject a live one or to validate
	// the value it then ignores for --claim. Resolving it again would repeat
	// the malformed-value warning and stamp a cap this route cannot enforce.
	in, err := gatherReadyInput(cmd, nil)
	if err != nil {
		return err
	}

	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}

	if in.claim {
		return runReadyProxiedClaim(ctx, in)
	}

	// Wake expired dated defers before the read below. The unit of work this
	// route opens is read-only-by-ending (Close rolls back), so the sweep runs
	// in a committing UOW of its own first; the claim route above gets the
	// same sweep from its role (uow.readyClaimer.ClaimNext).
	uow.WakeExpiredDefersAdvisory(ctx, getUOWProvider())

	uw, err := getUOWProvider().NewUOW(ctx)
	if err != nil {
		return HandleErrorRespectJSON("open unit of work: %v", err)
	}
	defer uw.Close(ctx)

	switch {
	case in.gated:
		return runReadyProxiedGated(ctx, uw, in)
	case in.molID != "":
		return runReadyProxiedMolecule(ctx, uw, in)
	case in.explain:
		return runReadyProxiedExplain(ctx, uw, in)
	default:
		return runReadyProxiedList(ctx, uw, in)
	}
}

func runBlockedProxiedServer(cmd *cobra.Command, ctx context.Context) error {
	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}
	uw, err := getUOWProvider().NewUOW(ctx)
	if err != nil {
		return HandleErrorRespectJSON("open unit of work: %v", err)
	}
	defer uw.Close(ctx)

	var filter types.WorkFilter
	if parentID, _ := cmd.Flags().GetString("parent"); parentID != "" {
		filter.ParentID = &parentID
	}

	blocked, err := uw.IssueUseCase().GetBlockedIssues(ctx, filter)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	if isJSONOutput() {
		if blocked == nil {
			blocked = []*types.BlockedIssue{}
		}
		_ = outputJSON(blocked)
		return nil
	}
	printBlockedHuman(blocked)
	return nil
}

func runReadyProxiedList(ctx context.Context, uw uow.UnitOfWork, in readyInput) error {
	if in.jsonOut {
		return runReadyProxiedListJSON(ctx, uw, in)
	}
	return runReadyProxiedListHuman(ctx, uw, in)
}

func runReadyProxiedListJSON(ctx context.Context, uw uow.UnitOfWork, in readyInput) error {
	page, err := uw.IssueUseCase().GetReadyWorkWithCounts(ctx, in.filter)
	if err != nil {
		return HandleError("%v", err)
	}
	// The same epilogue issueops.Reader.Ready runs, through the same
	// function: this seam reports a has-more natively and ready has no
	// display order, so the trim is a no-op and the verdict is the seam's
	// — but it is reached the one way, not restated here.
	results, truncated := workapi.FinishPage(page.Items, "", false, in.filter.Limit, page.HasMore)
	truncated = truncated && in.filter.Limit > 0
	_ = outputJSONWithPagination(results, proxiedReadyPagination(ctx, in, results, truncated))
	if truncated {
		fmt.Fprintf(os.Stderr, "Showing %d ready issues; more matched but were hidden by --limit. Use --limit 0 for all, or --limit N to raise the cap.\n", len(results))
	}
	return nil
}

func proxiedReadyPagination(ctx context.Context, in readyInput, results []*types.IssueWithCounts, truncated bool) *PaginationMeta {
	if !truncated {
		return nil
	}

	// Parity with the direct route: the pagination key is emitted only
	// when truncated, and it now carries the same Total, from the same
	// role. This route published no total at all until ReadyCounter
	// existed, so a script that read `pagination.total` got one number
	// under a direct-mode workspace and no key at all under a proxied one.
	//
	// The guard is the direct route's guard: the two queries are not one
	// snapshot (issueops.ReadyCounter.CountReady), so a close landing
	// between them must not publish a total smaller than the page beside
	// it.
	pag := &PaginationMeta{Returned: len(results), Truncated: true}
	if n, countErr := proxiedReadyTotal(ctx, in); countErr == nil && n > len(results) {
		pag.Total = n
	}
	return pag
}

func runReadyProxiedListHuman(ctx context.Context, uw uow.UnitOfWork, in readyInput) error {
	page, err := uw.IssueUseCase().GetReadyWork(ctx, in.filter)
	if err != nil {
		return HandleError("%v", err)
	}
	issues, truncated := workapi.FinishPage(page.Items, "", false, in.filter.Limit, page.HasMore)
	truncated = truncated && in.filter.Limit > 0

	maybeShowUpgradeNotification()

	if len(issues) == 0 {
		showReadyProxiedEmpty(ctx, uw)
		return nil
	}

	parentEpicMap := buildParentEpicMapProxied(ctx, uw, issues)
	printReadyProxiedIssues(issues, parentEpicMap, in.plainFormat || !in.prettyFormat)

	if truncated {
		printReadyProxiedTruncation(len(issues))
	}
	return nil
}

func showReadyProxiedEmpty(ctx context.Context, uw uow.UnitOfWork) {
	hasOpenIssues := false
	if stats, err := uw.IssueUseCase().GetStatistics(ctx); err == nil {
		hasOpenIssues = stats.OpenIssues > 0 || stats.InProgressIssues > 0
	}
	hasStoredBlocked := false
	st := types.StatusBlocked
	if page, err := uw.IssueUseCase().SearchIssues(ctx, "", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Status: &st, Limit: 1}}); err == nil && len(page.Items) > 0 {
		hasStoredBlocked = true
	}
	printReadyEmptyHuman(hasOpenIssues, hasStoredBlocked)
}

func printReadyProxiedIssues(issues []*types.Issue, parentEpicMap map[string]string, plain bool) {
	if !plain {
		displayReadyList(issues, parentEpicMap)
		return
	}

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
}

func printReadyProxiedTruncation(issueCount int) {
	fmt.Printf("%s\n\n", ui.RenderMuted(fmt.Sprintf("Showing %d ready issues; more matched but were hidden by --limit. Use --limit 0 for all, or --limit N to raise the cap.", issueCount)))
}

// runReadyProxiedClaim is the proxied route of `bd ready --claim`, on the same
// ReadyClaimer role the direct route reaches through the store's accessor.
// Both build their request in claimNextRequest, so the two doors ask one
// question — and neither opens a unit of work of its own: the role's request
// IS the transaction, which is what lets the selection, the compare-and-set
// and the hydration this route prints share it.
func runReadyProxiedClaim(ctx context.Context, in readyInput) error {
	if err := CheckReadonly("ready --claim"); err != nil {
		return err
	}

	claimer, err := proxiedReadyClaimer()
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	res, err := claimer.ClaimNext(ctx, claimNextRequest(in))
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	if res.Claimed == nil {
		if in.jsonOut {
			_ = outputJSON([]*types.IssueWithCounts{})
		} else {
			fmt.Printf("\n%s No ready work to claim\n\n", ui.RenderWarn("○"))
		}
		return nil
	}

	SetLastTouchedID(res.Claimed.ID)

	if in.jsonOut {
		_ = outputJSON([]*types.IssueWithCounts{res.Claimed})
	} else {
		fmt.Printf("%s Claimed issue: %s\n", ui.RenderPass("✓"), formatFeedbackID(res.Claimed.ID, res.Claimed.Title))
	}
	return nil
}

// proxiedReadyTotal sizes the whole ready set through the provider's own
// ReadyCounter accessor — the same role the direct route reaches through the
// store's accessor, over the request both build in readyRoleRequest.
//
// It opens NO unit of work of its own: the role's request IS the transaction,
// so the count runs in its own read-only unit of work rather than the one the
// page came from, which is why the two are not one snapshot.
func proxiedReadyTotal(ctx context.Context, in readyInput) (int, error) {
	if getUOWProvider() == nil {
		return 0, errors.New("proxied-server UOW provider not initialized")
	}
	src, ok := getUOWProvider().(uow.ReadyCounterSource)
	if !ok {
		return 0, fmt.Errorf("proxied-server provider %T does not offer the ready-count surface", getUOWProvider())
	}
	counter, err := src.ReadyCounter()
	if err != nil {
		return 0, err
	}
	result, err := counter.CountReady(ctx, readyRoleRequest(in))
	if err != nil {
		return 0, err
	}
	return int(result.Total), nil
}

// proxiedReadyClaimer hands back the guarded claim surface for the
// proxied-server provider, through the provider's OWN capability accessor —
// the same two-step proxiedIssueReader performs, and for the same reason: the
// accessor is where a decorator adds its layer.
func proxiedReadyClaimer() (issueops.ReadyClaimer, error) {
	if getUOWProvider() == nil {
		return nil, errors.New("proxied-server UOW provider not initialized")
	}
	src, ok := getUOWProvider().(uow.ReadyClaimerSource)
	if !ok {
		return nil, fmt.Errorf("proxied-server provider %T does not offer the ready-claim surface", getUOWProvider())
	}
	return src.ReadyClaimer()
}

func runReadyProxiedExplain(ctx context.Context, uw uow.UnitOfWork, _ readyInput) error {
	data, err := loadReadyExplanationDataProxied(ctx, uw)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	explanation := types.BuildReadyExplanation(
		data.readyIssues,
		data.blockedIssues,
		data.depCounts,
		data.allDeps,
		data.blockerMap,
		data.cycles,
	)

	if isJSONOutput() {
		_ = outputJSON(explanation)
		return nil
	}
	renderReadyExplanation(explanation)
	return nil
}

func loadReadyExplanationDataProxied(ctx context.Context, uw uow.UnitOfWork) (readyExplanationData, error) {
	filter, err := readyExplainFilter()
	if err != nil {
		return readyExplanationData{}, err
	}
	readyPage, err := uw.IssueUseCase().GetReadyWork(ctx, filter)
	if err != nil {
		return readyExplanationData{}, err
	}
	readyIssues := readyPage.Items

	blockedIssues, err := uw.IssueUseCase().GetBlockedIssues(ctx, types.WorkFilter{})
	if err != nil {
		return readyExplanationData{}, err
	}

	readyIDs := readyExplanationIssueIDs(readyIssues)
	depCounts, allDeps, cycles := loadReadyExplanationDependenciesProxied(ctx, uw, readyIDs)
	blockerMap := loadReadyExplanationBlockersProxied(ctx, uw, blockedIssues)
	return readyExplanationData{
		readyIssues:   readyIssues,
		blockedIssues: blockedIssues,
		depCounts:     depCounts,
		allDeps:       allDeps,
		blockerMap:    blockerMap,
		cycles:        cycles,
	}, nil
}

func loadReadyExplanationDependenciesProxied(ctx context.Context, uw uow.UnitOfWork, readyIDs []string) (map[string]*types.DependencyCounts, map[string][]*types.Dependency, [][]*types.Issue) {
	depCounts, err := uw.DependencyUseCase().CountsByIssueIDs(ctx, readyIDs)
	if err != nil {
		debug.Logf("warning: failed to get dependency counts: %v", err)
	}
	allDeps, err := uw.DependencyUseCase().GetForIssueIDs(ctx, readyIDs)
	if err != nil {
		debug.Logf("warning: failed to get dependency records: %v", err)
	}

	cycles, err := uw.DependencyUseCase().DetectCycles(ctx)
	if err != nil {
		debug.Logf("warning: failed to detect cycles: %v", err)
	}
	return depCounts, allDeps, cycles
}

func loadReadyExplanationBlockersProxied(ctx context.Context, uw uow.UnitOfWork, blockedIssues []*types.BlockedIssue) map[string]*types.Issue {
	blockerIDs := readyExplanationBlockerIDs(blockedIssues)
	blockerIssues, err := uw.IssueUseCase().GetIssuesByIDs(ctx, blockerIDs)
	if err != nil {
		debug.Logf("warning: failed to get blocker issues: %v", err)
	}
	blockerWisps, err := uw.IssueUseCase().GetWispsByIDs(ctx, blockerIDs)
	if err != nil {
		debug.Logf("warning: failed to get blocker wisps: %v", err)
	}
	blockerMap := make(map[string]*types.Issue, len(blockerIssues)+len(blockerWisps))
	for _, issue := range blockerIssues {
		blockerMap[issue.ID] = issue
	}
	for _, wisp := range blockerWisps {
		blockerMap[wisp.ID] = wisp
	}
	return blockerMap
}

func runReadyProxiedMolecule(ctx context.Context, uw uow.UnitOfWork, in readyInput) error {
	moleculeID := in.molID
	subgraph, err := loadTemplateSubgraph(ctx, uowMolReader{uw: uw}, moleculeID)
	if err != nil {
		return HandleError("loading molecule: %v", err)
	}

	analysis := analyzeMoleculeParallel(subgraph)
	readySteps := collectReadyMoleculeSteps(subgraph, analysis)

	if in.jsonOut {
		_ = outputJSON(MoleculeReadyOutput{
			MoleculeID:     moleculeID,
			MoleculeTitle:  subgraph.Root.Title,
			TotalSteps:     analysis.TotalSteps,
			ReadySteps:     len(readySteps),
			Steps:          readySteps,
			ParallelGroups: analysis.ParallelGroups,
		})
		return nil
	}
	renderMoleculeReadyHuman(moleculeID, subgraph, analysis, readySteps)
	return nil
}

func runReadyProxiedGated(ctx context.Context, uw uow.UnitOfWork, _ readyInput) error {
	molecules, err := findGateReadyMolecules(ctx, uowMolReader{uw: uw})
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	return renderGatedReadyMolecules(molecules)
}

func buildParentEpicMapProxied(ctx context.Context, uw uow.UnitOfWork, issues []*types.Issue) map[string]string {
	if len(issues) == 0 {
		return nil
	}
	ids := make([]string, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
	}
	allDeps, err := uw.DependencyUseCase().GetForIssueIDs(ctx, ids)
	if err != nil {
		return nil
	}
	parentIDs, childToParent := proxiedParentEpicRelationships(allDeps)
	if len(parentIDs) == 0 {
		return nil
	}
	epicTitles := loadProxiedEpicTitles(ctx, uw, parentIDs)
	return mapChildrenToEpicTitles(childToParent, epicTitles)
}

func proxiedParentEpicRelationships(allDeps map[string][]*types.Dependency) (map[string]bool, map[string]string) {
	parentIDs := make(map[string]bool)
	childToParent := make(map[string]string)
	for issueID, deps := range allDeps {
		for _, dep := range deps {
			if dep.Type != types.DepParentChild {
				continue
			}
			parentIDs[dep.DependsOnID] = true
			childToParent[issueID] = dep.DependsOnID
		}
	}
	return parentIDs, childToParent
}

func loadProxiedEpicTitles(ctx context.Context, uw uow.UnitOfWork, parentIDs map[string]bool) map[string]string {
	epicTitles := make(map[string]string)
	for parentID := range parentIDs {
		parent, err := uw.IssueUseCase().GetIssue(ctx, parentID)
		if err != nil || parent == nil {
			continue
		}
		if parent.IssueType == "epic" {
			epicTitles[parentID] = parent.Title
		}
	}
	return epicTitles
}
