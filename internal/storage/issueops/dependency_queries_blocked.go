package issueops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jonbaldie/beads/internal/types"
)

// IsBlockedInTx checks if an issue is blocked by active dependencies within
// an existing transaction. Returns whether the issue is blocked and, if so,
// a list of blocker descriptions for display.
//
//nolint:gosec // G201: table names are hardcoded constants.
func IsBlockedInTx(ctx context.Context, tx DBTX, issueID string) (bool, []string, error) {
	found, blocked, err := loadStoredBlockedFlag(ctx, tx, issueID)
	if err != nil {
		return false, nil, err
	}
	if !found || !blocked {
		return false, nil, nil
	}

	edges, err := loadBlockedEdges(ctx, tx, issueID)
	if err != nil {
		return false, nil, err
	}
	if len(edges) == 0 {
		return true, nil, nil
	}

	blockerIDs := make([]string, 0, len(edges))
	for _, e := range edges {
		blockerIDs = append(blockerIDs, e.dependsOnID)
	}
	statusByID, err := loadStatusByIDInTx(ctx, tx, blockerIDs)
	if err != nil {
		return false, nil, fmt.Errorf("check blocker status: %w", err)
	}
	return true, activeBlockerDescriptions(edges, statusByID), nil
}

func loadStoredBlockedFlag(ctx context.Context, tx DBTX, issueID string) (bool, bool, error) {
	for _, table := range []string{"issues", "wisps"} {
		var blocked int
		//nolint:gosec // G201: table is a hardcoded "issues" or "wisps".
		err := tx.QueryRowContext(ctx, "SELECT is_blocked FROM "+table+" WHERE id = ?", issueID).Scan(&blocked)
		if err == nil {
			return true, blocked != 0, nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if optionalBlockedTable(table) && isTableNotExistError(err) {
			continue
		}
		return false, false, fmt.Errorf("read is_blocked from %s: %w", table, err)
	}
	return false, false, nil
}

type blockedDependencyEdge struct {
	dependsOnID, depType string
}

func loadBlockedEdges(ctx context.Context, tx DBTX, issueID string) ([]blockedDependencyEdge, error) {
	var edges []blockedDependencyEdge
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		tableEdges, err := readBlockedEdgesFromTable(ctx, tx, depTable, issueID)
		if err != nil {
			if optionalBlockedTable(depTable) && isTableNotExistError(err) {
				continue
			}
			return nil, fmt.Errorf("check blockers from %s: %w", depTable, err)
		}
		edges = append(edges, tableEdges...)
	}
	return edges, nil
}

//nolint:gosec // G201: depTable is hardcoded.
func readBlockedEdgesFromTable(ctx context.Context, tx DBTX, depTable, issueID string) ([]blockedDependencyEdge, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s AS depends_on_id, type FROM %s
		WHERE issue_id = ? AND type IN ('blocks', 'waits-for', 'conditional-blocks')
	`, DepTargetExpr, depTable), issueID)
	if err != nil {
		return nil, err
	}
	var edges []blockedDependencyEdge
	for rows.Next() {
		var edge blockedDependencyEdge
		if err := rows.Scan(&edge.dependsOnID, &edge.depType); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan blocker edge: %w", err)
		}
		edges = append(edges, edge)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("blocker edge rows from %s: %w", depTable, err)
	}
	return edges, nil
}

func activeBlockerDescriptions(edges []blockedDependencyEdge, statusByID map[string]types.Status) []string {
	var blockers []string
	for _, e := range edges {
		status, ok := statusByID[e.dependsOnID]
		if !ok {
			continue
		}
		if status == types.StatusClosed || status == types.StatusPinned {
			continue
		}
		if e.depType != "blocks" {
			blockers = append(blockers, e.dependsOnID+" ("+e.depType+")")
		} else {
			blockers = append(blockers, e.dependsOnID)
		}
	}

	return blockers
}

// IsBlockedBatchInTx returns the denormalized, TRANSITIVE is_blocked flag for
// each of ids in one batched read — the same value IsBlockedInTx returns per id,
// without the per-row blocker-list recompute. It reads the stored is_blocked
// column (SELECT id, is_blocked FROM {issues,wisps} WHERE id IN (...)), batched
// at queryBatchSize, so it reflects inherited/ancestor blockedness (a child of a
// blocked parent has is_blocked=1 with no direct blocking edge of its own) with
// no graph walk. ids missing from both tables are absent from the map (callers
// treat absent as not-blocked). On a cross-table id collision the ISSUES row wins,
// matching IsBlockedInTx exactly: the single read scans issues→wisps and breaks on
// the first table that has the id, so the batch keeps the first-seen (issues) value
// and skips any later (wisps) duplicate. The two reads share the is_blocked field,
// so they must resolve a collision identically — this is a data anomaly, but the
// single and batch reads would otherwise disagree on the same stored flag.
func IsBlockedBatchInTx(ctx context.Context, tx DBTX, ids []string) (map[string]bool, error) {
	blocked := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return blocked, nil
	}
	// De-dup by id across the two tables, keeping the first-seen (issues) value so
	// a cross-table collision resolves ISSUES-win, exactly as IsBlockedInTx does
	// (see above). The wisps table is optional, so a missing one is skipped —
	// same two-table read shape as GetDependentRecordsForIssuesInTx.
	seen := make(map[string]bool, len(ids))
	for _, table := range []string{"issues", "wisps"} {
		if err := readIsBlockedIntoFromTable(ctx, tx, table, ids, seen, blocked); err != nil {
			if optionalBlockedTable(table) && isTableNotExistError(err) {
				continue
			}
			return nil, err
		}
	}
	return blocked, nil
}

//nolint:gosec // G201: table is a hardcoded "issues" or "wisps"; placeholders are ? only.
func readIsBlockedIntoFromTable(ctx context.Context, tx DBTX, table string, ids []string, seen, blocked map[string]bool) error {
	total := len(ids)
	for start := 0; start < total; start += queryBatchSize {
		end := start + queryBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		placeholders, args := buildSQLInClause(ids[start:end])
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(
			"SELECT id, is_blocked FROM %s WHERE id IN (%s)", table, placeholders), args...)
		if err != nil {
			return fmt.Errorf("read is_blocked from %s: %w", table, err)
		}
		for rows.Next() {
			var id string
			var b int
			if err := rows.Scan(&id, &b); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan is_blocked from %s: %w", table, err)
			}
			// Keep the first-seen (issues) value and skip any later (wisps)
			// duplicate, so the batch is_blocked matches per-row IsBlocked.
			if seen[id] {
				continue
			}
			seen[id] = true
			blocked[id] = b != 0
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("is_blocked rows from %s: %w", table, err)
		}
	}
	return nil
}

// scanDependencyRow scans a single dependency row from a *sql.Rows.
func scanDependencyRow(rows *sql.Rows) (*types.Dependency, error) {
	var dep types.Dependency
	var createdAt sql.NullTime
	var metadata, threadID sql.NullString

	if err := rows.Scan(&dep.IssueID, &dep.DependsOnID, &dep.Type, &createdAt, &dep.CreatedBy, &metadata, &threadID); err != nil {
		return nil, fmt.Errorf("scan dependency: %w", err)
	}

	if createdAt.Valid {
		dep.CreatedAt = createdAt.Time
	}
	if metadata.Valid {
		dep.Metadata = metadata.String
	}
	if threadID.Valid {
		dep.ThreadID = threadID.String
	}

	return &dep, nil
}
