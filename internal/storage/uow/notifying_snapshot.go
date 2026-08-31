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

// ── Snapshots ───────────────────────────────────────────────────────

// snapshotter reads post-mutation state inside the transaction, so a buffered
// notification carries the issue as the mutation left it. Every read is
// best-effort: a failure drops the notification rather than the mutation.
//
// EVERY buffered snapshot is read here rather than taken off the verb's own
// result, and that is what makes the payload uniform. A hook script on the
// DoltStorage plumbing is handed a hydrated issue — the row with its LABELS,
// and for an edge change its dependency records too (fireHookByID and
// fireDependencyHookByID both re-read) — so hydrating in one place is the only
// way the two plumbings hand a script the same thing. A verb result carries
// whatever that verb happened to build.
type snapshotter struct {
	issues domain.IssueUseCase
	labels domain.LabelUseCase
	deps   domain.DependencyUseCase
}

// issue reads the issue plane, wisp the wisp plane. Which one a mutation
// touched is never in doubt at the call sites below — the use cases spell the
// plane in the method name.
func (s *snapshotter) issue(ctx context.Context, id string) *types.Issue {
	issue, err := s.issues.GetIssue(ctx, id)
	if err != nil {
		return nil
	}
	return withLabels(issue, func() ([]string, error) { return s.labels.GetLabels(ctx, id) })
}

func (s *snapshotter) wisp(ctx context.Context, id string) *types.Issue {
	issue, err := s.issues.GetWisp(ctx, id)
	if err != nil {
		return nil
	}
	return withLabels(issue, func() ([]string, error) { return s.labels.GetWispLabels(ctx, id) })
}

// withLabels attaches the read labels to issue, leaving the row alone when the
// label read fails: a partial snapshot still beats no notification.
func withLabels(issue *types.Issue, read func() ([]string, error)) *types.Issue {
	if issue == nil {
		return nil
	}
	labels, err := read()
	if err != nil {
		return issue
	}
	issue.Labels = labels
	return issue
}

// anyPlane resolves an id whose plane the mutation did not pin, the way the
// package's own operationIssue does: wisps first, then issues.
//
// The DoltStorage plumbing probes the other way round, and the order cannot
// matter: an id names a row on ONE plane. Both planes mint from the same
// per-prefix counter (issue_counter, via NextCounterID), and a promotion MOVES
// the row rather than copying it, so the two tables never hold the same id. If
// they ever could, the probe order would be a coin toss on both plumbings
// rather than a difference between them.
func (s *snapshotter) AnyPlane(ctx context.Context, id string) *types.Issue {
	if issue := s.wisp(ctx, id); issue != nil {
		return issue
	}
	return s.issue(ctx, id)
}

// The *WithEdges snapshots are what a dependency mutation records: the issue
// plus its dependency records, so a hook script sees the graph the edit
// produced rather than the row alone. This is what fireDependencyHookByID hands
// a script on the DoltStorage plumbing.
func (s *snapshotter) issueWithEdges(ctx context.Context, id string) *types.Issue {
	return withEdges(s.issue(ctx, id), id, func() (map[string][]*types.Dependency, error) {
		return s.deps.GetIssueDependencyRecords(ctx, []string{id})
	})
}

func (s *snapshotter) wispWithEdges(ctx context.Context, id string) *types.Issue {
	return withEdges(s.wisp(ctx, id), id, func() (map[string][]*types.Dependency, error) {
		return s.deps.GetWispDependencyRecords(ctx, []string{id})
	})
}

func (s *snapshotter) AnyPlaneWithEdges(ctx context.Context, id string) *types.Issue {
	if issue := s.wispWithEdges(ctx, id); issue != nil {
		return issue
	}
	return s.issueWithEdges(ctx, id)
}

// withEdges attaches the read records to issue, leaving the row alone when the
// edge read fails: a partial snapshot still beats no notification.
func withEdges(issue *types.Issue, id string, read func() (map[string][]*types.Dependency, error)) *types.Issue {
	if issue == nil {
		return nil
	}
	records, err := read()
	if err != nil {
		return issue
	}
	issue.Dependencies = records[id]
	return issue
}
