package dolt

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

// finishCLIPull runs the merge-conflict auto-resolver after a CLI-based pull
// (git-protocol, credentialed, or cloud-auth remotes). CLI `dolt pull` writes any
// merge conflicts into the shared working set but, unlike the SQL DOLT_PULL path,
// returns without a transaction we can inspect — so these remotes historically
// skipped the resolver entirely. With deterministic dependency ids (#4259) a
// same-edge conflict that differs only in audit columns is safe to auto-resolve, and
// the git remote topology in #4259 is exactly this CLI path; route it through the
// same resolver as the SQL path. pullErr is what doltCLIPull returned: a pull that
// fails *because* of conflicts is recoverable once they resolve, so we inspect the
// working set regardless and only surface pullErr when nothing was resolved.
func (s *DoltStore) finishCLIPull(ctx context.Context, pullErr error) error {
	if s.readOnly {
		// A read-only store cannot resolve or commit; surface the pull result as-is.
		return pullErr
	}
	resolved, resolveErr := s.autoResolveConflictsAfterCLIPull(ctx)
	if resolveErr != nil {
		if pullErr != nil {
			return pullErr
		}
		return resolveErr
	}
	if pullErr != nil && !resolved {
		// Pull failed for a non-conflict reason, or conflicts are not auto-resolvable;
		// leave them in the working set for the operator.
		return pullErr
	}
	return nil
}

// autoResolveConflictsAfterCLIPull inspects the working set and auto-resolves the
// conflict classes that are safe without operator input (#4259 audit-only dependency
// edges, GH#2466 metadata, GH#4698 issues-table LWW). It runs on a connection from
// the store pool (s.db) on
// purpose: those connections are on the same branch the CLI `dolt pull` merged into,
// whereas a separately opened connection would default to the base branch and never
// see the conflicts. The pull's
// network transfer already completed in the subprocess, so no long-timeout connection
// is needed for the local resolve. Returns (true, nil) only if all conflicts were
// resolved and committed; (false, nil) when there is nothing to resolve or a conflict
// needs the operator, leaving the working set untouched for manual resolution.
func (s *DoltStore) autoResolveConflictsAfterCLIPull(ctx context.Context) (bool, error) {
	// Pin a single connection: @@dolt_allow_commit_conflicts is session-scoped,
	// and setting it through a pooled transaction leaks it to whichever caller
	// drains that connection next. Reset it before releasing the connection; if
	// the reset cannot run, discard the connection rather than return it dirty.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to acquire connection: %w", err)
	}
	varSet := false
	defer func() {
		if varSet {
			if _, err := conn.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 0"); err != nil {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}
		_ = conn.Close()
	}()
	// Allow committing while conflicts exist so we can inspect and resolve them.
	if _, err := conn.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 1"); err != nil {
		return false, fmt.Errorf("failed to set dolt_allow_commit_conflicts: %w", err)
	}
	varSet = true
	return resolveCLIPullTransaction(s, ctx, conn)
}

func resolveCLIPullTransaction(s *DoltStore, ctx context.Context, conn *sql.Conn) (bool, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("failed to begin transaction: %w", err)
	}
	resolved, err := resolveCLIPullConflicts(s, ctx, tx)
	if err != nil {
		return false, err
	}
	repaired, err := repairCLIPullViolations(s, ctx, tx)
	if err != nil {
		return false, err
	}
	if !resolved && !repaired {
		_ = tx.Rollback()
		return false, nil
	}
	// Conclude the merge for resolved conflicts only now, after the FK repair:
	// DOLT_COMMIT refuses a violated working set, so a merge carrying both
	// classes could never settle when the resolver committed first (bd-578h9.14).
	if resolved {
		if err := versioncontrolops.CommitResolvedConflicts(ctx, tx); err != nil {
			_ = tx.Rollback()
			return false, err
		}
	}
	if err := s.commitSQLTx(ctx, "commit resolved CLI pull conflicts", tx); err != nil {
		return false, err
	}
	return true, nil
}

func resolveCLIPullConflicts(s *DoltStore, ctx context.Context, tx *sql.Tx) (bool, error) {
	resolved, err := s.tryAutoResolveMergeConflicts(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
	}
	return resolved, err
}

func repairCLIPullViolations(s *DoltStore, ctx context.Context, tx *sql.Tx) (bool, error) {
	// bd-6dnrw.4: a CLI pull can also leave FK cascade violations in the
	// shared working set (child rows whose parent issue was deleted on the
	// other clone). Repair them like the SQL route does; unrepaired
	// violations roll back untouched for the operator.
	repaired, hadViol, err := s.tryRepairFKCascadeViolations(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		return false, err
	}
	if hadViol && !repaired {
		_ = tx.Rollback()
		return false, nil
	}
	return repaired, nil
}

// tryAutoResolveMergeConflicts auto-resolves merge conflicts that are safe to
// resolve without operator input (GH#2466 metadata, #4259 audit-only
// dependency edges, bd-6dnrw.29 schema_migrations vintage rows, GH#2474
// convergent kv.memory.* config rows, GH#4698 issues-table LWW by updated_at),
// returning (true, nil) only if ALL conflicts were resolved. The
// implementation is
// shared with the embedded pull path (bd-6dnrw.40); see
// versioncontrolops.TryAutoResolveMergeConflicts for the full contract.
func (s *DoltStore) tryAutoResolveMergeConflicts(ctx context.Context, tx *sql.Tx) (bool, error) {
	return versioncontrolops.TryAutoResolveMergeConflicts(ctx, tx)
}

// tryRepairFKCascadeViolations repairs the post-merge foreign-key constraint
// violations produced by the delete-vs-insert cascade hazard (bd-6dnrw.4).
// The caller's transaction must run with @@dolt_force_transaction_commit=1
// for the merge to survive long enough to be repaired, and must NOT commit
// when (repaired=false, had=true) — unrepaired violations are the operator's.
// The implementation is shared with the embedded pull path (bd-6dnrw.40); see
// versioncontrolops.TryRepairFKCascadeViolations for the full contract.
func (s *DoltStore) tryRepairFKCascadeViolations(ctx context.Context, tx *sql.Tx) (repaired, had bool, err error) {
	return versioncontrolops.TryRepairFKCascadeViolations(ctx, tx)
}
