package uow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/proxy"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/server"
)

// ExternalDoltServerUOWOptions describes an externally managed Dolt server
// and the schema context used by its provider.
type ExternalDoltServerUOWOptions struct {
	ServerRootDir     string
	Database          string
	ServerLogFilePath string
	External          configfile.ExternalDoltConfig
	RootUser          string
	RootPassword      string
	ProxyPort         int
	IdleTimeout       time.Duration
	TeamServer        bool
	ExpectedProjectID string
}

func NewExternalDoltServerUOWProvider(ctx context.Context, cfg ExternalDoltServerUOWOptions, opts ...ProviderOption) (UnitOfWorkProvider, error) {
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = defaultProxyIdleTimeout
	}
	if err := validateExternalDoltServerOptions(cfg); err != nil {
		return nil, err
	}
	ep, tlsConfigName, err := openExternalDoltServerEndpoint(cfg)
	if err != nil {
		return nil, err
	}

	return openAndInitSchema(ctx, ep, cfg.Database, cfg.RootUser, cfg.RootPassword, tlsConfigName, cfg.TeamServer, cfg.ExpectedProjectID, applyProviderOptions(opts))
}

func validateExternalDoltServerOptions(cfg ExternalDoltServerUOWOptions) error {
	if cfg.Database == "" {
		return fmt.Errorf("uow: database name must not be empty (caller should default to %q)", "beads")
	}
	if cfg.RootUser == "" {
		return fmt.Errorf("uow: rootUser must not be empty")
	}
	if err := cfg.External.Validate(); err != nil {
		return fmt.Errorf("uow: external: %w", err)
	}
	return nil
}

func openExternalDoltServerEndpoint(cfg ExternalDoltServerUOWOptions) (proxy.Endpoint, string, error) {
	absServerRootDir, err := filepath.Abs(cfg.ServerRootDir)
	if err != nil {
		return proxy.Endpoint{}, "", fmt.Errorf("uow: resolving server root dir: %w", err)
	}
	if err := os.MkdirAll(absServerRootDir, config.BeadsDirPerm); err != nil {
		return proxy.Endpoint{}, "", fmt.Errorf("uow: creating server root directory: %w", err)
	}
	tlsConfigName, err := registerExternalTLSConfig(cfg.External)
	if err != nil {
		return proxy.Endpoint{}, "", fmt.Errorf("uow: external TLS: %w", err)
	}
	ep, err := proxy.GetCreateDatabaseProxyServerEndpoint(absServerRootDir, proxy.OpenOpts{
		Backend:     proxy.BackendExternal,
		LogFilePath: cfg.ServerLogFilePath,
		External:    cfg.External,
		IdleTimeout: cfg.IdleTimeout,
		Port:        cfg.ProxyPort,
	})
	if err != nil {
		return proxy.Endpoint{}, "", fmt.Errorf("uow: get proxy endpoint: %w", err)
	}
	return ep, tlsConfigName, nil
}

func registerExternalTLSConfig(external configfile.ExternalDoltConfig) (string, error) {
	if !external.TLSRequired {
		return "", nil
	}
	tc, err := external.TLSClientConfig()
	if err != nil {
		return "", err
	}
	name := "beads-external-" + server.ExternalDoltServerID(external)
	if err := mysql.RegisterTLSConfig(name, tc); err != nil {
		return "", fmt.Errorf("register TLS config: %w", err)
	}
	return name, nil
}
