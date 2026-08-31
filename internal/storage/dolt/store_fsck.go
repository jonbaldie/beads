package dolt

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// prePushFSCK runs dolt fsck --quiet to verify local chunk integrity before
// pushing. This prevents propagating Dolt remote corruption (dangling blob
// references) that arise when concurrent pushes race on the remote manifest.
//
// When multiple agents push simultaneously, one push's manifest update can
// land before another's chunks finish uploading, leaving a manifest that
// references chunks that were never stored. Any agent that then fetches and
// re-pushes that remote faithfully propagates the dangling reference.
//
// If CLIDir is empty or .dolt/noms does not exist, the check is skipped.
// Six outcomes are possible when fsck exits non-zero (see classifyFSCKFailure):
//   - non-empty output, could-not-open: skipped with a log warning.
//   - non-empty output, other: ErrDanglingReference — push aborted.
//   - parent context canceled: cancellation error — push aborted.
//   - parent context deadline exceeded: ErrFSCKTimeout (caller timeout) — push aborted.
//   - fsck own timeout: ErrFSCKTimeout (raise BEADS_FSCK_TIMEOUT) — push aborted.
//   - cancellation phrasing in output, no context error: cancellation error — push aborted.
func (s *DoltStore) prePushFSCK(ctx context.Context) error {
	dir := s.CLIDir()
	if dir == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, ".dolt", "noms")); os.IsNotExist(err) {
		return nil
	}
	fsckCtx, cancel := context.WithTimeout(ctx, fsckTimeoutDuration())
	defer cancel()
	cmd := exec.CommandContext(fsckCtx, "dolt", "fsck", "--quiet") // #nosec G204 -- fixed command
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := strings.TrimSpace(string(out))
		if classified := classifyFSCKFailure(ctx.Err(), fsckCtx.Err(), output); classified != nil {
			return classified
		}
		log.Printf("pre-push fsck could not run, skipping integrity check: %s", output)
		return nil
	}
	return nil
}

// classifyFSCKFailure maps a failed dolt fsck exit into one of six outcomes,
// evaluated in priority order:
//
//	(a) Non-empty output → route by content: could-not-open → nil (caller logs
//	    and skips); cancellation phrasing (fsckOutputInterrupted) → fall
//	    through to the context-state branches below, because an interrupted
//	    fsck prints e.g. "context canceled" before dying and that text is not
//	    an integrity finding; any other content → ErrDanglingReference abort.
//	    Non-empty output means fsck actually ran and said something (--quiet
//	    fsck is silent until it finds a problem), so content wins over context
//	    state. This also closes the race where real corruption arrives at the
//	    deadline instant and would otherwise be masked as a timeout.
//
//	(b) Parent context canceled (Ctrl-C, caller abort) → plain cancellation
//	    error wrapping context.Canceled; neither ErrDanglingReference nor
//	    ErrFSCKTimeout (the store is not implicated).
//
//	(c) Parent context deadline exceeded → ErrFSCKTimeout, but guidance points
//	    at the CALLER's timeout (e.g. dolt.auto-push-timeout for auto-push)
//	    and explicitly notes BEADS_FSCK_TIMEOUT cannot extend it. Checked
//	    before fsck's own deadline because a fired parent context propagates
//	    cancellation into all child contexts, making both parentErr and fsckErr
//	    DeadlineExceeded simultaneously.
//
//	(d) fsck's own deadline exceeded, parent still running → ErrFSCKTimeout
//	    with dolt gc / CALL DOLT_GC() / BEADS_FSCK_TIMEOUT guidance.
//
//	(e) Cancellation phrasing in output but no recognized context error — the
//	    bd process (group) was killed out from under fsck, so neither context
//	    carries the reason. Plain cancellation error wrapping context.Canceled;
//	    the store is not implicated.
//
//	(f) Generic non-zero exit, empty output → ErrDanglingReference abort.
//
// Returning nil for the could-not-open case (branch a) distinguishes "fsck
// couldn't run at all" from "fsck ran and found a problem". Wrapping an
// open-failure as ErrDanglingReference misleads users (dolthub/dolt#10915);
// so does wrapping an interrupt's "context canceled" noise as corruption
// (same genus, observed when a background push's process group was killed).
func classifyFSCKFailure(parentErr, fsckErr error, output string) error {
	// (a) Non-empty output: fsck actually reported something; route by content.
	if output != "" {
		if fsckCouldNotOpen(output) {
			return nil
		}
		if !fsckOutputInterrupted(output) {
			return fmt.Errorf("%w: aborting push to prevent propagating corrupt chunks: %s",
				ErrDanglingReference, output)
		}
		// Cancellation phrasing: not an integrity finding — classify by
		// context state below.
	}
	// (b) Parent context canceled — user interrupt or caller abort.
	if errors.Is(parentErr, context.Canceled) {
		return fmt.Errorf("pre-push integrity check interrupted: %w", parentErr)
	}
	// (c) Caller's deadline expired during fsck. Point at the caller's timeout;
	// BEADS_FSCK_TIMEOUT cannot extend a deadline imposed by the caller.
	if errors.Is(parentErr, context.DeadlineExceeded) {
		return fmt.Errorf("%w: the surrounding operation's deadline expired during the "+
			"pre-push integrity check; to extend it, raise the caller timeout "+
			"(for auto-push: the dolt.auto-push-timeout config); "+
			"note that BEADS_FSCK_TIMEOUT cannot extend the caller deadline",
			ErrFSCKTimeout)
	}
	// (d) fsck's own per-call timeout expired (parent still running).
	if errors.Is(fsckErr, context.DeadlineExceeded) {
		return fmt.Errorf("%w: fsck did not complete within the configured timeout; "+
			"the push was aborted without checking integrity (the store is not necessarily corrupt); "+
			"large stores can be shrunk with `dolt gc` (or `CALL DOLT_GC()` on a running sql-server); "+
			"the timeout can be raised via the BEADS_FSCK_TIMEOUT environment variable",
			ErrFSCKTimeout)
	}
	// (e) Interrupted fsck whose cancellation reason lives only in the output
	// (process group killed: both contexts look healthy from here).
	if fsckOutputInterrupted(output) {
		return fmt.Errorf("pre-push integrity check interrupted: %w: %s",
			context.Canceled, output)
	}
	// (f) Generic failure with no output and no recognized context error.
	return fmt.Errorf("%w: aborting push to prevent propagating corrupt chunks",
		ErrDanglingReference)
}

// fsckCouldNotOpen reports whether dolt fsck output indicates the check
// could not run at all (as opposed to finding integrity problems). Matches
// the known error phrasings dolt emits before any integrity work begins.
func fsckCouldNotOpen(output string) bool {
	switch {
	case strings.Contains(output, "Could not open dolt database"):
		return true
	case strings.Contains(output, "repository state is invalid"):
		return true
	default:
		return false
	}
}

// fsckOutputInterrupted reports whether dolt fsck output is cancellation
// noise from a dying process rather than an integrity finding. An fsck
// interrupted mid-run (Ctrl-C, killed process group, expired deadline)
// prints the literal cancellation text to its combined output before
// exiting; treating that as a dangling-reference finding produces a false
// corruption report for a plain interrupt (bd-f2b15).
func fsckOutputInterrupted(output string) bool {
	for _, phrase := range []string{
		"context canceled",
		"context deadline exceeded",
		"signal: killed",
		"signal: terminated",
	} {
		if strings.Contains(output, phrase) {
			return true
		}
	}
	return false
}
