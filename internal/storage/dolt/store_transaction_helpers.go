package dolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/cenkalti/backoff/v4"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/jonbaldie/beads/internal/storage/issueops"
)

// withTransactionSetupRetry extends the general connection retry policy with
// rollback-guaranteed transaction errors. Public transaction wrappers use it
// only before their callback begins; callback-entered errors are permanent.
func (s *DoltStore) withTransactionSetupRetry(ctx context.Context, op func() error) error {
	return s.withRetryClassified(ctx, op, func(err error) bool {
		return isRetryableError(err) || isDoltAutocommitRollbackError(err) || isSerializationError(err)
	})
}

func (s *DoltStore) scopeEventsJournalTransaction(tx *sql.Tx) func() {
	return issueops.ScopeEventsJournalTransaction(tx, s.eventsJournalEnabled.Load())
}

// withReadTx runs fn inside a transaction while holding the store's read-lock.
// Used for read operations that need a *sql.Tx to share issueops functions.
//
// The whole BeginTx+fn is wrapped in withRetry so a transient connection error
// (e.g. "invalid connection" when the dolt sql-server reaps a pooled connection
// that has been idle past its wait_timeout) is retried rather than surfaced to
// the caller. This is safe because fn is read-only and the transaction is always
// rolled back, so re-running the operation has no side effects.
func (s *DoltStore) withReadTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if s.closed.Load() {
		return ErrStoreClosed
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.withRetry(ctx, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin read tx: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		return fn(tx)
	})
}

// withReadTxLongTimeout is like withReadTx but runs fn against a dedicated
// one-shot connection with a 5-minute read timeout (see openLongTimeoutConn)
// instead of the shared pool's 10s ReadTimeout (see buildServerDSN). Use for
// read queries that are known to legitimately run long, e.g. dolt_history_*
// system-table scans on issues with many revisions — the pooled 10s client
// timeout otherwise surfaces as an intermittent MySQL i/o timeout / invalid
// connection error (ga-ahnxx) well before the query would have finished on
// its own. Note this only removes the client-side ceiling: the Dolt server's
// own read_timeout_millis (often configured short to bound orphaned-connection
// pileup — see the comment next to it) still applies server-side and can
// independently abort a query whose per-row production stalls past that window.
func (s *DoltStore) withReadTxLongTimeout(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if s.closed.Load() {
		return ErrStoreClosed
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.withRetry(ctx, func() error {
		db, err := s.openLongTimeoutConn()
		if err != nil {
			return err
		}
		defer db.Close()
		// The fresh one-shot connection defaults to the default branch, not
		// whatever branch the store's pooled session (s.db) is actually
		// checked out to. The pool is branch-isolated (MaxOpenConns(1)
		// semantics — see the comment on MaxOpenConns above), so ask it for
		// its real active branch and reproduce that checkout here before
		// starting the read tx; otherwise a query like dolt_history_* would
		// silently read the wrong branch after any in-process checkout,
		// including a test-harness CALL DOLT_CHECKOUT run directly against
		// s.db, which bypasses Store.Checkout and leaves s.branch stale.
		var branch string
		if scanErr := s.db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&branch); scanErr == nil {
			if branch != "" {
				if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", branch); err != nil {
					return fmt.Errorf("checkout active branch %q on long-timeout connection: %w", branch, err)
				}
			}
		} else if s.branch != "" {
			// Fall back to the store's recorded branch rather than failing
			// the whole call outright.
			if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", s.branch); err != nil {
				return fmt.Errorf("checkout fallback branch %q on long-timeout connection: %w", s.branch, err)
			}
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin read tx: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		return fn(tx)
	})
}

func (s *DoltStore) withRetryTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	// Keep circuit admission at the transaction retry boundary. Calling
	// withRetry from here would multiply retries and could replay a write after
	// an indeterminate commit.
	if err := s.admitRetryTransaction(ctx); err != nil {
		return err
	}

	bo := retryTransactionBackoff(s.serverMode)
	return backoff.Retry(func() error {
		return s.retryWriteTransaction(ctx, fn)
	}, backoff.WithContext(bo, ctx))
}

func (s *DoltStore) admitRetryTransaction(ctx context.Context) error {
	if !circuitWriteManaged(ctx) && s.breaker != nil && !s.breaker.Allow() {
		doltMetrics.circuitRejected.Add(ctx, 1)
		return ErrCircuitOpen
	}
	return nil
}

func retryTransactionBackoff(serverMode bool) *backoff.ExponentialBackOff {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 25 * time.Millisecond
	bo.MaxElapsedTime = 5 * time.Second
	if serverMode {
		bo.MaxElapsedTime = 15 * time.Second
	}
	return bo
}

func (s *DoltStore) retryWriteTransaction(ctx context.Context, fn func(tx *sql.Tx) error) error {
	err := s.withWriteTx(ctx, fn)
	if err == nil {
		if !circuitWriteManaged(ctx) && s.breaker != nil {
			s.breaker.RecordSuccess()
		}
		return nil
	}
	// Dolt's exact 1105 autocommit rollback proves the transaction did not
	// land. This is the only 1105 replayed, and withRetryTx is the boundary
	// that recreates the complete SQL transaction on every attempt.
	if isDoltAutocommitRollbackError(err) {
		doltMetrics.serializationErrors.Add(ctx, 1)
		doltMetrics.writeRetries.Add(ctx, 1, metric.WithAttributes(attribute.String("type", "serialization")))
		return err
	}
	// A commit result marked indeterminate may have landed before its response
	// was lost. Never replay the callback in that case.
	if errors.Is(err, ErrCommitIndeterminate) {
		err = s.recordDoltPublicationFailure(ctx, err)
		return backoff.Permanent(fmt.Errorf("write commit result indeterminate after connection loss (not retried to avoid double-apply): %w", err))
	}
	// Serialization failures (1213/1205) guarantee a server-side rollback, so
	// the write never landed — safe to replay at any phase.
	if isSerializationError(err) {
		doltMetrics.serializationErrors.Add(ctx, 1)
		doltMetrics.writeRetries.Add(ctx, 1, metric.WithAttributes(attribute.String("type", "serialization")))
		return err
	}
	return s.classifyRetryableWriteError(ctx, err)
}

func (s *DoltStore) classifyRetryableWriteError(ctx context.Context, err error) error {
	// Connection failures reaching this branch happened before commit;
	// withWriteTx marks ambiguous commit response loss with the public
	// ErrCommitIndeterminate sentinel above.
	if !isRetryableError(err) {
		return backoff.Permanent(err)
	}
	doltMetrics.writeRetries.Add(ctx, 1, metric.WithAttributes(attribute.String("type", "connection")))
	if s.breaker != nil && isConnectionError(err) {
		s.breaker.RecordFailure()
		if s.breaker.State() == circuitOpen {
			doltMetrics.circuitTrips.Add(ctx, 1)
			return backoff.Permanent(fmt.Errorf("%w (circuit breaker tripped)", err))
		}
	}
	return err
}

func (s *DoltStore) withWriteTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if s.closed.Load() {
		return ErrStoreClosed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin write tx: %w", err)
	}
	clearJournalScope := s.scopeEventsJournalTransaction(tx)
	defer clearJournalScope()
	if err := fn(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		return wrapSQLCommitError("commit write tx", err)
	}
	return nil
}
