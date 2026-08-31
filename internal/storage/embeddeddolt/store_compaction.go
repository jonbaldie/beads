//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"

	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

func (s *EmbeddedDoltStore) CheckEligibility(ctx context.Context, issueID string, tier int) (bool, string, error) {
	var eligible bool
	var reason string
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		eligible, reason, err = issueops.CheckEligibilityInTx(ctx, tx, issueID, tier)
		return err
	})
	return eligible, reason, err
}

func (s *EmbeddedDoltStore) ApplyCompaction(ctx context.Context, issueID string, tier int, originalSize int, _ int, commitHash string) error {
	return s.withConn(ctx, true, func(tx *sql.Tx) error {
		return issueops.ApplyCompactionInTx(ctx, tx, issueID, tier, originalSize, commitHash)
	})
}

func (s *EmbeddedDoltStore) SnapshotIssue(ctx context.Context, issueID string, tier int) error {
	return s.withConn(ctx, true, func(tx *sql.Tx) error {
		return issueops.SnapshotIssueInTx(ctx, tx, issueID, tier)
	})
}

func (s *EmbeddedDoltStore) GetCompactionSnapshot(ctx context.Context, issueID string) (*types.IssueSnapshot, error) {
	var snap *types.IssueSnapshot
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		snap, err = issueops.GetLatestSnapshotInTx(ctx, tx, issueID)
		return err
	})
	return snap, err
}

func (s *EmbeddedDoltStore) RestoreFromSnapshot(ctx context.Context, issueID string) (*types.IssueSnapshot, error) {
	var snap *types.IssueSnapshot
	err := s.withConn(ctx, true, func(tx *sql.Tx) error {
		var err error
		snap, err = issueops.RestoreFromSnapshotInTx(ctx, tx, issueID)
		return err
	})
	return snap, err
}

func (s *EmbeddedDoltStore) GetTier1Candidates(ctx context.Context) ([]*types.CompactionCandidate, error) {
	var result []*types.CompactionCandidate
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetTier1CandidatesInTx(ctx, tx)
		return err
	})
	return result, err
}

func (s *EmbeddedDoltStore) GetTier2Candidates(ctx context.Context) ([]*types.CompactionCandidate, error) {
	var result []*types.CompactionCandidate
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetTier2CandidatesInTx(ctx, tx)
		return err
	})
	return result, err
}
