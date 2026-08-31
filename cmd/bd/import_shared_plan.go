package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/types"
)

// importChangePlan reports how the import batch relates to existing local
// issues, so the import can surface what it changed instead of doing it
// silently (bd-hj85c).
type importChangePlan struct {
	// Updates lists existing issues the batch will rewrite: incoming row
	// strictly newer and row content differs.
	Updates []ImportChange
	// TieKeptLocal lists incoming rows with the same updated_at as the
	// local issue but different row content. The stale-guarded upsert keeps
	// every stored column for these (second-granularity timestamp tie),
	// while their aux data still merges.
	TieKeptLocal []string
	// NewIDs lists incoming rows with no local match (would-create), deduped
	// by ID for display and excluding title-only rows that carry no ID at
	// all. NewCount is the authoritative row count for those same rows: it
	// counts every classified-new row, including duplicate IDs and ID-less
	// rows, so dry-run counts sum to the number of rows considered instead
	// of undercounting when NewIDs collapses duplicates.
	NewIDs []string
	// NewCount is the number of incoming rows classified as new. See NewIDs.
	NewCount int
}

type importNewIssueCollector struct {
	plan *importChangePlan
	seen map[string]struct{}
}

func (c *importNewIssueCollector) add(id string) {
	c.plan.NewCount++
	if id == "" {
		// Title-only rows have no ID to look up or report — they always
		// create, but there's nothing to add to the display list.
		return
	}
	if _, duplicate := c.seen[id]; duplicate {
		return
	}
	c.seen[id] = struct{}{}
	c.plan.NewIDs = append(c.plan.NewIDs, id)
}

func (c *importNewIssueCollector) addAll(issues []*types.Issue) {
	for _, issue := range issues {
		if issue != nil {
			c.add(issue.ID)
		}
	}
}

func collectImportIssueIDs(issues []*types.Issue) []string {
	ids := make([]string, 0, len(issues))
	seen := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if issue == nil || issue.ID == "" {
			continue
		}
		if _, duplicate := seen[issue.ID]; duplicate {
			continue
		}
		seen[issue.ID] = struct{}{}
		ids = append(ids, issue.ID)
	}
	return ids
}

func loadImportIssuesByID(ctx context.Context, store importIssueLookup, ids []string, requireTimestamp bool) (map[string]*types.Issue, error) {
	localByID := make(map[string]*types.Issue)
	if len(ids) == 0 {
		return localByID, nil
	}

	localIssues, err := store.GetIssuesByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("check existing issues before import: %w", err)
	}
	for _, issue := range localIssues {
		if issue == nil || issue.ID == "" {
			continue
		}
		if requireTimestamp && issue.UpdatedAt.IsZero() {
			continue
		}
		localByID[issue.ID] = issue
	}
	return localByID, nil
}

func filterStaleImportIssues(ctx context.Context, store importIssueLookup, issues []*types.Issue) ([]*types.Issue, []string, importChangePlan, error) {
	var plan importChangePlan
	newIssues := importNewIssueCollector{plan: &plan, seen: make(map[string]struct{})}
	ids := collectImportIssueIDs(issues)

	if len(ids) == 0 {
		// There is no ID to look up, but title-only rows still create on
		// execution. Classify each non-nil row before the short-circuit so a
		// dry run reports it as created rather than unchanged.
		newIssues.addAll(issues)
		return issues, nil, plan, nil
	}

	localByID, err := loadImportIssuesByID(ctx, store, ids, true)
	if err != nil {
		return nil, nil, plan, err
	}

	if len(localByID) == 0 {
		// Nothing matched locally, so every considered row is new.
		newIssues.addAll(issues)
		return issues, nil, plan, nil
	}

	filtered := make([]*types.Issue, 0, len(issues))
	skippedIDs := make([]string, 0)
	for _, issue := range issues {
		keep, skippedID := classifyStaleImportIssue(issue, localByID, &plan, &newIssues)
		if !keep {
			skippedIDs = append(skippedIDs, skippedID)
			continue
		}
		filtered = append(filtered, issue)
	}
	return filtered, skippedIDs, plan, nil
}

func classifyStaleImportIssue(issue *types.Issue, localByID map[string]*types.Issue, plan *importChangePlan, newIssues *importNewIssueCollector) (bool, string) {
	if issue == nil {
		return true, ""
	}
	if importIssueHasNoTimestamp(issue) {
		// No incoming timestamp to stale-check (or, for a title-only row, no
		// ID at all): these rows still write on execution, so classify them
		// via an existence lookup instead of silently falling through as
		// unchanged (GH#4901 follow-up).
		if local, ok := localByID[issue.ID]; ok {
			plan.Updates = append(plan.Updates, ImportChange{
				ID:      issue.ID,
				Changes: importRowChangeSummary(local, issue),
			})
		} else {
			newIssues.add(issue.ID)
		}
		return true, ""
	}

	local, ok := localByID[issue.ID]
	if !ok {
		newIssues.add(issue.ID)
		return true, ""
	}
	// Compare at second granularity: updated_at is DATETIME(0) in the store,
	// so a sub-second component on the JSONL side must not turn a tie into a
	// spurious "newer" classification.
	incomingAt := issue.UpdatedAt.UTC().Truncate(time.Second)
	localAt := local.UpdatedAt.UTC().Truncate(time.Second)
	if incomingAt.Before(localAt) {
		return false, issue.ID
	}
	if summary := importRowChangeSummary(local, issue); summary != "" {
		if incomingAt.Equal(localAt) {
			plan.TieKeptLocal = append(plan.TieKeptLocal, issue.ID)
		} else {
			plan.Updates = append(plan.Updates, ImportChange{ID: issue.ID, Changes: summary})
		}
	}
	return true, ""
}

func importIssueHasNoTimestamp(issue *types.Issue) bool {
	return issue.ID == "" || issue.UpdatedAt.IsZero()
}

// classifyImportIssuesExistence classifies incoming rows as created or
// updated purely by whether their ID already exists locally, without the
// staleness policy: the --allow-stale dry-run path (like the real
// --allow-stale write) imports every row regardless of timestamp ordering,
// so no row is ever stale-skipped or tie-kept — existence is the only
// question.
func classifyImportIssuesExistence(ctx context.Context, store importIssueLookup, issues []*types.Issue) (importChangePlan, error) {
	var plan importChangePlan
	ids := collectImportIssueIDs(issues)
	localByID, err := loadImportIssuesByID(ctx, store, ids, false)
	if err != nil {
		return plan, err
	}
	newIssues := importNewIssueCollector{plan: &plan, seen: make(map[string]struct{})}
	for _, issue := range issues {
		classifyImportIssueExistence(issue, localByID, &plan, &newIssues)
	}
	return plan, nil
}

func classifyImportIssueExistence(issue *types.Issue, localByID map[string]*types.Issue, plan *importChangePlan, newIssues *importNewIssueCollector) {
	if issue == nil {
		return
	}
	local, ok := localByID[issue.ID]
	if !ok {
		newIssues.add(issue.ID)
		return
	}
	plan.Updates = append(plan.Updates, ImportChange{
		ID:      issue.ID,
		Changes: importRowChangeSummary(local, issue),
	})
}

// classifyDryRunImport runs the same id lookup as a real import, without
// writing anything, so --dry-run can report create/update/skip counts
// instead of treating every row as a create (GH#4901).
func classifyDryRunImport(ctx context.Context, store importIssueLookup, issues []*types.Issue, allowStale bool) (*ImportResult, error) {
	if len(issues) == 0 {
		return &ImportResult{}, nil
	}
	if allowStale {
		// Matches the real path: --allow-stale skips the stale guard
		// entirely, so a row is never stale-skipped or tie-kept here — but a
		// row matching an existing local issue still writes as an update,
		// not a create (GH#4901 follow-up).
		plan, err := classifyImportIssuesExistence(ctx, store, issues)
		if err != nil {
			return nil, err
		}
		return &ImportResult{
			Created:       plan.NewCount,
			Updated:       len(plan.Updates),
			ImportedIDs:   plan.NewIDs,
			UpdatedIssues: plan.Updates,
		}, nil
	}

	filtered, staleSkippedIDs, plan, err := filterStaleImportIssues(ctx, store, issues)
	if err != nil {
		return nil, err
	}
	// TieKeptLocal rows are not rewritten (the stale-guarded upsert keeps
	// every stored column), so they belong in Unchanged, not Updated —
	// they're still reported separately via TieKeptLocalIDs.
	created := plan.NewCount
	updated := len(plan.Updates)
	return &ImportResult{
		Created:         created,
		Updated:         updated,
		Unchanged:       len(filtered) - created - updated,
		Skipped:         len(staleSkippedIDs),
		ImportedIDs:     plan.NewIDs,
		StaleSkippedIDs: staleSkippedIDs,
		UpdatedIssues:   plan.Updates,
		TieKeptLocalIDs: plan.TieKeptLocal,
	}, nil
}

// importRowChangeSummary summarizes the differences between the local issue
// row and the incoming import row, restricted to the columns the import
// upsert rewrites. Returns "" when none of those fields differ. Status,
// priority, and type transitions show old → new; long-form fields are listed
// by name only.
func importRowChangeSummary(local, incoming *types.Issue) string {
	var parts []string
	parts = appendImportStatusChanges(parts, local, incoming)
	parts = appendImportContentChanges(parts, local, incoming)
	parts = appendImportAuxiliaryChanges(parts, local, incoming)
	return strings.Join(parts, ", ")
}

func appendImportStatusChanges(parts []string, local, incoming *types.Issue) []string {
	if local.Status != incoming.Status {
		parts = append(parts, fmt.Sprintf("status %s → %s", local.Status, incoming.Status))
	}
	if local.Priority != incoming.Priority {
		parts = append(parts, fmt.Sprintf("priority %d → %d", local.Priority, incoming.Priority))
	}
	if local.IssueType != incoming.IssueType {
		parts = append(parts, fmt.Sprintf("type %s → %s", local.IssueType, incoming.IssueType))
	}
	return parts
}

func appendImportContentChanges(parts []string, local, incoming *types.Issue) []string {
	if local.Assignee != incoming.Assignee {
		parts = append(parts, "assignee")
	}
	if local.Title != incoming.Title {
		parts = append(parts, "title")
	}
	if local.Description != incoming.Description {
		parts = append(parts, "description")
	}
	if local.Design != incoming.Design {
		parts = append(parts, "design")
	}
	if local.AcceptanceCriteria != incoming.AcceptanceCriteria {
		parts = append(parts, "acceptance_criteria")
	}
	if local.Notes != incoming.Notes {
		if incoming.Notes == "" {
			parts = append(parts, "notes cleared")
		} else {
			parts = append(parts, "notes")
		}
	}
	if local.CloseReason != incoming.CloseReason {
		parts = append(parts, "close_reason")
	}
	return parts
}

func appendImportAuxiliaryChanges(parts []string, local, incoming *types.Issue) []string {
	if !stringPtrEqual(local.ExternalRef, incoming.ExternalRef) {
		parts = append(parts, "external_ref")
	}
	if !intPtrEqual(local.EstimatedMinutes, incoming.EstimatedMinutes) {
		parts = append(parts, "estimate")
	}
	if string(local.Metadata) != string(incoming.Metadata) {
		parts = append(parts, "metadata")
	}
	return parts
}

func stringPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
