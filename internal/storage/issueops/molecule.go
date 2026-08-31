package issueops

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/types"
)

// GetMoleculeProgressInTx returns progress stats for a molecule within an
// existing transaction. Routes to the correct table (issues/wisps) automatically.
//
//nolint:gosec // G201: table names come from WispTableRouting (hardcoded constants)
func GetMoleculeProgressInTx(ctx context.Context, tx *sql.Tx, moleculeID string) (*types.MoleculeProgressStats, error) {
	stats := &types.MoleculeProgressStats{
		MoleculeID: moleculeID,
	}

	isWisp := IsActiveWispInTx(ctx, tx, moleculeID)
	issueTable, _, _, depTable := WispTableRouting(isWisp)
	parentCol := "depends_on_issue_id"
	if isWisp {
		parentCol = "depends_on_wisp_id"
	}
	setMoleculeTitle(ctx, tx, issueTable, moleculeID, stats)
	childIDs, err := moleculeChildIDs(ctx, tx, depTable, parentCol, moleculeID)
	if err != nil {
		return nil, err
	}
	childStatuses, err := moleculeChildStatuses(ctx, tx, issueTable, childIDs)
	if err != nil {
		return nil, err
	}
	applyMoleculeProgress(stats, childIDs, childStatuses)
	return stats, nil
}

func setMoleculeTitle(ctx context.Context, tx *sql.Tx, issueTable, moleculeID string, stats *types.MoleculeProgressStats) {
	var title sql.NullString
	err := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT title FROM %s WHERE id = ?", issueTable), moleculeID).Scan(&title)
	if err == nil && title.Valid {
		stats.MoleculeTitle = title.String
	}
}

func moleculeChildIDs(ctx context.Context, tx *sql.Tx, depTable, parentCol, moleculeID string) ([]string, error) {
	depRows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT issue_id FROM %s
		WHERE %s = ? AND type = 'parent-child'
	`, depTable, parentCol), moleculeID)
	if err != nil {
		return nil, fmt.Errorf("failed to get molecule children: %w", err)
	}
	var childIDs []string
	for depRows.Next() {
		var id string
		if err := depRows.Scan(&id); err != nil {
			_ = depRows.Close()
			return nil, fmt.Errorf("get molecule progress: scan child: %w", err)
		}
		childIDs = append(childIDs, id)
	}
	_ = depRows.Close()
	if err := depRows.Err(); err != nil {
		return nil, fmt.Errorf("get molecule progress: child rows: %w", err)
	}
	return childIDs, nil
}

func moleculeChildStatuses(ctx context.Context, tx *sql.Tx, issueTable string, childIDs []string) (map[string]string, error) {
	childMap := make(map[string]string, len(childIDs))
	totalChildIDs := len(childIDs)
	for start := 0; start < totalChildIDs; start += queryBatchSize {
		end := start + queryBatchSize
		if end > totalChildIDs {
			end = totalChildIDs
		}
		batch := childIDs[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}
		statusRows, err := tx.QueryContext(ctx, fmt.Sprintf("SELECT id, status FROM %s WHERE id IN (%s)", issueTable, strings.Join(placeholders, ",")), args...)
		if err != nil {
			return nil, fmt.Errorf("failed to batch-fetch child statuses: %w", err)
		}
		for statusRows.Next() {
			var id, status string
			if err := statusRows.Scan(&id, &status); err != nil {
				_ = statusRows.Close()
				return nil, fmt.Errorf("get molecule progress: scan status: %w", err)
			}
			childMap[id] = status
		}
		_ = statusRows.Close()
	}
	return childMap, nil
}

func applyMoleculeProgress(stats *types.MoleculeProgressStats, childIDs []string, childStatuses map[string]string) {
	for _, childID := range childIDs {
		status, ok := childStatuses[childID]
		if !ok {
			continue
		}
		stats.Total++
		switch types.Status(status) {
		case types.StatusClosed:
			stats.Completed++
		case types.StatusInProgress:
			stats.InProgress++
			if stats.CurrentStepID == "" {
				stats.CurrentStepID = childID
			}
		}
	}
}
