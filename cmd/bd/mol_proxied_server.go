package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jonbaldie/beads/internal/formula"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/utils"
)

type pourProxiedResult struct {
	spawn         *InstantiateResult
	totalAttached int
	attachCount   int
}

type pourAttachmentInfo struct {
	id       string
	issue    *types.Issue
	subgraph *TemplateSubgraph
}

func runPourProxiedServer(ctx context.Context, in pourInput) error {
	if err := CheckReadonly("pour"); err != nil {
		return err
	}

	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}

	vars, err := parseVarFlags(in.varFlags)
	if err != nil {
		return HandleError("%v", err)
	}

	formulaSubgraph, formulaProtoID, err := resolvePourProxiedFormula(in.protoArg, in.varFlags, vars)
	if err != nil {
		return HandleError("%v", err)
	}

	res, err := uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (pourProxiedResult, string, error) {
		return executePourProxiedTransaction(ctx, uw, in, vars, formulaSubgraph, formulaProtoID)
	})
	if err != nil {
		return HandleError("%v", err)
	}
	if res.spawn == nil {
		return nil
	}

	return renderPourResult(res.spawn, res.totalAttached, res.attachCount)
}

func resolvePourProxiedFormula(protoArg string, varFlags []string, vars map[string]string) (*TemplateSubgraph, string, error) {
	subgraph, err := resolveAndCookFormulaWithVars(protoArg, nil, vars)
	if err == nil {
		if subgraph.Phase == "vapor" {
			warnPourVaporFormula(protoArg, varFlags)
		}
		return subgraph, subgraph.Root.ID, nil
	}
	if errors.Is(err, formula.ErrVarValidation) {
		// in.protoArg IS a formula; report invalid variables directly instead of
		// masking the formula error as an unknown formula or proto ID.
		return nil, "", err
	}
	return nil, "", nil
}

func executePourProxiedTransaction(
	ctx context.Context,
	uw uow.UnitOfWork,
	in pourInput,
	vars map[string]string,
	formulaSubgraph *TemplateSubgraph,
	formulaProtoID string,
) (pourProxiedResult, string, error) {
	w := newUOWMolWriter(uw)
	subgraph, protoID, err := loadPourProxiedTemplate(ctx, w, in.protoArg, formulaSubgraph, formulaProtoID)
	if err != nil {
		return pourProxiedResult{}, "", err
	}
	attachments, err := loadPourProxiedAttachments(ctx, w, in.attachArgs)
	if err != nil {
		return pourProxiedResult{}, "", err
	}

	vars = applyVariableDefaults(vars, subgraph)
	attachSubgraphs := make([]*TemplateSubgraph, 0, len(attachments))
	for _, attachment := range attachments {
		attachSubgraphs = append(attachSubgraphs, attachment.subgraph)
	}
	if err := checkPourVars(subgraph, attachSubgraphs, vars); err != nil {
		return pourProxiedResult{}, "", err
	}
	if in.dryRun {
		previews := make([]pourAttachPreview, 0, len(attachments))
		for _, attachment := range attachments {
			previews = append(previews, pourAttachPreview{title: attachment.issue.Title, steps: len(attachment.subgraph.Issues)})
		}
		renderPourDryRun(protoID, subgraph, vars, in.assignee, in.attachType, previews)
		return pourProxiedResult{}, "", nil
	}

	spawnResult, err := cloneSubgraphInto(ctx, w, subgraph, CloneOptions{
		Vars:     vars,
		Assignee: in.assignee,
		Actor:    getActor(),
		Prefix:   types.IDPrefixMol,
	})
	if err != nil {
		return pourProxiedResult{}, "", fmt.Errorf("pouring proto: %w", err)
	}
	totalAttached, err := attachPourProxied(ctx, w, spawnResult, attachments, in, vars)
	if err != nil {
		return pourProxiedResult{}, "", err
	}
	return pourProxiedResult{spawn: spawnResult, totalAttached: totalAttached, attachCount: len(attachments)},
		fmt.Sprintf("bd: mol pour %s", protoID), nil
}

func loadPourProxiedTemplate(ctx context.Context, w molWriter, protoArg string, subgraph *TemplateSubgraph, protoID string) (*TemplateSubgraph, string, error) {
	if subgraph != nil {
		return subgraph, protoID, nil
	}
	resolvedID, err := utils.ResolvePartialID(ctx, w, protoArg)
	if err != nil {
		return nil, "", fmt.Errorf("%s not found as formula or proto ID", protoArg)
	}
	protoIssue, err := w.GetIssue(ctx, resolvedID)
	if err != nil {
		return nil, "", fmt.Errorf("loading proto %s: %w", resolvedID, err)
	}
	if !isProto(protoIssue) {
		return nil, "", fmt.Errorf("%s is not a proto (missing '%s' label)", resolvedID, MoleculeLabel)
	}
	subgraph, err = loadTemplateSubgraph(ctx, w, resolvedID)
	if err != nil {
		return nil, "", fmt.Errorf("loading proto: %w", err)
	}
	return subgraph, resolvedID, nil
}

func loadPourProxiedAttachments(ctx context.Context, w molReader, attachArgs []string) ([]pourAttachmentInfo, error) {
	attachments := make([]pourAttachmentInfo, 0, len(attachArgs))
	for _, attachArg := range attachArgs {
		attachID, err := utils.ResolvePartialID(ctx, w, attachArg)
		if err != nil {
			return nil, fmt.Errorf("resolving attachment ID %s: %w", attachArg, err)
		}
		attachIssue, err := w.GetIssue(ctx, attachID)
		if err != nil {
			return nil, fmt.Errorf("loading attachment %s: %w", attachID, err)
		}
		if !isProto(attachIssue) {
			return nil, fmt.Errorf("%s is not a proto (missing '%s' label)", attachID, MoleculeLabel)
		}
		attachSubgraph, err := loadTemplateSubgraph(ctx, w, attachID)
		if err != nil {
			return nil, fmt.Errorf("loading attachment subgraph %s: %w", attachID, err)
		}
		attachments = append(attachments, pourAttachmentInfo{id: attachID, issue: attachIssue, subgraph: attachSubgraph})
	}
	return attachments, nil
}

func attachPourProxied(ctx context.Context, w molWriter, spawnResult *InstantiateResult, attachments []pourAttachmentInfo, in pourInput, vars map[string]string) (int, error) {
	if len(attachments) == 0 {
		return 0, nil
	}
	spawnedMol, err := w.GetIssue(ctx, spawnResult.NewEpicID)
	if err != nil {
		return 0, fmt.Errorf("loading spawned mol: %w", err)
	}
	totalAttached := 0
	for _, attachment := range attachments {
		bondResult, err := bondProtoMolAttachInto(ctx, w, attachment.subgraph, attachment.issue, spawnedMol, newBondAttachmentOptions(in.attachType, vars, "", getActor(), false, true))
		if err != nil {
			return 0, fmt.Errorf("attaching %s: %w", attachment.id, err)
		}
		totalAttached += bondResult.Spawned
	}
	return totalAttached, nil
}
