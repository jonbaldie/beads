//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

// withDBConn opens a short-lived database connection configured for the
// store's database and branch and passes it to fn. Unlike withConn, no
// transaction is started — this is required for Dolt stored procedures
// (CALL DOLT_BRANCH, CALL DOLT_MERGE, etc.) that cannot run inside
// explicit SQL transactions.
func (s *EmbeddedDoltStore) withDBConn(ctx context.Context, fn func(db versioncontrolops.DBConn) error) (err error) {
	if s.closed.Load() {
		return errClosed
	}

	var db *sql.DB
	var cleanup func() error
	db, cleanup, err = OpenSQL(ctx, s.dataDir, s.database, s.branch)
	if err != nil {
		return
	}
	defer func() {
		err = errors.Join(err, cleanup())
		// Best-effort cleanup of orphaned tmp_pack_* files left by git
		// fetch in the Dolt git-remote-cache. Rate-limited internally.
		s.cleanGitRemoteCacheGarbage()
	}()

	return fn(db)
}

// withPinnedDBConn is withDBConn pinned to a single *sql.Conn, for operation
// sequences that depend on session state spanning statements — the pull path
// sets @@dolt_allow_commit_conflicts/@@dolt_force_transaction_commit and needs
// the subsequent DOLT_MERGE and settle statements to see them (bd-6dnrw.40).
// A *sql.DB may rotate connections between statements; a pinned conn cannot.
//
// The pinned conn inherits the database/branch session setup OpenSQL applied:
// the pool holds exactly the one connection OpenSQL configured (sequential
// Ping/USE/SET on a fresh pool), and db.Conn returns it — the same invariant
// ApplySchemaMigrations relies on.
func (s *EmbeddedDoltStore) withPinnedDBConn(ctx context.Context, fn func(db versioncontrolops.DBConn) error) (err error) {
	if s.closed.Load() {
		return errClosed
	}

	var db *sql.DB
	var cleanup func() error
	db, cleanup, err = OpenSQL(ctx, s.dataDir, s.database, s.branch)
	if err != nil {
		return
	}
	defer func() {
		err = errors.Join(err, cleanup())
		// Best-effort cleanup of orphaned tmp_pack_* files left by git
		// fetch in the Dolt git-remote-cache. Rate-limited internally.
		s.cleanGitRemoteCacheGarbage()
	}()

	conn, connErr := db.Conn(ctx)
	if connErr != nil {
		return fmt.Errorf("embeddeddolt: pin connection: %w", connErr)
	}
	defer conn.Close()

	return fn(conn)
}

// withMutatingDBConn is withDBConn for operations that mutate the database
// or its version-control state (merge, push/pull, branch ops, backups, GC).
// withDBConn runs outside any SQL transaction, so withConn's commit guard
// never sees these — a read-only store satisfies the full DoltStorage
// interface and must refuse them here instead (bd-578h9.12).
func (s *EmbeddedDoltStore) withMutatingDBConn(ctx context.Context, fn func(db versioncontrolops.DBConn) error) error {
	if s.readOnly {
		return ErrReadOnly
	}
	return s.withDBConn(ctx, fn)
}

// withMutatingPinnedDBConn is withPinnedDBConn with the same read-only
// refusal as withMutatingDBConn (bd-578h9.12).
func (s *EmbeddedDoltStore) withMutatingPinnedDBConn(ctx context.Context, fn func(db versioncontrolops.DBConn) error) error {
	if s.readOnly {
		return ErrReadOnly
	}
	return s.withPinnedDBConn(ctx, fn)
}

// commitAll runs the single embedded commit statement, DOLT_COMMIT('-Am'),
// on one connection (via withConn) and reports whether a commit actually
// landed. When tolerateEmpty is true, Dolt's "nothing to commit" response is
// treated as a no-op (committed=false, err=nil) instead of an error — the
// GH#3886 parity behavior Commit relies on. When tolerateEmpty is false, that
// same response is returned as an error instead, which CommitMergeResolution
// relies on (see its doc comment).
//
// Callers that need to know whether a commit landed (CommitPending) get it
// from the returned bool instead of reading HEAD before and after: HEAD reads
// are extra engine opens and are subject to a HEAD-moved-between-reads race
// if anything else writes concurrently.
func (s *EmbeddedDoltStore) commitAll(ctx context.Context, message string, tolerateEmpty bool) (committed bool, err error) {
	err = s.withConn(ctx, true, func(tx *sql.Tx) error {
		var commitErr error
		committed, commitErr = commitAllInTx(ctx, tx, message, tolerateEmpty)
		return commitErr
	})
	return committed, err
}

func commitAllInTx(ctx context.Context, tx *sql.Tx, message string, tolerateEmpty bool) (bool, error) {
	if _, err := tx.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', ?)", message); err != nil {
		if issueops.IsNothingToCommitError(err) {
			if tolerateEmpty {
				return false, nil
			}
			return false, fmt.Errorf("dolt commit: %w", err)
		}
		return false, wrapCommitIndeterminate("dolt commit", err)
	}
	return true, nil
}

// stageAndCommitAfterSQLCommit preserves the no-replay boundary for version
// publication after an already-visible SQL mutation.
func stageAndCommitAfterSQLCommit(ctx context.Context, db versioncontrolops.DBConn, dirtyTables map[string]bool, commitMsg, author string) error {
	if err := versioncontrolops.StageAndCommit(ctx, db, dirtyTables, commitMsg, author); err != nil {
		return wrapCommitIndeterminate("embeddeddolt: stage and commit after SQL commit", err)
	}
	return nil
}

// Commit stages and commits the full working set. A clean working set is not
// an error here: the server store (DoltStore.Commit et al., via
// isDoltNothingToCommit) has always tolerated Dolt's "nothing to commit"
// response, but this embedded path wrapped it as a hard failure — so `bd
// bootstrap`, which builds an embedded store unconditionally and calls
// CommitWithConfig (below) right after SetConfig on a pristine, otherwise-clean
// store, died on it (GH#3886). Tolerating it here brings embedded to parity.
func (s *EmbeddedDoltStore) Commit(ctx context.Context, message string) error {
	_, err := s.commitAll(ctx, message, true)
	return err
}

// CommitWithConfig commits all working set changes including config.
// so this is just an alias to satisfy the VersionControl interface (GH#3216).
func (s *EmbeddedDoltStore) CommitWithConfig(ctx context.Context, message string) error {
	return s.Commit(ctx, message)
}

// CommitMergeResolution concludes an operator --strategy merge resolution with
// config included. Embedded Commit already stages everything via DOLT_COMMIT
// ('-Am'), so config is never dropped here the way server-mode Commit drops it
// (GH#2455).
//
// Unlike Commit/CommitWithConfig, this does NOT alias Commit and does NOT
// tolerate a "nothing to commit" response: a merge resolution that leaves the
// working set clean is the --ours case, where our values already stood and
// resolving the conflict dirtied nothing — DoltStore.CommitMergeResolution
// handles exactly this by explicitly concluding the open merge instead of
// treating the empty diff as a no-op (concludeOpenMerge, wy-36ilm F12; see
// versioncontrolops.GetMergeBlockers, which documents the same class of gap:
// merge state that a plain commit-error check cannot see). Swallowing the
// error here without also concluding the merge would leave
// dolt_merge_status.is_merging true while reporting success, silently
// re-wedging the next pull/sync — worse than today's explicit failure.
// Whether embedded DOLT_COMMIT('-Am') already concludes an open merge on a
// clean working set (unlike the server-mode stored-procedure path, which
// requires the explicit conclude step) was not established here, so this
// keeps the pre-existing non-tolerant behavior rather than guess (GH#3886
// scope: fix bootstrap's plain-Commit path, not merge conclusion semantics).
func (s *EmbeddedDoltStore) CommitMergeResolution(ctx context.Context, message string) error {
	_, err := s.commitAll(ctx, message, false)
	return err
}

func (s *EmbeddedDoltStore) AddRemote(ctx context.Context, name, url string) error {
	return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
		_, err := db.ExecContext(ctx, "CALL DOLT_REMOTE('add', ?, ?)", name, url)
		return err
	})
}

func (s *EmbeddedDoltStore) HasRemote(ctx context.Context, name string) (bool, error) {
	var count int
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT count(*) FROM dolt_remotes WHERE name = ?", name).Scan(&count)
	})
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// ---------------------------------------------------------------------------
// Branch operations
// ---------------------------------------------------------------------------

func (s *EmbeddedDoltStore) Branch(ctx context.Context, name string) error {
	return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
		return versioncontrolops.CreateBranch(ctx, db, name)
	})
}

func (s *EmbeddedDoltStore) Checkout(ctx context.Context, branch string) error {
	return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
		return versioncontrolops.CheckoutBranch(ctx, db, branch)
	})
}

func (s *EmbeddedDoltStore) CurrentBranch(ctx context.Context) (string, error) {
	var branch string
	err := s.withDBConn(ctx, func(db versioncontrolops.DBConn) error {
		var err error
		branch, err = versioncontrolops.CurrentBranch(ctx, db)
		return err
	})
	return branch, err
}

func (s *EmbeddedDoltStore) DeleteBranch(ctx context.Context, branch string) error {
	return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
		return versioncontrolops.DeleteBranch(ctx, db, branch)
	})
}

func (s *EmbeddedDoltStore) ListBranches(ctx context.Context) ([]string, error) {
	var branches []string
	err := s.withDBConn(ctx, func(db versioncontrolops.DBConn) error {
		var err error
		branches, err = versioncontrolops.ListBranches(ctx, db)
		return err
	})
	return branches, err
}

// ---------------------------------------------------------------------------
// Version control operations
// ---------------------------------------------------------------------------

// commitAuthor returns the author string for merge commits.
const commitAuthor = commitName + " <" + commitEmail + ">"

func (s *EmbeddedDoltStore) CommitExists(ctx context.Context, commitHash string) (bool, error) {
	var exists bool
	err := s.withDBConn(ctx, func(db versioncontrolops.DBConn) error {
		var err error
		exists, err = versioncontrolops.CommitExists(ctx, db, commitHash)
		return err
	})
	return exists, err
}

func (s *EmbeddedDoltStore) Status(ctx context.Context) (*storage.Status, error) {
	var status *storage.Status
	err := s.withDBConn(ctx, func(db versioncontrolops.DBConn) error {
		var err error
		status, err = versioncontrolops.Status(ctx, db)
		return err
	})
	return status, err
}

func (s *EmbeddedDoltStore) Log(ctx context.Context, limit int) ([]storage.CommitInfo, error) {
	var commits []storage.CommitInfo
	err := s.withDBConn(ctx, func(db versioncontrolops.DBConn) error {
		var err error
		commits, err = versioncontrolops.Log(ctx, db, limit)
		return err
	})
	return commits, err
}
