package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/doltutil"
	"github.com/jonbaldie/beads/internal/ui"
)

type doltConfigChange struct {
	yamlKey string
	handled bool
}

func setDoltDatabaseConfig(cfg *configfile.Config, value string) (doltConfigChange, error) {
	if value == "" {
		return doltConfigChange{}, HandleError("database name cannot be empty")
	}
	cfg.DoltDatabase = value
	return doltConfigChange{yamlKey: "dolt.database"}, nil
}

func setDoltHostConfig(cfg *configfile.Config, value string) (doltConfigChange, error) {
	if value == "" {
		return doltConfigChange{}, HandleError("host cannot be empty")
	}
	cfg.DoltServerHost = value
	return doltConfigChange{yamlKey: "dolt.host"}, nil
}

func setDoltPortConfig(cfg *configfile.Config, value string) (doltConfigChange, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port <= 0 || port > 65535 {
		return doltConfigChange{}, HandleError("port must be a valid port number (1-65535)")
	}
	cfg.DoltServerPort = port
	return doltConfigChange{yamlKey: "dolt.port"}, nil
}

func setDoltSocketConfig(cfg *configfile.Config, value string) (doltConfigChange, error) {
	// Empty value clears the socket (reverts to TCP host/port).
	cfg.DoltServerSocket = value
	return doltConfigChange{yamlKey: "dolt.socket"}, nil
}

func setDoltUserConfig(cfg *configfile.Config, value string) (doltConfigChange, error) {
	if value == "" {
		return doltConfigChange{}, HandleError("user cannot be empty")
	}
	cfg.DoltServerUser = value
	return doltConfigChange{yamlKey: "dolt.user"}, nil
}

func setDoltDataDirConfig(cfg *configfile.Config, value string) (doltConfigChange, error) {
	// GH#2438: In server mode, data-dir has no effect on which database
	// the server connects to. Setting it silently switches the local
	// resolution path without affecting the running server, causing
	// commands to operate on the wrong (often empty) database.
	if value != "" && cfg.IsDoltServerMode() {
		fmt.Fprintf(os.Stderr, "Error: setting data-dir in server mode is not supported (GH#2438).\n")
		fmt.Fprintf(os.Stderr, "In server mode, the database is determined by the 'database' config key,\n")
		fmt.Fprintf(os.Stderr, "not the local data directory. Setting data-dir would silently disconnect\n")
		fmt.Fprintf(os.Stderr, "from the configured database '%s'.\n", cfg.GetDoltDatabase())
		fmt.Fprintf(os.Stderr, "\nTo change which database to use:\n")
		fmt.Fprintf(os.Stderr, "  bd dolt set database <name>\n")
		return doltConfigChange{}, SilentExit()
	}
	if value == "" {
		// Allow clearing the custom data dir (revert to default .beads/dolt)
		cfg.DoltDataDir = ""
		return doltConfigChange{yamlKey: "dolt.data-dir"}, nil
	}
	if !filepath.IsAbs(value) {
		return doltConfigChange{}, HandleError("data-dir must be an absolute path")
	}
	cfg.DoltDataDir = value
	// Absolute paths are machine-specific and won't be persisted to
	// metadata.json (which is committed to git). Use the env var for
	// persistence across sessions. (GH#2251)
	fmt.Fprintf(os.Stderr, "Note: absolute paths are not saved to metadata.json (it propagates via git).\n")
	fmt.Fprintf(os.Stderr, "For persistence, add to your shell profile:\n")
	fmt.Fprintf(os.Stderr, "  export BEADS_DOLT_DATA_DIR=%s\n", value)
	return doltConfigChange{yamlKey: "dolt.data-dir"}, nil
}

func setDoltSharedServerConfig(value string) (doltConfigChange, error) {
	lower := strings.ToLower(value)
	if lower != "true" && lower != "false" {
		return doltConfigChange{}, HandleError("shared-server must be 'true' or 'false'")
	}
	// shared-server is yaml-only (not stored in metadata.json)
	if err := config.SetYamlConfig("dolt.shared-server", lower); err != nil {
		return doltConfigChange{}, HandleError("setting shared-server: %v", err)
	}
	if isJSONOutput() {
		if err := outputJSON(map[string]interface{}{
			"key":      "shared-server",
			"value":    lower,
			"location": "config.yaml",
		}); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return doltConfigChange{handled: true}, nil
	}
	if lower == "true" {
		fmt.Println("Shared server mode enabled.")
		fmt.Println("All projects will use a single Dolt server at ~/.beads/shared-server/.")
		fmt.Println("Each project's data remains isolated in its own database.")
	} else {
		fmt.Println("Shared server mode disabled. Each project will use its own Dolt server.")
	}
	return doltConfigChange{handled: true}, nil
}

func applyDoltConfigChange(cfg *configfile.Config, key, value string) (doltConfigChange, error) {
	switch key {
	case "mode":
		// Mode will be configurable again when embedded Dolt support returns.
		// For now, server mode is required (embedded driver not yet re-integrated).
		return doltConfigChange{}, HandleError("mode is not yet configurable; embedded mode is coming soon")
	case "database":
		return setDoltDatabaseConfig(cfg, value)
	case "host":
		return setDoltHostConfig(cfg, value)
	case "port":
		return setDoltPortConfig(cfg, value)
	case "socket":
		return setDoltSocketConfig(cfg, value)
	case "user":
		return setDoltUserConfig(cfg, value)
	case "data-dir":
		return setDoltDataDirConfig(cfg, value)
	case "shared-server":
		return setDoltSharedServerConfig(value)
	default:
		fmt.Fprintf(os.Stderr, "Error: unknown key '%s'\n", key)
		fmt.Fprintf(os.Stderr, "Valid keys: database, host, port, socket, user, data-dir, shared-server\n")
		return doltConfigChange{}, SilentExit()
	}
}

func renderDoltConfigChangeJSON(key, value string, updateConfig bool) error {
	result := map[string]interface{}{
		"key":      key,
		"value":    value,
		"location": "metadata.json",
	}
	if updateConfig {
		result["config_yaml_updated"] = true
	}
	if err := outputJSON(result); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	}
	return nil
}

func updateDoltConfigYAML(yamlKey, value string) error {
	if yamlKey == "" {
		return nil
	}
	if err := config.SetYamlConfig(yamlKey, value); err != nil {
		fmt.Printf("%s\n", ui.RenderWarn(fmt.Sprintf("Warning: failed to update config.yaml: %v", err)))
		return nil
	}
	fmt.Printf("Set %s = %s (in config.yaml)\n", yamlKey, value)
	return nil
}

func persistDoltConfigChange(beadsDir string, cfg *configfile.Config, key, value string, change doltConfigChange, updateConfig bool) error {
	// Audit log: record who changed what
	logDoltConfigChange(beadsDir, key, value)
	if err := cfg.Save(beadsDir); err != nil {
		return HandleError("saving config: %v", err)
	}
	if isJSONOutput() {
		return renderDoltConfigChangeJSON(key, value, updateConfig)
	}
	fmt.Printf("Set %s = %s (in metadata.json)\n", key, value)
	return updateDoltConfigYAML(change.yamlKey, value)
}

func setDoltConfig(key, value string, updateConfig bool) error {
	beadsDir := selectedDoltBeadsDir()
	if beadsDir == "" {
		return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
	}

	cfg, err := loadDoltBackendConfig(beadsDir)
	if err != nil {
		return HandleError("%v", err)
	}
	change, err := applyDoltConfigChange(cfg, key, value)
	if err != nil {
		return err
	}
	if change.handled {
		return nil
	}
	return persistDoltConfigChange(beadsDir, cfg, key, value, change, updateConfig)
}

func renderDoltConnectionJSON(host string, port int) error {
	ok := testServerConnection(host, port)
	if err := outputJSON(map[string]interface{}{
		"host":          host,
		"port":          port,
		"connection_ok": ok,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	}
	if !ok {
		return SilentExit()
	}
	return nil
}

func printDoltRemoteConnectivity(remote storage.RemoteInfo) {
	if doltutil.IsSSHURL(remote.URL) {
		// Test SSH connectivity by parsing host from URL
		sshHost := extractSSHHost(remote.URL)
		if sshHost == "" {
			return
		}
		fmt.Printf("  %s (%s)... ", remote.Name, remote.URL)
		if testSSHConnectivity(sshHost) {
			fmt.Printf("%s\n", ui.RenderPass("✓ reachable"))
		} else {
			fmt.Printf("%s\n", ui.RenderWarn("✗ unreachable"))
		}
		return
	}
	if strings.HasPrefix(remote.URL, "https://") || strings.HasPrefix(remote.URL, "http://") {
		fmt.Printf("  %s (%s)... ", remote.Name, remote.URL)
		if testHTTPConnectivity(remote.URL) {
			fmt.Printf("%s\n", ui.RenderPass("✓ reachable"))
		} else {
			fmt.Printf("%s\n", ui.RenderWarn("✗ unreachable"))
		}
		return
	}
	fmt.Printf("  %s (%s)... skipped (no connectivity test for this scheme)\n", remote.Name, remote.URL)
}

func testDoltRemoteConnectivity() error {
	st := getStore()
	if st == nil {
		return nil
	}
	remotes, err := st.ListRemotes(context.Background())
	if err != nil || len(remotes) == 0 {
		return nil
	}
	fmt.Println("\nRemote connectivity:")
	for _, remote := range remotes {
		printDoltRemoteConnectivity(remote)
	}
	return nil
}
