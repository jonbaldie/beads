// repro-dolt-prod-timeouts runs production-shaped bd CLI timeout scenarios.
//
// It initializes a real server-mode beads workspace, bulk-loads a graph that
// mirrors a large production deployment's skew (large mostly-closed issue table, large
// dependency table, small active frontier), then forks actual bd commands.
//
// Usage:
//
//	go run ./scripts/repro-dolt-prod-timeouts --bd ./bd --scenario all
package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jonbaldie/beads/internal/storage/depid"
)

func insertIssues(ctx context.Context, db *sql.DB, count, depOps, chainDepth int) error {
	const batchSize = 500
	total := count + depFixtureIssueCount(depOps, chainDepth)
	for start := 0; start < total; start += batchSize {
		end := minSeedEnd(start, batchSize, total)
		q, args := buildIssueInsertBatch(start, end, count)
		if _, err := db.ExecContext(ctx, q.String(), args...); err != nil {
			return fmt.Errorf("insert issues %d-%d: %w", start, end, err)
		}
	}
	return nil
}

func minSeedEnd(start, batchSize, total int) int {
	end := start + batchSize
	if end > total {
		return total
	}
	return end
}

func buildIssueInsertBatch(start, end, count int) (strings.Builder, []any) {
	var q strings.Builder
	q.WriteString(`INSERT INTO issues
		(id, title, description, design, acceptance_criteria, notes,
		 status, priority, issue_type, assignee, metadata)
		VALUES `)
	args := make([]any, 0, (end-start)*11)
	for i := start; i < end; i++ {
		if i > start {
			q.WriteByte(',')
		}
		q.WriteString("(?,?,?,?,?,?,?,?,?,?,?)")
		row := issueSeedRow(i, count)
		args = append(args, row.id, fmt.Sprintf("prod timeout issue %d", i), "fixture", "", "", "", row.status, row.priority, "task", row.assignee, row.metadata)
	}
	return q, args
}

type issueSeedRowValues struct {
	id, status, assignee, metadata string
	priority                       int
}

func issueSeedRow(i, count int) issueSeedRowValues {
	row := issueSeedRowValues{id: fmt.Sprintf("perf-%06d", i), status: "closed", metadata: "{}", priority: (i % 4) + 1}
	if i < 350 {
		row.status = "open"
	}
	if i < 40 {
		row.assignee = "example-org--control-dispatcher"
	}
	if i >= 40 && i < 80 {
		row.metadata = `{"route.routed_to":"example-org/control-dispatcher"}`
	}
	if i >= count {
		row.status = "open"
		row.id = depIssueID(i - count)
	}
	return row
}

func insertDepIssues(ctx context.Context, db *sql.DB, depOps, chainDepth int) error {
	const batchSize = 500
	total := depFixtureIssueCount(depOps, chainDepth)
	for start := 0; start < total; start += batchSize {
		end := minSeedEnd(start, batchSize, total)
		var q strings.Builder
		q.WriteString(`INSERT INTO issues
			(id, title, description, design, acceptance_criteria, notes,
			 status, priority, issue_type, assignee, metadata)
			VALUES `)
		args := make([]any, 0, (end-start)*11)
		for i := start; i < end; i++ {
			if i > start {
				q.WriteByte(',')
			}
			q.WriteString("(?,?,?,?,?,?,?,?,?,?,?)")
			args = append(args, depIssueID(i), fmt.Sprintf("prod copy dep issue %d", i), "fixture", "", "", "", "open", (i%4)+1, "task", "", "{}")
		}
		if _, err := db.ExecContext(ctx, q.String(), args...); err != nil {
			return fmt.Errorf("insert dep issues %d-%d: %w", start, end, err)
		}
	}
	return nil
}

func depFixtureIssueCount(depOps, chainDepth int) int {
	if chainDepth > 0 {
		return depOps*(chainDepth+3) + chainDepth + 2
	}
	return depOps * 2
}

func insertDependencies(ctx context.Context, db *sql.DB, count, issueCount int) error {
	const batchSize = 500
	if count <= 0 {
		return nil
	}
	maxPairs, err := maxDependencyPairs(issueCount)
	if err != nil {
		return err
	}
	if count > maxPairs {
		return fmt.Errorf("dependency count %d exceeds unique dependency pairs for %d issues", count, issueCount)
	}
	for start := 0; start < count; start += batchSize {
		end := minSeedEnd(start, batchSize, count)
		var q strings.Builder
		q.WriteString(`INSERT INTO dependencies
			(id, issue_id, depends_on_issue_id, type, created_by, metadata)
			VALUES `)
		args := make([]any, 0, (end-start)*6)
		for i := start; i < end; i++ {
			if i > start {
				q.WriteByte(',')
			}
			q.WriteString("(?,?,?,?,?,?)")
			issueID, dependsOnID, depType := dependencySeedRow(i, issueCount)
			args = append(args, depid.New(issueID, dependsOnID), issueID, dependsOnID, depType, "bench", "{}")
		}
		if _, err := db.ExecContext(ctx, q.String(), args...); err != nil {
			return fmt.Errorf("insert dependencies %d-%d: %w", start, end, err)
		}
	}
	return nil
}

func dependencySeedRow(i, issueCount int) (string, string, string) {
	issueID, dependsOnID := dependencyEndpoints(i, issueCount, 1000, 300)
	if i < 20 || (i >= 40 && i < 60) {
		issueID, dependsOnID = dependencyEndpoints(i, issueCount, 0, 200)
		return issueID, dependsOnID, "blocks"
	}
	if i < 5000 {
		return issueID, dependsOnID, "blocks"
	}
	return issueID, dependsOnID, "parent-child"
}

func maxDependencyPairs(issueCount int) (int, error) {
	if issueCount <= 0 {
		return 0, fmt.Errorf("issue count must be positive, got %d", issueCount)
	}
	if issueCount == 1 {
		return 1, nil
	}
	return issueCount * (issueCount - 1), nil
}

func dependencyEndpoints(i, issueCount, sourceOffset, targetOffset int) (string, string) {
	if issueCount == 1 {
		return perfIssueID(0), perfIssueID(0)
	}
	source := (i + sourceOffset) % issueCount
	round := i / issueCount
	offset := 1 + ((targetOffset - 1 + round) % (issueCount - 1))
	target := (source + offset) % issueCount
	return perfIssueID(source), perfIssueID(target)
}

func perfIssueID(i int) string {
	return fmt.Sprintf("perf-%06d", i)
}

func insertDepAddChains(ctx context.Context, db *sql.DB, ops, depth int) error {
	const batchSize = 500
	if depth <= 0 || ops <= 0 {
		return nil
	}

	total := ops * depth
	for start := 0; start < total; start += batchSize {
		end := start + batchSize
		if end > total {
			end = total
		}
		var q strings.Builder
		q.WriteString(`INSERT INTO dependencies
			(id, issue_id, depends_on_issue_id, type, created_by, metadata)
			VALUES `)
		args := make([]any, 0, (end-start)*6)
		for i := start; i < end; i++ {
			if i > start {
				q.WriteByte(',')
			}
			q.WriteString("(?,?,?,?,?,?)")
			op := i / depth
			step := i % depth
			base := depBase(op, depth)
			issueID := depIssueID(base + 1 + step)
			dependsOnID := depIssueID(base + 2 + step)
			args = append(args, depid.New(issueID, dependsOnID), issueID, dependsOnID, "blocks", "bench", "{}")
		}
		if _, err := db.ExecContext(ctx, q.String(), args...); err != nil {
			return fmt.Errorf("insert dep-add chains %d-%d: %w", start, end, err)
		}
	}
	return nil
}
