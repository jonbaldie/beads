package main

import (
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

var graphCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Check dependency graph integrity",
	Long: `Check the dependency graph for cycles, orphans, and other integrity issues.

Returns exit code 0 if the graph is clean, 1 if issues are found.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("graph-check")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if usesProxiedServer() {
			return runGraphCheckProxiedServer(getRootContext())
		}

		cycles, err := getStore().DetectCycles(getRootContext())
		if err != nil {
			return HandleErrorRespectJSON("cycle detection failed: %v", err)
		}
		return renderGraphCheck(cycles)
	},
}

type graphCheckReport struct {
	Clean   bool       `json:"clean"`
	Cycles  [][]string `json:"cycles"`
	Summary struct {
		CycleCount int `json:"cycle_count"`
	} `json:"summary"`
}

func renderGraphCheck(cycles [][]*types.Issue) error {
	result := buildGraphCheckReport(cycles)
	if isJSONOutput() {
		return renderGraphCheckJSON(result)
	}
	renderGraphCheckText(result)
	if !result.Clean {
		return SilentExit()
	}
	return nil
}

func buildGraphCheckReport(cycles [][]*types.Issue) graphCheckReport {
	result := graphCheckReport{Clean: true}
	for _, cycle := range cycles {
		ids := make([]string, len(cycle))
		for i, issue := range cycle {
			ids[i] = issue.ID
		}
		result.Cycles = append(result.Cycles, ids)
	}
	result.Summary.CycleCount = len(cycles)

	if len(cycles) > 0 {
		result.Clean = false
	}
	return result
}

func renderGraphCheckJSON(result graphCheckReport) error {
	if err := outputJSON(result); err != nil {
		return err
	}
	if result.Clean {
		return nil
	}
	return SilentExit()
}

func renderGraphCheckText(result graphCheckReport) {
	if result.Clean {
		fmt.Printf("\n%s Graph integrity check passed\n\n", ui.RenderPass("✓"))
	} else {
		fmt.Printf("\n%s Graph integrity issues found\n\n", ui.RenderFail("✗"))
	}

	if len(result.Cycles) > 0 {
		fmt.Printf("%s Cycles (%d):\n\n", ui.RenderFail("⚠"), len(result.Cycles))
		for _, cycle := range result.Cycles {
			fmt.Printf("  %s → %s\n", strings.Join(cycle, " → "), cycle[0])
		}
		fmt.Println()
	} else {
		fmt.Printf("  %s No dependency cycles\n", ui.RenderPass("✓"))
	}

	fmt.Println()
}

func init() {
	graphCmd := newGraphCommand()
	graphCmd.Flags().Bool("all", false, "Show graph for all open issues")
	graphCmd.Flags().Bool("compact", false, "Tree format, one line per issue, more scannable")
	graphCmd.Flags().Bool("box", false, "ASCII boxes showing layers")
	graphCmd.Flags().Bool("dot", false, "Output Graphviz DOT format (pipe to: dot -Tsvg > graph.svg)")
	graphCmd.Flags().Bool("html", false, "Output self-contained interactive HTML (redirect to file)")
	graphCmd.Flags().Bool("open", false, "Show only open issues (filters out closed/deferred), forces compact layer format")
	// Defensive row cap (be-x42v): exits 2 on overage, default disabled.
	addMaxRowsFlag(graphCmd)
	graphCmd.ValidArgsFunction = issueIDCompletion
	rootCmd.AddCommand(graphCmd)
	graphCmd.AddCommand(graphCheckCmd)
}

// loadGraphSubgraph loads an issue and its subgraph for visualization
// Unlike template loading, this includes ALL dependency types (not just parent-child)
