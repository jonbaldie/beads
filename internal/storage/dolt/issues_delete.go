package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/idgen"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

// DeleteIssue permanently removes an issue
func (s *DoltStore) DeleteIssue(ctx context.Context, id string) error {
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		return s.deleteIssue(ctx, id)
	})
}

func (s *DoltStore) deleteIssue(ctx context.Context, id string) error {
	// Route ephemeral IDs to wisps table (falls through for promoted wisps)
	if s.isActiveWisp(ctx, id) {
		return s.deleteWisp(ctx, id)
	}

	if err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		if err := issueops.DeleteIssueInTx(ctx, tx, id); err != nil {
			return err
		}

		commitMsg := fmt.Sprintf("bd: delete %s", id)
		return s.doltAddAndCommitInTx(ctx, tx,
			[]string{"issues", "dependencies", "labels", "comments", "events", "provenance_events", "child_counters", "issue_snapshots", "compaction_snapshots"},
			commitMsg)
	}); err != nil {
		return s.recordDoltPublicationFailure(ctx, err)
	}
	return nil
}

// DeleteIssues deletes multiple issues in a single transaction.
// If cascade is true, recursively deletes dependents.
// If cascade is false but force is true, deletes issues and orphans dependents.
// If both are false, returns an error if any issue has dependents.
// If dryRun is true, only computes statistics without deleting.
// deleteBatchSize controls the maximum number of IDs per IN-clause query.
// Kept small to avoid large IN-clause queries. See steveyegge/beads#1692.
const deleteBatchSize = 50

// maxRecursiveResults is the safety limit for the total number of issues discovered
// during recursive dependent traversal. Used by wisps.go.
const maxRecursiveResults = 10000

// queryBatchSize controls the maximum number of IDs per IN-clause in read
// queries (label hydration, wisp lookups). Without batching, queries like
// `SELECT ... FROM wisp_labels WHERE issue_id IN (?,?,?,...thousands)` take
// 20+ seconds on databases with many wisps (e.g., hq with 29K wisps).
const queryBatchSize = 200

func (s *DoltStore) DeleteIssues(ctx context.Context, ids []string, cascade bool, force bool, dryRun bool) (*types.DeleteIssuesResult, error) {
	var result *types.DeleteIssuesResult
	err := s.withCircuitWrite(ctx, func(ctx context.Context) error {
		var err error
		result, err = s.deleteIssues(ctx, ids, cascade, force, dryRun)
		return err
	})
	return result, err
}

func (s *DoltStore) deleteIssues(ctx context.Context, ids []string, cascade bool, force bool, dryRun bool) (*types.DeleteIssuesResult, error) {
	if len(ids) == 0 {
		return &types.DeleteIssuesResult{}, nil
	}

	wispDeleteCount, regularIDs, err := s.deleteRequestedWisps(ctx, ids, dryRun)
	if err != nil {
		return nil, err
	}
	ids = regularIDs
	if len(ids) == 0 {
		return &types.DeleteIssuesResult{DeletedCount: wispDeleteCount}, nil
	}

	result, err := s.deleteRegularIssues(ctx, ids, cascade, force, dryRun)
	if err != nil {
		// Preserve partial result (e.g., OrphanedIssues) on error.
		if result != nil {
			result.DeletedCount += wispDeleteCount
		}
		return result, s.recordDoltPublicationFailure(ctx, err)
	}
	result.DeletedCount += wispDeleteCount

	return result, nil
}

func (s *DoltStore) deleteRequestedWisps(ctx context.Context, ids []string, dryRun bool) (int, []string, error) {
	// Route wisp IDs to wisp deletion; process regular IDs in batch below.
	// DoltStore uses its own batch wisp deletion (separate transactions per batch
	// to avoid write timeout on large sets — see bd-2ehd, ff-tqm).
	ephIDs, regularIDs := s.partitionByWispStatus(ctx, ids)
	if len(ephIDs) == 0 {
		return 0, regularIDs, nil
	}
	activeWispIDs := make([]string, 0, len(ephIDs))
	for _, eid := range ephIDs {
		if s.isActiveWisp(ctx, eid) {
			activeWispIDs = append(activeWispIDs, eid)
		}
	}
	if dryRun || len(activeWispIDs) == 0 {
		return len(activeWispIDs), regularIDs, nil
	}
	deleted, err := s.deleteWispBatch(ctx, activeWispIDs)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to batch delete wisps: %w", err)
	}
	return deleted, regularIDs, nil
}

func (s *DoltStore) deleteRegularIssues(ctx context.Context, ids []string, cascade, force, dryRun bool) (*types.DeleteIssuesResult, error) {
	var result *types.DeleteIssuesResult
	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		r, err := issueops.DeleteIssuesInTx(ctx, tx, ids, cascade, force, dryRun)
		result = r
		if err != nil || dryRun {
			return err
		}

		commitMsg := fmt.Sprintf("bd: delete %d issue(s)", result.DeletedCount)
		return s.doltAddAndCommitInTx(ctx, tx,
			[]string{"issues", "dependencies", "labels", "comments", "events", "provenance_events", "child_counters", "issue_snapshots", "compaction_snapshots"},
			commitMsg)
	})
	return result, err
}

// doltBuildSQLInClause builds a parameterized IN clause for SQL queries
func doltBuildSQLInClause(ids []string) (string, []interface{}) {
	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	return strings.Join(placeholders, ","), args
}

// =============================================================================
// Helper functions
// =============================================================================

func recordEvent(ctx context.Context, tx *sql.Tx, issueID string, eventType types.EventType, actor, oldValue, newValue string) error {
	return wrapExecError("record event", issueops.RecordFullEventInTable(ctx, tx, "events", issueID, eventType, actor, oldValue, newValue))
}

// seedCounterFromExistingIssuesTx scans existing issues to find the highest numeric suffix
// for the given prefix, then seeds the issue_counter table if no row exists yet.
// This is called when counter mode is first enabled on a repo that already has issues,
// to prevent counter collisions with manually-created sequential IDs (GH#2002).
// It is idempotent: if a counter row already exists for this prefix, it does nothing.
func seedCounterFromExistingIssuesTx(ctx context.Context, tx *sql.Tx, prefix string) error {
	exists, err := counterRowExists(ctx, tx, prefix)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	maxNum, err := maxExistingIssueSuffix(ctx, tx, prefix)
	if err != nil {
		return err
	}

	// Only insert a seed row if we found at least one numeric ID.
	// If no numeric IDs exist, the counter will naturally start at 1 on first use.
	if maxNum > 0 {
		_, err = tx.ExecContext(ctx,
			"INSERT INTO issue_counter (prefix, last_id) VALUES (?, ?)",
			prefix, maxNum)
		if err != nil {
			return fmt.Errorf("failed to seed issue_counter for prefix %q at %d: %w", prefix, maxNum, err)
		}
	}

	return nil
}

func counterRowExists(ctx context.Context, tx *sql.Tx, prefix string) (bool, error) {
	var existing int
	err := tx.QueryRowContext(ctx, "SELECT last_id FROM issue_counter WHERE prefix = ?", prefix).Scan(&existing)
	if err == nil {
		return true, nil
	}
	if err == sql.ErrNoRows {
		return false, nil
	}
	return false, fmt.Errorf("failed to check issue_counter for prefix %q: %w", prefix, err)
}

func maxExistingIssueSuffix(ctx context.Context, tx *sql.Tx, prefix string) (int, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM issues WHERE id LIKE ?", prefix+"-%")
	if err != nil {
		return 0, fmt.Errorf("failed to query existing issues for prefix %q: %w", prefix, err)
	}
	defer rows.Close()

	maxNum := 0
	prefixDash := prefix + "-"
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("failed to scan issue id: %w", err)
		}
		num, ok := parseIssueSuffix(id, prefixDash)
		if ok && num > maxNum {
			maxNum = num
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("failed to iterate existing issues for prefix %q: %w", prefix, err)
	}
	return maxNum, nil
}

func parseIssueSuffix(id, prefixDash string) (int, bool) {
	suffix := strings.TrimPrefix(id, prefixDash)
	if suffix == id {
		return 0, false
	}
	var num int
	if _, err := fmt.Sscanf(suffix, "%d", &num); err != nil || fmt.Sprintf("%d", num) != suffix {
		return 0, false
	}
	return num, true
}

// nextCounterIDTx atomically increments and returns the next sequential issue ID
// for the given prefix within an existing transaction. Returns the full ID string
// (e.g., "bd-1"). Used by both generateIssueID and generateIssueIDInTable.
func nextCounterIDTx(ctx context.Context, tx *sql.Tx, prefix string) (string, error) {
	// Increment atomically at the DB level to avoid duplicate IDs under
	// concurrent transactions (GH#2002). "last_id = last_id + 1" is evaluated
	// by the DB engine atomically within Dolt's MVCC.

	rowsAffected, err := incrementCounterTx(ctx, tx, prefix, "failed to increment issue counter for prefix %q")
	if err != nil {
		return "", err
	}
	if rowsAffected == 0 {
		if err := initializeCounterTx(ctx, tx, prefix); err != nil {
			return "", err
		}
	}

	// Read back the value that was atomically set by the DB engine.
	var nextID int
	err = tx.QueryRowContext(ctx, "SELECT last_id FROM issue_counter WHERE prefix = ?", prefix).Scan(&nextID)
	if err != nil {
		return "", fmt.Errorf("failed to read issue counter after increment for prefix %q: %w", prefix, err)
	}
	return fmt.Sprintf("%s-%d", prefix, nextID), nil
}

func incrementCounterTx(ctx context.Context, tx *sql.Tx, prefix, message string) (int64, error) {
	res, err := tx.ExecContext(ctx, "UPDATE issue_counter SET last_id = last_id + 1 WHERE prefix = ?", prefix)
	if err != nil {
		return 0, fmt.Errorf(message+": %w", prefix, err)
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to check rows affected for issue counter prefix %q: %w", prefix, err)
	}
	return rowsAffected, nil
}

func initializeCounterTx(ctx context.Context, tx *sql.Tx, prefix string) error {
	if err := seedCounterFromExistingIssuesTx(ctx, tx, prefix); err != nil {
		return fmt.Errorf("failed to seed issue counter for prefix %q: %w", prefix, err)
	}
	rowsAffected, err := incrementCounterTx(ctx, tx, prefix, "failed to increment issue counter after seeding for prefix %q")
	if err != nil {
		return err
	}
	if rowsAffected != 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO issue_counter (prefix, last_id) VALUES (?, 1)", prefix); err != nil {
		return fmt.Errorf("failed to insert initial issue counter for prefix %q: %w", prefix, err)
	}
	return nil
}

// isCounterModeTx checks whether issue_id_mode=counter is configured.
func isCounterModeTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var idMode string
	err := tx.QueryRowContext(ctx, "SELECT value FROM config WHERE `key` = ?", "issue_id_mode").Scan(&idMode)
	if err != nil && err != sql.ErrNoRows {
		return false, fmt.Errorf("failed to read issue_id_mode config: %w", err)
	}
	return idMode == "counter", nil
}

// generateHashID creates a hash-based ID for a top-level issue.
// Uses base36 encoding (0-9, a-z) for better information density than hex.
func generateHashID(prefix, title, description, creator string, timestamp time.Time, length, nonce int) string {
	return idgen.GenerateHashID(prefix, title, description, creator, timestamp, length, nonce)
}

// Thin wrappers around exported issueops functions, kept for internal callers.
var (
	isAllowedUpdateField = issueops.IsAllowedUpdateField
)

// Aliases for shared nullable helpers from issueops.
var (
	nullString    = issueops.NullString
	nullStringPtr = issueops.NullStringPtr
	nullInt       = issueops.NullInt
	nullIntVal    = issueops.NullIntVal
)

// Aliases for shared helpers from issueops.
var (
	jsonMetadata          = issueops.JSONMetadata
	parseJSONStringArray  = issueops.ParseJSONStringArray
	formatJSONStringArray = issueops.FormatJSONStringArray
)

// DeleteIssuesBySourceRepo permanently removes all issues from a specific source repository.
// This is used when a repo is removed from the multi-repo configuration.
// It also cleans up related data: dependencies, labels, comments, and events.
// Returns the number of issues deleted.
func (s *DoltStore) DeleteIssuesBySourceRepo(ctx context.Context, sourceRepo string) (int, error) {
	var count int
	err := s.withRetryTx(ctx, func(tx *sql.Tx) error {
		var err error
		count, err = issueops.DeleteIssuesBySourceRepoInTx(ctx, tx, sourceRepo)
		return err
	})
	return count, err
}

// ClearRepoMtime removes the mtime cache entry for a repository.
func (s *DoltStore) ClearRepoMtime(ctx context.Context, repoPath string) error {
	return s.withRetryTx(ctx, func(tx *sql.Tx) error {
		return issueops.ClearRepoMtimeInTx(ctx, tx, repoPath)
	})
}

// GetRepoMtime returns the cached mtime (in nanoseconds) for a repository's data file.
// Returns 0 if no cache entry exists.
func (s *DoltStore) GetRepoMtime(ctx context.Context, repoPath string) (int64, error) {
	var result int64
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetRepoMtimeInTx(ctx, tx, repoPath)
		return err
	})
	return result, err
}

// SetRepoMtime updates the mtime cache for a repository's data file.
func (s *DoltStore) SetRepoMtime(ctx context.Context, repoPath, jsonlPath string, mtimeNs int64) error {
	return s.withRetryTx(ctx, func(tx *sql.Tx) error {
		return issueops.SetRepoMtimeInTx(ctx, tx, repoPath, jsonlPath, mtimeNs)
	})
}
