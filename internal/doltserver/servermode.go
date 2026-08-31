package doltserver

import (
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/configfile"
)

// ServerMode describes who owns and manages the dolt sql-server lifecycle.
type ServerMode int

const (
	// ServerModeOwned means beads auto-starts and manages the server.
	// This is the default for standalone users with no explicit port config.
	ServerModeOwned ServerMode = iota

	// ServerModeExternal means the user manages the server lifecycle
	// (e.g., systemd, Docker, Hosted Dolt, VPS). Beads never starts or
	// stops the server. Determined when metadata.json has an explicit
	// dolt_server_port or BEADS_DOLT_SHARED_SERVER is set.
	ServerModeExternal

	// ServerModeEmbedded is the legacy in-process embedded dolt path.
	// Determined when metadata.json dolt_mode is "embedded".
	ServerModeEmbedded
)

// String returns a human-readable label for the server mode.
func (m ServerMode) String() string {
	switch m {
	case ServerModeOwned:
		return "owned"
	case ServerModeExternal:
		return "external"
	case ServerModeEmbedded:
		return "embedded"
	default:
		return fmt.Sprintf("ServerMode(%d)", int(m))
	}
}

// ResolveServerMode determines the server mode from the given beadsDir.
// This is the single source of truth for how the server lifecycle is managed.
//
// Decision logic (checked in order):
//  1. BEADS_DOLT_SERVER_MODE=1 env var             -> ServerModeExternal
//  2. BEADS_DOLT_SHARED_SERVER env var is set       -> ServerModeExternal
//     2b. host-based inference (HostImpliesServerMode) -> ServerModeExternal
//  3. metadata.json dolt_mode == "embedded"         -> ServerModeEmbedded
//  4. metadata.json has explicit dolt_server_port   -> ServerModeExternal
//  5. default                                       -> ServerModeOwned
//
// Runtime env vars (1, 2) take precedence over persisted metadata.json
// to prevent stale dolt_mode=embedded from silently overriding an active
// shared-server or server-mode configuration (GH#2949).
//
// The function loads metadata.json only if the file exists, to avoid
// triggering the legacy config.json -> metadata.json migration side effect.
func ResolveServerMode(beadsDir string) ServerMode {
	if mode, ok := runtimeServerMode(); ok {
		return mode
	}
	return metadataServerMode(loadServerModeConfig(beadsDir))
}

func runtimeServerMode() (ServerMode, bool) {
	if os.Getenv("BEADS_DOLT_SERVER_MODE") == "1" || IsSharedServerMode() {
		return ServerModeExternal, true
	}
	return ServerModeOwned, false
}

func loadServerModeConfig(beadsDir string) *configfile.Config {
	metadataPath := configfile.ConfigPath(beadsDir)
	if _, err := os.Stat(metadataPath); err != nil {
		return nil
	}
	cfg, err := configfile.Load(beadsDir)
	if err != nil || cfg == nil {
		return nil
	}
	return cfg
}

func metadataServerMode(fileCfg *configfile.Config) ServerMode {
	// Host-based inference runs before metadata mode so a runtime host beats
	// stale embedded metadata. A nil config is treated as an empty config.
	hostCfg := fileCfg
	if hostCfg == nil {
		hostCfg = &configfile.Config{}
	}
	if hostCfg.HostImpliesServerMode() {
		return ServerModeExternal
	}
	if fileCfg != nil && fileCfg.DoltMode != "" && strings.EqualFold(fileCfg.DoltMode, configfile.DoltModeEmbedded) {
		return ServerModeEmbedded
	}
	if fileCfg != nil && fileCfg.DoltServerPort > 0 {
		return ServerModeExternal
	}
	return ServerModeOwned
}
