package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

func loadGraphSubgraph(ctx context.Context, s storage.DoltStorage, issueID string) (*TemplateSubgraph, error) {
	if s == nil {
		return nil, fmt.Errorf("no database connection")
	}
	root, err := s.GetIssue(ctx, issueID)
	if err != nil {
		return nil, fmt.Errorf("failed to get issue: %w", err)
	}
	if root == nil {
		return nil, fmt.Errorf("issue %s not found", issueID)
	}

	subgraph := &TemplateSubgraph{
		Root:     root,
		Issues:   []*types.Issue{root},
		IssueMap: map[string]*types.Issue{root.ID: root},
	}
	walkGraphConnections(ctx, s, subgraph)
	loadGraphDependencyRecords(ctx, s, subgraph)
	return subgraph, nil
}

func walkGraphConnections(ctx context.Context, s storage.DoltStorage, subgraph *TemplateSubgraph) {
	queue := []string{subgraph.Root.ID}
	visited := map[string]bool{subgraph.Root.ID: true}
	for {
		if len(queue) == 0 {
			break
		}
		currentID := queue[0]
		queue = queue[1:]
		dependents, err := s.GetDependents(ctx, currentID)
		if err != nil {
			continue
		}
		queue = appendGraphIssues(subgraph, visited, queue, dependents)
		dependencies, err := s.GetDependencies(ctx, currentID)
		if err != nil {
			continue
		}
		queue = appendGraphIssues(subgraph, visited, queue, dependencies)
	}
}

func appendGraphIssues(subgraph *TemplateSubgraph, visited map[string]bool, queue []string, issues []*types.Issue) []string {
	for _, issue := range issues {
		if visited[issue.ID] {
			continue
		}
		visited[issue.ID] = true
		subgraph.Issues = append(subgraph.Issues, issue)
		subgraph.IssueMap[issue.ID] = issue
		queue = append(queue, issue.ID)
	}
	return queue
}

func loadGraphDependencyRecords(ctx context.Context, s storage.DoltStorage, subgraph *TemplateSubgraph) {
	for _, issue := range subgraph.Issues {
		deps, err := s.GetDependencyRecords(ctx, issue.ID)
		if err != nil {
			continue
		}
		for _, dep := range deps {
			if !resolveGraphExternalDependency(ctx, dep, subgraph) {
				continue
			}
			if _, ok := subgraph.IssueMap[dep.DependsOnID]; ok {
				subgraph.Dependencies = append(subgraph.Dependencies, dep)
			}
		}
	}
}

func resolveGraphExternalDependency(ctx context.Context, dep *types.Dependency, subgraph *TemplateSubgraph) bool {
	if !strings.HasPrefix(dep.DependsOnID, "external:") {
		return true
	}
	parts := strings.SplitN(dep.DependsOnID, ":", 3)
	if len(parts) != 3 || parts[2] == "" {
		return true
	}
	targetID := parts[2]
	if _, exists := subgraph.IssueMap[targetID]; exists {
		dep.DependsOnID = targetID
		return true
	}
	result, routeErr := resolveAndGetIssueWithRouting(ctx, getStore(), targetID)
	if routeErr != nil || result == nil || result.Issue == nil {
		if result != nil {
			result.Close()
		}
		return false
	}
	subgraph.Issues = append(subgraph.Issues, result.Issue)
	subgraph.IssueMap[result.Issue.ID] = result.Issue
	dep.DependsOnID = result.Issue.ID
	result.Close()
	return true
}

// loadAllGraphSubgraphs loads all open issues and groups them by connected component
// Each component is a subgraph of issues that share dependencies. The defensive
// row cap (be-x42v) is propagated to each per-status SearchIssues call so any
// single status that exceeds the cap returns the typed error directly.
func loadAllGraphSubgraphs(ctx context.Context, s storage.DoltStorage, maxRows int, maxRowsSource string) ([]*TemplateSubgraph, error) {
	if s == nil {
		return nil, fmt.Errorf("no database connection")
	}
	allIssues, err := searchGraphIssues(ctx, s, maxRows, maxRowsSource)
	if err != nil {
		return nil, err
	}
	if len(allIssues) == 0 {
		return nil, nil
	}
	issueMap := graphIssueMap(allIssues)
	allDeps := loadAllGraphDependencies(ctx, s, allIssues, issueMap)
	return assembleAllSubgraphs(allIssues, issueMap, allDeps), nil
}

func searchGraphIssues(ctx context.Context, s storage.DoltStorage, maxRows int, maxRowsSource string) ([]*types.Issue, error) {
	// Get all open issues (open, in_progress, blocked). IssueFilter takes a
	// single status, so one query is needed for each status.
	var allIssues []*types.Issue
	for _, status := range []types.Status{types.StatusOpen, types.StatusInProgress, types.StatusBlocked} {
		statusCopy := status
		issues, err := s.SearchIssues(ctx, "", types.IssueFilter{
			IssueFilterCore: types.IssueFilterCore{
				Status: &statusCopy,
			},
			IssueFilterPage: types.IssueFilterPage{
				MaxRows:       maxRows,
				MaxRowsSource: maxRowsSource,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("failed to search issues: %w", err)
		}
		allIssues = append(allIssues, issues...)
	}
	return allIssues, nil
}

func graphIssueMap(issues []*types.Issue) map[string]*types.Issue {
	issueMap := make(map[string]*types.Issue, len(issues))
	for _, issue := range issues {
		issueMap[issue.ID] = issue
	}
	return issueMap
}

func loadAllGraphDependencies(ctx context.Context, s storage.DoltStorage, allIssues []*types.Issue, issueMap map[string]*types.Issue) []*types.Dependency {
	var allDeps []*types.Dependency
	for _, issue := range allIssues {
		deps, err := s.GetDependencyRecords(ctx, issue.ID)
		if err != nil {
			continue
		}
		for _, dep := range deps {
			if !resolveGraphExternalDependencyForMap(ctx, dep, issueMap, &allIssues) {
				continue
			}
			if _, ok := issueMap[dep.DependsOnID]; ok {
				allDeps = append(allDeps, dep)
			}
		}
	}
	return allDeps
}

func resolveGraphExternalDependencyForMap(ctx context.Context, dep *types.Dependency, issueMap map[string]*types.Issue, allIssues *[]*types.Issue) bool {
	if !strings.HasPrefix(dep.DependsOnID, "external:") {
		return true
	}
	parts := strings.SplitN(dep.DependsOnID, ":", 3)
	if len(parts) != 3 || parts[2] == "" {
		return true
	}
	targetID := parts[2]
	if _, exists := issueMap[targetID]; exists {
		dep.DependsOnID = targetID
		return true
	}
	result, routeErr := resolveAndGetIssueWithRouting(ctx, getStore(), targetID)
	if routeErr != nil || result == nil || result.Issue == nil {
		if result != nil {
			result.Close()
		}
		return false
	}
	*allIssues = append(*allIssues, result.Issue)
	issueMap[result.Issue.ID] = result.Issue
	dep.DependsOnID = result.Issue.ID
	result.Close()
	return true
}

func assembleAllSubgraphs(allIssues []*types.Issue, issueMap map[string]*types.Issue, allDeps []*types.Dependency) []*TemplateSubgraph {
	adj := graphAdjacency(allDeps)
	components := graphComponents(allIssues, adj)
	sortGraphComponents(components, issueMap)
	return buildGraphSubgraphs(components, issueMap, allDeps)
}

func graphAdjacency(allDeps []*types.Dependency) map[string][]string {
	adj := make(map[string][]string)
	for _, dep := range allDeps {
		adj[dep.IssueID] = append(adj[dep.IssueID], dep.DependsOnID)
		adj[dep.DependsOnID] = append(adj[dep.DependsOnID], dep.IssueID)
	}
	return adj
}

func graphComponents(allIssues []*types.Issue, adj map[string][]string) [][]string {
	visited := make(map[string]bool)
	var components [][]string
	for _, issue := range allIssues {
		if visited[issue.ID] {
			continue
		}
		component := []string{issue.ID}
		queue := []string{issue.ID}
		visited[issue.ID] = true
		for {
			if len(queue) == 0 {
				break
			}
			current := queue[0]
			queue = queue[1:]
			for _, neighbor := range adj[current] {
				if visited[neighbor] {
					continue
				}
				visited[neighbor] = true
				queue = append(queue, neighbor)
				component = append(component, neighbor)
			}
		}
		components = append(components, component)
	}
	return components
}

func sortGraphComponents(components [][]string, issueMap map[string]*types.Issue) {
	sort.Slice(components, func(i, j int) bool {
		if len(components[i]) != len(components[j]) {
			return len(components[i]) > len(components[j])
		}
		issueI := issueMap[components[i][0]]
		issueJ := issueMap[components[j][0]]
		return issueI.Priority < issueJ.Priority
	})
}

func buildGraphSubgraphs(components [][]string, issueMap map[string]*types.Issue, allDeps []*types.Dependency) []*TemplateSubgraph {
	var subgraphs []*TemplateSubgraph
	for _, component := range components {
		if len(component) == 0 {
			continue
		}
		root := selectGraphRoot(component, issueMap)
		subgraph := &TemplateSubgraph{Root: root, IssueMap: make(map[string]*types.Issue)}
		for _, id := range component {
			issue := issueMap[id]
			subgraph.Issues = append(subgraph.Issues, issue)
			subgraph.IssueMap[id] = issue
		}
		appendGraphComponentDependencies(subgraph, allDeps)
		subgraphs = append(subgraphs, subgraph)
	}
	return subgraphs
}

func selectGraphRoot(component []string, issueMap map[string]*types.Issue) *types.Issue {
	var root *types.Issue
	for _, id := range component {
		issue := issueMap[id]
		if root == nil {
			root = issue
			continue
		}
		if issue.IssueType == types.TypeEpic && root.IssueType != types.TypeEpic {
			root = issue
			continue
		}
		if root.IssueType == types.TypeEpic && issue.IssueType != types.TypeEpic {
			continue
		}
		if issue.Priority < root.Priority {
			root = issue
		}
	}
	return root
}

func appendGraphComponentDependencies(subgraph *TemplateSubgraph, allDeps []*types.Dependency) {
	for _, dep := range allDeps {
		if _, inComponent := subgraph.IssueMap[dep.IssueID]; !inComponent {
			continue
		}
		if _, depInComponent := subgraph.IssueMap[dep.DependsOnID]; depInComponent {
			subgraph.Dependencies = append(subgraph.Dependencies, dep)
		}
	}
}

// isOpenStatus returns true for statuses considered "open" / actionable.
// Closed and deferred (frozen) issues are excluded by --open.
func isOpenStatus(s types.Status) bool {
	switch s {
	case types.StatusOpen, types.StatusInProgress, types.StatusBlocked:
		return true
	default:
		return false
	}
}

// filterSubgraphOpen removes closed/deferred issues from a subgraph and
// computes a transitive closure of blocking edges through removed nodes so
// that indirect blocking relationships are preserved.
//
// Example: if A(open) blocks B(closed) blocks C(open), the filtered graph
// contains A and C with a synthetic edge A→C.
func filterSubgraphOpen(subgraph *TemplateSubgraph) *TemplateSubgraph {
	if subgraph == nil {
		return nil
	}
	openSet := openGraphIssueSet(subgraph.Issues)
	if len(openSet) == 0 {
		return nil
	}
	blocksAdj := graphBlockingAdjacency(subgraph.Dependencies)
	syntheticEdges := graphSyntheticBlockingEdges(openSet, blocksAdj)
	filtered := openGraphSubgraph(subgraph, openSet)
	appendOpenGraphDependencies(filtered, subgraph.Dependencies, openSet, syntheticEdges)
	filtered.Root = selectOpenGraphRoot(subgraph, filtered, openSet)
	return filtered
}
