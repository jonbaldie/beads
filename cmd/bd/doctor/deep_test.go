//go:build cgo

package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/types"
)

// TestRunDeepValidation_NoBeadsDir verifies deep validation handles missing .beads directory
func TestRunDeepValidation_NoBeadsDir(t *testing.T) {
	tmpDir := t.TempDir()
	result := RunDeepValidation(tmpDir)

	if len(result.AllChecks) != 1 {
		t.Errorf("Expected 1 check, got %d", len(result.AllChecks))
	}
	if result.AllChecks[0].Status != StatusOK {
		t.Errorf("Status = %q, want %q", result.AllChecks[0].Status, StatusOK)
	}
}

// TestRunDeepValidation_EmptyBeadsDir verifies deep validation with empty .beads directory
func TestRunDeepValidation_EmptyBeadsDir(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.Mkdir(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	result := RunDeepValidation(tmpDir)

	// Should return OK with "no database" message (no dolt/ directory)
	if len(result.AllChecks) != 1 {
		t.Errorf("Expected 1 check, got %d", len(result.AllChecks))
	}
	if result.AllChecks[0].Status != StatusOK {
		t.Errorf("Status = %q, want %q", result.AllChecks[0].Status, StatusOK)
	}
}

func TestRunDeepValidationSQLiteIsNotAMigrationWarning(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := (&configfile.Config{Backend: configfile.BackendSQLite, ConfigCompatibilityFields: configfile.ConfigCompatibilityFields{SQLitePath: "beads.db"}}).Save(beadsDir); err != nil {
		t.Fatalf("save SQLite config: %v", err)
	}

	result := RunDeepValidation(tmpDir)
	if len(result.AllChecks) != 1 {
		t.Fatalf("SQLite deep-validation result = %#v, want one N/A check", result)
	}
	check := result.AllChecks[0]
	if check.Status != StatusWarning || !strings.Contains(check.Message, "sqlite") || check.Fix != "" {
		t.Fatalf("SQLite deep-validation check = %#v, want non-Dolt N/A without migration fix", check)
	}
}

// TestCheckParentConsistency_OrphanedDeps verifies detection of orphaned parent-child deps
func TestCheckParentConsistency_OrphanedDeps(t *testing.T) {
	store := newTestDoltStore(t, "bd")
	ctx := context.Background()

	// Create an issue
	issue := &types.Issue{
		IssueID: types.IssueID{
			ID: "bd-1",
		},
		IssueContent: types.IssueContent{
			Title: "Test Issue",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			IssueType: types.TypeTask,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
		},
	}
	if err := store.CreateIssue(ctx, issue, "test"); err != nil {
		t.Fatal(err)
	}

	// Insert a parent-child dep pointing to non-existent parent via raw SQL.
	// FK on depends_on_issue_id would normally block this; disable checks to
	// simulate the schema-drift scenario the validator is designed to catch.
	db := store.UnderlyingDB()
	if _, err := db.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		t.Fatal(err)
	}
	_, err := db.ExecContext(ctx,
		"INSERT INTO dependencies (id, issue_id, depends_on_issue_id, type, created_at, created_by) VALUES (UUID(), ?, ?, ?, NOW(), ?)",
		"bd-1", "bd-missing", "parent-child", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 1"); err != nil {
		t.Fatal(err)
	}

	check := checkParentConsistency(db)

	if check.Status != StatusError {
		t.Errorf("Status = %q, want %q", check.Status, StatusError)
	}
}

// TestCheckEpicCompleteness_CompletedEpic verifies detection of closeable epics
func TestCheckEpicCompleteness_CompletedEpic(t *testing.T) {
	store := newTestDoltStore(t, "epic")
	ctx := context.Background()

	// Insert an open epic
	epic := &types.Issue{
		IssueID: types.IssueID{
			ID: "epic-1",
		},
		IssueContent: types.IssueContent{
			Title: "Epic",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			IssueType: types.TypeEpic,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
		},
	}
	if err := store.CreateIssue(ctx, epic, "test"); err != nil {
		t.Fatal(err)
	}

	// Insert a closed child task
	task := &types.Issue{
		IssueID: types.IssueID{
			ID: "epic-1.1",
		},
		IssueContent: types.IssueContent{
			Title: "Task",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusClosed,
			IssueType: types.TypeTask,
		},
		IssueTimes: types.IssueTimes{
			ClosedAt:  ptrTime(time.Now()),
			CreatedAt: time.Now(),
		},
	}
	if err := store.CreateIssue(ctx, task, "test"); err != nil {
		t.Fatal(err)
	}

	// Create parent-child relationship
	dep := &types.Dependency{
		IssueID:     "epic-1.1",
		DependsOnID: "epic-1",
		Type:        types.DepParentChild,
		CreatedAt:   time.Now(),
		CreatedBy:   "test",
	}
	if err := store.AddDependency(ctx, dep, "test"); err != nil {
		t.Fatal(err)
	}

	db := store.UnderlyingDB()
	check := checkEpicCompleteness(db)

	// Epic with all children closed should be detected
	if check.Status != StatusWarning {
		t.Errorf("Status = %q, want %q", check.Status, StatusWarning)
	}
}

func TestCheckEpicCompleteness_CountsWispChildren(t *testing.T) {
	store := newTestDoltStore(t, "epic")
	ctx := context.Background()

	epic := &types.Issue{
		IssueID: types.IssueID{
			ID: "epic-wisp",
		},
		IssueContent: types.IssueContent{
			Title: "Epic",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			IssueType: types.TypeEpic,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
		},
	}
	if err := store.CreateIssue(ctx, epic, "test"); err != nil {
		t.Fatal(err)
	}

	child := &types.Issue{
		IssueID: types.IssueID{
			ID: "epic-wisp.1",
		},
		IssueContent: types.IssueContent{
			Title: "Wisp child",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusClosed,
			IssueType: types.TypeTask,
		},
		IssueTimes: types.IssueTimes{
			ClosedAt:  ptrTime(time.Now()),
			CreatedAt: time.Now(),
		},
		IssueWisp: types.IssueWisp{
			NoHistory: true,
		},
	}
	if err := store.CreateIssue(ctx, child, "test"); err != nil {
		t.Fatal(err)
	}

	dep := &types.Dependency{
		IssueID:     child.ID,
		DependsOnID: epic.ID,
		Type:        types.DepParentChild,
		CreatedAt:   time.Now(),
		CreatedBy:   "test",
	}
	if err := store.AddDependency(ctx, dep, "test"); err != nil {
		t.Fatal(err)
	}

	check := checkEpicCompleteness(store.UnderlyingDB())
	if check.Status != StatusWarning {
		t.Errorf("Status = %q, want %q", check.Status, StatusWarning)
	}
}

func TestCheckEpicCompleteness_OpenWispChildPreventsCompletedEpic(t *testing.T) {
	store := newTestDoltStore(t, "epic")
	ctx := context.Background()

	epic := &types.Issue{
		IssueID: types.IssueID{
			ID: "epic-open-wisp",
		},
		IssueContent: types.IssueContent{
			Title: "Epic",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			IssueType: types.TypeEpic,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
		},
	}
	if err := store.CreateIssue(ctx, epic, "test"); err != nil {
		t.Fatal(err)
	}

	child := &types.Issue{
		IssueID: types.IssueID{
			ID: "epic-open-wisp.1",
		},
		IssueContent: types.IssueContent{
			Title: "Open wisp child",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			IssueType: types.TypeTask,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
		},
		IssueWisp: types.IssueWisp{
			NoHistory: true,
		},
	}
	if err := store.CreateIssue(ctx, child, "test"); err != nil {
		t.Fatal(err)
	}

	dep := &types.Dependency{
		IssueID:     child.ID,
		DependsOnID: epic.ID,
		Type:        types.DepParentChild,
		CreatedAt:   time.Now(),
		CreatedBy:   "test",
	}
	if err := store.AddDependency(ctx, dep, "test"); err != nil {
		t.Fatal(err)
	}

	check := checkEpicCompleteness(store.UnderlyingDB())
	if check.Status != StatusOK {
		t.Errorf("Status = %q, want %q; detail=%s", check.Status, StatusOK, check.Detail)
	}
}

// TestCheckMailThreadIntegrity_ValidThreads verifies valid thread references pass
func TestCheckMailThreadIntegrity_ValidThreads(t *testing.T) {
	store := newTestDoltStore(t, "thread")
	ctx := context.Background()

	// Insert issues
	root := &types.Issue{
		IssueID: types.IssueID{
			ID: "thread-root",
		},
		IssueContent: types.IssueContent{
			Title: "Thread Root",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			IssueType: types.TypeTask,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
		},
	}
	if err := store.CreateIssue(ctx, root, "test"); err != nil {
		t.Fatal(err)
	}

	reply := &types.Issue{
		IssueID: types.IssueID{
			ID: "thread-reply",
		},
		IssueContent: types.IssueContent{
			Title: "Reply",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			IssueType: types.TypeTask,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
		},
	}
	if err := store.CreateIssue(ctx, reply, "test"); err != nil {
		t.Fatal(err)
	}

	// Insert a dependency with valid thread_id via raw SQL (replies-to with thread_id)
	db := store.UnderlyingDB()
	_, err := db.ExecContext(ctx,
		"INSERT INTO dependencies (id, issue_id, depends_on_issue_id, type, thread_id, created_at, created_by) VALUES (UUID(), ?, ?, ?, ?, NOW(), ?)",
		"thread-reply", "thread-root", "replies-to", "thread-root", "test")
	if err != nil {
		t.Fatalf("Failed to insert thread dep: %v", err)
	}

	check := checkMailThreadIntegrity(db)

	// On Dolt/MySQL, pragma_table_info is not available, so the check
	// returns StatusOK with "N/A" message. This is expected behavior —
	// the check functions will be updated to use Dolt-compatible queries
	// in later subtasks (bd-o0u.2+).
	if check.Status != StatusOK {
		t.Errorf("Status = %q, want %q: %s", check.Status, StatusOK, check.Message)
	}
}

// TestDeepValidationResultJSON verifies JSON serialization
func TestDeepValidationResultJSON(t *testing.T) {
	result := DeepValidationResult{
		TotalIssues:       10,
		TotalDependencies: 5,
		OverallOK:         true,
		AllChecks: []DoctorCheck{
			{Name: "Test", Status: StatusOK, Message: "All good"},
		},
	}

	jsonBytes, err := DeepValidationResultJSON(result)
	if err != nil {
		t.Fatalf("Failed to serialize: %v", err)
	}

	if len(jsonBytes) == 0 {
		t.Error("Expected non-empty JSON output")
	}

	// Should contain expected fields
	jsonStr := string(jsonBytes)
	if !strings.Contains(jsonStr, "total_issues") {
		t.Error("JSON should contain total_issues")
	}
	if !strings.Contains(jsonStr, "overall_ok") {
		t.Error("JSON should contain overall_ok")
	}
}
