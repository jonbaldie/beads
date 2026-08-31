package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
)

// showIssueRefs displays issues that reference the given issue(s), grouped by relationship type
func showIssueRefs(ctx context.Context, args []string, jsonOut bool) error {
	allRefs := collectIssueRefs(ctx, args)
	if jsonOut {
		return outputJSON(allRefs)
	}
	renderIssueRefs(allRefs)
	return nil
}

func collectIssueRefs(ctx context.Context, args []string) map[string][]*types.IssueWithDependencyMetadata {
	allRefs := make(map[string][]*types.IssueWithDependencyMetadata)
	for _, id := range args {
		result, err := resolveAndGetIssueWithRouting(ctx, getStore(), id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error resolving %s: %v\n", id, err)
			continue
		}
		if result == nil || result.Issue == nil {
			if result != nil {
				result.Close()
			}
			fmt.Fprintf(os.Stderr, "Issue %s not found\n", id)
			continue
		}
		refs, err := result.Store.GetDependentsWithMetadata(ctx, result.ResolvedID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting refs for %s: %v\n", id, err)
		} else {
			allRefs[result.ResolvedID] = refs
		}
		result.Close()
	}
	return allRefs
}

func renderIssueRefs(allRefs map[string][]*types.IssueWithDependencyMetadata) {
	for issueID, refs := range allRefs {
		renderIssueRefGroup(issueID, refs)
	}
}

func renderIssueRefGroup(issueID string, refs []*types.IssueWithDependencyMetadata) {
	if len(refs) == 0 {
		fmt.Printf("\n%s: No references found\n", ui.RenderAccent(issueID))
		return
	}
	fmt.Printf("\n%s References to %s:\n", ui.RenderAccent("📎"), issueID)
	// Every ref is an edge pointing AT this issue, so each group is named
	// from this issue's end. The bare type name would read from the other
	// end for the types whose name runs source-first: a (dup, canonical)
	// edge under a "duplicates" heading says the canonical is the copy.
	for _, sec := range groupDepSections(refs, false, nil) {
		displayRefGroup(sec)
	}
	fmt.Println()
}

// displayRefGroup displays one group of references under its relationship name
// Closed items get entire row muted - the work is done, no need for attention
func displayRefGroup(sec depSection) {
	emoji := getRefTypeEmoji(sec.Type)
	fmt.Printf("\n  %s %s (%d):\n", emoji, sec.Heading, len(sec.Deps))

	for _, ref := range sec.Deps {
		// Closed items: mute entire row since the work is complete
		if ref.Status == types.StatusClosed {
			fmt.Printf("    %s: %s %s\n",
				ui.RenderMuted(ref.ID),
				ui.RenderMuted(ref.Title),
				ui.RenderMuted(fmt.Sprintf("[P%d - %s]", ref.Priority, ref.Status)))
			continue
		}

		// Active items: color ID based on status
		var idStr string
		switch ref.Status {
		case types.StatusOpen:
			idStr = ui.StatusOpenStyle().Render(ref.ID)
		case types.StatusInProgress:
			idStr = ui.StatusInProgressStyle().Render(ref.ID)
		case types.StatusBlocked:
			idStr = ui.StatusBlockedStyle().Render(ref.ID)
		default:
			idStr = ref.ID
		}
		fmt.Printf("    %s: %s [P%d - %s]\n", idStr, ref.Title, ref.Priority, ref.Status)
	}
}

// getRefTypeEmoji returns a symbol for a dependency/reference type.
func getRefTypeEmoji(depType types.DependencyType) string {
	if symbol, ok := refTypeSymbols[depType]; ok {
		return symbol
	}
	return "→"
}

var refTypeSymbols = map[types.DependencyType]string{
	types.DepUntil:          "⏳",
	types.DepCausedBy:       "⚡",
	types.DepValidates:      "✅",
	types.DepBlocks:         "🚫",
	types.DepParentChild:    "↳",
	types.DepRelatesTo:      "↔",
	types.DepRelated:        "↔",
	types.DepTracks:         "👁",
	types.DepDiscoveredFrom: "◊",
	types.DepSupersedes:     "⬆",
	types.DepDuplicates:     "🔄",
	types.DepRepliesTo:      "💬",
	types.DepApprovedBy:     "👍",
	types.DepAuthoredBy:     "✏",
	types.DepAssignedTo:     "👤",
}
