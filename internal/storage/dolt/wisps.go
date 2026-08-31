package dolt

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/storage/sqlbuild"
	"github.com/jonbaldie/beads/internal/types"
)

// Wisp table routing helpers.
// Wisps are stored in dolt_ignored tables (wisps, wisp_labels, wisp_dependencies,
// wisp_events, wisp_comments) to avoid Dolt history bloat. All operations use the
// same Dolt SQL connection — no separate store or transaction routing needed.

// insertIssueIntoTable delegates to the shared issueops.InsertIssueIntoTable.
func insertIssueIntoTable(ctx context.Context, tx *sql.Tx, table string, issue *types.Issue) error {
	return issueops.InsertIssueIntoTable(ctx, tx, table, issue)
}

// scanIssueFromTable scans a single issue from the specified table.
//
//nolint:gosec // G201: table is a hardcoded constant ("issues" or "wisps")
func scanIssueFromTable(ctx context.Context, db *sql.DB, table, id string) (*types.Issue, error) {
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM %s %s
		WHERE id = ?
	`, issueSelectColumns, table, sqlbuild.LeaseJoin(table)), id)

	issue, err := scanIssueFrom(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: issue %s", storage.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get issue from %s: %w", table, err)
	}
	return issue, nil
}

// generateIssueIDInTable generates a unique ID, checking for collisions
// in the specified table. Supports counter mode for non-ephemeral issues.
//
//nolint:gosec // G201: table is a hardcoded constant
func generateIssueIDInTable(ctx context.Context, tx *sql.Tx, table, prefix string, issue *types.Issue, actor string) (string, error) {
	// Counter mode only applies to the issues table (not wisps).
	if table == "issues" {
		counterMode, err := isCounterModeTx(ctx, tx)
		if err != nil {
			return "", err
		}
		if counterMode {
			return nextCounterIDTx(ctx, tx, prefix)
		}
	}

	baseLength := getAdaptiveIDLengthFromTable(ctx, tx, table, prefix)

	var err error
	maxLength := 8
	if baseLength > maxLength {
		baseLength = maxLength
	}

	for length := baseLength; length <= maxLength; length++ {
		for nonce := 0; nonce < 10; nonce++ {
			candidate := generateHashID(prefix, issue.Title, issue.Description, actor, issue.CreatedAt, length, nonce)

			var count int
			err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE id = ?`, table), candidate).Scan(&count) //nolint:gosec // G201
			if err != nil {
				return "", fmt.Errorf("failed to check for ID collision: %w", err)
			}

			if count == 0 {
				return candidate, nil
			}
		}
	}

	return "", fmt.Errorf("failed to generate unique ID after trying lengths %d-%d with 10 nonces each", baseLength, maxLength)
}

// getAdaptiveIDLengthFromTable returns the adaptive ID length based on table size.
//
//nolint:gosec // G201: table is a hardcoded constant
func getAdaptiveIDLengthFromTable(ctx context.Context, tx *sql.Tx, table, prefix string) int {
	var count int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE id LIKE ?`, table), prefix+"%").Scan(&count); err != nil {
		return 4 // Default for wisps (small tables)
	}

	switch {
	case count < 100:
		return 4
	case count < 1000:
		return 5
	case count < 10000:
		return 6
	default:
		return 7
	}
}

// insertIssueTxIntoTable is the transaction-context version for inserting into a named table.
// Delegates to insertIssueIntoTable to ensure all columns are written.
func insertIssueTxIntoTable(ctx context.Context, tx *sql.Tx, table string, issue *types.Issue) error {
	return insertIssueIntoTable(ctx, tx, table, issue)
}

// scanIssueTxFromTable scans a full issue from a named table within a transaction.
// Delegates to the unified scanIssueFrom to ensure all columns are hydrated.
//
//nolint:gosec // G201: table is a hardcoded constant ("issues" or "wisps")
func scanIssueTxFromTable(ctx context.Context, tx *sql.Tx, table, id string) (*types.Issue, error) {
	row := tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s FROM %s %s WHERE id = ?
	`, issueSelectColumns, table, sqlbuild.LeaseJoin(table)), id)

	issue, err := scanIssueFrom(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: issue %s", storage.ErrNotFound, id)
	}
	if err != nil {
		return nil, wrapScanError("scan issue from "+table, err)
	}
	return issue, nil
}

// wispPrefix returns the ID prefix for wisp ID generation.
// Uses IDPrefix if set (e.g., IDPrefix="wisp" → "bd-wisp"), otherwise
// appends "-wisp" to the config prefix (e.g., "bd" → "bd-wisp").
func wispPrefix(configPrefix string, issue *types.Issue) string {
	if issue.PrefixOverride != "" {
		return issue.PrefixOverride
	}
	if issue.IDPrefix != "" {
		return configPrefix + "-" + issue.IDPrefix
	}
	return configPrefix + "-wisp"
}

// getWisp retrieves an issue from the wisps table.
func (s *DoltStore) getWisp(ctx context.Context, id string) (*types.Issue, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	issue, err := scanIssueFromTable(ctx, s.db, "wisps", id)
	if err != nil {
		return nil, err
	}
	if issue == nil {
		return nil, nil
	}
	labels, err := s.getWispLabels(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get wisp labels: %w", err)
	}
	issue.Labels = labels
	return issue, nil
}

// getWispLabels retrieves labels from the wisp_labels table.
func (s *DoltStore) getWispLabels(ctx context.Context, issueID string) ([]string, error) {
	rows, err := s.queryContext(ctx, `SELECT label FROM wisp_labels WHERE issue_id = ? ORDER BY label`, issueID)
	if err != nil {
		return nil, wrapQueryError("get wisp labels", err)
	}
	defer rows.Close()

	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, wrapScanError("scan wisp label", err)
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

// updateWisp updates fields on a wisp in the wisps table.
// Delegates SQL work to issueops.UpdateIssueInTx; no Dolt versioning needed
// since wisps live in dolt_ignored tables.
func (s *DoltStore) updateWisp(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	clearJournalScope := s.scopeEventsJournalTransaction(tx)
	defer clearJournalScope()

	if _, err := issueops.UpdateIssueInTx(ctx, tx, id, updates, actor); err != nil {
		return err
	}

	return s.commitSQLTx(ctx, "commit update wisp", tx)
}

// updateWispChecked updates a wisp with the optional atomic preconditions of
// UpdateIssueChecked, mirroring updateWisp but first enforcing — in the SAME
// transaction — opts.ExpectedVersion (issueops.CheckVersionInTx →
// storage.ErrVersionMismatch) and the opts.ExpectedAssignee/ExpectedStatus
// field guards (issueops.CheckExpectedFieldsInTx → ErrAssigneeMismatch/
// ErrStatusMismatch), so a stale precondition refuses before any write and the
// deferred Rollback discards the transaction (a true compare-and-swap). Like
// updateWisp it uses a bare BeginTx/Commit with no withRetryTx (consistent with
// the rest of the wisp write path — do not add one here); wisps live in
// dolt_ignored tables, so there is no DOLT_COMMIT.
func (s *DoltStore) updateWispChecked(ctx context.Context, id string, updates map[string]interface{}, actor string, opts storage.UpdateIssueOptions) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	clearJournalScope := s.scopeEventsJournalTransaction(tx)
	defer clearJournalScope()

	if opts.ExpectedVersion != nil {
		if err := issueops.CheckVersionInTx(ctx, tx, id, *opts.ExpectedVersion); err != nil {
			return err
		}
	}
	if err := issueops.CheckExpectedFieldsInTx(ctx, tx, id, opts.ExpectedAssignee, opts.ExpectedStatus); err != nil {
		return err
	}
	if _, err := issueops.UpdateIssueInTx(ctx, tx, id, updates, actor); err != nil {
		return err
	}

	return s.commitSQLTx(ctx, "commit update wisp", tx)
}

// closeWisp closes a wisp in the wisps table.
// Delegates SQL work to issueops.CloseIssueInTx; no Dolt versioning needed
// since wisps live in dolt_ignored tables.
func (s *DoltStore) closeWisp(ctx context.Context, id string, reason string, actor string, session string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	clearJournalScope := s.scopeEventsJournalTransaction(tx)
	defer clearJournalScope()

	if _, err := issueops.CloseIssueInTx(ctx, tx, id, reason, actor, session); err != nil {
		return err
	}

	return s.commitSQLTx(ctx, "commit close wisp", tx)
}

// closeWispChecked closes a wisp with the is_blocked guard, mirroring closeWisp
// but refusing with storage.ErrCloseBlocked when the wisp is still blocked
// unless opts.Force is set — and, when opts.ExpectedVersion is non-nil, with
// storage.ErrVersionMismatch when the row's RowVersion no longer matches (an
// orthogonal CAS that Force does not bypass). The checks and the close share the
// same transaction; wisps live in dolt_ignored tables, so there is no
// DOLT_COMMIT. On any rejection the deferred Rollback discards the transaction —
// no close or event is written.
//
// Unlike the permanent path, the wisp close uses a bare BeginTx/Commit with no
// withRetryTx (consistent with the rest of the wisp write path — do not add one
// here). So the CAS's read-side limb still returns ErrVersionMismatch for a
// writer that committed before this tx began, but a CONCURRENT wisp mutation
// that loses the race at commit surfaces as a transaction/serialization error
// rather than ErrVersionMismatch. Either way atomicity holds: no lost update and
// no stale close.
func (s *DoltStore) closeWispChecked(ctx context.Context, id string, actor string, opts storage.CloseIssueOptions) (storage.CloseIssueResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return storage.CloseIssueResult{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	clearJournalScope := s.scopeEventsJournalTransaction(tx)
	defer clearJournalScope()

	res, err := issueops.CloseIssueCheckedInTx(ctx, tx, id, opts.Reason, actor, opts.Session, opts.Force, opts.ExpectedVersion)
	if err != nil {
		return storage.CloseIssueResult{}, err
	}

	if err := s.commitSQLTx(ctx, "commit close wisp", tx); err != nil {
		return storage.CloseIssueResult{}, err
	}
	return storage.CloseIssueResult{Unchanged: res.AlreadyClosed, OpenChildren: res.OpenChildren}, nil
}

// wispAuxCascadeTables lists the wisp auxiliary tables a wisp delete must
// also clean up, mirroring internal/storage/schema/cli_migrations.go:300-315.
// wisp_child_counters is keyed on parent_id (a wisp can be a parent whose
// children hold the counter row); the other three are keyed on issue_id.
// Some deployed stores enforce this via FK ON DELETE CASCADE and some do not
// (be-zdqyl: the migration adding those FKs was never promoted out of
// migrations/ignored/), so the delete paths below must not rely on the
// database to do it for them.
var wispAuxCascadeTables = []struct{ table, column string }{
	{"wisp_labels", "issue_id"},
	{"wisp_events", "issue_id"},
	{"wisp_comments", "issue_id"},
	{"wisp_child_counters", "parent_id"},
}

// deleteWispAuxRowsInTx removes every row the given wisp ids own across
// wispAuxCascadeTables. Shared by deleteWisp and deleteWispBatchTx so the
// table set cannot drift between the two paths.
func deleteWispAuxRowsInTx(ctx context.Context, tx *sql.Tx, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	inClause, args := doltBuildSQLInClause(ids)
	for _, aux := range wispAuxCascadeTables {
		//nolint:gosec // G201: aux.table/aux.column come from the fixed wispAuxCascadeTables literal; inClause contains only ? markers
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("DELETE FROM %s WHERE %s IN (%s)", aux.table, aux.column, inClause),
			args...); err != nil {
			return fmt.Errorf("delete wisp aux rows from %s: %w", aux.table, err)
		}
	}
	return nil
}

// deleteWisp permanently removes a wisp and its related data.
func (s *DoltStore) deleteWisp(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	clearJournalScope := s.scopeEventsJournalTransaction(tx)
	defer clearJournalScope()

	if err := s.deleteWispInTx(ctx, tx, id); err != nil {
		return err
	}

	return s.commitSQLTx(ctx, "commit delete wisp", tx)
}

type wispDeletionImpact struct {
	affectedIssues []string
	affectedWisps  []string
}

func (s *DoltStore) deleteWispInTx(ctx context.Context, tx *sql.Tx, id string) error {
	impact, err := loadWispDeletionImpact(ctx, tx, []string{id}, fmt.Sprintf("wisp delete for %s", id))
	if err != nil {
		return err
	}

	// Edges are journaled before the row goes, while their source snapshots can
	// still be read. An active wisp is a bead like any other, so its delete is
	// a journalled mutation, not silent cleanup.
	if err := journalWispDependencyRemovals(ctx, tx, []string{id}, fmt.Sprintf("wisp %s", id)); err != nil {
		return err
	}
	if err := deleteWispRowInTx(ctx, tx, id); err != nil {
		return err
	}
	return finishWispDeletionInTx(ctx, tx, id, impact, fmt.Sprintf("wisp delete for %s", id))
}

func loadWispDeletionImpact(ctx context.Context, tx *sql.Tx, ids []string, description string) (wispDeletionImpact, error) {
	affectedIssues, affectedWisps, err := issueops.AffectedByDeletionInTx(ctx, tx, nil, ids)
	if err != nil {
		return wispDeletionImpact{}, fmt.Errorf("affected by %s: %w", description, err)
	}
	return wispDeletionImpact{affectedIssues: affectedIssues, affectedWisps: affectedWisps}, nil
}

func journalWispDependencyRemovals(ctx context.Context, tx *sql.Tx, ids []string, description string) error {
	if err := issueops.RecordDependencyRemovalsForIssuesInTx(ctx, tx, ids); err != nil {
		return fmt.Errorf("journal dependency removals for %s: %w", description, err)
	}
	return nil
}

func deleteWispRowInTx(ctx context.Context, tx *sql.Tx, id string) error {
	result, err := tx.ExecContext(ctx, "DELETE FROM wisps WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("failed to delete wisp: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("wisp not found: %s", id)
	}
	// The rows==0 return above keeps this actually-deleted-only.
	return issueops.RecordDeleteInTx(ctx, tx, id)
}

func finishWispDeletionInTx(ctx context.Context, tx *sql.Tx, id string, impact wispDeletionImpact, description string) error {
	if err := issueops.DeleteWispFromDependenciesInTx(ctx, tx, id); err != nil {
		return err
	}
	if err := deleteWispAuxRowsInTx(ctx, tx, []string{id}); err != nil {
		return fmt.Errorf("delete wisp aux rows for %s: %w", id, err)
	}
	if err := issueops.RecomputeIsBlockedInTx(ctx, tx, impact.affectedIssues, impact.affectedWisps); err != nil {
		return fmt.Errorf("recompute is_blocked after %s: %w", description, err)
	}
	return nil
}

// deleteWispBatch permanently removes multiple wisps using one transaction per
// batch of 200. Committing per-batch keeps each transaction short enough to
// complete within Dolt's writeTimeout (10 s), preventing i/o timeout errors
// when GC-ing hundreds of wisps at once (ff-tqm).
//
// Previously the entire set was wrapped in one mega-transaction; at 631 wisps
// the commit exceeded the driver write timeout and failed with
// "read tcp …: i/o timeout".
//
// Partial cleanup is acceptable: if one batch fails the earlier batches are
// already committed and the next GC run will handle the remainder.
func (s *DoltStore) deleteWispBatch(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	const batchSize = 200
	totalDeleted := 0

	idCount := len(ids)
	for i := 0; i < idCount; i += batchSize {
		end := i + batchSize
		if end > idCount {
			end = idCount
		}
		deleted, err := s.deleteWispBatchTx(ctx, ids[i:end])
		if err != nil {
			return totalDeleted, err
		}
		totalDeleted += deleted
	}

	return totalDeleted, nil
}

// deleteWispBatchTx deletes one batch of wisps inside its own transaction.
// Keeping each transaction to ≤200 wisps (6 DELETE statements) ensures it
// completes well within Dolt's 10 s write timeout.
func (s *DoltStore) deleteWispBatchTx(ctx context.Context, ids []string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	clearJournalScope := s.scopeEventsJournalTransaction(tx)
	defer clearJournalScope()

	deleted, err := s.deleteWispBatchInTx(ctx, tx, ids)
	if err != nil {
		return 0, err
	}

	if err := s.commitSQLTx(ctx, "commit batch wisp delete", tx); err != nil {
		return 0, err
	}

	return deleted, nil
}

func (s *DoltStore) deleteWispBatchInTx(ctx context.Context, tx *sql.Tx, ids []string) (int, error) {
	// Resolve WHICH wisps this batch actually removes before the DELETE runs:
	// afterwards they are gone, and RowsAffected reports a count, not a set.
	// GC hands this path ids it scanned earlier, so an already-collected wisp is
	// a routine case, and a phantom delete record would tell a consumer to drop
	// a bead this transaction never touched.
	impact, err := loadWispDeletionImpact(ctx, tx, ids, "batched wisp delete")
	if err != nil {
		return 0, err
	}
	deletedIDs, err := issueops.ExistingIssueIDsInTableInTx(ctx, tx, "wisps", ids)
	if err != nil {
		return 0, fmt.Errorf("resolve existing wisps for batch delete: %w", err)
	}

	// Edges are journaled before the rows go, while their source snapshots can
	// still be read.
	if err := journalWispDependencyRemovals(ctx, tx, deletedIDs, "batched wisp delete"); err != nil {
		return 0, err
	}
	rowsAffected, err := deleteWispRowsInTx(ctx, tx, ids)
	if err != nil {
		return 0, err
	}
	if err := finishWispBatchDeletionInTx(ctx, tx, ids, deletedIDs, impact); err != nil {
		return 0, err
	}
	return rowsAffected, nil
}

func deleteWispRowsInTx(ctx context.Context, tx *sql.Tx, ids []string) (int, error) {
	inClause, args := doltBuildSQLInClause(ids)

	//nolint:gosec // G201: inClause contains only ? markers
	result, err := tx.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM wisps WHERE id IN (%s)", inClause),
		args...)
	if err != nil {
		return 0, fmt.Errorf("failed to batch delete wisps: %w", err)
	}
	rowsAffected, _ := result.RowsAffected()
	return int(rowsAffected), nil
}

func finishWispBatchDeletionInTx(ctx context.Context, tx *sql.Tx, ids, deletedIDs []string, impact wispDeletionImpact) error {
	for _, id := range deletedIDs {
		if err := issueops.RecordDeleteInTx(ctx, tx, id); err != nil {
			return err
		}
	}
	if err := issueops.DeleteWispsFromDependenciesInTx(ctx, tx, ids); err != nil {
		return err
	}
	if err := deleteWispAuxRowsInTx(ctx, tx, ids); err != nil {
		return fmt.Errorf("delete wisp aux rows: %w", err)
	}
	if err := issueops.RecomputeIsBlockedInTx(ctx, tx, impact.affectedIssues, impact.affectedWisps); err != nil {
		return fmt.Errorf("recompute is_blocked after batched wisp delete: %w", err)
	}
	return nil
}

// claimWisp atomically claims a wisp.
// Delegates SQL work to issueops.ClaimIssueInTx; no Dolt versioning needed
// since wisps live in dolt_ignored tables.
func (s *DoltStore) claimWisp(ctx context.Context, id string, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	clearJournalScope := s.scopeEventsJournalTransaction(tx)
	defer clearJournalScope()

	if _, err := issueops.ClaimIssueInTx(ctx, tx, id, actor); err != nil {
		return err
	}

	return s.commitSQLTx(ctx, "commit claim wisp", tx)
}
