package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

func findCandidateIssues(ctx context.Context, s storage.DoltStorage, p migrateIssuesParams) ([]string, error) {
	filter := migrationIssueFilter(p)

	issues, err := s.SearchIssues(ctx, "", filter)
	if err != nil {
		return nil, err
	}

	candidates := make([]string, len(issues))
	for i, issue := range issues {
		candidates[i] = issue.ID
	}

	return candidates, nil
}

func migrationIssueFilter(p migrateIssuesParams) types.IssueFilter {
	// migrate-issues is round-trip and must enumerate every candidate, so it
	// opts out of BEADS_MAX_ROWS (designer §4.1).
	filter := types.IssueFilter{
		IssueFilterFlags: types.IssueFilterFlags{
			SourceRepo: &p.from,
		},
		IssueFilterPage: types.IssueFilterPage{
			MaxRows:       0,
			MaxRowsSource: "",
		},
	}
	if p.status != "" && p.status != "all" {
		status := types.Status(p.status)
		filter.Status = &status
	}
	if p.priority >= 0 {
		filter.Priority = &p.priority
	}
	if p.issueType != "" && p.issueType != "all" {
		issueType := types.IssueType(p.issueType)
		filter.IssueType = &issueType
	}
	if len(p.labels) > 0 {
		filter.Labels = p.labels
	}
	if len(p.ids) > 0 {
		filter.IDs = p.ids
	}
	return filter
}

type dependencyStats struct {
	incomingEdges int
	outgoingEdges int
}

func expandMigrationSet(ctx context.Context, s storage.DoltStorage, candidates []string, p migrateIssuesParams) ([]string, dependencyStats, error) {
	if p.include == "none" || p.include == "" {
		return candidates, dependencyStats{}, nil
	}

	migrationSet, err := collectMigrationSet(ctx, s, candidates, p)
	if err != nil {
		return nil, dependencyStats{}, err
	}

	result := migrationSetIDs(migrationSet)

	stats, err := countCrossRepoEdges(ctx, s, result)
	if err != nil {
		return nil, dependencyStats{}, err
	}

	return result, stats, nil
}

func collectMigrationSet(ctx context.Context, s storage.DoltStorage, candidates []string, p migrateIssuesParams) (map[string]bool, error) {
	migrationSet := make(map[string]bool, len(candidates))
	for _, id := range candidates {
		migrationSet[id] = true
	}

	visited := make(map[string]bool)
	queue := make([]string, len(candidates))
	copy(queue, candidates)
	queueIndex := 0
	for {
		if queueIndex >= len(queue) {
			break
		}
		current := queue[queueIndex]
		queueIndex++
		if visited[current] {
			continue
		}
		visited[current] = true

		deps, err := migrationDependencies(ctx, s, current, p)
		if err != nil {
			return nil, err
		}
		for _, dep := range deps {
			if !visited[dep] {
				migrationSet[dep] = true
				queue = append(queue, dep)
			}
		}
	}
	return migrationSet, nil
}

func migrationSetIDs(migrationSet map[string]bool) []string {
	result := make([]string, 0, len(migrationSet))
	for id := range migrationSet {
		result = append(result, id)
	}
	return result
}

func migrationDependencies(ctx context.Context, s storage.DoltStorage, issueID string, p migrateIssuesParams) ([]string, error) {
	switch p.include {
	case "upstream":
		return getUpstreamDependencies(ctx, s, issueID, p.from, p.withinFromOnly)
	case "downstream":
		return getDownstreamDependencies(ctx, s, issueID, p.from, p.withinFromOnly)
	case "closure":
		return migrationClosureDependencies(ctx, s, issueID, p)
	default:
		return nil, nil
	}
}

func migrationClosureDependencies(ctx context.Context, s storage.DoltStorage, issueID string, p migrateIssuesParams) ([]string, error) {
	upstream, err := getUpstreamDependencies(ctx, s, issueID, p.from, p.withinFromOnly)
	if err != nil {
		return nil, err
	}
	downstream, err := getDownstreamDependencies(ctx, s, issueID, p.from, p.withinFromOnly)
	if err != nil {
		return nil, err
	}
	return append(upstream, downstream...), nil
}

// getUpstreamDependencies returns IDs of issues that the given issue depends on.
// If withinFromOnly is true, only returns dependencies whose issues are in fromRepo.
func getUpstreamDependencies(ctx context.Context, s storage.DoltStorage, issueID, fromRepo string, withinFromOnly bool) ([]string, error) {
	// GetDependencyRecords returns deps where issue_id = issueID
	depRecords, err := s.GetDependencyRecords(ctx, issueID)
	if err != nil {
		return nil, err
	}

	var deps []string
	for _, dep := range depRecords {
		if withinFromOnly {
			// Check if the depended-on issue is in the source repo
			depIssue, err := s.GetIssue(ctx, dep.DependsOnID)
			if err != nil {
				return nil, err
			}
			if depIssue == nil || depIssue.SourceRepo != fromRepo {
				continue
			}
		}
		deps = append(deps, dep.DependsOnID)
	}

	return deps, nil
}

// getDownstreamDependencies returns IDs of issues that depend on the given issue.
// If withinFromOnly is true, only returns dependents whose issues are in fromRepo.
func getDownstreamDependencies(ctx context.Context, s storage.DoltStorage, issueID, fromRepo string, withinFromOnly bool) ([]string, error) {
	// GetDependents returns full Issue objects that depend on issueID
	dependents, err := s.GetDependents(ctx, issueID)
	if err != nil {
		return nil, err
	}

	var deps []string
	for _, issue := range dependents {
		if withinFromOnly && issue.SourceRepo != fromRepo {
			continue
		}
		deps = append(deps, issue.ID)
	}

	return deps, nil
}

func countCrossRepoEdges(ctx context.Context, s storage.DoltStorage, migrationSet []string) (dependencyStats, error) {
	if len(migrationSet) == 0 {
		return dependencyStats{}, nil
	}

	setMap := make(map[string]bool, len(migrationSet))
	for _, id := range migrationSet {
		setMap[id] = true
	}

	// Get all dependency records for migration set issues (outgoing direction)
	depsByIssue, err := s.GetDependencyRecordsForIssues(ctx, migrationSet)
	if err != nil {
		return dependencyStats{}, fmt.Errorf("failed to get dependency records: %w", err)
	}
	outgoing := countOutgoingDependencyEdges(depsByIssue, setMap)

	allDeps, err := s.GetAllDependencyRecords(ctx)
	if err != nil {
		return dependencyStats{}, fmt.Errorf("failed to get all dependency records: %w", err)
	}
	incoming := countIncomingDependencyEdges(allDeps, setMap)
	return dependencyStats{
		incomingEdges: incoming,
		outgoingEdges: outgoing,
	}, nil
}

func countOutgoingDependencyEdges(depsByIssue map[string][]*types.Dependency, setMap map[string]bool) int {
	outgoing := 0
	for _, deps := range depsByIssue {
		for _, dep := range deps {
			if !setMap[dep.DependsOnID] {
				outgoing++
			}
		}
	}
	return outgoing
}

func countIncomingDependencyEdges(allDeps map[string][]*types.Dependency, setMap map[string]bool) int {
	incoming := 0
	for issueID, deps := range allDeps {
		if setMap[issueID] {
			continue // Skip edges from within the migration set
		}
		for _, dep := range deps {
			if setMap[dep.DependsOnID] {
				incoming++
			}
		}
	}
	return incoming
}

func checkOrphanedDependencies(ctx context.Context, s storage.DoltStorage) ([]string, error) {
	// Get all dependency records to check for orphans
	allDeps, err := s.GetAllDependencyRecords(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get dependency records: %w", err)
	}

	// Collect all unique IDs referenced in dependencies
	referencedIDs := make(map[string]bool)
	for issueID, deps := range allDeps {
		referencedIDs[issueID] = true
		for _, dep := range deps {
			referencedIDs[dep.DependsOnID] = true
		}
	}

	// Batch-check which IDs exist
	idList := make([]string, 0, len(referencedIDs))
	for id := range referencedIDs {
		idList = append(idList, id)
	}

	existingIssues, err := s.SearchIssues(ctx, "", types.IssueFilter{
		IssueFilterCore: types.IssueFilterCore{
			IDs: idList,
		},
		IssueFilterPage: types.IssueFilterPage{
			MaxRows:       0,
			MaxRowsSource: "",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to check issue existence: %w", err)
	}

	existingSet := make(map[string]bool, len(existingIssues))
	for _, issue := range existingIssues {
		existingSet[issue.ID] = true
	}

	// Find orphans (referenced but non-existent)
	var orphans []string
	for id := range referencedIDs {
		if !existingSet[id] {
			orphans = append(orphans, id)
		}
	}

	return orphans, nil
}

func buildMigrationPlan(candidates, migrationSet []string, stats dependencyStats, orphans []string, from, to string) migrationPlan {
	orphanSamples := orphans
	if len(orphanSamples) > 10 {
		orphanSamples = orphanSamples[:10]
	}

	return migrationPlan{
		TotalSelected:     len(candidates),
		AddedByDependency: len(migrationSet) - len(candidates),
		IncomingEdges:     stats.incomingEdges,
		OutgoingEdges:     stats.outgoingEdges,
		Orphans:           len(orphans),
		OrphanSamples:     orphanSamples,
		IssueIDs:          migrationSet,
		From:              from,
		To:                to,
	}
}

func displayMigrationPlan(plan migrationPlan, dryRun bool) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"plan":    plan,
			"dry_run": dryRun,
		})
	}

	displayMigrationPlanSummary(plan)
	displayMigrationOrphanWarning(plan)
	if dryRun {
		displayDryRunMigrationIssues(plan.IssueIDs)
	}
	return nil
}

func displayMigrationPlanSummary(plan migrationPlan) {
	fmt.Println("\n=== Migration Plan ===")
	fmt.Printf("From: %s\n", plan.From)
	fmt.Printf("To:   %s\n", plan.To)
	fmt.Println()
	fmt.Printf("Total selected:           %d issues\n", plan.TotalSelected)
	if plan.AddedByDependency > 0 {
		fmt.Printf("Added by dependencies:    %d issues\n", plan.AddedByDependency)
	}
	fmt.Printf("Total to migrate:         %d issues\n", len(plan.IssueIDs))
	fmt.Println()
	fmt.Printf("Cross-repo edges preserved:\n")
	fmt.Printf("  Incoming:  %d\n", plan.IncomingEdges)
	fmt.Printf("  Outgoing:  %d\n", plan.OutgoingEdges)
}

func displayMigrationOrphanWarning(plan migrationPlan) {
	if plan.Orphans > 0 {
		fmt.Println()
		fmt.Printf("⚠️  Warning: Found %d orphaned dependencies\n", plan.Orphans)
		if len(plan.OrphanSamples) > 0 {
			fmt.Println("Sample orphaned IDs:")
			for _, id := range plan.OrphanSamples {
				fmt.Printf("  - %s\n", id)
			}
		}
	}
}

func displayDryRunMigrationIssues(issueIDs []string) {
	fmt.Println("\n[DRY RUN] No changes made")
	if len(issueIDs) <= 20 {
		fmt.Println("\nIssues to migrate:")
		for _, id := range issueIDs {
			fmt.Printf("  - %s\n", id)
		}
		return
	}
	fmt.Printf("\n(%d issues would be migrated, showing first 20)\n", len(issueIDs))
	maxDisplayed := minMigrationIssueCount(len(issueIDs), 20)
	for i := 0; i < maxDisplayed; i++ {
		fmt.Printf("  - %s\n", issueIDs[i])
	}
}

func minMigrationIssueCount(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func confirmMigration(plan migrationPlan) bool {
	fmt.Printf("\nMigrate %d issues from %s to %s? [y/N] ", len(plan.IssueIDs), plan.From, plan.To)
	var response string
	_, _ = fmt.Scanln(&response)
	return strings.ToLower(strings.TrimSpace(response)) == "y"
}

func executeMigration(ctx context.Context, s storage.DoltStorage, migrationSet []string, to string) error {
	return transact(ctx, s, fmt.Sprintf("bd: migrate %d issues to %s", len(migrationSet), to), func(tx storage.Transaction) error {
		for _, id := range migrationSet {
			if err := tx.UpdateIssue(ctx, id, map[string]interface{}{
				"source_repo": to,
			}, getActor()); err != nil {
				return fmt.Errorf("failed to update issue %s: %w", id, err)
			}
		}
		return nil
	})
}

func loadIDsFromFile(path string) ([]string, error) {
	// #nosec G304 -- file path supplied explicitly via CLI flag
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(data), "\n")
	var ids []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			ids = append(ids, line)
		}
	}

	return ids, nil
}

func init() {
	migrateCmd.AddCommand(migrateIssuesCmd)

	migrateIssuesCmd.Flags().String("from", "", "Source repository (required)")
	migrateIssuesCmd.Flags().String("to", "", "Destination repository (required)")
	migrateIssuesCmd.Flags().String("status", "", "Filter by status (open/closed/all)")
	migrateIssuesCmd.Flags().Int("priority", -1, "Filter by priority (0-4)")
	migrateIssuesCmd.Flags().String("type", "", "Filter by issue type (bug/feature/task/epic/chore/decision)")
	migrateIssuesCmd.Flags().StringSlice("label", nil, "Filter by labels (can specify multiple)")
	migrateIssuesCmd.Flags().StringSlice("id", nil, "Specific issue IDs to migrate (can specify multiple)")
	migrateIssuesCmd.Flags().String("ids-file", "", "File containing issue IDs (one per line)")
	migrateIssuesCmd.Flags().String("include", "none", "Include dependencies: none/upstream/downstream/closure")
	migrateIssuesCmd.Flags().Bool("within-from-only", true, "Only include dependencies from source repo")
	migrateIssuesCmd.Flags().Bool("dry-run", false, "Show plan without making changes")
	migrateIssuesCmd.Flags().Bool("strict", false, "Fail on orphaned dependencies or missing repos")
	migrateIssuesCmd.Flags().Bool("yes", false, "Skip confirmation prompt")

	_ = migrateIssuesCmd.MarkFlagRequired("from") // Only fails if flag missing (caught in tests)
	_ = migrateIssuesCmd.MarkFlagRequired("to")   // Only fails if flag missing (caught in tests)

	// Backwards compatibility alias at root level (hidden)
	migrateIssuesAliasCmd := *migrateIssuesCmd
	migrateIssuesAliasCmd.Use = "migrate-issues"
	migrateIssuesAliasCmd.Hidden = true
	migrateIssuesAliasCmd.Deprecated = "use 'bd migrate issues' instead (will be removed in v1.0.0)"
	rootCmd.AddCommand(&migrateIssuesAliasCmd)
}
