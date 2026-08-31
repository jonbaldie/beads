package dolt

import (
	"strings"
	"testing"

	"github.com/jonbaldie/beads/internal/types"
)

// TestCreateWispValidation verifies that CreateIssue enforces field
// validation for ephemeral issues (wisps) (validation parity, GH#2031).
func TestCreateWispValidation(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	tests := []struct {
		name    string
		issue   *types.Issue
		wantErr string // substring expected in error; empty means success
	}{
		{
			name: "valid wisp creates successfully",
			issue: &types.Issue{
				IssueContent: types.IssueContent{
					Title: "a valid wisp",
				},
				IssueWorkflow: types.IssueWorkflow{
					Status:    types.StatusOpen,
					Priority:  2,
					IssueType: types.TypeTask,
				},
				IssueWisp: types.IssueWisp{
					Ephemeral: true,
				},
			},
		},
		{
			name: "empty title rejected",
			issue: &types.Issue{
				IssueContent: types.IssueContent{
					Title: "",
				},
				IssueWorkflow: types.IssueWorkflow{
					Status:    types.StatusOpen,
					Priority:  2,
					IssueType: types.TypeTask,
				},
				IssueWisp: types.IssueWisp{
					Ephemeral: true,
				},
			},
			wantErr: "title is required",
		},
		{
			name: "invalid status rejected",
			issue: &types.Issue{
				IssueContent: types.IssueContent{
					Title: "bad status wisp",
				},
				IssueWorkflow: types.IssueWorkflow{
					Status:    types.Status("bogus_status"),
					Priority:  2,
					IssueType: types.TypeTask,
				},
				IssueWisp: types.IssueWisp{
					Ephemeral: true,
				},
			},
			wantErr: "invalid status",
		},
		{
			name: "invalid type rejected",
			issue: &types.Issue{
				IssueContent: types.IssueContent{
					Title: "bad type wisp",
				},
				IssueWorkflow: types.IssueWorkflow{
					Status:    types.StatusOpen,
					Priority:  2,
					IssueType: types.IssueType("nonexistent_type"),
				},
				IssueWisp: types.IssueWisp{
					Ephemeral: true,
				},
			},
			wantErr: "invalid issue type",
		},
		{
			name: "event type accepted without custom config",
			issue: &types.Issue{
				IssueContent: types.IssueContent{
					Title: "wisp event",
				},
				IssueWorkflow: types.IssueWorkflow{
					Status:    types.StatusOpen,
					Priority:  4,
					IssueType: types.TypeEvent,
				},
				IssueWisp: types.IssueWisp{
					Ephemeral: true,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := store.CreateIssue(ctx, tt.issue, "test-user")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got error: %v", err)
				}
				// Verify round-trip: wisp is retrievable
				got, err := store.GetIssue(ctx, tt.issue.ID)
				if err != nil {
					t.Fatalf("GetIssue failed for created wisp: %v", err)
				}
				if got.Title != tt.issue.Title {
					t.Errorf("title mismatch: got %q, want %q", got.Title, tt.issue.Title)
				}
			} else {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}
