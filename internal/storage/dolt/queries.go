package dolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

// SearchIssues finds issues matching query and filters.
// Delegates to issueops.SearchIssuesInTx for shared query logic.
func (s *DoltStore) SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error) {
	var result []*types.Issue
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.SearchIssuesInTx(ctx, tx, query, filter)
		return err
	})
	return result, err
}

// SearchIssueIDs is the narrow-projection variant of SearchIssues; returns
// only matching IDs.
func (s *DoltStore) SearchIssueIDs(ctx context.Context, query string, filter types.IssueFilter) ([]string, error) {
	var result []string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.SearchIssueIDsInTx(ctx, tx, query, filter)
		return err
	})
	return result, err
}

func (s *DoltStore) SearchIssuesWithCounts(ctx context.Context, query string, filter types.IssueFilter) ([]*types.IssueWithCounts, error) {
	var result []*types.IssueWithCounts
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.SearchIssuesWithCountsInTx(ctx, tx, query, filter)
		return err
	})
	return result, err
}

// wakeExpiredDefers runs the lazy defer-wake sweep (issueops.WakeExpiredDefersInTx)
// in its own write transaction before a ready-work read. Advisory by contract:
// a ready listing must never fail because the sweep could not run, so every
// error is swallowed here — silently for the expected shapes (read-only store,
// open write circuit, closed store), with a stderr warning otherwise. It runs
// OUTSIDE the read tx below because withReadTx unconditionally rolls back (and
// may retry its body on the read-only justification).
func (s *DoltStore) wakeExpiredDefers(ctx context.Context) {
	if s.readOnly {
		return
	}
	err := s.withCircuitWrite(ctx, func(ctx context.Context) error {
		return s.runIssueOperationTxWithMessage(ctx, func(tx *sql.Tx) (issueops.ChangedTables, string, error) {
			woke, err := issueops.WakeExpiredDefersInTx(ctx, tx)
			if err != nil {
				return nil, "", err
			}
			if len(woke.Issues) == 0 {
				// Wisp-only wakes persist with the SQL commit but mint no
				// version commit: wisp tables are dolt_ignored.
				return nil, "", nil
			}
			tables := issueops.ChangedTables{}
			tables.Add("issues", "events")
			return tables, issueops.WakeDefersCommitMessage(len(woke.Issues)), nil
		})
	})
	if err != nil && !errors.Is(err, ErrCircuitOpen) && !errors.Is(err, ErrStoreClosed) {
		warnDeferWakeSweepSkipped(err)
	}
}

// deferWakeAccessDeniedOnce rate-limits the access-denied advisory to one
// warning per process: a read-only-privileged SQL user hits it on every
// ready-front read, and repeating a configuration fact on each `bd ready`
// is noise, not signal.
var deferWakeAccessDeniedOnce sync.Once

func warnDeferWakeSweepSkipped(err error) {
	if dberrors.IsAccessDenied(err) {
		deferWakeAccessDeniedOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "warning: defer-wake sweep skipped (SQL user lacks write privileges; expired defers will not auto-wake from this client): %v\n", err)
		})
		return
	}
	fmt.Fprintf(os.Stderr, "warning: defer-wake sweep skipped: %v\n", err)
}

func (s *DoltStore) GetReadyWork(ctx context.Context, filter types.WorkFilter) ([]*types.Issue, error) {
	s.wakeExpiredDefers(ctx)
	var result []*types.Issue
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetReadyWorkInTx(ctx, tx, filter)
		return err
	})
	return result, err
}

func (s *DoltStore) GetReadyWorkWithCounts(ctx context.Context, filter types.WorkFilter) ([]*types.IssueWithCounts, error) {
	s.wakeExpiredDefers(ctx)
	var result []*types.IssueWithCounts
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetReadyWorkWithCountsInTx(ctx, tx, filter)
		return err
	})
	return result, err
}

// CountReadyWork returns the total ready-work count for filter. It is identical
// to len(GetReadyWorkWithCounts(filter with Limit=0)) but sizes the total with
// cheap indexed COUNT(*)s instead of re-running the counts mega-query. Backs the
// storage.ReadyWorkCounter capability.
func (s *DoltStore) CountReadyWork(ctx context.Context, filter types.WorkFilter) (int, error) {
	s.wakeExpiredDefers(ctx)
	var n int
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = issueops.CountReadyWorkInTx(ctx, tx, filter)
		return err
	})
	return n, err
}

func (s *DoltStore) GetBlockedIssues(ctx context.Context, filter types.WorkFilter) ([]*types.BlockedIssue, error) {
	var result []*types.BlockedIssue
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetBlockedIssuesInTx(ctx, tx, filter)
		return err
	})
	return result, err
}

// GetEpicsEligibleForClosure returns epics whose children are all closed
func (s *DoltStore) GetEpicsEligibleForClosure(ctx context.Context) ([]*types.EpicStatus, error) {
	var result []*types.EpicStatus
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetEpicsEligibleForClosureInTx(ctx, tx)
		return err
	})
	return result, err
}

// GetStaleIssues returns issues that haven't been updated recently
func (s *DoltStore) GetStaleIssues(ctx context.Context, filter types.StaleFilter) ([]*types.Issue, error) {
	var result []*types.Issue
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetStaleIssuesInTx(ctx, tx, filter)
		return err
	})
	return result, err
}

// GetStatistics returns summary statistics
func (s *DoltStore) GetStatistics(ctx context.Context) (*types.Statistics, error) {
	stats := &types.Statistics{}

	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		return issueops.ScanIssueCountsInTx(ctx, tx, stats)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get statistics: %w", err)
	}

	var blockedCount int
	if err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM issues
			WHERE is_blocked = 1 AND status <> 'closed' AND status <> 'pinned'
		`).Scan(&blockedCount)
	}); err != nil {
		return nil, fmt.Errorf("failed to count blocked issues: %w", err)
	}
	stats.BlockedIssues = &blockedCount

	ready := stats.OpenIssues - blockedCount
	if ready < 0 {
		ready = 0
	}
	stats.ReadyIssues = &ready

	return stats, nil
}

// GetStatisticsNoBlocked returns aggregate counts without the blocked-set traversal.
// BlockedIssues and ReadyIssues are nil in the result (readiness needs the blocked
// set). Use for bd stats --no-blocked fast path.
func (s *DoltStore) GetStatisticsNoBlocked(ctx context.Context) (*types.Statistics, error) {
	stats := &types.Statistics{}
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		return issueops.ScanIssueCountsInTx(ctx, tx, stats)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get statistics: %w", err)
	}
	// BlockedIssues stays nil; ReadyIssues not computable without blocked set.
	return stats, nil
}

// getChildrenOfDeferredParents returns IDs of issues whose parent has a future
// defer_until date. Uses separate single-table queries to avoid correlated
// cross-table JOIN subqueries that trigger Dolt joinIter hangs (GH#1190).
// Caller must hold s.mu (at least RLock).
func (s *DoltStore) getChildrenOfDeferredParents(ctx context.Context) ([]string, error) {
	// Step 1: Get IDs of issues with future defer_until
	deferredRows, err := s.queryContext(ctx, `
		SELECT id FROM issues
		WHERE defer_until IS NOT NULL AND defer_until > UTC_TIMESTAMP()
	`)
	if err != nil {
		return nil, wrapQueryError("deferred parents: get deferred issues", err)
	}
	var deferredIDs []string
	for deferredRows.Next() {
		var id string
		if err := deferredRows.Scan(&id); err != nil {
			_ = deferredRows.Close()
			return nil, wrapScanError("deferred parents: scan deferred issue", err)
		}
		deferredIDs = append(deferredIDs, id)
	}
	_ = deferredRows.Close()
	if err := deferredRows.Err(); err != nil {
		return nil, wrapQueryError("deferred parents: deferred rows", err)
	}
	if len(deferredIDs) == 0 {
		return nil, nil
	}

	// Step 2: Get children of those deferred parents
	return s.getChildrenOfIssues(ctx, deferredIDs)
}

// getChildrenOfIssues returns IDs of direct children (parent-child deps) of the given issue IDs.
func (s *DoltStore) getChildrenOfIssues(ctx context.Context, parentIDs []string) ([]string, error) {
	var result []string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetChildrenOfIssuesInTx(ctx, tx, parentIDs)
		return err
	})
	return result, err
}

// getChildrenWithParents returns a map of childID -> parentID for direct children
// (parent-child deps) of the given parent IDs.
func (s *DoltStore) getChildrenWithParents(ctx context.Context, parentIDs []string) (map[string]string, error) {
	var result map[string]string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetChildrenWithParentsInTx(ctx, tx, parentIDs)
		return err
	})
	return result, err
}

func (s *DoltStore) getDescendantIDs(ctx context.Context, rootID string) ([]string, error) {
	var result []string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetDescendantIDsInTx(ctx, tx, rootID, 0)
		return err
	})
	return result, err
}

// GetMoleculeProgress returns progress stats for a molecule
func (s *DoltStore) GetMoleculeProgress(ctx context.Context, moleculeID string) (*types.MoleculeProgressStats, error) {
	stats := &types.MoleculeProgressStats{
		MoleculeID: moleculeID,
	}
	tables := moleculeProgressTablesFor(s, ctx, moleculeID)
	stats.MoleculeTitle = s.moleculeProgressTitle(ctx, tables.issueTable, moleculeID)
	childIDs, err := s.moleculeProgressChildren(ctx, tables, moleculeID)
	if err != nil {
		return nil, err
	}
	childStatuses, err := s.moleculeProgressStatuses(ctx, tables.issueTable, childIDs)
	if err != nil {
		return nil, err
	}
	applyMoleculeProgress(stats, childIDs, childStatuses)
	return stats, nil
}

type moleculeProgressTables struct {
	issueTable string
	depTable   string
	parentCol  string
}

func moleculeProgressTablesFor(s *DoltStore, ctx context.Context, moleculeID string) moleculeProgressTables {
	tables := moleculeProgressTables{issueTable: "issues", depTable: "dependencies", parentCol: "depends_on_issue_id"}
	if s.isActiveWisp(ctx, moleculeID) {
		tables.issueTable = "wisps"
		tables.depTable = "wisp_dependencies"
		tables.parentCol = "depends_on_wisp_id"
	}
	return tables
}

func (s *DoltStore) moleculeProgressTitle(ctx context.Context, issueTable, moleculeID string) string {
	var title sql.NullString
	//nolint:gosec // G201: issueTable is hardcoded to "issues" or "wisps"
	err := s.db.QueryRowContext(ctx, fmt.Sprintf("SELECT title FROM %s WHERE id = ?", issueTable), moleculeID).Scan(&title)
	if err == nil && title.Valid {
		return title.String
	}
	return ""
}

func (s *DoltStore) moleculeProgressChildren(ctx context.Context, tables moleculeProgressTables, moleculeID string) ([]string, error) {
	//nolint:gosec // G201: table and column are selected from hardcoded values
	rows, err := s.queryContext(ctx, fmt.Sprintf(`
		SELECT issue_id FROM %s
		WHERE %s = ? AND type = 'parent-child'
	`, tables.depTable, tables.parentCol), moleculeID)
	if err != nil {
		return nil, fmt.Errorf("failed to get molecule children: %w", err)
	}
	defer rows.Close()
	var childIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, wrapScanError("get molecule progress: scan child", err)
		}
		childIDs = append(childIDs, id)
	}
	return childIDs, nil
}

type moleculeChildStatus struct{ status string }

func (s *DoltStore) moleculeProgressStatuses(ctx context.Context, issueTable string, childIDs []string) (map[string]moleculeChildStatus, error) {
	statuses := make(map[string]moleculeChildStatus)
	nChildren := len(childIDs)
	for start := 0; start < nChildren; start += queryBatchSize {
		end := min(start+queryBatchSize, nChildren)
		batch := childIDs[start:end]
		placeholders, args := doltBuildSQLInClause(batch)
		//nolint:gosec // G201: issueTable is hardcoded, placeholders contains only ? markers
		query := fmt.Sprintf("SELECT id, status FROM %s WHERE id IN (%s)", issueTable, placeholders)
		if err := s.readMoleculeStatusBatch(ctx, query, args, statuses); err != nil {
			return nil, err
		}
	}
	return statuses, nil
}

func (s *DoltStore) readMoleculeStatusBatch(ctx context.Context, query string, args []interface{}, statuses map[string]moleculeChildStatus) error {
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("failed to batch-fetch child statuses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			return wrapScanError("get molecule progress: scan status", err)
		}
		statuses[id] = moleculeChildStatus{status: status}
	}
	return nil
}

func applyMoleculeProgress(stats *types.MoleculeProgressStats, childIDs []string, statuses map[string]moleculeChildStatus) {
	for _, childID := range childIDs {
		info, ok := statuses[childID]
		if !ok {
			continue
		}
		stats.Total++
		switch types.Status(info.status) {
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

// GetMoleculeLastActivity returns the most recent activity timestamp for a molecule.
func (s *DoltStore) GetMoleculeLastActivity(ctx context.Context, moleculeID string) (*types.MoleculeLastActivity, error) {
	var result *types.MoleculeLastActivity
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetMoleculeLastActivityInTx(ctx, tx, moleculeID)
		return err
	})
	return result, err
}

// GetNextChildID returns the next available child ID for a parent.
// Delegates SQL work to issueops.GetNextChildIDTx.
func (s *DoltStore) GetNextChildID(ctx context.Context, parentID string) (string, error) {
	var childID string
	err := s.withRetryTx(ctx, func(tx *sql.Tx) error {
		var err error
		childID, err = issueops.GetNextChildIDTx(ctx, tx, parentID)
		return err
	})
	return childID, err
}
