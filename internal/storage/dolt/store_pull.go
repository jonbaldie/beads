package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/jonbaldie/beads/internal/storage/schema"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

// Pull pulls changes from the remote.
// Passes branch explicitly to avoid "did not specify a branch" errors.
// For git-protocol remotes (SSH, git+https://, git://), uses CLI `dolt pull` to avoid MySQL connection timeouts.
// For non-SSH Hosted Dolt (remoteUser set), uses CALL DOLT_PULL with --user authentication.
//
// If the pull results in merge conflicts on the metadata table only (e.g., from
// stale dolt_auto_push_* rows on multi-machine setups), the conflicts are
// automatically resolved using "theirs" strategy (GH#2466).
func (s *DoltStore) Pull(ctx context.Context) (retErr error) {
	return s.pullFromRemote(ctx, s.remote)
}

// PullRemote pulls changes from a named remote. Unlike Pull(), which always
// uses the configured default remote (s.remote), PullRemote targets an
// explicit remote name. Credentials are only applied when the target remote
// matches the default remote; otherwise nil creds are used.
func (s *DoltStore) PullRemote(ctx context.Context, remote string) error {
	return s.pullFromRemote(ctx, remote)
}

// pullFromRemote is the internal implementation for all pull operations.
// It routes through CLI or SQL based on the remote's protocol and credentials.
func (s *DoltStore) pullFromRemote(ctx context.Context, remote string) (retErr error) {
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		return s.pullFromRemoteUnchecked(ctx, remote)
	})
}

func (s *DoltStore) pullFromRemoteUnchecked(ctx context.Context, remote string) (retErr error) {
	ctx, span := doltTracer.Start(ctx, "dolt.pull",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append(s.doltSpanAttrs(),
			attribute.String("dolt.remote", remote),
			attribute.String("dolt.branch", s.branch),
		)...),
	)
	defer func() { endSpan(span, retErr) }()

	// GH#2474: Auto-commit pending changes before pull to prevent
	// "cannot merge with uncommitted changes" errors. Store initialization
	// (schema init, molecule loading, metadata writes) can dirty the working
	// set before the user's pull command runs.
	if !s.readOnly {
		if err := s.commitBeforePull(ctx, "auto-commit before pull"); err != nil {
			// "nothing to commit" is fine — working set is already clean
			if !isDoltNothingToCommit(err) {
				return fmt.Errorf("failed to commit pending changes before pull: %w", err)
			}
		}
	}

	// bd-6dnrw.3: capture the pre-pull HEAD so a successful merge can recompute
	// the denormalized is_blocked column for the rows it changed. Read before
	// the transport; an unreadable HEAD degrades to a full recompute.
	preHead := ""
	if !s.readOnly {
		if h, err := s.GetCurrentCommit(ctx); err == nil {
			preHead = h
		}
	}

	if err := s.pullTransport(ctx, remote); err != nil {
		return err
	}

	if !s.readOnly {
		if err := s.recomputeBlockedAfterPull(ctx, preHead); err != nil {
			return fmt.Errorf("pull succeeded but is_blocked recompute failed: %w", err)
		}
	}
	return nil
}

// pullTransport routes one pull through CLI or SQL based on the remote's
// protocol and credentials, including the post-pull conflict auto-resolution
// each route carries. Split from pullFromRemote so every successful route
// funnels back through the is_blocked recompute.
func (s *DoltStore) pullTransport(ctx context.Context, remote string) error {
	creds := s.credentialsForRemote(remote)
	if handled, err := s.tryCLIPullRoutes(ctx, remote, creds); handled {
		return err
	}
	return s.pullSQLRemote(ctx, remote, creds)
}

func (s *DoltStore) tryCLIPullRoutes(ctx context.Context, remote string, creds *remoteCredentials) (bool, error) {
	// Git-protocol remotes: use CLI to avoid MySQL connection timeout during transfer.
	// Must check before remoteUser — Hosted Dolt SSH remotes have remoteUser set
	// but still need CLI to avoid SQL connection timeout.
	// Credentials are passed directly to the subprocess via cmd.Env.
	if useCLI, err := s.prepareCLIRouteForGitProtocol(ctx, remote); err != nil {
		return true, err
	} else if useCLI {
		// CLI pull leaves any conflicts in the working set; run the auto-resolver so
		// git-protocol remotes get the same audit-only dependency / metadata repair
		// as the SQL DOLT_PULL path (#4259).
		return true, s.finishCLIPull(ctx, s.doltCLIPull(ctx, remote, creds))
	}
	// Credential CLI routing: mirrors git-protocol path, including post-pull
	// auto-resolution.
	if useCLI, err := s.prepareCLIRouteForCredentials(ctx, remote, creds); err != nil {
		return true, err
	} else if useCLI {
		return true, s.finishCLIPull(ctx, s.doltCLIPull(ctx, remote, creds))
	}
	// Cloud auth CLI routing (GH#6), including post-pull auto-resolution.
	if useCLI, err := s.prepareCLIRouteForCloudAuth(ctx, remote); err != nil {
		return true, err
	} else if useCLI {
		return true, s.finishCLIPull(ctx, s.doltCLIPull(ctx, remote, creds))
	}
	return false, nil
}

func (s *DoltStore) pullSQLRemote(ctx context.Context, remote string, creds *remoteCredentials) error {
	// Local file:// pulls intentionally stay on the SQL path. The matching CLI
	// guard is a push-only optimization; SQL pull keeps pullWithAutoResolve in
	// charge of metadata-only conflict repair.
	if s.remoteUser != "" && remote == s.remote {
		return withRemoteOperationEnv(creds, s.isS3Remote(ctx, remote), func() error {
			return s.executeSQLPull(ctx, remote, s.remoteUser)
		})
	}
	return withRemoteOperationEnv(nil, s.isS3Remote(ctx, remote), func() error {
		return s.executeSQLPull(ctx, remote, "")
	})
}

func (s *DoltStore) executeSQLPull(ctx context.Context, remote, user string) error {
	var err error
	if user != "" {
		err = s.pullWithAutoResolve(ctx, remote, "CALL DOLT_PULL('--user', ?, ?, ?)", user, remote, s.branch)
	} else {
		err = s.pullWithAutoResolve(ctx, remote, "CALL DOLT_PULL(?, ?)", remote, s.branch)
	}
	if err != nil {
		return fmt.Errorf("failed to pull from %s/%s: %w", remote, s.branch, err)
	}
	return nil
}

// pullWithAutoResolve executes a DOLT_PULL query with long timeout and auto-resolves
// metadata-only merge conflicts using "theirs" strategy. This handles the common case
// where machine-local metadata rows (e.g., dolt_auto_push_*) diverge across clones
// and cause recurring merge conflicts on pull (GH#2466).
//
// Dolt may report merge conflicts in two ways:
//  1. DOLT_PULL itself returns an error (under autocommit)
//  2. DOLT_PULL succeeds but tx.Commit() fails (conflicts in working set)
//
// This method handles both by checking for conflicts after the pull call
// (whether it errored or not) and auto-resolving metadata-only conflicts.
// openLongTimeoutConn opens a dedicated single-connection *sql.DB to this store's
// database with a long read timeout, for merge/pull/conflict operations that can run
// longer than the default connection timeout. The caller must Close the returned DB.
func (s *DoltStore) openLongTimeoutConn() (*sql.DB, error) {
	cfg, err := mysql.ParseDSN(s.connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse DSN for long-timeout connection: %w", err)
	}
	cfg.ReadTimeout = 5 * time.Minute
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("failed to open long-timeout connection: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// remote names the remote the query pulls from; the GH#3144 fetch+merge
// fallback targets it directly, so pulls from non-default remotes (PullRemote,
// federation peers) no longer fall back to s.remote.
func (s *DoltStore) pullWithAutoResolve(ctx context.Context, remote string, query string, args ...any) error {
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		return s.pullWithAutoResolveUnchecked(ctx, remote, query, args...)
	})
}

func (s *DoltStore) pullWithAutoResolveUnchecked(ctx context.Context, remote string, query string, args ...any) error {
	db, err := s.openLongTimeoutConn()
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	if err := configurePullTransaction(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}

	pullErr := schema.DrainCall(ctx, tx, query, args...)
	pullErr = applyPullTrackingFallback(ctx, tx, remote, s.branch, pullErr)

	return s.settleMergeInTx(ctx, tx, pullErr)
}

func configurePullTransaction(ctx context.Context, tx *sql.Tx) error {
	// Allow commits with conflicts so we can inspect and resolve them.
	if _, err := tx.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 1"); err != nil {
		return fmt.Errorf("failed to set dolt_allow_commit_conflicts: %w", err)
	}
	// bd-6dnrw.4: a merge that violates a foreign key (e.g. one clone deleted
	// an issue while another inserted a child row referencing it) rolls the
	// whole transaction back before it can be inspected. Let it land in the
	// working set instead so tryRepairFKCascadeViolations can apply the
	// cascade semantics; the violation check before tx.Commit() below refuses
	// to commit anything the repair did not fully clear.
	if _, err := tx.ExecContext(ctx, "SET @@dolt_force_transaction_commit = 1"); err != nil {
		return fmt.Errorf("failed to set dolt_force_transaction_commit: %w", err)
	}
	return nil
}

func applyPullTrackingFallback(ctx context.Context, tx *sql.Tx, remote, branch string, pullErr error) error {
	// GH#3144: When DOLT_PULL fails because upstream branch tracking is not
	// configured in repo_state.json (common when remote was added via
	// bd dolt remote add rather than bd bootstrap/dolt clone), fall back to
	// DOLT_FETCH + DOLT_MERGE which does not require tracking config.
	if pullErr == nil || !isBranchTrackingError(pullErr) {
		return pullErr
	}
	if err := schema.DrainCall(ctx, tx, "CALL DOLT_FETCH(?, ?)", remote, branch); err != nil {
		return fmt.Errorf("fetch from %s/%s: %w", remote, branch, err)
	}
	trackingRef := remote + "/" + branch
	mergeErr := schema.DrainCall(ctx, tx, "CALL DOLT_MERGE(?)", trackingRef)
	if mergeErr != nil && strings.Contains(mergeErr.Error(), "up to date") {
		return nil
	}
	return mergeErr
}

// settleMergeInTx finishes a pull/merge that ran in tx: it auto-resolves the
// safe conflict classes, repairs FK cascade violations (bd-6dnrw.4), and
// commits — or rolls back when anything needs the operator. pullErr is the
// pull/merge statement's own error; it is surfaced whenever nothing was
// resolved or repaired. The tx must have been opened with
// dolt_allow_commit_conflicts and dolt_force_transaction_commit set, which is
// why the violation gate here is mandatory: with the force flag on, committing
// without it would persist a violated working set.
func (s *DoltStore) settleMergeInTx(ctx context.Context, tx *sql.Tx, pullErr error) error {
	resolved, err := settleMergeConflicts(s, ctx, tx, pullErr)
	if err != nil {
		return err
	}
	repaired, err := settleMergeViolations(s, ctx, tx, pullErr)
	if err != nil {
		return err
	}

	if pullErr != nil && !resolved && !repaired {
		// Pull failed for a non-conflict reason, or conflicts include non-metadata tables.
		_ = tx.Rollback()
		return pullErr
	}

	// Conclude the merge for resolved conflicts only now, after the FK repair:
	// DOLT_COMMIT refuses a violated working set, so a merge carrying both
	// classes could never settle when the resolver committed first (bd-578h9.14).
	if resolved {
		if err := versioncontrolops.CommitResolvedConflicts(ctx, tx); err != nil {
			return settleMergeFailure(tx, pullErr, err)
		}
	}

	return s.commitSQLTx(ctx, "commit pull merge settlement", tx)
}

func settleMergeConflicts(s *DoltStore, ctx context.Context, tx *sql.Tx, pullErr error) (bool, error) {
	// Check for merge conflicts regardless of whether DOLT_PULL errored.
	// Some Dolt versions error on conflicts, others leave them in the working set.
	resolved, resolveErr := s.tryAutoResolveMergeConflicts(ctx, tx)
	if resolveErr != nil {
		return false, settleMergeFailure(tx, pullErr, resolveErr)
	}
	if resolved {
		return true, nil
	}

	// bd-578h9.15: conflicts the resolver declined are the operator's. Capture
	// them BEFORE the rollback wipes merge state — a post-rollback GetConflicts
	// on a fresh transaction sees an empty set, which made PullFrom's
	// conflict-reporting contract dead code on the SQL route. The resolver
	// pre-screens every table before resolving any, so a declined resolve
	// leaves dolt_conflicts fully intact here.
	conflicts, err := versioncontrolops.GetConflicts(ctx, tx)
	if err == nil && len(conflicts) > 0 {
		_ = tx.Rollback()
		return false, &versioncontrolops.MergeConflictsError{Conflicts: conflicts, MergeErr: pullErr}
	}
	return false, nil
}

func settleMergeViolations(s *DoltStore, ctx context.Context, tx *sql.Tx, pullErr error) (bool, error) {
	// bd-6dnrw.4: repair FK cascade violations the merge produced (child rows
	// whose parent issue was deleted on the other clone). Unrepaired
	// violations MUST NOT be committed.
	repaired, hadViol, err := s.tryRepairFKCascadeViolations(ctx, tx)
	if err != nil {
		return false, settleMergeFailure(tx, pullErr, err)
	}
	if hadViol && !repaired {
		_ = tx.Rollback()
		if pullErr != nil {
			return false, pullErr
		}
		return false, fmt.Errorf("pull merge left constraint violations bd cannot auto-repair; inspect dolt_constraint_violations and resolve before retrying")
	}
	return repaired, nil
}

func settleMergeFailure(tx *sql.Tx, pullErr, settlementErr error) error {
	_ = tx.Rollback()
	if pullErr != nil {
		return pullErr
	}
	return settlementErr
}
