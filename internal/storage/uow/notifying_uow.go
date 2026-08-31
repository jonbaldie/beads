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
	"context"

	"github.com/jonbaldie/beads/internal/storage/domain"
	storageissueops "github.com/jonbaldie/beads/internal/storage/issueops"
)

// ── The unit of work ────────────────────────────────────────────────

// notifyingUOW wraps a UnitOfWork, buffering committed mutations and firing
// hooks after commit. The embedded UnitOfWork provides passthrough for the use
// cases and methods that record nothing.
type notifyingUOW struct {
	UnitOfWork
	rec   *recorder
	sinks Sinks

	issueUC   domain.IssueUseCase
	depUC     domain.DependencyUseCase
	labelUC   domain.LabelUseCase
	commentUC domain.CommentUseCase
}

// StatementRunner forwards the inner transaction so Lifecycle can run the
// shared Execute* body without peeling this decorator.
func (u *notifyingUOW) StatementRunner() storageissueops.DBTX {
	if src, ok := u.UnitOfWork.(statementSource); ok {
		return src.StatementRunner()
	}
	return nil
}

func (u *notifyingUOW) IssueUseCase() domain.IssueUseCase {
	if u.issueUC == nil {
		ctx := &RecordingIssueContext{IssueUseCase: u.UnitOfWork.IssueUseCase(), rec: u.rec, snap: u.snapshotter()}
		u.issueUC = &recordingIssueUC{
			IssueUseCasePassthrough:        IssueUseCasePassthrough{IssueUseCase: ctx.IssueUseCase},
			RecordingIssueCreateMethods:    &RecordingIssueCreateMethods{RecordingIssueContext: ctx},
			RecordingIssueGraphMethods:     &RecordingIssueGraphMethods{RecordingIssueContext: ctx},
			RecordingIssueUpdateMethods:    &RecordingIssueUpdateMethods{RecordingIssueContext: ctx},
			RecordingIssueClaimMethods:     &RecordingIssueClaimMethods{RecordingIssueContext: ctx},
			RecordingIssueLifecycleMethods: &RecordingIssueLifecycleMethods{RecordingIssueContext: ctx},
		}
	}
	return u.issueUC
}

func (u *notifyingUOW) DependencyUseCase() domain.DependencyUseCase {
	if u.depUC == nil {
		u.depUC = &recordingDepUC{DependencyUseCase: u.UnitOfWork.DependencyUseCase(), rec: u.rec, snap: u.snapshotter()}
	}
	return u.depUC
}

func (u *notifyingUOW) LabelUseCase() domain.LabelUseCase {
	if u.labelUC == nil {
		u.labelUC = &recordingLabelUC{LabelUseCase: u.UnitOfWork.LabelUseCase(), rec: u.rec, snap: u.snapshotter()}
	}
	return u.labelUC
}

func (u *notifyingUOW) CommentUseCase() domain.CommentUseCase {
	if u.commentUC == nil {
		u.commentUC = &recordingCommentUC{CommentUseCase: u.UnitOfWork.CommentUseCase(), rec: u.rec, snap: u.snapshotter()}
	}
	return u.commentUC
}

// Commit commits the underlying unit of work, then fires the buffered hooks in
// the order the mutations happened. Hooks are fire-and-forget, so the user's
// committed mutation always succeeds.
func (u *notifyingUOW) Commit(ctx context.Context, message string) error {
	if err := u.UnitOfWork.Commit(ctx, message); err != nil {
		return err
	}
	entries := u.rec.Drain()
	if u.sinks.Hook == nil {
		return nil
	}
	for _, entry := range entries {
		u.sinks.Hook.Run(entry.event, entry.issue)
	}
	return nil
}

// Close rolls the unit of work back unless it committed, so whatever the
// attempt buffered is dropped rather than fired.
func (u *notifyingUOW) Close(ctx context.Context) {
	u.rec.Drain()
	u.UnitOfWork.Close(ctx)
}

func (u *notifyingUOW) snapshotter() *snapshotter {
	return &snapshotter{
		issues: u.UnitOfWork.IssueUseCase(),
		labels: u.UnitOfWork.LabelUseCase(),
		deps:   u.UnitOfWork.DependencyUseCase(),
	}
}

// Unwrap returns the unit of work beneath the recording layer, satisfying
// unitOfWorkUnwrapper.
func (u *notifyingUOW) Unwrap() UnitOfWork { return u.UnitOfWork }
