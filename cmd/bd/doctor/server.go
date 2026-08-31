package doctor

import (
	"database/sql"
	"fmt"

	// Import MySQL driver for server mode connections
	_ "github.com/go-sql-driver/mysql"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
)

// ServerHealthResult holds the results of all server health checks
type ServerHealthResult struct {
	Checks    []DoctorCheck `json:"checks"`
	OverallOK bool          `json:"overall_ok"`
}

// RunServerHealthChecks runs all server-mode health checks and returns the result.
// This is called when `bd doctor --server` is used.
func RunServerHealthChecks(path string) ServerHealthResult {
	result := ServerHealthResult{
		OverallOK: true,
	}
	cfg, beadsDir, configCheck, continueChecks := loadServerHealthConfig(path)
	if configCheck != nil {
		result.Checks = append(result.Checks, *configCheck)
		result.OverallOK = configCheck.Status == StatusOK
	}
	if !continueChecks {
		return result
	}
	return runConfiguredServerHealthChecks(result, cfg, beadsDir)
}

func loadServerHealthConfig(path string) (*configfile.Config, string, *DoctorCheck, bool) {
	_, beadsDir := getBackendAndBeadsDir(path)
	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		check := DoctorCheck{
			Name:     "Server Config",
			Status:   StatusError,
			Message:  "Failed to load config",
			Detail:   err.Error(),
			Category: CategoryFederation,
		}
		return nil, beadsDir, &check, false
	}
	if cfg == nil {
		check := DoctorCheck{
			Name:     "Server Config",
			Status:   StatusError,
			Message:  "No metadata.json found",
			Fix:      "Run 'bd init' to initialize beads",
			Category: CategoryFederation,
		}
		return nil, beadsDir, &check, false
	}
	if cfg.GetBackend() != configfile.BackendDolt {
		check := DoctorCheck{
			Name:     "Server Config",
			Status:   StatusWarning,
			Message:  fmt.Sprintf("Server checks require Dolt; configured backend is %q", cfg.GetBackend()),
			Detail:   "Dolt server health checks do not apply to this backend",
			Category: CategoryFederation,
		}
		return nil, beadsDir, &check, false
	}
	if !cfg.IsDoltServerMode() {
		check := DoctorCheck{
			Name:     "Server Config",
			Status:   StatusOK,
			Message:  fmt.Sprintf("Dolt mode is '%s' (embedded is the default)", cfg.GetDoltMode()),
			Detail:   "Server health checks only apply when dolt_mode is explicitly set to 'server'",
			Category: CategoryFederation,
		}
		return cfg, beadsDir, &check, false
	}
	return cfg, beadsDir, nil, true
}

func runConfiguredServerHealthChecks(result ServerHealthResult, cfg *configfile.Config, beadsDir string) ServerHealthResult {
	host := cfg.GetDoltServerHost()
	// Use doltserver.DefaultConfig for port resolution (env > port file > config.yaml).
	// Port 0 means server not yet started — report that clearly.
	port := doltserver.DefaultConfig(beadsDir).Port
	if port == 0 {
		result.Checks = append(result.Checks, DoctorCheck{
			Name:     "Server port",
			Status:   StatusWarning,
			Message:  "No Dolt server port configured and no server running. Run any bd command to auto-start.",
			Category: CategoryFederation,
		})
		return result
	}

	// Check 1: Server reachability (TCP connect)
	recordServerHealthCheck(&result, checkServerReachable(host, port))
	if !result.OverallOK {
		return result
	}

	// Check 2: Connect and verify it's Dolt (get version)
	versionCheck, db := checkDoltVersion(cfg, beadsDir)
	recordServerHealthCheck(&result, versionCheck)
	if versionCheck.Status == StatusError {
		closeHealthCheckDB(db)
		return result
	}
	defer closeHealthCheckDB(db)

	// Get database name from config (uses dolt_database field, default: "beads")
	database := cfg.GetDoltDatabase()
	appendDatabaseHealthChecks(&result, db, database)
	return result
}

func recordServerHealthCheck(result *ServerHealthResult, check DoctorCheck) {
	result.Checks = append(result.Checks, check)
	if check.Status == StatusError {
		result.OverallOK = false
	}
}

func closeHealthCheckDB(db *sql.DB) {
	if db != nil {
		_ = db.Close()
	}
}

func appendDatabaseHealthChecks(result *ServerHealthResult, db *sql.DB, database string) {
	checks := []DoctorCheck{
		checkDatabaseExists(db, database),
		checkSchemaCompatible(db, database),
		checkConnectionPool(db),
		checkStaleDatabases(db),
	}
	for _, check := range checks {
		recordServerHealthCheck(result, check)
	}
}

// staleDatabasePrefixes are prefixes that indicate test/polecat databases that
// should not exist on the production Dolt server. These accumulate from interrupted
// test runs and terminated polecats, wasting server memory and potentially
// contributing to performance degradation under concurrent load.
// - testdb_*: BEADS_TEST_MODE=1 FNV hash of temp paths
// - doctest_*: doctor test helpers
// - doctortest_*: doctor test helpers
// - beads_pt*: orchestrator patrol_helpers_test.go random prefixes
// - beads_vr*: orchestrator mail/router_test.go random prefixes
// - beads_t[0-9a-f]*: protocol test random prefixes (t + 8 hex chars)
var staleDatabasePrefixes = []string{
	"testdb_",
	"doctest_",
	"doctortest_",
	"beads_pt",
	"beads_vr",
	"beads_t",
}

// knownProductionDatabases are the databases that should exist on a production server.
// Everything else matching a stale prefix is a candidate for cleanup.
var knownProductionDatabases = map[string]bool{
	"information_schema": true,
	"mysql":              true,
}
