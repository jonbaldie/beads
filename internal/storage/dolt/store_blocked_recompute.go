package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

// recomputeBlockedAfterPull recomputes the denormalized is_blocked column for
// the rows a pull's merge changed (bd-6dnrw.3) and commits the result.
// is_blocked is otherwise maintained only by local write paths, so a merge
// that brings in another clone's status or dependency changes leaves it stale
// and `bd ready` trusts it. fromCommit is the pre-pull HEAD; empty means it
// could not be read, which degrades to a full recompute. A pull that merged
// nothing (HEAD unchanged) is a no-op.
func (s *DoltStore) recomputeBlockedAfterPull(ctx context.Context, fromCommit string) error {
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		return s.recomputeBlockedAfterPullUnchecked(ctx, fromCommit)
	})
}

func (s *DoltStore) recomputeBlockedAfterPullUnchecked(ctx context.Context, fromCommit string) error {
	if err := s.recomputeBlockedTx(ctx, fromCommit); err != nil {
		// The merge this recompute covers is already committed, so a plain
		// retry on the next pull would skip as "nothing merged" — leave a
		// marker so it widens its window instead (bd-578h9.11). Best-effort:
		// the recompute error is what matters.
		s.markBlockedRecomputePending(ctx, fromCommit)
		return err
	}
	// Derived state converges: every clone computes the same values from the
	// same merged graph, so committing is merge-safe. Commit no-ops when the
	// recompute changed nothing.
	if err := s.commitWorkingSetAfterSQLCommit(ctx, "bd: recompute is_blocked after pull", configExclude); err != nil && !isDoltNothingToCommit(err) {
		return fmt.Errorf("commit is_blocked recompute: %w", err)
	}
	return nil
}

// RecomputeAllBlocked recomputes is_blocked for every issue and wisp in one full
// pass and returns the number of rows it corrected. It is the mode-independent
// repair behind 'bd recompute-blocked' and 'bd doctor --fix' (bd-6dnrw.37): the
// scoped post-pull recompute is skipped when a re-pull merges nothing, so a
// recompute that failed after its merge committed — or a conflicted pull the
// operator resolved by hand — leaves is_blocked stale until this full pass runs.
// Idempotent: a consistent database corrects nothing.
func (s *DoltStore) RecomputeAllBlocked(ctx context.Context) (int, error) {
	var changed int
	err := s.withCircuitWrite(ctx, func(ctx context.Context) error {
		var err error
		changed, err = s.recomputeAllBlocked(ctx)
		return err
	})
	return changed, err
}

func (s *DoltStore) recomputeAllBlocked(ctx context.Context) (int, error) {
	// The full pass's batched UPDATEs carry five correlated EXISTS subqueries
	// each; on a loaded shared server a single batch can outlive the pool's
	// per-I/O deadline (default 10s, see buildServerDSN), killing the repair
	// with "i/o timeout" — and the retry dies the same way, so the owed
	// recompute never lands (bd-bn8jo). Run it on a dedicated long-timeout
	// connection like the other known-long maintenance ops.
	db, err := s.openLongTimeoutConn()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	return s.recomputeAllBlockedWithDB(ctx, db)
}

func (s *DoltStore) recomputeAllBlockedWithDB(ctx context.Context, db *sql.DB) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin is_blocked recompute: %w", err)
	}
	// One shared body across every mode (guard a dirty graph, then recompute):
	// the guard refuses to derive and commit is_blocked from uncommitted
	// issue/dependency edits, and it runs inside THIS tx so it sees exactly the
	// working set the recompute reads (bd-6dnrw.37).
	changed, err := versioncontrolops.GuardedRecomputeAllBlockedInTx(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if err := s.commitSQLTx(ctx, "commit is_blocked recompute", tx); err != nil {
		return 0, err
	}
	if changed > 0 {
		// Stage only issues — the synced table is_blocked lives on (wisps are
		// dolt_ignore'd) — so an unrelated dirty working set is not swept in.
		if err := s.doltAddAndCommit(ctx, blockedRecomputeStagedTableList(),
			versioncontrolops.BlockedRecomputeCommitMsg); err != nil {
			return int(changed), err
		}
	}
	return int(changed), nil
}

// blockedRecomputeStagedTableList is versioncontrolops.BlockedRecomputeStagedTables
// in the ordered form doltAddAndCommit takes, so the staging set of the repair
// commit is defined in exactly one place for every mode.
func blockedRecomputeStagedTableList() []string {
	staged := versioncontrolops.BlockedRecomputeStagedTables()
	tables := make([]string, 0, len(staged))
	for table := range staged {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	return tables
}

// recomputeBlockedTx runs the post-merge is_blocked recompute in its own
// transaction. Like RecomputeAllBlocked it runs on a long-timeout connection:
// a heavy merge scopes the recompute over a large diff, and a pool-deadline
// kill here is what turns into the owed full recompute in the first place
// (bd-bn8jo).
func (s *DoltStore) recomputeBlockedTx(ctx context.Context, fromCommit string) error {
	db, err := s.openLongTimeoutConn()
	if err != nil {
		return err
	}
	defer db.Close()
	return s.recomputeBlockedTxWithDB(ctx, db, fromCommit)
}

func (s *DoltStore) recomputeBlockedTxWithDB(ctx context.Context, db *sql.DB, fromCommit string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin is_blocked recompute: %w", err)
	}
	if err := issueops.RecomputeIsBlockedAfterMergeInTx(ctx, tx, fromCommit); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := s.commitSQLTx(ctx, "commit is_blocked recompute", tx); err != nil {
		return err
	}
	return nil
}

// markBlockedRecomputePending best-effort records a failed post-merge
// is_blocked recompute (bd-578h9.11); see
// issueops.MarkIsBlockedRecomputePendingInTx.
func (s *DoltStore) markBlockedRecomputePending(ctx context.Context, fromCommit string) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	if err := issueops.MarkIsBlockedRecomputePendingInTx(ctx, tx, fromCommit); err != nil {
		_ = tx.Rollback()
		return
	}
	_ = tx.Commit()
}
