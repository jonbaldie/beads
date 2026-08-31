package tracker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/jonbaldie/beads/internal/types"
)

// doPull imports issues from the external tracker into beads.
// doPull imports tracker issues. IDs of issues it creates or updates are
// added to pulledIDs so a bidirectional sync's push phase does not echo the
// freshly pulled content straight back to the tracker.
func doPull(e *Engine, ctx context.Context, opts SyncOptions, allowOverwriteIDs, pulledIDs map[string]bool) (*PullStats, error) {
	ctx, span := syncTracer.Start(ctx, "tracker.pull",
		trace.WithAttributes(
			attribute.String("sync.tracker", e.Tracker.DisplayName()),
			attribute.Bool("sync.dry_run", opts.DryRun),
		),
	)
	defer span.End()

	session, extIssues, err := preparePull(e, ctx, opts, allowOverwriteIDs, pulledIDs)
	if err != nil {
		return nil, err
	}
	for index := range extIssues {
		processPullIssue(session, ctx, &extIssues[index])
	}
	finishPull(session, ctx)

	span.SetAttributes(
		attribute.Int("sync.created", session.stats.Created),
		attribute.Int("sync.updated", session.stats.Updated),
		attribute.Int("sync.skipped", session.stats.Skipped),
	)
	return session.stats, nil
}

type pullSession struct {
	engine                    *Engine
	opts                      SyncOptions
	stats                     *PullStats
	lastSync                  *time.Time
	localIssues               []*types.Issue
	localByExternalIdentifier map[string]*types.Issue
	localByID                 map[string]*types.Issue
	prelinkedHydrateIDs       map[string]bool
	pulledIDs                 map[string]bool
	overwriteIDs              map[string]bool
	mapper                    FieldMapper
	PendingDeps               []DependencyInfo
	DryRunIssues              []*types.Issue
}

type pullCandidate struct {
	ExtIssue *TrackerIssue
	Ref      string
	Existing *types.Issue
	Conv     *IssueConversion
}

func preparePull(e *Engine, ctx context.Context, opts SyncOptions, allowOverwriteIDs, pulledIDs map[string]bool) (*pullSession, []TrackerIssue, error) {
	session := &pullSession{
		engine:              e,
		opts:                opts,
		stats:               &PullStats{},
		prelinkedHydrateIDs: make(map[string]bool),
		pulledIDs:           pulledIDs,
		overwriteIDs:        allowOverwriteIDs,
	}
	loadPullCursor(session, ctx)
	if err := indexPullLocalIssues(session, ctx); err != nil {
		return nil, nil, err
	}
	extIssues, err := fetchPullIssues(session, ctx)
	if err != nil {
		return nil, nil, err
	}
	session.mapper = e.Tracker.FieldMapper()
	return session, extIssues, nil
}

func loadPullCursor(s *pullSession, ctx context.Context) {
	key := s.engine.Tracker.ConfigPrefix() + ".last_sync"
	lastSyncStr, err := s.engine.Store.GetLocalMetadata(ctx, key)
	if err != nil || lastSyncStr == "" {
		return
	}
	t, err := parseSyncTime(lastSyncStr)
	if err != nil {
		return
	}
	s.lastSync = &t
	s.stats.Incremental = true
	s.stats.SyncedSince = lastSyncStr
}

func indexPullLocalIssues(s *pullSession, ctx context.Context) error {
	localIssues, err := s.engine.Store.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return fmt.Errorf("searching local issues: %w", err)
	}
	s.localIssues = localIssues
	s.localByExternalIdentifier = make(map[string]*types.Issue, len(localIssues))
	s.localByID = make(map[string]*types.Issue, len(localIssues))
	for _, localIssue := range localIssues {
		if localIssue == nil {
			continue
		}
		if localID := strings.TrimSpace(localIssue.ID); localID != "" {
			s.localByID[localID] = localIssue
		}
		if localIssue.ExternalRef == nil {
			continue
		}
		localRef := strings.TrimSpace(*localIssue.ExternalRef)
		if localRef == "" || !s.engine.Tracker.IsExternalRef(localRef) {
			continue
		}
		identifier := s.engine.Tracker.ExtractIdentifier(localRef)
		if identifier != "" {
			s.localByExternalIdentifier[identifier] = localIssue
		}
	}
	return nil
}

func fetchPullIssues(s *pullSession, ctx context.Context) ([]TrackerIssue, error) {
	if len(s.opts.IssueIDs) > 0 {
		return fetchSelectivePullIssues(s, ctx)
	}
	return fetchBulkPullIssues(s, ctx)
}

func fetchSelectivePullIssues(s *pullSession, ctx context.Context) ([]TrackerIssue, error) {
	prefix, _ := s.engine.Store.GetConfig(ctx, "issue_prefix")
	extIssues := make([]TrackerIssue, 0, len(s.opts.IssueIDs))
	for _, id := range s.opts.IssueIDs {
		identifier, ok := selectivePullIdentifier(s, id, prefix)
		if !ok {
			continue
		}
		extIssue, err := s.engine.Tracker.FetchIssue(ctx, identifier)
		if err != nil {
			s.engine.warn("Failed to fetch %s: %v", identifier, err)
			s.stats.Errors++
			continue
		}
		if extIssue == nil {
			s.engine.warn("Issue %s not found in %s", identifier, s.engine.Tracker.DisplayName())
			s.stats.Skipped++
			continue
		}
		extIssues = append(extIssues, *extIssue)
	}
	s.stats.Candidates = len(extIssues)
	return extIssues, nil
}

func selectivePullIdentifier(s *pullSession, id, prefix string) (string, bool) {
	if !isBeadID(id, prefix) {
		return id, true
	}
	local := s.localByID[id]
	if local != nil && local.ExternalRef != nil {
		if identifier := s.engine.Tracker.ExtractIdentifier(*local.ExternalRef); identifier != "" {
			return identifier, true
		}
	}
	s.engine.warn("No external ref found for local issue %s, skipping pull", id)
	s.stats.Skipped++
	return "", false
}

func fetchBulkPullIssues(s *pullSession, ctx context.Context) ([]TrackerIssue, error) {
	fetchOpts := FetchOptions{State: s.opts.State}
	if s.lastSync != nil {
		fetchOpts.Since = s.lastSync
	}
	extIssues, err := s.engine.Tracker.FetchIssues(ctx, fetchOpts)
	if err != nil {
		return nil, fmt.Errorf("fetching issues: %w", err)
	}
	s.stats.Candidates = len(extIssues)
	if provider, ok := s.engine.Tracker.(PullStatsProvider); ok {
		s.stats.Queried, s.stats.Candidates = provider.LastPullStats()
	}
	hydrated, hydratedLocalIDs, err := fetchPrelinkedIssues(s.engine, ctx, extIssues, s.localIssues, s.lastSync)
	if err != nil {
		return nil, fmt.Errorf("hydrating pre-linked %s issues: %w", s.engine.Tracker.DisplayName(), err)
	}
	extIssues = append(extIssues, hydrated...)
	s.stats.Candidates += len(hydrated)
	for id := range hydratedLocalIDs {
		s.prelinkedHydrateIDs[id] = true
	}
	return extIssues, nil
}
