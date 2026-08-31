package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/debug"
)

// GetIdentity resolves the user's identity for messaging.
// Priority chain:
//  1. flagValue (if non-empty, from --identity flag)
//  2. BEADS_IDENTITY env var / config.yaml identity field (via viper)
//  3. git config user.name
//  4. hostname
//
// This is used as the sender field in bd mail commands.
func GetIdentity(flagValue string) string {
	// 1. Command-line flag takes precedence
	if flagValue != "" {
		return flagValue
	}

	// 2. BEADS_IDENTITY env var or config.yaml identity (viper handles both)
	if identity := GetString("identity"); identity != "" {
		return identity
	}

	// 3. git config user.name
	cmd := exec.Command("git", "config", "user.name")
	if output, err := cmd.Output(); err == nil {
		if gitUser := strings.TrimSpace(string(output)); gitUser != "" {
			return gitUser
		}
	}

	// 4. hostname
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		return hostname
	}

	return "unknown"
}

// NodeID returns the identity of THIS replica: the beads STORE that grants
// and enforces leases here. A lease is enforceable exactly as far as that
// store reaches, and the replica-aware reclaim guard
// (issueops.ReclaimExpiredLeasesInTx) refuses to revert a lease some OTHER
// node granted.
//
// It is read from BEADS_NODE_ID / BD_NODE_ID, or node_id in config.yaml, and
// from nowhere else. It deliberately does NOT fall back to os.Hostname(),
// because the hostname answers the wrong question — it names the client
// PROCESS's machine, not the store:
//
//   - With a shared or remote dolt sql-server (BEADS_DOLT_SERVER_HOST, or any
//     ServerModeExternal deployment — systemd, Docker, Hosted Dolt, a VPS),
//     many hosts are clients of ONE store. There is no sync interval between
//     them and no stale liveness view to defend against, but per-hostname
//     identity would make a supervisor unable to reap any worker's lease —
//     reclaim would return 0 forever and every dead worker's unit would sit
//     in_progress permanently.
//   - In a container the hostname is the container ID, regenerated on every
//     run, so a replaced worker's own single-machine leases would look
//     foreign to its successor.
//   - On macOS/DHCP the transient hostname changes with the network.
//
// Each of those is a fail-CLOSED regression on a deployment that has no
// federation at all, which is a far worse failure than the cross-replica
// reclaim this guard exists to prevent. So the guard is armed only where an
// operator has said, explicitly, that this store is one replica among
// several. "" means "this deployment does not name its replicas" and is the
// default: every consumer degrades to the pre-replica-aware behavior rather
// than fail closed.
func NodeID() string {
	return strings.TrimSpace(GetString("node_id"))
}

// FederationConfig holds the federation (Dolt remote) configuration.
type FederationConfig struct {
	Remote       string      // dolthub://org/beads, gs://bucket/beads, s3://bucket/beads
	Sovereignty  Sovereignty // T1, T2, T3, T4
	ExcludeTypes []string    // issue types excluded from federation push (e.g. ["wisp"])
}

// GetFederationConfig returns the current federation configuration.
func GetFederationConfig() FederationConfig {
	return FederationConfig{
		Remote:       GetString("federation.remote"),
		Sovereignty:  GetSovereignty(),
		ExcludeTypes: GetStringSlice("federation.exclude_types"),
	}
}

// GetCustomTypesFromYAML retrieves custom issue types from config.yaml.
// This is used as a fallback when the database doesn't have types.custom set yet
// (e.g., during bd init auto-import before the database is fully configured).
// Returns nil if no custom types are configured in config.yaml.
func GetCustomTypesFromYAML() []string {
	return getConfigList("types.custom")
}

// GetInfraTypesFromYAML retrieves infrastructure type names from config.yaml.
// Infrastructure types are routed to the wisps table instead of the versioned issues table.
// Returns nil if no infra types are configured in config.yaml (caller should use defaults).
func GetInfraTypesFromYAML() []string {
	return getConfigList("types.infra")
}

// GetCustomStatusesFromYAML retrieves custom statuses from config.yaml.
// This is used as a fallback when the database doesn't have status.custom set yet
// or when the database connection is temporarily unavailable.
// Returns nil if no custom statuses are configured in config.yaml.
func GetCustomStatusesFromYAML() []string {
	return getConfigList("status.custom")
}

// MetadataValidationMode returns the metadata schema validation mode.
// Returns "none" if config is not initialized or mode is empty/unknown.
func MetadataValidationMode() string {
	if v == nil {
		return "none"
	}
	mode := v.GetString("validation.metadata.mode")
	switch mode {
	case "warn", "error":
		return mode
	default:
		return "none"
	}
}

// MetadataSchemaFields returns the raw field definitions from config.
// Returns nil if config is not initialized or no fields are defined.
// Each entry maps field name → map of properties (type, values, required, min, max).
func MetadataSchemaFields() map[string]interface{} {
	if v == nil {
		return nil
	}
	raw := v.Get("validation.metadata.fields")
	if raw == nil {
		return nil
	}
	// Viper returns map[string]interface{} for nested YAML maps
	if m, ok := raw.(map[string]interface{}); ok {
		return m
	}
	return nil
}

// DefaultAgentsFile is the default filename for agent instructions.
const DefaultAgentsFile = "AGENTS.md"

// AgentsFile returns the configured agents instruction filename.
// Returns DefaultAgentsFile ("AGENTS.md") if no custom value is set.
// Note: Use SafeAgentsFile() when the value will be used for file I/O,
// as config.yaml may be manually edited with invalid values.
func AgentsFile() string {
	if name := GetString("agents.file"); name != "" {
		return name
	}
	return DefaultAgentsFile
}

// SafeAgentsFile returns the configured agents filename after validation.
// If the stored config value is invalid (e.g. manually edited with traversal
// paths), it falls back to DefaultAgentsFile and logs a warning.
func SafeAgentsFile() string {
	name := AgentsFile()
	if err := ValidateAgentsFile(name); err != nil {
		debug.Logf("config: agents.file %q failed validation (%v), using default", name, err)
		return DefaultAgentsFile
	}
	return name
}

// ValidateAgentsFile checks that filename is safe to use as an agents file path.
// It rejects absolute paths, path separators, names longer than 255 characters,
// and non-markdown extensions. This is a pure string validation function — I/O
// checks (e.g. symlink detection) are deferred to the file write layer.
func ValidateAgentsFile(filename string) error {
	if filename == "" {
		return fmt.Errorf("agents file name must not be empty")
	}
	if len(filename) > 255 {
		return fmt.Errorf("agents file name exceeds 255 characters")
	}
	if strings.ContainsAny(filename, "/\\") {
		return fmt.Errorf("agents file must be a simple filename without path separators, got %q", filename)
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if ext != ".md" {
		return fmt.Errorf("agents file must have .md extension, got %q", ext)
	}
	return nil
}

// getConfigList retrieves a list-typed configuration value from config.yaml,
// accepting either the YAML list form (e.g. `types: { custom: [step, wisp] }`)
// or the legacy comma-separated string form (e.g.
// `types.custom = "step,wisp"`). Entries are trimmed; empty entries are
// dropped. The dual-form support is required for project-extension
// types/statuses declared in .beads/config.yaml — see gastownhall/beads#4024.
func getConfigList(key string) []string {
	if v == nil {
		debug.Logf("config: viper not initialized, returning nil for key %q", key)
		return nil
	}

	// Try the YAML-list form first. Viper's GetStringSlice returns:
	//   * []string for a YAML sequence value,
	//   * []string{value} when the underlying value is a single string,
	//   * nil/empty when the key is unset.
	// Re-splitting each entry on comma covers the case where the entry is
	// itself a comma-separated string (legacy form bound via GetStringSlice).
	if slice := v.GetStringSlice(key); len(slice) > 0 {
		result := splitConfigListEntries(slice)
		if len(result) > 0 {
			return result
		}
	}

	// Fallback to direct string retrieval for the comma-separated form when
	// GetStringSlice didn't surface a value (e.g. some viper builds short-
	// circuit GetStringSlice for pure-string values).
	value := v.GetString(key)
	if value == "" {
		return nil
	}
	return splitConfigListEntries([]string{value})
}

func splitConfigListEntries(entries []string) []string {
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		for _, part := range strings.Split(entry, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				result = append(result, trimmed)
			}
		}
	}
	return result
}
