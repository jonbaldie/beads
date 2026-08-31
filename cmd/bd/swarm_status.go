// Package main implements the bd CLI swarm management commands.
package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

type SwarmStatus struct {
	EpicID       string        `json:"epic_id"`
	EpicTitle    string        `json:"epic_title"`
	TotalIssues  int           `json:"total_issues"`
	Completed    []StatusIssue `json:"completed"`
	Active       []StatusIssue `json:"active"`
	Ready        []StatusIssue `json:"ready"`
	Blocked      []StatusIssue `json:"blocked"`
	Progress     float64       `json:"progress_percent"`
	ActiveCount  int           `json:"active_count"`
	ReadyCount   int           `json:"ready_count"`
	BlockedCount int           `json:"blocked_count"`
}

// StatusIssue represents an issue in swarm status output.
type StatusIssue struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Assignee  string   `json:"assignee,omitempty"`
	BlockedBy []string `json:"blocked_by,omitempty"`
	ClosedAt  string   `json:"closed_at,omitempty"`
}

var swarmStatusCmd = &cobra.Command{
	Use:   "status [epic-or-swarm-id]",
	Short: "Show current swarm status",
	Long: `Show the current status of a swarm, computed from beads.

Accepts either:
- An epic ID (shows status for that epic's children)
- A swarm molecule ID (follows the link to find the epic)

Displays issues grouped by state:
- Completed: Closed issues
- Active: Issues currently in_progress (with assignee)
- Ready: Open issues with all dependencies satisfied
- Blocked: Open issues waiting on dependencies

The status is COMPUTED from beads, not stored separately.
If beads changes, status changes.

Examples:
  bd swarm status gt-epic-123       # Show swarm status by epic
  bd swarm status gt-swarm-456      # Show status via swarm molecule
  bd swarm status gt-epic-123 --json  # Machine-readable output`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if usesProxiedServer() {
			return runSwarmStatusProxiedServer(cmd, getRootContext(), args)
		}
		evt := metrics.NewCommandEvent("swarm-status")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		ctx := getRootContext()

		if getStore() == nil {
			return HandleErrorRespectJSON("no database connection")
		}

		issueID, err := utils.ResolvePartialID(ctx, getStore(), args[0])
		if err != nil {
			return HandleErrorRespectJSON("issue '%s' not found: %v", args[0], err)
		}

		issue, err := getStore().GetIssue(ctx, issueID)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return HandleErrorRespectJSON("issue '%s' not found", issueID)
			}
			return HandleErrorRespectJSON("failed to get issue: %v", err)
		}

		var epic *types.Issue

		if issue.IssueType == "molecule" && issue.MolType == types.MolTypeSwarm {
			deps, err := getStore().GetDependencyRecords(ctx, issue.ID)
			if err != nil {
				return HandleErrorRespectJSON("failed to get swarm dependencies: %v", err)
			}
			for _, dep := range deps {
				if dep.Type == types.DepRelatesTo {
					epic, err = getStore().GetIssue(ctx, dep.DependsOnID)
					if err != nil {
						return HandleErrorRespectJSON("failed to get linked epic: %v", err)
					}
					break
				}
			}
			if epic == nil {
				return HandleErrorRespectJSON("swarm molecule '%s' has no linked epic", issueID)
			}
		} else if issue.IssueType == types.TypeEpic || issue.IssueType == "molecule" {
			epic = issue
		} else {
			return HandleErrorRespectJSON("'%s' is not an epic or swarm molecule (type: %s)", issueID, issue.IssueType)
		}

		status, err := getSwarmStatus(ctx, getStore(), epic)
		if err != nil {
			return HandleErrorRespectJSON("failed to get swarm status: %v", err)
		}

		if isJSONOutput() {
			return outputJSON(status)
		}

		renderSwarmStatus(status)
		return nil
	},
}

// getSwarmStatus computes current swarm status from beads.
func getSwarmStatus(ctx context.Context, s SwarmStorage, epic *types.Issue) (*SwarmStatus, error) {
	status := newSwarmStatus(epic)

	// Get all child issues of the epic
	childIssues, err := getEpicChildren(ctx, s, epic.ID)
	if err != nil {
		return nil, err
	}

	status.TotalIssues = len(childIssues)
	if len(childIssues) == 0 {
		return status, nil
	}
	dependsOn := buildSwarmDependencyMap(ctx, s, epic.ID, childIssues)
	categorizeSwarmIssues(ctx, s, childIssues, dependsOn, status)
	sortSwarmStatus(status)
	setSwarmStatusCounts(status)

	return status, nil
}

func newSwarmStatus(epic *types.Issue) *SwarmStatus {
	return &SwarmStatus{
		EpicID:    epic.ID,
		EpicTitle: epic.Title,
		Completed: []StatusIssue{},
		Active:    []StatusIssue{},
		Ready:     []StatusIssue{},
		Blocked:   []StatusIssue{},
	}
}

func buildSwarmDependencyMap(ctx context.Context, s SwarmStorage, epicID string,
	childIssues []*types.Issue) map[string][]string {
	childIDSet := make(map[string]bool, len(childIssues))
	for _, issue := range childIssues {
		childIDSet[issue.ID] = true
	}
	dependsOn := make(map[string][]string)
	for _, issue := range childIssues {
		deps, err := s.GetDependencyRecords(ctx, issue.ID)
		if err != nil {
			continue
		}
		for _, dep := range deps {
			appendSwarmStatusDependency(dependsOn, issue.ID, epicID, childIDSet, dep)
		}
	}
	return dependsOn
}

func appendSwarmStatusDependency(dependsOn map[string][]string, issueID, epicID string,
	childIDSet map[string]bool, dep *types.Dependency) {
	if dep.DependsOnID == epicID && dep.Type == types.DepParentChild {
		return
	}
	if dep.Type.AffectsReadyWork() && childIDSet[dep.DependsOnID] {
		dependsOn[issueID] = append(dependsOn[issueID], dep.DependsOnID)
	}
}

func categorizeSwarmIssues(ctx context.Context, s SwarmStorage, issues []*types.Issue,
	dependsOn map[string][]string, status *SwarmStatus) {
	for _, issue := range issues {
		switch issue.Status {
		case types.StatusClosed:
			status.Completed = append(status.Completed, closedStatusIssue(issue))
		case types.StatusInProgress:
			status.Active = append(status.Active, baseStatusIssue(issue))
		default:
			appendOpenSwarmStatusIssue(ctx, s, issue, dependsOn[issue.ID], status)
		}
	}
}

func baseStatusIssue(issue *types.Issue) StatusIssue {
	return StatusIssue{ID: issue.ID, Title: issue.Title, Assignee: issue.Assignee}
}

func closedStatusIssue(issue *types.Issue) StatusIssue {
	statusIssue := baseStatusIssue(issue)
	if issue.ClosedAt != nil {
		statusIssue.ClosedAt = issue.ClosedAt.Format("2006-01-02 15:04")
	}
	return statusIssue
}

func appendOpenSwarmStatusIssue(ctx context.Context, s SwarmStorage, issue *types.Issue,
	dependencyIDs []string, status *SwarmStatus) {
	statusIssue := baseStatusIssue(issue)
	blockers := openSwarmBlockers(ctx, s, dependencyIDs)
	if len(blockers) > 0 {
		statusIssue.BlockedBy = blockers
		status.Blocked = append(status.Blocked, statusIssue)
		return
	}
	status.Ready = append(status.Ready, statusIssue)
}

func openSwarmBlockers(ctx context.Context, s SwarmStorage, dependencyIDs []string) []string {
	var blockers []string
	for _, dependencyID := range dependencyIDs {
		dependencyIssue, _ := s.GetIssue(ctx, dependencyID)
		if dependencyIssue != nil && dependencyIssue.Status != types.StatusClosed {
			blockers = append(blockers, dependencyID)
		}
	}
	return blockers
}

func sortSwarmStatus(status *SwarmStatus) {
	byID := func(i, j int) bool {
		return utils.NaturalCompareIDs(status.Completed[i].ID, status.Completed[j].ID) < 0
	}
	sort.Slice(status.Completed, byID)
	sort.Slice(status.Active, func(i, j int) bool {
		return utils.NaturalCompareIDs(status.Active[i].ID, status.Active[j].ID) < 0
	})
	sort.Slice(status.Ready, func(i, j int) bool {
		return utils.NaturalCompareIDs(status.Ready[i].ID, status.Ready[j].ID) < 0
	})
	sort.Slice(status.Blocked, func(i, j int) bool {
		return utils.NaturalCompareIDs(status.Blocked[i].ID, status.Blocked[j].ID) < 0
	})
}

func setSwarmStatusCounts(status *SwarmStatus) {
	status.ActiveCount = len(status.Active)
	status.ReadyCount = len(status.Ready)
	status.BlockedCount = len(status.Blocked)
	if status.TotalIssues > 0 {
		status.Progress = float64(len(status.Completed)) / float64(status.TotalIssues) * 100
	}
}

// renderSwarmStatus outputs human-readable swarm status.
func renderSwarmStatus(status *SwarmStatus) {
	fmt.Printf("\n%s Ready Front Analysis: %s\n\n", ui.RenderAccent("●"), status.EpicTitle)
	renderSwarmCompleted(status.Completed)
	renderSwarmActive(status.Active)
	renderSwarmReady(status.Ready, status.Blocked)
	renderSwarmBlocked(status.Blocked)
	renderSwarmProgress(status)
}

func renderSwarmCompleted(issues []StatusIssue) {
	fmt.Printf("Completed:     ")
	if len(issues) == 0 {
		fmt.Printf("(none)\n")
		return
	}
	for i, issue := range issues {
		if i > 0 {
			fmt.Printf("               ")
		}
		fmt.Printf("%s %s\n", ui.RenderPass("✓"), ui.RenderID(issue.ID))
	}
}

func renderSwarmActive(issues []StatusIssue) {
	fmt.Printf("Active:        ")
	if len(issues) == 0 {
		fmt.Printf("(none)\n")
		return
	}
	var parts []string
	for _, issue := range issues {
		part := fmt.Sprintf("⟳ %s", issue.ID)
		if issue.Assignee != "" {
			part += fmt.Sprintf(" [%s]", issue.Assignee)
		}
		parts = append(parts, part)
	}
	fmt.Printf("%s\n", strings.Join(parts, ", "))
}

func renderSwarmReady(ready, blocked []StatusIssue) {
	fmt.Printf("Ready:         ")
	if len(ready) == 0 {
		if len(blocked) > 0 {
			fmt.Printf("(none - waiting for %s)\n", strings.Join(swarmNeededBlockers(blocked), ", "))
			return
		}
		fmt.Printf("(none)\n")
		return
	}
	var parts []string
	for _, issue := range ready {
		parts = append(parts, fmt.Sprintf("○ %s", issue.ID))
	}
	fmt.Printf("%s\n", strings.Join(parts, ", "))
}

func swarmNeededBlockers(blocked []StatusIssue) []string {
	needed := make(map[string]bool)
	for _, issue := range blocked {
		for _, dependencyID := range issue.BlockedBy {
			needed[dependencyID] = true
		}
	}
	var neededList []string
	for dependencyID := range needed {
		neededList = append(neededList, dependencyID)
	}
	sort.Strings(neededList)
	return neededList
}

func renderSwarmBlocked(issues []StatusIssue) {
	fmt.Printf("Blocked:       ")
	if len(issues) == 0 {
		fmt.Printf("(none)\n")
		return
	}
	for i, issue := range issues {
		if i > 0 {
			fmt.Printf("               ")
		}
		fmt.Printf("◌ %s (needs %s)\n", issue.ID, strings.Join(issue.BlockedBy, ", "))
	}
}

func renderSwarmProgress(status *SwarmStatus) {
	fmt.Printf("\nProgress: %d/%d complete", len(status.Completed), status.TotalIssues)
	if status.ActiveCount > 0 {
		fmt.Printf(", %d/%d active", status.ActiveCount, status.TotalIssues)
	}
	fmt.Printf(" (%.0f%%)\n\n", status.Progress)
}
