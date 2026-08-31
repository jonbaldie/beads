package uow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/proxy"
)

// DoltServerUOWOptions describes the server endpoint and schema context used
// to construct a unit-of-work provider.
type DoltServerUOWOptions struct {
	ServerRootDir        string
	Database             string
	ServerLogFilePath    string
	ServerConfigFilePath string
	Backend              proxy.Backend
	RootUser             string
	RootPassword         string
	DoltBinExec          string
	ProxyPort            int
	IdleTimeout          time.Duration
	TeamServer           bool
	ExpectedProjectID    string
}

func NewDoltServerUOWProvider(ctx context.Context, cfg DoltServerUOWOptions, opts ...ProviderOption) (UnitOfWorkProvider, error) {
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = defaultProxyIdleTimeout
	}
	if err := validateDoltServerOptions(cfg); err != nil {
		return nil, err
	}
	ep, err := openDoltServerEndpoint(cfg)
	if err != nil {
		return nil, err
	}
	return openAndInitSchema(ctx, ep, cfg.Database, cfg.RootUser, cfg.RootPassword, "", cfg.TeamServer, cfg.ExpectedProjectID, applyProviderOptions(opts))
}

func validateDoltServerOptions(cfg DoltServerUOWOptions) error {
	if cfg.Database == "" {
		return fmt.Errorf("uow: database name must not be empty (caller should default to %q)", "beads")
	}
	if err := cfg.Backend.Validate(); err != nil {
		return fmt.Errorf("uow: backend: %w", err)
	}
	if cfg.RootUser == "" {
		return fmt.Errorf("uow: rootUser must not be empty")
	}
	if cfg.DoltBinExec == "" {
		return fmt.Errorf("uow: doltBinExec must not be empty")
	}
	return nil
}

func openDoltServerEndpoint(cfg DoltServerUOWOptions) (proxy.Endpoint, error) {
	absServerRootDir, err := filepath.Abs(cfg.ServerRootDir)
	if err != nil {
		return proxy.Endpoint{}, fmt.Errorf("uow: resolving server root dir: %w", err)
	}
	absDoltBinExec, err := filepath.Abs(cfg.DoltBinExec)
	if err != nil {
		return proxy.Endpoint{}, fmt.Errorf("uow: resolving dolt bin exec: %w", err)
	}

	if err := os.MkdirAll(absServerRootDir, config.BeadsDirPerm); err != nil {
		return proxy.Endpoint{}, fmt.Errorf("uow: creating server root directory: %w", err)
	}

	ep, err := proxy.GetCreateDatabaseProxyServerEndpoint(absServerRootDir, proxy.OpenOpts{
		Backend:        cfg.Backend,
		ConfigFilePath: cfg.ServerConfigFilePath,
		LogFilePath:    cfg.ServerLogFilePath,
		DoltBinPath:    absDoltBinExec,
		Database:       cfg.Database,
		IdleTimeout:    cfg.IdleTimeout,
		Port:           cfg.ProxyPort,
	})
	if err != nil {
		return proxy.Endpoint{}, fmt.Errorf("uow: get proxy endpoint: %w", err)
	}
	return ep, nil
}
