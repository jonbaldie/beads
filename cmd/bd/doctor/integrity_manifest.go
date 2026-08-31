package doctor

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/git"
	"github.com/jonbaldie/beads/internal/storage/dolt"
)

// CheckDeletionsManifest checks the status of the legacy deletions.jsonl file
func CheckDeletionsManifest(path string) DoctorCheck {
	beadsDir := ResolveBeadsDirForRepo(path)

	// Skip if .beads doesn't exist
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		return deletionsManifestCheck("N/A (no .beads directory)")
	}

	// Check if we're in a git repository using worktree-aware detection
	_, err := git.GetGitDir()
	if err != nil {
		return deletionsManifestCheck("N/A (not a git repository)")
	}

	if check, found := inspectDeletionsManifest(filepath.Join(beadsDir, "deletions.jsonl")); found {
		return check
	}

	// deletions.jsonl doesn't exist - this is the expected state
	// Check for .migrated file to confirm migration happened
	migratedPath := filepath.Join(beadsDir, "deletions.jsonl.migrated")
	if _, err := os.Stat(migratedPath); err == nil {
		return deletionsManifestCheck("Migrated (legacy file removed)")
	}

	// No deletions.jsonl - expected for Dolt-native repos
	return deletionsManifestCheck("Not needed (Dolt-native)")
}

func deletionsManifestCheck(message string) DoctorCheck {
	return DoctorCheck{
		Name:    "Deletions Manifest",
		Status:  StatusOK,
		Message: message,
	}
}

func inspectDeletionsManifest(path string) (DoctorCheck, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return DoctorCheck{}, false
	}
	if info.Size() == 0 {
		return deletionsManifestCheck("Empty (no legacy deletions)"), true
	}

	count, err := countDeletionsManifestEntries(path)
	if err != nil {
		return DoctorCheck{}, false
	}
	if count == 0 {
		return deletionsManifestCheck("Empty (no legacy deletions)"), true
	}
	return DoctorCheck{
		Name:    "Deletions Manifest",
		Status:  StatusWarning,
		Message: fmt.Sprintf("Legacy format (%d entries)", count),
		Detail:  "deletions.jsonl is a legacy format no longer used",
		Fix:     "Safe to delete deletions.jsonl (Dolt handles delete propagation natively)",
	}, true
}

func countDeletionsManifestEntries(path string) (int, error) {
	file, err := os.Open(path) // #nosec G304 - controlled path
	if err != nil {
		return 0, err
	}
	defer file.Close()

	count := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if len(scanner.Bytes()) > 0 {
			count++
		}
	}
	return count, nil
}

// CheckRepoFingerprint validates that the database belongs to this repository.
// This detects when a .beads directory was copied from another repo or when
// the git remote URL changed. A mismatch can cause data loss during sync.
// Opens its own store; prefer CheckRepoFingerprintWithStore when a shared store is available.
func CheckRepoFingerprint(path string) DoctorCheck {
	_, beadsDir := getBackendAndBeadsDir(path)

	if info, err := os.Stat(getDatabasePath(beadsDir)); err != nil || !info.IsDir() {
		return DoctorCheck{
			Name:    "Repo Fingerprint",
			Status:  StatusOK,
			Message: "N/A (no database)",
		}
	}

	ctx := context.Background()
	store, err := dolt.NewFromConfigWithCLIOptions(ctx, beadsDir, &dolt.Config{ReadOnly: true})
	if err != nil {
		return DoctorCheck{
			Name:    "Repo Fingerprint",
			Status:  StatusWarning,
			Message: "Unable to open database",
			Detail:  err.Error(),
		}
	}
	defer func() { _ = store.Close() }()

	return checkRepoFingerprintWithStore(store, path)
}

// CheckRepoFingerprintWithStore checks repo fingerprint using a shared store (GH#2636).
func CheckRepoFingerprintWithStore(ss *SharedStore, path string) DoctorCheck {
	store := ss.Store()
	if store == nil {
		return DoctorCheck{
			Name:    "Repo Fingerprint",
			Status:  StatusOK,
			Message: "N/A (no database)",
		}
	}
	return checkRepoFingerprintWithStore(store, path)
}

func checkRepoFingerprintWithStore(store *dolt.DoltStore, path string) DoctorCheck {
	ctx := context.Background()

	storedRepoID, err := store.GetMetadata(ctx, "repo_id")
	if err != nil {
		return DoctorCheck{
			Name:    "Repo Fingerprint",
			Status:  StatusWarning,
			Message: "Unable to read repo fingerprint",
			Detail:  err.Error(),
		}
	}

	if storedRepoID == "" {
		return DoctorCheck{
			Name:    "Repo Fingerprint",
			Status:  StatusWarning,
			Message: "Missing repo fingerprint metadata",
			Detail:  "Storage: Dolt",
			Fix:     "Run 'bd doctor --fix' to repair metadata",
		}
	}

	currentRepoID, currentSource, err := beads.ComputeRepoIDForPathWithSource(path)
	if err != nil {
		if strings.Contains(err.Error(), "not a git repository") {
			return DoctorCheck{
				Name:    "Repo Fingerprint",
				Status:  StatusOK,
				Message: "N/A (not a git repository)",
			}
		}
		return DoctorCheck{
			Name:    "Repo Fingerprint",
			Status:  StatusWarning,
			Message: "Unable to compute current repo ID",
			Detail:  err.Error(),
		}
	}

	return classifyRepoFingerprint(storedRepoID, currentRepoID, currentSource)
}

// classifyRepoFingerprint turns a stored-vs-current fingerprint comparison into
// a doctor check. Pure so the mismatch branches are unit-testable without a
// store.
func classifyRepoFingerprint(storedRepoID, currentRepoID string, currentSource beads.RepoIDSource) DoctorCheck {
	if storedRepoID != currentRepoID {
		// bd-46vla: with no origin remote here, the local fingerprint is a
		// path hash that can never match a remote-derived stored id — the
		// signature of a synced clone on a host without the canonical remote.
		// The stored id is the shared value; repo_id lives in the VERSIONED
		// metadata table, so 'bd migrate --update-repo-id' would stamp this
		// host's path hash into shared state and propagate it to every clone
		// on the next sync (the GH#4361 class).
		if currentSource == beads.RepoIDSourcePath {
			return DoctorCheck{
				Name:    "Repo Fingerprint",
				Status:  StatusWarning,
				Message: "Fingerprint differs, but this checkout has no origin remote",
				Detail:  fmt.Sprintf("stored: %s, current (path hash): %s — on a synced clone the stored id is the canonical shared value and this mismatch is cosmetic", truncateID(storedRepoID), truncateID(currentRepoID)),
				Fix:     "On a synced clone, leave it (or add the canonical origin remote). Only run 'bd migrate --update-repo-id' if this checkout is the canonical repository — the new id propagates to every clone on the next sync",
			}
		}
		return DoctorCheck{
			Name:    "Repo Fingerprint",
			Status:  StatusError,
			Message: "Database belongs to different repository",
			Detail:  fmt.Sprintf("stored: %s, current: %s", truncateID(storedRepoID), truncateID(currentRepoID)),
			Fix:     "Run 'bd migrate --update-repo-id' if URL changed, or 'rm -rf .beads && bd init' if wrong database",
		}
	}

	return DoctorCheck{
		Name:    "Repo Fingerprint",
		Status:  StatusOK,
		Message: fmt.Sprintf("Verified (%s)", truncateID(currentRepoID)),
	}
}

// Helper functions

// truncateID safely truncates an ID to at most 8 characters for display.
func truncateID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// DetectHashBasedIDs uses multiple heuristics to determine if the database uses hash-based IDs.
// This is more robust than checking a single ID's format, since base36 hash IDs can be all-numeric.
func DetectHashBasedIDs(db *sql.DB, sampleIDs []string) bool {
	if hasChildCountersTable(db) {
		return true
	}

	if containsHashBasedID(sampleIDs) {
		return true
	}

	suffixes := issueIDSuffixes(sampleIDs)
	if len(suffixes) < 2 {
		return false
	}

	return hasAdaptiveHashLengths(suffixes) ||
		hasLeadingZeroSuffix(suffixes) ||
		hasNonSequentialNumericSuffixes(suffixes)
}

func hasChildCountersTable(db *sql.DB) bool {
	var count int
	return db.QueryRow("SELECT COUNT(*) FROM child_counters").Scan(&count) == nil
}

func containsHashBasedID(sampleIDs []string) bool {
	for _, id := range sampleIDs {
		if isHashID(id) {
			return true
		}
	}
	return false
}

func issueIDSuffixes(sampleIDs []string) []string {
	var suffixes []string
	for _, id := range sampleIDs {
		parts := strings.SplitN(id, "-", 2)
		if len(parts) == 2 {
			suffixes = append(suffixes, strings.Split(parts[1], ".")[0])
		}
	}
	return suffixes
}

func hasAdaptiveHashLengths(suffixes []string) bool {
	lengths := make(map[int]int)
	for _, suffix := range suffixes {
		lengths[len(suffix)]++
	}
	return len(lengths) >= 3
}

func hasLeadingZeroSuffix(suffixes []string) bool {
	for _, suffix := range suffixes {
		if len(suffix) > 1 && suffix[0] == '0' {
			return true
		}
	}
	return false
}

func hasNonSequentialNumericSuffixes(suffixes []string) bool {
	numbers, allNumeric := numericIDSuffixes(suffixes)
	if !allNumeric || len(numbers) < 2 {
		return false
	}
	return !isRoughlySequential(numbers)
}

func numericIDSuffixes(suffixes []string) ([]int, bool) {
	var numbers []int
	for _, suffix := range suffixes {
		var number int
		if _, err := fmt.Sscanf(suffix, "%d", &number); err != nil {
			return nil, false
		}
		numbers = append(numbers, number)
	}
	return numbers, true
}

func isRoughlySequential(numbers []int) bool {
	numberCount := len(numbers)
	for i := 1; i < numberCount; i++ {
		diff := numbers[i] - numbers[i-1]
		if diff < 0 || diff > 100 {
			return false
		}
	}
	return true
}

// isHashID checks if a single ID contains hash characteristics
// Hash IDs contain hex letters (a-f), sequential IDs are only digits
// May have hierarchical suffix like .1 or .1.2
func isHashID(id string) bool {
	lastSeperatorIndex := strings.LastIndex(id, "-")
	if lastSeperatorIndex == -1 {
		return false
	}

	suffix := id[lastSeperatorIndex+1:]
	// Strip hierarchical suffix like .1 or .1.2
	baseSuffix := strings.Split(suffix, ".")[0]

	if len(baseSuffix) == 0 {
		return false
	}

	// Must be valid Base36 (0-9, a-z)
	if !regexp.MustCompile(`^[0-9a-z]+$`).MatchString(baseSuffix) {
		return false
	}

	// If it's 5+ characters long, it's almost certainly a hash ID
	// (sequential IDs rarely exceed 9999 = 4 digits)
	if len(baseSuffix) >= 5 {
		return true
	}

	// For shorter IDs, check if it contains any letter (a-z)
	// Sequential IDs are purely numeric
	return regexp.MustCompile(`[a-z]`).MatchString(baseSuffix)
}
