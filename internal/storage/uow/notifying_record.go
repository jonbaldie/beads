// Package uow — notifying.go
//
// bd has two write plumbings. The DoltStorage decorator chain fires the
// workspace's script hooks after every mutation it lands
// (internal/storage/hook_decorator.go and its per-role siblings). The
// unit-of-work plumbing — the one proxied-server mode writes through — fired
// nothing, so any script wired to .beads/hooks/ silently missed every mutation
// that went through it.
//
// A NotifyingProvider closes that gap. It wraps a UnitOfWorkProvider so that
// every committed bead mutation performed through a unit of work runs the same
// fire-and-forget hooks the DoltStorage plumbing runs, with the same event per
// operation (see hookEventForOp).
//
// THE WRAP IS AT THE PROVIDER, NOT AT EACH VERB. Most capabilities this package
// publishes reach their writes through the unit of work's use cases, so
// re-binding the accessors to the wrapper and recording at the use-case seam
// covers those roles — including roles added later. Lifecycle is the exception:
// it runs the shared Execute* body on the statement runner and records hooks
// from the result, so it does not depend on the use-case recorder.
// The accessors are declared rather than delegated for the reason
// hook_issue_operations.go gives: an accessor that hands back the INNER
// provider's role builds it on the inner provider and silently drops every hook
// this file exists to fire. notifying_parity_test.go enforces both halves.
//
// Hooks fire strictly post-commit: mutations are buffered as they flow through
// the wrapped use cases (each buffered snapshot is read inside the transaction,
// so it reflects the mutation), and the buffer is drained to the runner only
// after Commit succeeds. A rolled-back unit of work fires nothing, and a
// retried transaction (RunTxResultWithin replays the whole attempt on a fresh
// unit of work) fires only what the winning attempt recorded.
//
// # Where this plumbing deliberately differs from the DoltStorage one
//
// Every per-verb firing rule below is matched 1:1 against the decorator that
// implements it (each override cites its twin). These five are the exceptions,
// and they are listed here rather than argued in five places:
//
//  1. LABEL MULTIPLICITY ON CREATE. The DoltStorage chain replays a legacy
//     sequence for a create that carries labels: on_create with the labels
//     STRIPPED, then one synthetic on_update per label, cumulatively
//     (createHookEvents). This plumbing fires ONE on_create carrying the issue
//     with its labels. The event vocabulary is the same and the information is
//     the same; the multiplicity is a compatibility shim for the pre-decorator
//     CLI, and a new plumbing that reproduced it would cement it.
//
//  2. EDGE MULTIPLICITY. A create or an edge batch that writes several edges
//     leaving one issue fires ONE on_update for that issue, where the
//     DoltStorage create path fires one per edge. The SET of issues told is the
//     same — the created row and every far end, each carrying its records; only
//     the repeat count differs. Per distinct source is the rule its own
//     dependency-editor role applies (hook_dependency_editor.go), and two edges
//     leaving an issue are one change to it.
//
//  3. IMPORT FIRES NOTHING. `bd import` runs the batch-upsert engine on the
//     unit of work's statement runner (importer.go), so its writes never pass a
//     use case for the recorder to see. The DoltStorage plumbing imports
//     through the same engine and fires nothing either; a hook per imported
//     issue would be a new behavior on both, not a fix to this one.
//
//  4. A GUARDED VERB'S PRECONDITION IS NOT AN UPDATE. Lifecycle close and
//     reopen pass ExpectedVersion into ExecuteClose / ExecuteReopen, matching
//     the store adapters. Other roles that still spell a compare-and-set as a
//     separate ApplyUpdate must not record that precondition as an update.
//     See specWrites.
//
//  5. THE PROMOTION COMMENT. `bd promote` records an audit comment on the
//     promoted issue, and this plumbing's one comment verb fires on_update for
//     it. The DoltStorage route writes that comment through the legacy
//     AddComment, which the decorator wraps only INSIDE a transaction
//     (hookTrackingTransaction.AddComment) and not at the store level, where
//     promote calls it — so an embedded promote fires nothing. One verb cannot
//     tell which caller it is serving, and "a comment landed" is the on_update
//     this vocabulary has.
//
// A sixth is not a divergence but is worth stating: a unit of work whose commit
// message is empty is ROLLED BACK by RunTxResult, and a rolled-back attempt
// fires nothing — so a write that lands nothing reports nothing, on both
// plumbings.
package uow

import (
	"github.com/jonbaldie/beads/internal/hooks"
	storageissueops "github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
	publicops "github.com/jonbaldie/beads/issueops"
)

// ── The buffer ──────────────────────────────────────────────────────

// mutationEntry is a buffered post-commit hook notification: the resolved hook
// event and the post-mutation issue snapshot to pass to the runner.
type mutationEntry struct {
	event string
	issue *types.Issue
}

// recorder buffers hook notifications accumulated during one unit of work.
type recorder struct {
	entries []mutationEntry
}

// record resolves op to a script-hook event and buffers a notification. Ops
// with no hook and mutations with no snapshot are dropped — the latter matches
// the DoltStorage plumbing, whose re-read before firing is best-effort too.
func (r *recorder) Record(op string, issue *types.Issue) {
	event, ok := hookEventForOp(op)
	if !ok || issue == nil {
		return
	}
	r.entries = append(r.entries, mutationEntry{event: event, issue: cloneIssueForHook(issue)})
}

func (r *recorder) Drain() []mutationEntry { e := r.entries; r.entries = nil; return e }

// batchNotificationBuffer is this decorator seen from the BATCH COMPOSITIONS in
// this package. They need it because the close verbs are shared: a single close
// and a batch item reach the same recordingIssueUC method, and the two have
// different firing rules — the single close announces an idempotent re-close,
// the batch verbs do not (ga-2yaqp.1). Gating inside the verb would change both.
//
// So the composition marks the buffer before the item's close and rewinds to
// that mark when the close turned out to have persisted nothing. Rewinding
// rather than not-recording is what keeps the rule where it belongs: the verb
// stays honest about what it did, and the composition decides what is worth
// telling a script about.
//
// It is an interface rather than a concrete field because a UnitOfWork is only
// sometimes this decorator — with hooks disabled NewNotifyingProvider hands back
// the inner provider unwrapped, and then there is no buffer and nothing to do.
type batchNotificationBuffer interface {
	markNotifications() int
	rewindNotifications(mark int)
}

func (u *notifyingUOW) markNotifications() int { return len(u.rec.entries) }

func (u *notifyingUOW) rewindNotifications(mark int) {
	if mark >= 0 && mark <= len(u.rec.entries) {
		u.rec.entries = u.rec.entries[:mark]
	}
}

// markBatchNotifications returns a rewind token for a batch item's close, or -1
// when this unit of work buffers nothing.
func markBatchNotifications(uw UnitOfWork) int {
	if buf, ok := uw.(batchNotificationBuffer); ok {
		return buf.markNotifications()
	}
	return -1
}

// rewindBatchNotifications drops whatever a batch item's close buffered. The
// caller has established the item changed nothing, which is the only thing that
// makes discarding a recorded mutation safe: nothing observes the buffer before
// Commit drains it.
func rewindBatchNotifications(uw UnitOfWork, mark int) {
	if mark < 0 {
		return
	}
	if buf, ok := uw.(batchNotificationBuffer); ok {
		buf.rewindNotifications(mark)
	}
}

// The recorder's op vocabulary. These name what happened; hookEventForOp maps
// them to the three events internal/hooks publishes.
const (
	opCreate    = "create"
	opUpdate    = "update"
	opClose     = "close"
	opDepAdd    = "dep_add"
	opDepRemove = "dep_remove"
)

// hookEventForOp maps a mutation to its script-hook event, matching the
// DoltStorage plumbing verb for verb: creates fire on_create, closes fire
// on_close, and everything else that changes a bead — field updates, reopens,
// claims, labels, comments, dependency edits — fires on_update, which is what
// HookFiringStore fires for each of them. Deletes are absent on purpose: they
// fire no script hook there either (hook_deleter.go), and the vocabulary has no
// name for a deletion.
func hookEventForOp(op string) (string, bool) {
	switch op {
	case opCreate:
		return hooks.EventCreate, true
	case opUpdate, opDepAdd, opDepRemove:
		return hooks.EventUpdate, true
	case opClose:
		return hooks.EventClose, true
	default:
		return "", false
	}
}

// cloneIssueForHook deep-copies a snapshot before it is buffered. The runner
// marshals the issue on its own goroutine while the caller goes on to mutate
// the result it was handed — `bd update` and `bd reopen` both strip fields off
// theirs — so the buffer must not share it. The DoltStorage decorator clones
// for the same reason.
func cloneIssueForHook(issue *types.Issue) *types.Issue {
	if issue == nil {
		return nil
	}
	return storageissueops.CloneCreateRequest(publicops.CreateRequest{Issue: issue}).Issue
}
