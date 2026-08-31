package conformance

import (
	"reflect"
	"testing"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// Audit cases for the "search-counts-stats" slice. Each case encodes a strange
// but real behavior of the embedded-Dolt reference (the oracle) that the shared
// SQL surface — SearchIssuesWithCounts, CountIssuesByGroup, GetStatistics,
// GetStaleIssues, GetEpicsEligibleForClosure, GetIssuesByLabel, and the default
// SearchIssues ordering contract — is expected to reproduce on every backend.
//
// Grounded in: sqlbuild/counts.go (the counts mega-query), issueops/count.go
// (COALESCE(col,'') + P-prefix / (unassigned) / (no labels) normalization),
// issueops/statistics.go (blocked count + Ready clamp), issueops/stale.go
// (status override + ephemeral exclusion), issueops/epic_closure.go (zero-child
// skip), issueops/bulk_ops.go (wisp_labels union), and sqlbuild/sort.go
// (priority/created_at/id and NULLS-last-on-DESC ordering).

// RunAudit_search_counts_stats runs every store case in this slice.
func RunAudit_search_counts_stats(t *testing.T, f Factory) {
	t.Helper()
	t.Run("SearchIssuesWithCounts", func(t *testing.T) { testAuditSearchIssuesWithCounts(t, f) })
	t.Run("ReadyWorkDepCreatedAtParity", func(t *testing.T) { testAuditReadyWorkDepCreatedAtParity(t, f) })
	t.Run("CountByPriority", func(t *testing.T) { testAuditCountByPriority(t, f) })
	t.Run("CountByLabel", func(t *testing.T) { testAuditCountByLabel(t, f) })
	t.Run("CountByAssigneeAndType", func(t *testing.T) { testAuditCountByAssigneeAndType(t, f) })
	t.Run("Statistics", func(t *testing.T) { testAuditStatistics(t, f) })
	t.Run("StatisticsReadyClamp", func(t *testing.T) { testAuditStatisticsReadyClamp(t, f) })
	t.Run("SearchDefaultOrderTieBreak", func(t *testing.T) { testAuditSearchDefaultOrderTieBreak(t, f) })
	t.Run("SearchIdenticalTimestampIDOrder", func(t *testing.T) { testAuditSearchIdenticalTimestampIDOrder(t, f) })
	t.Run("SearchSortByClosedNullsLast", func(t *testing.T) { testAuditSearchSortByClosedNullsLast(t, f) })
	t.Run("SearchSortByTitleCaseFolded", func(t *testing.T) { testAuditSearchSortByTitleCaseFolded(t, f) })
	t.Run("SearchSortTieBreakSurvivesReversal", func(t *testing.T) { testAuditSearchSortTieBreakSurvivesReversal(t, f) })
	t.Run("SearchSortNullsDirectional", func(t *testing.T) { testAuditSearchSortNullsDirectional(t, f) })
	t.Run("SearchSortByIDCompleteSet", func(t *testing.T) { testAuditSearchSortByIDCompleteSet(t, f) })
	t.Run("SearchTextIDBranchExternalRef", func(t *testing.T) { testAuditSearchTextIDBranchExternalRef(t, f) })
	t.Run("SearchIDPrefixCaseSensitive", func(t *testing.T) { testAuditSearchIDPrefixCaseSensitive(t, f) })
	t.Run("SearchParentDescendantCaseSensitive", func(t *testing.T) { testAuditSearchParentDescendantCaseSensitive(t, f) })
	t.Run("StaleStatusOverride", func(t *testing.T) { testAuditStaleStatusOverride(t, f) })
	t.Run("EpicsEligiblePartial", func(t *testing.T) { testAuditEpicsEligiblePartial(t, f) })
	t.Run("GetIssuesByLabelWithWisp", func(t *testing.T) { testAuditGetIssuesByLabelWithWisp(t, f) })
	t.Run("WispMergeSearchCount", func(t *testing.T) { testAuditWispMergeSearchCount(t, f) })
}

// --- file-private helpers (audit-prefixed to avoid collisions) ---

func auditCountsByID(items []*types.IssueWithCounts) map[string]*types.IssueWithCounts {
	m := make(map[string]*types.IssueWithCounts, len(items))
	for _, it := range items {
		if it != nil && it.Issue != nil {
			m[it.Issue.ID] = it
		}
	}
	return m
}

func auditWholeSec(y int) time.Time {
	return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
}

// --- cases ---

// The counts mega-query (sqlbuild.SearchCountsSQL) computes DependencyCount from
// outgoing type='blocks' edges, DependentCount from reverse blockers, CommentCount,
// and Parent from the MIN parent-child target. Every construct is dialect text
// (JSON_ARRAYAGG / DATE_FORMAT / CAST / MIN(COALESCE)) translated verbatim.
func testAuditSearchIssuesWithCounts(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "swc-e"}, IssueContent: types.IssueContent{Title: "Epic"}, IssueWorkflow: types.IssueWorkflow{IssueType: types.TypeEpic}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "swc-c1"}, IssueContent: types.IssueContent{Title: "Child"}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "swc-a"}, IssueContent: types.IssueContent{Title: "A"}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "swc-b"}, IssueContent: types.IssueContent{Title: "B"}}), "a"))
	// swc-a depends_on swc-b (blocks); swc-b depends_on swc-c1 (blocks); swc-c1 parent-child swc-e.
	must(t, s.AddDependency(c, &types.Dependency{IssueID: "swc-a", DependsOnID: "swc-b", Type: types.DepBlocks}, "a"))
	must(t, s.AddDependency(c, &types.Dependency{IssueID: "swc-b", DependsOnID: "swc-c1", Type: types.DepBlocks}, "a"))
	must(t, s.AddDependency(c, &types.Dependency{IssueID: "swc-c1", DependsOnID: "swc-e", Type: types.DepParentChild}, "a"))
	_, err := s.AddIssueComment(c, "swc-a", "alice", "one")
	must(t, err)
	_, err = s.AddIssueComment(c, "swc-a", "bob", "two")
	must(t, err)
	must(t, s.AddLabel(c, "swc-a", "bug", "a"))

	items, err := s.SearchIssuesWithCounts(c, "", types.IssueFilter{})
	must(t, err)
	byID := auditCountsByID(items)
	assertAuditSearchCountsA(t, byID["swc-a"])
	assertAuditSearchCountsB(t, byID["swc-b"])
	assertAuditSearchCountsChild(t, byID["swc-c1"])
}

func assertAuditSearchCountsA(t *testing.T, a *types.IssueWithCounts) {
	t.Helper()
	if a == nil {
		t.Fatal("swc-a missing from counts result")
	}
	if a.DependencyCount != 1 {
		t.Errorf("swc-a DependencyCount = %d, want 1 (outgoing blocks)", a.DependencyCount)
	}
	if a.DependentCount != 0 {
		t.Errorf("swc-a DependentCount = %d, want 0", a.DependentCount)
	}
	if a.CommentCount != 2 {
		t.Errorf("swc-a CommentCount = %d, want 2", a.CommentCount)
	}
	if a.Parent != nil {
		t.Errorf("swc-a Parent = %v, want nil", *a.Parent)
	}
	if !contains(a.Labels, "bug") {
		t.Errorf("swc-a Labels = %v, want to include bug", a.Labels)
	}
}

func assertAuditSearchCountsB(t *testing.T, b *types.IssueWithCounts) {
	t.Helper()
	if b == nil {
		t.Fatal("swc-b missing from counts result")
	}
	if b.DependencyCount != 1 {
		t.Errorf("swc-b DependencyCount = %d, want 1 (blocked by swc-c1)", b.DependencyCount)
	}
	if b.DependentCount != 1 {
		t.Errorf("swc-b DependentCount = %d, want 1 (swc-a depends on it)", b.DependentCount)
	}
}

func assertAuditSearchCountsChild(t *testing.T, c1 *types.IssueWithCounts) {
	t.Helper()
	if c1 == nil {
		t.Fatal("swc-c1 missing from counts result")
	}
	if c1.Parent == nil || *c1.Parent != "swc-e" {
		t.Errorf("swc-c1 Parent = %v, want swc-e", c1.Parent)
	}
	if c1.DependentCount != 1 {
		t.Errorf("swc-c1 DependentCount = %d, want 1 (swc-b depends on it)", c1.DependentCount)
	}
}

// The counts mega-query renders each dependency's created_at through
// DATE_FORMAT(created_at,'%Y-%m-%dT%H:%i:%sZ') into deps_json, and every backend must
// reproduce the stored timestamp — not the zero time. This is the assertion the suite
// was missing: DependencyCount only proves the edge exists, so a backend that rendered
// the edge's created_at as NULL/zero stayed green. SQLite does exactly that when a
// dependency created_at bound as a Go time.Time is stored in t.String() form and
// strftime cannot parse it — the reason the DSN must set _time_format=datetime. The
// batch/import path (CreateIssuesWithFullOptions, i.e. `bd import`) binds dep.CreatedAt
// verbatim, so it is the path that exposes the divergence. GetDependencies returns the
// target issues, not the edges, so it cannot witness the edge timestamp; assert the
// rendered edge created_at equals the value that was imported.
func testAuditReadyWorkDepCreatedAtParity(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	// A fixed non-zero past timestamp, whole-second to match DATE_FORMAT's granularity.
	depCreatedAt := time.Date(2023, 5, 15, 10, 20, 30, 0, time.UTC)

	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "dca-t"}, IssueContent: types.IssueContent{Title: "target"}}), "a"))
	must(t, s.CreateIssuesWithFullOptions(c, []*types.Issue{
		withDefaults(&types.Issue{
			IssueID: types.IssueID{
				ID: "dca-s",
			},
			IssueContent: types.IssueContent{
				Title: "source",
			},
			IssueGraph: types.IssueGraph{
				Dependencies: []*types.Dependency{
					{IssueID: "dca-s", DependsOnID: "dca-t", Type: types.DepBlocks, CreatedAt: depCreatedAt},
				},
			},
		}),
	}, "a", storage.BatchCreateOptions{SkipPrefixValidation: true}))

	// Rendered path: SearchIssuesWithCounts (bd list --with-counts) and
	// GetReadyWorkWithCounts (bd ready) share ScanReadyWorkRowWithCounts, which parses
	// deps_json into issue.Dependencies. The rendered created_at must be the real
	// imported timestamp, not the zero time a NULL DATE_FORMAT would unmarshal to.
	items, err := s.SearchIssuesWithCounts(c, "", types.IssueFilter{})
	must(t, err)
	src := auditCountsByID(items)["dca-s"]
	if src == nil {
		t.Fatal("dca-s missing from SearchIssuesWithCounts result")
	}
	if len(src.Dependencies) != 1 {
		t.Fatalf("dca-s rendered deps = %d, want 1", len(src.Dependencies))
	}
	got := src.Dependencies[0].CreatedAt
	if got.IsZero() {
		t.Fatal("rendered dependency created_at is the zero time: deps_json lost the timestamp " +
			"(SQLite DATE_FORMAT/strftime parity break — DSN needs _time_format=datetime)")
	}
	if !got.Equal(depCreatedAt) {
		t.Fatalf("rendered dependency created_at = %v, want %v (imported edge timestamp)", got, depCreatedAt)
	}
}

// countByColumnInTx emits COALESCE(priority, ”) GROUP BY priority; priority is
// integer NOT NULL. Both maintained implementations return a string key, then
// countGroupForTablesInTx prepends 'P'. Reference keys: P0/P1/P2.
func testAuditCountByPriority(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	must(t, s.SetConfig(c, "issue_prefix", "test"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "cp-1"}, IssueContent: types.IssueContent{Title: "a"}, IssueWorkflow: types.IssueWorkflow{Priority: 0}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "cp-2"}, IssueContent: types.IssueContent{Title: "b"}, IssueWorkflow: types.IssueWorkflow{Priority: 1}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "cp-3"}, IssueContent: types.IssueContent{Title: "c"}, IssueWorkflow: types.IssueWorkflow{Priority: 1}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "cp-4"}, IssueContent: types.IssueContent{Title: "d"}, IssueWorkflow: types.IssueWorkflow{Priority: 2}}), "a"))

	got, err := s.CountIssuesByGroup(c, types.IssueFilter{}, "priority")
	must(t, err)
	want := map[string]int{"P0": 1, "P1": 2, "P2": 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CountIssuesByGroup(priority) = %v, want %v", got, want)
	}
}

// countByLabelInTx does not join: it counts labels via an IN-subquery (dodging the
// Dolt joinIter panic), counts multi-label issues once per label (overlapping
// buckets), and appends a synthetic "(no labels)" bucket ONLY when >0.
func testAuditCountByLabel(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "cl-1"}, IssueContent: types.IssueContent{Title: "a"}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "cl-2"}, IssueContent: types.IssueContent{Title: "b"}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "cl-3"}, IssueContent: types.IssueContent{Title: "c"}}), "a"))
	must(t, s.AddLabel(c, "cl-1", "bug", "a"))
	must(t, s.AddLabel(c, "cl-1", "urgent", "a"))
	must(t, s.AddLabel(c, "cl-2", "bug", "a"))

	got, err := s.CountIssuesByGroup(c, types.IssueFilter{}, "label")
	must(t, err)
	want := map[string]int{"bug": 2, "urgent": 1, "(no labels)": 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CountIssuesByGroup(label) = %v, want %v", got, want)
	}

	// With every issue labeled, "(no labels)" is ABSENT (not present with value 0).
	must(t, s.AddLabel(c, "cl-3", "bug", "a"))
	got2, err := s.CountIssuesByGroup(c, types.IssueFilter{}, "label")
	must(t, err)
	if _, present := got2["(no labels)"]; present {
		t.Errorf("(no labels) key present when all issues labeled: %v", got2)
	}
	if got2["bug"] != 3 {
		t.Errorf("bug = %d, want 3", got2["bug"])
	}
}

// assignee: COALESCE(assignee,”) collapses NULL/” into one key, then normalized
// to "(unassigned)". type maps to issue_type and returns raw strings unprefixed.
func testAuditCountByAssigneeAndType(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "ca-1"}, IssueContent: types.IssueContent{Title: "a"}, IssueWorkflow: types.IssueWorkflow{Assignee: "alice", IssueType: "bug"}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "ca-2"}, IssueContent: types.IssueContent{Title: "b"}, IssueWorkflow: types.IssueWorkflow{Assignee: "alice", IssueType: "bug"}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "ca-3"}, IssueContent: types.IssueContent{Title: "c"}, IssueWorkflow: types.IssueWorkflow{IssueType: "task"}}), "a"))

	gotA, err := s.CountIssuesByGroup(c, types.IssueFilter{}, "assignee")
	must(t, err)
	wantA := map[string]int{"alice": 2, "(unassigned)": 1}
	if !reflect.DeepEqual(gotA, wantA) {
		t.Errorf("CountIssuesByGroup(assignee) = %v, want %v", gotA, wantA)
	}

	gotT, err := s.CountIssuesByGroup(c, types.IssueFilter{}, "type")
	must(t, err)
	wantT := map[string]int{"bug": 2, "task": 1}
	if !reflect.DeepEqual(gotT, wantT) {
		t.Errorf("CountIssuesByGroup(type) = %v, want %v (raw, unprefixed)", gotT, wantT)
	}
}

// GetStatistics computes six status counts plus BlockedIssues (is_blocked=1 and
// status not closed/pinned) and PinnedIssues (the pinned=1 column flag, distinct
// from status='pinned'). ReadyIssues = OpenIssues - BlockedIssues clamped at 0.
func testAuditStatistics(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "st-o1"}, IssueContent: types.IssueContent{Title: "o1"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "st-o2"}, IssueContent: types.IssueContent{Title: "o2"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "st-p1"}, IssueContent: types.IssueContent{Title: "p1"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusInProgress}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "st-c1"}, IssueContent: types.IssueContent{Title: "c1"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusClosed}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "st-d1"}, IssueContent: types.IssueContent{Title: "d1"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusDeferred}}), "a"))
	// pinned=1 column flag, with a non-open/closed status so it isolates PinnedIssues.
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "st-pin"}, IssueContent: types.IssueContent{Title: "pin"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusPinned}, IssueWisp: types.IssueWisp{Pinned: true}}), "a"))
	// st-p1 (in_progress) blocked by an open issue -> is_blocked=1, counted in BlockedIssues.
	must(t, s.AddDependency(c, &types.Dependency{IssueID: "st-p1", DependsOnID: "st-o1", Type: types.DepBlocks}, "a"))

	stats, err := s.GetStatistics(c)
	must(t, err)
	assertAuditStatistics(t, stats)
}

func assertAuditStatistics(t *testing.T, stats *types.Statistics) {
	t.Helper()
	assertAuditStatisticCounts(t, stats)
	assertAuditOptionalCounts(t, stats)
}

func assertAuditStatisticCounts(t *testing.T, stats *types.Statistics) {
	t.Helper()
	if stats.TotalIssues != 6 {
		t.Errorf("TotalIssues = %d, want 6", stats.TotalIssues)
	}
	if stats.OpenIssues != 2 {
		t.Errorf("OpenIssues = %d, want 2", stats.OpenIssues)
	}
	if stats.InProgressIssues != 1 {
		t.Errorf("InProgressIssues = %d, want 1", stats.InProgressIssues)
	}
	if stats.ClosedIssues != 1 {
		t.Errorf("ClosedIssues = %d, want 1", stats.ClosedIssues)
	}
	if stats.DeferredIssues != 1 {
		t.Errorf("DeferredIssues = %d, want 1", stats.DeferredIssues)
	}
	if stats.PinnedIssues != 1 {
		t.Errorf("PinnedIssues = %d, want 1", stats.PinnedIssues)
	}
}

func assertAuditOptionalCounts(t *testing.T, stats *types.Statistics) {
	t.Helper()
	if stats.BlockedIssues == nil || *stats.BlockedIssues != 1 {
		got := -1
		if stats.BlockedIssues != nil {
			got = *stats.BlockedIssues
		}
		t.Errorf("BlockedIssues = %d, want 1", got)
	}
	// Ready = Open(2) - Blocked(1) = 1 (the blocked issue is in_progress, not open).
	if stats.ReadyIssues == nil || *stats.ReadyIssues != 1 {
		got := -1
		if stats.ReadyIssues != nil {
			got = *stats.ReadyIssues
		}
		t.Errorf("ReadyIssues = %d, want 1", got)
	}
}

// The Ready clamp is load-bearing: when BlockedIssues exceeds OpenIssues,
// OpenIssues - BlockedIssues goes negative and is clamped to 0.
func testAuditStatisticsReadyClamp(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	// A blocker plus two in_progress issues blocked by it: zero OPEN issues, two blocked.
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "rc-blk"}, IssueContent: types.IssueContent{Title: "blk"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusInProgress}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "rc-1"}, IssueContent: types.IssueContent{Title: "b1"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusInProgress}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "rc-2"}, IssueContent: types.IssueContent{Title: "b2"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusInProgress}}), "a"))
	must(t, s.AddDependency(c, &types.Dependency{IssueID: "rc-1", DependsOnID: "rc-blk", Type: types.DepBlocks}, "a"))
	must(t, s.AddDependency(c, &types.Dependency{IssueID: "rc-2", DependsOnID: "rc-blk", Type: types.DepBlocks}, "a"))

	stats, err := s.GetStatistics(c)
	must(t, err)
	if stats.OpenIssues != 0 {
		t.Errorf("OpenIssues = %d, want 0", stats.OpenIssues)
	}
	if stats.BlockedIssues == nil || *stats.BlockedIssues != 2 {
		got := -1
		if stats.BlockedIssues != nil {
			got = *stats.BlockedIssues
		}
		t.Errorf("BlockedIssues = %d, want 2", got)
	}
	// 0 - 2 = -2, clamped to 0.
	if stats.ReadyIssues == nil || *stats.ReadyIssues != 0 {
		got := -1
		if stats.ReadyIssues != nil {
			got = *stats.ReadyIssues
		}
		t.Errorf("ReadyIssues = %d, want 0 (clamped)", got)
	}
}

// The default/priority sort contract is ORDER BY priority ASC, created_at DESC,
// id ASC. With pinned distinct whole-second timestamps the ordering is fully
// deterministic and portable.
func testAuditSearchDefaultOrderTieBreak(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	t1, t2, t3 := auditWholeSec(2020), auditWholeSec(2021), auditWholeSec(2022)
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "so-p0"}, IssueContent: types.IssueContent{Title: "p0"}, IssueWorkflow: types.IssueWorkflow{Priority: 0}, IssueTimes: types.IssueTimes{CreatedAt: t1, UpdatedAt: t1}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "so-old"}, IssueContent: types.IssueContent{Title: "old"}, IssueWorkflow: types.IssueWorkflow{Priority: 1}, IssueTimes: types.IssueTimes{CreatedAt: t1, UpdatedAt: t1}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "so-mid"}, IssueContent: types.IssueContent{Title: "mid"}, IssueWorkflow: types.IssueWorkflow{Priority: 1}, IssueTimes: types.IssueTimes{CreatedAt: t2, UpdatedAt: t2}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "so-new"}, IssueContent: types.IssueContent{Title: "new"}, IssueWorkflow: types.IssueWorkflow{Priority: 1}, IssueTimes: types.IssueTimes{CreatedAt: t3, UpdatedAt: t3}}), "a"))

	results, err := s.SearchIssues(c, "", types.IssueFilter{})
	must(t, err)
	// priority ASC (p0 first), then created_at DESC among the prio-1 rows.
	want := []string{"so-p0", "so-new", "so-mid", "so-old"}
	if got := orderedIDs(results); !reflect.DeepEqual(got, want) {
		t.Errorf("default order = %v, want %v", got, want)
	}
}

// Same priority + identical whole-second created_at forces the final id-ASC leg.
func testAuditSearchIdenticalTimestampIDOrder(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	tie := auditWholeSec(2020)
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "tie-b"}, IssueContent: types.IssueContent{Title: "b"}, IssueWorkflow: types.IssueWorkflow{Priority: 1}, IssueTimes: types.IssueTimes{CreatedAt: tie, UpdatedAt: tie}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "tie-a"}, IssueContent: types.IssueContent{Title: "a"}, IssueWorkflow: types.IssueWorkflow{Priority: 1}, IssueTimes: types.IssueTimes{CreatedAt: tie, UpdatedAt: tie}}), "a"))

	results, err := s.SearchIssues(c, "", types.IssueFilter{})
	must(t, err)
	want := []string{"tie-a", "tie-b"}
	if got := orderedIDs(results); !reflect.DeepEqual(got, want) {
		t.Errorf("identical-timestamp order = %v, want %v (id ASC)", got, want)
	}
}

// SortBy="closed" emits ORDER BY closed_at DESC, id ASC. closed_at is nullable;
// the storage contract places NULLs last on DESC. The open (NULL-closed) rows follow the
// closed ones, ordered by id ASC.
func testAuditSearchSortByClosedNullsLast(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	tNew, tOld := auditWholeSec(2022), auditWholeSec(2021)
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "cs-open-a"}, IssueContent: types.IssueContent{Title: "oa"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "cs-open-b"}, IssueContent: types.IssueContent{Title: "ob"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "cs-closed-new"}, IssueContent: types.IssueContent{Title: "cn"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusClosed}, IssueTimes: types.IssueTimes{ClosedAt: &tNew}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "cs-closed-old"}, IssueContent: types.IssueContent{Title: "co"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusClosed}, IssueTimes: types.IssueTimes{ClosedAt: &tOld}}), "a"))

	results, err := s.SearchIssues(c, "", types.IssueFilter{IssueFilterPage: types.IssueFilterPage{SortBy: "closed"}})
	must(t, err)
	want := []string{"cs-closed-new", "cs-closed-old", "cs-open-a", "cs-open-b"}
	if got := orderedIDs(results); !reflect.DeepEqual(got, want) {
		t.Errorf("closed-sort order = %v, want %v (NULLs last on DESC)", got, want)
	}
}

// A search sorted by title and cut to a limit keeps the first N rows under
// case-folded title order — "apple" before "Zebra" — with case-folded ties broken
// by id ASC, on every backend and database collation. sqlbuild/sort.go renders
// LOWER(title) for both the SQL ORDER BY and the Go-side merge, but an engine whose
// default text collation is byte-wise or linguistic satisfies every other ordering
// case in this slice while reordering this one, silently changing which rows survive
// the cut with no error and no other signal.
func testAuditSearchSortByTitleCaseFolded(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	// Byte order (APPLE2, Apple, Zebra, apple, banana) and priority order both
	// disagree with the case-folded order asserted below.
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "ti-1"}, IssueContent: types.IssueContent{Title: "Zebra"}, IssueWorkflow: types.IssueWorkflow{Priority: 0}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "ti-2"}, IssueContent: types.IssueContent{Title: "apple"}, IssueWorkflow: types.IssueWorkflow{Priority: 3}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "ti-3"}, IssueContent: types.IssueContent{Title: "APPLE2"}, IssueWorkflow: types.IssueWorkflow{Priority: 2}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "ti-4"}, IssueContent: types.IssueContent{Title: "banana"}, IssueWorkflow: types.IssueWorkflow{Priority: 1}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "ti-5"}, IssueContent: types.IssueContent{Title: "Apple"}, IssueWorkflow: types.IssueWorkflow{Priority: 3}}), "a"))

	// ti-2/ti-5 fold to the same title, so they pin the id-ASC tie leg.
	want := []string{"ti-2", "ti-5", "ti-3", "ti-4", "ti-1"}
	results, err := s.SearchIssues(c, "", types.IssueFilter{IssueFilterPage: types.IssueFilterPage{SortBy: "title"}})
	must(t, err)
	if got := orderedIDs(results); !reflect.DeepEqual(got, want) {
		t.Errorf("title-sort order = %v, want %v (LOWER(title) ASC, id ASC)", got, want)
	}

	page, err := s.SearchIssues(c, "", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Limit: 2}, IssueFilterPage: types.IssueFilterPage{SortBy: "title"}})
	must(t, err)
	if got := orderedIDs(page); !reflect.DeepEqual(got, want[:2]) {
		t.Errorf("title-sort page = %v, want %v (first 2 rows of the case-folded order)", got, want[:2])
	}
}

// Every non-default sort breaks exact ties by id ASC, and reversing the sort flips
// the primary key only — the id tie-break never flips. sqlbuild/sort.go emits
// ORDER BY <col> <dir>, id ASC for every non-default key and runs flipDir over the
// primary direction alone, so a tied group's internal order is identical in both
// directions. The slice pins tie-breaking for the default and closed sorts only;
// this case covers a timestamp key and a vocabulary key below the same seam.
func testAuditSearchSortTieBreakSurvivesReversal(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	tied, newer := auditWholeSec(2020), auditWholeSec(2022)
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "tb-a"}, IssueContent: types.IssueContent{Title: "a"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen}, IssueTimes: types.IssueTimes{CreatedAt: tied, UpdatedAt: tied}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "tb-b"}, IssueContent: types.IssueContent{Title: "b"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen}, IssueTimes: types.IssueTimes{CreatedAt: tied, UpdatedAt: tied}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "tb-c"}, IssueContent: types.IssueContent{Title: "c"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusClosed}, IssueTimes: types.IssueTimes{CreatedAt: tied, UpdatedAt: tied}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "tb-new"}, IssueContent: types.IssueContent{Title: "n"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusInProgress}, IssueTimes: types.IssueTimes{CreatedAt: newer, UpdatedAt: newer}}), "a"))

	results, err := s.SearchIssues(c, "", types.IssueFilter{IssueFilterPage: types.IssueFilterPage{SortBy: "updated"}})
	must(t, err)
	want := []string{"tb-new", "tb-a", "tb-b", "tb-c"}
	if got := orderedIDs(results); !reflect.DeepEqual(got, want) {
		t.Errorf("updated-sort order = %v, want %v (updated_at DESC, id ASC)", got, want)
	}

	results, err = s.SearchIssues(c, "", types.IssueFilter{IssueFilterPage: types.IssueFilterPage{SortBy: "updated", SortDesc: true}})
	must(t, err)
	want = []string{"tb-a", "tb-b", "tb-c", "tb-new"}
	if got := orderedIDs(results); !reflect.DeepEqual(got, want) {
		t.Errorf("reversed updated-sort order = %v, want %v (oldest first; the tied group is STILL id ASC)", got, want)
	}

	results, err = s.SearchIssues(c, "", types.IssueFilter{IssueFilterPage: types.IssueFilterPage{SortBy: "status"}})
	must(t, err)
	want = []string{"tb-c", "tb-new", "tb-a", "tb-b"}
	if got := orderedIDs(results); !reflect.DeepEqual(got, want) {
		t.Errorf("status-sort order = %v, want %v (status ASC, id ASC)", got, want)
	}

	results, err = s.SearchIssues(c, "", types.IssueFilter{IssueFilterPage: types.IssueFilterPage{SortBy: "status", SortDesc: true}})
	must(t, err)
	want = []string{"tb-a", "tb-b", "tb-new", "tb-c"}
	if got := orderedIDs(results); !reflect.DeepEqual(got, want) {
		t.Errorf("reversed status-sort order = %v, want %v (the tied open group is STILL id ASC)", got, want)
	}
}

// Rows with no value for a nullable sort key (closed_at, assignee) sort first on
// ascending order and last on descending order, regardless of the engine's native
// NULL ordering. sqlbuild/sort.go leads those keys with an explicit (col IS NULL)
// term precisely "so the contract does not depend on a driver's default NULL
// ordering", but the slice pins only the closed sort in its default direction.
// The unassigned rows are created exactly as `bd create` leaves them, so the case
// pins the observable order without prescribing a NULL-vs-empty-string encoding.
func testAuditSearchSortNullsDirectional(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	tNew, tOld := auditWholeSec(2022), auditWholeSec(2021)
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "nl-open-a"}, IssueContent: types.IssueContent{Title: "oa"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "nl-open-b"}, IssueContent: types.IssueContent{Title: "ob"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "nl-closed-new"}, IssueContent: types.IssueContent{Title: "cn"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusClosed, Assignee: "alice"}, IssueTimes: types.IssueTimes{ClosedAt: &tNew}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "nl-closed-old"}, IssueContent: types.IssueContent{Title: "co"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusClosed, Assignee: "bob"}, IssueTimes: types.IssueTimes{ClosedAt: &tOld}}), "a"))

	results, err := s.SearchIssues(c, "", types.IssueFilter{IssueFilterPage: types.IssueFilterPage{SortBy: "closed", SortDesc: true}})
	must(t, err)
	want := []string{"nl-open-a", "nl-open-b", "nl-closed-old", "nl-closed-new"}
	if got := orderedIDs(results); !reflect.DeepEqual(got, want) {
		t.Errorf("reversed closed-sort order = %v, want %v (NULLs first on ASC, then oldest closed)", got, want)
	}

	results, err = s.SearchIssues(c, "", types.IssueFilter{IssueFilterPage: types.IssueFilterPage{SortBy: "assignee"}})
	must(t, err)
	want = []string{"nl-open-a", "nl-open-b", "nl-closed-new", "nl-closed-old"}
	if got := orderedIDs(results); !reflect.DeepEqual(got, want) {
		t.Errorf("assignee-sort order = %v, want %v (unassigned first on ASC, id ASC among them)", got, want)
	}

	results, err = s.SearchIssues(c, "", types.IssueFilter{IssueFilterPage: types.IssueFilterPage{SortBy: "assignee", SortDesc: true}})
	must(t, err)
	want = []string{"nl-closed-old", "nl-closed-new", "nl-open-a", "nl-open-b"}
	if got := orderedIDs(results); !reflect.DeepEqual(got, want) {
		t.Errorf("reversed assignee-sort order = %v, want %v (unassigned last on DESC)", got, want)
	}
}

// A search with SortBy="id" succeeds and answers the complete matching set. "id" is
// the one key sqlbuild renders no ORDER BY for at all — IsGoSideSort suppresses the
// clause and "id" is deliberately absent from SortDefs — so a store that validated
// the key against SortDefs would refuse it, and a store that trimmed the set in its
// own byte order would break the id sort for every caller. Sequence is not asserted:
// the natural-numeric display order is owned above the store (internal/workapi/sort.go
// CompareIssuesBy via utils.NaturalCompareIDs), which is why callers push Limit 0 here.
//
// RunReaderListNaturalNumericIDSortTrimsAfterTheFetch looks like a dominator and
// is not: it drives the Reader, which rides SearchIssuesWithCountsInTx (and the
// unit-of-work UNION), while this drives the PLAIN SearchIssues verb over
// searchInTx. Those are separate bodies — make searchInTx refuse the go-side sort
// key and the reader case stays green while this one goes red (measured with
// scripts/mutation-equivalence.sh). This is the only place the go-side sort key
// meets the plain search projection.
func testAuditSearchSortByIDCompleteSet(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	// Natural order (test-2, test-9, test-10) and byte order (test-10, test-2, test-9) differ.
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "test-9"}, IssueContent: types.IssueContent{Title: "nine"}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "test-10"}, IssueContent: types.IssueContent{Title: "ten"}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "test-2"}, IssueContent: types.IssueContent{Title: "two"}}), "a"))

	results, err := s.SearchIssues(c, "", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Limit: 0}, IssueFilterPage: types.IssueFilterPage{SortBy: "id"}})
	must(t, err)
	want := []string{"test-10", "test-2", "test-9"}
	if got := issueIDs(results); !reflect.DeepEqual(got, want) {
		t.Errorf("SortBy=id set = %v, want %v (complete matching set)", got, want)
	}
}

// The text-query ID branch (LooksLikeIssueID) matches external_ref substrings via
// LOWER(external_ref) LIKE, and matches id exactly via id = lowerQuery.
func testAuditSearchTextIDBranchExternalRef(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	ref := "JIRA-1234"
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "test-9"}, IssueContent: types.IssueContent{Title: "Zulu"}, IssueMeta: types.IssueMeta{ExternalRef: &ref}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "test-8"}, IssueContent: types.IssueContent{Title: "Yankee"}}), "a"))

	// ID-shaped token hits the external_ref branch (LOWER(external_ref) LIKE).
	got, err := s.SearchIssues(c, "jira-12", types.IssueFilter{})
	must(t, err)
	if ids := issueIDs(got); !reflect.DeepEqual(ids, []string{"test-9"}) {
		t.Errorf("search 'jira-12' = %v, want [test-9]", ids)
	}

	// ID-shaped token that equals the (lowercase) id hits the id = ? branch.
	got, err = s.SearchIssues(c, "test-9", types.IssueFilter{})
	must(t, err)
	if ids := issueIDs(got); !reflect.DeepEqual(ids, []string{"test-9"}) {
		t.Errorf("search 'test-9' = %v, want [test-9]", ids)
	}
}

// LIKE over raw-cased operands is case-sensitive on both implementations: Dolt uses
// a binary table collation, and SQLite set PRAGMA case_sensitive_like (formerly sqlite/dsn.go).
// SQLite's default LIKE is
// ASCII-case-insensitive and silently diverged (bd-oyvc2.10). IDPrefix filtering
// (sqlbuild/filter.go `id LIKE ?`) must therefore distinguish test-AB from test-ab.
func testAuditSearchIDPrefixCaseSensitive(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "test-AB1"}, IssueContent: types.IssueContent{Title: "Upper"}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "test-ab2"}, IssueContent: types.IssueContent{Title: "Lower"}}), "a"))

	got, err := s.SearchIssues(c, "", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{IDPrefix: "test-AB"}})
	must(t, err)
	if ids := issueIDs(got); !reflect.DeepEqual(ids, []string{"test-AB1"}) {
		t.Errorf("IDPrefix test-AB = %v, want [test-AB1] (LIKE must be case-sensitive)", ids)
	}

	got, err = s.SearchIssues(c, "", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{IDPrefix: "test-ab"}})
	must(t, err)
	if ids := issueIDs(got); !reflect.DeepEqual(ids, []string{"test-ab2"}) {
		t.Errorf("IDPrefix test-ab = %v, want [test-ab2] (LIKE must be case-sensitive)", ids)
	}
}

// The ParentID descendant branch (sqlbuild/filter.go `id LIKE CONCAT(?, '.%')`)
// is likewise case-sensitive: with two sibling parents differing only by case,
// each with a dotted child and no parent-child dep rows, listing one parent's
// descendants must not leak the other-cased parent's child (bd-oyvc2.10).
func testAuditSearchParentDescendantCaseSensitive(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "test-pc"}, IssueContent: types.IssueContent{Title: "lower parent"}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "test-PC"}, IssueContent: types.IssueContent{Title: "upper parent"}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "test-pc.1"}, IssueContent: types.IssueContent{Title: "lower child"}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "test-PC.1"}, IssueContent: types.IssueContent{Title: "upper child"}}), "a"))

	parent := "test-pc"
	got, err := s.SearchIssues(c, "", types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{ParentID: &parent}})
	must(t, err)
	if ids := issueIDs(got); !reflect.DeepEqual(ids, []string{"test-pc.1"}) {
		t.Errorf("ParentID test-pc = %v, want [test-pc.1] (different-cased sibling's child must be excluded)", ids)
	}

	upper := "test-PC"
	got, err = s.SearchIssues(c, "", types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{ParentID: &upper}})
	must(t, err)
	if ids := issueIDs(got); !reflect.DeepEqual(ids, []string{"test-PC.1"}) {
		t.Errorf("ParentID test-PC = %v, want [test-PC.1] (different-cased sibling's child must be excluded)", ids)
	}
}

// A non-empty filter.Status REPLACES the default open+in_progress set entirely,
// so Status="closed" returns aged closed issues. Ephemeral rows are always
// excluded (the query hits only the issues table).
func testAuditStaleStatusOverride(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	y2020 := auditWholeSec(2020)
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "sa-open"}, IssueContent: types.IssueContent{Title: "aged open"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen}, IssueTimes: types.IssueTimes{CreatedAt: y2020, UpdatedAt: y2020}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "sa-closed"}, IssueContent: types.IssueContent{Title: "aged closed"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusClosed}, IssueTimes: types.IssueTimes{CreatedAt: y2020, UpdatedAt: y2020}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "sa-eph"}, IssueContent: types.IssueContent{Title: "aged ephemeral"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen}, IssueTimes: types.IssueTimes{CreatedAt: y2020, UpdatedAt: y2020}, IssueWisp: types.IssueWisp{Ephemeral: true}}), "a"))

	// Status override: the normally-excluded closed issue is returned.
	closed, err := s.GetStaleIssues(c, types.StaleFilter{Days: 30, Status: "closed"})
	must(t, err)
	if got := orderedIDs(closed); !reflect.DeepEqual(got, []string{"sa-closed"}) {
		t.Errorf("stale(status=closed) = %v, want [sa-closed]", got)
	}

	// Default (open+in_progress): the aged open issue, never the ephemeral one.
	def, err := s.GetStaleIssues(c, types.StaleFilter{Days: 30})
	must(t, err)
	if got := orderedIDs(def); !reflect.DeepEqual(got, []string{"sa-open"}) {
		t.Errorf("stale(default) = %v, want [sa-open] (ephemeral excluded)", got)
	}
}

// Epics with ZERO children are silently skipped; epics WITH children are returned
// with Total/Closed counts and EligibleForClose = (Total>0 && Total==Closed).
func testAuditEpicsEligiblePartial(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "ee-1"}, IssueContent: types.IssueContent{Title: "E1"}, IssueWorkflow: types.IssueWorkflow{IssueType: types.TypeEpic, Status: types.StatusOpen}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "ee-1a"}, IssueContent: types.IssueContent{Title: "c"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusClosed}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "ee-1b"}, IssueContent: types.IssueContent{Title: "o"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "ee-2"}, IssueContent: types.IssueContent{Title: "E2"}, IssueWorkflow: types.IssueWorkflow{IssueType: types.TypeEpic, Status: types.StatusOpen}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "ee-2a"}, IssueContent: types.IssueContent{Title: "c"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusClosed}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "ee-3"}, IssueContent: types.IssueContent{Title: "E3"}, IssueWorkflow: types.IssueWorkflow{IssueType: types.TypeEpic, Status: types.StatusOpen}}), "a"))
	must(t, s.AddDependency(c, &types.Dependency{IssueID: "ee-1a", DependsOnID: "ee-1", Type: types.DepParentChild}, "a"))
	must(t, s.AddDependency(c, &types.Dependency{IssueID: "ee-1b", DependsOnID: "ee-1", Type: types.DepParentChild}, "a"))
	must(t, s.AddDependency(c, &types.Dependency{IssueID: "ee-2a", DependsOnID: "ee-2", Type: types.DepParentChild}, "a"))

	epics, err := s.GetEpicsEligibleForClosure(c)
	must(t, err)
	assertAuditEpicsEligible(t, epics)
}

func assertAuditEpicsEligible(t *testing.T, epics []*types.EpicStatus) {
	t.Helper()
	byID := make(map[string]*types.EpicStatus, len(epics))
	for _, e := range epics {
		if e != nil && e.Epic != nil {
			byID[e.Epic.ID] = e
		}
	}

	assertAuditEpicStatus(t, byID["ee-1"], 2, 1, false)
	assertAuditEpicStatus(t, byID["ee-2"], 1, 1, true)
	if _, present := byID["ee-3"]; present {
		t.Error("ee-3 (zero children) must be ABSENT (silent skip)")
	}
}

func assertAuditEpicStatus(t *testing.T, status *types.EpicStatus, total, closed int, eligible bool) {
	t.Helper()
	if status == nil {
		t.Fatalf("epic status missing (has children, must be returned)")
	}
	if status.TotalChildren != total || status.ClosedChildren != closed || status.EligibleForClose != eligible {
		t.Errorf("epic status = {Total:%d Closed:%d Eligible:%v}, want {%d %d %v}",
			status.TotalChildren, status.ClosedChildren, status.EligibleForClose, total, closed, eligible)
	}
}

// GetIssuesByLabel unions wisp_labels: an ephemeral (wisp) issue sharing the label
// IS included. Exact label equality; order is engine-dependent (assert as a set).
func testAuditGetIssuesByLabelWithWisp(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "gil-d1"}, IssueContent: types.IssueContent{Title: "d1"}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "gil-d2"}, IssueContent: types.IssueContent{Title: "d2"}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "gil-w1"}, IssueContent: types.IssueContent{Title: "w1"}, IssueWisp: types.IssueWisp{Ephemeral: true}}), "a"))
	must(t, s.AddLabel(c, "gil-d1", "shared", "a"))
	must(t, s.AddLabel(c, "gil-d2", "other", "a"))
	must(t, s.AddLabel(c, "gil-w1", "shared", "a"))

	issues, err := s.GetIssuesByLabel(c, "shared")
	must(t, err)
	if got := issueIDs(issues); !reflect.DeepEqual(got, []string{"gil-d1", "gil-w1"}) {
		t.Errorf("GetIssuesByLabel(shared) = %v, want {gil-d1 gil-w1} (wisp included, d2 excluded)", got)
	}
}

// The durable+wisp merge: default reads merge the wisps tier; SkipWisps counts the
// durable tier only; Ephemeral=&true routes to wisps; Ephemeral=&false returns
// durable non-ephemeral rows only.
func testAuditWispMergeSearchCount(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "wm-d1"}, IssueContent: types.IssueContent{Title: "D1"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen}}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{IssueID: types.IssueID{ID: "wm-w1"}, IssueContent: types.IssueContent{Title: "W1"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen}, IssueWisp: types.IssueWisp{Ephemeral: true}}), "a"))

	total, err := s.CountIssues(c, "", types.IssueFilter{})
	must(t, err)
	if total != 2 {
		t.Errorf("CountIssues (merged) = %d, want 2", total)
	}

	durOnly, err := s.CountIssues(c, "", types.IssueFilter{IssueFilterHydrate: types.IssueFilterHydrate{SkipWisps: true}})
	must(t, err)
	if durOnly != 1 {
		t.Errorf("CountIssues(SkipWisps) = %d, want 1", durOnly)
	}

	all, err := s.SearchIssues(c, "", types.IssueFilter{})
	must(t, err)
	if got := issueIDs(all); !reflect.DeepEqual(got, []string{"wm-d1", "wm-w1"}) {
		t.Errorf("SearchIssues (merged) = %v, want {wm-d1 wm-w1}", got)
	}

	yes := true
	onlyWisp, err := s.SearchIssues(c, "", types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{Ephemeral: &yes}})
	must(t, err)
	if got := issueIDs(onlyWisp); !reflect.DeepEqual(got, []string{"wm-w1"}) {
		t.Errorf("SearchIssues(Ephemeral=true) = %v, want [wm-w1]", got)
	}

	no := false
	onlyDur, err := s.SearchIssues(c, "", types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{Ephemeral: &no}})
	must(t, err)
	if got := issueIDs(onlyDur); !reflect.DeepEqual(got, []string{"wm-d1"}) {
		t.Errorf("SearchIssues(Ephemeral=false) = %v, want [wm-d1]", got)
	}

	byStatus, err := s.CountIssuesByGroup(c, types.IssueFilter{}, "status")
	must(t, err)
	if byStatus["open"] != 2 {
		t.Errorf("CountIssuesByGroup(status) open = %d, want 2 (merged)", byStatus["open"])
	}
}
