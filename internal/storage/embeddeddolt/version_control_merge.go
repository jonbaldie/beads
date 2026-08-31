//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

func (s *EmbeddedDoltStore) Merge(ctx context.Context, branch string) ([]storage.Conflict, error) {
	// bd-578h9.11: like every pull path, a branch merge brings in writes that
	// bypassed the local is_blocked hooks; recompute after a conflict-free
	// merge. Conflicted merges defer to the caller's post-resolution hook
	// (Sync, bd vc merge --strategy) — recomputing over unresolved rows would
	// read garbage.
	preHead := ""
	if !s.readOnly {
		preHead = s.preMergeHead(ctx)
	}
	var conflicts []storage.Conflict
	err := s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
		var err error
		conflicts, err = versioncontrolops.Merge(ctx, db, branch, commitAuthor)
		return err
	})
	if err == nil && len(conflicts) == 0 && !s.readOnly {
		if rerr := s.recomputeBlockedAfterPull(ctx, preHead); rerr != nil {
			return conflicts, fmt.Errorf("merge succeeded but is_blocked recompute failed: %w", rerr)
		}
	}
	return conflicts, err
}

// MergeWithStrategy implements storage.StrategicMerger for `bd vc merge
// --strategy` (#4992). Unlike Merge, it runs on a PINNED session
// (withMutatingPinnedDBConn, not withMutatingDBConn): the conflict-tolerant
// session flags versioncontrolops.MergeWithStrategy sets are session state
// and must be visible to the merge, resolve, repair, and commit statements
// that follow — a *sql.DB pool (OpenSQL allows 2 idle conns) could otherwise
// hand out a different connection mid-sequence.
//
// A resolved merge (conflicted or clean) always commits, so — unlike plain
// Merge, which skips the recompute for a still-conflicted merge — the
// is_blocked recompute always runs on success here.
func (s *EmbeddedDoltStore) MergeWithStrategy(ctx context.Context, branch, strategy string) ([]storage.Conflict, error) {
	preHead := ""
	if !s.readOnly {
		preHead = s.preMergeHead(ctx)
	}
	var conflicts []storage.Conflict
	err := s.withMutatingPinnedDBConn(ctx, func(db versioncontrolops.DBConn) error {
		var err error
		conflicts, err = versioncontrolops.MergeWithStrategy(ctx, db, branch, commitAuthor, strategy)
		return err
	})
	if err != nil {
		return conflicts, err
	}
	if !s.readOnly {
		if rerr := s.recomputeBlockedAfterPull(ctx, preHead); rerr != nil {
			return conflicts, fmt.Errorf("merge succeeded but is_blocked recompute failed: %w", rerr)
		}
	}
	return conflicts, nil
}

// RecomputeBlockedAfterMerge recomputes the denormalized is_blocked column
// for the rows changed since fromCommit and commits the result — the hook a
// caller that resolved merge conflicts itself must run after committing the
// resolution (bd-578h9.11): conflicted merges skip the automatic recompute
// because unresolved rows would feed it garbage, and nothing else covers the
// merged-in writes. fromCommit is the pre-merge HEAD; empty degrades to a
// full-graph recompute.
func (s *EmbeddedDoltStore) RecomputeBlockedAfterMerge(ctx context.Context, fromCommit string) error {
	return s.recomputeBlockedAfterPull(ctx, fromCommit)
}

// RecomputeAllBlocked recomputes is_blocked for every issue and wisp in one full
// pass and returns the number of rows it corrected. This is the embedded path
// of the mode-independent repair (bd-6dnrw.37); see DoltStore.RecomputeAllBlocked.
func (s *EmbeddedDoltStore) RecomputeAllBlocked(ctx context.Context) (int, error) {
	var changed int64
	if err := s.withConn(ctx, true, func(tx *sql.Tx) error {
		// One shared body across every mode: refuse to derive and commit
		// is_blocked from a dirty graph (see DoltStore.RecomputeAllBlocked),
		// checked inside the recompute tx so it sees the same working set the
		// recompute will read (bd-6dnrw.37).
		var e error
		changed, e = versioncontrolops.GuardedRecomputeAllBlockedInTx(ctx, tx)
		return e
	}); err != nil {
		return 0, err
	}
	if changed > 0 {
		// Stage only issues (wisps are dolt_ignore'd), matching the post-pull
		// recompute, so an unrelated dirty working set is not swept in.
		if err := s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
			return stageAndCommitAfterSQLCommit(ctx, db,
				versioncontrolops.BlockedRecomputeStagedTables(),
				versioncontrolops.BlockedRecomputeCommitMsg, commitAuthor)
		}); err != nil {
			return int(changed), err
		}
	}
	return int(changed), nil
}

func (s *EmbeddedDoltStore) GetConflicts(ctx context.Context) ([]storage.Conflict, error) {
	var conflicts []storage.Conflict
	err := s.withDBConn(ctx, func(db versioncontrolops.DBConn) error {
		var err error
		conflicts, err = versioncontrolops.GetConflicts(ctx, db)
		return err
	})
	return conflicts, err
}

func (s *EmbeddedDoltStore) ResolveConflicts(ctx context.Context, table string, strategy string) error {
	return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
		return versioncontrolops.ResolveConflicts(ctx, db, table, strategy)
	})
}

// The CLI reaches these two methods through storage.UnwrapStore, so the
// assertion must keep holding on the concrete store.
var _ storage.ConflictInspector = (*EmbeddedDoltStore)(nil)

// GetConflictRows returns the live conflicted rows of table, per field.
// Implements storage.ConflictInspector (backs `bd conflicts list|show`).
func (s *EmbeddedDoltStore) GetConflictRows(ctx context.Context, table string) ([]storage.ConflictRow, error) {
	var rows []storage.ConflictRow
	err := s.withDBConn(ctx, func(db versioncontrolops.DBConn) error {
		var err error
		rows, err = versioncontrolops.GetConflictRows(ctx, db, table)
		return err
	})
	return rows, err
}

// The CLI reaches this through storage.UnwrapStore too.
var _ storage.MergeBlockerInspector = (*EmbeddedDoltStore)(nil)

// GetMergeBlockers reports schema conflicts, constraint violations, and
// whether a merge is open. Implements storage.MergeBlockerInspector.
func (s *EmbeddedDoltStore) GetMergeBlockers(ctx context.Context) (storage.MergeBlockers, error) {
	var blockers storage.MergeBlockers
	err := s.withDBConn(ctx, func(db versioncontrolops.DBConn) error {
		var err error
		blockers, err = versioncontrolops.GetMergeBlockers(ctx, db)
		return err
	})
	return blockers, err
}

// ResolveConflictRows resolves individual conflicted rows of table by key.
// Implements storage.ConflictInspector (backs `bd conflicts resolve <id>`).
// It runs on a PINNED connection: the resolution sets dolt's
// conflict-tolerance session flags, which the writes that follow must see.
func (s *EmbeddedDoltStore) ResolveConflictRows(ctx context.Context, table string, keys []string, strategy string) (int, error) {
	var n int
	err := s.withMutatingPinnedDBConn(ctx, func(db versioncontrolops.DBConn) error {
		var err error
		n, err = versioncontrolops.ResolveConflictRows(ctx, db, table, keys, strategy)
		return err
	})
	return n, err
}

// ---------------------------------------------------------------------------
// Remote operations
// ---------------------------------------------------------------------------

const defaultRemote = "origin"

// remoteAuthUser returns the username to authenticate with the remote, read
// from DOLT_REMOTE_USER. When set, push/pull/fetch invocations pass --user so
// the in-process Dolt server authenticates against the remotesapi (which
// otherwise rejects with CLONE_ADMIN). DOLT_REMOTE_PASSWORD is read by Dolt
// itself from the same process environment. Returns "" when no auth is
// configured (typical for git+ssh, file://, or unauthenticated remotes).
//
// Every remote verb reaches this through withPeerAuth, which prefers the
// credentials add-peer stored for the remote and falls back here when the
// remote has no peer entry.
func remoteAuthUser() string {
	return os.Getenv("DOLT_REMOTE_USER")
}

// The remote entry points the verbs below reach are held in variables purely
// as a test seam: credentials only change the outcome against a remotesapi
// server enforcing authentication, which a unit test cannot stand up, so the
// credential-routing tests swap these to observe the remote name and user each
// verb presents. TestRemoteEntryPointsUseVersionControlOps pins the production
// bindings.
var (
	vcPush             = versioncontrolops.Push
	vcForcePush        = versioncontrolops.ForcePush
	vcPull             = versioncontrolops.Pull
	vcPullWithStrategy = versioncontrolops.PullWithStrategy
)

func (s *EmbeddedDoltStore) RemoveRemote(ctx context.Context, name string) error {
	return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
		return versioncontrolops.RemoveRemote(ctx, db, name)
	})
}

func (s *EmbeddedDoltStore) ListRemotes(ctx context.Context) ([]storage.RemoteInfo, error) {
	var remotes []storage.RemoteInfo
	err := s.withDBConn(ctx, func(db versioncontrolops.DBConn) error {
		var err error
		remotes, err = versioncontrolops.ListRemotes(ctx, db)
		return err
	})
	return remotes, err
}
