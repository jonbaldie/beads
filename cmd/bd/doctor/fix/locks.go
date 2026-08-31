package fix

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StaleLockFiles removes stale lock files from the .beads directory.
// This is safe because:
// - Bootstrap/sync/startup locks use flock, which is released on process exit
// - If the flock is released but the file remains, the file is just clutter
func StaleLockFiles(path string) error {
	beadsDir, err := resolvedWorkspaceBeadsDir(path)
	if err != nil {
		return nil
	}
	var removed []string
	var errors []string
	removeAgedLock(beadsDir, "dolt.bootstrap.lock", 5*time.Minute, &removed, &errors)
	removeAgedLock(beadsDir, ".sync.lock", 1*time.Hour, &removed, &errors)
	// WARNING: DO NOT remove, delete, or modify files inside Dolt's .dolt/
	// directory — including noms/LOCK files. These are Dolt-internal files.
	// Removing them WILL cause unrecoverable data corruption and data loss.
	// Dolt manages these files itself; external interference is never safe.
	removeStaleStartlocks(beadsDir, &removed, &errors)
	return reportStaleLockCleanup(removed, errors)
}

func removeAgedLock(beadsDir, name string, maxAge time.Duration, removed, errors *[]string) {
	lockPath := filepath.Join(beadsDir, name)
	info, err := os.Stat(lockPath)
	if err != nil {
		return
	}
	if time.Since(info.ModTime()) <= maxAge {
		return
	}
	if err := os.Remove(lockPath); err != nil {
		*errors = append(*errors, fmt.Sprintf("%s: %v", name, err))
		return
	}
	*removed = append(*removed, name)
}

func removeStaleStartlocks(beadsDir string, removed, errors *[]string) {
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
		if time.Since(info.ModTime()) <= 30*time.Second {
			continue
		}
		lockPath := filepath.Join(beadsDir, entry.Name())
		if err := os.Remove(lockPath); err != nil {
			*errors = append(*errors, fmt.Sprintf("%s: %v", entry.Name(), err))
			continue
		}
		*removed = append(*removed, entry.Name())
	}
}

func reportStaleLockCleanup(removed, errors []string) error {
	if len(removed) > 0 {
		fmt.Printf("  Removed stale lock files: %s\n", strings.Join(removed, ", "))
	}
	if len(errors) > 0 {
		return fmt.Errorf("failed to remove some lock files: %s", strings.Join(errors, "; "))
	}
	return nil
}
