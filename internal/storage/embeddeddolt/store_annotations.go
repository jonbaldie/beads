//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

func (s *EmbeddedDoltStore) AddIssueComment(ctx context.Context, issueID, author, text string) (*types.Comment, error) {
	var result *types.Comment
	err := s.withConn(ctx, true, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.AddIssueCommentInTx(ctx, tx, issueID, author, text)
		return err
	})
	return result, err
}

func (s *EmbeddedDoltStore) GetIssueComments(ctx context.Context, issueID string) ([]*types.Comment, error) {
	var result []*types.Comment
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetIssueCommentsInTx(ctx, tx, issueID)
		return err
	})
	return result, err
}

// GetIssueCommentsPage returns one keyset page of an issue's comments in
// (created_at ASC, id ASC) order, resuming strictly after the cursor. See the
// storage.Storage doc for the ordering, sargability, and page-walk contract.
func (s *EmbeddedDoltStore) GetIssueCommentsPage(ctx context.Context, issueID string, after storage.CommentPageCursor, limit int) ([]*types.Comment, error) {
	var result []*types.Comment
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetIssueCommentsPageInTx(ctx, tx, issueID, after, limit)
		return err
	})
	return result, err
}

func (s *EmbeddedDoltStore) GetEvents(ctx context.Context, issueID string, limit int) ([]*types.Event, error) {
	var result []*types.Event
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetEventsInTx(ctx, tx, issueID, limit)
		return err
	})
	return result, err
}

func (s *EmbeddedDoltStore) GetAllEventsSince(ctx context.Context, since time.Time) ([]*types.Event, error) {
	var result []*types.Event
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetAllEventsSinceInTx(ctx, tx, since)
		return err
	})
	return result, err
}

// EventsSince returns durable events strictly after the keyset cursor, ordered
// by (created_at ASC, id ASC) and bounded by limit. Durable events table only.
// issueID != "" scopes the feed to one bead's history.
func (s *EmbeddedDoltStore) EventsSince(ctx context.Context, cursor storage.EventCursor, issueID string, limit int) ([]*types.Event, error) {
	var result []*types.Event
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.EventsSinceInTx(ctx, tx, cursor.CreatedAt, cursor.ID, issueID, limit)
		return err
	})
	return result, err
}

// RecordProvenanceEvent appends a provenance event idempotently. inserted is
// false when the deterministic id already existed. Append-only — no update path.
func (s *EmbeddedDoltStore) RecordProvenanceEvent(ctx context.Context, ev types.ProvenanceEvent) (id string, inserted bool, err error) {
	err = s.withConn(ctx, true, func(tx *sql.Tx) error {
		var txErr error
		id, inserted, txErr = issueops.RecordProvenanceEventInTx(ctx, tx, ev)
		return txErr
	})
	if err != nil {
		return "", false, err
	}
	return id, inserted, nil
}

func (s *EmbeddedDoltStore) GetProvenanceEvents(ctx context.Context, issueID, kindFilter string) ([]types.ProvenanceEvent, error) {
	var result []types.ProvenanceEvent
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetProvenanceEventsInTx(ctx, tx, issueID, kindFilter)
		return err
	})
	return result, err
}

func (s *EmbeddedDoltStore) GetProvenanceByRef(ctx context.Context, ref string) ([]types.ProvenanceEvent, error) {
	var result []types.ProvenanceEvent
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetProvenanceByRefInTx(ctx, tx, ref)
		return err
	})
	return result, err
}
