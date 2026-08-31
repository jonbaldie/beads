package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

func executeGraphApply(ctx context.Context, plan *GraphApplyPlan, opts GraphApplyOptions) (*GraphApplyResult, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}

	keyToID := make(map[string]string, len(plan.Nodes))
	owner := getOwner()
	commitMsg := plan.CommitMessage
	if commitMsg == "" {
		commitMsg = fmt.Sprintf("bd: graph-apply %d nodes", len(plan.Nodes))
	}

	if err := getStore().RunInTransaction(ctx, commitMsg, func(tx storage.Transaction) error {
		return applyGraphApplyTransaction(ctx, tx, plan, opts, owner, keyToID)
	}); err != nil {
		return nil, err
	}

	return &GraphApplyResult{IDs: keyToID}, nil
}

func applyGraphApplyTransaction(ctx context.Context, tx storage.Transaction, plan *GraphApplyPlan, opts GraphApplyOptions, owner string, keyToID map[string]string) error {
	issues, pendingAssignees, err := prepareGraphApplyIssues(plan, opts, owner)
	if err != nil {
		return err
	}
	if err := tx.CreateIssues(ctx, issues, getActor()); err != nil {
		return fmt.Errorf("batch create: %w", err)
	}
	graphApplyIssueIDs(plan, issues, keyToID)
	if err := applyGraphApplyMetadataRefs(ctx, tx, plan, issues, keyToID); err != nil {
		return err
	}

	parentDepPairs := graphApplyParentDepPairs(plan.Nodes, keyToID)
	if err := validateGraphApplyRelationships(ctx, tx, plan, keyToID, parentDepPairs); err != nil {
		return err
	}
	newSchedulingEdges, err := addGraphApplyParentDependencies(ctx, tx, plan, issues, keyToID)
	if err != nil {
		return err
	}
	newSchedulingEdges, err = addGraphApplyPlanDependencies(ctx, tx, plan, issues, keyToID, parentDepPairs, newSchedulingEdges)
	if err != nil {
		return err
	}
	if err := validateGraphApplyFinalCycles(ctx, tx, newSchedulingEdges); err != nil {
		return err
	}
	return applyGraphApplyPendingAssignees(ctx, tx, plan, issues, pendingAssignees)
}

func prepareGraphApplyIssues(plan *GraphApplyPlan, opts GraphApplyOptions, owner string) ([]*types.Issue, map[int]string, error) {
	issues := make([]*types.Issue, 0, len(plan.Nodes))
	pendingAssignees := make(map[int]string)
	for i, node := range plan.Nodes {
		issue, err := graphApplyNodeIssue(node, opts, getActor(), owner)
		if err != nil {
			return nil, nil, err
		}
		if node.graphApplyNodeIssueFields.Assignee != "" {
			if node.graphApplyNodeIssueFields.AssignAfterCreate {
				pendingAssignees[i] = node.graphApplyNodeIssueFields.Assignee
			} else {
				issue.Assignee = node.graphApplyNodeIssueFields.Assignee
			}
		}
		issues = append(issues, issue)
	}
	return issues, pendingAssignees, nil
}

func graphApplyIssueIDs(plan *GraphApplyPlan, issues []*types.Issue, keyToID map[string]string) {
	for i, node := range plan.Nodes {
		keyToID[node.Key] = issues[i].ID
	}
}

func applyGraphApplyMetadataRefs(ctx context.Context, tx storage.Transaction, plan *GraphApplyPlan, issues []*types.Issue, keyToID map[string]string) error {
	// Resolve MetadataRefs now that all IDs are known.
	for i, node := range plan.Nodes {
		if len(node.graphApplyNodeExtendedFields.MetadataRefs) == 0 {
			continue
		}
		metaJSON, err := types.MergeMetadataRefs(issues[i].Metadata, node.graphApplyNodeExtendedFields.MetadataRefs, keyToID)
		if err != nil {
			return fmt.Errorf("node %q: %w", node.Key, err)
		}
		updates := map[string]interface{}{
			"metadata": metaJSON,
		}
		if err := tx.UpdateIssue(ctx, issues[i].ID, updates, getActor()); err != nil {
			return fmt.Errorf("node %q: updating metadata refs: %w", node.Key, err)
		}
	}
	return nil
}

func validateGraphApplyRelationships(ctx context.Context, tx storage.Transaction, plan *GraphApplyPlan, keyToID map[string]string, parentDepPairs map[string]bool) error {
	if err := validateGraphApplyPlannedParentBlockingPaths(ctx, tx, plan, keyToID, parentDepPairs); err != nil {
		return err
	}
	if err := validateGraphApplyPlannedBlockingCycles(ctx, tx, plan, keyToID); err != nil {
		return err
	}
	return validateGraphApplyEdgeConflicts(plan, keyToID, parentDepPairs)
}

func validateGraphApplyEdgeConflicts(plan *GraphApplyPlan, keyToID map[string]string, parentDepPairs map[string]bool) error {
	for i, edge := range plan.Edges {
		fromID := resolveEdgeRef(edge.FromKey, edge.FromID, keyToID)
		toID := resolveEdgeRef(edge.ToKey, edge.ToID, keyToID)
		depType := graphApplyDependencyType(edge.Type)
		if parentDepPairs[graphApplyDepPairKey(fromID, toID)] && depType != types.DepParentChild {
			return fmt.Errorf("edge %d %s->%s duplicates a parent-child relationship with dependency type %q", i, fromID, toID, depType)
		}
		if parentDepPairs[graphApplyDepPairKey(toID, fromID)] && graphApplyCycleRelevantDependencyType(depType) {
			return fmt.Errorf("edge %d %s->%s creates a blocking reverse of a parent-child relationship", i, fromID, toID)
		}
	}
	return nil
}

func addGraphApplyParentDependencies(ctx context.Context, tx storage.Transaction, plan *GraphApplyPlan, issues []*types.Issue, keyToID map[string]string) ([][2]string, error) {
	// Add node parent-child dependencies first. The explicit and inline
	// dependency sources below are also processed parent-first, so every
	// blocking edge sees the plan's full hierarchy in storage.
	newSchedulingEdges := make([][2]string, 0, len(plan.Nodes)+len(plan.Edges))
	for i, node := range plan.Nodes {
		parentID := node.ParentID
		if parentKey := node.effectiveParentKey(); parentKey != "" {
			parentID = keyToID[parentKey]
		}
		if parentID == "" {
			continue
		}
		dep := &types.Dependency{
			IssueID:     issues[i].ID,
			DependsOnID: parentID,
			Type:        types.DepParentChild,
		}
		if err := tx.AddDependency(ctx, dep, getActor()); err != nil {
			return nil, fmt.Errorf("node %q: adding parent-child dep: %w", node.Key, err)
		}
		newSchedulingEdges = append(newSchedulingEdges, [2]string{dep.IssueID, dep.DependsOnID})
	}
	return newSchedulingEdges, nil
}

func addGraphApplyPlanDependencies(ctx context.Context, tx storage.Transaction, plan *GraphApplyPlan, issues []*types.Issue, keyToID map[string]string, parentDepPairs map[string]bool, newSchedulingEdges [][2]string) ([][2]string, error) {
	for phase := 0; phase < 2; phase++ {
		parentPhase := phase == 0
		var err error
		newSchedulingEdges, err = addGraphApplyExplicitPhase(ctx, tx, plan, keyToID, parentDepPairs, parentPhase, newSchedulingEdges)
		if err != nil {
			return nil, err
		}
		newSchedulingEdges, err = addGraphApplyInlinePhase(ctx, tx, plan, issues, keyToID, parentPhase, newSchedulingEdges)
		if err != nil {
			return nil, err
		}
	}
	return newSchedulingEdges, nil
}

func addGraphApplyExplicitPhase(ctx context.Context, tx storage.Transaction, plan *GraphApplyPlan, keyToID map[string]string, parentDepPairs map[string]bool, parentPhase bool, schedulingEdges [][2]string) ([][2]string, error) {
	// Add explicit edges in stable order for this phase.
	for i, edge := range plan.Edges {
		schedulingEdge, add, err := addGraphApplyExplicitEdge(ctx, tx, edge, i, keyToID, parentDepPairs, parentPhase)
		if err != nil {
			return nil, err
		}
		if add {
			schedulingEdges = append(schedulingEdges, schedulingEdge)
		}
	}
	return schedulingEdges, nil
}

func addGraphApplyExplicitEdge(ctx context.Context, tx storage.Transaction, edge GraphApplyEdge, index int, keyToID map[string]string, parentDepPairs map[string]bool, parentPhase bool) ([2]string, bool, error) {
	fromID := resolveEdgeRef(edge.FromKey, edge.FromID, keyToID)
	toID := resolveEdgeRef(edge.ToKey, edge.ToID, keyToID)
	depType := graphApplyDependencyType(edge.Type)
	if (depType == types.DepParentChild) != parentPhase {
		return [2]string{}, false, nil
	}
	if parentDepPairs[graphApplyDepPairKey(fromID, toID)] {
		if depType == types.DepParentChild {
			return [2]string{}, false, nil
		}
		return [2]string{}, false, fmt.Errorf("edge %d %s->%s duplicates a parent-child relationship with dependency type %q", index, fromID, toID, depType)
	}
	if parentDepPairs[graphApplyDepPairKey(toID, fromID)] && graphApplyCycleRelevantDependencyType(depType) {
		return [2]string{}, false, fmt.Errorf("edge %d %s->%s creates a blocking reverse of a parent-child relationship", index, fromID, toID)
	}
	dep, err := types.NewGraphEdgeDependency(fromID, toID, depType, edge.Gate, edge.SpawnerKey, edge.SpawnerID, edge.ThreadID, keyToID)
	if err != nil {
		return [2]string{}, false, fmt.Errorf("edge %s->%s: %w", fromID, toID, err)
	}
	if err := tx.AddDependencyWithOptions(ctx, dep, getActor(), storage.DependencyAddOptions{}); err != nil {
		return [2]string{}, false, fmt.Errorf("adding edge %s->%s: %w", fromID, toID, err)
	}
	if !graphApplySchedulingDependencyType(depType) {
		return [2]string{}, false, nil
	}
	return [2]string{fromID, toID}, true, nil
}

func addGraphApplyInlinePhase(ctx context.Context, tx storage.Transaction, plan *GraphApplyPlan, issues []*types.Issue, keyToID map[string]string, parentPhase bool, schedulingEdges [][2]string) ([][2]string, error) {
	// Add per-node inline dependencies in stable order for this phase.
	for i, node := range plan.Nodes {
		var err error
		schedulingEdges, err = addGraphApplyInlineNode(ctx, tx, node, issues[i].ID, keyToID, parentPhase, schedulingEdges)
		if err != nil {
			return nil, err
		}
	}
	return schedulingEdges, nil
}

func addGraphApplyInlineNode(ctx context.Context, tx storage.Transaction, node GraphApplyNode, issueID string, keyToID map[string]string, parentPhase bool, schedulingEdges [][2]string) ([][2]string, error) {
	for _, inlineDep := range node.Deps {
		depType := types.DependencyType(inlineDep.Type)
		if depType == "" {
			depType = types.DepBlocks
		}
		if (depType == types.DepParentChild) != parentPhase {
			continue
		}
		schedulingEdge, add, err := addGraphApplyInlineDependency(ctx, tx, node, issueID, inlineDep, depType, keyToID)
		if err != nil {
			return nil, err
		}
		if add {
			schedulingEdges = append(schedulingEdges, schedulingEdge)
		}
	}
	return schedulingEdges, nil
}

func addGraphApplyInlineDependency(ctx context.Context, tx storage.Transaction, node GraphApplyNode, issueID string, inlineDep GraphApplyNodeDep, depType types.DependencyType, keyToID map[string]string) ([2]string, bool, error) {
	dep, err := types.NewGraphNodeDependency(issueID, depType, inlineDep.Target, keyToID)
	if err != nil {
		return [2]string{}, false, fmt.Errorf("node %q: %w", node.Key, err)
	}
	if err := tx.AddDependency(ctx, dep, getActor()); err != nil {
		return [2]string{}, false, fmt.Errorf("node %q: adding dep to %q: %w", node.Key, inlineDep.Target, err)
	}
	if !graphApplySchedulingDependencyType(dep.Type) {
		return [2]string{}, false, nil
	}
	return [2]string{dep.IssueID, dep.DependsOnID}, true, nil
}

func validateGraphApplyFinalCycles(ctx context.Context, tx storage.Transaction, schedulingEdges [][2]string) error {
	cyclePath, err := tx.CycleThroughEdges(ctx, schedulingEdges)
	if err != nil {
		return fmt.Errorf("final graph cycle check: %w", err)
	}
	if cyclePath != "" {
		return fmt.Errorf("graph dependency cycle would be created: %s", cyclePath)
	}
	return nil
}

func applyGraphApplyPendingAssignees(ctx context.Context, tx storage.Transaction, plan *GraphApplyPlan, issues []*types.Issue, pendingAssignees map[int]string) error {
	// Apply deferred assignees.
	for i, assignee := range pendingAssignees {
		updates := map[string]interface{}{
			"assignee": assignee,
		}
		if err := tx.UpdateIssue(ctx, issues[i].ID, updates, getActor()); err != nil {
			return fmt.Errorf("node %q: setting assignee: %w", plan.Nodes[i].Key, err)
		}
	}
	return nil
}

// validateGraphApplyPlannedBlockingCycles rejects planned blocking edges that
// would close a blocking-dependency cycle, evaluated whole-graph before any
// insert. This early preflight is restricted to blocking edges for precise
// plan errors. Each stored edge still runs issueops.CheckDependencyCycleInTx,
// which enforces the combined blocks + conditional-blocks + parent-child graph.
func validateGraphApplyPlannedBlockingCycles(ctx context.Context, tx storage.Transaction, plan *GraphApplyPlan, keyToID map[string]string) error {
	type plannedEdge struct {
		index  int
		fromID string
		toID   string
	}

	adj := make(map[string][]string)
	checks := make([]plannedEdge, 0, len(plan.Edges))
	for i, edge := range plan.Edges {
		depType := graphApplyDependencyType(edge.Type)
		if !graphApplyCycleRelevantDependencyType(depType) {
			continue
		}
		fromID := resolveEdgeRef(edge.FromKey, edge.FromID, keyToID)
		toID := resolveEdgeRef(edge.ToKey, edge.ToID, keyToID)
		if fromID == "" || toID == "" {
			continue
		}
		if fromID == toID {
			return fmt.Errorf("edge %d %s->%s creates a blocking dependency cycle", i, fromID, toID)
		}
		adj[fromID] = append(adj[fromID], toID)
		checks = append(checks, plannedEdge{index: i, fromID: fromID, toID: toID})
	}

	depCache := make(map[string][]*types.Dependency)
	for _, edge := range checks {
		hasPath, err := graphApplyHasPath(ctx, tx, adj, depCache, edge.toID, edge.fromID, graphApplyCycleRelevantDependencyType)
		if err != nil {
			return fmt.Errorf("edge %d %s->%s: checking planned blocking cycle: %w", edge.index, edge.fromID, edge.toID, err)
		}
		if hasPath {
			return fmt.Errorf("edge %d %s->%s creates a blocking dependency cycle", edge.index, edge.fromID, edge.toID)
		}
	}
	return nil
}

// validateGraphApplyPlannedParentBlockingPaths rejects plans where a planned
// blocking edge would create a path from a parent to its child. Unlike
// validateGraphApplyPlannedBlockingCycles, its existing-dep walk follows the
// full AffectsReadyWork set (blocks, conditional-blocks, parent-child,
// waits-for) because a parent→child path closed through any ready-affecting
// dependency is a real ready-work deadlock. The two predicates must stay
// distinct: narrowing this one would miss real deadlocks, while this broader
// walk may additionally reject a return path through waits-for.
func validateGraphApplyPlannedParentBlockingPaths(ctx context.Context, tx storage.Transaction, plan *GraphApplyPlan, keyToID map[string]string, parentDepPairs map[string]bool) error {
	adj := graphApplyParentPathAdjacency(plan, keyToID, parentDepPairs)

	depCache := make(map[string][]*types.Dependency)
	for _, node := range plan.Nodes {
		if err := validateGraphApplyParentPath(ctx, tx, adj, depCache, node, keyToID); err != nil {
			return err
		}
	}
	return nil
}

func graphApplyParentPathAdjacency(plan *GraphApplyPlan, keyToID map[string]string, parentDepPairs map[string]bool) map[string][]string {
	adj := graphApplyParentDependencyAdjacency(parentDepPairs)
	return graphApplyPlannedReadyPathAdjacency(adj, plan, keyToID, parentDepPairs)
}

func graphApplyParentDependencyAdjacency(parentDepPairs map[string]bool) map[string][]string {
	adj := make(map[string][]string)
	for pair := range parentDepPairs {
		fromID, toID, ok := graphApplyDepPairIDs(pair)
		if ok {
			adj[fromID] = append(adj[fromID], toID)
		}
	}
	return adj
}

func graphApplyPlannedReadyPathAdjacency(adj map[string][]string, plan *GraphApplyPlan, keyToID map[string]string, parentDepPairs map[string]bool) map[string][]string {
	for _, edge := range plan.Edges {
		depType := graphApplyDependencyType(edge.Type)
		if !graphApplyReadyPathDependencyType(depType) {
			continue
		}
		fromID := resolveEdgeRef(edge.FromKey, edge.FromID, keyToID)
		toID := resolveEdgeRef(edge.ToKey, edge.ToID, keyToID)
		if fromID == "" || toID == "" {
			continue
		}
		// Direct parent -> child blocking edges have a dedicated error below.
		// This prewrite pass covers transitive parent -> ... -> child paths.
		if graphApplyCycleRelevantDependencyType(depType) && parentDepPairs[graphApplyDepPairKey(toID, fromID)] {
			continue
		}
		adj[fromID] = append(adj[fromID], toID)
	}
	return adj
}

func validateGraphApplyParentPath(ctx context.Context, tx storage.Transaction, adj map[string][]string, depCache map[string][]*types.Dependency, node GraphApplyNode, keyToID map[string]string) error {
	childID, parentID := graphApplyParentPathIDs(node, keyToID)
	if childID == "" || parentID == "" {
		return nil
	}
	hasPath, err := graphApplyHasPath(ctx, tx, adj, depCache, parentID, childID, graphApplyReadyPathDependencyType)
	if err != nil {
		return err
	}
	if hasPath {
		return fmt.Errorf("node %q: planned blocking dependencies create a path from parent %q to child %q", node.Key, parentID, childID)
	}
	return nil
}

func graphApplyParentPathIDs(node GraphApplyNode, keyToID map[string]string) (childID, parentID string) {
	childID = keyToID[node.Key]
	parentID = node.ParentID
	if parentKey := node.effectiveParentKey(); parentKey != "" {
		parentID = keyToID[parentKey]
	}
	return childID, parentID
}

// graphApplyHasPath reports whether fromID can reach toID by following the
// in-memory planned adjacency plus existing store dependencies. followExistingDep
// selects which existing dep types the walk traverses, letting callers mirror
// either the early blocking-only preflight or the broader ready-work graph.
func graphApplyHasPath(ctx context.Context, tx storage.Transaction, adj map[string][]string, depCache map[string][]*types.Dependency, fromID, toID string, followExistingDep func(types.DependencyType) bool) (bool, error) {
	search := graphApplyPathSearch{
		ctx:               ctx,
		tx:                tx,
		adj:               adj,
		depCache:          depCache,
		toID:              toID,
		followExistingDep: followExistingDep,
		seen:              make(map[string]bool),
	}
	return search.visit(fromID)
}

type graphApplyPathSearch struct {
	ctx               context.Context
	tx                storage.Transaction
	adj               map[string][]string
	depCache          map[string][]*types.Dependency
	toID              string
	followExistingDep func(types.DependencyType) bool
	seen              map[string]bool
}

func (s *graphApplyPathSearch) visit(id string) (bool, error) {
	if id == s.toID {
		return true, nil
	}
	if s.seen[id] {
		return false, nil
	}
	s.seen[id] = true
	if found, err := s.visitPlanned(id); err != nil || found {
		return found, err
	}
	deps, err := s.dependencies(id)
	if err != nil {
		return false, err
	}
	return s.visitExisting(deps)
}

func (s *graphApplyPathSearch) visitPlanned(id string) (bool, error) {
	for _, next := range s.adj[id] {
		found, err := s.visit(next)
		if err != nil || found {
			return found, err
		}
	}
	return false, nil
}

func (s *graphApplyPathSearch) dependencies(id string) ([]*types.Dependency, error) {
	if deps, ok := s.depCache[id]; ok {
		return deps, nil
	}
	deps, err := s.tx.GetDependencyRecords(s.ctx, id)
	if err != nil {
		return nil, fmt.Errorf("reading existing dependencies for %s: %w", id, err)
	}
	s.depCache[id] = deps
	return deps, nil
}

func (s *graphApplyPathSearch) visitExisting(deps []*types.Dependency) (bool, error) {
	for _, dep := range deps {
		if !s.followExistingDep(dep.Type) {
			continue
		}
		found, err := s.visit(dep.DependsOnID)
		if err != nil || found {
			return found, err
		}
	}
	return false, nil
}

// graphApplyEdgeIsLocalCycleRelevant reports whether an edge participates in the
// in-memory local cycle check run by validateGraphApplyLocalCycles: it must be a
// fully-local edge (both endpoints addressed by key, neither by an existing ID)
// of a cycle-relevant dependency type.
func graphApplyEdgeIsLocalCycleRelevant(edge GraphApplyEdge, depType types.DependencyType) bool {
	if edge.FromKey == "" || edge.ToKey == "" || edge.FromID != "" || edge.ToID != "" {
		return false
	}
	return graphApplyCycleRelevantDependencyType(depType)
}

func graphApplyDependencyType(depType string) types.DependencyType {
	if depType == "" {
		return types.DepBlocks
	}
	return types.DependencyType(depType)
}

func graphApplyCycleRelevantDependencyType(depType types.DependencyType) bool {
	return depType == types.DepBlocks || depType == types.DepConditionalBlocks
}

func graphApplySchedulingDependencyType(depType types.DependencyType) bool {
	return graphApplyCycleRelevantDependencyType(depType) || depType == types.DepParentChild
}

func graphApplyReadyPathDependencyType(depType types.DependencyType) bool {
	return depType.AffectsReadyWork()
}

func graphApplySortedKeys(keys map[string]bool) []string {
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func graphApplyParentDepPairs(nodes []GraphApplyNode, keyToID map[string]string) map[string]bool {
	pairs := make(map[string]bool)
	for _, node := range nodes {
		parentID := node.ParentID
		if parentKey := node.effectiveParentKey(); parentKey != "" {
			parentID = keyToID[parentKey]
		}
		childID := keyToID[node.Key]
		if childID != "" && parentID != "" {
			pairs[graphApplyDepPairKey(childID, parentID)] = true
		}
	}
	return pairs
}

func graphApplyDepPairKey(issueID, dependsOnID string) string {
	return issueID + "\x00" + dependsOnID
}

func graphApplyDepPairIDs(pair string) (string, string, bool) {
	return strings.Cut(pair, "\x00")
}

func resolveEdgeRef(key, id string, keyToID map[string]string) string {
	if id != "" {
		return id
	}
	if key != "" {
		return keyToID[key]
	}
	return ""
}
