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

func (s *EmbeddedDoltStore) History(ctx context.Context, issueID string) ([]*storage.HistoryEntry, error) {
	var result []*storage.HistoryEntry
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.HistoryInTx(ctx, tx, issueID)
		return err
	})
	return result, err
}

func (s *EmbeddedDoltStore) AsOf(ctx context.Context, issueID string, ref string) (*types.Issue, error) {
	var result *types.Issue
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.AsOfInTx(ctx, tx, issueID, ref)
		return err
	})
	return result, err
}

func (s *EmbeddedDoltStore) Diff(ctx context.Context, fromRef, toRef string) ([]*storage.DiffEntry, error) {
	var result []*storage.DiffEntry
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.DiffInTx(ctx, tx, fromRef, toRef)
		return err
	})
	return result, err
}

// PreviousExternalRef returns the external_ref value recorded for issueID
// as of the most recent commit at or before asOf.
func (s *EmbeddedDoltStore) PreviousExternalRef(ctx context.Context, issueID string, asOf time.Time) (string, bool, error) {
	var ref string
	var found bool
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		ref, found, err = issueops.PreviousExternalRefInTx(ctx, tx, issueID, asOf)
		return err
	})
	return ref, found, err
}
