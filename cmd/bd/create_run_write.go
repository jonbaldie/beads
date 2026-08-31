package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/remotecache"
	"github.com/jonbaldie/beads/internal/routing"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/validation"
	"github.com/jonbaldie/beads/issueops"
	"github.com/spf13/cobra"
)

type createDirectState struct {
	cmd      *cobra.Command
	ident    createDirectIdent
	body     createDirectBody
	ids      createDirectIDs
	class    createDirectClass
	event    createDirectEvent
	schedule createDirectSchedule
	repo     createDirectRepo
}

type createDirectIdent struct {
	title, description    string
	silent, dryRun, force bool
	wisp, noHistory       bool
}

type createDirectBody struct {
	design, acceptance, notes, specID string
	issueType, assignee, status       string
	priority                          int
	labels                            []string
}

type createDirectIDs struct {
	explicit, parent, externalRef string
	deps                          []string
	waitsFor, waitsForGate        string
}

type createDirectClass struct {
	storageClass types.StorageClass
	molType      types.MolType
	wispType     types.WispType
}

type createDirectEvent struct {
	category, actor, target, payload string
}

type createDirectSchedule struct {
	dueAt, deferUntil *time.Time
	estimatedMinutes  *int
	metadata          json.RawMessage
}

type createDirectRepo struct {
	override, path string
	cache          *remotecache.Cache
	target         storage.DoltStorage
}

func (st *createDirectState) issueParams() createIssueParams {
	return createIssueParams{
		ident: createIssueIdentity{
			ID:          st.ids.explicit,
			Title:       st.ident.title,
			SpecID:      st.body.specID,
			Assignee:    st.body.assignee,
			ExternalRef: st.ids.externalRef,
			CreatedBy:   getActorWithGit(),
			Owner:       getOwner(),
		},
		body: createIssueBody{
			Description:        st.ident.description,
			Design:             st.body.design,
			AcceptanceCriteria: st.body.acceptance,
			Notes:              st.body.notes,
			Labels:             st.body.labels,
			Metadata:           st.schedule.metadata,
		},
		class: createIssueClass{
			Priority:      st.body.priority,
			IssueType:     types.IssueType(st.body.issueType).Normalize(),
			Ephemeral:     st.ident.wisp,
			NoHistory:     st.ident.noHistory,
			StorageClass:  st.class.storageClass,
			MolType:       st.class.molType,
			WispType:      st.class.wispType,
			InitialStatus: st.body.status,
		},
		schedule: createIssueSchedule{
			EstimatedMinutes: st.schedule.estimatedMinutes,
			DueAt:            st.schedule.dueAt,
			DeferUntil:       st.schedule.deferUntil,
		},
		event: createIssueEvent{
			EventKind: st.event.category,
			Actor:     st.event.actor,
			Target:    st.event.target,
			Payload:   st.event.payload,
		},
	}
}

func resolveCreateDirectRepo(st *createDirectState) {
	st.repo.path = "."
	if st.cmd.Flags().Changed("repo") {
		st.repo.path = st.repo.override
		return
	}
	userRole, err := routing.DetectUserRole(".")
	if err != nil {
		debug.Logf("Warning: failed to detect user role: %v\n", err)
	}
	st.repo.path = routing.DetermineTargetRepo(createDirectRoutingConfig(st.repo.override), userRole, ".")
}

func createDirectRoutingConfig(repoOverride string) *routing.RoutingConfig {
	routingMode := getRoutingConfigValue(getRootContext(), getStore(), "routing.mode")
	contributorRepo := getRoutingConfigValue(getRootContext(), getStore(), "routing.contributor")
	if routingMode == "" && getRoutingConfigValue(getRootContext(), getStore(), "contributor.auto_route") == "true" {
		routingMode = "auto"
	}
	if contributorRepo == "" {
		contributorRepo = getRoutingConfigValue(getRootContext(), getStore(), "contributor.planning_repo")
	}
	return &routing.RoutingConfig{
		Mode:             routingMode,
		DefaultRepo:      getRoutingConfigValue(getRootContext(), getStore(), "routing.default"),
		MaintainerRepo:   getRoutingConfigValue(getRootContext(), getStore(), "routing.maintainer"),
		ContributorRepo:  contributorRepo,
		ExplicitOverride: repoOverride,
	}
}

func renderCreateDirectDryRun(st *createDirectState) error {
	previewIssue := buildCreateIssue(st.issueParams())
	if isJSONOutput() {
		return outputJSON(previewIssue)
	}
	renderCreateDryRunPreview(previewIssue, st.body.labels, st.ids.deps)
	return nil
}

func maybeOpenCreateTargetStore(st *createDirectState) error {
	if st.ident.dryRun || st.repo.path == "." {
		return nil
	}
	targetStore, cache, err := openCreateTargetStore(st.repo.path)
	if err != nil {
		return err
	}
	st.repo.cache = cache
	st.repo.target = targetStore
	if getStore() != nil {
		_ = getStore().Close()
	}
	setStore(targetStore)
	return nil
}

func openCreateTargetStore(repoPath string) (storage.DoltStorage, *remotecache.Cache, error) {
	if remotecache.IsRemoteURL(repoPath) {
		return openCreateRemoteStore(repoPath)
	}
	targetBeadsDir := routing.ExpandPath(repoPath)
	debug.Logf("DEBUG: Routing to target repo: %s\n", targetBeadsDir)
	if err := ensureBeadsDirForPath(getRootContext(), targetBeadsDir, getStore()); err != nil {
		return nil, nil, HandleError("failed to initialize target repo: %v", err)
	}
	targetStore, err := newDoltStoreFromConfig(getRootContext(), filepath.Join(targetBeadsDir, ".beads"))
	if err != nil {
		return nil, nil, HandleError("failed to open target store: %v", err)
	}
	return targetStore, nil, nil
}

func openCreateRemoteStore(repoPath string) (storage.DoltStorage, *remotecache.Cache, error) {
	remoteCache, err := remotecache.DefaultCache()
	if err != nil {
		return nil, nil, HandleError("failed to initialize remote cache: %v", err)
	}
	if _, err := remoteCache.Ensure(getRootContext(), repoPath); err != nil {
		return nil, nil, HandleError("failed to sync remote %s: %v", repoPath, err)
	}
	targetStore, err := remoteCache.OpenStore(getRootContext(), repoPath, newDoltStoreFromConfig)
	if err != nil {
		return nil, nil, HandleError("failed to open remote store: %v", err)
	}
	return targetStore, remoteCache, nil
}

func applyCreateDirectParent(st *createDirectState) (func(), error) {
	if st.ids.explicit != "" && st.ids.parent != "" {
		return nil, HandleError("cannot specify both --id and --parent flags")
	}
	parentLookup := getStore()
	closer := func() {}
	if st.ident.dryRun && st.repo.path != "." {
		var err error
		parentLookup, err = openDryRunTargetStore(getRootContext(), st.repo.path)
		if err != nil {
			return nil, HandleError("%v", err)
		}
		lookup := parentLookup
		closer = func() { _ = lookup.Close() }
	}
	if err := inheritCreateDirectParentLabels(st, parentLookup); err != nil {
		closer()
		return nil, err
	}
	return closer, nil
}

func inheritCreateDirectParentLabels(st *createDirectState, parentLookup storage.DoltStorage) error {
	if st.ids.parent == "" {
		return nil
	}
	ctx := getRootContext()
	if _, err := parentLookup.GetIssue(ctx, st.ids.parent); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return HandleError("parent issue %s not found", st.ids.parent)
		}
		return HandleError("failed to check parent issue: %v", err)
	}
	noInheritLabels, _ := st.cmd.Flags().GetBool("no-inherit-labels")
	var inheritedLabels []string
	if !noInheritLabels {
		inheritedLabels, _ = parentLookup.GetLabels(ctx, st.ids.parent)
	}
	st.body.labels = mergeCreateLabels(st.body.labels, inheritedLabels)
	return nil
}

func writeCreateDirect(st *createDirectState) error {
	depSpecs, err := parseDepSpecs(st.ids.deps)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	waitsForSpec, err := buildWaitsFor(st.ids.waitsFor, st.ids.waitsForGate, st.cmd.Flags().Changed("waits-for-gate"))
	if err != nil {
		return HandleError("%v", err)
	}
	createCtx, err := reserveCreateDirectChild(st)
	if err != nil {
		return err
	}
	if err := validateCreateDirectID(st); err != nil {
		return err
	}
	return commitCreateDirectIssue(st, createCtx, depSpecs, waitsForSpec)
}

func reserveCreateDirectChild(st *createDirectState) (context.Context, error) {
	createCtx := getRootContext()
	if st.ids.parent == "" {
		return createCtx, nil
	}
	childID, err := getStore().GetNextChildID(getRootContext(), st.ids.parent)
	if err != nil {
		return nil, HandleError("%v", err)
	}
	st.ids.explicit = childID
	return storage.WithReservedChildCounter(createCtx, st.ids.parent, childID), nil
}

func validateCreateDirectID(st *createDirectState) error {
	if st.ids.explicit == "" {
		return nil
	}
	if _, err := validation.ValidateIDFormat(st.ids.explicit); err != nil {
		return HandleError("%v", err)
	}
	dbPrefix, allowedPrefixes := loadEmbeddedIDPrefixes()
	if err := validation.ValidateIDPrefixAllowed(st.ids.explicit, dbPrefix, allowedPrefixes, st.ident.force); err != nil {
		return HandleError("%v", err)
	}
	return nil
}

func commitCreateDirectIssue(st *createDirectState, ctx context.Context, depSpecs []domain.DependencySpec, waitsForSpec *domain.WaitsForSpec) error {
	issue := buildCreateIssue(st.issueParams())
	depSpecs, err := resolveCreateDirectDeps(ctx, depSpecs, issue)
	if err != nil {
		return err
	}
	created, err := createDirectViaOps(ctx, st, issue, depSpecs, waitsForSpec)
	if err != nil {
		return err
	}
	return finishCreateDirect(st, ctx, created, depSpecs, waitsForSpec)
}

func resolveCreateDirectDeps(ctx context.Context, depSpecs []domain.DependencySpec, issue *issueops.Issue) ([]domain.DependencySpec, error) {
	resolved, err := resolveDepSpecTargets(ctx, getStore(), depSpecs)
	if err != nil {
		return nil, HandleErrorRespectJSON("%v", err)
	}
	if dfParent := discoveredFromParentSpec(resolved); dfParent != "" {
		parentIssue, err := getStore().GetIssue(ctx, dfParent)
		if err == nil && parentIssue.SourceRepo != "" {
			issue.SourceRepo = parentIssue.SourceRepo
		}
	}
	return resolved, nil
}

func createDirectViaOps(ctx context.Context, st *createDirectState, issue *issueops.Issue, depSpecs []domain.DependencySpec, waitsForSpec *domain.WaitsForSpec) (*issueops.Issue, error) {
	ops, err := writeOps(getStore())
	if err != nil {
		return nil, HandleErrorRespectJSON("%v", err)
	}
	opsCtx, err := issueOpsContext(ctx)
	if err != nil {
		return nil, HandleErrorRespectJSON("%v", err)
	}
	result, err := ops.Create(opsCtx, issueops.CreateRequest{
		Actor:         getActor(),
		Issue:         issue,
		ParentID:      st.ids.parent,
		Dependencies:  createDependencyRequests(depSpecs),
		WaitsFor:      waitsForRequest(waitsForSpec),
		ForceIDPrefix: st.ident.force,
		IDPrefix:      createIDPrefixOverride(),
	})
	if err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) && st.ids.explicit != "" {
			return nil, HandleErrorRespectJSON("%s already exists; use bd update, or bd import for upsert semantics", st.ids.explicit)
		}
		return nil, HandleErrorRespectJSON("%v", err)
	}
	created := result.Issue
	created.Dependencies = nil
	created.Comments = nil
	return created, nil
}

func finishCreateDirect(st *createDirectState, ctx context.Context, created *issueops.Issue, depSpecs []domain.DependencySpec, waitsForSpec *domain.WaitsForSpec) error {
	edges := createDepEdges{parentID: st.ids.parent, specs: depSpecs, waitsFor: waitsForSpec}
	if err := maybeCommitBareCreateDirect(ctx, created, edges); err != nil {
		return err
	}
	if st.repo.path != "." && st.repo.target != nil {
		if err := commitPendingIfEmbedded(ctx, st.repo.target, getActor(), doltAutoCommitParams{
			Command:  "create",
			IssueIDs: []string{created.ID},
		}); err != nil {
			debug.Logf("warning: failed to commit routed repo: %v", err)
		}
	}
	if st.repo.cache != nil {
		if pushErr := st.repo.cache.Push(getRootContext(), st.repo.path); pushErr != nil {
			return HandleError("failed to push to %s: %v\nThe issue was created locally but not synced to the remote.", st.repo.path, pushErr)
		}
	}
	return printCreateDirectResult(created, st.ident.silent)
}

func maybeCommitBareCreateDirect(ctx context.Context, created *issueops.Issue, edges createDepEdges) error {
	if !edges.empty() {
		return nil
	}
	shouldCommit, err := shouldCommitCreatePostWrites(created, false)
	if err != nil {
		return HandleError("dolt auto-commit failed: %v", err)
	}
	if !shouldCommit {
		return nil
	}
	commitMsg := fmt.Sprintf("bd: create %s", created.ID)
	if err := getStore().Commit(ctx, commitMsg); err != nil && !isDoltNothingToCommit(err) {
		WarnError("failed to commit: %v", err)
	}
	return nil
}
