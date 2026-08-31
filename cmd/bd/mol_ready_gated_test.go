//go:build cgo

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/types"
)

// setupGatedTestDB creates a temporary file-based test database
func setupGatedTestDB(t *testing.T) (*dolt.DoltStore, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "bd-test-gated-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	testDB := filepath.Join(tmpDir, "test.db")
	store, err := dolt.New(context.Background(), &dolt.Config{Path: testDB})
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Skipf("skipping: Dolt server not available: %v", err)
	}

	// Set issue_prefix (required for beads)
	ctx := context.Background()
	if err := store.SetConfig(ctx, "issue_prefix", "test"); err != nil {
		store.Close()
		os.RemoveAll(tmpDir)
		t.Fatalf("Failed to set issue_prefix: %v", err)
	}

	// Configure orchestrator custom types for test compatibility (bd-find4)
	if err := store.SetConfig(ctx, "types.custom", "molecule,gate,convoy,merge-request,slot,agent,role,rig,event,message"); err != nil {
		store.Close()
		os.RemoveAll(tmpDir)
		t.Fatalf("Failed to set types.custom: %v", err)
	}

	cleanup := func() {
		store.Close()
		os.RemoveAll(tmpDir)
	}

	return store, cleanup
}

// =============================================================================
// mol ready --gated Tests (bd-lhalq: Gate-resume discovery)
// =============================================================================

// TestFindGateReadyMolecules_NoGates tests finding gate-ready molecules when no gates exist
func TestFindGateReadyMolecules_NoGates(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupGatedTestDB(t)
	defer cleanup()

	// Create a regular molecule (no gates)
	mol := &types.Issue{
		IssueID: types.IssueID{
			ID: "test-mol-001",
		},
		IssueContent: types.IssueContent{
			Title: "Test Molecule",
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: types.TypeEpic,
			Status:    types.StatusInProgress,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
	}
	step := &types.Issue{
		IssueID: types.IssueID{
			ID: "test-mol-001.step1",
		},
		IssueContent: types.IssueContent{
			Title: "Step 1",
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: types.TypeTask,
			Status:    types.StatusOpen,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
	}

	if err := store.CreateIssue(ctx, mol, "test"); err != nil {
		t.Fatalf("Failed to create molecule: %v", err)
	}
	if err := store.CreateIssue(ctx, step, "test"); err != nil {
		t.Fatalf("Failed to create step: %v", err)
	}

	// Add parent-child relationship
	if err := store.AddDependency(ctx, &types.Dependency{
		IssueID:     step.ID,
		DependsOnID: mol.ID,
		Type:        types.DepParentChild,
	}, "test"); err != nil {
		t.Fatalf("Failed to add dependency: %v", err)
	}

	// Find gate-ready molecules
	molecules, err := findGateReadyMolecules(ctx, store)
	if err != nil {
		t.Fatalf("findGateReadyMolecules failed: %v", err)
	}

	if len(molecules) != 0 {
		t.Errorf("Expected 0 gate-ready molecules, got %d", len(molecules))
	}
}

// TestFindGateReadyMolecules_ClosedGate tests finding molecules with closed gates
func TestFindGateReadyMolecules_ClosedGate(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupGatedTestDB(t)
	defer cleanup()

	// Create molecule structure:
	// mol-001
	//   └── gate-await-ci (closed)
	//   └── step1 (blocked by gate-await-ci, should become ready)

	mol := &types.Issue{
		IssueID: types.IssueID{
			ID: "test-mol-002",
		},
		IssueContent: types.IssueContent{
			Title: "Test Molecule with Gate",
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: types.TypeEpic,
			Status:    types.StatusInProgress,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
	}
	gate := &types.Issue{
		IssueID: types.IssueID{
			ID: "test-mol-002.gate-await-ci",
		},
		IssueContent: types.IssueContent{
			Title: "Gate: gh:run ci-workflow",
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: "gate",
			Status:    types.StatusClosed,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
		IssueCoord: types.IssueCoord{
			// Gate has closed
			AwaitType: "gh:run",
			AwaitID:   "ci-workflow",
		},
	}
	step := &types.Issue{
		IssueID: types.IssueID{
			ID: "test-mol-002.step1",
		},
		IssueContent: types.IssueContent{
			Title: "Deploy after CI",
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: types.TypeTask,
			Status:    types.StatusOpen,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
	}

	if err := store.CreateIssue(ctx, mol, "test"); err != nil {
		t.Fatalf("Failed to create molecule: %v", err)
	}
	if err := store.CreateIssue(ctx, gate, "test"); err != nil {
		t.Fatalf("Failed to create gate: %v", err)
	}
	if err := store.CreateIssue(ctx, step, "test"); err != nil {
		t.Fatalf("Failed to create step: %v", err)
	}

	// Add parent-child relationships
	if err := store.AddDependency(ctx, &types.Dependency{
		IssueID:     gate.ID,
		DependsOnID: mol.ID,
		Type:        types.DepParentChild,
	}, "test"); err != nil {
		t.Fatalf("Failed to add gate parent-child: %v", err)
	}
	if err := store.AddDependency(ctx, &types.Dependency{
		IssueID:     step.ID,
		DependsOnID: mol.ID,
		Type:        types.DepParentChild,
	}, "test"); err != nil {
		t.Fatalf("Failed to add step parent-child: %v", err)
	}

	// Add blocking dependency: step depends on gate (gate blocks step)
	if err := store.AddDependency(ctx, &types.Dependency{
		IssueID:     step.ID,
		DependsOnID: gate.ID,
		Type:        types.DepBlocks,
	}, "test"); err != nil {
		t.Fatalf("Failed to add blocking dependency: %v", err)
	}

	// Find gate-ready molecules
	molecules, err := findGateReadyMolecules(ctx, store)
	if err != nil {
		t.Fatalf("findGateReadyMolecules failed: %v", err)
	}

	if len(molecules) != 1 {
		t.Errorf("Expected 1 gate-ready molecule, got %d", len(molecules))
		return
	}

	if molecules[0].MoleculeID != mol.ID {
		t.Errorf("Expected molecule ID %s, got %s", mol.ID, molecules[0].MoleculeID)
	}
	if molecules[0].ClosedGate == nil {
		t.Error("Expected closed gate to be set")
	} else if molecules[0].ClosedGate.ID != gate.ID {
		t.Errorf("Expected closed gate ID %s, got %s", gate.ID, molecules[0].ClosedGate.ID)
	}
	if molecules[0].ReadyStep == nil {
		t.Error("Expected ready step to be set")
	} else if molecules[0].ReadyStep.ID != step.ID {
		t.Errorf("Expected ready step ID %s, got %s", step.ID, molecules[0].ReadyStep.ID)
	}
}

// TestFindGateReadyMolecules_OpenGate tests that open gates don't trigger ready
func TestFindGateReadyMolecules_OpenGate(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupGatedTestDB(t)
	defer cleanup()

	// Create molecule with OPEN gate
	mol := &types.Issue{
		IssueID: types.IssueID{
			ID: "test-mol-003",
		},
		IssueContent: types.IssueContent{
			Title: "Test Molecule with Open Gate",
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: types.TypeEpic,
			Status:    types.StatusInProgress,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
	}
	gate := &types.Issue{
		IssueID: types.IssueID{
			ID: "test-mol-003.gate-await-ci",
		},
		IssueContent: types.IssueContent{
			Title: "Gate: gh:run ci-workflow",
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: "gate",
			Status:    types.StatusOpen,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
		IssueCoord: types.IssueCoord{
			// Gate is still open
			AwaitType: "gh:run",
			AwaitID:   "ci-workflow",
		},
	}
	step := &types.Issue{
		IssueID: types.IssueID{
			ID: "test-mol-003.step1",
		},
		IssueContent: types.IssueContent{
			Title: "Deploy after CI",
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: types.TypeTask,
			Status:    types.StatusOpen,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
	}

	if err := store.CreateIssue(ctx, mol, "test"); err != nil {
		t.Fatalf("Failed to create molecule: %v", err)
	}
	if err := store.CreateIssue(ctx, gate, "test"); err != nil {
		t.Fatalf("Failed to create gate: %v", err)
	}
	if err := store.CreateIssue(ctx, step, "test"); err != nil {
		t.Fatalf("Failed to create step: %v", err)
	}

	// Add parent-child relationships
	if err := store.AddDependency(ctx, &types.Dependency{
		IssueID:     gate.ID,
		DependsOnID: mol.ID,
		Type:        types.DepParentChild,
	}, "test"); err != nil {
		t.Fatalf("Failed to add gate parent-child: %v", err)
	}
	if err := store.AddDependency(ctx, &types.Dependency{
		IssueID:     step.ID,
		DependsOnID: mol.ID,
		Type:        types.DepParentChild,
	}, "test"); err != nil {
		t.Fatalf("Failed to add step parent-child: %v", err)
	}

	// Add blocking dependency: step depends on gate
	if err := store.AddDependency(ctx, &types.Dependency{
		IssueID:     step.ID,
		DependsOnID: gate.ID,
		Type:        types.DepBlocks,
	}, "test"); err != nil {
		t.Fatalf("Failed to add blocking dependency: %v", err)
	}

	// Find gate-ready molecules
	molecules, err := findGateReadyMolecules(ctx, store)
	if err != nil {
		t.Fatalf("findGateReadyMolecules failed: %v", err)
	}

	if len(molecules) != 0 {
		t.Errorf("Expected 0 gate-ready molecules (gate is open), got %d", len(molecules))
	}
}

// TestFindGateReadyMolecules_HookedMolecule tests that hooked molecules are filtered out
func TestFindGateReadyMolecules_HookedMolecule(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupGatedTestDB(t)
	defer cleanup()

	// Create molecule with closed gate, but molecule is hooked
	mol := &types.Issue{
		IssueID: types.IssueID{
			ID: "test-mol-004",
		},
		IssueContent: types.IssueContent{
			Title: "Test Hooked Molecule",
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: types.TypeEpic,
			Status:    types.StatusHooked,
		},
		IssueTimes: types.IssueTimes{
			// Already hooked by an agent
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
	}
	gate := &types.Issue{
		IssueID: types.IssueID{
			ID: "test-mol-004.gate-await-ci",
		},
		IssueContent: types.IssueContent{
			Title: "Gate: gh:run ci-workflow",
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: "gate",
			Status:    types.StatusClosed,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
		IssueCoord: types.IssueCoord{
			AwaitType: "gh:run",
			AwaitID:   "ci-workflow",
		},
	}
	step := &types.Issue{
		IssueID: types.IssueID{
			ID: "test-mol-004.step1",
		},
		IssueContent: types.IssueContent{
			Title: "Deploy after CI",
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: types.TypeTask,
			Status:    types.StatusOpen,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
	}

	if err := store.CreateIssue(ctx, mol, "test"); err != nil {
		t.Fatalf("Failed to create molecule: %v", err)
	}
	if err := store.CreateIssue(ctx, gate, "test"); err != nil {
		t.Fatalf("Failed to create gate: %v", err)
	}
	if err := store.CreateIssue(ctx, step, "test"); err != nil {
		t.Fatalf("Failed to create step: %v", err)
	}

	// Add parent-child relationships
	if err := store.AddDependency(ctx, &types.Dependency{
		IssueID:     gate.ID,
		DependsOnID: mol.ID,
		Type:        types.DepParentChild,
	}, "test"); err != nil {
		t.Fatalf("Failed to add gate parent-child: %v", err)
	}
	if err := store.AddDependency(ctx, &types.Dependency{
		IssueID:     step.ID,
		DependsOnID: mol.ID,
		Type:        types.DepParentChild,
	}, "test"); err != nil {
		t.Fatalf("Failed to add step parent-child: %v", err)
	}

	// Add blocking dependency
	if err := store.AddDependency(ctx, &types.Dependency{
		IssueID:     step.ID,
		DependsOnID: gate.ID,
		Type:        types.DepBlocks,
	}, "test"); err != nil {
		t.Fatalf("Failed to add blocking dependency: %v", err)
	}

	// Find gate-ready molecules
	molecules, err := findGateReadyMolecules(ctx, store)
	if err != nil {
		t.Fatalf("findGateReadyMolecules failed: %v", err)
	}

	if len(molecules) != 0 {
		t.Errorf("Expected 0 gate-ready molecules (molecule is hooked), got %d", len(molecules))
	}
}

// TestFindGateReadyMolecules_MultipleGates tests handling multiple closed gates
func TestFindGateReadyMolecules_MultipleGates(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupGatedTestDB(t)
	defer cleanup()

	// Create two molecules, each with a closed gate
	for i := 1; i <= 2; i++ {
		molID := fmt.Sprintf("test-multi-%d", i)
		mol := &types.Issue{
			IssueID: types.IssueID{
				ID: molID,
			},
			IssueContent: types.IssueContent{
				Title: fmt.Sprintf("Multi Gate Mol %d", i),
			},
			IssueWorkflow: types.IssueWorkflow{
				IssueType: types.TypeEpic,
				Status:    types.StatusInProgress,
			},
			IssueTimes: types.IssueTimes{
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			},
		}
		gate := &types.Issue{
			IssueID: types.IssueID{
				ID: fmt.Sprintf("%s.gate", molID),
			},
			IssueContent: types.IssueContent{
				Title: "Gate: gh:run",
			},
			IssueWorkflow: types.IssueWorkflow{
				IssueType: "gate",
				Status:    types.StatusClosed,
			},
			IssueTimes: types.IssueTimes{
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			},
			IssueCoord: types.IssueCoord{
				AwaitType: "gh:run",
			},
		}
		step := &types.Issue{
			IssueID: types.IssueID{
				ID: fmt.Sprintf("%s.step1", molID),
			},
			IssueContent: types.IssueContent{
				Title: "Step 1",
			},
			IssueWorkflow: types.IssueWorkflow{
				IssueType: types.TypeTask,
				Status:    types.StatusOpen,
			},
			IssueTimes: types.IssueTimes{
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			},
		}

		if err := store.CreateIssue(ctx, mol, "test"); err != nil {
			t.Fatalf("Failed to create molecule %d: %v", i, err)
		}
		if err := store.CreateIssue(ctx, gate, "test"); err != nil {
			t.Fatalf("Failed to create gate %d: %v", i, err)
		}
		if err := store.CreateIssue(ctx, step, "test"); err != nil {
			t.Fatalf("Failed to create step %d: %v", i, err)
		}

		// Add dependencies
		if err := store.AddDependency(ctx, &types.Dependency{
			IssueID:     gate.ID,
			DependsOnID: mol.ID,
			Type:        types.DepParentChild,
		}, "test"); err != nil {
			t.Fatalf("Failed to add gate parent-child %d: %v", i, err)
		}
		if err := store.AddDependency(ctx, &types.Dependency{
			IssueID:     step.ID,
			DependsOnID: mol.ID,
			Type:        types.DepParentChild,
		}, "test"); err != nil {
			t.Fatalf("Failed to add step parent-child %d: %v", i, err)
		}
		if err := store.AddDependency(ctx, &types.Dependency{
			IssueID:     step.ID,
			DependsOnID: gate.ID,
			Type:        types.DepBlocks,
		}, "test"); err != nil {
			t.Fatalf("Failed to add blocking dep %d: %v", i, err)
		}
	}

	// Find gate-ready molecules
	molecules, err := findGateReadyMolecules(ctx, store)
	if err != nil {
		t.Fatalf("findGateReadyMolecules failed: %v", err)
	}

	if len(molecules) != 2 {
		t.Errorf("Expected 2 gate-ready molecules, got %d", len(molecules))
	}
}
