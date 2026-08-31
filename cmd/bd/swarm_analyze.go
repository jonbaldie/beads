// Package main implements the bd CLI swarm management commands.
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
)

func analyzeEpicForSwarm(ctx context.Context, s SwarmStorage, epic *types.Issue) (*SwarmAnalysis, error) {
	analysis := newSwarmAnalysis(epic)

	// Get all child issues of the epic
	childIssues, err := getEpicChildren(ctx, s, epic.ID)
	if err != nil {
		return nil, err
	}

	if len(childIssues) == 0 {
		analysis.Warnings = append(analysis.Warnings, "Epic has no children")
		return analysis, nil
	}

	analysis.TotalIssues = len(childIssues)
	if err := buildSwarmIssueGraph(ctx, s, epic, childIssues, analysis); err != nil {
		return nil, err
	}

	// Detect structural issues
	detectStructuralIssues(analysis, childIssues)

	// Compute ready fronts (waves of parallel work)
	computeReadyFronts(analysis)

	// Set swarmable based on errors
	analysis.Swarmable = len(analysis.Errors) == 0

	return analysis, nil
}

func newSwarmAnalysis(epic *types.Issue) *SwarmAnalysis {
	return &SwarmAnalysis{
		EpicID:    epic.ID,
		EpicTitle: epic.Title,
		Swarmable: true,
		Issues:    make(map[string]*IssueNode),
	}
}

func buildSwarmIssueGraph(ctx context.Context, s SwarmStorage, epic *types.Issue,
	childIssues []*types.Issue, analysis *SwarmAnalysis) error {
	childIDSet := make(map[string]bool, len(childIssues))
	for _, issue := range childIssues {
		childIDSet[issue.ID] = true
		analysis.Issues[issue.ID] = newSwarmIssueNode(issue)
		if issue.Status == types.StatusClosed {
			analysis.ClosedIssues++
		}
	}

	for _, issue := range childIssues {
		deps, err := s.GetDependencyRecords(ctx, issue.ID)
		if err != nil {
			return fmt.Errorf("failed to get dependencies for %s: %w", issue.ID, err)
		}
		for _, dep := range deps {
			appendSwarmDependency(analysis, epic.ID, issue.ID, childIDSet, dep)
		}
	}
	return nil
}

func newSwarmIssueNode(issue *types.Issue) *IssueNode {
	return &IssueNode{
		ID:           issue.ID,
		Title:        issue.Title,
		Status:       string(issue.Status),
		Priority:     issue.Priority,
		DependsOn:    []string{},
		DependedOnBy: []string{},
		Wave:         -1,
	}
}

func appendSwarmDependency(analysis *SwarmAnalysis, epicID, issueID string,
	childIDSet map[string]bool, dep *types.Dependency) {
	if dep.DependsOnID == epicID && dep.Type == types.DepParentChild {
		return
	}
	if !dep.Type.AffectsReadyWork() {
		return
	}
	if childIDSet[dep.DependsOnID] {
		node := analysis.Issues[issueID]
		node.DependsOn = append(node.DependsOn, dep.DependsOnID)
		if targetNode, ok := analysis.Issues[dep.DependsOnID]; ok {
			targetNode.DependedOnBy = append(targetNode.DependedOnBy, issueID)
		}
		return
	}
	if dep.DependsOnID != epicID {
		analysis.Warnings = append(analysis.Warnings, swarmExternalDependencyWarning(issueID, dep.DependsOnID))
	}
}

func swarmExternalDependencyWarning(issueID, dependencyID string) string {
	if strings.HasPrefix(dependencyID, "external:") {
		return fmt.Sprintf("%s has external dependency: %s", issueID, dependencyID)
	}
	return fmt.Sprintf("%s depends on %s (outside epic)", issueID, dependencyID)
}

// issueIsClosed reports whether id is closed. Closed issues count as satisfied
// and are excluded from cycle detection and ready-front scheduling (GH#4564).
func issueIsClosed(analysis *SwarmAnalysis, id string) bool {
	n, ok := analysis.Issues[id]
	return ok && n.Status == string(types.StatusClosed)
}

// detectStructuralIssues looks for common problems in the dependency graph.
//
//nolint:unparam // issues reserved for future use
func detectStructuralIssues(analysis *SwarmAnalysis, _ []*types.Issue) {
	roots := swarmRoots(analysis)
	appendSwarmInversionWarnings(analysis)
	if disconnected := disconnectedSwarmIssues(analysis, roots); len(disconnected) > 0 {
		analysis.Warnings = append(analysis.Warnings,
			fmt.Sprintf("Disconnected issues (not reachable from roots): %v", disconnected))
	}
	if cyclePath, ok := findSwarmCycle(analysis); ok {
		analysis.Errors = append(analysis.Errors,
			fmt.Sprintf("Dependency cycle detected involving: %v", cyclePath))
	}
}

func swarmRoots(analysis *SwarmAnalysis) []string {
	var roots []string
	for id, node := range analysis.Issues {
		if len(node.DependsOn) == 0 {
			roots = append(roots, id)
		}
	}
	return roots
}

func appendSwarmInversionWarnings(analysis *SwarmAnalysis) {
	for id, node := range analysis.Issues {
		lowerTitle := strings.ToLower(node.Title)
		if len(node.DependedOnBy) == 0 && hasSwarmFoundationWord(lowerTitle) {
			analysis.Warnings = append(analysis.Warnings,
				fmt.Sprintf("%s (%s) has no dependents - should other issues depend on it?", id, node.Title))
		}
		if len(node.DependsOn) == 0 && hasSwarmIntegrationWord(lowerTitle) {
			analysis.Warnings = append(analysis.Warnings,
				fmt.Sprintf("%s (%s) has no dependencies - should it depend on implementation?", id, node.Title))
		}
	}
}

func hasSwarmFoundationWord(title string) bool {
	return strings.Contains(title, "foundation") || strings.Contains(title, "setup") ||
		strings.Contains(title, "base") || strings.Contains(title, "core")
}

func hasSwarmIntegrationWord(title string) bool {
	return strings.Contains(title, "integration") || strings.Contains(title, "final") ||
		strings.Contains(title, "test")
}

func disconnectedSwarmIssues(analysis *SwarmAnalysis, roots []string) []string {
	visited := make(map[string]bool)
	var visit func(string)
	visit = func(id string) {
		if visited[id] {
			return
		}
		visited[id] = true
		if node, ok := analysis.Issues[id]; ok {
			for _, dependentID := range node.DependedOnBy {
				visit(dependentID)
			}
		}
	}
	for _, root := range roots {
		visit(root)
	}
	var disconnected []string
	for id := range analysis.Issues {
		if !visited[id] {
			disconnected = append(disconnected, id)
		}
	}
	return disconnected
}

func findSwarmCycle(analysis *SwarmAnalysis) ([]string, bool) {
	inProgress := make(map[string]bool)
	completed := make(map[string]bool)
	var cyclePath []string
	for id := range analysis.Issues {
		if issueIsClosed(analysis, id) || completed[id] {
			continue
		}
		if walkSwarmCycle(analysis, id, inProgress, completed, &cyclePath) {
			return cyclePath, true
		}
	}
	return nil, false
}

func walkSwarmCycle(analysis *SwarmAnalysis, id string, inProgress, completed map[string]bool,
	cyclePath *[]string) bool {
	if issueIsClosed(analysis, id) || completed[id] {
		return false
	}
	if inProgress[id] {
		return true
	}
	inProgress[id] = true
	*cyclePath = append(*cyclePath, id)
	if node, ok := analysis.Issues[id]; ok {
		for _, depID := range node.DependsOn {
			if walkSwarmCycle(analysis, depID, inProgress, completed, cyclePath) {
				return true
			}
		}
	}
	*cyclePath = (*cyclePath)[:len(*cyclePath)-1]
	inProgress[id] = false
	completed[id] = true
	return false
}

// computeReadyFronts calculates the waves of parallel work.
// Closed issues are excluded from waves and do not block dependents (GH#4564):
// a closed dependency is treated as already satisfied for ready-front purposes.
func computeReadyFronts(analysis *SwarmAnalysis) {
	if len(analysis.Errors) > 0 {
		// Can't compute ready fronts if there are cycles
		return
	}
	inDegree := swarmOpenInDegrees(analysis)
	currentWave := initialSwarmReadyWave(analysis, inDegree)
	for wave := 0; hasSwarmWave(currentWave); wave++ {
		currentWave = processSwarmWave(analysis, inDegree, currentWave, wave)
	}
	analysis.EstimatedSessions = countOpenSwarmIssues(analysis)
}

func hasSwarmWave(issueIDs []string) bool {
	return len(issueIDs) > 0
}

func swarmOpenInDegrees(analysis *SwarmAnalysis) map[string]int {
	inDegree := make(map[string]int)
	for id, node := range analysis.Issues {
		if issueIsClosed(analysis, id) {
			continue
		}
		openBlockers := 0
		for _, depID := range node.DependsOn {
			if !issueIsClosed(analysis, depID) {
				openBlockers++
			}
		}
		inDegree[id] = openBlockers
	}
	return inDegree
}

func initialSwarmReadyWave(analysis *SwarmAnalysis, inDegree map[string]int) []string {
	var currentWave []string
	for id, degree := range inDegree {
		if degree == 0 {
			currentWave = append(currentWave, id)
			analysis.Issues[id].Wave = 0
		}
	}
	return currentWave
}

func processSwarmWave(analysis *SwarmAnalysis, inDegree map[string]int, currentWave []string, wave int) []string {
	sort.Strings(currentWave)
	analysis.ReadyFronts = append(analysis.ReadyFronts, ReadyFront{
		Wave:   wave,
		Issues: currentWave,
		Titles: swarmWaveTitles(analysis, currentWave),
	})
	if len(currentWave) > analysis.MaxParallelism {
		analysis.MaxParallelism = len(currentWave)
	}
	return nextSwarmWave(analysis, inDegree, currentWave, wave)
}

func swarmWaveTitles(analysis *SwarmAnalysis, issueIDs []string) []string {
	var titles []string
	for _, id := range issueIDs {
		if node, ok := analysis.Issues[id]; ok {
			titles = append(titles, node.Title)
		}
	}
	return titles
}

func nextSwarmWave(analysis *SwarmAnalysis, inDegree map[string]int, currentWave []string, wave int) []string {
	var nextWave []string
	for _, id := range currentWave {
		node, ok := analysis.Issues[id]
		if !ok {
			continue
		}
		for _, dependentID := range node.DependedOnBy {
			if issueIsClosed(analysis, dependentID) {
				continue
			}
			if _, tracked := inDegree[dependentID]; !tracked {
				continue
			}
			inDegree[dependentID]--
			if inDegree[dependentID] == 0 {
				nextWave = append(nextWave, dependentID)
				analysis.Issues[dependentID].Wave = wave + 1
			}
		}
	}
	return nextWave
}

func countOpenSwarmIssues(analysis *SwarmAnalysis) int {
	openCount := 0
	for id := range analysis.Issues {
		if !issueIsClosed(analysis, id) {
			openCount++
		}
	}
	return openCount
}

// renderSwarmAnalysis outputs human-readable analysis.
func renderSwarmAnalysis(analysis *SwarmAnalysis) {
	fmt.Printf("\n%s Swarm Analysis: %s\n", ui.RenderAccent("●"), analysis.EpicTitle)
	fmt.Printf("   Epic ID: %s\n", analysis.EpicID)
	fmt.Printf("   Total issues: %d (%d closed)\n", analysis.TotalIssues, analysis.ClosedIssues)

	if analysis.TotalIssues == 0 {
		fmt.Printf("\n%s Epic has no children to swarm\n\n", ui.RenderWarn("⚠"))
		return
	}
	renderSwarmReadyFronts(analysis)
	renderSwarmAnalysisSummary(analysis)
	renderSwarmAnalysisWarnings(analysis)
	renderSwarmAnalysisErrors(analysis)
	renderSwarmAnalysisVerdict(analysis)
}

func renderSwarmReadyFronts(analysis *SwarmAnalysis) {
	if len(analysis.ReadyFronts) > 0 {
		fmt.Printf("\n%s Ready Fronts (waves of parallel work):\n", ui.RenderPass("▦"))
		for _, front := range analysis.ReadyFronts {
			fmt.Printf("   Wave %d: %d issues\n", front.Wave+1, len(front.Issues))
			for i, id := range front.Issues {
				renderSwarmReadyIssue(front, i, id)
			}
		}
	}
}

func renderSwarmReadyIssue(front ReadyFront, index int, id string) {
	title := ""
	if index < len(front.Titles) {
		title = front.Titles[index]
	}
	fmt.Printf("      • %s: %s\n", ui.RenderID(id), title)
}

func renderSwarmAnalysisSummary(analysis *SwarmAnalysis) {
	fmt.Printf("\n%s Summary:\n", ui.RenderAccent("↗"))
	fmt.Printf("   Estimated worker-sessions: %d\n", analysis.EstimatedSessions)
	fmt.Printf("   Max parallelism: %d\n", analysis.MaxParallelism)
	fmt.Printf("   Total waves: %d\n", len(analysis.ReadyFronts))
}

func renderSwarmAnalysisWarnings(analysis *SwarmAnalysis) {
	if len(analysis.Warnings) > 0 {
		fmt.Printf("\n%s Warnings:\n", ui.RenderWarn("⚠"))
		for _, warning := range analysis.Warnings {
			fmt.Printf("   • %s\n", warning)
		}
	}
}

func renderSwarmAnalysisErrors(analysis *SwarmAnalysis) {
	if len(analysis.Errors) > 0 {
		fmt.Printf("\n%s Errors:\n", ui.RenderFail("✗"))
		for _, err := range analysis.Errors {
			fmt.Printf("   • %s\n", err)
		}
	}
}

func renderSwarmAnalysisVerdict(analysis *SwarmAnalysis) {
	fmt.Println()
	if analysis.Swarmable {
		fmt.Printf("%s Swarmable: YES\n\n", ui.RenderPass("✓"))
	} else {
		fmt.Printf("%s Swarmable: NO (fix errors first)\n\n", ui.RenderFail("✗"))
	}
}
