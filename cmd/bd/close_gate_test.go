package main

import (
	"strings"
	"testing"

	"github.com/jonbaldie/beads/internal/types"
)

func TestIsMachineCheckableGate(t *testing.T) {
	tests := []struct {
		name  string
		issue *types.Issue
		want  bool
	}{
		{
			name:  "nil issue",
			issue: nil,
			want:  false,
		},
		{
			name: "non-gate issue",
			issue: &types.Issue{
				IssueWorkflow: types.IssueWorkflow{
					IssueType: "task",
				},
			},
			want: false,
		},
		{
			name: "gate with human await type",
			issue: &types.Issue{
				IssueWorkflow: types.IssueWorkflow{
					IssueType: "gate",
				},
				IssueCoord: types.IssueCoord{
					AwaitType: "human",
				},
			},
			want: false,
		},
		{
			name: "gate with gh:pr await type",
			issue: &types.Issue{
				IssueWorkflow: types.IssueWorkflow{
					IssueType: "gate",
				},
				IssueCoord: types.IssueCoord{
					AwaitType: "gh:pr",
				},
			},
			want: true,
		},
		{
			name: "gate with gh:run await type",
			issue: &types.Issue{
				IssueWorkflow: types.IssueWorkflow{
					IssueType: "gate",
				},
				IssueCoord: types.IssueCoord{
					AwaitType: "gh:run",
				},
			},
			want: true,
		},
		{
			name: "gate with timer await type",
			issue: &types.Issue{
				IssueWorkflow: types.IssueWorkflow{
					IssueType: "gate",
				},
				IssueCoord: types.IssueCoord{
					AwaitType: "timer",
				},
			},
			want: true,
		},
		{
			name: "gate with bead await type",
			issue: &types.Issue{
				IssueWorkflow: types.IssueWorkflow{
					IssueType: "gate",
				},
				IssueCoord: types.IssueCoord{
					AwaitType: "bead",
				},
			},
			want: true,
		},
		{
			name: "gate with empty await type",
			issue: &types.Issue{
				IssueWorkflow: types.IssueWorkflow{
					IssueType: "gate",
				},
				IssueCoord: types.IssueCoord{
					AwaitType: "",
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isMachineCheckableGate(tt.issue)
			if got != tt.want {
				t.Errorf("isMachineCheckableGate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckGateSatisfaction_NonGateIssues(t *testing.T) {
	// Non-gate issues should always pass (return nil)
	tests := []struct {
		name  string
		issue *types.Issue
	}{
		{
			name:  "nil issue",
			issue: nil,
		},
		{
			name: "task issue",
			issue: &types.Issue{
				IssueContent: types.IssueContent{
					Title: "Regular task",
				},
				IssueWorkflow: types.IssueWorkflow{
					IssueType: "task",
				},
			},
		},
		{
			name: "bug issue",
			issue: &types.Issue{
				IssueContent: types.IssueContent{
					Title: "A bug",
				},
				IssueWorkflow: types.IssueWorkflow{
					IssueType: "bug",
				},
			},
		},
		{
			name: "gate with human await (not machine-checkable)",
			issue: &types.Issue{
				IssueContent: types.IssueContent{
					Title: "Human gate",
				},
				IssueWorkflow: types.IssueWorkflow{
					IssueType: "gate",
				},
				IssueCoord: types.IssueCoord{
					AwaitType: "human",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkGateSatisfaction(tt.issue)
			if err != nil {
				t.Errorf("checkGateSatisfaction() returned error for non-machine-checkable issue: %v", err)
			}
		})
	}
}

func TestCheckGateSatisfaction_GHPRWithoutAwaitID(t *testing.T) {
	// gh:pr gate without an await_id is unsatisfied (no PR to check)
	issue := &types.Issue{
		IssueContent: types.IssueContent{
			Title: "PR gate without ID",
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: "gate",
		},
		IssueCoord: types.IssueCoord{
			AwaitType: "gh:pr",
			AwaitID:   "",
		},
	}

	err := checkGateSatisfaction(issue)
	if err == nil {
		t.Error("checkGateSatisfaction() should return error for gh:pr gate without await_id")
	}
	if err != nil && !strings.Contains(err.Error(), "no PR number") {
		t.Errorf("error should mention 'no PR number', got: %v", err)
	}
}

func TestCheckGateSatisfaction_GHRunWithoutAwaitID(t *testing.T) {
	// gh:run gate without an await_id is unsatisfied (no run to check)
	issue := &types.Issue{
		IssueContent: types.IssueContent{
			Title: "Run gate without ID",
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: "gate",
		},
		IssueCoord: types.IssueCoord{
			AwaitType: "gh:run",
			AwaitID:   "",
		},
	}

	err := checkGateSatisfaction(issue)
	if err == nil {
		t.Error("checkGateSatisfaction() should return error for gh:run gate without await_id")
	}
	if err != nil && !strings.Contains(err.Error(), "no run ID") {
		t.Errorf("error should mention 'no run ID', got: %v", err)
	}
}

func TestCheckGateSatisfaction_BeadGateInvalidFormat(t *testing.T) {
	// bead gate with invalid await_id should return an error
	issue := &types.Issue{
		IssueContent: types.IssueContent{
			Title: "Bead gate with bad format",
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: "gate",
		},
		IssueCoord: types.IssueCoord{
			AwaitType: "bead",
			AwaitID:   "invalid-no-colon",
		},
	}

	err := checkGateSatisfaction(issue)
	if err == nil {
		t.Error("checkGateSatisfaction() should return error for bead gate with invalid await_id format")
	}
}

func TestCheckGateSatisfaction_ErrorMessageFormat(t *testing.T) {
	// Verify error messages contain the force override hint
	issue := &types.Issue{
		IssueContent: types.IssueContent{
			Title: "Test gate",
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: "gate",
		},
		IssueCoord: types.IssueCoord{
			AwaitType: "bead",
			AwaitID:   "invalid-no-colon",
		},
	}

	err := checkGateSatisfaction(issue)
	if err == nil {
		t.Fatal("expected error")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "--force") {
		t.Errorf("error message should mention --force, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "gate condition not satisfied") {
		t.Errorf("error message should mention 'gate condition not satisfied', got: %s", errMsg)
	}
}
