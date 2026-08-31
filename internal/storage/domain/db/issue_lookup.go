package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/storage/sqlbuild"
	"github.com/jonbaldie/beads/internal/types"
)

func (r *issueLookupRepository) Get(ctx context.Context, id string, opts domain.IssueTableOpts) (*types.Issue, error) {
	if id == "" {
		return nil, errors.New("db: Get: id must not be empty")
	}
	table := pickIssueTable(opts.UseWispsTable)
	//nolint:gosec // G201: table is one of two hardcoded constants
	row := r.runner.QueryRowContext(ctx, fmt.Sprintf("SELECT %s FROM %s %s WHERE id = ?",
		issueSelectColumns, table, sqlbuild.LeaseJoin(table)), id)
	issue, err := scanIssue(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("db: Get %s: %w", id, err)
	}
	return issue, nil
}

func (r *issueLookupRepository) GetByIDs(ctx context.Context, ids []string, opts domain.IssueTableOpts) ([]*types.Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	table := pickIssueTable(opts.UseWispsTable)
	//nolint:gosec // G201: table is one of two hardcoded constants
	q := fmt.Sprintf("SELECT %s FROM %s %s WHERE id IN (%s)",
		issueSelectColumns, table, sqlbuild.LeaseJoin(table), strings.Join(placeholders, ","))
	rows, err := r.runner.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: GetByIDs: %w", err)
	}
	defer rows.Close()

	var out []*types.Issue
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("db: GetByIDs: scan: %w", err)
		}
		out = append(out, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: GetByIDs: rows: %w", err)
	}
	return out, nil
}

func (r *issueLookupRepository) Exists(ctx context.Context, id string, opts domain.IssueTableOpts) (bool, error) {
	if id == "" {
		return false, errors.New("db: Exists: id must not be empty")
	}
	table := pickIssueTable(opts.UseWispsTable)
	//nolint:gosec // G201: table is one of two hardcoded constants
	row := r.runner.QueryRowContext(ctx, fmt.Sprintf("SELECT 1 FROM %s WHERE id = ? LIMIT 1", table), id)
	var one int
	err := row.Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("db: Exists %s: %w", id, err)
	}
	return true, nil
}

func (r *issueLookupRepository) CountForPrefix(ctx context.Context, prefix string, opts domain.IssueTableOpts) (int, error) {
	if prefix == "" {
		return 0, errors.New("db: CountForPrefix: prefix must not be empty")
	}
	table := pickIssueTable(opts.UseWispsTable)
	var count int
	//nolint:gosec // G201: table is one of two hardcoded constants
	err := r.runner.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COUNT(*)
		FROM %s
		WHERE id LIKE CONCAT(?, '-%%')
		  AND INSTR(SUBSTRING(id, LENGTH(?) + 2), '.') = 0
	`, table), prefix, prefix).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("db: CountForPrefix %s: %w", prefix, err)
	}
	return count, nil
}

func normalizeIssueTimestamps(issue *types.Issue) {
	now := time.Now().UTC()
	if issue.CreatedAt.IsZero() {
		issue.CreatedAt = now
	} else {
		issue.CreatedAt = issue.CreatedAt.UTC()
	}
	if issue.UpdatedAt.IsZero() {
		issue.UpdatedAt = now
	} else {
		issue.UpdatedAt = issue.UpdatedAt.UTC()
	}
}

func pickIssueTable(useWisps bool) string {
	if useWisps {
		return "wisps"
	}
	return "issues"
}

//nolint:gosec // G201: table is a hardcoded constant ("issues" or "wisps")
func insertIssueRow(ctx context.Context, runner Runner, table string, issue *types.Issue) error {
	// Bound the VARCHAR(255) assignment columns at the raw-SQL chokepoint, so
	// every proxied-server (uow) create — single, batch, and import — rejects an
	// over-length assignee/owner with a typed ErrFieldTooLong instead of a raw
	// backend "data too long" error. Mirrors ValidateWithCustom on the embedded
	// create path.
	if err := types.CheckFieldLen("assignee", issue.Assignee); err != nil {
		return err
	}
	if err := types.CheckFieldLen("owner", issue.Owner); err != nil {
		return err
	}
	// Stamp a fresh non-zero row_lock at create, exactly like the classic
	// insertIssueIntoTable (issueops/helpers.go). Without it a proxied-server
	// (uow) create leaves row_lock at the schema DEFAULT 0, so the row's
	// RowVersion CAS token is stale-zero on read — the backend-divergent break
	// the RowVersion contract (types.Issue.RowVersion) forbids. The duplicate-key
	// path rewrites it too so an upsert also advances the token.
	_, err := runner.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (
			id, content_hash, title, description, design, acceptance_criteria, notes,
			status, priority, issue_type, assignee, estimated_minutes,
			created_at, created_by, owner, updated_at, started_at, closed_at, external_ref, spec_id,
			compaction_level, compacted_at, compacted_at_commit, original_size,
			sender, ephemeral, no_history, wisp_type, pinned, is_template,
			mol_type, work_type, source_system, source_repo, close_reason,
			event_kind, actor, target, payload,
			await_type, await_id, timeout_ns, waiters,
			due_at, defer_until, metadata,
			row_lock, storage_class
		) VALUES (
			?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?,
			?, ?
		)
		ON DUPLICATE KEY UPDATE
			content_hash = VALUES(content_hash),
			title = VALUES(title),
			description = VALUES(description),
			design = VALUES(design),
			acceptance_criteria = VALUES(acceptance_criteria),
			notes = VALUES(notes),
			status = VALUES(status),
			priority = VALUES(priority),
			issue_type = VALUES(issue_type),
			assignee = VALUES(assignee),
			estimated_minutes = VALUES(estimated_minutes),
			updated_at = VALUES(updated_at),
			started_at = VALUES(started_at),
			closed_at = VALUES(closed_at),
			external_ref = VALUES(external_ref),
			source_repo = VALUES(source_repo),
			close_reason = VALUES(close_reason),
			metadata = VALUES(metadata),
			row_lock = VALUES(row_lock)
	`, table),
		issue.ID, issue.ContentHash, issue.Title, issue.Description, issue.Design, issue.AcceptanceCriteria, issue.Notes,
		string(issue.Status), issue.Priority, string(issue.IssueType), nullString(issue.Assignee), nullIntPtr(issue.EstimatedMinutes),
		issue.CreatedAt, issue.CreatedBy, issue.Owner, issue.UpdatedAt, issue.StartedAt, issue.ClosedAt, nullStringPtr(issue.ExternalRef), issue.SpecID,
		issue.CompactionLevel, issue.CompactedAt, nullStringPtr(issue.CompactedAtCommit), nullIntVal(issue.OriginalSize),
		issue.Sender, issue.Ephemeral, issue.NoHistory, string(issue.WispType), issue.Pinned, issue.IsTemplate,
		string(issue.MolType), string(issue.WorkType), issue.SourceSystem, issue.SourceRepo, issue.CloseReason,
		issue.EventKind, issue.Actor, issue.Target, issue.Payload,
		issue.AwaitType, issue.AwaitID, issue.Timeout.Nanoseconds(), formatJSONStringArray(issue.Waiters),
		issue.DueAt, issue.DeferUntil, jsonMetadata(issue.Metadata),
		issueops.FreshRowLock(), nullString(string(issue.StorageClass.Normalize())),
	)
	if err != nil {
		return fmt.Errorf("db: insert into %s: %w", table, err)
	}
	return nil
}

type issueScanner interface {
	Scan(dest ...any) error
}

// scanIssue delegates to the classic scan so both stacks hydrate issues with
// identical semantics (bd-6dnrw.44 item 12, extract-don't-duplicate per .46).
// The shared scan reads created_at/updated_at as strings with format
// fallbacks where a hand-rolled sql.NullTime scan hard-fails on any driver
// that hands timestamps back as text.
func scanIssue(s issueScanner) (*types.Issue, error) {
	return issueops.ScanIssueFrom(s)
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullStringPtr(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

func nullIntPtr(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}

func nullIntVal(i int) any {
	if i == 0 {
		return nil
	}
	return i
}

func jsonMetadata(raw json.RawMessage) any {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

func formatJSONStringArray(items []string) string {
	if len(items) == 0 {
		return ""
	}
	b, err := json.Marshal(items)
	if err != nil {
		return ""
	}
	return string(b)
}

var timestampUpdateFields = map[string]struct{}{
	"started_at": {}, "closed_at": {}, "due_at": {}, "defer_until": {},
}

func normalizeUpdateValue(key string, value any) any {
	if _, ok := timestampUpdateFields[key]; ok {
		return normalizeTimestampUpdateValue(value)
	}
	switch key {
	case "status":
		return normalizeStatusValue(value)
	case "issue_type":
		return normalizeIssueTypeValue(value)
	case "metadata":
		return normalizeMetadataValue(value)
	case "waiters":
		return normalizeWaitersValue(value)
	}
	return value
}

func normalizeTimestampUpdateValue(value any) any {
	switch v := value.(type) {
	case time.Time:
		return v.UTC()
	case *time.Time:
		if v == nil {
			return nil
		}
		return v.UTC()
	default:
		return value
	}
}

func normalizeStatusValue(value any) any {
	if s, ok := value.(types.Status); ok {
		return string(s)
	}
	return value
}

func normalizeIssueTypeValue(value any) any {
	if t, ok := value.(types.IssueType); ok {
		return string(t)
	}
	return value
}

func normalizeMetadataValue(value any) any {
	switch v := value.(type) {
	case json.RawMessage:
		return string(v)
	case []byte:
		return string(v)
	default:
		return value
	}
}

func normalizeWaitersValue(value any) any {
	// The column is TEXT holding a JSON array; the embedded path
	// (issueops.updateIssueInTx) marshals unconditionally, and a raw
	// []string would be refused by the SQL driver here.
	waitersJSON, _ := json.Marshal(value)
	return string(waitersJSON)
}
