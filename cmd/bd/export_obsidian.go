package main

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/types"
)

// formatObsidianTask converts a single issue to Obsidian Tasks format
func formatObsidianTask(issue *types.Issue) string {
	parts := []string{obsidianCheckboxFor(issue.Status), issue.Title, fmt.Sprintf("🆔 %s", issue.ID)}
	if priority := obsidianPriorityFor(issue.Priority); priority != "" {
		parts = append(parts, priority)
	}
	if tag := obsidianTypeTagFor(issue.IssueType); tag != "" {
		parts = append(parts, tag)
	}
	for _, label := range issue.Labels {
		parts = append(parts, "#"+strings.ReplaceAll(label, " ", "-"))
	}
	parts = append(parts, fmt.Sprintf("🛫 %s", issue.CreatedAt.Format("2006-01-02")))
	if issue.ClosedAt != nil {
		parts = append(parts, fmt.Sprintf("✅ %s", issue.ClosedAt.Format("2006-01-02")))
	}
	for _, dep := range issue.Dependencies {
		if dep.Type == types.DepBlocks || dep.Type == types.DepParentChild {
			parts = append(parts, fmt.Sprintf("⛔ %s", dep.DependsOnID))
		}
	}

	return strings.Join(parts, " ")
}

func obsidianCheckboxFor(status types.Status) string {
	switch status {
	case types.StatusInProgress, types.StatusHooked:
		return "- [/]"
	case types.StatusBlocked:
		return "- [c]"
	case types.StatusClosed:
		return "- [x]"
	case types.StatusDeferred:
		return "- [-]"
	case types.StatusPinned:
		return "- [n]"
	default:
		return "- [ ]"
	}
}

func obsidianPriorityFor(priority int) string {
	priorities := [...]string{"🔺", "⏫", "🔼", "🔽", "⏬"}
	if priority < 0 || priority >= len(priorities) {
		return ""
	}
	return priorities[priority]
}

func obsidianTypeTagFor(issueType types.IssueType) string {
	switch issueType {
	case types.TypeBug:
		return "#Bug"
	case types.TypeFeature:
		return "#Feature"
	case types.TypeTask:
		return "#Task"
	case types.TypeEpic:
		return "#Epic"
	case types.TypeChore:
		return "#Chore"
	default:
		return ""
	}
}

// groupIssuesByDate groups issues by their most recent activity date
func groupIssuesByDate(issues []*types.Issue) map[string][]*types.Issue {
	grouped := make(map[string][]*types.Issue)
	for _, issue := range issues {
		// Use the most recent date: closed_at > updated_at > created_at
		var date time.Time
		if issue.ClosedAt != nil {
			date = *issue.ClosedAt
		} else {
			date = issue.UpdatedAt
		}
		key := date.Format("2006-01-02")
		grouped[key] = append(grouped[key], issue)
	}
	return grouped
}

// buildParentChildMap builds a map of parent ID -> child issues from parent-child dependencies
func buildParentChildMap(issues []*types.Issue) (map[string][]*types.Issue, map[string]bool) {
	parentToChildren := make(map[string][]*types.Issue)
	isChild := make(map[string]bool)

	// Build lookup map
	issueByID := make(map[string]*types.Issue)
	for _, issue := range issues {
		issueByID[issue.ID] = issue
	}

	// Find parent-child relationships
	for _, issue := range issues {
		for _, dep := range issue.Dependencies {
			if dep.Type == types.DepParentChild {
				parentID := dep.DependsOnID
				parentToChildren[parentID] = append(parentToChildren[parentID], issue)
				isChild[issue.ID] = true
			}
		}
	}

	return parentToChildren, isChild
}

// writeObsidianExport writes issues in Obsidian Tasks markdown format
func writeObsidianExport(w io.Writer, issues []*types.Issue) error {
	if err := writeObsidianHeader(w); err != nil {
		return err
	}
	parentToChildren, isChild := buildParentChildMap(issues)
	grouped := groupIssuesByDate(issues)
	for _, date := range sortedObsidianDates(grouped) {
		if err := writeObsidianDateSection(w, date, grouped[date], parentToChildren, isChild); err != nil {
			return err
		}
	}
	return nil
}

func writeObsidianHeader(w io.Writer) error {
	if _, err := fmt.Fprintln(w, "# Changes Log"); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w)
	return err
}

func sortedObsidianDates(grouped map[string][]*types.Issue) []string {
	dates := make([]string, 0, len(grouped))
	for date := range grouped {
		dates = append(dates, date)
	}
	slices.SortFunc(dates, func(a, b string) int { return cmp.Compare(b, a) })
	return dates
}

func writeObsidianDateSection(w io.Writer, date string, issues []*types.Issue, children map[string][]*types.Issue, isChild map[string]bool) error {
	if _, err := fmt.Fprintf(w, "## %s\n\n", date); err != nil {
		return err
	}
	for _, issue := range issues {
		if isChild[issue.ID] {
			continue
		}
		if _, err := fmt.Fprintln(w, formatObsidianTask(issue)); err != nil {
			return err
		}
		for _, child := range children[issue.ID] {
			if _, err := fmt.Fprintln(w, "  "+formatObsidianTask(child)); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}
