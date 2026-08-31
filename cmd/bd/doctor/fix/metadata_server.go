package fix

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage/doltutil"
)

// ResolveAuthoritativeServerMetadata reconciles local metadata.json against the
// authoritative server state. In shared-server/server mode this repairs two
// drift cases without guessing across unrelated projects:
//  1. A stale dolt_database when another server DB matches the local project_id.
//  2. A stale/missing local project_id when the currently configured DB has one.
func ResolveAuthoritativeServerMetadata(path string, apply bool) (*configfile.Config, string, error) {
	if err := validateBeadsWorkspace(path); err != nil {
		return nil, "", err
	}

	beadsDir := resolveBeadsDir(filepath.Join(path, ".beads"))
	cfg, err := configfile.Load(beadsDir)
	if err != nil || cfg == nil {
		return cfg, "", err
	}
	msg, err := resolveAuthoritativeServerMetadata(path, cfg, apply)
	return cfg, msg, err
}

func resolveAuthoritativeServerMetadata(path string, cfg *configfile.Config, apply bool) (string, error) {
	if !shouldResolveAuthoritativeServerMetadata(cfg) {
		return "", nil
	}

	beadsDir := resolveBeadsDir(filepath.Join(path, ".beads"))
	databases, err := listServerMetadataDatabases(beadsDir, cfg)
	if err != nil {
		return "", err
	}

	return applyAuthoritativeServerMetadata(beadsDir, cfg, databases, apply)
}

func shouldResolveAuthoritativeServerMetadata(cfg *configfile.Config) bool {
	return cfg != nil && cfg.GetBackend() == configfile.BackendDolt &&
		(cfg.IsDoltServerMode() || doltserver.IsSharedServerMode())
}

func applyAuthoritativeServerMetadata(beadsDir string, cfg *configfile.Config, databases []serverDatabaseMetadata, apply bool) (string, error) {
	changed, msg, err := reconcileAuthoritativeServerMetadata(cfg, databases)
	if err != nil || !changed {
		return msg, err
	}
	if apply {
		if err := cfg.Save(beadsDir); err != nil {
			return "", fmt.Errorf("failed to save metadata.json: %w", err)
		}
		return msg, nil
	}
	return "would " + msg, nil
}

func reconcileAuthoritativeServerMetadata(cfg *configfile.Config, databases []serverDatabaseMetadata) (bool, string, error) {
	if cfg == nil {
		return false, "", nil
	}

	index := indexServerMetadataDatabases(databases)
	if changed, msg, err := repairMetadataByProjectID(cfg, index); changed || err != nil {
		return changed, msg, err
	}
	if changed, msg := repairMetadataFromCurrentDatabase(cfg, index); changed {
		return true, msg, nil
	}
	return adoptSoleServerDatabase(cfg, index)
}

type serverMetadataIndex struct {
	byName           map[string]serverDatabaseMetadata
	schemaCandidates []serverDatabaseMetadata
}

func indexServerMetadataDatabases(databases []serverDatabaseMetadata) serverMetadataIndex {
	byName := make(map[string]serverDatabaseMetadata, len(databases))
	var schemaCandidates []serverDatabaseMetadata
	for _, db := range databases {
		byName[db.Name] = db
		if db.HasSchema {
			schemaCandidates = append(schemaCandidates, db)
		}
	}
	return serverMetadataIndex{byName: byName, schemaCandidates: schemaCandidates}
}

func repairMetadataByProjectID(cfg *configfile.Config, index serverMetadataIndex) (bool, string, error) {
	if cfg.ProjectID != "" {
		matches := matchingServerMetadataDatabases(index.schemaCandidates, cfg.ProjectID)
		if len(matches) > 1 {
			return false, "", ambiguousServerMetadataError(cfg.ProjectID, matches)
		}
		if len(matches) == 1 {
			return repairDatabaseNameFromProjectID(cfg, index, matches[0])
		}
	}
	return false, "", nil
}

func matchingServerMetadataDatabases(databases []serverDatabaseMetadata, projectID string) []serverDatabaseMetadata {
	var matches []serverDatabaseMetadata
	for _, db := range databases {
		if db.ProjectID == projectID {
			matches = append(matches, db)
		}
	}
	return matches
}

func ambiguousServerMetadataError(projectID string, matches []serverDatabaseMetadata) error {
	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, match.Name)
	}
	sort.Strings(names)
	return fmt.Errorf(
		"multiple server databases match project_id %s: %s",
		projectID,
		strings.Join(names, ", "),
	)
}

func repairDatabaseNameFromProjectID(cfg *configfile.Config, index serverMetadataIndex, match serverDatabaseMetadata) (bool, string, error) {
	if cfg.DoltDatabase == match.Name {
		return false, "", nil
	}
	// Safety guard: when the configured database has its own project_id that
	// disagrees with metadata.json, and a different database matches the local
	// project_id, we have conflicting identity signals. Auto-switching databases
	// here can silently attach this workspace to another project.
	current, hasCurrent := index.byName[cfg.GetDoltDatabase()]
	if hasCurrent && current.HasSchema && current.ProjectID != "" && current.ProjectID != cfg.ProjectID {
		return false, "", fmt.Errorf(
			"conflicting project identity: metadata.json project_id %s matches database %q, but configured database %q reports project_id %s",
			cfg.ProjectID,
			match.Name,
			current.Name,
			current.ProjectID,
		)
	}
	from := cfg.GetDoltDatabase()
	cfg.DoltDatabase = match.Name
	return true, fmt.Sprintf("repaired dolt_database: %q -> %q using project_id %s", from, match.Name, cfg.ProjectID), nil
}

func repairMetadataFromCurrentDatabase(cfg *configfile.Config, index serverMetadataIndex) (bool, string) {
	current, hasCurrent := index.byName[cfg.GetDoltDatabase()]
	if !hasCurrent || !current.HasSchema || current.ProjectID == "" || cfg.ProjectID == current.ProjectID {
		return false, ""
	}

	from := cfg.ProjectID
	cfg.ProjectID = current.ProjectID
	if from == "" {
		return true, fmt.Sprintf("backfilled project_id %s from database %q", current.ProjectID, current.Name)
	}
	return true, fmt.Sprintf("repaired project_id: %s -> %s from database %q", from, current.ProjectID, current.Name)
}

func adoptSoleServerDatabase(cfg *configfile.Config, index serverMetadataIndex) (bool, string, error) {
	// In shared-server mode, a sole schema candidate can belong to a different
	// workspace. Without a local project_id anchor, auto-adopting that database
	// can redirect this workspace to another project's data.
	if cfg.ProjectID != "" || len(index.schemaCandidates) != 1 || doltserver.IsSharedServerMode() {
		return false, "", nil
	}

	candidate := index.schemaCandidates[0]
	var repairs []string
	if cfg.DoltDatabase != candidate.Name {
		repairs = append(repairs, fmt.Sprintf("dolt_database: %q -> %q", cfg.GetDoltDatabase(), candidate.Name))
		cfg.DoltDatabase = candidate.Name
	}
	if candidate.ProjectID != "" {
		repairs = append(repairs, fmt.Sprintf("project_id: %s", candidate.ProjectID))
		cfg.ProjectID = candidate.ProjectID
	}
	if len(repairs) > 0 {
		return true, "repaired metadata from the only server database with Beads schema (" + strings.Join(repairs, "; ") + ")", nil
	}

	return false, "", nil
}

func inspectServerMetadataDatabases(beadsDir string, cfg *configfile.Config) ([]serverDatabaseMetadata, error) {
	db, err := openServerCatalogDB(beadsDir, cfg)
	if err != nil {
		return nil, fmt.Errorf("server metadata probe failed: %w", err)
	}
	defer db.Close()

	ctx := context.Background()
	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var databases []serverDatabaseMetadata
	for rows.Next() {
		var dbName string
		if err := rows.Scan(&dbName); err != nil {
			return nil, err
		}
		if isServerCatalogDatabase(dbName) {
			continue
		}
		meta, err := inspectServerMetadataDatabase(ctx, db, dbName)
		if err != nil {
			return nil, err
		}
		databases = append(databases, meta)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return databases, nil
}

func isServerCatalogDatabase(dbName string) bool {
	return dbName == "information_schema" || dbName == "mysql"
}

func inspectServerMetadataDatabase(ctx context.Context, db *sql.DB, dbName string) (serverDatabaseMetadata, error) {
	// Escape backticks in database name to prevent SQL injection (` → ``)
	safeName := strings.ReplaceAll(dbName, "`", "``")
	hasSchema, err := probeServerDatabaseSchema(ctx, db, dbName, safeName)
	if err != nil {
		return serverDatabaseMetadata{}, err
	}
	projectID, err := probeServerDatabaseProjectID(ctx, db, dbName, safeName)
	if err != nil {
		return serverDatabaseMetadata{}, err
	}
	return serverDatabaseMetadata{Name: dbName, HasSchema: hasSchema, ProjectID: projectID}, nil
}

func probeServerDatabaseSchema(ctx context.Context, db *sql.DB, dbName, safeName string) (bool, error) {
	var count int
	//nolint:gosec // G201: identifier-escaped, dbName from SHOW DATABASES
	err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM `%s`.issues LIMIT 1", safeName)).Scan(&count)
	if err == nil || isExpectedProbeError(err) {
		return err == nil, nil
	}
	return false, fmt.Errorf("probing database %q for schema: %w", dbName, err)
}

func probeServerDatabaseProjectID(ctx context.Context, db *sql.DB, dbName, safeName string) (string, error) {
	var projectID string
	//nolint:gosec // G201: identifier-escaped, dbName from SHOW DATABASES
	err := db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT value FROM `%s`.metadata WHERE `key` = '_project_id' LIMIT 1", safeName),
	).Scan(&projectID)
	if err == nil || isExpectedProbeError(err) {
		return projectID, nil
	}
	return "", fmt.Errorf("probing database %q for project_id: %w", dbName, err)
}

// isExpectedProbeError returns true for errors that indicate the table/row
// simply doesn't exist — or that this user cannot inspect that database —
// safe to treat as "not present" when enumerating SHOW DATABASES.
//
// GH#4931: shared sql-servers often host non-beads databases the beads
// user cannot read. Access denied on those peers must not abort bootstrap
// / metadata reconciliation for the configured beads database.
//
// Hard connection failures (server down, auth to the catalog itself) still
// surface via openServerCatalogDB / SHOW DATABASES, not this probe helper.
func isExpectedProbeError(err error) bool {
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		return true
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && isExpectedMySQLProbeError(mysqlErr) {
		return true
	}
	// Driver / wrapper may not expose MySQLError; match the common message.
	return strings.Contains(strings.ToLower(err.Error()), "access denied")
}

func isExpectedMySQLProbeError(err *mysql.MySQLError) bool {
	switch err.Number {
	case 1049: // Unknown database
		return true
	case 1146: // Table doesn't exist
		return true
	case 1054: // Unknown column
		return true
	case 1044: // Access denied for user to database
		return true
	case 1142: // Access denied for user to table
		return true
	case 1143: // Access denied for user to column
		return true
	case 1227: // Access denied (requires privilege)
		return true
	case 1105: // HY000 — Dolt often wraps access denied here (GH#4931)
		return strings.Contains(strings.ToLower(err.Message), "access denied")
	default:
		return false
	}
}

func openServerCatalogDB(beadsDir string, cfg *configfile.Config) (*sql.DB, error) {
	port := doltserver.DefaultConfig(beadsDir).Port
	connStr := doltutil.ServerDSN{
		Host:     cfg.GetDoltServerHost(),
		Port:     port,
		User:     cfg.GetDoltServerUser(),
		Password: cfg.GetDoltServerPasswordForPort(port),
		TLS:      cfg.GetDoltServerTLS(),
	}.String()
	return sql.Open("mysql", connStr)
}
