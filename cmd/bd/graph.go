package main

import (
	"io"
	"strings"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

// GraphNode represents a node in the rendered graph
type GraphNode struct {
	Issue     *types.Issue
	Layer     int      // Horizontal layer (topological order)
	Position  int      // Vertical position within layer
	DependsOn []string // IDs this node depends on (blocks dependencies only)
}

// GraphLayout holds the computed graph layout
type GraphLayout struct {
	Nodes    map[string]*GraphNode
	Layers   [][]string // Layer index -> node IDs in that layer
	MaxLayer int
	RootID   string
}

type graphOptions struct {
	compact bool
	box     bool
	all     bool
	dot     bool
	html    bool
	open    bool
}

func graphOptionsFromCommand(cmd *cobra.Command) graphOptions {
	if cmd == nil {
		return graphOptions{}
	}
	flags := cmd.Flags()
	compact, _ := flags.GetBool("compact")
	box, _ := flags.GetBool("box")
	all, _ := flags.GetBool("all")
	dot, _ := flags.GetBool("dot")
	html, _ := flags.GetBool("html")
	open, _ := flags.GetBool("open")
	return graphOptions{
		compact: compact,
		box:     box,
		all:     all,
		dot:     dot,
		html:    html,
		open:    open,
	}
}

func runGraphCommand(cmd *cobra.Command, args []string) error {
	opts := graphOptionsFromCommand(cmd)
	evt := metrics.NewCommandEvent("graph")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	if err := validateGraphArgs(opts, args); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if usesProxiedServer() {
		if err := rejectMaxRowsUnderProxiedServer(cmd); err != nil {
			return err
		}
		return runGraphProxiedServer(getRootContext(), out, args, opts)
	}
	return runGraphEmbedded(cmd, out, args, opts)
}

func validateGraphArgs(opts graphOptions, args []string) error {
	if opts.all && len(args) > 0 {
		return HandleErrorRespectJSON("cannot specify issue ID with --all flag")
	}
	if !opts.all && len(args) == 0 {
		return HandleErrorWithHintRespectJSON("issue ID required", "Use --all for all open issues")
	}
	return nil
}

func runGraphEmbedded(cmd *cobra.Command, out io.Writer, args []string, opts graphOptions) error {
	if getStore() == nil {
		return HandleErrorRespectJSON("no database connection")
	}
	if opts.all {
		return runGraphAllEmbedded(cmd, out, opts)
	}
	return runGraphSingleEmbedded(cmd, out, args[0], opts)
}

func runGraphAllEmbedded(cmd *cobra.Command, out io.Writer, opts graphOptions) error {
	maxRows, maxRowsSource, err := resolveMaxRows(cmd)
	if err != nil {
		return err
	}
	subgraphs, err := loadAllGraphSubgraphs(getRootContext(), getStore(), maxRows, maxRowsSource)
	if err != nil {
		if capErr := handleMaxRowsError(err); capErr != nil {
			return capErr
		}
		return HandleErrorRespectJSON("loading all issues: %v", err)
	}
	return renderGraphAllSubgraphs(out, subgraphs, opts)
}

func runGraphSingleEmbedded(cmd *cobra.Command, out io.Writer, issueArg string, opts graphOptions) error {
	issueID, err := utils.ResolvePartialID(getRootContext(), getStore(), issueArg)
	if err != nil {
		return HandleErrorRespectJSON("issue '%s' not found", issueArg)
	}
	subgraph, err := loadGraphSubgraph(getRootContext(), getStore(), issueID)
	if err != nil {
		return HandleErrorRespectJSON("loading graph: %v", err)
	}
	if err := enforceGraphMaxRows(cmd, len(subgraph.Issues)); err != nil {
		return err
	}
	return renderGraphSingleSubgraph(out, subgraph, opts)
}

func enforceGraphMaxRows(cmd *cobra.Command, issueCount int) error {
	graphMaxRows, graphMaxRowsSource, err := resolveMaxRows(cmd)
	if err != nil {
		return err
	}
	if graphMaxRows > 0 && issueCount > graphMaxRows {
		if capErr := handleMaxRowsError(&issueops.ErrTooManyRows{
			Found:  issueCount,
			Cap:    graphMaxRows,
			Source: graphMaxRowsSource,
		}); capErr != nil {
			return capErr
		}
	}
	return nil
}

func newGraphCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "graph [issue-id]",
		GroupID: "deps",
		Short:   "Display issue dependency graph",
		Long: `Display a visualization of an issue's dependency graph.

For epics, shows all children and their dependencies.
For regular issues, shows the issue and its direct dependencies.

With --all, shows all open issues grouped by connected component.
With --open, filters to only open/actionable issues (compact layer format).

Display formats:
  (default)        DAG with columns and box-drawing edges (terminal-native)
  --box            ASCII boxes showing layers, more detailed
  --compact        Tree format, one line per issue, more scannable
  --dot            Graphviz DOT format (pipe to dot -Tsvg > graph.svg)
  --html           Self-contained interactive HTML with D3.js visualization
  --open           Open issues only, compact layers (LLM-friendly)

The graph shows execution order:
- Layer 0 / leftmost = no dependencies (can start immediately)
- Higher layers depend on lower layers
- Nodes in the same layer can run in parallel

Status icons: ○ open  ◐ in_progress  ● blocked  ✓ closed  ❄ deferred

Examples:
  bd graph issue-id              # Terminal DAG visualization (default)
  bd graph --box issue-id        # ASCII boxes with layer grouping
  bd graph --dot issue-id | dot -Tsvg > graph.svg  # SVG via Graphviz
  bd graph --dot issue-id | dot -Tpng > graph.png  # PNG via Graphviz
  bd graph --html issue-id > graph.html  # Interactive browser view
  bd graph --all --html > all.html       # All issues, interactive
  bd graph --open issue-id       # Open issues only, layered by blocking order
  bd graph --all --open          # All open issues, compact layers

--max-rows / BEADS_MAX_ROWS caveat: the cap is checked differently per mode.
Single-issue graphs (no --all) check the connected-component node count
after the BFS traversal completes — the whole subgraph is always walked
first, then rejected if it's over cap. --all checks each status
(open/in_progress/blocked) independently, so up to 3x the cap can be loaded
in total before any individual status trips it.`,
		Args:          cobra.RangeArgs(0, 1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runGraphCommand,
	}
}

func writeGraphLine(out io.Writer, value string) error {
	w := &graphExportWriter{out: out}
	w.println(value)
	return w.wrapError("graph")
}

func renderGraphAllSubgraphs(out io.Writer, subgraphs []*TemplateSubgraph, opts graphOptions) error {
	if opts.open {
		subgraphs = filterOpenGraphSubgraphs(subgraphs)
	}
	if len(subgraphs) == 0 {
		return writeGraphLine(out, "No open issues found")
	}

	if isJSONOutput() {
		return outputJSON(subgraphs)
	}

	if opts.html && !opts.open {
		merged := mergeSubgraphsForHTML(subgraphs)
		layout := computeLayout(merged)
		return renderGraphHTML(out, layout, merged)
	}
	if opts.open {
		return renderOpenGraphSubgraphs(out, subgraphs)
	}
	return renderGraphSubgraphs(out, subgraphs, opts)
}

func filterOpenGraphSubgraphs(subgraphs []*TemplateSubgraph) []*TemplateSubgraph {
	var filtered []*TemplateSubgraph
	for _, subgraph := range subgraphs {
		open := filterSubgraphOpen(subgraph)
		if open != nil && len(open.Issues) > 0 {
			filtered = append(filtered, open)
		}
	}
	return filtered
}

func renderOpenGraphSubgraphs(out io.Writer, subgraphs []*TemplateSubgraph) error {
	for i, subgraph := range subgraphs {
		layout := computeLayout(subgraph)
		renderGraphCompact(layout, subgraph)
		if i < len(subgraphs)-1 {
			if err := writeGraphLine(out, strings.Repeat("─", 60)); err != nil {
				return err
			}
		}
	}
	return nil
}

func renderGraphSubgraphs(out io.Writer, subgraphs []*TemplateSubgraph, opts graphOptions) error {
	for i, subgraph := range subgraphs {
		layout := computeLayout(subgraph)
		if opts.dot {
			if err := renderGraphDOT(out, layout, subgraph); err != nil {
				return err
			}
		} else if opts.compact {
			renderGraphCompact(layout, subgraph)
		} else if opts.box {
			renderGraph(layout, subgraph)
		} else {
			renderGraphVisual(layout, subgraph)
		}
		if !opts.dot && i < len(subgraphs)-1 {
			if err := writeGraphLine(out, strings.Repeat("─", 60)); err != nil {
				return err
			}
		}
	}
	return nil
}

func renderGraphSingleSubgraph(out io.Writer, subgraph *TemplateSubgraph, opts graphOptions) error {
	if opts.open {
		subgraph = filterSubgraphOpen(subgraph)
		if subgraph == nil || len(subgraph.Issues) == 0 {
			return writeGraphLine(out, "No open issues in subgraph")
		}
	}

	layout := computeLayout(subgraph)

	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"root":   subgraph.Root,
			"issues": subgraph.Issues,
			"layout": layout,
		})
	}
	return renderSingleGraphFormat(out, layout, subgraph, opts)
}

func renderSingleGraphFormat(out io.Writer, layout *GraphLayout, subgraph *TemplateSubgraph, opts graphOptions) error {
	if opts.open {
		renderGraphCompact(layout, subgraph)
		return nil
	}
	if opts.dot {
		return renderGraphDOT(out, layout, subgraph)
	}
	if opts.html {
		return renderGraphHTML(out, layout, subgraph)
	}
	if opts.compact {
		renderGraphCompact(layout, subgraph)
		return nil
	}
	if opts.box {
		renderGraph(layout, subgraph)
		return nil
	}
	renderGraphVisual(layout, subgraph)
	return nil
}
