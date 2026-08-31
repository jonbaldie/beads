package main

import (
	"fmt"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

// LargeMoleculeThreshold is the step count above which we show summary instead of full list.
// This prevents overwhelming output and slow queries for mega-molecules.
const LargeMoleculeThreshold = 100

// MoleculeProgress holds the progress information for a molecule
type MoleculeProgress struct {
	MoleculeID    string        `json:"molecule_id"`
	MoleculeTitle string        `json:"molecule_title"`
	Assignee      string        `json:"assignee,omitempty"`
	CurrentStep   *types.Issue  `json:"current_step,omitempty"`
	NextStep      *types.Issue  `json:"next_step,omitempty"`
	Steps         []*StepStatus `json:"steps"`
	Completed     int           `json:"completed"`
	Total         int           `json:"total"`
}

// StepStatus represents the status of a step in a molecule
type StepStatus struct {
	Issue     *types.Issue `json:"issue"`
	Status    string       `json:"status"`     // "done", "current", "ready", "blocked", "pending"
	IsCurrent bool         `json:"is_current"` // true if this is the in_progress step
}

var molCurrentCmd = &cobra.Command{
	Use:   "current [molecule-id]",
	Short: "Show current position in molecule workflow",
	Long: `Show where you are in a molecule workflow.

If molecule-id is given, show status for that molecule.
If not given, infer from in_progress issues assigned to current agent.

The output shows all steps with status indicators:
  [done]     - Step is complete (closed)
  [current]  - Step is in_progress (you are here)
  [ready]    - Step is ready to start (unblocked)
  [blocked]  - Step is blocked by dependencies
  [pending]  - Step is waiting

For large molecules (>100 steps), a summary is shown instead.
Use --limit or --range to view specific steps:
  bd mol current <id> --limit 50       # Show first 50 steps
  bd mol current <id> --range 100-150  # Show steps 100-150`,
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("mol-current")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		forAgent, _ := cmd.Flags().GetString("for")
		limit, _ := cmd.Flags().GetInt("limit")
		rangeStr, _ := cmd.Flags().GetString("range")

		agent := forAgent
		if agent == "" {
			agent = getActor()
		}

		if usesProxiedServer() {
			return runMolCurrentProxiedServer(getRootContext(), args, agent, limit, rangeStr)
		}

		ctx := getRootContext()

		if getStore() == nil {
			return HandleErrorRespectJSON("no database connection")
		}

		var rangeStart, rangeEnd int
		if rangeStr != "" {
			var err error
			rangeStart, rangeEnd, err = parseRange(rangeStr)
			if err != nil {
				return HandleErrorRespectJSON("invalid range '%s': %v", rangeStr, err)
			}
		}

		explicitSteps := limit > 0 || rangeStr != ""

		var molecules []*MoleculeProgress

		if len(args) == 1 {
			moleculeID, err := utils.ResolvePartialID(ctx, getStore(), args[0])
			if err != nil {
				return HandleErrorRespectJSON("molecule '%s' not found", args[0])
			}

			stats, err := getStore().GetMoleculeProgress(ctx, moleculeID)
			if err != nil {
				return HandleErrorRespectJSON("loading molecule: %v", err)
			}

			if stats.Total > LargeMoleculeThreshold && !explicitSteps && !isJSONOutput() {
				printLargeMoleculeSummary(stats)
				return nil
			}

			progress, err := getMoleculeProgress(ctx, getStore(), moleculeID)
			if err != nil {
				return HandleErrorRespectJSON("loading molecule: %v", err)
			}

			if rangeStr != "" {
				progress.Steps = filterStepsByRange(progress.Steps, rangeStart, rangeEnd)
			} else if limit > 0 && len(progress.Steps) > limit {
				progress.Steps = progress.Steps[:limit]
			}

			molecules = append(molecules, progress)
		} else {
			molecules = findInProgressMolecules(ctx, getStore(), agent)

			if len(molecules) == 0 {
				molecules = findHookedMolecules(ctx, getStore(), agent)
			}

			if len(molecules) == 0 {
				if isJSONOutput() {
					return outputJSON([]interface{}{})
				}
				fmt.Printf("No molecules in progress")
				if agent != "" {
					fmt.Printf(" for %s", agent)
				}
				fmt.Println(".")
				fmt.Println("\nTo start work on a molecule:")
				fmt.Println("  bd mol wisp create <proto-id>  # Instantiate as ephemeral wisp")
				fmt.Println("  bd update <step-id> --claim  # Claim a step")
				return nil
			}
		}

		if isJSONOutput() {
			return outputJSON(molecules)
		}

		for i, mol := range molecules {
			if i > 0 {
				fmt.Println()
			}
			printMoleculeProgress(mol)
		}
		return nil
	},
}
