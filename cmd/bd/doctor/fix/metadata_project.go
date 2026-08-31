package fix

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage/dolt"
)

// FixProjectIdentity generates a project_id UUID and backfills it into both
// metadata.json and the database metadata table. For pre-GH#2372 projects that
// lack cross-project identity verification.
func FixProjectIdentity(path string) error {
	beadsDir, err := resolvedWorkspaceBeadsDir(path)
	if err != nil {
		return err
	}

	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		return fmt.Errorf("failed to load metadata.json: %w", err)
	}
	if cfg == nil {
		return fmt.Errorf("no metadata.json found")
	}
	if cfg.GetBackend() != configfile.BackendDolt {
		return nil // Not a Dolt backend
	}

	if msg, err := resolveAuthoritativeServerMetadata(path, cfg, true); err != nil {
		return err
	} else if msg != "" {
		fmt.Printf("  %s\n", msg)
		return nil
	}

	ctx := context.Background()

	store, err := dolt.NewFromConfig(ctx, beadsDir)
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	return backfillProjectIdentity(ctx, beadsDir, cfg, store)
}

func backfillProjectIdentity(ctx context.Context, beadsDir string, cfg *configfile.Config, store *dolt.DoltStore) error {
	dbID, _ := store.GetMetadata(ctx, "_project_id")
	resolved, err := projectIdentityIsResolved(cfg.ProjectID, dbID)
	if err != nil {
		return err
	}
	if resolved {
		return nil
	}

	projectID := projectIDToBackfill(cfg.ProjectID, dbID)
	repaired, err := writeProjectIdentity(ctx, beadsDir, cfg, store, projectID, dbID)
	if err != nil {
		return err
	}
	fmt.Printf("  Backfilled project_id %s into: %s\n", projectID, strings.Join(repaired, ", "))
	return nil
}

func projectIdentityIsResolved(localID, dbID string) (bool, error) {
	if localID == "" || dbID == "" {
		return false, nil
	}
	if localID == dbID {
		return true, nil
	}
	return false, fmt.Errorf(
		"project identity mismatch persists after repair attempt: metadata.json=%s, database=%s",
		localID,
		dbID,
	)
}

func projectIDToBackfill(localID, dbID string) string {
	if localID != "" {
		return localID
	}
	if dbID != "" {
		return dbID
	}
	return configfile.GenerateProjectID()
}

func writeProjectIdentity(ctx context.Context, beadsDir string, cfg *configfile.Config, store *dolt.DoltStore, projectID, dbID string) ([]string, error) {
	var repaired []string

	// Backfill metadata.json
	if cfg.ProjectID == "" {
		cfg.ProjectID = projectID
		if err := cfg.Save(beadsDir); err != nil {
			return nil, fmt.Errorf("failed to save project_id to metadata.json: %w", err)
		}
		repaired = append(repaired, "metadata.json")
	}

	// Backfill database
	if dbID == "" {
		if err := store.SetMetadata(ctx, "_project_id", projectID); err != nil {
			return nil, fmt.Errorf("failed to write _project_id to database: %w", err)
		}
		repaired = append(repaired, "database")
	}
	return repaired, nil
}

// FixMissingDoltDatabase detects and repairs missing dolt_database in metadata.json.
// Pre-#2142 migrations created databases without writing dolt_database to config,
// so after upgrading, bd falls back to default "beads" (empty) instead of the real
// database. This fix probes the server for a database with beads tables and backfills
// the config. (GH#2160)
func FixMissingDoltDatabase(path string) error {
	beadsDir, err := resolvedWorkspaceBeadsDir(path)
	if err != nil {
		return err
	}

	cfg, err := configfile.Load(beadsDir)
	if err != nil || cfg == nil {
		return nil // No config, nothing to fix
	}
	if cfg.GetBackend() != configfile.BackendDolt {
		return nil // Not Dolt backend
	}

	if msg, err := resolveAuthoritativeServerMetadata(path, cfg, true); err != nil {
		return err
	} else if msg != "" {
		fmt.Printf("  %s\n", msg)
		return nil
	}

	// Only fall back to schema-only probing when dolt_database is missing.
	if cfg.DoltDatabase != "" {
		return nil
	}

	return repairMissingDoltDatabase(beadsDir, cfg)
}

func repairMissingDoltDatabase(beadsDir string, cfg *configfile.Config) error {

	// Connect to the server and probe for the correct database. No
	// verifyFixTargetIdentity guard here: cfg.DoltDatabase is empty at this
	// point (that's the condition that got us here), so there is no target
	// database identity to verify yet — this call establishes it by probing
	// schema across the server, it does not delete or mutate anything.
	db, _, err := openDoltDB(beadsDir)
	if err != nil {
		fmt.Printf("  dolt_database fix skipped (server not reachable: %v)\n", err)
		return nil
	}
	defer db.Close()

	correctDB := probeForCorrectDoltDatabase(db, configfile.DefaultDoltDatabase)
	if correctDB == "" {
		return nil // No alternate database found
	}

	// Backfill dolt_database in metadata.json
	cfg.DoltDatabase = correctDB
	if err := cfg.Save(beadsDir); err != nil {
		return fmt.Errorf("failed to save metadata.json: %w", err)
	}

	fmt.Printf("  Fixed dolt_database: set to %q in metadata.json (was using default %q)\n",
		correctDB, configfile.DefaultDoltDatabase)
	return nil
}

// probeForCorrectDoltDatabase checks if another database on the server has the
// expected beads tables (issues, dependencies, config). Returns the database name
// if found, empty string otherwise.
func probeForCorrectDoltDatabase(db *sql.DB, skipDB string) string {
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return ""
	}
	defer rows.Close()

	for _, dbName := range doltDatabaseProbeCandidates(rows, skipDB) {
		if doltDatabaseHasIssuesTable(ctx, db, dbName) {
			return dbName
		}
	}

	return ""
}

func doltDatabaseProbeCandidates(rows *sql.Rows, skipDB string) []string {
	skip := map[string]bool{
		"information_schema": true,
		"mysql":              true,
		skipDB:               true,
	}

	var candidates []string
	for rows.Next() {
		var dbName string
		if err := rows.Scan(&dbName); err != nil {
			continue
		}
		if isSkippedDoltDatabase(dbName, skip) {
			continue
		}
		candidates = append(candidates, dbName)
	}
	return candidates
}

func isSkippedDoltDatabase(dbName string, skip map[string]bool) bool {
	if skip[dbName] {
		return true
	}
	// Skip known test databases
	return strings.HasPrefix(dbName, "testdb_") || strings.HasPrefix(dbName, "doctest_") ||
		strings.HasPrefix(dbName, "doctortest_")
}

func doltDatabaseHasIssuesTable(ctx context.Context, db *sql.DB, dbName string) bool {
	var count int
	//nolint:gosec // G201: dbName from SHOW DATABASES, not user input
	err := db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM `%s`.issues LIMIT 1", dbName)).Scan(&count)
	return err == nil
}
