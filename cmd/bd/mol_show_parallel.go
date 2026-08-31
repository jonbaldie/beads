package main

import (
	"fmt"
	"sort"

	"github.com/jonbaldie/beads/internal/types"
)

func newParallelAnalysis(subgraph *MoleculeSubgraph) *ParallelAnalysis {
	return &ParallelAnalysis{
		MoleculeID:     subgraph.Root.ID,
		TotalSteps:     len(subgraph.Issues),
		ParallelGroups: make(map[string][]string),
		Steps:          make(map[string]*ParallelInfo),
	}
}

func buildParallelDepMaps(subgraph *MoleculeSubgraph) (blockedBy, blocks map[string]map[string]bool, parentChildren map[string][]string) {
	blockedBy = make(map[string]map[string]bool)
	blocks = make(map[string]map[string]bool)
	parentChildren = make(map[string][]string)
	for _, issue := range subgraph.Issues {
		blockedBy[issue.ID] = make(map[string]bool)
		blocks[issue.ID] = make(map[string]bool)
	}
	for _, dep := range subgraph.Dependencies {
		if dep.Type == types.DepParentChild {
			parentChildren[dep.DependsOnID] = append(parentChildren[dep.DependsOnID], dep.IssueID)
		}
	}
	return blockedBy, blocks, parentChildren
}

func applyParallelDependencies(subgraph *MoleculeSubgraph, blockedBy, blocks map[string]map[string]bool, parentChildren map[string][]string) {
	for _, dep := range subgraph.Dependencies {
		switch dep.Type {
		case types.DepBlocks, types.DepConditionalBlocks:
			applyBlocksDependency(dep, blockedBy, blocks)
		case types.DepWaitsFor:
			applyWaitsForDependency(dep, subgraph, parentChildren, blockedBy, blocks)
		}
	}
}

func applyBlocksDependency(dep *types.Dependency, blockedBy, blocks map[string]map[string]bool) {
	if _, ok := blockedBy[dep.IssueID]; ok {
		blockedBy[dep.IssueID][dep.DependsOnID] = true
	}
	if _, ok := blocks[dep.DependsOnID]; ok {
		blocks[dep.DependsOnID][dep.IssueID] = true
	}
}

func applyWaitsForDependency(dep *types.Dependency, subgraph *MoleculeSubgraph, parentChildren map[string][]string, blockedBy, blocks map[string]map[string]bool) {
	children := parentChildren[dep.DependsOnID]
	if len(children) == 0 {
		return
	}
	if types.ParseWaitsForGateMetadata(dep.Metadata) == types.WaitsForAnyChildren && waitsForAnyChildClosed(subgraph, children) {
		return
	}
	for _, childID := range children {
		child := subgraph.IssueMap[childID]
		if child == nil || child.Status == types.StatusClosed {
			continue
		}
		applyBlocksEdge(dep.IssueID, childID, blockedBy, blocks)
	}
}

func waitsForAnyChildClosed(subgraph *MoleculeSubgraph, children []string) bool {
	for _, childID := range children {
		child := subgraph.IssueMap[childID]
		if child != nil && child.Status == types.StatusClosed {
			return true
		}
	}
	return false
}

func applyBlocksEdge(issueID, blockerID string, blockedBy, blocks map[string]map[string]bool) {
	if _, ok := blockedBy[issueID]; ok {
		blockedBy[issueID][blockerID] = true
	}
	if _, ok := blocks[blockerID]; ok {
		blocks[blockerID][issueID] = true
	}
}

func fillParallelStepInfo(subgraph *MoleculeSubgraph, analysis *ParallelAnalysis, blockedBy, blocks map[string]map[string]bool) {
	for _, issue := range subgraph.Issues {
		info := &ParallelInfo{
			StepID:    issue.ID,
			Status:    string(issue.Status),
			BlockedBy: openBlockers(subgraph, blockedBy[issue.ID]),
			Blocks:    mapKeys(blocks[issue.ID]),
		}
		info.IsReady = (issue.Status == types.StatusOpen || issue.Status == types.StatusInProgress) &&
			len(info.BlockedBy) == 0
		if info.IsReady {
			analysis.ReadySteps++
		}
		sort.Strings(info.BlockedBy)
		sort.Strings(info.Blocks)
		analysis.Steps[issue.ID] = info
	}
}

func openBlockers(subgraph *MoleculeSubgraph, blockers map[string]bool) []string {
	out := []string{}
	for blockerID := range blockers {
		blocker := subgraph.IssueMap[blockerID]
		if blocker != nil && blocker.Status != types.StatusClosed {
			out = append(out, blockerID)
		}
	}
	return out
}

func mapKeys(m map[string]bool) []string {
	out := []string{}
	for id := range m {
		out = append(out, id)
	}
	return out
}

func groupParallelSteps(subgraph *MoleculeSubgraph, analysis *ParallelAnalysis, blockedBy, blocks map[string]map[string]bool) {
	depths := calculateBlockingDepths(subgraph, blockedBy)
	depthGroups := make(map[int][]string)
	for id, depth := range depths {
		depthGroups[depth] = append(depthGroups[depth], id)
	}
	groupCounter := 0
	issueCount := len(subgraph.Issues)
	for depth := 0; depth <= issueCount; depth++ {
		stepsAtDepth := depthGroups[depth]
		if len(stepsAtDepth) == 0 {
			continue
		}
		groups := unionParallelSteps(stepsAtDepth, blockedBy, blocks)
		groupCounter = assignParallelGroups(analysis, groups, groupCounter)
	}
}

func unionParallelSteps(stepsAtDepth []string, blockedBy, blocks map[string]map[string]bool) map[string][]string {
	parent := make(map[string]string, len(stepsAtDepth))
	for _, id := range stepsAtDepth {
		parent[id] = id
	}
	n := len(stepsAtDepth)
	for i, id1 := range stepsAtDepth {
		for j := i + 1; j < n; j++ {
			id2 := stepsAtDepth[j]
			if canParallelize(id1, id2, blocks, blockedBy) {
				unionFindMerge(parent, id1, id2)
			}
		}
	}
	groups := make(map[string][]string)
	for _, id := range stepsAtDepth {
		root := unionFindParent(parent, id)
		groups[root] = append(groups[root], id)
	}
	return groups
}

func canParallelize(id1, id2 string, blocks, blockedBy map[string]map[string]bool) bool {
	return !blocks[id1][id2] && !blocks[id2][id1] &&
		!blockedBy[id1][id2] && !blockedBy[id2][id1]
}

func unionFindParent(parent map[string]string, x string) string {
	for parent[x] != x {
		parent[x] = parent[parent[x]]
		x = parent[x]
	}
	return x
}

func unionFindMerge(parent map[string]string, x, y string) {
	px, py := unionFindParent(parent, x), unionFindParent(parent, y)
	if px != py {
		parent[px] = py
	}
}

func assignParallelGroups(analysis *ParallelAnalysis, groups map[string][]string, groupCounter int) int {
	for _, members := range groups {
		if len(members) <= 1 {
			continue
		}
		groupCounter++
		groupName := fmt.Sprintf("group-%d", groupCounter)
		analysis.ParallelGroups[groupName] = members
		for _, id := range members {
			info := analysis.Steps[id]
			info.ParallelGroup = groupName
			info.CanParallel = otherGroupMembers(members, id)
			sort.Strings(info.CanParallel)
		}
	}
	return groupCounter
}

func otherGroupMembers(members []string, id string) []string {
	out := make([]string, 0, len(members)-1)
	for _, otherID := range members {
		if otherID != id {
			out = append(out, otherID)
		}
	}
	return out
}
