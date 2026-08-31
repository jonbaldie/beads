package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

var wispGCCmd = &cobra.Command{
	Use:   "gc",
	Short: "Garbage collect old/abandoned wisps",
	Long: `Garbage collect old or abandoned wisps from the database.

A wisp is considered abandoned if:
  - It hasn't been updated in --age duration and is not closed
  - AND it is not live work: blocked steps (waiting on a dependency), pinned
    beads, and any step whose status category is wip (in_progress, blocked,
    hooked) or frozen (deferred, pinned) are never reclaimed by age, no matter
    how long they have been waiting (GH#4394). Custom statuses count by their
    configured category, so only plain open (active) and closed (done) steps
    are age-reclaimable. If the blocked set or the custom-status list cannot be
    read, the GC aborts rather than risk reclaiming live steps.

Abandoned wisps are deleted without creating a digest. Use 'bd mol squash'
if you want to preserve a summary before garbage collection.

Use --closed to purge ALL closed wisps (regardless of age). This is the
fastest way to reclaim space from accumulated wisp bloat. Safe by default:
requires --force to actually delete.

Note: This uses time-based cleanup, appropriate for ephemeral wisps.
For graph-pressure staleness detection (blocking other work), see 'bd mol stale'.

Examples:
  bd mol wisp gc                                    # Clean abandoned wisps (default: 1h threshold)
  bd mol wisp gc --dry-run                          # Preview what would be cleaned
  bd mol wisp gc --age 24h                          # Custom age threshold
  bd mol wisp gc --all                              # Also clean closed wisps older than threshold
  bd mol wisp gc --closed                           # Preview closed wisp deletion
  bd mol wisp gc --closed --force                   # Delete all closed wisps
  bd mol wisp gc --closed --dry-run                 # Explicit dry-run (same as no --force)
  bd mol wisp gc --exclude-type agent,rig           # Protect agent and rig wisps from GC
  bd mol wisp gc --closed --force --exclude-type mol # Delete closed wisps except mol type`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runWispGC,
}

// WispGCResult is the JSON output for wisp gc
type WispGCResult struct {
	CleanedIDs   []string `json:"cleaned_ids"`
	CleanedCount int      `json:"cleaned_count"`
	Candidates   int      `json:"candidates,omitempty"`
	DryRun       bool     `json:"dry_run,omitempty"`
}

type wispGCInput struct {
	dryRun       bool
	ageThreshold time.Duration
	cleanAll     bool
	closedMode   bool
	force        bool
	excludeTypes []types.IssueType
}

// protectedWispStatuses returns the statuses whose category means a wisp is
// live work rather than abandoned, so age-based GC must never reclaim it.
//
// Protection is derived from the status *category* rather than a hand-written
// list: CategoryWIP (in_progress/blocked/hooked) is work in flight, and
// CategoryFrozen (deferred/pinned) is work deliberately put on ice — reclaiming
// something a user explicitly deferred defeats the point of deferring it. Only
// CategoryActive (plain open) and CategoryDone (closed) are age-reclaimable.
//
// Custom statuses (status.custom) participate on the same footing, matching the
// sibling destructive command in purge.go. Reading them is required, not
// best-effort: if we cannot enumerate them we must not under-protect and risk
// deleting live molecule steps, so the error propagates and aborts the GC.
func protectedWispStatuses(ctx context.Context, r molReader) (map[types.Status]bool, error) {
	protected := make(map[types.Status]bool)
	for _, s := range []types.Status{
		types.StatusOpen,
		types.StatusInProgress,
		types.StatusBlocked,
		types.StatusClosed,
		types.StatusDeferred,
		types.StatusPinned,
		types.StatusHooked,
	} {
		switch types.BuiltInStatusCategory(s) {
		case types.CategoryWIP, types.CategoryFrozen:
			protected[s] = true
		}
	}

	customStatuses, err := r.GetCustomStatusesDetailed(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading custom statuses for wisp age GC: %w", err)
	}
	for _, cs := range customStatuses {
		switch cs.Category {
		case types.CategoryWIP, types.CategoryFrozen:
			protected[types.Status(cs.Name)] = true
		}
	}
	return protected, nil
}

// isProtectedWisp reports whether a wisp is live work that age-based GC must
// never reclaim. A wisp is protected if it is explicitly pinned, if it is
// blocked on an open dependency (blockedSet, derived from is_blocked), or if
// its status falls in a protected category. Reclaiming any of these
// mid-execution destroys active molecules (GH#4394).
//
// Named isProtectedWisp rather than isActiveWisp to avoid confusion with
// (*DoltStore).isActiveWisp in internal/storage/dolt, which is in this same
// delete path but means only "a row for this ID exists in the wisps table".
func isProtectedWisp(issue *types.Issue, blockedSet map[string]bool, protectedStatuses map[types.Status]bool) bool {
	// The pinned flag is independent of the pinned status; the closed-purge
	// branch of this same command already honors it (see runWispPurgeClosed).
	if issue.Pinned {
		return true
	}
	if blockedSet[issue.ID] {
		return true
	}
	return protectedStatuses[issue.Status]
}

func runWispGC(cmd *cobra.Command, _ []string) error {
	if err := CheckReadonly("wisp gc"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("wisp-gc")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	in, err := gatherWispGCInput(cmd)
	if err != nil {
		return HandleError("%v", err)
	}

	if usesProxiedServer() {
		return runWispGCProxiedServer(getRootContext(), in.dryRun, in.ageThreshold, in.cleanAll, in.closedMode, in.force, in.excludeTypes)
	}
	return runWispGCLocal(in, cmd)
}

func runWispGCLocal(in wispGCInput, cmd *cobra.Command) error {
	if getStore() == nil {
		return HandleErrorWithHint("no database connection", diagHint())
	}

	if in.closedMode {
		return runWispPurgeClosed(getRootContext(), in.dryRun, in.force, in.excludeTypes)
	}

	abandoned, err := findAbandonedWisps(getRootContext(), getStore(), in.cleanAll, in.ageThreshold, in.excludeTypes)
	if err != nil && abandoned == nil {
		return HandleError("%v", err)
	}
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: cascade expansion incomplete: %v\n", err)
	}
	return finishWispGC(in, abandoned)
}

func gatherWispGCInput(cmd *cobra.Command) (wispGCInput, error) {
	in := wispGCInput{ageThreshold: time.Hour}
	in.dryRun, _ = cmd.Flags().GetBool("dry-run")
	ageStr, _ := cmd.Flags().GetString("age")
	in.cleanAll, _ = cmd.Flags().GetBool("all")
	in.closedMode, _ = cmd.Flags().GetBool("closed")
	in.force, _ = cmd.Flags().GetBool("force")
	excludeTypeStrs, _ := cmd.Flags().GetStringSlice("exclude-type")

	if ageStr != "" {
		var err error
		in.ageThreshold, err = time.ParseDuration(ageStr)
		if err != nil {
			return wispGCInput{}, fmt.Errorf("invalid --age duration: %v", err)
		}
	}
	for _, t := range excludeTypeStrs {
		in.excludeTypes = append(in.excludeTypes, types.IssueType(t))
	}
	return in, nil
}

func finishWispGC(in wispGCInput, abandoned []*types.Issue) error {
	if len(abandoned) == 0 {
		if isJSONOutput() {
			return outputJSON(WispGCResult{
				CleanedIDs:   []string{},
				CleanedCount: 0,
				DryRun:       in.dryRun,
			})
		}
		fmt.Println("No abandoned wisps found")
		return nil
	}

	if in.dryRun {
		return renderWispGCDryRun(abandoned)
	}

	ids := wispIDs(abandoned)
	if err := deleteBatch(nil, ids, true, false, true, isJSONOutput(), false, "wisp gc"); err != nil {
		return HandleError("%v", err)
	}
	return nil
}

func renderWispGCDryRun(abandoned []*types.Issue) error {
	if isJSONOutput() {
		return outputJSON(WispGCResult{
			CleanedIDs:   wispIDs(abandoned),
			Candidates:   len(abandoned),
			CleanedCount: 0,
			DryRun:       true,
		})
	}
	fmt.Printf("Dry run: would clean %d abandoned wisp(s):\n\n", len(abandoned))
	for _, issue := range abandoned {
		age := formatTimeAgo(issue.UpdatedAt)
		fmt.Printf("  %s: %s (last updated: %s)\n", issue.ID, issue.Title, age)
	}
	fmt.Printf("\nRun without --dry-run to delete these wisps.\n")
	return nil
}

func wispIDs(issues []*types.Issue) []string {
	ids := make([]string, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
	}
	return ids
}

func findAbandonedWisps(ctx context.Context, r molReader, cleanAll bool, ageThreshold time.Duration, excludeTypes []types.IssueType) ([]*types.Issue, error) {
	ephemeralFlag := true
	filter := types.IssueFilter{
		IssueFilterCore: types.IssueFilterCore{
			Limit: 5000,
		},
		IssueFilterFlags: types.IssueFilterFlags{
			Ephemeral: &ephemeralFlag,
		},
		IssueFilterHydrate: types.IssueFilterHydrate{
			ExcludeTypes: excludeTypes,
		},
	}
	issues, err := r.SearchIssues(ctx, "", filter)
	if err != nil {
		return nil, err
	}

	blockedSet, err := loadBlockedWispSet(ctx, r)
	if err != nil {
		return nil, err
	}

	protectedStatuses, err := protectedWispStatuses(ctx, r)
	if err != nil {
		return nil, err
	}

	abandoned := selectAbandonedWisps(ctx, r, issues, cleanAll, ageThreshold, blockedSet, protectedStatuses)

	if len(abandoned) == 0 {
		return abandoned, nil
	}

	parentIDs := wispIDs(abandoned)
	childIDs, cascadeErr := r.FindWispDependentsRecursive(ctx, parentIDs)
	if len(childIDs) > 0 {
		abandoned = appendAbandonedWispDependents(ctx, r, abandoned, childIDs, blockedSet, protectedStatuses)
	}
	return abandoned, cascadeErr
}

func loadBlockedWispSet(ctx context.Context, r molReader) (map[string]bool, error) {
	blocked, err := r.GetBlockedIssues(ctx, types.WorkFilter{})
	if err != nil {
		return nil, fmt.Errorf("determining blocked wisps for age GC: %w", err)
	}
	blockedSet := make(map[string]bool, len(blocked))
	for _, issue := range blocked {
		blockedSet[issue.ID] = true
	}
	return blockedSet, nil
}

func selectAbandonedWisps(ctx context.Context, r molReader, issues []*types.Issue, cleanAll bool, ageThreshold time.Duration, blockedSet map[string]bool, protectedStatuses map[types.Status]bool) []*types.Issue {
	now := time.Now()
	var abandoned []*types.Issue
	for _, issue := range issues {
		if isAbandonedWispCandidate(ctx, r, issue, cleanAll, ageThreshold, now, blockedSet, protectedStatuses) {
			abandoned = append(abandoned, issue)
		}
	}
	return abandoned
}

func isAbandonedWispCandidate(ctx context.Context, r molReader, issue *types.Issue, cleanAll bool, ageThreshold time.Duration, now time.Time, blockedSet map[string]bool, protectedStatuses map[types.Status]bool) bool {
	if r.IsInfraTypeCtx(ctx, issue.IssueType) {
		return false
	}
	if issue.Status == types.StatusClosed && !cleanAll {
		return false
	}
	if isProtectedWisp(issue, blockedSet, protectedStatuses) {
		return false
	}
	return now.Sub(issue.UpdatedAt) > ageThreshold
}

func appendAbandonedWispDependents(ctx context.Context, r molReader, abandoned []*types.Issue, childIDs map[string]bool, blockedSet map[string]bool, protectedStatuses map[types.Status]bool) []*types.Issue {
	childIDSlice := make([]string, 0, len(childIDs))
	for id := range childIDs {
		childIDSlice = append(childIDSlice, id)
	}
	childIssues, fetchErr := r.GetIssuesByIDs(ctx, childIDSlice)
	if fetchErr != nil {
		return abandoned
	}

	abandonedSet := make(map[string]bool, len(abandoned))
	for _, issue := range abandoned {
		abandonedSet[issue.ID] = true
	}
	for _, child := range childIssues {
		if abandonedSet[child.ID] {
			continue
		}
		if r.IsInfraTypeCtx(ctx, child.IssueType) {
			continue
		}
		if isProtectedWisp(child, blockedSet, protectedStatuses) {
			continue
		}
		abandoned = append(abandoned, child)
	}
	return abandoned
}

func runWispPurgeClosed(ctx context.Context, dryRun bool, force bool, excludeTypes []types.IssueType) error {
	closedIssues, pinnedCount, infraCount, err := listClosedWispCandidates(ctx, excludeTypes)
	if err != nil {
		return err
	}
	reportClosedWispProtection(pinnedCount, infraCount)
	if len(closedIssues) == 0 {
		return reportNoClosedWisps()
	}
	return completeClosedWispPurge(closedIssues, dryRun, force)
}

func listClosedWispCandidates(ctx context.Context, excludeTypes []types.IssueType) ([]*types.Issue, int, int, error) {
	statusClosed := types.StatusClosed
	ephemeralTrue := true
	filter := types.IssueFilter{
		IssueFilterCore: types.IssueFilterCore{
			Status: &statusClosed,
			Limit:  5000,
		},
		IssueFilterFlags: types.IssueFilterFlags{
			Ephemeral: &ephemeralTrue,
		},
		IssueFilterHydrate: types.IssueFilterHydrate{
			ExcludeTypes: excludeTypes,
		},
	}

	closedIssues, err := getStore().SearchIssues(ctx, "", filter)
	if err != nil {
		return nil, 0, 0, HandleError("listing closed wisps: %v", err)
	}

	// Filter out pinned and infra issues (protected from cleanup)
	pinnedCount := 0
	infraCount := 0
	filtered := make([]*types.Issue, 0, len(closedIssues))
	for _, issue := range closedIssues {
		if issue.Pinned {
			pinnedCount++
			continue
		}
		if getStore().IsInfraTypeCtx(ctx, issue.IssueType) {
			infraCount++
			continue
		}
		filtered = append(filtered, issue)
	}
	closedIssues = filtered
	return closedIssues, pinnedCount, infraCount, nil
}

func reportClosedWispProtection(pinnedCount, infraCount int) {
	if pinnedCount > 0 && !isJSONOutput() {
		fmt.Printf("Skipping %d pinned issue(s) (protected from cleanup)\n", pinnedCount)
	}
	if infraCount > 0 && !isJSONOutput() {
		fmt.Printf("Skipping %d configured infra issue(s) protected from GC\n", infraCount)
	}
}

func reportNoClosedWisps() error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"deleted_count": 0,
			"message":       "No closed wisps to delete",
		})
	}
	fmt.Println("No closed wisps to delete")
	return nil
}

func completeClosedWispPurge(closedIssues []*types.Issue, dryRun bool, force bool) error {
	ids := wispIDs(closedIssues)
	if !force && !dryRun {
		return reportClosedWispPreview(len(ids))
	}

	reportClosedWispPurge(len(ids), dryRun)
	if err := deleteBatch(nil, ids, force, dryRun, true, isJSONOutput(), false, "wisp gc --closed"); err != nil {
		return HandleError("%v", err)
	}

	if !dryRun && force && !isJSONOutput() {
		fmt.Printf("\nHint: Run 'bd compact --dolt' to reclaim disk space\n")
	}
	return nil
}

func reportClosedWispPreview(count int) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"candidates": count,
			"dry_run":    true,
		})
	}
	fmt.Printf("Found %d closed wisp(s) to delete\n", count)
	fmt.Printf("\nUse --force to proceed, or --dry-run for detailed preview.\n")
	return nil
}

func reportClosedWispPurge(count int, dryRun bool) {
	if isJSONOutput() {
		return
	}
	fmt.Printf("Found %d closed wisp(s)\n", count)
	if dryRun {
		fmt.Println(ui.RenderWarn("DRY RUN - no changes will be made"))
	}
	fmt.Println()
}

func init() {
	// Wisp command flags (for direct create: bd mol wisp <proto>)
	wispCmd.Flags().StringArray("var", []string{}, "Variable substitution (key=value)")
	wispCmd.Flags().Bool("dry-run", false, "Preview what would be created")
	wispCmd.Flags().Bool("root-only", false, "Create only the root issue (no child step issues)")

	// Wisp create command flags (kept for backwards compat: bd mol wisp create <proto>)
	wispCreateCmd.Flags().StringArray("var", []string{}, "Variable substitution (key=value)")
	wispCreateCmd.Flags().Bool("dry-run", false, "Preview what would be created")
	wispCreateCmd.Flags().Bool("root-only", false, "Create only the root issue (no child step issues)")

	wispListCmd.Flags().Bool("all", false, "Include closed wisps")
	wispListCmd.Flags().String("type", "", "Filter by issue type (e.g., agent, task, patrol)")

	wispGCCmd.Flags().Bool("dry-run", false, "Preview what would be cleaned")
	wispGCCmd.Flags().String("age", "1h", "Age threshold for abandoned wisp detection")
	wispGCCmd.Flags().Bool("all", false, "Also clean closed wisps older than threshold")
	wispGCCmd.Flags().Bool("closed", false, "Delete all closed wisps (ignores --age threshold)")
	wispGCCmd.Flags().BoolP("force", "f", false, "Actually delete (default: preview only)")
	wispGCCmd.Flags().StringSlice("exclude-type", nil, "Exclude wisps of these types from GC (comma-separated, e.g., agent,rig)")

	wispCmd.AddCommand(wispCreateCmd)
	wispCmd.AddCommand(wispListCmd)
	wispCmd.AddCommand(wispGCCmd)
	molCmd.AddCommand(wispCmd)
}
