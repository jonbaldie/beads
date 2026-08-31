package doctor

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/utils"
)

// CheckDoltLocks checks if the Dolt database has any locks or uncommitted changes.
func CheckDoltLocks(path string) DoctorCheck {
	beadsDir := ResolveBeadsDirForRepo(path)

	// Only run for Dolt backend
	if !IsDoltBackend(beadsDir) {
		return DoctorCheck{
			Name:     "Dolt Locks",
			Status:   StatusOK,
			Message:  "N/A (not Dolt backend)",
			Category: CategoryMaintenance,
		}
	}

	locked, detail, err := checkDoltLocks(beadsDir)
	if err != nil {
		return DoctorCheck{
			Name:     "Dolt Locks",
			Status:   StatusWarning,
			Message:  "Could not check Dolt locks",
			Detail:   err.Error(),
			Fix:      "Ensure the Dolt server is running: gt dolt status",
			Category: CategoryMaintenance,
		}
	}
	if locked {
		return DoctorCheck{
			Name:     "Dolt Locks",
			Status:   StatusWarning,
			Message:  "Uncommitted changes detected",
			Detail:   detail,
			Fix:      "Run 'bd vc commit -m \"commit changes\"' to commit, or changes will auto-commit on next bd command",
			Category: CategoryMaintenance,
		}
	}

	return DoctorCheck{
		Name:     "Dolt Locks",
		Status:   StatusOK,
		Message:  "No locks or uncommitted changes",
		Category: CategoryMaintenance,
	}
}

// Helper functions

// findJSONLFile locates the JSONL file in a .beads directory.
// Temporary: will be removed with Phase 2c (doctor JSONL cleanup).
func findJSONLFile(beadsDir string) string {
	for _, name := range []string{"issues.jsonl", "beads.jsonl"} {
		p := filepath.Join(beadsDir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// validateJSONLForMigration validates a JSONL file for migration readiness.
// Returns: count of valid issues, count of malformed lines, set of valid IDs, and error if blocking.
func validateJSONLForMigration(jsonlPath string) (int, int, map[string]bool, error) {
	file, err := os.Open(jsonlPath) //nolint:gosec
	if err != nil {
		return 0, 0, nil, fmt.Errorf("failed to open JSONL: %w", err)
	}
	defer file.Close()

	ids := make(map[string]bool)
	var malformed int

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1024), 2*1024*1024) // 2MB buffer for large lines

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		id, ok := migrationJSONLLineID(line)
		if !ok {
			malformed++
			continue
		}

		ids[id] = true
	}

	if err := scanner.Err(); err != nil {
		return len(ids), malformed, ids, fmt.Errorf("failed to read JSONL: %w", err)
	}

	// Return error only if ALL lines are malformed (blocking)
	if len(ids) == 0 && malformed > 0 {
		return 0, malformed, ids, fmt.Errorf("JSONL file is completely corrupt: %d malformed lines", malformed)
	}

	return len(ids), malformed, ids, nil
}

func migrationJSONLLineID(line []byte) (string, bool) {
	var issue struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(line, &issue); err != nil || issue.ID == "" {
		return "", false
	}
	return issue.ID, true
}

// compareDoltWithJSONL compares Dolt database with JSONL IDs.
// Returns IDs in JSONL but not in Dolt (sample first 100).
func compareDoltWithJSONL(ctx context.Context, store storage.DoltStorage, jsonlIDs map[string]bool) []string {
	ids := make([]string, 0, len(jsonlIDs))
	for id := range jsonlIDs {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	foundIDs := fetchDoltIssueIDs(ctx, store, ids)
	return missingDoltIssueIDs(ids, foundIDs)
}

func fetchDoltIssueIDs(ctx context.Context, store storage.DoltStorage, ids []string) map[string]bool {
	foundIDs := make(map[string]bool, len(ids))
	const batchSize = 500
	idCount := len(ids)
	for i := 0; i < idCount; i += batchSize {
		end := i + batchSize
		if end > idCount {
			end = idCount
		}
		issues, err := store.GetIssuesByIDs(ctx, ids[i:end])
		if err != nil {
			continue
		}
		for _, issue := range issues {
			foundIDs[issue.ID] = true
		}
	}
	return foundIDs
}

func missingDoltIssueIDs(ids []string, foundIDs map[string]bool) []string {
	var missing []string
	for _, id := range ids {
		if !foundIDs[id] {
			missing = append(missing, id)
			if len(missing) >= 100 {
				break
			}
		}
	}
	return missing
}

// checkDoltLocks checks for uncommitted changes in Dolt.
// Returns (locked, detail, error). A non-nil error means the check could not
// be performed (e.g. connection failure) and the locked result is meaningless.
func checkDoltLocks(beadsDir string) (bool, string, error) {
	conn, err := openDoltConn(beadsDir)
	if err != nil {
		return false, "", fmt.Errorf("cannot connect to Dolt: %w", err)
	}
	defer conn.Close()

	ctx := context.Background()

	// Check dolt_status for uncommitted changes
	rows, err := conn.db.QueryContext(ctx, "SELECT table_name, staged, status FROM dolt_status")
	if err != nil {
		return false, "", fmt.Errorf("cannot query dolt_status: %w", err)
	}
	defer rows.Close()

	// Same filter as the "Dolt Status" check — see describeUncommittedTables.
	scanned, err := scanDoltStatus(rows)
	if err != nil {
		return false, "", fmt.Errorf("row iteration error: %w", err)
	}
	changes := describeUncommittedTables(scanned)

	if len(changes) > 0 {
		return true, strings.Join(changes, ", "), nil
	}

	return false, "", nil
}

// categorizeDoltExtras finds issues in Dolt that aren't in JSONL and categorizes them
// as either foreign-prefix (cross-rig contamination) or ephemeral (same-prefix).
// Returns: foreignCount, foreignPrefixes map, ephemeralCount.
func categorizeDoltExtras(ctx context.Context, store storage.DoltStorage, jsonlIDs map[string]bool) (int, map[string]int, int) {
	localPrefix, _ := store.GetConfig(ctx, "issue_prefix") // Best effort: empty prefix means no prefix-based validation
	db, ok := rawDoltDatabase(store)
	if !ok {
		return 0, nil, 0
	}

	rows, err := db.QueryContext(ctx, "SELECT id FROM issues")
	if err != nil {
		return 0, nil, 0
	}
	defer rows.Close()
	foreignPrefixes, ephemeralCount := scanDoltExtras(rows, localPrefix, jsonlIDs)
	foreignCount := countForeignPrefixes(foreignPrefixes)
	return foreignCount, foreignPrefixes, ephemeralCount
}

func rawDoltDatabase(store storage.DoltStorage) (*sql.DB, bool) {
	accessor, ok := storage.UnwrapStore(store).(storage.RawDBAccessor)
	if !ok {
		return nil, false
	}
	db := accessor.UnderlyingDB()
	if db == nil {
		return nil, false
	}
	return db, true
}

func scanDoltExtras(rows *sql.Rows, localPrefix string, jsonlIDs map[string]bool) (map[string]int, int) {
	foreignPrefixes := make(map[string]int)
	var ephemeralCount int

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		// Skip issues that are in JSONL (those are expected)
		if jsonlIDs[id] {
			continue
		}
		// This issue is in Dolt but not in JSONL - categorize it
		prefix := utils.ExtractIssuePrefix(id)
		if localPrefix != "" && prefix != "" && prefix != localPrefix {
			foreignPrefixes[prefix]++
		} else {
			ephemeralCount++
		}
	}
	// Best effort: rows.Err() ignored here since this is a diagnostic categorization
	// and partial results are acceptable.
	_ = rows.Err()
	return foreignPrefixes, ephemeralCount
}

func countForeignPrefixes(foreignPrefixes map[string]int) int {
	var foreignCount int
	for _, count := range foreignPrefixes {
		foreignCount += count
	}
	return foreignCount
}

// formatPrefixCounts formats a map of prefix -> count as "prefix1 (N), prefix2 (M)".
func formatPrefixCounts(prefixes map[string]int) string {
	var parts []string
	for prefix, count := range prefixes {
		parts = append(parts, fmt.Sprintf("%s (%d)", prefix, count))
	}
	return strings.Join(parts, ", ")
}
