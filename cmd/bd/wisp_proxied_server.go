package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/formula"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
)

func runWispCreateProxiedServer(ctx context.Context, in wispCreateInput) error {
	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}
	vars, err := parseVarFlags(in.varFlags)
	if err != nil {
		return HandleError("%v", err)
	}
	subgraph, protoID, err := resolveWispCreateSubgraph(ctx, in.protoArg, vars)
	if err != nil {
		return err
	}
	vars = applyVariableDefaults(vars, subgraph)
	if err := checkRequiredVars(subgraph, vars); err != nil {
		return HandleErrorWithHint(err.Error(), fmt.Sprintf("Provide them with: --var %s=<value>", firstMissingVar(subgraph, vars)))
	}
	if in.dryRun {
		renderWispCreateDryRun(protoID, subgraph, vars, in.rootOnly)
		return nil
	}
	return writeWispCreate(ctx, protoID, subgraph, vars, in.rootOnly)
}

func resolveWispCreateSubgraph(ctx context.Context, protoArg string, vars map[string]string) (*TemplateSubgraph, string, error) {
	sg, err := resolveAndCookFormulaWithVars(protoArg, nil, vars)
	if err == nil {
		return sg, sg.Root.ID, nil
	}
	if errors.Is(err, formula.ErrVarValidation) {
		// protoArg IS a formula; the --var values it was given fail
		// enum/pattern/required-empty constraints. Report that directly
		// instead of falling through to the proto-ID lookup below, which
		// would otherwise mask this as "not found as formula or proto".
		return nil, "", HandleError("%v", err)
	}
	return loadWispCreateProto(ctx, protoArg)
}

func loadWispCreateProto(ctx context.Context, protoArg string) (*TemplateSubgraph, string, error) {
	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return nil, "", err
	}
	defer uw.Close(ctx)
	r := uowMolReader{uw: uw}
	protoID, err := utils.ResolvePartialID(ctx, r, protoArg)
	if err != nil {
		return nil, "", HandleErrorWithHint(fmt.Sprintf("'%s' not found as formula or proto", protoArg), "run 'bd formula list' to see available formulas")
	}
	protoIssue, err := r.GetIssue(ctx, protoID)
	if err != nil {
		return nil, "", HandleError("loading proto %s: %v", protoID, err)
	}
	if !isProtoIssue(protoIssue) {
		return nil, "", HandleError("%s is not a proto (missing '%s' label)", protoID, MoleculeLabel)
	}
	sg, err := loadTemplateSubgraph(ctx, r, protoID)
	if err != nil {
		return nil, "", HandleError("loading proto: %v", err)
	}
	return sg, protoID, nil
}

func writeWispCreate(ctx context.Context, protoID string, subgraph *TemplateSubgraph, vars map[string]string, rootOnly bool) error {
	result, err := uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (*InstantiateResult, string, error) {
		w := newUOWMolWriter(uw)
		spawnResult, err := cloneSubgraphInto(ctx, w, subgraph, CloneOptions{
			Vars:      vars,
			Actor:     getActor(),
			Ephemeral: true,
			Prefix:    types.IDPrefixWisp,
			RootOnly:  rootOnly,
		})
		if err != nil {
			return nil, "", fmt.Errorf("creating wisp: %w", err)
		}
		return spawnResult, fmt.Sprintf("bd: wisp create %s", protoID), nil
	})
	if err != nil {
		return HandleError("%v", err)
	}
	return renderWispCreateResult(result)
}

func runWispListProxiedServer(ctx context.Context, showAll bool, typeFilter string) error {
	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return err
	}
	defer uw.Close(ctx)

	r := uowMolReader{uw: uw}
	issues, err := r.SearchIssues(ctx, "", wispListFilter(typeFilter))
	if err != nil {
		return HandleError("listing wisps: %v", err)
	}

	return renderWispListResult(buildWispListResult(issues, showAll))
}

func runWispGCProxiedServer(ctx context.Context, dryRun bool, ageThreshold time.Duration, cleanAll, closedMode, force bool, excludeTypes []types.IssueType) error {
	if err := CheckReadonly("wisp gc"); err != nil {
		return err
	}
	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}
	if closedMode {
		return runWispPurgeClosedProxiedServer(ctx, dryRun, force, excludeTypes)
	}
	abandoned, err := loadAbandonedWisps(ctx, cleanAll, ageThreshold, excludeTypes)
	if err != nil {
		return err
	}
	if len(abandoned) == 0 {
		return renderWispGCEmpty(dryRun)
	}
	if dryRun {
		return renderWispGCDryRun(abandoned)
	}
	ids := make([]string, len(abandoned))
	for i, issue := range abandoned {
		ids[i] = issue.ID
	}
	return deleteWispsProxied(ctx, ids, "bd: wisp gc %d")
}

func loadAbandonedWisps(ctx context.Context, cleanAll bool, ageThreshold time.Duration, excludeTypes []types.IssueType) ([]*types.Issue, error) {
	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return nil, err
	}
	r := uowMolReader{uw: uw}
	abandoned, err := findAbandonedWisps(ctx, r, cleanAll, ageThreshold, excludeTypes)
	uw.Close(ctx)
	if err != nil && abandoned == nil {
		return nil, HandleError("%v", err)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: cascade expansion incomplete: %v\n", err)
	}
	return abandoned, nil
}

func renderWispGCEmpty(dryRun bool) error {
	if isJSONOutput() {
		return outputJSON(WispGCResult{CleanedIDs: []string{}, CleanedCount: 0, DryRun: dryRun})
	}
	fmt.Println("No abandoned wisps found")
	return nil
}

func deleteWispsProxied(ctx context.Context, ids []string, commitFmt string) error {
	res, err := uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (domain.DeleteIssuesResult, string, error) {
		res, err := uw.IssueUseCase().DeleteIssues(ctx, domain.DeleteIssuesParams{
			IDs:                  ids,
			Cascade:              true,
			UpdateTextReferences: true,
		}, getActor())
		if err != nil {
			return domain.DeleteIssuesResult{}, "", err
		}
		return res, fmt.Sprintf(commitFmt, res.DeletedCount), nil
	})
	if err != nil {
		return HandleError("%v", err)
	}
	return renderWispGCDeleteResult(ids, res)
}

func renderWispGCDeleteResult(ids []string, res domain.DeleteIssuesResult) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"deleted":              ids,
			"deleted_count":        res.DeletedCount,
			"dependencies_removed": res.DependenciesCount,
			"labels_removed":       res.LabelsCount,
			"events_removed":       res.EventsCount,
			"references_updated":   res.ReferencesUpdated,
			"orphaned_issues":      res.OrphanedIssues,
		})
	}
	fmt.Printf("%s Deleted %d issue(s)\n", ui.RenderPass("✓"), res.DeletedCount)
	fmt.Printf("  Removed %d dependency link(s)\n", res.DependenciesCount)
	fmt.Printf("  Removed %d label(s)\n", res.LabelsCount)
	fmt.Printf("  Removed %d event(s)\n", res.EventsCount)
	fmt.Printf("  Updated text references in %d issue(s)\n", res.ReferencesUpdated)
	if len(res.OrphanedIssues) > 0 {
		fmt.Printf("  %s Orphaned %d issue(s): %s\n",
			ui.RenderWarn("⚠"), len(res.OrphanedIssues), strings.Join(res.OrphanedIssues, ", "))
	}
	return nil
}

func runWispPurgeClosedProxiedServer(ctx context.Context, dryRun, force bool, excludeTypes []types.IssueType) error {
	closedIssues, err := loadClosedWispsForPurge(ctx, excludeTypes)
	if err != nil {
		return err
	}
	if len(closedIssues) == 0 {
		return renderWispPurgeEmpty()
	}
	ids := make([]string, len(closedIssues))
	for i, issue := range closedIssues {
		ids[i] = issue.ID
	}
	if skip, err := confirmWispPurge(ids, dryRun, force); skip || err != nil {
		return err
	}
	return deleteWispsProxied(ctx, ids, "bd: wisp gc --closed %d")
}

func loadClosedWispsForPurge(ctx context.Context, excludeTypes []types.IssueType) ([]*types.Issue, error) {
	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return nil, err
	}
	defer uw.Close(ctx)
	r := uowMolReader{uw: uw}
	statusClosed := types.StatusClosed
	ephemeralTrue := true
	closedIssues, err := r.SearchIssues(ctx, "", types.IssueFilter{
		IssueFilterCore: types.IssueFilterCore{
			Status: &statusClosed,
			Limit:  5000,
		},
		IssueFilterFlags: types.IssueFilterFlags{
			Ephemeral: &ephemeralTrue,
		},
		IssueFilterHydrate: types.IssueFilterHydrate{
			ExcludeTypes: excludeTypes,
		},
	})
	if err != nil {
		return nil, HandleError("listing closed wisps: %v", err)
	}
	return filterClosedWisps(ctx, r, closedIssues), nil
}

func filterClosedWisps(ctx context.Context, r uowMolReader, closedIssues []*types.Issue) []*types.Issue {
	pinnedCount := 0
	infraCount := 0
	filtered := make([]*types.Issue, 0, len(closedIssues))
	for _, issue := range closedIssues {
		if issue.Pinned {
			pinnedCount++
			continue
		}
		if r.IsInfraTypeCtx(ctx, issue.IssueType) {
			infraCount++
			continue
		}
		filtered = append(filtered, issue)
	}
	if pinnedCount > 0 && !isJSONOutput() {
		fmt.Printf("Skipping %d pinned issue(s) (protected from cleanup)\n", pinnedCount)
	}
	if infraCount > 0 && !isJSONOutput() {
		fmt.Printf("Skipping %d configured infra issue(s) protected from GC\n", infraCount)
	}
	return filtered
}

func renderWispPurgeEmpty() error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"deleted_count": 0,
			"message":       "No closed wisps to delete",
		})
	}
	fmt.Println("No closed wisps to delete")
	return nil
}

func confirmWispPurge(ids []string, dryRun, force bool) (skip bool, err error) {
	if !force && !dryRun {
		if isJSONOutput() {
			return true, outputJSON(map[string]interface{}{"candidates": len(ids), "dry_run": true})
		}
		fmt.Printf("Found %d closed wisp(s) to delete\n", len(ids))
		fmt.Printf("\nUse --force to proceed, or --dry-run for detailed preview.\n")
		return true, nil
	}
	if !dryRun {
		return false, nil
	}
	if isJSONOutput() {
		return true, outputJSON(map[string]interface{}{"candidates": len(ids), "dry_run": true})
	}
	fmt.Printf("Found %d closed wisp(s)\n", len(ids))
	fmt.Println("DRY RUN - no changes will be made")
	return true, nil
}
