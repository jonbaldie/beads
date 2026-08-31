// Package main implements the bd CLI swarm management commands.
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

var swarmCmd = &cobra.Command{
	Use:     "swarm",
	GroupID: "deps",
	Short:   "Swarm management for structured epics",
	Long: `Swarm management commands for coordinating parallel work on epics.

A swarm is a structured body of work defined by an epic and its children,
with dependencies forming a DAG (directed acyclic graph) of work.`,
}

// SwarmAnalysis holds the results of analyzing an epic's structure for swarming.
type SwarmAnalysis struct {
	EpicID            string                `json:"epic_id"`
	EpicTitle         string                `json:"epic_title"`
	TotalIssues       int                   `json:"total_issues"`
	ClosedIssues      int                   `json:"closed_issues"`
	ReadyFronts       []ReadyFront          `json:"ready_fronts"`
	MaxParallelism    int                   `json:"max_parallelism"`
	EstimatedSessions int                   `json:"estimated_sessions"`
	Warnings          []string              `json:"warnings"`
	Errors            []string              `json:"errors"`
	Swarmable         bool                  `json:"swarmable"`
	Issues            map[string]*IssueNode `json:"issues,omitempty"` // Only included with --verbose
}

// ReadyFront represents a group of issues that can be worked on in parallel.
type ReadyFront struct {
	Wave   int      `json:"wave"`
	Issues []string `json:"issues"`
	Titles []string `json:"titles,omitempty"` // Only for human output
}

// IssueNode represents an issue in the dependency graph.
type IssueNode struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Status       string   `json:"status"`
	Priority     int      `json:"priority"`
	DependsOn    []string `json:"depends_on"`     // What this issue depends on
	DependedOnBy []string `json:"depended_on_by"` // What depends on this issue
	Wave         int      `json:"wave"`           // Which ready front this belongs to (-1 if blocked by cycle)
}

// SwarmStorage defines the storage interface needed by swarm commands.
type SwarmStorage interface {
	GetIssue(context.Context, string) (*types.Issue, error)
	GetDependents(context.Context, string) ([]*types.Issue, error)
	GetDependencyRecords(context.Context, string) ([]*types.Dependency, error)
}

// findExistingSwarm returns the swarm molecule for an epic, if one exists.
// Returns nil if no swarm molecule is linked to the epic.
func findExistingSwarm(ctx context.Context, s SwarmStorage, epicID string) (*types.Issue, error) {
	// Get all issues that depend on the epic
	dependents, err := s.GetDependents(ctx, epicID)
	if err != nil {
		return nil, fmt.Errorf("failed to get epic dependents: %w", err)
	}

	// Find a swarm molecule with relates-to dependency to this epic
	for _, dep := range dependents {
		fullIssue, ok := loadSwarmDependent(ctx, s, dep)
		if ok && swarmRelatesToEpic(ctx, s, fullIssue.ID, epicID) {
			return fullIssue, nil
		}
	}

	return nil, nil
}

func loadSwarmDependent(ctx context.Context, s SwarmStorage, dependent *types.Issue) (*types.Issue, bool) {
	// GetDependents doesn't populate mol_type, so fetch the full issue.
	if dependent.IssueType != "molecule" {
		return nil, false
	}
	fullIssue, err := s.GetIssue(ctx, dependent.ID)
	if err != nil || fullIssue == nil || fullIssue.MolType != types.MolTypeSwarm {
		return nil, false
	}
	return fullIssue, true
}

func swarmRelatesToEpic(ctx context.Context, s SwarmStorage, swarmID, epicID string) bool {
	deps, err := s.GetDependencyRecords(ctx, swarmID)
	if err != nil {
		return false
	}
	for _, dep := range deps {
		if dep.DependsOnID == epicID && dep.Type == types.DepRelatesTo {
			return true
		}
	}
	return false
}

// getEpicChildren returns all child issues of an epic (via parent-child dependencies).
func getEpicChildren(ctx context.Context, s SwarmStorage, epicID string) ([]*types.Issue, error) {
	// Get all issues that depend on the epic
	allDependents, err := s.GetDependents(ctx, epicID)
	if err != nil {
		return nil, fmt.Errorf("failed to get epic dependents: %w", err)
	}

	// Filter to only parent-child relationships by checking each dependent's dependency records
	var children []*types.Issue
	for _, dependent := range allDependents {
		deps, err := s.GetDependencyRecords(ctx, dependent.ID)
		if err != nil {
			continue // Skip issues we can't query
		}
		for _, dep := range deps {
			if dep.DependsOnID == epicID && dep.Type == types.DepParentChild {
				children = append(children, dependent)
				break
			}
		}
	}

	return children, nil
}

var swarmValidateCmd = &cobra.Command{
	Use:   "validate [epic-id]",
	Short: "Validate epic structure for swarming",
	Long: `Validate an epic's structure to ensure it's ready for swarm execution.

Checks for:
- Correct dependency direction (requirement-based, not temporal)
- Orphaned issues (roots with no dependents)
- Missing dependencies (leaves that should depend on something)
- Cycles (impossible to resolve)
- Disconnected subgraphs

Reports:
- Ready fronts (waves of parallel work)
- Estimated worker-sessions
- Maximum parallelism
- Warnings for potential issues

Examples:
  bd swarm validate gt-epic-123           # Validate epic structure
  bd swarm validate gt-epic-123 --verbose # Include detailed issue graph`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if usesProxiedServer() {
			return runSwarmValidateProxiedServer(cmd, getRootContext(), args)
		}
		evt := metrics.NewCommandEvent("swarm-validate")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		ctx := getRootContext()
		verbose, _ := cmd.Flags().GetBool("verbose")

		if getStore() == nil {
			return HandleErrorRespectJSON("no database connection")
		}

		epicID, err := utils.ResolvePartialID(ctx, getStore(), args[0])
		if err != nil {
			return HandleErrorRespectJSON("epic '%s' not found: %v", args[0], err)
		}

		epic, err := getStore().GetIssue(ctx, epicID)
		if err != nil {
			return HandleErrorRespectJSON("failed to get epic: %v", err)
		}
		if epic == nil {
			return HandleErrorRespectJSON("epic '%s' not found", epicID)
		}

		if epic.IssueType != types.TypeEpic && epic.IssueType != "molecule" {
			return HandleErrorRespectJSON("'%s' is not an epic or molecule (type: %s)", epicID, epic.IssueType)
		}

		analysis, err := analyzeEpicForSwarm(ctx, getStore(), epic)
		if err != nil {
			return HandleErrorRespectJSON("failed to analyze epic: %v", err)
		}

		if !verbose {
			analysis.Issues = nil
		}

		if isJSONOutput() {
			if jerr := outputJSON(analysis); jerr != nil {
				return jerr
			}
			if !analysis.Swarmable {
				return SilentExit()
			}
			return nil
		}

		renderSwarmAnalysis(analysis)

		if !analysis.Swarmable {
			return SilentExit()
		}
		return nil
	},
}

// analyzeEpicForSwarm performs structural analysis of an epic for swarm execution.

var swarmCreateCmd = &cobra.Command{
	Use:   "create [epic-id]",
	Short: "Create a swarm molecule from an epic",
	Long: `Create a swarm molecule to orchestrate parallel work on an epic.

The swarm molecule:
- Links to the epic it orchestrates
- Has mol_type=swarm for discovery
- Specifies a coordinator (optional)
- Can be picked up by any coordinator agent

If given a single issue (not an epic), it will be auto-wrapped:
- Creates an epic with that issue as its only child
- Then creates the swarm molecule for that epic

Examples:
  bd swarm create bd-epic-123                          # Create swarm for epic
  bd swarm create bd-epic-123 --coordinator=observer/   # With specific coordinator
  bd swarm create bd-task-456                          # Auto-wrap single issue`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if usesProxiedServer() {
			return HandleErrorRespectJSON("swarm create is not supported in proxied-server mode")
		}
		if err := CheckReadonly("swarm create"); err != nil {
			return err
		}

		evt := metrics.NewCommandEvent("swarm-create")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		ctx := getRootContext()
		coordinator, _ := cmd.Flags().GetString("coordinator")
		force, _ := cmd.Flags().GetBool("force")

		if getStore() == nil {
			return HandleErrorRespectJSON("no database connection")
		}

		inputID, err := utils.ResolvePartialID(ctx, getStore(), args[0])
		if err != nil {
			return HandleErrorRespectJSON("issue '%s' not found: %v", args[0], err)
		}

		issue, err := getStore().GetIssue(ctx, inputID)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return HandleErrorRespectJSON("issue '%s' not found", inputID)
			}
			return HandleErrorRespectJSON("failed to get issue: %v", err)
		}

		var epicID string
		var epicTitle string

		if issue.IssueType == types.TypeEpic || issue.IssueType == "molecule" {
			epicID = issue.ID
			epicTitle = issue.Title
		} else {
			if !isJSONOutput() {
				fmt.Printf("Auto-wrapping single issue as epic...\n")
			}

			wrapperEpic := &types.Issue{
				IssueContent: types.IssueContent{
					Title:       fmt.Sprintf("Swarm Epic: %s", issue.Title),
					Description: fmt.Sprintf("Auto-generated epic to wrap single issue %s for swarm execution.", issue.ID),
				},
				IssueWorkflow: types.IssueWorkflow{
					Status:    types.StatusOpen,
					Priority:  issue.Priority,
					IssueType: types.TypeEpic,
				},
				IssueTimes: types.IssueTimes{
					CreatedBy: getActor(),
				},
			}

			if err := getStore().CreateIssue(ctx, wrapperEpic, getActor()); err != nil {
				return HandleErrorRespectJSON("failed to create wrapper epic: %v", err)
			}

			dep := &types.Dependency{
				IssueID:     issue.ID,
				DependsOnID: wrapperEpic.ID,
				Type:        types.DepParentChild,
				CreatedBy:   getActor(),
			}
			if err := getStore().AddDependency(ctx, dep, getActor()); err != nil {
				return HandleErrorRespectJSON("failed to link issue to epic: %v", err)
			}

			epicID = wrapperEpic.ID
			epicTitle = wrapperEpic.Title

			if !isJSONOutput() {
				fmt.Printf("Created wrapper epic: %s\n", epicID)
			}
		}

		existingSwarm, err := findExistingSwarm(ctx, getStore(), epicID)
		if err != nil {
			return HandleErrorRespectJSON("failed to check for existing swarm: %v", err)
		}
		if existingSwarm != nil && !force {
			if isJSONOutput() {
				if jerr := outputJSON(map[string]interface{}{
					"error":          "swarm already exists",
					"existing_id":    existingSwarm.ID,
					"existing_title": existingSwarm.Title,
				}); jerr != nil {
					return jerr
				}
				return SilentExit()
			}
			fmt.Printf("%s Swarm already exists: %s\n", ui.RenderWarn("⚠"), ui.RenderID(existingSwarm.ID))
			fmt.Printf("   Use --force to create another.\n")
			return SilentExit()
		}

		epic, err := getStore().GetIssue(ctx, epicID)
		if err != nil {
			return HandleErrorRespectJSON("failed to get epic: %v", err)
		}

		analysis, err := analyzeEpicForSwarm(ctx, getStore(), epic)
		if err != nil {
			return HandleErrorRespectJSON("failed to analyze epic: %v", err)
		}

		if !analysis.Swarmable {
			if isJSONOutput() {
				if jerr := outputJSON(map[string]interface{}{
					"error":    "epic is not swarmable",
					"analysis": analysis,
				}); jerr != nil {
					return jerr
				}
				return SilentExit()
			}
			fmt.Printf("\n%s Epic is not swarmable. Fix errors first:\n", ui.RenderFail("✗"))
			for _, e := range analysis.Errors {
				fmt.Printf("  • %s\n", e)
			}
			return SilentExit()
		}

		swarmMol := &types.Issue{
			IssueContent: types.IssueContent{
				Title:       fmt.Sprintf("Swarm: %s", epicTitle),
				Description: fmt.Sprintf("Swarm molecule orchestrating epic %s.\n\nEpic: %s\nCoordinator: %s", epicID, epicID, coordinator),
			},
			IssueWorkflow: types.IssueWorkflow{
				Status:    types.StatusOpen,
				Priority:  epic.Priority,
				IssueType: "molecule",
				Assignee:  coordinator,
			},
			IssueTimes: types.IssueTimes{
				CreatedBy: getActor(),
			},
			IssueCoord: types.IssueCoord{
				MolType: types.MolTypeSwarm,
			},
		}

		if err := getStore().CreateIssue(ctx, swarmMol, getActor()); err != nil {
			return HandleErrorRespectJSON("failed to create swarm molecule: %v", err)
		}

		dep := &types.Dependency{
			IssueID:     swarmMol.ID,
			DependsOnID: epicID,
			Type:        types.DepRelatesTo,
			CreatedBy:   getActor(),
		}
		if err := getStore().AddDependency(ctx, dep, getActor()); err != nil {
			return HandleErrorRespectJSON("failed to link swarm to epic: %v", err)
		}

		commandDidWrite.Store(true)

		if isJSONOutput() {
			return outputJSON(map[string]interface{}{
				"swarm_id":    swarmMol.ID,
				"epic_id":     epicID,
				"coordinator": coordinator,
				"analysis":    analysis,
			})
		}
		fmt.Printf("\n%s Created swarm molecule: %s\n", ui.RenderPass("✓"), ui.RenderID(swarmMol.ID))
		fmt.Printf("   Epic: %s (%s)\n", epicID, epicTitle)
		if coordinator != "" {
			fmt.Printf("   Coordinator: %s\n", coordinator)
		}
		fmt.Printf("   Total issues: %d\n", analysis.TotalIssues)
		fmt.Printf("   Max parallelism: %d\n", analysis.MaxParallelism)
		fmt.Printf("   Waves: %d\n", len(analysis.ReadyFronts))
		return nil
	},
}

var swarmListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all swarm molecules",
	Long: `List all swarm molecules with their status.

Shows each swarm molecule with:
- Progress (completed/total issues)
- Active workers
- Epic ID and title

Examples:
  bd swarm list         # List all swarms
  bd swarm list --json  # Machine-readable output`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if usesProxiedServer() {
			return HandleErrorRespectJSON("swarm list is not supported in proxied-server mode")
		}
		evt := metrics.NewCommandEvent("swarm-list")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		ctx := getRootContext()

		if getStore() == nil {
			return HandleErrorRespectJSON("no database connection")
		}

		swarmType := types.MolTypeSwarm
		filter := types.IssueFilter{
			IssueFilterFlags: types.IssueFilterFlags{
				MolType: &swarmType,
			},
		}
		swarms, err := getStore().SearchIssues(ctx, "", filter)
		if err != nil {
			return HandleErrorRespectJSON("failed to list swarms: %v", err)
		}

		if len(swarms) == 0 {
			if isJSONOutput() {
				return outputJSON(map[string]interface{}{"swarms": []interface{}{}})
			}
			fmt.Printf("No swarm molecules found.\n")
			return nil
		}

		// Build output with status for each swarm
		type SwarmListItem struct {
			ID          string  `json:"id"`
			Title       string  `json:"title"`
			EpicID      string  `json:"epic_id"`
			EpicTitle   string  `json:"epic_title"`
			Status      string  `json:"status"`
			Coordinator string  `json:"coordinator"`
			Total       int     `json:"total_issues"`
			Completed   int     `json:"completed_issues"`
			Active      int     `json:"active_issues"`
			Progress    float64 `json:"progress_percent"`
		}

		var items []SwarmListItem
		for _, swarm := range swarms {
			item := SwarmListItem{
				ID:          swarm.ID,
				Title:       swarm.Title,
				Status:      string(swarm.Status),
				Coordinator: swarm.Assignee,
			}

			// Find linked epic via relates-to dependency
			deps, err := getStore().GetDependencyRecords(ctx, swarm.ID)
			if err == nil {
				for _, dep := range deps {
					if dep.Type == types.DepRelatesTo {
						item.EpicID = dep.DependsOnID
						epic, err := getStore().GetIssue(ctx, dep.DependsOnID)
						if err == nil && epic != nil {
							item.EpicTitle = epic.Title
							// Get swarm status for this epic
							status, err := getSwarmStatus(ctx, getStore(), epic)
							if err == nil {
								item.Total = status.TotalIssues
								item.Completed = len(status.Completed)
								item.Active = status.ActiveCount
								item.Progress = status.Progress
							}
						}
						break
					}
				}
			}

			items = append(items, item)
		}

		if isJSONOutput() {
			return outputJSON(map[string]interface{}{"swarms": items})
		}

		fmt.Printf("\n%s Active Swarms (%d)\n\n", ui.RenderAccent("●"), len(items))
		for _, item := range items {
			progressStr := fmt.Sprintf("%d/%d", item.Completed, item.Total)
			if item.Active > 0 {
				progressStr += fmt.Sprintf(", %d active", item.Active)
			}

			fmt.Printf("%s %s\n", ui.RenderID(item.ID), item.Title)
			if item.EpicID != "" {
				fmt.Printf("   Epic: %s (%s)\n", item.EpicID, item.EpicTitle)
			}
			fmt.Printf("   Progress: %s (%.0f%%)\n", progressStr, item.Progress)
			if item.Coordinator != "" {
				fmt.Printf("   Coordinator: %s\n", item.Coordinator)
			}
			fmt.Println()
		}
		return nil
	},
}

func init() {
	swarmValidateCmd.Flags().Bool("verbose", false, "Include detailed issue graph in output")
	swarmCreateCmd.Flags().String("coordinator", "", "Coordinator address (e.g., my-project/witness)")
	swarmCreateCmd.Flags().Bool("force", false, "Create new swarm even if one already exists")

	swarmCmd.AddCommand(swarmValidateCmd)
	swarmCmd.AddCommand(swarmStatusCmd)
	swarmCmd.AddCommand(swarmCreateCmd)
	swarmCmd.AddCommand(swarmListCmd)
	rootCmd.AddCommand(swarmCmd)
}
