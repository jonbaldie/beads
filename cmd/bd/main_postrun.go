package main

import (
	"github.com/spf13/cobra"
)

func runPersistentPostRun(cmd *cobra.Command, _ []string) error {
	// Registered FIRST so it runs LAST: the signal context must outlive
	// the store/gate cleanup below, which passes rootCtx to
	// uowProvider.Close. Canceling in the function body (as this used
	// to) handed those closers a dead context on the way out.
	//
	// Clearing matters as much as canceling. Leaving rootCtx pointing at
	// the context we just canceled is harmless when a real bd process
	// exits here, but every in-process caller that runs Execute() more
	// than once -- the cmd/bd test binary, library embedders -- would
	// hand that dead context to the next command, and anything reading
	// it refuses work nobody canceled. nil is the documented "no process
	// signal context yet" state and normalizes back to Background().
	//
	// Deferred rather than inline so the early error returns below clear
	// the globals too.
	defer func() {
		if getRootCancel() != nil {
			getRootCancel()()
		}
		setRootContext(nil, nil)
	}()
	defer restoreChangeDirSelection()
	// Give the hooks this command fired their moment before the process
	// exits. Both plumbings run them fire-and-forget on their own
	// goroutines, and a bd command is short enough that returning from main
	// can kill one that has not reached exec yet — a hook that silently did
	// not fire, which is the failure this whole seam exists to stop.
	// Bounded by the runner's own per-hook budget: a script that outlives it
	// is being killed anyway, so waiting longer buys nothing.
	//
	// Deferred order is load-bearing on both sides. It runs AFTER the
	// close-and-release below, because a hook script commonly shells out to
	// bd and an EMBEDDED workspace's Dolt lock is held until this process
	// closes its store — the child would fail to open it. (The workspace
	// gates are not the reason: a normal command holds them SHARED, and the
	// child takes them shared too, so those never contend.) It runs BEFORE
	// restoreChangeDirSelection above, because under `-C` the child inherits
	// this process's environment and must see the workspace the command
	// actually ran against.
	defer waitForCommandHooks()
	// Release the workspace/physical-root gates on EVERY exit from
	// PostRunE — deferred so the early error returns below cannot leak
	// the handle past the function. Ordering is enforced, not assumed:
	// the success path closes uowProvider/store itself (and nils them),
	// making the close call here a no-op; on the early error returns
	// the store is still open, so it is closed HERE, before the gates
	// drop — gates must always outlive the store.
	defer func() {
		closeStoreBeforeGateRelease()
		releaseWorkspaceGates()
	}()

	if proxiedServerMode {
		finishPersistentProxiedPostRun(cmd)
	} else if err := finishPersistentEmbeddedPostRun(cmd); err != nil {
		return err
	}
	shutdownPersistentTelemetryAndProfiles()
	return nil
}
