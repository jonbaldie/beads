//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"

	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

func (s *EmbeddedDoltStore) GetDependencyRecords(ctx context.Context, issueID string) ([]*types.Dependency, error) {
	var result []*types.Dependency
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		m, err := issueops.GetDependencyRecordsForIssuesInTx(ctx, tx, []string{issueID})
		if err != nil {
			return err
		}
		result = m[issueID]
		return nil
	})
	return result, err
}

// GetDependentRecords returns raw dependency rows whose target is targetID,
// without hydrating the source issues.
func (s *EmbeddedDoltStore) GetDependentRecords(ctx context.Context, targetID string, depType string, limit int, afterID string) ([]*types.Dependency, error) {
	var result []*types.Dependency
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetDependentRecordsInTx(ctx, tx, targetID, depType, limit, afterID)
		return err
	})
	return result, err
}

// CountDependentRecords returns the total inbound-edge count of targetID.
func (s *EmbeddedDoltStore) CountDependentRecords(ctx context.Context, targetID string, depType string) (int, error) {
	var n int
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		n, err = issueops.CountDependentRecordsInTx(ctx, tx, targetID, depType)
		return err
	})
	return n, err
}

func (s *EmbeddedDoltStore) FindWispDependentsRecursive(ctx context.Context, ids []string) (map[string]bool, error) {
	var result map[string]bool
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.FindWispDependentsRecursiveInTx(ctx, tx, ids)
		return err
	})
	return result, err
}
