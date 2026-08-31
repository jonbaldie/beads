//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

// RunInTransaction executes a function within a database transaction. Its
// callback is invoked at most once per call; callers retry explicitly after a
// callback has started when their operation is safe to repeat. An error
// wrapping storage.ErrCommitIndeterminate must not be blindly replayed.
// After the SQL transaction commits, dirty tables are selectively staged and a
// Dolt version commit is created with the given message.
func (s *EmbeddedDoltStore) RunInTransaction(ctx context.Context, commitMsg string, fn func(tx storage.Transaction) error) error {
	return s.runTransaction(ctx, commitMsg, func(tx *embeddedTransaction) error { return fn(tx) })
}

// RunInIssueLifecycleTransaction keeps lifecycle work on the embedded store's
// one transaction and stages only durable rows changed by that work.
func (s *EmbeddedDoltStore) RunInIssueLifecycleTransaction(ctx context.Context, commitMsg string, fn func(tx storage.IssueLifecycleTransaction) error) error {
	return s.runTransaction(ctx, commitMsg, func(tx *embeddedTransaction) error { return fn(tx) })
}

func (s *EmbeddedDoltStore) runTransaction(ctx context.Context, commitMsg string, fn func(tx *embeddedTransaction) error) error {
	return s.runTransactionWithMessage(ctx, func(tx *embeddedTransaction) (string, error) {
		return commitMsg, fn(tx)
	})
}

// runTransactionWithMessage is runTransaction for work whose commit message is
// only known once the body has run. A ready claim names the id it won, and
// nothing outside the transaction can predict which one that is.
func (s *EmbeddedDoltStore) runTransactionWithMessage(ctx context.Context, fn func(tx *embeddedTransaction) (string, error)) error {
	var tracker versioncontrolops.DirtyTableTracker
	var commitMsg string

	if err := s.withConn(ctx, true, func(tx *sql.Tx) error {
		var err error
		commitMsg, err = fn(&embeddedTransaction{tx: tx, dirty: &tracker})
		return err
	}); err != nil {
		return err
	}

	// Create a Dolt version commit from the working set changes.
	if commitMsg != "" && len(tracker.DirtyTables()) > 0 {
		if err := s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
			return versioncontrolops.StageAndCommit(ctx, db, tracker.DirtyTables(), commitMsg, commitAuthor)
		}); err != nil {
			return wrapCommitIndeterminate("embeddeddolt: stage and commit after SQL commit", err)
		}
	}
	return nil
}

func wrapCommitIndeterminate(op string, err error) error {
	return fmt.Errorf("%s: %w: %w", op, err, storage.ErrCommitIndeterminate)
}

type embeddedTransaction struct {
	tx    *sql.Tx
	dirty *versioncontrolops.DirtyTableTracker
}

// ReopenIssueWithResult lets transaction-scoped callers preserve lifecycle
// semantics when an update crosses the done boundary.

// SearchIssueIDs returns matching IDs only via issueops.SearchIssueIDsInTx.

// CycleThroughEdges reports a scheduling cycle through one of the new edges,
// including the transaction's own uncommitted dependency writes
// (bd-6dnrw.8, bd-578h9.9).

// The composite-view reads below all run on the single embedded transaction
// handle. Unlike the classic Dolt store, the embedded store has no durable/wisp
// session split, so every read here — including the both-tiers-spanning ones —
// is read-your-writes on both tiers; the InTx functions do their own wisp
// routing on the one handle. The two-session wisp caveat in the
// storage.Transaction doc applies only to the server (Dolt) backend.

// GetIssueCommentsPage returns one keyset page of an issue's comments within the
// transaction. Mirrors EmbeddedDoltStore.GetIssueCommentsPage's issueops
// delegation.

// CountIssuesByGroup returns per-group issue counts within the transaction.
// Mirrors EmbeddedDoltStore.CountIssuesByGroup's issueops delegation.

// GetDependentRecords returns the raw inbound dependency rows of targetID within
// the transaction. Mirrors EmbeddedDoltStore.GetDependentRecords; the InTx
// function spans both dependency tables itself.

// GetDependentRecordsForIssues returns the raw inbound dependency rows for a set
// of target ids within the transaction, keyed by target id. Mirrors
// EmbeddedDoltStore.GetDependentRecordsForIssues.

// CountDependentRecords returns the total inbound-edge count of targetID within
// the transaction. Mirrors EmbeddedDoltStore.CountDependentRecords.

// IsBlocked reports the denormalized transitive is_blocked flag and direct
// blockers of issueID within the transaction. Mirrors
// EmbeddedDoltStore.IsBlocked's issueops delegation.

// IsBlockedBatch reports the denormalized transitive is_blocked flag for a page
// of ids within the transaction. Mirrors EmbeddedDoltStore.IsBlockedBatch; the
// InTx function batches over both the issues and wisps tables itself.

// EventsSince returns durable events strictly after the keyset cursor within the
// transaction. Mirrors EmbeddedDoltStore.EventsSince's issueops delegation.
