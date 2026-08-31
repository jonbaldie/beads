package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/uimd"
)

// showIssueChildren displays only the children of the specified issue(s)
func showIssueChildren(ctx context.Context, args []string, jsonOut bool, shortMode bool) error {
	allChildren := make(map[string][]*types.IssueWithDependencyMetadata)
	for _, id := range args {
		collectIssueChildren(ctx, id, allChildren)
	}
	if jsonOut {
		return outputJSON(allChildren)
	}
	renderIssueChildren(allChildren, shortMode)
	return nil
}

func collectIssueChildren(ctx context.Context, id string, allChildren map[string][]*types.IssueWithDependencyMetadata) {
	result, err := resolveAndGetIssueWithRouting(ctx, getStore(), id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving %s: %v\n", id, err)
		return
	}
	if result == nil || result.Issue == nil {
		if result != nil {
			result.Close()
		}
		fmt.Fprintf(os.Stderr, "Issue %s not found\n", id)
		return
	}
	defer result.Close()
	if _, exists := allChildren[result.ResolvedID]; !exists {
		allChildren[result.ResolvedID] = []*types.IssueWithDependencyMetadata{}
	}
	refs, err := result.Store.GetDependentsWithMetadata(ctx, result.ResolvedID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting children for %s: %v\n", id, err)
		return
	}
	for _, ref := range refs {
		if ref.DependencyType == types.DepParentChild {
			allChildren[result.ResolvedID] = append(allChildren[result.ResolvedID], ref)
		}
	}
}

func renderIssueChildren(allChildren map[string][]*types.IssueWithDependencyMetadata, shortMode bool) {
	for issueID, children := range allChildren {
		if len(children) == 0 {
			fmt.Printf("%s: No children found\n", ui.RenderAccent(issueID))
			continue
		}

		fmt.Printf("%s Children of %s (%d):\n", ui.RenderAccent("↳"), issueID, len(children))
		for _, child := range children {
			if shortMode {
				fmt.Printf("  %s\n", formatShortIssue(&child.Issue))
			} else {
				fmt.Println(formatDependencyLine("↳", child))
			}
		}
		fmt.Println()
	}
}

// showIssueAsOf displays issues as they existed at a specific commit or branch ref.
// This requires a versioned storage backend (e.g., Dolt).
func showIssueAsOf(ctx context.Context, args []string, ref string, shortMode bool) error {
	var allIssues []*types.Issue
	for idx, id := range args {
		issue, err := getStore().AsOf(ctx, id, ref)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error fetching %s as of %s: %v\n", id, ref, err)
			continue
		}
		if issue == nil {
			fmt.Fprintf(os.Stderr, "Issue %s did not exist at %s\n", id, ref)
			continue
		}

		if isJSONOutput() && !shortMode {
			allIssues = append(allIssues, issue)
			continue
		}
		renderIssueAsOf(issue, ref, idx, shortMode)
	}

	if isJSONOutput() && len(allIssues) > 0 {
		return outputJSON(allIssues)
	}
	return nil
}

func renderIssueAsOf(issue *types.Issue, ref string, idx int, shortMode bool) {
	if shortMode {
		fmt.Println(formatShortIssue(issue))
		return
	}
	if idx > 0 {
		fmt.Println("\n" + ui.RenderMuted(strings.Repeat("-", 60)))
	}
	fmt.Printf("\n%s (as of %s)\n", formatIssueHeader(issue), ui.RenderMuted(ref))
	fmt.Println(formatIssueMetadata(issue))
	if issue.Description != "" {
		fmt.Printf("\n%s\n%s\n", ui.RenderBold("DESCRIPTION"), uimd.RenderMarkdown(issue.Description))
	}
	fmt.Println()
}
