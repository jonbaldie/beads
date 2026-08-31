package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
)

// getMoleculeProgress loads a molecule and computes progress
func getMoleculeProgress(ctx context.Context, s molReader, moleculeID string) (*MoleculeProgress, error) {
	subgraph, err := loadTemplateSubgraph(ctx, s, moleculeID)
	if err != nil {
		return nil, err
	}

	progress := &MoleculeProgress{
		MoleculeID:    subgraph.Root.ID,
		MoleculeTitle: subgraph.Root.Title,
		Assignee:      subgraph.Root.Assignee,
		Total:         len(subgraph.Issues) - 1, // Exclude root
	}

	// Compute step readiness from within-molecule dependencies.
	// Uses analyzeMoleculeParallel instead of GetReadyWork because GetReadyWork
	// excludes ephemeral issues (wisp steps are ephemeral by definition).
	// See: https://github.com/steveyegge/gastown/issues/1276 (historical reference)
	analysis := analyzeMoleculeParallel(subgraph)
	readyIDs := readyStepIDs(analysis)
	steps, completed, current, next := buildMoleculeSteps(subgraph, readyIDs)
	progress.Completed = completed
	progress.CurrentStep = current
	progress.NextStep = next

	// Sort steps by dependency order
	sortStepsByDependencyOrder(steps, subgraph)
	progress.Steps = steps

	// If no current step but there's a ready step, set it as next
	if progress.CurrentStep == nil && progress.NextStep == nil {
		progress.NextStep = firstReadyStep(steps)
	}

	return progress, nil
}

func readyStepIDs(analysis *ParallelAnalysis) map[string]bool {
	readyIDs := make(map[string]bool)
	for id, info := range analysis.Steps {
		if info.IsReady {
			readyIDs[id] = true
		}
	}
	return readyIDs
}

func buildMoleculeSteps(subgraph *MoleculeSubgraph, readyIDs map[string]bool) (steps []*StepStatus, completed int, current, next *types.Issue) {
	for _, issue := range subgraph.Issues {
		if issue.ID == subgraph.Root.ID {
			continue
		}

		step := moleculeStepStatus(issue, readyIDs)
		switch step.Status {
		case "done":
			completed++
		case "current":
			current = issue
		case "ready":
			if next == nil {
				next = issue
			}
		}
		steps = append(steps, step)
	}
	return steps, completed, current, next
}

func moleculeStepStatus(issue *types.Issue, readyIDs map[string]bool) *StepStatus {
	step := &StepStatus{Issue: issue}
	switch issue.Status {
	case types.StatusClosed:
		step.Status = "done"
	case types.StatusInProgress:
		step.Status = "current"
		step.IsCurrent = true
	case types.StatusBlocked:
		step.Status = "blocked"
	case types.StatusOpen:
		if readyIDs[issue.ID] {
			step.Status = "ready"
		} else {
			step.Status = "pending"
		}
	default:
		if readyIDs[issue.ID] {
			step.Status = "ready"
		} else {
			step.Status = "pending"
		}
	}
	return step
}

func firstReadyStep(steps []*StepStatus) *types.Issue {
	for _, step := range steps {
		if step.Status == "ready" {
			return step.Issue
		}
	}
	return nil
}

// findInProgressMolecules finds molecules with in_progress steps for an agent
func findInProgressMolecules(ctx context.Context, s molReader, agent string) []*MoleculeProgress {
	inProgressIssues, err := searchMoleculeIssues(ctx, s, types.StatusInProgress, agent)
	if err != nil || len(inProgressIssues) == 0 {
		return nil
	}
	return moleculeProgressForIssues(ctx, s, inProgressIssues)
}

func searchMoleculeIssues(ctx context.Context, s molReader, status types.Status, agent string) ([]*types.Issue, error) {
	filter := types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Status: &status}}
	if agent != "" {
		filter.Assignee = &agent
	}
	return s.SearchIssues(ctx, "", filter)
}

func moleculeProgressForIssues(ctx context.Context, s molReader, issues []*types.Issue) []*MoleculeProgress {
	if len(issues) == 0 {
		return nil
	}

	// Batch-find parent molecules for all in_progress issues (bd-hn4q)
	issueIDs := make([]string, len(issues))
	for i, issue := range issues {
		issueIDs[i] = issue.ID
	}
	moleculeRoots := findParentMolecules(ctx, s, issueIDs)

	moleculeMap := make(map[string]*MoleculeProgress)
	for _, issue := range issues {
		moleculeID := moleculeRoots[issue.ID]
		if moleculeID == "" {
			continue
		}

		if _, exists := moleculeMap[moleculeID]; !exists {
			progress, err := getMoleculeProgress(ctx, s, moleculeID)
			if err == nil {
				moleculeMap[moleculeID] = progress
			}
		}
	}

	return sortedMoleculeProgresses(moleculeMap)
}

// findHookedMolecules finds molecules bonded to hooked issues for an agent.
// This is a fallback when no in_progress steps exist but a molecule is attached
// to the agent's hooked work via a "blocks" dependency.
func findHookedMolecules(ctx context.Context, s molReader, agent string) []*MoleculeProgress {
	// Query for hooked issues assigned to the agent
	hookedIssues, err := searchMoleculeIssues(ctx, s, types.StatusHooked, agent)
	if err != nil || len(hookedIssues) == 0 {
		return nil
	}

	// For each hooked issue, check if it IS a molecule or has blocks deps on one
	moleculeMap := make(map[string]*MoleculeProgress)
	for _, issue := range hookedIssues {
		collectHookedMoleculeProgress(ctx, s, issue, moleculeMap)
	}

	return sortedMoleculeProgresses(moleculeMap)
}

func collectHookedMoleculeProgress(ctx context.Context, s molReader, issue *types.Issue, moleculeMap map[string]*MoleculeProgress) {
	// Check if the hooked issue itself is a molecule (e.g., patrol wisps
	// are directly hooked without a separate handoff bead). hq-3paz0m
	if issue.IssueType == types.TypeEpic {
		if _, exists := moleculeMap[issue.ID]; !exists {
			progress, err := getMoleculeProgress(ctx, s, issue.ID)
			if err == nil {
				moleculeMap[issue.ID] = progress
				return
			}
		}
	}

	for _, candidate := range hookedMoleculeCandidates(ctx, s, issue.ID) {
		addMoleculeProgress(ctx, s, candidate.ID, moleculeMap)
	}
}

func hookedMoleculeCandidates(ctx context.Context, s molReader, issueID string) []*types.Issue {
	deps, err := s.GetDependencyRecords(ctx, issueID)
	if err != nil {
		return nil
	}

	var candidates []*types.Issue
	for _, dep := range deps {
		if dep.Type != types.DepBlocks {
			continue
		}
		candidate, err := s.GetIssue(ctx, dep.DependsOnID)
		if err != nil || candidate == nil || !isHookedMoleculeCandidate(candidate) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func isHookedMoleculeCandidate(issue *types.Issue) bool {
	if issue.IssueType == types.TypeEpic {
		return true
	}
	for _, label := range issue.Labels {
		if label == BeadsTemplateLabel {
			return true
		}
	}
	return false
}

func addMoleculeProgress(ctx context.Context, s molReader, moleculeID string, moleculeMap map[string]*MoleculeProgress) {
	if _, exists := moleculeMap[moleculeID]; exists {
		return
	}
	progress, err := getMoleculeProgress(ctx, s, moleculeID)
	if err == nil {
		moleculeMap[moleculeID] = progress
	}
}

func sortedMoleculeProgresses(moleculeMap map[string]*MoleculeProgress) []*MoleculeProgress {
	molecules := make([]*MoleculeProgress, 0, len(moleculeMap))
	for _, mol := range moleculeMap {
		molecules = append(molecules, mol)
	}
	sort.Slice(molecules, func(i, j int) bool {
		return molecules[i].MoleculeID < molecules[j].MoleculeID
	})
	return molecules
}

// findParentMolecules batch-finds the root molecule for multiple issue IDs.
// Returns a map of issueID → moleculeRootID for issues that belong to a molecule.
// Issues not part of a molecule are omitted from the result.
//
// This replaces the previous N+1 pattern where findParentMolecule was called
// in a loop, issuing GetDependencyRecords + GetIssue per level per issue.
// Instead, this walks parent-child chains level-by-level using batch queries,
// reducing O(N * depth) round-trips to O(depth). (bd-hn4q)
func findParentMolecules(ctx context.Context, s molReader, issueIDs []string) map[string]string {
	if len(issueIDs) == 0 {
		return nil
	}

	rootOf, current := initializeMoleculeRootWalk(issueIDs)

	// Walk up parent-child chains in batch, level by level
	for depth := 0; depth < 50; depth++ {
		if len(current) == 0 {
			break
		}
		var err error
		current, err = advanceMoleculeRootWalk(ctx, s, current, rootOf)
		if err != nil {
			return nil
		}
	}

	// Anything still walking after max depth — treat as root
	for startID, curID := range current {
		rootOf[startID] = curID
	}

	return filterMoleculeRoots(ctx, s, rootOf)
}

func initializeMoleculeRootWalk(issueIDs []string) (map[string]string, map[string]string) {
	rootOf := make(map[string]string, len(issueIDs))
	current := make(map[string]string, len(issueIDs))
	for _, id := range issueIDs {
		current[id] = id
	}
	return rootOf, current
}

func advanceMoleculeRootWalk(ctx context.Context, s molReader, current, rootOf map[string]string) (map[string]string, error) {
	toCheck := uniqueCurrentIDs(current)
	allDeps, err := s.GetDependencyRecordsForIssues(ctx, toCheck)
	if err != nil {
		return nil, err
	}
	parentOf := parentIDsByChild(allDeps)

	nextCurrent := make(map[string]string)
	for startID, curID := range current {
		if parent, ok := parentOf[curID]; ok {
			nextCurrent[startID] = parent
		} else {
			rootOf[startID] = curID
		}
	}
	return nextCurrent, nil
}

func uniqueCurrentIDs(current map[string]string) []string {
	seen := make(map[string]bool, len(current))
	toCheck := make([]string, 0, len(current))
	for _, curID := range current {
		if !seen[curID] {
			seen[curID] = true
			toCheck = append(toCheck, curID)
		}
	}
	return toCheck
}

func parentIDsByChild(allDeps map[string][]*types.Dependency) map[string]string {
	parentOf := make(map[string]string, len(allDeps))
	for childID, deps := range allDeps {
		for _, dep := range deps {
			if dep.Type == types.DepParentChild {
				parentOf[childID] = dep.DependsOnID
				break
			}
		}
	}
	return parentOf
}

func filterMoleculeRoots(ctx context.Context, s molReader, rootOf map[string]string) map[string]string {
	rootIDs := uniqueRootIDs(rootOf)
	rootIssues, err := s.GetIssuesByIDs(ctx, rootIDs)
	if err != nil {
		return nil
	}

	isMolecule := make(map[string]bool, len(rootIssues))
	for _, issue := range rootIssues {
		if isMoleculeRoot(issue) {
			isMolecule[issue.ID] = true
		}
	}

	result := make(map[string]string, len(rootOf))
	for startID, rootID := range rootOf {
		if isMolecule[rootID] {
			result[startID] = rootID
		}
	}
	return result
}

func uniqueRootIDs(rootOf map[string]string) []string {
	uniqueRoots := make(map[string]bool, len(rootOf))
	for _, rootID := range rootOf {
		uniqueRoots[rootID] = true
	}
	rootIDs := make([]string, 0, len(uniqueRoots))
	for id := range uniqueRoots {
		rootIDs = append(rootIDs, id)
	}
	return rootIDs
}

func isMoleculeRoot(issue *types.Issue) bool {
	if issue.IssueType == types.TypeEpic || issue.IssueType == types.TypeMolecule {
		return true
	}
	for _, label := range issue.Labels {
		if label == BeadsTemplateLabel {
			return true
		}
	}
	return false
}

// findParentMolecule walks up the parent-child chain to find the root molecule
// for a single issue. Returns "" if the issue is not part of a molecule.
func findParentMolecule(ctx context.Context, s molReader, issueID string) string {
	roots := findParentMolecules(ctx, s, []string{issueID})
	return roots[issueID]
}

// sortStepsByDependencyOrder sorts steps by their dependency order
func sortStepsByDependencyOrder(steps []*StepStatus, subgraph *TemplateSubgraph) {
	// Build dependency graph
	depCount := make(map[string]int) // issue ID -> number of deps
	for _, step := range steps {
		depCount[step.Issue.ID] = 0
	}

	// Count blocking dependencies within the step set
	stepIDs := make(map[string]bool)
	for _, step := range steps {
		stepIDs[step.Issue.ID] = true
	}

	for _, dep := range subgraph.Dependencies {
		if dep.Type == types.DepBlocks && stepIDs[dep.IssueID] && stepIDs[dep.DependsOnID] {
			depCount[dep.IssueID]++
		}
	}

	// Stable sort by dependency count (fewer deps first)
	sort.SliceStable(steps, func(i, j int) bool {
		return depCount[steps[i].Issue.ID] < depCount[steps[j].Issue.ID]
	})
}

// printMoleculeProgress prints the progress in human-readable format
func printMoleculeProgress(mol *MoleculeProgress) {
	fmt.Printf("You're working on molecule %s\n", ui.RenderAccent(mol.MoleculeID))
	fmt.Printf("  %s\n", mol.MoleculeTitle)
	if mol.Assignee != "" {
		fmt.Printf("  Assigned to: %s\n", mol.Assignee)
	}
	fmt.Println()

	for _, step := range mol.Steps {
		statusIcon := getStatusIcon(step.Status)
		marker := ""
		if step.IsCurrent {
			marker = " <- YOU ARE HERE"
		}
		fmt.Printf("  %s %s: %s%s\n", statusIcon, step.Issue.ID, step.Issue.Title, marker)
	}

	fmt.Println()
	fmt.Printf("Progress: %d/%d steps complete\n", mol.Completed, mol.Total)

	if mol.NextStep != nil && mol.CurrentStep == nil {
		fmt.Printf("\nNext ready: %s - %s\n", mol.NextStep.ID, mol.NextStep.Title)
		fmt.Printf("  Start with: bd update %s --claim\n", mol.NextStep.ID)
	}

	// Show hint about viewing step instructions
	var hintStepID string
	if mol.CurrentStep != nil {
		hintStepID = mol.CurrentStep.ID
	} else if mol.NextStep != nil {
		hintStepID = mol.NextStep.ID
	}
	if hintStepID != "" {
		fmt.Printf("\n%s Run `bd show %s` to see detailed instructions.\n", ui.RenderAccent("💡"), hintStepID)
	}
}

// getStatusIcon returns the icon for a step status
func getStatusIcon(status string) string {
	switch status {
	case "done":
		return ui.RenderPass("[done]")
	case "current":
		return ui.RenderWarn("[current]")
	case "ready":
		return ui.RenderAccent("[ready]")
	case "blocked":
		return ui.RenderFail("[blocked]")
	default:
		return "[pending]"
	}
}

// ContinueResult holds the result of advancing to the next molecule step
type ContinueResult struct {
	ClosedStep   *types.Issue `json:"closed_step"`
	NextStep     *types.Issue `json:"next_step,omitempty"`
	AutoAdvanced bool         `json:"auto_advanced"`
	MolComplete  bool         `json:"molecule_complete"`
	MoleculeID   string       `json:"molecule_id,omitempty"`
}
