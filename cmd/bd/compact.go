package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jonbaldie/beads/internal/compact"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/spf13/cobra"
)

type compactOptions struct {
	target    compactTargetOptions
	execution compactExecutionOptions
	modes     compactModeOptions
	workflow  compactWorkflowOptions
}

type compactTargetOptions struct {
	tier  int
	all   bool
	id    string
	force bool
}

type compactExecutionOptions struct {
	dryRun  bool
	workers int
}

type compactModeOptions struct {
	stats   bool
	analyze bool
	apply   bool
	auto    bool
	dolt    bool
}

type compactWorkflowOptions struct {
	summary string
	actor   string
	limit   int
}

func compactOptionsFromCommand(cmd *cobra.Command) compactOptions {
	return compactOptions{
		target: compactTargetOptions{
			tier:  compactFlagInt(cmd, "tier"),
			all:   compactFlagBool(cmd, "all"),
			id:    compactFlagString(cmd, "id"),
			force: compactFlagBool(cmd, "force"),
		},
		execution: compactExecutionOptions{
			dryRun:  compactFlagBool(cmd, "dry-run"),
			workers: compactFlagInt(cmd, "workers"),
		},
		modes: compactModeOptions{
			stats:   compactFlagBool(cmd, "stats"),
			analyze: compactFlagBool(cmd, "analyze"),
			apply:   compactFlagBool(cmd, "apply"),
			auto:    compactFlagBool(cmd, "auto"),
			dolt:    compactFlagBool(cmd, "dolt"),
		},
		workflow: compactWorkflowOptions{
			summary: compactFlagString(cmd, "summary"),
			actor:   compactFlagString(cmd, "actor"),
			limit:   compactFlagInt(cmd, "limit"),
		},
	}
}

func compactFlagBool(cmd *cobra.Command, name string) bool {
	value, _ := cmd.Flags().GetBool(name)
	return value
}

func compactFlagInt(cmd *cobra.Command, name string) int {
	value, _ := cmd.Flags().GetInt(name)
	return value
}

func compactFlagString(cmd *cobra.Command, name string) string {
	value, _ := cmd.Flags().GetString(name)
	return value
}

var compactCmd = &cobra.Command{
	Use:           "compact",
	Short:         "Compact old closed issues to save space",
	SilenceUsage:  true,
	SilenceErrors: true,
	Long: `Compact old closed issues using semantic summarization.

Compaction reduces database size by summarizing closed issues that are no longer
actively referenced. This is permanent graceful decay - original content is discarded.

Modes:
  - Analyze: Export candidates for agent review (no API key needed)
  - Apply: Accept agent-provided summary (no API key needed)
  - Auto: AI-powered compaction (requires ANTHROPIC_API_KEY, MINIMAX_API_KEY, or ai.api_key)
  - Dolt: Run Dolt garbage collection (for Dolt-backend repositories)

Tiers:
  - Tier 1: Semantic compression (30 days closed, 70% reduction)
  - Tier 2: Ultra compression (90 days closed) - planned, not yet implemented

Dolt Garbage Collection:
  With auto-commit per mutation, Dolt commit history grows over time. Use
  --dolt to run Dolt garbage collection and reclaim disk space.

  --dolt: Run Dolt GC on .beads/dolt directory to free disk space.
          This removes unreachable commits and compacts storage.

Examples:
  # Dolt garbage collection
  bd compact --dolt                        # Run Dolt GC
  bd compact --dolt --dry-run              # Preview without running GC

  # Agent-driven workflow (recommended)
  bd compact --analyze --json              # Get candidates with full content
  bd compact --apply --id bd-42 --summary summary.txt
  bd compact --apply --id bd-42 --summary - < summary.txt

  # AI-powered workflow
  bd compact --auto --dry-run              # Preview candidates
  bd compact --auto --all                  # Compact all eligible issues
  bd compact --auto --id bd-42             # Compact specific issue
  MINIMAX_API_KEY=... bd compact --auto --all

  # Statistics
  bd compact --stats                       # Show statistics
`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runCompactCommand(getRootContext(), getStore(), compactOptionsFromCommand(cmd))
	},
}

func runCompactCommand(ctx context.Context, store storage.DoltStorage, opts compactOptions) error {
	if usesProxiedServer() {
		return runCompactProxied(ctx, opts)
	}
	if err := validateCompactAccess(opts); err != nil {
		return err
	}
	return runCompactDirect(ctx, store, opts)
}

func runCompactProxied(ctx context.Context, opts compactOptions) error {
	if opts.modes.dolt {
		return runCompactDoltProxiedServer(ctx, opts.execution.dryRun)
	}
	return HandleErrorRespectJSON("only 'compact --dolt' is supported in proxied-server mode")
}

func validateCompactAccess(opts compactOptions) error {
	// Block mutating operations in embedded mode; allow --stats, --analyze, --dry-run read-only paths.
	if compactNeedsServerMode(opts) {
		if err := requireServerMode("compact"); err != nil {
			return HandleError("%v", err)
		}
	}
	// Compact modifies data unless --stats or --analyze or --dry-run or --dolt with --dry-run
	if compactNeedsReadonlyCheck(opts) {
		if err := CheckReadonly("compact"); err != nil {
			return err
		}
	}
	return nil
}

func compactNeedsServerMode(opts compactOptions) bool {
	return !opts.modes.stats && !opts.modes.analyze && !opts.execution.dryRun
}

func compactNeedsReadonlyCheck(opts compactOptions) bool {
	return !opts.modes.stats && !opts.modes.analyze && !opts.execution.dryRun && !(opts.modes.dolt && opts.execution.dryRun)
}

func runCompactDirect(ctx context.Context, store storage.DoltStorage, opts compactOptions) error {
	if opts.modes.stats {
		return runCompactStats(ctx, store)
	}
	if opts.modes.dolt {
		return runCompactDolt(opts.execution.dryRun)
	}

	activeModes := countCompactModes(opts.modes)
	if err := validateCompactModes(activeModes); err != nil {
		return err
	}
	if err := validateCompactTier(opts.target.tier); err != nil {
		return err
	}
	return runCompactMode(ctx, store, opts)
}

func validateCompactTier(tier int) error {
	if tier != 1 {
		return HandleError("Tier %d compaction is not yet implemented; only --tier 1 is available", tier)
	}
	return nil
}

func runCompactMode(ctx context.Context, store storage.DoltStorage, opts compactOptions) error {
	if opts.modes.analyze {
		return runCompactAnalyzeMode(ctx, store, opts)
	}
	if opts.modes.apply {
		return runCompactApplyMode(ctx, store, opts)
	}
	return runCompactAuto(ctx, store, opts)
}

func runCompactAnalyzeMode(ctx context.Context, store storage.DoltStorage, opts compactOptions) error {
	if err := ensureDirectMode("compact --analyze requires direct database access"); err != nil {
		return HandleErrorWithHint(err.Error(), diagHint())
	}
	return runCompactAnalyze(ctx, store, opts)
}

func runCompactApplyMode(ctx context.Context, store storage.DoltStorage, opts compactOptions) error {
	if err := ensureDirectMode("compact --apply requires direct database access"); err != nil {
		return HandleErrorWithHint(err.Error(), diagHint())
	}
	if opts.target.id == "" {
		return HandleError("--apply requires --id")
	}
	if opts.workflow.summary == "" {
		return HandleError("--apply requires --summary")
	}
	return runCompactApply(ctx, store, opts)
}

func countCompactModes(modes compactModeOptions) int {
	activeModes := 0
	if modes.analyze {
		activeModes++
	}
	if modes.apply {
		activeModes++
	}
	if modes.auto {
		activeModes++
	}
	return activeModes
}

func validateCompactModes(activeModes int) error {
	if activeModes == 0 {
		return HandleError("must specify one mode: --analyze, --apply, or --auto")
	}
	if activeModes > 1 {
		return HandleError("cannot use multiple modes together (--analyze, --apply, --auto are mutually exclusive)")
	}
	return nil
}

func runCompactAuto(ctx context.Context, store storage.DoltStorage, opts compactOptions) error {
	if err := validateCompactAutoOptions(opts); err != nil {
		return err
	}
	apiKey, _ := config.ResolveAIAPIKey("")
	if apiKey == "" && !opts.execution.dryRun {
		return HandleError("--auto mode requires ANTHROPIC_API_KEY, MINIMAX_API_KEY, or ai.api_key in config")
	}

	compactCfg := &compact.Config{
		APIKey:      apiKey,
		Concurrency: opts.execution.workers,
		DryRun:      opts.execution.dryRun,
	}
	compactor, err := compact.New(store, apiKey, compactCfg)
	if err != nil {
		return HandleError("failed to create compactor: %v", err)
	}
	if opts.target.id != "" {
		return runCompactSingle(ctx, compactor, store, opts.target.id, opts)
	}
	return runCompactAll(ctx, compactor, store, opts)
}

func validateCompactAutoOptions(opts compactOptions) error {
	if opts.target.id != "" && opts.target.all {
		return HandleError("cannot use --id and --all together")
	}
	if opts.target.force && opts.target.id == "" {
		return HandleError("--force requires --id")
	}
	if opts.target.id == "" && !opts.target.all && !opts.execution.dryRun {
		return HandleError("must specify --all, --id, or --dry-run")
	}
	return nil
}

func runCompactSingle(ctx context.Context, compactor *compact.Compactor, store storage.DoltStorage, issueID string, opts compactOptions) error {
	start := time.Now()
	if err := ensureCompactSingleEligibility(ctx, store, issueID, opts); err != nil {
		return err
	}

	issue, err := store.GetIssue(ctx, issueID)
	if err != nil {
		return HandleError("failed to get issue: %v", err)
	}
	originalSize := len(issue.Description) + len(issue.Design) + len(issue.Notes) + len(issue.AcceptanceCriteria)
	if opts.execution.dryRun {
		return renderCompactSingleDryRun(issue, issueID, originalSize, opts.target.tier)
	}

	if err := compactSingleIssue(ctx, compactor, issueID, opts.target.tier); err != nil {
		return err
	}
	issue, err = store.GetIssue(ctx, issueID)
	if err != nil {
		return HandleError("failed to get updated issue: %v", err)
	}
	compactedSize := len(issue.Description)
	return renderCompactSingleResult(issueID, originalSize, compactedSize, time.Since(start), opts.target.tier)
}

func ensureCompactSingleEligibility(ctx context.Context, store storage.DoltStorage, issueID string, opts compactOptions) error {
	if opts.target.force {
		return nil
	}
	eligible, reason, err := store.CheckEligibility(ctx, issueID, opts.target.tier)
	if err != nil {
		return HandleError("failed to check eligibility: %v", err)
	}
	if !eligible {
		return HandleError("%s is not eligible for Tier %d compaction: %s", issueID, opts.target.tier, reason)
	}
	return nil
}

func renderCompactSingleDryRun(issue *types.Issue, issueID string, originalSize, tier int) error {
	ageDays := 0
	var closedAtStr string
	if issue.ClosedAt != nil {
		ageDays = int(time.Since(*issue.ClosedAt).Hours() / 24)
		closedAtStr = issue.ClosedAt.Format(time.RFC3339)
	}
	candidate := map[string]interface{}{
		"id":           issueID,
		"title":        issue.Title,
		"closed_at":    closedAtStr,
		"age_days":     ageDays,
		"content_size": originalSize,
	}
	if isJSONOutput() {
		output := map[string]interface{}{
			"dry_run":    true,
			"tier":       tier,
			"candidates": []interface{}{candidate},
			"summary": map[string]interface{}{
				"total_candidates":    1,
				"total_content_bytes": originalSize,
			},
		}
		if err := outputJSON(output); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return nil
	}
	fmt.Printf("DRY RUN - Tier %d compaction\n\n", tier)
	fmt.Printf("  %-12s %-40s %8s %10s\n", "ID", "TITLE", "AGE", "SIZE")
	title := issue.Title
	if len(title) > 40 {
		title = title[:37] + "..."
	}
	fmt.Printf("  %-12s %-40s %5dd %10d B\n", issueID, title, ageDays, originalSize)
	fmt.Printf("\nSummary: 1 candidate, %d bytes total content\n", originalSize)
	return nil
}

func compactSingleIssue(ctx context.Context, compactor *compact.Compactor, issueID string, tier int) error {
	if tier != 1 {
		return HandleError("Tier 2 compaction not yet implemented")
	}
	if err := compactor.CompactTier1(ctx, issueID); err != nil {
		return HandleError("%v", err)
	}
	return nil
}

func renderCompactSingleResult(issueID string, originalSize, compactedSize int, elapsed time.Duration, tier int) error {
	savingBytes := originalSize - compactedSize
	if isJSONOutput() {
		output := map[string]interface{}{
			"success":        true,
			"tier":           tier,
			"issue_id":       issueID,
			"original_size":  originalSize,
			"compacted_size": compactedSize,
			"saved_bytes":    savingBytes,
			"reduction_pct":  float64(savingBytes) / float64(originalSize) * 100,
			"elapsed_ms":     elapsed.Milliseconds(),
		}
		if err := outputJSON(output); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return nil
	}
	fmt.Printf("✓ Compacted %s (Tier %d)\n", issueID, tier)
	fmt.Printf("  %d → %d bytes (saved %d, %.1f%%)\n",
		originalSize, compactedSize, savingBytes,
		float64(savingBytes)/float64(originalSize)*100)
	fmt.Printf("  Time: %v\n", elapsed)
	return nil
}

func runCompactAll(ctx context.Context, compactor *compact.Compactor, store storage.DoltStorage, opts compactOptions) error {
	start := time.Now()
	candidates, err := compactCandidateIDs(ctx, store, opts.target.tier)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return renderNoCompactCandidates()
	}
	if opts.execution.dryRun {
		dryCandidates, totalSize := buildCompactDryRunCandidates(ctx, store, candidates)
		return renderCompactAllDryRun(dryCandidates, totalSize, opts.target.tier)
	}
	if !isJSONOutput() {
		fmt.Printf("Compacting %d issues (Tier %d)...\n\n", len(candidates), opts.target.tier)
	}
	results, err := compactor.CompactTier1Batch(ctx, candidates)
	if err != nil {
		return HandleError("batch compaction failed: %v", err)
	}
	successCount, failCount, totalSaved, totalOriginal := summarizeCompactResults(results)
	return renderCompactAllResults(results, successCount, failCount, totalSaved, totalOriginal, time.Since(start), opts.target.tier)
}

func compactCandidateIDs(ctx context.Context, store storage.DoltStorage, tier int) ([]string, error) {
	if tier == 1 {
		tier1, err := store.GetTier1Candidates(ctx)
		if err != nil {
			return nil, HandleError("failed to get candidates: %v", err)
		}
		candidates := make([]string, 0, len(tier1))
		for _, c := range tier1 {
			candidates = append(candidates, c.IssueID)
		}
		return candidates, nil
	}
	tier2, err := store.GetTier2Candidates(ctx)
	if err != nil {
		return nil, HandleError("failed to get candidates: %v", err)
	}
	candidates := make([]string, 0, len(tier2))
	for _, c := range tier2 {
		candidates = append(candidates, c.IssueID)
	}
	return candidates, nil
}

func renderNoCompactCandidates() error {
	if isJSONOutput() {
		if err := outputJSON(map[string]interface{}{
			"success": true,
			"count":   0,
			"message": "No eligible candidates",
		}); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return nil
	}
	fmt.Println("No eligible candidates for compaction")
	return nil
}

type compactDryRunCandidate struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	ClosedAt    string `json:"closed_at"`
	AgeDays     int    `json:"age_days"`
	ContentSize int    `json:"content_size"`
}

func buildCompactDryRunCandidates(ctx context.Context, store storage.DoltStorage, candidates []string) ([]compactDryRunCandidate, int) {
	var dryCandidates []compactDryRunCandidate
	totalSize := 0
	for _, id := range candidates {
		issue, err := store.GetIssue(ctx, id)
		if err != nil {
			continue
		}
		contentSize := len(issue.Description) + len(issue.Design) + len(issue.Notes) + len(issue.AcceptanceCriteria)
		totalSize += contentSize
		ageDays := 0
		var closedAtStr string
		if issue.ClosedAt != nil {
			ageDays = int(time.Since(*issue.ClosedAt).Hours() / 24)
			closedAtStr = issue.ClosedAt.Format(time.RFC3339)
		}
		dryCandidates = append(dryCandidates, compactDryRunCandidate{
			ID:          issue.ID,
			Title:       issue.Title,
			ClosedAt:    closedAtStr,
			AgeDays:     ageDays,
			ContentSize: contentSize,
		})
	}
	return dryCandidates, totalSize
}

func renderCompactAllDryRun(dryCandidates []compactDryRunCandidate, totalSize, tier int) error {
	if isJSONOutput() {
		output := map[string]interface{}{
			"dry_run":    true,
			"tier":       tier,
			"candidates": dryCandidates,
			"summary": map[string]interface{}{
				"total_candidates":    len(dryCandidates),
				"total_content_bytes": totalSize,
			},
		}
		if err := outputJSON(output); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return nil
	}
	fmt.Printf("DRY RUN - Tier %d compaction\n\n", tier)
	fmt.Printf("  %-12s %-40s %8s %10s\n", "ID", "TITLE", "AGE", "SIZE")
	for _, c := range dryCandidates {
		title := c.Title
		if len(title) > 40 {
			title = title[:37] + "..."
		}
		fmt.Printf("  %-12s %-40s %5dd %10d B\n", c.ID, title, c.AgeDays, c.ContentSize)
	}
	fmt.Printf("\nSummary: %d candidates, %d bytes total content\n", len(dryCandidates), totalSize)
	return nil
}

func summarizeCompactResults(results []compact.BatchResult) (int, int, int, int) {
	successCount := 0
	failCount := 0
	totalSaved := 0
	totalOriginal := 0
	for i, result := range results {
		if !isJSONOutput() {
			fmt.Printf("[%s] %d/%d\r", progressBar(i+1, len(results)), i+1, len(results))
		}
		if result.Err != nil {
			failCount++
			continue
		}
		successCount++
		totalOriginal += result.OriginalSize
		totalSaved += result.OriginalSize - result.CompactedSize
	}
	return successCount, failCount, totalSaved, totalOriginal
}

func renderCompactAllResults(results []compact.BatchResult, successCount, failCount, totalSaved, totalOriginal int, elapsed time.Duration, tier int) error {
	if isJSONOutput() {
		output := map[string]interface{}{
			"success":       true,
			"tier":          tier,
			"total":         len(results),
			"succeeded":     successCount,
			"failed":        failCount,
			"saved_bytes":   totalSaved,
			"original_size": totalOriginal,
			"elapsed_ms":    elapsed.Milliseconds(),
		}
		if err := outputJSON(output); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return nil
	}
	fmt.Printf("\n\nCompleted in %v\n\n", elapsed)
	fmt.Printf("Summary:\n")
	fmt.Printf("  Succeeded: %d\n", successCount)
	fmt.Printf("  Failed: %d\n", failCount)
	if totalOriginal > 0 {
		fmt.Printf("  Saved: %d bytes (%.1f%%)\n", totalSaved, float64(totalSaved)/float64(totalOriginal)*100)
	}
	return nil
}
