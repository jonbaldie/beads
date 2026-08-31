//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

// Flatten squashes all Dolt commit history into a single commit.
// Pins a single *sql.Conn for session-scoped stored procedures.
func (s *EmbeddedDoltStore) Flatten(ctx context.Context) error {
	return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
		if pooled, ok := db.(*sql.DB); ok {
			conn, err := pooled.Conn(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()
			return versioncontrolops.Flatten(ctx, conn)
		}
		return versioncontrolops.Flatten(ctx, db)
	})
}

// Compact squashes old Dolt commits while preserving recent ones.
// Pins a single *sql.Conn for session-scoped stored procedures.
func (s *EmbeddedDoltStore) Compact(ctx context.Context, initialHash, boundaryHash string, oldCommits int, recentHashes []string) error {
	return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
		// withDBConn returns *sql.DB; pin a single connection for
		// session-scoped operations (checkout, reset, cherry-pick).
		if pooled, ok := db.(*sql.DB); ok {
			conn, err := pooled.Conn(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()
			return versioncontrolops.Compact(ctx, conn, initialHash, boundaryHash, oldCommits, recentHashes)
		}
		return versioncontrolops.Compact(ctx, db, initialHash, boundaryHash, oldCommits, recentHashes)
	})
}

// Path returns the embedded dolt data directory (.beads/embeddeddolt/).
func (s *EmbeddedDoltStore) Path() string {
	return s.dataDir
}

// CLIDir returns the directory for dolt CLI operations (push/pull/remote).
// This is the actual database directory within the data dir.
func (s *EmbeddedDoltStore) CLIDir() string {
	if s.dataDir == "" {
		return ""
	}
	return filepath.Join(s.dataDir, s.database)
}

// ActiveDatabaseSize returns the approximate size of this store's active
// database directory. Sibling databases under the embedded data root are not
// part of the result.
func (s *EmbeddedDoltStore) ActiveDatabaseSize(ctx context.Context) (int64, error) {
	if s.closed.Load() {
		return 0, errClosed
	}
	activeDir := s.CLIDir()
	if activeDir == "" {
		return 0, fmt.Errorf("embeddeddolt: active database directory is empty")
	}
	size, err := storage.MeasureDirectorySize(ctx, activeDir)
	if err != nil {
		return 0, fmt.Errorf("measure active database directory %q: %w", activeDir, err)
	}
	return size, nil
}

// CommitPending commits all working set changes and reports whether a commit
// actually landed. commitAll supplies the authoritative result, including the
// clean-working-set case.
func (s *EmbeddedDoltStore) CommitPending(ctx context.Context, actor string) (bool, error) {
	msg := fmt.Sprintf("bd: commit pending changes by %s", actor)
	return s.commitAll(ctx, msg, true)
}

func (s *EmbeddedDoltStore) GetCurrentCommit(ctx context.Context) (string, error) {
	var hash string
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT HASHOF('HEAD')").Scan(&hash)
	})
	return hash, err
}
