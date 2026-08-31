package domain

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/types"
)

func checkGraphApplyCycles(ctx context.Context, repo DependencySQLRepository, edges [][2]string) error {
	cyclePath, err := repo.CycleThroughEdges(ctx, edges)
	if err != nil {
		return fmt.Errorf("applyGraph: final cycle check: %w", err)
	}
	if cyclePath != "" {
		return fmt.Errorf("applyGraph: dependency cycle would be created: %s", cyclePath)
	}
	return nil
}

func applyDeferredGraphAssignees(ctx context.Context, repo IssueSQLRepository, plan GraphPlan, keyToID map[string]string, pendingAssignees map[int]string, actor string, useWisp bool) error {
	for i, assignee := range pendingAssignees {
		if assignee == "" {
			continue
		}
		id := keyToID[plan.Nodes[i].Key]
		if err := repo.Update(ctx, id, map[string]any{"assignee": assignee}, actor, IssueTableOpts{UseWispsTable: useWisp}); err != nil {
			return fmt.Errorf("applyGraph: node %q: defer assignee: %w", plan.Nodes[i].Key, err)
		}
	}
	return nil
}

// graphParentDepPairs encodes the (childID, parentID) parent-child pairs
// implied by the plan's node ParentKey/ParentID fields. Used by applyGraph
// to dedup explicit edges against implicit parent-child relationships and
// to seed the in-memory adjacency for live cycle detection.
func graphParentDepPairs(nodes []GraphNode, keyToID map[string]string) map[string]bool {
	pairs := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		parentID := n.ParentID
		if n.ParentKey != "" {
			parentID = keyToID[n.ParentKey]
		}
		childID := keyToID[n.Key]
		if childID == "" || parentID == "" {
			continue
		}
		pairs[depPairKey(childID, parentID)] = true
	}
	return pairs
}

// depPairKey encodes an ordered (issueID, dependsOnID) pair using a NUL
// separator so the two halves can be recovered unambiguously without ID
// characters colliding with the delimiter.
func depPairKey(issueID, dependsOnID string) string {
	return issueID + "\x00" + dependsOnID
}

// depPairIDs decodes a key produced by depPairKey, returning (from, to, ok).
func depPairIDs(pair string) (string, string, bool) {
	i := strings.IndexByte(pair, 0)
	if i >= 0 {
		return pair[:i], pair[i+1:], true
	}
	return "", "", false
}

// cycleRelevantDepType returns true for dep types whose presence in the
// reverse direction of a parent-child link would form a cycle.
func cycleRelevantDepType(t types.DependencyType) bool {
	return t == types.DepBlocks || t == types.DepConditionalBlocks
}

// readyPathDepType reports whether a dependency type affects ready-work. It is
// the broad predicate used when walking existing deps for parent→child
// blocking-path validation, in contrast to the blocking-only
// cycleRelevantDepType used by the early pure-blocking preflight. The two must
// stay distinct: narrowing the parent-path walk would miss real ready-work
// deadlocks, while it may additionally reject a return path through waits-for.
func readyPathDepType(t types.DependencyType) bool {
	return t.AffectsReadyWork()
}

// resolveEdgeRef returns the ID for an edge endpoint: the explicit id when set
// (an ID override wins over a plan-local key, matching the CLI embedded path),
// else the keyToID lookup. Returns "" when neither resolves, which the caller
// should treat as a structural error.
func resolveEdgeRef(key, id string, keyToID map[string]string) string {
	if id != "" {
		return id
	}
	return keyToID[key]
}

// validatePlannedBlockingPaths rejects plans that would close a cycle
// once the parent-child deps are inserted. The adjacency it walks combines
// the planned parent-child pairs (child → parent), the cycle-relevant
// planned edges (excluding reverse-of-parent-child which is rejected by
// applyGraph's dedup pass), and live AffectsReadyWork dependencies pulled
// lazily from the store via depRepo. Mirrors embedded
// validateGraphApplyPlannedParentBlockingPaths.
func (u *issueGraphModule) validatePlannedBlockingPaths(
	ctx context.Context,
	plan GraphPlan,
	keyToID map[string]string,
	parentDepPairs map[string]bool,
) error {
	adj := plannedBlockingPathAdjacency(plan, keyToID, parentDepPairs)
	depCache := make(map[string][]*types.Dependency)
	return u.validatePlannedParentPaths(ctx, plan.Nodes, keyToID, adj, depCache)
}

func plannedBlockingPathAdjacency(plan GraphPlan, keyToID map[string]string, parentDepPairs map[string]bool) map[string][]string {
	adj := make(map[string][]string)
	for pair := range parentDepPairs {
		fromID, toID, ok := depPairIDs(pair)
		if ok {
			adj[fromID] = append(adj[fromID], toID)
		}
	}
	for _, edge := range plan.Edges {
		depType := graphEdgeType(edge.Type)
		if !readyPathDepType(depType) {
			continue
		}
		fromID := resolveEdgeRef(edge.FromKey, edge.FromID, keyToID)
		toID := resolveEdgeRef(edge.ToKey, edge.ToID, keyToID)
		if fromID == "" || toID == "" {
			continue
		}
		// Skip the reverse-of-parent-child case for cycle-relevant types —
		// applyGraph's edge dedup already errors on those with a clearer
		// message, so we don't want them showing up here as ambiguous
		// "blocking path" errors.
		if cycleRelevantDepType(depType) && parentDepPairs[depPairKey(toID, fromID)] {
			continue
		}
		adj[fromID] = append(adj[fromID], toID)
	}
	return adj
}

func (u *issueGraphModule) validatePlannedParentPaths(
	ctx context.Context,
	nodes []GraphNode,
	keyToID map[string]string,
	adj map[string][]string,
	depCache map[string][]*types.Dependency,
) error {
	for _, node := range nodes {
		parentID, childID := graphNodeParentIDs(node, keyToID)
		if childID == "" || parentID == "" {
			continue
		}
		hasPath, err := u.graphHasPath(ctx, adj, depCache, parentID, childID, readyPathDepType)
		if err != nil {
			return err
		}
		if hasPath {
			return fmt.Errorf("applyGraph: node %q: planned blocking dependencies create a path from parent %q to child %q", node.Key, parentID, childID)
		}
	}
	return nil
}

func graphNodeParentIDs(node GraphNode, keyToID map[string]string) (string, string) {
	parentID := node.ParentID
	if node.ParentKey != "" {
		parentID = keyToID[node.ParentKey]
	}
	return parentID, keyToID[node.Key]
}

// validatePlannedBlockingCycles rejects planned blocking edges that would close
// a blocking-dependency cycle, evaluated whole-graph before any insert. It
// mirrors embedded validateGraphApplyPlannedBlockingCycles. This early
// preflight is intentionally restricted to blocking edges; repository Insert
// subsequently enforces the combined scheduling graph for every stored edge.
func (u *issueGraphModule) validatePlannedBlockingCycles(
	ctx context.Context,
	plan GraphPlan,
	keyToID map[string]string,
) error {
	adj, checks, err := plannedBlockingGraph(plan, keyToID)
	if err != nil {
		return err
	}
	depCache := make(map[string][]*types.Dependency)
	return u.validatePlannedCycleEdges(ctx, adj, checks, depCache)
}

type plannedBlockingEdge struct {
	index  int
	fromID string
	toID   string
}

func plannedBlockingGraph(plan GraphPlan, keyToID map[string]string) (map[string][]string, []plannedBlockingEdge, error) {
	adj := make(map[string][]string)
	checks := make([]plannedBlockingEdge, 0, len(plan.Edges))
	for i, edge := range plan.Edges {
		depType := graphEdgeType(edge.Type)
		if !cycleRelevantDepType(depType) {
			continue
		}
		fromID := resolveEdgeRef(edge.FromKey, edge.FromID, keyToID)
		toID := resolveEdgeRef(edge.ToKey, edge.ToID, keyToID)
		if fromID == "" || toID == "" {
			continue
		}
		if fromID == toID {
			return nil, nil, fmt.Errorf("applyGraph: edge %d %s->%s creates a blocking dependency cycle", i, fromID, toID)
		}
		adj[fromID] = append(adj[fromID], toID)
		checks = append(checks, plannedBlockingEdge{index: i, fromID: fromID, toID: toID})
	}
	return adj, checks, nil
}
func (u *issueGraphModule) validatePlannedCycleEdges(ctx context.Context, adj map[string][]string, checks []plannedBlockingEdge, depCache map[string][]*types.Dependency) error {
	for _, edge := range checks {
		hasPath, err := u.graphHasPath(ctx, adj, depCache, edge.toID, edge.fromID, cycleRelevantDepType)
		if err != nil {
			return fmt.Errorf("applyGraph: edge %d %s->%s: checking planned blocking cycle: %w", edge.index, edge.fromID, edge.toID, err)
		}
		if hasPath {
			return fmt.Errorf("applyGraph: edge %d %s->%s creates a blocking dependency cycle", edge.index, edge.fromID, edge.toID)
		}
	}
	return nil
}

// graphHasPath returns true if fromID can reach toID by following the
// in-memory adjacency (planned parent-child + planned blocking edges) and
// existing deps loaded lazily from the store. followExistingDep selects which
// existing dep types the walk traverses, so callers can mirror either the
// early blocking-only preflight or the broader ready-work graph. Per-node dep
// fetches are cached so each visited node hits the DB at most once.
//
// Existing deps are loaded from BOTH dependency tables. The per-edge
// depRepo.HasCycle probe this walk replaced traversed dependencies ∪
// wisp_dependencies (and the embedded path's GetDependencyRecords selects the
// table per node), so a blocking cycle that closes through the other table —
// e.g. an existing wisp edge reached during a regular graph-apply — must still
// be detected regardless of which table this graph-apply primarily writes.
func (u *issueGraphModule) graphHasPath(
	ctx context.Context,
	adj map[string][]string,
	depCache map[string][]*types.Dependency,
	fromID, toID string,
	followExistingDep func(types.DependencyType) bool,
) (bool, error) {
	return visitGraphPath(ctx, u.depRepo, adj, depCache, fromID, toID, followExistingDep, make(map[string]bool))
}

func visitGraphPath(
	ctx context.Context,
	depRepo DependencySQLRepository,
	adj map[string][]string,
	depCache map[string][]*types.Dependency,
	fromID, toID string,
	followExistingDep func(types.DependencyType) bool,
	seen map[string]bool,
) (bool, error) {
	if fromID == toID {
		return true, nil
	}
	if seen[fromID] {
		return false, nil
	}
	seen[fromID] = true
	found, err := visitPlannedGraphEdges(ctx, depRepo, adj, depCache, fromID, toID, followExistingDep, seen)
	if err != nil || found {
		return found, err
	}
	deps, err := cachedGraphPathDependencies(ctx, depRepo, depCache, fromID)
	if err != nil {
		return false, err
	}
	return visitExistingGraphEdges(ctx, depRepo, adj, depCache, deps, toID, followExistingDep, seen)
}

func visitPlannedGraphEdges(
	ctx context.Context,
	depRepo DependencySQLRepository,
	adj map[string][]string,
	depCache map[string][]*types.Dependency,
	fromID, toID string,
	followExistingDep func(types.DependencyType) bool,
	seen map[string]bool,
) (bool, error) {
	for _, nextID := range adj[fromID] {
		found, err := visitGraphPath(ctx, depRepo, adj, depCache, nextID, toID, followExistingDep, seen)
		if err != nil || found {
			return found, err
		}
	}
	return false, nil
}

func cachedGraphPathDependencies(ctx context.Context, depRepo DependencySQLRepository, depCache map[string][]*types.Dependency, id string) ([]*types.Dependency, error) {
	if deps, ok := depCache[id]; ok {
		return deps, nil
	}
	regular, err := depRepo.ListByIssueIDs(ctx, []string{id}, DepListOpts{
		Direction:     DepDirectionOut,
		UseWispsTable: false,
	})
	if err != nil {
		return nil, fmt.Errorf("applyGraph: read existing deps for %s: %w", id, err)
	}
	wisp, err := depRepo.ListByIssueIDs(ctx, []string{id}, DepListOpts{
		Direction:     DepDirectionOut,
		UseWispsTable: true,
	})
	if err != nil {
		return nil, fmt.Errorf("applyGraph: read existing wisp deps for %s: %w", id, err)
	}
	deps := append(regular.Outgoing[id], wisp.Outgoing[id]...)
	depCache[id] = deps
	return deps, nil
}

func visitExistingGraphEdges(
	ctx context.Context,
	depRepo DependencySQLRepository,
	adj map[string][]string,
	depCache map[string][]*types.Dependency,
	deps []*types.Dependency,
	toID string,
	followExistingDep func(types.DependencyType) bool,
	seen map[string]bool,
) (bool, error) {
	for _, dep := range deps {
		if !followExistingDep(dep.Type) {
			continue
		}
		found, err := visitGraphPath(ctx, depRepo, adj, depCache, dep.DependsOnID, toID, followExistingDep, seen)
		if err != nil || found {
			return found, err
		}
	}
	return false, nil
}
