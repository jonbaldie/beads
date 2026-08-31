package fix

import (
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/configfile"
)

// DatabaseConfig auto-detects and fixes metadata.json database config mismatches.
// This fix only applies to SQLite backends where .db files on disk may not match
// the configured database name. Dolt backends store data on a server, so there
// are no local .db files to reconcile.
func DatabaseConfig(path string) error {
	beadsDir, err := resolvedWorkspaceBeadsDir(path)
	if err != nil {
		return err
	}

	// Load existing config
	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if cfg == nil {
		// No config exists - nothing to fix
		return fmt.Errorf("no metadata.json found")
	}

	// Dolt backend stores data on the server — no local .db files to reconcile
	if cfg.GetBackend() == configfile.BackendDolt {
		return fmt.Errorf("database config fix not applicable for Dolt backend (data is on the server)")
	}

	fixed := false

	// Check if configured database name matches the actual .db file on disk
	actualDB := findActualDBFile(beadsDir)
	if actualDB != "" && cfg.Database != actualDB {
		fmt.Printf("  Updating database: %s → %s\n", cfg.Database, actualDB)
		cfg.Database = actualDB
		fixed = true
	}

	if !fixed {
		return fmt.Errorf("no configuration mismatches detected")
	}

	// Save updated config
	if err := cfg.Save(beadsDir); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	return nil
}

// findActualDBFile scans .beads/ for the actual SQLite database file in use.
// Only finds .db files; does not handle Dolt directories.
// Prefers beads.db (canonical name), skips backups and vc.db.
func findActualDBFile(beadsDir string) string {
	entries, err := os.ReadDir(beadsDir)
	if err != nil {
		return ""
	}
	candidates := sqliteDBFileCandidates(entries)
	if len(candidates) == 0 {
		return ""
	}
	for _, name := range candidates {
		if name == "beads.db" {
			return name
		}
	}
	return candidates[0]
}

func isSQLiteDBCandidate(entry os.DirEntry) bool {
	if entry.IsDir() {
		return false
	}
	name := entry.Name()
	if !strings.HasSuffix(name, ".db") {
		return false
	}
	return !strings.Contains(name, "backup") && name != "vc.db"
}

func sqliteDBFileCandidates(entries []os.DirEntry) []string {
	var candidates []string
	for _, entry := range entries {
		if isSQLiteDBCandidate(entry) {
			candidates = append(candidates, entry.Name())
		}
	}
	return candidates
}
