package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

type compactDoltPlan struct {
	totalCommits  int
	oldCommits    int
	recentCommits int
	recentHashes  []string
	initialHash   string
	boundaryHash  string
	cutoff        time.Time
}

func runCompactProxiedServer(ctx context.Context, opts compactDoltOptions) error {
	evt := metrics.NewCommandEvent("compact")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	return executeCompactProxiedServer(ctx, opts)
}

func executeCompactProxiedServer(ctx context.Context, opts compactDoltOptions) error {
	if err := prepareCompactDoltOptions(opts); err != nil {
		return err
	}
	start := time.Now()
	plan, err := loadCompactDoltPlan(ctx, opts.days)
	if err != nil {
		return HandleError("%v", err)
	}
	if plan.totalCommits <= 1 {
		return reportCompactDoltNoOp(plan.totalCommits)
	}
	if opts.dryRun {
		return reportCompactDoltDryRun(plan, opts)
	}
	return applyCompactDoltPlan(ctx, start, plan, opts)
}

func prepareCompactDoltOptions(opts compactDoltOptions) error {
	if !opts.dryRun {
		if err := CheckReadonly("compact"); err != nil {
			return err
		}
	}
	if opts.days < 0 {
		return HandleError("--days must be non-negative")
	}
	return nil
}

func applyCompactDoltPlan(ctx context.Context, start time.Time, plan compactDoltPlan, opts compactDoltOptions) error {
	if plan.oldCommits <= 1 {
		return reportCompactDoltNoOp(plan.totalCommits, plan.oldCommits)
	}
	if err := validateCompactDoltPlan(plan, opts.force); err != nil {
		return err
	}
	if !isJSONOutput() {
		fmt.Printf("Compacting: %d old commits → 1, preserving %d recent\n",
			plan.oldCommits, plan.recentCommits)
	}
	pruned, tags, err := compactDoltHistory(ctx, plan)
	if err != nil {
		return HandleError("compact failed: %v", err)
	}
	return reportCompactDoltResult(start, plan, pruned, tags)
}

func loadCompactDoltPlan(ctx context.Context, days int) (compactDoltPlan, error) {
	var logEntries []storage.CommitInfo
	if err := runProxiedNonTx(ctx, func(ctx context.Context, conn *sql.Conn) error {
		var err error
		logEntries, err = versioncontrolops.Log(ctx, conn, 0)
		return err
	}); err != nil {
		return compactDoltPlan{}, fmt.Errorf("failed to read commit log: %v", err)
	}
	return compactDoltPlanFromLog(logEntries, days), nil
}

func compactDoltPlanFromLog(logEntries []storage.CommitInfo, days int) compactDoltPlan {
	plan := compactDoltPlan{totalCommits: len(logEntries)}
	if plan.totalCommits <= 1 {
		return plan
	}
	plan.cutoff = time.Now().AddDate(0, 0, -days)
	plan.initialHash = logEntries[plan.totalCommits-1].Hash
	for _, entry := range logEntries {
		if entry.Date.Before(plan.cutoff) {
			plan.oldCommits++
		} else {
			plan.recentHashes = append(plan.recentHashes, entry.Hash)
		}
	}
	for _, entry := range logEntries {
		if entry.Date.Before(plan.cutoff) {
			plan.boundaryHash = entry.Hash
			break
		}
	}
	for i, j := 0, len(plan.recentHashes)-1; i < j; i, j = i+1, j-1 {
		plan.recentHashes[i], plan.recentHashes[j] = plan.recentHashes[j], plan.recentHashes[i]
	}
	plan.recentCommits = len(plan.recentHashes)
	return plan
}

func reportCompactDoltNoOp(values ...int) error {
	if isJSONOutput() {
		result := map[string]interface{}{
			"success":       true,
			"message":       "nothing to compact",
			"total_commits": values[0],
		}
		if len(values) > 1 {
			result["old_commits"] = values[1]
		}
		return outputJSON(result)
	}
	count := values[0]
	label := "commit(s)"
	if len(values) > 1 {
		count = values[1]
		label = "old commit(s)"
	}
	fmt.Printf("Only %d %s. Nothing to compact.\n", count, label)
	return nil
}

func reportCompactDoltDryRun(plan compactDoltPlan, opts compactDoltOptions) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"dry_run":        true,
			"total_commits":  plan.totalCommits,
			"old_commits":    plan.oldCommits,
			"recent_commits": plan.recentCommits,
			"cutoff_days":    opts.days,
			"cutoff_date":    plan.cutoff.Format("2006-01-02"),
			"initial_hash":   plan.initialHash,
			"boundary_hash":  plan.boundaryHash,
		})
	}
	fmt.Printf("DRY RUN — Compact preview\n\n")
	fmt.Printf("  Total commits:  %d\n", plan.totalCommits)
	fmt.Printf("  Old (>%d days): %d (would be squashed into 1)\n", opts.days, plan.oldCommits)
	fmt.Printf("  Recent:         %d (preserved)\n", plan.recentCommits)
	fmt.Printf("  Cutoff date:    %s\n", plan.cutoff.Format("2006-01-02"))
	if plan.oldCommits <= 1 {
		fmt.Printf("\n  Nothing to compact (0-1 old commits).\n")
		return nil
	}
	fmt.Printf("\n  Result: %d commits → %d commits\n", plan.totalCommits, plan.recentCommits+1)
	fmt.Printf("  Run with --force to proceed.\n")
	return nil
}

func validateCompactDoltPlan(plan compactDoltPlan, force bool) error {
	if plan.boundaryHash == "" {
		return HandleError("could not find boundary commit for compaction")
	}
	if !force {
		return HandleErrorWithHint(
			fmt.Sprintf("would squash %d old commits into 1, preserving %d recent commits",
				plan.oldCommits, plan.recentCommits),
			"Use --force to confirm or --dry-run to preview.")
	}
	return nil
}

func compactDoltHistory(ctx context.Context, plan compactDoltPlan) ([]string, []string, error) {
	var pruned, tags []string
	if err := runProxiedNonTx(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if err := versioncontrolops.Compact(ctx, conn, plan.initialHash, plan.boundaryHash, plan.oldCommits, plan.recentHashes); err != nil {
			return err
		}
		var perr error
		if pruned, perr = versioncontrolops.PruneRemoteRefs(ctx, conn); perr != nil {
			WarnError("pruning remote-tracking refs before GC: %v (GC may reclaim little)", perr)
		}
		if tags, perr = versioncontrolops.ListTags(ctx, conn); perr != nil {
			WarnError("listing tags before GC: %v", perr)
		}
		if perr := versioncontrolops.DoltGC(ctx, conn); perr != nil {
			WarnError("dolt gc after compact failed: %v", perr)
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return pruned, tags, nil
}

func reportCompactDoltResult(start time.Time, plan compactDoltPlan, pruned, tags []string) error {
	if !isJSONOutput() {
		printPruneReport(pruned, tags)
	}

	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"success":            true,
			"commits_before":     plan.totalCommits,
			"commits_after":      plan.recentCommits + 1,
			"old_squashed":       plan.oldCommits,
			"recent_kept":        plan.recentCommits,
			"remote_refs_pruned": pruned,
			"tags_anchoring":     tags,
			"elapsed_ms":         time.Since(start).Milliseconds(),
		})
	}

	fmt.Printf("✓ Compacted %d commits → %d\n", plan.totalCommits, plan.recentCommits+1)
	fmt.Printf("  Squashed: %d old commits → 1 base\n", plan.oldCommits)
	fmt.Printf("  Preserved: %d recent commits\n", plan.recentCommits)
	fmt.Printf("  Time: %v\n", time.Since(start).Round(time.Millisecond))
	return nil
}
