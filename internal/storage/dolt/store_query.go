package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

// registerPoolGauges registers observable gauges that report sql.DB pool stats
// on each OTel collection cycle. These are essential for diagnosing shared-server
// degradation under multi-worktree load (GH#3140).
func (s *DoltStore) registerPoolGauges() {
	m := otel.Meter("github.com/jonbaldie/beads/storage/dolt")
	db := s.db

	m.Int64ObservableGauge("bd.db.pool_open", //nolint:errcheck,gosec
		metric.WithDescription("Current number of open connections (in-use + idle)"),
		metric.WithUnit("{connection}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(db.Stats().OpenConnections))
			return nil
		}),
	)
	m.Int64ObservableGauge("bd.db.pool_in_use", //nolint:errcheck,gosec
		metric.WithDescription("Connections currently in use"),
		metric.WithUnit("{connection}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(db.Stats().InUse))
			return nil
		}),
	)
	m.Int64ObservableGauge("bd.db.pool_idle", //nolint:errcheck,gosec
		metric.WithDescription("Idle connections in pool"),
		metric.WithUnit("{connection}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(db.Stats().Idle))
			return nil
		}),
	)
	m.Int64ObservableGauge("bd.db.pool_max_open", //nolint:errcheck,gosec
		metric.WithDescription("Maximum number of open connections (pool limit)"),
		metric.WithUnit("{connection}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(db.Stats().MaxOpenConnections))
			return nil
		}),
	)
}

func (s *DoltStore) commitSQLTx(ctx context.Context, op string, tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return s.recordDoltPublicationFailure(ctx, wrapSQLCommitError(op, err))
	}
	return nil
}

// DB returns the underlying sql.DB connection for direct queries.
// Use sparingly — prefer the store's typed methods for normal operations.
func (s *DoltStore) DB() *sql.DB {
	return s.db
}

// RemoteName returns the configured default sync remote name ("origin" unless
// overridden), the remote Push/Pull target when no explicit remote is given.
func (s *DoltStore) RemoteName() string {
	return s.remote
}

// BackupAdd registers a Dolt backup destination.
func (s *DoltStore) BackupAdd(ctx context.Context, name, url string) error {
	return versioncontrolops.BackupAdd(ctx, s.db, name, url)
}

// BackupSync pushes the database to the named backup destination.
// Runs on a long-timeout connection: a sync to a remote destination
// streams the database and outlives the pool's 10s ReadTimeout.
func (s *DoltStore) BackupSync(ctx context.Context, name string) error {
	db, err := s.oneShotConn(0)
	if err != nil {
		return err
	}
	defer db.Close()
	return versioncontrolops.BackupSync(ctx, db, name)
}

// BackupRemove removes a configured Dolt backup destination.
func (s *DoltStore) BackupRemove(ctx context.Context, name string) error {
	return versioncontrolops.BackupRemove(ctx, s.db, name)
}

// BackupDatabase registers dir as a file:// Dolt backup remote and syncs
// the full database to it, preserving complete commit history.
func (s *DoltStore) BackupDatabase(ctx context.Context, dir string) error {
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

	syncDB, err := s.oneShotConn(0)
	if err != nil {
		return err
	}
	defer syncDB.Close()

	// Register as a backup remote (idempotent — remove first if exists).
	_ = versioncontrolops.BackupRemove(ctx, s.db, backupName)
	if err := versioncontrolops.BackupAdd(ctx, s.db, backupName, backupURL); err != nil {
		// Another backup (e.g. "default" registered by `bd backup init`) may
		// already point to this URL. In that case, sync using the existing
		// remote name rather than failing.
		if conflict := versioncontrolops.ExtractAddressConflictName(err); conflict != "" {
			if syncErr := versioncontrolops.BackupSync(ctx, syncDB, conflict); syncErr != nil {
				return fmt.Errorf("sync to backup: %w", syncErr)
			}
			return nil
		}
		return fmt.Errorf("register backup remote: %w", err)
	}
	if err := versioncontrolops.BackupSync(ctx, syncDB, backupName); err != nil {
		return fmt.Errorf("sync to backup: %w", err)
	}
	return nil
}

// RestoreDatabase restores the database from a Dolt backup at dir.
// When force is true, an existing database is overwritten.
func (s *DoltStore) RestoreDatabase(ctx context.Context, dir string, force bool) error {
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
	db, err := s.oneShotConn(0)
	if err != nil {
		return err
	}
	defer db.Close()
	return versioncontrolops.BackupRestore(ctx, db, backupURL, s.database, force)
}

// QueryContext wraps s.db.QueryContext with retry for transient errors.
// Exported so callers (e.g. backup) can run ad-hoc queries with retry
// instead of going through the raw *sql.DB.
func (s *DoltStore) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.queryContext(ctx, query, args...)
}

// queryContext wraps s.db.QueryContext with retry for transient errors.
func (s *DoltStore) queryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if s.closed.Load() {
		return nil, ErrStoreClosed
	}
	ctx, span := doltTracer.Start(ctx, "dolt.query",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append(s.doltSpanAttrs(),
			attribute.String("db.operation", "query"),
			attribute.String("db.statement", spanSQL(query)),
		)...),
	)
	var rows *sql.Rows
	err := s.withRetry(ctx, func() error {
		// Close any Rows from a previous failed attempt to avoid leaking connections.
		if rows != nil {
			_ = rows.Close()
			rows = nil
		}
		var queryErr error
		rows, queryErr = s.db.QueryContext(ctx, query, args...)
		return queryErr
	})
	finalErr := wrapLockError(err)
	endSpan(span, finalErr)
	return rows, finalErr
}

// queryRowContext wraps s.db.QueryRowContext with retry for transient errors.
// The scan function receives the *sql.Row and should call .Scan() on it.
func (s *DoltStore) queryRowContext(ctx context.Context, scan func(*sql.Row) error, query string, args ...any) error {
	if s.closed.Load() {
		return ErrStoreClosed
	}
	ctx, span := doltTracer.Start(ctx, "dolt.query_row",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append(s.doltSpanAttrs(),
			attribute.String("db.operation", "query_row"),
			attribute.String("db.statement", spanSQL(query)),
		)...),
	)
	finalErr := wrapLockError(s.withRetry(ctx, func() error {
		row := s.db.QueryRowContext(ctx, query, args...)
		return scan(row)
	}))
	endSpan(span, finalErr)
	return finalErr
}
