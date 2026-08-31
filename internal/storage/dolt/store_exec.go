package dolt

import (
	"context"
	"database/sql"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type spanAttributeCache struct {
	once  sync.Once
	cache []attribute.KeyValue
}

// execContext wraps a write statement in an explicit BEGIN/COMMIT to ensure
// durability when the Dolt server runs with autocommit disabled (the default
// when started with --no-auto-commit). Without this, writes remain in an
// uncommitted implicit transaction that Dolt rolls back on connection close,
// causing silent data loss for callers that do not use db.BeginTx themselves.
func (s *DoltStore) execContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if s.closed.Load() {
		return nil, ErrStoreClosed
	}
	ctx, span := doltTracer.Start(ctx, "dolt.exec",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append(s.doltSpanAttrs(),
			attribute.String("db.operation", "exec"),
			attribute.String("db.statement", spanSQL(query)),
		)...),
	)
	var result sql.Result
	err := s.withRetry(ctx, func() error {
		tx, txErr := s.db.BeginTx(ctx, nil)
		if txErr != nil {
			return txErr
		}
		var execErr error
		result, execErr = tx.ExecContext(ctx, query, args...)
		if execErr != nil {
			_ = tx.Rollback()
			return execErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return wrapSQLCommitError("commit exec tx", commitErr)
		}
		return nil
	})
	finalErr := wrapLockError(err)
	endSpan(span, finalErr)
	return result, finalErr
}

// doltSpanAttrs returns the fixed attributes shared by all SQL spans.
// Cached to avoid allocating on every call (hot path when telemetry is disabled
// still flows through no-op tracers).
func (s *DoltStore) doltSpanAttrs() []attribute.KeyValue {
	s.spanAttrs.once.Do(func() {
		s.spanAttrs.cache = []attribute.KeyValue{
			attribute.String("db.system", "dolt"),
			attribute.Bool("db.readonly", s.readOnly),
			attribute.Bool("db.server_mode", true), // TODO: update when embedded mode returns
		}
	})
	return s.spanAttrs.cache
}
