package issueops

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/types"
)

// GetStaleIssuesInTx returns issues that haven't been updated within the
// given number of days. Only non-ephemeral issues are considered. When
// filter.Status is empty, open and in_progress issues are returned.
// Results are ordered by updated_at ascending (stalest first).
//
// nolint:gosec // G201: statusClause contains only literal SQL or a single ? placeholder
func GetStaleIssuesInTx(ctx context.Context, tx DBTX, filter types.StaleFilter) ([]*types.Issue, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -filter.Days)
	ids, err := staleIssueIDs(ctx, tx, filter, cutoff)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	issues, err := GetIssuesByIDsInTx(ctx, tx, ids, nil)
	if err != nil {
		return nil, err
	}
	return orderStaleIssues(ids, issues), nil
}

func staleIssueIDs(ctx context.Context, tx DBTX, filter types.StaleFilter, cutoff time.Time) ([]string, error) {
	statusClause := "status IN ('open', 'in_progress')"
	if filter.Status != "" {
		statusClause = "status = ?"
	}

	// Heartbeats live in the ephemeral leases table and no longer stamp
	// issues.updated_at (bd-lrgn1), so an actively-worked claim can carry an
	// old updated_at: an issue with a heartbeat since the cutoff is not stale.
	query := fmt.Sprintf(`
		SELECT id FROM issues
		WHERE updated_at < ?
		  AND %s
		  AND (ephemeral = 0 OR ephemeral IS NULL)
		  AND NOT EXISTS (
			SELECT 1 FROM leases WHERE leases.issue_id = issues.id AND leases.heartbeat_at >= ?
		  )
		ORDER BY updated_at ASC
	`, statusClause)
	args := []interface{}{cutoff}
	if filter.Status != "" {
		args = append(args, filter.Status)
	}
	args = append(args, cutoff) // NOT EXISTS heartbeat cutoff, after any status arg

	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get stale issues: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(fmt.Errorf("failed to scan stale issue id: %w", err), rows.Close())
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("stale issues rows: %w", err), rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close stale issues rows: %w", err)
	}
	return ids, nil
}

func orderStaleIssues(ids []string, issues []*types.Issue) []*types.Issue {
	issueByID := make(map[string]*types.Issue, len(issues))
	for _, iss := range issues {
		if iss != nil {
			issueByID[iss.ID] = iss
		}
	}

	ordered := make([]*types.Issue, 0, len(ids))
	for _, id := range ids {
		if iss, ok := issueByID[id]; ok {
			ordered = append(ordered, iss)
		}
	}

	return ordered
}
