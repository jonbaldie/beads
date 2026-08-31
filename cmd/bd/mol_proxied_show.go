package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/utils"
)

func runMolShowProxiedServer(ctx context.Context, arg string, parallel bool) error {
	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return err
	}
	defer uw.Close(ctx)

	r := uowMolReader{uw: uw}
	moleculeID, err := utils.ResolvePartialID(ctx, r, arg)
	if err != nil {
		return HandleErrorRespectJSON("molecule '%s' not found", arg)
	}

	subgraph, err := loadTemplateSubgraph(ctx, r, moleculeID)
	if err != nil {
		return HandleErrorRespectJSON("loading molecule: %v", err)
	}

	if parallel {
		return showMoleculeWithParallel(subgraph)
	}
	return showMolecule(subgraph)
}

func runMolCurrentProxiedServer(ctx context.Context, args []string, agent string, limit int, rangeStr string) error {
	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return err
	}
	defer uw.Close(ctx)

	r := uowMolReader{uw: uw}

	var rangeStart, rangeEnd int
	if rangeStr != "" {
		rangeStart, rangeEnd, err = parseRange(rangeStr)
		if err != nil {
			return HandleErrorRespectJSON("invalid range '%s': %v", rangeStr, err)
		}
	}
	explicitSteps := limit > 0 || rangeStr != ""

	var molecules []*MoleculeProgress
	handled := false
	if len(args) == 1 {
		molecules, handled, err = loadSingleCurrentMolecule(ctx, r, args[0], limit, rangeStart, rangeEnd, rangeStr != "", explicitSteps)
	} else {
		molecules, handled, err = loadCurrentMolecules(ctx, r, agent)
	}
	if err != nil {
		return err
	}
	if handled {
		return nil
	}

	return renderCurrentMolecules(molecules)
}

func renderCurrentMolecules(molecules []*MoleculeProgress) error {
	if isJSONOutput() {
		return outputJSON(molecules)
	}
	for i, mol := range molecules {
		if i > 0 {
			fmt.Println()
		}
		printMoleculeProgress(mol)
	}
	return nil
}

func loadSingleCurrentMolecule(
	ctx context.Context,
	r molReader,
	arg string,
	limit, rangeStart, rangeEnd int,
	hasRange, explicitSteps bool,
) ([]*MoleculeProgress, bool, error) {
	moleculeID, err := utils.ResolvePartialID(ctx, r, arg)
	if err != nil {
		return nil, false, HandleErrorRespectJSON("molecule '%s' not found", arg)
	}
	stats, err := r.GetMoleculeProgress(ctx, moleculeID)
	if err != nil {
		return nil, false, HandleErrorRespectJSON("loading molecule: %v", err)
	}
	if stats.Total > LargeMoleculeThreshold && !explicitSteps && !isJSONOutput() {
		printLargeMoleculeSummary(stats)
		return nil, true, nil
	}

	progress, err := getMoleculeProgress(ctx, r, moleculeID)
	if err != nil {
		return nil, false, HandleErrorRespectJSON("loading molecule: %v", err)
	}
	applyCurrentStepLimit(progress, limit, rangeStart, rangeEnd, hasRange)
	return []*MoleculeProgress{progress}, false, nil
}

func applyCurrentStepLimit(progress *MoleculeProgress, limit, rangeStart, rangeEnd int, hasRange bool) {
	if hasRange {
		progress.Steps = filterStepsByRange(progress.Steps, rangeStart, rangeEnd)
	} else if limit > 0 && len(progress.Steps) > limit {
		progress.Steps = progress.Steps[:limit]
	}
}

func loadCurrentMolecules(ctx context.Context, r molReader, agent string) ([]*MoleculeProgress, bool, error) {
	molecules := findInProgressMolecules(ctx, r, agent)
	if len(molecules) == 0 {
		molecules = findHookedMolecules(ctx, r, agent)
	}
	if len(molecules) > 0 {
		return molecules, false, nil
	}
	if isJSONOutput() {
		return nil, true, outputJSON([]interface{}{})
	}
	fmt.Printf("No molecules in progress")
	if agent != "" {
		fmt.Printf(" for %s", agent)
	}
	fmt.Println(".")
	fmt.Println("\nTo start work on a molecule:")
	fmt.Println("  bd mol wisp create <proto-id>  # Instantiate as ephemeral wisp")
	fmt.Println("  bd update <step-id> --claim  # Claim a step")
	return nil, true, nil
}

func runMolProgressProxiedServer(ctx context.Context, args []string) error {
	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return err
	}
	defer uw.Close(ctx)

	r := uowMolReader{uw: uw}

	var moleculeID string
	if len(args) == 1 {
		resolved, err := utils.ResolvePartialID(ctx, r, args[0])
		if err != nil {
			return HandleErrorRespectJSON("molecule '%s' not found", args[0])
		}
		moleculeID = resolved
	} else {
		moleculeIDs := findInProgressMoleculeIDs(ctx, r, getActor())
		if len(moleculeIDs) == 0 {
			if isJSONOutput() {
				return outputJSON([]interface{}{})
			}
			fmt.Println("No molecules in progress.")
			fmt.Println("\nUse: bd mol progress <molecule-id>")
			return nil
		}
		moleculeID = moleculeIDs[0]
	}

	stats, err := r.GetMoleculeProgress(ctx, moleculeID)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	if isJSONOutput() {
		output := map[string]interface{}{
			"molecule_id":     stats.MoleculeID,
			"molecule_title":  stats.MoleculeTitle,
			"total":           stats.Total,
			"completed":       stats.Completed,
			"in_progress":     stats.InProgress,
			"current_step_id": stats.CurrentStepID,
		}
		if stats.Total > 0 {
			output["percent"] = float64(stats.Completed) * 100 / float64(stats.Total)
		}
		return outputJSON(output)
	}

	printMoleculeProgressStats(stats)
	return nil
}

func runMolLastActivityProxiedServer(ctx context.Context, arg string) error {
	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return err
	}
	defer uw.Close(ctx)

	r := uowMolReader{uw: uw}
	moleculeID, err := utils.ResolvePartialID(ctx, r, arg)
	if err != nil {
		return HandleErrorRespectJSON("molecule '%s' not found", arg)
	}

	activity, err := r.GetMoleculeLastActivity(ctx, moleculeID)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	if isJSONOutput() {
		return outputJSON(activity)
	}
	fmt.Println(activity.LastActivity.UTC().Format(time.RFC3339))
	return nil
}

func runMolStaleProxiedServer(ctx context.Context, blockingOnly, unassignedOnly, showAll bool) error {
	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return err
	}
	defer uw.Close(ctx)

	result, err := findStaleMolecules(ctx, uowMolReader{uw: uw}, blockingOnly, unassignedOnly, showAll)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	if isJSONOutput() {
		return outputJSON(result)
	}
	renderStaleResult(result, blockingOnly)
	return nil
}

func runMolReadyGatedProxiedServer(ctx context.Context) error {
	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return err
	}
	defer uw.Close(ctx)

	molecules, err := findGateReadyMolecules(ctx, uowMolReader{uw: uw})
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	return renderGatedReadyMolecules(molecules)
}
