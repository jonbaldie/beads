package dolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/cenkalti/backoff/v4"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
	"github.com/jonbaldie/beads/internal/types"
)

// doltTransaction implements storage.Transaction for Dolt
type doltTransaction struct {
	transactionOperations

	resources doltTransactionResources

	flags transactionFlags
}

type doltTransactionResources struct {
	regularTx *sql.Tx
	ignoredTx *sql.Tx
	store     *DoltStore
	dirty     versioncontrolops.DirtyTableTracker
}

type transactionOperations struct {
	transactionRuntime
	transactionIssueWrite
	transactionIssueImport
	transactionIssueRead
	transactionDependencyAdd
	transactionDependencyWrite
	transactionDependencyRead
	transactionLabels
	transactionConfig
	transactionComments
	transactionComposite
}

type transactionRuntime struct{ *doltTransaction }
type transactionIssueWrite struct{ *doltTransaction }
type transactionIssueImport struct{ *doltTransaction }
type transactionIssueRead struct{ *doltTransaction }
type transactionDependencyAdd struct{ *doltTransaction }
type transactionDependencyWrite struct{ *doltTransaction }
type transactionDependencyRead struct{ *doltTransaction }
type transactionLabels struct{ *doltTransaction }
type transactionConfig struct{ *doltTransaction }
type transactionComments struct{ *doltTransaction }
type transactionComposite struct{ *doltTransaction }

func newDoltTransaction(regularTx, ignoredTx *sql.Tx, store *DoltStore) *doltTransaction {
	t := &doltTransaction{
		resources: doltTransactionResources{regularTx: regularTx, ignoredTx: ignoredTx, store: store},
		flags:     transactionFlags{},
	}
	t.transactionOperations.transactionRuntime.doltTransaction = t
	t.transactionOperations.transactionIssueWrite.doltTransaction = t
	t.transactionOperations.transactionIssueImport.doltTransaction = t
	t.transactionOperations.transactionIssueRead.doltTransaction = t
	t.transactionOperations.transactionDependencyAdd.doltTransaction = t
	t.transactionOperations.transactionDependencyWrite.doltTransaction = t
	t.transactionOperations.transactionDependencyRead.doltTransaction = t
	t.transactionOperations.transactionLabels.doltTransaction = t
	t.transactionOperations.transactionConfig.doltTransaction = t
	t.transactionOperations.transactionComments.doltTransaction = t
	t.transactionOperations.transactionComposite.doltTransaction = t
	return t
}

// CreateIssueImport is the import-friendly issue creation hook.
// Dolt does not enforce prefix validation at the storage layer, so this delegates to CreateIssue.
func (t *transactionIssueImport) CreateIssueImport(ctx context.Context, issue *types.Issue, actor string, _ bool) error {
	return t.CreateIssue(ctx, issue, actor)
}

// RunInTransaction executes a function within a database transaction. Its
// callback is invoked at most once per call; callers retry explicitly after a
// callback has started when their operation is safe to repeat. The commitMsg is
// used for the DOLT_COMMIT that makes regular writes visible in Dolt history.
// Wisp routing is handled by individual transaction methods based on
// ID/Ephemeral.
func (s *DoltStore) RunInTransaction(ctx context.Context, commitMsg string, fn func(tx storage.Transaction) error) error {
	return s.runInTransaction(ctx, commitMsg, fn, s.runDoltTransaction)
}

func (s *DoltStore) runInTransaction(
	ctx context.Context,
	commitMsg string,
	fn func(storage.Transaction) error,
	run func(context.Context, string, func(storage.Transaction) error) error,
) error {
	return s.withTransactionSetupRetry(ctx, func() error {
		invoked := false
		var callbackErr error
		err := run(ctx, commitMsg, func(tx storage.Transaction) error {
			invoked = true
			callbackErr = fn(tx)
			return callbackErr
		})
		if invoked && err != nil {
			// Callback failures are caller-owned and must not affect server
			// health accounting. Infrastructure failures after a successful
			// callback keep the at-most-once boundary too, except an explicitly
			// indeterminate commit reaches withRetry so it can record the lost
			// connection before stopping without replay.
			if callbackErr == nil && errors.Is(err, ErrCommitIndeterminate) {
				return err
			}
			return backoff.Permanent(err)
		}
		return err
	})
}

// RunInIssueLifecycleTransaction runs a lifecycle transition and its durable
// side effects through one SQL transaction and one Dolt commit attempt.
func (s *DoltStore) RunInIssueLifecycleTransaction(ctx context.Context, commitMsg string, fn func(tx storage.IssueLifecycleTransaction) error) error {
	return s.runInIssueLifecycleTransaction(ctx, commitMsg, fn, s.withWriteTx)
}

// runInIssueLifecycleTransaction retries only failures that occur before the
// public callback starts. Once fn has run, its caller-owned work must never be
// replayed, even when Dolt proves that the SQL transaction rolled back.
func (s *DoltStore) runInIssueLifecycleTransaction(
	ctx context.Context,
	commitMsg string,
	fn func(tx storage.IssueLifecycleTransaction) error,
	run func(context.Context, func(*sql.Tx) error) error,
) error {
	return s.withTransactionSetupRetry(ctx, func() error {
		invoked := false
		var callbackErr error
		err := run(ctx, func(sqlTx *sql.Tx) error {
			invoked = true
			tx := newDoltTransaction(sqlTx, sqlTx, s)
			markTransactionLifecycle(tx)
			if callbackErr = fn(tx); callbackErr != nil {
				return callbackErr
			}
			tables := tx.dirtyTableNames()
			if len(tables) == 0 {
				return nil
			}
			return s.doltAddAndCommitInTx(ctx, sqlTx, tables, commitMsg)
		})
		if invoked && err != nil {
			// An ambiguous commit reaches withRetry so connection failures still
			// count toward the circuit breaker, but it is never replayed.
			if callbackErr == nil && errors.Is(err, ErrCommitIndeterminate) {
				return err
			}
			return backoff.Permanent(err)
		}
		return err
	})
}

func (t *transactionRuntime) dirtyTableNames() []string {
	tables := make([]string, 0, len(t.resources.dirty.DirtyTables()))
	for table := range t.resources.dirty.DirtyTables() {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	return tables
}

func (s *DoltStore) runDoltTransaction(ctx context.Context, commitMsg string, fn func(tx storage.Transaction) error) error {
	// Pin a single connection for the entire operation: SQL transaction,
	// config protection, and DOLT_COMMIT must all run on the same Dolt
	// session. Each pool connection has an independent working set in Dolt
	// SQL server mode, so mixing connections causes DOLT_COMMIT to see
	// stale or unrelated changes. (GH#2455)
	conn, err := s.acquireDoltTransactionConnection(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	currentBranch, regularTx, err := beginDoltRegularTransaction(ctx, conn)
	if err != nil {
		return err
	}
	ignoredTx, ignoredCleanup, journalPinned, err := s.prepareDoltTransaction(ctx, currentBranch, regularTx)
	if err != nil {
		_ = regularTx.Rollback()
		return err
	}
	defer ignoredCleanup()

	clearJournalScope := issueops.ScopeEventsJournalTransaction(regularTx, journalPinned)
	defer clearJournalScope()

	tx := newDoltTransaction(regularTx, ignoredTx, s)
	setTransactionJournalPinned(tx, journalPinned)
	if err := invokeDoltTransaction(tx, fn); err != nil {
		return err
	}
	return s.finishDoltTransaction(ctx, conn, tx, commitMsg)
}

func (s *DoltStore) acquireDoltTransactionConnection(ctx context.Context) (*sql.Conn, error) {
	statsBefore := s.db.Stats()
	acquireStart := time.Now()
	conn, err := s.db.Conn(ctx)
	acquireMs := float64(time.Since(acquireStart).Microseconds()) / 1000.0
	doltMetrics.connAcquireMs.Record(ctx, acquireMs)

	if err == nil {
		statsAfter := s.db.Stats()
		if statsAfter.WaitCount > statsBefore.WaitCount {
			doltMetrics.poolWaitCount.Add(ctx, statsAfter.WaitCount-statsBefore.WaitCount)
			waitMs := float64(statsAfter.WaitDuration-statsBefore.WaitDuration) / float64(time.Millisecond)
			doltMetrics.poolWaitMs.Record(ctx, waitMs)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("failed to acquire connection: %w", err)
	}
	return conn, nil
}

func beginDoltRegularTransaction(ctx context.Context, conn *sql.Conn) (string, *sql.Tx, error) {
	var currentBranch string
	if err := conn.QueryRowContext(ctx, "SELECT active_branch()").Scan(&currentBranch); err != nil {
		return "", nil, fmt.Errorf("failed to read active branch: %w", err)
	}
	regularTx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, fmt.Errorf("failed to begin regular tx: %w", err)
	}
	return currentBranch, regularTx, nil
}

func (s *DoltStore) prepareDoltTransaction(ctx context.Context, branch string, regularTx *sql.Tx) (*sql.Tx, func(), bool, error) {
	journalPinned := s.eventsJournalEnabled.Load()
	if journalPinned {
		return regularTx, func() {}, true, nil
	}

	ignoredCleanup, ignoredTx, err := s.beginIgnoredTxOnBranch(ctx, branch)
	if err != nil {
		return nil, nil, false, err
	}
	return ignoredTx, ignoredCleanup, false, nil
}

func invokeDoltTransaction(tx *doltTransaction, fn func(tx storage.Transaction) error) error {
	defer func() {
		if r := recover(); r != nil {
			rollbackDoltTransaction(tx)
			panic(r)
		}
	}()
	if err := fn(tx); err != nil {
		rollbackDoltTransaction(tx)
		return err
	}
	return nil
}

func rollbackDoltTransaction(tx *doltTransaction) {
	_ = tx.resources.regularTx.Rollback()
	if !transactionJournalPinned(tx) {
		_ = tx.resources.ignoredTx.Rollback()
	}
}

// finishDoltTransaction commits the regular SQL transaction, its associated
// Dolt revision, and then the ignored-table transaction. Once the regular SQL
// transaction succeeds, later failures have an indeterminate durable outcome.
// When the journal pinned both planes into the regular transaction, that single
// commit already carried the ignored tables and there is no second transaction
// to roll back or commit.
func (s *DoltStore) finishDoltTransaction(ctx context.Context, conn *sql.Conn, tx *doltTransaction, commitMsg string) error {
	rollbackIgnored := func() {
		if !transactionJournalPinned(tx) {
			_ = tx.resources.ignoredTx.Rollback()
		}
	}

	if err := tx.resources.regularTx.Commit(); err != nil {
		rollbackIgnored()
		return wrapSQLCommitError("sql commit (regular)", err)
	}

	if err := versioncontrolops.StageAndCommit(ctx, conn, tx.resources.dirty.DirtyTables(), commitMsg, s.commitAuthorString()); err != nil {
		rollbackIgnored()
		return fmt.Errorf("stage and commit after regular SQL commit: %w: %w", err, ErrCommitIndeterminate)
	}

	if transactionJournalPinned(tx) {
		return nil
	}
	if err := tx.resources.ignoredTx.Commit(); err != nil {
		return fmt.Errorf("sql commit (ignored, regular already committed): %w: %w", err, ErrCommitIndeterminate)
	}
	return nil
}

// ignoredTxBorrowTimeout bounds how long a borrow of a second warm connection
// from the main pool may wait before falling back to a dedicated fresh dial. It
// keeps the second acquisition from ever waiting unboundedly while the caller
// already holds the first (regular-tx) connection, which is what makes deadlock
// impossible by construction on the borrow path.
const ignoredTxBorrowTimeout = 250 * time.Millisecond

// beginIgnoredTxOnBranch starts the ignored-tables transaction, checked out to
// the regular transaction's branch. It borrows a second warm connection from the
// main pool when one is safely available — the hosted-gateway churn fix: once the
// pool is warm this costs zero new MySQL handshakes and zero Dolt session-setup
// round-trips per write. It falls back to a dedicated single-connection pool when
// borrowing could deadlock (MaxOpenConns==1, the documented case that every
// branch-isolated test exercises) or when the pool is exhausted or a borrowed
// connection turns out to be stale.
//
// The returned cleanup closure releases whichever acquisition path was taken, so
// the caller does not need to know which one ran.
func (s *DoltStore) beginIgnoredTxOnBranch(ctx context.Context, branch string) (cleanup func(), tx *sql.Tx, err error) {
	// Borrow fast path: reuse an already-open pooled connection. Unlike the
	// fallback below, this path never switches the session's branch — see
	// beginBorrowedTx for the pool invariant it preserves.
	if conn := s.borrowConnForIgnoredTx(ctx); conn != nil {
		tx, err := beginBorrowedTx(ctx, conn, branch)
		if err == nil {
			return func() { _ = conn.Close() }, tx, nil
		}
		// A stale pooled connection or a session on another branch: a fresh
		// dial always worked before, so discard this one (its session state is
		// untouched) and fall through to the fallback.
		_ = conn.Close()
	}

	// Fallback: a dedicated single-connection pool, paying the fresh dial the
	// borrow path exists to avoid.
	doltMetrics.ignoredTxFreshPool.Add(ctx, 1)
	db, err := sql.Open("mysql", s.connStr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open ignored tx connection: %w", err)
	}
	db.SetMaxOpenConns(1)

	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("failed to acquire ignored tx connection: %w", err)
	}

	tx, err = beginTxOnConn(ctx, conn, branch)
	if err != nil {
		_ = conn.Close()
		_ = db.Close()
		return nil, nil, err
	}

	return func() { _ = conn.Close(); _ = db.Close() }, tx, nil
}

// borrowConnForIgnoredTx returns a second connection borrowed from the main pool
// for the ignored-tables transaction, or nil if borrowing is unsafe or would
// block. The caller falls back to a dedicated single-connection pool on nil.
func (s *DoltStore) borrowConnForIgnoredTx(ctx context.Context) *sql.Conn {
	st := s.db.Stats()
	// MaxOpenConns==1: the caller already pinned the pool's only connection for
	// the regular tx, so a borrow would deadlock. Preserve today's behavior.
	if st.MaxOpenConnections == 1 {
		return nil
	}
	// Exhausted pool: skip the wait and go straight to the fallback.
	// MaxOpenConnections==0 means unlimited — always safe to grow.
	if st.MaxOpenConnections > 0 && st.InUse >= st.MaxOpenConnections {
		return nil
	}

	bctx, cancel := context.WithTimeout(ctx, ignoredTxBorrowTimeout)
	defer cancel()
	conn, err := s.db.Conn(bctx)
	if err != nil {
		// Lost a stats/Conn race, parent ctx canceled, or a slow dial exceeded
		// the borrow timeout — fall back to a fresh dial.
		return nil
	}
	return conn
}

// beginBorrowedTx begins the ignored-tables transaction on a connection
// borrowed from the main pool, without ever changing that session's branch.
//
// Pool invariant: DOLT_CHECKOUT is session-level, and the borrow cleanup
// returns the connection to the pool as-is — so switching its branch here
// would leak a foreign branch into the pool for an unrelated later caller.
// Every other production checkout site (federation staging, compact, flatten)
// restores the branch before releasing the connection; the borrow path
// preserves the same invariant by refusing instead of switching. Today no
// shipped flow diverges a pool session's branch from the regular tx's branch
// (DoltStore.Checkout has no non-test callers), so this is defense in depth
// for future Checkout callers and for multi-connection tests.
//
// Instead of an unconditional checkout it verifies the session is already on
// the requested branch — the overwhelmingly common case — and sends the
// caller to the fresh-dial fallback otherwise. Same round-trip count as the
// checkout it replaces (one statement), so the borrow fast path stays free.
func beginBorrowedTx(ctx context.Context, conn *sql.Conn, branch string) (*sql.Tx, error) {
	var active string
	if err := conn.QueryRowContext(ctx, "SELECT active_branch()").Scan(&active); err != nil {
		return nil, fmt.Errorf("failed to read borrowed conn's active branch: %w", err)
	}
	if active != branch {
		return nil, fmt.Errorf("borrowed conn is on branch %q, want %q: refusing to switch a pooled session's branch", active, branch)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin ignored tx: %w", err)
	}
	return tx, nil
}

// beginTxOnConn checks a connection out to branch and begins a transaction on
// it. Only the fallback path uses it: the fallback owns a dedicated
// single-connection pool, so checking its session out is safe. Every Dolt SQL
// session has its own active branch, so the explicit checkout is required on
// a fresh dial.
func beginTxOnConn(ctx context.Context, conn *sql.Conn, branch string) (*sql.Tx, error) {
	if _, err := conn.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", branch); err != nil {
		return nil, fmt.Errorf("failed to checkout ignored tx branch %s: %w", branch, err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin ignored tx: %w", err)
	}
	return tx, nil
}

// isDoltNothingToCommit returns true if the error indicates there were no
// staged changes for Dolt to commit — a benign condition.
func isDoltNothingToCommit(err error) bool {
	return issueops.IsNothingToCommitError(err)
}
