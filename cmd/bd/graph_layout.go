package main

import (
	"sort"

	"github.com/jonbaldie/beads/internal/types"
)

type graphEdge struct {
	from string
	to   string
}

func openGraphIssueSet(issues []*types.Issue) map[string]bool {
	openSet := make(map[string]bool)
	for _, issue := range issues {
		if isOpenStatus(issue.Status) {
			openSet[issue.ID] = true
		}
	}
	return openSet
}

func graphBlockingAdjacency(dependencies []*types.Dependency) map[string][]string {
	blocksAdj := make(map[string][]string)
	for _, dep := range dependencies {
		if dep.Type == types.DepBlocks {
			blocksAdj[dep.DependsOnID] = append(blocksAdj[dep.DependsOnID], dep.IssueID)
		}
	}
	return blocksAdj
}

func graphSyntheticBlockingEdges(openSet map[string]bool, blocksAdj map[string][]string) map[graphEdge]bool {
	syntheticEdges := make(map[graphEdge]bool)
	for src := range openSet {
		for edge := range graphReachableOpenEdges(src, openSet, blocksAdj) {
			syntheticEdges[edge] = true
		}
	}
	return syntheticEdges
}

func graphReachableOpenEdges(src string, openSet map[string]bool, blocksAdj map[string][]string) map[graphEdge]bool {
	visited := map[string]bool{src: true}
	queue := []string{src}
	edges := make(map[graphEdge]bool)
	for {
		if len(queue) == 0 {
			break
		}
		cur := queue[0]
		queue = queue[1:]
		for _, next := range blocksAdj[cur] {
			if visited[next] {
				continue
			}
			visited[next] = true
			if openSet[next] {
				edges[graphEdge{from: src, to: next}] = true
			}
			if !openSet[next] {
				queue = append(queue, next)
			}
		}
	}
	return edges
}

func openGraphSubgraph(subgraph *TemplateSubgraph, openSet map[string]bool) *TemplateSubgraph {
	filtered := &TemplateSubgraph{IssueMap: make(map[string]*types.Issue, len(openSet))}
	for _, issue := range subgraph.Issues {
		if openSet[issue.ID] {
			filtered.Issues = append(filtered.Issues, issue)
			filtered.IssueMap[issue.ID] = issue
		}
	}
	return filtered
}

func appendOpenGraphDependencies(filtered *TemplateSubgraph, dependencies []*types.Dependency, openSet map[string]bool, syntheticEdges map[graphEdge]bool) {
	seen := make(map[graphEdge]bool)
	for _, dep := range dependencies {
		if !openSet[dep.IssueID] || !openSet[dep.DependsOnID] {
			continue
		}
		if dep.Type != types.DepBlocks {
			filtered.Dependencies = append(filtered.Dependencies, dep)
			continue
		}
		edge := graphEdge{from: dep.DependsOnID, to: dep.IssueID}
		if seen[edge] {
			continue
		}
		seen[edge] = true
		filtered.Dependencies = append(filtered.Dependencies, dep)
	}
	for edge := range syntheticEdges {
		if seen[edge] {
			continue
		}
		seen[edge] = true
		filtered.Dependencies = append(filtered.Dependencies, &types.Dependency{
			IssueID: edge.to, DependsOnID: edge.from, Type: types.DepBlocks,
		})
	}
}

func selectOpenGraphRoot(original, filtered *TemplateSubgraph, openSet map[string]bool) *types.Issue {
	if original.Root != nil && openSet[original.Root.ID] {
		return original.Root
	}
	var root *types.Issue
	for _, issue := range filtered.Issues {
		if root == nil || issue.Priority < root.Priority {
			root = issue
		}
	}
	return root
}

// computeLayout assigns layers to nodes using topological sort
// mergeSubgraphsForHTML joins disconnected components into one subgraph so
// `bd graph --all --html` emits a single valid HTML document.
func mergeSubgraphsForHTML(subgraphs []*TemplateSubgraph) *TemplateSubgraph {
	switch len(subgraphs) {
	case 0:
		return &TemplateSubgraph{IssueMap: make(map[string]*types.Issue)}
	case 1:
		return subgraphs[0]
	}
	merged := &TemplateSubgraph{
		IssueMap: make(map[string]*types.Issue),
	}
	for _, sg := range subgraphs {
		for _, issue := range sg.Issues {
			merged.IssueMap[issue.ID] = issue
		}
		merged.Dependencies = append(merged.Dependencies, sg.Dependencies...)
	}
	merged.Issues = make([]*types.Issue, 0, len(merged.IssueMap))
	for _, issue := range merged.IssueMap {
		merged.Issues = append(merged.Issues, issue)
	}
	sort.Slice(merged.Issues, func(i, j int) bool {
		return merged.Issues[i].ID < merged.Issues[j].ID
	})
	return merged
}

func computeLayout(subgraph *TemplateSubgraph) *GraphLayout {
	layout := &GraphLayout{
		Nodes: make(map[string]*GraphNode),
	}
	if subgraph.Root != nil {
		layout.RootID = subgraph.Root.ID
	}
	dependsOn := graphLayoutDependencies(subgraph.Dependencies)
	initializeGraphNodes(layout, subgraph.Issues, dependsOn)
	assignGraphLayers(layout, dependsOn)
	liftGraphChildren(layout, subgraph.Dependencies)
	finishGraphLayout(layout)
	return layout
}

func graphLayoutDependencies(dependencies []*types.Dependency) map[string][]string {
	dependsOn := make(map[string][]string)
	for _, dep := range dependencies {
		if dep.Type == types.DepBlocks {
			dependsOn[dep.IssueID] = append(dependsOn[dep.IssueID], dep.DependsOnID)
		}
	}
	return dependsOn
}

func initializeGraphNodes(layout *GraphLayout, issues []*types.Issue, dependsOn map[string][]string) {
	for _, issue := range issues {
		layout.Nodes[issue.ID] = &GraphNode{
			Issue:     issue,
			Layer:     -1,
			DependsOn: dependsOn[issue.ID],
		}
	}
}

func assignGraphLayers(layout *GraphLayout, dependsOn map[string][]string) {
	// Assign layers using the longest path from sources. Layer 0 contains
	// nodes with no blocking dependencies.
	changed := true
	for changed {
		changed = false
		for id, node := range layout.Nodes {
			if node.Layer >= 0 {
				continue
			}
			deps := dependsOn[id]
			if len(deps) == 0 {
				node.Layer = 0
				changed = true
				continue
			}
			maxDepLayer, allAssigned := graphDependencyLayer(layout, deps)
			if allAssigned {
				node.Layer = maxDepLayer + 1
				changed = true
			}
		}
	}
	for _, node := range layout.Nodes {
		if node.Layer < 0 {
			node.Layer = 0
		}
	}
}

func graphDependencyLayer(layout *GraphLayout, dependencies []string) (int, bool) {
	maxDepLayer := -1
	for _, depID := range dependencies {
		depNode := layout.Nodes[depID]
		if depNode == nil || depNode.Layer < 0 {
			return -1, false
		}
		if depNode.Layer > maxDepLayer {
			maxDepLayer = depNode.Layer
		}
	}
	return maxDepLayer, true
}

func liftGraphChildren(layout *GraphLayout, dependencies []*types.Dependency) {
	parentOf := make(map[string]string)
	for _, dep := range dependencies {
		if dep.Type == types.DepParentChild {
			parentOf[dep.IssueID] = dep.DependsOnID
		}
	}
	if len(parentOf) == 0 {
		return
	}
	// Iterate until stable so nested parent-child hierarchies inherit the
	// layer of their highest-layer ancestor.
	changed := true
	for changed {
		changed = false
		for childID, parentID := range parentOf {
			childNode := layout.Nodes[childID]
			parentNode := layout.Nodes[parentID]
			if childNode != nil && parentNode != nil && childNode.Layer < parentNode.Layer {
				childNode.Layer = parentNode.Layer
				changed = true
			}
		}
	}
}

func finishGraphLayout(layout *GraphLayout) {
	for _, node := range layout.Nodes {
		if node.Layer > layout.MaxLayer {
			layout.MaxLayer = node.Layer
		}
	}
	layout.Layers = make([][]string, layout.MaxLayer+1)
	for id, node := range layout.Nodes {
		layout.Layers[node.Layer] = append(layout.Layers[node.Layer], id)
	}
	for i := range layout.Layers {
		sort.Strings(layout.Layers[i])
	}
	for _, layer := range layout.Layers {
		for pos, id := range layer {
			layout.Nodes[id].Position = pos
		}
	}
}
