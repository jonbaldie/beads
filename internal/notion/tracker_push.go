package notion

import (
	"context"
	"fmt"
	"strings"
	"sync"

	itracker "github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
)

type pushOutcome struct {
	created      *itracker.BatchPushItem
	updated      *itracker.BatchPushItem
	skipped      string
	warning      string
	err          *itracker.BatchPushError
	trackerIssue *itracker.TrackerIssue
}

func (t *trackerPush) BatchPush(ctx context.Context, issues []*types.Issue, forceIDs map[string]bool) (*itracker.BatchPushResult, error) {
	return t.executeBatchPush(ctx, issues, forceIDs, false)
}

func (t *trackerPush) BatchPushDryRun(ctx context.Context, issues []*types.Issue, forceIDs map[string]bool) (*itracker.BatchPushResult, error) {
	return t.executeBatchPush(ctx, issues, forceIDs, true)
}

func (t *trackerPush) executeBatchPush(ctx context.Context, issues []*types.Issue, forceIDs map[string]bool, dryRun bool) (*itracker.BatchPushResult, error) {
	if err := t.owner.trackerCache.ensureRemoteIndex(ctx); err != nil {
		return nil, err
	}
	result := &itracker.BatchPushResult{}
	if len(issues) == 0 {
		return result, nil
	}
	for _, outcome := range t.runPushWorkers(ctx, issues, forceIDs, dryRun) {
		t.appendPushOutcome(result, outcome)
	}
	return result, nil
}

func notionPushWorkerCount(issueCount int) int {
	workerCount := defaultBatchPushWorkers
	if issueCount < workerCount {
		workerCount = issueCount
	}
	if workerCount < 1 {
		workerCount = 1
	}
	return workerCount
}

func (t *trackerPush) runPushWorkers(ctx context.Context, issues []*types.Issue, forceIDs map[string]bool, dryRun bool) []pushOutcome {
	workerCount := notionPushWorkerCount(len(issues))
	jobs := make(chan *types.Issue)
	outcomes := make(chan pushOutcome, len(issues))
	var wg sync.WaitGroup

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for issue := range jobs {
				outcomes <- t.pushOne(ctx, issue, forceIDs[issue.ID], dryRun)
			}
		}()
	}

	for _, issue := range issues {
		if issue != nil {
			jobs <- issue
		}
	}
	close(jobs)
	wg.Wait()
	close(outcomes)

	collected := make([]pushOutcome, 0, len(issues))
	for outcome := range outcomes {
		collected = append(collected, outcome)
	}
	return collected
}

func (t *trackerPush) appendPushOutcome(result *itracker.BatchPushResult, outcome pushOutcome) {
	if outcome.created != nil {
		result.Created = append(result.Created, *outcome.created)
	}
	if outcome.updated != nil {
		result.Updated = append(result.Updated, *outcome.updated)
	}
	if strings.TrimSpace(outcome.skipped) != "" {
		result.Skipped = append(result.Skipped, outcome.skipped)
	}
	if strings.TrimSpace(outcome.warning) != "" {
		result.Warnings = append(result.Warnings, outcome.warning)
	}
	if outcome.err != nil {
		result.Errors = append(result.Errors, *outcome.err)
	}
	if outcome.trackerIssue != nil {
		t.owner.trackerCache.upsertRemoteIssue(outcome.trackerIssue)
	}
}

func (t *trackerPush) pushOne(ctx context.Context, issue *types.Issue, force, dryRun bool) pushOutcome {
	if issue == nil {
		return pushOutcome{}
	}
	pushIssue, err := PushIssueFromIssue(issue, t.owner.state.config)
	if err != nil {
		return notionPushError(issue.ID, err)
	}

	remote, hasRemote, warning := t.findRemoteForPush(issue)
	if warning != "" {
		return pushOutcome{
			skipped: issue.ID,
			warning: warning,
		}
	}
	if hasRemote && !force && shouldSkipNotionPush(issue, remote) {
		return pushOutcome{skipped: issue.ID}
	}
	if !hasRemote {
		return t.createOne(ctx, issue, pushIssue, dryRun)
	}
	return t.updateOne(ctx, issue, pushIssue, remote, dryRun)
}

func (t *trackerPush) findRemoteForPush(issue *types.Issue) (*itracker.TrackerIssue, bool, string) {
	extRef := derefStr(issue.ExternalRef)
	pageID := ExtractNotionIdentifier(extRef)
	remote, hasRemote := t.owner.trackerCache.lookupRemoteByPageID(pageID)
	if !hasRemote {
		remote, hasRemote = t.owner.trackerCache.lookupRemoteByLocalID(issue.ID)
	}
	if !hasRemote && strings.TrimSpace(extRef) != "" && pageID != "" {
		return nil, false, fmt.Sprintf("Skipped %s: Notion external_ref points outside the current target; clear external_ref to recreate it in this data source", issue.ID)
	}
	return remote, hasRemote, ""
}

func shouldSkipNotionPush(issue *types.Issue, remote *itracker.TrackerIssue) bool {
	if trackerIssueEqual(issue, remote) {
		return true
	}
	return !remote.UpdatedAt.Before(issue.UpdatedAt)
}

func notionPushError(localID string, err error) pushOutcome {
	return pushOutcome{err: &itracker.BatchPushError{LocalID: localID, Message: err.Error()}}
}

func (t *trackerPush) createOne(ctx context.Context, issue *types.Issue, pushIssue *PushIssue, dryRun bool) pushOutcome {
	if dryRun {
		return pushOutcome{created: &itracker.BatchPushItem{LocalID: issue.ID}}
	}
	page, err := t.owner.state.client.CreatePage(ctx, t.owner.state.dataSourceID, BuildPageProperties(pushIssue))
	if err != nil {
		return notionPushError(issue.ID, err)
	}
	trackerIssue, err := TrackerIssueFromPullIssue(PulledIssueFromPage(*page), t.owner.state.config)
	if err != nil {
		return notionPushError(issue.ID, err)
	}
	ref := firstNonEmpty(t.owner.BuildExternalRef(trackerIssue), trackerIssue.URL)
	return pushOutcome{
		created:      &itracker.BatchPushItem{LocalID: issue.ID, ExternalRef: ref},
		trackerIssue: trackerIssue,
	}
}

func (t *trackerPush) updateOne(ctx context.Context, issue *types.Issue, pushIssue *PushIssue, remote *itracker.TrackerIssue, dryRun bool) pushOutcome {
	if dryRun {
		ref := firstNonEmpty(t.owner.BuildExternalRef(remote), remote.URL)
		return pushOutcome{updated: &itracker.BatchPushItem{LocalID: issue.ID, ExternalRef: ref}}
	}
	page, err := t.owner.state.client.UpdatePage(ctx, remote.ID, BuildPageProperties(pushIssue))
	if err != nil {
		return notionPushError(issue.ID, err)
	}
	trackerIssue, err := TrackerIssueFromPullIssue(PulledIssueFromPage(*page), t.owner.state.config)
	if err != nil {
		return notionPushError(issue.ID, err)
	}
	ref := firstNonEmpty(t.owner.BuildExternalRef(trackerIssue), trackerIssue.URL)
	return pushOutcome{
		updated:      &itracker.BatchPushItem{LocalID: issue.ID, ExternalRef: ref},
		trackerIssue: trackerIssue,
	}
}

func (t *trackerMapping) FieldMapper() itracker.FieldMapper {
	return NewFieldMapper(t.owner.state.config)
}

func (t *trackerMapping) IsExternalRef(ref string) bool {
	return IsNotionExternalRef(ref)
}

func (t *trackerMapping) ExtractIdentifier(ref string) string {
	return ExtractNotionIdentifier(ref)
}

func (t *trackerMapping) BuildExternalRef(issue *itracker.TrackerIssue) string {
	if issue == nil {
		return ""
	}
	if canonical, ok := CanonicalizeNotionPageURL(issue.URL); ok {
		return canonical
	}
	if canonical, ok := CanonicalizeNotionPageURL(issue.ID); ok {
		return canonical
	}
	if canonical, ok := CanonicalizeNotionPageURL(issue.Identifier); ok {
		return canonical
	}
	return ""
}
