//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"

	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

func (s *EmbeddedDoltStore) DeleteIssues(ctx context.Context, ids []string, cascade bool, force bool, dryRun bool) (*types.DeleteIssuesResult, error) {
	var result *types.DeleteIssuesResult
	err := s.withConn(ctx, !dryRun, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.DeleteIssuesInTx(ctx, tx, ids, cascade, force, dryRun)
		return err
	})
	return result, err
}

func (s *EmbeddedDoltStore) DeleteIssuesBySourceRepo(ctx context.Context, sourceRepo string) (int, error) {
	var count int
	err := s.withConn(ctx, true, func(tx *sql.Tx) error {
		var err error
		count, err = issueops.DeleteIssuesBySourceRepoInTx(ctx, tx, sourceRepo)
		return err
	})
	return count, err
}

func (s *EmbeddedDoltStore) UpdateIssueID(ctx context.Context, oldID, newID string, issue *types.Issue, actor string) error {
	return s.withConn(ctx, true, func(tx *sql.Tx) error {
		return issueops.UpdateIssueIDInTx(ctx, tx, oldID, newID, issue, actor)
	})
}

func (s *EmbeddedDoltStore) PromoteFromEphemeral(ctx context.Context, id string, actor string) error {
	return s.withConn(ctx, true, func(tx *sql.Tx) error {
		return issueops.PromoteFromEphemeralInTx(ctx, tx, id, actor)
	})
}

// PartitionWispIDs reports which of ids currently live in the wisps table.
// IDs absent from the wisps table are returned as permanent.
func (s *EmbeddedDoltStore) PartitionWispIDs(ctx context.Context, ids []string) (wispIDs, permIDs []string, err error) {
	err = s.withConn(ctx, false, func(tx *sql.Tx) error {
		var inErr error
		wispIDs, permIDs, inErr = issueops.PartitionWispIDsInTx(ctx, tx, ids)
		return inErr
	})
	return wispIDs, permIDs, err
}
