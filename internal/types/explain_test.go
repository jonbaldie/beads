package types

import (
	"testing"
)

func TestBuildReadyExplanation_NoIssues(t *testing.T) {
	result := BuildReadyExplanation(nil, nil, nil, nil, nil, nil)

	if len(result.Ready) != 0 {
		t.Errorf("expected 0 ready items, got %d", len(result.Ready))
	}
	if len(result.Blocked) != 0 {
		t.Errorf("expected 0 blocked items, got %d", len(result.Blocked))
	}
	if result.Summary.TotalReady != 0 {
		t.Errorf("expected TotalReady=0, got %d", result.Summary.TotalReady)
	}
	if result.Summary.TotalBlocked != 0 {
		t.Errorf("expected TotalBlocked=0, got %d", result.Summary.TotalBlocked)
	}
	if result.Summary.CycleCount != 0 {
		t.Errorf("expected CycleCount=0, got %d", result.Summary.CycleCount)
	}
}

func TestBuildReadyExplanation_ReadyWithNoDeps(t *testing.T) {
	issues := []*Issue{
		{IssueID: IssueID{ID: "bd-1"}, IssueContent: IssueContent{Title: "First"}, IssueWorkflow: IssueWorkflow{Priority: 1, Status: StatusOpen}},
		{IssueID: IssueID{ID: "bd-2"}, IssueContent: IssueContent{Title: "Second"}, IssueWorkflow: IssueWorkflow{Priority: 2, Status: StatusOpen}},
	}

	result := BuildReadyExplanation(issues, nil, nil, nil, nil, nil)

	if len(result.Ready) != 2 {
		t.Fatalf("expected 2 ready items, got %d", len(result.Ready))
	}
	if result.Ready[0].Reason != "no blocking dependencies" {
		t.Errorf("expected reason 'no blocking dependencies', got %q", result.Ready[0].Reason)
	}
	if result.Ready[0].DependencyCount != 0 {
		t.Errorf("expected DependencyCount=0, got %d", result.Ready[0].DependencyCount)
	}
	if result.Summary.TotalReady != 2 {
		t.Errorf("expected TotalReady=2, got %d", result.Summary.TotalReady)
	}
}

func TestBuildReadyExplanation_ReadyWithResolvedBlockers(t *testing.T) {
	issues := []*Issue{
		{IssueID: IssueID{ID: "bd-1"}, IssueContent: IssueContent{Title: "Unblocked"}, IssueWorkflow: IssueWorkflow{Priority: 1, Status: StatusOpen}},
	}

	depCounts := map[string]*DependencyCounts{
		"bd-1": {DependencyCount: 2, DependentCount: 1},
	}

	allDeps := map[string][]*Dependency{
		"bd-1": {
			{IssueID: "bd-1", DependsOnID: "bd-blocker-1", Type: DepBlocks},
			{IssueID: "bd-1", DependsOnID: "bd-blocker-2", Type: DepConditionalBlocks},
		},
	}

	result := BuildReadyExplanation(issues, nil, depCounts, allDeps, nil, nil)

	if len(result.Ready) != 1 {
		t.Fatalf("expected 1 ready item, got %d", len(result.Ready))
	}

	item := result.Ready[0]
	if item.Reason != "2 blocker(s) resolved" {
		t.Errorf("expected reason '2 blocker(s) resolved', got %q", item.Reason)
	}
	if len(item.ResolvedBlockers) != 2 {
		t.Errorf("expected 2 resolved blockers, got %d", len(item.ResolvedBlockers))
	}
	if item.DependencyCount != 2 {
		t.Errorf("expected DependencyCount=2, got %d", item.DependencyCount)
	}
	if item.DependentCount != 1 {
		t.Errorf("expected DependentCount=1, got %d", item.DependentCount)
	}
}

func TestBuildReadyExplanation_ReadyWithParent(t *testing.T) {
	issues := []*Issue{
		{IssueID: IssueID{ID: "bd-child"}, IssueContent: IssueContent{Title: "Child task"}, IssueWorkflow: IssueWorkflow{Priority: 2, Status: StatusOpen}},
	}

	allDeps := map[string][]*Dependency{
		"bd-child": {
			{IssueID: "bd-child", DependsOnID: "bd-epic", Type: DepParentChild},
		},
	}

	result := BuildReadyExplanation(issues, nil, nil, allDeps, nil, nil)

	if len(result.Ready) != 1 {
		t.Fatalf("expected 1 ready item, got %d", len(result.Ready))
	}
	if result.Ready[0].Parent == nil {
		t.Fatal("expected Parent to be set")
	}
	if *result.Ready[0].Parent != "bd-epic" {
		t.Errorf("expected Parent='bd-epic', got %q", *result.Ready[0].Parent)
	}
}

func TestBuildReadyExplanation_BlockedIssues(t *testing.T) {
	blockedIssues := []*BlockedIssue{
		{
			Issue:          Issue{IssueID: IssueID{ID: "bd-blocked"}, IssueContent: IssueContent{Title: "Stuck"}, IssueWorkflow: IssueWorkflow{Priority: 2, Status: StatusOpen}},
			BlockedByCount: 2,
			BlockedBy:      []string{"bd-blocker-1", "bd-blocker-2"},
		},
	}

	blockerMap := map[string]*Issue{
		"bd-blocker-1": {IssueID: IssueID{ID: "bd-blocker-1"}, IssueContent: IssueContent{Title: "Critical bug"}, IssueWorkflow: IssueWorkflow{Status: StatusOpen, Priority: 0}},
		"bd-blocker-2": {IssueID: IssueID{ID: "bd-blocker-2"}, IssueContent: IssueContent{Title: "Design review"}, IssueWorkflow: IssueWorkflow{Status: StatusInProgress, Priority: 1}},
	}

	result := BuildReadyExplanation(nil, blockedIssues, nil, nil, blockerMap, nil)

	if len(result.Blocked) != 1 {
		t.Fatalf("expected 1 blocked item, got %d", len(result.Blocked))
	}

	item := result.Blocked[0]
	if item.BlockedByCount != 2 {
		t.Errorf("expected BlockedByCount=2, got %d", item.BlockedByCount)
	}
	if len(item.BlockedBy) != 2 {
		t.Fatalf("expected 2 blockers, got %d", len(item.BlockedBy))
	}

	// Check blocker details were populated
	if item.BlockedBy[0].Title != "Critical bug" {
		t.Errorf("expected blocker title 'Critical bug', got %q", item.BlockedBy[0].Title)
	}
	if item.BlockedBy[0].Priority != 0 {
		t.Errorf("expected blocker priority 0, got %d", item.BlockedBy[0].Priority)
	}
	if item.BlockedBy[1].Status != StatusInProgress {
		t.Errorf("expected blocker status 'in_progress', got %q", item.BlockedBy[1].Status)
	}

	if result.Summary.TotalBlocked != 1 {
		t.Errorf("expected TotalBlocked=1, got %d", result.Summary.TotalBlocked)
	}
}

func TestBuildReadyExplanation_BlockedWithMissingBlocker(t *testing.T) {
	blockedIssues := []*BlockedIssue{
		{
			Issue:          Issue{IssueID: IssueID{ID: "bd-blocked"}, IssueContent: IssueContent{Title: "Stuck"}, IssueWorkflow: IssueWorkflow{Priority: 2, Status: StatusOpen}},
			BlockedByCount: 1,
			BlockedBy:      []string{"bd-unknown"},
		},
	}

	// Empty blocker map — blocker not found
	result := BuildReadyExplanation(nil, blockedIssues, nil, nil, nil, nil)

	if len(result.Blocked) != 1 {
		t.Fatalf("expected 1 blocked item, got %d", len(result.Blocked))
	}
	// Blocker info should have ID but no enriched fields
	if result.Blocked[0].BlockedBy[0].ID != "bd-unknown" {
		t.Errorf("expected blocker ID 'bd-unknown', got %q", result.Blocked[0].BlockedBy[0].ID)
	}
	if result.Blocked[0].BlockedBy[0].Title != "" {
		t.Errorf("expected empty title for missing blocker, got %q", result.Blocked[0].BlockedBy[0].Title)
	}
}

func TestBuildReadyExplanation_Cycles(t *testing.T) {
	cycles := [][]*Issue{
		{
			{IssueID: IssueID{ID: "bd-a"}, IssueContent: IssueContent{Title: "A"}},
			{IssueID: IssueID{ID: "bd-b"}, IssueContent: IssueContent{Title: "B"}},
			{IssueID: IssueID{ID: "bd-c"}, IssueContent: IssueContent{Title: "C"}},
		},
	}

	result := BuildReadyExplanation(nil, nil, nil, nil, nil, cycles)

	if len(result.Cycles) != 1 {
		t.Fatalf("expected 1 cycle, got %d", len(result.Cycles))
	}
	if len(result.Cycles[0]) != 3 {
		t.Fatalf("expected cycle of length 3, got %d", len(result.Cycles[0]))
	}
	if result.Cycles[0][0] != "bd-a" || result.Cycles[0][1] != "bd-b" || result.Cycles[0][2] != "bd-c" {
		t.Errorf("unexpected cycle IDs: %v", result.Cycles[0])
	}
	if result.Summary.CycleCount != 1 {
		t.Errorf("expected CycleCount=1, got %d", result.Summary.CycleCount)
	}
}

func TestBuildReadyExplanation_FullScenario(t *testing.T) {
	readyIssues := []*Issue{
		{IssueID: IssueID{ID: "bd-ready1"}, IssueContent: IssueContent{Title: "Ready task"}, IssueWorkflow: IssueWorkflow{Priority: 1, Status: StatusOpen}},
	}
	blockedIssues := []*BlockedIssue{
		{
			Issue:          Issue{IssueID: IssueID{ID: "bd-stuck"}, IssueContent: IssueContent{Title: "Blocked task"}, IssueWorkflow: IssueWorkflow{Priority: 2, Status: StatusOpen}},
			BlockedByCount: 1,
			BlockedBy:      []string{"bd-ready1"},
		},
	}
	depCounts := map[string]*DependencyCounts{
		"bd-ready1": {DependencyCount: 0, DependentCount: 1},
	}
	blockerMap := map[string]*Issue{
		"bd-ready1": readyIssues[0],
	}
	cycles := [][]*Issue{
		{{IssueID: IssueID{ID: "bd-x"}}, {IssueID: IssueID{ID: "bd-y"}}},
	}

	result := BuildReadyExplanation(readyIssues, blockedIssues, depCounts, nil, blockerMap, cycles)

	if result.Summary.TotalReady != 1 {
		t.Errorf("TotalReady=%d, want 1", result.Summary.TotalReady)
	}
	if result.Summary.TotalBlocked != 1 {
		t.Errorf("TotalBlocked=%d, want 1", result.Summary.TotalBlocked)
	}
	if result.Summary.CycleCount != 1 {
		t.Errorf("CycleCount=%d, want 1", result.Summary.CycleCount)
	}
	if result.Ready[0].DependentCount != 1 {
		t.Errorf("Ready item DependentCount=%d, want 1", result.Ready[0].DependentCount)
	}
	if result.Blocked[0].BlockedBy[0].Title != "Ready task" {
		t.Errorf("Blocked item blocker title=%q, want 'Ready task'", result.Blocked[0].BlockedBy[0].Title)
	}
}
