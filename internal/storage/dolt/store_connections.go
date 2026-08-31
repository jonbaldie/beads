package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
)

// execWithLongTimeoutNoTx executes a long-running Dolt stored procedure without
// an explicit transaction. Push operations do not need the pull/merge conflict
// handling above, and DOLT_PUSH has diverged from direct `dolt push` behavior
// when wrapped in a SQL transaction.
func (s *DoltStore) execWithLongTimeoutNoTx(ctx context.Context, query string, args ...any) error {
	db, err := s.oneShotConn(5 * time.Minute)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, query, args...)
	return err
}

// oneShotConn opens a one-shot connection with the given read deadline
// (0 = no deadline), for callers that pass a DBConn into versioncontrolops.
// The pool's 10s ReadTimeout kills any server-side procedure that performs
// sustained network I/O; push/pull use 5m, while backup sync/restore use no
// deadline at all — a first sync to a remote destination (gs://) can exceed
// any fixed budget, and the server aborts the transfer when the client
// connection drops, so a too-short deadline can never converge by retrying.
// Caller closes.
func (s *DoltStore) oneShotConn(readTimeout time.Duration) (*sql.DB, error) {
	cfg, err := mysql.ParseDSN(s.connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse DSN for long-timeout connection: %w", err)
	}
	cfg.ReadTimeout = readTimeout
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("failed to open long-timeout connection: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
