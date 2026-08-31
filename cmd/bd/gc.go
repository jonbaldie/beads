package main

import (
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/spf13/cobra"
)

type gcOptions struct {
	dryRun    bool
	force     bool
	olderThan int
	skipDecay bool
	skipDolt  bool
}

type gcPhaseResult struct {
	name    string
	skipped bool
	detail  string
}

func gcOptionsFromCommand(cmd *cobra.Command) gcOptions {
	options := gcOptions{}
	options.dryRun, _ = cmd.Flags().GetBool("dry-run")
	options.force, _ = cmd.Flags().GetBool("force")
	options.olderThan, _ = cmd.Flags().GetInt("older-than")
	options.skipDecay, _ = cmd.Flags().GetBool("skip-decay")
	options.skipDolt, _ = cmd.Flags().GetBool("skip-dolt")
	return options
}

var gcCmd = &cobra.Command{
	Use:     "gc",
	GroupID: "maint",
	Short:   "Garbage collect: decay old issues, compact Dolt commits, run Dolt GC",
	Long: `Full lifecycle garbage collection for standalone Beads databases.

Runs three phases in sequence:
  1. DECAY   — Delete closed issues older than N days (default 90)
  2. COMPACT — Squash old Dolt commits into fewer commits (bd compact)
  3. GC      — Run Dolt garbage collection to reclaim disk space

Each phase can be skipped individually. Use --dry-run to preview all phases
without making changes.

Examples:
  bd gc                              # Full GC with defaults (90 day decay)
  bd gc --dry-run                    # Preview what would happen
  bd gc --older-than 30              # Decay issues closed 30+ days ago
  bd gc --skip-decay                 # Skip issue deletion, just compact+GC
  bd gc --skip-dolt                  # Skip Dolt GC, just decay+compact
  bd gc --force                      # Skip confirmation prompt`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		opts := gcOptionsFromCommand(cmd)
		if usesProxiedServer() {
			return runGCProxiedServer(getRootContext(), opts)
		}
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
		ctx := getRootContext()
		start := time.Now()

		if opts.olderThan < 0 {
			return HandleErrorRespectJSON("--older-than must be non-negative")
		}

		var results []gcPhaseResult

		if opts.skipDecay {
			results = append(results, gcPhaseResult{name: "Decay", skipped: true})
		} else {
			if !isJSONOutput() {
				fmt.Println("Phase 1/3: Decay (delete old closed issues)")
			}

			cutoffDays := opts.olderThan
			cutoffTime := time.Now().UTC().AddDate(0, 0, -cutoffDays)
			statusClosed := types.StatusClosed
			// gc is a scripted internal sweep — opt out of BEADS_MAX_ROWS
			// (designer §4.1) so a misconfigured env doesn't abort the sweep.
			filter := types.IssueFilter{
				IssueFilterCore: types.IssueFilterCore{
					Status: &statusClosed,
				},
				IssueFilterMatch: types.IssueFilterMatch{
					ClosedBefore: &cutoffTime,
				},
				IssueFilterPage: types.IssueFilterPage{
					MaxRows:       0,
					MaxRowsSource: "",
				},
			}

			closedIssues, err := getStore().SearchIssues(ctx, "", filter)
			if err != nil {
				return HandleErrorRespectJSON("searching closed issues: %v", err)
			}

			var stats closedDeletionCandidateStats
			closedIssues, stats = filterClosedDeletionCandidates(closedIssues, &cutoffTime)
			warnClosedDeletionSafetySkips(stats)

			if len(closedIssues) == 0 {
				detail := fmt.Sprintf("  No closed issues older than %d days", cutoffDays)
				if !isJSONOutput() {
					fmt.Println(detail)
				}
				results = append(results, gcPhaseResult{name: "Decay", detail: "0 issues deleted"})
			} else {
				if opts.dryRun {
					detail := fmt.Sprintf("  Would delete %d closed issue(s)", len(closedIssues))
					if !isJSONOutput() {
						fmt.Println(detail)
					}
					results = append(results, gcPhaseResult{name: "Decay", detail: fmt.Sprintf("%d issues (dry-run)", len(closedIssues))})
				} else {
					if !opts.force {
						return HandleErrorWithHintRespectJSON(
							fmt.Sprintf("would delete %d closed issue(s) older than %d days", len(closedIssues), cutoffDays),
							"Use --force to confirm or --dry-run to preview.")
					}

					deleted := 0
					for _, issue := range closedIssues {
						if err := getStore().DeleteIssue(ctx, issue.ID); err != nil {
							WarnError("failed to delete %s: %v", issue.ID, err)
						} else {
							deleted++
						}
					}
					commandDidWrite.Store(true)
					detail := fmt.Sprintf("  Deleted %d issue(s)", deleted)
					if !isJSONOutput() {
						fmt.Println(detail)
					}
					results = append(results, gcPhaseResult{name: "Decay", detail: fmt.Sprintf("%d issues deleted", deleted)})

					if deleted > 0 {
						commandDidWrite.Store(true)
					}
				}
			}
			if !isJSONOutput() {
				fmt.Println()
			}
		}

		if !isJSONOutput() {
			fmt.Println("Phase 2/3: Compact (Dolt commit history info)")
		}

		commitCount := 0
		logEntries, logErr := getStore().Log(ctx, 0)
		if logErr != nil {
			WarnError("could not read Dolt commit log: %v", logErr)
		} else {
			commitCount = len(logEntries)
		}

		if commitCount <= 1 {
			if !isJSONOutput() {
				fmt.Printf("  Only %d commit(s), nothing to compact\n\n", commitCount)
			}
			results = append(results, gcPhaseResult{name: "Compact", detail: "nothing to compact"})
		} else {
			if opts.dryRun {
				if !isJSONOutput() {
					fmt.Printf("  %d commits in history (use bd flatten to squash)\n\n", commitCount)
				}
				results = append(results, gcPhaseResult{name: "Compact", detail: fmt.Sprintf("%d commits (dry-run)", commitCount)})
			} else {
				if !isJSONOutput() {
					fmt.Printf("  %d commits in history\n", commitCount)
					fmt.Printf("  Tip: use 'bd flatten' to squash all history to one commit\n\n")
				}
				results = append(results, gcPhaseResult{name: "Compact", detail: fmt.Sprintf("%d commits", commitCount)})
			}
		}

		var gcSizeInfo map[string]interface{}
		if opts.skipDolt {
			results = append(results, gcPhaseResult{name: "Dolt GC", skipped: true})
		} else {
			if !isJSONOutput() {
				fmt.Println("Phase 3/3: Dolt GC (reclaim disk space)")
			}

			gc, ok := storage.UnwrapStore(getStore()).(storage.GarbageCollector)
			if !ok {
				if !isJSONOutput() {
					fmt.Println("  Storage backend does not support GC, skipping")
				}
				results = append(results, gcPhaseResult{name: "Dolt GC", detail: "not supported"})
			} else if opts.dryRun {
				if !isJSONOutput() {
					fmt.Println("  Would run DOLT_GC()")
				}
				results = append(results, gcPhaseResult{name: "Dolt GC", detail: "dry-run"})
			} else {
				// bd gc runs without a preceding squash, so remote-tracking
				// refs are left alone here (they cache the remote tip for the
				// migrate gate); flatten/compact prune them before their GC
				// (bd-agctw). Sizes are reported so a no-op reclaim is visible.
				sizeBefore := storeSizeBytes(ctx)
				remoteRefs, tags := listRemoteRefsAndTags(ctx)
				if err := gc.DoltGC(ctx); err != nil {
					WarnError("dolt gc failed: %v", err)
					results = append(results, gcPhaseResult{name: "Dolt GC", detail: "failed"})
				} else {
					sizeAfter := storeSizeBytes(ctx)
					detail := "complete"
					if line := gcSizeLine(sizeBefore, sizeAfter); line != "" {
						detail = "complete: " + line
					}
					if !isJSONOutput() {
						fmt.Printf("  Done (%s)\n", detail)
						if len(remoteRefs)+len(tags) > 0 {
							fmt.Printf("  Note: %d remote-tracking ref(s) and %d tag(s) anchor history;\n", len(remoteRefs), len(tags))
							fmt.Printf("  after a history squash, use bd flatten / bd compact so they are pruned first.\n")
						}
					}
					results = append(results, gcPhaseResult{name: "Dolt GC", detail: detail})
					gcSizeInfo = map[string]interface{}{
						"remote_refs": len(remoteRefs),
						"tags":        len(tags),
					}
					addGCSizeJSON(gcSizeInfo, sizeBefore, sizeAfter)
				}
			}
			if !isJSONOutput() {
				fmt.Println()
			}
		}

		elapsed := time.Since(start)

		if isJSONOutput() {
			summaryMap := make(map[string]interface{})
			summaryMap["dry_run"] = opts.dryRun
			summaryMap["elapsed_ms"] = elapsed.Milliseconds()
			phases := make([]map[string]interface{}, 0, len(results))
			for _, r := range results {
				p := map[string]interface{}{
					"name":    r.name,
					"skipped": r.skipped,
				}
				if r.detail != "" {
					p["detail"] = r.detail
				}
				phases = append(phases, p)
			}
			summaryMap["phases"] = phases
			if gcSizeInfo != nil {
				summaryMap["dolt_gc"] = gcSizeInfo
			}
			return outputJSON(summaryMap)
		}

		mode := "✓ GC complete"
		if opts.dryRun {
			mode = "DRY RUN complete"
		}
		fmt.Printf("%s (%v)\n", mode, elapsed.Round(time.Millisecond))
		for _, r := range results {
			if r.skipped {
				fmt.Printf("  %s: skipped\n", r.name)
			} else {
				fmt.Printf("  %s: %s\n", r.name, r.detail)
			}
		}
		return nil
	},
}

func init() {
	gcCmd.Flags().Bool("dry-run", false, "Preview without making changes")
	gcCmd.Flags().BoolP("force", "f", false, "Skip confirmation prompts")
	gcCmd.Flags().Int("older-than", 90, "Delete closed issues older than N days")
	gcCmd.Flags().Bool("skip-decay", false, "Skip issue deletion phase")
	gcCmd.Flags().Bool("skip-dolt", false, "Skip Dolt garbage collection phase")

	rootCmd.AddCommand(gcCmd)
}
