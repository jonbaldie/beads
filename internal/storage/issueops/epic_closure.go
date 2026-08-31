package issueops

import (
	"context"
	"errors"
	"fmt"

	"github.com/jonbaldie/beads/internal/types"
)

// GetEpicsEligibleForClosureInTx returns epics whose children are all closed.
// nolint:gosec // G201: table names are hardcoded, placeholders contain only ? markers
func GetEpicsEligibleForClosureInTx(ctx context.Context, tx DBTX) ([]*types.EpicStatus, error) {
	epicIDs, err := openEpicIDsInTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if len(epicIDs) == 0 {
		return nil, nil
	}

	epicChildMap, err := epicChildrenInTx(ctx, tx, epicIDs)
	if err != nil {
		return nil, err
	}
	allChildIDs := allEpicChildIDs(epicChildMap)
	childStatusMap, err := epicChildStatusesInTx(ctx, tx, allChildIDs)
	if err != nil {
		return nil, err
	}

	epicsWithChildren := epicsWithChildrenIDs(epicIDs, epicChildMap)
	epics, err := GetIssuesByIDsInTx(ctx, tx, epicsWithChildren, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to batch-fetch epic issues: %w", err)
	}
	epicIssueMap := make(map[string]*types.Issue, len(epics))
	for _, issue := range epics {
		epicIssueMap[issue.ID] = issue
	}
	return buildEpicClosureResults(epicIDs, epicChildMap, childStatusMap, epicIssueMap), nil
}

func openEpicIDsInTx(ctx context.Context, tx DBTX) ([]string, error) {
	// Step 1: Get open epic IDs (single-table scan)
	epicsRows, err := tx.QueryContext(ctx, `
		SELECT id FROM issues
		WHERE issue_type = 'epic'
		  AND status != 'closed'
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to get epics: %w", err)
	}
	var epicIDs []string
	for epicsRows.Next() {
		var id string
		if err := epicsRows.Scan(&id); err != nil {
			return nil, errors.Join(fmt.Errorf("scan epic id: %w", err), epicsRows.Close())
		}
		epicIDs = append(epicIDs, id)
	}
	if err := errors.Join(epicsRows.Err(), epicsRows.Close()); err != nil {
		return nil, fmt.Errorf("iterate epic ids: %w", err)
	}
	return epicIDs, nil
}

func epicChildrenInTx(ctx context.Context, tx DBTX, epicIDs []string) (map[string][]string, error) {
	// Step 2: Get parent-child dependencies from both tables (bd-w2w)
	// Wisp children store their parent-child deps in wisp_dependencies,
	// so we must check both tables to find all children of an epic.
	epicChildMap := make(map[string][]string)
	epics := make(map[string]bool, len(epicIDs))
	for _, id := range epicIDs {
		epics[id] = true
	}
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		err := appendEpicChildrenFromTable(ctx, tx, depTable, epics, epicChildMap)
		if err != nil {
			if optionalBlockedTable(depTable) && isTableNotExistError(err) {
				continue // wisp_dependencies may not exist on pre-migration databases
			}
			return nil, err
		}
	}
	return epicChildMap, nil
}

func appendEpicChildrenFromTable(ctx context.Context, tx DBTX, depTable string, epics map[string]bool, epicChildMap map[string][]string) error {
	depRows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s AS parent_id, issue_id FROM %s
		WHERE type = 'parent-child' AND %s IS NOT NULL
	`, DepTargetExpr, depTable, DepTargetExpr))
	if err != nil {
		return fmt.Errorf("failed to get parent-child deps from %s: %w", depTable, err)
	}
	for depRows.Next() {
		var parentID, childID string
		if err := depRows.Scan(&parentID, &childID); err != nil {
			return errors.Join(fmt.Errorf("scan parent-child dep from %s: %w", depTable, err), depRows.Close())
		}
		if epics[parentID] {
			epicChildMap[parentID] = append(epicChildMap[parentID], childID)
		}
	}
	if err := errors.Join(depRows.Err(), depRows.Close()); err != nil {
		return fmt.Errorf("iterate parent-child deps from %s: %w", depTable, err)
	}
	return nil
}

func allEpicChildIDs(epicChildMap map[string][]string) []string {
	allChildIDs := make([]string, 0)
	for _, children := range epicChildMap {
		allChildIDs = append(allChildIDs, children...)
	}
	return allChildIDs
}

func epicsWithChildrenIDs(epicIDs []string, epicChildMap map[string][]string) []string {
	epicsWithChildren := make([]string, 0)
	for _, epicID := range epicIDs {
		if len(epicChildMap[epicID]) > 0 {
			epicsWithChildren = append(epicsWithChildren, epicID)
		}
	}
	return epicsWithChildren
}

func epicChildStatusesInTx(ctx context.Context, tx DBTX, allChildIDs []string) (map[string]string, error) {
	childStatusMap := make(map[string]string)
	if len(allChildIDs) == 0 {
		return childStatusMap, nil
	}
	// Step 3: Batch-fetch statuses for all child issues across all epics
	for _, table := range []string{"issues", "wisps"} {
		statuses, err := epicChildStatusesFromTableInTx(ctx, tx, table, allChildIDs)
		if err != nil {
			return nil, err
		}
		for id, status := range statuses {
			childStatusMap[id] = status
		}
	}
	return childStatusMap, nil
}

func epicChildStatusesFromTableInTx(ctx context.Context, tx DBTX, table string, allChildIDs []string) (map[string]string, error) {
	statuses := make(map[string]string)
	totalChildIDs := len(allChildIDs)
	for start := 0; start < totalChildIDs; start += queryBatchSize {
		end := start + queryBatchSize
		if end > totalChildIDs {
			end = totalChildIDs
		}
		batch := allChildIDs[start:end]
		placeholders, args := buildSQLInClause(batch)
		statusQuery := fmt.Sprintf("SELECT id, status FROM %s WHERE id IN (%s)", table, placeholders)
		statusRows, err := tx.QueryContext(ctx, statusQuery, args...)
		if err != nil {
			if isTableNotExistError(err) {
				return statuses, nil // wisps table may not exist on pre-migration databases
			}
			return nil, fmt.Errorf("failed to batch-fetch child statuses from %s: %w", table, err)
		}
		for statusRows.Next() {
			var id, status string
			if err := statusRows.Scan(&id, &status); err != nil {
				return nil, errors.Join(fmt.Errorf("scan child status: %w", err), statusRows.Close())
			}
			statuses[id] = status
		}
		if err := errors.Join(statusRows.Err(), statusRows.Close()); err != nil {
			return nil, fmt.Errorf("iterate child statuses from %s: %w", table, err)
		}
	}
	return statuses, nil
}

func buildEpicClosureResults(epicIDs []string, epicChildMap map[string][]string, childStatusMap map[string]string, epicIssueMap map[string]*types.Issue) []*types.EpicStatus {
	// Step 5: Build results from cached data
	var results []*types.EpicStatus
	for _, epicID := range epicIDs {
		children := epicChildMap[epicID]
		if len(children) == 0 {
			continue
		}

		issue, ok := epicIssueMap[epicID]
		if !ok || issue == nil {
			continue
		}

		totalChildren := len(children)
		closedChildren := 0
		for _, childID := range children {
			if status, ok := childStatusMap[childID]; ok && types.Status(status) == types.StatusClosed {
				closedChildren++
			}
		}

		results = append(results, &types.EpicStatus{
			Epic:             issue,
			TotalChildren:    totalChildren,
			ClosedChildren:   closedChildren,
			EligibleForClose: totalChildren > 0 && totalChildren == closedChildren,
		})
	}
	return results
}
