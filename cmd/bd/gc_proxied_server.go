package main

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
	"github.com/jonbaldie/beads/internal/types"
)

func runGCProxiedServer(ctx context.Context, opts gcOptions) error {
	evt := metrics.NewCommandEvent("gc")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	if !opts.dryRun {
		if err := CheckReadonly("gc"); err != nil {
			return err
		}
	}
	if getUOWProvider() == nil {
		return HandleErrorRespectJSON("proxied-server UOW provider not initialized")
	}
	if opts.olderThan < 0 {
		return HandleErrorRespectJSON("--older-than must be non-negative")
	}

	start := time.Now()
	results, gcFailed, err := runProxiedGCPhases(ctx, opts)
	if err != nil {
		return err
	}
	return reportProxiedGC(results, opts, gcFailed, time.Since(start))
}

func runProxiedGCPhases(ctx context.Context, opts gcOptions) ([]gcPhaseResult, bool, error) {
	var results []gcPhaseResult
	if opts.skipDecay {
		results = append(results, gcPhaseResult{name: "Decay", skipped: true})
	} else {
		decay, err := runProxiedGCDecay(ctx, opts)
		if err != nil {
			return nil, false, err
		}
		results = append(results, decay)
	}

	results = append(results, runProxiedGCCompact(ctx, opts))
	if opts.skipDolt {
		return append(results, gcPhaseResult{name: "Dolt GC", skipped: true}), false, nil
	}

	doltGC, failed := runProxiedDoltGC(ctx, opts)
	return append(results, doltGC), failed, nil
}

func runProxiedGCDecay(ctx context.Context, opts gcOptions) (gcPhaseResult, error) {
	if !isJSONOutput() {
		fmt.Println("Phase 1/3: Decay (delete old closed issues)")
	}

	cutoffDays := opts.olderThan
	cutoffTime := time.Now().UTC().AddDate(0, 0, -cutoffDays)
	closedIssues, err := loadProxiedGCClosedIssues(ctx, cutoffTime)
	if err != nil {
		return gcPhaseResult{}, HandleErrorRespectJSON("searching closed issues: %v", err)
	}

	var stats closedDeletionCandidateStats
	closedIssues, stats = filterClosedDeletionCandidates(closedIssues, &cutoffTime)
	warnClosedDeletionSafetySkips(stats)
	result, err := resolveProxiedDecay(ctx, closedIssues, cutoffDays, opts)
	if !isJSONOutput() {
		fmt.Println()
	}
	return result, err
}

func loadProxiedGCClosedIssues(ctx context.Context, cutoffTime time.Time) ([]*types.Issue, error) {
	statusClosed := types.StatusClosed
	filter := types.IssueFilter{
		IssueFilterCore: types.IssueFilterCore{
			Status: &statusClosed,
		},
		IssueFilterMatch: types.IssueFilterMatch{
			ClosedBefore: &cutoffTime,
		},
	}
	return uow.RunTxRead(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) ([]*types.Issue, error) {
		page, err := uw.IssueUseCase().SearchIssues(ctx, "", filter)
		if err != nil {
			return nil, err
		}
		return page.Items, nil
	})
}

func resolveProxiedDecay(ctx context.Context, closedIssues []*types.Issue, cutoffDays int, opts gcOptions) (gcPhaseResult, error) {
	switch {
	case len(closedIssues) == 0:
		if !isJSONOutput() {
			fmt.Printf("  No closed issues older than %d days\n", cutoffDays)
		}
		return gcPhaseResult{name: "Decay", detail: "0 issues deleted"}, nil
	case opts.dryRun:
		if !isJSONOutput() {
			fmt.Printf("  Would delete %d closed issue(s)\n", len(closedIssues))
		}
		return gcPhaseResult{name: "Decay", detail: fmt.Sprintf("%d issues (dry-run)", len(closedIssues))}, nil
	case !opts.force:
		return gcPhaseResult{}, HandleErrorWithHintRespectJSON(
			fmt.Sprintf("would delete %d closed issue(s) older than %d days", len(closedIssues), cutoffDays),
			"Use --force to confirm or --dry-run to preview.")
	default:
		return deleteProxiedClosedIssues(ctx, closedIssues)
	}
}

func deleteProxiedClosedIssues(ctx context.Context, closedIssues []*types.Issue) (gcPhaseResult, error) {
	ids := make([]string, len(closedIssues))
	for i, issue := range closedIssues {
		ids[i] = issue.ID
	}
	deleteResult, err := uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (domain.DeleteIssuesResult, string, error) {
		res, err := uw.IssueUseCase().DeleteIssues(ctx, domain.DeleteIssuesParams{IDs: ids}, getActor())
		if err != nil {
			return domain.DeleteIssuesResult{}, "", err
		}
		return res, fmt.Sprintf("bd: gc decay %d issue(s)", res.DeletedCount), nil
	})
	if err != nil {
		return gcPhaseResult{}, HandleErrorRespectJSON("deleting closed issues: %v", err)
	}
	commandDidWrite.Store(true)
	if !isJSONOutput() {
		fmt.Printf("  Deleted %d issue(s)\n", deleteResult.DeletedCount)
	}
	return gcPhaseResult{name: "Decay", detail: fmt.Sprintf("%d issues deleted", deleteResult.DeletedCount)}, nil
}

func runProxiedGCCompact(ctx context.Context, opts gcOptions) gcPhaseResult {
	if !isJSONOutput() {
		fmt.Println("Phase 2/3: Compact (Dolt commit history info)")
	}
	commitCount, logErr := uow.RunTxRead(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (int, error) {
		res, err := uw.RawSQLUseCase().Query(ctx, "SELECT COUNT(*) FROM dolt_log")
		if err != nil {
			return 0, err
		}
		return scalarCount(res), nil
	})
	if logErr != nil {
		WarnError("could not read Dolt commit log: %v", logErr)
		commitCount = 0
	}

	switch {
	case commitCount <= 1:
		if !isJSONOutput() {
			fmt.Printf("  Only %d commit(s), nothing to compact\n\n", commitCount)
		}
		return gcPhaseResult{name: "Compact", detail: "nothing to compact"}
	case opts.dryRun:
		if !isJSONOutput() {
			fmt.Printf("  %d commits in history (use bd flatten to squash)\n\n", commitCount)
		}
		return gcPhaseResult{name: "Compact", detail: fmt.Sprintf("%d commits (dry-run)", commitCount)}
	default:
		if !isJSONOutput() {
			fmt.Printf("  %d commits in history\n", commitCount)
			fmt.Printf("  Tip: use 'bd flatten' to squash all history to one commit\n\n")
		}
		return gcPhaseResult{name: "Compact", detail: fmt.Sprintf("%d commits", commitCount)}
	}
}

func runProxiedDoltGC(ctx context.Context, opts gcOptions) (gcPhaseResult, bool) {
	if !isJSONOutput() {
		fmt.Println("Phase 3/3: Dolt GC (reclaim disk space)")
	}
	if opts.dryRun {
		printProxiedDoltGCDryRun()
		return gcPhaseResult{name: "Dolt GC", detail: "dry-run"}, false
	}

	err := executeProxiedDoltGC(ctx)
	if err != nil {
		WarnError("dolt gc failed: %v", err)
		printProxiedDoltGCBlankLine()
		return gcPhaseResult{name: "Dolt GC", detail: "failed"}, true
	}
	if !isJSONOutput() {
		fmt.Println("  Done (complete)")
		fmt.Println()
	}
	return gcPhaseResult{name: "Dolt GC", detail: "complete"}, false
}

func executeProxiedDoltGC(ctx context.Context) error {
	return runProxiedNonTx(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if err := versioncontrolops.DoltGC(ctx, conn); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "CALL DOLT_STATS_GC()"); err != nil {
			return fmt.Errorf("dolt_stats_gc: %w", err)
		}
		return nil
	})
}

func printProxiedDoltGCDryRun() {
	if !isJSONOutput() {
		fmt.Println("  Would run DOLT_GC() and DOLT_STATS_GC()")
		fmt.Println()
	}
}

func printProxiedDoltGCBlankLine() {
	if !isJSONOutput() {
		fmt.Println()
	}
}

func reportProxiedGC(results []gcPhaseResult, opts gcOptions, gcFailed bool, elapsed time.Duration) error {
	if isJSONOutput() {
		return reportProxiedGCJSON(results, opts, gcFailed, elapsed)
	}

	mode := "✓ GC complete"
	if opts.dryRun {
		mode = "DRY RUN complete"
	} else if gcFailed {
		mode = "⚠ GC completed with errors"
	}
	fmt.Printf("%s (%v)\n", mode, elapsed.Round(time.Millisecond))
	printGCPhaseSummary(results)
	if gcFailed {
		return SilentExit()
	}
	return nil
}

func reportProxiedGCJSON(results []gcPhaseResult, opts gcOptions, gcFailed bool, elapsed time.Duration) error {
	summaryMap := map[string]interface{}{
		"dry_run":    opts.dryRun,
		"success":    !gcFailed,
		"elapsed_ms": elapsed.Milliseconds(),
		"phases":     gcPhaseSummary(results),
	}
	if err := outputJSON(summaryMap); err != nil {
		return err
	}
	if gcFailed {
		return SilentExit()
	}
	return nil
}

func gcPhaseSummary(results []gcPhaseResult) []map[string]interface{} {
	phases := make([]map[string]interface{}, 0, len(results))
	for _, result := range results {
		phase := map[string]interface{}{
			"name":    result.name,
			"skipped": result.skipped,
		}
		if result.detail != "" {
			phase["detail"] = result.detail
		}
		phases = append(phases, phase)
	}
	return phases
}

func printGCPhaseSummary(results []gcPhaseResult) {
	for _, result := range results {
		if result.skipped {
			fmt.Printf("  %s: skipped\n", result.name)
		} else {
			fmt.Printf("  %s: %s\n", result.name, result.detail)
		}
	}
}

func scalarCount(res *domain.RawSQLResult) int {
	if res == nil || len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return 0
	}
	return scalarCountValue(res.Rows[0][0])
}

func scalarCountValue(value interface{}) int {
	switch v := value.(type) {
	case int64:
		return int(v)
	case int:
		return v
	case uint64:
		return int(v)
	case float64:
		return int(v)
	case []byte:
		n, _ := strconv.Atoi(string(v))
		return n
	case string:
		n, _ := strconv.Atoi(v)
		return n
	default:
		return 0
	}
}
