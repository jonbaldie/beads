package main

import (
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
)

// filterIssueGates selects the gate-type issues from an issue's dependency set,
// honoring the same open/closed and limit semantics as the DB-wide list path.
// Pulled out as a pure helper so the bead-scoping logic is unit-testable without
// a live store.
func filterIssueGates(deps []*types.Issue, all bool, limit int) []*types.Issue {
	var gates []*types.Issue
	for _, d := range deps {
		if d == nil || d.IssueType != types.IssueType("gate") {
			continue
		}
		if !all && d.Status == types.StatusClosed {
			continue
		}
		gates = append(gates, d)
		if limit > 0 && len(gates) >= limit {
			break
		}
	}
	return gates
}

// displayGates formats and displays gate issues, separating open and closed gates
func displayGates(gates []*types.Issue, showAll bool) {
	if len(gates) == 0 {
		fmt.Println("No gates found.")
		return
	}

	openGates, closedGates := partitionGates(gates)
	displayGateGroup("Open Gates", openGates, ui.RenderAccent("⏳"))
	if showAll {
		displayGateGroup("Closed Gates", closedGates, ui.RenderMuted("●"))
	}
	if noGatesDisplayed(openGates, closedGates, showAll) {
		fmt.Println("No gates found.")
		return
	}
	fmt.Printf("To resolve a gate: bd close <gate-id>\n")
}

func partitionGates(gates []*types.Issue) (open, closed []*types.Issue) {
	for _, gate := range gates {
		if gate.Status == types.StatusClosed {
			closed = append(closed, gate)
			continue
		}
		open = append(open, gate)
	}
	return open, closed
}

func displayGateGroup(name string, gates []*types.Issue, symbol string) {
	if len(gates) == 0 {
		return
	}
	fmt.Printf("\n%s %s (%d):\n\n", symbol, name, len(gates))
	for _, gate := range gates {
		displaySingleGate(gate)
	}
}

func noGatesDisplayed(open, closed []*types.Issue, showAll bool) bool {
	return len(open) == 0 && (!showAll || len(closed) == 0)
}

// displaySingleGate formats and displays a single gate issue
func displaySingleGate(gate *types.Issue) {
	statusSym := "○"
	if gate.Status == types.StatusClosed {
		statusSym = "●"
	}

	// Format gate info
	gateInfo := gate.AwaitType
	if gate.AwaitID != "" {
		gateInfo = fmt.Sprintf("%s %s", gate.AwaitType, gate.AwaitID)
	}

	// Format timeout if present
	timeoutStr := ""
	if gate.Timeout > 0 {
		timeoutStr = fmt.Sprintf(" (timeout: %s)", gate.Timeout)
	}

	// Find blocked step from ID (gate ID format: parent.gate-stepid)
	blockedStep := ""
	if strings.Contains(gate.ID, ".gate-") {
		parts := strings.Split(gate.ID, ".gate-")
		if len(parts) == 2 {
			blockedStep = fmt.Sprintf("%s.%s", parts[0], parts[1])
		}
	}

	fmt.Printf("%s %s - %s%s\n", statusSym, ui.RenderID(gate.ID), gateInfo, timeoutStr)
	if blockedStep != "" {
		fmt.Printf("  Blocks: %s\n", blockedStep)
	}
	fmt.Println()
}
