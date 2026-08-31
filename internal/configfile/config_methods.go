package configfile

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/jonbaldie/beads/internal/storage/backendnames"
)

func (c *Config) Save(beadsDir string) error {
	configPath := ConfigPath(beadsDir)

	saved := *c
	if filepath.IsAbs(saved.DoltDataDir) {
		saved.DoltDataDir = ""
	}

	data, err := json.MarshalIndent(&saved, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	// Write-temp-then-rename: a plain os.WriteFile truncates in place, so a
	// concurrent Load can observe an empty or partial metadata.json and feed
	// store selection a corrupt config. Rename within the same directory is
	// atomic, so readers see either the old or the new file, never a torn one.
	if err := writeFileAtomic(configPath, data, 0o600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	return nil
}

func (c *Config) DatabasePath(beadsDir string) string {
	// Check for custom dolt data directory (absolute path on a faster filesystem).
	// This is useful on WSL where .beads/ lives on NTFS (slow 9P mount) but
	// dolt data can be placed on native ext4 for 5-10x I/O speedup.
	if customDir := c.GetDoltDataDir(); customDir != "" {
		if filepath.IsAbs(customDir) {
			return customDir
		}
		return filepath.Join(beadsDir, customDir)
	}

	if filepath.IsAbs(c.Database) {
		return c.Database
	}
	// Always use "dolt" as the directory name.
	// Stale values like "town", "wyvern", "beads_rig" caused split-brain (see DOLT-HEALTH-P0.md).
	return filepath.Join(beadsDir, "dolt")
}

func (c *Config) GetDeletionsRetentionDays() int {
	if c.DeletionsRetentionDays <= 0 {
		return DefaultDeletionsRetentionDays
	}
	return c.DeletionsRetentionDays
}

func (c *Config) GetStaleClosedIssuesDays() int {
	if c.StaleClosedIssuesDays < 0 {
		return 0
	}
	return c.StaleClosedIssuesDays
}

func (c *Config) GetCapabilities() BackendCapabilities {
	backend := c.GetBackend()
	if backend == BackendDolt && (c.IsDoltServerMode() || c.IsDoltProxiedServerMode()) {
		return BackendCapabilities{SingleProcessOnly: false}
	}
	return CapabilitiesForBackend(backend)
}

func (c *Config) GetBackend() string {
	if c != nil {
		switch c.Backend {
		case BackendPostgres:
			return BackendPostgres
		case BackendMySQL:
			return BackendMySQL
		case BackendSQLite:
			return BackendSQLite
		}
		if backendnames.Has(c.Backend) {
			return c.Backend
		}
	}
	return BackendDolt
}

func (c *Config) GetSQLitePath() string {
	if c == nil {
		return ""
	}
	return c.SQLitePath
}
