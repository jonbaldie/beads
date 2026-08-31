package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
)

// renderGraph renders the ASCII visualization
func renderGraph(layout *GraphLayout, subgraph *TemplateSubgraph) {
	if len(layout.Nodes) == 0 {
		fmt.Println("Empty graph")
		return
	}
	fmt.Printf("\n%s Dependency graph for %s:\n\n", ui.RenderAccent("📊"), layout.RootID)
	boxWidth := graphBoxWidth(layout)
	fmt.Println("  Status: ○ open  ◐ in_progress  ● blocked  ✓ closed")
	fmt.Println()
	blocksCounts, blockedByCounts := computeDependencyCounts(subgraph)
	layerBoxes := graphLayerBoxes(layout, boxWidth, blocksCounts, blockedByCounts)
	renderGraphLayers(layerBoxes)
	renderGraphDependencySummary(subgraph)
	fmt.Printf("  Total: %d issues across %d layers\n\n", len(layout.Nodes), len(layout.Layers))
}

func graphBoxWidth(layout *GraphLayout) int {
	maxTitleLen := 0
	for _, node := range layout.Nodes {
		titleLen := len(truncateTitle(node.Issue.Title, 30))
		if titleLen > maxTitleLen {
			maxTitleLen = titleLen
		}
	}
	return maxTitleLen + 4
}

func graphLayerBoxes(layout *GraphLayout, boxWidth int, blocksCounts, blockedByCounts map[string]int) [][]string {
	layerBoxes := make([][]string, len(layout.Layers))
	for layerIdx, layer := range layout.Layers {
		for _, id := range layer {
			node := layout.Nodes[id]
			box := renderNodeBoxWithDeps(node, boxWidth, blocksCounts[id], blockedByCounts[id])
			layerBoxes[layerIdx] = append(layerBoxes[layerIdx], box)
		}
	}
	return layerBoxes
}

func renderGraphLayers(layerBoxes [][]string) {
	for layerIdx, boxes := range layerBoxes {
		fmt.Printf("  Layer %d", layerIdx)
		if layerIdx == 0 {
			fmt.Print(" (ready)")
		}
		fmt.Println()
		for _, box := range boxes {
			fmt.Println(box)
		}
		if layerIdx < len(layerBoxes)-1 {
			fmt.Println("      │")
			fmt.Println("      ▼")
		}
		fmt.Println()
	}
}

func renderGraphDependencySummary(subgraph *TemplateSubgraph) {
	if len(subgraph.Dependencies) == 0 {
		return
	}
	blocksDeps := 0
	for _, dep := range subgraph.Dependencies {
		if dep.Type == types.DepBlocks {
			blocksDeps++
		}
	}
	if blocksDeps > 0 {
		fmt.Printf("  Dependencies: %d blocking relationships\n", blocksDeps)
	}
}

// renderGraphCompact renders the graph in compact tree format
// One line per issue, more scannable, uses tree connectors (├──, └──, │)
func renderGraphCompact(layout *GraphLayout, subgraph *TemplateSubgraph) {
	if len(layout.Nodes) == 0 {
		fmt.Println("Empty graph")
		return
	}
	fmt.Printf("\n%s Dependency graph for %s (%d issues, %d layers)\n\n",
		ui.RenderAccent("📊"), layout.RootID, len(layout.Nodes), len(layout.Layers))
	fmt.Println("  Status: ○ open  ◐ in_progress  ● blocked  ✓ closed  ❄ deferred")
	fmt.Println()
	children := compactGraphChildren(subgraph.Dependencies)
	sortCompactGraphChildren(layout, children)
	renderCompactGraphLayers(layout, children)
}

func compactGraphChildren(dependencies []*types.Dependency) map[string][]string {
	children := make(map[string][]string)
	for _, dep := range dependencies {
		if dep.Type == types.DepParentChild {
			children[dep.DependsOnID] = append(children[dep.DependsOnID], dep.IssueID)
		}
	}
	return children
}

func sortCompactGraphChildren(layout *GraphLayout, children map[string][]string) {
	for parentID := range children {
		sort.Slice(children[parentID], func(i, j int) bool {
			nodeI := layout.Nodes[children[parentID][i]]
			nodeJ := layout.Nodes[children[parentID][j]]
			if nodeI.Issue.Priority != nodeJ.Issue.Priority {
				return nodeI.Issue.Priority < nodeJ.Issue.Priority
			}
			return utils.NaturalCompareIDs(nodeI.Issue.ID, nodeJ.Issue.ID) < 0
		})
	}
}

func renderCompactGraphLayers(layout *GraphLayout, children map[string][]string) {
	for layerIdx, layer := range layout.Layers {
		layerHeader := fmt.Sprintf("LAYER %d", layerIdx)
		if layerIdx == 0 {
			layerHeader += " (ready)"
		}
		fmt.Printf("  %s\n", ui.RenderAccent(layerHeader))

		for i, id := range layer {
			node := layout.Nodes[id]
			isLast := i == len(layer)-1
			line := formatCompactNode(node)
			connector := "├── "
			if isLast {
				connector = "└── "
			}

			fmt.Printf("  %s%s\n", connector, line)
			if childIDs, ok := children[id]; ok && len(childIDs) > 0 {
				childPrefix := "│   "
				if isLast {
					childPrefix = "    "
				}
				renderCompactChildren(layout, childIDs, children, childPrefix, 1)
			}
		}
		fmt.Println()
	}
}

// renderCompactChildren recursively renders children in tree format
func renderCompactChildren(layout *GraphLayout, childIDs []string, children map[string][]string, prefix string, depth int) {
	for i, childID := range childIDs {
		node := layout.Nodes[childID]
		if node == nil {
			continue
		}

		isLast := i == len(childIDs)-1
		connector := "├── "
		if isLast {
			connector = "└── "
		}

		line := formatCompactNode(node)
		fmt.Printf("  %s%s%s\n", prefix, connector, line)

		// Recurse for nested children
		if grandchildren, ok := children[childID]; ok && len(grandchildren) > 0 {
			childPrefix := prefix
			if isLast {
				childPrefix += "    "
			} else {
				childPrefix += "│   "
			}
			renderCompactChildren(layout, grandchildren, children, childPrefix, depth+1)
		}
	}
}

// formatCompactNode formats a single node for compact output
// Format: STATUS_ICON ID PRIORITY Title
func formatCompactNode(node *GraphNode) string {
	status := string(node.Issue.Status)

	// Use shared status icon with semantic color
	statusIcon := ui.RenderStatusIcon(status)

	// Priority with semantic color (P-label only)
	priorityTag := ui.RenderPriority(node.Issue.Priority)

	// Title - truncate if too long
	title := truncateTitle(node.Issue.Title, 50)

	// Build line - apply status style to entire line for closed issues
	style := ui.GetStatusStyle(status)
	if node.Issue.Status == types.StatusClosed {
		return fmt.Sprintf("%s %s %s %s",
			statusIcon,
			style.Render(node.Issue.ID),
			style.Render(fmt.Sprintf("P%d", node.Issue.Priority)),
			style.Render(title))
	}

	return fmt.Sprintf("%s %s %s %s", statusIcon, node.Issue.ID, priorityTag, title)
}

// renderNodeBox renders a single node as an ASCII box
// Uses semantic status styles from ui package for consistency
func renderNodeBox(node *GraphNode, width int) string {
	title := truncateTitle(node.Issue.Title, width-4)
	paddedTitle := padRight(title, width-4)
	status := string(node.Issue.Status)

	// Use shared status icon and style
	statusIcon := ui.RenderStatusIcon(status)
	style := ui.GetStatusStyle(status)

	// Apply style to title for actionable statuses
	var titleStr string
	if node.Issue.Status == types.StatusOpen {
		titleStr = paddedTitle // no color for open - available but not urgent
	} else {
		titleStr = style.Render(paddedTitle)
	}

	id := node.Issue.ID

	// Build the box
	topBottom := "  ┌" + strings.Repeat("─", width) + "┐"
	middle := fmt.Sprintf("  │ %s %s │", statusIcon, titleStr)
	idLine := fmt.Sprintf("  │ %s │", ui.RenderMuted(padRight(id, width-2)))
	bottom := "  └" + strings.Repeat("─", width) + "┘"

	return topBottom + "\n" + middle + "\n" + idLine + "\n" + bottom
}

// truncateTitle truncates a title to max length (rune-safe)
func truncateTitle(title string, maxLen int) string {
	runes := []rune(title)
	if len(runes) <= maxLen {
		return title
	}
	return string(runes[:maxLen-1]) + "…"
}

// padRight pads a string to the right with spaces (rune-safe)
func padRight(s string, width int) string {
	runes := []rune(s)
	if len(runes) >= width {
		return string(runes[:width])
	}
	return s + strings.Repeat(" ", width-len(runes))
}

// computeDependencyCounts calculates how many issues each issue blocks and is blocked by
// Excludes parent-child relationships and the root issue from counts to reduce cognitive noise
func computeDependencyCounts(subgraph *TemplateSubgraph) (blocks map[string]int, blockedBy map[string]int) {
	blocks = make(map[string]int)
	blockedBy = make(map[string]int)

	if subgraph == nil {
		return blocks, blockedBy
	}

	rootID := ""
	if subgraph.Root != nil {
		rootID = subgraph.Root.ID
	}

	for _, dep := range subgraph.Dependencies {
		// Only count "blocks" dependencies (not parent-child, related, etc.)
		if dep.Type != types.DepBlocks {
			continue
		}

		// Skip if the blocker is the root issue - this is obvious from graph structure
		// and showing "needs:1" when it's just the parent epic is cognitive noise
		if dep.DependsOnID == rootID {
			continue
		}

		// dep.DependsOnID blocks dep.IssueID
		// So dep.DependsOnID "blocks" count increases
		blocks[dep.DependsOnID]++
		// And dep.IssueID "blocked by" count increases
		blockedBy[dep.IssueID]++
	}

	return blocks, blockedBy
}

// renderNodeBoxWithDeps renders a node box with dependency information
// Uses semantic status styles from ui package for consistency across commands
// Design principle: only actionable states get color, closed items fade
func renderNodeBoxWithDeps(node *GraphNode, width int, blocksCount int, blockedByCount int) string {
	title := truncateTitle(node.Issue.Title, width-4)
	paddedTitle := padRight(title, width-4)
	status := string(node.Issue.Status)

	// Use shared status icon and style from ui package
	statusIcon := ui.RenderStatusIcon(status)
	style := ui.GetStatusStyle(status)

	// Apply style to title for actionable statuses
	var titleStr string
	if node.Issue.Status == types.StatusOpen {
		titleStr = paddedTitle // no color for open - available but not urgent
	} else {
		titleStr = style.Render(paddedTitle)
	}

	id := node.Issue.ID

	// Build dependency info string - only show if meaningful counts exist
	// Note: we build the plain text version first for padding, then apply colors
	var depInfoPlain string
	var depInfoStyled string
	if blocksCount > 0 || blockedByCount > 0 {
		plainParts := []string{}
		styledParts := []string{}
		if blocksCount > 0 {
			plainText := fmt.Sprintf("blocks:%d", blocksCount)
			plainParts = append(plainParts, plainText)
			// Use semantic color for blocks indicator - attention-grabbing
			styledParts = append(styledParts, ui.StatusBlockedStyle().Render(plainText))
		}
		if blockedByCount > 0 {
			plainText := fmt.Sprintf("needs:%d", blockedByCount)
			plainParts = append(plainParts, plainText)
			// Use muted color for needs indicator - informational
			styledParts = append(styledParts, ui.MutedStyle().Render(plainText))
		}
		depInfoPlain = strings.Join(plainParts, " ")
		depInfoStyled = strings.Join(styledParts, " ")
	}

	// Build the box
	topBottom := "  ┌" + strings.Repeat("─", width) + "┐"
	middle := fmt.Sprintf("  │ %s %s │", statusIcon, titleStr)
	idLine := fmt.Sprintf("  │ %s │", ui.RenderMuted(padRight(id, width-2)))

	var result string
	if depInfoPlain != "" {
		// Pad based on plain text length, then render with styled version
		padding := width - 2 - len([]rune(depInfoPlain))
		if padding < 0 {
			padding = 0
		}
		depLine := fmt.Sprintf("  │ %s%s │", depInfoStyled, strings.Repeat(" ", padding))
		bottom := "  └" + strings.Repeat("─", width) + "┘"
		result = topBottom + "\n" + middle + "\n" + idLine + "\n" + depLine + "\n" + bottom
	} else {
		bottom := "  └" + strings.Repeat("─", width) + "┘"
		result = topBottom + "\n" + middle + "\n" + idLine + "\n" + bottom
	}

	return result
}
