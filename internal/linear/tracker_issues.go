package linear

import (
	"context"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
)

func (t *trackerIssues) FetchIssues(ctx context.Context, opts tracker.FetchOptions) ([]tracker.TrackerIssue, error) {
	state := opts.State
	if state == "" {
		state = "all"
	}

	seen := make(map[string]bool)
	var result []tracker.TrackerIssue

	for _, teamID := range t.owner.teamIDs {
		client := t.owner.clients[teamID]
		if client == nil {
			continue
		}

		var issues []Issue
		var err error
		if opts.Since != nil {
			issues, err = client.FetchIssuesSince(ctx, state, *opts.Since)
		} else {
			issues, err = client.FetchIssues(ctx, state)
		}
		if err != nil {
			return result, fmt.Errorf("fetching issues from team %s: %w", teamID, err)
		}

		for _, li := range issues {
			if seen[li.ID] {
				continue
			}
			seen[li.ID] = true
			result = append(result, linearToTrackerIssue(&li))
		}
	}

	return result, nil
}

func (t *trackerIssues) FetchIssue(ctx context.Context, identifier string) (*tracker.TrackerIssue, error) {
	// Try the primary client first (first team), then others.
	for _, teamID := range t.owner.teamIDs {
		client := t.owner.clients[teamID]
		if client == nil {
			continue
		}
		li, err := client.FetchIssueByIdentifier(ctx, identifier)
		if err != nil {
			continue // Issue might belong to a different team.
		}
		if li != nil {
			ti := linearToTrackerIssue(li)
			return &ti, nil
		}
	}
	return nil, nil
}

func (t *trackerIssues) CreateIssue(ctx context.Context, issue *types.Issue) (*tracker.TrackerIssue, error) {
	client := t.owner.primaryClient()
	if client == nil {
		return nil, fmt.Errorf("no Linear client available")
	}

	priority := PriorityToLinear(issue.Priority, t.owner.config)

	stateID, err := t.owner.findStateID(ctx, client, issue.Status)
	if err != nil {
		return nil, fmt.Errorf("finding state for status %s: %w", issue.Status, err)
	}

	labelIDs, err := t.owner.linearLabelIDs(ctx, client, issue)
	if err != nil {
		return nil, err
	}

	// Use issue.Description as-is: the sync engine's FormatDescription hook
	// (BuildLinearDescription) has already merged AcceptanceCriteria/Design/Notes
	// into the description before calling CreateIssue. Calling BuildLinearDescription
	// here a second time would duplicate those sections for issues with structured fields.
	description := issue.Description

	created, err := createLinearIssue(ctx, client, issue, description, priority, stateID, labelIDs)
	if err != nil {
		return nil, err
	}

	ti := linearToTrackerIssue(created)
	return &ti, nil
}

func (t *Tracker) linearLabelIDs(ctx context.Context, client *Client, issue *types.Issue) ([]string, error) {
	labelCache, err := BuildLabelCache(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("loading team labels: %w", err)
	}
	labelIDs, unknown := ResolveLabelIDs(issue, labelCache, t.config)
	for _, name := range unknown {
		fmt.Fprintf(os.Stderr, "linear: bead %s: label %q not found on Linear team (skipped)\n", issue.ID, name)
	}
	return labelIDs, nil
}

func createLinearIssue(ctx context.Context, client *Client, issue *types.Issue, description string, priority int, stateID string, labelIDs []string) (*Issue, error) {
	// Use idempotent creation when we have enough bead metadata to generate a
	// stable marker. This prevents duplicates if sync is interrupted between
	// the API create call and the local external_ref write-back.
	if issue.ID != "" && issue.CreatedBy != "" {
		marker := GenerateIdempotencyMarker(issue.ID, issue.CreatedBy, issue.CreatedAt.UnixNano())
		created, deduped, err := client.CreateIssueIdempotent(ctx, issue.Title, description, priority, stateID, labelIDs, marker)
		if err != nil {
			return nil, err
		}
		if deduped {
			fmt.Fprintf(os.Stderr, "linear: dedup — reusing existing issue %s for bead %s\n", created.Identifier, issue.ID)
		}
		return created, nil
	}
	return client.CreateIssue(ctx, issue.Title, description, priority, stateID, labelIDs)
}

func (t *trackerIssues) UpdateIssue(ctx context.Context, externalID string, issue *types.Issue) (*tracker.TrackerIssue, error) {
	// Route to the correct team's client based on the external ID.
	client := t.owner.clientForExternalID(ctx, externalID)
	if client == nil {
		return nil, fmt.Errorf("cannot determine Linear team for issue %s", externalID)
	}

	labelCache, err := BuildLabelCache(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("loading team labels: %w", err)
	}
	_, unknown := ResolveLabelIDs(issue, labelCache, t.owner.config)
	for _, name := range unknown {
		fmt.Fprintf(os.Stderr, "linear: bead %s: label %q not found on Linear team (skipped)\n", issue.ID, name)
	}
	mapper := &linearFieldMapper{config: t.owner.config, labelCache: labelCache}
	updates := mapper.IssueToTracker(issue)

	// Resolve and include state so status changes are pushed to Linear.
	stateID, err := t.owner.findStateID(ctx, client, issue.Status)
	if err != nil {
		return nil, fmt.Errorf("finding state for status %s: %w", issue.Status, err)
	}
	if stateID != "" {
		updates["stateId"] = stateID
	}

	updated, err := client.UpdateIssue(ctx, externalID, updates)
	if err != nil {
		return nil, err
	}

	ti := linearToTrackerIssue(updated)
	return &ti, nil
}
