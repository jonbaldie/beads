package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
)

// dagEdgeInfo represents a directed edge routed through a gutter column
type dagEdgeInfo struct {
	sourceRow int
	targetRow int
}

type dagEdgeKey struct {
	sourceRow int
	targetRow int
}

// renderGraphVisual renders a terminal-native DAG with nodes arranged in
// layer columns (left-to-right) and box-drawing edges between them.
// Each layer is a vertical column of node boxes, with edges drawn in
// gutter areas between columns.
func renderGraphVisual(layout *GraphLayout, subgraph *TemplateSubgraph) {
	renderGraphVisualTo(os.Stdout, layout, subgraph)
}

// renderGraphVisualTo is the writer-aware test seam for the terminal visual
// renderer. The production wrapper intentionally retains its existing stdout
// destination and ignored write-error behavior.
func renderGraphVisualTo(out io.Writer, layout *GraphLayout, subgraph *TemplateSubgraph) {
	if len(layout.Nodes) == 0 {
		writeGraphVisualLine(out, "Empty graph")
		return
	}

	writeGraphVisualf(out, "\n%s Dependency graph for %s:\n\n", ui.RenderAccent("▦"), layout.RootID)
	writeGraphVisualLine(out, "  Status: ○ open  ◐ in_progress  ● blocked  ✓ closed  ❄ deferred")
	writeGraphVisualLine(out)

	numLayers := len(layout.Layers)
	if numLayers == 0 {
		return
	}

	nodeW := computeDAGNodeWidth(layout)
	maxRows := maxDAGRows(layout.Layers)
	gutterEdges := collectGutterEdges(layout, subgraph, numLayers)

	const nodeH = 4  // top border, title, id, bottom border
	const rowGap = 1 // gap between vertically stacked nodes
	bandH := nodeH + rowGap
	gutterW := 6
	totalLines := maxRows*bandH - rowGap
	gutterGrids := buildDAGGutterGrids(gutterEdges, gutterW, totalLines, bandH)
	renderDAGHeaders(out, layout.Layers, nodeW, gutterW)
	renderDAGLines(out, layout, gutterGrids, nodeW, gutterW, bandH, totalLines)
	renderDAGSummary(out, layout, subgraph)
}

func writeGraphVisualf(out io.Writer, format string, args ...interface{}) {
	_, _ = fmt.Fprintf(out, format, args...)
}

func writeGraphVisualLine(out io.Writer, args ...interface{}) {
	_, _ = fmt.Fprintln(out, args...)
}

func maxDAGRows(layers [][]string) int {
	maxRows := 0
	for _, layer := range layers {
		if len(layer) > maxRows {
			maxRows = len(layer)
		}
	}
	return maxRows
}

func buildDAGGutterGrids(edges [][]dagEdgeInfo, gutterW, totalLines, bandH int) [][]string {
	grids := make([][]string, len(edges))
	for gutter := range edges {
		grids[gutter] = buildDAGGutterGrid(edges[gutter], gutterW, totalLines, bandH)
	}
	return grids
}

func renderDAGHeaders(out io.Writer, layers [][]string, nodeW, gutterW int) {
	var headerLine strings.Builder
	headerLine.WriteString("  ")
	for layerIdx := range layers {
		header := fmt.Sprintf("LAYER %d", layerIdx)
		if layerIdx == 0 {
			header += " (ready)"
		}
		headerLine.WriteString(ui.RenderAccent(padRight(header, nodeW+2)))
		if layerIdx < len(layers)-1 {
			headerLine.WriteString(strings.Repeat(" ", gutterW))
		}
	}
	writeGraphVisualLine(out, headerLine.String())
	writeGraphVisualLine(out)
}

func renderDAGLines(out io.Writer, layout *GraphLayout, gutterGrids [][]string, nodeW, gutterW, bandH, totalLines int) {
	for y := 0; y < totalLines; y++ {
		row := y / bandH
		subLine := y % bandH
		var line strings.Builder
		line.WriteString("  ")
		for layerIdx, layer := range layout.Layers {
			line.WriteString(dagLayerLine(layout.Nodes, layer, nodeW, row, subLine))
			if layerIdx < len(layout.Layers)-1 {
				line.WriteString(dagGutterLine(gutterGrids[layerIdx], y, gutterW))
			}
		}
		writeGraphVisualLine(out, strings.TrimRight(line.String(), " "))
	}
	writeGraphVisualLine(out)
}

func dagLayerLine(nodes map[string]*GraphNode, layer []string, nodeW, row, subLine int) string {
	const nodeH = 4
	if subLine < nodeH && row < len(layer) {
		return dagNodeLine(nodes[layer[row]], nodeW, subLine)
	}
	return strings.Repeat(" ", nodeW+2)
}

func dagGutterLine(grid []string, line, gutterW int) string {
	if line < len(grid) {
		return grid[line]
	}
	return strings.Repeat(" ", gutterW)
}

func renderDAGSummary(out io.Writer, layout *GraphLayout, subgraph *TemplateSubgraph) {
	blocksDeps := 0
	for _, dep := range subgraph.Dependencies {
		if dep.Type == types.DepBlocks {
			blocksDeps++
		}
	}
	if blocksDeps > 0 {
		writeGraphVisualf(out, "  Dependencies: %d blocking relationships\n", blocksDeps)
	}
	writeGraphVisualf(out, "  Total: %d issues across %d layers\n\n", len(layout.Nodes), len(layout.Layers))
}

// computeDAGNodeWidth calculates a consistent width for all DAG node boxes
func computeDAGNodeWidth(layout *GraphLayout) int {
	maxW := 0
	for _, node := range layout.Nodes {
		titleLen := len([]rune(truncateTitle(node.Issue.Title, 22)))
		contentW := titleLen + 3      // icon(1) + space(1) + trailing(1)
		idW := len(node.Issue.ID) + 4 // space + ID + "  Pn"
		if idW > contentW {
			contentW = idW
		}
		if contentW > maxW {
			maxW = contentW
		}
	}
	w := maxW + 2 // inner padding
	if w < 18 {
		w = 18
	}
	return w
}

// collectGutterEdges organizes blocking dependencies by gutter index.
// For edges spanning multiple layers, intermediate gutters get pass-through entries.
func collectGutterEdges(layout *GraphLayout, subgraph *TemplateSubgraph, numLayers int) [][]dagEdgeInfo {
	result := make([][]dagEdgeInfo, numLayers-1)

	// Deduplicate edges per gutter
	seen := make([]map[dagEdgeKey]bool, numLayers-1)
	for i := range seen {
		seen[i] = make(map[dagEdgeKey]bool)
	}

	for _, dep := range subgraph.Dependencies {
		if dep.Type != types.DepBlocks {
			continue
		}
		src := layout.Nodes[dep.DependsOnID]
		tgt := layout.Nodes[dep.IssueID]
		if !validDAGEdge(src, tgt) {
			continue
		}
		routeDAGEdge(result, seen, src, tgt)
	}

	return result
}

func validDAGEdge(source, target *GraphNode) bool {
	return source != nil && target != nil && target.Layer > source.Layer
}

func routeDAGEdge(result [][]dagEdgeInfo, seen []map[dagEdgeKey]bool, source, target *GraphNode) {
	for gutter := source.Layer; gutter < target.Layer; gutter++ {
		sourceRow := target.Position
		if gutter == source.Layer {
			sourceRow = source.Position
		}
		key := dagEdgeKey{sourceRow: sourceRow, targetRow: target.Position}
		if seen[gutter][key] {
			continue
		}
		seen[gutter][key] = true
		result[gutter] = append(result[gutter], dagEdgeInfo{sourceRow: sourceRow, targetRow: target.Position})
	}
}

// dagNodeLine renders one line of a DAG node box with status colors
func dagNodeLine(node *GraphNode, nodeW, lineIdx int) string {
	switch lineIdx {
	case 0: // top border
		return "┌" + strings.Repeat("─", nodeW) + "┐"

	case 1: // status icon + title
		icon := ui.RenderStatusIcon(string(node.Issue.Status))
		title := truncateTitle(node.Issue.Title, nodeW-4) // room for icon + spaces
		padded := padRight(title, nodeW-4)

		status := string(node.Issue.Status)
		style := ui.GetStatusStyle(status)
		styled := padded
		if node.Issue.Status != types.StatusOpen {
			styled = style.Render(padded)
		}
		return fmt.Sprintf("│ %s %s │", icon, styled)

	case 2: // ID + priority
		idPri := fmt.Sprintf("%s P%d", node.Issue.ID, node.Issue.Priority)
		return "│ " + ui.RenderMuted(padRight(idPri, nodeW-2)) + " │"

	case 3: // bottom border
		return "└" + strings.Repeat("─", nodeW) + "┘"

	default:
		return strings.Repeat(" ", nodeW+2)
	}
}

// buildDAGGutterGrid precomputes the edge routing display for a gutter.
// Returns one string per output line, containing box-drawing characters
// that connect nodes between adjacent layer columns.
func buildDAGGutterGrid(edges []dagEdgeInfo, gutterW, totalLines, bandH int) []string {
	grid := newDAGRuneGrid(totalLines, gutterW)
	channelPositions := dagChannelPositions(edges, gutterW)
	for i, edge := range edges {
		renderDAGGutterEdge(grid, edge, channelPositions[i], gutterW, totalLines, bandH)
	}
	return dagRuneGridStrings(grid)
}

func newDAGRuneGrid(totalLines, gutterW int) [][]rune {
	grid := make([][]rune, totalLines)
	for line := range grid {
		grid[line] = []rune(strings.Repeat(" ", gutterW))
	}
	return grid
}

func dagChannelPositions(edges []dagEdgeInfo, gutterW int) map[int]int {
	var verticalEdgeIndices []int
	for i, edge := range edges {
		if edge.sourceRow != edge.targetRow {
			verticalEdgeIndices = append(verticalEdgeIndices, i)
		}
	}

	positions := make(map[int]int, len(verticalEdgeIndices))
	for channel, edgeIndex := range verticalEdgeIndices {
		if len(verticalEdgeIndices) == 1 {
			positions[edgeIndex] = gutterW / 2
			continue
		}
		positions[edgeIndex] = 1 + channel*(gutterW-3)/(len(verticalEdgeIndices)-1)
	}
	return positions
}

func renderDAGGutterEdge(grid [][]rune, edge dagEdgeInfo, channel, gutterW, totalLines, bandH int) {
	const contentOffset = 1
	sourceY := edge.sourceRow*bandH + contentOffset
	targetY := edge.targetRow*bandH + contentOffset
	if sourceY >= totalLines || targetY >= totalLines {
		return
	}
	if sourceY == targetY {
		renderDAGHorizontal(grid, sourceY, 0, gutterW-1)
		grid[sourceY][gutterW-1] = '▶'
		return
	}

	minY, maxY := dagOrderedBounds(sourceY, targetY)
	renderDAGHorizontal(grid, sourceY, 0, channel)
	startCorner, endCorner := dagRouteCorners(sourceY, targetY)
	grid[sourceY][channel] = dagMergeRune(grid[sourceY][channel], startCorner)
	for y := minY + 1; y < maxY; y++ {
		grid[y][channel] = dagMergeRune(grid[y][channel], '│')
	}
	grid[targetY][channel] = dagMergeRune(grid[targetY][channel], endCorner)
	renderDAGHorizontal(grid, targetY, channel+1, gutterW-1)
	grid[targetY][gutterW-1] = '▶'
}

func renderDAGHorizontal(grid [][]rune, line, start, end int) {
	for x := start; x < end; x++ {
		grid[line][x] = dagMergeRune(grid[line][x], '─')
	}
}

func dagOrderedBounds(first, second int) (int, int) {
	if first <= second {
		return first, second
	}
	return second, first
}

func dagRouteCorners(sourceY, targetY int) (rune, rune) {
	if sourceY < targetY {
		return '╮', '╰'
	}
	return '╯', '╭'
}

func dagRuneGridStrings(grid [][]rune) []string {
	result := make([]string, len(grid))
	for line := range grid {
		result[line] = string(grid[line])
	}
	return result
}

// dagMergeRune merges a new character into an existing cell, handling overlaps
func dagMergeRune(existing, incoming rune) rune {
	if existing == ' ' {
		return incoming
	}
	if incoming == '▶' {
		return '▶'
	}
	return mergeDAGExisting(existing, incoming)
}

func mergeDAGExisting(existing, incoming rune) rune {
	switch existing {
	case '│':
		return mergeDAGVertical(incoming)
	case '─':
		return mergeDAGHorizontal(incoming)
	}
	return incoming
}

func mergeDAGVertical(incoming rune) rune {
	switch incoming {
	case '─':
		return '┼'
	case '│':
		return '│'
	case '╮', '╯':
		return '┤'
	case '╰', '╭':
		return '├'
	default:
		return incoming
	}
}

func mergeDAGHorizontal(incoming rune) rune {
	switch incoming {
	case '─':
		return '─'
	case '│':
		return '┼'
	case '╮', '╭':
		return '┬'
	case '╰', '╯':
		return '┴'
	default:
		return incoming
	}
}
