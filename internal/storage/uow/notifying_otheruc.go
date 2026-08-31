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
	"github.com/jonbaldie/beads/internal/types"
)

// ── Dependency use case ─────────────────────────────────────────────

type recordingDepUC struct {
	domain.DependencyUseCase
	rec  *recorder
	snap *snapshotter
}

func (u *recordingDepUC) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	if err := u.DependencyUseCase.AddDependency(ctx, dep, actor); err != nil {
		return err
	}
	u.rec.Record(opDepAdd, u.snap.issueWithEdges(ctx, dep.IssueID))
	return nil
}

func (u *recordingDepUC) AddWispDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	if err := u.DependencyUseCase.AddWispDependency(ctx, dep, actor); err != nil {
		return err
	}
	u.rec.Record(opDepAdd, u.snap.wispWithEdges(ctx, dep.IssueID))
	return nil
}

// AddDependencies fires once per DISTINCT SOURCE ISSUE the batch edited, in
// first-edit order — the multiplicity hookDependencyEditor produces on the
// other plumbing. Edges are routed per source, so the snapshot resolves each
// source's plane rather than assuming one.
func (u *recordingDepUC) AddDependencies(ctx context.Context, deps []*types.Dependency, actor string, opts domain.BulkAddDepsOpts) (domain.BulkAddDepsResult, error) {
	res, err := u.DependencyUseCase.AddDependencies(ctx, deps, actor, opts)
	if err != nil {
		return res, err
	}
	seen := make(map[string]bool, len(res.Added))
	for _, dep := range res.Added {
		if dep == nil || dep.IssueID == "" || seen[dep.IssueID] {
			continue
		}
		seen[dep.IssueID] = true
		u.rec.Record(opDepAdd, u.snap.AnyPlaneWithEdges(ctx, dep.IssueID))
	}
	return res, nil
}

func (u *recordingDepUC) RemoveDependency(ctx context.Context, issueID, dependsOnID, actor string) error {
	if err := u.DependencyUseCase.RemoveDependency(ctx, issueID, dependsOnID, actor); err != nil {
		return err
	}
	u.rec.Record(opDepRemove, u.snap.issueWithEdges(ctx, issueID))
	return nil
}

func (u *recordingDepUC) RemoveWispDependency(ctx context.Context, wispID, dependsOnID, actor string) error {
	if err := u.DependencyUseCase.RemoveWispDependency(ctx, wispID, dependsOnID, actor); err != nil {
		return err
	}
	u.rec.Record(opDepRemove, u.snap.wispWithEdges(ctx, wispID))
	return nil
}

// RemoveDependencyBySource reads the source's plane itself and reports whether
// an edge was actually removed, so nothing is recorded when nothing changed.
func (u *recordingDepUC) RemoveDependencyBySource(ctx context.Context, sourceID, dependsOnID, actor string) (bool, error) {
	removed, err := u.DependencyUseCase.RemoveDependencyBySource(ctx, sourceID, dependsOnID, actor)
	if err != nil || !removed {
		return removed, err
	}
	u.rec.Record(opDepRemove, u.snap.AnyPlaneWithEdges(ctx, sourceID))
	return removed, nil
}

func (u *recordingDepUC) Reparent(ctx context.Context, childID, newParentID, actor string) error {
	if err := u.DependencyUseCase.Reparent(ctx, childID, newParentID, actor); err != nil {
		return err
	}
	u.rec.Record(opUpdate, u.snap.issueWithEdges(ctx, childID))
	return nil
}

func (u *recordingDepUC) ReparentWisp(ctx context.Context, childWispID, newParentID, actor string) error {
	if err := u.DependencyUseCase.ReparentWisp(ctx, childWispID, newParentID, actor); err != nil {
		return err
	}
	u.rec.Record(opUpdate, u.snap.wispWithEdges(ctx, childWispID))
	return nil
}

// ── Label use case ──────────────────────────────────────────────────

type recordingLabelUC struct {
	domain.LabelUseCase
	rec  *recorder
	snap *snapshotter
}

// labeled records the update a label write is, from the issue as the write
// left it. One event per call, not per label: a label write is one change to
// one issue.
func (u *recordingLabelUC) labeled(ctx context.Context, issueID string) {
	u.rec.Record(opUpdate, u.snap.issue(ctx, issueID))
}

func (u *recordingLabelUC) wispLabeled(ctx context.Context, wispID string) {
	u.rec.Record(opUpdate, u.snap.wisp(ctx, wispID))
}

func (u *recordingLabelUC) AddLabel(ctx context.Context, issueID, label, actor string) error {
	if err := u.LabelUseCase.AddLabel(ctx, issueID, label, actor); err != nil {
		return err
	}
	u.labeled(ctx, issueID)
	return nil
}

func (u *recordingLabelUC) RemoveLabel(ctx context.Context, issueID, label, actor string) error {
	if err := u.LabelUseCase.RemoveLabel(ctx, issueID, label, actor); err != nil {
		return err
	}
	u.labeled(ctx, issueID)
	return nil
}

func (u *recordingLabelUC) AddLabels(ctx context.Context, issueID string, labels []string, actor string) error {
	if err := u.LabelUseCase.AddLabels(ctx, issueID, labels, actor); err != nil {
		return err
	}
	u.labeled(ctx, issueID)
	return nil
}

func (u *recordingLabelUC) RemoveLabels(ctx context.Context, issueID string, labels []string, actor string) error {
	if err := u.LabelUseCase.RemoveLabels(ctx, issueID, labels, actor); err != nil {
		return err
	}
	u.labeled(ctx, issueID)
	return nil
}

func (u *recordingLabelUC) SetLabels(ctx context.Context, issueID string, labels []string, actor string) error {
	if err := u.LabelUseCase.SetLabels(ctx, issueID, labels, actor); err != nil {
		return err
	}
	u.labeled(ctx, issueID)
	return nil
}

// InheritFromParent records only when it actually wrote a label; a child that
// inherits nothing is not an update.
func (u *recordingLabelUC) InheritFromParent(ctx context.Context, childID, parentID, actor string, skipExisting []string) ([]string, error) {
	inherited, err := u.LabelUseCase.InheritFromParent(ctx, childID, parentID, actor, skipExisting)
	if err == nil && len(inherited) > 0 {
		u.labeled(ctx, childID)
	}
	return inherited, err
}

func (u *recordingLabelUC) AddWispLabel(ctx context.Context, wispID, label, actor string) error {
	if err := u.LabelUseCase.AddWispLabel(ctx, wispID, label, actor); err != nil {
		return err
	}
	u.wispLabeled(ctx, wispID)
	return nil
}

func (u *recordingLabelUC) RemoveWispLabel(ctx context.Context, wispID, label, actor string) error {
	if err := u.LabelUseCase.RemoveWispLabel(ctx, wispID, label, actor); err != nil {
		return err
	}
	u.wispLabeled(ctx, wispID)
	return nil
}

func (u *recordingLabelUC) AddWispLabels(ctx context.Context, wispID string, labels []string, actor string) error {
	if err := u.LabelUseCase.AddWispLabels(ctx, wispID, labels, actor); err != nil {
		return err
	}
	u.wispLabeled(ctx, wispID)
	return nil
}

func (u *recordingLabelUC) RemoveWispLabels(ctx context.Context, wispID string, labels []string, actor string) error {
	if err := u.LabelUseCase.RemoveWispLabels(ctx, wispID, labels, actor); err != nil {
		return err
	}
	u.wispLabeled(ctx, wispID)
	return nil
}

func (u *recordingLabelUC) SetWispLabels(ctx context.Context, wispID string, labels []string, actor string) error {
	if err := u.LabelUseCase.SetWispLabels(ctx, wispID, labels, actor); err != nil {
		return err
	}
	u.wispLabeled(ctx, wispID)
	return nil
}

func (u *recordingLabelUC) InheritFromWispParent(ctx context.Context, childWispID, parentWispID, actor string, skipExisting []string) ([]string, error) {
	inherited, err := u.LabelUseCase.InheritFromWispParent(ctx, childWispID, parentWispID, actor, skipExisting)
	if err == nil && len(inherited) > 0 {
		u.wispLabeled(ctx, childWispID)
	}
	return inherited, err
}

// ── Comment use case ────────────────────────────────────────────────

type recordingCommentUC struct {
	domain.CommentUseCase
	rec  *recorder
	snap *snapshotter
}

// AddCommentToIssue records an update, which is what a comment fires on the
// DoltStorage plumbing: there is no on_comment event, and a comment is a change
// to the issue as far as a script is concerned.
func (u *recordingCommentUC) AddCommentToIssue(ctx context.Context, issueID, author, text string) (*types.Comment, error) {
	comment, err := u.CommentUseCase.AddCommentToIssue(ctx, issueID, author, text)
	if err == nil {
		u.rec.Record(opUpdate, u.snap.issue(ctx, issueID))
	}
	return comment, err
}

func (u *recordingCommentUC) AddCommentToWisp(ctx context.Context, wispID, author, text string) (*types.Comment, error) {
	comment, err := u.CommentUseCase.AddCommentToWisp(ctx, wispID, author, text)
	if err == nil {
		u.rec.Record(opUpdate, u.snap.wisp(ctx, wispID))
	}
	return comment, err
}
