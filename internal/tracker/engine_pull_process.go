package tracker

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
)

func processPullIssue(s *pullSession, ctx context.Context, extIssue *TrackerIssue) {
	candidate, ok := preparePullCandidate(s, ctx, extIssue)
	if !ok {
		return
	}
	if candidate.Existing != nil && pullIssueEqual(candidate.Existing, candidate.Conv.Issue, candidate.Ref) {
		s.stats.Skipped++
		return
	}
	if s.opts.DryRun {
		previewPullCandidate(s, candidate)
		return
	}
	persistPullCandidate(s, ctx, candidate)
}

func preparePullCandidate(s *pullSession, ctx context.Context, extIssue *TrackerIssue) (*pullCandidate, bool) {
	if s.engine.PullHooks != nil && s.engine.PullHooks.ShouldImport != nil && !s.engine.PullHooks.ShouldImport(extIssue) {
		s.stats.Skipped++
		return nil, false
	}
	ref := s.engine.Tracker.BuildExternalRef(extIssue)
	existing := findExistingPullIssue(s, ctx, ref)
	conv := s.mapper.IssueToBeads(extIssue)
	if conv == nil || conv.Issue == nil {
		s.stats.Skipped++
		return nil, false
	}
	var accepted bool
	existing, accepted = prepareConvertedPullIssue(s, ctx, extIssue, ref, existing, conv)
	if !accepted {
		s.stats.Skipped++
		return nil, false
	}
	s.PendingDeps = appendFilteredDependencies(s.PendingDeps, conv.Dependencies, s.opts.DependencyTypes, s.opts.DependencySources)
	recordPullDryRunIssue(s, ref, conv.Issue)
	return &pullCandidate{ExtIssue: extIssue, Ref: ref, Existing: existing, Conv: conv}, true
}

func prepareConvertedPullIssue(s *pullSession, ctx context.Context, extIssue *TrackerIssue, ref string, existing *types.Issue, conv *IssueConversion) (*types.Issue, bool) {
	existing = resolvedPullExisting(s, existing, conv.Issue)
	if s.engine.PullHooks != nil && s.engine.PullHooks.TransformIssue != nil {
		s.engine.PullHooks.TransformIssue(conv.Issue)
	}
	if !generatePullID(s, ctx, extIssue, conv.Issue) {
		return nil, false
	}
	if pullConflictsWithLocalEdit(s, existing) {
		return nil, false
	}
	if !runPullAfterConvert(s, ctx, extIssue, conv, ref, existing) {
		return nil, false
	}
	return existing, true
}

func resolvedPullExisting(s *pullSession, existing *types.Issue, converted *types.Issue) *types.Issue {
	if existing != nil {
		return existing
	}
	if localID := strings.TrimSpace(converted.ID); localID != "" {
		return s.localByID[localID]
	}
	return nil
}

func generatePullID(s *pullSession, ctx context.Context, extIssue *TrackerIssue, issue *types.Issue) bool {
	if s.engine.PullHooks == nil || s.engine.PullHooks.GenerateID == nil {
		return true
	}
	if err := s.engine.PullHooks.GenerateID(ctx, issue); err != nil {
		s.engine.warn("Failed to generate ID for %s: %v", extIssue.Identifier, err)
		return false
	}
	return true
}

func runPullAfterConvert(s *pullSession, ctx context.Context, extIssue *TrackerIssue, conv *IssueConversion, ref string, existing *types.Issue) bool {
	if s.engine.PullHooks == nil || s.engine.PullHooks.AfterConvert == nil {
		return true
	}
	if err := s.engine.PullHooks.AfterConvert(ctx, extIssue, conv, ref, existing, s.opts); err != nil {
		s.engine.warn("Failed to prepare %s: %v", extIssue.Identifier, err)
		return false
	}
	return true
}

func findExistingPullIssue(s *pullSession, ctx context.Context, ref string) *types.Issue {
	existing, _ := s.engine.Store.GetIssueByExternalRef(ctx, ref)
	if existing != nil || ref == "" {
		return existing
	}
	identifier := s.engine.Tracker.ExtractIdentifier(ref)
	if identifier == "" {
		return nil
	}
	return s.localByExternalIdentifier[identifier]
}

func pullConflictsWithLocalEdit(s *pullSession, existing *types.Issue) bool {
	if existing == nil || s.lastSync == nil {
		return false
	}
	return existing.UpdatedAt.After(*s.lastSync) &&
		!s.overwriteIDs[existing.ID] && !s.prelinkedHydrateIDs[existing.ID]
}

func recordPullDryRunIssue(s *pullSession, ref string, issue *types.Issue) {
	if !s.opts.DryRun {
		return
	}
	dryRunIssue := *issue
	if strings.TrimSpace(ref) != "" {
		dryRunIssue.ExternalRef = strPtr(ref)
	}
	s.DryRunIssues = append(s.DryRunIssues, &dryRunIssue)
}

func previewPullCandidate(s *pullSession, candidate *pullCandidate) {
	if candidate.Existing != nil {
		s.engine.msg("[dry-run] Would update local issue: %s - %s", candidate.ExtIssue.Identifier, ui.SanitizeForTerminal(candidate.ExtIssue.Title))
		s.stats.Updated++
		return
	}
	s.engine.msg("[dry-run] Would import: %s - %s", candidate.ExtIssue.Identifier, ui.SanitizeForTerminal(candidate.ExtIssue.Title))
	s.stats.Created++
}

func persistPullCandidate(s *pullSession, ctx context.Context, candidate *pullCandidate) {
	if candidate.Existing != nil {
		updateExistingPullIssue(s, ctx, candidate)
		return
	}
	createNewPullIssue(s, ctx, candidate)
}

func updateExistingPullIssue(s *pullSession, ctx context.Context, candidate *pullCandidate) {
	updates := buildPullIssueUpdates(candidate.Existing, candidate.Conv.Issue, candidate.Ref)
	if raw, ok := marshalTrackerMetadata(candidate.ExtIssue.Metadata); ok {
		updates["metadata"] = raw
	}
	err := s.engine.Store.RunInIssueLifecycleTransaction(ctx, fmt.Sprintf("bd: pull update %s", candidate.Existing.ID), func(tx storage.IssueLifecycleTransaction) error {
		return applyPullIssueUpdate(ctx, tx, candidate.Existing.ID, updates, candidate.Conv.Issue.Labels, s.engine.Actor)
	})
	if err != nil {
		s.engine.warn("Failed to update %s: %v", candidate.Existing.ID, err)
		s.stats.Errors++
		markPulledIssue(s, candidate.Existing.ID)
		return
	}
	s.stats.Updated++
	markPulledIssue(s, candidate.Existing.ID)
}

func createNewPullIssue(s *pullSession, ctx context.Context, candidate *pullCandidate) {
	candidate.Conv.Issue.ExternalRef = strPtr(candidate.Ref)
	if raw, ok := marshalTrackerMetadata(candidate.ExtIssue.Metadata); ok {
		candidate.Conv.Issue.Metadata = raw
	}
	if err := s.engine.Store.CreateIssue(ctx, candidate.Conv.Issue, s.engine.Actor); err != nil {
		s.engine.warn("Failed to create issue for %s: %v", candidate.ExtIssue.Identifier, err)
		return
	}
	s.stats.Created++
	markPulledIssue(s, candidate.Conv.Issue.ID)
}

func markPulledIssue(s *pullSession, id string) {
	if s.pulledIDs != nil {
		s.pulledIDs[id] = true
	}
}

func finishPull(s *pullSession, ctx context.Context) {
	depErrors := 0
	if s.opts.DryRun {
		depErrors = previewDependencies(s.engine, ctx, s.PendingDeps, s.DryRunIssues)
	} else {
		depErrors = createDependencies(s.engine, ctx, s.PendingDeps)
	}
	s.stats.Skipped += depErrors
}
