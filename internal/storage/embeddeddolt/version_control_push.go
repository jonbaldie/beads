//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

// GH#5080 follow-up: the verbs below resolve credentials through withPeerAuth
// rather than the environment alone, so a remote registered as a federation
// peer presents its stored credentials however it is reached (`bd sync
// --remote`, `bd dolt push|pull --remote`, or as the default remote). A remote
// with no peer entry keeps the DOLT_REMOTE_USER/DOLT_REMOTE_PASSWORD fallback
// unchanged. Routing every verb through the one resolver also narrows the
// window around withPeerAuth's mutation of that process-wide pair: a verb
// operating on a peer-backed remote now reads it holding federationEnvMutex,
// where before it read it holding no lock at all.

func (s *EmbeddedDoltStore) Push(ctx context.Context) error {
	return s.withPeerAuth(ctx, defaultRemote, func(user string) error {
		return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
			return vcPush(ctx, db, defaultRemote, s.branch, user)
		})
	})
}

func (s *EmbeddedDoltStore) Pull(ctx context.Context) error {
	// GH#2474 / bd-578h9.2: auto-commit pending changes before pull, matching
	// server-mode pullFromRemote and PullFrom. Leftovers from a crashed
	// command would otherwise make the merge refuse to start.
	if _, err := s.CommitPending(ctx, "beads"); err != nil {
		return fmt.Errorf("commit pending before pull: %w", err)
	}
	preHead := s.preMergeHead(ctx)
	err := s.withPeerAuth(ctx, defaultRemote, func(user string) error {
		return s.withMutatingPinnedDBConn(ctx, func(db versioncontrolops.DBConn) error {
			return vcPull(ctx, db, defaultRemote, s.branch, user)
		})
	})
	if err != nil {
		return err
	}
	return s.recomputeBlockedAfterPull(ctx, preHead)
}

// PullWithStrategy implements storage.StrategicPuller for `bd dolt pull
// --strategy` (#4992 part 2). Identical to Pull except conflicts the
// auto-resolver declines are resolved with strategy instead of aborting the
// merge for the operator; see versioncontrolops.PullWithStrategy.
func (s *EmbeddedDoltStore) PullWithStrategy(ctx context.Context, strategy string) error {
	if _, err := s.CommitPending(ctx, "beads"); err != nil {
		return fmt.Errorf("commit pending before pull: %w", err)
	}
	preHead := s.preMergeHead(ctx)
	err := s.withPeerAuth(ctx, defaultRemote, func(user string) error {
		return s.withMutatingPinnedDBConn(ctx, func(db versioncontrolops.DBConn) error {
			return vcPullWithStrategy(ctx, db, defaultRemote, s.branch, user, strategy)
		})
	})
	if err != nil {
		return err
	}
	return s.recomputeBlockedAfterPull(ctx, preHead)
}

// PullRemoteWithStrategy implements storage.StrategicPuller for a named
// remote; see PullWithStrategy.
func (s *EmbeddedDoltStore) PullRemoteWithStrategy(ctx context.Context, remote, strategy string) error {
	if _, err := s.CommitPending(ctx, "beads"); err != nil {
		return fmt.Errorf("commit pending before pull: %w", err)
	}
	preHead := s.preMergeHead(ctx)
	err := s.withPeerAuth(ctx, remote, func(user string) error {
		return s.withMutatingPinnedDBConn(ctx, func(db versioncontrolops.DBConn) error {
			return vcPullWithStrategy(ctx, db, remote, s.branch, user, strategy)
		})
	})
	if err != nil {
		return err
	}
	return s.recomputeBlockedAfterPull(ctx, preHead)
}

func (s *EmbeddedDoltStore) ForcePush(ctx context.Context) error {
	return s.withPeerAuth(ctx, defaultRemote, func(user string) error {
		return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
			return vcForcePush(ctx, db, defaultRemote, s.branch, user)
		})
	})
}

func (s *EmbeddedDoltStore) PushRemote(ctx context.Context, remote string, force bool) error {
	return s.withPeerAuth(ctx, remote, func(user string) error {
		return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
			if force {
				return vcForcePush(ctx, db, remote, s.branch, user)
			}
			return vcPush(ctx, db, remote, s.branch, user)
		})
	})
}

func (s *EmbeddedDoltStore) PullRemote(ctx context.Context, remote string) error {
	// GH#2474 / bd-578h9.2: see Pull.
	if _, err := s.CommitPending(ctx, "beads"); err != nil {
		return fmt.Errorf("commit pending before pull: %w", err)
	}
	preHead := s.preMergeHead(ctx)
	err := s.withPeerAuth(ctx, remote, func(user string) error {
		return s.withMutatingPinnedDBConn(ctx, func(db versioncontrolops.DBConn) error {
			return vcPull(ctx, db, remote, s.branch, user)
		})
	})
	if err != nil {
		return err
	}
	return s.recomputeBlockedAfterPull(ctx, preHead)
}

func (s *EmbeddedDoltStore) Fetch(ctx context.Context, peer string) error {
	return s.withPeerAuth(ctx, peer, func(user string) error {
		return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
			return versioncontrolops.Fetch(ctx, db, peer, user)
		})
	})
}

func (s *EmbeddedDoltStore) PushTo(ctx context.Context, peer string) error {
	return s.withPeerAuth(ctx, peer, func(user string) error {
		return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
			return versioncontrolops.Push(ctx, db, peer, s.branch, user)
		})
	})
}

func (s *EmbeddedDoltStore) PullFrom(ctx context.Context, peer string) ([]storage.Conflict, error) {
	// Auto-commit pending changes before pull to prevent
	// "cannot merge with uncommitted changes" errors.
	if _, err := s.CommitPending(ctx, "beads"); err != nil {
		return nil, fmt.Errorf("commit pending before pull: %w", err)
	}

	preHead := s.preMergeHead(ctx)
	var conflicts []storage.Conflict
	err := s.withPeerAuth(ctx, peer, func(user string) error {
		return s.withMutatingPinnedDBConn(ctx, func(db versioncontrolops.DBConn) error {
			if pullErr := versioncontrolops.Pull(ctx, db, peer, s.branch, user); pullErr != nil {
				// bd-578h9.15: the settle machinery aborts a merge it cannot
				// auto-resolve before returning, so dolt_conflicts is already
				// empty here; the conflicts arrive captured pre-abort inside
				// MergeConflictsError instead.
				var mce *versioncontrolops.MergeConflictsError
				if errors.As(pullErr, &mce) {
					conflicts = mce.Conflicts
					return nil
				}
				return fmt.Errorf("pull from %s: %w", peer, pullErr)
			}
			return nil
		})
	})
	if err != nil || len(conflicts) > 0 {
		// Conflicted pulls skip the recompute: the operator resolves first,
		// and the next sync picks the rows up.
		return conflicts, err
	}
	if err := s.recomputeBlockedAfterPull(ctx, preHead); err != nil {
		return conflicts, fmt.Errorf("pull succeeded but is_blocked recompute failed: %w", err)
	}
	return conflicts, nil
}

// preMergeHead reads the pre-pull HEAD for the post-merge is_blocked
// recompute (bd-6dnrw.3). Empty on failure, which degrades the recompute to a
// full pass instead of skipping the hook.
func (s *EmbeddedDoltStore) preMergeHead(ctx context.Context) string {
	head, err := s.GetCurrentCommit(ctx)
	if err != nil {
		return ""
	}
	return head
}

// recomputeBlockedAfterPull recomputes the denormalized is_blocked column for
// the rows a pull's merge changed (bd-6dnrw.3) and creates a Dolt commit for
// the result. is_blocked is otherwise maintained only by local write paths, so
// a merge that brings in another clone's status or dependency changes leaves
// it stale and `bd ready` trusts it. A pull that merged nothing (HEAD
// unchanged) is a no-op; derived state converges, so committing it on every
// clone is merge-safe.
func (s *EmbeddedDoltStore) recomputeBlockedAfterPull(ctx context.Context, preHead string) error {
	if err := s.withConn(ctx, true, func(tx *sql.Tx) error {
		return issueops.RecomputeIsBlockedAfterMergeInTx(ctx, tx, preHead)
	}); err != nil {
		// The merge this recompute covers is already committed, so a plain
		// retry on the next pull would skip as "nothing merged" — leave a
		// marker so it widens its window instead (bd-578h9.11). Best-effort:
		// the recompute error is what matters.
		_ = s.withConn(ctx, true, func(tx *sql.Tx) error {
			return issueops.MarkIsBlockedRecomputePendingInTx(ctx, tx, preHead)
		})
		return err
	}
	return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
		return stageAndCommitAfterSQLCommit(ctx, db,
			map[string]bool{"issues": true}, "bd: recompute is_blocked after pull", commitAuthor)
	})
}

// ---------------------------------------------------------------------------
// Backup operations
// ---------------------------------------------------------------------------

func (s *EmbeddedDoltStore) BackupAdd(ctx context.Context, name, url string) error {
	return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
		return versioncontrolops.BackupAdd(ctx, db, name, url)
	})
}

func (s *EmbeddedDoltStore) BackupSync(ctx context.Context, name string) error {
	return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
		return versioncontrolops.BackupSync(ctx, db, name)
	})
}

func (s *EmbeddedDoltStore) BackupRemove(ctx context.Context, name string) error {
	return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
		return versioncontrolops.BackupRemove(ctx, db, name)
	})
}

// BackupDatabase registers dir as a file:// Dolt backup remote and syncs
// the database to it. The dir must exist locally. This preserves full Dolt
// commit history.
func (s *EmbeddedDoltStore) BackupDatabase(ctx context.Context, dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("backup destination does not exist: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("backup destination is not a directory: %s", dir)
	}

	backupURL, err := versioncontrolops.DirToFileURL(dir)
	if err != nil {
		return err
	}
	backupName := "backup_export"

	return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
		// Register as a backup remote (idempotent — remove first if exists).
		_ = versioncontrolops.BackupRemove(ctx, db, backupName)
		if err := versioncontrolops.BackupAdd(ctx, db, backupName, backupURL); err != nil {
			// Another backup (e.g. "default" registered by `bd backup init`) may
			// already point to this URL. In that case, sync using the existing
			// remote name rather than failing.
			if conflict := versioncontrolops.ExtractAddressConflictName(err); conflict != "" {
				if syncErr := versioncontrolops.BackupSync(ctx, db, conflict); syncErr != nil {
					return fmt.Errorf("sync to backup: %w", syncErr)
				}
				return nil
			}
			return fmt.Errorf("register backup remote: %w", err)
		}
		if err := versioncontrolops.BackupSync(ctx, db, backupName); err != nil {
			return fmt.Errorf("sync to backup: %w", err)
		}
		return nil
	})
}

// RestoreDatabase restores the database from a Dolt backup at dir.
// The dir must exist locally and contain a valid Dolt backup.
// When force is true, an existing database is overwritten.
func (s *EmbeddedDoltStore) RestoreDatabase(ctx context.Context, dir string, force bool) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("backup source does not exist: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("backup source is not a directory: %s", dir)
	}

	backupURL, err := versioncontrolops.DirToFileURL(dir)
	if err != nil {
		return err
	}

	return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
		return versioncontrolops.BackupRestore(ctx, db, backupURL, s.database, force)
	})
}
