package dolt

import (
	"context"
	"errors"
	"fmt"

	"github.com/cenkalti/backoff/v4"
)

// withRetry executes an operation with retry for transient errors.
// If a circuit breaker is configured, it checks the breaker before each attempt
// and records connection failures/successes to coordinate fail-fast across processes.
func (s *DoltStore) withRetry(ctx context.Context, op func() error) error {
	return s.withRetryClassified(ctx, op, isRetryableError)
}

// withCircuitWrite admits one externally visible write and records its
// terminal success. Nested retry helpers keep failure accounting but defer
// their success reset to this boundary.
func (s *DoltStore) withCircuitWrite(ctx context.Context, op func(context.Context) error) error {
	if circuitWriteManaged(ctx) {
		return op(ctx)
	}
	if s.breaker != nil && !s.breaker.Allow() {
		doltMetrics.circuitRejected.Add(ctx, 1)
		return ErrCircuitOpen
	}
	err := op(context.WithValue(ctx, circuitWriteContextKey{}, struct{}{}))
	if err == nil && s.breaker != nil {
		s.breaker.RecordSuccess()
	}
	return err
}

func (s *DoltStore) withRetryClassified(ctx context.Context, op func() error, retryable func(error) bool) error {
	// Circuit breaker: fail-fast if the server is known to be down.
	if !circuitWriteManaged(ctx) && s.breaker != nil && !s.breaker.Allow() {
		doltMetrics.circuitRejected.Add(ctx, 1)
		return ErrCircuitOpen
	}

	attempts := 0
	bo := newServerRetryBackoff()
	err := backoff.Retry(func() error {
		attempts++
		return s.classifyManagedRetry(ctx, op(), retryable)
	}, backoff.WithContext(bo, ctx))
	if attempts > 1 {
		doltMetrics.retryCount.Add(ctx, int64(attempts-1))
	}
	return err
}

// classifyManagedRetry maps one attempt's result to a backoff decision: nil to
// stop on success, a bare error to retry, or a backoff.Permanent to stop on
// failure. It owns the attempt's breaker accounting, deferring the success reset
// to an outer withCircuitWrite boundary when one is active (circuitWriteManaged).
func (s *DoltStore) classifyManagedRetry(ctx context.Context, err error, retryable func(error) bool) error {
	if err == nil {
		if !circuitWriteManaged(ctx) && s.breaker != nil {
			s.breaker.RecordSuccess()
		}
		return nil
	}
	// An already-permanent error (e.g. a callback-entered RunInTransaction
	// failure) is terminal and must not be re-wrapped.
	var permanent *backoff.PermanentError
	if errors.As(err, &permanent) {
		return err
	}
	// An indeterminate commit is never replayed — replay could double-apply —
	// but a connection loss still feeds the breaker before we stop.
	if errors.Is(err, ErrCommitIndeterminate) {
		if tripped := s.recordRetryFailure(ctx, err); tripped != nil {
			return tripped
		}
		return backoff.Permanent(err)
	}
	if retryable(err) {
		if tripped := s.recordRetryFailure(ctx, err); tripped != nil {
			return tripped
		}
		return err // backoff will retry
	}
	return backoff.Permanent(err) // non-retryable — stop immediately
}

// recordRetryFailure records a connection-level failure to the breaker. It
// returns a permanent "circuit breaker tripped" error when this failure trips
// the breaker — signaling the retry loop to stop — and nil otherwise, including
// when err is not a connection error or no breaker is configured.
func (s *DoltStore) recordRetryFailure(ctx context.Context, err error) error {
	if s.breaker == nil || !isConnectionError(err) {
		return nil
	}
	s.breaker.RecordFailure()
	if s.breaker.State() == circuitOpen {
		doltMetrics.circuitTrips.Add(ctx, 1)
		return backoff.Permanent(fmt.Errorf("%w (circuit breaker tripped)", err))
	}
	return nil
}
