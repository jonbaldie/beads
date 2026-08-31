package linear

import (
	"context"
	"errors"
	"fmt"
)

// fetchIssueAcrossTeams locates an issue by its Linear identifier across
// all configured team clients. Single-team setups hit the primary client
// directly; multi-team setups fall through each client in order until one
// returns a non-nil issue. Returns (issue, hostClient, nil) on success;
// the hostClient is the team's client that owned the issue and should be
// reused for subsequent mutations (so the update path doesn't re-probe
// and silently fall back to the wrong client on transient probe errors —
// see clientForExternalID's fallback behavior in tracker.go).
//
// Returns (nil, nil, nil) when no team has the issue.
func (t *trackerParent) fetchIssueAcrossTeams(ctx context.Context, identifier string) (*Issue, *Client, error) {
	if identifier == "" {
		return nil, nil, nil
	}
	if len(t.owner.teamIDs) <= 1 {
		return t.fetchIssueFromPrimaryTeam(ctx, identifier)
	}
	return t.fetchIssueFromAllTeams(ctx, identifier)
}

func (t *trackerParent) fetchIssueFromPrimaryTeam(ctx context.Context, identifier string) (*Issue, *Client, error) {
	client := t.owner.primaryClient()
	if client == nil {
		return nil, nil, errors.New("no Linear client available")
	}
	li, err := client.FetchIssueByIdentifier(ctx, identifier)
	if err != nil || li == nil {
		return li, nil, err
	}
	return li, client, nil
}

func (t *trackerParent) fetchIssueFromAllTeams(ctx context.Context, identifier string) (*Issue, *Client, error) {
	// Multi-team: try each client. First non-nil result wins. Rate-limit
	// errors abort immediately (the cross-team probe shouldn't burn
	// through quota when the circuit breaker has already tripped).
	for _, teamID := range t.owner.teamIDs {
		client := t.owner.clients[teamID]
		if client == nil {
			continue
		}
		li, err := client.FetchIssueByIdentifier(ctx, identifier)
		if err != nil {
			if isRateLimitExhausted(err) {
				return nil, nil, err
			}
			continue
		}
		if li != nil {
			return li, client, nil
		}
	}
	return nil, nil, nil
}

// isRateLimitExhausted returns true when err (or any error it wraps)
// signals that Linear's rate-limit circuit breaker has tripped.
// Mirrors internal/tracker/engine.go isRateLimitExhausted, duplicated
// here to avoid an import cycle.
func isRateLimitExhausted(err error) bool {
	if err == nil {
		return false
	}
	type rle interface{ RateLimitExhausted() bool }
	var r rle
	return errors.As(err, &r) && r.RateLimitExhausted()
}

// ParentLink describes a desired parent-child relationship to wire up
// on the Linear side. Both fields are Linear identifiers (e.g. "HOU-159")
// — typically extracted from each bead's external_ref via
// Tracker.ExtractIdentifier.
type ParentLink struct {
	ChildIdentifier  string
	ParentIdentifier string
}

// ParentReconcileStats summarizes a ReconcileParents run.
type ParentReconcileStats struct {
	// Updated is the count of Linear issues whose parent field was changed
	// (set, cleared, or rewired) by this pass. Zero in dry-run mode — see
	// WouldUpdate for the dry-run counterpart.
	Updated int
	// WouldUpdate is the count of mutations the pass WOULD have issued
	// in wet-run. Populated only in dry-run mode; zero in wet-run.
	WouldUpdate int
	// Mutations is the (child, parent) link list that was applied (wet-run)
	// or would have been applied (dry-run). In wet-run, an entry is appended
	// only after the IssueUpdate API call succeeds — so this list reflects
	// state actually propagated to Linear, not attempted. In dry-run, all
	// candidates that pass the idempotency check appear. Lets callers print
	// per-link detail without having to re-derive it.
	Mutations []ParentLink
	// Skipped is the count of links where Linear's parent already matched
	// the desired value — no API mutation was issued.
	Skipped int
	// NotFound is the list of identifiers (child or parent) that didn't
	// resolve to a Linear issue (typically because their bead has no
	// external_ref yet, or the Linear issue was deleted out-of-band).
	// Their links are silently skipped; the next sync will retry.
	NotFound []string
	// Errors collects per-link failures that did not abort the pass.
	Errors []error
}

type parentIssueEntry struct {
	issue  *Issue
	client *Client
}

type parentIssueCache struct {
	tracker *trackerParent
	ctx     context.Context
	issues  map[string]parentIssueEntry
}

func newParentIssueCache(ctx context.Context, tracker *trackerParent, capacity int) *parentIssueCache {
	return &parentIssueCache{
		tracker: tracker,
		ctx:     ctx,
		issues:  make(map[string]parentIssueEntry, capacity),
	}
}

func (c *parentIssueCache) fetch(identifier string) (parentIssueEntry, error) {
	if cached, ok := c.issues[identifier]; ok {
		return cached, nil
	}
	issue, client, err := c.tracker.fetchIssueAcrossTeams(c.ctx, identifier)
	if err != nil {
		return parentIssueEntry{}, err
	}
	entry := parentIssueEntry{issue: issue, client: client}
	// Cache nil too — repeated lookups are still cheap.
	c.issues[identifier] = entry
	return entry, nil
}

// ReconcileParents wires parent-child relationships from bead-side
// dependencies to Linear's parent issue field. Used as a post-sync pass
// to handle two cases that the per-issue create/update path can't cover:
//
//  1. New tree push: when a child is created before its parent's external
//     ref is known to the engine, the create call has no parentId to send.
//     This pass closes the loop after every issue in the tree has an
//     external ref.
//  2. Orphan repair: existing Linear issues that were created (in earlier
//     bd versions or by interrupted syncs) without parentId can be wired
//     up retroactively.
//
// Idempotent: fetches each child's current parent and only issues
// IssueUpdate when the remote parent differs from the desired value.
// Each unique parent identifier is fetched once to resolve its UUID
// (IssueUpdateInput.parentId requires the internal UUID, not the
// human-readable identifier).
//
// When dryRun is true, the read-only fetches still run (so the caller
// gets accurate Skipped / NotFound counts and a populated Mutations
// list for preview output) but the IssueUpdate mutation is skipped.
// Mutations that would have fired increment stats.WouldUpdate instead
// of stats.Updated.
//
// Returns nil error when the pass completed (even if per-link errors
// were collected in Stats.Errors). A non-nil error indicates a setup-level
// failure that prevented any work from running.
func (t *trackerParent) ReconcileParents(ctx context.Context, links []ParentLink, dryRun bool) (*ParentReconcileStats, error) {
	stats := &ParentReconcileStats{}
	if len(links) == 0 {
		return stats, nil
	}
	if t.owner.primaryClient() == nil {
		return nil, errors.New("no Linear client available")
	}
	fetched := newParentIssueCache(ctx, t, len(links)*2)
	for _, link := range links {
		if err := t.reconcileParentLink(ctx, fetched, link, dryRun, stats); err != nil {
			return stats, err
		}
	}

	return stats, nil
}

func (t *trackerParent) reconcileParentLink(ctx context.Context, fetched *parentIssueCache, link ParentLink, dryRun bool, stats *ParentReconcileStats) error {
	if link.ChildIdentifier == "" || link.ParentIdentifier == "" {
		return nil
	}
	child, parent, ok, err := lookupParentPair(fetched, link, stats)
	if err != nil || !ok {
		return err
	}
	if child.issue.Parent != nil && child.issue.Parent.ID == parent.issue.ID {
		stats.Skipped++
		return nil
	}
	if dryRun {
		stats.Mutations = append(stats.Mutations, link)
		stats.WouldUpdate++
		return nil
	}
	return t.applyParentMutation(ctx, fetched, link, child, parent, stats)
}

func lookupParentPair(fetched *parentIssueCache, link ParentLink, stats *ParentReconcileStats) (parentIssueEntry, parentIssueEntry, bool, error) {
	child, ok, err := lookupParentIssue(fetched, link.ChildIdentifier, "child", stats)
	if err != nil || !ok {
		return parentIssueEntry{}, parentIssueEntry{}, false, err
	}
	parent, ok, err := lookupParentIssue(fetched, link.ParentIdentifier, "parent", stats)
	if err != nil || !ok {
		return parentIssueEntry{}, parentIssueEntry{}, false, err
	}
	return child, parent, true, nil
}

func lookupParentIssue(fetched *parentIssueCache, identifier, role string, stats *ParentReconcileStats) (parentIssueEntry, bool, error) {
	entry, err := fetched.fetch(identifier)
	if err == nil {
		if entry.issue == nil {
			stats.NotFound = append(stats.NotFound, identifier)
			return parentIssueEntry{}, false, nil
		}
		return entry, true, nil
	}
	if isRateLimitExhausted(err) {
		return parentIssueEntry{}, false, fmt.Errorf("fetch %s %s: %w", role, identifier, err)
	}
	stats.Errors = append(stats.Errors, fmt.Errorf("fetch %s %s: %w", role, identifier, err))
	return parentIssueEntry{}, false, nil
}

func (t *trackerParent) applyParentMutation(ctx context.Context, fetched *parentIssueCache, link ParentLink, child, parent parentIssueEntry, stats *ParentReconcileStats) error {
	updated, err := child.client.UpdateIssue(ctx, child.issue.ID, map[string]interface{}{"parentId": parent.issue.ID})
	if err != nil {
		if isRateLimitExhausted(err) {
			return fmt.Errorf("set parent of %s → %s: %w", link.ChildIdentifier, link.ParentIdentifier, err)
		}
		stats.Errors = append(stats.Errors, fmt.Errorf("set parent of %s → %s: %w", link.ChildIdentifier, link.ParentIdentifier, err))
		return nil
	}
	if updated != nil {
		fetched.issues[link.ChildIdentifier] = parentIssueEntry{issue: updated, client: child.client}
	}
	stats.Mutations = append(stats.Mutations, link)
	stats.Updated++
	return nil
}
