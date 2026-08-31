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
	storageissueops "github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

// hookRunner is the subset of *hooks.Runner this plumbing needs — the same
// fire-and-forget contract the DoltStorage plumbing uses. Declared as an
// interface so tests can supply a recording fake.
type hookRunner interface {
	Run(event string, issue *types.Issue)
}

// Sinks are the post-commit notification targets. Hook may be nil to disable.
type Sinks struct {
	// Hook is the workspace's script-hook runner (*hooks.Runner). Its behavior
	// is unchanged; the unit-of-work plumbing simply now feeds it too.
	Hook hookRunner
}

func (s Sinks) empty() bool { return s.Hook == nil }

// NewNotifyingProvider wraps inner so committed mutations fire hooks
// post-commit. With no sinks configured — `no-hooks`, or a caller that has no
// runner to give — inner is returned unwrapped, so a bd that cannot fire hooks
// pays nothing for this file.
//
// A workspace whose .beads/hooks/ holds no script is NOT that case: the runner
// exists, the wrapper is built, and the buffering happens; the runner then
// finds no script and does nothing. That is the same deal the DoltStorage chain
// takes (wireStorageDecorators wraps whenever a runner exists), and it is what
// lets a user drop in a hook script without restarting anything.
func NewNotifyingProvider(inner UnitOfWorkProvider, sinks Sinks) UnitOfWorkProvider {
	if sinks.empty() || isNilUnitOfWorkProvider(inner) {
		return inner
	}
	return &notifyingProvider{inner: inner, sinks: sinks}
}

type notifyingProvider struct {
	inner UnitOfWorkProvider
	sinks Sinks
}

// Unwrap returns the provider beneath the hook layer, satisfying
// ProviderUnwrapper.

// ProviderUnwrapper is implemented by provider decorators that wrap an inner
// provider — the unit-of-work twin of storage.Unwrapper.
type ProviderUnwrapper interface {
	Unwrap() UnitOfWorkProvider
}

// unitOfWorkUnwrapper is the same idea one level down, for decorators that wrap
// a unit of work.
type unitOfWorkUnwrapper interface {
	Unwrap() UnitOfWork
}

// unwrapUOW peels any chain of decorators off a unit of work. A caller reaches
// for it when it needs the CONCRETE unit of work rather than the surface —
// today only the importer, which asks the transaction for its statement runner
// (importer.go). Nothing about a decorator's behavior is bypassed by this: the
// recording layer wraps the USE CASES, and a caller that wants those still asks
// the unit of work it was handed.
//
// UNEXPORTED, unlike UnwrapProvider beside it, and deliberately: peeling a unit
// of work reaches the transaction, where a write fires no hook, and
// notifying_parity_test.go can only account for the callers it can PARSE. Keep
// it reachable only from this package and that guard stays complete. The
// provider twin is exported because `bd serve` genuinely needs it and peeling
// there loses no coverage — it hands back a provider, not a transaction.
func unwrapUOW(uw UnitOfWork) UnitOfWork {
	for {
		unwrapper, ok := uw.(unitOfWorkUnwrapper)
		if !ok {
			return uw
		}
		uw = unwrapper.Unwrap()
	}
}

// UnwrapProvider peels any chain of decorators and returns the innermost
// provider. A caller that must NOT run the workspace's hook scripts asks for
// this: `bd serve` serves from beneath the hook layer, as it does on the store
// side ((*storage.HookFiringStore).Unwrap).
func UnwrapProvider(p UnitOfWorkProvider) UnitOfWorkProvider {
	for {
		unwrapper, ok := p.(ProviderUnwrapper)
		if !ok {
			return p
		}
		p = unwrapper.Unwrap()
	}
}

// ProviderFiresHooks reports whether a landed write through this provider runs
// the workspace's hook scripts. It is the question a network server has to
// answer before it serves anything, and one a caller cannot answer from the
// provider's own type — the accessors on a notifying provider return notifying
// roles by design.
//
// A provider is asked about its type, not its configuration: NewNotifyingProvider
// hands back the inner provider when no sinks are set, so a wrapper only exists
// where hooks are meant to fire.
func ProviderFiresHooks(p UnitOfWorkProvider) bool {
	_, ok := p.(*notifyingProvider)
	return ok
}

// ── Capability accessors ────────────────────────────────────────────
//
// Each builds its role from THIS provider, so the role's units of work are
// notifying ones. Passing p.inner instead would compile, keep every type
// assertion working, and stop the hooks.

// BatchApplier builds on THIS provider. Landed items are announced after
// ApplyBatchInTx, not by composing recording use cases.

// EventsJournalCursor builds on THIS provider, like every role above it, so a
// journal read taken through a notifying provider still runs in a unit of work
// this layer opened. Nothing here records, and nothing needs to: this accessor
// reaches only the READ half of EventsJournalUseCase, which the parity guard
// exempts for reading state and changing none.

// ── Provider capabilities that are not roles ────────────────────────

// RunNonTx forwards the maintenance escape hatch. It runs on a pinned
// connection OUTSIDE any unit of work, so there is nothing to record: its
// callers are server-wide database maintenance, not bead writes.

// SetPoolLimits forwards pool tuning to the wrapped provider, which owns the
// pool. A provider that is not pool-backed is left alone, as it would be
// without this wrapper.

// SetEventsJournalEnabled forwards durable-journal activation to the provider
// that actually binds it to a transaction (doltSQLProvider.BeginTx). Activation
// is per instance, and the instance that matters is the inner one; the wrapper
// holds no journal state of its own.
//
// It fires no hook and records nothing, which is not an omission: the journal
// is written at the issueops seam inside the mutation's transaction, so every
// write this wrapper already records is journaled by the layer beneath it.

// eventsMaintenanceRunner is issueops.EventsMaintenanceRunner under this file's
// import alias, named once so the forwarder below reads as an ordinary
// capability check.
type eventsMaintenanceRunner = storageissueops.EventsMaintenanceRunner

// RunEventsMaintenanceTx forwards one events-journal retention step to the
// inner provider's transaction machinery. It is maintenance on a dolt_ignored
// table — a prefix delete of records already committed — so there is nothing
// for a hook to describe and no bead a script could be handed.
//
// A provider that cannot maintain the journal fails here rather than silently
// succeeding, matching RunNonTx above: the caller logs it and carries on.
//
// The forwarder is deliberately not the only defense. Auto-prune's resolution
// peels ProviderUnwrapper before asserting this capability, so it reaches the
// inner provider even through a decorator that never declared the method. Both
// halves are cheap and the failure they prevent — retention silently not
// running behind a wrapper — is invisible from the outside.

var (
	_ UnitOfWorkProvider        = (*notifyingProvider)(nil)
	_ MaintenanceProvider       = (*notifyingProvider)(nil)
	_ PoolTuner                 = (*notifyingProvider)(nil)
	_ IssueLifecycleSource      = (*notifyingProvider)(nil)
	_ IssueReaderSource         = (*notifyingProvider)(nil)
	_ IssueClaimerSource        = (*notifyingProvider)(nil)
	_ RelationsSource           = (*notifyingProvider)(nil)
	_ EdgeReaderSource          = (*notifyingProvider)(nil)
	_ BlockingAnnotatorSource   = (*notifyingProvider)(nil)
	_ TreeWalkerSource          = (*notifyingProvider)(nil)
	_ GraphCounterSource        = (*notifyingProvider)(nil)
	_ CounterSource             = (*notifyingProvider)(nil)
	_ ReadyCounterSource        = (*notifyingProvider)(nil)
	_ ReadyClaimerSource        = (*notifyingProvider)(nil)
	_ QuerierSource             = (*notifyingProvider)(nil)
	_ StatsReporterSource       = (*notifyingProvider)(nil)
	_ CycleDetectorSource       = (*notifyingProvider)(nil)
	_ CommenterSource           = (*notifyingProvider)(nil)
	_ BatchCloserSource         = (*notifyingProvider)(nil)
	_ BatchCreatorSource        = (*notifyingProvider)(nil)
	_ DependencyEditorSource    = (*notifyingProvider)(nil)
	_ DeleterSource             = (*notifyingProvider)(nil)
	_ SweeperSource             = (*notifyingProvider)(nil)
	_ ImporterSource            = (*notifyingProvider)(nil)
	_ BootstrapperSource        = (*notifyingProvider)(nil)
	_ InitVerifierSource        = (*notifyingProvider)(nil)
	_ WorkspaceConfigSource     = (*notifyingProvider)(nil)
	_ VersionReconcilerSource   = (*notifyingProvider)(nil)
	_ MemoriesSource            = (*notifyingProvider)(nil)
	_ EventsJournalCursorSource = (*notifyingProvider)(nil)
)
