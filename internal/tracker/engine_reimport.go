package tracker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
)

// reimportIssue fetches an external version and reapplies its scalar fields.
// It deliberately preserves local labels because conflict reimport has no
// authoritative label collection to synchronize.
func (e *Engine) reimportIssue(ctx context.Context, c Conflict) {
	extIssue, err := e.Tracker.FetchIssue(ctx, c.ExternalIdentifier)
	if err != nil || extIssue == nil {
		e.warn("Failed to re-import %s: %v", c.IssueID, err)
		return
	}

	conv := e.Tracker.FieldMapper().IssueToBeads(extIssue)
	if conv == nil || conv.Issue == nil {
		return
	}

	updates := map[string]interface{}{
		"title":       conv.Issue.Title,
		"description": conv.Issue.Description,
		"priority":    conv.Issue.Priority,
		"status":      string(conv.Issue.Status),
	}
	if extIssue.Metadata != nil {
		if raw, err := json.Marshal(extIssue.Metadata); err == nil {
			updates["metadata"] = json.RawMessage(raw)
		}
	}

	if err := e.Store.RunInIssueLifecycleTransaction(ctx, fmt.Sprintf("bd: reimport update %s", c.IssueID), func(tx storage.IssueLifecycleTransaction) error {
		return applyPullIssueFields(ctx, tx, c.IssueID, updates, e.Actor)
	}); err != nil {
		e.warn("Failed to update %s during reimport: %v", c.IssueID, err)
	}
}
