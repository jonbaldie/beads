package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

// CurrentBranch returns the current branch name
func (s *DoltStore) CurrentBranch(ctx context.Context) (string, error) {
	return versioncontrolops.CurrentBranch(ctx, s.db)
}

// DeleteBranch deletes a branch (used to clean up import branches)
func (s *DoltStore) DeleteBranch(ctx context.Context, branch string) error {
	return versioncontrolops.DeleteBranch(ctx, s.db, branch)
}

// Log returns recent commit history
func (s *DoltStore) Log(ctx context.Context, limit int) ([]CommitInfo, error) {
	return versioncontrolops.Log(ctx, s.db, limit)
}

// CommitInfo is an alias for storage.CommitInfo.
type CommitInfo = storage.CommitInfo

// HistoryEntry represents a row from dolt_history_* table
type HistoryEntry struct {
	CommitHash string
	Committer  string
	CommitDate time.Time
	// Issue data at that commit
	IssueData map[string]interface{}
}

// HasRemote checks if a Dolt remote with the given name exists.
func (s *DoltStore) HasRemote(ctx context.Context, name string) (bool, error) {
	var count int
	err := s.queryRowContext(ctx, func(row *sql.Row) error {
		return row.Scan(&count)
	}, "SELECT COUNT(*) FROM dolt_remotes WHERE name = ?", name)
	if err != nil {
		return false, fmt.Errorf("failed to check remote %s: %w", name, err)
	}
	return count > 0, nil
}

// AddRemote adds a Dolt remote
func (s *DoltStore) AddRemote(ctx context.Context, name, url string) error {
	_, err := s.db.ExecContext(ctx, "CALL DOLT_REMOTE('add', ?, ?)", name, url)
	if err != nil {
		return fmt.Errorf("failed to add remote %s: %w", name, err)
	}
	return nil
}

// Status returns the current Dolt status (staged/unstaged changes)
func (s *DoltStore) Status(ctx context.Context) (*DoltStatus, error) {
	return versioncontrolops.Status(ctx, s.db)
}
