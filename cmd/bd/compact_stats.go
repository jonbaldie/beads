package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jonbaldie/beads/internal/compact"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

func runCompactStats(ctx context.Context, store storage.DoltStorage) error {
	tier1, err := store.GetTier1Candidates(ctx)
	if err != nil {
		return HandleError("failed to get Tier 1 candidates: %v", err)
	}

	tier2, err := store.GetTier2Candidates(ctx)
	if err != nil {
		return HandleError("failed to get Tier 2 candidates: %v", err)
	}

	tier1Size := 0
	for _, c := range tier1 {
		tier1Size += c.OriginalSize
	}

	tier2Size := 0
	for _, c := range tier2 {
		tier2Size += c.OriginalSize
	}

	if isJSONOutput() {
		output := map[string]interface{}{
			"tier1": map[string]interface{}{
				"candidates": len(tier1),
				"total_size": tier1Size,
			},
			"tier2": map[string]interface{}{
				"candidates":  len(tier2),
				"total_size":  tier2Size,
				"implemented": false,
			},
		}
		if err := outputJSON(output); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return nil
	}

	fmt.Println("Compaction Statistics")
	fmt.Printf("Tier 1 (30+ days closed):\n")
	fmt.Printf("  Candidates: %d\n", len(tier1))
	fmt.Printf("  Total size: %d bytes\n", tier1Size)
	if tier1Size > 0 {
		fmt.Printf("  Estimated savings: %d bytes (70%%)\n\n", tier1Size*7/10)
	}

	fmt.Printf("Tier 2 (90+ days closed, Tier 1 compacted): not yet implemented\n")
	fmt.Printf("  Candidates: %d\n", len(tier2))
	fmt.Printf("  Total size: %d bytes\n", tier2Size)
	return nil
}

func runCompactAnalyze(ctx context.Context, store storage.DoltStorage, opts compactOptions) error {
	candidates, err := buildCompactAnalysisCandidates(ctx, store, opts)
	if err != nil {
		return err
	}
	return renderCompactAnalysis(candidates, opts.target.tier)
}

type compactAnalysisCandidate struct {
	ID                 string `json:"id"`
	Title              string `json:"title"`
	Description        string `json:"description"`
	Design             string `json:"design"`
	Notes              string `json:"notes"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
	SizeBytes          int    `json:"size_bytes"`
	AgeDays            int    `json:"age_days"`
	Tier               int    `json:"tier"`
	Compacted          bool   `json:"compacted"`
}

func buildCompactAnalysisCandidates(ctx context.Context, store storage.DoltStorage, opts compactOptions) ([]compactAnalysisCandidate, error) {
	if opts.target.id != "" {
		return buildSingleCompactAnalysisCandidate(ctx, store, opts)
	}
	return buildTierCompactAnalysisCandidates(ctx, store, opts)
}

func buildSingleCompactAnalysisCandidate(ctx context.Context, store storage.DoltStorage, opts compactOptions) ([]compactAnalysisCandidate, error) {
	issue, err := store.GetIssue(ctx, opts.target.id)
	if err != nil {
		return nil, HandleError("failed to get issue: %v", err)
	}
	ageDays := 0
	if issue.ClosedAt != nil {
		ageDays = int(time.Since(*issue.ClosedAt).Hours() / 24)
	}
	return []compactAnalysisCandidate{newCompactAnalysisCandidate(issue, compactIssueContentSize(issue), ageDays, opts.target.tier)}, nil
}

func buildTierCompactAnalysisCandidates(ctx context.Context, store storage.DoltStorage, opts compactOptions) ([]compactAnalysisCandidate, error) {
	tierCandidates, err := getCompactAnalysisTierCandidates(ctx, store, opts.target.tier)
	if err != nil {
		return nil, err
	}
	if opts.workflow.limit > 0 && len(tierCandidates) > opts.workflow.limit {
		tierCandidates = tierCandidates[:opts.workflow.limit]
	}
	var candidates []compactAnalysisCandidate
	for _, candidate := range tierCandidates {
		issue, err := store.GetIssue(ctx, candidate.IssueID)
		if err != nil {
			continue
		}
		ageDays := int(time.Since(candidate.ClosedAt).Hours() / 24)
		candidates = append(candidates, newCompactAnalysisCandidate(issue, candidate.OriginalSize, ageDays, opts.target.tier))
	}
	return candidates, nil
}

func getCompactAnalysisTierCandidates(ctx context.Context, store storage.DoltStorage, tier int) ([]*types.CompactionCandidate, error) {
	if tier == 1 {
		candidates, err := store.GetTier1Candidates(ctx)
		if err != nil {
			return nil, HandleError("failed to get candidates: %v", err)
		}
		return candidates, nil
	}
	candidates, err := store.GetTier2Candidates(ctx)
	if err != nil {
		return nil, HandleError("failed to get candidates: %v", err)
	}
	return candidates, nil
}

func newCompactAnalysisCandidate(issue *types.Issue, sizeBytes, ageDays, tier int) compactAnalysisCandidate {
	return compactAnalysisCandidate{
		ID:                 issue.ID,
		Title:              issue.Title,
		Description:        issue.Description,
		Design:             issue.Design,
		Notes:              issue.Notes,
		AcceptanceCriteria: issue.AcceptanceCriteria,
		SizeBytes:          sizeBytes,
		AgeDays:            ageDays,
		Tier:               tier,
		Compacted:          issue.CompactionLevel > 0,
	}
}

func compactIssueContentSize(issue *types.Issue) int {
	return len(issue.Description) + len(issue.Design) + len(issue.Notes) + len(issue.AcceptanceCriteria)
}

func renderCompactAnalysis(candidates []compactAnalysisCandidate, tier int) error {
	if isJSONOutput() {
		totalSize := 0
		for _, c := range candidates {
			totalSize += c.SizeBytes
		}
		output := map[string]interface{}{
			"candidates": candidates,
			"summary": map[string]interface{}{
				"total_candidates":    len(candidates),
				"total_content_bytes": totalSize,
			},
		}
		if err := outputJSON(output); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return nil
	}

	// Human-readable output
	fmt.Printf("Compaction Candidates (Tier %d)\n\n", tier)
	fmt.Printf("  %-12s %-40s %8s %10s\n", "ID", "TITLE", "AGE", "SIZE")
	totalSize := 0
	for _, c := range candidates {
		compactStatus := ""
		if c.Compacted {
			compactStatus = " *"
		}
		title := c.Title
		if len(title) > 40 {
			title = title[:37] + "..."
		}
		fmt.Printf("  %-12s %-40s %5dd %10d B%s\n", c.ID, title, c.AgeDays, c.SizeBytes, compactStatus)
		totalSize += c.SizeBytes
	}
	fmt.Printf("\nSummary: %d candidates, %d bytes total content\n", len(candidates), totalSize)
	return nil
}

func runCompactApply(ctx context.Context, store storage.DoltStorage, opts compactOptions) error {
	start := time.Now()
	summary, err := readCompactSummary(opts.workflow.summary)
	if err != nil {
		return err
	}
	issue, err := store.GetIssue(ctx, opts.target.id)
	if err != nil {
		return HandleError("failed to get issue: %v", err)
	}
	originalSize := compactIssueContentSize(issue)
	compactedSize := len(summary)
	if err := validateCompactApply(ctx, store, originalSize, compactedSize, opts); err != nil {
		return err
	}
	actor := compactApplyActor(opts.workflow.actor)
	if err := applyCompactSummary(ctx, store, summary, actor, originalSize, compactedSize, opts.target); err != nil {
		return err
	}
	return renderCompactApplyResult(originalSize, compactedSize, time.Since(start), opts.target)
}

func readCompactSummary(summaryPath string) (string, error) {
	var summaryBytes []byte
	var err error
	if summaryPath == "-" {
		summaryBytes, err = io.ReadAll(os.Stdin)
		if err != nil {
			return "", HandleError("failed to read summary from stdin: %v", err)
		}
	} else {
		// #nosec G304 -- summary file path provided explicitly by operator
		summaryBytes, err = os.ReadFile(summaryPath)
		if err != nil {
			return "", HandleError("failed to read summary file: %v", err)
		}
	}
	return string(summaryBytes), nil
}

func validateCompactApply(ctx context.Context, store storage.DoltStorage, originalSize, compactedSize int, opts compactOptions) error {
	if opts.target.force {
		return nil
	}
	eligible, reason, err := store.CheckEligibility(ctx, opts.target.id, opts.target.tier)
	if err != nil {
		return HandleError("failed to check eligibility: %v", err)
	}
	if !eligible {
		return HandleErrorWithHint(fmt.Sprintf("%s is not eligible for Tier %d compaction: %s", opts.target.id, opts.target.tier, reason), "use --force to bypass eligibility checks")
	}
	if compactedSize >= originalSize {
		return HandleErrorWithHint(fmt.Sprintf("summary (%d bytes) is not shorter than original (%d bytes)", compactedSize, originalSize), "use --force to bypass size validation")
	}
	return nil
}

func compactApplyActor(actor string) string {
	if actor == "" {
		return "agent"
	}
	return actor
}

func applyCompactSummary(ctx context.Context, store storage.DoltStorage, summary, actor string, originalSize, compactedSize int, target compactTargetOptions) error {
	updates := map[string]interface{}{
		"description":         summary,
		"design":              "",
		"notes":               "",
		"acceptance_criteria": "",
	}
	if err := store.UpdateIssue(ctx, target.id, updates, actor); err != nil {
		return HandleError("failed to update issue: %v", err)
	}
	commitHash := compact.GetCurrentCommitHash()
	if err := store.ApplyCompaction(ctx, target.id, target.tier, originalSize, compactedSize, commitHash); err != nil {
		return HandleError("failed to apply compaction: %v", err)
	}
	savingBytes := originalSize - compactedSize
	reductionPct := float64(savingBytes) / float64(originalSize) * 100
	eventData := fmt.Sprintf("Tier %d compaction: %d → %d bytes (saved %d, %.1f%%)", target.tier, originalSize, compactedSize, savingBytes, reductionPct)
	if err := store.AddComment(ctx, target.id, actor, eventData); err != nil {
		return HandleError("failed to record event: %v", err)
	}
	return nil
}

func renderCompactApplyResult(originalSize, compactedSize int, elapsed time.Duration, target compactTargetOptions) error {
	savingBytes := originalSize - compactedSize
	reductionPct := float64(savingBytes) / float64(originalSize) * 100
	if isJSONOutput() {
		output := map[string]interface{}{
			"success":        true,
			"issue_id":       target.id,
			"tier":           target.tier,
			"original_size":  originalSize,
			"compacted_size": compactedSize,
			"saved_bytes":    savingBytes,
			"reduction_pct":  reductionPct,
			"elapsed_ms":     elapsed.Milliseconds(),
		}
		if err := outputJSON(output); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return nil
	}
	fmt.Printf("✓ Compacted %s (Tier %d)\n", target.id, target.tier)
	fmt.Printf("  %d → %d bytes (saved %d, %.1f%%)\n", originalSize, compactedSize, savingBytes, reductionPct)
	fmt.Printf("  Time: %v\n", elapsed)
	return nil
}
