package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

// execWithLongTimeout opens a one-shot database connection with readTimeout=5m
// and executes the given query. Push/pull operations can exceed the default
// readTimeout when the server performs network I/O to git remotes.
//
// The query is wrapped in an explicit transaction (BEGIN/COMMIT) so that
// DOLT_PULL merge operations succeed even when the server runs with
// autocommit=1. Without this, Dolt rejects merges under autocommit because
// it cannot expose conflict-resolution tables to the caller.
func (s *DoltStore) execWithLongTimeout(ctx context.Context, query string, args ...any) error {
	cfg, err := mysql.ParseDSN(s.connStr)
	if err != nil {
		return fmt.Errorf("failed to parse DSN for long-timeout connection: %w", err)
	}
	cfg.ReadTimeout = 5 * time.Minute
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return fmt.Errorf("failed to open long-timeout connection: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
