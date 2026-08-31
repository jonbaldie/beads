package issueops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/types"
)

func sortReadyIssues(issues []*types.Issue, policy types.SortPolicy) {
	recentCutoff := time.Now().UTC().Add(-48 * time.Hour)
	sort.SliceStable(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		switch policy {
		case types.SortPolicyOldest:
			return issueCreatedBefore(a, b)
		case types.SortPolicyPriority:
			return issuePriorityBefore(a, b)
		case types.SortPolicyHybrid, "":
			aRecent := !a.CreatedAt.Before(recentCutoff)
			bRecent := !b.CreatedAt.Before(recentCutoff)
			if aRecent != bRecent {
				return aRecent
			}
			if aRecent && a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
			return issueCreatedBefore(a, b)
		default:
			return issuePriorityBefore(a, b)
		}
	})
}

func issuePriorityBefore(a, b *types.Issue) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

func issueCreatedBefore(a, b *types.Issue) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

func queryReadyIssueIDPage(ctx context.Context, tx DBTX, query string, args []interface{}) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get ready work: %w", err)
	}

	var issueIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("get ready work: scan id: %w", err)
		}
		issueIDs = append(issueIDs, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get ready work: rows: %w", err)
	}
	return issueIDs, nil
}

// getChildrenOfDeferredParentsInTx returns IDs of issues whose parent has a
// future defer_until. Works within an existing transaction.
//
//nolint:gosec // G201: depTable is selected from a hardcoded list below.
func getChildrenOfDeferredParentsInTx(ctx context.Context, tx DBTX) ([]string, error) {
	hasDeferredParent, err := hasFutureDeferredParentInTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if !hasDeferredParent {
		return nil, nil
	}

	var childIDs []string
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		ids, err := readDeferredChildrenFromDependencyTable(ctx, tx, depTable)
		if err != nil {
			return nil, err
		}
		childIDs = append(childIDs, ids...)
	}
	return childIDs, nil
}

//nolint:gosec // G201: issueTable is hardcoded to "issues" or "wisps".
func hasFutureDeferredParentInTx(ctx context.Context, tx DBTX) (bool, error) {
	for _, issueTable := range []string{"issues", "wisps"} {
		var exists int
		err := tx.QueryRowContext(ctx, fmt.Sprintf(`
			SELECT 1 FROM %s
			WHERE defer_until IS NOT NULL
			  AND defer_until > UTC_TIMESTAMP()
			LIMIT 1
		`, issueTable)).Scan(&exists)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, sql.ErrNoRows) || (issueTable == "wisps" && isTableNotExistError(err)) {
			continue
		}
		return false, fmt.Errorf("deferred parents: check future-deferred parents from %s: %w", issueTable, err)
	}
	return false, nil
}

//nolint:gosec // G201: depTable and issueTable are selected from fixed values.
func readDeferredChildrenFromDependencyTable(ctx context.Context, tx DBTX, depTable string) ([]string, error) {
	var childIDs []string
	for _, issueTable := range []string{"issues", "wisps"} {
		targetCol := "depends_on_issue_id"
		if issueTable == "wisps" {
			targetCol = "depends_on_wisp_id"
		}
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
			SELECT dep.issue_id
			FROM %s dep
			JOIN %s parent ON parent.id = dep.%s
			WHERE dep.type = 'parent-child'
			  AND parent.defer_until IS NOT NULL
			  AND parent.defer_until > UTC_TIMESTAMP()
		`, depTable, issueTable, targetCol))
		if err != nil {
			if depTable == "wisp_dependencies" && isTableNotExistError(err) {
				break
			}
			if issueTable == "wisps" && isTableNotExistError(err) {
				continue
			}
			return nil, fmt.Errorf("deferred parents: get deferred children from %s/%s: %w", depTable, issueTable, err)
		}
		ids, err := scanDeferredChildIDs(rows, depTable, issueTable)
		if err != nil {
			return nil, err
		}
		childIDs = append(childIDs, ids...)
	}
	return childIDs, nil
}

func scanDeferredChildIDs(rows *sql.Rows, depTable, issueTable string) ([]string, error) {
	var childIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("deferred parents: scan deferred child from %s/%s: %w", depTable, issueTable, err)
		}
		childIDs = append(childIDs, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("deferred parents: child rows from %s/%s: %w", depTable, issueTable, err)
	}
	return childIDs, nil
}

//nolint:gosec // G201: depTable is hardcoded to "dependencies" or "wisp_dependencies"
func getParentedIDSetInTx(ctx context.Context, tx DBTX, issueIDs []string) (map[string]struct{}, error) {
	parented := make(map[string]struct{})
	if len(issueIDs) == 0 {
		return parented, nil
	}
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		ids, err := readParentedIDsFromTableInTx(ctx, tx, depTable, issueIDs)
		if err != nil {
			if depTable == "wisp_dependencies" && isTableNotExistError(err) {
				break
			}
			return nil, err
		}
		for _, id := range ids {
			parented[id] = struct{}{}
		}
	}
	return parented, nil
}

//nolint:gosec // G201: depTable is hardcoded to "dependencies" or "wisp_dependencies".
func readParentedIDsFromTableInTx(ctx context.Context, tx DBTX, depTable string, issueIDs []string) ([]string, error) {
	var parented []string
	total := len(issueIDs)
	for start := 0; start < total; start += queryBatchSize {
		end := start + queryBatchSize
		if end > total {
			end = total
		}
		placeholders, args := buildSQLInClause(issueIDs[start:end])
		query := fmt.Sprintf(`
			SELECT issue_id FROM %s
			WHERE type = 'parent-child' AND issue_id IN (%s)
		`, depTable, placeholders)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("get parented IDs from %s: %w", depTable, err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("get parented IDs: scan: %w", err)
			}
			parented = append(parented, id)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get parented IDs: rows from %s: %w", depTable, err)
		}
	}
	return parented, nil
}

// buildSQLInClause builds a parameterized IN clause from a slice of IDs.
func buildSQLInClause(ids []string) (string, []interface{}) {
	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	return strings.Join(placeholders, ","), args
}
