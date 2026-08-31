package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/jonbaldie/beads/internal/workapi"
	"github.com/spf13/cobra"
)

type readyExplanationData struct {
	readyIssues   []*types.Issue
	blockedIssues []*types.BlockedIssue
	depCounts     map[string]*types.DependencyCounts
	allDeps       map[string][]*types.Dependency
	blockerMap    map[string]*types.Issue
	cycles        [][]*types.Issue
}

func runReadyExplain(_ *cobra.Command) error {
	ctx := getRootContext()
	data, err := loadReadyExplanationData(ctx, getStore())
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	explanation := types.BuildReadyExplanation(
		data.readyIssues,
		data.blockedIssues,
		data.depCounts,
		data.allDeps,
		data.blockerMap,
		data.cycles,
	)

	if isJSONOutput() {
		return outputJSON(explanation)
	}

	renderReadyExplanation(explanation)
	return nil
}

func loadReadyExplanationData(ctx context.Context, activeStore storage.DoltStorage) (readyExplanationData, error) {
	readyIssues, blockedIssues, err := loadReadyExplanationIssues(ctx, activeStore)
	if err != nil {
		return readyExplanationData{}, err
	}
	readyIDs := readyExplanationIssueIDs(readyIssues)
	depCounts, allDeps, cycles := loadReadyExplanationDependencies(ctx, activeStore, readyIDs)
	blockerMap := loadReadyExplanationBlockers(ctx, activeStore, blockedIssues)
	return readyExplanationData{
		readyIssues:   readyIssues,
		blockedIssues: blockedIssues,
		depCounts:     depCounts,
		allDeps:       allDeps,
		blockerMap:    blockerMap,
		cycles:        cycles,
	}, nil
}

func loadReadyExplanationIssues(ctx context.Context, activeStore storage.DoltStorage) ([]*types.Issue, []*types.BlockedIssue, error) {
	filter, err := readyExplainFilter()
	if err != nil {
		return nil, nil, err
	}
	readyIssues, err := activeStore.GetReadyWork(ctx, filter)
	if err != nil {
		return nil, nil, err
	}
	blockedIssues, err := activeStore.GetBlockedIssues(ctx, types.WorkFilter{})
	if err != nil {
		return nil, nil, err
	}
	return readyIssues, blockedIssues, nil
}

func readyExplanationIssueIDs(readyIssues []*types.Issue) []string {
	readyIDs := make([]string, len(readyIssues))
	for i, issue := range readyIssues {
		readyIDs[i] = issue.ID
	}
	return readyIDs
}

func loadReadyExplanationDependencies(ctx context.Context, activeStore storage.DoltStorage, readyIDs []string) (map[string]*types.DependencyCounts, map[string][]*types.Dependency, [][]*types.Issue) {
	depCounts, err := activeStore.GetDependencyCounts(ctx, readyIDs)
	if err != nil {
		debug.Logf("warning: failed to get dependency counts: %v", err)
	}
	allDeps, err := activeStore.GetDependencyRecordsForIssues(ctx, readyIDs)
	if err != nil {
		debug.Logf("warning: failed to get dependency records: %v", err)
	}
	cycles, err := activeStore.DetectCycles(ctx)
	if err != nil {
		debug.Logf("warning: failed to detect cycles: %v", err)
	}
	return depCounts, allDeps, cycles
}

func loadReadyExplanationBlockers(ctx context.Context, activeStore storage.DoltStorage, blockedIssues []*types.BlockedIssue) map[string]*types.Issue {
	blockerIDs := readyExplanationBlockerIDs(blockedIssues)
	blockerIssues, err := activeStore.GetIssuesByIDs(ctx, blockerIDs)
	if err != nil {
		debug.Logf("warning: failed to get blocker issues: %v", err)
	}
	blockerMap := make(map[string]*types.Issue, len(blockerIssues))
	for _, issue := range blockerIssues {
		blockerMap[issue.ID] = issue
	}
	return blockerMap
}

func readyExplanationBlockerIDs(blockedIssues []*types.BlockedIssue) []string {
	allBlockerIDs := make(map[string]bool)
	for _, blockedIssue := range blockedIssues {
		for _, blockerID := range blockedIssue.BlockedBy {
			allBlockerIDs[blockerID] = true
		}
	}
	blockerIDs := make([]string, 0, len(allBlockerIDs))
	for id := range allBlockerIDs {
		blockerIDs = append(blockerIDs, id)
	}
	return blockerIDs
}

func renderReadyExplanation(explanation types.ReadyExplanation) {
	fmt.Printf("\n%s Ready Work Explanation\n\n", ui.RenderAccent("▸"))
	renderReadyExplanationItems(explanation.Ready)
	renderBlockedExplanationItems(explanation.Blocked)
	renderReadyExplanationCycles(explanation.Cycles)
	renderReadyExplanationSummary(explanation.Summary)
}

func renderReadyExplanationItems(items []types.ReadyItem) {
	if len(items) == 0 {
		fmt.Printf("%s No ready work\n\n", ui.RenderWarn("○"))
		return
	}
	fmt.Printf("%s Ready (%d issues):\n\n", ui.RenderPass("●"), len(items))
	for _, item := range items {
		fmt.Printf("  %s [%s] %s\n",
			ui.RenderID(item.ID),
			ui.RenderPriority(item.Priority),
			item.Title)
		fmt.Printf("    Reason: %s\n", item.Reason)
		if len(item.ResolvedBlockers) > 0 {
			fmt.Printf("    Resolved blockers: %s\n", strings.Join(item.ResolvedBlockers, ", "))
		}
		if item.DependentCount > 0 {
			fmt.Printf("    Unblocks: %d issue(s)\n", item.DependentCount)
		}
		fmt.Println()
	}
}

func renderBlockedExplanationItems(items []types.BlockedItem) {
	if len(items) == 0 {
		return
	}
	fmt.Printf("%s Blocked (%d issues):\n\n", ui.RenderFail("●"), len(items))
	for _, item := range items {
		fmt.Printf("  %s [%s] %s\n",
			ui.RenderID(item.ID),
			ui.RenderPriority(item.Priority),
			item.Title)
		for _, blocker := range item.BlockedBy {
			fmt.Printf("    ← blocked by %s: %s [%s]\n",
				ui.RenderID(blocker.ID), blocker.Title, blocker.Status)
		}
		fmt.Println()
	}
}

func renderReadyExplanationCycles(cycles [][]string) {
	if len(cycles) == 0 {
		return
	}
	fmt.Printf("%s Cycles detected (%d):\n\n", ui.RenderFail("⚠"), len(cycles))
	for _, cycle := range cycles {
		fmt.Printf("  %s → %s\n", strings.Join(cycle, " → "), cycle[0])
	}
	fmt.Println()
}

func renderReadyExplanationSummary(summary types.ExplainSummary) {
	fmt.Printf("%s Summary: %d ready, %d blocked",
		ui.RenderMuted("─"),
		summary.TotalReady,
		summary.TotalBlocked)
	if summary.CycleCount > 0 {
		fmt.Printf(", %d cycle(s)", summary.CycleCount)
	}
	fmt.Printf("\n\n")
}

func runMoleculeReady(_ *cobra.Command, molIDArg string) error {
	ctx := getRootContext()
	moleculeID, subgraph, err := loadReadyMolecule(ctx, molIDArg)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	analysis := analyzeMoleculeParallel(subgraph)
	readySteps := collectReadyMoleculeSteps(subgraph, analysis)

	if isJSONOutput() {
		return outputJSON(MoleculeReadyOutput{
			MoleculeID:     moleculeID,
			MoleculeTitle:  subgraph.Root.Title,
			TotalSteps:     analysis.TotalSteps,
			ReadySteps:     len(readySteps),
			Steps:          readySteps,
			ParallelGroups: analysis.ParallelGroups,
		})
	}

	renderMoleculeReadyHuman(moleculeID, subgraph, analysis, readySteps)
	return nil
}

func loadReadyMolecule(ctx context.Context, molIDArg string) (string, *MoleculeSubgraph, error) {
	if getStore() == nil {
		return "", nil, fmt.Errorf("no database connection")
	}
	moleculeID, err := utils.ResolvePartialID(ctx, getStore(), molIDArg)
	if err != nil {
		return "", nil, fmt.Errorf("molecule '%s' not found", molIDArg)
	}
	subgraph, err := loadTemplateSubgraph(ctx, getStore(), moleculeID)
	if err != nil {
		return "", nil, fmt.Errorf("loading molecule: %v", err)
	}
	return moleculeID, subgraph, nil
}

func collectReadyMoleculeSteps(subgraph *MoleculeSubgraph, analysis *ParallelAnalysis) []*MoleculeReadyStep {
	var readySteps []*MoleculeReadyStep
	for _, issue := range subgraph.Issues {
		info := analysis.Steps[issue.ID]
		if info != nil && info.IsReady {
			readySteps = append(readySteps, &MoleculeReadyStep{
				Issue:         issue,
				ParallelInfo:  info,
				ParallelGroup: info.ParallelGroup,
			})
		}
	}
	return readySteps
}

func renderMoleculeReadyHuman(moleculeID string, subgraph *MoleculeSubgraph, analysis *ParallelAnalysis, readySteps []*MoleculeReadyStep) {
	fmt.Printf("\n%s Ready steps in molecule: %s\n", ui.RenderAccent("🧪"), subgraph.Root.Title)
	fmt.Printf("   ID: %s\n", moleculeID)
	fmt.Printf("   Total: %d steps, %d ready\n", analysis.TotalSteps, len(readySteps))

	if len(readySteps) == 0 {
		fmt.Printf("\n%s No ready steps (all blocked or completed)\n\n", ui.RenderWarn("○"))
		return
	}

	renderMoleculeParallelGroups(analysis)
	fmt.Printf("\n%s Ready steps:\n\n", ui.RenderPass("▸"))
	renderReadyMoleculeSteps(analysis, readySteps)
	fmt.Println()
}

func renderMoleculeParallelGroups(analysis *ParallelAnalysis) {
	if len(analysis.ParallelGroups) == 0 {
		return
	}
	fmt.Printf("\n%s Parallel Groups:\n", ui.RenderPass("⚡"))
	for groupName, members := range analysis.ParallelGroups {
		readyInGroup := 0
		for _, id := range members {
			if info := analysis.Steps[id]; info != nil && info.IsReady {
				readyInGroup++
			}
		}
		if readyInGroup > 0 {
			fmt.Printf("   %s: %d ready\n", groupName, readyInGroup)
		}
	}
}

func renderReadyMoleculeSteps(analysis *ParallelAnalysis, readySteps []*MoleculeReadyStep) {
	for i, step := range readySteps {
		groupAnnotation := readyMoleculeGroupAnnotation(step)

		fmt.Printf("%d. [%s] [%s] %s: %s%s\n", i+1,
			ui.RenderPriority(step.Issue.Priority),
			ui.RenderType(string(step.Issue.IssueType)),
			ui.RenderID(step.Issue.ID),
			step.Issue.Title,
			groupAnnotation)

		readyParallel := readyParallelMoleculeIDs(analysis, step)
		if len(readyParallel) > 0 {
			fmt.Printf("   Can run with: %v\n", readyParallel)
		}
	}
}

func readyMoleculeGroupAnnotation(step *MoleculeReadyStep) string {
	if step.ParallelGroup == "" {
		return ""
	}
	return fmt.Sprintf(" [%s]", ui.RenderAccent(step.ParallelGroup))
}

func readyParallelMoleculeIDs(analysis *ParallelAnalysis, step *MoleculeReadyStep) []string {
	var readyParallel []string
	for _, pID := range step.ParallelInfo.CanParallel {
		if pInfo := analysis.Steps[pID]; pInfo != nil && pInfo.IsReady {
			readyParallel = append(readyParallel, pID)
		}
	}
	return readyParallel
}

// MoleculeReadyStep holds a ready step with its parallel info
type MoleculeReadyStep struct {
	Issue         *types.Issue  `json:"issue"`
	ParallelInfo  *ParallelInfo `json:"parallel_info"`
	ParallelGroup string        `json:"parallel_group,omitempty"`
}

// MoleculeReadyOutput is the JSON output for bd ready --mol
type MoleculeReadyOutput struct {
	MoleculeID     string               `json:"molecule_id"`
	MoleculeTitle  string               `json:"molecule_title"`
	TotalSteps     int                  `json:"total_steps"`
	ReadySteps     int                  `json:"ready_steps"`
	Steps          []*MoleculeReadyStep `json:"steps"`
	ParallelGroups map[string][]string  `json:"parallel_groups"`
}

func init() {
	readyCmd.Flags().IntP("limit", "n", workapi.DefaultReadyLimit, "Maximum issues to show (use 0 for unlimited)")
	readyCmd.Flags().Int("offset", 0, "Skip the first N matching results (0-based). Only supported under --proxied-server.")
	readyCmd.Flags().IntP("priority", "p", 0, "Filter by priority")
	readyCmd.Flags().StringP("assignee", "a", "", "Filter by assignee")
	readyCmd.Flags().BoolP("unassigned", "u", false, "Show only unassigned issues")
	readyCmd.Flags().StringP("sort", "s", "priority", "Sort policy: priority (default), hybrid, oldest")
	readyCmd.Flags().StringSliceP("label", "l", []string{}, "Filter by labels (AND: must have ALL). Can combine with --label-any")
	readyCmd.Flags().StringSlice("label-any", []string{}, "Filter by labels (OR: must have AT LEAST ONE). Can combine with --label")
	readyCmd.Flags().StringSlice("exclude-label", []string{}, "Exclude issues that have ANY of these labels")
	readyCmd.Flags().String("label-pattern", "", "Filter by label glob pattern (e.g., 'tech-*' matches tech-debt, tech-legacy)")
	readyCmd.Flags().String("label-regex", "", "Filter by label regex pattern (e.g., 'tech-(debt|legacy)')")
	readyCmd.Flags().StringP("type", "t", "", "Filter by issue type (task, bug, feature, epic, decision, merge-request). Aliases: mr→merge-request, feat→feature, mol→molecule, dec/adr→decision")
	readyCmd.Flags().String("mol", "", "Filter to steps within a specific molecule")
	readyCmd.Flags().String("parent", "", "Filter to descendants of this bead/epic")
	readyCmd.Flags().String("mol-type", "", "Filter by molecule type: swarm, patrol, or work")
	readyCmd.Flags().Bool("pretty", true, "Display issues in a tree format with status/priority symbols")
	readyCmd.Flags().Bool("plain", false, "Display issues as a plain numbered list")
	readyCmd.Flags().Bool("include-deferred", false, "Include issues with future defer_until timestamps")
	readyCmd.Flags().Bool("include-ephemeral", false, "Include ephemeral issues (wisps) in results")
	readyCmd.Flags().Bool("gated", false, "Find molecules ready for gate-resume dispatch")
	readyCmd.Flags().StringSlice("exclude-type", nil, "Exclude issue types from results (comma-separated or repeatable, e.g., --exclude-type=convoy,epic)")
	readyCmd.Flags().Bool("explain", false, "Show dependency-aware reasoning for why issues are ready or blocked")
	readyCmd.Flags().Bool("claim", false, "Atomically claim the first ready issue matching the filters")
	// Projection toggle, the same one `bd list --brief` sets. Refused with
	// --claim, which returns one whole row by contract; see gatherReadyInput.
	readyCmd.Flags().Bool("brief", false,
		"Omit the free-form text (description, design, acceptance criteria, notes, "+
			"payload, waiters) from each row. Filters that read those fields still "+
			"select on them. An omitted field is indistinguishable from an empty "+
			"one; fetch a whole issue with bd show. Requires --json, and cannot be "+
			"combined with --claim, --gated, --mol or --explain.")
	// Metadata filtering (GH#1406)
	readyCmd.Flags().StringArray("metadata-field", nil, "Filter by metadata field (key=value, repeatable)")
	readyCmd.Flags().String("has-metadata-key", "", "Filter issues that have this metadata key set")
	// Defensive row cap (be-x42v): exits 2 on overage, default disabled.
	addMaxRowsFlag(readyCmd)
	rootCmd.AddCommand(readyCmd)
	blockedCmd.Flags().String("parent", "", "Filter to descendants of this bead/epic")
	rootCmd.AddCommand(blockedCmd)
}
