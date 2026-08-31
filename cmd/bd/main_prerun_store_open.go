package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/hooks"
	"github.com/jonbaldie/beads/internal/molecules"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/storage/schema"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/spf13/cobra"
)

func openPersistentProxiedStore(beadsDir, databaseOverride string, previewMode bool) error {
	p, err := newProxiedServerUOWProvider(getRootContext(), beadsDir, databaseOverride, previewProviderOptions(previewMode)...)
	if err != nil {
		return HandleError("failed to open uow provider: %v", err)
	}
	var uowSinks uow.Sinks
	if beadsDir != "" && !config.GetBool("no-hooks") {
		setHookRunner(hooks.NewRunner(filepath.Join(beadsDir, "hooks")))
		uowSinks.Hook = getHookRunner()
	}
	setUOWProvider(uow.NewNotifyingProvider(p, uowSinks))

	if !previewMode {
		reconcileVersionProxiedServer(getRootContext())
	}

	syncCommandContext()
	return nil
}

func reportPersistentStoreOpenError(err error) error {
	// A failed factory can return a typed-nil concrete pointer, which the
	// interface assignment makes non-nil; gate-release cleanup would then
	// call Close on a nil receiver and panic. No store was opened, so drop it.
	setStore(nil)
	if handleFreshCloneError(err) {
		return SilentExit()
	}
	var skewErr *schema.SchemaSkewError
	if errors.As(err, &skewErr) {
		if isJSONOutput() {
			handleSchemaSkewJSON(skewErr)
		} else {
			fmt.Fprint(os.Stderr, skewErr.UserMessage())
		}
		return SilentExit()
	}
	var gateErr *schema.RemoteMigrateGateError
	if errors.As(err, &gateErr) {
		if isJSONOutput() {
			handleRemoteMigrateGateJSON(gateErr)
		} else {
			fmt.Fprint(os.Stderr, gateErr.UserMessage())
		}
		return SilentExit()
	}
	return HandleError("failed to open database: %v", err)
}

func finalizePersistentOpenedStore(cmd *cobra.Command, args []string, beadsDir string, useReadOnly bool, doltCfg *dolt.Config) error {
	storeMutex.Lock()
	setStoreActive(true)
	storeMutex.Unlock()
	if err := maybePersistentAutoImport(cmd, args, beadsDir, useReadOnly, doltCfg); err != nil {
		return err
	}
	wirePersistentStoreHooks(beadsDir)
	loadPersistentMolecules(cmd, beadsDir)
	syncCommandContext()
	return nil
}

func maybePersistentAutoImport(cmd *cobra.Command, args []string, beadsDir string, useReadOnly bool, doltCfg *dolt.Config) error {
	if shouldRunAutoImportJSONL(cmd, getStore(), useReadOnly, isGlobalFlag(), doltCfg.ServerMode) &&
		!isDisablingImportAutoViaConfigCommand(cmd, args) {
		maybeAutoImportJSONL(getRootContext(), getStore(), beadsDir)
	}
	if !useReadOnly && !isGlobalFlag() && os.Getenv("BEADS_SKIP_IDENTITY_CHECK") != "1" {
		return validateWorkspaceIdentity(getRootContext(), beadsDir)
	}
	return nil
}

func wirePersistentStoreHooks(beadsDir string) {
	if beadsDir != "" {
		setHookRunner(hooks.NewRunner(filepath.Join(beadsDir, "hooks")))
	}
	setStore(wireStorageDecorators(getStore(), getHookRunner(), config.GetBool("no-hooks")))
	warnMultipleDatabases(getDBPath())
}

func loadPersistentMolecules(cmd *cobra.Command, beadsDir string) {
	if cmd.Name() == "import" || getStore() == nil {
		return
	}
	loader := molecules.NewLoader(getStore())
	if result, err := loader.LoadAll(getRootContext(), beadsDir); err != nil {
		debug.Logf("warning: failed to load molecules: %v", err)
	} else if result.Loaded > 0 {
		debug.Logf("loaded %d molecules from %v", result.Loaded, result.Sources)
	}
}
