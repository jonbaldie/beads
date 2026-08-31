// Package fixtures provides realistic test data generation for benchmarks and tests.
package fixtures

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/types"
)

// labels used across all fixtures
var commonLabels = []string{
	"backend",
	"frontend",
	"urgent",
	"tech-debt",
	"documentation",
	"performance",
	"security",
	"ux",
	"api",
	"database",
}

// assignees used across all fixtures
var commonAssignees = []string{
	"alice",
	"bob",
	"charlie",
	"diana",
	"eve",
	"frank",
}

// epic titles for realistic data
var epicTitles = []string{
	"User Authentication System",
	"Payment Processing Integration",
	"Mobile App Redesign",
	"Performance Optimization",
	"API v2 Migration",
	"Search Functionality Enhancement",
	"Analytics Dashboard",
	"Multi-tenant Support",
	"Notification System",
	"Data Export Feature",
}

// feature titles (under epics)
var featureTitles = []string{
	"OAuth2 Integration",
	"Password Reset Flow",
	"Two-Factor Authentication",
	"Session Management",
	"API Endpoints",
	"Database Schema",
	"UI Components",
	"Background Jobs",
	"Error Handling",
	"Testing Infrastructure",
}

// task titles (under features)
var taskTitles = []string{
	"Implement login endpoint",
	"Add validation logic",
	"Write unit tests",
	"Update documentation",
	"Fix memory leak",
	"Optimize query performance",
	"Add error logging",
	"Refactor helper functions",
	"Update database migrations",
	"Configure deployment",
}

// Fixture size rationale:
// We only provide Large (10K) and XLarge (20K) fixtures because:
// - Performance characteristics only emerge at scale (10K+ issues)
// - Smaller fixtures don't provide meaningful optimization insights
// - Code weight matters; we avoid unused complexity
// - Target use case: repositories with thousands of issues

// DataConfig controls the distribution and characteristics of generated test data
type DataConfig struct {
	TotalIssues       int     // total number of issues to generate
	EpicRatio         float64 // percentage of issues that are epics (e.g., 0.1 for 10%)
	FeatureRatio      float64 // percentage of issues that are features (e.g., 0.3 for 30%)
	OpenRatio         float64 // percentage of issues that are open (e.g., 0.5 for 50%)
	CrossLinkRatio    float64 // percentage of tasks with cross-epic blocking dependencies (e.g., 0.2 for 20%)
	MaxEpicAgeDays    int     // maximum age in days for epics (e.g., 180)
	MaxFeatureAgeDays int     // maximum age in days for features (e.g., 150)
	MaxTaskAgeDays    int     // maximum age in days for tasks (e.g., 120)
	MaxClosedAgeDays  int     // maximum days since closure (e.g., 30)
	RandSeed          int64   // random seed for reproducibility
}

// DefaultLargeConfig returns configuration for 10K issue dataset
func DefaultLargeConfig() DataConfig {
	return DataConfig{
		TotalIssues:       10000,
		EpicRatio:         0.1,
		FeatureRatio:      0.3,
		OpenRatio:         0.5,
		CrossLinkRatio:    0.2,
		MaxEpicAgeDays:    180,
		MaxFeatureAgeDays: 150,
		MaxTaskAgeDays:    120,
		MaxClosedAgeDays:  30,
		RandSeed:          42,
	}
}

// DefaultXLargeConfig returns configuration for 20K issue dataset
func DefaultXLargeConfig() DataConfig {
	return DataConfig{
		TotalIssues:       20000,
		EpicRatio:         0.1,
		FeatureRatio:      0.3,
		OpenRatio:         0.5,
		CrossLinkRatio:    0.2,
		MaxEpicAgeDays:    180,
		MaxFeatureAgeDays: 150,
		MaxTaskAgeDays:    120,
		MaxClosedAgeDays:  30,
		RandSeed:          43,
	}
}

// LargeDolt creates a 10K issue database with realistic patterns
func LargeDolt(ctx context.Context, store *dolt.DoltStore) error {
	cfg := DefaultLargeConfig()
	return generateIssuesWithConfig(ctx, store, cfg)
}

// XLargeDolt creates a 20K issue database with realistic patterns
func XLargeDolt(ctx context.Context, store *dolt.DoltStore) error {
	cfg := DefaultXLargeConfig()
	return generateIssuesWithConfig(ctx, store, cfg)
}

// LargeFromJSONL creates a 10K issue database by exporting to JSONL and reimporting
func LargeFromJSONL(ctx context.Context, store *dolt.DoltStore, tempDir string) error {
	cfg := DefaultLargeConfig()
	cfg.RandSeed = 44 // different seed for JSONL path
	return generateFromJSONL(ctx, store, tempDir, cfg)
}

// generateIssuesWithConfig creates issues with realistic epic hierarchies and cross-links using provided configuration
func generateIssuesWithConfig(ctx context.Context, store *dolt.DoltStore, cfg DataConfig) error {
	rng := rand.New(rand.NewSource(cfg.RandSeed)) // #nosec G404 -- deterministic math/rand used for repeatable fixture data
	numEpics := int(float64(cfg.TotalIssues) * cfg.EpicRatio)
	numFeatures := int(float64(cfg.TotalIssues) * cfg.FeatureRatio)
	numTasks := cfg.TotalIssues - numEpics - numFeatures

	progress := &generationProgress{total: cfg.TotalIssues, lastPercent: -1}
	epicIssues, err := generateEpics(ctx, store, cfg, rng, progress)
	if err != nil {
		return err
	}
	featureIssues, err := generateFeatures(ctx, store, cfg, rng, progress, epicIssues)
	if err != nil {
		return err
	}
	taskIssues, err := generateTasks(ctx, store, cfg, rng, progress, featureIssues)
	if err != nil {
		return err
	}

	fmt.Printf("  Progress: 100%% (%d/%d issues created) - Complete!\n", cfg.TotalIssues, cfg.TotalIssues)

	if err := generateCrossLinks(ctx, store, rng, taskIssues, numTasks, cfg.CrossLinkRatio); err != nil {
		return err
	}

	return nil
}

type generationProgress struct {
	total       int
	created     int
	lastPercent int
}

func (p *generationProgress) note() {
	pct := (p.created * 100) / p.total
	if pct >= p.lastPercent+10 {
		fmt.Printf("  Progress: %d%% (%d/%d issues created)\n", pct, p.created, p.total)
		p.lastPercent = pct
	}
}

func generateEpics(ctx context.Context, store *dolt.DoltStore, cfg DataConfig, rng *rand.Rand, progress *generationProgress) ([]*types.Issue, error) {
	numEpics := int(float64(cfg.TotalIssues) * cfg.EpicRatio)
	epics := make([]*types.Issue, 0, numEpics)
	for i := 0; i < numEpics; i++ {
		name := epicTitles[i%len(epicTitles)]
		issue := newFixtureIssue(fmt.Sprintf("%s (Epic %d)", name, i), fmt.Sprintf("Epic for %s", name), types.TypeEpic, cfg.MaxEpicAgeDays, cfg, rng)
		if err := store.CreateIssue(ctx, issue, "fixture"); err != nil {
			return nil, fmt.Errorf("failed to create epic: %w", err)
		}
		addFixtureLabels(ctx, store, issue, rng, 3)
		epics = append(epics, issue)
		progress.created++
		progress.note()
	}
	return epics, nil
}

func generateFeatures(ctx context.Context, store *dolt.DoltStore, cfg DataConfig, rng *rand.Rand, progress *generationProgress, epics []*types.Issue) ([]*types.Issue, error) {
	numFeatures := int(float64(cfg.TotalIssues) * cfg.FeatureRatio)
	features := make([]*types.Issue, 0, numFeatures)
	for i := 0; i < numFeatures; i++ {
		parent := epics[i%len(epics)]
		name := featureTitles[i%len(featureTitles)]
		issue := newFixtureIssue(fmt.Sprintf("%s (Feature %d)", name, i), fmt.Sprintf("Feature under %s", parent.Title), types.TypeFeature, cfg.MaxFeatureAgeDays, cfg, rng)
		if err := store.CreateIssue(ctx, issue, "fixture"); err != nil {
			return nil, fmt.Errorf("failed to create feature: %w", err)
		}
		if err := addFixtureParent(ctx, store, issue, parent, "feature-epic"); err != nil {
			return nil, err
		}
		addFixtureLabels(ctx, store, issue, rng, 3)
		features = append(features, issue)
		progress.created++
		progress.note()
	}
	return features, nil
}

func generateTasks(ctx context.Context, store *dolt.DoltStore, cfg DataConfig, rng *rand.Rand, progress *generationProgress, features []*types.Issue) ([]*types.Issue, error) {
	numTasks := cfg.TotalIssues - int(float64(cfg.TotalIssues)*cfg.EpicRatio) - int(float64(cfg.TotalIssues)*cfg.FeatureRatio)
	tasks := make([]*types.Issue, 0, numTasks)
	for i := 0; i < numTasks; i++ {
		parent := features[i%len(features)]
		name := taskTitles[i%len(taskTitles)]
		issue := newFixtureIssue(fmt.Sprintf("%s (Task %d)", name, i), fmt.Sprintf("Task under %s", parent.Title), types.TypeTask, cfg.MaxTaskAgeDays, cfg, rng)
		if err := store.CreateIssue(ctx, issue, "fixture"); err != nil {
			return nil, fmt.Errorf("failed to create task: %w", err)
		}
		if err := addFixtureParent(ctx, store, issue, parent, "task-feature"); err != nil {
			return nil, err
		}
		addFixtureLabels(ctx, store, issue, rng, 2)
		tasks = append(tasks, issue)
		progress.created++
		progress.note()
	}
	return tasks, nil
}

func newFixtureIssue(title, description string, issueType types.IssueType, maxAgeDays int, cfg DataConfig, rng *rand.Rand) *types.Issue {
	issue := &types.Issue{
		IssueContent: types.IssueContent{Title: title, Description: description},
		IssueWorkflow: types.IssueWorkflow{
			Status: randomStatus(rng, cfg.OpenRatio), Priority: randomPriority(rng),
			IssueType: issueType, Assignee: commonAssignees[rng.Intn(len(commonAssignees))],
		},
		IssueTimes: types.IssueTimes{CreatedAt: randomTime(rng, maxAgeDays), UpdatedAt: time.Now()},
	}
	if issue.Status == types.StatusClosed {
		closedAt := randomTime(rng, cfg.MaxClosedAgeDays)
		issue.ClosedAt = &closedAt
	}
	return issue
}

func addFixtureLabels(ctx context.Context, store *dolt.DoltStore, issue *types.Issue, rng *rand.Rand, maxLabels int) {
	for j := 0; j < rng.Intn(maxLabels)+1; j++ {
		label := commonLabels[rng.Intn(len(commonLabels))]
		_ = store.AddLabel(ctx, issue.ID, label, "fixture")
	}
}

func addFixtureParent(ctx context.Context, store *dolt.DoltStore, issue, parent *types.Issue, relation string) error {
	dep := &types.Dependency{IssueID: issue.ID, DependsOnID: parent.ID, Type: types.DepParentChild, CreatedAt: time.Now(), CreatedBy: "fixture"}
	if err := store.AddDependency(ctx, dep, "fixture"); err != nil {
		return fmt.Errorf("failed to add %s dependency: %w", relation, err)
	}
	return nil
}

func generateCrossLinks(ctx context.Context, store *dolt.DoltStore, rng *rand.Rand, tasks []*types.Issue, numTasks int, ratio float64) error {
	numCrossLinks := int(float64(numTasks) * ratio)
	for i := 0; i < numCrossLinks; i++ {
		fromTask := tasks[rng.Intn(len(tasks))]
		toTask := tasks[rng.Intn(len(tasks))]
		if fromTask.ID == toTask.ID {
			continue
		}
		dep := &types.Dependency{IssueID: fromTask.ID, DependsOnID: toTask.ID, Type: types.DepBlocks, CreatedAt: time.Now(), CreatedBy: "fixture"}
		// Ignore cycle errors for cross-links (they're expected).
		_ = store.AddDependency(ctx, dep, "fixture")
	}
	return nil
}

// generateFromJSONL creates issues, exports to JSONL, clears DB, and reimports
func generateFromJSONL(ctx context.Context, store *dolt.DoltStore, tempDir string, cfg DataConfig) error {
	// First generate issues normally
	if err := generateIssuesWithConfig(ctx, store, cfg); err != nil {
		return fmt.Errorf("failed to generate issues: %w", err)
	}

	// Export to JSONL
	jsonlPath := filepath.Join(tempDir, "issues.jsonl")
	if err := exportToJSONL(ctx, store, jsonlPath); err != nil {
		return fmt.Errorf("failed to export to JSONL: %w", err)
	}

	// Clear all issues (we'll reimport them)
	allIssues, err := store.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return fmt.Errorf("failed to get all issues: %w", err)
	}

	for _, issue := range allIssues {
		if err := store.DeleteIssue(ctx, issue.ID); err != nil {
			return fmt.Errorf("failed to delete issue %s: %w", issue.ID, err)
		}
	}

	// Import from JSONL
	if err := importFromJSONL(ctx, store, jsonlPath); err != nil {
		return fmt.Errorf("failed to import from JSONL: %w", err)
	}

	return nil
}

// exportToJSONL exports all issues to a JSONL file
func exportToJSONL(ctx context.Context, store *dolt.DoltStore, path string) error {
	// Get all issues
	allIssues, err := store.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return fmt.Errorf("failed to query issues: %w", err)
	}

	// Populate dependencies and labels for each issue
	allDeps, err := store.GetAllDependencyRecords(ctx)
	if err != nil {
		return fmt.Errorf("failed to get dependencies: %w", err)
	}

	for _, issue := range allIssues {
		issue.Dependencies = allDeps[issue.ID]

		labels, err := store.GetLabels(ctx, issue.ID)
		if err != nil {
			return fmt.Errorf("failed to get labels for %s: %w", issue.ID, err)
		}
		issue.Labels = labels
	}

	// Write to JSONL file
	// #nosec G304 -- fixture exports to deterministic file controlled by tests
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create JSONL file: %w", err)
	}
	defer f.Close()

	encoder := json.NewEncoder(f)
	for _, issue := range allIssues {
		if err := encoder.Encode(issue); err != nil {
			return fmt.Errorf("failed to encode issue: %w", err)
		}
	}

	return nil
}

// importFromJSONL imports issues from a JSONL file
func importFromJSONL(ctx context.Context, store *dolt.DoltStore, path string) error {
	data, err := readFixtureJSONL(path)
	if err != nil {
		return err
	}
	issues, err := parseFixtureIssues(data)
	if err != nil {
		return err
	}
	metadata, err := createFixtureIssues(ctx, store, issues)
	if err != nil {
		return err
	}
	return restoreFixtureRelations(ctx, store, metadata)
}

func readFixtureJSONL(path string) ([]byte, error) {
	// #nosec G304 -- fixture imports from deterministic file created earlier in test
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read JSONL file: %w", err)
	}
	return data, nil
}

func parseFixtureIssues(data []byte) ([]*types.Issue, error) {
	var issues []*types.Issue
	for i, line := range splitLines(string(data)) {
		if len(line) == 0 {
			continue
		}
		var issue types.Issue
		if err := json.Unmarshal([]byte(line), &issue); err != nil {
			return nil, fmt.Errorf("failed to parse issue at line %d: %w", i+1, err)
		}
		issues = append(issues, &issue)
	}
	return issues, nil
}

type savedFixtureMetadata struct {
	deps   []*types.Dependency
	labels []string
}

func createFixtureIssues(ctx context.Context, store *dolt.DoltStore, issues []*types.Issue) (map[string]savedFixtureMetadata, error) {
	metadata := make(map[string]savedFixtureMetadata)
	for _, issue := range issues {
		metadata[issue.ID] = savedFixtureMetadata{deps: issue.Dependencies, labels: issue.Labels}
		issue.Dependencies = nil
		issue.Labels = nil
		if err := store.CreateIssue(ctx, issue, "fixture"); err != nil {
			return nil, fmt.Errorf("failed to create issue %s: %w", issue.ID, err)
		}
	}
	return metadata, nil
}

func restoreFixtureRelations(ctx context.Context, store *dolt.DoltStore, metadata map[string]savedFixtureMetadata) error {
	for issueID, meta := range metadata {
		if err := restoreFixtureDependencies(ctx, store, issueID, meta.deps); err != nil {
			return err
		}
		for _, label := range meta.labels {
			_ = store.AddLabel(ctx, issueID, label, "fixture")
		}
	}
	return nil
}

func restoreFixtureDependencies(ctx context.Context, store *dolt.DoltStore, issueID string, deps []*types.Dependency) error {
	for _, dep := range deps {
		if err := store.AddDependency(ctx, dep, "fixture"); err != nil && !isIgnorableFixtureDependencyError(err) {
			return fmt.Errorf("failed to add dependency for %s: %w", issueID, err)
		}
	}
	return nil
}

func isIgnorableFixtureDependencyError(err error) bool {
	return strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "cycle")
}

// splitLines splits a string by newlines
func splitLines(s string) []string {
	var lines []string
	start := 0
	n := len(s)
	for i := 0; i < n; i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// randomStatus returns a random status with given open ratio
func randomStatus(rng *rand.Rand, openRatio float64) types.Status {
	r := rng.Float64()
	if r < openRatio {
		// Open statuses: open, in_progress, blocked
		statuses := []types.Status{types.StatusOpen, types.StatusInProgress, types.StatusBlocked}
		return statuses[rng.Intn(len(statuses))]
	}
	return types.StatusClosed
}

// randomPriority returns a random priority with realistic distribution
// P0: 5%, P1: 15%, P2: 50%, P3: 25%, P4: 5%
func randomPriority(rng *rand.Rand) int {
	r := rng.Intn(100)
	switch {
	case r < 5:
		return 0
	case r < 20:
		return 1
	case r < 70:
		return 2
	case r < 95:
		return 3
	default:
		return 4
	}
}

// randomTime returns a random time up to maxDaysAgo days in the past
func randomTime(rng *rand.Rand, maxDaysAgo int) time.Time {
	daysAgo := rng.Intn(maxDaysAgo)
	return time.Now().Add(-time.Duration(daysAgo) * 24 * time.Hour)
}
