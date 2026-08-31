package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/validation"
	"github.com/jonbaldie/beads/issueops"
)

func resolveProxiedCustomTypes(dbTypes []string) []string {
	if len(dbTypes) > 0 {
		return dbTypes
	}
	return config.GetCustomTypesFromYAML()
}

func runCreateProxiedServer(cmd *cobra.Command, ctx context.Context, in createInput) error {
	if in.repoOverrideSet {
		return HandleError("--repo is not supported with --proxied-server")
	}
	switch {
	case in.graphFile != "":
		return runCreateProxiedGraph(cmd, ctx, in)
	case in.markdownFile != "":
		return runCreateProxiedMarkdown(cmd, ctx, in)
	default:
		return runCreateProxiedSingle(cmd, ctx, in)
	}
}

func runCreateProxiedSingle(_ *cobra.Command, ctx context.Context, in createInput) error {
	deps, waitsFor, err := prepareCreateProxiedSingle(in)
	if err != nil {
		return err
	}
	if in.dryRun {
		return runCreateProxiedDryRun(ctx, in)
	}
	return runCreateProxiedSingleWrite(ctx, in, deps, waitsFor)
}

func prepareCreateProxiedSingle(in createInput) ([]domain.DependencySpec, *domain.WaitsForSpec, error) {
	if err := runCreateLintIssue(in); err != nil {
		return nil, nil, err
	}
	if in.explicitID != "" {
		if _, err := validation.ValidateIDFormat(in.explicitID); err != nil {
			return nil, nil, HandleError("%v", err)
		}
	}
	deps, err := parseDepSpecs(in.deps)
	if err != nil {
		return nil, nil, HandleError("%v", err)
	}
	waitsFor, err := buildWaitsFor(in.waitsFor, in.waitsForGate, in.waitsForGateSet)
	if err != nil {
		return nil, nil, HandleError("%v", err)
	}
	return deps, waitsFor, nil
}

func runCreateProxiedDryRun(ctx context.Context, in createInput) error {
	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}
	previewLabels, err := createProxiedDryRunLabels(ctx, in)
	if err != nil {
		return err
	}
	previewIssue := buildCreateIssueFromInput(in)
	if in.jsonOutput {
		if err := outputJSON(previewIssue); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return nil
	}
	renderCreateDryRunPreview(previewIssue, previewLabels, in.deps)
	return nil
}

func createProxiedDryRunLabels(ctx context.Context, in createInput) ([]string, error) {
	if in.parentID == "" {
		return in.labels, nil
	}
	dryUW, err := getUOWProvider().NewUOW(ctx)
	if err != nil {
		return nil, HandleError("open unit of work: %v", err)
	}
	defer dryUW.Close(ctx)
	if _, err := dryUW.IssueUseCase().GetIssue(ctx, in.parentID); err != nil {
		return nil, HandleError("parent issue %s not found: %v", in.parentID, err)
	}
	if in.noInheritLabels {
		return in.labels, nil
	}
	// A READ inside the DRY-RUN unit of work, which is opened only to
	// be discarded: this previews what --parent would inherit without
	// creating anything. The role that answers it for real is
	// CreateRequest.InheritLabelsFromParent, which resolves the parent's
	// labels inside the create it is part of — and a preview has no
	// create to be part of. A dry-run mode on the create role is the
	// follow-up (ga-2ltro.12).
	inherited, lerr := dryUW.LabelUseCase().GetLabels(ctx, in.parentID) //nolint:forbidigo // dry-run preview; the role resolves this only inside a real create
	if lerr != nil {
		return nil, HandleError("dry-run inherit labels: %v", lerr)
	}
	return mergeCreateLabels(in.labels, inherited), nil
}

func runCreateProxiedSingleWrite(ctx context.Context, in createInput, deps []domain.DependencySpec, waitsFor *domain.WaitsForSpec) error {
	ops, err := proxiedIssueLifecycle()
	if err != nil {
		return HandleError("%v", err)
	}

	issue := buildCreateIssueFromInput(in)
	// Labels ride on the issue because that is where the contract reads them:
	// CreateRequest.Issue documents them as authoritative.
	issue.Labels = append([]string(nil), in.labels...)
	if err := inheritProxiedSourceRepo(ctx, issue, deps); err != nil {
		return err
	}

	// SPEC-GAP bd-yby99.32: Lifecycle.Create promises nothing about the
	// version-control entry a create records and CreateRequest carries no
	// Provenance to spell one, so this route's commit message moves from
	// "bd: create <id>" to whatever the implementation defaults to.
	result, err := ops.Create(ctx, issueops.CreateRequest{
		Actor:                   in.createdBy,
		Issue:                   issue,
		ParentID:                in.parentID,
		InheritLabelsFromParent: inheritLabelsFromParent(in),
		Dependencies:            createDependencyRequests(deps),
		WaitsFor:                waitsForRequest(waitsFor),
		ForceIDPrefix:           in.force,
		// The workspace's config.yaml prefix wins over the server database's,
		// and only this side can see it. Without this the proxied route mints
		// ids the workspace's own configuration forbids and the direct route
		// refuses.
		IDPrefix: createIDPrefixOverride(),
	})
	if err != nil {
		// RULING R1, reported the same way the direct route reports it: an
		// occupied --id is a refusal, not a silent full-row upsert dressed up
		// as success.
		if errors.Is(err, storage.ErrAlreadyExists) && in.explicitID != "" {
			return HandleErrorRespectJSON("%s already exists; use bd update, or bd import for upsert semantics", in.explicitID)
		}
		return HandleError("%v", err)
	}
	// Every post-write read comes from the contract's result snapshot: the
	// local struct still has no ID for an auto-minted create. Dependencies and
	// comments come off because `bd create` has never printed them.
	created := result.Issue
	created.Dependencies = nil
	created.Comments = nil
	printCreateProxiedSingle(in, created)
	return nil
}

func inheritLabelsFromParent(in createInput) bool {
	return !in.noInheritLabels && in.parentID != ""
}

func printCreateProxiedSingle(in createInput, created *types.Issue) {
	switch {
	case in.jsonOutput:
		if err := outputJSON(created); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
	case in.silent:
		fmt.Println(created.ID)
	default:
		fmt.Printf("%s Created issue: %s\n", ui.RenderPass("✓"), formatFeedbackID(created.ID, created.Title))
		fmt.Printf("  Priority: P%d\n", created.Priority)
		fmt.Printf("  Status: %s\n", created.Status)
	}
}

// inheritProxiedSourceRepo copies a discovered-from parent's source repo onto
// the new issue, which is what the direct route does before it calls the role
// (cmd/bd/create.go).
//
// A failed lookup is not a verdict. The direct route ignores one and creates
// with the default source repo; a genuinely absent target is refused by the
// create itself, with the contract's own error for a dangling edge.
func inheritProxiedSourceRepo(ctx context.Context, issue *types.Issue, deps []domain.DependencySpec) error {
	// Reuse the already-parsed specs (not the raw --deps strings) so this
	// can't drift from parseDepSpec's normalization rules.
	parentID := discoveredFromParentSpec(deps)
	if parentID == "" {
		return nil
	}
	rd, err := proxiedIssueReader()
	if err != nil {
		return HandleError("%v", err)
	}
	details, err := rd.Get(ctx, issueops.GetRequest{ID: parentID})
	if err != nil {
		return nil
	}
	if details.Issue.SourceRepo != "" {
		issue.SourceRepo = details.Issue.SourceRepo
	}
	return nil
}

func runCreateLintIssue(in createInput) error {
	if in.validationMode != "error" && in.validationMode != "warn" {
		return nil
	}
	lintIssue := &types.Issue{
		IssueContent: types.IssueContent{
			Description:        in.description,
			AcceptanceCriteria: in.acceptanceCriteria,
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: types.IssueType(in.issueType).Normalize(),
		},
	}
	if err := validation.LintIssue(lintIssue); err != nil {
		if in.validationMode == "error" {
			return HandleError("%v", err)
		}
		fmt.Fprintf(os.Stderr, "%s %v\n", ui.RenderWarn("⚠"), err)
	}
	return nil
}

func buildCreateIssueFromInput(in createInput) *types.Issue {
	return buildCreateIssue(createIssueParams{
		ident: createIssueIdentity{
			ID:          in.explicitID,
			Title:       in.title,
			SpecID:      in.specID,
			Assignee:    in.assignee,
			ExternalRef: in.externalRef,
			CreatedBy:   in.createdBy,
			Owner:       in.owner,
		},
		body: createIssueBody{
			Description:        in.description,
			Design:             in.design,
			AcceptanceCriteria: in.acceptanceCriteria,
			Notes:              in.notes,
			Metadata:           in.metadata,
		},
		class: createIssueClass{
			Priority:      in.priority,
			IssueType:     types.IssueType(in.issueType).Normalize(),
			Ephemeral:     in.ephemeral,
			NoHistory:     in.noHistory,
			MolType:       in.molType,
			WispType:      in.wispType,
			InitialStatus: in.status,
		},
		schedule: createIssueSchedule{
			EstimatedMinutes: in.estimatedMinutes,
			DueAt:            in.dueAt,
			DeferUntil:       in.deferUntil,
		},
		event: createIssueEvent{
			EventKind: in.eventCategory,
			Actor:     in.eventActor,
			Target:    in.eventTarget,
			Payload:   in.eventPayload,
		},
	})
}

// runCreateProxiedMarkdown creates every issue in a markdown file as ONE act,
// through issueops.BatchCreator.
func runCreateProxiedMarkdown(_ *cobra.Command, ctx context.Context, in createInput) error {
	templates, err := parseMarkdownFile(in.markdownFile)
	if err != nil {
		return HandleError("parsing markdown file: %v", err)
	}
	if len(templates) == 0 {
		return HandleError("no issues found in markdown file")
	}
	request, err := buildMarkdownBatchRequest(templates, in)
	if err != nil {
		return err
	}
	creator, err := proxiedBatchCreator()
	if err != nil {
		return HandleError("%v", err)
	}
	result, err := creator.CreateBatch(ctx, request)
	if err != nil {
		return HandleError("creating issues from markdown: %v", err)
	}
	return reportMarkdownBatch(result.Issues, in)
}

// proxiedBatchCreator reaches the batch-create role through the provider's own
// capability accessor, which is where each decorator adds its layer.
func proxiedBatchCreator() (issueops.BatchCreator, error) {
	if getUOWProvider() == nil {
		return nil, errors.New("proxied-server UOW provider not initialized")
	}
	src, ok := getUOWProvider().(uow.BatchCreatorSource)
	if !ok {
		return nil, fmt.Errorf("proxied-server provider %T does not offer the batch-create surface", getUOWProvider())
	}
	return src.BatchCreator()
}

func runCreateProxiedGraph(_ *cobra.Command, ctx context.Context, in createInput) error {
	plan, err := loadCreateGraphPlan(in)
	if err != nil {
		return err
	}
	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}
	if in.dryRun {
		return runCreateProxiedGraphDryRun(ctx, in, &plan)
	}
	return runCreateProxiedGraphWrite(ctx, in, plan)
}

func loadCreateGraphPlan(in createInput) (GraphApplyPlan, error) {
	var plan GraphApplyPlan
	data, err := os.ReadFile(in.graphFile) // #nosec G304 -- user-provided path is intentional
	if err != nil {
		return plan, HandleError("reading graph plan: %v", err)
	}
	if unknown := detectUnknownGraphFields(data); len(unknown) > 0 {
		warnUnknownGraphFields(os.Stderr, unknown)
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		return plan, HandleError("parsing graph plan: %v", err)
	}
	return plan, nil
}

func runCreateProxiedGraphDryRun(ctx context.Context, in createInput, plan *GraphApplyPlan) error {
	dryUW, err := getUOWProvider().NewUOW(ctx)
	if err != nil {
		return HandleError("open unit of work: %v", err)
	}
	cctx, err := dryUW.ConfigUseCase().LoadCreateContext(ctx)
	if err != nil {
		dryUW.Close(ctx)
		return HandleError("load create context: %v", err)
	}
	// Keep the UOW open through validation: the explicit-ID collision
	// preflight reads through it.
	_, err = validateProxiedGraphPlan(plan, in, cctx, uowIssueExists(ctx, dryUW))
	dryUW.Close(ctx)
	if err != nil {
		return HandleError("invalid graph plan: %v", err)
	}
	if err := emitGraphApplyDryRun(plan, in.graphApplyOptions()); err != nil {
		return HandleError("%v", err)
	}
	return nil
}

func runCreateProxiedGraphWrite(ctx context.Context, in createInput, plan GraphApplyPlan) error {
	domainPlan, err := buildDomainGraphPlan(plan, in)
	if err != nil {
		return err
	}

	commitMsg := plan.CommitMessage
	if commitMsg == "" {
		commitMsg = fmt.Sprintf("bd: graph-apply %d nodes", len(plan.Nodes))
	}

	res, err := uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (map[string]string, string, error) {
		return applyCreateProxiedGraphTx(ctx, uw, in, plan, domainPlan, commitMsg)
	})
	if err != nil {
		return HandleError("%v", err)
	}
	return printCreateProxiedGraphResult(in, res)
}

func applyCreateProxiedGraphTx(ctx context.Context, uw uow.UnitOfWork, in createInput, plan GraphApplyPlan, domainPlan domain.GraphPlan, commitMsg string) (map[string]string, string, error) {
	cctx, err := uw.ConfigUseCase().LoadCreateContext(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("load create context: %w", err)
	}
	// validateProxiedGraphPlan enforces a uniform storage class, so its
	// resolved useWisp decides which table the whole plan routes to. The
	// collision preflight runs inside this transaction, so it cannot race
	// a concurrent create of the same explicit ID.
	useWisp, err := validateProxiedGraphPlan(&plan, in, cctx, uowIssueExists(ctx, uw))
	if err != nil {
		return nil, "", fmt.Errorf("invalid graph plan: %w", err)
	}
	result, applyErr := applyCreateProxiedGraphPlan(ctx, uw, in, domainPlan, useWisp)
	if applyErr != nil {
		return nil, "", fmt.Errorf("graph create: %w", applyErr)
	}
	return result.IDs, commitMsg, nil
}

func applyCreateProxiedGraphPlan(ctx context.Context, uw uow.UnitOfWork, in createInput, domainPlan domain.GraphPlan, useWisp bool) (domain.GraphApplyResult, error) {
	if useWisp {
		return uw.IssueUseCase().ApplyWispGraph(ctx, domainPlan, in.createdBy)
	}
	return uw.IssueUseCase().ApplyIssueGraph(ctx, domainPlan, in.createdBy)
}

func printCreateProxiedGraphResult(in createInput, res map[string]string) error {
	if in.jsonOutput {
		if err := outputJSON(GraphApplyResult{IDs: res}); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return nil
	}
	fmt.Printf("Created %d issues\n", len(res))
	keys := make([]string, 0, len(res))
	for k := range res {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %s -> %s\n", k, res[k])
	}
	return nil
}

// validateProxiedGraphPlan runs full plan validation for proxied-server mode:
// shared plan checks, uniform storage class (proxied routes the whole plan to
// one table), explicit-ID prefix checks against the server's config, and the
// explicit-ID collision preflight through the unit of work's issue lookup.
// The returned useWisp is the plan-wide table routing decision.
func validateProxiedGraphPlan(plan *GraphApplyPlan, in createInput, cctx domain.CreateContext, issueExists func(id string) (bool, error)) (useWisp bool, err error) {
	cfg := graphPlanConfig{
		customTypes: resolveProxiedCustomTypes(cctx.CustomTypes),
		// No YAML fallback for statuses — the server database is authoritative
		// (that's where 'bd config set status.custom' writes) and statuses are
		// store-only everywhere (single-issue create, list filters), unlike
		// custom types.
		customStatuses:  types.CustomStatusNames(cctx.CustomStatuses),
		dbPrefix:        overlayYAMLPrefix(cctx.IssuePrefix),
		allowedPrefixes: cctx.AllowedPrefixes,
		issueExists:     issueExists,
	}
	return validateFullGraphPlan(plan, cfg, in.graphApplyOptions(), true)
}

// uowIssueExists adapts a unit of work's issue lookups to the plan
// validator's explicit-ID collision probe, bound to the caller's context so
// in-transaction validation reads its own transaction. Issues and wisps share
// one ID space but the domain getters are per-table, so probe both.
func uowIssueExists(ctx context.Context, uw uow.UnitOfWork) func(id string) (bool, error) {
	isNotFound := func(err error) bool {
		return errors.Is(err, storage.ErrNotFound) || errors.Is(err, sql.ErrNoRows)
	}
	return func(id string) (bool, error) {
		if _, err := uw.IssueUseCase().GetIssue(ctx, id); err == nil {
			return true, nil
		} else if !isNotFound(err) {
			return false, err
		}
		if _, err := uw.IssueUseCase().GetWisp(ctx, id); err == nil {
			return true, nil
		} else if !isNotFound(err) {
			return false, err
		}
		return false, nil
	}
}

// graphApplyNodeIssue path (full issue-model parity with `bd create`).
func buildDomainGraphPlan(plan GraphApplyPlan, in createInput) (domain.GraphPlan, error) {
	opts := in.graphApplyOptions()
	nodes := make([]domain.GraphNode, 0, len(plan.Nodes))
	for _, n := range plan.Nodes {
		issue, err := graphApplyNodeIssue(n, opts, in.createdBy, in.owner)
		if err != nil {
			return domain.GraphPlan{}, fmt.Errorf("invalid graph plan: %w", err)
		}
		deps := make([]domain.GraphNodeDep, 0, len(n.Deps))
		for _, d := range n.Deps {
			deps = append(deps, domain.GraphNodeDep{
				Type:   graphApplyDependencyType(d.Type),
				Target: d.Target,
			})
		}
		nodes = append(nodes, domain.GraphNode{
			Key:               n.Key,
			Issue:             issue,
			ParentKey:         n.effectiveParentKey(),
			ParentID:          n.ParentID,
			Assignee:          n.Assignee,
			AssignAfterCreate: n.AssignAfterCreate,
			MetadataRefs:      n.MetadataRefs,
			Labels:            n.Labels,
			Deps:              deps,
		})
	}
	edges := make([]domain.GraphEdge, 0, len(plan.Edges))
	for _, e := range plan.Edges {
		edges = append(edges, domain.GraphEdge{
			FromKey:    e.FromKey,
			FromID:     e.FromID,
			ToKey:      e.ToKey,
			ToID:       e.ToID,
			Type:       graphApplyDependencyType(e.Type),
			Gate:       e.Gate,
			SpawnerKey: e.SpawnerKey,
			SpawnerID:  e.SpawnerID,
			ThreadID:   e.ThreadID,
		})
	}
	return domain.GraphPlan{Nodes: nodes, Edges: edges}, nil
}
