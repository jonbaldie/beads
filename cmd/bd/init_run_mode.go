package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/backends"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/spf13/cobra"
)

type initRunResolved struct {
	nonInteractive bool
	backend        string
	externalConfig *configfile.ExternalDoltConfig
	paths          initRunResolvedPaths
	remote         initRunResolvedRemote
	store          initRunResolvedStore
}

type initFinalizeArgs struct {
	cmd     *cobra.Command
	store   storage.DoltStorage
	doltCfg *dolt.Config
	ctx     context.Context
	ident   initFinalizeIdent
	mode    initFinalizeMode
	remote  initFinalizeRemote
}

type initFinalizeIdent struct {
	prefix, dbName, beadsDir, cwd, backend, roleFlag            string
	database, serverHost, serverSocket, serverUser, storagePath string
	serverPort                                                  int
	useLocalBeads                                               bool
}

type initFinalizeMode struct {
	quiet, contributor, team, stealth, skipHooks, skipAgents bool
	fromJSONL, nonInteractive, initServerMode                bool
	bootstrappedFromRemote, sharedServer, debugMode          bool
}

type initFinalizeRemote struct {
	syncURL, initRemote                                     string
	syncFromRemote, syncURLFromConfig, syncURLFromGitOrigin bool
	initRemoteChanged                                       bool
}

func resolveInitBackend(st *initRunContext) error {
	backendFlag := st.ident.backendFlag
	if !configfile.IsSupportedBackend(backendFlag) {
		return unsupportedInitBackendError(backendFlag)
	}
	// A registered extension backend passes IsSupportedBackend so its
	// existing workspaces can be opened, but init provisions Dolt only and
	// would otherwise create the workspace and persist backend: dolt. Reject
	// it here rather than silently creating the wrong workspace; downstream
	// registrants supply their own workspace-creation path.
	if backends.Registered(backendFlag) {
		return fmt.Errorf("backend %q cannot be created by bd init; it can only open an existing workspace (bd init provisions \"dolt\", the default)", backendFlag)
	}
	for _, legacyFlag := range removedBackendInitFlags {
		if st.cmd.Flags().Changed(legacyFlag.name) {
			return fmt.Errorf("--%s belonged to %s: %s; use --backend=dolt (the default)", legacyFlag.name, legacyFlag.origin, legacyFlag.rationale)
		}
	}
	if st.ident.database != "" {
		if err := dolt.ValidateDatabaseName(st.ident.database); err != nil {
			return fmt.Errorf("invalid database name %q: %v", st.ident.database, err)
		}
	}
	st.resolved.nonInteractive = isNonInteractiveInit(st.safety.nonInteractiveFlag)
	if err := validateInitRoleAndTeam(st); err != nil {
		return err
	}
	st.resolved.backend = configfile.BackendDolt
	applyInitServerModeEnv(st)
	applyInitServerModeGlobals(st)
	return nil
}

func unsupportedInitBackendError(backendFlag string) error {
	switch backendFlag {
	case configfile.BackendPostgres, configfile.BackendMySQL:
		return fmt.Errorf("storage backend %q is no longer supported: %s; the supported backend is \"dolt\" (default)", backendFlag, configfile.RemovedBackendRationale)
	case configfile.BackendSQLite:
		return fmt.Errorf("storage backend %q is no longer supported: %s; the supported backend is \"dolt\" (default)", backendFlag, configfile.RemovedSQLiteRationale)
	default:
		return fmt.Errorf("unknown backend %q: the supported backend is \"dolt\" (default)", backendFlag)
	}
}

func validateInitRoleAndTeam(st *initRunContext) error {
	if st.ident.roleFlag != "" {
		switch st.ident.roleFlag {
		case "maintainer", "contributor":
		default:
			return fmt.Errorf("invalid --role %q: must be \"maintainer\" or \"contributor\"", st.ident.roleFlag)
		}
	}
	if st.resolved.nonInteractive && st.safety.team {
		return fmt.Errorf("--team requires interactive prompts and cannot be used with --non-interactive")
	}
	return nil
}

func applyInitServerModeEnv(st *initRunContext) {
	if os.Getenv("BEADS_DOLT_SERVER_MODE") == "1" {
		st.server.initServerMode = true
	}
	// Shared server mode still uses a Dolt sql-server, so it must select
	// the server-backed store path during init. Without this, init can
	// persist shared-server intent in YAML while still creating an embedded
	// store and recording dolt_mode=embedded in metadata.json (GH#2946).
	shared := os.Getenv("BEADS_DOLT_SHARED_SERVER")
	if st.server.sharedServer || strings.EqualFold(shared, "true") || shared == "1" {
		st.server.initServerMode = true
	}
}

func applyInitServerModeGlobals(st *initRunContext) {
	// Set serverMode so !usesSQLServer() returns the correct value.
	// Both the global and cmdCtx must be set because PersistentPreRun
	// creates a fresh cmdCtx (with ServerMode=false) before Run executes.
	setServerMode(st.server.initServerMode)
	setProxiedServerMode(st.proxied.enabled)
}

func maybeRunInitProxied(st *initRunContext) (bool, error) {
	if !st.proxied.enabled {
		return false, nil
	}
	if beadsDir := resolveInitBeadsDir(); beadsDir != "" {
		if err := guardLegacyUpgradeWorkspace(beadsDir); err != nil {
			return true, err
		}
	}
	err := runInitProxiedServer(st.cmd, getRootContext(), initProxiedServerInput{
		prefix:                 st.ident.prefix,
		database:               st.ident.database,
		roleFlag:               st.ident.roleFlag,
		initRemote:             st.ident.initRemote,
		initRemoteChanged:      st.ident.initRemoteChanged,
		initProxiedServerPaths: st.proxied.paths,
		externalConfig:         st.resolved.externalConfig,
		initProxiedServerModes: initProxiedServerModes{
			quiet:          st.safety.quiet,
			stealth:        st.safety.stealth,
			skipHooks:      st.safety.skipHooks,
			skipAgents:     st.safety.skipAgents,
			contributor:    st.safety.contributor,
			team:           st.safety.team,
			teamServer:     st.proxied.teamServer,
			fromJSONL:      st.safety.fromJSONL,
			nonInteractive: st.resolved.nonInteractive,
		},
	})
	return true, err
}

func prepareInitServerMode(st *initRunContext) (func(), error) {
	applyInitServerEnv(st)
	if err := inheritInitWorkspaceDoltMode(st); err != nil {
		return func() {}, err
	}
	applyInitConfigYAMLServerMode(st)
	restore, err := promoteInitServerConnFlags(st)
	if err != nil {
		return func() {}, err
	}
	return restore, finishPrepareInitServerMode(st)
}

func applyInitServerEnv(st *initRunContext) {
	if st.server.sharedServer {
		_ = os.Setenv("BEADS_DOLT_SHARED_SERVER", "1")
	}
	if st.server.debugMode {
		_ = os.Setenv("BEADS_DOLT_DEBUG", "1")
	}
	if err := config.Initialize(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to initialize config: %v\n", err)
	}
}

func promoteInitServerConnFlags(st *initRunContext) (func(), error) {
	if !st.server.initServerMode {
		return func() {}, nil
	}
	return promoteExplicitServerConnFlags(st.cmd)
}

func finishPrepareInitServerMode(st *initRunContext) error {
	if err := rejectInitEmbeddedHyphenDatabase(st); err != nil {
		return err
	}
	if err := rejectInitRemoteHostWithoutServer(st); err != nil {
		return err
	}
	if beadsDir := resolveInitBeadsDir(); beadsDir != "" {
		return guardLegacyUpgradeWorkspace(beadsDir)
	}
	return nil
}

func inheritInitWorkspaceDoltMode(st *initRunContext) error {
	if st.server.initServerMode || initModeExplicitlyRequested(st.cmd) {
		return nil
	}
	inheritServer, inheritErr := inheritWorkspaceDoltMode()
	if inheritErr != nil {
		return inheritErr
	}
	if !inheritServer {
		return nil
	}
	st.server.initServerMode = true
	setServerMode(true)
	if !st.safety.quiet {
		fmt.Fprintln(os.Stderr, "Preserving server mode from the existing .beads/metadata.json.")
	}
	return nil
}

func applyInitConfigYAMLServerMode(st *initRunContext) {
	if st.server.initServerMode {
		return
	}
	if modeVal := config.GetYamlConfig("dolt.mode"); strings.EqualFold(modeVal, "server") {
		st.server.initServerMode = true
		setServerMode(true)
	}
}

func rejectInitEmbeddedHyphenDatabase(st *initRunContext) error {
	database := st.ident.database
	if database != "" && strings.ContainsRune(database, '-') && !usesSQLServer() {
		return fmt.Errorf("database name %q contains hyphens which are invalid in embedded mode; use underscores instead (e.g. %q)",
			database, sanitizeDBName(database))
	}
	return nil
}

func rejectInitRemoteHostWithoutServer(st *initRunContext) error {
	if st.server.initServerMode {
		return nil
	}
	configHost := config.GetYamlConfig("dolt.host")
	envHost := os.Getenv("BEADS_DOLT_SERVER_HOST")
	configPort := config.GetYamlConfig("dolt.port")
	envPort := os.Getenv("BEADS_DOLT_SERVER_PORT")
	conflict := detectInitRemoteHostConflict(configHost, envHost, configPort, envPort)
	if conflict == nil {
		return nil
	}
	detail := fmt.Sprintf("dolt.host (%s) is", conflict.host)
	if conflict.includesPort {
		detail = fmt.Sprintf("dolt.host (%s) and dolt.port are", conflict.host)
	}
	return fmt.Errorf("%s set via %s but server mode is not enabled.\n"+
		"  Embedded mode has no host/port — these settings require server mode.\n"+
		"  Set dolt.mode: server in %s or pass --server to bd init.",
		detail, conflict.source, config.UserConfigYamlDisplayPath())
}

func initFinalizeFromState(st *initRunContext) initFinalizeArgs {
	return initFinalizeArgs{
		cmd: st.cmd, store: st.resolved.store.store, doltCfg: st.resolved.store.doltCfg, ctx: st.resolved.store.ctx,
		ident: initFinalizeIdent{
			prefix: st.ident.prefix, dbName: st.resolved.paths.dbName, beadsDir: st.resolved.paths.beadsDir,
			cwd: st.resolved.paths.cwd, backend: st.resolved.backend, roleFlag: st.ident.roleFlag,
			database: st.ident.database, serverHost: st.server.host, serverSocket: st.server.socket,
			serverUser: st.server.user, storagePath: st.resolved.paths.storagePath, serverPort: st.server.port,
			useLocalBeads: st.resolved.paths.useLocalBeads,
		},
		mode: initFinalizeMode{
			quiet: st.safety.quiet, contributor: st.safety.contributor, team: st.safety.team,
			stealth: st.safety.stealth, skipHooks: st.safety.skipHooks, skipAgents: st.safety.skipAgents,
			fromJSONL: st.safety.fromJSONL, nonInteractive: st.resolved.nonInteractive,
			initServerMode: st.server.initServerMode, bootstrappedFromRemote: st.resolved.remote.bootstrappedFromRemote,
			sharedServer: st.server.sharedServer, debugMode: st.server.debugMode,
		},
		remote: initFinalizeRemote{
			syncURL: st.resolved.remote.syncURL, initRemote: st.ident.initRemote,
			syncFromRemote: st.resolved.remote.syncFromRemote, syncURLFromConfig: st.resolved.remote.syncURLFromConfig,
			syncURLFromGitOrigin: st.resolved.remote.syncURLFromGitOrigin, initRemoteChanged: st.ident.initRemoteChanged,
		},
	}
}
