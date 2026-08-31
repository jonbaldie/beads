package dolt

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// =============================================================================
// GetReadyWork tests
// =============================================================================

func TestGetReadyWork_EmptyStore(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	work, err := store.GetReadyWork(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(work) != 0 {
		t.Errorf("expected 0 ready work from empty store, got %d", len(work))
	}
}

func TestRigIssueIsPersistentButHiddenFromReady(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	rig := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-rig-durable",
		},
		IssueContent: types.IssueContent{
			Title: "Rig identity",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.IssueType("rig"),
		},
	}
	if err := store.CreateIssue(ctx, rig, "tester"); err != nil {
		t.Fatalf("CreateIssue rig: %v", err)
	}
	if rig.Ephemeral {
		t.Fatal("CreateIssue marked type=rig as ephemeral")
	}

	got, err := store.GetIssue(ctx, rig.ID)
	if err != nil {
		t.Fatalf("GetIssue rig: %v", err)
	}
	if got.Ephemeral {
		t.Fatal("stored type=rig issue is ephemeral")
	}

	var issueRows int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM issues WHERE id = ?", rig.ID).Scan(&issueRows); err != nil {
		t.Fatalf("count rig issue rows: %v", err)
	}
	if issueRows != 1 {
		t.Fatalf("type=rig rows in issues = %d, want 1", issueRows)
	}

	var wispRows int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM wisps WHERE id = ?", rig.ID).Scan(&wispRows); err != nil {
		t.Fatalf("count rig wisp rows: %v", err)
	}
	if wispRows != 0 {
		t.Fatalf("type=rig rows in wisps = %d, want 0", wispRows)
	}

	work, err := store.GetReadyWork(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatalf("GetReadyWork: %v", err)
	}
	for _, item := range work {
		if item.ID == rig.ID {
			t.Fatalf("type=rig issue appeared in ready work: %v", issueIDs(work))
		}
	}
}

func TestGetReadyWork_ExcludesClosedIssues(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	issue := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-closed",
		},
		IssueContent: types.IssueContent{
			Title: "Closed Issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, issue, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}
	if err := store.CloseIssue(ctx, issue.ID, "done", "tester", "s1"); err != nil {
		t.Fatalf("failed to close issue: %v", err)
	}

	work, err := store.GetReadyWork(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, w := range work {
		if w.ID == issue.ID {
			t.Error("closed issue should not appear in ready work")
		}
	}
}

func TestGetReadyWork_StatusFilter(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	open := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-open",
		},
		IssueContent: types.IssueContent{
			Title: "Open Issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	inProgress := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-inprog",
		},
		IssueContent: types.IssueContent{
			Title: "In Progress",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusInProgress,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}

	for _, iss := range []*types.Issue{open, inProgress} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue %s: %v", iss.ID, err)
		}
	}

	// Filter by in_progress only
	work, err := store.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Status: types.StatusInProgress}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	foundOpen := false
	foundInProgress := false
	for _, w := range work {
		if w.ID == open.ID {
			foundOpen = true
		}
		if w.ID == inProgress.ID {
			foundInProgress = true
		}
	}
	if foundOpen {
		t.Error("open issue should not appear when filtering for in_progress")
	}
	if !foundInProgress {
		t.Error("in_progress issue should appear when filtering for in_progress")
	}
}

func TestGetReadyWork_PriorityFilter(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	p1 := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-p1",
		},
		IssueContent: types.IssueContent{
			Title: "Priority 1",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
	}
	p3 := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-p3",
		},
		IssueContent: types.IssueContent{
			Title: "Priority 3",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  3,
			IssueType: types.TypeTask,
		},
	}

	for _, iss := range []*types.Issue{p1, p3} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue %s: %v", iss.ID, err)
		}
	}

	priority := 1
	work, err := store.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Priority: &priority}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, w := range work {
		if w.ID == p3.ID {
			t.Error("priority 3 issue should not appear when filtering for priority 1")
		}
	}
}

func TestGetReadyWork_ExcludesPinnedIssues(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	pinned := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-pinned",
		},
		IssueContent: types.IssueContent{
			Title: "Pinned Context",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
		IssueWisp: types.IssueWisp{
			Pinned: true,
		},
	}
	if err := store.CreateIssue(ctx, pinned, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}

	work, err := store.GetReadyWork(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, w := range work {
		if w.ID == pinned.ID {
			t.Error("pinned issue should not appear in ready work")
		}
	}
}

func TestGetReadyWork_ExcludesBlockedIssues(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	blocker := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-blocker",
		},
		IssueContent: types.IssueContent{
			Title: "Blocker",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
	}
	blocked := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-blocked",
		},
		IssueContent: types.IssueContent{
			Title: "Blocked",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}

	for _, iss := range []*types.Issue{blocker, blocked} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}

	dep := &types.Dependency{
		IssueID:     blocked.ID,
		DependsOnID: blocker.ID,
		Type:        types.DepBlocks,
	}
	if err := store.AddDependency(ctx, dep, "tester"); err != nil {
		t.Fatalf("failed to add dependency: %v", err)
	}

	work, err := store.GetReadyWork(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, w := range work {
		if w.ID == blocked.ID {
			t.Error("blocked issue should not appear in ready work")
		}
	}
}

func TestGetReadyWork_UnassignedFilter(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	assigned := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-assigned",
		},
		IssueContent: types.IssueContent{
			Title: "Assigned Issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
			Assignee:  "alice",
		},
	}
	unassigned := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-unassigned",
		},
		IssueContent: types.IssueContent{
			Title: "Unassigned Issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}

	for _, iss := range []*types.Issue{assigned, unassigned} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}

	work, err := store.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Unassigned: true}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, w := range work {
		if w.ID == assigned.ID {
			t.Error("assigned issue should not appear when filtering for unassigned")
		}
	}
}

func TestGetReadyWork_LimitFilter(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	for i := 0; i < 5; i++ {
		iss := &types.Issue{
			IssueID: types.IssueID{
				ID: fmt.Sprintf("rw-limit-%d", i),
			},
			IssueContent: types.IssueContent{
				Title: fmt.Sprintf("Limit Issue %d", i),
			},
			IssueWorkflow: types.IssueWorkflow{
				Status:    types.StatusOpen,
				Priority:  2,
				IssueType: types.TypeTask,
			},
		}
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}

	work, err := store.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Limit: 2}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(work) > 2 {
		t.Errorf("expected at most 2 results with Limit=2, got %d", len(work))
	}
}

func TestGetReadyWork_LimitSkipsBlockedCandidates(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	blocker := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-page-blocker",
		},
		IssueContent: types.IssueContent{
			Title: "Blocking Gate",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeGate,
		},
	}
	issues := []*types.Issue{
		blocker,
		{
			IssueID: types.IssueID{
				ID: "rw-page-blocked-1",
			},
			IssueContent: types.IssueContent{
				Title: "Blocked 1",
			},
			IssueWorkflow: types.IssueWorkflow{
				Status:    types.StatusOpen,
				Priority:  1,
				IssueType: types.TypeTask,
			},
		},
		{
			IssueID: types.IssueID{
				ID: "rw-page-blocked-2",
			},
			IssueContent: types.IssueContent{
				Title: "Blocked 2",
			},
			IssueWorkflow: types.IssueWorkflow{
				Status:    types.StatusOpen,
				Priority:  1,
				IssueType: types.TypeTask,
			},
		},
		{
			IssueID: types.IssueID{
				ID: "rw-page-ready-1",
			},
			IssueContent: types.IssueContent{
				Title: "Ready 1",
			},
			IssueWorkflow: types.IssueWorkflow{
				Status:    types.StatusOpen,
				Priority:  1,
				IssueType: types.TypeTask,
			},
		},
		{
			IssueID: types.IssueID{
				ID: "rw-page-ready-2",
			},
			IssueContent: types.IssueContent{
				Title: "Ready 2",
			},
			IssueWorkflow: types.IssueWorkflow{
				Status:    types.StatusOpen,
				Priority:  1,
				IssueType: types.TypeTask,
			},
		},
	}
	for _, iss := range issues {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue %s: %v", iss.ID, err)
		}
	}
	for _, blockedID := range []string{"rw-page-blocked-1", "rw-page-blocked-2"} {
		dep := &types.Dependency{
			IssueID:     blockedID,
			DependsOnID: blocker.ID,
			Type:        types.DepBlocks,
		}
		if err := store.AddDependency(ctx, dep, "tester"); err != nil {
			t.Fatalf("failed to add dependency for %s: %v", blockedID, err)
		}
	}

	work, err := store.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Limit: 2, SortPolicy: types.SortPolicyOldest}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ids := issueIDs(work)
	if len(ids) != 2 {
		t.Fatalf("expected 2 ready items after skipping blocked candidates, got %d: %v", len(ids), ids)
	}
	idSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		idSet[id] = struct{}{}
	}
	for _, blockedID := range []string{"rw-page-blocked-1", "rw-page-blocked-2"} {
		if _, ok := idSet[blockedID]; ok {
			t.Fatalf("blocked issue %s appeared in limited ready work: %v", blockedID, ids)
		}
	}
}

func TestGetReadyWork_LimitScansMultipleCandidatePages(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	const blockedCount = 220
	const readyCount = 10
	blocker := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-multi-blocker",
		},
		IssueContent: types.IssueContent{
			Title: "Blocking Gate",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeGate,
		},
	}
	issues := []*types.Issue{blocker}
	for i := 0; i < blockedCount; i++ {
		issues = append(issues, &types.Issue{
			IssueID: types.IssueID{
				ID: fmt.Sprintf("rw-multi-blocked-%03d", i),
			},
			IssueContent: types.IssueContent{
				Title: fmt.Sprintf("Blocked %03d", i),
			},
			IssueWorkflow: types.IssueWorkflow{
				Status:    types.StatusOpen,
				Priority:  1,
				IssueType: types.TypeTask,
			},
		})
	}
	for i := 0; i < readyCount; i++ {
		issues = append(issues, &types.Issue{
			IssueID: types.IssueID{
				ID: fmt.Sprintf("rw-multi-ready-%03d", i),
			},
			IssueContent: types.IssueContent{
				Title: fmt.Sprintf("Ready %03d", i),
			},
			IssueWorkflow: types.IssueWorkflow{
				Status:    types.StatusOpen,
				Priority:  1,
				IssueType: types.TypeTask,
			},
		})
	}

	err := store.RunInTransaction(ctx, "test: seed multi-page ready work", func(tx storage.Transaction) error {
		if err := tx.CreateIssues(ctx, issues, "tester"); err != nil {
			return err
		}
		for i := 0; i < blockedCount; i++ {
			if err := tx.AddDependency(ctx, &types.Dependency{
				IssueID:     fmt.Sprintf("rw-multi-blocked-%03d", i),
				DependsOnID: blocker.ID,
				Type:        types.DepBlocks,
			}, "tester"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed multi-page ready work: %v", err)
	}

	limited, err := store.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Limit: readyCount, SortPolicy: types.SortPolicyOldest}})
	if err != nil {
		t.Fatalf("limited ready work: %v", err)
	}
	limitedIDs := issueIDs(limited)
	if len(limitedIDs) != readyCount {
		t.Fatalf("expected %d limited ready items, got %d: %v", readyCount, len(limitedIDs), limitedIDs)
	}
	for i := 0; i < readyCount; i++ {
		want := fmt.Sprintf("rw-multi-ready-%03d", i)
		if limitedIDs[i] != want {
			t.Fatalf("limited[%d] = %s, want %s (all ids: %v)", i, limitedIDs[i], want, limitedIDs)
		}
	}

	unbounded, err := store.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{SortPolicy: types.SortPolicyOldest}})
	if err != nil {
		t.Fatalf("unbounded ready work: %v", err)
	}
	unboundedIDs := issueIDs(unbounded)
	if len(unboundedIDs) < readyCount {
		t.Fatalf("expected at least %d unbounded ready items, got %d: %v", readyCount, len(unboundedIDs), unboundedIDs)
	}
	for i := 0; i < readyCount; i++ {
		if limitedIDs[i] != unboundedIDs[i] {
			t.Fatalf("limited result diverged from unbounded at %d: limited=%v unbounded=%v", i, limitedIDs, unboundedIDs[:readyCount])
		}
	}
}

func TestGetReadyWork_LimitCandidateGraphSemantics(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	issues := []*types.Issue{
		// This blocker is intentionally a blocked epic instead of the older open
		// gate fixture: current dependency validation rejects gate->epic block
		// edges, and the test only needs a non-ready blocker for the parent chain.
		{IssueID: types.IssueID{ID: "rw-graph-parent-blocker"}, IssueContent: types.IssueContent{Title: "Parent blocker"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusBlocked, Priority: 1, IssueType: types.TypeEpic}},
		{IssueID: types.IssueID{ID: "rw-graph-parent"}, IssueContent: types.IssueContent{Title: "Blocked parent"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeEpic}},
		{IssueID: types.IssueID{ID: "rw-graph-parent-child"}, IssueContent: types.IssueContent{Title: "Child of blocked parent"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}},
		{IssueID: types.IssueID{ID: "rw-graph-all-waiter"}, IssueContent: types.IssueContent{Title: "All waiter"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}},
		{IssueID: types.IssueID{ID: "rw-graph-all-spawner"}, IssueContent: types.IssueContent{Title: "All spawner"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeGate}},
		{IssueID: types.IssueID{ID: "rw-graph-all-child"}, IssueContent: types.IssueContent{Title: "All active child"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeGate}},
		{IssueID: types.IssueID{ID: "rw-graph-any-blocked"}, IssueContent: types.IssueContent{Title: "Any blocked waiter"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}},
		{IssueID: types.IssueID{ID: "rw-graph-any-blocked-spawner"}, IssueContent: types.IssueContent{Title: "Any blocked spawner"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeGate}},
		{IssueID: types.IssueID{ID: "rw-graph-any-blocked-child"}, IssueContent: types.IssueContent{Title: "Any blocked active child"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeGate}},
		{IssueID: types.IssueID{ID: "rw-graph-any-ready"}, IssueContent: types.IssueContent{Title: "Any ready waiter"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}},
		{IssueID: types.IssueID{ID: "rw-graph-any-ready-spawner"}, IssueContent: types.IssueContent{Title: "Any ready spawner"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeGate}},
		{IssueID: types.IssueID{ID: "rw-graph-any-ready-child-closed"}, IssueContent: types.IssueContent{Title: "Any ready closed child"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeGate}},
		{IssueID: types.IssueID{ID: "rw-graph-any-ready-child-active"}, IssueContent: types.IssueContent{Title: "Any ready active child"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeGate}},
		{IssueID: types.IssueID{ID: "rw-graph-ready"}, IssueContent: types.IssueContent{Title: "Ready control"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}},
	}
	err := store.RunInTransaction(ctx, "test: seed candidate graph ready work", func(tx storage.Transaction) error {
		if err := tx.CreateIssues(ctx, issues, "tester"); err != nil {
			return err
		}
		deps := []*types.Dependency{
			{IssueID: "rw-graph-parent", DependsOnID: "rw-graph-parent-blocker", Type: types.DepBlocks},
			{IssueID: "rw-graph-parent-child", DependsOnID: "rw-graph-parent", Type: types.DepParentChild},
			{IssueID: "rw-graph-all-waiter", DependsOnID: "rw-graph-all-spawner", Type: types.DepWaitsFor},
			{IssueID: "rw-graph-all-child", DependsOnID: "rw-graph-all-spawner", Type: types.DepParentChild},
			{IssueID: "rw-graph-any-blocked", DependsOnID: "rw-graph-any-blocked-spawner", Type: types.DepWaitsFor, Metadata: `{"gate":"any-children"}`},
			{IssueID: "rw-graph-any-blocked-child", DependsOnID: "rw-graph-any-blocked-spawner", Type: types.DepParentChild},
			{IssueID: "rw-graph-any-ready", DependsOnID: "rw-graph-any-ready-spawner", Type: types.DepWaitsFor, Metadata: `{"gate":"any-children"}`},
			{IssueID: "rw-graph-any-ready-child-closed", DependsOnID: "rw-graph-any-ready-spawner", Type: types.DepParentChild},
			{IssueID: "rw-graph-any-ready-child-active", DependsOnID: "rw-graph-any-ready-spawner", Type: types.DepParentChild},
		}
		for _, dep := range deps {
			if err := tx.AddDependency(ctx, dep, "tester"); err != nil {
				return err
			}
		}
		return tx.CloseIssue(ctx, "rw-graph-any-ready-child-closed", "done", "tester", "s1")
	})
	if err != nil {
		t.Fatalf("seed candidate graph ready work: %v", err)
	}

	work, err := store.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Limit: 2, SortPolicy: types.SortPolicyOldest}})
	if err != nil {
		t.Fatalf("limited ready work: %v", err)
	}
	ids := issueIDs(work)
	want := []string{"rw-graph-any-ready", "rw-graph-ready"}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("limited candidate graph result = %v, want %v", ids, want)
	}
	for _, blockedID := range []string{"rw-graph-parent-child", "rw-graph-all-waiter", "rw-graph-any-blocked"} {
		if readyIDSet(ids)[blockedID] {
			t.Fatalf("blocked candidate %s appeared in limited ready work: %v", blockedID, ids)
		}
	}
}

func TestGetReadyWork_LimitMatchesUnboundedForChildrenOfInactiveParents(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	type scenario struct {
		name         string
		parentStatus types.Status
		depType      types.DependencyType
	}
	scenarios := []scenario{
		{name: "closed-blocks", parentStatus: types.StatusClosed, depType: types.DepBlocks},
		{name: "closed-conditional-blocks", parentStatus: types.StatusClosed, depType: types.DepConditionalBlocks},
		{name: "closed-waits-for", parentStatus: types.StatusClosed, depType: types.DepWaitsFor},
		{name: "pinned-blocks", parentStatus: types.StatusPinned, depType: types.DepBlocks},
		{name: "pinned-conditional-blocks", parentStatus: types.StatusPinned, depType: types.DepConditionalBlocks},
		{name: "pinned-waits-for", parentStatus: types.StatusPinned, depType: types.DepWaitsFor},
	}

	var issues []*types.Issue
	var deps []*types.Dependency
	var childIDs []string
	for _, sc := range scenarios {
		prefix := "rw-inactive-parent-" + sc.name
		parentID := prefix + "-parent"
		childID := prefix + "-child"
		blockerID := prefix + "-blocker"
		childIDs = append(childIDs, childID)
		issues = append(issues,
			&types.Issue{IssueID: types.IssueID{ID: parentID}, IssueContent: types.IssueContent{Title: sc.name + " parent"}, IssueWorkflow: types.IssueWorkflow{Status: sc.parentStatus, Priority: 1, IssueType: types.TypeTask}, IssueWisp: types.IssueWisp{Pinned: sc.parentStatus == types.StatusPinned}},
			&types.Issue{IssueID: types.IssueID{ID: childID}, IssueContent: types.IssueContent{Title: sc.name + " child"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}},
			&types.Issue{IssueID: types.IssueID{ID: blockerID}, IssueContent: types.IssueContent{Title: sc.name + " blocker"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}},
		)
		deps = append(deps,
			&types.Dependency{IssueID: childID, DependsOnID: parentID, Type: types.DepParentChild},
			&types.Dependency{IssueID: parentID, DependsOnID: blockerID, Type: sc.depType},
		)
		if sc.depType == types.DepWaitsFor {
			spawnedChildID := prefix + "-spawned-child"
			issues = append(issues, &types.Issue{
				IssueID: types.IssueID{
					ID: spawnedChildID,
				},
				IssueContent: types.IssueContent{
					Title: sc.name + " spawned child",
				},
				IssueWorkflow: types.IssueWorkflow{
					Status:    types.StatusOpen,
					Priority:  1,
					IssueType: types.TypeTask,
				},
			})
			deps = append(deps, &types.Dependency{
				IssueID:     spawnedChildID,
				DependsOnID: blockerID,
				Type:        types.DepParentChild,
			})
		}
	}

	err := store.RunInTransaction(ctx, "test: seed inactive parent ready work", func(tx storage.Transaction) error {
		if err := tx.CreateIssues(ctx, issues, "tester"); err != nil {
			return err
		}
		for _, dep := range deps {
			if err := tx.AddDependency(ctx, dep, "tester"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed inactive parent ready work: %v", err)
	}

	limited, err := store.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Limit: 100, SortPolicy: types.SortPolicyOldest}})
	if err != nil {
		t.Fatalf("limited ready work: %v", err)
	}
	unbounded, err := store.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{SortPolicy: types.SortPolicyOldest}})
	if err != nil {
		t.Fatalf("unbounded ready work: %v", err)
	}

	limitedIDs := issueIDs(limited)
	unboundedIDs := issueIDs(unbounded)
	if fmt.Sprint(limitedIDs) != fmt.Sprint(unboundedIDs) {
		t.Fatalf("limited ready work diverged from unbounded:\nlimited:   %v\nunbounded: %v", limitedIDs, unboundedIDs)
	}
	limitedSet := readyIDSet(limitedIDs)
	for _, childID := range childIDs {
		if !limitedSet[childID] {
			t.Fatalf("child of inactive blocked parent %s missing from ready work: %v", childID, limitedIDs)
		}
	}
}

func TestGetReadyWork_LimitIncludeEphemeralWispBlocker(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	wispBlocker := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-eph-wisp-blocker",
		},
		IssueContent: types.IssueContent{
			Title: "Ephemeral blocker",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeGate,
		},
		IssueWisp: types.IssueWisp{
			Ephemeral: true,
		},
	}
	blocked := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-eph-blocked",
		},
		IssueContent: types.IssueContent{
			Title: "Blocked by wisp",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
	}
	ready := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-eph-ready",
		},
		IssueContent: types.IssueContent{
			Title: "Ready",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
	}
	for _, iss := range []*types.Issue{wispBlocker, blocked, ready} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("create issue %s: %v", iss.ID, err)
		}
	}
	if err := store.AddDependency(ctx, &types.Dependency{
		IssueID:     blocked.ID,
		DependsOnID: wispBlocker.ID,
		Type:        types.DepBlocks,
	}, "tester"); err != nil {
		t.Fatalf("add wisp blocker dependency: %v", err)
	}

	work, err := store.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Limit: 1, SortPolicy: types.SortPolicyOldest}, WorkFilterExtra: types.WorkFilterExtra{IncludeEphemeral: true}})
	if err != nil {
		t.Fatalf("limited ready work with wisps: %v", err)
	}
	ids := readyIDSet(issueIDs(work))
	if ids[blocked.ID] {
		t.Fatalf("issue blocked by active wisp appeared in ready work: %v", issueIDs(work))
	}
	if !ids[ready.ID] {
		t.Fatalf("ready issue missing from limited ready work with wisps: %v", issueIDs(work))
	}
}

func TestGetReadyWork_LimitIncludeEphemeralHonorsOldestSortAcrossWispPages(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	now := time.Now().UTC()
	lowPriorityOld := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-eph-oldest-low-priority",
		},
		IssueContent: types.IssueContent{
			Title: "Oldest low priority wisp",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  4,
			IssueType: types.TypeTask,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: now.Add(-72 * time.Hour),
		},
		IssueWisp: types.IssueWisp{
			Ephemeral: true,
		},
	}
	if err := store.CreateIssue(ctx, lowPriorityOld, "tester"); err != nil {
		t.Fatalf("create old wisp: %v", err)
	}

	for i := 0; i < 101; i++ {
		iss := &types.Issue{
			IssueID: types.IssueID{
				ID: fmt.Sprintf("rw-eph-priority-noise-%03d", i),
			},
			IssueContent: types.IssueContent{
				Title: fmt.Sprintf("Priority noise %03d", i),
			},
			IssueWorkflow: types.IssueWorkflow{
				Status:    types.StatusOpen,
				Priority:  1,
				IssueType: types.TypeTask,
			},
			IssueTimes: types.IssueTimes{
				CreatedAt: now.Add(time.Duration(i) * time.Minute),
			},
			IssueWisp: types.IssueWisp{
				Ephemeral: true,
			},
		}
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("create priority noise %03d: %v", i, err)
		}
	}

	work, err := store.GetReadyWork(ctx, types.WorkFilter{
		WorkFilterCore: types.WorkFilterCore{
			Limit:      1,
			SortPolicy: types.SortPolicyOldest,
		},
		WorkFilterExtra: types.WorkFilterExtra{
			IncludeEphemeral: true,
		},
	})
	if err != nil {
		t.Fatalf("limited oldest ready work with wisps: %v", err)
	}
	ids := issueIDs(work)
	want := []string{lowPriorityOld.ID}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("limited oldest ready work = %v, want %v", ids, want)
	}
}

func TestGetReadyWork_IncludeEphemeralPropagatesWispSearchError(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	if err := store.CreateIssue(ctx, &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-wisp-error-ready",
		},
		IssueContent: types.IssueContent{
			Title: "Ready control",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
	}, "tester"); err != nil {
		t.Fatalf("create ready issue: %v", err)
	}
	if err := store.CreateIssue(ctx, &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-wisp-error-wisp",
		},
		IssueContent: types.IssueContent{
			Title: "Wisp control",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
		IssueWisp: types.IssueWisp{
			Ephemeral: true,
		},
	}, "tester"); err != nil {
		t.Fatalf("create wisp: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "ALTER TABLE wisps DROP COLUMN title"); err != nil {
		t.Fatalf("damage wisps table for regression test: %v", err)
	}

	_, err := store.GetReadyWork(ctx, types.WorkFilter{WorkFilterExtra: types.WorkFilterExtra{IncludeEphemeral: true}})
	if err == nil {
		t.Fatal("expected IncludeEphemeral ready work to propagate wisp search error")
	}
	if !strings.Contains(err.Error(), "search wisps (ready work)") {
		t.Fatalf("expected ready-work wisp error context, got %v", err)
	}
}

func TestGetReadyWork_DottedHasMetadataKey(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	matching := &types.Issue{
		IssueID: types.IssueID{
			ID: "test-rw-meta-dotted",
		},
		IssueContent: types.IssueContent{
			Title: "Dotted metadata",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
		IssueMeta: types.IssueMeta{
			Metadata: []byte(`{"gc.routed_to":"beads/workflows.codex-max"}`),
		},
	}
	nonMatching := &types.Issue{
		IssueID: types.IssueID{
			ID: "test-rw-meta-nested",
		},
		IssueContent: types.IssueContent{
			Title: "Nested metadata",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
		IssueMeta: types.IssueMeta{
			Metadata: []byte(`{"gc":{"routed_to":"beads/workflows.codex-max"}}`),
		},
	}
	if err := store.CreateIssues(ctx, []*types.Issue{matching, nonMatching}, "tester"); err != nil {
		t.Fatalf("create metadata issues: %v", err)
	}

	work, err := store.GetReadyWork(ctx, types.WorkFilter{WorkFilterExtra: types.WorkFilterExtra{HasMetadataKey: "gc.routed_to"}})
	if err != nil {
		t.Fatalf("ready work with dotted metadata key: %v", err)
	}
	ids := readyIDSet(issueIDs(work))
	if !ids[matching.ID] {
		t.Fatalf("literal dotted metadata key issue missing from ready work: %v", issueIDs(work))
	}
	if ids[nonMatching.ID] {
		t.Fatalf("nested metadata issue matched literal dotted key: %v", issueIDs(work))
	}
}

func TestGetReadyWork_TypePriorityLimitUsesDoltSafeTypePredicate(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	priority := 1
	issues := []*types.Issue{
		{IssueID: types.IssueID{ID: "test-rw-type-task"}, IssueContent: types.IssueContent{Title: "Task"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: priority, IssueType: types.TypeTask}},
		{IssueID: types.IssueID{ID: "test-rw-type-bug-1"}, IssueContent: types.IssueContent{Title: "Bug 1"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: priority, IssueType: types.TypeBug}},
		{IssueID: types.IssueID{ID: "test-rw-type-bug-2"}, IssueContent: types.IssueContent{Title: "Bug 2"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: priority, IssueType: types.TypeBug}},
	}
	if err := store.CreateIssues(ctx, issues, "tester"); err != nil {
		t.Fatalf("create typed issues: %v", err)
	}

	work, err := store.GetReadyWork(ctx, types.WorkFilter{
		WorkFilterCore: types.WorkFilterCore{
			Type:       string(types.TypeBug),
			Status:     types.StatusOpen,
			Priority:   &priority,
			Limit:      2,
			SortPolicy: types.SortPolicyOldest,
		},
	})
	if err != nil {
		t.Fatalf("ready work with type+status+priority+limit: %v", err)
	}
	ids := issueIDs(work)
	want := []string{"test-rw-type-bug-1", "test-rw-type-bug-2"}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("typed ready work = %v, want %v", ids, want)
	}
}

func readyIDSet(ids []string) map[string]bool {
	result := make(map[string]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result
}

func TestGetReadyWork_TypeFilter(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	task := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-task",
		},
		IssueContent: types.IssueContent{
			Title: "A Task",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	bug := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-bug",
		},
		IssueContent: types.IssueContent{
			Title: "A Bug",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeBug,
		},
	}

	for _, iss := range []*types.Issue{task, bug} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}

	work, err := store.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Type: string(types.TypeBug)}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	foundTask := false
	foundBug := false
	for _, w := range work {
		if w.ID == task.ID {
			foundTask = true
		}
		if w.ID == bug.ID {
			foundBug = true
		}
	}
	if foundTask {
		t.Error("task should not appear when filtering for bug type")
	}
	if !foundBug {
		t.Error("bug should appear when filtering for bug type")
	}
}

// TestGetReadyWork_ExcludeTypeFilter verifies that filter.ExcludeTypes is
// honored in addition to the hardcoded default exclusion list. Regression test
// for GH#3397: the CLI flag --exclude-type was silently ignored because
// GetReadyWorkInTx built the NOT IN clause from the hardcoded defaults only.
func TestGetReadyWork_ExcludeTypeFilter(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	epic := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-ex-epic",
		},
		IssueContent: types.IssueContent{
			Title: "An Epic",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeEpic,
		},
	}
	task := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-ex-task",
		},
		IssueContent: types.IssueContent{
			Title: "A Task",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	bug := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-ex-bug",
		},
		IssueContent: types.IssueContent{
			Title: "A Bug",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeBug,
		},
	}

	for _, iss := range []*types.Issue{epic, task, bug} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}

	// Single-type exclusion.
	work, err := store.GetReadyWork(ctx, types.WorkFilter{
		WorkFilterExtra: types.WorkFilterExtra{
			ExcludeTypes: []types.IssueType{types.TypeEpic},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, w := range work {
		if w.ID == epic.ID {
			t.Error("epic should not appear when ExcludeTypes includes epic")
		}
	}

	// Multi-type exclusion.
	work, err = store.GetReadyWork(ctx, types.WorkFilter{
		WorkFilterExtra: types.WorkFilterExtra{
			ExcludeTypes: []types.IssueType{types.TypeEpic, types.TypeTask},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	foundBug := false
	for _, w := range work {
		if w.ID == epic.ID {
			t.Error("epic should not appear when ExcludeTypes includes epic")
		}
		if w.ID == task.ID {
			t.Error("task should not appear when ExcludeTypes includes task")
		}
		if w.ID == bug.ID {
			foundBug = true
		}
	}
	if !foundBug {
		t.Error("bug should still appear when ExcludeTypes excludes only epic and task")
	}
}

// TestGetReadyWork_ParentFilterReturnsDescendants verifies that --parent
// returns all transitive descendants, not just direct children. Regression
// test for GH#3396: the SQL clause was a one-hop subquery, so grandchildren
// of the given parent were silently dropped despite the help text and the
// WorkFilter.ParentID godoc both promising "descendants (recursive)".
func TestGetReadyWork_ParentFilterReturnsDescendants(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	epic := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-pd-epic",
		},
		IssueContent: types.IssueContent{
			Title: "Epic",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeEpic,
		},
	}
	phase := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-pd-phase",
		},
		IssueContent: types.IssueContent{
			Title: "Phase",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeEpic,
		},
	}
	leaf := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-pd-leaf",
		},
		IssueContent: types.IssueContent{
			Title: "Leaf Task",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}

	for _, iss := range []*types.Issue{epic, phase, leaf} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue %s: %v", iss.ID, err)
		}
	}

	// epic <- phase <- leaf via parent-child deps.
	for _, dep := range []*types.Dependency{
		{IssueID: phase.ID, DependsOnID: epic.ID, Type: types.DepParentChild},
		{IssueID: leaf.ID, DependsOnID: phase.ID, Type: types.DepParentChild},
	} {
		if err := store.AddDependency(ctx, dep, "tester"); err != nil {
			t.Fatalf("failed to add dep %s->%s: %v", dep.IssueID, dep.DependsOnID, err)
		}
	}

	// Direct parent filter still works (control).
	parentPhase := phase.ID
	workPhase, err := store.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{ParentID: &parentPhase}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	foundLeaf := false
	for _, w := range workPhase {
		if w.ID == leaf.ID {
			foundLeaf = true
		}
	}
	if !foundLeaf {
		t.Error("direct-parent filter should return the leaf task")
	}

	// Grandparent filter: leaf must appear (the bug under test).
	parentEpic := epic.ID
	workEpic, err := store.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{ParentID: &parentEpic}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	foundLeaf = false
	foundPhase := false
	for _, w := range workEpic {
		if w.ID == leaf.ID {
			foundLeaf = true
		}
		if w.ID == phase.ID {
			foundPhase = true
		}
	}
	if !foundPhase {
		t.Error("grandparent filter should include the direct child (phase)")
	}
	if !foundLeaf {
		t.Error("grandparent filter should include transitive grandchildren (leaf) - regression for GH#3396")
	}
}

func TestGetReadyWork_ParentFilterReturnsDeepDescendants(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	root := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-deep-root",
		},
		IssueContent: types.IssueContent{
			Title: "Deep Root",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeEpic,
		},
	}
	if err := store.CreateIssue(ctx, root, "tester"); err != nil {
		t.Fatalf("failed to create root: %v", err)
	}

	parentID := root.ID
	const depth = 105
	for i := 1; i <= depth; i++ {
		issue := &types.Issue{
			IssueID: types.IssueID{
				ID: fmt.Sprintf("rw-deep-%03d", i),
			},
			IssueContent: types.IssueContent{
				Title: fmt.Sprintf("Deep child %03d", i),
			},
			IssueWorkflow: types.IssueWorkflow{
				Status:    types.StatusOpen,
				Priority:  2,
				IssueType: types.TypeTask,
			},
		}
		if err := store.CreateIssue(ctx, issue, "tester"); err != nil {
			t.Fatalf("failed to create issue %s: %v", issue.ID, err)
		}
		if err := store.AddDependency(ctx, &types.Dependency{
			IssueID:     issue.ID,
			DependsOnID: parentID,
			Type:        types.DepParentChild,
		}, "tester"); err != nil {
			t.Fatalf("failed to add parent-child dep for %s: %v", issue.ID, err)
		}
		parentID = issue.ID
	}

	rootID := root.ID
	work, err := store.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{ParentID: &rootID}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	foundLeaf := false
	for _, w := range work {
		if w.ID == fmt.Sprintf("rw-deep-%03d", depth) {
			foundLeaf = true
			break
		}
	}
	if !foundLeaf {
		t.Fatalf("parent filter should include descendant beyond depth 100, got %d descendants", len(work))
	}
}

// TestGetReadyWork_CustomStatusBlockerStillBlocks verifies that a blocker with
// a custom status still prevents blocked issues from appearing in ready work.
// Regression test for bd-1x0.
func TestGetReadyWork_CustomStatusBlockerStillBlocks(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	// Configure custom status
	if err := store.SetConfig(ctx, "status.custom", "review"); err != nil {
		t.Fatalf("failed to set custom status config: %v", err)
	}

	blocker := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-cs-blocker",
		},
		IssueContent: types.IssueContent{
			Title: "Custom Status Blocker",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.Status("review"),
			Priority:  1,
			IssueType: types.TypeTask,
		},
	}
	blocked := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-cs-blocked",
		},
		IssueContent: types.IssueContent{
			Title: "Blocked by Custom Status",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}

	for _, iss := range []*types.Issue{blocker, blocked} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}

	dep := &types.Dependency{
		IssueID:     blocked.ID,
		DependsOnID: blocker.ID,
		Type:        types.DepBlocks,
	}
	if err := store.AddDependency(ctx, dep, "tester"); err != nil {
		t.Fatalf("failed to add dependency: %v", err)
	}

	work, err := store.GetReadyWork(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, w := range work {
		if w.ID == blocked.ID {
			t.Error("issue blocked by custom-status blocker should NOT appear in ready work")
		}
	}
}

// TestGetReadyWork_PastDeferredIssueIsReady verifies that an issue whose
// defer_until is in the past appears in ready work. Regression test for a
// timezone bug: Go stores defer_until as UTC, but Dolt's NOW() returns local
// time. On non-UTC machines, the comparison defer_until <= NOW() would
// incorrectly exclude past-deferred issues. The fix uses UTC_TIMESTAMP().
func TestGetReadyWork_PastDeferredIssueIsReady(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	// Create an issue and set defer_until to 1 hour in the past (UTC).
	pastDeferred := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-past-deferred",
		},
		IssueContent: types.IssueContent{
			Title: "Past Deferred Task",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, pastDeferred, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}
	pastTime := time.Now().UTC().Add(-1 * time.Hour)
	if err := store.UpdateIssue(ctx, pastDeferred.ID, map[string]interface{}{
		"defer_until": pastTime,
	}, "tester"); err != nil {
		t.Fatalf("failed to set defer_until: %v", err)
	}

	// Create a normal issue (no defer) as a control.
	normal := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-normal",
		},
		IssueContent: types.IssueContent{
			Title: "Normal Task",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, normal, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}

	work, err := store.GetReadyWork(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	foundPastDeferred := false
	foundNormal := false
	for _, w := range work {
		if w.ID == pastDeferred.ID {
			foundPastDeferred = true
		}
		if w.ID == normal.ID {
			foundNormal = true
		}
	}
	if !foundNormal {
		t.Error("normal issue should appear in ready work")
	}
	if !foundPastDeferred {
		t.Error("past-deferred issue (defer_until in the past) should appear in ready work")
	}
}

// TestGetReadyWork_FutureDeferredIssueExcluded verifies that an issue whose
// defer_until is in the future does NOT appear in ready work.
func TestGetReadyWork_FutureDeferredIssueExcluded(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	futureDeferred := &types.Issue{
		IssueID: types.IssueID{
			ID: "rw-future-deferred",
		},
		IssueContent: types.IssueContent{
			Title: "Future Deferred Task",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, futureDeferred, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}
	futureTime := time.Now().UTC().Add(24 * time.Hour)
	if err := store.UpdateIssue(ctx, futureDeferred.ID, map[string]interface{}{
		"defer_until": futureTime,
	}, "tester"); err != nil {
		t.Fatalf("failed to set defer_until: %v", err)
	}

	work, err := store.GetReadyWork(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, w := range work {
		if w.ID == futureDeferred.ID {
			t.Error("future-deferred issue should NOT appear in ready work")
		}
	}
}

// =============================================================================
// GetBlockedIssues tests
// =============================================================================

func TestGetBlockedIssues_EmptyStore(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	blocked, err := store.GetBlockedIssues(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(blocked) != 0 {
		t.Errorf("expected 0 blocked issues from empty store, got %d", len(blocked))
	}
}

func TestGetBlockedIssues_ReturnsBlockedWithBlockers(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	blocker := &types.Issue{
		IssueID: types.IssueID{
			ID: "bi-blocker",
		},
		IssueContent: types.IssueContent{
			Title: "Blocker Issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
	}
	blocked := &types.Issue{
		IssueID: types.IssueID{
			ID: "bi-blocked",
		},
		IssueContent: types.IssueContent{
			Title: "Blocked Issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}

	for _, iss := range []*types.Issue{blocker, blocked} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}

	dep := &types.Dependency{
		IssueID:     blocked.ID,
		DependsOnID: blocker.ID,
		Type:        types.DepBlocks,
	}
	if err := store.AddDependency(ctx, dep, "tester"); err != nil {
		t.Fatalf("failed to add dependency: %v", err)
	}

	results, err := store.GetBlockedIssues(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, bi := range results {
		if bi.Issue.ID == blocked.ID {
			found = true
			if bi.BlockedByCount != 1 {
				t.Errorf("expected 1 blocker, got %d", bi.BlockedByCount)
			}
			if len(bi.BlockedBy) != 1 || bi.BlockedBy[0] != blocker.ID {
				t.Errorf("expected blocker %s, got %v", blocker.ID, bi.BlockedBy)
			}
		}
	}
	if !found {
		t.Error("expected to find the blocked issue in results")
	}
}

func TestGetBlockedIssues_ExcludesClosedBlockers(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	blocker := &types.Issue{
		IssueID: types.IssueID{
			ID: "bi-closeblocker",
		},
		IssueContent: types.IssueContent{
			Title: "Closed Blocker",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
	}
	blocked := &types.Issue{
		IssueID: types.IssueID{
			ID: "bi-wouldbeblocked",
		},
		IssueContent: types.IssueContent{
			Title: "Would Be Blocked",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}

	for _, iss := range []*types.Issue{blocker, blocked} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}

	dep := &types.Dependency{
		IssueID:     blocked.ID,
		DependsOnID: blocker.ID,
		Type:        types.DepBlocks,
	}
	if err := store.AddDependency(ctx, dep, "tester"); err != nil {
		t.Fatalf("failed to add dependency: %v", err)
	}

	// Close the blocker
	if err := store.CloseIssue(ctx, blocker.ID, "done", "tester", "s1"); err != nil {
		t.Fatalf("failed to close blocker: %v", err)
	}

	results, err := store.GetBlockedIssues(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, bi := range results {
		if bi.Issue.ID == blocked.ID {
			t.Error("issue should not be blocked when its blocker is closed")
		}
	}
}

func TestGetBlockedIssues_MultipleBlockers(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	blockerA := &types.Issue{
		IssueID: types.IssueID{
			ID: "bi-blockerA",
		},
		IssueContent: types.IssueContent{
			Title: "Blocker A",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
	}
	blockerB := &types.Issue{
		IssueID: types.IssueID{
			ID: "bi-blockerB",
		},
		IssueContent: types.IssueContent{
			Title: "Blocker B",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
	}
	blocked := &types.Issue{
		IssueID: types.IssueID{
			ID: "bi-multiblocked",
		},
		IssueContent: types.IssueContent{
			Title: "Multi Blocked",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}

	for _, iss := range []*types.Issue{blockerA, blockerB, blocked} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue %s: %v", iss.ID, err)
		}
	}

	for _, blockerID := range []string{blockerA.ID, blockerB.ID} {
		dep := &types.Dependency{
			IssueID:     blocked.ID,
			DependsOnID: blockerID,
			Type:        types.DepBlocks,
		}
		if err := store.AddDependency(ctx, dep, "tester"); err != nil {
			t.Fatalf("failed to add dependency: %v", err)
		}
	}

	results, err := store.GetBlockedIssues(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, bi := range results {
		if bi.Issue.ID == blocked.ID {
			if bi.BlockedByCount != 2 {
				t.Errorf("expected 2 blockers, got %d", bi.BlockedByCount)
			}
		}
	}
}

func TestGetBlockedIssues_IncludesChildrenOfBlockedParents(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	blocker := &types.Issue{
		IssueID: types.IssueID{
			ID: "bi-preblocker",
		},
		IssueContent: types.IssueContent{
			Title: "Prerequisite",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeEpic,
		},
		// must match blocked type for DepBlocks (GH#1495),
	}
	epic := &types.Issue{
		IssueID: types.IssueID{
			ID: "bi-epic",
		},
		IssueContent: types.IssueContent{
			Title: "Gated Epic",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeEpic,
		},
	}
	child := &types.Issue{
		IssueID: types.IssueID{
			ID: "bi-epic.1",
		},
		IssueContent: types.IssueContent{
			Title: "Child Task",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}

	for _, iss := range []*types.Issue{blocker, epic, child} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue %s: %v", iss.ID, err)
		}
	}

	// Block the epic
	if err := store.AddDependency(ctx, &types.Dependency{
		IssueID:     epic.ID,
		DependsOnID: blocker.ID,
		Type:        types.DepBlocks,
	}, "tester"); err != nil {
		t.Fatalf("failed to add blocking dep: %v", err)
	}

	// Make child a child of the epic
	if err := store.AddDependency(ctx, &types.Dependency{
		IssueID:     child.ID,
		DependsOnID: epic.ID,
		Type:        types.DepParentChild,
	}, "tester"); err != nil {
		t.Fatalf("failed to add parent-child dep: %v", err)
	}

	// Child should NOT be in ready work (parent is blocked)
	ready, err := store.GetReadyWork(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatalf("GetReadyWork: %v", err)
	}
	for _, iss := range ready {
		if iss.ID == child.ID {
			t.Error("child of blocked parent should NOT be in ready work")
		}
	}

	// Child SHOULD appear in blocked issues (GH#1495)
	blocked, err := store.GetBlockedIssues(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatalf("GetBlockedIssues: %v", err)
	}

	epicFound := false
	childFound := false
	for _, bi := range blocked {
		if bi.Issue.ID == epic.ID {
			epicFound = true
		}
		if bi.Issue.ID == child.ID {
			childFound = true
			// Child should show parent as the blocker
			if bi.BlockedByCount != 1 || len(bi.BlockedBy) == 0 || bi.BlockedBy[0] != epic.ID {
				t.Errorf("child blocked-by should be [%s], got %v", epic.ID, bi.BlockedBy)
			}
		}
	}
	if !epicFound {
		t.Error("epic should be in blocked list")
	}
	if !childFound {
		t.Error("child of blocked parent should appear in blocked list (GH#1495)")
	}
}

// =============================================================================
// SearchIssues tests
// =============================================================================

func TestSearchIssues_EmptyStore(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	results, err := store.SearchIssues(ctx, "anything", types.IssueFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results from empty store, got %d", len(results))
	}
}

func TestSearchIssues_ByTitle(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	issue := &types.Issue{
		IssueID: types.IssueID{
			ID: "si-title",
		},
		IssueContent: types.IssueContent{
			Title: "Unique Searchable Title",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, issue, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}

	results, err := store.SearchIssues(ctx, "Unique Searchable", types.IssueFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].ID != issue.ID {
		t.Errorf("expected issue %s, got %s", issue.ID, results[0].ID)
	}
}

// TestSearchIssues_ByDescription verifies that DescriptionContains filter finds
// issues by description text. Free-text search no longer scans descriptions
// (hq-319 optimization) — use DescriptionContains for explicit description search.
func TestSearchIssues_ByDescription(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	issue := &types.Issue{
		IssueID: types.IssueID{
			ID: "si-desc",
		},
		IssueContent: types.IssueContent{
			Title:       "Normal Title",
			Description: "Special unique description text",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, issue, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}

	// Free-text query should NOT match description-only content (hq-319).
	results, err := store.SearchIssues(ctx, "Special unique description", types.IssueFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("free-text search should not scan descriptions (hq-319), got %d results", len(results))
	}

	// DescriptionContains filter should still find it.
	results, err = store.SearchIssues(ctx, "", types.IssueFilter{IssueFilterMatch: types.IssueFilterMatch{DescriptionContains: "Special unique description"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result with DescriptionContains, got %d", len(results))
	}
	if results[0].ID != issue.ID {
		t.Errorf("expected issue %s, got %s", issue.ID, results[0].ID)
	}
}

// TestSearchIssues_ByExternalRef verifies two things:
//  1. A free-text query like "BE-1521" (which looksLikeIssueID returns true for)
//     matches an issue whose external_ref contains that string.
//  2. The ExternalRefContains filter works for explicit substring search.
func TestSearchIssues_ByExternalRef(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	linearURL := "https://linear.app/example-org/issue/BE-1521"
	issue := &types.Issue{
		IssueID: types.IssueID{
			ID: "si-extref-xyz",
		},
		IssueContent: types.IssueContent{
			Title: "Migrate EmailUtils across all services",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  3,
			IssueType: types.TypeTask,
		},
		IssueMeta: types.IssueMeta{
			ExternalRef: &linearURL,
		},
	}
	if err := store.CreateIssue(ctx, issue, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}

	// Free-text ID-like query should match via external_ref LIKE.
	results, err := store.SearchIssues(ctx, "BE-1521", types.IssueFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("free-text search for external ref id: expected 1 result, got %d", len(results))
	}
	if results[0].ID != issue.ID {
		t.Errorf("expected %s, got %s", issue.ID, results[0].ID)
	}

	// ExternalRefContains filter should also find it.
	results, err = store.SearchIssues(ctx, "", types.IssueFilter{IssueFilterMatch: types.IssueFilterMatch{ExternalRefContains: "BE-1521"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("ExternalRefContains filter: expected 1 result, got %d", len(results))
	}
	if results[0].ID != issue.ID {
		t.Errorf("expected %s, got %s", issue.ID, results[0].ID)
	}

	// Unrelated query should not match.
	results, err = store.SearchIssues(ctx, "BE-9999", types.IssueFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no results for unrelated external ref, got %d", len(results))
	}

	// ExternalRef exact match should find the issue.
	results, err = store.SearchIssues(ctx, "", types.IssueFilter{IssueFilterMatch: types.IssueFilterMatch{ExternalRef: &linearURL}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("ExternalRef exact match: expected 1 result, got %d", len(results))
	}
	if results[0].ID != issue.ID {
		t.Errorf("expected %s, got %s", issue.ID, results[0].ID)
	}

	// ExternalRef exact match with wrong value should return nothing.
	wrongRef := "jira-WRONG-123"
	results, err = store.SearchIssues(ctx, "", types.IssueFilter{IssueFilterMatch: types.IssueFilterMatch{ExternalRef: &wrongRef}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("ExternalRef exact match with wrong value: expected 0 results, got %d", len(results))
	}
}

func TestSearchIssues_ByID(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	issue := &types.Issue{
		IssueID: types.IssueID{
			ID: "si-searchbyid-xyz",
		},
		IssueContent: types.IssueContent{
			Title: "ID Search",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, issue, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}

	results, err := store.SearchIssues(ctx, "si-searchbyid-xyz", types.IssueFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
}

func TestSearchIssues_NoMatch(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	issue := &types.Issue{
		IssueID: types.IssueID{
			ID: "si-nomatch",
		},
		IssueContent: types.IssueContent{
			Title: "Existing Issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, issue, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}

	results, err := store.SearchIssues(ctx, "zzz-never-matches-zzz", types.IssueFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestSearchIssues_StatusFilter(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	open := &types.Issue{
		IssueID: types.IssueID{
			ID: "si-stat-open",
		},
		IssueContent: types.IssueContent{
			Title:       "Status Filter Test",
			Description: "Open issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	closed := &types.Issue{
		IssueID: types.IssueID{
			ID: "si-stat-closed",
		},
		IssueContent: types.IssueContent{
			Title:       "Status Filter Test Closed",
			Description: "Closed issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}

	for _, iss := range []*types.Issue{open, closed} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}
	if err := store.CloseIssue(ctx, closed.ID, "done", "tester", "s1"); err != nil {
		t.Fatalf("failed to close issue: %v", err)
	}

	openStatus := types.StatusOpen
	results, err := store.SearchIssues(ctx, "Status Filter Test", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Status: &openStatus}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 open result, got %d", len(results))
	}
	if results[0].ID != open.ID {
		t.Errorf("expected open issue, got %s", results[0].ID)
	}
}

func TestSearchIssues_ExcludesPinnedByDefault(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	regular := &types.Issue{
		IssueID: types.IssueID{
			ID: "si-reg",
		},
		IssueContent: types.IssueContent{
			Title: "Regular Issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	pinned := &types.Issue{
		IssueID: types.IssueID{
			ID: "si-pinned",
		},
		IssueContent: types.IssueContent{
			Title: "Pinned Reference",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
		IssueWisp: types.IssueWisp{
			Pinned: true,
		},
	}

	for _, iss := range []*types.Issue{regular, pinned} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}

	// Filter with pinned=false (as bd list now does by default) should exclude pinned beads
	openStatus := types.StatusOpen
	notPinned := false
	results, err := store.SearchIssues(ctx, "", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Status: &openStatus}, IssueFilterFlags: types.IssueFilterFlags{Pinned: &notPinned}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, r := range results {
		if r.ID == pinned.ID {
			t.Error("pinned issue should not appear when Pinned filter is false")
		}
	}
	found := false
	for _, r := range results {
		if r.ID == regular.ID {
			found = true
		}
	}
	if !found {
		t.Error("regular issue should appear when Pinned filter is false")
	}
}

func TestSearchIssues_PriorityFilter(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	p1 := &types.Issue{
		IssueID: types.IssueID{
			ID: "si-pri-1",
		},
		IssueContent: types.IssueContent{
			Title: "Priority Filter",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
	}
	p4 := &types.Issue{
		IssueID: types.IssueID{
			ID: "si-pri-4",
		},
		IssueContent: types.IssueContent{
			Title: "Priority Filter Low",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  4,
			IssueType: types.TypeTask,
		},
	}

	for _, iss := range []*types.Issue{p1, p4} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}

	priority := 1
	results, err := store.SearchIssues(ctx, "Priority Filter", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Priority: &priority}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result for priority 1, got %d", len(results))
	}
	if results[0].ID != p1.ID {
		t.Errorf("expected p1 issue, got %s", results[0].ID)
	}
}

func TestSearchIssues_LimitFilter(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	for i := 0; i < 5; i++ {
		iss := &types.Issue{
			IssueID: types.IssueID{
				ID: fmt.Sprintf("si-limit-%d", i),
			},
			IssueContent: types.IssueContent{
				Title: "Limit Test Issue",
			},
			IssueWorkflow: types.IssueWorkflow{
				Status:    types.StatusOpen,
				Priority:  2,
				IssueType: types.TypeTask,
			},
		}
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}

	results, err := store.SearchIssues(ctx, "Limit Test", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Limit: 3}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) > 3 {
		t.Errorf("expected at most 3 results with Limit=3, got %d", len(results))
	}
}

func TestSearchIssues_LabelFilter(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	labeled := &types.Issue{
		IssueID: types.IssueID{
			ID: "si-labeled",
		},
		IssueContent: types.IssueContent{
			Title: "Label Filter Test",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	unlabeled := &types.Issue{
		IssueID: types.IssueID{
			ID: "si-unlabeled",
		},
		IssueContent: types.IssueContent{
			Title: "Label Filter Test No Label",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}

	for _, iss := range []*types.Issue{labeled, unlabeled} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}

	if err := store.AddLabel(ctx, labeled.ID, "important", "tester"); err != nil {
		t.Fatalf("failed to add label: %v", err)
	}

	results, err := store.SearchIssues(ctx, "Label Filter Test", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Labels: []string{"important"}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result with label filter, got %d", len(results))
	}
	if results[0].ID != labeled.ID {
		t.Errorf("expected labeled issue, got %s", results[0].ID)
	}
}

func TestSearchIssues_EmptyQuery(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	for i := 0; i < 3; i++ {
		iss := &types.Issue{
			IssueID: types.IssueID{
				ID: fmt.Sprintf("si-empty-%d", i),
			},
			IssueContent: types.IssueContent{
				Title: fmt.Sprintf("Empty Query Issue %d", i),
			},
			IssueWorkflow: types.IssueWorkflow{
				Status:    types.StatusOpen,
				Priority:  2,
				IssueType: types.TypeTask,
			},
		}
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}

	results, err := store.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) < 3 {
		t.Errorf("expected at least 3 results with empty query, got %d", len(results))
	}
}

func TestSearchIssues_IssueTypeFilter(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	task := &types.Issue{
		IssueID: types.IssueID{
			ID: "si-type-task",
		},
		IssueContent: types.IssueContent{
			Title: "Type Filter",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	bug := &types.Issue{
		IssueID: types.IssueID{
			ID: "si-type-bug",
		},
		IssueContent: types.IssueContent{
			Title: "Type Filter Bug",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeBug,
		},
	}

	for _, iss := range []*types.Issue{task, bug} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}

	bugType := types.TypeBug
	results, err := store.SearchIssues(ctx, "Type Filter", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{IssueType: &bugType}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 bug result, got %d", len(results))
	}
	if results[0].ID != bug.ID {
		t.Errorf("expected bug issue, got %s", results[0].ID)
	}
}

func TestSearchIssues_IncludeDependencies(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	parent := &types.Issue{
		IssueID: types.IssueID{
			ID: "si-dep-parent",
		},
		IssueContent: types.IssueContent{
			Title: "DepHydration Parent",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
	}
	child := &types.Issue{
		IssueID: types.IssueID{
			ID: "si-dep-child",
		},
		IssueContent: types.IssueContent{
			Title: "DepHydration Child",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	standalone := &types.Issue{
		IssueID: types.IssueID{
			ID: "si-dep-standalone",
		},
		IssueContent: types.IssueContent{
			Title: "DepHydration Standalone",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  3,
			IssueType: types.TypeTask,
		},
	}
	for _, iss := range []*types.Issue{parent, child, standalone} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue %s: %v", iss.ID, err)
		}
	}

	dep := &types.Dependency{
		IssueID:     child.ID,
		DependsOnID: parent.ID,
		Type:        types.DepBlocks,
	}
	if err := store.AddDependency(ctx, dep, "tester"); err != nil {
		t.Fatalf("failed to add dependency: %v", err)
	}

	t.Run("false_by_default", func(t *testing.T) {
		results, err := store.SearchIssues(ctx, "DepHydration", types.IssueFilter{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, iss := range results {
			if len(iss.Dependencies) > 0 {
				t.Errorf("issue %s has Dependencies populated without IncludeDependencies", iss.ID)
			}
		}
	})

	t.Run("true_hydrates_deps", func(t *testing.T) {
		results, err := store.SearchIssues(ctx, "DepHydration", types.IssueFilter{
			IssueFilterHydrate: types.IssueFilterHydrate{
				IncludeDependencies: true,
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("expected 3 results, got %d", len(results))
		}

		depsByID := make(map[string][]*types.Dependency)
		for _, iss := range results {
			depsByID[iss.ID] = iss.Dependencies
		}

		// child should have one dependency on parent
		childDeps := depsByID[child.ID]
		if len(childDeps) != 1 {
			t.Fatalf("child expected 1 dependency, got %d", len(childDeps))
		}
		if childDeps[0].DependsOnID != parent.ID {
			t.Errorf("child dep.DependsOnID = %s, want %s", childDeps[0].DependsOnID, parent.ID)
		}
		if childDeps[0].Type != types.DepBlocks {
			t.Errorf("child dep.Type = %s, want %s", childDeps[0].Type, types.DepBlocks)
		}

		// parent and standalone should have no dependencies
		if len(depsByID[parent.ID]) != 0 {
			t.Errorf("parent expected 0 dependencies, got %d", len(depsByID[parent.ID]))
		}
		if len(depsByID[standalone.ID]) != 0 {
			t.Errorf("standalone expected 0 dependencies, got %d", len(depsByID[standalone.ID]))
		}
	})
}

// =============================================================================
// GetStatistics tests
// =============================================================================

func TestGetStatistics_EmptyStore(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	stats, err := store.GetStatistics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.TotalIssues != 0 {
		t.Errorf("expected 0 total issues, got %d", stats.TotalIssues)
	}
	if stats.OpenIssues != 0 {
		t.Errorf("expected 0 open issues, got %d", stats.OpenIssues)
	}
	if stats.ClosedIssues != 0 {
		t.Errorf("expected 0 closed issues, got %d", stats.ClosedIssues)
	}
	if stats.BlockedIssues == nil || *stats.BlockedIssues != 0 {
		got := 0
		if stats.BlockedIssues != nil {
			got = *stats.BlockedIssues
		}
		t.Errorf("expected 0 blocked issues, got %d", got)
	}
}

func TestGetStatistics_CountsByStatus(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	issues := []*types.Issue{
		{IssueID: types.IssueID{ID: "stat-open-1"}, IssueContent: types.IssueContent{Title: "Open 1"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}},
		{IssueID: types.IssueID{ID: "stat-open-2"}, IssueContent: types.IssueContent{Title: "Open 2"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}},
		{IssueID: types.IssueID{ID: "stat-inprog"}, IssueContent: types.IssueContent{Title: "In Progress"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusInProgress, Priority: 1, IssueType: types.TypeTask}},
		{IssueID: types.IssueID{ID: "stat-closed-1"}, IssueContent: types.IssueContent{Title: "Closed 1"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}},
		{IssueID: types.IssueID{ID: "stat-closed-2"}, IssueContent: types.IssueContent{Title: "Closed 2"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}},
	}
	for _, iss := range issues {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue %s: %v", iss.ID, err)
		}
	}

	// Close two issues
	for _, id := range []string{"stat-closed-1", "stat-closed-2"} {
		if err := store.CloseIssue(ctx, id, "done", "tester", "s1"); err != nil {
			t.Fatalf("failed to close issue %s: %v", id, err)
		}
	}

	stats, err := store.GetStatistics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.TotalIssues != 5 {
		t.Errorf("expected 5 total issues, got %d", stats.TotalIssues)
	}
	if stats.OpenIssues != 2 {
		t.Errorf("expected 2 open issues, got %d", stats.OpenIssues)
	}
	if stats.InProgressIssues != 1 {
		t.Errorf("expected 1 in-progress issue, got %d", stats.InProgressIssues)
	}
	if stats.ClosedIssues != 2 {
		t.Errorf("expected 2 closed issues, got %d", stats.ClosedIssues)
	}
}

func TestGetStatistics_BlockedCount(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	blocker := &types.Issue{
		IssueID: types.IssueID{
			ID: "stat-blocker",
		},
		IssueContent: types.IssueContent{
			Title: "Blocker",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
	}
	blocked := &types.Issue{
		IssueID: types.IssueID{
			ID: "stat-blocked",
		},
		IssueContent: types.IssueContent{
			Title: "Blocked",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}

	for _, iss := range []*types.Issue{blocker, blocked} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}

	dep := &types.Dependency{
		IssueID:     blocked.ID,
		DependsOnID: blocker.ID,
		Type:        types.DepBlocks,
	}
	if err := store.AddDependency(ctx, dep, "tester"); err != nil {
		t.Fatalf("failed to add dependency: %v", err)
	}

	stats, err := store.GetStatistics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.BlockedIssues == nil || *stats.BlockedIssues != 1 {
		got := 0
		if stats.BlockedIssues != nil {
			got = *stats.BlockedIssues
		}
		t.Errorf("expected 1 blocked issue, got %d", got)
	}
}

func TestGetStatistics_PinnedCount(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	pinned := &types.Issue{
		IssueID: types.IssueID{
			ID: "stat-pinned",
		},
		IssueContent: types.IssueContent{
			Title: "Pinned Issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
		IssueWisp: types.IssueWisp{
			Pinned: true,
		},
	}
	if err := store.CreateIssue(ctx, pinned, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}

	stats, err := store.GetStatistics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.PinnedIssues != 1 {
		t.Errorf("expected 1 pinned issue, got %d", stats.PinnedIssues)
	}
}

func TestGetStatistics_DeferredCount(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	deferred := &types.Issue{
		IssueID: types.IssueID{
			ID: "stat-deferred",
		},
		IssueContent: types.IssueContent{
			Title: "Deferred Issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusDeferred,
			Priority:  1,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, deferred, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}

	stats, err := store.GetStatistics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.DeferredIssues != 1 {
		t.Errorf("expected 1 deferred issue, got %d", stats.DeferredIssues)
	}
}

func TestGetStatistics_ReadyIssuesExcludesBlocked(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	blocker := &types.Issue{
		IssueID: types.IssueID{
			ID: "stat-r-blocker",
		},
		IssueContent: types.IssueContent{
			Title: "Blocker",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
	}
	blocked := &types.Issue{
		IssueID: types.IssueID{
			ID: "stat-r-blocked",
		},
		IssueContent: types.IssueContent{
			Title: "Blocked",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	ready := &types.Issue{
		IssueID: types.IssueID{
			ID: "stat-r-ready",
		},
		IssueContent: types.IssueContent{
			Title: "Ready",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  3,
			IssueType: types.TypeTask,
		},
	}

	for _, iss := range []*types.Issue{blocker, blocked, ready} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}

	dep := &types.Dependency{
		IssueID:     blocked.ID,
		DependsOnID: blocker.ID,
		Type:        types.DepBlocks,
	}
	if err := store.AddDependency(ctx, dep, "tester"); err != nil {
		t.Fatalf("failed to add dependency: %v", err)
	}

	stats, err := store.GetStatistics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 3 open issues, 1 blocked => ready = 3 - 1 = 2
	if stats.ReadyIssues == nil || *stats.ReadyIssues != 2 {
		got := -1
		if stats.ReadyIssues != nil {
			got = *stats.ReadyIssues
		}
		t.Errorf("expected 2 ready issues (3 open - 1 blocked), got %d", got)
	}
}

// TestGetStatisticsNoBlocked_LeavesBlockedAndReadyNil verifies the --no-blocked
// fast path (GetStatisticsNoBlocked) leaves BlockedIssues and ReadyIssues nil
// (readiness needs the blocked set), while the full GetStatistics path on the
// same data populates both. Guards against a *int fake-zero regression: a nil
// pointer must never silently render/serialize as 0.
func TestGetStatisticsNoBlocked_LeavesBlockedAndReadyNil(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	blocker := &types.Issue{
		IssueID: types.IssueID{
			ID: "stat-nb-blocker",
		},
		IssueContent: types.IssueContent{
			Title: "Blocker",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
	}
	blocked := &types.Issue{
		IssueID: types.IssueID{
			ID: "stat-nb-blocked",
		},
		IssueContent: types.IssueContent{
			Title: "Blocked",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	for _, iss := range []*types.Issue{blocker, blocked} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}
	dep := &types.Dependency{
		IssueID:     blocked.ID,
		DependsOnID: blocker.ID,
		Type:        types.DepBlocks,
	}
	if err := store.AddDependency(ctx, dep, "tester"); err != nil {
		t.Fatalf("failed to add dependency: %v", err)
	}

	noBlocked, err := store.GetStatisticsNoBlocked(ctx)
	if err != nil {
		t.Fatalf("GetStatisticsNoBlocked: unexpected error: %v", err)
	}
	if noBlocked.TotalIssues != 2 {
		t.Errorf("GetStatisticsNoBlocked: expected 2 total issues, got %d", noBlocked.TotalIssues)
	}
	if noBlocked.BlockedIssues != nil {
		t.Errorf("GetStatisticsNoBlocked: expected BlockedIssues nil, got %d", *noBlocked.BlockedIssues)
	}
	if noBlocked.ReadyIssues != nil {
		t.Errorf("GetStatisticsNoBlocked: expected ReadyIssues nil, got %d", *noBlocked.ReadyIssues)
	}

	full, err := store.GetStatistics(ctx)
	if err != nil {
		t.Fatalf("GetStatistics: unexpected error: %v", err)
	}
	if full.BlockedIssues == nil {
		t.Fatal("GetStatistics: expected BlockedIssues populated, got nil")
	}
	if *full.BlockedIssues != 1 {
		t.Errorf("GetStatistics: expected 1 blocked issue, got %d", *full.BlockedIssues)
	}
	if full.ReadyIssues == nil {
		t.Fatal("GetStatistics: expected ReadyIssues populated, got nil")
	}
	if *full.ReadyIssues != 1 {
		t.Errorf("GetStatistics: expected 1 ready issue (2 open - 1 blocked), got %d", *full.ReadyIssues)
	}
}

// =============================================================================
// GetStaleIssues tests
// =============================================================================

func TestGetStaleIssues_EmptyStore(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	stale, err := store.GetStaleIssues(ctx, types.StaleFilter{Days: 7})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("expected 0 stale issues from empty store, got %d", len(stale))
	}
}

func TestGetStaleIssues_ReturnsStale(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	issue := &types.Issue{
		IssueID: types.IssueID{
			ID: "stale-old",
		},
		IssueContent: types.IssueContent{
			Title: "Old Issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, issue, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}

	// Backdate updated_at to 15 days ago
	oldDate := time.Now().UTC().AddDate(0, 0, -15)
	_, err := store.db.ExecContext(ctx,
		"UPDATE issues SET updated_at = ? WHERE id = ?", oldDate, issue.ID)
	if err != nil {
		t.Fatalf("failed to backdate: %v", err)
	}

	stale, err := store.GetStaleIssues(ctx, types.StaleFilter{Days: 7})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stale) != 1 {
		t.Fatalf("expected 1 stale issue, got %d", len(stale))
	}
	if stale[0].ID != issue.ID {
		t.Errorf("expected issue %s, got %s", issue.ID, stale[0].ID)
	}
}

func TestGetStaleIssues_ExcludesRecent(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	issue := &types.Issue{
		IssueID: types.IssueID{
			ID: "stale-fresh",
		},
		IssueContent: types.IssueContent{
			Title: "Fresh Issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, issue, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}
	// updated_at is "now" (set by CreateIssue)

	stale, err := store.GetStaleIssues(ctx, types.StaleFilter{Days: 7})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, s := range stale {
		if s.ID == issue.ID {
			t.Error("recently updated issue should not be stale")
		}
	}
}

func TestGetStaleIssues_ExcludesClosed(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	issue := &types.Issue{
		IssueID: types.IssueID{
			ID: "stale-closed",
		},
		IssueContent: types.IssueContent{
			Title: "Closed Stale",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, issue, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}
	if err := store.CloseIssue(ctx, issue.ID, "done", "tester", "s1"); err != nil {
		t.Fatalf("failed to close issue: %v", err)
	}

	// Backdate updated_at
	oldDate := time.Now().UTC().AddDate(0, 0, -15)
	_, err := store.db.ExecContext(ctx,
		"UPDATE issues SET updated_at = ? WHERE id = ?", oldDate, issue.ID)
	if err != nil {
		t.Fatalf("failed to backdate: %v", err)
	}

	stale, err := store.GetStaleIssues(ctx, types.StaleFilter{Days: 7})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, s := range stale {
		if s.ID == issue.ID {
			t.Error("closed issue should not appear in stale results")
		}
	}
}

func TestGetStaleIssues_StatusFilter(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	open := &types.Issue{
		IssueID: types.IssueID{
			ID: "stale-sf-open",
		},
		IssueContent: types.IssueContent{
			Title: "Open Stale",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	inProg := &types.Issue{
		IssueID: types.IssueID{
			ID: "stale-sf-inprog",
		},
		IssueContent: types.IssueContent{
			Title: "In Progress Stale",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusInProgress,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}

	for _, iss := range []*types.Issue{open, inProg} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}

	// Backdate both
	oldDate := time.Now().UTC().AddDate(0, 0, -15)
	for _, id := range []string{open.ID, inProg.ID} {
		_, err := store.db.ExecContext(ctx,
			"UPDATE issues SET updated_at = ? WHERE id = ?", oldDate, id)
		if err != nil {
			t.Fatalf("failed to backdate: %v", err)
		}
	}

	// Filter for in_progress only
	stale, err := store.GetStaleIssues(ctx, types.StaleFilter{Days: 7, Status: string(types.StatusInProgress)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	foundOpen := false
	foundInProg := false
	for _, s := range stale {
		if s.ID == open.ID {
			foundOpen = true
		}
		if s.ID == inProg.ID {
			foundInProg = true
		}
	}
	if foundOpen {
		t.Error("open issue should not appear when filtering for in_progress")
	}
	if !foundInProg {
		t.Error("in_progress issue should appear when filtering for in_progress")
	}
}

func TestGetStaleIssues_LimitFilter(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	for i := 0; i < 5; i++ {
		iss := &types.Issue{
			IssueID: types.IssueID{
				ID: fmt.Sprintf("stale-lim-%d", i),
			},
			IssueContent: types.IssueContent{
				Title: fmt.Sprintf("Stale Limit %d", i),
			},
			IssueWorkflow: types.IssueWorkflow{
				Status:    types.StatusOpen,
				Priority:  2,
				IssueType: types.TypeTask,
			},
		}
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}
	}

	// Backdate all
	oldDate := time.Now().UTC().AddDate(0, 0, -15)
	for i := 0; i < 5; i++ {
		_, err := store.db.ExecContext(ctx,
			"UPDATE issues SET updated_at = ? WHERE id = ?", oldDate, fmt.Sprintf("stale-lim-%d", i))
		if err != nil {
			t.Fatalf("failed to backdate: %v", err)
		}
	}

	stale, err := store.GetStaleIssues(ctx, types.StaleFilter{Days: 7, Limit: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stale) > 2 {
		t.Errorf("expected at most 2 results with Limit=2, got %d", len(stale))
	}
}

func TestGetStaleIssues_ExcludesEphemeral(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	eph := &types.Issue{
		IssueID: types.IssueID{
			ID: "stale-eph",
		},
		IssueContent: types.IssueContent{
			Title: "Ephemeral Stale",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
		IssueWisp: types.IssueWisp{
			Ephemeral: true,
		},
	}
	if err := store.CreateIssue(ctx, eph, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}

	// Backdate
	oldDate := time.Now().UTC().AddDate(0, 0, -15)
	_, err := store.db.ExecContext(ctx,
		"UPDATE issues SET updated_at = ? WHERE id = ?", oldDate, eph.ID)
	if err != nil {
		t.Fatalf("failed to backdate: %v", err)
	}

	stale, err := store.GetStaleIssues(ctx, types.StaleFilter{Days: 7})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, s := range stale {
		if s.ID == eph.ID {
			t.Error("ephemeral issue should not appear in stale results")
		}
	}
}

// =============================================================================
// Counter mode tests (issue_id_mode=counter)
// =============================================================================

func TestCreateIssue_CounterMode(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	// Enable counter mode
	if err := store.SetConfig(ctx, "issue_id_mode", "counter"); err != nil {
		t.Fatalf("failed to set issue_id_mode: %v", err)
	}

	// Create first issue - should get test-1
	issue1 := &types.Issue{
		IssueContent: types.IssueContent{
			Title: "First issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, issue1, "tester"); err != nil {
		t.Fatalf("failed to create issue1: %v", err)
	}
	if issue1.ID != "test-1" {
		t.Errorf("expected test-1, got %q", issue1.ID)
	}

	// Create second issue - should get test-2
	issue2 := &types.Issue{
		IssueContent: types.IssueContent{
			Title: "Second issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, issue2, "tester"); err != nil {
		t.Fatalf("failed to create issue2: %v", err)
	}
	if issue2.ID != "test-2" {
		t.Errorf("expected test-2, got %q", issue2.ID)
	}
}

func TestCreateIssue_ExplicitIDOverridesCounter(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	// Enable counter mode
	if err := store.SetConfig(ctx, "issue_id_mode", "counter"); err != nil {
		t.Fatalf("failed to set issue_id_mode: %v", err)
	}

	// Create issue with explicit ID - counter should NOT be used
	issue := &types.Issue{
		IssueID: types.IssueID{
			ID: "test-explicit",
		},
		IssueContent: types.IssueContent{
			Title: "Explicit ID issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, issue, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}
	if issue.ID != "test-explicit" {
		t.Errorf("expected test-explicit, got %q", issue.ID)
	}
}

func TestCreateIssue_HashModeDefault(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	// No issue_id_mode set (default = hash mode)
	issue := &types.Issue{
		IssueContent: types.IssueContent{
			Title: "Hash ID issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, issue, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}
	// Hash IDs have format "prefix-<alphanum>", not "prefix-<int>"
	if issue.ID == "" {
		t.Error("expected non-empty ID in hash mode")
	}
	// Hash mode IDs should NOT be purely numeric after the prefix
	// (they use base36: 0-9a-z, so length > 1 and not just digits)
	if issue.ID == "test-1" || issue.ID == "test-2" {
		t.Errorf("hash mode should not generate sequential IDs, got %q", issue.ID)
	}
}

// =============================================================================
// Counter mode seeding tests (GH#2002)
// =============================================================================

// TestCounterMode_SeedsFromExistingIssues verifies that enabling counter mode
// on a repo with pre-existing sequential IDs seeds the counter from the max
// existing ID rather than starting at 1 (which would cause collisions).
func TestCounterMode_SeedsFromExistingIssues(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	// Create issues with explicit sequential IDs (simulating manual creation
	// before counter mode was enabled).
	for _, id := range []string{"test-5", "test-10", "test-3"} {
		issue := &types.Issue{
			IssueID: types.IssueID{
				ID: id,
			},
			IssueContent: types.IssueContent{
				Title: "Pre-existing issue " + id,
			},
			IssueWorkflow: types.IssueWorkflow{
				Status:    types.StatusOpen,
				Priority:  2,
				IssueType: types.TypeTask,
			},
		}
		if err := store.CreateIssue(ctx, issue, "tester"); err != nil {
			t.Fatalf("failed to create issue %s: %v", id, err)
		}
	}

	// Now enable counter mode (simulating the user running bd config set issue_id_mode counter).
	if err := store.SetConfig(ctx, "issue_id_mode", "counter"); err != nil {
		t.Fatalf("failed to enable counter mode: %v", err)
	}

	// The next auto-generated issue should be test-11 (max existing was 10).
	next := &types.Issue{
		IssueContent: types.IssueContent{
			Title: "First counter-mode issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, next, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}
	if next.ID != "test-11" {
		t.Errorf("expected test-11 (seeded from max existing id 10), got %q", next.ID)
	}
}

// TestCounterMode_SeedsFromMixed verifies that when existing issues contain a
// mix of hash-based IDs and numeric IDs, only the numeric ones are counted
// for seeding purposes.
func TestCounterMode_SeedsFromMixed(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	// Create a mix: one hash-based ID and one numeric ID.
	hashIssue := &types.Issue{
		IssueID: types.IssueID{
			ID: "test-a3f2",
		},
		IssueContent: types.IssueContent{
			Title: "Hash-based issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	numericIssue := &types.Issue{
		IssueID: types.IssueID{
			ID: "test-7",
		},
		IssueContent: types.IssueContent{
			Title: "Numeric issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	for _, iss := range []*types.Issue{hashIssue, numericIssue} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue %s: %v", iss.ID, err)
		}
	}

	// Enable counter mode.
	if err := store.SetConfig(ctx, "issue_id_mode", "counter"); err != nil {
		t.Fatalf("failed to enable counter mode: %v", err)
	}

	// Only the numeric ID (test-7) should count; next should be test-8.
	next := &types.Issue{
		IssueContent: types.IssueContent{
			Title: "First counter-mode issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, next, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}
	if next.ID != "test-8" {
		t.Errorf("expected test-8 (seeded from max numeric id 7, ignoring hash id), got %q", next.ID)
	}
}

// TestCounterMode_NoExistingIssues verifies that a fresh repo with counter mode
// enabled starts the counter at 1 (existing behavior preserved).
func TestCounterMode_NoExistingIssues(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	// Enable counter mode immediately (no prior issues).
	if err := store.SetConfig(ctx, "issue_id_mode", "counter"); err != nil {
		t.Fatalf("failed to enable counter mode: %v", err)
	}

	first := &types.Issue{
		IssueContent: types.IssueContent{
			Title: "First issue in fresh repo",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, first, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}
	if first.ID != "test-1" {
		t.Errorf("expected test-1 in fresh repo, got %q", first.ID)
	}
}

// TestCounterMode_AlreadySeeded verifies that if a counter row already exists
// (e.g., the counter is at 20), seeding is skipped even if higher manually-
// created IDs like test-99 exist. The counter must NOT regress.
func TestCounterMode_AlreadySeeded(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	// Manually insert a counter row at 20 (simulates an already-running counter).
	_, err := store.db.ExecContext(ctx,
		"INSERT INTO issue_counter (prefix, last_id) VALUES (?, ?)", "test", 20)
	if err != nil {
		t.Fatalf("failed to seed counter: %v", err)
	}

	// Create a manually-specified issue with a higher ID than the counter.
	highIssue := &types.Issue{
		IssueID: types.IssueID{
			ID: "test-99",
		},
		IssueContent: types.IssueContent{
			Title: "High manual ID",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, highIssue, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}

	// Enable counter mode.
	if err := store.SetConfig(ctx, "issue_id_mode", "counter"); err != nil {
		t.Fatalf("failed to enable counter mode: %v", err)
	}

	// Next issue should be test-21 (counter was at 20; seeding must NOT override
	// the existing counter row even though test-99 exists).
	next := &types.Issue{
		IssueContent: types.IssueContent{
			Title: "Next counter issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	if err := store.CreateIssue(ctx, next, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}
	if next.ID != "test-21" {
		t.Errorf("expected test-21 (counter must not re-seed over existing row), got %q", next.ID)
	}
}

// TestSearchIssues_NoDuplicatesWithMultipleBlockers verifies that
// SearchIssues returns an issue exactly once even when it has multiple
// blocks dependencies. GH#3567.
func TestSearchIssues_NoDuplicatesWithMultipleBlockers(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	// Create an epic parent and two blocker issues.
	epic := &types.Issue{
		IssueID: types.IssueID{
			ID: "dup-epic",
		},
		IssueContent: types.IssueContent{
			Title: "Epic parent",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeEpic,
		},
	}
	blockerA := &types.Issue{
		IssueID: types.IssueID{
			ID: "dup-blocker-a",
		},
		IssueContent: types.IssueContent{
			Title: "Blocker A",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	blockerB := &types.Issue{
		IssueID: types.IssueID{
			ID: "dup-blocker-b",
		},
		IssueContent: types.IssueContent{
			Title: "Blocker B",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}
	blocked := &types.Issue{
		IssueID: types.IssueID{
			ID: "dup-blocked",
		},
		IssueContent: types.IssueContent{
			Title: "Blocked issue with two blockers",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		},
	}

	for _, iss := range []*types.Issue{epic, blockerA, blockerB, blocked} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue %s: %v", iss.ID, err)
		}
	}

	// blocked is a child of epic and blocked by both A and B.
	deps := []*types.Dependency{
		{IssueID: blocked.ID, DependsOnID: epic.ID, Type: types.DepParentChild},
		{IssueID: blocked.ID, DependsOnID: blockerA.ID, Type: types.DepBlocks},
		{IssueID: blocked.ID, DependsOnID: blockerB.ID, Type: types.DepBlocks},
	}
	for _, dep := range deps {
		if err := store.AddDependency(ctx, dep, "tester"); err != nil {
			t.Fatalf("failed to add dependency %s -> %s: %v", dep.IssueID, dep.DependsOnID, err)
		}
	}

	results, err := store.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	idCounts := make(map[string]int)
	for _, iss := range results {
		idCounts[iss.ID]++
	}

	for id, count := range idCounts {
		if count > 1 {
			t.Errorf("issue %s appeared %d times (expected 1)", id, count)
		}
	}

	if idCounts[blocked.ID] != 1 {
		t.Errorf("blocked issue %s appeared %d times (expected exactly 1)", blocked.ID, idCounts[blocked.ID])
	}
}

// TestSearchIssues_StableOrdering verifies that SearchIssues returns
// deterministic ordering when multiple issues share the same priority
// and created_at timestamp. The id column acts as a tiebreaker.
func TestSearchIssues_StableOrdering(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	now := time.Now()
	// Create issues with identical priority and created_at but different IDs.
	for _, id := range []string{"stable-c", "stable-a", "stable-b"} {
		iss := &types.Issue{
			IssueID: types.IssueID{
				ID: id,
			},
			IssueContent: types.IssueContent{
				Title: "Stable Ordering Test",
			},
			IssueWorkflow: types.IssueWorkflow{
				Status:    types.StatusOpen,
				Priority:  2,
				IssueType: types.TypeTask,
			},
			IssueTimes: types.IssueTimes{
				CreatedAt: now,
			},
		}
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue %s: %v", id, err)
		}
	}

	// Run the query multiple times and verify identical ordering each time.
	var firstOrder string
	for i := 0; i < 5; i++ {
		results, err := store.SearchIssues(ctx, "Stable Ordering", types.IssueFilter{})
		if err != nil {
			t.Fatalf("run %d: unexpected error: %v", i, err)
		}
		if len(results) != 3 {
			t.Fatalf("run %d: expected 3 results, got %d", i, len(results))
		}
		var ids []string
		for _, r := range results {
			ids = append(ids, r.ID)
		}
		order := strings.Join(ids, ",")
		if i == 0 {
			firstOrder = order
			// With id ASC tiebreaker, expect alphabetical: a, b, c.
			if ids[0] != "stable-a" || ids[1] != "stable-b" || ids[2] != "stable-c" {
				t.Errorf("expected [stable-a, stable-b, stable-c], got %v", ids)
			}
		} else if order != firstOrder {
			t.Errorf("run %d: ordering changed from %q to %q", i, firstOrder, order)
		}
	}
}
