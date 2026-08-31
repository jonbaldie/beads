package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

// syncOutcome is one run of the sync loop.
type syncConflictState struct {
	// ConflictsPreexisting distinguishes conflicts that were already live in
	// the working set when sync started (an earlier halted sync or a hand-run
	// merge) from ones this run's pull surfaced.
	ConflictsPreexisting bool `json:"conflicts_preexisting,omitempty"`
	// ConflictsLive reports whether the conflicts are sitting in the working
	// set right now rather than having been aborted away. It is read from
	// which detection source fired, never assumed: the SQL pull route aborts a
	// conflicted merge and restores the working set, while the CLI/git-protocol
	// route deliberately leaves the conflict rows live for the operator.
	ConflictsLive bool `json:"conflicts_live,omitempty"`
}

type syncOutcome struct {
	Status    string   `json:"status"`
	Attempts  int      `json:"attempts"`
	Conflicts []string `json:"conflicts,omitempty"`
	syncConflictState
	RowsCorrected int `json:"rows_corrected"`
	// Pulled reports whether an earlier attempt in this run already completed a
	// pull and is_blocked repair, which makes "this run touched nothing" untrue
	// on a later attempt's conflict.
	Pulled        bool   `json:"pulled,omitempty"`
	Pushed        bool   `json:"pushed"`
	PushSkipped   bool   `json:"push_skipped,omitempty"`
	LastPushError string `json:"last_push_error,omitempty"`
	// LastRecomputeError records a retryable is_blocked-repair failure (the
	// working set was dirty). At most one of LastPushError and
	// LastRecomputeError is set at a time: each retry clears the other, so on
	// an exhausted run the one that survives names what the FINAL attempt
	// actually failed on.
	LastRecomputeError string `json:"last_recompute_error,omitempty"`
	// DiscardedPullError records a genuine pull failure (transport, auth) that
	// this run reports as a conflict instead, because live conflict rows from
	// a DIFFERENT cause (e.g. another writer on a shared sql-server) were also
	// found at the same instant. The conflict report is correct — the database
	// really is conflicted — but attributing the halt to that conflict alone
	// would hide a real transport error the operator also needs to see.
	DiscardedPullError string `json:"discarded_pull_error,omitempty"`
	// Transients is every transient failure this run hit, in attempt order.
	// LastPushError/LastRecomputeError answer "what did the FINAL attempt fail
	// on" — deliberately, since that is what the operator's next step depends
	// on — and because each retry clears the other they cannot answer "what did
	// this run actually fight". A run that lost a push race and then hit a
	// dirty working set reports only the second in those two fields; both are
	// here (wy-wub2s, from the wy-mlnz2 review's F7/F8).
	Transients []syncTransient `json:"transients,omitempty"`
	// DirtyGraphFingerprint identifies the pending graph edits that blocked the
	// is_blocked repair, when every blocked attempt in this run saw the SAME
	// ones. Empty means either no attempt was blocked, or the working set was
	// visibly moving between attempts, or the evidence was unavailable — in all
	// of which cases there is nothing to compare across ticks. It is an opaque
	// token: compare for equality, never parse.
	DirtyGraphFingerprint string `json:"dirty_graph_fingerprint,omitempty"`
	// DirtyGraphStuckTicks counts consecutive sync runs, including this one,
	// that exhausted their budget against this same fingerprint. Set by the
	// caller from the persisted marker, not by the loop.
	DirtyGraphStuckTicks int `json:"dirty_graph_stuck_ticks,omitempty"`
	// ConstraintViolations names the graph-table (issues, dependencies)
	// constraint violations that escalated this run straight to dirty-stuck
	// (exit 4) on the attempt that found them, instead of waiting out
	// syncStuckTicks. Empty means this run's dirty-stuck status, if any, came
	// from the tick-count inference instead (wy-mhouc).
	ConstraintViolations []storage.ConstraintViolation `json:"constraint_violations,omitempty"`
}

// syncTransient is one attempt's transient failure.
type syncTransient struct {
	Attempt int    `json:"attempt"`
	Kind    string `json:"kind"`
	Error   string `json:"error,omitempty"`
}

// syncOps is the store surface the loop drives, injected as functions so the
// loop's control flow (the part with the interesting failure modes) is testable
// without a live Dolt remote.
type syncOps struct {
	// pull merges the remote into the local branch. It returns the conflicted
	// table names the merge captured, if any — see runSyncLoop's property (1).
	pull func(context.Context) ([]string, error)
	// conflicts positively reports live merge conflicts (dolt_conflicts).
	conflicts func(context.Context) ([]string, error)
	// recompute runs the full is_blocked recompute and returns rows corrected.
	recompute func(context.Context) (int, error)
	// push publishes local commits to the remote.
	push func(context.Context) error
	// dirtyFingerprint identifies the pending graph edits currently blocking
	// the is_blocked repair (issueops.DirtyGraphFingerprint semantics: "" means
	// clean, an error means the evidence is unavailable). May be nil, which the
	// loop treats exactly like unavailable evidence — it never escalates on a
	// question it could not ask.
	dirtyFingerprint func(context.Context) (string, error)
	// mergeBlockers reports schema conflicts, constraint violations, and
	// merge state for the current working set (storage.MergeBlockerInspector).
	// Used to tell a graph table stuck on constraint violations no writer
	// will ever commit from a merely busy one, on the FIRST blocked attempt
	// rather than waiting out syncStuckTicks. May be nil, which the loop
	// treats exactly like a probe failure — it never escalates on a question
	// it could not ask.
	mergeBlockers func(context.Context) (storage.MergeBlockers, error)
	// progress reports a step to the operator; may be nil.
	progress func(format string, args ...interface{})
}

func (o syncOps) report(format string, args ...interface{}) {
	if o.progress != nil {
		o.progress(format, args...)
	}
}

// runSyncLoop is the whole federation loop: pull -> positive conflict check ->
// recompute-blocked -> push, retrying a bounded number of times when another
// replica wins the push race.
//
// Two properties are load-bearing and are why this is not just three shell
// lines:
//
//  1. Conflicts are detected POSITIVELY — from structured conflict data, never
//     inferred from the pull's exit status. A pull fails for plenty of reasons
//     that are not conflicts, and it can also leave conflicts behind without
//     failing, so an exit-status guess both invents phantom conflicts and
//     misses real ones. Two structured sources are consulted per attempt, and
//     either one firing means conflict:
//
//     a. the conflicts the merge itself captured — the settle pass aborts a
//     merge it cannot auto-resolve and hands back a MergeConflictsError
//     holding the conflict rows it read BEFORE the abort;
//     b. live rows in dolt_conflicts, which is what a conflict left behind by
//     some earlier operation (a hand-run merge, a halted sync) looks like.
//
//     Source (a) is why the check cannot just be a dolt_conflicts query: by
//     the time a conflicted pull returns, its merge has been aborted and the
//     working set restored, so dolt_conflicts is empty again.
//
//  2. Conflicts are NEVER auto-resolved here. The loop halts before recomputing
//     or pushing and reports the conflict as a distinct exit code, leaving the
//     divergence for an operator. A sync timer that silently picks a side loses
//     work on a schedule.
//
// The recompute between pull and push is not optional bookkeeping: is_blocked
// is denormalized, and a merge that brings in a dependency edge from another
// replica leaves it stale, so `bd ready` silently hides or surfaces the wrong
// work until someone repairs it.
//
// It runs UNCONDITIONALLY, on every attempt. That is deliberate, and it is the
// second thing about this loop that looks like it wants an optimization and
// must not get one. RecomputeAllBlocked is specifically the repair that does
// NOT depend on a merge advancing HEAD (bd-6dnrw.37): it is what recovers an
// is_blocked column left stale by a post-merge recompute that failed after its
// merge committed, or by a conflicted pull an operator resolved BY HAND. Gating
// it on "did this pull merge anything" re-imposes the exact condition it exists
// to escape, and sync manufactures that state itself — it exits 2 on a
// conflict, the operator resolves by hand, and from then on no tick merges
// anything, so a gated repair would never run again while every tick reports
// success. HEAD is also the wrong instrument for the question even when the
// question is right: the pull's own pre-merge auto-commit (GH#2474) moves HEAD
// for purely local dirty state, and an auto-resolved or cascade-repaired merge
// can land in the working set without moving it at all (bd-6dnrw.39). Anyone
// revisiting the cost of this pass must start from storage.StateHasher and the
// pending-recompute marker, not from HEAD.
//
// Returns (outcome, nil) for every outcome the caller maps to an exit code, and
// (outcome, err) only for genuine failures (exit 1).
func runSyncLoop(ctx context.Context, ops syncOps, maxAttempts int) (*syncOutcome, error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	out := &syncOutcome{Status: syncStatusOK}

	// Pre-flight. A previous halted sync leaves its conflicts live, and Dolt
	// refuses to merge over them — without this check that shows up as an
	// opaque pull failure (exit 1) instead of the conflict it actually is.
	preConflicts, err := ops.conflicts(ctx)
	if err != nil {
		return out, fmt.Errorf("conflict check: %w", err)
	}
	if len(preConflicts) > 0 {
		recordPreexistingSyncConflict(out, preConflicts)
		return out, nil
	}

	var evidence dirtyEvidence
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// A push race re-enters the loop; honor cancellation between attempts
		// so ^C or a timer's deadline is not swallowed by the retry budget.
		if err := ctx.Err(); err != nil {
			return out, err
		}
		out.Attempts = attempt
		done, err := runSyncAttempt(ctx, ops, attempt, maxAttempts, out, &evidence)
		if err != nil || done {
			return out, err
		}
	}

	out.Status = syncStatusRetriesExhausted
	out.DirtyGraphFingerprint = evidence.fold()
	return out, nil
}

func recordPreexistingSyncConflict(out *syncOutcome, conflicts []string) {
	out.Status = syncStatusConflict
	out.Conflicts = conflicts
	out.syncConflictState.ConflictsPreexisting = true
	// These came from the live conflict rows by definition, so a consumer
	// asking "is this database conflicted right now" gets a straight yes.
	out.syncConflictState.ConflictsLive = true
}

// runSyncAttempt executes one pull/recompute/push pass. It returns done when
// the run has reached a terminal outcome; false means the caller should retry.
func runSyncAttempt(ctx context.Context, ops syncOps, attempt, maxAttempts int, out *syncOutcome, evidence *dirtyEvidence) (bool, error) {
	ops.report("pull (attempt %d/%d)", attempt, maxAttempts)
	merged, pullErr := ops.pull(ctx)

	// Positive conflict check, run whether or not the pull reported an error,
	// and unioned with what the merge itself captured — see property (1) above.
	live, conflictErr := ops.conflicts(ctx)
	if conflicts := unionTables(merged, live); len(conflicts) > 0 {
		recordSyncConflict(out, conflicts, live, merged, pullErr)
		return true, nil
	}
	if pullErr != nil {
		return false, fmt.Errorf("pull: %w", pullErr)
	}
	if conflictErr != nil {
		return false, fmt.Errorf("conflict check: %w", conflictErr)
	}

	return runSyncAttemptAfterPull(ctx, ops, attempt, out, evidence)
}

func recordSyncConflict(out *syncOutcome, conflicts, live, merged []string, pullErr error) {
	out.Status = syncStatusConflict
	out.Conflicts = conflicts
	// Which source fired decides what the operator is told about the state of
	// the database. Live rows mean the conflict is sitting in the working set;
	// captured-only means the settle pass aborted the merge and restored it.
	out.syncConflictState.ConflictsLive = len(live) > 0
	// A pull error is part of the conflict only when the merge captured it. If
	// it raced an unrelated live conflict, retain it separately for reporting.
	if pullErr != nil && len(merged) == 0 {
		out.DiscardedPullError = pullErr.Error()
	}
}

func runSyncAttemptAfterPull(ctx context.Context, ops syncOps, attempt int, out *syncOutcome, evidence *dirtyEvidence) (bool, error) {
	ops.report("recompute-blocked")
	corrected, err := ops.recompute(ctx)
	if err != nil {
		return handleSyncRecomputeFailure(ctx, ops, attempt, out, evidence, err)
	}

	out.LastRecomputeError = ""
	// The repair ran, so whatever was dirty cleared: this run has seen the
	// working set advance, and any earlier blocked attempt was transient.
	*evidence = dirtyEvidence{}
	out.RowsCorrected += corrected
	out.Pulled = true

	return finishSyncPush(ctx, ops, attempt, out)
}

func handleSyncRecomputeFailure(ctx context.Context, ops syncOps, attempt int, out *syncOutcome, evidence *dirtyEvidence, err error) (bool, error) {
	if !isRecomputeDirtyGraphErr(err) {
		return false, fmt.Errorf("recompute-blocked: %w", err)
	}

	// A concurrent graph edit is transient: retry the pull/recompute cycle,
	// but retain evidence so repeated identical edits can be classified as
	// stuck after the caller's cross-tick threshold.
	out.LastRecomputeError = err.Error()
	out.LastPushError = ""
	out.Transients = append(out.Transients, syncTransient{
		Attempt: attempt, Kind: syncTransientDirtyGraph, Error: err.Error(),
	})
	evidence.observe(sampleDirtyGraph(ctx, ops))

	// Constraint violations are positive evidence that no writer can publish
	// the dirty graph, so escalate immediately rather than waiting for ticks.
	if violations := graphConstraintViolations(ctx, ops); len(violations) > 0 {
		out.Status = syncStatusDirtyStuck
		out.ConstraintViolations = violations
		ops.report("recompute-blocked: constraint violations on the dirty graph table(s) — escalating")
		return true, nil
	}
	ops.report("recompute-blocked: working set dirty (concurrent writer) — re-pulling and retrying")
	return false, nil
}

func finishSyncPush(ctx context.Context, ops syncOps, attempt int, out *syncOutcome) (bool, error) {
	ops.report("push")
	pushErr := ops.push(ctx)
	if pushErr == nil {
		out.Status = syncStatusOK
		out.Pushed = true
		out.LastPushError = ""
		return true, nil
	}
	if !isPushRaceErr(pushErr) {
		return false, fmt.Errorf("push: %w", pushErr)
	}

	// Another replica pushed between our merge and our push, so re-pull and
	// pick up its commits. Anything else cannot converge by retrying.
	out.LastPushError = pushErr.Error()
	out.Transients = append(out.Transients, syncTransient{
		Attempt: attempt, Kind: syncTransientPushRace, Error: pushErr.Error(),
	})
	ops.report("push race (non-fast-forward) — re-pulling and retrying")
	return false, nil
}

// dirtyEvidence accumulates one fingerprint per blocked attempt.
type dirtyEvidence struct {
	samples []string
	// unavailable records that at least one sample could not be taken, which
	// disqualifies the whole run: a fold over the attempts we happened to see
	// would claim "nothing changed" about attempts we never looked at.
	unavailable bool
}

func (e *dirtyEvidence) observe(fingerprint string, err error) {
	if err != nil || fingerprint == "" {
		// An error means the evidence is unavailable. So does "" here, which
		// says the graph tables were CLEAN by the time we looked — the guard
		// fired and then the other writer committed, i.e. exactly the transient
		// case, and a value that is not a fingerprint must never be compared as
		// one.
		e.unavailable = true
		return
	}
	e.samples = append(e.samples, fingerprint)
}

// fold returns the fingerprint common to every blocked attempt, or "" when the
// run proves nothing: no samples, an unavailable one, or a working set that
// visibly moved between attempts (a busy fleet, which must never escalate).
func (e *dirtyEvidence) fold() string {
	if e.unavailable || len(e.samples) == 0 {
		return ""
	}
	for _, s := range e.samples[1:] {
		if s != e.samples[0] {
			return ""
		}
	}
	return e.samples[0]
}

// sampleDirtyGraph reads the current dirty-graph fingerprint, treating an
// absent hook as unavailable evidence.
func sampleDirtyGraph(ctx context.Context, ops syncOps) (string, error) {
	if ops.dirtyFingerprint == nil {
		return "", errors.New("dirty-graph evidence not available")
	}
	return ops.dirtyFingerprint(ctx)
}

// pushRacePattern matches the ways a push fails because the remote moved. Kept
// as a message match because the rejection arrives as an untyped error, and
// deliberately narrow: it matches *race signatures* rather than the word
// "rejected". All three routes a real race travels are covered:
//
//   - the SQL procedure says the branch "is behind its remote counterpart";
//   - the CLI route folds git's `! [rejected] ... (non-fast-forward)` in;
//   - the git-blobstore layer behind `git+*` remotes pushes with
//     --force-with-lease, and a lost lease reads as `(stale info)`,
//     `(fetch first)`, or "the remote contains work that you do not have".
//
// A bare "rejected" is NOT enough. A protected branch or a declining
// pre-receive hook also rejects, permanently; treating that as a race means a
// sync timer burns its whole attempt budget every tick and reports exit 3
// ("transient, retry next tick") forever, never surfacing the failure as the
// error it is.
var pushRacePattern = regexp.MustCompile(
	`(?i)behind|fast.?forward|not\s+(an\s+)?ancestor|stale\s+info|fetch\s+first|contains\s+work\s+that\s+you\s+do\s+not\s+have`)

// isPushRaceErr reports whether a push failed because the remote moved under us
// — the one push failure that retrying can fix.
//
// Hard divergence (no common ancestor, or an ancestor primary-key mismatch) is
// explicitly NOT a race: those messages can contain "fast-forward"-adjacent
// wording, and retrying them burns the whole attempt budget on an operation
// that can never converge. They fall through to the generic error path, which
// prints the existing recovery guidance.
func isPushRaceErr(err error) bool {
	if err == nil {
		return false
	}
	if isDivergedHistoryErr(err) || isAncestorPKMismatchErr(err) {
		return false
	}
	return pushRacePattern.MatchString(err.Error())
}

// isRecomputeDirtyGraphErr reports whether the is_blocked repair refused to run
// because the graph tables (issues, dependencies) had uncommitted working-set
// changes — the one recompute failure that retrying can fix.
//
// This is classified from the typed sentinel, never from the message. The guard
// is a foreign package's error text; matching on it would let a reworded guard
// silently demote this back to a hard error, which is exactly the failure being
// fixed (wy-mlnz2).
//
// Why it is retryable at all: on a shared sql-server topology every agent
// shares one working set, so an uncommitted write from ANOTHER agent — no part
// of this sync, and gone as soon as they commit — is what trips the guard. The
// condition is transient, foreign, and self-healing, so the loop's existing
// retry budget is the right response. It is still never *ignored*: the repair
// is not optional (see runSyncLoop), so an exhausted budget halts before the
// push rather than publishing a stale is_blocked.
func isRecomputeDirtyGraphErr(err error) bool {
	return err != nil && errors.Is(err, issueops.ErrBlockedRecomputeDirtyGraph)
}

// graphConstraintViolations reports the constraint violations, if any,
// outstanding on the graph tables (issues, dependencies) — the same table set
// isRecomputeDirtyGraphErr just found dirty. It is what lets the loop tell a
// table that is dirty because of a constraint violation no writer will ever
// commit from one that is merely being written to right now: the former is
// knowable positively, on the spot, from storage.MergeBlockerInspector,
// instead of waited out over several ticks (wy-mhouc).
//
// A nil hook or a failed probe reports no violations. That is deliberate:
// this only ever narrows an already-transient classification to a stronger
// one, so unavailable evidence must fall back to "keep retrying", never
// invent a violation it could not confirm.
func graphConstraintViolations(ctx context.Context, ops syncOps) []storage.ConstraintViolation {
	if ops.mergeBlockers == nil {
		return nil
	}
	blockers, err := ops.mergeBlockers(ctx)
	if err != nil {
		return nil
	}
	var out []storage.ConstraintViolation
	for _, v := range blockers.ConstraintViolations {
		if issueops.IsBlockedRecomputeGraphTable(v.Table) {
			out = append(out, v)
		}
	}
	return out
}

// bareNoRemotePattern matches Dolt's bare "no remote" wording. `bd dolt push`
// and `bd dolt pull` classify only the "remote ... not found" phrasing, but a
// default-remote fetch on a rig that never configured one fails with
// `Error 1105: no remote` instead, which that phrasing misses.
var bareNoRemotePattern = regexp.MustCompile(`(?i)\bno remote\b`)

// isNoRemoteConfiguredErr reports whether a sync failure *sounds* like the
// benign "no remote configured" case. It is a strict superset of
// isRemoteNotFoundErr because sync is a timer verb: a solo rig that ran
// `bd init` and never added a remote would otherwise fail on every tick with a
// raw Dolt error code, where `bd dolt push` exits 0. The widening is safe only
// because it is a hint, never the decision — hasNoRemoteConfigured must
// independently prove the remotes are empty before anything exits 0, so a
// deleted remote-side repo or a typoed remote name (both of which have a
// remote configured) still fails loudly.
func isNoRemoteConfiguredErr(err error) bool {
	if err == nil {
		return false
	}
	return isRemoteNotFoundErr(err) || bareNoRemotePattern.MatchString(err.Error())
}

// classifyPullError splits a pull failure into the conflicts the merge itself
// captured and the residual error.
//
// The settle pass aborts a merge it will not auto-resolve and restores the
// working set, so dolt_conflicts is empty again by the time anyone could query
// it; the conflict rows it read pre-abort ride back inside the error instead
// (bd-578h9.15). Reading them here is what makes the sync loop's conflict
// detection positive rather than an exit-status guess, and it is the same
// contract PullFrom implements.
func classifyPullError(err error) ([]string, error) {
	var mce *versioncontrolops.MergeConflictsError
	if errors.As(err, &mce) {
		// A conflict error carrying no conflict rows would otherwise vanish
		// here and let the loop recompute and push on top of a failed merge.
		// Both construction sites guard against it today; this keeps the
		// invariant enforced at the consuming end too.
		if tables := conflictTables(mce.Conflicts); len(tables) > 0 {
			return tables, nil
		}
		return nil, err
	}
	return nil, err
}

// unionTables merges two conflicted-table lists, de-duplicated and sorted.
func unionTables(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(a)+len(b))
	var out []string
	for _, list := range [][]string{a, b} {
		for _, name := range list {
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// conflictTables reports the distinct table names with live conflicts, sorted
// for stable output.
func conflictTables(conflicts []storage.Conflict) []string {
	seen := make(map[string]bool, len(conflicts))
	var tables []string
	for _, c := range conflicts {
		name := c.Field
		if name == "" {
			name = "(unknown)"
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		tables = append(tables, name)
	}
	sort.Strings(tables)
	return tables
}
