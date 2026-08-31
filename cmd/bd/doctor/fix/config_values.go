package fix

import (
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/configfile"
)

// ConfigValues fixes invalid configuration values in metadata.json.
// Currently handles: database field pointing to SQLite name when backend is Dolt.
func ConfigValues(path string) error {
	beadsDir, cfg, err := loadConfigForValueFix(path)
	if err != nil {
		return err
	}
	if !fixDoltDatabaseField(cfg) {
		fmt.Println("  → No configuration issues to fix")
		return nil
	}
	if err := cfg.Save(beadsDir); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}
	return nil
}

func loadConfigForValueFix(path string) (string, *configfile.Config, error) {
	beadsDir, err := resolvedWorkspaceBeadsDir(path)
	if err != nil {
		return "", nil, err
	}
	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		return "", nil, fmt.Errorf("failed to load config: %w", err)
	}
	if cfg == nil {
		return "", nil, fmt.Errorf("no metadata.json found")
	}
	return beadsDir, cfg, nil
}

func sqliteDatabaseName(name string) bool {
	return strings.HasSuffix(name, ".db") || strings.HasSuffix(name, ".sqlite") || strings.HasSuffix(name, ".sqlite3")
}

func fixDoltDatabaseField(cfg *configfile.Config) bool {
	if cfg.GetBackend() != configfile.BackendDolt || !sqliteDatabaseName(cfg.Database) {
		return false
	}
	fmt.Printf("  Updating database: %q → %q (Dolt backend uses directory)\n", cfg.Database, "dolt")
	cfg.Database = "dolt"
	return true
}
