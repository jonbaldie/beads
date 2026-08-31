package doctor

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage/dolt"
)

// MigrationValidationPhase identifies the validation stage and overall readiness.
type MigrationValidationPhase struct {
	Phase          string `json:"phase"`           // "pre-migration" or "post-migration"
	Ready          bool   `json:"ready"`           // true if migration can proceed/succeeded
	Backend        string `json:"backend"`         // current backend: "sqlite", "dolt", or "jsonl-only"
	RecommendedFix string `json:"recommended_fix"` // suggested command to fix issues
}

// MigrationValidationCounts holds issue and malformation counts.
type MigrationValidationCounts struct {
	JSONLCount         int `json:"jsonl_count"`          // issue count in JSONL
	SQLiteCount        int `json:"sqlite_count"`         // issue count in SQLite (pre-migration)
	DoltCount          int `json:"dolt_count"`           // issue count in Dolt (post-migration)
	JSONLMalformed     int `json:"jsonl_malformed"`      // count of malformed JSONL lines
	ForeignPrefixCount int `json:"foreign_prefix_count"` // count of issues with non-local prefixes (cross-rig contamination)
}

// MigrationValidationFlags holds boolean health flags.
type MigrationValidationFlags struct {
	JSONLValid  bool `json:"jsonl_valid"`  // true if JSONL is parseable
	DoltHealthy bool `json:"dolt_healthy"` // true if Dolt DB is healthy
	DoltLocked  bool `json:"dolt_locked"`  // true if Dolt has uncommitted changes
	SchemaValid bool `json:"schema_valid"` // true if schema is complete
}

// MigrationValidationDiffs holds sample mismatches and diagnostics.
type MigrationValidationDiffs struct {
	MissingInDB     []string       `json:"missing_in_db"`    // issue IDs in JSONL but not in DB (sample)
	MissingInJSONL  []string       `json:"missing_in_jsonl"` // issue IDs in DB but not in JSONL (sample)
	Errors          []string       `json:"errors"`           // blocking errors
	Warnings        []string       `json:"warnings"`         // non-blocking warnings
	ForeignPrefixes map[string]int `json:"foreign_prefixes"` // prefix -> count for foreign-prefix issues
}

// MigrationValidationResult provides machine-parseable migration validation output.
// This struct is designed to be consumed by Claude and other automation tools.
type MigrationValidationResult struct {
	MigrationValidationPhase
	MigrationValidationCounts
	MigrationValidationFlags
	MigrationValidationDiffs
}

// CheckMigrationReadiness validates that a beads installation is ready for Dolt migration.
// This is a pre-migration check that ensures:
// 1. JSONL file exists and is valid (parseable, no corruption)
// 2. All issues in JSONL are also in the database (or explains discrepancies)
// 3. No blocking issues prevent migration
//
// Returns a doctor check suitable for standard output and a detailed result for automation.
func CheckMigrationReadiness(path string) (DoctorCheck, MigrationValidationResult) {
	result := MigrationValidationResult{
		MigrationValidationPhase: MigrationValidationPhase{
			Phase: "pre-migration",
			Ready: true,
		},
		MigrationValidationFlags: MigrationValidationFlags{
			JSONLValid:  true,
			SchemaValid: true,
		},
	}

	beadsDir := ResolveBeadsDirForRepo(path)
	jsonlPath, earlyCheck, done := prepareMigrationReadiness(beadsDir, &result)
	if done {
		return earlyCheck, result
	}

	jsonlCount, malformed, _, err := validateJSONLForMigration(jsonlPath)
	result.JSONLCount = jsonlCount
	result.JSONLMalformed = malformed
	if err != nil {
		return migrationReadinessJSONLError(&result, malformed, err)
	}

	if malformed > 0 {
		result.JSONLValid = false
		result.Warnings = append(result.Warnings, fmt.Sprintf("%d malformed lines in JSONL (skipped)", malformed))
	}

	result.Backend = "jsonl-only"
	return migrationReadinessStatus(&result, jsonlCount)
}

func prepareMigrationReadiness(beadsDir string, result *MigrationValidationResult) (string, DoctorCheck, bool) {
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		result.Ready = false
		result.Errors = append(result.Errors, "No active beads workspace found")
		return "", DoctorCheck{
			Name:     "Migration Readiness",
			Status:   StatusError,
			Message:  "No active beads workspace found",
			Fix:      "Run 'bd where' to inspect the resolved workspace, or 'bd init' to create a beads installation",
			Category: CategoryMaintenance,
		}, true
	}

	result.Backend = GetBackend(beadsDir)
	if result.Backend == configfile.BackendDolt {
		return "", DoctorCheck{
			Name:     "Migration Readiness",
			Status:   StatusOK,
			Message:  "Already using Dolt backend",
			Category: CategoryMaintenance,
		}, true
	}

	jsonlPath := findJSONLFile(beadsDir)
	if jsonlPath == "" {
		result.Ready = false
		result.Errors = append(result.Errors, "No JSONL file found")
		return "", DoctorCheck{
			Name:     "Migration Readiness",
			Status:   StatusError,
			Message:  "No JSONL file found",
			Detail:   "Migration requires issues.jsonl or beads.jsonl",
			Fix:      "Run 'bd export' to create JSONL file from database",
			Category: CategoryMaintenance,
		}, true
	}
	return jsonlPath, DoctorCheck{}, false
}

func migrationReadinessJSONLError(result *MigrationValidationResult, malformed int, err error) (DoctorCheck, MigrationValidationResult) {
	result.Ready = false
	result.JSONLValid = false
	result.Errors = append(result.Errors, fmt.Sprintf("JSONL validation failed: %v", err))
	return DoctorCheck{
		Name:     "Migration Readiness",
		Status:   StatusError,
		Message:  fmt.Sprintf("JSONL has %d malformed lines", malformed),
		Detail:   err.Error(),
		Fix:      "Run 'bd doctor --fix' to repair JSONL from database",
		Category: CategoryMaintenance,
	}, *result
}

func migrationReadinessStatus(result *MigrationValidationResult, jsonlCount int) (DoctorCheck, MigrationValidationResult) {

	if len(result.Errors) > 0 {
		result.Ready = false
		return DoctorCheck{
			Name:     "Migration Readiness",
			Status:   StatusError,
			Message:  fmt.Sprintf("Not ready: %d error(s)", len(result.Errors)),
			Detail:   strings.Join(result.Errors, "\n"),
			Fix:      "Fix errors before running migration",
			Category: CategoryMaintenance,
		}, *result
	}

	status := StatusOK
	message := fmt.Sprintf("Ready (%d issues in JSONL)", jsonlCount)
	if len(result.Warnings) > 0 {
		status = StatusWarning
		message = fmt.Sprintf("Ready with warnings (%d issues)", jsonlCount)
	}

	return DoctorCheck{
		Name:     "Migration Readiness",
		Status:   status,
		Message:  message,
		Detail:   strings.Join(result.Warnings, "\n"),
		Fix:      "Follow 'bd help init-safety' to reinitialize with Dolt, then import and verify this JSONL export",
		Category: CategoryMaintenance,
	}, *result
}

// CheckMigrationCompletion validates that a Dolt migration completed successfully.
// This is a post-migration check that ensures:
// 1. Dolt database exists and is healthy
// 2. All issues from JSONL are present in Dolt
// 3. No data was lost during migration
// 4. Dolt database has no locks or uncommitted changes
func CheckMigrationCompletion(path string) (DoctorCheck, MigrationValidationResult) {
	result := MigrationValidationResult{
		MigrationValidationPhase: MigrationValidationPhase{
			Phase: "post-migration",
			Ready: true,
		},
		MigrationValidationFlags: MigrationValidationFlags{
			DoltHealthy: true,
			SchemaValid: true,
		},
	}

	beadsDir := ResolveBeadsDirForRepo(path)
	check, done := prepareMigrationCompletion(beadsDir, &result)
	if done {
		return check, result
	}
	return inspectMigrationCompletion(beadsDir, &result)
}

func prepareMigrationCompletion(beadsDir string, result *MigrationValidationResult) (DoctorCheck, bool) {
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		result.Ready = false
		result.DoltHealthy = false
		result.Errors = append(result.Errors, "No active beads workspace found")
		return DoctorCheck{
			Name:     "Migration Completion",
			Status:   StatusError,
			Message:  "No active beads workspace found",
			Category: CategoryMaintenance,
		}, true
	}
	result.Backend = GetBackend(beadsDir)
	if result.Backend != configfile.BackendDolt {
		result.Ready = false
		result.DoltHealthy = false
		result.Errors = append(result.Errors, fmt.Sprintf("Backend is %s, not Dolt", result.Backend))
		return DoctorCheck{
			Name:     "Migration Completion",
			Status:   StatusError,
			Message:  "Not using Dolt backend",
			Detail:   fmt.Sprintf("Current backend: %s", result.Backend),
			Fix:      "Follow 'bd help init-safety' to reinitialize with Dolt, then import and verify the issue export",
			Category: CategoryMaintenance,
		}, true
	}
	return DoctorCheck{}, false
}

func inspectMigrationCompletion(beadsDir string, result *MigrationValidationResult) (DoctorCheck, MigrationValidationResult) {
	ctx := context.Background()
	doltPath := getDatabasePath(beadsDir)
	store, err := dolt.New(ctx, doltServerConfig(beadsDir, doltPath))
	if err != nil {
		return migrationCompletionOpenError(result, err)
	}
	defer func() { _ = store.Close() }()

	stats, err := store.GetStatistics(ctx)
	if err != nil {
		return migrationCompletionQueryError(result, err)
	}
	result.DoltCount = stats.TotalIssues
	appendMigrationLockWarnings(beadsDir, result)
	compareMigrationCompletion(ctx, store, beadsDir, result)
	return migrationCompletionStatus(result)
}

func migrationCompletionOpenError(result *MigrationValidationResult, err error) (DoctorCheck, MigrationValidationResult) {
	result.Ready = false
	result.DoltHealthy = false
	result.Errors = append(result.Errors, fmt.Sprintf("Failed to open Dolt: %v", err))
	return DoctorCheck{
		Name:     "Migration Completion",
		Status:   StatusError,
		Message:  "Cannot open Dolt database",
		Detail:   err.Error(),
		Fix:      "Check Dolt database integrity or re-run migration",
		Category: CategoryMaintenance,
	}, *result
}

func migrationCompletionQueryError(result *MigrationValidationResult, err error) (DoctorCheck, MigrationValidationResult) {
	result.Ready = false
	result.SchemaValid = false
	result.Errors = append(result.Errors, fmt.Sprintf("Failed to query Dolt: %v", err))
	return DoctorCheck{
		Name:     "Migration Completion",
		Status:   StatusError,
		Message:  "Cannot query Dolt database",
		Detail:   err.Error(),
		Fix:      "Database schema may be incomplete",
		Category: CategoryMaintenance,
	}, *result
}

func appendMigrationLockWarnings(beadsDir string, result *MigrationValidationResult) {
	doltLocked, lockDetail, lockErr := checkDoltLocks(beadsDir)
	if lockErr != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("Could not check Dolt locks: %v", lockErr))
	} else {
		result.DoltLocked = doltLocked
		if doltLocked {
			result.Warnings = append(result.Warnings, fmt.Sprintf("Dolt has uncommitted changes: %s", lockDetail))
		}
	}
}

func compareMigrationCompletion(ctx context.Context, store *dolt.DoltStore, beadsDir string, result *MigrationValidationResult) {
	jsonlPath := findJSONLFile(beadsDir)
	if jsonlPath == "" {
		result.Warnings = append(result.Warnings, "No JSONL file found for comparison")
		return
	}
	jsonlCount, _, jsonlIDs, err := validateJSONLForMigration(jsonlPath)
	result.JSONLCount = jsonlCount
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("Could not validate JSONL: %v", err))
		return
	}
	missingInDolt := compareDoltWithJSONL(ctx, store, jsonlIDs)
	result.MissingInDB = missingInDolt
	if len(missingInDolt) > 0 {
		result.Ready = false
		result.Errors = append(result.Errors,
			fmt.Sprintf("%d issues in JSONL missing from Dolt", len(missingInDolt)))
	}
	if result.DoltCount == jsonlCount {
		return
	}
	if result.DoltCount < jsonlCount {
		result.Ready = false
		result.Errors = append(result.Errors,
			fmt.Sprintf("Count mismatch: Dolt has %d, JSONL has %d", result.DoltCount, jsonlCount))
		return
	}
	foreignCount, foreignPrefixes, ephemeralCount := categorizeDoltExtras(ctx, store, jsonlIDs)
	result.ForeignPrefixCount = foreignCount
	result.ForeignPrefixes = foreignPrefixes
	appendMigrationExtraWarnings(result, foreignCount, foreignPrefixes, ephemeralCount)
}

func appendMigrationExtraWarnings(result *MigrationValidationResult, foreignCount int, foreignPrefixes map[string]int, ephemeralCount int) {
	if foreignCount > 0 {
		prefixList := formatPrefixCounts(foreignPrefixes)
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("Dolt has %d issues from other rigs (cross-rig contamination): %s", foreignCount, prefixList))
	}
	if ephemeralCount > 0 {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("Dolt has %d ephemeral issues not in JSONL", ephemeralCount))
	}
}

func migrationCompletionStatus(result *MigrationValidationResult) (DoctorCheck, MigrationValidationResult) {
	if len(result.Errors) > 0 {
		result.Ready = false
		return DoctorCheck{
			Name:     "Migration Completion",
			Status:   StatusError,
			Message:  fmt.Sprintf("Migration incomplete: %d error(s)", len(result.Errors)),
			Detail:   strings.Join(result.Errors, "\n"),
			Fix:      "Check the export/import results; follow 'bd help init-safety' before reinitializing again",
			Category: CategoryMaintenance,
		}, *result
	}

	status := StatusOK
	message := fmt.Sprintf("Complete (%d issues in Dolt)", result.DoltCount)
	if len(result.Warnings) > 0 {
		status = StatusWarning
		message = fmt.Sprintf("Complete with warnings (%d issues)", result.DoltCount)
	}

	return DoctorCheck{
		Name:     "Migration Completion",
		Status:   status,
		Message:  message,
		Detail:   strings.Join(result.Warnings, "\n"),
		Category: CategoryMaintenance,
	}, *result
}
