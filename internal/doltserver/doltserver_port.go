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
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
)

// Config holds the server configuration.
type Config struct {
	BeadsDir string     // Path to .beads/ directory
	Port     int        // MySQL protocol port (0 = allocate ephemeral port on Start)
	Host     string     // Bind address (default: 127.0.0.1)
	Mode     ServerMode // Server ownership mode (Owned, External, Embedded)

	// PortSource records which step of the precedence chain (see
	// portSources) resolved Port. PortSourceUnset when Port == 0. Callers
	// that auto-start a replacement server use this to decide whether
	// silently retargeting to a different port is safe (GH#4052): safe for
	// bd's own port-file bookkeeping, not safe for a source where the user
	// (or config on the user's behalf) explicitly asserted the port.
	PortSource PortSource

	// PortSharedServer records whether Port was resolved in shared-server
	// mode (BEADS_DOLT_SHARED_SERVER=1): either because port resolution
	// consulted the shared server directory's sources, or because no source
	// resolved a port and DefaultConfig fell back to DefaultSharedServerPort.
	// Orthogonal to PortSource — a shared-mode port can come from any source
	// (env, port file, config.yaml, ...). Callers use this together with
	// PortSource.IsAuthoritative() to decide whether auto-starting a
	// repo-local server on a different port is safe: it never is here,
	// because the shared server is a different database than whatever
	// auto-start would create locally (GH#4052).
	PortSharedServer bool
}

// State holds runtime information about a managed server.
type State struct {
	Running bool   `json:"running"`
	PID     int    `json:"pid"`
	Port    int    `json:"port"`
	DataDir string `json:"data_dir"`
}

// file paths within .beads/
func pidPath(beadsDir string) string  { return filepath.Join(beadsDir, PIDFileName) }
func logPath(beadsDir string) string  { return filepath.Join(beadsDir, "dolt-server.log") }
func lockPath(beadsDir string) string { return filepath.Join(beadsDir, "dolt-server.lock") }
func portPath(beadsDir string) string { return filepath.Join(beadsDir, PortFileName) }

// MaxDoltServers is the hard ceiling on concurrent dolt sql-server processes.
// Allows up to 3 (e.g., multiple projects).
func maxDoltServers() int {
	return 3
}

// allocateEphemeralPort asks the OS for a free TCP port on host.
// It binds to port 0, reads the assigned port, and closes the listener.
// The caller should pass the returned port to dolt sql-server promptly
// to minimize the TOCTOU window.
func allocateEphemeralPort(host string) (int, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return 0, fmt.Errorf("allocating ephemeral port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}

// isPortAvailable checks if a TCP port is available for binding.
func isPortAvailable(host string, port int) bool {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// reclaimPort ensures an explicit (user-configured) port is available for use.
// Only called for explicit ports (env var, config.yaml, metadata.json).
// If the port is busy:
//   - If our dolt server (same data dir) → return its PID for adoption
//   - If a stale/orphan dolt sql-server holds it → kill it and reclaim
//   - If another project's dolt or a non-dolt process → return error
//
// Returns (adoptPID, nil) when an existing server should be adopted.
// Returns (0, nil) when the port is free for a new server.
// Returns (0, err) when the port can't be used.
func reclaimPort(host string, port int, beadsDir string) (adoptPID int, err error) {
	if isPortAvailable(host, port) {
		return 0, nil // port is free
	}

	// Port is busy — find out what's using it
	pid := findPIDOnPort(port)
	if pid == 0 {
		// Can't identify the process; port may be in TIME_WAIT or transient use.
		// Wait briefly and retry.
		time.Sleep(2 * time.Second)
		if isPortAvailable(host, port) {
			return 0, nil
		}
		return 0, fmt.Errorf("port %d is busy but cannot identify the process.\n\n%s", port, portConflictDiagnostics(port))
	}

	// Check if it's a dolt sql-server process
	if !isDoltProcess(pid) {
		return 0, fmt.Errorf("port %d is in use by a non-dolt process (PID %d).\n\n%s\n\nFree the port or configure a different one with: bd dolt set port <port>", port, pid, portConflictDiagnostics(port))
	}

	// It's a dolt process. Check if it's one we should adopt.

	// Check if the process is using our data directory (CWD matches our dolt dir).
	// dolt sql-server is started with cmd.Dir = doltDir, so CWD is the data dir.
	doltDir := ResolveDoltDir(beadsDir)
	if isProcessInDir(pid, doltDir) {
		return pid, nil // our server — adopt it
	}

	// Another beads project's Dolt server is on this port.
	return 0, fmt.Errorf("port %d is in use by another project's dolt server (PID %d).\n\n%s\n\nFree the port or use a different one with: bd dolt set port <port>", port, pid, portConflictDiagnostics(port))
}

// portConflictDiagnostics returns a multi-line block of operator-actionable
// hints for diagnosing what's holding a port. Combines the platform-specific
// listener-discovery command with a docker-in-the-loop hint that frequently
// applies in practice — operators running their own dolt sql-server in a
// container don't realize bd would otherwise try to start a competing
// instance and lose the race (GH#3516).
func portConflictDiagnostics(port int) string {
	return fmt.Sprintf("Identify the listener:\n  %s\n\n"+
		"If the listener is YOUR own Dolt instance (e.g., a docker container "+
		"or systemd unit you manage), bd does not need to start a new server. "+
		"Configure bd to talk to the existing server instead:\n"+
		"  export BEADS_DOLT_SERVER_HOST=<host>  # 127.0.0.1 for local container\n"+
		"  export BEADS_DOLT_SERVER_PORT=%d\n"+
		"  bd dolt status   # verify reachable",
		fmt.Sprintf(portConflictHint, port), port)
}

// countDoltProcesses returns the number of running dolt sql-server processes.
func countDoltProcesses() int { return len(listDoltProcessPIDs()) }

// isDoltProcess checks if a PID belongs to a running dolt sql-server.
func isDoltProcess(pid int) bool {
	for _, p := range listDoltProcessPIDs() {
		if p == pid {
			return true
		}
	}
	return false
}

// readPortFile reads the actual port from the port file, if it exists.
// Returns 0 if the file doesn't exist or is unreadable.
func readPortFile(beadsDir string) int {
	data, err := os.ReadFile(portPath(beadsDir))
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return port
}

// writePortFile records the actual port the server is listening on.
// Write-temp-then-rename: a plain os.WriteFile truncates in place, so a
// concurrent readPortFile can observe an empty or partial file and resolve
// port 0 (or a truncated port). Rename within .beads is atomic.
func writePortFile(beadsDir string, port int) error {
	path := portPath(beadsDir)
	tmp, err := os.CreateTemp(beadsDir, PortFileName+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // no-op after successful rename
	if _, err := tmp.WriteString(strconv.Itoa(port)); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// EnsurePortFile makes the repo-local port file match the connected server port.
// This is a best-effort repair path for upgraded repos that are missing
// .beads/dolt-server.port even though commands can still connect.
func EnsurePortFile(beadsDir string, port int) error {
	if beadsDir == "" || port <= 0 {
		return nil
	}
	existing := readPortFile(beadsDir)
	if existing == port {
		return nil
	}
	if existing > 0 {
		fmt.Fprintf(os.Stderr, "Info: updating port file %d → %d in %s\n", existing, port, beadsDir)
	}
	return writePortFile(beadsDir, port)
}

// ReadPortFile returns the port from the project's dolt-server.port file,
// or 0 if the file doesn't exist or is invalid. Exported for use by bd init
// to detect whether this project has its own running server (GH#2336).
func ReadPortFile(beadsDir string) int {
	return readPortFile(beadsDir)
}

// PortFileSnapshot captures the exact prior contents of a project's port
// file, so a caller that speculatively lets EnsureRunningDetailed write a
// new one can restore the pre-call state exactly if it later decides not to
// use the new server (GH#4052 fail-closed paths). Existed is false when the
// file did not exist before the snapshot; in that case Data is nil and
// RestorePortFile removes the file rather than rewriting it.
type PortFileSnapshot struct {
	Data    []byte
	Existed bool
}

// SnapshotPortFile captures the current on-disk state of beadsDir's port
// file (see PortFileSnapshot). Read-only; does not mutate anything.
func SnapshotPortFile(beadsDir string) (PortFileSnapshot, error) {
	data, err := os.ReadFile(portPath(beadsDir))
	if err != nil {
		if os.IsNotExist(err) {
			return PortFileSnapshot{}, nil
		}
		return PortFileSnapshot{}, err
	}
	// Copy: os.ReadFile's backing array should not be retained/mutated by
	// later callers of the same path.
	cp := make([]byte, len(data))
	copy(cp, data)
	return PortFileSnapshot{Data: cp, Existed: true}, nil
}

// RestorePortFile restores beadsDir's port file to the state captured by
// snap: removes the file if it did not exist before, or rewrites it with the
// exact prior bytes (write-temp-then-rename, matching writePortFile, so a
// concurrent reader never observes a partially written file). Used on
// fail-closed auto-start paths (GH#4052) that must leave no port-file trace
// of a server they decided not to use.
func RestorePortFile(beadsDir string, snap PortFileSnapshot) error {
	path := portPath(beadsDir)
	if !snap.Existed {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	tmp, err := os.CreateTemp(beadsDir, PortFileName+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // no-op after successful rename
	if _, err := tmp.Write(snap.Data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func configYamlPort(beadsDir string) int {
	path := filepath.Join(ResolveDoltDir(beadsDir), "config.yaml")
	if _, err := os.Stat(path); err != nil {
		return 0
	}
	body, err := os.ReadFile(path) //nolint:gosec // G304: path is the internally resolved Dolt config location.
	if err != nil {
		return 0
	}
	var cfg doltServerYAMLConfig
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		return 0
	}
	return cfg.Listener.Port
}

// PortSource identifies which step of the port-resolution precedence chain
// (see portSources) produced a Config's Port. Callers use it to distinguish
// a port the user explicitly asserted (authoritative) from bd's own
// bookkeeping (the gitignored port file), which auto-start is free to
// replace without confirmation (GH#4052).
type PortSource string

const (
	// PortSourceUnset means no source resolved a port (Port == 0); Start()
	// will allocate an ephemeral port.
	PortSourceUnset PortSource = ""
	// PortSourceEnv is the BEADS_DOLT_SERVER_PORT environment variable.
	PortSourceEnv PortSource = "env"
	// PortSourcePortFile is the gitignored .beads/dolt-server.port file that
	// bd itself writes and rewrites as part of normal server bookkeeping.
	PortSourcePortFile PortSource = "port_file"
	// PortSourceDoltConfigYaml is the Dolt server's own config.yaml
	// (listener.port) in the dolt data directory.
	PortSourceDoltConfigYaml PortSource = "dolt_config_yaml"
	// PortSourceConfigYaml is Beads' project/global config.yaml (dolt.port).
	PortSourceConfigYaml PortSource = "config_yaml"
	// PortSourceGlobalConfig is retained as an alias of PortSourceConfigYaml
	// for callers that think of it as "global config" (~/.config/bd/config.yaml
	// resolves through the same config.GetYamlConfig("dolt.port") lookup).
	PortSourceGlobalConfig = PortSourceConfigYaml
	// PortSourceMetadataJSON is the deprecated metadata.json dolt_server_port
	// fallback. Still an explicit user/tooling assertion, not bd bookkeeping.
	PortSourceMetadataJSON PortSource = "metadata_json"
	// PortSourceExternalHostDefault is the documented default port (3307)
	// assumed for a host-inferred external server when no port source
	// resolved (GH#3545): the user asserted a remote host, so bd fills in
	// the default port rather than dialing :0 or allocating locally.
	PortSourceExternalHostDefault PortSource = "external_host_default"
)

// IsAuthoritative reports whether this source represents a user (or
// tooling-on-the-user's-behalf) assertion of where the Dolt server lives, as
// opposed to bd's own bookkeeping (the port file). Auto-start may freely
// replace a non-authoritative port; it must not silently replace an
// authoritative one (GH#4052).
func (s PortSource) IsAuthoritative() bool {
	switch s {
	case PortSourceEnv, PortSourceDoltConfigYaml, PortSourceConfigYaml, PortSourceMetadataJSON:
		return true
	default:
		return false
	}
}

// portSource is one step in the port-resolution precedence chain that
// DefaultConfig walks. resolve reports (port, true) when this source has a
// usable value; DefaultConfig stops at the first true.
type portSource struct {
	label   string
	source  PortSource
	resolve func(beadsDir string) (int, bool)
}

// portSources is the single precedence chain for server-port resolution.
// DefaultConfig consumes it behaviorally; PortSourceLabels exposes the same
// slice's labels so callers like `bd dolt show-config` render the actual
// chain instead of hand-copying it out of sync (GH#4511).
var portSources = []portSource{
	{
		label:  "Environment variable (BEADS_DOLT_SERVER_PORT)",
		source: PortSourceEnv,
		resolve: func(beadsDir string) (int, bool) {
			p := os.Getenv("BEADS_DOLT_SERVER_PORT")
			if p == "" {
				return 0, false
			}
			port, err := strconv.Atoi(p)
			return port, err == nil
		},
	},
	{
		// Gitignored, local-only. Elevated above the YAML sources to prevent
		// git-tracked values from causing cross-project data leakage (GH#2372).
		label:  "Port file (.beads/dolt-server.port; shared-server dir in shared mode)",
		source: PortSourcePortFile,
		resolve: func(beadsDir string) (int, bool) {
			p := readPortFile(beadsDir)
			return p, p > 0
		},
	},
	{
		label:  "Dolt server config.yaml (listener.port, in the dolt data directory)",
		source: PortSourceDoltConfigYaml,
		resolve: func(beadsDir string) (int, bool) {
			p := configYamlPort(beadsDir)
			return p, p > 0
		},
	},
	{
		// ~/.config/bd/config.yaml or project config.yaml (GH#2073). Git-tracked,
		// so it ranks below the port file above.
		label:  "Beads config.yaml / global config (dolt.port)",
		source: PortSourceConfigYaml,
		resolve: func(beadsDir string) (int, bool) {
			p := config.GetYamlConfig("dolt.port")
			if p == "" {
				return 0, false
			}
			port, err := strconv.Atoi(p)
			return port, err == nil && port > 0
		},
	},
	{
		// Deprecated: git-tracked, propagates to all contributors, causing
		// cross-project data leakage (GH#2372). Kept as a fallback so existing
		// setups don't break silently.
		label:  "metadata.json dolt_server_port (deprecated fallback)",
		source: PortSourceMetadataJSON,
		resolve: func(beadsDir string) (int, bool) {
			metaCfg, err := configfile.Load(beadsDir)
			if err != nil || metaCfg == nil || metaCfg.DoltServerPort <= 0 {
				return 0, false
			}
			fmt.Fprintf(os.Stderr, "Warning: dolt_server_port in metadata.json is deprecated (can cause cross-project data leakage).\n")
			fmt.Fprintf(os.Stderr, "  The port file (.beads/dolt-server.port) is now the primary source.\n")
			fmt.Fprintf(os.Stderr, "  Remove dolt_server_port from .beads/metadata.json to silence this warning.\n")
			return metaCfg.DoltServerPort, true
		},
	},
}

// PortSourceLabels returns the operator-facing description of each step in
// the port-resolution precedence chain, in priority order.
func PortSourceLabels() []string {
	labels := make([]string, len(portSources))
	for i, s := range portSources {
		labels[i] = s.label
	}
	return labels
}
