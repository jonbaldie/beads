package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CheckCircuitBreaker checks for stale circuit breaker state files that may
// block all bd operations. Returns a fixable DoctorCheck if stale files exist.
func CheckCircuitBreaker() DoctorCheck {
	// Derived from os.TempDir() to match where the storage layer now writes
	// breaker files; hardcoding "/tmp" checked the wrong place on Windows,
	// where files land under %TEMP% not C:\tmp (GH#4636).
	dir := filepath.Join(os.TempDir(), "beads-circuit")
	matches, err := filepath.Glob(filepath.Join(dir, "beads-dolt-circuit-*.json"))
	if err != nil || len(matches) == 0 {
		return okCircuitBreaker()
	}
	staleCount := countStaleCircuitFiles(matches)
	if staleCount == 0 {
		return okCircuitBreaker()
	}
	return DoctorCheck{
		Name:     "Circuit Breaker",
		Status:   StatusWarning,
		Message:  fmt.Sprintf("%d stale circuit breaker file(s) found in %s", staleCount, dir),
		Fix:      "Run 'bd doctor --fix' to clear stale circuit breaker files",
		Category: CategoryRuntime,
	}
}

func okCircuitBreaker() DoctorCheck {
	return DoctorCheck{
		Name:     "Circuit Breaker",
		Status:   StatusOK,
		Message:  "No stale circuit breaker files",
		Category: CategoryRuntime,
	}
}

func countStaleCircuitFiles(matches []string) int {
	var staleCount int
	for _, path := range matches {
		if isStaleCircuitFile(path) {
			staleCount++
		}
	}
	return staleCount
}

type circuitBreakerState struct {
	State     string    `json:"state"`
	TrippedAt time.Time `json:"tripped_at,omitempty"`
	LastFail  time.Time `json:"last_failure,omitempty"`
}

func isStaleCircuitFile(path string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is from filepath.Glob with controlled pattern
	if err != nil {
		return false
	}
	var state circuitBreakerState
	if err := json.Unmarshal(data, &state); err != nil {
		return true // corrupt file counts as stale
	}
	if state.State != "open" && state.State != "half-open" {
		return false
	}
	// Only flag as stale if the breaker has been tripped for longer
	// than the staleness TTL (5 minutes). A recently-tripped breaker
	// during a real outage should not be cleared.
	ref := state.TrippedAt
	if ref.IsZero() {
		ref = state.LastFail
	}
	return ref.IsZero() || time.Since(ref) > 5*time.Minute
}
