package main

import (
	"strings"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage/backends"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/spf13/cobra"
)

func openPersistentPreRunStore(cmd *cobra.Command, args []string, dbNameFromDBFlag string) (retErr error) {
	beadsDir := resolveCommandBeadsDir(getDBPath())
	prepareSelectedCommandContext(beadsDir, true)
	refreshBoundCommandConfig(cmd)
	if guardErr := guardLegacyUpgradeWorkspace(beadsDir); guardErr != nil {
		return HandleError("%v", guardErr)
	}

	// Workspace operation gate: every command that reaches this point
	// will open the store (the skipsStoreInit early return is above),
	// so take the workspace + physical-root gates now, in the final
	// mode (SHARED for normal commands, EXCLUSIVE for bd backup
	// restore — there is no upgrade path). See workspace_gate.go for
	// the fail-open/fail-closed posture. The handle is released in
	// PersistentPostRunE after store close; if this PreRunE fails
	// later, cobra never runs PostRunE, so the deferred release below
	// covers the PreRunE error paths after acquisition.
	if err := acquireCommandWorkspaceGates(getRootContext(), cmd, beadsDir); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			// Gate-outlives-store: a PreRunE failure AFTER the store
			// opened (cobra will skip PostRunE) must close the
			// store/provider before the gates drop, or maintenance
			// could start against un-quiesced storage.
			closeStoreBeforeGateRelease()
			releaseWorkspaceGates()
		}
	}()
	if _, err := getDoltAutoCommitMode(); err != nil {
		return HandleError("%v", err)
	}
	return openPersistentStoreAfterGates(cmd, args, beadsDir, dbNameFromDBFlag)
}

func openPersistentStoreAfterGates(cmd *cobra.Command, args []string, beadsDir, dbNameFromDBFlag string) error {
	cfg, err := loadPersistentStoreConfig(beadsDir)
	if err != nil {
		return err
	}
	previewMode, useReadOnly, policy := applyPersistentStoreActorAndPolicy(cmd)
	if err := applyPersistentMigrateGates(cmd, beadsDir, policy, previewMode); err != nil {
		return err
	}
	return openPersistentConfiguredStore(cmd, args, beadsDir, cfg, previewMode, useReadOnly, policy, dbNameFromDBFlag)
}

func openPersistentConfiguredStore(cmd *cobra.Command, args []string, beadsDir string, cfg *configfile.Config, previewMode, useReadOnly bool, policy rootStorePolicy, dbNameFromDBFlag string) error {
	doltCfg, doltPath := newPersistentDoltConfig(cmd, beadsDir, useReadOnly, previewMode, policy)
	cfg, err := applyPersistentDoltMetadata(beadsDir, cfg, doltCfg)
	if err != nil {
		return err
	}
	databaseOverride, err := applyPersistentDatabaseOverride(beadsDir, dbNameFromDBFlag, doltCfg)
	if err != nil {
		return err
	}
	if isProxiedServerMode() {
		return openPersistentProxiedStore(beadsDir, databaseOverride, previewMode)
	}
	return openPersistentEmbeddedStore(cmd, args, beadsDir, cfg, doltCfg, doltPath, useReadOnly)
}

func openPersistentEmbeddedStore(cmd *cobra.Command, args []string, beadsDir string, cfg *configfile.Config, doltCfg *dolt.Config, doltPath string, useReadOnly bool) error {
	// Default auto-commit to ON when the user hasn't set a value, in both
	// modes — "on" names what each mode already does per write:
	// - Embedded mode: each command writes to the working set and commits
	//   it in PersistentPostRun.
	// - Server mode: the storage layer creates one Dolt commit inside each
	//   write transaction (the post-run flush stays embedded-only). The
	//   default here used to be OFF, but the mode was inert in server mode
	//   — every value behaved like ON — so ON is the compatible default
	//   now that batch/off actually defer version commits (bd-4wamg).
	if strings.TrimSpace(getDoltAutoCommit()) == "" {
		setDoltAutoCommit(string(doltAutoCommitOn))
	}

	doltCfg.Path = doltPath

	// WARNING: DO NOT remove, delete, or modify files inside Dolt's .dolt/
	// directory — including noms/LOCK files. These are Dolt-internal files.
	// Removing them WILL cause unrecoverable data corruption and data loss.
	// Dolt manages these files itself; external interference is never safe.

	var err error
	if _, ok := backends.Lookup(cfg.GetBackend()); ok {
		setStore(nil)
		opened, openErr := newRegisteredBackendStore(getRootContext(), cfg.GetBackend(), beadsDir, useReadOnly)
		setStore(opened)
		err = openErr
	} else {
		opened, openErr := newDoltStore(getRootContext(), doltCfg)
		setStore(opened)
		err = openErr
	}

	// Track final read-only state for staleness checks (GH#1089)
	storeIsReadOnly.Store(doltCfg.ReadOnly)

	if err != nil {
		return reportPersistentStoreOpenError(err)
	}
	return finalizePersistentOpenedStore(cmd, args, beadsDir, useReadOnly, doltCfg)
}
