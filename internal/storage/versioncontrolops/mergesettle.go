package versioncontrolops

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/kvkeys"
)

// memoryConfigKeyPrefix is the config-table key prefix under which `bd remember`
// stores persistent memories. It is sourced from the shared kvkeys package so it
// can never drift from the prefix cmd/bd actually writes (kvkeys.Prefix "kv." +
// kvkeys.MemoryPrefix "memory."), which cmd/bd also reserves against generic
// `bd kv set` keys. Config rows with this prefix are the only config class safe
// to auto-resolve on merge; any other key (issue_prefix above all) is left for
// the operator.
const memoryConfigKeyPrefix = kvkeys.MemoryConfigKeyPrefix

// This file holds the merge-settlement machinery shared by server-mode
// DoltStore (which drives it inside an explicit *sql.Tx) and the embedded
// store's pull path (which drives it on a pinned autocommit connection via
// MergeAndSettle): auto-resolving the conflict classes that are safe without
// operator input (GH#2466 metadata, #4259 audit-only dependency edges,
// bd-6dnrw.29 schema_migrations vintage rows, GH#2474 convergent kv.memory.*
// config rows) and repairing FK cascade
// violations (bd-6dnrw.4). All functions take a DBConn, which *sql.Tx,
// *sql.Conn, and *sql.DB all satisfy.

// MergeAndSettle merges ref into the current branch and settles the result:
// safe conflict classes are auto-resolved, FK cascade violations are
// repaired, and anything needing the operator aborts the merge. It is the
// autocommit-mode equivalent of server-mode pullWithAutoResolve and is used
// by the embedded pull path (bd-6dnrw.40), where Dolt stored procedures
// cannot run inside an explicit SQL transaction.
//
// db must be a single session (a pinned *sql.Conn, or a *sql.DB used
// sequentially whose pool holds one connection): the session flags set here
// must be visible to the DOLT_MERGE and to every settle statement. The flags
// let a conflicted or FK-violating merge land in the working set instead of
// rolling back, so the settle pass can repair it; SettleMerge's gates ensure
// nothing unrepaired survives without an error.
func MergeAndSettle(ctx context.Context, db DBConn, ref string) error {
	return MergeAndSettleWithStrategy(ctx, db, ref, "")
}

// MergeAndSettleWithStrategy is MergeAndSettle with an operator escape hatch
// (#4992 part 2): a conflict TryAutoResolveMergeConflicts declines is, when
// strategy is non-empty, resolved with strategy ("ours" or "theirs") instead
// of aborting the merge for the operator. strategy == "" is exactly
// MergeAndSettle's behavior (a declined conflict aborts with
// MergeConflictsError). Used by the embedded pull path's `--strategy` flag;
// see SettleMerge for the resolution logic.
func MergeAndSettleWithStrategy(ctx context.Context, db DBConn, ref, strategy string) error {
	// Capture pre-merge cleanliness before anything runs: abortMerge's
	// hard-reset fallback is only safe when nothing uncommitted predates
	// the merge (bd-578h9.2).
	preMergeClean := workingSetClean(ctx, db)

	if _, err := db.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 1"); err != nil {
		return fmt.Errorf("set dolt_allow_commit_conflicts: %w", err)
	}
	if _, err := db.ExecContext(ctx, "SET @@dolt_force_transaction_commit = 1"); err != nil {
		return fmt.Errorf("set dolt_force_transaction_commit: %w", err)
	}

	_, mergeErr := db.ExecContext(ctx, "CALL DOLT_MERGE(?)", ref)
	if mergeErr != nil && strings.Contains(mergeErr.Error(), "up to date") {
		// DOLT_PULL swallows "Already up to date." internally; we do the same.
		mergeErr = nil
	}
	return SettleMerge(ctx, db, mergeErr, preMergeClean, strategy)
}

// MergeConflictsError reports the conflicts a settle pass refused to
// auto-resolve. By the time the caller sees it the merge has been aborted (or
// the transaction rolled back) and the working set restored, so the conflicts
// are no longer queryable from dolt_conflicts — they were captured before the
// abort precisely so callers with a conflict-reporting contract (PullFrom) can
// still surface them (bd-578h9.15). Unwrap returns the merge statement's own
// error, when there was one.
type MergeConflictsError struct {
	Conflicts []storage.Conflict
	// MergeErr is the merge/pull statement's own error; nil on Dolt versions
	// that leave conflicts in the working set without erroring.
	MergeErr error
}

func (e *MergeConflictsError) Error() string {
	tables := make([]string, len(e.Conflicts))
	for i, c := range e.Conflicts {
		tables[i] = c.Field
	}
	return fmt.Sprintf("merge conflicts in %s require operator resolution; merge aborted and working set restored",
		strings.Join(tables, ", "))
}

func (e *MergeConflictsError) Unwrap() error { return e.MergeErr }

// SettleMerge finishes a merge that ran on db with the session flags
// MergeAndSettle sets: it auto-resolves the safe conflict classes, repairs FK
// cascade violations (bd-6dnrw.4), and leaves the settled working set in
// place — or aborts the merge when anything needs the operator. mergeErr is
// the merge statement's own error; it is surfaced whenever nothing was
// resolved or repaired. preMergeClean reports whether the working set was
// clean before the merge ran; it gates abortMerge's hard-reset fallback.
// strategy is the #4992 operator escape hatch: "" preserves the exact
// MergeAndSettle behavior below (a declined conflict aborts with
// MergeConflictsError); "ours" or "theirs" resolves whatever the auto-resolver
// declined with that strategy instead of aborting. The decision logic mirrors
// server-mode settleMergeInTx exactly; the abort stands in for that path's
// transaction rollback, restoring the pre-merge working set so a retry is
// possible.
func SettleMerge(ctx context.Context, db DBConn, mergeErr error, preMergeClean bool, strategy string) error {
	resolved, strategyResolved, err := settleMergeResolution(ctx, db, mergeErr, preMergeClean, strategy)
	if err != nil {
		return err
	}

	// bd-6dnrw.4: repair FK cascade violations the merge produced (child rows
	// whose parent issue was deleted on the other clone). Unrepaired
	// violations MUST NOT survive: with the force flag on, every statement
	// autocommits, so the abort below is what keeps them out of the database.
	// This also covers violations a strategy resolution left behind (e.g.
	// --ours keeps a child row whose parent was deleted on the other side).
	repairedViol, hadViol, violErr := TryRepairFKCascadeViolations(ctx, db)
	if violErr != nil {
		return settleMergeViolationError(ctx, db, mergeErr, preMergeClean, violErr)
	}
	if hadViol && !repairedViol {
		return settleMergeUnrepairedError(ctx, db, mergeErr, preMergeClean)
	}

	if err := settleMergeStatementError(ctx, db, mergeErr, preMergeClean, resolved, strategyResolved, repairedViol); err != nil {
		return err
	}

	return commitSettledMerge(ctx, db, mergeErr, preMergeClean, resolved, strategyResolved, strategy)
}

// settleMergeResolution auto-resolves safe conflicts and applies the optional
// operator strategy to conflicts the allowlist declines. The two booleans tell
// the caller which kind of resolution, if any, happened so it can repair
// constraints before committing.
func settleMergeResolution(ctx context.Context, db DBConn, mergeErr error, preMergeClean bool, strategy string) (resolved, strategyResolved bool, err error) {
	// Check for merge conflicts regardless of whether the merge errored.
	// Some Dolt versions error on conflicts, others leave them in the working set.
	resolved, err = TryAutoResolveMergeConflicts(ctx, db)
	if err != nil {
		abortMerge(ctx, db, preMergeClean)
		if mergeErr != nil {
			return false, false, mergeErr
		}
		return false, false, err
	}
	if resolved {
		return true, false, nil
	}

	// bd-578h9.15: capture declined conflicts before abort wipes merge state.
	// GetConflicts errors are intentionally ignored here, matching the original
	// settle path; the merge error below remains the authoritative failure.
	conflicts, conflictErr := GetConflicts(ctx, db)
	if conflictErr != nil || len(conflicts) == 0 {
		return false, false, nil
	}
	if strategy == "" {
		abortMerge(ctx, db, preMergeClean)
		return false, false, &MergeConflictsError{Conflicts: conflicts, MergeErr: mergeErr}
	}

	// #4999 part 2: the operator asked for an escape hatch. Unlike the
	// auto-resolver, no allowlist applies — every conflicted table is resolved.
	for _, conflict := range conflicts {
		if err := resolveSettledConflict(ctx, db, conflict, strategy); err != nil {
			abortMerge(ctx, db, preMergeClean)
			return false, false, err
		}
	}
	return false, true, nil
}

func resolveSettledConflict(ctx context.Context, db DBConn, conflict storage.Conflict, strategy string) error {
	table := conflict.Field
	if table == "" {
		table = "issues"
	}
	if err := ResolveConflicts(ctx, db, table, strategy); err != nil {
		return fmt.Errorf("resolve %s conflicts with '%s' strategy: %w", table, strategy, err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_ADD(?)", table); err != nil {
		return fmt.Errorf("stage resolved %s: %w", table, err)
	}
	return nil
}

func settleMergeViolationError(ctx context.Context, db DBConn, mergeErr error, preMergeClean bool, violationErr error) error {
	abortMerge(ctx, db, preMergeClean)
	if mergeErr != nil {
		return mergeErr
	}
	return violationErr
}

func settleMergeUnrepairedError(ctx context.Context, db DBConn, mergeErr error, preMergeClean bool) error {
	abortMerge(ctx, db, preMergeClean)
	if mergeErr != nil {
		return mergeErr
	}
	return fmt.Errorf("pull merge left constraint violations bd cannot auto-repair; inspect dolt_constraint_violations and resolve before retrying")
}

func settleMergeStatementError(ctx context.Context, db DBConn, mergeErr error, preMergeClean bool, resolved, strategyResolved, repaired bool) error {
	if mergeErr == nil || resolved || strategyResolved || repaired {
		return nil
	}
	// Merge failed for a non-conflict reason, or conflicts include a table the
	// auto-resolver declined.
	abortMerge(ctx, db, preMergeClean)
	return mergeErr
}

func commitSettledMerge(ctx context.Context, db DBConn, mergeErr error, preMergeClean bool, resolved, strategyResolved bool, strategy string) error {
	var commitErr error
	switch {
	case resolved:
		commitErr = CommitResolvedConflicts(ctx, db)
	case strategyResolved:
		msg := fmt.Sprintf("Resolve merge conflicts using '%s' strategy", strategy)
		if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-m', ?)", msg); err != nil {
			commitErr = fmt.Errorf("conflicts resolved but commit failed: %w", err)
		}
	}
	if commitErr == nil {
		return nil
	}
	abortMerge(ctx, db, preMergeClean)
	if mergeErr != nil {
		return mergeErr
	}
	return commitErr
}

// MergeWithStrategy merges ref into the current branch and, when the merge
// produces conflicts, resolves EVERY conflicted table with the operator's
// explicit strategy ("ours" or "theirs") instead of aborting for later
// resolution. It backs `bd vc merge --strategy` (#4992): the flag existed
// and was documented, but the merge ran as a bare `CALL DOLT_MERGE` inside an
// implicit autocommit transaction, so Dolt rejected any real conflict with
// Error 1105 ("@autocommit must be disabled ...") before the strategy could
// ever be applied — the strategy path was dead code.
//
// Unlike TryAutoResolveMergeConflicts (which only resolves conflict classes
// proven safe without operator input, e.g. GH#2466 metadata), no allowlist
// applies here: the operator named the strategy, so every conflicted table is
// resolved with it. FK cascade violations a resolution can leave behind
// (bd-6dnrw.4) are still repaired — or refused — exactly like SettleMerge, so
// a strategy-resolved merge can never silently commit a violated working set.
//
// db must be a single session (a pinned *sql.Conn, or a *sql.DB used
// sequentially whose pool holds one connection) — see MergeAndSettle for why:
// the conflict-tolerance flags set here are session state and must be visible
// to the DOLT_MERGE and to every resolve/repair/commit statement that
// follows. author formats "Name <email>"; strategy must be "ours" or
// "theirs" (validated via ValidateConflictStrategy).
//
// Returns the conflicts the merge produced (empty for a clean merge) and any
// error. A returned error means the merge was aborted and the working set
// restored — nothing is left half-resolved.
func MergeWithStrategy(ctx context.Context, db DBConn, ref, author, strategy string) ([]storage.Conflict, error) {
	if err := ValidateConflictStrategy(strategy); err != nil {
		return nil, err
	}

	// Capture pre-merge cleanliness before anything runs: abortMerge's
	// hard-reset fallback is only safe when nothing uncommitted predates the
	// merge (bd-578h9.2), same as MergeAndSettle.
	preMergeClean := workingSetClean(ctx, db)

	mergeErr, err := runStrategyMerge(ctx, db, ref, author)
	if err != nil {
		return nil, err
	}

	// Check for conflicts regardless of whether the merge statement itself
	// errored: with the flags above set, a genuinely conflicted merge lands in
	// the working set instead of erroring, but older Dolt behavior (or a
	// conflict class the flags don't cover) may still report both.
	conflicts, err := strategyMergeConflicts(ctx, db, ref, mergeErr)
	if err != nil {
		abortMerge(ctx, db, preMergeClean)
		return nil, err
	}

	if len(conflicts) == 0 {
		return strategyMergeWithoutConflicts(ctx, db, ref, mergeErr, preMergeClean)
	}

	dirtyTables, err := resolveStrategyConflicts(ctx, db, conflicts, strategy)
	if err != nil {
		abortMerge(ctx, db, preMergeClean)
		return conflicts, err
	}

	// bd-6dnrw.4 / #4992: a strategy resolution can leave FK cascade
	// violations behind exactly like the auto-resolve path (e.g. --ours keeps
	// a child row whose parent was deleted on the other side); repair them the
	// same way so a strategy-resolved merge cannot silently commit a violated
	// working set.
	if err := settleStrategyMergeViolations(ctx, db, strategy, preMergeClean); err != nil {
		return conflicts, err
	}

	if err := commitStrategyMerge(ctx, db, dirtyTables, ref, strategy, author, preMergeClean); err != nil {
		return conflicts, err
	}

	return conflicts, nil
}

func runStrategyMerge(ctx context.Context, db DBConn, ref, author string) (error, error) {
	if _, err := db.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 1"); err != nil {
		return nil, fmt.Errorf("set dolt_allow_commit_conflicts: %w", err)
	}
	if _, err := db.ExecContext(ctx, "SET @@dolt_force_transaction_commit = 1"); err != nil {
		return nil, fmt.Errorf("set dolt_force_transaction_commit: %w", err)
	}

	_, mergeErr := db.ExecContext(ctx, "CALL DOLT_MERGE('--author', ?, ?)", author, ref)
	if mergeErr != nil && strings.Contains(mergeErr.Error(), "up to date") {
		mergeErr = nil
	}
	return mergeErr, nil
}

func strategyMergeConflicts(ctx context.Context, db DBConn, ref string, mergeErr error) ([]storage.Conflict, error) {
	conflicts, err := GetConflicts(ctx, db)
	if err == nil {
		return conflicts, nil
	}
	if mergeErr != nil {
		return nil, fmt.Errorf("merge branch %s: %w", ref, mergeErr)
	}
	return nil, fmt.Errorf("check merge conflicts for branch %s: %w", ref, err)
}

func strategyMergeWithoutConflicts(ctx context.Context, db DBConn, ref string, mergeErr error, preMergeClean bool) ([]storage.Conflict, error) {
	if mergeErr == nil {
		return nil, nil
	}
	// Not a conflict: some other merge failure (unknown branch, dirty working
	// set, ...). Nothing to resolve — surface it as-is.
	abortMerge(ctx, db, preMergeClean)
	return nil, fmt.Errorf("merge branch %s: %w", ref, mergeErr)
}

func resolveStrategyConflicts(ctx context.Context, db DBConn, conflicts []storage.Conflict, strategy string) (map[string]bool, error) {
	dirtyTables := make(map[string]bool, len(conflicts))
	for _, conflict := range conflicts {
		table := conflict.Field
		if table == "" {
			table = "issues"
		}
		if err := ResolveConflicts(ctx, db, table, strategy); err != nil {
			return nil, fmt.Errorf("resolve %s conflicts: %w", table, err)
		}
		dirtyTables[table] = true
	}
	return dirtyTables, nil
}

func settleStrategyMergeViolations(ctx context.Context, db DBConn, strategy string, preMergeClean bool) error {
	repaired, had, err := TryRepairFKCascadeViolations(ctx, db)
	if err != nil {
		abortMerge(ctx, db, preMergeClean)
		return err
	}
	if had && !repaired {
		abortMerge(ctx, db, preMergeClean)
		return fmt.Errorf("conflicts resolved with '%s' strategy but merge left constraint violations bd cannot auto-repair; inspect dolt_constraint_violations and resolve before retrying", strategy)
	}
	return nil
}

func commitStrategyMerge(ctx context.Context, db DBConn, dirtyTables map[string]bool, ref, strategy, author string, preMergeClean bool) error {
	if err := StageAndCommit(ctx, db, dirtyTables,
		fmt.Sprintf("Resolve merge conflicts from %s using %s strategy", ref, strategy), author); err != nil {
		abortMerge(ctx, db, preMergeClean)
		return fmt.Errorf("conflicts resolved but commit failed: %w", err)
	}
	return nil
}

// abortMerge restores the pre-merge state after a settle pass refused the
// merge — the autocommit-mode stand-in for server mode's tx.Rollback().
// DOLT_MERGE('--abort') is the precise tool but only works while merge state
// is active; a force-committed violation-only merge may have closed it, so
// fall back to a hard reset — but only when the working set was clean before
// the merge ran. The most common reason --abort fails is a merge that
// REFUSED TO START on a dirty working set; hard-resetting there would
// destroy uncommitted data the merge never touched (bd-578h9.2).
// Best-effort: the caller's error is what matters.
func abortMerge(ctx context.Context, db DBConn, preMergeClean bool) {
	if _, err := db.ExecContext(ctx, "CALL DOLT_MERGE('--abort')"); err != nil && preMergeClean {
		_, _ = db.ExecContext(ctx, "CALL DOLT_RESET('--hard')")
	}
}

// workingSetClean reports whether dolt_status is empty. Errors count as
// dirty so the hard-reset fallback stays conservative.
func workingSetClean(ctx context.Context, db DBConn) bool {
	rows, err := db.QueryContext(ctx, "SELECT 1 FROM dolt_status LIMIT 1")
	if err != nil {
		return false
	}
	defer rows.Close()
	return !rows.Next() && rows.Err() == nil
}
