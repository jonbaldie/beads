// Package main provides the CommandContext struct that consolidates runtime state.
// This addresses the code smell of 20+ global variables in main.go by grouping
// related state into a single struct for better testability and clearer ownership.
package main

import (
	"context"
	"math/rand"
	"os"
	"time"

	"github.com/jonbaldie/beads/internal/hooks"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/workspacegate"
)

// CommandContext holds all runtime state for command execution.
// This consolidates the previously scattered global variables for:
// - Better testability (can inject mock contexts)
// - Clearer state ownership (all state in one place)
// - Reduced global count (20+ globals → 1 context)
// - Thread safety (mutexes grouped with the data they protect)
type commandContextConfig struct {
	ChangeDir         string
	DBPath            string
	DatabaseFlag      string
	Actor             string
	SandboxMode       bool
	GlobalFlag        bool
	ReadonlyMode      bool
	IgnoreSchemaSkew  bool
	LockTimeout       time.Duration
	CPUProfileEnabled bool
	MemProfilePath    string
	DoltAutoCommit    string
	commandContextOutput
}

type commandContextOutput struct {
	JSONOutput  bool
	NoColorFlag bool
	Verbose     bool
	Quiet       bool
}

type commandContextMode struct {
	ServerMode        bool
	ProxiedServerMode bool
}

type commandContextRuntime struct {
	Store                      storage.DoltStorage
	UOWProvider                uow.UnitOfWorkProvider
	StoreActive                bool
	RootCtx                    context.Context
	RootCancel                 context.CancelFunc
	HookRunner                 *hooks.Runner
	ChangeDirEnvSnapshot       map[string]envSnapshotValue
	ChangeDirOrigWD            string
	CommandDidExplicitCommit   bool
	CommandDidWriteTipMetadata bool
	CommandTipIDsShown         map[string]struct{}
	WorkspaceGateHandle        *workspacegate.MultiHandle
}

type commandContextVersion struct {
	VersionUpgradeDetected bool
	PreviousVersion        string
	UpgradeAcknowledged    bool
}

type commandContextProfile struct {
	ProfileFile *os.File
	TraceFile   *os.File
}

// persistentFlagValues is kept separately from the per-command context because
// Cobra parses flags before PersistentPreRunE creates that context. The pointer
// is held by the flag package for the lifetime of the process, while each
// command receives a snapshot in initCommandContext.
type persistentFlagValues struct {
	ChangeDir         string
	DBPath            string
	DatabaseFlag      string
	Actor             string
	JSONOutput        bool
	NoColorFlag       bool
	SandboxMode       bool
	GlobalFlag        bool
	ReadonlyMode      bool
	IgnoreSchemaSkew  bool
	CPUProfileEnabled bool
	MemProfilePath    string
	Verbose           bool
	Quiet             bool
	DoltAutoCommit    string
}

var persistentFlags = func() func() *persistentFlagValues {
	var values persistentFlagValues
	return func() *persistentFlagValues { return &values }
}()

// commandContextState owns the process-level command state without exposing a
// mutable package variable. A command is executed serially, so a single
// context is sufficient; tests can still install the legacy context pointer
// when they explicitly exercise that compatibility path.
var commandContextState = func() func() *CommandContext {
	state := &CommandContext{}
	return func() *CommandContext { return state }
}()

// legacyStateCallbacks are installed by the test package so setters continue
// to keep the legacy test variables in sync. Production has no callbacks, so
// the compatibility bridge is zero-cost and does not mutate package globals.
type legacyStateCallbacks struct {
	storeValue    func() storage.DoltStorage
	store         func(storage.DoltStorage)
	actorValue    func() string
	actor         func(string)
	uowProvider   func(uow.UnitOfWorkProvider)
	storeActive   func(bool)
	dbPath        func(string)
	rootContext   func(context.Context, context.CancelFunc)
	workspaceGate func(*workspacegate.MultiHandle)
	Tips          func([]Tip)
	TipRand       func(*rand.Rand)
	tipMetadata   func(bool)
	tipIDs        func(map[string]struct{})
	versionState  legacyVersionCallbacks
	mode          legacyModeCallbacks
}

// legacyVersionCallbacks let the test binary keep its historical package
// variables in sync without requiring production code to define mutable
// process-global version state.
type legacyVersionCallbacks struct {
	upgradeDetectedValue func() bool
	setUpgradeDetected   func(bool)
	previousValue        func() string
	setPrevious          func(string)
	acknowledgedValue    func() bool
	setAcknowledged      func(bool)
	explicitCommitValue  func() bool
	setExplicitCommit    func(bool)
}

type legacyModeCallbacks struct {
	commandContext func(*CommandContext)
	changeDirEnv   func(map[string]envSnapshotValue)
	changeDirWD    func(string)
	jsonOutput     func(bool)
	readonlyMode   func(bool)
	doltAutoCommit func(string)
	serverMode     func(bool)
	proxiedMode    func(bool)
}

var legacyCallbacks = func() func() *legacyStateCallbacks {
	callbacks := &legacyStateCallbacks{}
	return func() *legacyStateCallbacks { return callbacks }
}()

// CommandContext holds all runtime state for command execution.
type CommandContext struct {
	commandContextConfig
	commandContextMode
	commandContextRuntime
	commandContextVersion
	commandContextProfile
}

// cmdCtx is the global CommandContext instance.
// Commands access state through this single point instead of scattered globals.
var cmdCtx = commandContextState()

// testModeUseGlobals when true forces accessor functions to use legacy globals.
// This ensures backward compatibility with tests that manipulate globals directly.
var testModeUseGlobals bool

// initCommandContext creates and initializes a new CommandContext.
// Called from PersistentPreRun to set up runtime state.
func initCommandContext() {
	ctx := commandContext()
	*ctx = CommandContext{
		commandContextConfig: commandContextConfig{
			ChangeDir:         persistentFlags().ChangeDir,
			DBPath:            persistentFlags().DBPath,
			DatabaseFlag:      persistentFlags().DatabaseFlag,
			Actor:             persistentFlags().Actor,
			SandboxMode:       persistentFlags().SandboxMode,
			GlobalFlag:        persistentFlags().GlobalFlag,
			ReadonlyMode:      persistentFlags().ReadonlyMode,
			IgnoreSchemaSkew:  persistentFlags().IgnoreSchemaSkew,
			LockTimeout:       lockTimeout,
			CPUProfileEnabled: persistentFlags().CPUProfileEnabled,
			MemProfilePath:    persistentFlags().MemProfilePath,
			DoltAutoCommit:    persistentFlags().DoltAutoCommit,
			commandContextOutput: commandContextOutput{
				JSONOutput:  persistentFlags().JSONOutput,
				NoColorFlag: persistentFlags().NoColorFlag,
				Verbose:     persistentFlags().Verbose,
				Quiet:       persistentFlags().Quiet,
			},
		},
		commandContextMode:    commandContextMode{},
		commandContextRuntime: commandContextRuntime{},
		commandContextVersion: commandContextVersion{},
		commandContextProfile: commandContextProfile{},
	}
	if callback := legacyCallbacks().mode.commandContext; callback != nil {
		callback(ctx)
	}
}

// GetCommandContext returns the current CommandContext.
// Returns nil if called before initialization (e.g., during init() or help).
func GetCommandContext() *CommandContext {
	return commandContext()
}

// commandContext returns the active execution context. The raw cmdCtx pointer
// is a test-only compatibility override; normal command execution uses the
// closure-owned state above.
func commandContext() *CommandContext {
	if !testModeUseGlobals && cmdCtx != nil {
		return cmdCtx
	}
	return commandContextState()
}

// shouldUseGlobals returns true if accessor functions should use globals.
func shouldUseGlobals() bool {
	return testModeUseGlobals
}

// The following accessor functions provide backward-compatible access
// to the CommandContext fields. Commands can use these during the
// migration period, and they can be gradually replaced with direct
// cmdCtx access as files are updated.

// getStore returns the current storage backend.
// This is the primary way commands should access storage.
func getStore() storage.DoltStorage {
	if shouldUseGlobals() {
		if callback := legacyCallbacks().storeValue; callback != nil {
			return callback()
		}
		return nil
	}
	return commandContext().Store
}

func getChangeDir() string {
	if shouldUseGlobals() {
		return changeDir
	}
	return commandContext().ChangeDir
}

func setChangeDir(value string) {
	commandContext().ChangeDir = value
	persistentFlags().ChangeDir = value
}

func getDatabaseFlag() string {
	if shouldUseGlobals() {
		return databaseFlag
	}
	return commandContext().DatabaseFlag
}

func setDatabaseFlag(value string) {
	commandContext().DatabaseFlag = value
	persistentFlags().DatabaseFlag = value
}

func getChangeDirEnvSnapshot() map[string]envSnapshotValue {
	if shouldUseGlobals() {
		return changeDirEnvSnapshot
	}
	return commandContext().ChangeDirEnvSnapshot
}

func setChangeDirEnvSnapshot(snapshot map[string]envSnapshotValue) {
	commandContext().ChangeDirEnvSnapshot = snapshot
	if callback := legacyCallbacks().mode.changeDirEnv; callback != nil {
		callback(snapshot)
	}
}

func getChangeDirOrigWD() string {
	if shouldUseGlobals() {
		return changeDirOrigWD
	}
	return commandContext().ChangeDirOrigWD
}

func setChangeDirOrigWD(value string) {
	commandContext().ChangeDirOrigWD = value
	if callback := legacyCallbacks().mode.changeDirWD; callback != nil {
		callback(value)
	}
}

// setStore updates the storage backend in the CommandContext.
func setStore(s storage.DoltStorage) {
	commandContext().Store = s
	if callback := legacyCallbacks().store; callback != nil {
		callback(s)
	}
}

func getUOWProvider() uow.UnitOfWorkProvider {
	if shouldUseGlobals() {
		return uowProvider
	}
	return commandContext().UOWProvider
}

func setUOWProvider(provider uow.UnitOfWorkProvider) {
	commandContext().UOWProvider = provider
	if callback := legacyCallbacks().uowProvider; callback != nil {
		callback(provider)
	}
}

// getActor returns the current actor name for audit trail.
func getActor() string {
	if shouldUseGlobals() {
		if callback := legacyCallbacks().actorValue; callback != nil {
			return callback()
		}
		return ""
	}
	return commandContext().Actor
}

// setActor updates the actor name in the CommandContext.
func setActor(a string) {
	commandContext().Actor = a
	persistentFlags().Actor = a
	if callback := legacyCallbacks().actor; callback != nil {
		callback(a)
	}
}

// isJSONOutput returns true if JSON output mode is enabled.
func isJSONOutput() bool {
	if shouldUseGlobals() {
		return jsonOutput
	}
	return commandContext().JSONOutput
}

// setJSONOutput updates the JSON output flag.
func setJSONOutput(j bool) {
	commandContext().JSONOutput = j
	persistentFlags().JSONOutput = j
	if callback := legacyCallbacks().mode.jsonOutput; callback != nil {
		callback(j)
	}
}

// getDBPath returns the database path.
func getDBPath() string {
	if shouldUseGlobals() {
		return dbPath
	}
	return commandContext().DBPath
}

// setDBPath updates the database path.
func setDBPath(p string) {
	commandContext().DBPath = p
	if callback := legacyCallbacks().dbPath; callback != nil {
		callback(p)
	}
	persistentFlags().DBPath = p
}

// getRootContext returns the signal-aware root context.
// Returns context.Background() if the root context is nil (e.g., before CLI initialization).
func getRootContext() context.Context {
	var ctx context.Context
	if shouldUseGlobals() {
		ctx = rootCtx
	} else {
		ctx = commandContext().RootCtx
	}
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// setRootContext updates the root context and cancel function.
func setRootContext(ctx context.Context, cancel context.CancelFunc) {
	commandContext().RootCtx = ctx
	commandContext().RootCancel = cancel
	if callback := legacyCallbacks().rootContext; callback != nil {
		callback(ctx, cancel)
	}
}

func getRootCancel() context.CancelFunc {
	if shouldUseGlobals() {
		return rootCancel
	}
	return commandContext().RootCancel
}

// getHookRunner returns the hook runner instance.
func getHookRunner() *hooks.Runner {
	if shouldUseGlobals() {
		return hookRunner
	}
	return commandContext().HookRunner
}

// setHookRunner updates the hook runner.
func setHookRunner(h *hooks.Runner) {
	commandContext().HookRunner = h
}

// isReadonlyMode returns true if read-only mode is enabled.
func isReadonlyMode() bool {
	if shouldUseGlobals() {
		return readonlyMode
	}
	return commandContext().ReadonlyMode
}

func setReadonlyMode(value bool) {
	commandContext().ReadonlyMode = value
	persistentFlags().ReadonlyMode = value
	if callback := legacyCallbacks().mode.readonlyMode; callback != nil {
		callback(value)
	}
}

func isNoColorFlag() bool {
	if shouldUseGlobals() {
		return noColorFlag
	}
	return commandContext().NoColorFlag
}

func setNoColorFlag(value bool) {
	commandContext().NoColorFlag = value
	persistentFlags().NoColorFlag = value
}

func isGlobalFlag() bool {
	if shouldUseGlobals() {
		return globalFlag
	}
	return commandContext().GlobalFlag
}

func setGlobalFlag(value bool) {
	commandContext().GlobalFlag = value
	persistentFlags().GlobalFlag = value
}

func isIgnoreSchemaSkew() bool {
	if shouldUseGlobals() {
		return ignoreSchemaSkew
	}
	return commandContext().IgnoreSchemaSkew
}

func setIgnoreSchemaSkew(value bool) {
	commandContext().IgnoreSchemaSkew = value
	persistentFlags().IgnoreSchemaSkew = value
}

func isCPUProfileEnabled() bool {
	if shouldUseGlobals() {
		return cpuProfileEnabled
	}
	return commandContext().CPUProfileEnabled
}

func setCPUProfileEnabled(value bool) {
	commandContext().CPUProfileEnabled = value
	persistentFlags().CPUProfileEnabled = value
}

// getLockTimeout returns the SQLite lock timeout.
func getLockTimeout() time.Duration {
	if shouldUseGlobals() {
		return lockTimeout
	}
	return commandContext().LockTimeout
}

// lockStore acquires the store mutex for thread-safe access.
func lockStore() {
	storeMutex.Lock()
}

// unlockStore releases the store mutex.
func unlockStore() {
	storeMutex.Unlock()
}

// isStoreActive returns true if the store is currently available.
func isStoreActive() bool {
	if shouldUseGlobals() {
		return storeActive
	}
	return commandContext().StoreActive
}

// setStoreActive updates the store active flag.
func setStoreActive(active bool) {
	commandContext().StoreActive = active
	if callback := legacyCallbacks().storeActive; callback != nil {
		callback(active)
	}
}

// isVerbose returns true if verbose mode is enabled.
func isVerbose() bool {
	if shouldUseGlobals() {
		return verboseFlag
	}
	return commandContext().Verbose
}

// isQuiet returns true if quiet mode is enabled.
func isQuiet() bool {
	if shouldUseGlobals() {
		return quietFlag
	}
	return commandContext().Quiet
}

// isSandboxMode returns true if sandbox mode is enabled.
func isSandboxMode() bool {
	if shouldUseGlobals() {
		return sandboxMode
	}
	return commandContext().SandboxMode
}

// setSandboxMode updates the sandbox mode flag.
func setSandboxMode(sm bool) {
	commandContext().SandboxMode = sm
	persistentFlags().SandboxMode = sm
}

// isVersionUpgradeDetected returns true if a version upgrade was detected.
func isVersionUpgradeDetected() bool {
	if shouldUseGlobals() {
		if callback := legacyCallbacks().versionState.upgradeDetectedValue; callback != nil {
			return callback()
		}
	}
	return commandContext().VersionUpgradeDetected
}

// setVersionUpgradeDetected updates the version upgrade detected flag.
func setVersionUpgradeDetected(detected bool) {
	commandContext().VersionUpgradeDetected = detected
	if shouldUseGlobals() {
		if callback := legacyCallbacks().versionState.setUpgradeDetected; callback != nil {
			callback(detected)
		}
	}
}

// getPreviousVersion returns the previous bd version.
func getPreviousVersion() string {
	if shouldUseGlobals() {
		if callback := legacyCallbacks().versionState.previousValue; callback != nil {
			return callback()
		}
	}
	return commandContext().PreviousVersion
}

// setPreviousVersion updates the previous version.
func setPreviousVersion(v string) {
	commandContext().PreviousVersion = v
	if shouldUseGlobals() {
		if callback := legacyCallbacks().versionState.setPrevious; callback != nil {
			callback(v)
		}
	}
}

// isUpgradeAcknowledged returns true if the upgrade notification was shown.
func isUpgradeAcknowledged() bool {
	if shouldUseGlobals() {
		if callback := legacyCallbacks().versionState.acknowledgedValue; callback != nil {
			return callback()
		}
	}
	return commandContext().UpgradeAcknowledged
}

// setUpgradeAcknowledged updates the upgrade acknowledged flag.
func setUpgradeAcknowledged(ack bool) {
	commandContext().UpgradeAcknowledged = ack
	if shouldUseGlobals() {
		if callback := legacyCallbacks().versionState.setAcknowledged; callback != nil {
			callback(ack)
		}
	}
}

// getProfileFile returns the CPU profile file handle.
func getProfileFile() *os.File {
	if shouldUseGlobals() {
		return profileFile
	}
	return commandContext().ProfileFile
}

// setProfileFile updates the CPU profile file handle.
func setProfileFile(f *os.File) {
	commandContext().ProfileFile = f
}

// getTraceFile returns the trace file handle.
func getTraceFile() *os.File {
	if shouldUseGlobals() {
		return traceFile
	}
	return commandContext().TraceFile
}

// setTraceFile updates the trace file handle.
func setTraceFile(f *os.File) {
	commandContext().TraceFile = f
}

func getMemProfilePath() string {
	if shouldUseGlobals() {
		return memProfilePath
	}
	return commandContext().MemProfilePath
}

func setMemProfilePath(value string) {
	commandContext().MemProfilePath = value
	persistentFlags().MemProfilePath = value
}

func getDoltAutoCommit() string {
	if shouldUseGlobals() {
		return doltAutoCommit
	}
	return commandContext().DoltAutoCommit
}

func setDoltAutoCommit(value string) {
	commandContext().DoltAutoCommit = value
	persistentFlags().DoltAutoCommit = value
	if callback := legacyCallbacks().mode.doltAutoCommit; callback != nil {
		callback(value)
	}
}

func isCommandDidExplicitDoltCommit() bool {
	if shouldUseGlobals() {
		if callback := legacyCallbacks().versionState.explicitCommitValue; callback != nil {
			return callback()
		}
	}
	return commandContext().CommandDidExplicitCommit
}

func setCommandDidExplicitDoltCommit(value bool) {
	commandContext().CommandDidExplicitCommit = value
	if shouldUseGlobals() {
		if callback := legacyCallbacks().versionState.setExplicitCommit; callback != nil {
			callback(value)
		}
	}
}

func isCommandDidWriteTipMetadata() bool {
	if shouldUseGlobals() {
		return commandDidWriteTipMetadata
	}
	return commandContext().CommandDidWriteTipMetadata
}

func setCommandDidWriteTipMetadata(value bool) {
	commandContext().CommandDidWriteTipMetadata = value
	if callback := legacyCallbacks().tipMetadata; callback != nil {
		callback(value)
	}
}

func getCommandTipIDsShown() map[string]struct{} {
	if shouldUseGlobals() {
		return commandTipIDsShown
	}
	return commandContext().CommandTipIDsShown
}

func setCommandTipIDsShown(ids map[string]struct{}) {
	commandContext().CommandTipIDsShown = ids
	if callback := legacyCallbacks().tipIDs; callback != nil {
		callback(ids)
	}
}

func getWorkspaceGateHandle() *workspacegate.MultiHandle {
	if shouldUseGlobals() {
		return workspaceGateHandle
	}
	return commandContext().WorkspaceGateHandle
}

func setWorkspaceGateHandle(handle *workspacegate.MultiHandle) {
	commandContext().WorkspaceGateHandle = handle
	if callback := legacyCallbacks().workspaceGate; callback != nil {
		callback(handle)
	}
}

func isServerMode() bool {
	if shouldUseGlobals() {
		return serverMode
	}
	return commandContext().ServerMode
}

func setServerMode(value bool) {
	commandContext().ServerMode = value
	if callback := legacyCallbacks().mode.serverMode; callback != nil {
		callback(value)
	}
}

func isProxiedServerMode() bool {
	if shouldUseGlobals() {
		return proxiedServerMode
	}
	return commandContext().ProxiedServerMode
}

func setProxiedServerMode(value bool) {
	commandContext().ProxiedServerMode = value
	if callback := legacyCallbacks().mode.proxiedMode; callback != nil {
		callback(value)
	}
}

// syncCommandContext copies all legacy global values to the CommandContext.
// This is called after initialization is complete to ensure cmdCtx has all values.
func syncCommandContext() {
	// State is updated through the accessors as each phase completes. This
	// function remains as a compatibility hook for callers from older command
	// paths, but it intentionally does not copy raw package variables into the
	// active context.
}
