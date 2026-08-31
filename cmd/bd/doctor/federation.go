package doctor

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/storage/doltutil"
)

// doltDatabaseName returns the configured Dolt database name for the given beads directory.
// Falls back to the default ("beads") if config cannot be read.
func doltDatabaseName(beadsDir string) string {
	dbName := configfile.DefaultDoltDatabase
	if cfg, err := configfile.Load(beadsDir); err == nil && cfg != nil {
		dbName = cfg.GetDoltDatabase()
	}
	return dbName
}

// doltServerConfig returns a read-only dolt.Config populated with server
// connection settings from beads configuration. This ensures federation checks
// use the configured host/port rather than falling back to defaults.
func doltServerConfig(beadsDir, doltPath string) *dolt.Config {
	cfg := &dolt.Config{
		Path:     doltPath,
		ReadOnly: true,
		Database: doltDatabaseName(beadsDir),
	}
	if bcfg, err := configfile.Load(beadsDir); err == nil && bcfg != nil {
		cfg.ServerHost = bcfg.GetDoltServerHost()
		cfg.ServerPort = doltserver.DefaultConfig(beadsDir).Port
		cfg.ServerUser = bcfg.GetDoltServerUser()
		cfg.ServerTLS = bcfg.GetDoltServerTLS()
		cfg.ServerPassword = bcfg.GetDoltServerPasswordForPort(cfg.ServerPort)
	}
	dolt.ApplyCLIAutoStart(beadsDir, cfg)
	return cfg
}

type legacyCLILocation struct {
	label string
	dir   string
}

func listSQLRemoteURLs(ctx context.Context, store *dolt.DoltStore) (map[string]string, error) {
	remotes, err := store.ListRemotes(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]string, len(remotes))
	for _, remote := range remotes {
		byName[remote.Name] = remote.URL
	}
	return byName, nil
}

func inspectLegacyCLIRemotes(store *dolt.DoltStore, sqlByName map[string]string) ([]string, []string, []string) {
	locations := []legacyCLILocation{
		{label: "database CLI directory", dir: store.CLIDir()},
		{label: "Dolt server root", dir: store.Path()},
	}
	var inspected, missing, inspectErrors []string
	seenDirs := make(map[string]bool, len(locations))
	for _, loc := range locations {
		inspectedDir, dirMissing, inspectErr := inspectLegacyCLILocation(loc, sqlByName, seenDirs)
		if inspectedDir != "" {
			inspected = append(inspected, inspectedDir)
		}
		missing = append(missing, dirMissing...)
		if inspectErr != "" {
			inspectErrors = append(inspectErrors, inspectErr)
		}
	}
	return inspected, missing, inspectErrors
}

func inspectLegacyCLILocation(loc legacyCLILocation, sqlByName map[string]string, seenDirs map[string]bool) (string, []string, string) {
	if loc.dir == "" {
		return "", nil, ""
	}
	dir := filepath.Clean(loc.dir)
	if seenDirs[dir] {
		return "", nil, ""
	}
	seenDirs[dir] = true
	if _, err := os.Stat(filepath.Join(dir, ".dolt")); err != nil {
		if os.IsNotExist(err) {
			return "", nil, ""
		}
		return "", nil, fmt.Sprintf("%s (%s): %v", loc.label, dir, err)
	}

	cliRemotes, err := doltutil.ListCLIRemotes(dir)
	if err != nil {
		return "", nil, fmt.Sprintf("%s (%s): %v", loc.label, dir, err)
	}
	inspected := fmt.Sprintf("%s: %s", loc.label, dir)
	var missing []string
	for _, remote := range cliRemotes {
		if !doltutil.RemoteURLsMatch(sqlByName[remote.Name], remote.URL) {
			missing = append(missing, fmt.Sprintf("%s %s=%s", loc.label, remote.Name, remote.URL))
		}
	}
	return inspected, missing, ""
}

func federationNotApplicable(name, message string) DoctorCheck {
	return DoctorCheck{
		Name:     name,
		Status:   StatusOK,
		Message:  message,
		Category: CategoryFederation,
	}
}

func countFederationPeers(remotes []storage.RemoteInfo) int {
	count := 0
	for _, remote := range remotes {
		if remote.Name != "origin" {
			count++
		}
	}
	return count
}

func hasFederationPeers(ctx context.Context, store *dolt.DoltStore) bool {
	remotes, err := store.ListRemotes(ctx)
	return err == nil && countFederationPeers(remotes) > 0
}

func checkFederationRemotesAPINotRunning(ctx context.Context, beadsDir, doltPath string) DoctorCheck {
	store, err := dolt.New(ctx, doltServerConfig(beadsDir, doltPath))
	if err != nil {
		return federationNotApplicable("Federation remotesapi", "N/A (server not running, no remotes check needed)")
	}
	defer func() { _ = store.Close() }()

	remotes, err := store.ListRemotes(ctx)
	if err != nil || len(remotes) == 0 {
		return federationNotApplicable("Federation remotesapi", "N/A (no peers configured)")
	}
	return DoctorCheck{
		Name:     "Federation remotesapi",
		Status:   StatusWarning,
		Message:  fmt.Sprintf("Server not running (%d peers configured)", len(remotes)),
		Detail:   "Federation requires dolt sql-server for peer sync",
		Fix:      "Start dolt sql-server in server mode to enable peer-to-peer sync",
		Category: CategoryFederation,
	}
}

func federationRemotesAPIPort(beadsDir string) int {
	port := configfile.DefaultDoltRemotesAPIPort
	if cfg, err := configfile.Load(beadsDir); err == nil && cfg != nil {
		port = cfg.GetDoltRemotesAPIPort()
	}
	return port
}

func probeFederationRemotesAPI(serverState *doltserver.State, port int) DoctorCheck {
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port))
	// remotesapi speaks gRPC/HTTP, not MySQL, so a bare dial is the useful probe.
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return DoctorCheck{
			Name:     "Federation remotesapi",
			Status:   StatusError,
			Message:  fmt.Sprintf("remotesapi port %d not accessible", port),
			Detail:   fmt.Sprintf("Server running (PID %d) but remotesapi port unreachable: %v", serverState.PID, err),
			Fix:      "Check if dolt sql-server is running with --remotesapi-port flag",
			Category: CategoryFederation,
		}
	}
	_ = conn.Close()
	return DoctorCheck{
		Name:     "Federation remotesapi",
		Status:   StatusOK,
		Message:  fmt.Sprintf("Port %d accessible", port),
		Category: CategoryFederation,
	}
}
