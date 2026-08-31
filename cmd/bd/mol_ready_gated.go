package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

// GatedMolecule represents a molecule ready for gate-resume dispatch
type GatedMolecule struct {
	MoleculeID    string       `json:"molecule_id"`
	MoleculeTitle string       `json:"molecule_title"`
	ClosedGate    *types.Issue `json:"closed_gate"`
	ReadyStep     *types.Issue `json:"ready_step"`
}

// GatedReadyOutput is the JSON output for bd mol ready --gated
type GatedReadyOutput struct {
	Molecules []*GatedMolecule `json:"molecules"`
	Count     int              `json:"count"`
}

var molReadyGatedCmd = &cobra.Command{
	Use:   "ready --gated",
	Short: "Find molecules ready for gate-resume dispatch",
	Long: `Find molecules where a gate has closed and the workflow is ready to resume.

This command discovers molecules waiting at a gate step where:
1. The molecule has a gate bead that blocks a step
2. The gate bead is now closed (condition satisfied)
3. The blocked step is now ready to proceed
4. No agent currently has this molecule hooked

This enables discovery-based resume without explicit waiter tracking.
The patrol system uses this to find and dispatch gate-ready molecules.

Examples:
  bd mol ready --gated           # Find all gate-ready molecules
  bd mol ready --gated --json    # JSON output for automation`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runMolReadyGated,
}

func runMolReadyGated(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("mol-ready-gated")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	return runMolReadyGatedCore(cmd, args)
}

// runMolReadyGatedCore runs the gate-ready molecule discovery and rendering
// without emitting a metrics event, so the caller owns emission. `bd ready
// --gated` delegates here after recording its own "ready" event, while the
// standalone runMolReadyGated entrypoint records "mol-ready-gated"; this keeps a
// single `bd ready --gated` invocation to exactly one cli_command event.
func runMolReadyGatedCore(_ *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return runMolReadyGatedProxiedServer(getRootContext())
	}

	ctx := getRootContext()

	if getStore() == nil {
		return HandleErrorRespectJSON("no database connection")
	}

	molecules, err := findGateReadyMolecules(ctx, getStore())
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	return renderGatedReadyMolecules(molecules)
}

func renderGatedReadyMolecules(molecules []*GatedMolecule) error {
	if isJSONOutput() {
		output := GatedReadyOutput{
			Molecules: molecules,
			Count:     len(molecules),
		}
		if output.Molecules == nil {
			output.Molecules = []*GatedMolecule{}
		}
		return outputJSON(output)
	}

	if len(molecules) == 0 {
		fmt.Printf("\n%s No molecules ready for gate-resume dispatch\n\n", ui.RenderWarn(""))
		return nil
	}

	fmt.Printf("\n%s Molecules ready for gate-resume dispatch (%d):\n\n",
		ui.RenderAccent(""), len(molecules))

	for i, mol := range molecules {
		fmt.Printf("%d. %s: %s\n", i+1, ui.RenderID(mol.MoleculeID), mol.MoleculeTitle)
		if mol.ClosedGate != nil {
			fmt.Printf("   Gate closed: %s (%s)\n", mol.ClosedGate.ID, mol.ClosedGate.AwaitType)
		}
		if mol.ReadyStep != nil {
			fmt.Printf("   Ready step: %s - %s\n", mol.ReadyStep.ID, mol.ReadyStep.Title)
		}
		fmt.Println()
	}

	fmt.Println("To dispatch a molecule:")
	fmt.Println("  bd sling <agent> --mol <molecule-id>")
	return nil
}

// findGateReadyMolecules finds molecules where a gate has closed and work can resume.
//
// Logic:
// 1. Find all closed gate beads
// 2. For each closed gate, find what step it was blocking
// 3. Check if that step is now ready (unblocked)
// 4. Find the parent molecule
// 5. Filter out molecules that are already hooked by someone
func findGateReadyMolecules(ctx context.Context, s molReader) ([]*GatedMolecule, error) {
	closedGates, err := searchClosedGates(ctx, s)
	if err != nil || len(closedGates) == 0 {
		return nil, err
	}
	readyIDs, err := readyWorkIDs(ctx, s)
	if err != nil {
		return nil, err
	}
	hookedMolecules := hookedMoleculeIDs(ctx, s)
	readyDependents := readyGateDependents(ctx, s, closedGates, readyIDs)
	return assembleGatedMolecules(ctx, s, readyDependents, hookedMolecules), nil
}

func searchClosedGates(ctx context.Context, s molReader) ([]*types.Issue, error) {
	gateType := types.IssueType("gate")
	closedStatus := types.StatusClosed
	closedGates, err := s.SearchIssues(ctx, "", types.IssueFilter{
		IssueFilterCore: types.IssueFilterCore{
			IssueType: &gateType,
			Status:    &closedStatus,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("searching closed gates: %w", err)
	}
	return closedGates, nil
}

func readyWorkIDs(ctx context.Context, s molReader) (map[string]bool, error) {
	readyIssues, err := s.GetReadyWork(ctx, types.WorkFilter{})
	if err != nil {
		return nil, fmt.Errorf("getting ready work: %w", err)
	}
	readyIDs := make(map[string]bool, len(readyIssues))
	for _, issue := range readyIssues {
		readyIDs[issue.ID] = true
	}
	return readyIDs, nil
}

func hookedMoleculeIDs(ctx context.Context, s molReader) map[string]bool {
	hookedStatus := types.StatusHooked
	hookedIssues, err := s.SearchIssues(ctx, "", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Status: &hookedStatus}})
	if err != nil {
		return map[string]bool{}
	}
	hookedMolecules := make(map[string]bool)
	if len(hookedIssues) == 0 {
		return hookedMolecules
	}
	hookedIDs := make([]string, len(hookedIssues))
	for i, issue := range hookedIssues {
		hookedIDs[i] = issue.ID
		hookedMolecules[issue.ID] = true // Mark hooked issue itself
	}
	for _, molID := range findParentMolecules(ctx, s, hookedIDs) {
		hookedMolecules[molID] = true
	}
	return hookedMolecules
}

type gateDependent struct {
	gate      *types.Issue
	dependent *types.Issue
}

func readyGateDependents(ctx context.Context, s molReader, closedGates []*types.Issue, readyIDs map[string]bool) []gateDependent {
	var readyDependents []gateDependent
	for _, gate := range closedGates {
		dependents, err := s.GetDependents(ctx, gate.ID)
		if err != nil {
			continue
		}
		for _, dependent := range dependents {
			if readyIDs[dependent.ID] {
				readyDependents = append(readyDependents, gateDependent{gate: gate, dependent: dependent})
			}
		}
	}
	return readyDependents
}

func assembleGatedMolecules(ctx context.Context, s molReader, readyDependents []gateDependent, hookedMolecules map[string]bool) []*GatedMolecule {
	readyDepIDs := make([]string, 0, len(readyDependents))
	for _, gd := range readyDependents {
		readyDepIDs = append(readyDepIDs, gd.dependent.ID)
	}
	depMolRoots := findParentMolecules(ctx, s, readyDepIDs)
	moleculeMap := make(map[string]*GatedMolecule)
	for _, gd := range readyDependents {
		addGatedMolecule(ctx, s, gd, depMolRoots, hookedMolecules, moleculeMap)
	}
	var molecules []*GatedMolecule
	for _, mol := range moleculeMap {
		molecules = append(molecules, mol)
	}
	sort.Slice(molecules, func(i, j int) bool {
		return molecules[i].MoleculeID < molecules[j].MoleculeID
	})
	return molecules
}

func addGatedMolecule(ctx context.Context, s molReader, gd gateDependent, depMolRoots map[string]string, hookedMolecules map[string]bool, moleculeMap map[string]*GatedMolecule) {
	moleculeID := depMolRoots[gd.dependent.ID]
	if moleculeID == "" || hookedMolecules[moleculeID] {
		return
	}
	if _, exists := moleculeMap[moleculeID]; exists {
		return
	}
	moleculeIssue, err := s.GetIssue(ctx, moleculeID)
	if err != nil || moleculeIssue == nil {
		return
	}
	moleculeMap[moleculeID] = &GatedMolecule{
		MoleculeID:    moleculeID,
		MoleculeTitle: moleculeIssue.Title,
		ClosedGate:    gd.gate,
		ReadyStep:     gd.dependent,
	}
}

func init() {
	// `bd ready --gated` registers --gated on readyCmd in ready.go.
	// `bd mol ready` is a separate subcommand under molCmd that always runs
	// in gated mode, so accept --gated here too: both spellings work and the
	// documented `bd mol ready --gated` form actually matches the help text.
	molReadyGatedCmd.Flags().Bool("gated", false, "Find molecules ready for gate-resume dispatch (always on for this subcommand)")
	molCmd.AddCommand(molReadyGatedCmd)
}
