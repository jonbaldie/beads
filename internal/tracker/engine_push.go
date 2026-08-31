package tracker

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
)

// doPush exports beads issues to the external tracker.
// pushHashKey returns the local_metadata key under which the last-pushed
// content hash for an issue is stored, namespaced per tracker (e.g.
// "github.pushhash.bd-123"). local_metadata is dolt-ignored, so these hashes
// are clone-local and reset on clone/branch-checkout/server-restart; that only
// costs one fetch+ContentEqual pass to repopulate, never a missed update.
func pushHashKey(e *Engine, issueID string) string {
	return e.Tracker.ConfigPrefix() + ".pushhash." + issueID
}

// pushCacheValue binds the content fingerprint to the remote target and its
// configured namespace. A local issue can be relinked, or a repo-less ref can
// resolve under a newly configured namespace, without changing any pushable
// content. Treating the content hash alone as valid in either case would skip
// the first update to the new target. Length prefixes keep fields unambiguous.
func pushCacheValue(e *Engine, issue *types.Issue, externalRef string) string {
	if e.PushHooks == nil || e.PushHooks.ContentHash == nil {
		return ""
	}
	content := e.PushHooks.ContentHash(issue)
	if content == "" {
		return ""
	}
	target := strings.TrimSpace(externalRef)
	if target == "" {
		return ""
	}
	scope := ""
	if e.PushHooks.TargetScope != nil {
		scope = strings.TrimSpace(e.PushHooks.TargetScope())
	}
	return fmt.Sprintf("v3:%d:%s%d:%s%s", len(content), content, len(scope), scope, target)
}

// storedPushHashMatches reports whether the persisted push hash for issue still
// equals its current content-and-target fingerprint. When true, a push is
// provably a no-op and both the remote fetch and the update can be skipped.
func storedPushHashMatches(e *Engine, ctx context.Context, issue *types.Issue, externalRef string) bool {
	current := pushCacheValue(e, issue, externalRef)
	if current == "" {
		return false
	}
	stored, err := e.Store.GetLocalMetadata(ctx, pushHashKey(e, issue.ID))
	return err == nil && stored != "" && stored == current
}

// recordPushHash persists the current content-and-target fingerprint for issue
// so subsequent pushes can short-circuit via storedPushHashMatches. No-op when
// ContentHash is unset or returns "", or when the target is empty. Never called
// during dry-run.
func recordPushHash(e *Engine, ctx context.Context, issue *types.Issue, externalRef string) {
	h := pushCacheValue(e, issue, externalRef)
	if h == "" {
		return
	}
	if err := e.Store.SetLocalMetadata(ctx, pushHashKey(e, issue.ID), h); err != nil {
		e.warn("Failed to record push hash for %s: %v", issue.ID, err)
	}
}

func doPush(e *Engine, ctx context.Context, opts SyncOptions, skipIDs, forceIDs map[string]bool) (*PushStats, error) {
	ctx, span := syncTracer.Start(ctx, "tracker.push",
		trace.WithAttributes(
			attribute.String("sync.tracker", e.Tracker.DisplayName()),
			attribute.Bool("sync.dry_run", opts.DryRun),
		),
	)
	defer span.End()

	stats := &PushStats{}
	if err := buildPushStateCache(e, ctx); err != nil {
		return nil, err
	}
	issues, err := loadPushIssues(e, ctx, opts)
	if err != nil {
		return nil, err
	}
	descendantSet, err := loadPushDescendants(e, ctx, opts)
	if err != nil {
		return nil, err
	}
	if batchTracker, ok := e.Tracker.(BatchPushTracker); ok {
		handled, err := handleBatchPush(e, ctx, batchTracker, issues, opts, descendantSet, skipIDs, forceIDs, stats)
		if err != nil {
			return nil, err
		}
		if handled {
			return stats, nil
		}
	}
	if err := pushIssueList(e, ctx, issues, opts, descendantSet, skipIDs, forceIDs, stats); err != nil {
		return stats, err
	}

	span.SetAttributes(
		attribute.Int("sync.created", stats.Created),
		attribute.Int("sync.updated", stats.Updated),
		attribute.Int("sync.skipped", stats.Skipped),
		attribute.Int("sync.errors", stats.Errors),
	)
	return stats, nil
}

func buildPushStateCache(e *Engine, ctx context.Context) error {
	setEngineStateCache(e, nil)
	if e.PushHooks == nil || e.PushHooks.BuildStateCache == nil {
		return nil
	}
	var err error
	cache, err := e.PushHooks.BuildStateCache(ctx)
	if err != nil {
		return fmt.Errorf("building state cache: %w", err)
	}
	setEngineStateCache(e, cache)
	return nil
}

func loadPushIssues(e *Engine, ctx context.Context, opts SyncOptions) ([]*types.Issue, error) {
	issues, err := e.Store.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return nil, fmt.Errorf("searching local issues: %w", err)
	}
	issueIDSet := buildIssueIDSet(opts.IssueIDs)
	if issueIDSet == nil {
		return issues, nil
	}
	filtered := make([]*types.Issue, 0, len(opts.IssueIDs))
	for _, issue := range issues {
		if issueIDSet[issue.ID] {
			filtered = append(filtered, issue)
		}
	}
	return filtered, nil
}

func loadPushDescendants(e *Engine, ctx context.Context, opts SyncOptions) (map[string]bool, error) {
	if opts.ParentID == "" {
		return nil, nil
	}
	descendants, err := buildDescendantSet(e, ctx, opts.ParentID)
	if err != nil {
		return nil, fmt.Errorf("resolving parent %s: %w", opts.ParentID, err)
	}
	return descendants, nil
}

func handleBatchPush(e *Engine, ctx context.Context, tracker BatchPushTracker, issues []*types.Issue, opts SyncOptions, descendantSet, skipIDs, forceIDs map[string]bool, stats *PushStats) (bool, error) {
	pushIssues, skipped := collectBatchPushIssues(e, issues, opts, descendantSet, skipIDs, forceIDs)
	stats.Skipped += skipped
	if len(pushIssues) == 0 {
		return true, nil
	}
	if opts.DryRun {
		dryRunner, ok := e.Tracker.(BatchPushDryRunner)
		if !ok {
			return false, nil
		}
		result, err := dryRunner.BatchPushDryRun(ctx, pushIssues, forceIDs)
		if err != nil {
			return false, fmt.Errorf("previewing batch push: %w", err)
		}
		applyBatchDryRunResult(e, pushIssues, result, stats)
		return true, nil
	}
	result, err := tracker.BatchPush(ctx, pushIssues, forceIDs)
	if err != nil {
		return false, fmt.Errorf("batch pushing issues: %w", err)
	}
	applyBatchPushResult(e, ctx, result)
	applyBatchResultStats(result, stats)
	warnBatchResultErrors(e, result, false)
	return true, nil
}

func applyBatchDryRunResult(e *Engine, issues []*types.Issue, result *BatchPushResult, stats *PushStats) {
	renderBatchDryRun(e, issues, result)
	applyBatchResultStats(result, stats)
	warnBatchResultErrors(e, result, true)
}

func applyBatchResultStats(result *BatchPushResult, stats *PushStats) {
	if result == nil {
		return
	}
	stats.Created += len(result.Created)
	stats.Updated += len(result.Updated)
	stats.Skipped += len(result.Skipped)
	stats.Errors += len(result.Errors)
	stats.Warnings = append(stats.Warnings, result.Warnings...)
}

func warnBatchResultErrors(e *Engine, result *BatchPushResult, preview bool) {
	if result == nil {
		return
	}
	for _, item := range result.Errors {
		if item.LocalID != "" {
			verb := "push"
			if preview {
				verb = "preview push"
			}
			e.warn("Failed to %s %s in %s: %s", verb, item.LocalID, e.Tracker.DisplayName(), item.Message)
			continue
		}
		verb := "push issues"
		if preview {
			verb = "preview pushes"
		}
		e.warn("Failed to %s in %s: %s", verb, e.Tracker.DisplayName(), item.Message)
	}
}

func pushIssueList(e *Engine, ctx context.Context, issues []*types.Issue, opts SyncOptions, descendantSet, skipIDs, forceIDs map[string]bool, stats *PushStats) error {
	for _, issue := range issues {
		if !pushIssueEligible(e, issue, opts, descendantSet, skipIDs) {
			stats.Skipped++
			continue
		}
		stop, err := pushOneIssue(e, ctx, issue, issues, opts, forceIDs, stats)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
	return nil
}

func pushIssueEligible(e *Engine, issue *types.Issue, opts SyncOptions, descendantSet, skipIDs map[string]bool) bool {
	if descendantSet != nil && !descendantSet[issue.ID] {
		return false
	}
	if !shouldPushIssue(e, issue, opts) {
		return false
	}
	if e.PushHooks != nil && e.PushHooks.ShouldPush != nil && !e.PushHooks.ShouldPush(issue) {
		return false
	}
	return !skipIDs[issue.ID]
}

func pushOneIssue(e *Engine, ctx context.Context, issue *types.Issue, issues []*types.Issue, opts SyncOptions, forceIDs map[string]bool, stats *PushStats) (bool, error) {
	extRef := derefStr(issue.ExternalRef)
	willCreate := extRef == "" || !e.Tracker.IsExternalRef(extRef)
	if opts.DryRun {
		previewPushIssue(e, ctx, issue, extRef, willCreate, forceIDs, stats)
		return false, nil
	}
	pushIssue := formatPushIssue(e, issue)
	if willCreate {
		return pushNewIssue(e, ctx, issue, pushIssue, issues, stats)
	}
	if opts.CreateOnly && !forceIDs[issue.ID] {
		stats.Skipped++
		return false, nil
	}
	return pushExistingIssue(e, ctx, issue, pushIssue, extRef, issues, forceIDs, stats)
}

func previewPushIssue(e *Engine, ctx context.Context, issue *types.Issue, extRef string, willCreate bool, forceIDs map[string]bool, stats *PushStats) {
	if willCreate {
		e.msg("[dry-run] Would create in %s: %s", e.Tracker.DisplayName(), ui.SanitizeForTerminal(issue.Title))
		stats.Created++
		return
	}
	if !forceIDs[issue.ID] && storedPushHashMatches(e, ctx, issue, extRef) {
		stats.Skipped++
		return
	}
	e.msg("[dry-run] Would update in %s: %s", e.Tracker.DisplayName(), ui.SanitizeForTerminal(issue.Title))
	stats.Updated++
}

func formatPushIssue(e *Engine, issue *types.Issue) *types.Issue {
	if e.PushHooks == nil || e.PushHooks.FormatDescription == nil {
		return issue
	}
	copy := *issue
	copy.Description = e.PushHooks.FormatDescription(issue)
	return &copy
}

func pushNewIssue(e *Engine, ctx context.Context, issue, pushIssue *types.Issue, issues []*types.Issue, stats *PushStats) (bool, error) {
	created, err := e.Tracker.CreateIssue(ctx, pushIssue)
	if err != nil {
		return handlePushAPIError(e, err, "create", issue.ID, len(issues), stats)
	}
	ref := e.Tracker.BuildExternalRef(created)
	updates := map[string]interface{}{"external_ref": ref}
	if err := e.Store.UpdateIssue(ctx, issue.ID, updates, e.Actor); err != nil {
		e.warn("Failed to update external_ref for %s: %v", issue.ID, err)
		stats.Errors++
	}
	for _, warning := range created.Warnings {
		e.warn("%s (%s)", warning, issue.ID)
	}
	recordPushHash(e, ctx, issue, ref)
	stats.Created++
	return false, nil
}

func pushExistingIssue(e *Engine, ctx context.Context, issue, pushIssue *types.Issue, extRef string, issues []*types.Issue, forceIDs map[string]bool, stats *PushStats) (bool, error) {
	extID := e.Tracker.ExtractIdentifier(extRef)
	if extID == "" {
		stats.Skipped++
		return false, nil
	}
	if !forceIDs[issue.ID] {
		skip, err := existingPushIsCurrent(e, ctx, issue, extID, extRef, stats)
		if err != nil {
			return false, err
		}
		if skip {
			return false, nil
		}
	}
	if _, err := e.Tracker.UpdateIssue(ctx, extID, pushIssue); err != nil {
		return handlePushAPIError(e, err, "update", issue.ID, len(issues), stats)
	}
	recordPushHash(e, ctx, issue, extRef)
	stats.Updated++
	return false, nil
}

func existingPushIsCurrent(e *Engine, ctx context.Context, issue *types.Issue, extID, extRef string, stats *PushStats) (bool, error) {
	if storedPushHashMatches(e, ctx, issue, extRef) {
		stats.Skipped++
		return true, nil
	}
	extIssue, err := e.Tracker.FetchIssue(ctx, extID)
	if isRateLimitExhausted(err) {
		return false, fmt.Errorf("sync aborted: %w", err)
	}
	if err != nil || extIssue == nil {
		return false, nil
	}
	if e.PushHooks != nil && e.PushHooks.ContentEqual != nil {
		if e.PushHooks.ContentEqual(issue, extIssue) {
			recordPushHash(e, ctx, issue, extRef)
			stats.Skipped++
			return true, nil
		}
		return false, nil
	}
	if !extIssue.UpdatedAt.Before(issue.UpdatedAt) {
		stats.Skipped++
		return true, nil
	}
	return false, nil
}

func handlePushAPIError(e *Engine, err error, action, issueID string, total int, stats *PushStats) (bool, error) {
	if isRateLimitExhausted(err) {
		return false, fmt.Errorf("sync aborted: %w", err)
	}
	e.warn("Failed to %s %s in %s: %v", action, issueID, e.Tracker.DisplayName(), err)
	stats.Errors++
	if isRateLimitedErr(err) {
		e.warnRateLimitAbort(err, remainingPushIssues(total, stats))
		return true, nil
	}
	return false, nil
}

func remainingPushIssues(total int, stats *PushStats) int {
	return total - stats.Created - stats.Updated - stats.Skipped - stats.Errors
}

func collectBatchPushIssues(e *Engine, issues []*types.Issue, opts SyncOptions, descendantSet, skipIDs, forceIDs map[string]bool) ([]*types.Issue, int) {
	pushIssues := make([]*types.Issue, 0, len(issues))
	skipped := 0
	for _, issue := range issues {
		if !batchPushIssueEligible(e, issue, opts, descendantSet, skipIDs, forceIDs) {
			skipped++
			continue
		}
		pushIssues = append(pushIssues, formatPushIssue(e, issue))
	}
	return pushIssues, skipped
}

func batchPushIssueEligible(e *Engine, issue *types.Issue, opts SyncOptions, descendantSet, skipIDs, forceIDs map[string]bool) bool {
	if !pushIssueEligible(e, issue, opts, descendantSet, skipIDs) {
		return false
	}
	extRef := derefStr(issue.ExternalRef)
	willCreate := extRef == "" || !e.Tracker.IsExternalRef(extRef)
	return willCreate || !opts.CreateOnly || forceIDs[issue.ID]
}

func applyBatchPushResult(e *Engine, ctx context.Context, result *BatchPushResult) {
	if result == nil {
		return
	}
	items := append(append([]BatchPushItem(nil), result.Created...), result.Updated...)
	for _, item := range items {
		if item.LocalID == "" || strings.TrimSpace(item.ExternalRef) == "" {
			continue
		}
		updates := map[string]interface{}{"external_ref": strings.TrimSpace(item.ExternalRef)}
		if err := e.Store.UpdateIssue(ctx, item.LocalID, updates, e.Actor); err != nil {
			e.warn("Failed to update external_ref for %s: %v", item.LocalID, err)
		}
	}
}

func renderBatchDryRun(e *Engine, issues []*types.Issue, result *BatchPushResult) {
	if result == nil {
		return
	}
	titles := make(map[string]string, len(issues))
	for _, issue := range issues {
		if issue == nil || issue.ID == "" {
			continue
		}
		titles[issue.ID] = issue.Title
	}
	for _, item := range result.Created {
		e.msg("[dry-run] Would create in %s: %s", e.Tracker.DisplayName(), titles[item.LocalID])
	}
	for _, item := range result.Updated {
		e.msg("[dry-run] Would update in %s: %s", e.Tracker.DisplayName(), titles[item.LocalID])
	}
}
