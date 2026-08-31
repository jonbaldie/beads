package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

func (t *transactionLabels) AddLabel(ctx context.Context, issueID, label, actor string) error {
	table := "labels"
	eventTable := "events"
	if t.isActiveWisp(ctx, issueID) {
		table = "wisp_labels"
		eventTable = "wisp_events"
	}

	if err := issueops.AddLabelInTx(ctx, t.txFor(table), table, eventTable, issueID, label, actor); err != nil {
		return wrapExecError("add label in tx", err)
	}
	t.resources.dirty.MarkDirty(table)
	t.resources.dirty.MarkDirty(eventTable)
	return nil
}

func (t *transactionLabels) GetLabels(ctx context.Context, issueID string) ([]string, error) {
	table := "labels"
	if t.isActiveWisp(ctx, issueID) {
		table = "wisp_labels"
	}

	//nolint:gosec // G201: table is hardcoded
	rows, err := t.txFor(table).QueryContext(ctx, fmt.Sprintf(`SELECT label FROM %s WHERE issue_id = ? ORDER BY label`, table), issueID)
	if err != nil {
		return nil, wrapQueryError("get labels in tx", err)
	}
	defer rows.Close()
	var labels []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, wrapScanError("get labels in tx", err)
		}
		labels = append(labels, l)
	}
	return labels, rows.Err()
}

// RemoveLabel removes a label within the transaction
func (t *transactionLabels) RemoveLabel(ctx context.Context, issueID, label, actor string) error {
	table := "labels"
	eventTable := "events"
	if t.isActiveWisp(ctx, issueID) {
		table = "wisp_labels"
		eventTable = "wisp_events"
	}

	if err := issueops.RemoveLabelInTx(ctx, t.txFor(table), table, eventTable, issueID, label, actor); err != nil {
		return wrapExecError("remove label in tx", err)
	}
	t.resources.dirty.MarkDirty(table)
	t.resources.dirty.MarkDirty(eventTable)
	return nil
}

// SetConfig sets a config value within the transaction
func (t *transactionConfig) SetConfig(ctx context.Context, key, value string) error {
	_, err := t.resources.regularTx.ExecContext(ctx, `
		INSERT INTO config (`+"`key`"+`, value) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE value = VALUES(value)
	`, key, value)
	if err != nil {
		return wrapExecError("set config in tx", err)
	}
	t.resources.dirty.MarkDirty("config")

	// ResolveCustomTypesInTx reads the normalized tables first, so without
	// this sync a type registered in-transaction stays invisible to
	// validation whenever the table already has rows.
	table, err := issueops.SyncConfigTables(ctx, t.resources.regularTx, key, value)
	if err != nil {
		return err
	}
	if table != "" {
		t.resources.dirty.MarkDirty(table)
	}

	// Keep store-level caches (GetCustomTypes and friends) coherent with
	// in-transaction config writes; see invalidateConfigCaches.
	if t.resources.store != nil {
		t.resources.store.invalidateConfigCaches(key)
	}
	return nil
}

// GetConfig gets a config value within the transaction
func (t *transactionConfig) GetConfig(ctx context.Context, key string) (string, error) {
	var value string
	err := t.resources.regularTx.QueryRowContext(ctx, "SELECT value FROM config WHERE `key` = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, wrapQueryError("get config in tx", err)
}

// SetMetadata sets a metadata value within the transaction
func (t *transactionConfig) SetMetadata(ctx context.Context, key, value string) error {
	_, err := t.resources.regularTx.ExecContext(ctx, `
		INSERT INTO metadata (`+"`key`"+`, value) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE value = VALUES(value)
	`, key, value)
	if err == nil {
		t.resources.dirty.MarkDirty("metadata")
	}
	return wrapExecError("set metadata in tx", err)
}

// GetMetadata gets a metadata value within the transaction
func (t *transactionConfig) GetMetadata(ctx context.Context, key string) (string, error) {
	var value string
	err := t.resources.regularTx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE `key` = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, wrapQueryError("get metadata in tx", err)
}

// SetLocalMetadata sets a value in the dolt-ignored local_metadata table within the transaction.
func (t *transactionConfig) SetLocalMetadata(ctx context.Context, key, value string) error {
	_, err := t.resources.ignoredTx.ExecContext(ctx, "REPLACE INTO local_metadata (`key`, value) VALUES (?, ?)", key, value)
	return wrapExecError("set local metadata in tx", err)
}

// GetLocalMetadata gets a value from the dolt-ignored local_metadata table within the transaction.
func (t *transactionConfig) GetLocalMetadata(ctx context.Context, key string) (string, error) {
	var value string
	err := t.resources.ignoredTx.QueryRowContext(ctx, "SELECT value FROM local_metadata WHERE `key` = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, wrapQueryError("get local metadata in tx", err)
}

func (t *transactionComments) ImportIssueComment(ctx context.Context, issueID, author, text string, createdAt time.Time) (*types.Comment, error) {
	_, err := t.GetIssue(ctx, issueID)
	if err != nil {
		return nil, err
	}

	table := "comments"
	if t.isActiveWisp(ctx, issueID) {
		table = "wisp_comments"
	}

	createdAtText := issueops.FormatAuxTime(createdAt)
	id, _, err := issueops.InsertDerivedComment(ctx, t.txFor(table), table, issueID, author, text, createdAtText)
	if err != nil {
		return nil, fmt.Errorf("failed to add comment: %w", err)
	}
	t.resources.dirty.MarkDirty(table)

	stored, err := issueops.ParseAuxTime(createdAtText)
	if err != nil {
		return nil, fmt.Errorf("failed to add comment: %w", err)
	}
	// This path writes the comment row directly rather than through
	// issueops.ImportIssueCommentInTx, so it must journal the comment op itself
	// — the create/comment entry points cover their own writes, not this one.
	if err := issueops.RecordCommentEventInTx(ctx, t.txFor(table), issueID, &issueops.EventComment{
		ID: id, Author: author, Text: text, CreatedAt: stored, Source: issueops.CommentSourceStructured,
	}); err != nil {
		return nil, wrapExecError("journal import comment in tx", err)
	}
	return &types.Comment{ID: id, IssueID: issueID, Author: author, Text: text, CreatedAt: stored}, nil
}

func (t *transactionComments) GetIssueComments(ctx context.Context, issueID string) ([]*types.Comment, error) {
	table := "comments"
	if t.isActiveWisp(ctx, issueID) {
		table = "wisp_comments"
	}

	//nolint:gosec // G201: table is hardcoded
	rows, err := t.txFor(table).QueryContext(ctx, fmt.Sprintf(`
		SELECT id, issue_id, author, text, created_at
		FROM %s
		WHERE issue_id = ?
		ORDER BY created_at ASC, id ASC
	`, table), issueID)
	if err != nil {
		return nil, wrapQueryError("get comments in tx", err)
	}
	defer rows.Close()
	var comments []*types.Comment
	for rows.Next() {
		var c types.Comment
		if err := rows.Scan(&c.ID, &c.IssueID, &c.Author, &c.Text, &c.CreatedAt); err != nil {
			return nil, wrapScanError("get comments in tx", err)
		}
		comments = append(comments, &c)
	}
	return comments, rows.Err()
}

// AddComment adds a comment within the transaction
func (t *transactionComments) AddComment(ctx context.Context, issueID, actor, comment string) error {
	table := "events"
	if t.isActiveWisp(ctx, issueID) {
		table = "wisp_events"
	}

	createdAt := issueops.NowAuxTime()
	id, err := issueops.InsertDerivedEventReturningID(ctx, t.txFor(table), table, issueops.AuxEvent{
		IssueID:   issueID,
		EventType: types.EventCommented,
		Actor:     actor,
		Comment:   sql.NullString{String: comment, Valid: true},
		CreatedAt: createdAt,
	})
	if err != nil {
		return wrapExecError("add comment in tx", err)
	}
	t.resources.dirty.MarkDirty(table)
	stored, err := issueops.ParseAuxTime(createdAt)
	if err != nil {
		return wrapExecError("add comment in tx", err)
	}
	// This path writes the audit comment row directly rather than through
	// issueops.AddCommentEventInTx, so it must journal the comment op itself.
	// The text is replayable content, so it carries the same payload as a
	// structured comment, distinguished by Source.
	if err := issueops.RecordCommentEventInTx(ctx, t.txFor(table), issueID, &issueops.EventComment{
		ID: id, Author: actor, Text: comment, CreatedAt: stored, Source: issueops.CommentSourceAudit,
	}); err != nil {
		return wrapExecError("journal comment in tx", err)
	}
	return nil
}

// GetIssueCommentsPage returns one keyset page of an issue's comments within the
// transaction. Like the OLD GetIssueComments/GetDependencyRecords tx methods, it
// pre-resolves wispness on the ignored session and hands the InTx read the
// handle that owns issueID's tier, so a comment written on either tier earlier in
// THIS uncommitted transaction is visible (durable rows live on regularTx, wisp
// rows on ignoredTx — see the struct comment on the two-session split).
func (t *transactionComments) GetIssueCommentsPage(ctx context.Context, issueID string, after storage.CommentPageCursor, limit int) ([]*types.Comment, error) {
	tx := t.resources.regularTx
	if t.isActiveWisp(ctx, issueID) {
		tx = t.resources.ignoredTx
	}
	return issueops.GetIssueCommentsPageInTx(ctx, tx, issueID, after, limit)
}

// CountIssuesByGroup returns per-group issue counts within the transaction.
//
// TWO-SESSION SCOPING: the count runs on regularTx, so it reflects this tx's own
// uncommitted DURABLE issues plus all COMMITTED issues and wisps, but NOT wisps
// created in this same uncommitted transaction (those live on the separate
// ignored session). This matches doltTransaction.SearchIssues, which is likewise
// durable-tier for the tx's own writes. Note the pre-existing count-vs-search
// asymmetry: CountIssuesByGroupInTx merges committed wisps into the buckets while
// SearchIssues reads the issues table only, so the two need not agree when
// committed wisps exist. The embedded backend has no session split and sees
// in-tx wisps here.
func (t *transactionComposite) CountIssuesByGroup(ctx context.Context, filter types.IssueFilter, groupBy string) (map[string]int, error) {
	return issueops.CountIssuesByGroupInTx(ctx, t.resources.regularTx, filter, groupBy)
}

// GetDependentRecords returns the raw inbound dependency rows of targetID within
// the transaction.
//
// TWO-SESSION SCOPING: a target's inbound edges genuinely span BOTH dependency
// tables (a wisp source points at a durable target), and the InTx read unions
// them with an in-query, cross-table de-dup that must run on a single handle.
// Run on regularTx, it sees this tx's own uncommitted DURABLE edges plus all
// COMMITTED edges, but NOT wisp edges written in this same uncommitted
// transaction (those live on the ignored session and become visible after
// commit). The embedded backend has no session split and sees in-tx wisp edges.
func (t *transactionDependencyRead) GetDependentRecords(ctx context.Context, targetID string, depType string, limit int, afterID string) ([]*types.Dependency, error) {
	return issueops.GetDependentRecordsInTx(ctx, t.resources.regularTx, targetID, depType, limit, afterID)
}

// GetDependentRecordsForIssues returns the raw inbound dependency rows for a set
// of target ids within the transaction, keyed by target id. Same TWO-SESSION
// SCOPING as GetDependentRecords: uncommitted-durable plus committed edges on the
// server backend; wisp edges written in this same transaction are visible after
// commit. The embedded backend sees in-tx wisp edges.
func (t *transactionDependencyRead) GetDependentRecordsForIssues(ctx context.Context, targetIDs []string) (map[string][]*types.Dependency, error) {
	return issueops.GetDependentRecordsForIssuesInTx(ctx, t.resources.regularTx, targetIDs)
}

// CountDependentRecords returns the total inbound-edge count of targetID within
// the transaction. Same TWO-SESSION SCOPING as GetDependentRecords — the count
// uses a cross-table NOT-IN subquery that must run on one handle, so on the
// server backend it excludes wisp edges written in this same uncommitted
// transaction (visible after commit). The embedded backend sees them.
func (t *transactionDependencyRead) CountDependentRecords(ctx context.Context, targetID string, depType string) (int, error) {
	return issueops.CountDependentRecordsInTx(ctx, t.resources.regularTx, targetID, depType)
}

// IsBlocked reports the denormalized transitive is_blocked flag and direct
// blockers of issueID within the transaction. Like GetIssueCommentsPage, it
// pre-resolves wispness and reads on the session that owns issueID's tier, so the
// is_blocked flag and blocker edges written for issueID earlier in THIS
// uncommitted transaction are visible on either tier.
func (t *transactionDependencyRead) IsBlocked(ctx context.Context, issueID string) (bool, []string, error) {
	tx := t.resources.regularTx
	if t.isActiveWisp(ctx, issueID) {
		tx = t.resources.ignoredTx
	}
	return issueops.IsBlockedInTx(ctx, tx, issueID)
}

// IsBlockedBatch reports the denormalized transitive is_blocked flag for a page
// of ids within the transaction. A batch can mix durable and wisp ids whose
// is_blocked columns live on different sessions, so — unlike a single-handle
// delegation — it partitions the ids by wispness (resolved on the ignored
// session so this tx's own uncommitted wisps count) and reads each tier's
// is_blocked on its owning session, then merges. Every id therefore reflects the
// flag written earlier in THIS uncommitted transaction, on either tier.
func (t *transactionDependencyRead) IsBlockedBatch(ctx context.Context, ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return map[string]bool{}, nil
	}
	wispIDs, permIDs, err := issueops.PartitionWispIDsInTx(ctx, t.resources.ignoredTx, ids)
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(ids))
	if len(permIDs) > 0 {
		durable, err := issueops.IsBlockedBatchInTx(ctx, t.resources.regularTx, permIDs)
		if err != nil {
			return nil, err
		}
		for id, blocked := range durable {
			result[id] = blocked
		}
	}
	if len(wispIDs) > 0 {
		wisp, err := issueops.IsBlockedBatchInTx(ctx, t.resources.ignoredTx, wispIDs)
		if err != nil {
			return nil, err
		}
		for id, blocked := range wisp {
			result[id] = blocked
		}
	}
	return result, nil
}

// EventsSince returns durable events strictly after the keyset cursor within the
// transaction. Mirrors DoltStore.EventsSince's issueops delegation. The feed is
// durable-only by contract (wisp events are excluded), and durable event writes
// land on regularTx, so an event recorded earlier in THIS uncommitted
// transaction is visible.
func (t *transactionComposite) EventsSince(ctx context.Context, cursor storage.EventCursor, issueID string, limit int) ([]*types.Event, error) {
	return issueops.EventsSinceInTx(ctx, t.resources.regularTx, cursor.CreatedAt, cursor.ID, issueID, limit)
}
