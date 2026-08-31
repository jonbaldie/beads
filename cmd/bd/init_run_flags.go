package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/proxy"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/workspacegate"
	"github.com/spf13/cobra"
)

type initRunContext struct {
	cmd      *cobra.Command
	ident    initRunIdentFlags
	safety   initRunSafetyFlags
	server   initRunServerFlags
	proxied  initRunProxiedFlags
	resolved initRunResolved
	gate     *workspacegate.MultiHandle
}

type initRunIdentFlags struct {
	prefix, roleFlag, database, initRemote, backendFlag string
	initRemoteChanged                                   bool
}

type initRunSafetyFlags struct {
	quiet, contributor, team, stealth, skipHooks, skipAgents bool
	force, reinitLocal, initIfMissing, discardRemote         bool
	fromJSONL, nonInteractiveFlag                            bool
	destroyToken                                             string
}

type initRunServerFlags struct {
	initServerMode, sharedServer, externalServer, debugMode bool
	host, socket, user                                      string
	port                                                    int
}

type initRunProxiedFlags struct {
	enabled, teamServer bool
	paths               initProxiedServerPaths
	idleTimeoutSet      bool
	external            initRunExternalConn
}

type initRunExternalConn struct {
	host, socket, user string
	port               int
	tls                initRunExternalTLS
}

type initRunExternalTLS struct {
	required, skipVerify          bool
	caCert, cert, key, serverName string
	keepAlive                     time.Duration
}

func gatherInitFlags(cmd *cobra.Command) *initRunContext {
	st := &initRunContext{cmd: cmd, gate: nil}
	st.ident.prefix, _ = cmd.Flags().GetString("prefix")
	st.safety.quiet, _ = cmd.Flags().GetBool("quiet")
	st.safety.contributor, _ = cmd.Flags().GetBool("contributor")
	st.safety.team, _ = cmd.Flags().GetBool("team")
	st.safety.stealth, _ = cmd.Flags().GetBool("stealth")
	st.safety.skipHooks, _ = cmd.Flags().GetBool("skip-hooks")
	st.safety.skipAgents, _ = cmd.Flags().GetBool("skip-agents")
	st.safety.force, _ = cmd.Flags().GetBool("force")
	st.safety.reinitLocal, _ = cmd.Flags().GetBool("reinit-local")
	st.safety.initIfMissing, _ = cmd.Flags().GetBool("init-if-missing")
	st.safety.discardRemote, _ = cmd.Flags().GetBool("discard-remote")
	st.safety.nonInteractiveFlag, _ = cmd.Flags().GetBool("non-interactive")
	st.ident.roleFlag, _ = cmd.Flags().GetString("role")
	st.safety.fromJSONL, _ = cmd.Flags().GetBool("from-jsonl")
	st.ident.initRemote, _ = cmd.Flags().GetString("remote")
	st.ident.initRemoteChanged = cmd.Flags().Changed("remote")
	st.ident.backendFlag, _ = cmd.Flags().GetString("backend")
	st.server.initServerMode, _ = cmd.Flags().GetBool("server")
	st.server.host, _ = cmd.Flags().GetString("server-host")
	st.server.port, _ = cmd.Flags().GetInt("server-port")
	st.server.socket, _ = cmd.Flags().GetString("server-socket")
	st.server.user, _ = cmd.Flags().GetString("server-user")
	st.ident.database, _ = cmd.Flags().GetString("database")
	st.safety.destroyToken, _ = cmd.Flags().GetString("destroy-token")
	st.server.sharedServer, _ = cmd.Flags().GetBool("shared-server")
	st.server.externalServer, _ = cmd.Flags().GetBool("external")
	st.server.debugMode, _ = cmd.Flags().GetBool("debug")
	st.proxied.enabled, _ = cmd.Flags().GetBool("proxied-server")
	st.proxied.teamServer, _ = cmd.Flags().GetBool("team-server")
	st.proxied.paths.serverConfigPath, _ = cmd.Flags().GetString("proxied-server-config-path")
	st.proxied.paths.serverLogPath, _ = cmd.Flags().GetString("proxied-server-log-path")
	st.proxied.paths.serverRootPath, _ = cmd.Flags().GetString("proxied-server-root-path")
	st.proxied.paths.serverProxyPort, _ = cmd.Flags().GetInt("proxied-server-port")
	st.proxied.paths.serverProxyIdleTimeout, _ = cmd.Flags().GetDuration("proxied-server-idle-timeout")
	st.proxied.idleTimeoutSet = cmd.Flags().Changed("proxied-server-idle-timeout")
	gatherInitExternalFlags(cmd, &st.proxied.external)
	if os.Getenv("BEADS_DOLT_PROXIED_SERVER") == "1" {
		st.proxied.enabled = true
	}
	applyInitForceAlias(st)
	return st
}

func gatherInitExternalFlags(cmd *cobra.Command, ext *initRunExternalConn) {
	ext.host, _ = cmd.Flags().GetString("proxied-server-external-host")
	ext.port, _ = cmd.Flags().GetInt("proxied-server-external-port")
	ext.socket, _ = cmd.Flags().GetString("proxied-server-external-socket-path")
	ext.user, _ = cmd.Flags().GetString("proxied-server-external-user")
	ext.tls.required, _ = cmd.Flags().GetBool("proxied-server-external-tls")
	ext.tls.caCert, _ = cmd.Flags().GetString("proxied-server-external-tls-ca-cert-path")
	ext.tls.cert, _ = cmd.Flags().GetString("proxied-server-external-tls-cert-path")
	ext.tls.key, _ = cmd.Flags().GetString("proxied-server-external-tls-key-path")
	ext.tls.serverName, _ = cmd.Flags().GetString("proxied-server-external-tls-server-name")
	ext.tls.skipVerify, _ = cmd.Flags().GetBool("proxied-server-external-tls-skip-verify")
	ext.tls.keepAlive, _ = cmd.Flags().GetDuration("proxied-server-external-keep-alive")
}

func applyInitForceAlias(st *initRunContext) {
	// --force is a deprecated alias for --reinit-local. They share
	// semantics for the local data-safety guard; both refuse remote
	// divergence unless --discard-remote is also passed. See
	// engdocs/adr/0002-init-safety-invariants.md.
	if st.safety.force && !st.safety.reinitLocal {
		fmt.Fprintf(os.Stderr, "%s --force is deprecated; use --reinit-local instead.\n", ui.RenderWarn("DeprecationWarning:"))
		fmt.Fprintf(os.Stderr, "  See 'bd help init-safety' for the init flag surface.\n\n")
		st.safety.reinitLocal = true
	}
}

func validateInitFlagCombos(st *initRunContext) error {
	if err := validateInitModeExclusivity(st); err != nil {
		return err
	}
	if err := validateInitProxiedPathFlags(st); err != nil {
		return err
	}
	if err := validateInitProxiedPort(st.proxied.enabled, st.proxied.paths.serverProxyPort); err != nil {
		return err
	}
	if err := validateInitProxiedIdleTimeout(st); err != nil {
		return err
	}
	return validateInitExternalFlags(st)
}

func validateInitModeExclusivity(st *initRunContext) error {
	if st.proxied.enabled && st.server.initServerMode {
		return fmt.Errorf("--server and --proxied-server are mutually exclusive")
	}
	if st.proxied.enabled {
		if err := rejectInitProxiedServerCombos(st); err != nil {
			return err
		}
	}
	if st.proxied.teamServer && !st.proxied.enabled {
		return fmt.Errorf("--team-server requires --proxied-server")
	}
	return nil
}

func rejectInitProxiedServerCombos(st *initRunContext) error {
	s := st.server
	if rejectInitProxiedSharedCombos(s) || st.cmd.Flags().Changed("server-tls") {
		return fmt.Errorf("--proxied-server cannot be combined with --shared-server, --external, or any --server-* flag")
	}
	return nil
}

func rejectInitProxiedSharedCombos(s initRunServerFlags) bool {
	return s.sharedServer || s.externalServer || s.host != "" || s.port != 0 || s.socket != "" || s.user != ""
}

func validateInitProxiedPathFlags(st *initRunContext) error {
	p := st.proxied.paths
	if err := validateInitProxiedAbsPath(st.proxied.enabled, p.serverConfigPath, "--proxied-server-config-path", validateProxiedServerConfig); err != nil {
		return err
	}
	if err := validateInitProxiedAbsPath(st.proxied.enabled, p.serverLogPath, "--proxied-server-log-path", validateProxiedServerLogPath); err != nil {
		return err
	}
	return validateInitProxiedAbsPath(st.proxied.enabled, p.serverRootPath, "--proxied-server-root-path", validateProxiedServerRootPath)
}

func validateInitProxiedAbsPath(enabled bool, value, flag string, validate func(string) error) error {
	if value == "" {
		return nil
	}
	if !enabled {
		return fmt.Errorf("%s requires --proxied-server", flag)
	}
	if !filepath.IsAbs(value) {
		return fmt.Errorf("%s must be an absolute path, got %q", flag, value)
	}
	if err := validate(value); err != nil {
		return fmt.Errorf("%s %v", flag, err)
	}
	return nil
}

func validateInitProxiedPort(enabled bool, port int) error {
	if port == 0 {
		return nil
	}
	if !enabled {
		return fmt.Errorf("--proxied-server-port requires --proxied-server")
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("--proxied-server-port must be between 1 and 65535, got %d", port)
	}
	return nil
}

func validateInitProxiedIdleTimeout(st *initRunContext) error {
	if !st.proxied.idleTimeoutSet {
		return nil
	}
	if !st.proxied.enabled {
		return fmt.Errorf("--proxied-server-idle-timeout requires --proxied-server")
	}
	timeout := st.proxied.paths.serverProxyIdleTimeout
	if timeout < 0 {
		return fmt.Errorf("--proxied-server-idle-timeout must be 0 (never) or a positive duration, got %s", timeout)
	}
	if timeout == 0 {
		st.proxied.paths.serverProxyIdleTimeout = proxy.IdleTimeoutNever
	}
	return nil
}

func validateInitExternalFlags(st *initRunContext) error {
	if !initExternalFlagsProvided(st.proxied.external) {
		return nil
	}
	if !st.proxied.enabled {
		return fmt.Errorf("--proxied-server-external-* flags require --proxied-server")
	}
	if st.proxied.paths.serverConfigPath != "" {
		return fmt.Errorf("--proxied-server-external-* flags cannot be combined with --proxied-server-config-path (external mode has no managed dolt sql-server to configure)")
	}
	if st.server.debugMode {
		return fmt.Errorf("--debug cannot be combined with --proxied-server-external-* (debug applies to the managed dolt sql-server only)")
	}
	return buildInitExternalConfig(st)
}

func initExternalFlagsProvided(e initRunExternalConn) bool {
	return initExternalAddrProvided(e) || initExternalTLSProvided(e)
}

func initExternalAddrProvided(e initRunExternalConn) bool {
	return e.host != "" || e.port != 0 || e.socket != "" || e.user != ""
}

func initExternalTLSProvided(e initRunExternalConn) bool {
	t := e.tls
	return t.required || t.caCert != "" || t.cert != "" || t.key != "" || t.serverName != "" || t.skipVerify || t.keepAlive != 0
}

func buildInitExternalConfig(st *initRunContext) error {
	e := st.proxied.external
	cfg := configfile.ExternalDoltConfig{
		Host:            e.host,
		Port:            e.port,
		Socket:          e.socket,
		User:            e.user,
		TLSRequired:     e.tls.required,
		TLSCACert:       e.tls.caCert,
		TLSCert:         e.tls.cert,
		TLSKey:          e.tls.key,
		TLSServerName:   e.tls.serverName,
		TLSSkipVerify:   e.tls.skipVerify,
		KeepAlivePeriod: e.tls.keepAlive,
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("--proxied-server-external-*: %v", err)
	}
	st.resolved.externalConfig = &cfg
	return nil
}
