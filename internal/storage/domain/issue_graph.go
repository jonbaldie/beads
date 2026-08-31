package domain

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/types"
)

func (u *issueGraphModule) ApplyIssueGraph(ctx context.Context, plan GraphPlan, actor string) (GraphApplyResult, error) {
	return u.applyGraph(ctx, plan, actor, false)
}

func (u *issueGraphModule) ApplyWispGraph(ctx context.Context, plan GraphPlan, actor string) (GraphApplyResult, error) {
	return u.applyGraph(ctx, plan, actor, true)
}

func (u *issueGraphModule) applyGraph(ctx context.Context, plan GraphPlan, actor string, useWisp bool) (GraphApplyResult, error) {
	keyToID, pendingAssignees, err := createGraphNodes(ctx, u.creator, plan, actor, useWisp)
	if err != nil {
		return GraphApplyResult{}, err
	}
	if err := applyGraphMetadataRefs(ctx, u.issueRepo, plan, keyToID, actor, useWisp); err != nil {
		return GraphApplyResult{}, err
	}
	parentDepPairs, err := prepareGraphDependencies(ctx, u, plan, keyToID)
	if err != nil {
		return GraphApplyResult{}, err
	}
	newSchedulingEdges, err := insertGraphDependencies(ctx, u.depRepo, plan, keyToID, parentDepPairs, actor, useWisp)
	if err != nil {
		return GraphApplyResult{}, err
	}
	if err := checkGraphApplyCycles(ctx, u.depRepo, newSchedulingEdges); err != nil {
		return GraphApplyResult{}, err
	}
	if err := applyDeferredGraphAssignees(ctx, u.issueRepo, plan, keyToID, pendingAssignees, actor, useWisp); err != nil {
		return GraphApplyResult{}, err
	}
	return GraphApplyResult{IDs: keyToID}, nil
}

func createGraphNodes(ctx context.Context, creator *issueCreateModule, plan GraphPlan, actor string, useWisp bool) (map[string]string, map[int]string, error) {
	keyToID := make(map[string]string, len(plan.Nodes))
	pendingAssignees := make(map[int]string, len(plan.Nodes))
	// Create every node as a top-level issue. Parent linkage is inserted after
	// all IDs are known, matching the embedded graph-apply ordering.
	for i, node := range plan.Nodes {
		if node.Issue == nil {
			return nil, nil, fmt.Errorf("applyGraph: node %d (key=%q) has nil Issue", i, node.Key)
		}
		if nodeWisp := node.Issue.Ephemeral || node.Issue.NoHistory; nodeWisp != useWisp {
			return nil, nil, fmt.Errorf("applyGraph: node %q storage class (ephemeral=%t, no_history=%t) does not match plan routing (wisp=%t)", node.Key, node.Issue.Ephemeral, node.Issue.NoHistory, useWisp)
		}
		if node.AssignAfterCreate {
			pendingAssignees[i] = node.Assignee
			node.Issue.Assignee = ""
		} else if node.Assignee != "" {
			node.Issue.Assignee = node.Assignee
		}
		r, err := creator.create(ctx, CreateIssueParams{Issue: node.Issue, Labels: node.Labels}, actor, useWisp)
		if err != nil {
			return nil, nil, fmt.Errorf("applyGraph: node %q: %w", node.Key, err)
		}
		keyToID[node.Key] = r.Issue.ID
	}
	return keyToID, pendingAssignees, nil
}

func applyGraphMetadataRefs(ctx context.Context, repo IssueSQLRepository, plan GraphPlan, keyToID map[string]string, actor string, useWisp bool) error {
	for _, node := range plan.Nodes {
		if len(node.MetadataRefs) == 0 {
			continue
		}
		metaJSON, err := types.MergeMetadataRefs(node.Issue.Metadata, node.MetadataRefs, keyToID)
		if err != nil {
			return fmt.Errorf("applyGraph: node %q: %w", node.Key, err)
		}
		if err := repo.Update(ctx, keyToID[node.Key], map[string]any{"metadata": metaJSON}, actor, IssueTableOpts{UseWispsTable: useWisp}); err != nil {
			return fmt.Errorf("applyGraph: node %q: updating metadata refs: %w", node.Key, err)
		}
	}
	return nil
}

func prepareGraphDependencies(ctx context.Context, graph *issueGraphModule, plan GraphPlan, keyToID map[string]string) (map[string]bool, error) {
	parentDepPairs := graphParentDepPairs(plan.Nodes, keyToID)
	if err := graph.validatePlannedBlockingPaths(ctx, plan, keyToID, parentDepPairs); err != nil {
		return nil, err
	}
	if err := graph.validatePlannedBlockingCycles(ctx, plan, keyToID); err != nil {
		return nil, err
	}
	if err := validateGraphEdgeConflicts(plan, keyToID, parentDepPairs); err != nil {
		return nil, err
	}
	return parentDepPairs, nil
}

func validateGraphEdgeConflicts(plan GraphPlan, keyToID map[string]string, parentDepPairs map[string]bool) error {
	for i, edge := range plan.Edges {
		if err := validateGraphEdgeConflict(i, edge, keyToID, parentDepPairs); err != nil {
			return err
		}
	}
	return nil
}

func validateGraphEdgeConflict(index int, edge GraphEdge, keyToID map[string]string, parentDepPairs map[string]bool) error {
	fromID := resolveEdgeRef(edge.FromKey, edge.FromID, keyToID)
	if fromID == "" {
		return fmt.Errorf("applyGraph: edge %d references undefined from_key %q", index, edge.FromKey)
	}
	toID := resolveEdgeRef(edge.ToKey, edge.ToID, keyToID)
	if toID == "" {
		return fmt.Errorf("applyGraph: edge %d references undefined to_key %q", index, edge.ToKey)
	}
	depType := edge.Type
	if depType == "" {
		depType = types.DepBlocks
	}
	if parentDepPairs[depPairKey(fromID, toID)] && depType != types.DepParentChild {
		return fmt.Errorf("applyGraph: edge %d %s->%s duplicates a parent-child relationship with dependency type %q", index, fromID, toID, depType)
	}
	if parentDepPairs[depPairKey(toID, fromID)] && cycleRelevantDepType(depType) {
		return fmt.Errorf("applyGraph: edge %d %s->%s creates a blocking reverse of a parent-child relationship", index, fromID, toID)
	}
	return nil
}

func insertGraphDependencies(ctx context.Context, repo DependencySQLRepository, plan GraphPlan, keyToID map[string]string, parentDepPairs map[string]bool, actor string, useWisp bool) ([][2]string, error) {
	newSchedulingEdges, err := insertGraphParentDependencies(ctx, repo, plan.Nodes, keyToID, actor, useWisp)
	if err != nil {
		return nil, err
	}
	parentEdges, err := insertGraphExplicitPhase(ctx, repo, plan.Edges, keyToID, parentDepPairs, actor, useWisp, true)
	if err != nil {
		return nil, err
	}
	newSchedulingEdges = append(newSchedulingEdges, parentEdges...)
	parentInlineEdges, err := insertGraphInlinePhase(ctx, repo, plan.Nodes, keyToID, actor, useWisp, true)
	if err != nil {
		return nil, err
	}
	newSchedulingEdges = append(newSchedulingEdges, parentInlineEdges...)
	otherEdges, err := insertGraphExplicitPhase(ctx, repo, plan.Edges, keyToID, parentDepPairs, actor, useWisp, false)
	if err != nil {
		return nil, err
	}
	newSchedulingEdges = append(newSchedulingEdges, otherEdges...)
	otherInlineEdges, err := insertGraphInlinePhase(ctx, repo, plan.Nodes, keyToID, actor, useWisp, false)
	if err != nil {
		return nil, err
	}
	return append(newSchedulingEdges, otherInlineEdges...), nil
}

func insertGraphParentDependencies(ctx context.Context, repo DependencySQLRepository, nodes []GraphNode, keyToID map[string]string, actor string, useWisp bool) ([][2]string, error) {
	newSchedulingEdges := make([][2]string, 0, len(nodes))
	for _, node := range nodes {
		parentID := node.ParentID
		if node.ParentKey != "" {
			parentID = keyToID[node.ParentKey]
		}
		if parentID == "" {
			continue
		}
		childID := keyToID[node.Key]
		dep := &types.Dependency{IssueID: childID, DependsOnID: parentID, Type: types.DepParentChild}
		if err := repo.Insert(ctx, dep, actor, DepInsertOpts{UseWispsTable: useWisp}); err != nil {
			return nil, fmt.Errorf("applyGraph: node %q: parent-child dep %s->%s: %w", node.Key, childID, parentID, err)
		}
		newSchedulingEdges = append(newSchedulingEdges, [2]string{childID, parentID})
	}
	return newSchedulingEdges, nil
}

func insertGraphExplicitPhase(ctx context.Context, repo DependencySQLRepository, edges []GraphEdge, keyToID map[string]string, parentDepPairs map[string]bool, actor string, useWisp, parentPhase bool) ([][2]string, error) {
	newSchedulingEdges := make([][2]string, 0, len(edges))
	for i, edge := range edges {
		dep, depType, fromID, toID, skip, err := buildGraphEdgeForPhase(i, edge, keyToID, parentDepPairs, parentPhase)
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}
		if err := repo.Insert(ctx, dep, actor, DepInsertOpts{UseWispsTable: useWisp}); err != nil {
			return nil, fmt.Errorf("applyGraph: edge %d (%s -> %s): %w", i, fromID, toID, err)
		}
		if types.IsSchedulingEdge(depType) {
			newSchedulingEdges = append(newSchedulingEdges, [2]string{fromID, toID})
		}
	}
	return newSchedulingEdges, nil
}

func buildGraphEdgeForPhase(index int, edge GraphEdge, keyToID map[string]string, parentDepPairs map[string]bool, parentPhase bool) (*types.Dependency, types.DependencyType, string, string, bool, error) {
	fromID, toID, err := resolveGraphEdgeEndpoints(index, edge, keyToID)
	if err != nil {
		return nil, "", "", "", false, err
	}
	depType := graphEdgeType(edge.Type)
	if (depType == types.DepParentChild) != parentPhase {
		return nil, depType, fromID, toID, true, nil
	}
	skip, err := validateGraphEdgeRelationship(index, fromID, toID, depType, parentDepPairs)
	if err != nil || skip {
		return nil, depType, fromID, toID, skip, err
	}
	dep, err := types.NewGraphEdgeDependency(fromID, toID, depType, edge.Gate, edge.SpawnerKey, edge.SpawnerID, edge.ThreadID, keyToID)
	if err != nil {
		return nil, depType, fromID, toID, false, fmt.Errorf("applyGraph: edge %d: %w", index, err)
	}
	return dep, depType, fromID, toID, false, nil
}

func resolveGraphEdgeEndpoints(index int, edge GraphEdge, keyToID map[string]string) (string, string, error) {
	fromID := resolveEdgeRef(edge.FromKey, edge.FromID, keyToID)
	if fromID == "" {
		return "", "", fmt.Errorf("applyGraph: edge %d references undefined from_key %q", index, edge.FromKey)
	}
	toID := resolveEdgeRef(edge.ToKey, edge.ToID, keyToID)
	if toID == "" {
		return "", "", fmt.Errorf("applyGraph: edge %d references undefined to_key %q", index, edge.ToKey)
	}
	return fromID, toID, nil
}

func graphEdgeType(depType types.DependencyType) types.DependencyType {
	if depType == "" {
		return types.DepBlocks
	}
	return depType
}

func validateGraphEdgeRelationship(index int, fromID, toID string, depType types.DependencyType, parentDepPairs map[string]bool) (bool, error) {
	if parentDepPairs[depPairKey(fromID, toID)] {
		if depType == types.DepParentChild {
			return true, nil
		}
		return false, fmt.Errorf("applyGraph: edge %d %s->%s duplicates a parent-child relationship with dependency type %q", index, fromID, toID, depType)
	}
	if parentDepPairs[depPairKey(toID, fromID)] && cycleRelevantDepType(depType) {
		return false, fmt.Errorf("applyGraph: edge %d %s->%s creates a blocking reverse of a parent-child relationship", index, fromID, toID)
	}
	return false, nil
}

func insertGraphInlinePhase(ctx context.Context, repo DependencySQLRepository, nodes []GraphNode, keyToID map[string]string, actor string, useWisp, parentPhase bool) ([][2]string, error) {
	newSchedulingEdges := make([][2]string, 0)
	for _, node := range nodes {
		for _, nodeDep := range node.Deps {
			dep, err := types.NewGraphNodeDependency(keyToID[node.Key], nodeDep.Type, nodeDep.Target, keyToID)
			if err != nil {
				return nil, fmt.Errorf("applyGraph: node %q: %w", node.Key, err)
			}
			if (dep.Type == types.DepParentChild) != parentPhase {
				continue
			}
			if err := repo.Insert(ctx, dep, actor, DepInsertOpts{UseWispsTable: useWisp}); err != nil {
				return nil, fmt.Errorf("applyGraph: node %q: adding dep to %q: %w", node.Key, nodeDep.Target, err)
			}
			if types.IsSchedulingEdge(dep.Type) {
				newSchedulingEdges = append(newSchedulingEdges, [2]string{dep.IssueID, dep.DependsOnID})
			}
		}
	}
	return newSchedulingEdges, nil
}
