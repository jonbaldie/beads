package notion

import (
	"reflect"
	"testing"
	"time"

	itracker "github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
)

func TestNotionPushWorkerCountBoundaries(t *testing.T) {
	for _, tc := range []struct{ count, want int }{{-1, 1}, {0, 1}, {1, 1}, {7, 7}, {8, 8}, {9, 8}} {
		if got := notionPushWorkerCount(tc.count); got != tc.want {
			t.Fatalf("notionPushWorkerCount(%d) = %d, want %d", tc.count, got, tc.want)
		}
	}
}

func TestTrackerIssueEqualityExact(t *testing.T) {
	local := &types.Issue{
		IssueContent:  types.IssueContent{Title: " title ", Description: " description "},
		IssueWorkflow: types.IssueWorkflow{Priority: 1, Status: types.StatusOpen, IssueType: types.TypeTask, Assignee: " person "},
		IssueGraph:    types.IssueGraph{Labels: []string{" beta ", "alpha"}},
	}
	remote := &itracker.TrackerIssue{
		Title: "title", Description: "description", Priority: 1,
		State: types.StatusOpen, Type: types.TypeTask, Assignee: "person",
		Labels: []string{"alpha", "beta"},
	}
	if !trackerIssueEqual(local, remote) {
		t.Fatal("equivalent issues were not equal")
	}
	if trackerIssueEqual(nil, remote) || trackerIssueEqual(local, nil) {
		t.Fatal("nil issue compared equal")
	}

	tests := []struct {
		name   string
		mutate func(*itracker.TrackerIssue)
	}{
		{"title", func(i *itracker.TrackerIssue) { i.Title = "different" }},
		{"description", func(i *itracker.TrackerIssue) { i.Description = "different" }},
		{"priority", func(i *itracker.TrackerIssue) { i.Priority = 2 }},
		{"state value", func(i *itracker.TrackerIssue) { i.State = types.StatusClosed }},
		{"state type", func(i *itracker.TrackerIssue) { i.State = "open" }},
		{"issue type value", func(i *itracker.TrackerIssue) { i.Type = types.TypeBug }},
		{"issue type type", func(i *itracker.TrackerIssue) { i.Type = "task" }},
		{"assignee", func(i *itracker.TrackerIssue) { i.Assignee = "other" }},
		{"labels", func(i *itracker.TrackerIssue) { i.Labels = []string{"alpha"} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changed := *remote
			changed.Labels = append([]string(nil), remote.Labels...)
			tc.mutate(&changed)
			if trackerIssueEqual(local, &changed) {
				t.Fatal("different issues compared equal")
			}
		})
	}
}

func TestStringSetNormalizationExact(t *testing.T) {
	values := []string{" beta ", "", "alpha", "  ", "alpha"}
	want := []string{"alpha", "alpha", "beta"}
	if got := normalizeStringSlice(values); !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeStringSlice = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(values, []string{" beta ", "", "alpha", "  ", "alpha"}) {
		t.Fatalf("normalizeStringSlice mutated input: %#v", values)
	}
	if !equalStringSets([]string{" beta", "alpha"}, []string{"alpha", "beta "}) {
		t.Fatal("equal sets with different order were not equal")
	}
	if equalStringSets([]string{"alpha", ""}, []string{"alpha"}) {
		t.Fatal("different raw lengths compared equal")
	}
	if equalStringSets([]string{"alpha"}, []string{"beta"}) {
		t.Fatal("different values compared equal")
	}
}

func TestSameAndCloneTrackerIssue(t *testing.T) {
	pageID := "01234567-89ab-cdef-0123-456789abcdef"
	left := itracker.TrackerIssue{ID: pageID, Labels: []string{"one"}}
	for _, right := range []itracker.TrackerIssue{
		{ID: pageID},
		{Identifier: pageID},
		{URL: "https://www.notion.so/Task-0123456789abcdef0123456789abcdef"},
	} {
		if !sameTrackerIssue(left, right) {
			t.Fatalf("sameTrackerIssue(%#v, %#v) = false", left, right)
		}
	}
	if sameTrackerIssue(itracker.TrackerIssue{}, itracker.TrackerIssue{}) || sameTrackerIssue(left, itracker.TrackerIssue{ID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}) {
		t.Fatal("unrelated issues compared equal")
	}

	clone := cloneTrackerIssue(left)
	if len(clone.Labels) != 1 {
		t.Fatalf("clone labels = %#v, want one label", clone.Labels)
	}
	clone.Labels[0] = "changed"
	if left.Labels[0] != "one" {
		t.Fatal("clone shared label backing array")
	}
	withoutLabels := cloneTrackerIssue(itracker.TrackerIssue{ID: pageID})
	if withoutLabels.Labels != nil {
		t.Fatalf("nil labels became %#v", withoutLabels.Labels)
	}
}

func TestFetchPredicatesBoundaries(t *testing.T) {
	boundary := time.Date(2026, 4, 5, 12, 34, 45, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		issue *itracker.TrackerIssue
		since *time.Time
		want  bool
	}{
		{"nil issue", nil, nil, false},
		{"no since", &itracker.TrackerIssue{}, nil, true},
		{"zero update", &itracker.TrackerIssue{}, &boundary, true},
		{"previous minute", &itracker.TrackerIssue{UpdatedAt: boundary.Truncate(time.Minute).Add(-time.Nanosecond)}, &boundary, false},
		{"boundary minute", &itracker.TrackerIssue{UpdatedAt: boundary.Truncate(time.Minute)}, &boundary, true},
		{"after", &itracker.TrackerIssue{UpdatedAt: boundary.Add(time.Minute)}, &boundary, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesFetchSince(tc.issue, tc.since); got != tc.want {
				t.Fatalf("matchesFetchSince = %v, want %v", got, tc.want)
			}
		})
	}

	open := &itracker.TrackerIssue{State: types.StatusOpen}
	closed := &itracker.TrackerIssue{State: types.StatusClosed}
	untyped := &itracker.TrackerIssue{State: "closed"}
	for _, tc := range []struct {
		issue  *itracker.TrackerIssue
		filter string
		want   bool
	}{
		{nil, "all", false}, {open, "", true}, {open, " ALL ", true},
		{open, "open", true}, {closed, "open", false}, {untyped, "open", true},
		{open, "closed", false}, {closed, " CLOSED ", true}, {untyped, "closed", false},
		{closed, "unknown", true},
	} {
		if got := matchesFetchState(tc.issue, tc.filter); got != tc.want {
			t.Fatalf("matchesFetchState(%#v, %q) = %v, want %v", tc.issue, tc.filter, got, tc.want)
		}
	}
}

func TestShouldBackfillNotionIssueExact(t *testing.T) {
	pageID := "01234567-89ab-cdef-0123-456789abcdef"
	base := &itracker.TrackerIssue{
		ID:  pageID,
		Raw: &PulledIssue{ID: "bd-1"},
	}
	if !shouldBackfillNotionIssue(base, map[string]struct{}{}, map[string]struct{}{}) {
		t.Fatal("new remote issue was not selected for backfill")
	}
	if shouldBackfillNotionIssue(nil, nil, nil) {
		t.Fatal("nil issue selected for backfill")
	}
	if shouldBackfillNotionIssue(base, map[string]struct{}{pageID: {}}, map[string]struct{}{}) {
		t.Fatal("known external identifier selected for backfill")
	}
	if shouldBackfillNotionIssue(base, map[string]struct{}{}, map[string]struct{}{"bd-1": {}}) {
		t.Fatal("known local ID selected for backfill")
	}
	for _, raw := range []any{nil, PulledIssue{ID: "bd-1"}, (*PulledIssue)(nil), &PulledIssue{ID: " "}} {
		issue := *base
		issue.Raw = raw
		if shouldBackfillNotionIssue(&issue, map[string]struct{}{}, map[string]struct{}{}) {
			t.Fatalf("raw %#v selected for backfill", raw)
		}
	}
}

func TestDerefStrExact(t *testing.T) {
	value := " value "
	if got := derefStr(nil); got != "" {
		t.Fatalf("derefStr(nil) = %q", got)
	}
	if got := derefStr(&value); got != "value" {
		t.Fatalf("derefStr(value) = %q", got)
	}
}
