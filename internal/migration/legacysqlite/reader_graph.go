//go:build cgo

// Package legacysqlite reads the small, authenticated SQLite history that
// predates the current Dolt store. It intentionally has no general migration
// registry: each accepted layout is an exact, audited contract.
package legacysqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/jonbaldie/beads/internal/types"
)

func loadChildren(ctx context.Context, db *sql.Tx, issues []*types.Issue) error {
	byID := map[string]*types.Issue{}
	for _, i := range issues {
		byID[i.ID] = i
	}
	if err := loadLabels(ctx, db, byID); err != nil {
		return err
	}
	if err := loadDependencies(ctx, db, byID); err != nil {
		return err
	}
	if err := validateDependencyGraph(issues); err != nil {
		return err
	}
	return loadComments(ctx, db, byID)
}

func loadLabels(ctx context.Context, db *sql.Tx, byID map[string]*types.Issue) error {
	labels, err := db.QueryContext(ctx, "SELECT issue_id,label FROM labels ORDER BY issue_id,label")
	if err != nil {
		return err
	}
	defer labels.Close()
	for labels.Next() {
		var id, label string
		if err := labels.Scan(&id, &label); err != nil {
			return err
		}
		if err := checkUTF8(
			currentString{"label issue_id", id},
			currentString{"label", label},
		); err != nil {
			return err
		}
		if err := checkCurrentVarchars(
			currentVarchar{"label issue_id", id, types.MaxFieldLen},
			currentVarchar{"label", label, types.MaxFieldLen},
		); err != nil {
			return err
		}
		issue := byID[id]
		if issue == nil {
			return fmt.Errorf("orphan label for %s", id)
		}
		issue.Labels = append(issue.Labels, label)
	}
	return labels.Err()
}

func loadDependencies(ctx context.Context, db *sql.Tx, byID map[string]*types.Issue) error {
	deps, err := db.QueryContext(ctx, "SELECT issue_id,depends_on_id,type,CAST(created_at AS TEXT),created_by,metadata,thread_id FROM dependencies ORDER BY issue_id,depends_on_id,type")
	if err != nil {
		return err
	}
	defer deps.Close()
	seenDeps := make(map[string]bool)
	for deps.Next() {
		if err := appendLegacyDependencyRow(deps, byID, seenDeps); err != nil {
			return err
		}
	}
	return deps.Err()
}

// appendLegacyDependencyRow validates one legacy dependencies row and appends
// the resulting edge to its owning issue. seenDeps carries the per-load
// (issue_id, depends_on_id) set so duplicate legacy edges are rejected across
// rows.
func appendLegacyDependencyRow(deps *sql.Rows, byID map[string]*types.Issue, seenDeps map[string]bool) error {
	row, err := scanLegacyDependencyRow(deps)
	if err != nil {
		return err
	}
	issue, created, err := validateLegacyDependencyRow(row, byID, seenDeps)
	if err != nil {
		return err
	}
	issue.Dependencies = append(issue.Dependencies, &types.Dependency{
		IssueID: row.id, DependsOnID: row.to, Type: types.DependencyType(row.typ),
		CreatedAt: created, CreatedBy: row.by, Metadata: nullString(row.metadata), ThreadID: nullString(row.thread),
	})
	return nil
}

type legacyDependencyRow struct {
	id, to, typ, at, by string
	metadata, thread    sql.NullString
}

func scanLegacyDependencyRow(rows *sql.Rows) (legacyDependencyRow, error) {
	var row legacyDependencyRow
	err := rows.Scan(&row.id, &row.to, &row.typ, &row.at, &row.by, &row.metadata, &row.thread)
	return row, err
}

func validateLegacyDependencyRow(row legacyDependencyRow, byID map[string]*types.Issue, seenDeps map[string]bool) (*types.Issue, time.Time, error) {
	if err := validateLegacyDependencyValues(row); err != nil {
		return nil, time.Time{}, err
	}
	issue, err := legacyDependencyIssue(row, byID)
	if err != nil {
		return nil, time.Time{}, err
	}
	key := row.id + "\x00" + row.to
	if seenDeps[key] {
		return nil, time.Time{}, fmt.Errorf("multiple legacy dependencies for %s -> %s", row.id, row.to)
	}
	seenDeps[key] = true
	created, err := parseTime(row.at)
	if err != nil {
		return nil, time.Time{}, err
	}
	if created.IsZero() {
		return nil, time.Time{}, fmt.Errorf("dependency created_at is zero for %s -> %s", row.id, row.to)
	}
	if err := validateLegacyDependencyTail(row); err != nil {
		return nil, time.Time{}, err
	}
	return issue, created, nil
}

func validateLegacyDependencyValues(row legacyDependencyRow) error {
	if err := checkUTF8(
		currentString{"dependency issue_id", row.id},
		currentString{"dependency depends_on_id", row.to},
		currentString{"dependency type", row.typ},
		currentString{"dependency created_by", row.by},
	); err != nil {
		return err
	}
	if err := checkCurrentVarchars(
		currentVarchar{"dependency issue_id", row.id, types.MaxFieldLen},
		currentVarchar{"dependency depends_on_id", row.to, types.MaxFieldLen},
		currentVarchar{"dependency type", row.typ, currentShortVarcharRunes},
		currentVarchar{"dependency created_by", row.by, types.MaxFieldLen},
	); err != nil {
		return err
	}
	if row.by == "" {
		return fmt.Errorf("dependency created_by is empty for %s -> %s", row.id, row.to)
	}
	return nil
}

func legacyDependencyIssue(row legacyDependencyRow, byID map[string]*types.Issue) (*types.Issue, error) {
	issue := byID[row.id]
	if issue == nil || byID[row.to] == nil {
		return nil, fmt.Errorf("orphan dependency %s -> %s", row.id, row.to)
	}
	if issue.Ephemeral != byID[row.to].Ephemeral {
		return nil, fmt.Errorf("dependency %s -> %s crosses ephemeral storage", row.id, row.to)
	}
	return issue, nil
}

func validateLegacyDependencyTail(row legacyDependencyRow) error {
	if (row.metadata.Valid && row.metadata.String != "") || (row.thread.Valid && row.thread.String != "") {
		return fmt.Errorf("dependency %s -> %s uses unsupported metadata or thread ID", row.id, row.to)
	}
	if !types.DependencyType(row.typ).IsValid() {
		return fmt.Errorf("dependency %s -> %s has invalid type", row.id, row.to)
	}
	return nil
}

func loadComments(ctx context.Context, db *sql.Tx, byID map[string]*types.Issue) error {
	comments, err := db.QueryContext(ctx, "SELECT id,issue_id,author,text,CAST(created_at AS TEXT) FROM comments ORDER BY issue_id,created_at,id")
	if err != nil {
		return err
	}
	defer comments.Close()
	seenComments := make(map[legacyCommentIdentity]int64)
	for comments.Next() {
		if err := appendLegacyCommentRow(comments, byID, seenComments); err != nil {
			return err
		}
	}
	return comments.Err()
}

type legacyCommentRow struct {
	id int64
	legacyCommentIdentity
	at string
}

type legacyCommentIdentity struct {
	issueID, author, text string
	createdAt             time.Time
}

func appendLegacyCommentRow(rows *sql.Rows, byID map[string]*types.Issue, seen map[legacyCommentIdentity]int64) error {
	row, err := scanLegacyCommentRow(rows)
	if err != nil {
		return err
	}
	issue, created, err := validateLegacyCommentRow(row, byID, seen)
	if err != nil {
		return err
	}
	row.createdAt = created
	seen[row.legacyCommentIdentity] = row.id
	issue.Comments = append(issue.Comments, &types.Comment{
		ID: strconv.FormatInt(row.id, 10), IssueID: row.issueID, Author: row.author,
		Text: row.text, CreatedAt: row.createdAt,
	})
	return nil
}

func scanLegacyCommentRow(rows *sql.Rows) (legacyCommentRow, error) {
	var row legacyCommentRow
	err := rows.Scan(&row.id, &row.issueID, &row.author, &row.text, &row.at)
	return row, err
}

func validateLegacyCommentRow(row legacyCommentRow, byID map[string]*types.Issue, seen map[legacyCommentIdentity]int64) (*types.Issue, time.Time, error) {
	if err := checkUTF8(
		currentString{"comment issue_id", row.issueID},
		currentString{"comment author", row.author},
		currentString{"comment text", row.text},
	); err != nil {
		return nil, time.Time{}, err
	}
	if err := checkCurrentVarchars(
		currentVarchar{"comment issue_id", row.issueID, types.MaxFieldLen},
		currentVarchar{"comment author", row.author, types.MaxFieldLen},
	); err != nil {
		return nil, time.Time{}, err
	}
	issue := byID[row.issueID]
	if issue == nil {
		return nil, time.Time{}, fmt.Errorf("orphan comment for %s", row.issueID)
	}
	if issue.Ephemeral && len(row.text) > currentTextBytes {
		return nil, time.Time{}, fmt.Errorf("legacy SQLite ephemeral comment text is %d bytes (current TEXT maximum %d)", len(row.text), currentTextBytes)
	}
	created, err := parseTime(row.at)
	if err != nil {
		return nil, time.Time{}, err
	}
	if created.IsZero() {
		return nil, time.Time{}, fmt.Errorf("comment created_at is zero for issue %s", row.issueID)
	}
	row.createdAt = created
	if priorID, exists := seen[row.legacyCommentIdentity]; exists {
		return nil, time.Time{}, fmt.Errorf("legacy SQLite comments %d and %d share current import identity", priorID, row.id)
	}
	return issue, created, nil
}

func validateDependencyGraph(issues []*types.Issue) error {
	scheduling, hierarchy, blocking, err := buildDependencyGraphs(issues)
	if err != nil {
		return err
	}
	if hasDirectedCycle(scheduling) {
		return fmt.Errorf("legacy SQLite dependency graph has a scheduling cycle")
	}
	return validateBlockingDependencies(hierarchy, blocking)
}

func buildDependencyGraphs(issues []*types.Issue) (map[string][]string, map[string][]string, []*types.Dependency, error) {
	scheduling := make(map[string][]string)
	hierarchy := make(map[string][]string)
	var blocking []*types.Dependency
	for _, issue := range issues {
		for _, dep := range issue.Dependencies {
			if dep.IssueID == dep.DependsOnID {
				return nil, nil, nil, fmt.Errorf("dependency %s -> %s is a self-dependency", dep.IssueID, dep.DependsOnID)
			}
			blocking = appendDependencyGraphEdge(dep, scheduling, hierarchy, blocking)
		}
	}
	return scheduling, hierarchy, blocking, nil
}

func appendDependencyGraphEdge(dep *types.Dependency, scheduling, hierarchy map[string][]string, blocking []*types.Dependency) []*types.Dependency {
	switch dep.Type {
	case types.DepBlocks, types.DepConditionalBlocks:
		blocking = append(blocking, dep)
		scheduling[dep.IssueID] = append(scheduling[dep.IssueID], dep.DependsOnID)
	case types.DepParentChild:
		hierarchy[dep.IssueID] = append(hierarchy[dep.IssueID], dep.DependsOnID)
		scheduling[dep.IssueID] = append(scheduling[dep.IssueID], dep.DependsOnID)
	}
	return blocking
}

func validateBlockingDependencies(hierarchy map[string][]string, blocking []*types.Dependency) error {
	for _, dep := range blocking {
		if blockingDependencyConflictsWithHierarchy(hierarchy, dep) {
			return fmt.Errorf("blocking dependency %s -> %s conflicts with parent-child hierarchy", dep.IssueID, dep.DependsOnID)
		}
	}
	return nil
}

func blockingDependencyConflictsWithHierarchy(hierarchy map[string][]string, dep *types.Dependency) bool {
	if types.ExtractPrefix(dep.IssueID) != types.ExtractPrefix(dep.DependsOnID) {
		return false
	}
	return reachable(hierarchy, dep.IssueID, dep.DependsOnID) || reachable(hierarchy, dep.DependsOnID, dep.IssueID)
}

func hasDirectedCycle(graph map[string][]string) bool {
	indegree := make(map[string]int, len(graph))
	for from, targets := range graph {
		if _, ok := indegree[from]; !ok {
			indegree[from] = 0
		}
		for _, target := range targets {
			indegree[target]++
		}
	}
	queue := make([]string, 0, len(indegree))
	for id, degree := range indegree {
		if degree == 0 {
			queue = append(queue, id)
		}
	}
	visited := 0
	queueLen := len(queue)
	for head := 0; head < queueLen; head++ {
		id := queue[head]
		visited++
		for _, target := range graph[id] {
			indegree[target]--
			if indegree[target] == 0 {
				queue = append(queue, target)
				queueLen++
			}
		}
	}
	return visited != len(indegree)
}

func reachable(graph map[string][]string, start, target string) bool {
	seen := map[string]bool{start: true}
	queue := []string{start}
	queueLen := len(queue)
	for head := 0; head < queueLen; head++ {
		node := queue[head]
		for _, next := range graph[node] {
			if next == target {
				return true
			}
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
				queueLen++
			}
		}
	}
	return false
}
