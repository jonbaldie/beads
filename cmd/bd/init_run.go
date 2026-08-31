package main

import (
	"errors"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/spf13/cobra"
)

func runInit(cmd *cobra.Command, _ []string) (retErr error) {
	st := gatherInitFlags(cmd)
	initEvt := metrics.NewCommandEvent("init-" + resolveInitDoltMode(st.proxied.enabled, st.server.sharedServer, st.server.initServerMode))
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(initEvt)
		}
	}()
	if err := validateInitFlagCombos(st); err != nil {
		return err
	}
	if err := resolveInitBackend(st); err != nil {
		return err
	}
	if done, err := maybeRunInitProxied(st); done || err != nil {
		return err
	}
	restore, err := prepareInitServerMode(st)
	if err != nil {
		return err
	}
	defer restore()
	return runInitWorkspace(st)
}

func runInitWorkspace(st *initRunContext) error {
	if err := guardInitExistingWorkspace(st); err != nil {
		if errors.Is(err, errInitSkipped) {
			return nil
		}
		return err
	}
	if err := confirmInitReinit(st); err != nil {
		return err
	}
	if err := setupInitStealthAndPrefix(st); err != nil {
		return err
	}
	if err := checkInitRemoteSafety(st); err != nil {
		return err
	}
	return runInitWorkspaceWrite(st)
}

func runInitWorkspaceWrite(st *initRunContext) error {
	if err := resolveInitPaths(st); err != nil {
		return err
	}
	if err := acquireInitWorkspaceGate(st); err != nil {
		return err
	}
	defer func() { _ = st.gate.Release() }()
	if err := createInitBeadsDir(st); err != nil {
		return err
	}
	if err := ensureInitGitRepo(st); err != nil {
		return err
	}
	if err := ensureInitStorageDir(st); err != nil {
		return err
	}
	return runInitStorage(st)
}

func runInitStorage(st *initRunContext) error {
	if err := resolveInitDatabaseName(st); err != nil {
		return err
	}
	if err := maybeBootstrapInitRemote(st); err != nil {
		return err
	}
	if err := openInitDoltStore(st); err != nil {
		return err
	}
	defer st.resolved.store.lock.Unlock()
	finalizeInitStore(st)
	return writeInitIdentity(initFinalizeFromState(st))
}
