package domain

import (
	"context"
	"errors"
	"fmt"

	"github.com/jonbaldie/beads/internal/types"
)

// AddDependencies asserts every edge in one pass, writing each to the plane its
// own source lives in. The ordering and the final cycle gate deliberately do
// not partition by plane, because the hierarchy a blocking edge is checked
// against and the graph the gate walks both span the two tables.
func (u *dependencyBatchMutationUseCase) AddDependencies(ctx context.Context, deps []*types.Dependency, actor string, opts BulkAddDepsOpts) (BulkAddDepsResult, error) {
	return addDependencies(ctx, u.depRepo, deps, actor, opts)
}

func addDependencies(ctx context.Context, depRepo DependencySQLRepository, deps []*types.Dependency, actor string, opts BulkAddDepsOpts) (BulkAddDepsResult, error) {
	if len(deps) == 0 {
		return BulkAddDepsResult{Added: []*types.Dependency{}}, nil
	}
	if err := validateBulkDependencies(deps); err != nil {
		return BulkAddDepsResult{}, err
	}
	sources := bulkDependencySources(deps)
	wispSources, err := depRepo.WispSourceIDs(ctx, sources)
	if err != nil {
		return BulkAddDepsResult{}, fmt.Errorf("add deps: classify sources: %w", err)
	}
	// Parent-child edges must be visible before blocking edges in the same
	// request. The shared repository guard can then evaluate existing + planned
	// ancestry without widening #4034 into #4035's combined-graph cycle check.
	if err := addDependencyPhase(ctx, depRepo, deps, actor, opts, wispSources, true); err != nil {
		return BulkAddDepsResult{}, err
	}
	if err := addDependencyPhase(ctx, depRepo, deps, actor, opts, wispSources, false); err != nil {
		return BulkAddDepsResult{}, err
	}
	if err := validateFinalDependencyCycle(ctx, depRepo, deps); err != nil {
		return BulkAddDepsResult{}, err
	}
	return BulkAddDepsResult{Added: deps}, nil
}

func validateBulkDependencies(deps []*types.Dependency) error {
	// Validate the entire input shape before the first write. Multi-edge callers
	// run in a UOW, but this also avoids an avoidable partial prefix for direct
	// use-case consumers.
	for i, dep := range deps {
		if dep == nil {
			return fmt.Errorf("add deps[%d]: dep must not be nil", i)
		}
		if dep.IssueID == "" || dep.DependsOnID == "" {
			return fmt.Errorf("add deps[%d]: IssueID and DependsOnID must be non-empty", i)
		}
		// Self-dependency guard mirrors the single-edge add() path and
		// issueops.CheckDependencyCycleInTx: reject a self-edge for ALL dep
		// types before the hierarchy/cycle probe, so a scheduling self-edge is
		// typed as ErrSelfDependency instead of tripping HasCycle (or the final
		// CycleThroughEdges gate) and surfacing as a cycle. The message is
		// byte-identical to every other self-dep site so the proxied bulk CLI
		// (bd dep add / bd link) shows one consistent self-dependency error.
		if dep.IssueID == dep.DependsOnID {
			return fmt.Errorf("%w: %s cannot depend on itself", ErrSelfDependency, dep.IssueID)
		}
	}
	return nil
}

func bulkDependencySources(deps []*types.Dependency) []string {
	sources := make([]string, 0, len(deps))
	for _, dep := range deps {
		sources = append(sources, dep.IssueID)
	}
	return sources
}

func addDependencyPhase(ctx context.Context, depRepo DependencySQLRepository, deps []*types.Dependency, actor string, opts BulkAddDepsOpts, wispSources map[string]struct{}, parentPhase bool) error {
	for i, dep := range deps {
		if (dep.Type == types.DepParentChild) != parentPhase {
			continue
		}
		if err := addDependencyInPhase(ctx, depRepo, dep, actor, opts, wispSources, i); err != nil {
			return err
		}
	}
	return nil
}

func addDependencyInPhase(ctx context.Context, depRepo DependencySQLRepository, dep *types.Dependency, actor string, opts BulkAddDepsOpts, wispSources map[string]struct{}, index int) error {
	if err := depRepo.ValidateBlockingHierarchy(ctx, dep); err != nil {
		var hierarchyConflict *DependencyHierarchyConflictError
		if errors.As(err, &hierarchyConflict) {
			return err
		}
		return fmt.Errorf("add deps[%d]: hierarchy check: %w", index, err)
	}
	if err := checkBulkDependencyCycle(ctx, depRepo, dep, opts, index); err != nil {
		return err
	}
	_, sourceIsWisp := wispSources[dep.IssueID]
	return insertBulkDependency(ctx, depRepo, dep, actor, sourceIsWisp, index)
}

func checkBulkDependencyCycle(ctx context.Context, depRepo DependencySQLRepository, dep *types.Dependency, opts BulkAddDepsOpts, index int) error {
	if opts.SkipPerEdgeCycleCheck || !types.IsSchedulingEdge(dep.Type) {
		return nil
	}
	cycle, err := depRepo.HasCycle(ctx, dep.IssueID, dep.DependsOnID)
	if err != nil {
		return fmt.Errorf("add deps[%d]: cycle check: %w", index, err)
	}
	if cycle {
		return cycleErrorf("add deps[%d]: adding %s -> %s would create a cycle", index, dep.IssueID, dep.DependsOnID)
	}
	return nil
}

func insertBulkDependency(ctx context.Context, depRepo DependencySQLRepository, dep *types.Dependency, actor string, sourceIsWisp bool, index int) error {
	// The explicit `bd dep add` / `bd link` verb records a dependency_added
	// event for each genuine new edge. UseWispsTable routes both the edge and
	// that event to the source's own pair of tables.
	if err := depRepo.Insert(ctx, dep, actor, DepInsertOpts{
		UseWispsTable:      sourceIsWisp,
		HierarchyValidated: true,
		CycleValidated:     true,
		EmitEvent:          true,
	}); err != nil {
		var hierarchyConflict *DependencyHierarchyConflictError
		if errors.As(err, &hierarchyConflict) {
			return err
		}
		var missingEndpoint *DependencyEndpointNotFoundError
		if errors.As(err, &missingEndpoint) {
			return err
		}
		return fmt.Errorf("add deps[%d]: insert: %w", index, err)
	}
	return nil
}

func validateFinalDependencyCycle(ctx context.Context, depRepo DependencySQLRepository, deps []*types.Dependency) error {
	pairs := make([][2]string, 0, len(deps))
	for _, dep := range deps {
		if !types.IsSchedulingEdge(dep.Type) {
			continue
		}
		pairs = append(pairs, [2]string{dep.IssueID, dep.DependsOnID})
	}
	if len(pairs) > 0 {
		cyclePath, err := depRepo.CycleThroughEdges(ctx, pairs)
		if err != nil {
			return fmt.Errorf("add deps: final cycle check: %w", err)
		}
		if cyclePath != "" {
			return cycleErrorf("add deps: dependency cycle would be created: %s", cyclePath)
		}
	}
	return nil
}

// ValidateBlockingHierarchy passes the edge straight to the repository check
// AddDependencies runs per edge, so a caller re-running the gate at the end of
// a mixed request raises the identical *DependencyHierarchyConflictError rather
// than a second opinion about the same graph.
func (u *dependencyBatchMutationUseCase) ValidateBlockingHierarchy(ctx context.Context, dep *types.Dependency) error {
	return u.depRepo.ValidateBlockingHierarchy(ctx, dep)
}

// CycleThroughEdges passes the pairs straight to the repository walk
// AddDependencies runs as its own final gate. The caller composes the refusal,
// because the two callers word it differently and both spellings are already
// pinned.
func (u *dependencyBatchMutationUseCase) CycleThroughEdges(ctx context.Context, edges [][2]string) (string, error) {
	if len(edges) == 0 {
		return "", nil
	}
	return u.depRepo.CycleThroughEdges(ctx, edges)
}

func (u *dependencyRecordsUseCase) GetIssueDependencyRecords(ctx context.Context, issueIDs []string) (map[string][]*types.Dependency, error) {
	if len(issueIDs) == 0 {
		return map[string][]*types.Dependency{}, nil
	}
	out, err := u.depRepo.GetDependencyRecordsForIssues(ctx, issueIDs)
	if err != nil {
		return nil, fmt.Errorf("GetIssueDependencyRecords: %w", err)
	}
	return out, nil
}

func (u *dependencyRecordsUseCase) GetWispDependencyRecords(ctx context.Context, wispIDs []string) (map[string][]*types.Dependency, error) {
	if len(wispIDs) == 0 {
		return map[string][]*types.Dependency{}, nil
	}
	out, err := u.depRepo.GetWispDependencyRecordsForIDs(ctx, wispIDs)
	if err != nil {
		return nil, fmt.Errorf("GetWispDependencyRecords: %w", err)
	}
	return out, nil
}
