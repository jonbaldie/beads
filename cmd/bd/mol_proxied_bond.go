package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
)

func runMolBondProxiedServer(ctx context.Context, in molBondInput) error {
	if getUOWProvider() == nil {
		return HandleErrorRespectJSON("proxied-server UOW provider not initialized")
	}

	if in.dryRun {
		return runMolBondProxiedDryRun(ctx, in)
	}

	res, err := uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (molBondProxiedResult, string, error) {
		return runMolBondProxiedTransaction(ctx, uw, in)
	})
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	return renderMolBondResult(res.result, res.idA, res.idB, in.ephemeral, in.pour)
}

type molBondProxiedResult struct {
	result *BondResult
	idA    string
	idB    string
}

func runMolBondProxiedDryRun(ctx context.Context, in molBondInput) error {
	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return err
	}
	defer uw.Close(ctx)
	r := uowMolReader{uw: uw}

	issueA, formulaA, err := resolveOrDescribe(ctx, r, in.argA, in.vars)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	issueB, formulaB, err := resolveOrDescribe(ctx, r, in.argB, in.vars)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	renderMolBondDryRun(in, issueA, formulaA, issueB, formulaB)
	return nil
}

func runMolBondProxiedTransaction(ctx context.Context, uw uow.UnitOfWork, in molBondInput) (molBondProxiedResult, string, error) {
	w := newUOWMolWriter(uw)
	subgraphA, cookedA, err := resolveOrCookToSubgraph(ctx, w, in.argA, in.vars)
	if err != nil {
		return molBondProxiedResult{}, "", err
	}
	subgraphB, cookedB, err := resolveOrCookToSubgraph(ctx, w, in.argB, in.vars)
	if err != nil {
		return molBondProxiedResult{}, "", err
	}

	issueA := subgraphA.Root
	issueB := subgraphB.Root
	result, err := bondProxiedOperands(ctx, w, issueA, issueB, subgraphA, subgraphB, cookedA, cookedB, in)
	if err != nil {
		return molBondProxiedResult{}, "", fmt.Errorf("bonding: %w", err)
	}
	return molBondProxiedResult{result: result, idA: issueA.ID, idB: issueB.ID},
		fmt.Sprintf("bd: mol bond %s %s", issueA.ID, issueB.ID), nil
}

func bondProxiedOperands(
	ctx context.Context,
	w molWriter,
	issueA, issueB *types.Issue,
	subgraphA, subgraphB *TemplateSubgraph,
	cookedA, cookedB bool,
	in molBondInput,
) (*BondResult, error) {
	aIsProto := issueA.IsTemplate || cookedA
	bIsProto := issueB.IsTemplate || cookedB
	switch {
	case aIsProto && bIsProto:
		return bondProtoProtoInto(ctx, w, issueA, issueB, in.bondType, in.customTitle, getActor())
	case aIsProto && !bIsProto:
		return bondProtoMolAttachInto(ctx, w, subgraphA, issueA, issueB, in.bondAttachmentOptions(getActor()))
	case !aIsProto && bIsProto:
		return bondProtoMolAttachInto(ctx, w, subgraphB, issueB, issueA, in.bondAttachmentOptions(getActor()))
	default:
		return bondMolMolInto(ctx, w, issueA, issueB, in.bondType, getActor())
	}
}

func runMolSquashProxiedServer(ctx context.Context, in molSquashInput) error {
	if err := CheckReadonly("mol squash"); err != nil {
		return err
	}

	if getUOWProvider() == nil {
		return HandleErrorRespectJSON("proxied-server UOW provider not initialized")
	}

	if in.dryRun {
		return runMolSquashProxiedDryRun(ctx, in)
	}

	result, err := uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (*SquashResult, string, error) {
		return runMolSquashProxiedTransaction(ctx, uw, in)
	})
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	return renderMolSquashProxiedResult(result)
}

func runMolSquashProxiedDryRun(ctx context.Context, in molSquashInput) error {
	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return err
	}
	defer uw.Close(ctx)
	r := uowMolReader{uw: uw}
	moleculeID, err := utils.ResolvePartialID(ctx, r, in.moleculeArg)
	if err != nil {
		return HandleErrorRespectJSON("resolving molecule ID %s: %v", in.moleculeArg, err)
	}
	subgraph, err := loadTemplateSubgraph(ctx, r, moleculeID)
	if err != nil {
		return HandleErrorRespectJSON("loading molecule: %v", err)
	}
	wispChildren := wispChildrenOf(subgraph)
	if len(wispChildren) == 0 {
		if isJSONOutput() {
			return outputJSON(SquashResult{MoleculeID: moleculeID, SquashedCount: 0})
		}
		fmt.Printf("No ephemeral children found for molecule %s\n", moleculeID)
		return nil
	}
	renderSquashDryRun(moleculeID, subgraph, wispChildren, in.keepChildren)
	return nil
}

func runMolSquashProxiedTransaction(ctx context.Context, uw uow.UnitOfWork, in molSquashInput) (*SquashResult, string, error) {
	w := newUOWMolWriter(uw)
	moleculeID, err := utils.ResolvePartialID(ctx, w, in.moleculeArg)
	if err != nil {
		return nil, "", fmt.Errorf("resolving molecule ID %s: %w", in.moleculeArg, err)
	}
	subgraph, err := loadTemplateSubgraph(ctx, w, moleculeID)
	if err != nil {
		return nil, "", fmt.Errorf("loading molecule: %w", err)
	}
	wispChildren := wispChildrenOf(subgraph)
	if len(wispChildren) == 0 {
		return &SquashResult{MoleculeID: moleculeID, SquashedCount: 0}, "", nil
	}
	result, err := squashMoleculeInto(ctx, w, subgraph.Root, wispChildren, in.keepChildren, in.summary, getActor())
	if err != nil {
		return nil, "", fmt.Errorf("squashing molecule: %w", err)
	}
	return result, fmt.Sprintf("bd: mol squash %s", moleculeID), nil
}

func renderMolSquashProxiedResult(result *SquashResult) error {
	if result.SquashedCount == 0 && result.DigestID == "" {
		if isJSONOutput() {
			return outputJSON(result)
		}
		fmt.Printf("No ephemeral children found for molecule %s\n", result.MoleculeID)
		return nil
	}
	return renderSquashResult(result)
}

func runMolBurnProxiedServer(ctx context.Context, args []string, dryRun, force bool) error {
	if getUOWProvider() == nil {
		return HandleErrorRespectJSON("proxied-server UOW provider not initialized")
	}

	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return err
	}
	r := uowMolReader{uw: uw}
	targets := collectProxiedBurnTargets(ctx, r, args)
	uw.Close(ctx)

	if len(targets.wispIDs) == 0 && len(targets.persistentIDs) == 0 {
		if isJSONOutput() {
			return outputJSON(BatchBurnResult{FailedCount: len(targets.failedResolve)})
		}
		fmt.Println("No valid molecules to burn")
		return nil
	}

	if dryRun {
		return renderProxiedBurnDryRun(targets)
	}

	if !confirmProxiedBurn(targets, force) {
		return nil
	}

	return burnAndRenderProxiedTargets(ctx, targets)
}

type proxiedBurnTargets struct {
	wispIDs       []string
	persistentIDs []string
	failedResolve []string
}

func collectProxiedBurnTargets(ctx context.Context, r molReader, args []string) proxiedBurnTargets {
	targets := proxiedBurnTargets{}
	seenWispIDs := make(map[string]bool)
	for _, moleculeID := range args {
		collectOneProxiedBurnTarget(ctx, r, moleculeID, &targets, seenWispIDs)
	}
	return targets
}

func collectOneProxiedBurnTarget(ctx context.Context, r molReader, moleculeID string, targets *proxiedBurnTargets, seenWispIDs map[string]bool) {
	resolvedID, err := utils.ResolvePartialID(ctx, r, moleculeID)
	if err != nil {
		recordProxiedBurnFailure(targets, moleculeID, moleculeID, "failed to resolve", err)
		return
	}
	issue, err := r.GetIssue(ctx, resolvedID)
	if err != nil {
		recordProxiedBurnFailure(targets, resolvedID, moleculeID, "failed to load", err)
		return
	}
	if !issue.Ephemeral {
		targets.persistentIDs = append(targets.persistentIDs, resolvedID)
		return
	}
	subgraph, err := loadTemplateSubgraph(ctx, r, resolvedID)
	if err != nil {
		recordProxiedBurnFailure(targets, resolvedID, moleculeID, "failed to load wisp subgraph", err)
		return
	}
	for _, sgIssue := range subgraph.Issues {
		if sgIssue.Ephemeral && !seenWispIDs[sgIssue.ID] {
			seenWispIDs[sgIssue.ID] = true
			targets.wispIDs = append(targets.wispIDs, sgIssue.ID)
		}
	}
}

func recordProxiedBurnFailure(targets *proxiedBurnTargets, displayID, failureID, action string, err error) {
	if !isJSONOutput() {
		fmt.Fprintf(os.Stderr, "Warning: %s %s: %v\n", action, displayID, err)
	}
	targets.failedResolve = append(targets.failedResolve, failureID)
}

func renderProxiedBurnDryRun(targets proxiedBurnTargets) error {
	if isJSONOutput() {
		return nil
	}
	fmt.Printf("\nDry run: would burn %d wisp(s) and %d persistent molecule(s)\n", len(targets.wispIDs), len(targets.persistentIDs))
	if len(targets.wispIDs) > 0 {
		fmt.Printf("\nWisps to delete:\n")
		for _, id := range targets.wispIDs {
			fmt.Printf("  - %s\n", id)
		}
	}
	if len(targets.persistentIDs) > 0 {
		fmt.Printf("\nPersistent molecules to delete:\n")
		for _, id := range targets.persistentIDs {
			fmt.Printf("  - %s\n", id)
		}
	}
	if len(targets.failedResolve) > 0 {
		fmt.Printf("\nFailed to resolve (%d):\n", len(targets.failedResolve))
		for _, id := range targets.failedResolve {
			fmt.Printf("  - %s\n", id)
		}
	}
	return nil
}

func confirmProxiedBurn(targets proxiedBurnTargets, force bool) bool {
	if force || isJSONOutput() {
		return true
	}
	fmt.Printf("About to burn %d wisp(s) and %d persistent molecule(s)\n", len(targets.wispIDs), len(targets.persistentIDs))
	fmt.Printf("This will permanently delete all molecule data with no digest.\n")
	fmt.Printf("\nContinue? [y/N] ")

	var response string
	_, _ = fmt.Scanln(&response)
	if response == "y" || response == "Y" {
		return true
	}
	fmt.Println("Canceled.")
	return false
}

func burnAndRenderProxiedTargets(ctx context.Context, targets proxiedBurnTargets) error {
	batchResult := burnProxiedTargets(ctx, targets)
	if isJSONOutput() {
		return outputJSON(batchResult)
	}
	fmt.Printf("%s Burned %d molecule(s): %d issues deleted\n", ui.RenderPass("✓"), len(targets.wispIDs)+len(targets.persistentIDs), batchResult.TotalDeleted)
	if batchResult.FailedCount > 0 {
		fmt.Printf("  %d failed\n", batchResult.FailedCount)
	}
	return nil
}

func burnProxiedTargets(ctx context.Context, targets proxiedBurnTargets) BatchBurnResult {
	result := BatchBurnResult{Results: make([]BurnResult, 0), FailedCount: len(targets.failedResolve)}
	if len(targets.wispIDs) > 0 {
		burnResult, err := burnProxiedWisps(ctx, targets.wispIDs)
		if err != nil {
			if !isJSONOutput() {
				fmt.Fprintf(os.Stderr, "Error burning wisps: %v\n", err)
			}
		} else {
			result.TotalDeleted += burnResult.DeletedCount
			result.Results = append(result.Results, *burnResult)
		}
	}
	for _, id := range targets.persistentIDs {
		burnResult, err := burnOneProxiedPersistent(ctx, id)
		if err != nil {
			if !isJSONOutput() {
				fmt.Fprintf(os.Stderr, "Warning: failed to burn %s: %v\n", id, err)
			}
			result.FailedCount++
			continue
		}
		result.TotalDeleted += burnResult.DeletedCount
		result.Results = append(result.Results, burnResult)
	}
	return result
}

func burnProxiedWisps(ctx context.Context, ids []string) (*BurnResult, error) {
	return uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (*BurnResult, string, error) {
		result, err := burnWispsInto(ctx, newUOWMolWriter(uw), ids, getActor())
		if err != nil {
			return nil, "", err
		}
		return result, fmt.Sprintf("bd: mol burn %d wisp(s)", len(ids)), nil
	})
}

func burnOneProxiedPersistent(ctx context.Context, id string) (BurnResult, error) {
	result, err := uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (BurnResult, string, error) {
		subgraph, err := loadTemplateSubgraph(ctx, newUOWMolWriter(uw), id)
		if err != nil {
			return BurnResult{}, "", fmt.Errorf("loading subgraph for %s: %w", id, err)
		}
		issueIDs := make([]string, len(subgraph.Issues))
		for i, issue := range subgraph.Issues {
			issueIDs[i] = issue.ID
		}
		res, err := uw.IssueUseCase().DeleteIssues(ctx, domain.DeleteIssuesParams{
			IDs:                  issueIDs,
			Cascade:              true,
			UpdateTextReferences: true,
		}, getActor())
		if err != nil {
			return BurnResult{}, "", fmt.Errorf("burning %s: %w", id, err)
		}
		return BurnResult{MoleculeID: id, DeletedIDs: issueIDs, DeletedCount: res.DeletedCount},
			fmt.Sprintf("bd: mol burn %s", id), nil
	})
	return result, err
}

func runMolDistillProxiedServer(ctx context.Context, in molDistillInput) error {
	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return err
	}
	defer uw.Close(ctx)

	r := uowMolReader{uw: uw}
	epicID, err := utils.ResolvePartialID(ctx, r, in.epicID)
	if err != nil {
		return HandleErrorRespectJSON("'%s' not found", in.epicID)
	}

	subgraph, err := loadTemplateSubgraph(ctx, r, epicID)
	if err != nil {
		return HandleErrorRespectJSON("loading epic: %v", err)
	}

	return distillSubgraph(epicID, subgraph, in)
}
