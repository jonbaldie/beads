package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/util"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/storage/schema"
	"github.com/jonbaldie/beads/internal/ui"
	"golang.org/x/term"
)

type initRunResolvedRemote struct {
	syncURL                                                 string
	syncFromRemote, syncURLFromConfig, syncURLFromGitOrigin bool
	bootstrappedFromRemote, divergenceConfirmed             bool
}

type initRunResolvedStore struct {
	ctx     context.Context
	doltCfg *dolt.Config
	store   storage.DoltStorage
	lock    util.Unlocker
}

func maybeBootstrapInitRemote(st *initRunContext) error {
	url, source := resolveInitConfiguredSyncRemote(st.ident.initRemote, st.ident.initRemoteChanged, resolveSyncRemote)
	st.resolved.remote.syncURL = url
	st.resolved.remote.syncURLFromConfig = url != "" && source != initSyncRemoteNone
	if url != "" {
		if err := bootstrapInitConfiguredRemote(st, source); err != nil {
			return err
		}
	} else if err := bootstrapInitOriginRemote(st); err != nil {
		return err
	}
	return cloneInitRemoteIfNeeded(st)
}

func bootstrapInitConfiguredRemote(st *initRunContext, source initSyncRemoteSource) error {
	syncURL := st.resolved.remote.syncURL
	if source == initSyncRemoteExplicit && !st.safety.fromJSONL {
		st.resolved.remote.syncFromRemote = true
		return nil
	}
	if source != initSyncRemoteConfigured && source != initSyncRemoteExplicit {
		return nil
	}
	hasData, err := gitRemoteHasDoltDataRefStatus(syncURL)
	remoteHasDoltData, lateProbeNote := resolveRemoteHasDoltDataProbe(syncURL, hasData, err)
	if lateProbeNote != "" {
		fmt.Fprintf(os.Stderr, "%s %s\n", ui.RenderWarn("!"), lateProbeNote)
	}
	if st.resolved.remote.syncFromRemote {
		return nil
	}
	return applyInitBootstrapSafety(st, syncURL, remoteHasDoltData, func() (bool, error) {
		return gitRemoteHasDoltDataRefStatus(syncURL)
	})
}

func applyInitBootstrapSafety(st *initRunContext, syncURL string, hasData bool, probe func() (bool, error)) error {
	decision := CheckRemoteSafety(initBootstrapSafetyInput(st, hasData))
	bootstrap, err := handleRemoteSafetyDecision(decision, st.ident.prefix, syncURL, st.safety.destroyToken, probe, hasData, &st.resolved.remote.divergenceConfirmed)
	if err != nil {
		return err
	}
	st.resolved.remote.syncFromRemote = bootstrap
	if !bootstrap && decision.Action == ActionNoRemoteData && !st.safety.quiet {
		fmt.Printf("  %s Remote has no Dolt data yet; initialized a fresh local database\n", ui.RenderWarn("!"))
	}
	return nil
}

func initBootstrapSafetyInput(st *initRunContext, hasData bool) RemoteSafetyInput {
	return RemoteSafetyInput{
		Force:             st.safety.force,
		ReinitLocal:       st.safety.reinitLocal,
		FromJSONL:         st.safety.fromJSONL,
		DiscardRemote:     st.safety.discardRemote,
		DestroyToken:      st.safety.destroyToken,
		ExpectedToken:     FormatDestroyToken(st.ident.prefix),
		RemoteHasDoltData: hasData,
		IsInteractive:     term.IsTerminal(int(os.Stdin.Fd())),
	}
}

func bootstrapInitOriginRemote(st *initRunContext) error {
	if st.safety.stealth || !isGitRepo() || isBareGitRepo() {
		return nil
	}
	originURL, err := gitOriginGetURL()
	if err != nil || originURL == "" {
		return nil
	}
	st.resolved.remote.syncURL = normalizeRemoteURL(originURL)
	st.resolved.remote.syncURLFromGitOrigin = true
	hasData, probeErr := gitOriginHasDoltDataRefStatus()
	remoteHasDoltData, lateProbeNote := resolveRemoteHasDoltDataProbe(st.resolved.remote.syncURL, hasData, probeErr)
	if lateProbeNote != "" {
		fmt.Fprintf(os.Stderr, "%s %s\n", ui.RenderWarn("!"), lateProbeNote)
	}
	decision := CheckRemoteSafety(initBootstrapSafetyInput(st, remoteHasDoltData))
	bootstrap, err := handleRemoteSafetyDecision(decision, st.ident.prefix, st.resolved.remote.syncURL, st.safety.destroyToken, gitOriginHasDoltDataRefStatus, remoteHasDoltData, &st.resolved.remote.divergenceConfirmed)
	if err != nil {
		return err
	}
	return applyInitOriginBootstrap(st, bootstrap, probeErr)
}

func applyInitOriginBootstrap(st *initRunContext, bootstrap bool, probeErr error) error {
	if !bootstrap {
		return nil
	}
	if probeErr != nil {
		if !st.safety.quiet {
			fmt.Printf("  %s Could not verify the git origin's Dolt history; initialized a fresh local database\n", ui.RenderWarn("!"))
		}
		return nil
	}
	st.resolved.remote.syncFromRemote = true
	return nil
}

func cloneInitRemoteIfNeeded(st *initRunContext) error {
	if !st.resolved.remote.syncFromRemote {
		return nil
	}
	syncURL := st.resolved.remote.syncURL
	cloneCfg := initTimeCloneConfig(st.server.initServerMode, st.server.host, st.server.port, st.server.socket, st.server.user, st.resolved.paths.dbName)
	disposition, err := runInitRemoteClone(syncURL, func(remoteURL string) error {
		return cloneFromRemoteWithMode(getRootContext(), st.resolved.paths.beadsDir, remoteURL, st.resolved.paths.dbName, cloneCfg, initRemoteCloneMode(st.server.initServerMode, st.server.externalServer))
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to clone remote %q: %v\n", syncURL, err)
		fmt.Fprintf(os.Stderr, "Hint: verify the URL is reachable and any credentials are valid, or omit --remote to initialize a fresh local database.\n")
		return &exitError{Code: 1}
	}
	return recordInitRemoteClone(st, disposition)
}

func recordInitRemoteClone(st *initRunContext, disposition initRemoteCloneDisposition) error {
	if disposition == initRemoteCloneFresh {
		if !st.safety.quiet {
			fmt.Printf("  %s Remote has no Dolt data yet; initialized a fresh local database\n", ui.RenderWarn("!"))
		}
		st.resolved.remote.syncFromRemote = false
		return nil
	}
	st.resolved.remote.bootstrappedFromRemote = true
	if !st.safety.quiet {
		fmt.Printf("  %s Bootstrapped from remote: %s\n", ui.RenderPass("✓"), st.resolved.remote.syncURL)
	}
	return nil
}

func openInitDoltStore(st *initRunContext) error {
	st.resolved.store.ctx = getRootContext()
	st.resolved.store.doltCfg = buildInitDoltConfig(st)
	if err := applyInitGatewayCredential(st.resolved.store.ctx, st.resolved.paths.beadsDir, st.resolved.store.doltCfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: resolving dolt credential command: %v\n", err)
		return &exitError{Code: 1}
	}
	lock, err := acquireEmbeddedLock(st.resolved.paths.beadsDir, st.server.initServerMode || st.proxied.enabled)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return &exitError{Code: 1}
	}
	st.resolved.store.lock = lock
	if err := startInitSharedServer(st); err != nil {
		lock.Unlock()
		st.resolved.store.lock = nil
		return err
	}
	if err := connectInitDoltStore(st); err != nil {
		lock.Unlock()
		st.resolved.store.lock = nil
		return err
	}
	return nil
}

func buildInitDoltConfig(st *initRunContext) *dolt.Config {
	initPort := resolveInitDoltPort(st.resolved.paths.beadsDir)
	cfg := &dolt.Config{
		Path:     st.resolved.paths.storagePath,
		BeadsDir: st.resolved.paths.beadsDir,
		Database: st.resolved.paths.dbName,
		ServerOptions: dolt.ServerOptions{
			ServerPort:    initPort,
			ServerMode:    st.server.initServerMode,
			ProxiedServer: st.proxied.enabled,
			AutoStart:     st.server.initServerMode && os.Getenv("BEADS_DOLT_AUTO_START") != "0",
			ServerTLS:     initDoltServerTLSFromEnv(),
		},
		RemoteOptions: dolt.RemoteOptions{
			CreateIfMissing: true,
		},
	}
	applyInitDoltServerFlags(cfg, st.server)
	return cfg
}

func resolveInitDoltPort(beadsDir string) int {
	if p := os.Getenv("BEADS_DOLT_SERVER_PORT"); p != "" {
		if port, err := strconv.Atoi(p); err == nil && port > 0 {
			return port
		}
	}
	if port := doltserver.ReadPortFile(beadsDir); port != 0 {
		return port
	}
	if doltserver.IsSharedServerMode() {
		return doltserver.DefaultSharedServerPort
	}
	return 0
}

func applyInitDoltServerFlags(cfg *dolt.Config, server initRunServerFlags) {
	if server.host != "" {
		cfg.ServerHost = server.host
	}
	if server.port != 0 {
		cfg.ServerPort = server.port
	}
	if server.socket != "" {
		cfg.ServerSocket = server.socket
	}
	if server.user != "" {
		cfg.ServerUser = server.user
	}
}

func startInitSharedServer(st *initRunContext) error {
	doltCfg := st.resolved.store.doltCfg
	if st.server.externalServer || !shouldInitSharedGlobalDB(st.server.sharedServer, doltserver.IsSharedServerMode(), doltCfg.Gateway) {
		return nil
	}
	sharedDir, err := doltserver.SharedServerDir()
	if err != nil {
		return nil
	}
	if err := ensureInitSharedServerRunning(st, sharedDir); err != nil {
		return err
	}
	return ensureInitGlobalDatabase(st)
}

func ensureInitSharedServerRunning(st *initRunContext, sharedDir string) error {
	state, _ := doltserver.IsRunning(sharedDir)
	if state != nil && state.Running {
		if st.server.debugMode {
			fmt.Fprintf(os.Stderr, "Warning: shared Dolt server (PID %d, port %d) is already running without debug flags.\n", state.PID, state.Port)
			fmt.Fprintf(os.Stderr, "  Restart to pick up debug mode:\n")
			fmt.Fprintf(os.Stderr, "    bd dolt stop && bd dolt start\n")
		}
		return nil
	}
	if _, startErr := doltserver.Start(sharedDir); startErr != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to start shared Dolt server: %v\n", startErr)
		return &exitError{Code: 1}
	}
	if !st.safety.quiet {
		fmt.Printf("  %s Shared Dolt server started\n", ui.RenderPass("✓"))
	}
	return nil
}

func ensureInitGlobalDatabase(st *initRunContext) error {
	globalHost := configfile.DefaultDoltServerHost
	if st.server.host != "" {
		globalHost = st.server.host
	}
	globalPort := resolveInitDoltPort(st.resolved.paths.beadsDir)
	if globalPort == 0 {
		globalPort = doltserver.DefaultSharedServerPort
	}
	globalUser := configfile.DefaultDoltServerUser
	if st.server.user != "" {
		globalUser = st.server.user
	}
	if err := doltserver.EnsureGlobalDatabase(globalHost, globalPort, globalUser, ""); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to create global database: %v\n", err)
		return nil
	}
	if !st.safety.quiet {
		fmt.Printf("  %s Global database %s available\n", ui.RenderPass("✓"), doltserver.GlobalDatabaseName)
	}
	return nil
}

func connectInitDoltStore(st *initRunContext) error {
	store, err := newDoltStore(st.resolved.store.ctx, st.resolved.store.doltCfg)
	if err != nil {
		return handleInitStoreOpenError(st, err)
	}
	st.resolved.store.store = store
	return nil
}

func handleInitStoreOpenError(st *initRunContext, err error) error {
	var gateErr *schema.RemoteMigrateGateError
	if errors.As(err, &gateErr) {
		return handleInitRemoteMigrateGate(st, gateErr)
	}
	fmt.Fprintf(os.Stderr, "Error: failed to open Dolt store: %v\n", err)
	return &exitError{Code: 1}
}

func handleInitRemoteMigrateGate(st *initRunContext, gateErr *schema.RemoteMigrateGateError) error {
	if st.resolved.remote.bootstrappedFromRemote {
		fcfg := initTimeCloneConfig(st.server.initServerMode, st.server.host, st.server.port, st.server.socket, st.server.user, st.resolved.paths.dbName)
		if ferr := finalizeSyncedBootstrap(st.resolved.paths.beadsDir, st.resolved.remote.syncURL, fcfg, st.resolved.paths.dbName); ferr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to finalize bootstrapped workspace: %v\n", ferr)
		}
	}
	if isJSONOutput() {
		handleRemoteMigrateGateJSON(gateErr)
	} else if st.resolved.remote.bootstrappedFromRemote {
		printBootstrapRemoteBehindGuidance(os.Stderr, gateErr, st.resolved.remote.syncURL, "bd init")
	} else {
		fmt.Fprint(os.Stderr, gateErr.UserMessage())
	}
	return &exitError{Code: 1}
}

func finalizeInitStore(st *initRunContext) {
	doltCfg := st.resolved.store.doltCfg
	if shouldInitSharedGlobalDB(st.server.sharedServer, doltserver.IsSharedServerMode(), doltCfg.Gateway) {
		initGlobalDatabaseConfig(st.resolved.store.ctx, doltCfg, st.safety.quiet)
	}
	remote := st.resolved.remote
	if shouldWriteInitDoltRemote(doltCfg.Gateway, remote.syncURL, remote.syncFromRemote, remote.syncURLFromConfig, remote.syncURLFromGitOrigin, isDoltLocalOnly()) {
		configureInitDoltRemote(st.resolved.store.ctx, st.resolved.store.store, remote.syncURL, st.safety.quiet)
	}
}
