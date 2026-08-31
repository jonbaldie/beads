//go:build cgo

package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonbaldie/beads/internal/types"
)

// TestCreateWithNotes verifies that the --notes flag works correctly
// during issue creation in both direct mode and RPC mode.
func TestCreateWithNotes(t *testing.T) {
	tmpDir := t.TempDir()
	testDB := filepath.Join(tmpDir, ".beads", "beads.db")
	s := newTestStore(t, testDB)
	ctx := context.Background()

	t.Run("DirectMode_WithNotes", func(t *testing.T) {
		issue := &types.Issue{
			IssueContent: types.IssueContent{
				Title: "Issue with notes",
				Notes: "These are my test notes",
			},
			IssueWorkflow: types.IssueWorkflow{
				Priority:  1,
				IssueType: types.TypeTask,
				Status:    types.StatusOpen,
			},
			IssueTimes: types.IssueTimes{
				CreatedAt: time.Now(),
			},
		}

		if err := s.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}

		// Retrieve and verify
		retrieved, err := s.GetIssue(ctx, issue.ID)
		if err != nil {
			t.Fatalf("failed to retrieve issue: %v", err)
		}

		if retrieved.Notes != "These are my test notes" {
			t.Errorf("expected notes 'These are my test notes', got %q", retrieved.Notes)
		}
	})

	t.Run("DirectMode_WithoutNotes", func(t *testing.T) {
		issue := &types.Issue{
			IssueContent: types.IssueContent{
				Title: "Issue without notes",
			},
			IssueWorkflow: types.IssueWorkflow{
				Priority:  2,
				IssueType: types.TypeBug,
				Status:    types.StatusOpen,
			},
			IssueTimes: types.IssueTimes{
				CreatedAt: time.Now(),
			},
		}

		if err := s.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}

		// Retrieve and verify
		retrieved, err := s.GetIssue(ctx, issue.ID)
		if err != nil {
			t.Fatalf("failed to retrieve issue: %v", err)
		}

		if retrieved.Notes != "" {
			t.Errorf("expected empty notes, got %q", retrieved.Notes)
		}
	})

	t.Run("DirectMode_WithNotesAndOtherFields", func(t *testing.T) {
		issue := &types.Issue{
			IssueContent: types.IssueContent{
				Title:              "Full issue with notes",
				Description:        "Detailed description",
				Design:             "Design notes here",
				AcceptanceCriteria: "All tests pass",
				Notes:              "Additional implementation notes",
			},
			IssueWorkflow: types.IssueWorkflow{
				Priority:  1,
				IssueType: types.TypeFeature,
				Status:    types.StatusOpen,
				Assignee:  "testuser",
			},
			IssueTimes: types.IssueTimes{
				CreatedAt: time.Now(),
			},
		}

		if err := s.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}

		// Retrieve and verify all fields
		retrieved, err := s.GetIssue(ctx, issue.ID)
		if err != nil {
			t.Fatalf("failed to retrieve issue: %v", err)
		}

		if retrieved.Title != "Full issue with notes" {
			t.Errorf("expected title 'Full issue with notes', got %q", retrieved.Title)
		}
		if retrieved.Description != "Detailed description" {
			t.Errorf("expected description, got %q", retrieved.Description)
		}
		if retrieved.Design != "Design notes here" {
			t.Errorf("expected design, got %q", retrieved.Design)
		}
		if retrieved.AcceptanceCriteria != "All tests pass" {
			t.Errorf("expected acceptance criteria, got %q", retrieved.AcceptanceCriteria)
		}
		if retrieved.Notes != "Additional implementation notes" {
			t.Errorf("expected notes 'Additional implementation notes', got %q", retrieved.Notes)
		}
		if retrieved.Assignee != "testuser" {
			t.Errorf("expected assignee 'testuser', got %q", retrieved.Assignee)
		}
	})

	t.Run("DirectMode_NotesWithSpecialCharacters", func(t *testing.T) {
		specialNotes := "Notes with special chars: \n- Bullet point\n- Another one\n\nAnd \"quotes\" and 'apostrophes'"
		issue := &types.Issue{
			IssueContent: types.IssueContent{
				Title: "Issue with special char notes",
				Notes: specialNotes,
			},
			IssueWorkflow: types.IssueWorkflow{
				Priority:  2,
				IssueType: types.TypeTask,
				Status:    types.StatusOpen,
			},
			IssueTimes: types.IssueTimes{
				CreatedAt: time.Now(),
			},
		}

		if err := s.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatalf("failed to create issue: %v", err)
		}

		// Retrieve and verify
		retrieved, err := s.GetIssue(ctx, issue.ID)
		if err != nil {
			t.Fatalf("failed to retrieve issue: %v", err)
		}

		if retrieved.Notes != specialNotes {
			t.Errorf("notes mismatch.\nExpected: %q\nGot: %q", specialNotes, retrieved.Notes)
		}
	})
}
