package issueops

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// DiffInTx returns changes between two commits or branches by querying
// Dolt's dolt_diff() table function.
//
// nolint:gosec // G201: refs are validated by ValidateRef() - dolt_diff requires literal refs
func DiffInTx(ctx context.Context, tx *sql.Tx, fromRef, toRef string) ([]*storage.DiffEntry, error) {
	if err := ValidateRef(fromRef); err != nil {
		return nil, fmt.Errorf("invalid fromRef: %w", err)
	}
	if err := ValidateRef(toRef); err != nil {
		return nil, fmt.Errorf("invalid toRef: %w", err)
	}

	query := fmt.Sprintf(`
		SELECT
			COALESCE(from_id, '') as from_id,
			COALESCE(to_id, '') as to_id,
			diff_type,
			from_title, to_title,
			from_description, to_description,
			from_status, to_status,
			from_priority, to_priority
		FROM dolt_diff('%s', '%s', 'issues')
	`, fromRef, toRef)

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to get diff: %w", err)
	}
	defer rows.Close()

	var entries []*storage.DiffEntry
	for rows.Next() {
		row, err := scanDiffRow(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, diffEntry(row))
	}

	return entries, rows.Err()
}

type diffRow struct {
	fromID, toID string
	diffType     string
	old, new     diffIssueValues
}

type diffIssueValues struct {
	title, description, status *string
	priority                   *int
}

func scanDiffRow(rows *sql.Rows) (diffRow, error) {
	var row diffRow
	if err := rows.Scan(
		&row.fromID, &row.toID, &row.diffType,
		&row.old.title, &row.new.title,
		&row.old.description, &row.new.description,
		&row.old.status, &row.new.status,
		&row.old.priority, &row.new.priority,
	); err != nil {
		return diffRow{}, fmt.Errorf("failed to scan diff: %w", err)
	}
	return row, nil
}

func diffEntry(row diffRow) *storage.DiffEntry {
	issueID := row.fromID
	if row.toID != "" {
		issueID = row.toID
	}
	entry := &storage.DiffEntry{IssueID: issueID, DiffType: row.diffType}
	if row.diffType != "added" {
		entry.OldValue = diffIssue(row.fromID, row.old)
	}
	if row.diffType != "removed" {
		entry.NewValue = diffIssue(row.toID, row.new)
	}
	return entry
}

func diffIssue(id string, values diffIssueValues) *types.Issue {
	if id == "" {
		return nil
	}
	issue := &types.Issue{IssueID: types.IssueID{ID: id}}
	if values.title != nil {
		issue.Title = *values.title
	}
	if values.description != nil {
		issue.Description = *values.description
	}
	if values.status != nil {
		issue.Status = types.Status(*values.status)
	}
	if values.priority != nil {
		issue.Priority = *values.priority
	}
	return issue
}
