package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jonbaldie/beads/internal/atomicfile"
	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/debug"
)

// sawTransient reports whether any attempt failed with kind.
func (o *syncOutcome) sawTransient(kind string) bool {
	for _, t := range o.Transients {
		if t.Kind == kind {
			return true
		}
	}
	return false
}

func printSyncErrorGuidance(remote string, err error) {
	switch {
	case isAncestorPKMismatchErr(err):
		printAncestorPKMismatchGuidance(err)
	case isDivergedHistoryErr(err):
		printDivergedHistoryGuidance("sync")
	case remote != "" && isRemoteNotFoundErr(err):
		fmt.Fprintf(os.Stderr, "\nRemote %q is not configured.\n", remote)
		fmt.Fprintln(os.Stderr, "Use 'bd dolt remote add <name> <url>' to add it.")
		fmt.Fprintln(os.Stderr, "Use 'bd dolt remote list' to see configured remotes.")
	}
}

// syncConflictMessage renders the operator-facing halt report for a conflicted
// sync. It is a pure function of the outcome because the three cases it
// distinguishes are easy to describe wrongly, and a wrong description here is
// worse than none: an operator who is told "nothing was merged" will not go
// looking for the merge commit that is in fact sitting in the local history,
// and one who is told the working set was restored will not go looking for the
// live conflict rows that are in fact blocking every subsequent merge.
func syncConflictMessage(out *syncOutcome) []string {
	lines := []string{"Error: merge conflict — sync halted, nothing pushed."}
	for _, table := range out.Conflicts {
		lines = append(lines, fmt.Sprintf("  conflicted table: %s", table))
	}
	lines = append(lines, "Conflicts sync cannot resolve safely are never auto-resolved.")

	// First: is the database conflicted RIGHT NOW, or was the conflicted merge
	// aborted away? This is the part an operator acts on, and both answers are
	// real — which one applies depends on the pull route, not on luck.
	switch {
	case out.ConflictsPreexisting:
		lines = append(lines,
			"This replica was ALREADY in a conflicted state before this run: the conflict rows",
			"above are live in the working set, left by an earlier halted sync or a hand-run",
			"merge. Nothing was pulled, merged, or pushed this run. Resolve the live conflict",
			"before sync can make progress — Dolt refuses to merge over an unresolved one.",
			"Inspect with: bd conflicts list / bd conflicts show",
			conflictsResolveHint)
	case out.ConflictsLive:
		lines = append(lines,
			"The conflict rows above are LIVE in the working set — this pull route leaves them",
			"in place for you rather than aborting. The database is conflicted right now, and",
			"Dolt will refuse to merge over it, so every later sync halts here until you resolve",
			"it. Nothing was pushed.",
			"Inspect with: bd conflicts list / bd conflicts show",
			conflictsResolveHint)
	default:
		lines = append(lines,
			"The conflicted merge was aborted and the working set restored, so no local work was",
			"lost — and sync will keep halting here, unchanged, until an operator resolves the",
			"divergence between this replica and the remote. Nothing is lost by waiting.",
			"'bd conflicts list' will show nothing right now — this pull route restores the working",
			"set instead of leaving conflicts live for it. The same conflict recurs on every retry",
			"until the two histories are reconciled by hand (see 'bd vc merge --help' for a route",
			"that leaves conflicts live and resolvable instead of aborting).")
	}

	// Second: did anything at all happen before the halt? Only reported when
	// it did, so the common single-attempt case stays short.
	if out.Pulled {
		lines = append(lines,
			"Note: an earlier attempt in this run completed its pull and is_blocked repair before",
			"losing the push race; the retry is what conflicted. That earlier work remains in the",
			"local database and has NOT been published.")
	}
	// A dirty-working-set retry pulls without ever completing its repair, so it
	// leaves out.Pulled false while still having moved local history. Without
	// this the operator is told the run touched nothing, and goes looking in the
	// wrong place for the commits that pull merged.
	if out.LastRecomputeError != "" {
		lines = append(lines,
			"Note: an earlier attempt in this run completed its pull but its is_blocked repair was",
			"blocked by a dirty working set, so it retried. Anything that pull merged is in the local",
			"database, is NOT repaired, and has NOT been published.")
	}
	// The conflict rows above are real, but this run ALSO hit a pull error
	// unrelated to them — surfacing only the conflict would hide it.
	if out.DiscardedPullError != "" {
		lines = append(lines,
			"Note: this run's pull also failed with an error unrelated to the conflict above:",
			fmt.Sprintf("  pull error: %s", out.DiscardedPullError),
			"That failure is reported here only — resolving the conflict will not fix it.")
	}
	return lines
}

// syncRetriesExhaustedMessage renders the operator-facing report for exit 3.
// Two different transient conditions land here and they need different next
// steps: a push race is between REPLICAS and resolves by retrying or raising
// --attempts, while a dirty working set is another writer on THIS replica and
// resolves when they commit. Telling an operator "another replica kept winning
// the race" when the real blocker is an uncommitted local edit sends them to
// the wrong machine. A pure function of the outcome, for the same reason
// syncConflictMessage is.
func syncRetriesExhaustedMessage(out *syncOutcome) []string {
	if out.LastRecomputeError != "" {
		lines := []string{
			fmt.Sprintf("Error: is_blocked repair kept finding a dirty working set after %d attempt(s).", out.Attempts),
			fmt.Sprintf("  last recompute error: %s", out.LastRecomputeError),
			"Another writer has uncommitted changes to issues/dependencies on this replica, and the",
			"repair refuses to derive is_blocked from a graph it cannot commit. Nothing was pushed.",
			"This is transient — retry on the next tick, or commit/discard the pending changes.",
			"If it is NOT transient (a table left dirty by constraint violations never clears), the",
			"next few runs will see the identical pending edits and escalate to exit 4 rather than",
			"reporting this forever.",
		}
		return append(lines, syncMixedTransientNote(out)...)
	}
	lines := []string{fmt.Sprintf("Error: push-race retries exhausted after %d attempt(s).", out.Attempts)}
	if out.LastPushError != "" {
		lines = append(lines, fmt.Sprintf("  last push error: %s", out.LastPushError))
	}
	lines = append(lines,
		"This is transient — another replica kept winning the race. Retry on the next tick, or raise --attempts.")
	return append(lines, syncMixedTransientNote(out)...)
}

// syncMixedTransientNote reports the transient conditions this run fought that
// the headline does not name. The headline is about the FINAL attempt, which is
// the right thing to act on; without this an operator reading "push-race
// retries exhausted" has no way to know a dirty working set also ate an attempt
// of the budget, and would raise --attempts when the real story is contention
// on two different axes (wy-wub2s).
func syncMixedTransientNote(out *syncOutcome) []string {
	if !out.sawTransient(syncTransientPushRace) || !out.sawTransient(syncTransientDirtyGraph) {
		return nil
	}
	return []string{
		"Note: this run hit BOTH transient conditions — a lost push race and a dirty working set.",
		"The report above names what the final attempt failed on; --json lists every attempt under",
		"\"transients\".",
	}
}

// syncStuckMessage renders the operator-facing report for exit 4: the dirty
// working set is not going to clear on its own.
//
// This is the escalation exit 3 cannot make. A permanently-dirty graph table —
// constraint violations no writer will ever commit, an abandoned uncommitted
// edit — reports the same "transient, retry on the next tick" forever, so no
// tick ever publishes and nothing in the output ever changes to say so
// (wy-wub2s). Two distinct kinds of evidence can drive this: out.ConstraintViolations
// is a POSITIVE read, from storage.MergeBlockerInspector, naming exactly what
// is stuck and why on the very attempt that found it (wy-mhouc); absent that,
// out.DirtyGraphStuckTicks is the fallback INFERENCE — the pending graph edits
// have been byte-identical across every attempt of the last N runs, so this is
// a stuck table rather than a busy fleet, without knowing the specific cause.
func syncStuckMessage(out *syncOutcome) []string {
	if len(out.ConstraintViolations) > 0 {
		lines := []string{
			"Error: the is_blocked repair is blocked by constraint violations no writer will ever commit.",
		}
		for _, v := range out.ConstraintViolations {
			lines = append(lines, fmt.Sprintf("  constraint violation: %s (%d row(s))", v.Table, v.Count))
		}
		if out.LastRecomputeError != "" {
			lines = append(lines, fmt.Sprintf("  last recompute error: %s", out.LastRecomputeError))
		}
		lines = append(lines,
			"A constraint violation is not something any commit resolves — the auto-repair path already",
			"declined it, so retrying can never publish. Resolve it by hand, then the next tick syncs",
			"normally:",
			"  bd vc status                 # what is dirty",
			"  bd conflicts list            # constraint violations / conflicts holding it dirty",
			"  bd vc commit -m '...'        # commit the pending changes, if they are wanted",
			"Nothing was pushed. This exit is deliberately distinct from exit 3 so a sync timer can page",
			"instead of retrying forever.")
		return append(lines, syncMixedTransientNote(out)...)
	}
	lines := []string{
		fmt.Sprintf("Error: the is_blocked repair has been blocked by the SAME pending graph edits for %d consecutive sync run(s).", out.DirtyGraphStuckTicks),
	}
	if out.LastRecomputeError != "" {
		lines = append(lines, fmt.Sprintf("  last recompute error: %s", out.LastRecomputeError))
	}
	lines = append(lines,
		"Nothing is advancing: every attempt saw an identical set of uncommitted changes to",
		"issues/dependencies, so this is not a concurrent writer that is about to commit. Retrying",
		"cannot publish — the repair refuses to derive is_blocked from a graph it cannot commit, so",
		"local commits stay unpublished until an operator clears the working set.",
		"Resolve it by hand, then the next tick syncs normally:",
		"  bd vc status                 # what is dirty",
		"  bd conflicts list            # constraint violations / conflicts holding it dirty",
		"  bd vc commit -m '...'        # commit the pending changes, if they are wanted",
		"Nothing was pushed. This exit is deliberately distinct from exit 3 so a sync timer can page",
		"instead of retrying forever.")
	return append(lines, syncMixedTransientNote(out)...)
}

// syncStuckTicks is how many consecutive exhausted runs against byte-identical
// pending graph edits it takes before sync calls the working set stuck rather
// than busy.
//
// It is deliberately more than one. A single run's attempts are paced by one
// pull round trip each, so a fleet writing in bursts really can show the same
// fingerprint for the whole budget; requiring the evidence to survive several
// runs — minutes apart on a timer — is what keeps a busy shared server from
// being escalated as a stuck one. Every intervening run that publishes, or that
// sees any different pending edits, resets the count to zero.
const syncStuckTicks = 3

// syncStateFile holds the cross-tick half of the stuck detector, beside the
// auto-export state. It is local scratch, never version-controlled: writing this
// evidence into the database would add to the very dirty working set it is
// evidence about.
const syncStateFile = "sync-state.json"

// syncState is what one sync run leaves behind for the next one.
type syncState struct {
	// DirtyGraphFingerprint is the opaque token from the last exhausted run.
	DirtyGraphFingerprint string `json:"dirty_graph_fingerprint,omitempty"`
	// StuckTicks counts consecutive exhausted runs that saw it.
	StuckTicks int       `json:"stuck_ticks,omitempty"`
	FirstSeen  time.Time `json:"first_seen,omitempty"`
}

// classifyDirtyProgress folds this run's evidence into the persisted marker and
// reports the marker the next run should see, plus whether this run escalates.
//
// Pure, so the escalation rule is testable without a clock, a filesystem, or a
// Dolt server. Any outcome that is not an exhausted-on-dirty run clears the
// marker: a run that published, conflicted, or exhausted on a push race is
// evidence that this replica is not wedged on pending graph edits.
func classifyDirtyProgress(out *syncOutcome, prev *syncState, now time.Time) (*syncState, bool) {
	// LastRecomputeError, not just any dirty transient: the marker is about the
	// condition that is still blocking us as the run ends.
	blocked := out.Status == syncStatusRetriesExhausted &&
		out.LastRecomputeError != "" &&
		out.DirtyGraphFingerprint != ""
	if !blocked {
		return &syncState{}, false
	}
	next := &syncState{DirtyGraphFingerprint: out.DirtyGraphFingerprint, StuckTicks: 1, FirstSeen: now}
	if prev != nil && prev.DirtyGraphFingerprint == out.DirtyGraphFingerprint {
		next.StuckTicks = prev.StuckTicks + 1
		if !prev.FirstSeen.IsZero() {
			next.FirstSeen = prev.FirstSeen
		}
	}
	return next, next.StuckTicks >= syncStuckTicks
}

// applyDirtyProgress runs the cross-tick half of the detector: load the marker,
// classify, persist, and promote the outcome to the stuck status when the
// evidence has survived long enough.
//
// Failure to read or write the marker is not fatal and not reported: the detector
// is an escalation on top of a working retry, so a rig with no .beads directory
// (or an unwritable one) keeps the pre-existing exit-3 behavior instead of losing
// the sync.
func applyDirtyProgress(out *syncOutcome, now time.Time) {
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return
	}
	next, stuck := classifyDirtyProgress(out, loadSyncState(beadsDir), now)
	saveSyncState(beadsDir, next)
	out.DirtyGraphStuckTicks = next.StuckTicks
	if stuck {
		out.Status = syncStatusDirtyStuck
	}
}

func loadSyncState(beadsDir string) *syncState {
	data, err := os.ReadFile(filepath.Join(beadsDir, syncStateFile)) //nolint:gosec // path is the resolved .beads dir
	if err != nil {
		return &syncState{}
	}
	var state syncState
	if err := json.Unmarshal(data, &state); err != nil {
		return &syncState{}
	}
	return &state
}

func saveSyncState(beadsDir string, state *syncState) {
	path := filepath.Join(beadsDir, syncStateFile)
	if state == nil || state.DirtyGraphFingerprint == "" {
		// Nothing to remember. Remove rather than write an empty marker so a
		// healthy rig does not carry stale scratch around.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			debug.Logf("sync: failed to clear %s: %v\n", path, err)
		}
		return
	}
	data, err := json.Marshal(state)
	if err != nil {
		debug.Logf("sync: failed to marshal sync state: %v\n", err)
		return
	}
	if err := atomicfile.WriteFile(path, data, 0o600); err != nil {
		debug.Logf("sync: failed to save sync state: %v\n", err)
	}
}

func printSyncOutcome(out *syncOutcome, noPush bool) {
	switch out.Status {
	case syncStatusConflict:
		for _, line := range syncConflictMessage(out) {
			fmt.Fprintln(os.Stderr, line)
		}
	case syncStatusRetriesExhausted:
		for _, line := range syncRetriesExhaustedMessage(out) {
			fmt.Fprintln(os.Stderr, line)
		}
	case syncStatusDirtyStuck:
		for _, line := range syncStuckMessage(out) {
			fmt.Fprintln(os.Stderr, line)
		}
	default:
		printSyncSuccess(out, noPush)
	}
}

func printSyncSuccess(out *syncOutcome, noPush bool) {
	// The success path is exactly the non-essential output -q exists to
	// silence — this verb's whole point is running unattended on a short
	// timer. Conflict and retry branches are not gated: -q means errors only.
	if isQuiet() {
		return
	}
	if out.RowsCorrected > 0 {
		fmt.Printf("Recomputed is_blocked: %d row(s) corrected.\n", out.RowsCorrected)
	}
	if noPush {
		fmt.Println("Sync complete (push skipped: rig is local-only, no-push: true).")
		return
	}
	fmt.Println("Sync complete.")
}
