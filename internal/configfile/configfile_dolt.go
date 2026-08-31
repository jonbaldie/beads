package configfile

import (
	"crypto/rand"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/config"
)

// IsDoltServerMode returns true if Dolt should connect via sql-server.
// Server mode is the standard connection method.
//
// Checks (in priority order):
//  1. BEADS_DOLT_SERVER_MODE=1 env var
//  2. BEADS_DOLT_SHARED_SERVER env var (shared-server implies server mode)
//  3. BEADS_DOLT_SERVER_HOST env var set to a non-localhost value (GH#3545):
//     an operator who configures a remote host but forgets the mode flag
//     would otherwise silently fall through to embedded storage. Setting
//     the env var to an explicit localhost value (or empty string) is
//     treated as a deliberate suppression of host-based inference, not
//     merely "no signal" — it also suppresses the struct/config.yaml host
//     inference below.
//  4. dolt_mode field in metadata.json (project-local, explicit) — always
//     wins over any persisted or configured host, since it is the
//     operator's explicit statement of intent.
//  5. Non-localhost DoltServerHost persisted in metadata.json (GH#3545),
//     when no explicit dolt_mode is set and env didn't suppress inference.
//  6. dolt.mode in config.yaml (user-global fallback, only when
//     metadata.json has no mode)
//  7. Non-localhost dolt.host in config.yaml (GH#3545), gated the same way
//     as (5) — a configured host must mean server mode.
//
// Runtime env vars take precedence over persisted metadata.json to prevent
// stale dolt_mode=embedded from overriding active server intent (GH#2949).
func (c *Config) IsDoltServerMode() bool {
	if c.GetBackend() != BackendDolt {
		return false
	}
	if os.Getenv("BEADS_DOLT_SERVER_MODE") == "1" {
		return true
	}
	// Shared-server mode implies server-backed storage. Check env var
	// directly to avoid circular import with doltserver package.
	if v := os.Getenv("BEADS_DOLT_SHARED_SERVER"); v == "1" || strings.EqualFold(v, "true") {
		return true
	}
	// Host-based inference (GH#3545): a configured non-localhost host
	// implies server mode. Centralized in HostImpliesServerMode so the
	// storage-mode resolver (here) and the lifecycle resolver
	// (doltserver.ResolveServerMode) cannot disagree.
	if c.HostImpliesServerMode() {
		return true
	}
	if c.DoltMode != "" {
		// metadata.json has an explicit mode — respect it over any
		// persisted host and over config.yaml.
		return strings.ToLower(c.DoltMode) == DoltModeServer
	}
	// Fall back to config.yaml dolt.mode setting (no metadata.json mode set)
	return strings.EqualFold(config.GetYamlConfig("dolt.mode"), "server")
}

// HostImpliesServerMode evaluates host-based server-mode inference
// (GH#3545) against the EFFECTIVE host precedence chain (env >
// metadata.json > config.yaml, mirroring GetDoltServerHost), so that a
// lower-priority host that GetDoltServerHost would ignore can never
// drive the inference. Rules, in order:
//
//   - Proxied-server workspaces are exempt: IsDoltServerMode and
//     IsDoltProxiedServerMode are mutually exclusive (the
//     proxied-to-server migration depends on it).
//   - A non-empty BEADS_DOLT_SERVER_HOST decides alone: a non-localhost
//     value implies server mode even over explicit dolt_mode=embedded
//     (runtime env beats stale metadata, GH#2949); an explicit
//     localhost value suppresses inference from lower-priority hosts —
//     matching GetDoltServerHost, which then dials locally. An EMPTY
//     env value is ignored (behaves as unset), again matching
//     GetDoltServerHost: treating empty as suppression would select
//     embedded storage while the effective dial host stays remote —
//     exactly the split-storage failure this inference fixes.
//   - An explicit metadata.json dolt_mode disables inference from
//     persisted/configured hosts.
//   - A metadata.json dolt_server_host, when set, decides alone (a
//     lower-priority config.yaml dolt.host is not consulted, matching
//     GetDoltServerHost precedence).
//   - An explicit config.yaml dolt.mode disables inference from the
//     config.yaml dolt.host.
func (c *Config) HostImpliesServerMode() bool {
	if strings.EqualFold(c.DoltMode, DoltModeProxiedServer) {
		return false
	}
	if h := os.Getenv("BEADS_DOLT_SERVER_HOST"); h != "" {
		return !IsLocalHostString(h)
	}
	if c.DoltMode != "" {
		return false
	}
	if c.DoltServerHost != "" {
		return !IsLocalHostString(c.DoltServerHost)
	}
	if config.GetYamlConfig("dolt.mode") != "" {
		return false
	}
	if h := config.GetYamlConfig("dolt.host"); h != "" {
		return !IsLocalHostString(h)
	}
	return false
}

// IsLocalHostString reports whether host refers to the local machine, for
// the purposes of mode inference. Mirrors the helpers in cmd/bd/dolt.go
// and internal/storage/dolt/store.go; kept private to this package to
// limit the blast radius of GH#3545's fix — consolidating the (now four)
// definitions is a separate cleanup.
func IsLocalHostString(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "", "localhost", "127.0.0.1", "::1", "[::1]", "0.0.0.0":
		return true
	}
	return false
}

func (c *Config) IsDoltProxiedServerMode() bool {
	if c.GetBackend() != BackendDolt {
		return false
	}
	return strings.ToLower(c.DoltMode) == DoltModeProxiedServer
}

// IsTeamServerManaged reports whether the database's schema and identity are
// owned by beads-team-server (bts). Only meaningful in proxied-server mode.
func (c *Config) IsTeamServerManaged() bool {
	return c.IsDoltProxiedServerMode() && c.DoltTeamServer
}

// GetDoltMode returns the Dolt connection mode, defaulting to server.
// GetDoltMode returns the EFFECTIVE mode: when no mode is persisted but
// host-based inference (GH#3545) selects server mode, it reports
// "server" so effective-context consumers (bd context, integrations)
// agree with IsDoltServerMode instead of advertising an embedded mode
// next to a remote server endpoint.
func (c *Config) GetDoltMode() string {
	if c.DoltMode == "" {
		if c.HostImpliesServerMode() {
			return DoltModeServer
		}
		return DoltModeEmbedded
	}
	return c.DoltMode
}

// GetDoltServerHost returns the Dolt server host.
// Priority: BEADS_DOLT_SERVER_HOST env var > metadata.json dolt_server_host
// > config.yaml / global config dolt.host > DefaultDoltServerHost.
// The config.yaml layer mirrors the dolt.port fix (GH#2073) so a shared
// team / user-level Dolt server can be configured once without per-clone
// metadata.json edits.
func (c *Config) GetDoltServerHost() string {
	if h := os.Getenv("BEADS_DOLT_SERVER_HOST"); h != "" {
		return h
	}
	if c.DoltServerHost != "" {
		return c.DoltServerHost
	}
	if h := config.GetYamlConfig("dolt.host"); h != "" {
		return h
	}
	return DefaultDoltServerHost
}

// Deprecated: Use doltserver.DefaultConfig(beadsDir).Port instead.
// This method falls back to 3307 which is wrong for standalone mode
// (where the port is an OS-assigned ephemeral port).
// Kept for backward compatibility with external consumers.
//
// GetDoltServerPort returns the Dolt server port.
// Checks BEADS_DOLT_SERVER_PORT env var first, then BEADS_DOLT_PORT (orchestrator sets this),
// then config, then default.
func (c *Config) GetDoltServerPort() int {
	if p := os.Getenv("BEADS_DOLT_SERVER_PORT"); p != "" {
		if port, err := strconv.Atoi(p); err == nil {
			return port
		}
	}
	if p := os.Getenv("BEADS_DOLT_PORT"); p != "" {
		if port, err := strconv.Atoi(p); err == nil {
			return port
		}
	}
	if c.DoltServerPort > 0 {
		return c.DoltServerPort
	}
	return DefaultDoltServerPort
}

// GetDoltServerSocket returns the Dolt server Unix domain socket path.
// Checks BEADS_DOLT_SERVER_SOCKET env var first, then config. Empty means use TCP.
func (c *Config) GetDoltServerSocket() string {
	if s := os.Getenv("BEADS_DOLT_SERVER_SOCKET"); s != "" {
		return s
	}
	return c.DoltServerSocket
}

// GetDoltServerUser returns the Dolt server MySQL user.
// Checks BEADS_DOLT_SERVER_USER env var first, then config, then default.
func (c *Config) GetDoltServerUser() string {
	if u := os.Getenv("BEADS_DOLT_SERVER_USER"); u != "" {
		return u
	}
	if c.DoltServerUser != "" {
		return c.DoltServerUser
	}
	return DefaultDoltServerUser
}

// GetDoltCredentialCommand returns the server credential command:
// BEADS_DOLT_CREDENTIAL_COMMAND. Empty means no command — the static
// BEADS_DOLT_SERVER_USER / dolt_server_user path applies. The command's stdout is a
// short-lived token (bare, or a {token,expirationTimestamp} / {access_token,expires_in}
// envelope) presented as the connection username to an authenticating gateway server,
// which verifies it and routes to the database. It is deliberately read from the
// environment only, NOT metadata.json: a metadata-sourced command is arbitrary code run
// on open, so persisting it waits on a workspace-trust gate.
func (c *Config) GetDoltCredentialCommand() string {
	return os.Getenv("BEADS_DOLT_CREDENTIAL_COMMAND")
}

// GetDoltDatabase returns the Dolt SQL database name.
// Checks BEADS_DOLT_SERVER_DATABASE env var first, then config, then default.
func (c *Config) GetDoltDatabase() string {
	if d := os.Getenv("BEADS_DOLT_SERVER_DATABASE"); d != "" {
		return d
	}
	if c.DoltDatabase != "" {
		return c.DoltDatabase
	}
	return DefaultDoltDatabase
}

// GetGlobalDoltDatabase returns the global database name for shared-server mode.
// Returns empty string if no global database is configured.
func (c *Config) GetGlobalDoltDatabase() string {
	return c.GlobalDoltDatabase
}

func (c *Config) GetGlobalProjectID() string {
	return c.GlobalProjectID
}

// GetDoltServerPassword returns the Dolt server password.
// Checks in order:
//  1. BEADS_DOLT_PASSWORD env var (highest priority, existing behavior)
//  2. Credentials file lookup by [host:port] section
//     (path from BEADS_CREDENTIALS_FILE env var, or ~/.config/beads/credentials)
//  3. Empty string (no password)
//
// Note: uses the port from configfile (metadata.json / env var), which may differ
// from the resolved runtime port (doltserver port file). If you have the resolved
// port, prefer GetDoltServerPasswordForPort for correct credentials file lookup.
func (c *Config) GetDoltServerPassword() string {
	return c.GetDoltServerPasswordForPort(c.GetDoltServerPort())
}

// GetDoltServerPasswordForPort returns the Dolt server password using an explicit
// port for the credentials file lookup. Use this when the resolved runtime port
// (from doltserver.DefaultConfig) differs from the configfile port (metadata.json).
//
// This avoids a mismatch where metadata.json says port 3308 (tunnel) but the
// doltserver port file says 3307 (local), causing the credentials file lookup
// to use the wrong [host:port] section.
func (c *Config) GetDoltServerPasswordForPort(port int) string {
	if p := os.Getenv("BEADS_DOLT_PASSWORD"); p != "" {
		return p
	}
	host := c.GetDoltServerHost()
	if p := LookupCredentialsPassword(host, port); p != "" {
		return p
	}
	return ""
}

// GetDoltServerTLS returns whether TLS is enabled for server connections.
// Required for Hosted Dolt instances.
// Checks BEADS_DOLT_SERVER_TLS env var first ("1" or "true"), then config.
func (c *Config) GetDoltServerTLS() bool {
	if t := os.Getenv("BEADS_DOLT_SERVER_TLS"); t != "" {
		return t == "1" || strings.ToLower(t) == "true"
	}
	return c.DoltServerTLS
}

// GetDoltDataDir returns the custom dolt data directory path.
// When set, dolt stores its data in this directory instead of .beads/dolt/.
// This is useful on WSL where the project lives on a slow NTFS mount (9P)
// but dolt data can be placed on native ext4 for significantly better I/O.
// Checks BEADS_DOLT_DATA_DIR env var first, then config.
func (c *Config) GetDoltDataDir() string {
	if d := os.Getenv("BEADS_DOLT_DATA_DIR"); d != "" {
		return d
	}
	return c.DoltDataDir
}

// GetDoltRemotesAPIPort returns the Dolt remotesapi port used for federation.
// Checks BEADS_DOLT_REMOTESAPI_PORT env var first, then config, then default (8080).
func (c *Config) GetDoltRemotesAPIPort() int {
	if p := os.Getenv("BEADS_DOLT_REMOTESAPI_PORT"); p != "" {
		if port, err := strconv.Atoi(p); err == nil {
			return port
		}
	}
	if c.DoltRemotesAPIPort > 0 {
		return c.DoltRemotesAPIPort
	}
	return DefaultDoltRemotesAPIPort
}

// GenerateProjectID creates a UUID v4 for project identity verification (GH#2372).
func GenerateProjectID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback: use timestamp + PID as a unique-enough identifier
		return fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
	}
	// Set version (4) and variant (RFC 4122)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
