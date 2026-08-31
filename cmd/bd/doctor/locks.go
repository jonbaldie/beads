package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// staleLockThresholds defines the age thresholds for each lock type.
// Lock files older than these thresholds are considered stale.
var staleLockThresholds = map[string]time.Duration{
	"bootstrap.lock": 5 * time.Minute, // Bootstrap should complete quickly
	".sync.lock":     1 * time.Hour,   // Sync can be slow for large repos
}

// CheckStaleLockFiles detects leftover lock files from crashed processes.
// Stale lock files can block bootstrap and sync operations.
func CheckStaleLockFiles(path string) DoctorCheck {
	beadsDir := ResolveBeadsDirForRepo(path)
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		return DoctorCheck{
			Name:     "Lock Files",
			Status:   StatusOK,
			Message:  "N/A (no .beads directory)",
			Category: CategoryRuntime,
		}
	}
	staleFiles, details := collectStaleLockFindings(beadsDir)
	return reportStaleLockFiles(staleFiles, details)
}

func collectStaleLockFindings(beadsDir string) (staleFiles, details []string) {
	appendAgedNamedLock(beadsDir, "dolt.bootstrap.lock", "bootstrap.lock", &staleFiles, &details)
	appendAgedNamedLock(beadsDir, ".sync.lock", ".sync.lock", &staleFiles, &details)
	// WARNING: DO NOT remove, delete, or modify files inside Dolt's .dolt/
	// directory — including noms/LOCK files. These are Dolt-internal files.
	// Removing them WILL cause unrecoverable data corruption and data loss.
	// Dolt manages these files itself; external interference is never safe.
	appendStaleStartlocks(beadsDir, &staleFiles, &details)
	return staleFiles, details
}

func appendAgedNamedLock(beadsDir, filename, thresholdKey string, staleFiles, details *[]string) {
	info, err := os.Stat(filepath.Join(beadsDir, filename))
	if err != nil {
		return
	}
	age := time.Since(info.ModTime())
	threshold := staleLockThresholds[thresholdKey]
	if age <= threshold {
		return
	}
	*staleFiles = append(*staleFiles, filename)
	*details = append(*details, fmt.Sprintf("%s: age %s (threshold: %s)", filename, age.Round(time.Second), threshold))
}

func appendStaleStartlocks(beadsDir string, staleFiles, details *[]string) {
	entries, err := os.ReadDir(beadsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".startlock") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		age := time.Since(info.ModTime())
		if age <= 30*time.Second {
			continue
		}
		*staleFiles = append(*staleFiles, entry.Name())
		*details = append(*details, fmt.Sprintf("%s: age %s (startup locks should be < 30s)", entry.Name(), age.Round(time.Second)))
	}
}

func reportStaleLockFiles(staleFiles, details []string) DoctorCheck {
	if len(staleFiles) == 0 {
		return DoctorCheck{
			Name:     "Lock Files",
			Status:   StatusOK,
			Message:  "No stale lock files",
			Category: CategoryRuntime,
		}
	}
	return DoctorCheck{
		Name:     "Lock Files",
		Status:   StatusWarning,
		Message:  fmt.Sprintf("%d stale lock file(s): %s", len(staleFiles), strings.Join(staleFiles, ", ")),
		Detail:   strings.Join(details, "; "),
		Fix:      "Run 'bd doctor --fix' to remove stale lock files, or delete manually from .beads/",
		Category: CategoryRuntime,
	}
}
