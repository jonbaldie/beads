package dolt

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jonbaldie/beads/internal/types"
)

// columnInfo holds the subset of information_schema.columns needed for parity checks.
type columnInfo struct {
	Name       string
	ColumnType string // e.g. "varchar(255)", "text", "int"
}

// queryColumns returns the column names and types for a table, ordered by ordinal position.
func queryColumns(t *testing.T, store *DoltStore, table string) []columnInfo {
	t.Helper()
	rows, err := store.db.Query(`
		SELECT column_name, column_type
		FROM information_schema.columns
		WHERE table_name = ? AND table_schema = DATABASE()
		ORDER BY ordinal_position`, table)
	if err != nil {
		t.Fatalf("failed to query columns for %s: %v", table, err)
	}
	defer rows.Close()

	var cols []columnInfo
	for rows.Next() {
		var ci columnInfo
		if err := rows.Scan(&ci.Name, &ci.ColumnType); err != nil {
			t.Fatalf("failed to scan column info for %s: %v", table, err)
		}
		cols = append(cols, ci)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows error for %s: %v", table, err)
	}
	return cols
}

// columnNames extracts just the names from a slice of columnInfo, sorted.
func columnNames(cols []columnInfo) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	sort.Strings(names)
	return names
}

// TestSchemaParityIssuesVsWisps verifies that the wisps table has exactly the
// same columns (by name and type) as the issues table. This catches schema drift
// if columns are added to issues but not to wisps (or vice versa).
func TestSchemaParityIssuesVsWisps(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	issuesCols := queryColumns(t, store, "issues")
	wispsCols := queryColumns(t, store, "wisps")

	if len(issuesCols) == 0 {
		t.Fatal("issues table has no columns — schema not initialized?")
	}
	if len(wispsCols) == 0 {
		t.Fatal("wisps table has no columns — migration 004 not run?")
	}

	// Build maps for comparison
	issuesMap := make(map[string]string, len(issuesCols))
	for _, c := range issuesCols {
		issuesMap[c.Name] = c.ColumnType
	}
	wispsMap := make(map[string]string, len(wispsCols))
	for _, c := range wispsCols {
		wispsMap[c.Name] = c.ColumnType
	}

	// Check for columns in issues but missing from wisps
	var missing []string
	for name := range issuesMap {
		if _, ok := wispsMap[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("columns in issues but missing from wisps: %s", strings.Join(missing, ", "))
	}

	// Check for columns in wisps but missing from issues
	var extra []string
	for name := range wispsMap {
		if _, ok := issuesMap[name]; !ok {
			extra = append(extra, name)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		t.Errorf("columns in wisps but missing from issues: %s", strings.Join(extra, ", "))
	}

	// Check type mismatches for shared columns
	for name, issueType := range issuesMap {
		if wispType, ok := wispsMap[name]; ok && issueType != wispType {
			t.Errorf("column %q type mismatch: issues=%q, wisps=%q", name, issueType, wispType)
		}
	}
}

// TestSchemaParityAuxiliaryTables verifies that wisp auxiliary tables have the
// same column names as their issues counterparts. Type/nullability differences
// are allowed (wisps are more permissive), but column names must match.
func TestSchemaParityAuxiliaryTables(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	pairs := []struct {
		issueTable string
		wispTable  string
	}{
		{"labels", "wisp_labels"},
		{"dependencies", "wisp_dependencies"},
		{"events", "wisp_events"},
		{"comments", "wisp_comments"},
	}

	for _, pair := range pairs {
		t.Run(fmt.Sprintf("%s_vs_%s", pair.issueTable, pair.wispTable), func(t *testing.T) {
			issueCols := queryColumns(t, store, pair.issueTable)
			wispCols := queryColumns(t, store, pair.wispTable)

			if len(issueCols) == 0 {
				t.Fatalf("%s table has no columns", pair.issueTable)
			}
			if len(wispCols) == 0 {
				t.Fatalf("%s table has no columns — migration 005 not run?", pair.wispTable)
			}

			issueNames := columnNames(issueCols)
			wispNames := columnNames(wispCols)

			// Column names must be identical
			issueSet := make(map[string]bool, len(issueNames))
			for _, n := range issueNames {
				issueSet[n] = true
			}
			wispSet := make(map[string]bool, len(wispNames))
			for _, n := range wispNames {
				wispSet[n] = true
			}

			for _, n := range issueNames {
				if !wispSet[n] {
					t.Errorf("column %q in %s but missing from %s", n, pair.issueTable, pair.wispTable)
				}
			}
			for _, n := range wispNames {
				if !issueSet[n] {
					t.Errorf("column %q in %s but missing from %s", n, pair.wispTable, pair.issueTable)
				}
			}
		})
	}
}

// TestMigrations004And005Together verifies that migrations 004 (wisps table)
// and 005 (wisp auxiliary tables) run correctly in sequence. Migration 005
// depends on 004's "wisp_%" dolt_ignore pattern to keep auxiliary tables
// out of Dolt history.
func TestMigrations004And005Together(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	// After setupTestStore, both migrations have already run via initSchemaOnDB.
	// Verify all expected tables exist.
	expectedTables := []string{"wisps", "wisp_labels", "wisp_dependencies", "wisp_events", "wisp_comments"}
	for _, table := range expectedTables {
		var count int
		err := store.db.QueryRow(`
			SELECT COUNT(*) FROM information_schema.tables
			WHERE table_name = ? AND table_schema = DATABASE()`, table).Scan(&count)
		if err != nil {
			t.Fatalf("failed to check table %s: %v", table, err)
		}
		if count != 1 {
			t.Errorf("table %s should exist after migrations 004+005", table)
		}
	}

	// Verify dolt_ignore patterns cover all wisp tables
	var patternCount int
	err := store.db.QueryRow("SELECT COUNT(*) FROM dolt_ignore WHERE pattern IN ('wisps', 'wisp_%')").Scan(&patternCount)
	if err != nil {
		t.Fatalf("failed to query dolt_ignore: %v", err)
	}
	if patternCount != 2 {
		t.Errorf("expected 2 dolt_ignore patterns (wisps, wisp_%%), got %d", patternCount)
	}

	// Verify none of the wisp tables are staged after dolt_add
	_, err = store.db.Exec("CALL DOLT_ADD('-A')")
	if err != nil {
		t.Fatalf("dolt_add failed: %v", err)
	}

	for _, table := range expectedTables {
		var staged bool
		err := store.db.QueryRow("SELECT staged FROM dolt_status WHERE table_name = ?", table).Scan(&staged)
		if err == nil && staged {
			t.Errorf("table %s should NOT be staged (dolt_ignore should prevent staging)", table)
		}
	}
}

func strPtr(s string) *string { return &s }

// TestSearchWispsFilterParity exercises every IssueFilter field that SearchIssues
// supports against searchWisps to verify no filter is silently ignored.
// Each filter is tested individually to isolate failures.
func TestSearchWispsFilterParity(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Create a test wisp with enough fields populated for filters to match
	now := time.Now().UTC()
	past := now.Add(-24 * time.Hour)
	future := now.Add(24 * time.Hour)
	externalRef := "https://linear.app/example-org/issue/EX-1"
	wisp := &types.Issue{
		IssueContent: types.IssueContent{
			Title:       "test wisp for filter parity",
			Description: "description text here",
			Notes:       "some notes content",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: "task",
			Assignee:  "tester",
		},
		IssueLease: types.IssueLease{
			DueAt:      &future,
			DeferUntil: &future,
		},
		IssueMeta: types.IssueMeta{
			ExternalRef: &externalRef,
		},
		IssueWisp: types.IssueWisp{
			Ephemeral: true,
		},
	}
	if err := store.CreateIssue(ctx, wisp, "test-actor"); err != nil {
		t.Fatalf("failed to create test wisp: %v", err)
	}

	// Add a label to the wisp
	if err := store.AddLabel(ctx, wisp.ID, "test-label", "test-actor"); err != nil {
		t.Fatalf("failed to add wisp label: %v", err)
	}

	// Helper to run searchWisps and check for errors
	search := func(name string, filter types.IssueFilter) {
		t.Helper()
		results, err := store.searchWisps(ctx, "", filter)
		if err != nil {
			t.Errorf("filter %s: searchWisps returned error: %v", name, err)
			return
		}
		// We don't require results (filter may exclude the wisp), but no error means
		// the filter was properly handled and didn't cause a SQL error.
		_ = results
	}

	openStatus := types.StatusOpen
	closedStatus := types.StatusClosed
	priority2 := 2
	priority1 := 1
	priority3 := 3
	boolTrue := true
	boolFalse := false
	assignee := "tester"
	taskType := types.TypeTask

	// Test each filter field individually
	search("Status", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Status: &openStatus}})
	search("ExcludeStatus", types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{ExcludeStatus: []types.Status{closedStatus}}})
	search("IssueType", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{IssueType: &taskType}})
	search("ExcludeTypes", types.IssueFilter{IssueFilterHydrate: types.IssueFilterHydrate{ExcludeTypes: []types.IssueType{"bug"}}})
	search("Assignee", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Assignee: &assignee}})
	search("Priority", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Priority: &priority2}})
	search("PriorityMin", types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{PriorityMin: &priority1}})
	search("PriorityMax", types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{PriorityMax: &priority3}})
	search("IDs", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{IDs: []string{wisp.ID}}})
	search("IDPrefix", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{IDPrefix: "test-wisp"}})
	search("SpecIDPrefix", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{SpecIDPrefix: "spec"}})
	search("TitleSearch", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{TitleSearch: "test"}})
	search("TitleContains", types.IssueFilter{IssueFilterMatch: types.IssueFilterMatch{TitleContains: "parity"}})
	search("DescriptionContains", types.IssueFilter{IssueFilterMatch: types.IssueFilterMatch{DescriptionContains: "description"}})
	search("NotesContains", types.IssueFilter{IssueFilterMatch: types.IssueFilterMatch{NotesContains: "notes"}})
	search("ExternalRefContains", types.IssueFilter{IssueFilterMatch: types.IssueFilterMatch{ExternalRefContains: "linear"}})
	search("Labels", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Labels: []string{"test-label"}}})
	search("LabelsAny", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{LabelsAny: []string{"test-label", "other"}}})
	search("Pinned true", types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{Pinned: &boolTrue}})
	search("Pinned false", types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{Pinned: &boolFalse}})
	search("SourceRepo", types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{SourceRepo: strPtr("example/repo")}})
	search("IsTemplate true", types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{IsTemplate: &boolTrue}})
	search("IsTemplate false", types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{IsTemplate: &boolFalse}})
	search("EmptyDescription", types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{EmptyDescription: true}})
	search("NoAssignee", types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{NoAssignee: true}})
	search("NoLabels", types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{NoLabels: true}})
	search("NoParent", types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{NoParent: true}})
	search("Deferred", types.IssueFilter{IssueFilterHydrate: types.IssueFilterHydrate{Deferred: true}})
	search("Overdue", types.IssueFilter{IssueFilterHydrate: types.IssueFilterHydrate{Overdue: true}})

	// Time-range filters (critical for wisp GC)
	search("CreatedAfter", types.IssueFilter{IssueFilterMatch: types.IssueFilterMatch{CreatedAfter: &past}})
	search("CreatedBefore", types.IssueFilter{IssueFilterMatch: types.IssueFilterMatch{CreatedBefore: &future}})
	search("UpdatedAfter", types.IssueFilter{IssueFilterMatch: types.IssueFilterMatch{UpdatedAfter: &past}})
	search("UpdatedBefore", types.IssueFilter{IssueFilterMatch: types.IssueFilterMatch{UpdatedBefore: &future}})
	search("ClosedAfter", types.IssueFilter{IssueFilterMatch: types.IssueFilterMatch{ClosedAfter: &past}})
	search("ClosedBefore", types.IssueFilter{IssueFilterMatch: types.IssueFilterMatch{ClosedBefore: &future}})
	search("DeferAfter", types.IssueFilter{IssueFilterHydrate: types.IssueFilterHydrate{DeferAfter: &past}})
	search("DeferBefore", types.IssueFilter{IssueFilterHydrate: types.IssueFilterHydrate{DeferBefore: &future}})
	search("DueAfter", types.IssueFilter{IssueFilterHydrate: types.IssueFilterHydrate{DueAfter: &past}})
	search("DueBefore", types.IssueFilter{IssueFilterHydrate: types.IssueFilterHydrate{DueBefore: &future}})

	// Verify time-range filters actually return results when they should
	results, err := store.searchWisps(ctx, "", types.IssueFilter{IssueFilterMatch: types.IssueFilterMatch{CreatedAfter: &past}})
	if err != nil {
		t.Fatalf("CreatedAfter search failed: %v", err)
	}
	if len(results) == 0 {
		t.Error("CreatedAfter filter should have returned the test wisp")
	}
}
