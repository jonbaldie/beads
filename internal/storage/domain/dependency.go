package domain

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/issueops"
)

// The dependency-edge refusals are declared and documented by the public
// contract package, github.com/jonbaldie/beads/issueops. These are the same
// values, so every domain.ErrX reference and every errors.Is site keeps
// matching the identical error.
var (
	ErrSelfDependency           = issueops.ErrSelfDependency
	ErrDependencyCycle          = issueops.ErrDependencyCycle
	ErrDependencySourceNotFound = issueops.ErrDependencySourceNotFound
	ErrDependencyTargetNotFound = issueops.ErrDependencyTargetNotFound
)

// cycleError carries a fully-formatted cycle-rejection message while unwrapping
// to ErrDependencyCycle. The bulk dependency-add path surfaces this text
// verbatim through the proxied CLI (HandleErrorRespectJSON("%v", err)), so a
// plain fmt.Errorf("...: %w", ErrDependencyCycle) — which appends the sentinel's
// own "adding dependency would create a cycle" text to an already-complete
// message — would change the user-facing string. This keeps the message
// byte-for-byte and adds only errors.Is matchability.
type cycleError struct {
	msg string
}

func (e *cycleError) Error() string { return e.msg }
func (e *cycleError) Unwrap() error { return ErrDependencyCycle }

// cycleErrorf formats a cycle-rejection message that errors.Is-matches
// ErrDependencyCycle without altering the rendered text.
func cycleErrorf(format string, args ...any) error {
	return &cycleError{msg: fmt.Sprintf(format, args...)}
}

// NewCycleError is the exported entry point for cycleErrorf. The embedded bulk
// CLI final gate (cmd/bd/dep.go addBulkDependenciesInTx) lives in a different
// package but must type its cycle rejection identically to this bulk path, so it
// builds the same errors.Is-matchable-but-text-preserving error through here
// rather than duplicating the cycleError wrapper.
func NewCycleError(format string, args ...any) error {
	return cycleErrorf(format, args...)
}

// DependencyTypeConflictError reports a duplicate dependency pair with a
// conflicting requested type. See issueops.DependencyTypeConflictError.
type DependencyTypeConflictError = issueops.DependencyTypeConflictError

// DependencyHierarchyConflictError reports a dependency that would make a
// blocking hierarchy impossible to complete. See
// issueops.DependencyHierarchyConflictError.
type DependencyHierarchyConflictError = issueops.DependencyHierarchyConflictError

// DependencyEndpointNotFoundError reports which endpoint of a refused edge this
// database could see the absence of. See
// issueops.DependencyEndpointNotFoundError.
type DependencyEndpointNotFoundError = issueops.DependencyEndpointNotFoundError

type DepDirection int

const (
	DepDirectionBoth DepDirection = iota
	DepDirectionOut
	DepDirectionIn
)

type DepInsertOpts struct {
	UseWispsTable      bool
	HierarchyValidated bool // Set only after ValidateBlockingHierarchy on the same repository/UOW.
	CycleValidated     bool // Set only after HasCycle or a whole-graph check on the same repository/UOW.
	// EmitEvent records a dependency_added / dependency_removed event on the
	// source's event table for a genuine edge add/remove. Only the explicit dep
	// verbs (AddDependency/RemoveDependency plus their wisp twins, and the bulk
	// AddDependencies) set it; create-with-deps and reparent call Insert/Delete
	// directly with it unset, so an implicit parent-child / --deps / waits-for
	// edge produces no event.
	// The embedded plumbing matches edge-for-edge: its structural paths
	// wire edges through the plain AddDependency/tx.AddDependency, whose
	// issueops.AddDependencyInTx EmitEvent gate is unset, while only the explicit
	// bd dep add / bd link / bd dep remove verbs pass EmitEvent.
	EmitEvent bool
}

type DepListOpts struct {
	Types         []types.DependencyType
	Direction     DepDirection
	UseWispsTable bool
}

type DepCountsOpts struct {
	UseWispsTable bool
}

type DepBulkResult struct {
	Outgoing map[string][]*types.Dependency
	Incoming map[string][]*types.Dependency
}

type DepListFilter struct {
	Types     []types.DependencyType
	Direction DepDirection
}

type BlockingInfo struct {
	BlockedBy map[string][]string
	Blocks    map[string][]string
	Parent    map[string]string
}

type DepDeleteResult struct {
	Found       bool
	Type        types.DependencyType
	DependsOnID string
}

type DepTreeOpts struct {
	MaxDepth     int
	ShowAllPaths bool
	Direction    DepDirection
}

type BulkAddDepsOpts struct {
	SkipPerEdgeCycleCheck bool
}

type BulkAddDepsResult struct {
	Added []*types.Dependency
}

type DependencySQLRepository interface {
	ValidateBlockingHierarchy(ctx context.Context, dep *types.Dependency) error
	Insert(ctx context.Context, dep *types.Dependency, actor string, opts DepInsertOpts) error
	Delete(ctx context.Context, issueID, dependsOnID, actor string, opts DepInsertOpts) (DepDeleteResult, error)
	HasCycle(ctx context.Context, issueID, dependsOnID string) (bool, error)
	ListByIssueIDs(ctx context.Context, issueIDs []string, opts DepListOpts) (DepBulkResult, error)
	ListWithIssueMetadata(ctx context.Context, sourceID string, opts DepListOpts) ([]*types.IssueWithDependencyMetadata, error)
	IterWithIssueMetadata(ctx context.Context, sourceID string, opts DepListOpts) (storage.Iter[types.IssueWithDependencyMetadata], error)
	CountByID(ctx context.Context, sourceID string, opts DepListOpts) (int64, error)
	CountsByIssueIDs(ctx context.Context, issueIDs []string, opts DepCountsOpts) (map[string]*types.DependencyCounts, error)

	GetBlockingInfo(ctx context.Context, issueIDs []string, opts DepListOpts) (BlockingInfo, error)
	GetBlockingInfoAcrossIssuesAndWisps(ctx context.Context, issueIDs []string) (BlockingInfo, error)
	IsBlocked(ctx context.Context, issueID string, opts DepListOpts) (bool, []string, error)

	DeleteAllForIDs(ctx context.Context, ids []string, opts DepInsertOpts) (int, error)
	CountAllForIDs(ctx context.Context, ids []string, opts DepCountsOpts) (int, error)
	DetectCycles(ctx context.Context) ([][]*types.Issue, error)
	// DetectCycleReport answers the same walk in the shape issueops.CycleDetector
	// publishes: canonically ordered, and carrying every member of a cycle
	// whether or not this database can describe it. DetectCycles above is the
	// lossy legacy shape.
	DetectCycleReport(ctx context.Context) (issueops.CycleReport, error)

	GetTree(ctx context.Context, rootID string, opts DepTreeOpts) ([]*types.TreeNode, error)
	// WalkDependencyTree answers the tree walk in the shape issueops.TreeWalker
	// publishes: validated, rooted, pruned by status and capped, with both
	// directions of a `both` request inside ONE transaction. GetTree above is the
	// unvalidated shape.
	WalkDependencyTree(ctx context.Context, req issueops.WalkTreeRequest) (issueops.TreeResult, error)
	// CountEdges answers the batched edge count in the shape
	// issueops.GraphCounter publishes: validated, per-anchor, spanning both
	// dependency planes, with the existence probe and the tally in ONE
	// transaction. CountByIssueID above is the single-anchor, unprobed shape.
	CountEdges(ctx context.Context, req issueops.EdgeCountRequest) (issueops.EdgeCountResult, error)
	CycleThroughEdges(ctx context.Context, edges [][2]string) (string, error)
	GetDependencyRecordsForIssues(ctx context.Context, issueIDs []string) (map[string][]*types.Dependency, error)
	GetWispDependencyRecordsForIDs(ctx context.Context, wispIDs []string) (map[string][]*types.Dependency, error)

	// WispSourceIDs returns the subset of ids that are currently wisps, in one
	// scoped query rather than a probe per id.
	WispSourceIDs(ctx context.Context, ids []string) (map[string]struct{}, error)
}

type DependencyUseCase interface {
	AddDependency(ctx context.Context, dep *types.Dependency, actor string) error
	RemoveDependency(ctx context.Context, issueID, dependsOnID, actor string) error
	// RemoveDependencyBySource removes one edge from the plane its SOURCE
	// lives in, the way AddDependencies writes one. There is deliberately no
	// plane flag: `bd dep remove` names an id, not a table, and a removal
	// cannot put an edge anywhere, so reading the plane is always safer than
	// pinning it.
	RemoveDependencyBySource(ctx context.Context, sourceID, dependsOnID, actor string) (bool, error)
	Reparent(ctx context.Context, childID, newParentID, actor string) error
	ListByIssueIDs(ctx context.Context, issueIDs []string, filter DepListFilter) (DepBulkResult, error)
	ListWithIssueMetadata(ctx context.Context, issueID string, filter DepListFilter) ([]*types.IssueWithDependencyMetadata, error)
	IterWithIssueMetadata(ctx context.Context, issueID string, filter DepListFilter) (storage.Iter[types.IssueWithDependencyMetadata], error)
	CountByIssueID(ctx context.Context, issueID string, filter DepListFilter) (int64, error)
	CountsByIssueIDs(ctx context.Context, issueIDs []string) (map[string]*types.DependencyCounts, error)
	GetBlockingInfo(ctx context.Context, issueIDs []string) (BlockingInfo, error)
	IsBlocked(ctx context.Context, issueID string) (bool, []string, error)
	GetForIssueIDs(ctx context.Context, ids []string) (map[string][]*types.Dependency, error)
	DetectCycles(ctx context.Context) ([][]*types.Issue, error)
	// DetectCycleReport is the shape issueops.CycleDetector publishes; see the
	// repository method of the same name.
	DetectCycleReport(ctx context.Context) (issueops.CycleReport, error)

	GetDependencyTree(ctx context.Context, rootID string, opts DepTreeOpts) ([]*types.TreeNode, error)
	// WalkDependencyTree is the shape issueops.TreeWalker publishes; see the
	// repository method of the same name.
	WalkDependencyTree(ctx context.Context, req issueops.WalkTreeRequest) (issueops.TreeResult, error)
	// CountEdges is the shape issueops.GraphCounter publishes; see the
	// repository method of the same name.
	CountEdges(ctx context.Context, req issueops.EdgeCountRequest) (issueops.EdgeCountResult, error)
	// AddDependencies asserts a batch of edges, each landing in the plane its
	// own SOURCE lives in. There is deliberately no plane-pinned variant:
	// `bd dep add` takes whatever ids the caller names and one request may
	// legitimately mix them, so routing the whole batch by a flag would put an
	// edge on a row the target table does not have. It is ONE pass, not one
	// pass per plane: the parent-child-first ordering and the whole-graph
	// cycle gate both have to see the request as a single graph.
	AddDependencies(ctx context.Context, deps []*types.Dependency, actor string, opts BulkAddDepsOpts) (BulkAddDepsResult, error)
	// ValidateBlockingHierarchy and CycleThroughEdges are the two halves of the
	// whole-graph gate AddDependencies runs at the end of its own batch,
	// published so a caller that writes edges INTERLEAVED with other mutations
	// can run the same gate over the graph its whole request produced.
	//
	// issueops.BatchApplier is that caller and the reason these are here: it
	// applies items in the caller's declaration order and never reorders, so a
	// blocking edge can be written before the parent-child edge that makes it a
	// conflict, and the per-edge probe that ran at the time saw a hierarchy
	// that had not been built yet. Both must be re-run at the end, against the
	// same repository methods the per-edge path uses, or the two backends
	// answer different refusals to the same request.
	ValidateBlockingHierarchy(ctx context.Context, dep *types.Dependency) error
	// CycleThroughEdges reports the path of a scheduling cycle any of edges
	// closes, or "" when none does. Each pair is (source, target).
	CycleThroughEdges(ctx context.Context, edges [][2]string) (string, error)
	GetIssueDependencyRecords(ctx context.Context, issueIDs []string) (map[string][]*types.Dependency, error)

	GetWispDependencyRecords(ctx context.Context, wispIDs []string) (map[string][]*types.Dependency, error)

	AddWispDependency(ctx context.Context, dep *types.Dependency, actor string) error
	RemoveWispDependency(ctx context.Context, wispID, dependsOnID, actor string) error
	ReparentWisp(ctx context.Context, childWispID, newParentID, actor string) error
	ListByWispIDs(ctx context.Context, wispIDs []string, filter DepListFilter) (DepBulkResult, error)
	ListWispWithIssueMetadata(ctx context.Context, wispID string, filter DepListFilter) ([]*types.IssueWithDependencyMetadata, error)
	IterWispWithIssueMetadata(ctx context.Context, wispID string, filter DepListFilter) (storage.Iter[types.IssueWithDependencyMetadata], error)
	CountByWispID(ctx context.Context, wispID string, filter DepListFilter) (int64, error)
	CountsByWispIDs(ctx context.Context, wispIDs []string) (map[string]*types.DependencyCounts, error)
	IsWispBlocked(ctx context.Context, wispID string) (bool, []string, error)
}

func NewDependencyUseCase(depRepo DependencySQLRepository) DependencyUseCase {
	uc := &dependencyUseCaseImpl{
		dependencyEdgeMutationUseCase:  &dependencyEdgeMutationUseCase{depRepo: depRepo},
		dependencyBatchMutationUseCase: &dependencyBatchMutationUseCase{depRepo: depRepo},
		dependencyQueryUseCase:         &dependencyQueryUseCase{depRepo: depRepo},
		dependencyStatusUseCase:        &dependencyStatusUseCase{depRepo: depRepo},
		dependencyGraphUseCase:         &dependencyGraphUseCase{depRepo: depRepo},
		dependencyRecordsUseCase:       &dependencyRecordsUseCase{depRepo: depRepo},
	}
	// Keep the composition explicit: the public interface is implemented by
	// promoted role methods, and these references make that seam visible to
	// readers and static analyzers.
	_ = uc.dependencyEdgeMutationUseCase
	_ = uc.dependencyBatchMutationUseCase
	_ = uc.dependencyQueryUseCase
	_ = uc.dependencyStatusUseCase
	_ = uc.dependencyGraphUseCase
	_ = uc.dependencyRecordsUseCase
	return uc
}

type dependencyUseCaseImpl struct {
	*dependencyEdgeMutationUseCase
	*dependencyBatchMutationUseCase
	*dependencyQueryUseCase
	*dependencyStatusUseCase
	*dependencyGraphUseCase
	*dependencyRecordsUseCase
}

type dependencyEdgeMutationUseCase struct {
	depRepo DependencySQLRepository
}

type dependencyBatchMutationUseCase struct {
	depRepo DependencySQLRepository
}

type dependencyQueryUseCase struct {
	depRepo DependencySQLRepository
}

type dependencyStatusUseCase struct {
	depRepo DependencySQLRepository
}

type dependencyGraphUseCase struct {
	depRepo DependencySQLRepository
}

type dependencyRecordsUseCase struct {
	depRepo DependencySQLRepository
}

var _ DependencyUseCase = (*dependencyUseCaseImpl)(nil)

func (u *dependencyEdgeMutationUseCase) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	return addDependency(ctx, u.depRepo, dep, actor, false)
}

func (u *dependencyEdgeMutationUseCase) AddWispDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	return addDependency(ctx, u.depRepo, dep, actor, true)
}

func addDependency(ctx context.Context, depRepo DependencySQLRepository, dep *types.Dependency, actor string, useWisp bool) error {
	if err := validateUseCaseDependencyInput(dep); err != nil {
		return err
	}
	if err := validateUseCaseDependencyHierarchy(ctx, depRepo, dep); err != nil {
		return err
	}
	if err := checkUseCaseDependencyCycle(ctx, depRepo, dep); err != nil {
		return err
	}
	return insertUseCaseDependency(ctx, depRepo, dep, actor, useWisp)
}

func validateUseCaseDependencyInput(dep *types.Dependency) error {
	if dep == nil {
		return fmt.Errorf("add dep: dep must not be nil")
	}
	if dep.IssueID == "" || dep.DependsOnID == "" {
		return fmt.Errorf("add dep: IssueID and DependsOnID must be non-empty")
	}

	// Self-dependency guard mirrors issueops.CheckDependencyCycleInTx: it is
	// checked BEFORE the cycle probe and for ALL dep types, and emits the
	// dedicated self-dep message. A blocking self-edge otherwise trips HasCycle
	// and would report the wrong (cycle) error (#4547 F-1).
	if dep.IssueID == dep.DependsOnID {
		return fmt.Errorf("%w: %s cannot depend on itself", ErrSelfDependency, dep.IssueID)
	}
	return nil
}

func validateUseCaseDependencyHierarchy(ctx context.Context, depRepo DependencySQLRepository, dep *types.Dependency) error {
	if err := depRepo.ValidateBlockingHierarchy(ctx, dep); err != nil {
		var hierarchyConflict *DependencyHierarchyConflictError
		if errors.As(err, &hierarchyConflict) {
			return err
		}
		return fmt.Errorf("add dep: hierarchy check: %w", err)
	}

	return nil
}

func checkUseCaseDependencyCycle(ctx context.Context, depRepo DependencySQLRepository, dep *types.Dependency) error {
	if types.IsSchedulingEdge(dep.Type) {
		cycle, err := depRepo.HasCycle(ctx, dep.IssueID, dep.DependsOnID)
		if err != nil {
			return fmt.Errorf("add dep: cycle check: %w", err)
		}
		if cycle {
			// Match the embedded store's user-facing wording verbatim (no ids
			// prefix) so gc code that string-matches this error behaves the same
			// on both plumbings (#4547 F-1).
			return ErrDependencyCycle
		}
	}
	return nil
}

func insertUseCaseDependency(ctx context.Context, depRepo DependencySQLRepository, dep *types.Dependency, actor string, useWisp bool) error {
	if err := depRepo.Insert(ctx, dep, actor, DepInsertOpts{UseWispsTable: useWisp, HierarchyValidated: true, CycleValidated: true, EmitEvent: true}); err != nil {
		// The retype conflict is a user-facing error whose message already
		// matches embedded verbatim; pass it through unwrapped so the CLI does
		// not prepend "add dep: insert:" (#4547 F-1). The endpoint-existence
		// refusals are here for the same reason.
		var conflict *DependencyTypeConflictError
		if errors.As(err, &conflict) {
			return err
		}
		var hierarchyConflict *DependencyHierarchyConflictError
		if errors.As(err, &hierarchyConflict) {
			return err
		}
		var missingEndpoint *DependencyEndpointNotFoundError
		if errors.As(err, &missingEndpoint) {
			return err
		}
		return fmt.Errorf("add dep: insert: %w", err)
	}
	return nil
}

func (u *dependencyEdgeMutationUseCase) RemoveDependency(ctx context.Context, issueID, dependsOnID, actor string) error {
	return removeDependency(ctx, u.depRepo, issueID, dependsOnID, actor, false)
}

func (u *dependencyEdgeMutationUseCase) RemoveWispDependency(ctx context.Context, wispID, dependsOnID, actor string) error {
	return removeDependency(ctx, u.depRepo, wispID, dependsOnID, actor, true)
}

// RemoveDependencyBySource removes one edge from the plane its SOURCE lives in
// and reports whether there was an edge to remove.
//
// It is the source-routed twin of AddDependencies, and exists for the same
// reason: `bd dep remove` takes whatever id the caller names, and pinning the
// removal to the durable table means failing to remove an edge whose source is
// a wisp while reporting that it was never there (bd-yby99.17). The delete IS
// the verdict, the way the store-backed body reads it off RemoveDependencyInTx
// rather than from a separate lookup.
func (u *dependencyEdgeMutationUseCase) RemoveDependencyBySource(ctx context.Context, sourceID, dependsOnID, actor string) (bool, error) {
	return removeDependencyBySource(ctx, u.depRepo, sourceID, dependsOnID, actor)
}

func removeDependencyBySource(ctx context.Context, depRepo DependencySQLRepository, sourceID, dependsOnID, actor string) (bool, error) {
	if sourceID == "" || dependsOnID == "" {
		return false, fmt.Errorf("remove dep: sourceID and dependsOnID must not be empty")
	}
	wispSources, err := depRepo.WispSourceIDs(ctx, []string{sourceID})
	if err != nil {
		return false, fmt.Errorf("remove dep: classify source: %w", err)
	}
	_, sourceIsWisp := wispSources[sourceID]
	res, err := depRepo.Delete(ctx, sourceID, dependsOnID, actor, DepInsertOpts{UseWispsTable: sourceIsWisp, EmitEvent: true})
	if err != nil {
		return false, fmt.Errorf("remove dep %s -> %s: %w", sourceID, dependsOnID, err)
	}
	return res.Found, nil
}

func removeDependency(ctx context.Context, depRepo DependencySQLRepository, sourceID, dependsOnID, actor string, useWisp bool) error {
	if sourceID == "" || dependsOnID == "" {
		return fmt.Errorf("remove dep: sourceID and dependsOnID must not be empty")
	}
	if _, err := depRepo.Delete(ctx, sourceID, dependsOnID, actor, DepInsertOpts{UseWispsTable: useWisp, EmitEvent: true}); err != nil {
		return fmt.Errorf("remove dep %s -> %s: %w", sourceID, dependsOnID, err)
	}
	return nil
}

func (u *dependencyEdgeMutationUseCase) Reparent(ctx context.Context, childID, newParentID, actor string) error {
	return reparentDependency(ctx, u.depRepo, childID, newParentID, actor, false)
}

func (u *dependencyEdgeMutationUseCase) ReparentWisp(ctx context.Context, childWispID, newParentID, actor string) error {
	return reparentDependency(ctx, u.depRepo, childWispID, newParentID, actor, true)
}

func reparentDependency(ctx context.Context, depRepo DependencySQLRepository, childID, newParentID, actor string, useWisp bool) error {
	if childID == "" {
		return fmt.Errorf("reparent: childID must not be empty")
	}
	if childID == newParentID {
		return fmt.Errorf("reparent: %s cannot be its own parent", childID)
	}

	opts := DepInsertOpts{UseWispsTable: useWisp}
	existing, err := currentParentSet(ctx, depRepo, childID, useWisp)
	if err != nil {
		return err
	}
	target := targetParentSet(newParentID)
	if sameStringSet(existing, target) {
		return nil
	}
	if err := removeOldParents(ctx, depRepo, childID, actor, opts, existing, target); err != nil {
		return err
	}
	return addNewParents(ctx, depRepo, childID, actor, opts, target, existing)
}

func currentParentSet(ctx context.Context, depRepo DependencySQLRepository, childID string, useWisp bool) (map[string]struct{}, error) {
	res, err := depRepo.ListByIssueIDs(ctx, []string{childID}, DepListOpts{
		Types:         []types.DependencyType{types.DepParentChild},
		Direction:     DepDirectionOut,
		UseWispsTable: useWisp,
	})
	if err != nil {
		return nil, fmt.Errorf("reparent: list current parent: %w", err)
	}

	// A child can carry MORE THAN ONE parent-child edge — Create accepts
	// CreateRequest.ParentID and an explicit parent-child entry in
	// Dependencies in the same request — so this is a set replacement, not a
	// swap of one edge. Diffing the whole existing set against the target set
	// is the same rule the store-backed backends apply in
	// issueops.ApplyParentPatch; that body cannot be called from here because
	// internal/storage/issueops imports this package (bd-yby99.26).
	existing := map[string]struct{}{}
	for _, dep := range res.Outgoing[childID] {
		if dep.Type == types.DepParentChild {
			existing[dep.DependsOnID] = struct{}{}
		}
	}
	return existing, nil
}

func targetParentSet(newParentID string) map[string]struct{} {
	target := map[string]struct{}{}
	if newParentID != "" {
		target[newParentID] = struct{}{}
	}
	return target
}

func removeOldParents(ctx context.Context, depRepo DependencySQLRepository, childID, actor string, opts DepInsertOpts, existing, target map[string]struct{}) error {
	for _, oldParentID := range sortedSetDifference(existing, target) {
		if _, err := depRepo.Delete(ctx, childID, oldParentID, actor, opts); err != nil {
			return fmt.Errorf("reparent: remove old parent %s: %w", oldParentID, err)
		}
	}
	return nil
}

func addNewParents(ctx context.Context, depRepo DependencySQLRepository, childID, actor string, opts DepInsertOpts, target, existing map[string]struct{}) error {
	for _, addParentID := range sortedSetDifference(target, existing) {
		dep := &types.Dependency{
			IssueID:     childID,
			DependsOnID: addParentID,
			Type:        types.DepParentChild,
		}
		if err := depRepo.Insert(ctx, dep, actor, opts); err != nil {
			return fmt.Errorf("reparent: add new parent %s: %w", addParentID, err)
		}
	}
	return nil
}

func sameStringSet(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for value := range left {
		if _, ok := right[value]; !ok {
			return false
		}
	}
	return true
}

// sortedSetDifference returns the members of left absent from right, sorted so
// the writes it drives land in a deterministic order.
func sortedSetDifference(left, right map[string]struct{}) []string {
	values := make([]string, 0, len(left))
	for value := range left {
		if _, ok := right[value]; !ok {
			values = append(values, value)
		}
	}
	sort.Strings(values)
	return values
}
