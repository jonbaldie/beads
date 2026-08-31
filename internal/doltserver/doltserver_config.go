// Package doltserver manages the lifecycle of a local dolt sql-server process.
// It provides transparent auto-start so that `bd init` and `bd <command>` work
// without manual server management.
//
// Port assignment uses OS-assigned ephemeral ports by default. When no explicit
// port is configured (env var, config.yaml, metadata.json), Start() asks the OS
// for a free port via net.Listen(":0"), passes it to dolt sql-server, and writes
// the actual port to dolt-server.port. This eliminates the birthday-problem
// collisions that plagued the old hash-derived port scheme (GH#2098, GH#2372).
//
// Users with explicit port config via BEADS_DOLT_SERVER_PORT env var or
// config.yaml always use that port instead, with conflict detection via
// reclaimPort.
//
// Server state files (PID, port, log, lock) live in the .beads/ directory.
package doltserver

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/configfile"
)

// DefaultConfig returns config with sensible defaults. Port resolution walks
// portSources in priority order (see PortSourceLabels) and returns port 0
// when no source provides one, meaning Start() should allocate an ephemeral
// port from the OS.
//
// The port file (dolt-server.port) is written by Start() with the actual
// listening port, so already-running-server connections use the right port.
func DefaultConfig(beadsDir string) *Config {
	beadsDir, sharedMode, sharedEnabled := defaultConfigServerDir(beadsDir)
	cfg := &Config{BeadsDir: beadsDir, Host: "127.0.0.1", Mode: ResolveServerMode(beadsDir)}
	resolveDefaultConfigPort(cfg, beadsDir, sharedMode, sharedEnabled)
	applyExternalHostConfig(cfg, beadsDir, sharedMode)
	return cfg
}

func defaultConfigServerDir(beadsDir string) (string, bool, bool) {
	sharedEnabled := IsSharedServerMode()
	if !sharedEnabled {
		return beadsDir, false, false
	}
	sharedDir, err := SharedServerDir()
	if err != nil {
		return beadsDir, false, true
	}
	return sharedDir, true, true
}

func resolveDefaultConfigPort(cfg *Config, beadsDir string, sharedMode, sharedEnabled bool) {
	for _, src := range portSources {
		if port, ok := src.resolve(beadsDir); ok {
			cfg.Port = port
			cfg.PortSource = src.source
			cfg.PortSharedServer = sharedMode
			break
		}
	}
	// Port 0 means "no configured port". In shared mode, use the fixed
	// shared server port. In per-project mode, Start() will allocate an
	// ephemeral port from the OS (GH#2098, GH#2372).
	if cfg.Port == 0 && sharedEnabled {
		cfg.Port = DefaultSharedServerPort // 3308 - avoids orchestrator conflict on 3307
		cfg.PortSharedServer = true
	}
}

func applyExternalHostConfig(cfg *Config, beadsDir string, sharedMode bool) {
	if cfg.Mode != ServerModeExternal {
		return
	}
	fc := loadDoltServerConfig(beadsDir)
	if configfile.IsLocalHostString(fc.GetDoltServerHost()) {
		return
	}
	applyExternalHostPort(cfg, beadsDir, sharedMode)
}

func loadDoltServerConfig(beadsDir string) *configfile.Config {
	fc := &configfile.Config{}
	if _, err := os.Stat(configfile.ConfigPath(beadsDir)); err != nil {
		return fc
	}
	if loaded, err := configfile.Load(beadsDir); err == nil && loaded != nil {
		return loaded
	}
	return fc
}

func applyExternalHostPort(cfg *Config, beadsDir string, sharedMode bool) {
	// The legacy BEADS_DOLT_PORT override is not in portSources but the
	// storage layer honors it ahead of persisted sources; resolve it the same
	// way here so credentials/probes and the eventual connection agree on the
	// port. BEADS_DOLT_SERVER_PORT (PortSourceEnv) still wins over the legacy
	// spelling.
	if cfg.PortSource != PortSourceEnv {
		if p, err := strconv.Atoi(strings.TrimSpace(os.Getenv("BEADS_DOLT_PORT"))); err == nil && p > 0 {
			cfg.Port = p
			cfg.PortSource = PortSourceEnv
		}
	}
	if cfg.PortSource == PortSourcePortFile {
		resolveExternalHostPortSources(cfg, beadsDir, sharedMode)
	}
	// With no configured port, dial the documented default 3307, not :0 —
	// there is no local Start() to allocate an ephemeral port for a remote
	// server.
	if cfg.Port == 0 {
		cfg.Port = configfile.DefaultDoltServerPort
		cfg.PortSource = PortSourceExternalHostDefault
	}
}

func resolveExternalHostPortSources(cfg *Config, beadsDir string, sharedMode bool) {
	// The gitignored port file is bd's bookkeeping for a bd-owned LOCAL server;
	// pairing it with a remote host dials the remote machine on a stale local
	// ephemeral port. Discard it and resume the precedence chain so lower-
	// priority authoritative sources still apply.
	cfg.Port = 0
	cfg.PortSource = PortSourceUnset
	for _, src := range portSources {
		if src.source == PortSourcePortFile {
			continue
		}
		if port, ok := src.resolve(beadsDir); ok {
			cfg.Port = port
			cfg.PortSource = src.source
			cfg.PortSharedServer = sharedMode
			break
		}
	}
}

// IsRunning checks if a managed server is running for this beadsDir.
// Returns a State with Running=true if a valid dolt process is found.
func IsRunning(beadsDir string) (*State, error) {
	pid, found, err := readManagedPID(beadsDir)
	if err != nil {
		return nil, err
	}
	if !found {
		return &State{Running: false}, nil
	}
	if !managedProcessIsRunning(beadsDir, pid) {
		return &State{Running: false}, nil
	}

	port := runningServerPort(beadsDir)
	if port == 0 {
		stopUnknownPortServer(beadsDir, pid)
		return &State{Running: false}, nil
	}
	return &State{
		Running: true,
		PID:     pid,
		Port:    port,
		DataDir: ResolveDoltDir(beadsDir),
	}, nil
}

func readManagedPID(beadsDir string) (int, bool, error) {
	data, err := os.ReadFile(pidPath(beadsDir))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("reading PID file: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err == nil {
		return pid, true, nil
	}
	// Corrupt PID file implies stale state; clear the port file too.
	clearManagedState(beadsDir)
	return 0, false, nil
}

func clearManagedState(beadsDir string) {
	_ = os.Remove(pidPath(beadsDir))
	_ = os.Remove(portPath(beadsDir))
}

func managedProcessIsRunning(beadsDir string, pid int) bool {
	if !isProcessAlive(pid) {
		// Process is dead — clear all tracked state for this server.
		clearManagedState(beadsDir)
		return false
	}
	if !isDoltProcess(pid) {
		// PID was reused by another process.
		clearManagedState(beadsDir)
		return false
	}
	return true
}

func runningServerPort(beadsDir string) int {
	port := readPortFile(beadsDir)
	if port == 0 {
		port = DefaultConfig(beadsDir).Port
	}
	return port
}

func stopUnknownPortServer(beadsDir string, pid int) {
	// Server is running but we can't determine its port (port file missing, no
	// explicit config). Stop the orphan so EnsureRunning triggers a fresh Start.
	fmt.Fprintf(os.Stderr, "Dolt server (PID %d) running but port unknown; stopping for restart\n", pid)
	if err := gracefulStop(pid, 5*time.Second); err != nil {
		if proc, findErr := os.FindProcess(pid); findErr == nil {
			_ = proc.Kill()
		}
	}
	_ = os.Remove(pidPath(beadsDir))
}
