package main

import (
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

var molShowCmd = &cobra.Command{
	Use:   "show <molecule-id>",
	Short: "Show molecule details",
	Long: `Show molecule structure and details.

The --parallel flag highlights parallelizable steps:
  - Steps with no blocking dependencies can run in parallel
  - Shows which steps are ready to start now
  - Identifies parallel groups (steps that can run concurrently)

Example:
  bd mol show bd-patrol --parallel`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		parallel, _ := cmd.Flags().GetBool("parallel")
		evt := metrics.NewCommandEvent("mol-show")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if usesProxiedServer() {
			return runMolShowProxiedServer(getRootContext(), args[0], parallel)
		}

		ctx := getRootContext()

		if getStore() == nil {
			return HandleErrorRespectJSON("no database connection")
		}

		moleculeID, err := utils.ResolvePartialID(ctx, getStore(), args[0])
		if err != nil {
			return HandleErrorRespectJSON("molecule '%s' not found", args[0])
		}

		subgraph, err := loadTemplateSubgraph(ctx, getStore(), moleculeID)
		if err != nil {
			return HandleErrorRespectJSON("loading molecule: %v", err)
		}

		if parallel {
			return showMoleculeWithParallel(subgraph)
		}
		return showMolecule(subgraph)
	},
}

func showMolecule(subgraph *MoleculeSubgraph) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"root":         subgraph.Root,
			"issues":       subgraph.Issues,
			"dependencies": subgraph.Dependencies,
			"variables":    extractAllVariables(subgraph),
			"is_compound":  subgraph.Root.IsCompound(),
			"bonded_from":  subgraph.Root.BondedFrom,
		})
	}

	// Determine molecule type label
	moleculeType := "Molecule"
	if subgraph.Root.IsCompound() {
		moleculeType = "Compound"
	}

	fmt.Printf("\n%s %s: %s\n", ui.RenderAccent("🧪"), moleculeType, subgraph.Root.Title)
	fmt.Printf("   ID: %s\n", subgraph.Root.ID)
	fmt.Printf("   Steps: %d\n", len(subgraph.Issues))

	// Show compound bonding info if this is a compound molecule
	if subgraph.Root.IsCompound() {
		showCompoundBondingInfo(subgraph.Root)
	}

	vars := extractAllVariables(subgraph)
	if len(vars) > 0 {
		fmt.Printf("\n%s Variables:\n", ui.RenderWarn("📝"))
		for _, v := range vars {
			fmt.Printf("   {{%s}}\n", v)
		}
	}

	fmt.Printf("\n%s Structure:\n", ui.RenderPass("🌲"))
	printMoleculeTree(subgraph, subgraph.Root.ID, 0, true)
	fmt.Println()
	return nil
}

// showCompoundBondingInfo displays the bonding lineage for compound molecules.
// Caller must ensure root.IsCompound() is true.
func showCompoundBondingInfo(root *types.Issue) {
	constituents := root.GetConstituents()
	fmt.Printf("\n%s Bonded from:\n", ui.RenderAccent("🔗"))

	for i, ref := range constituents {
		connector := "├──"
		if i == len(constituents)-1 {
			connector = "└──"
		}

		// Format bond type for display
		bondTypeDisplay := formatBondType(ref.BondType)

		// Show source ID with bond type
		if ref.BondPoint != "" {
			fmt.Printf("   %s %s (%s, at %s)\n", connector, ref.SourceID, bondTypeDisplay, ref.BondPoint)
		} else {
			fmt.Printf("   %s %s (%s)\n", connector, ref.SourceID, bondTypeDisplay)
		}
	}
}

// formatBondType returns a human-readable bond type description
func formatBondType(bondType string) string {
	switch bondType {
	case types.BondTypeSequential:
		return "sequential"
	case types.BondTypeParallel:
		return "parallel"
	case types.BondTypeConditional:
		return "on-failure"
	case types.BondTypeRoot:
		return "root"
	default:
		if bondType == "" {
			return "default"
		}
		return bondType
	}
}

// ParallelInfo holds parallel analysis information for a step
type ParallelInfo struct {
	StepID        string   `json:"step_id"`
	Status        string   `json:"status"`
	IsReady       bool     `json:"is_ready"`       // Can start now (no blocking deps)
	ParallelGroup string   `json:"parallel_group"` // Group ID (steps with same group can parallelize)
	BlockedBy     []string `json:"blocked_by"`     // IDs of open steps blocking this one
	Blocks        []string `json:"blocks"`         // IDs of steps this one blocks
	CanParallel   []string `json:"can_parallel"`   // IDs of steps that can run in parallel with this
}

// ParallelAnalysis holds the complete parallel analysis for a molecule
type ParallelAnalysis struct {
	MoleculeID     string                   `json:"molecule_id"`
	TotalSteps     int                      `json:"total_steps"`
	ReadySteps     int                      `json:"ready_steps"`
	ParallelGroups map[string][]string      `json:"parallel_groups"` // group ID -> step IDs
	Steps          map[string]*ParallelInfo `json:"steps"`
}

// analyzeMoleculeParallel performs parallel detection on a molecule subgraph.
// Returns analysis of which steps can run in parallel.
func analyzeMoleculeParallel(subgraph *MoleculeSubgraph) *ParallelAnalysis {
	analysis := newParallelAnalysis(subgraph)
	blockedBy, blocks, parentChildren := buildParallelDepMaps(subgraph)
	applyParallelDependencies(subgraph, blockedBy, blocks, parentChildren)
	fillParallelStepInfo(subgraph, analysis, blockedBy, blocks)
	groupParallelSteps(subgraph, analysis, blockedBy, blocks)
	return analysis
}

// calculateBlockingDepths calculates the "blocking depth" of each step.
// Depth 0 = no blockers, Depth 1 = blocked by depth-0 steps, etc.
func calculateBlockingDepths(subgraph *MoleculeSubgraph, blockedBy map[string]map[string]bool) map[string]int {
	depths := make(map[string]int)
	visited := make(map[string]bool)

	var calculateDepth func(id string) int
	calculateDepth = func(id string) int {
		if d, ok := depths[id]; ok {
			return d
		}
		if visited[id] {
			// Cycle detected, return 0 to break
			return 0
		}
		visited[id] = true

		maxBlockerDepth := -1
		for blockerID := range blockedBy[id] {
			// Only count open blockers
			blocker := subgraph.IssueMap[blockerID]
			if blocker != nil && blocker.Status != types.StatusClosed {
				blockerDepth := calculateDepth(blockerID)
				if blockerDepth > maxBlockerDepth {
					maxBlockerDepth = blockerDepth
				}
			}
		}

		depth := maxBlockerDepth + 1
		depths[id] = depth
		return depth
	}

	for _, issue := range subgraph.Issues {
		calculateDepth(issue.ID)
	}

	return depths
}

func showMoleculeWithParallel(subgraph *MoleculeSubgraph) error {
	analysis := analyzeMoleculeParallel(subgraph)

	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"root":         subgraph.Root,
			"issues":       subgraph.Issues,
			"dependencies": subgraph.Dependencies,
			"variables":    extractAllVariables(subgraph),
			"parallel":     analysis,
			"is_compound":  subgraph.Root.IsCompound(),
			"bonded_from":  subgraph.Root.BondedFrom,
		})
	}

	// Determine molecule type label
	moleculeType := "Molecule"
	if subgraph.Root.IsCompound() {
		moleculeType = "Compound"
	}

	fmt.Printf("\n%s %s: %s\n", ui.RenderAccent("🧪"), moleculeType, subgraph.Root.Title)
	fmt.Printf("   ID: %s\n", subgraph.Root.ID)
	fmt.Printf("   Steps: %d (%d ready)\n", analysis.TotalSteps, analysis.ReadySteps)

	// Show compound bonding info if this is a compound molecule
	if subgraph.Root.IsCompound() {
		showCompoundBondingInfo(subgraph.Root)
	}

	// Show parallel groups summary
	if len(analysis.ParallelGroups) > 0 {
		fmt.Printf("\n%s Parallel Groups:\n", ui.RenderPass("⚡"))
		for groupName, members := range analysis.ParallelGroups {
			fmt.Printf("   %s: %s\n", groupName, strings.Join(members, ", "))
		}
	}

	vars := extractAllVariables(subgraph)
	if len(vars) > 0 {
		fmt.Printf("\n%s Variables:\n", ui.RenderWarn("📝"))
		for _, v := range vars {
			fmt.Printf("   {{%s}}\n", v)
		}
	}

	fmt.Printf("\n%s Structure:\n", ui.RenderPass("🌲"))
	printMoleculeTreeWithParallel(subgraph, analysis, subgraph.Root.ID, 0, true)
	fmt.Println()
	return nil
}

// printMoleculeTreeWithParallel prints the molecule structure with parallel annotations.
// Uses a visited set to detect cycles (GH#2719) and avoid infinite recursion.
func printMoleculeTreeWithParallel(subgraph *MoleculeSubgraph, analysis *ParallelAnalysis, parentID string, depth int, isRoot bool) {
	visited := make(map[string]bool)
	printMoleculeTreeWithParallelVisited(subgraph, analysis, parentID, depth, isRoot, visited)
}

// printMoleculeTreeWithParallelVisited is the internal recursive implementation with cycle tracking.
func printMoleculeTreeWithParallelVisited(subgraph *MoleculeSubgraph, analysis *ParallelAnalysis, parentID string, depth int, isRoot bool, visited map[string]bool) {
	indent := strings.Repeat("  ", depth)

	// Print root with parallel info
	if isRoot {
		rootInfo := analysis.Steps[subgraph.Root.ID]
		annotation := getParallelAnnotation(rootInfo)
		fmt.Printf("%s   %s%s\n", indent, subgraph.Root.Title, annotation)
		visited[parentID] = true
	}

	// Find children of this parent
	var children []*types.Issue
	for _, dep := range subgraph.Dependencies {
		if dep.DependsOnID == parentID && dep.Type == types.DepParentChild {
			if child, ok := subgraph.IssueMap[dep.IssueID]; ok {
				children = append(children, child)
			}
		}
	}

	// Print children
	for i, child := range children {
		connector := "├──"
		if i == len(children)-1 {
			connector = "└──"
		}

		info := analysis.Steps[child.ID]
		annotation := getParallelAnnotation(info)

		// Cycle detection (GH#2719)
		if visited[child.ID] {
			fmt.Printf("%s   %s %s%s (cycle detected, skipping)\n", indent, connector, child.Title, annotation)
			continue
		}
		fmt.Printf("%s   %s %s%s\n", indent, connector, child.Title, annotation)
		visited[child.ID] = true
		printMoleculeTreeWithParallelVisited(subgraph, analysis, child.ID, depth+1, false, visited)
	}
}

// getParallelAnnotation returns the annotation string for a step's parallel status
func getParallelAnnotation(info *ParallelInfo) string {
	if info == nil {
		return ""
	}

	parts := []string{}

	// Status indicator
	switch info.Status {
	case string(types.StatusOpen):
		if info.IsReady {
			parts = append(parts, ui.RenderPass("ready"))
		} else {
			parts = append(parts, ui.RenderFail("blocked"))
		}
	case string(types.StatusInProgress):
		parts = append(parts, ui.RenderWarn("in_progress"))
	case string(types.StatusClosed):
		parts = append(parts, ui.RenderPass("completed"))
	}

	// Parallel group
	if info.ParallelGroup != "" {
		parts = append(parts, ui.RenderAccent(info.ParallelGroup))
	}

	// Blocking info
	if len(info.BlockedBy) > 0 {
		parts = append(parts, fmt.Sprintf("needs: %s", strings.Join(info.BlockedBy, ", ")))
	}

	if len(parts) == 0 {
		return ""
	}
	return " [" + strings.Join(parts, " | ") + "]"
}

func init() {
	molShowCmd.Flags().BoolP("parallel", "p", false, "Show parallel step analysis")
	molCmd.AddCommand(molShowCmd)
}
