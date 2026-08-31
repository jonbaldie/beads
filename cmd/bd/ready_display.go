package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/workapi"
	"github.com/jonbaldie/beads/issueops"
)

func hasStoredBlockedStatus(ctx context.Context, search func(context.Context, string, types.IssueFilter) ([]*types.Issue, error)) bool {
	if search == nil {
		return false
	}
	st := types.StatusBlocked
	held, err := search(ctx, "", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Status: &st, Limit: 1}})
	return err == nil && len(held) > 0
}

func printReadyEmptyHuman(hasOpenIssues, hasStoredBlocked bool) {
	switch {
	case hasOpenIssues:
		fmt.Printf("\n%s No ready work found (all issues have blocking dependencies)\n\n", ui.RenderWarn("○"))
	case hasStoredBlocked:
		fmt.Printf("\n%s No ready work found (remaining issues have stored status blocked; use 'bd blocked' or 'bd update <id> --claim' to resume)\n\n", ui.RenderWarn("○"))
	default:
		fmt.Printf("\n%s No open issues\n\n", ui.RenderPass("○"))
	}
}

func printBlockedHuman(blocked []*types.BlockedIssue) {
	if len(blocked) == 0 {
		fmt.Printf("\n%s No blocked issues\n\n", ui.RenderPass("○"))
		return
	}
	fmt.Printf("\n%s Blocked issues (%d):\n\n", ui.RenderFail("●"), len(blocked))
	for _, issue := range blocked {
		fmt.Printf("[%s] %s: %s\n",
			ui.RenderPriority(issue.Priority),
			ui.RenderID(issue.ID), issue.Title)
		if issue.BlockedByCount == 0 {
			fmt.Printf("  Stored status blocked (no open dependencies). Resume with: bd update %s --claim\n", issue.ID)
		} else {
			blockedBy := issue.BlockedBy
			if blockedBy == nil {
				blockedBy = []string{}
			}
			fmt.Printf("  Blocked by %d open dependencies: %v\n", issue.BlockedByCount, blockedBy)
		}
		fmt.Println()
	}
}

// readyTotal sizes the whole ready set for the request `bd ready` just listed
// a page of, through the store's own ReadyCounter accessor.
//
// BOTH OUTPUT MODES CALL IT and only when the page came back full, which is
// the one situation where the answer can differ from what is already on
// screen. The role has no --max-rows field to honor and needs none: the cap
// bounds a page this machine materializes, and a count materializes no rows.
//
// A failed count is not a failed command — the page is already correct; all
// that is lost is the "of N" beside it.
func readyTotal(ctx context.Context, activeStore storage.DoltStorage, in readyInput) (int, error) {
	counter, err := activeStore.ReadyCounter()
	if err != nil {
		return 0, err
	}
	result, err := counter.CountReady(ctx, readyRoleRequest(in))
	if err != nil {
		return 0, err
	}
	return int(result.Total), nil
}

// buildParentEpicMap builds a map from child issue ID to parent epic title.
// Only includes parents that are epics.
func buildParentEpicMap(ctx context.Context, s storage.DoltStorage, issues []*types.Issue) map[string]string {
	if len(issues) == 0 {
		return nil
	}
	parentIDs, childToParent, err := parentEpicRelationships(ctx, s, issues)
	if err != nil || len(parentIDs) == 0 {
		return nil
	}
	epictitles := loadEpicTitles(ctx, s, parentIDs)
	return mapChildrenToEpicTitles(childToParent, epictitles)
}

func parentEpicRelationships(ctx context.Context, s storage.DoltStorage, issues []*types.Issue) (map[string]bool, map[string]string, error) {
	issueIDs := make([]string, len(issues))
	for i, issue := range issues {
		issueIDs[i] = issue.ID
	}
	allDeps, err := s.GetDependencyRecordsForIssues(ctx, issueIDs)
	if err != nil {
		return nil, nil, err
	}

	// Find parent-child deps where the issue is the child
	parentIDs := make(map[string]bool)
	childToParent := make(map[string]string) // childID -> parentID
	for issueID, deps := range allDeps {
		for _, dep := range deps {
			if dep.Type == types.DepParentChild {
				parentIDs[dep.DependsOnID] = true
				childToParent[issueID] = dep.DependsOnID
			}
		}
	}
	return parentIDs, childToParent, nil
}

func loadEpicTitles(ctx context.Context, s storage.DoltStorage, parentIDs map[string]bool) map[string]string {
	epicTitles := make(map[string]string)
	for parentID := range parentIDs {
		parent, err := s.GetIssue(ctx, parentID)
		if err != nil || parent == nil {
			continue
		}
		if parent.IssueType == "epic" {
			epicTitles[parentID] = parent.Title
		}
	}
	return epicTitles
}

func mapChildrenToEpicTitles(childToParent map[string]string, epicTitles map[string]string) map[string]string {
	result := make(map[string]string)
	for childID, parentID := range childToParent {
		if title, ok := epicTitles[parentID]; ok {
			result[childID] = title
		}
	}
	return result
}

// displayReadyList displays ready issues in pretty format with optional parent epic context
func displayReadyList(issues []*types.Issue, parentEpicMap map[string]string) {
	for _, issue := range issues {
		epicTitle := ""
		if parentEpicMap != nil {
			epicTitle = parentEpicMap[issue.ID]
		}
		fmt.Println(formatPrettyIssueWithContext(issue, epicTitle))
	}

	// Summary footer
	fmt.Println()
	fmt.Println(strings.Repeat("-", 80))
	fmt.Printf("Ready: %d issues with no active blockers\n", len(issues))
	fmt.Println()
	fmt.Println("Status: ○ open  ◐ in_progress  ● blocked  ✓ closed  ❄ deferred")
	fmt.Println("Priority: P0–P4 (label only; not a status icon)")
}

// readyExplainFilter is the filter both --explain routes run, derived from the
// same builder the listing uses so the set `bd ready --explain` explains cannot
// drift from the set `bd ready` shows (bd-3fs.3).
//
// --explain takes no listing flags: it is a whole-graph diagnostic, and the
// direct route reaches it before the flags are even gathered. The limit is
// therefore pinned to unlimited rather than left to workapi.DefaultReadyLimit,
// which would silently truncate the explanation at 100 rows.
func readyExplainFilter() (types.WorkFilter, error) {
	unlimited := 0
	return workapi.BuildReadyFilter(issueops.ReadyRequest{
		Sort:  string(types.SortPolicyPriority),
		Limit: &unlimited,
	})
}
