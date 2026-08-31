package issueops

import (
	"context"
	"fmt"
	"sort"

	"github.com/jonbaldie/beads/internal/types"
)

// GetNewlyUnblockedByCloseInTx finds issues that become unblocked when the
// given issue is closed. Works within an existing transaction.
// Returns full issue objects for the newly-unblocked issues.
//
//nolint:gosec // G201: table names come from hardcoded constants
func GetNewlyUnblockedByCloseInTx(ctx context.Context, tx DBTX, closedIssueID string) ([]*types.Issue, error) {
	candidateIDs, err := loadCloseCandidateIDs(ctx, tx, closedIssueID)
	if err != nil {
		return nil, err
	}
	if len(candidateIDs) == 0 {
		return nil, nil
	}

	candidateIDs, err = activeCloseCandidateIDs(ctx, tx, candidateIDs)
	if err != nil {
		return nil, err
	}
	if len(candidateIDs) == 0 {
		return nil, nil
	}

	stillBlocked, err := findStillBlockedCandidates(ctx, tx, candidateIDs, closedIssueID)
	if err != nil {
		return nil, err
	}

	return loadUnblockedIssues(ctx, tx, candidateIDs, stillBlocked), nil
}

func loadCloseCandidateIDs(ctx context.Context, tx DBTX, closedIssueID string) ([]string, error) {
	candidateSet := make(map[string]bool)
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		ids, err := readCloseCandidateIDsFromTable(ctx, tx, depTable, closedIssueID)
		if err != nil {
			if optionalBlockedTable(depTable) && isTableNotExistError(err) {
				continue
			}
			return nil, fmt.Errorf("find blocked candidates from %s: %w", depTable, err)
		}
		for _, id := range ids {
			candidateSet[id] = true
		}
	}

	candidateIDs := make([]string, 0, len(candidateSet))
	for id := range candidateSet {
		candidateIDs = append(candidateIDs, id)
	}
	sort.Strings(candidateIDs)
	return candidateIDs, nil
}

//nolint:gosec // G201: depTable is hardcoded.
func readCloseCandidateIDsFromTable(ctx context.Context, tx DBTX, depTable, closedIssueID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT issue_id FROM %s
		WHERE %s AND type = 'blocks'
	`, depTable, depTargetEquals("")), closedIssueID)
	if err != nil {
		return nil, err
	}
	var candidateIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan candidate from %s: %w", depTable, err)
		}
		candidateIDs = append(candidateIDs, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("candidate rows from %s: %w", depTable, err)
	}
	return candidateIDs, nil
}

func activeCloseCandidateIDs(ctx context.Context, tx DBTX, candidateIDs []string) ([]string, error) {
	candidateStatusByID, err := loadStatusByIDInTx(ctx, tx, candidateIDs)
	if err != nil {
		return nil, fmt.Errorf("check candidate status: %w", err)
	}
	activeCandidateIDs := candidateIDs[:0]
	for _, id := range candidateIDs {
		status, ok := candidateStatusByID[id]
		if !ok || status == types.StatusClosed || status == types.StatusPinned {
			continue
		}
		activeCandidateIDs = append(activeCandidateIDs, id)
	}
	return activeCandidateIDs, nil
}

func findStillBlockedCandidates(ctx context.Context, tx DBTX, candidateIDs []string, closedIssueID string) (map[string]bool, error) {
	stillBlocked := make(map[string]bool)
	total := len(candidateIDs)
	for start := 0; start < total; start += queryBatchSize {
		end := start + queryBatchSize
		if end > total {
			end = total
		}
		batchBlocked, err := findStillBlockedBatch(ctx, tx, candidateIDs[start:end], closedIssueID)
		if err != nil {
			return nil, err
		}
		for id := range batchBlocked {
			stillBlocked[id] = true
		}
	}
	return stillBlocked, nil
}

func findStillBlockedBatch(ctx context.Context, tx DBTX, batch []string, closedIssueID string) (map[string]bool, error) {
	remainingByCandidate, remainingBlockerIDs, err := loadRemainingBlockersForBatch(ctx, tx, batch, closedIssueID)
	if err != nil {
		return nil, err
	}
	statusByID, err := loadStatusByIDInTx(ctx, tx, remainingBlockerIDs)
	if err != nil {
		return nil, fmt.Errorf("check remaining blocker status: %w", err)
	}
	return activeBlockerCandidates(remainingByCandidate, statusByID), nil
}

func loadRemainingBlockersForBatch(ctx context.Context, tx DBTX, batch []string, closedIssueID string) (map[string][]string, []string, error) {
	placeholders, batchArgs := buildSQLInClause(batch)
	remainingByCandidate := make(map[string][]string, len(batch))
	remainingBlockerSet := make(map[string]struct{})
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		if err := readRemainingBlockersFromTable(ctx, tx, depTable, placeholders, batchArgs, closedIssueID, remainingByCandidate, remainingBlockerSet); err != nil {
			if optionalBlockedTable(depTable) && isTableNotExistError(err) {
				continue
			}
			return nil, nil, fmt.Errorf("check remaining blockers from %s: %w", depTable, err)
		}
	}

	remainingBlockerIDs := make([]string, 0, len(remainingBlockerSet))
	for blockerID := range remainingBlockerSet {
		remainingBlockerIDs = append(remainingBlockerIDs, blockerID)
	}
	sort.Strings(remainingBlockerIDs)
	return remainingByCandidate, remainingBlockerIDs, nil
}

//nolint:gosec // G201: depTable is hardcoded and placeholders contain only ?.
func readRemainingBlockersFromTable(ctx context.Context, tx DBTX, depTable, placeholders string, batchArgs []interface{}, closedIssueID string, remainingByCandidate map[string][]string, remainingBlockerSet map[string]struct{}) error {
	depRows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT issue_id, %s AS depends_on_id FROM %s
		WHERE issue_id IN (%s) AND type = 'blocks' AND %s != ?
	`, DepTargetExpr, depTable, placeholders, DepTargetExpr), append(batchArgs, closedIssueID)...)
	if err != nil {
		return err
	}
	for depRows.Next() {
		var candidateID, blockerID string
		if err := depRows.Scan(&candidateID, &blockerID); err != nil {
			_ = depRows.Close()
			return fmt.Errorf("scan remaining blocker: %w", err)
		}
		remainingByCandidate[candidateID] = append(remainingByCandidate[candidateID], blockerID)
		remainingBlockerSet[blockerID] = struct{}{}
	}
	_ = depRows.Close()
	if err := depRows.Err(); err != nil {
		return fmt.Errorf("remaining blocker rows from %s: %w", depTable, err)
	}
	return nil
}

func activeBlockerCandidates(remainingByCandidate map[string][]string, statusByID map[string]types.Status) map[string]bool {
	stillBlocked := make(map[string]bool)
	for candidateID, blockerIDs := range remainingByCandidate {
		for _, blockerID := range blockerIDs {
			status, ok := statusByID[blockerID]
			if ok && status != types.StatusClosed && status != types.StatusPinned {
				stillBlocked[candidateID] = true
				break
			}
		}
	}
	return stillBlocked
}

func loadUnblockedIssues(ctx context.Context, tx DBTX, candidateIDs []string, stillBlocked map[string]bool) []*types.Issue {
	var unblocked []*types.Issue
	for _, id := range candidateIDs {
		if stillBlocked[id] {
			continue
		}
		issue, err := GetIssueInTx(ctx, tx, id)
		if err != nil {
			continue
		}
		unblocked = append(unblocked, issue)
	}
	return unblocked
}
