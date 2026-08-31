package main

import (
	"context"
	"math/rand"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/workspacegate"
	"github.com/spf13/cobra"
)

// showCmd is the test-only handle for the command registered by show.go. The
// production command is constructed through showCommand so it does not remain
// mutable package state; tests retain the historical handle for flag probes.
var showCmd *cobra.Command

var (
	actor                        string
	store                        storage.DoltStorage
	versionUpgradeDetected       bool
	previousVersion              string
	upgradeAcknowledged          bool
	commandDidExplicitDoltCommit bool
)

// The production context no longer writes the legacy package variables. Tests
// still exercise older command paths that inspect those variables directly, so
// install a narrow compatibility bridge in the test binary only.
func installLegacyGlobalCallbacks() {
	callbacks := legacyCallbacks()
	callbacks.store = func(value storage.DoltStorage) { store = value }
	callbacks.storeValue = func() storage.DoltStorage { return store }
	callbacks.actor = func(value string) { actor = value }
	callbacks.actorValue = func() string { return actor }
	callbacks.uowProvider = func(value uow.UnitOfWorkProvider) { uowProvider = value }
	callbacks.storeActive = func(value bool) { storeActive = value }
	callbacks.dbPath = func(value string) { dbPath = value }
	callbacks.rootContext = func(value context.Context, cancel context.CancelFunc) {
		rootCtx = value
		rootCancel = cancel
		if cmdCtx != nil {
			cmdCtx.RootCtx = value
			cmdCtx.RootCancel = cancel
		}
	}
	callbacks.workspaceGate = func(value *workspacegate.MultiHandle) { workspaceGateHandle = value }
	callbacks.Tips = func(value []Tip) { tips = value }
	callbacks.TipRand = func(value *rand.Rand) { tipRand = value }
	callbacks.tipMetadata = func(value bool) { commandDidWriteTipMetadata = value }
	callbacks.tipIDs = func(value map[string]struct{}) { commandTipIDsShown = value }
	callbacks.versionState.upgradeDetectedValue = func() bool { return versionUpgradeDetected }
	callbacks.versionState.setUpgradeDetected = func(value bool) { versionUpgradeDetected = value }
	callbacks.versionState.previousValue = func() string { return previousVersion }
	callbacks.versionState.setPrevious = func(value string) { previousVersion = value }
	callbacks.versionState.acknowledgedValue = func() bool { return upgradeAcknowledged }
	callbacks.versionState.setAcknowledged = func(value bool) { upgradeAcknowledged = value }
	callbacks.versionState.explicitCommitValue = func() bool { return commandDidExplicitDoltCommit }
	callbacks.versionState.setExplicitCommit = func(value bool) { commandDidExplicitDoltCommit = value }
	callbacks.mode.commandContext = func(value *CommandContext) { cmdCtx = value }
	callbacks.mode.changeDirEnv = func(value map[string]envSnapshotValue) { changeDirEnvSnapshot = value }
	callbacks.mode.changeDirWD = func(value string) { changeDirOrigWD = value }
	callbacks.mode.jsonOutput = func(value bool) { jsonOutput = value }
	callbacks.mode.readonlyMode = func(value bool) { readonlyMode = value }
	callbacks.mode.doltAutoCommit = func(value string) { doltAutoCommit = value }
	callbacks.mode.serverMode = func(value bool) { serverMode = value }
	callbacks.mode.proxiedMode = func(value bool) { proxiedServerMode = value }
}

// resetCommandContext clears the test-only context override.
func resetCommandContext() {
	cmdCtx = nil
}

// enableTestModeGlobals keeps legacy tests on their explicit global state
// while production command execution uses the closure-owned context.
func enableTestModeGlobals() {
	installLegacyGlobalCallbacks()
	testModeUseGlobals = true
	cmdCtx = nil
	showCmd, _, _ = rootCmd.Find([]string{"show"})
}
