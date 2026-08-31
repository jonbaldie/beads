//go:build cgo

package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jonbaldie/beads/internal/types"
)

func TestGetReadyWork_MetadataSuite(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	store := newTestStore(t, tmpDir)
	ctx := context.Background()

	// Create all test data up front with unique metadata keys per subtest.
	allIssues := []*types.Issue{
		// --- FieldMatch data ---
		{
			IssueID: types.IssueID{
				ID: "mr-fm-1",
			},
			IssueContent: types.IssueContent{
				Title: "Platform task (fm)",
			},
			IssueWorkflow: types.IssueWorkflow{
				Priority:  2,
				IssueType: types.TypeTask,
				Status:    types.StatusOpen,
			},
			IssueMeta: types.IssueMeta{
				Metadata: json.RawMessage(`{"mr_fm_team":"platform"}`),
			},
		},
		{
			IssueID: types.IssueID{
				ID: "mr-fm-2",
			},
			IssueContent: types.IssueContent{
				Title: "Frontend task (fm)",
			},
			IssueWorkflow: types.IssueWorkflow{
				Priority:  2,
				IssueType: types.TypeTask,
				Status:    types.StatusOpen,
			},
			IssueMeta: types.IssueMeta{
				Metadata: json.RawMessage(`{"mr_fm_team":"frontend"}`),
			},
		},
		// --- HasMetadataKey data ---
		{
			IssueID: types.IssueID{
				ID: "mr-hmk-1",
			},
			IssueContent: types.IssueContent{
				Title: "Has team (hmk)",
			},
			IssueWorkflow: types.IssueWorkflow{
				Priority:  2,
				IssueType: types.TypeTask,
				Status:    types.StatusOpen,
			},
			IssueMeta: types.IssueMeta{
				Metadata: json.RawMessage(`{"mr_hmk_team":"platform"}`),
			},
		},
		{IssueID: types.IssueID{ID: "mr-hmk-2"}, IssueContent: types.IssueContent{Title: "No metadata (hmk)"}, IssueWorkflow: types.IssueWorkflow{Priority: 2, IssueType: types.TypeTask, Status: types.StatusOpen}},
		// --- NoMatch data ---
		{
			IssueID: types.IssueID{
				ID: "mr-nm-1",
			},
			IssueContent: types.IssueContent{
				Title: "Platform task (nm)",
			},
			IssueWorkflow: types.IssueWorkflow{
				Priority:  2,
				IssueType: types.TypeTask,
				Status:    types.StatusOpen,
			},
			IssueMeta: types.IssueMeta{
				Metadata: json.RawMessage(`{"mr_nm_team":"platform"}`),
			},
		},
	}
	for _, issue := range allIssues {
		if err := store.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatalf("CreateIssue(%s): %v", issue.ID, err)
		}
	}

	t.Run("FieldMatch", func(t *testing.T) {
		results, err := store.GetReadyWork(ctx, types.WorkFilter{
			WorkFilterCore: types.WorkFilterCore{
				Status: "open",
			},
			WorkFilterExtra: types.WorkFilterExtra{
				MetadataFields: map[string]string{"mr_fm_team": "platform"},
			},
		})
		if err != nil {
			t.Fatalf("GetReadyWork: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("expected 1 result, got %d", len(results))
		}
		if results[0].ID != "mr-fm-1" {
			t.Errorf("expected issue mr-fm-1, got %s", results[0].ID)
		}
	})

	t.Run("HasMetadataKey", func(t *testing.T) {
		results, err := store.GetReadyWork(ctx, types.WorkFilter{
			WorkFilterCore: types.WorkFilterCore{
				Status: "open",
			},
			WorkFilterExtra: types.WorkFilterExtra{
				HasMetadataKey: "mr_hmk_team",
			},
		})
		if err != nil {
			t.Fatalf("GetReadyWork: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("expected 1 result, got %d", len(results))
		}
		if results[0].ID != "mr-hmk-1" {
			t.Errorf("expected issue mr-hmk-1, got %s", results[0].ID)
		}
	})

	t.Run("FieldNoMatch", func(t *testing.T) {
		results, err := store.GetReadyWork(ctx, types.WorkFilter{
			WorkFilterCore: types.WorkFilterCore{
				Status: "open",
			},
			WorkFilterExtra: types.WorkFilterExtra{
				MetadataFields: map[string]string{"mr_nm_team": "backend"},
			},
		})
		if err != nil {
			t.Fatalf("GetReadyWork: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("expected 0 results, got %d", len(results))
		}
	})

	t.Run("FieldInvalidKey", func(t *testing.T) {
		_, err := store.GetReadyWork(ctx, types.WorkFilter{
			WorkFilterCore: types.WorkFilterCore{
				Status: "open",
			},
			WorkFilterExtra: types.WorkFilterExtra{
				MetadataFields: map[string]string{"'; DROP TABLE issues; --": "val"},
			},
		})
		if err == nil {
			t.Fatal("expected error for invalid metadata key, got nil")
		}
	})
}

func TestGetReadyWork_IncludeEphemeralAssigneeIsSuperset(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	store := newTestStore(t, tmpDir)
	ctx := context.Background()
	worker := "control-dispatcher"

	issues := []*types.Issue{
		{
			IssueID: types.IssueID{
				ID: "mr-assignee-persistent",
			},
			IssueContent: types.IssueContent{
				Title: "Persistent assigned task",
			},
			IssueWorkflow: types.IssueWorkflow{
				Priority:  2,
				IssueType: types.TypeTask,
				Status:    types.StatusOpen,
				Assignee:  worker,
			},
		},
		{
			IssueID: types.IssueID{
				ID: "mr-assignee-no-history",
			},
			IssueContent: types.IssueContent{
				Title: "No-history assigned task",
			},
			IssueWorkflow: types.IssueWorkflow{
				Priority:  2,
				IssueType: types.TypeTask,
				Status:    types.StatusOpen,
				Assignee:  worker,
			},
			IssueWisp: types.IssueWisp{
				NoHistory: true,
			},
		},
		{
			IssueID: types.IssueID{
				ID: "mr-assignee-ephemeral",
			},
			IssueContent: types.IssueContent{
				Title: "Ephemeral assigned task",
			},
			IssueWorkflow: types.IssueWorkflow{
				Priority:  2,
				IssueType: types.TypeTask,
				Status:    types.StatusOpen,
				Assignee:  worker,
			},
			IssueWisp: types.IssueWisp{
				Ephemeral: true,
			},
		},
		{
			IssueID: types.IssueID{
				ID: "mr-assignee-other",
			},
			IssueContent: types.IssueContent{
				Title: "Other assigned task",
			},
			IssueWorkflow: types.IssueWorkflow{
				Priority:  2,
				IssueType: types.TypeTask,
				Status:    types.StatusOpen,
				Assignee:  "someone-else",
			},
			IssueWisp: types.IssueWisp{
				Ephemeral: true,
			},
		},
	}
	for _, issue := range issues {
		if err := store.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatalf("CreateIssue(%s): %v", issue.ID, err)
		}
	}

	defaultResults, err := store.GetReadyWork(ctx, types.WorkFilter{
		WorkFilterCore: types.WorkFilterCore{
			Status:   types.StatusOpen,
			Assignee: &worker,
		},
	})
	if err != nil {
		t.Fatalf("GetReadyWork default: %v", err)
	}
	if got := issueIDs(defaultResults); !sameStringSet(got, []string{"mr-assignee-persistent", "mr-assignee-no-history"}) {
		t.Fatalf("default ready IDs = %v, want persistent and no-history only", got)
	}

	allResults, err := store.GetReadyWork(ctx, types.WorkFilter{
		WorkFilterCore: types.WorkFilterCore{
			Status:   types.StatusOpen,
			Assignee: &worker,
		},
		WorkFilterExtra: types.WorkFilterExtra{
			IncludeEphemeral: true,
		},
	})
	if err != nil {
		t.Fatalf("GetReadyWork include ephemeral: %v", err)
	}
	if got := issueIDs(allResults); !sameStringSet(got, []string{"mr-assignee-persistent", "mr-assignee-no-history", "mr-assignee-ephemeral"}) {
		t.Fatalf("include-ephemeral ready IDs = %v, want persistent, no-history, and ephemeral for assignee", got)
	}
}

func sameStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	counts := make(map[string]int, len(want))
	for _, v := range want {
		counts[v]++
	}
	for _, v := range got {
		counts[v]--
		if counts[v] < 0 {
			return false
		}
	}
	return true
}
