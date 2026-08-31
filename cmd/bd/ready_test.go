//go:build cgo

package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonbaldie/beads/internal/types"
)

func TestReadySuite(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	testDB := filepath.Join(tmpDir, ".beads", "beads.db")
	s := newTestStore(t, testDB)
	ctx := context.Background()

	// ========== Shared data setup ==========
	// All sub-tests share one DB. IDs are unique across all sub-tests.

	// --- Core ready work data ---
	coreIssues := []*types.Issue{
		{IssueID: types.IssueID{ID: "test-1"}, IssueContent: types.IssueContent{Title: "Ready task 1"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-2"}, IssueContent: types.IssueContent{Title: "Ready task 2"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-3"}, IssueContent: types.IssueContent{Title: "Blocked task"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-blocker"}, IssueContent: types.IssueContent{Title: "Blocking task"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 0, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-closed"}, IssueContent: types.IssueContent{Title: "Closed task"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusClosed, Priority: 2, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now(), ClosedAt: ptrTime(time.Now())}},
	}
	for _, issue := range coreIssues {
		if err := s.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatal(err)
		}
	}
	// test-3 depends on test-blocker
	if err := s.AddDependency(ctx, &types.Dependency{
		IssueID: "test-3", DependsOnID: "test-blocker", Type: types.DepBlocks, CreatedAt: time.Now(),
	}, "test"); err != nil {
		t.Fatal(err)
	}

	// --- Assignee data ---
	assigneeIssues := []*types.Issue{
		{IssueID: types.IssueID{ID: "test-alice"}, IssueContent: types.IssueContent{Title: "Alice's task"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask, Assignee: "alice"}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-bob"}, IssueContent: types.IssueContent{Title: "Bob's task"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask, Assignee: "bob"}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-unassigned"}, IssueContent: types.IssueContent{Title: "Unassigned task"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
	}
	for _, issue := range assigneeIssues {
		if err := s.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatal(err)
		}
	}

	// --- In-progress data ---
	if err := s.CreateIssue(ctx, &types.Issue{
		IssueID: types.IssueID{
			ID: "test-wip",
		},
		IssueContent: types.IssueContent{
			Title: "Work in progress",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusInProgress,
			Priority:  1,
			IssueType: types.TypeTask,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
		},
	}, "test"); err != nil {
		t.Fatal(err)
	}

	// --- Closed-blocker data ---
	closedBlockerIssues := []*types.Issue{
		{IssueID: types.IssueID{ID: "test-closed-blocker-1"}, IssueContent: types.IssueContent{Title: "Closed blocker 1"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-closed-blocker-2"}, IssueContent: types.IssueContent{Title: "Closed blocker 2"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-open-blocker"}, IssueContent: types.IssueContent{Title: "Open blocker"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-ready-via-closed-blockers"}, IssueContent: types.IssueContent{Title: "Ready when all blockers are closed"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-still-blocked"}, IssueContent: types.IssueContent{Title: "Still blocked by open blocker"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
	}
	for _, issue := range closedBlockerIssues {
		if err := s.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatal(err)
		}
	}
	closedBlockerDeps := []*types.Dependency{
		{IssueID: "test-ready-via-closed-blockers", DependsOnID: "test-closed-blocker-1", Type: types.DepBlocks, CreatedAt: time.Now()},
		{IssueID: "test-ready-via-closed-blockers", DependsOnID: "test-closed-blocker-2", Type: types.DepBlocks, CreatedAt: time.Now()},
		{IssueID: "test-still-blocked", DependsOnID: "test-closed-blocker-1", Type: types.DepBlocks, CreatedAt: time.Now()},
		{IssueID: "test-still-blocked", DependsOnID: "test-open-blocker", Type: types.DepBlocks, CreatedAt: time.Now()},
	}
	for _, dep := range closedBlockerDeps {
		if err := s.AddDependency(ctx, dep, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CloseIssue(ctx, "test-closed-blocker-1", "completed", "test", "session-ready-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseIssue(ctx, "test-closed-blocker-2", "completed", "test", "session-ready-2"); err != nil {
		t.Fatal(err)
	}

	// --- Epic/parent-child data (for buildParentEpicMap) ---
	epicIssues := []*types.Issue{
		{IssueID: types.IssueID{ID: "test-epic"}, IssueContent: types.IssueContent{Title: "Auth Overhaul"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeEpic}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-parent-task"}, IssueContent: types.IssueContent{Title: "Parent Task"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-child-1"}, IssueContent: types.IssueContent{Title: "Implement login"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-child-2"}, IssueContent: types.IssueContent{Title: "Subtask of non-epic"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-orphan"}, IssueContent: types.IssueContent{Title: "Standalone task"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 3, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
	}
	for _, issue := range epicIssues {
		if err := s.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatal(err)
		}
	}
	epicDeps := []*types.Dependency{
		{IssueID: "test-child-1", DependsOnID: "test-epic", Type: types.DepParentChild, CreatedAt: time.Now()},
		{IssueID: "test-child-2", DependsOnID: "test-parent-task", Type: types.DepParentChild, CreatedAt: time.Now()},
	}
	for _, dep := range epicDeps {
		if err := s.AddDependency(ctx, dep, "test"); err != nil {
			t.Fatal(err)
		}
	}

	// --- Defer data ---
	futureDefer := time.Now().Add(24 * time.Hour)
	pastDefer := time.Now().Add(-25 * time.Hour) // 25h to account for UTC/local timezone mismatch
	deferIssues := []*types.Issue{
		{IssueID: types.IssueID{ID: "test-future-defer"}, IssueContent: types.IssueContent{Title: "Future deferred task"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}, IssueLease: types.IssueLease{DeferUntil: &futureDefer}},
		{IssueID: types.IssueID{ID: "test-past-defer"}, IssueContent: types.IssueContent{Title: "Past deferred task"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}, IssueLease: types.IssueLease{DeferUntil: &pastDefer}},
		{IssueID: types.IssueID{ID: "test-no-defer"}, IssueContent: types.IssueContent{Title: "Normal task (no defer)"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
	}
	for _, issue := range deferIssues {
		if err := s.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatal(err)
		}
	}

	// --- Unassigned-specific data ---
	unassignedIssues := []*types.Issue{
		{IssueID: types.IssueID{ID: "test-unassigned-1"}, IssueContent: types.IssueContent{Title: "Unassigned task 1"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask, Assignee: ""}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-unassigned-2"}, IssueContent: types.IssueContent{Title: "Unassigned task 2"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-assigned-alice"}, IssueContent: types.IssueContent{Title: "Alice's task 2"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask, Assignee: "alice"}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-assigned-bob"}, IssueContent: types.IssueContent{Title: "Bob's task 2"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask, Assignee: "bob"}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
	}
	for _, issue := range unassignedIssues {
		if err := s.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatal(err)
		}
	}

	// ========== Sub-tests ==========

	t.Run("ReadyWork", func(t *testing.T) {
		ready, err := s.GetReadyWork(ctx, types.WorkFilter{})
		if err != nil {
			t.Fatalf("GetReadyWork failed: %v", err)
		}

		readyIDs := make(map[string]bool)
		for _, issue := range ready {
			readyIDs[issue.ID] = true
		}

		// test-1, test-2, test-blocker should be in ready work
		for _, id := range []string{"test-1", "test-2", "test-blocker"} {
			if !readyIDs[id] {
				t.Errorf("Expected %s in ready work", id)
			}
		}

		// test-3 (blocked) and test-closed should NOT be in ready work
		if readyIDs["test-3"] {
			t.Error("test-3 should not be in ready work (it's blocked)")
		}
		if readyIDs["test-closed"] {
			t.Error("test-closed should not be in ready work (it's closed)")
		}

		// Priority filter
		priority1 := 1
		readyP1, err := s.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Priority: &priority1}})
		if err != nil {
			t.Fatalf("GetReadyWork with priority filter failed: %v", err)
		}
		for _, issue := range readyP1 {
			if issue.Priority != 1 {
				t.Errorf("Expected priority 1, got %d for issue %s", issue.Priority, issue.ID)
			}
		}

		// Limit
		readyLimited, err := s.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Limit: 1}})
		if err != nil {
			t.Fatalf("GetReadyWork with limit failed: %v", err)
		}
		if len(readyLimited) > 1 {
			t.Errorf("Expected at most 1 issue with limit=1, got %d", len(readyLimited))
		}
	})

	t.Run("ReadyWorkWithAssignee", func(t *testing.T) {
		alice := "alice"
		readyAlice, err := s.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Assignee: &alice}})
		if err != nil {
			t.Fatalf("GetReadyWork with assignee filter failed: %v", err)
		}

		// All returned issues should be assigned to alice
		for _, issue := range readyAlice {
			if issue.Assignee != "alice" {
				t.Errorf("Expected assignee='alice', got %q for %s", issue.Assignee, issue.ID)
			}
		}

		// Should include test-alice
		found := false
		for _, issue := range readyAlice {
			if issue.ID == "test-alice" {
				found = true
				break
			}
		}
		if !found {
			t.Error("Expected test-alice in assignee-filtered results")
		}
	})

	t.Run("ReadyWorkUnassignedFilter", func(t *testing.T) {
		readyUnassigned, err := s.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Unassigned: true}})
		if err != nil {
			t.Fatalf("GetReadyWork with unassigned filter failed: %v", err)
		}

		// All returned issues should have no assignee
		for _, issue := range readyUnassigned {
			if issue.Assignee != "" {
				t.Errorf("Expected empty assignee, got %q for issue %s", issue.Assignee, issue.ID)
			}
		}

		// Should include test-unassigned
		found := false
		for _, issue := range readyUnassigned {
			if issue.ID == "test-unassigned" {
				found = true
				break
			}
		}
		if !found {
			t.Error("Expected to find test-unassigned in unassigned results")
		}
	})

	t.Run("ReadyWorkInProgressWithEmptyFilter", func(t *testing.T) {
		ready, err := s.GetReadyWork(ctx, types.WorkFilter{})
		if err != nil {
			t.Fatalf("GetReadyWork failed: %v", err)
		}

		found := false
		for _, i := range ready {
			if i.ID == "test-wip" {
				found = true
				break
			}
		}
		if !found {
			t.Error("In-progress issue should appear when filter.Status is empty")
		}
	})

	t.Run("ReadyWorkExcludesInProgressWithOpenFilter", func(t *testing.T) {
		ready, err := s.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Status: "open"}})
		if err != nil {
			t.Fatalf("GetReadyWork with Status=open failed: %v", err)
		}

		for _, i := range ready {
			if i.ID == "test-wip" {
				t.Error("In-progress issue should NOT appear when filter.Status='open'")
			}
		}
	})

	t.Run("ReadyWorkIncludesIssuesWhoseBlockersAreClosed", func(t *testing.T) {
		ready, err := s.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Status: "open"}})
		if err != nil {
			t.Fatalf("GetReadyWork with Status=open failed: %v", err)
		}

		foundReadyViaClosed := false
		foundStillBlocked := false
		for _, issue := range ready {
			if issue.ID == "test-ready-via-closed-blockers" {
				foundReadyViaClosed = true
			}
			if issue.ID == "test-still-blocked" {
				foundStillBlocked = true
			}
		}

		if !foundReadyViaClosed {
			t.Error("Issue with only closed blockers should be in ready work")
		}
		if foundStillBlocked {
			t.Error("Issue with any open blocker should not be in ready work")
		}
	})

	// --- buildParentEpicMap tests (merged from TestBuildParentEpicMap) ---

	t.Run("BuildParentEpicMap_MapsChildToEpicParentOnly", func(t *testing.T) {
		issues := []*types.Issue{
			{IssueID: types.IssueID{ID: "test-child-1"}, IssueContent: types.IssueContent{Title: "Implement login"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}},
			{IssueID: types.IssueID{ID: "test-child-2"}, IssueContent: types.IssueContent{Title: "Subtask of non-epic"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}},
			{IssueID: types.IssueID{ID: "test-orphan"}, IssueContent: types.IssueContent{Title: "Standalone task"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 3, IssueType: types.TypeTask}},
		}
		result := buildParentEpicMap(ctx, s, issues)

		if result["test-child-1"] != "Auth Overhaul" {
			t.Errorf("Expected test-child-1 to map to 'Auth Overhaul', got %q", result["test-child-1"])
		}
		if _, ok := result["test-child-2"]; ok {
			t.Errorf("test-child-2 should not be in map (parent is not an epic), got %q", result["test-child-2"])
		}
		if _, ok := result["test-orphan"]; ok {
			t.Errorf("test-orphan should not be in map (no parent)")
		}
	})

	t.Run("BuildParentEpicMap_EmptyIssuesReturnsNil", func(t *testing.T) {
		result := buildParentEpicMap(ctx, s, nil)
		if result != nil {
			t.Errorf("Expected nil for empty issues, got %v", result)
		}
	})

	t.Run("BuildParentEpicMap_NoParentDepsReturnsNil", func(t *testing.T) {
		orphan := &types.Issue{IssueID: types.IssueID{ID: "test-orphan"}, IssueContent: types.IssueContent{Title: "Standalone task"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 3, IssueType: types.TypeTask}}
		result := buildParentEpicMap(ctx, s, []*types.Issue{orphan})
		if result != nil {
			t.Errorf("Expected nil when no parent deps exist, got %v", result)
		}
	})

	// --- Defer tests (merged from TestReadyWorkDeferUntil) ---

	t.Run("DeferUntil_ExcludesFutureDeferredByDefault", func(t *testing.T) {
		ready, err := s.GetReadyWork(ctx, types.WorkFilter{})
		if err != nil {
			t.Fatalf("GetReadyWork failed: %v", err)
		}

		for _, issue := range ready {
			if issue.ID == "test-future-defer" {
				t.Error("Future deferred issue should not appear in ready work by default")
			}
		}

		foundPast := false
		foundNoDefer := false
		for _, issue := range ready {
			if issue.ID == "test-past-defer" {
				foundPast = true
			}
			if issue.ID == "test-no-defer" {
				foundNoDefer = true
			}
		}
		if !foundPast {
			t.Error("Past deferred issue should appear in ready work")
		}
		if !foundNoDefer {
			t.Error("Issue without defer should appear in ready work")
		}
	})

	t.Run("DeferUntil_IncludeDeferredShowsAll", func(t *testing.T) {
		ready, err := s.GetReadyWork(ctx, types.WorkFilter{WorkFilterExtra: types.WorkFilterExtra{IncludeDeferred: true}})
		if err != nil {
			t.Fatalf("GetReadyWork with IncludeDeferred failed: %v", err)
		}

		foundFuture := false
		for _, issue := range ready {
			if issue.ID == "test-future-defer" {
				foundFuture = true
				break
			}
		}
		if !foundFuture {
			t.Error("Future deferred issue should appear when IncludeDeferred=true")
		}
	})

	// --- Unassigned tests (merged from TestReadyWorkUnassigned) ---

	t.Run("Unassigned_FiltersCorrectly", func(t *testing.T) {
		readyUnassigned, err := s.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Unassigned: true}})
		if err != nil {
			t.Fatalf("GetReadyWork with Unassigned filter failed: %v", err)
		}

		// All returned issues should have no assignee
		for _, issue := range readyUnassigned {
			if issue.Assignee != "" {
				t.Errorf("Expected no assignee, got %q for issue %s", issue.Assignee, issue.ID)
			}
		}

		// Should include test-unassigned-1 and test-unassigned-2
		unassignedIDs := make(map[string]bool)
		for _, issue := range readyUnassigned {
			unassignedIDs[issue.ID] = true
		}
		if !unassignedIDs["test-unassigned-1"] {
			t.Error("Expected test-unassigned-1 in unassigned results")
		}
		if !unassignedIDs["test-unassigned-2"] {
			t.Error("Expected test-unassigned-2 in unassigned results")
		}
	})

	t.Run("Unassigned_TakesPrecedenceOverAssignee", func(t *testing.T) {
		alice := "alice"
		readyConflict, err := s.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Unassigned: true, Assignee: &alice}})
		if err != nil {
			t.Fatalf("GetReadyWork with conflicting filters failed: %v", err)
		}

		// Unassigned should win, returning only unassigned issues
		for _, issue := range readyConflict {
			if issue.Assignee != "" {
				t.Errorf("Unassigned should override Assignee filter, got %q for issue %s", issue.Assignee, issue.ID)
			}
		}
	})
}

// TestReadyWorkIncludesMoleculeSteps verifies that molecule steps (issues with
// -mol- in their IDs) appear in GetReadyWork results when they have no active
// blockers. Regression test for GH#1359: the old SQLite backend filtered out
// issues whose IDs contained "-mol-" or "-wisp-", hiding poured molecule steps
// from `bd ready`. The Dolt backend should not have this filtering.
// TestGetReadyWork_ExcludeLabels verifies that WorkFilter.ExcludeLabels filters
// out issues that carry any of the specified labels.
func TestGetReadyWork_ExcludeLabels(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	s := newTestStore(t, filepath.Join(tmpDir, ".beads", "beads.db"))
	ctx := context.Background()

	issues := []*types.Issue{
		{IssueID: types.IssueID{ID: "excl-1"}, IssueContent: types.IssueContent{Title: "Normal task"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "excl-2"}, IssueContent: types.IssueContent{Title: "Triage pending task"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "excl-3"}, IssueContent: types.IssueContent{Title: "Wontfix task"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "excl-4"}, IssueContent: types.IssueContent{Title: "Tagged with multiple labels"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
	}
	for _, iss := range issues {
		if err := s.CreateIssue(ctx, iss, "test"); err != nil {
			t.Fatalf("CreateIssue %s: %v", iss.ID, err)
		}
	}
	if err := s.AddLabel(ctx, "excl-2", "triage:pending", "test"); err != nil {
		t.Fatalf("AddLabel excl-2: %v", err)
	}
	if err := s.AddLabel(ctx, "excl-3", "wontfix", "test"); err != nil {
		t.Fatalf("AddLabel excl-3: %v", err)
	}
	if err := s.AddLabel(ctx, "excl-4", "triage:pending", "test"); err != nil {
		t.Fatalf("AddLabel excl-4 triage:pending: %v", err)
	}
	if err := s.AddLabel(ctx, "excl-4", "backend", "test"); err != nil {
		t.Fatalf("AddLabel excl-4 backend: %v", err)
	}

	t.Run("ExcludeSingleLabel", func(t *testing.T) {
		results, err := s.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{ExcludeLabels: []string{"triage:pending"}}})
		if err != nil {
			t.Fatalf("GetReadyWork: %v", err)
		}
		ids := make(map[string]bool)
		for _, r := range results {
			ids[r.ID] = true
		}
		if ids["excl-2"] {
			t.Error("excl-2 (triage:pending) should be excluded")
		}
		if ids["excl-4"] {
			t.Error("excl-4 (has triage:pending) should be excluded")
		}
		if !ids["excl-1"] {
			t.Error("excl-1 (unlabelled) should be included")
		}
		if !ids["excl-3"] {
			t.Error("excl-3 (wontfix, not triage:pending) should be included")
		}
	})

	t.Run("ExcludeMultipleLabels", func(t *testing.T) {
		results, err := s.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{ExcludeLabels: []string{"triage:pending", "wontfix"}}})
		if err != nil {
			t.Fatalf("GetReadyWork: %v", err)
		}
		ids := make(map[string]bool)
		for _, r := range results {
			ids[r.ID] = true
		}
		if ids["excl-2"] {
			t.Error("excl-2 (triage:pending) should be excluded")
		}
		if ids["excl-3"] {
			t.Error("excl-3 (wontfix) should be excluded")
		}
		if ids["excl-4"] {
			t.Error("excl-4 (triage:pending) should be excluded")
		}
		if !ids["excl-1"] {
			t.Error("excl-1 (no excluded labels) should be included")
		}
	})
}

func TestReadyWorkIncludesMoleculeSteps(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	testDB := filepath.Join(tmpDir, ".beads", "beads.db")
	s := newTestStore(t, testDB)
	ctx := context.Background()

	// Simulate a poured molecule: root epic + 3 child tasks
	// brainstorm has no blockers (should be ready)
	// specify is blocked by brainstorm
	// hydrate is blocked by specify
	molIssues := []*types.Issue{
		{IssueID: types.IssueID{ID: "test-mol-root"}, IssueContent: types.IssueContent{Title: "feature-workflow"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: types.TypeEpic}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-mol-brainstorm"}, IssueContent: types.IssueContent{Title: "Brainstorm feature"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-mol-specify"}, IssueContent: types.IssueContent{Title: "Update specifications"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
		{IssueID: types.IssueID{ID: "test-mol-hydrate"}, IssueContent: types.IssueContent{Title: "Hydrate planning"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}, IssueTimes: types.IssueTimes{CreatedAt: time.Now()}},
	}
	for _, issue := range molIssues {
		if err := s.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatal(err)
		}
	}

	// Parent-child: all children belong to root epic
	parentChildDeps := []*types.Dependency{
		{IssueID: "test-mol-brainstorm", DependsOnID: "test-mol-root", Type: types.DepParentChild, CreatedAt: time.Now()},
		{IssueID: "test-mol-specify", DependsOnID: "test-mol-root", Type: types.DepParentChild, CreatedAt: time.Now()},
		{IssueID: "test-mol-hydrate", DependsOnID: "test-mol-root", Type: types.DepParentChild, CreatedAt: time.Now()},
	}
	for _, dep := range parentChildDeps {
		if err := s.AddDependency(ctx, dep, "test"); err != nil {
			t.Fatal(err)
		}
	}

	// Blocking: brainstorm blocks specify, specify blocks hydrate
	blockDeps := []*types.Dependency{
		{IssueID: "test-mol-specify", DependsOnID: "test-mol-brainstorm", Type: types.DepBlocks, CreatedAt: time.Now()},
		{IssueID: "test-mol-hydrate", DependsOnID: "test-mol-specify", Type: types.DepBlocks, CreatedAt: time.Now()},
	}
	for _, dep := range blockDeps {
		if err := s.AddDependency(ctx, dep, "test"); err != nil {
			t.Fatal(err)
		}
	}

	// GetReadyWork with Status="open" (same as bd ready CLI)
	ready, err := s.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Status: "open"}})
	if err != nil {
		t.Fatalf("GetReadyWork failed: %v", err)
	}

	readyIDs := make(map[string]bool)
	for _, issue := range ready {
		readyIDs[issue.ID] = true
	}

	// Root epic and brainstorm should be ready (no active blockers)
	if !readyIDs["test-mol-root"] {
		t.Error("Molecule root epic (test-mol-root) should appear in ready work")
	}
	if !readyIDs["test-mol-brainstorm"] {
		t.Error("Unblocked molecule step (test-mol-brainstorm) should appear in ready work")
	}

	// specify and hydrate should be blocked
	if readyIDs["test-mol-specify"] {
		t.Error("test-mol-specify should NOT be in ready work (blocked by brainstorm)")
	}
	if readyIDs["test-mol-hydrate"] {
		t.Error("test-mol-hydrate should NOT be in ready work (blocked by specify)")
	}
}

func TestReadyCommandInit(t *testing.T) {
	t.Parallel()
	if readyCmd == nil {
		t.Fatal("readyCmd should be initialized")
	}

	if readyCmd.Use != "ready" {
		t.Errorf("Expected Use='ready', got %q", readyCmd.Use)
	}

	if len(readyCmd.Short) == 0 {
		t.Error("readyCmd should have Short description")
	}

	// Verify --pretty defaults to true
	prettyFlag := readyCmd.Flags().Lookup("pretty")
	if prettyFlag == nil {
		t.Fatal("--pretty flag should exist")
	}
	if prettyFlag.DefValue != "true" {
		t.Errorf("--pretty default should be 'true', got %q", prettyFlag.DefValue)
	}

	// Verify --plain flag exists and defaults to false
	plainFlag := readyCmd.Flags().Lookup("plain")
	if plainFlag == nil {
		t.Fatal("--plain flag should exist")
	}
	if plainFlag.DefValue != "false" {
		t.Errorf("--plain default should be 'false', got %q", plainFlag.DefValue)
	}

	// Verify --sort defaults to "priority"
	sortFlag := readyCmd.Flags().Lookup("sort")
	if sortFlag == nil {
		t.Fatal("--sort flag should exist")
	}
	if sortFlag.DefValue != "priority" {
		t.Errorf("--sort default should be 'priority', got %q", sortFlag.DefValue)
	}

	// Verify --exclude-label flag exists and defaults to empty
	excludeLabelFlag := readyCmd.Flags().Lookup("exclude-label")
	if excludeLabelFlag == nil {
		t.Fatal("--exclude-label flag should exist")
	}
	if excludeLabelFlag.DefValue != "[]" {
		t.Errorf("--exclude-label default should be '[]', got %q", excludeLabelFlag.DefValue)
	}
}

func TestGetBlockedIssuesIncludesStoredBlocked(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	s := newTestStore(t, filepath.Join(tmpDir, ".beads", "beads.db"))
	ctx := context.Background()

	held := &types.Issue{
		IssueID: types.IssueID{
			ID: "qa-hold",
		},
		IssueContent: types.IssueContent{
			Title: "Parked",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusBlocked,
			Priority:  2,
			IssueType: types.TypeTask,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
		},
	}
	if err := s.CreateIssue(ctx, held, "test"); err != nil {
		t.Fatal(err)
	}

	blocked, err := s.GetBlockedIssues(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatalf("GetBlockedIssues: %v", err)
	}
	found := false
	for _, issue := range blocked {
		if issue.ID == "qa-hold" {
			found = true
			if issue.BlockedByCount != 0 {
				t.Errorf("stored-blocked with no deps has BlockedByCount=%d", issue.BlockedByCount)
			}
		}
	}
	if !found {
		t.Fatal("stored status=blocked issue must appear in GetBlockedIssues")
	}

	if err := s.ClaimIssue(ctx, "qa-hold", "agent"); err != nil {
		t.Fatalf("ClaimIssue on stored-blocked: %v", err)
	}
	got, err := s.GetIssue(ctx, "qa-hold")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.StatusInProgress {
		t.Fatalf("claim should resume stored-blocked, status=%s", got.Status)
	}
}

func TestClaimReadyIssuePrefersLeafOverEpic(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	s := newTestStore(t, filepath.Join(tmpDir, ".beads", "beads.db"))
	ctx := context.Background()

	epic := &types.Issue{
		IssueID: types.IssueID{
			ID: "qa-epic",
		},
		IssueContent: types.IssueContent{
			Title: "Parent epic",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeEpic,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
		},
	}
	child := &types.Issue{
		IssueID: types.IssueID{
			ID: "qa-epic.1",
		},
		IssueContent: types.IssueContent{
			Title: "Leaf work",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
		},
	}
	for _, issue := range []*types.Issue{epic, child} {
		if err := s.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddDependency(ctx, &types.Dependency{
		IssueID: "qa-epic.1", DependsOnID: "qa-epic", Type: types.DepParentChild, CreatedAt: time.Now(),
	}, "test"); err != nil {
		t.Fatal(err)
	}

	listed, err := s.GetReadyWork(ctx, types.WorkFilter{WorkFilterCore: types.WorkFilterCore{Status: types.StatusOpen}})
	if err != nil {
		t.Fatal(err)
	}
	listedIDs := map[string]bool{}
	for _, issue := range listed {
		listedIDs[issue.ID] = true
	}
	if !listedIDs["qa-epic"] {
		t.Fatal("bd ready listing should still include the parent epic")
	}
	if !listedIDs["qa-epic.1"] {
		t.Fatal("bd ready listing should include the leaf")
	}

	claimed, err := s.ClaimReadyIssue(ctx, types.WorkFilter{}, "agent")
	if err != nil {
		t.Fatalf("ClaimReadyIssue: %v", err)
	}
	if claimed == nil {
		t.Fatal("ClaimReadyIssue returned nil")
	}
	if claimed.ID == "qa-epic" {
		t.Fatal("ClaimReadyIssue took the epic instead of the leaf")
	}
	if claimed.ID != "qa-epic.1" {
		t.Fatalf("ClaimReadyIssue took %s, want qa-epic.1", claimed.ID)
	}
}
