package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

var wispListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all wisps in current context",
	Long: `List all wisps (ephemeral molecules) in the current context.

Wisps are issues with Ephemeral=true in the main database. They are stored
locally but not synced via git.

The list shows:
  - ID: Issue ID of the wisp
  - Title: Wisp title
  - Status: Current status (open, in_progress, closed)
  - Started: When the wisp was created
  - Updated: Last modification time

Old wisp detection:
  - Old wisps haven't been updated in 24+ hours
  - Use 'bd mol wisp gc' to clean up old/abandoned wisps

Examples:
  bd mol wisp list              # List all wisps
  bd mol wisp list --json       # JSON output for programmatic use
  bd mol wisp list --all        # Include closed wisps`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runWispList,
}

func wispListFilter(typeFilter string) types.IssueFilter {
	ephemeralFlag := true
	filter := types.IssueFilter{
		IssueFilterCore: types.IssueFilterCore{
			Limit: 5000,
		},
		IssueFilterFlags: types.IssueFilterFlags{
			Ephemeral: &ephemeralFlag,
		},
	}
	if typeFilter != "" {
		it := types.IssueType(typeFilter)
		filter.IssueType = &it
	}
	return filter
}

func buildWispListResult(issues []*types.Issue, showAll bool) WispListResult {
	if !showAll {
		var filtered []*types.Issue
		for _, issue := range issues {
			if issue.Status != types.StatusClosed {
				filtered = append(filtered, issue)
			}
		}
		issues = filtered
	}

	now := time.Now()
	items := make([]WispListItem, 0, len(issues))
	oldCount := 0

	for _, issue := range issues {
		item := WispListItem{
			ID:        issue.ID,
			Title:     issue.Title,
			Status:    string(issue.Status),
			Priority:  issue.Priority,
			Type:      string(issue.IssueType),
			Labels:    issue.Labels,
			CreatedAt: issue.CreatedAt,
			UpdatedAt: issue.UpdatedAt,
		}
		if now.Sub(issue.UpdatedAt) > OldThreshold {
			item.Old = true
			oldCount++
		}
		items = append(items, item)
	}

	slices.SortFunc(items, func(a, b WispListItem) int {
		return b.UpdatedAt.Compare(a.UpdatedAt)
	})

	return WispListResult{
		Wisps:    items,
		Count:    len(items),
		OldCount: oldCount,
	}
}

func renderWispListResult(result WispListResult) error {
	if isJSONOutput() {
		return outputJSON(result)
	}

	if len(result.Wisps) == 0 {
		fmt.Println("No wisps found")
		return nil
	}

	fmt.Printf("Wisps (%d):\n\n", len(result.Wisps))
	fmt.Printf("%-12s %-10s %-4s %-10s %-46s %s\n",
		"ID", "STATUS", "PRI", "TYPE", "TITLE", "UPDATED")
	fmt.Println(strings.Repeat("-", 100))

	for _, item := range result.Wisps {
		title := item.Title
		if len(title) > 44 {
			title = title[:41] + "..."
		}
		status := ui.RenderStatus(item.Status)
		updated := formatTimeAgo(item.UpdatedAt)
		if item.Old {
			updated = ui.RenderWarn(updated + " ⚠")
		}
		fmt.Printf("%-12s %-10s P%-3d %-10s %-46s %s\n",
			item.ID, status, item.Priority, item.Type, title, updated)
	}

	if result.OldCount > 0 {
		fmt.Printf("\n%s %d old wisp(s) (not updated in 24+ hours)\n",
			ui.RenderWarn("⚠"), result.OldCount)
		fmt.Println("  Hint: Use 'bd mol wisp gc' to clean up old wisps")
	}
	return nil
}

func runWispList(cmd *cobra.Command, _ []string) error {
	evt := metrics.NewCommandEvent("wisp-list")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	showAll, _ := cmd.Flags().GetBool("all")
	typeFilter, _ := cmd.Flags().GetString("type")

	if usesProxiedServer() {
		return runWispListProxiedServer(getRootContext(), showAll, typeFilter)
	}

	ctx := getRootContext()

	if getStore() == nil {
		if isJSONOutput() {
			return outputJSON(WispListResult{
				Wisps: []WispListItem{},
				Count: 0,
			})
		}
		fmt.Println("No database connection")
		return nil
	}

	issues, err := getStore().SearchIssues(ctx, "", wispListFilter(typeFilter))
	if err != nil {
		return HandleError("listing wisps: %v", err)
	}

	return renderWispListResult(buildWispListResult(issues, showAll))
}

// formatTimeAgo returns a human-readable relative time
func formatTimeAgo(t time.Time) string {
	d := time.Since(t)

	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		mins := int(d.Minutes())
		if mins == 1 {
			return "1 min ago"
		}
		return fmt.Sprintf("%d mins ago", mins)
	case d < 24*time.Hour:
		hours := int(d.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	case d < 7*24*time.Hour:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	default:
		return t.Format("2006-01-02")
	}
}

// formatTimeUntil returns a human-readable relative time for a future instant,
// the forward-looking mirror of formatTimeAgo. Used for lease expiry in bd show.
// A past (or present) instant renders as "expired".
func formatTimeUntil(t time.Time) string {
	d := time.Until(t)
	if d <= 0 {
		return "expired"
	}
	switch {
	case d < time.Minute:
		return "in <1 min"
	case d < time.Hour:
		mins := int(d.Minutes())
		if mins == 1 {
			return "in 1 min"
		}
		return fmt.Sprintf("in %d mins", mins)
	case d < 24*time.Hour:
		hours := int(d.Hours())
		if hours == 1 {
			return "in 1 hour"
		}
		return fmt.Sprintf("in %d hours", hours)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "in 1 day"
		}
		return fmt.Sprintf("in %d days", days)
	}
}
