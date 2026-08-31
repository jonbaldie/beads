package fixtures

import (
	"encoding/json"
	"errors"
	"math/rand"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonbaldie/beads/internal/types"
)

func TestFixtureConfigs(t *testing.T) {
	large := DefaultLargeConfig()
	if large.TotalIssues != 10000 || large.RandSeed != 42 {
		t.Fatalf("DefaultLargeConfig() = %+v", large)
	}
	xlarge := DefaultXLargeConfig()
	if xlarge.TotalIssues != 20000 || xlarge.RandSeed != 43 {
		t.Fatalf("DefaultXLargeConfig() = %+v", xlarge)
	}
}

func TestNewFixtureIssueOpenAndClosed(t *testing.T) {
	base := DataConfig{OpenRatio: 1, MaxClosedAgeDays: 5}
	open := newFixtureIssue("title", "description", types.TypeTask, 5, base, rand.New(rand.NewSource(1))) // #nosec G404 -- deterministic test data
	if open.Title != "title" || open.Description != "description" || open.IssueType != types.TypeTask {
		t.Fatalf("open fixture = %+v", open)
	}
	if open.Status == types.StatusClosed || open.ClosedAt != nil || open.Assignee == "" {
		t.Fatalf("open fixture workflow = %+v", open)
	}
	if open.CreatedAt.After(open.UpdatedAt) || open.CreatedAt.Before(time.Now().Add(-6*24*time.Hour)) {
		t.Fatalf("open fixture timestamps = created %v, updated %v", open.CreatedAt, open.UpdatedAt)
	}

	base.OpenRatio = 0
	closed := newFixtureIssue("closed", "done", types.TypeFeature, 5, base, rand.New(rand.NewSource(2))) // #nosec G404 -- deterministic test data
	if closed.Status != types.StatusClosed || closed.ClosedAt == nil {
		t.Fatalf("closed fixture = %+v", closed)
	}
}

func TestParseFixtureIssuesAndSplitLines(t *testing.T) {
	first := &types.Issue{IssueID: types.IssueID{ID: "bd-1"}, IssueContent: types.IssueContent{Title: "first"}}
	second := &types.Issue{IssueID: types.IssueID{ID: "bd-2"}, IssueContent: types.IssueContent{Title: "second"}}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	data := append(append(append(firstJSON, '\n', '\n'), secondJSON...), '\n')
	issues, err := parseFixtureIssues(data)
	if err != nil {
		t.Fatalf("parseFixtureIssues: %v", err)
	}
	if len(issues) != 2 || issues[0].ID != "bd-1" || issues[1].ID != "bd-2" {
		t.Fatalf("parsed issues = %+v", issues)
	}
	if got := splitLines("one\n\nthree"); !slices.Equal(got, []string{"one", "", "three"}) {
		t.Fatalf("splitLines = %#v", got)
	}
	if got := splitLines(""); len(got) != 0 {
		t.Fatalf("splitLines(empty) = %#v", got)
	}

	_, err = parseFixtureIssues([]byte("\n{broken"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("malformed fixture error = %v", err)
	}
}

func TestReadFixtureJSONLAndDependencyErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.jsonl")
	if _, err := readFixtureJSONL(missing); err == nil || !strings.Contains(err.Error(), "failed to read JSONL file") {
		t.Fatalf("readFixtureJSONL(missing) = %v", err)
	}
	for _, message := range []string{"dependency already exists", "dependency would create cycle"} {
		if !isIgnorableFixtureDependencyError(errors.New(message)) {
			t.Errorf("%q should be ignorable", message)
		}
	}
	if isIgnorableFixtureDependencyError(errors.New("permission denied")) {
		t.Fatal("unrelated dependency error was ignored")
	}
}

func TestGenerationProgressThreshold(t *testing.T) {
	progress := generationProgress{total: 100, created: 9, lastPercent: 0}
	progress.note()
	if progress.lastPercent != 0 {
		t.Fatalf("progress below threshold advanced to %d", progress.lastPercent)
	}
	progress.created = 10
	progress.note()
	if progress.lastPercent != 10 {
		t.Fatalf("progress at threshold advanced to %d, want 10", progress.lastPercent)
	}
}
