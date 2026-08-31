package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/proxy"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/server"
)

type dbProxyChildOptions struct {
	root               string
	port               int
	idleTimeout        time.Duration
	backend            string
	config             string
	logPath            string
	doltBin            string
	database           string
	externalHost       string
	externalPort       int
	externalSocketPath string
	externalKeepAlive  time.Duration
	stopEpoch          string
}

func dbProxyChildOptionsFromCommand(cmd *cobra.Command) dbProxyChildOptions {
	flags := cmd.Flags()
	root, _ := flags.GetString("root")
	port, _ := flags.GetInt("port")
	idleTimeout, _ := flags.GetDuration("idle-timeout")
	backend, _ := flags.GetString("backend")
	config, _ := flags.GetString("config")
	logPath, _ := flags.GetString("logpath")
	doltBin, _ := flags.GetString("dolt-bin")
	database, _ := flags.GetString("database")
	externalHost, _ := flags.GetString("external-host")
	externalPort, _ := flags.GetInt("external-port")
	externalSocketPath, _ := flags.GetString("external-socket-path")
	externalKeepAlive, _ := flags.GetDuration("external-keep-alive")
	stopEpoch, _ := flags.GetString("stop-epoch")
	return dbProxyChildOptions{
		root:               root,
		port:               port,
		idleTimeout:        idleTimeout,
		backend:            backend,
		config:             config,
		logPath:            logPath,
		doltBin:            doltBin,
		database:           database,
		externalHost:       externalHost,
		externalPort:       externalPort,
		externalSocketPath: externalSocketPath,
		externalKeepAlive:  externalKeepAlive,
		stopEpoch:          stopEpoch,
	}
}

var dbProxyChildCmd = &cobra.Command{
	Use:    "db-proxy-child",
	Hidden: true,
	Short:  "Internal: run as the database proxy child process",
	Long: `db-proxy-child runs the long-lived per-rootDir TCP proxy that fronts a
DatabaseServer. It is spawned by the parent bd process via fork+exec and is
not intended to be invoked directly by users.`,

	PersistentPreRun:  func(cmd *cobra.Command, args []string) {},
	PersistentPostRun: func(cmd *cobra.Command, args []string) {},

	RunE: func(cmd *cobra.Command, _ []string) error {
		opts := dbProxyChildOptionsFromCommand(cmd)
		backend := proxy.Backend(opts.backend)
		if err := backend.Validate(); err != nil {
			return err
		}

		external := configfile.ExternalDoltConfig{
			Host:            opts.externalHost,
			Port:            opts.externalPort,
			Socket:          opts.externalSocketPath,
			KeepAlivePeriod: opts.externalKeepAlive,
		}

		srv, err := newDatabaseServer(backend, opts.root, opts.config, opts.logPath, opts.doltBin, opts.database, external)
		if err != nil {
			return err
		}
		defer func() { _ = srv.Stop(context.Background()) }()

		p := proxy.NewProxyServer(proxy.ProxyOpts{
			RootDir:     opts.root,
			Port:        opts.port,
			IdleTimeout: opts.idleTimeout,
			Server:      srv,
			StopEpoch:   opts.stopEpoch,
		})
		if err := p.ListenAndServe(cmd.Context()); err != nil {
			if errors.Is(err, proxy.ErrLockHeld) {
				return &exitError{Code: proxy.LockHeldExitCode}
			}
			return err
		}
		return nil
	},
}

func newDatabaseServer(backend proxy.Backend, rootDir, configPath, logPath, doltBin, database string, external configfile.ExternalDoltConfig) (server.DatabaseServer, error) {
	switch backend {
	case proxy.BackendLocalServer:
		return server.NewDoltServer(doltBin, rootDir, configPath, logPath, 0, database)
	case proxy.BackendExternal:
		return server.NewExternalDoltServer(external)
	case proxy.BackendLocalSharedServer:
		return nil, fmt.Errorf("backend %q: not yet implemented", backend)
	}
	return nil, fmt.Errorf("unknown backend %q", backend)
}

func init() {
	dbProxyChildCmd.Flags().String("root", "", "root directory holding proxy.lock, proxy.pid, proxy.log")
	dbProxyChildCmd.Flags().Int("port", 0, "port to listen on")
	dbProxyChildCmd.Flags().Duration("idle-timeout", 0, "idle timeout before shutdown (0 or negative = never shut down)")
	dbProxyChildCmd.Flags().String("backend", "",
		"backend kind: "+strings.Join(proxy.KnownBackendNames(), " | "))
	dbProxyChildCmd.Flags().String("config", "", "path to backend server config (e.g. dolt sql-server YAML)")
	dbProxyChildCmd.Flags().String("logpath", "", "path the backend server should write its stdout/stderr to")
	dbProxyChildCmd.Flags().String("dolt-bin", "", "path to the dolt executable")
	dbProxyChildCmd.Flags().String("database", "", "database to select when running shutdown maintenance (local-server backend)")
	dbProxyChildCmd.Flags().String("external-host", "", "external backend: hostname or IP of the dolt sql-server")
	dbProxyChildCmd.Flags().Int("external-port", 0, "external backend: TCP port of the dolt sql-server")
	dbProxyChildCmd.Flags().String("external-socket-path", "", "external backend: absolute path to a unix domain socket (overrides host/port)")
	dbProxyChildCmd.Flags().Duration("external-keep-alive", 0, "external backend: TCP keepalive period (default 30s)")
	dbProxyChildCmd.Flags().String("stop-epoch", "", "stop epoch the parent observed at spawn; the proxy aborts before publishing if it advances")
	_ = dbProxyChildCmd.MarkFlagRequired("root")
	_ = dbProxyChildCmd.MarkFlagRequired("port")
	_ = dbProxyChildCmd.MarkFlagRequired("backend")
	rootCmd.AddCommand(dbProxyChildCmd)
}
