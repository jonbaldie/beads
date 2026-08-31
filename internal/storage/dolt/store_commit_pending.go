package dolt

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/storage/schema"
)

// CommitWithConfig creates a Dolt commit that includes the config table.
// Use this instead of Commit when the caller intentionally modified config
// (e.g., CommitPending after 'bd config set', 'bd init', or 'bd rename-prefix').
// GH#2455: Commit() excludes config to prevent sweeping up stale changes.
func (s *DoltStore) CommitWithConfig(ctx context.Context, message string) error {
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("failed to acquire connection: %w", err)
		}
		defer conn.Close()

		if err := schema.DrainCall(ctx, conn, "CALL DOLT_COMMIT('-Am', ?, '--author', ?)", message, s.commitAuthorString()); err != nil {
			if isDoltNothingToCommit(err) {
				return nil
			}
			return s.wrapDoltPublicationFailure(ctx, "failed to commit", err)
		}
		return nil
	})
}

// doltAddAndCommit stages the specified tables and commits on a pinned
// connection. This prevents DOLT_COMMIT('-Am') from sweeping up stale
// working set changes from concurrent operations (GH#2455). Every caller has
// already committed its SQL mutation, so any publication failure here has an
// indeterminate durable outcome and must not be replayed.
func (s *DoltStore) doltAddAndCommit(ctx context.Context, tables []string, commitMsg string) error {
	// Batch/off auto-commit (bd-4wamg): leave the writes in the working set
	// for a later explicit commit point (bd dolt commit / CommitPending),
	// matching doltAddAndCommitInTx.
	if issueops.VersionCommitDeferred(ctx) {
		return nil
	}
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return s.recordDoltPublicationFailure(ctx,
				fmt.Errorf("acquire connection after SQL mutation: %w: %w", err, ErrCommitIndeterminate))
		}
		defer conn.Close()

		for _, table := range tables {
			if err := schema.DrainCall(ctx, conn, "CALL DOLT_ADD(?)", table); err != nil {
				return s.recordDoltPublicationFailure(ctx,
					fmt.Errorf("dolt add %s after SQL mutation: %w: %w", table, err, ErrCommitIndeterminate))
			}
		}

		// Skip the commit when nothing was actually staged (idempotent no-op
		// write), so Dolt does not log a server-side "nothing to commit" warning
		// on every reconcile-cadence call. The guard tests the STAGED set rather
		// than the whole working set because this helper stages only a fixed
		// table list — an unrelated dirty table must not trigger an empty '-m'
		// commit. A guard-read failure is NOT a publication failure: nothing has
		// been committed and nothing is indeterminate, so plain error return.
		staged, err := issueops.HasStagedChanges(ctx, conn)
		if err != nil {
			return fmt.Errorf("check staged changes before commit: %w", err)
		}
		if !staged {
			return nil
		}

		if err := schema.DrainCall(ctx, conn, "CALL DOLT_COMMIT('-m', ?, '--author', ?)",
			commitMsg, s.commitAuthorString()); err != nil && !isDoltNothingToCommit(err) {
			return s.recordDoltPublicationFailure(ctx,
				fmt.Errorf("dolt commit after SQL mutation: %w: %w", err, ErrCommitIndeterminate))
		}
		return nil
	})
}

func (s *DoltStore) wrapDoltPublicationFailure(ctx context.Context, op string, err error) error {
	return s.recordDoltPublicationFailure(ctx, wrapSQLCommitError(op, err))
}

// recordDoltPublicationFailure accounts once for an ambiguous connection loss
// at a Dolt publication boundary. Direct publication helpers stay outside
// withRetryTx; transaction-backed writes call this from withRetryTx itself.
func (s *DoltStore) recordDoltPublicationFailure(ctx context.Context, err error) error {
	if s.breaker == nil || !errors.Is(err, ErrCommitIndeterminate) || !isConnectionError(err) {
		return err
	}
	s.breaker.RecordFailure()
	if s.breaker.State() == circuitOpen {
		doltMetrics.circuitTrips.Add(ctx, 1)
		return fmt.Errorf("%w (circuit breaker tripped)", err)
	}
	return err
}

// HasCommittablePending reports whether the working set holds committable
// changes, excluding dolt_ignore'd tables (wisp and lease tables appear in
// dolt_status but can't be staged). Implements storage.PendingChangeDetector.
func (s *DoltStore) HasCommittablePending(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM dolt_status s
		WHERE NOT EXISTS (
			SELECT 1 FROM dolt_ignore di
			WHERE di.ignored = 1
			AND s.table_name LIKE di.pattern
		)`).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("failed to check status: %w", err)
	}
	return count > 0, nil
}

// CommitPending creates a single Dolt commit for all uncommitted changes in the working set.
// Returns (true, nil) if changes were committed, (false, nil) if there was nothing to commit,
// or (false, err) on failure. The commit message summarizes the accumulated changes by
// querying dolt_diff to count issue-level operations.
//
// This is the primary commit mechanism for batch mode, where multiple bd commands
// accumulate changes in the working set before committing at a logical boundary.
func (s *DoltStore) CommitPending(ctx context.Context, actor string) (bool, error) {
	dirty, err := s.HasCommittablePending(ctx)
	if err != nil {
		return false, err
	}
	if !dirty {
		return false, nil // Nothing to commit
	}

	msg := s.buildBatchCommitMessage(ctx, actor)
	// GH#2455: CommitPending is an explicit user action (bd dolt commit) that
	// should include ALL pending changes, including config. Use CommitWithConfig
	// instead of Commit to ensure intentional config changes are committed.
	if err := s.CommitWithConfig(ctx, msg); err != nil {
		// Dolt may report "nothing to commit" even when Status() showed changes
		// (e.g., system tables or schema-only diffs). Treat as no-op.
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "nothing to commit") || strings.Contains(errLower, "no changes") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// buildBatchCommitMessage generates a descriptive commit message summarizing
// what changed since the last commit by querying dolt_diff against HEAD.
// It reports issue-level create/update/delete counts and lists any other
// tables (labels, comments, events, etc.) that have uncommitted changes.
func (s *DoltStore) buildBatchCommitMessage(ctx context.Context, actor string) string {
	if actor == "" {
		actor = s.committerName
	}
	added, modified, removed := batchCommitIssueCounts(ctx, s.db)
	otherTables := batchCommitOtherTables(ctx, s.db)
	return formatBatchCommitMessage(actor, added, modified, removed, otherTables)
}
