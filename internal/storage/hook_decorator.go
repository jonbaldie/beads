// Package storage — hook_decorator.go
//
// HookFiringStore is a decorator around DoltStorage that automatically
// fires on_create/on_update/on_close hooks after successful mutations.
// This moves hook responsibility from individual CLI commands into the
// storage layer, ensuring ALL mutations fire hooks — including future
// commands that haven't been written yet.
//
// Usage:
//
//	store = storage.NewHookFiringStore(rawStore, hookRunner)
//
// Transaction support: mutations inside RunInTransaction are tracked
// and hooks fire only after the transaction commits successfully.
// If the transaction rolls back, no hooks fire.
package storage

import (
	"github.com/jonbaldie/beads/internal/hooks"
	"github.com/jonbaldie/beads/internal/types"
)

// HookFiringStore wraps a DoltStorage and fires hooks after mutations.
// Non-mutation methods pass through to the inner store unchanged.
type HookFiringStore struct {
	DoltStorage             // embed for passthrough of non-overridden methods
	inner       DoltStorage // the real store
	runner      hookRunner
}

type hookRunner interface {
	Run(event string, issue *types.Issue)
}

// NewHookFiringStore wraps store with automatic hook firing.
// If runner is nil, hooks are silently skipped (passthrough only).
func NewHookFiringStore(store DoltStorage, runner *hooks.Runner) *HookFiringStore {
	var r hookRunner
	if runner != nil {
		r = runner
	}
	return &HookFiringStore{
		DoltStorage: store,
		inner:       store,
		runner:      r,
	}
}

// Unwrapper is implemented by storage decorators that wrap an inner
// DoltStorage. UnwrapStore uses this to peel arbitrary chains of
// decorators (e.g. HookFiringStore wrapping telemetry.InstrumentedStorage
// wrapping the concrete dolt store) so type assertions to optional
// interfaces (StoreLocator, BackupStore, etc.) reach the concrete store.
type Unwrapper interface {
	Unwrap() DoltStorage
}

// Unwrap returns the underlying store, satisfying Unwrapper.
func (h *HookFiringStore) Unwrap() DoltStorage { return h.inner }

// RoleFiresHooks reports whether an issue role taken off a store carries this
// decorator's hook layer — that is, whether a landed write through it runs the
// workspace's user hook scripts.
//
// It is the question a caller cannot answer from the role's own type, and the
// one a NETWORK server has to answer before it serves anything: `bd serve`
// documents that hooks do not fire (a user-controlled subprocess per mutation
// is an unbounded latency multiplier and an orphaned child at shutdown), and
// the accessors on a decorated store return decorated roles by design —
// IssueClaimer recurses precisely so a CLI claim keeps its on_update. So the
// exact value the obvious `store.IssueClaimer()` produces on bd's own storage
// chain is the one value that server must refuse, and this is what lets it.
//
// A decorator built with a nil runner still answers true. Whether hooks fire
// on this instant's configuration is not the property: the type's contract is
// to fire them, and a server that admitted it would be one config change away
// from breaking its own.
//
// THE SET IS ENFORCED, and it is enforced because saying "every case that
// exists today is in the switch" did not make it so. That sentence stood here
// while FOUR decorators were missing — the commenter, the ready claimer, the
// batch closer and the batch creator — each of them found by hand by whoever
// happened to publish an operation over that role, which is a discovery process
// and not a guarantee. TestRoleFiresHooksKnowsEveryHookFiringRole now scans this
// package's source for every decorator that holds an issueOperationHooks and
// requires a case here that RETURNS TRUE for it, so the fifth cannot repeat the
// first four. It counts the return rather than the case label because an empty
// case answers false, which is how *hookMetadataCAS spent time silently
// disarmed.
//
// WHAT IS STILL NOT ENFORCED, narrowly and deliberately: the assertion is on the
// TYPE, so a role this decorator produced and something ELSE then wrapped is
// invisible here, and so is a future decorator in another package that fires
// hooks of its own. Both are outside what a scan of this package can see.
func RoleFiresHooks(role any) bool {
	switch role.(type) {
	case *hookIssueClaimer, *hookIssueOperations, *hookDependencyEditor, *hookReleaser,
		*hookMetadataCAS, *hookBatchApplier, *hookCommenter, *hookReadyClaimer, *hookBatchCloser,
		*hookBatchCreator:
		return true
	}
	return false
}

// UnwrapStore peels back any chain of Unwrapper decorators and returns
// the innermost store. Use this before type-asserting to optional
// interfaces (StoreLocator, BackupStore, Flattener, RawDBAccessor, etc.)
// so the assertion reaches the concrete store rather than a decorator.
func UnwrapStore(s DoltStorage) DoltStorage {
	for {
		u, ok := s.(Unwrapper)
		if !ok {
			return s
		}
		s = u.Unwrap()
	}
}
