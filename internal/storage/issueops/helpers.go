// Package issueops provides shared transaction-scoped SQL operations for
// issue creation and management. Both DoltStore and EmbeddedDoltStore call
// into these functions, passing their own *sql.Tx obtained through their
// respective connection lifecycle patterns.
package issueops

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/idgen"
	"github.com/jonbaldie/beads/internal/types"
)

// IsWisp returns true if the issue should be routed to the wisps table.
// Routes based on flags only — not the ID pattern. The "-wisp-" ID prefix is
// a naming convention for generated wisp IDs, but promoted wisps keep their
// ID while moving to the issues table (Ephemeral=false). Routing on the ID
// would send promoted wisps back to the wisps table on re-insert.
//
// WispPlaneOverride, when set, wins over the flags. Import sets it from the
// export stream's explicit "wisp" plane marker: a record carrying
// no_history=true but NO marker is a promoted no-history wisp — a durable
// issues-table row whose stray flag must not re-plane it into the wisps
// table on re-insert, which is how export→import→export silently dropped
// such rows' relations (bd-r9uce).
func IsWisp(issue *types.Issue) bool {
	if issue.WispPlaneOverride != nil {
		return *issue.WispPlaneOverride
	}
	return issue.Ephemeral || issue.NoHistory
}

// TableRouting returns the issue and event table names for an issue,
// routing ephemeral issues to the wisps tables.
func TableRouting(issue *types.Issue) (issueTable, eventTable string) {
	if IsWisp(issue) {
		return "wisps", "wisp_events"
	}
	return "issues", "events"
}

// issueUpsertColumns are the columns rewritten by the issue UPSERT's
// ON DUPLICATE KEY UPDATE clause. updated_at is deliberately last: the
// stale-guarded variant compares VALUES(updated_at) against the stored
// updated_at in every assignment, and ON DUPLICATE KEY UPDATE assignments are
// evaluated in order, so the comparison column must not be reassigned until
// all other columns have been decided.
//
// Leases are NOT issues columns (bd-lrgn1): they live in the ephemeral
// leases table and are restored by RestoreLeaseOnImportInTx, which enforces
// the never-clobber-a-live-local-lease rule (protocol L1.2: lease fields MUST
// round-trip the JSONL interchange, wy-urlct). row_lock rides along because
// any write that can change status/assignee must rewrite it to collide with a
// concurrent reclaim/close on the same row (see freshRowLock in lease.go).
var issueUpsertColumns = []string{
	"content_hash", "title", "description", "design", "acceptance_criteria",
	"notes", "status", "priority", "issue_type", "assignee",
	"estimated_minutes", "started_at", "closed_at", "external_ref",
	"source_repo", "close_reason", "closed_by_session", "metadata",
	"row_lock", "updated_at",
}

// issueUpsertAssignments renders the ON DUPLICATE KEY UPDATE clause. With
// rejectStaleUpdate, each assignment keeps the stored value unless the
// incoming row is strictly newer (VALUES(updated_at) > updated_at) — the
// transactional import stale guard (bd-pkim8). Strictly-older AND
// equal-timestamp rows keep every stored column: updated_at is DATETIME with
// second granularity, so two distinct updates in the same second tie, and an
// incoming tie row with an empty field (e.g. notes) must not wipe the
// populated local value (bd-hj85c). Re-importing an identical snapshot stays
// idempotent either way — the rewrite would have written identical values.
// Tie rows are deliberately NOT short-circuited by the staleRejected
// pre-check in InsertIssueIfNew, so their aux data (labels/comments/deps,
// which never bump updated_at) still merges additively.
func issueUpsertAssignments(table string, rejectStaleUpdate bool) string {
	assignments := make([]string, 0, len(issueUpsertColumns))
	for _, col := range issueUpsertColumns {
		if rejectStaleUpdate {
			// Qualify existing-row references with the table name so the target value
			// remains unambiguous after the SQLite upsert translation. VALUES(...) is
			// the incoming row in the canonical Dolt/MySQL-dialect statement.
			assignments = append(assignments,
				fmt.Sprintf("%s = IF(VALUES(updated_at) > %s.updated_at, VALUES(%s), %s.%s)", col, table, col, table, col))
		} else {
			assignments = append(assignments, fmt.Sprintf("%s = VALUES(%s)", col, col))
		}
	}
	return strings.Join(assignments, ",\n\t\t\t")
}

// InsertIssueIntoTable inserts an issue into the specified table ("issues" or "wisps"),
// using ON DUPLICATE KEY UPDATE to handle pre-existing records gracefully.
func InsertIssueIntoTable(ctx context.Context, tx DBTX, table string, issue *types.Issue) error {
	return insertIssueIntoTable(ctx, tx, table, issue, false)
}

//nolint:gosec // G201: table is a hardcoded constant ("issues" or "wisps")
func insertIssueIntoTable(ctx context.Context, tx DBTX, table string, issue *types.Issue, rejectStaleUpdate bool) error {
	return executeIssueInsert(ctx, tx, table, issue, "ON DUPLICATE KEY UPDATE\n\t\t\t"+issueUpsertAssignments(table, rejectStaleUpdate))
}

//nolint:gosec // G201: table is a hardcoded constant ("issues" or "wisps")
func insertIssueCreateOnly(ctx context.Context, tx DBTX, table string, issue *types.Issue) error {
	return executeIssueInsert(ctx, tx, table, issue, "")
}

//nolint:gosec // G201: table is a hardcoded constant ("issues" or "wisps")
func executeIssueInsert(ctx context.Context, tx DBTX, table string, issue *types.Issue, suffix string) error {
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (
			id, content_hash, title, description, design, acceptance_criteria, notes,
			status, priority, issue_type, assignee, estimated_minutes,
			created_at, created_by, owner, updated_at, started_at, closed_at, external_ref, spec_id,
			compaction_level, compacted_at, compacted_at_commit, original_size,
			sender, ephemeral, no_history, wisp_type, pinned, is_template,
			mol_type, work_type, source_system, source_repo, close_reason, closed_by_session,
			event_kind, actor, target, payload,
			await_type, await_id, timeout_ns, waiters,
			due_at, defer_until, metadata,
			row_lock, storage_class
		) VALUES (
			?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?,
			?, ?
		)
		%s
	`, table, suffix),
		issue.ID, issue.ContentHash, issue.Title, issue.Description, issue.Design, issue.AcceptanceCriteria, issue.Notes,
		issue.Status, issue.Priority, issue.IssueType, NullString(issue.Assignee), NullInt(issue.EstimatedMinutes),
		issue.CreatedAt, issue.CreatedBy, issue.Owner, issue.UpdatedAt, issue.StartedAt, issue.ClosedAt, NullStringPtr(issue.ExternalRef), issue.SpecID,
		issue.CompactionLevel, issue.CompactedAt, NullStringPtr(issue.CompactedAtCommit), NullIntVal(issue.OriginalSize),
		issue.Sender, issue.Ephemeral, issue.NoHistory, issue.WispType, issue.Pinned, issue.IsTemplate,
		issue.MolType, issue.WorkType, issue.SourceSystem, issue.SourceRepo, issue.CloseReason, issue.ClosedBySession,
		issue.EventKind, issue.Actor, issue.Target, issue.Payload,
		issue.AwaitType, issue.AwaitID, issue.Timeout.Nanoseconds(), FormatJSONStringArray(issue.Waiters),
		issue.DueAt, issue.DeferUntil, JSONMetadata(issue.Metadata),
		freshRowLock(), NullString(string(issue.StorageClass.Normalize())),
	)
	if err != nil {
		return fmt.Errorf("insert issue into %s: %w", table, err)
	}
	return nil
}

// RecordEventInTable records an event in the specified events table.
func RecordEventInTable(ctx context.Context, tx DBTX, table, issueID string, eventType types.EventType, actor, newValue string) error {
	return InsertDerivedEvent(ctx, tx, table, AuxEvent{
		IssueID:   issueID,
		EventType: eventType,
		Actor:     actor,
		OldValue:  str(""),
		NewValue:  str(newValue),
	})
}

// GenerateIssueIDInTable generates a unique ID, checking for collisions
// in the specified table. Supports counter mode for non-ephemeral issues.
//
//nolint:gosec // G201: table is a hardcoded constant
func GenerateIssueIDInTable(ctx context.Context, tx DBTX, table, prefix string, issue *types.Issue, actor string) (string, error) {
	if counterID, ok, err := counterIssueID(ctx, tx, table, prefix); err != nil {
		return "", err
	} else if ok {
		return counterID, nil
	}
	return hashIssueID(ctx, tx, table, prefix, issue, actor)
}

func counterIssueID(ctx context.Context, tx DBTX, table, prefix string) (string, bool, error) {
	// Counter mode only applies to the issues table (not wisps).
	if table != "issues" {
		return "", false, nil
	}
	counterMode, err := IsCounterModeTx(ctx, tx)
	if err != nil {
		return "", false, err
	}
	if !counterMode {
		return "", false, nil
	}
	id, err := NextCounterIDTx(ctx, tx, prefix)
	return id, true, err
}

//nolint:gosec // G201: table is a hardcoded constant
func hashIssueID(ctx context.Context, tx DBTX, table, prefix string, issue *types.Issue, actor string) (string, error) {
	baseLength, err := GetAdaptiveIDLengthTx(ctx, tx, table, prefix)
	if err != nil {
		baseLength = 6
	}

	maxLength := 8
	if baseLength > maxLength {
		baseLength = maxLength
	}

	for length := baseLength; length <= maxLength; length++ {
		for nonce := 0; nonce < 10; nonce++ {
			candidate := idgen.GenerateHashID(prefix, issue.Title, issue.Description, actor, issue.CreatedAt, length, nonce)

			var count int
			err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE id = ?`, table), candidate).Scan(&count)
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

// IsCounterModeTx checks whether issue_id_mode=counter is configured.
func IsCounterModeTx(ctx context.Context, tx DBTX) (bool, error) {
	var idMode string
	err := tx.QueryRowContext(ctx, "SELECT value FROM config WHERE `key` = ?", "issue_id_mode").Scan(&idMode)
	if err != nil && err != sql.ErrNoRows {
		return false, fmt.Errorf("failed to read issue_id_mode config: %w", err)
	}
	return idMode == "counter", nil
}

// NextCounterIDTx atomically increments and returns the next sequential issue ID.
func NextCounterIDTx(ctx context.Context, tx DBTX, prefix string) (string, error) {
	res, err := tx.ExecContext(ctx, "UPDATE issue_counter SET last_id = last_id + 1 WHERE prefix = ?", prefix)
	if err != nil {
		return "", fmt.Errorf("failed to increment issue counter for prefix %q: %w", prefix, err)
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("failed to check rows affected for issue counter prefix %q: %w", prefix, err)
	}

	if rowsAffected == 0 {
		if err := initializeMissingCounter(ctx, tx, prefix); err != nil {
			return "", err
		}
	}

	var nextID int
	err = tx.QueryRowContext(ctx, "SELECT last_id FROM issue_counter WHERE prefix = ?", prefix).Scan(&nextID)
	if err != nil {
		return "", fmt.Errorf("failed to read issue counter after increment for prefix %q: %w", prefix, err)
	}
	return fmt.Sprintf("%s-%d", prefix, nextID), nil
}

func initializeMissingCounter(ctx context.Context, tx DBTX, prefix string) error {
	if seedErr := SeedCounterFromExistingIssuesTx(ctx, tx, prefix); seedErr != nil {
		return fmt.Errorf("failed to seed issue counter for prefix %q: %w", prefix, seedErr)
	}
	res, err := tx.ExecContext(ctx, "UPDATE issue_counter SET last_id = last_id + 1 WHERE prefix = ?", prefix)
	if err != nil {
		return fmt.Errorf("failed to increment issue counter after seeding for prefix %q: %w", prefix, err)
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected after seeding for prefix %q: %w", prefix, err)
	}
	if rowsAffected != 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO issue_counter (prefix, last_id) VALUES (?, 1)", prefix); err != nil {
		return fmt.Errorf("failed to insert initial issue counter for prefix %q: %w", prefix, err)
	}
	return nil
}

// SeedCounterFromExistingIssuesTx scans existing issues to find the highest numeric suffix
// for the given prefix, then seeds the issue_counter table if no row exists yet.
func SeedCounterFromExistingIssuesTx(ctx context.Context, tx DBTX, prefix string) error {
	var existing int
	err := tx.QueryRowContext(ctx, "SELECT last_id FROM issue_counter WHERE prefix = ?", prefix).Scan(&existing)
	if err == nil {
		return nil // already seeded
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("failed to check existing counter for prefix %q: %w", prefix, err)
	}

	maxNum, err := maxExistingIssueNumber(ctx, tx, prefix)
	if err != nil {
		return err
	}

	if maxNum > 0 {
		_, err = tx.ExecContext(ctx, "INSERT INTO issue_counter (prefix, last_id) VALUES (?, ?)", prefix, maxNum)
		if err != nil {
			return fmt.Errorf("failed to seed issue counter for prefix %q at %d: %w", prefix, maxNum, err)
		}
	}
	return nil
}

func maxExistingIssueNumber(ctx context.Context, tx DBTX, prefix string) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM issues WHERE id LIKE CONCAT(?, '-%')`, prefix)
	if err != nil {
		return 0, fmt.Errorf("failed to scan existing issues for prefix %q: %w", prefix, err)
	}
	defer rows.Close()

	maxNum := 0
	pfxDash := prefix + "-"
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		suffix := strings.TrimPrefix(id, pfxDash)
		if strings.Contains(suffix, ".") {
			continue // skip child IDs
		}
		if n, err := strconv.Atoi(suffix); err == nil && n > maxNum {
			maxNum = n
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("failed to iterate issues for prefix %q: %w", prefix, err)
	}
	return maxNum, nil
}
