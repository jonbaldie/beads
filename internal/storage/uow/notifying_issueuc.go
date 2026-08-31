// Package uow — notifying.go
//
// bd has two write plumbings. The DoltStorage decorator chain fires the
// workspace's script hooks after every mutation it lands
// (internal/storage/hook_decorator.go and its per-role siblings). The
// unit-of-work plumbing — the one proxied-server mode writes through — fired
// nothing, so any script wired to .beads/hooks/ silently missed every mutation
// that went through it.
//
// A NotifyingProvider closes that gap. It wraps a UnitOfWorkProvider so that
// every committed bead mutation performed through a unit of work runs the same
// fire-and-forget hooks the DoltStorage plumbing runs, with the same event per
// operation (see hookEventForOp).
//
// THE WRAP IS AT THE PROVIDER, NOT AT EACH VERB. Most capabilities this package
// publishes reach their writes through the unit of work's use cases, so
// re-binding the accessors to the wrapper and recording at the use-case seam
// covers those roles — including roles added later. Lifecycle is the exception:
// it runs the shared Execute* body on the statement runner and records hooks
// from the result, so it does not depend on the use-case recorder.
// The accessors are declared rather than delegated for the reason
// hook_issue_operations.go gives: an accessor that hands back the INNER
// provider's role builds it on the inner provider and silently drops every hook
// this file exists to fire. notifying_parity_test.go enforces both halves.
//
// Hooks fire strictly post-commit: mutations are buffered as they flow through
// the wrapped use cases (each buffered snapshot is read inside the transaction,
// so it reflects the mutation), and the buffer is drained to the runner only
// after Commit succeeds. A rolled-back unit of work fires nothing, and a
// retried transaction (RunTxResultWithin replays the whole attempt on a fresh
// unit of work) fires only what the winning attempt recorded.
//
// # Where this plumbing deliberately differs from the DoltStorage one
//
// Every per-verb firing rule below is matched 1:1 against the decorator that
// implements it (each override cites its twin). These five are the exceptions,
// and they are listed here rather than argued in five places:
//
//  1. LABEL MULTIPLICITY ON CREATE. The DoltStorage chain replays a legacy
//     sequence for a create that carries labels: on_create with the labels
//     STRIPPED, then one synthetic on_update per label, cumulatively
//     (createHookEvents). This plumbing fires ONE on_create carrying the issue
//     with its labels. The event vocabulary is the same and the information is
//     the same; the multiplicity is a compatibility shim for the pre-decorator
//     CLI, and a new plumbing that reproduced it would cement it.
//
//  2. EDGE MULTIPLICITY. A create or an edge batch that writes several edges
//     leaving one issue fires ONE on_update for that issue, where the
//     DoltStorage create path fires one per edge. The SET of issues told is the
//     same — the created row and every far end, each carrying its records; only
//     the repeat count differs. Per distinct source is the rule its own
//     dependency-editor role applies (hook_dependency_editor.go), and two edges
//     leaving an issue are one change to it.
//
//  3. IMPORT FIRES NOTHING. `bd import` runs the batch-upsert engine on the
//     unit of work's statement runner (importer.go), so its writes never pass a
//     use case for the recorder to see. The DoltStorage plumbing imports
//     through the same engine and fires nothing either; a hook per imported
//     issue would be a new behavior on both, not a fix to this one.
//
//  4. A GUARDED VERB'S PRECONDITION IS NOT AN UPDATE. Lifecycle close and
//     reopen pass ExpectedVersion into ExecuteClose / ExecuteReopen, matching
//     the store adapters. Other roles that still spell a compare-and-set as a
//     separate ApplyUpdate must not record that precondition as an update.
//     See specWrites.
//
//  5. THE PROMOTION COMMENT. `bd promote` records an audit comment on the
//     promoted issue, and this plumbing's one comment verb fires on_update for
//     it. The DoltStorage route writes that comment through the legacy
//     AddComment, which the decorator wraps only INSIDE a transaction
//     (hookTrackingTransaction.AddComment) and not at the store level, where
//     promote calls it — so an embedded promote fires nothing. One verb cannot
//     tell which caller it is serving, and "a comment landed" is the on_update
//     this vocabulary has.
//
// A sixth is not a divergence but is worth stating: a unit of work whose commit
// message is empty is ROLLED BACK by RunTxResult, and a rolled-back attempt
// fires nothing — so a write that lands nothing reports nothing, on both
// plumbings.
package uow

import (
	"context"
	"reflect"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/types"
	publicops "github.com/jonbaldie/beads/issueops"
)

// ── Issue use case ──────────────────────────────────────────────────

type recordingIssueUC struct {
	IssueUseCasePassthrough
	*RecordingIssueCreateMethods
	*RecordingIssueGraphMethods
	*RecordingIssueUpdateMethods
	*RecordingIssueClaimMethods
	*RecordingIssueLifecycleMethods
}

// IssueUseCasePassthrough keeps unchanged methods one promotion level below
// the recording overrides, so the composed use case has an unambiguous method
// set.
type IssueUseCasePassthrough struct{ domain.IssueUseCase }

// recordingIssueContext is shared by the small method groups below. Keeping
// the recorder and snapshotter behind one context lets each group own one
// concern while the outer type still promotes the complete IssueUseCase seam.
type RecordingIssueContext struct {
	IssueUseCase domain.IssueUseCase
	rec          *recorder
	snap         *snapshotter
}

type RecordingIssueCreateMethods struct{ *RecordingIssueContext }
type RecordingIssueGraphMethods struct{ *RecordingIssueContext }
type RecordingIssueUpdateMethods struct{ *RecordingIssueContext }
type RecordingIssueClaimMethods struct{ *RecordingIssueContext }
type RecordingIssueLifecycleMethods struct{ *RecordingIssueContext }

func (u *RecordingIssueCreateMethods) CreateIssue(ctx context.Context, params domain.CreateIssueParams, actor string) (domain.CreateIssueResult, error) {
	res, err := u.IssueUseCase.CreateIssue(ctx, params, actor)
	if err == nil && res.Issue != nil {
		u.created(ctx, res.Issue.ID, params, u.snap.issue)
	}
	return res, err
}

func (u *RecordingIssueCreateMethods) CreateIssues(ctx context.Context, params []domain.CreateIssueParams, actor string) (domain.CreateIssuesResult, error) {
	res, err := u.IssueUseCase.CreateIssues(ctx, params, actor)
	if err == nil {
		for i, issue := range res.Issues {
			if issue == nil || i >= len(params) {
				continue
			}
			u.created(ctx, issue.ID, params[i], u.snap.issue)
		}
	}
	return res, err
}

func (u *RecordingIssueCreateMethods) CreateWisp(ctx context.Context, params domain.CreateIssueParams, actor string) (domain.CreateIssueResult, error) {
	res, err := u.IssueUseCase.CreateWisp(ctx, params, actor)
	if err == nil && res.Issue != nil {
		u.created(ctx, res.Issue.ID, params, u.snap.wisp)
	}
	return res, err
}

func (u *RecordingIssueCreateMethods) CreateWisps(ctx context.Context, params []domain.CreateIssueParams, actor string) (domain.CreateIssuesResult, error) {
	res, err := u.IssueUseCase.CreateWisps(ctx, params, actor)
	if err == nil {
		for i, issue := range res.Issues {
			if issue == nil || i >= len(params) {
				continue
			}
			u.created(ctx, issue.ID, params[i], u.snap.wisp)
		}
	}
	return res, err
}

// created records what one create wrote: the create itself, then an update for
// EVERY issue whose graph the create edited — the created row included.
//
// The far ends are not incidental. A create names its edges, and a REVERSE edge
// (`bd create --blocks X`) leaves the EXISTING issue rather than the new one, so
// the create mutated X's graph and X's watchers have to hear about it. The
// DoltStorage plumbing fires that update from the request's edge list
// (storage.CreatePublicCreateDependencies feeding dependencyHookEvents), which
// is where the source/target swap is decided; this reads the same three inputs
// off the params — ParentID, WaitsFor, Dependencies — because a reverse edge
// never appears in the created issue's own records.
//
// THE CREATED ROW IS A SOURCE LIKE ANY OTHER. A forward edge (`bd create
// --parent X`) leaves the new issue, and the DoltStorage plumbing fires an
// on_update for it carrying its dependency records — its on_create does not,
// which is the whole reason the update follows. Skipping it here on the grounds
// that "the create already said so" would drop the only event that carries the
// new issue's graph.
//
// ONE event per distinct source, in first-edit order, rather than the
// plumbing's one-per-edge: two edges leaving the same issue are one change to
// it, which is the rule the dependency-editor role already applies
// (hook_dependency_editor.go). See the multiplicity note in the file header.
//
// Every source is resolved across BOTH PLANES, including the created row's own.
// A create's edges may name a row on the other plane — an issue that blocks a
// wisp — and a plane-pinned read of the far end would miss it and silently drop
// the event.
func (u *RecordingIssueCreateMethods) created(
	ctx context.Context,
	createdID string,
	params domain.CreateIssueParams,
	plane func(context.Context, string) *types.Issue,
) {
	u.rec.Record(opCreate, plane(ctx, createdID))
	for _, source := range createEdgeSources(createdID, params) {
		u.rec.Record(opDepAdd, u.snap.AnyPlaneWithEdges(ctx, source))
	}
}

// updateRequestWrites reports whether a public update asked to change anything.
// It is the Lifecycle twin of specWrites: expectations alone are not a write.
func updateRequestWrites(request publicops.UpdateRequest) bool {
	if request.Claim {
		return true
	}
	return !reflect.DeepEqual(request.Patch, publicops.IssuePatch{})
}

// createEdgeSourcesFromRequest returns the distinct issue ids a create's edges
// leave, in write order, using the same prepared dependencies ExecuteCreate
// writes.
func createEdgeSourcesFromRequest(createdID string, request publicops.CreateRequest) []string {
	var sources []string
	seen := map[string]bool{}
	for _, dependency := range storage.CreatePublicCreateDependencies(createdID, request) {
		if dependency == nil || dependency.IssueID == "" || seen[dependency.IssueID] {
			continue
		}
		seen[dependency.IssueID] = true
		sources = append(sources, dependency.IssueID)
	}
	return sources
}

// createEdgeSources returns the distinct issue ids a create's edges LEAVE, in
// the order the edges are written, resolving the reverse-edge swap exactly as
// storage.CreatePublicCreateDependencies does.
func createEdgeSources(createdID string, params domain.CreateIssueParams) []string {
	request := publicops.CreateRequest{ParentID: params.ParentID}
	if params.WaitsFor != nil {
		request.WaitsFor = &publicops.WaitsFor{SpawnerID: params.WaitsFor.SpawnerID, Gate: params.WaitsFor.Gate}
	}
	for _, dependency := range params.Dependencies {
		request.Dependencies = append(request.Dependencies, publicops.CreateDependency{
			TargetID: dependency.TargetID,
			Type:     dependency.Type,
			Reverse:  dependency.SwapDirection,
			Metadata: dependency.Metadata,
			ThreadID: dependency.ThreadID,
		})
	}
	return createEdgeSourcesFromRequest(createdID, request)
}

// ApplyIssueGraph and ApplyWispGraph report a whole plan the way one create
// reports itself: a create per node, then an update per distinct issue whose
// GRAPH the plan wrote.
//
// The edges matter here more than anywhere else. applyGraph writes them through
// depRepo.Insert — BENEATH the use-case seam this recorder wraps — so nothing
// about them reaches the buffer on its own, and a plan is mostly edges. Worse,
// an explicit edge may leave an issue the plan did not create (GraphEdge.FromID
// names a live row), so the issue whose graph changed can be one no create event
// mentions at all.
func (u *RecordingIssueGraphMethods) ApplyIssueGraph(ctx context.Context, plan domain.GraphPlan, actor string) (domain.GraphApplyResult, error) {
	res, err := u.IssueUseCase.ApplyIssueGraph(ctx, plan, actor)
	if err == nil {
		u.appliedGraph(ctx, plan, res, u.snap.issue)
	}
	return res, err
}

func (u *RecordingIssueGraphMethods) ApplyWispGraph(ctx context.Context, plan domain.GraphPlan, actor string) (domain.GraphApplyResult, error) {
	res, err := u.IssueUseCase.ApplyWispGraph(ctx, plan, actor)
	if err == nil {
		u.appliedGraph(ctx, plan, res, u.snap.wisp)
	}
	return res, err
}

// appliedGraph records the plan: every created node, then one edge-carrying
// update per distinct source, resolved across both planes. It is created()'s
// rule at plan scale, including the part that reads oddly — a node that is also
// an edge source gets BOTH its create and its update, because the create event
// carries no dependency records and the update is the only one that does.
func (u *RecordingIssueGraphMethods) appliedGraph(
	ctx context.Context,
	plan domain.GraphPlan,
	res domain.GraphApplyResult,
	plane func(context.Context, string) *types.Issue,
) {
	for _, id := range graphIDs(plan, res) {
		u.rec.Record(opCreate, plane(ctx, id))
	}
	for _, source := range graphEdgeSources(plan, res) {
		u.rec.Record(opDepAdd, u.snap.AnyPlaneWithEdges(ctx, source))
	}
}

// graphIDs returns the created ids in the plan's node order. The result maps
// plan keys to minted ids, and a map ranges in no order — hook scripts see the
// batch in the order it was written, as they do for a batch create.
func graphIDs(plan domain.GraphPlan, res domain.GraphApplyResult) []string {
	ids := make([]string, 0, len(res.IDs))
	for _, node := range plan.Nodes {
		if id, ok := res.IDs[node.Key]; ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// graphEdgeSources returns the distinct issue ids the plan's edges LEAVE, in
// the order applyGraph names them: node parent links, then explicit edges, then
// per-node inline deps. It reads the same three inputs applyGraph writes from
// and resolves plan-local keys through the same minted-id map, so a key that
// named a node and an id that named a live row both land on the row the insert
// touched.
//
// None of the three swaps direction — a node's parent link leaves the CHILD, an
// explicit edge leaves resolveEdgeRef(FromKey, FromID), and an inline dep leaves
// its own node (types.NewGraphNodeDependency) — so the source is always the id
// on the left.
func graphEdgeSources(plan domain.GraphPlan, res domain.GraphApplyResult) []string {
	collector := graphSourceCollector{ids: res.IDs, seen: make(map[string]bool)}
	collector.addParents(plan.Nodes)
	collector.addEdges(plan.Edges)
	collector.addInlineDependencies(plan.Nodes)
	return collector.sources
}

type graphSourceCollector struct {
	ids     map[string]string
	sources []string
	seen    map[string]bool
}

func (c *graphSourceCollector) resolve(key, id string) string {
	if id != "" {
		return id
	}
	return c.ids[key]
}

func (c *graphSourceCollector) add(id string) {
	if id == "" || c.seen[id] {
		return
	}
	c.seen[id] = true
	c.sources = append(c.sources, id)
}

func (c *graphSourceCollector) addParents(nodes []domain.GraphNode) {
	for _, node := range nodes {
		if node.ParentKey == "" && node.ParentID == "" {
			continue
		}
		if c.resolve(node.ParentKey, node.ParentID) == "" {
			continue
		}
		c.add(c.ids[node.Key])
	}
}

func (c *graphSourceCollector) addEdges(edges []domain.GraphEdge) {
	for _, edge := range edges {
		c.add(c.resolve(edge.FromKey, edge.FromID))
	}
}

func (c *graphSourceCollector) addInlineDependencies(nodes []domain.GraphNode) {
	for _, node := range nodes {
		if len(node.Deps) > 0 {
			c.add(c.ids[node.Key])
		}
	}
}

func (u *RecordingIssueUpdateMethods) UpdateIssue(ctx context.Context, id string, updates map[string]any, actor string) error {
	if err := u.IssueUseCase.UpdateIssue(ctx, id, updates, actor); err != nil {
		return err
	}
	u.rec.Record(opUpdate, u.snap.issue(ctx, id))
	return nil
}

// CompareAndSetMetadataKey records an update for a swap that MOVED the value,
// and nothing for one that did not — the same line hookMetadataCAS draws on the
// DoltStorage chain, which fires on_update only when the row changed.
//
// It reads the fact rather than inferring it. The use case reports whether a
// row write landed, which is strictly better than the decorator's comparison of
// the request's two ends, and it is the only caller of this method that has it.
//
// THE SNAPSHOT IS anyPlane, because this role resolves the id across both
// planes itself: a swap on a wisp is an update to a wisp, and reading only the
// issues table would record a nil for it.
func (u *RecordingIssueUpdateMethods) CompareAndSetMetadataKey(ctx context.Context, plan storage.CompareAndSetKeyPlan) (publicops.CompareAndSetKeyResult, bool, error) {
	result, wrote, err := u.IssueUseCase.CompareAndSetMetadataKey(ctx, plan)
	if err != nil || !wrote {
		return result, wrote, err
	}
	u.rec.Record(opUpdate, u.snap.AnyPlane(ctx, plan.IssueID))
	return result, wrote, nil
}

// ReleaseIssue records an update for a release that landed.
//
// It is DECLARED rather than inherited, which is the whole reason this method
// exists: an accessor promoted onto an embedder compiles perfectly and records
// nothing, so a release through a notifying unit of work would silently lose
// the hook the DoltStorage chain fires for the same write.
//
// It reads the ROLE'S OWN verdict — ReleaseResult.Changed — which is the same
// fact the bool beside it carries and the one a notification is about. An
// ephemeral release changes a wisp and versions nothing, and it is still an
// update somebody asked to be notified about.
//
// THE SNAPSHOT IS anyPlane, because this role resolves the id across both
// planes itself: releasing a wisp is an update to a wisp, and reading only the
// issues table would record a nil for it.
func (u *RecordingIssueUpdateMethods) ReleaseIssue(ctx context.Context, req publicops.ReleaseRequest) (publicops.ReleaseResult, bool, error) {
	result, wrote, err := u.IssueUseCase.ReleaseIssue(ctx, req)
	if err != nil || !result.Changed {
		return result, wrote, err
	}
	u.rec.Record(opUpdate, u.snap.AnyPlane(ctx, req.IssueID))
	return result, wrote, nil
}

func (u *RecordingIssueUpdateMethods) UpdateWisp(ctx context.Context, id string, updates map[string]any, actor string) error {
	if err := u.IssueUseCase.UpdateWisp(ctx, id, updates, actor); err != nil {
		return err
	}
	u.rec.Record(opUpdate, u.snap.wisp(ctx, id))
	return nil
}

// ApplyUpdate is the guarded update every public update runs through, and it
// serves both planes, so the fallback snapshot resolves the id rather than
// pinning a table.
//
// A spec that only states EXPECTATIONS writes nothing — it is the
// compare-and-set precondition `bd close --expect-version` and `bd reopen`
// spell as a separate call before the operation they guard (issue_operations.go)
// — so it records nothing. The DoltStorage plumbing passes that precondition
// INTO the close and fires the close hook alone; recording here would put an
// extra on_update in front of every guarded close.
func (u *RecordingIssueUpdateMethods) ApplyUpdate(ctx context.Context, id string, spec domain.UpdateSpec, actor string) (*types.Issue, error) {
	issue, err := u.IssueUseCase.ApplyUpdate(ctx, id, spec, actor)
	if err != nil || !specWrites(spec) {
		return issue, err
	}
	u.rec.Record(opUpdate, u.snap.AnyPlane(ctx, id))
	return issue, nil
}

// specWrites reports whether spec asks ApplyUpdate to change anything. It
// clears the three expectation fields and asks whether ANYTHING is left, rather
// than listing the writing fields: a field added to UpdateSpec then counts as a
// write until someone says otherwise, which is the safe direction for a hook.
func specWrites(spec domain.UpdateSpec) bool {
	spec.ExpectedVersion, spec.ExpectedAssignee, spec.ExpectedStatus = nil, nil, nil
	if len(spec.Fields) == 0 {
		spec.Fields = nil
	}
	if len(spec.AddLabels) == 0 {
		spec.AddLabels = nil
	}
	if len(spec.RemoveLabels) == 0 {
		spec.RemoveLabels = nil
	}
	return !reflect.DeepEqual(spec, domain.UpdateSpec{})
}

func (u *RecordingIssueClaimMethods) ClaimIssue(ctx context.Context, id, actor string) (domain.ClaimResult, error) {
	res, err := u.IssueUseCase.ClaimIssue(ctx, id, actor)
	if err == nil && !res.AlreadyClaimed {
		u.rec.Record(opUpdate, u.snap.issue(ctx, id))
	}
	return res, err
}

func (u *RecordingIssueClaimMethods) ClaimIssueIfOpen(ctx context.Context, id, actor string) (domain.ClaimResult, error) {
	res, err := u.IssueUseCase.ClaimIssueIfOpen(ctx, id, actor)
	if err == nil && !res.AlreadyClaimed {
		u.rec.Record(opUpdate, u.snap.issue(ctx, id))
	}
	return res, err
}

func (u *RecordingIssueClaimMethods) ClaimWisp(ctx context.Context, id, actor string) (domain.ClaimResult, error) {
	res, err := u.IssueUseCase.ClaimWisp(ctx, id, actor)
	if err == nil && !res.AlreadyClaimed {
		u.rec.Record(opUpdate, u.snap.wisp(ctx, id))
	}
	return res, err
}

func (u *RecordingIssueClaimMethods) ClaimWispIfOpen(ctx context.Context, id, actor string) (domain.ClaimResult, error) {
	res, err := u.IssueUseCase.ClaimWispIfOpen(ctx, id, actor)
	if err == nil && !res.AlreadyClaimed {
		u.rec.Record(opUpdate, u.snap.wisp(ctx, id))
	}
	return res, err
}

func (u *RecordingIssueClaimMethods) ClaimReadyIssue(ctx context.Context, filter types.WorkFilter, actor string) (domain.ClaimReadyResult, error) {
	res, err := u.IssueUseCase.ClaimReadyIssue(ctx, filter, actor)
	if err == nil && res.Claimed && res.Issue != nil {
		u.rec.Record(opUpdate, u.snap.issue(ctx, res.Issue.ID))
	}
	return res, err
}

func (u *RecordingIssueClaimMethods) ClaimReadyWisp(ctx context.Context, filter types.WorkFilter, actor string) (domain.ClaimReadyResult, error) {
	res, err := u.IssueUseCase.ClaimReadyWisp(ctx, filter, actor)
	if err == nil && res.Claimed && res.Issue != nil {
		u.rec.Record(opUpdate, u.snap.wisp(ctx, res.Issue.ID))
	}
	return res, err
}

// CloseIssue and its three siblings record on SUCCESS, not on "something
// changed": the DoltStorage plumbing fires on_close for an idempotent re-close
// too — HookFiringStore.CloseIssueChecked says so in as many words. A close
// that found the issue already closed still answers "it is closed", and a
// script that reconciles on that answer must not be told only sometimes.
//
// THE BATCH COMPOSITIONS OVERRIDE THAT, and they do it above these verbs rather
// than inside them, because these are the SAME methods a single close reaches.
// closeBatchItem rewinds a recorded notification when the item persisted
// nothing. BatchApplier never records that close: announceBatchApply fires
// only on Changed. Both match hookBatchCloser and hookBatchApplier
// (ga-2yaqp.1) — otherwise a teardown replayed against an already-closed
// convoy runs on_close once per item on every pass. Gate here and the
// single close would lose its re-close firing with it.
//
// The snapshot read is PLANE-PINNED to the verb that was called, while the verb
// itself tolerates an id from either plane. That gap is unreachable as shipped:
// every caller resolves the plane first (issue_operations.go and batch_closer.go
// both route through workapi.GetIssueOrWisp or operationIssue and then call the
// matching verb), so a wisp id never arrives at the issues-plane verb. Were one
// to, the read would miss and the notification would be dropped rather than
// mis-addressed — which is why this stays pinned instead of falling back to the
// verb's own result: a result-shaped fallback would paper over the routing bug
// that produced it. The reopen pair below reads the same way.
func (u *RecordingIssueLifecycleMethods) CloseIssue(ctx context.Context, id string, params domain.CloseIssueParams, actor string) (domain.CloseIssueResult, error) {
	res, err := u.IssueUseCase.CloseIssue(ctx, id, params, actor)
	if err == nil {
		u.rec.Record(opClose, u.snap.issue(ctx, id))
	}
	return res, err
}

func (u *RecordingIssueLifecycleMethods) CloseIssueChecked(ctx context.Context, id string, params domain.CloseIssueParams, actor string, force bool) (domain.CloseIssueResult, error) {
	res, err := u.IssueUseCase.CloseIssueChecked(ctx, id, params, actor, force)
	if err == nil {
		u.rec.Record(opClose, u.snap.issue(ctx, id))
	}
	return res, err
}

func (u *RecordingIssueLifecycleMethods) CloseWisp(ctx context.Context, id string, params domain.CloseIssueParams, actor string) (domain.CloseIssueResult, error) {
	res, err := u.IssueUseCase.CloseWisp(ctx, id, params, actor)
	if err == nil {
		u.rec.Record(opClose, u.snap.wisp(ctx, id))
	}
	return res, err
}

func (u *RecordingIssueLifecycleMethods) CloseWispChecked(ctx context.Context, id string, params domain.CloseIssueParams, actor string, force bool) (domain.CloseIssueResult, error) {
	res, err := u.IssueUseCase.CloseWispChecked(ctx, id, params, actor, force)
	if err == nil {
		u.rec.Record(opClose, u.snap.wisp(ctx, id))
	}
	return res, err
}

func (u *RecordingIssueLifecycleMethods) ReopenIssue(ctx context.Context, id string, params domain.ReopenIssueParams, actor string) (domain.ReopenIssueResult, error) {
	res, err := u.IssueUseCase.ReopenIssue(ctx, id, params, actor)
	if err == nil && res.Reopened {
		u.rec.Record(opUpdate, u.snap.issue(ctx, id))
	}
	return res, err
}

func (u *RecordingIssueLifecycleMethods) ReopenWisp(ctx context.Context, id string, params domain.ReopenIssueParams, actor string) (domain.ReopenIssueResult, error) {
	res, err := u.IssueUseCase.ReopenWisp(ctx, id, params, actor)
	if err == nil && res.Reopened {
		u.rec.Record(opUpdate, u.snap.wisp(ctx, id))
	}
	return res, err
}
