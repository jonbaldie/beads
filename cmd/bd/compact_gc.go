package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/beads"
)

// runCompactDolt runs Dolt garbage collection on the .beads/dolt directory
func runCompactDolt(dryRun bool) error {
	start := time.Now()
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
	}
	doltPath := filepath.Join(beadsDir, "dolt")
	if done, err := handleMissingCompactDolt(doltPath, dryRun); done {
		return err
	}
	sizeBefore := compactDoltDirectorySize(doltPath, "before")
	if dryRun {
		return renderCompactDoltDryRun(doltPath, sizeBefore)
	}
	if err := runDoltGarbageCollection(doltPath); err != nil {
		return err
	}
	sizeAfter := compactDoltDirectorySize(doltPath, "after")
	return renderCompactDoltResult(doltPath, sizeBefore, sizeAfter, time.Since(start))
}

func handleMissingCompactDolt(doltPath string, dryRun bool) (bool, error) {
	if _, err := os.Stat(doltPath); !os.IsNotExist(err) {
		return false, nil
	}
	if !dryRun {
		return true, HandleErrorWithHint(fmt.Sprintf("Dolt directory not found at %s", doltPath), "--dolt flag is only for repositories using the Dolt backend")
	}
	if isJSONOutput() {
		output := map[string]interface{}{
			"dry_run":   true,
			"dolt_path": doltPath,
			"available": false,
		}
		if err := outputJSON(output); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return true, nil
	}
	fmt.Printf("DRY RUN - Dolt garbage collection\n\n")
	fmt.Printf("Dolt directory: %s\n", doltPath)
	fmt.Printf("No local Dolt directory found; nothing to collect.\n")
	return true, nil
}

func compactDoltDirectorySize(doltPath, phase string) int64 {
	size, err := getDirSize(doltPath)
	if err != nil {
		message := "Warning: could not calculate directory size: %v\n"
		if phase == "after" {
			message = "Warning: could not calculate directory size after GC: %v\n"
		}
		fmt.Fprintf(os.Stderr, message, err)
		return 0
	}
	return size
}

func renderCompactDoltDryRun(doltPath string, sizeBefore int64) error {
	if isJSONOutput() {
		output := map[string]interface{}{
			"dry_run":      true,
			"dolt_path":    doltPath,
			"size_before":  sizeBefore,
			"size_display": formatBytes(sizeBefore),
		}
		if err := outputJSON(output); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return nil
	}
	fmt.Printf("DRY RUN - Dolt garbage collection\n\n")
	fmt.Printf("Dolt directory: %s\n", doltPath)
	fmt.Printf("Current size: %s\n", formatBytes(sizeBefore))
	fmt.Printf("\nRun without --dry-run to perform garbage collection.\n")
	return nil
}

func runDoltGarbageCollection(doltPath string) error {
	if _, err := exec.LookPath("dolt"); err != nil {
		return HandleErrorWithHint("dolt command not found in PATH", "install Dolt from https://github.com/dolthub/dolt")
	}
	if !isJSONOutput() {
		fmt.Printf("Running Dolt garbage collection...\n")
	}
	output, err := executeDoltGarbageCollection(doltPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: dolt gc failed: %v\n", err)
		if len(output) > 0 {
			fmt.Fprintf(os.Stderr, "Output: %s\n", string(output))
		}
		return SilentExit()
	}
	return nil
}

func executeDoltGarbageCollection(doltPath string) ([]byte, error) {
	// Run dolt gc without archive compression. Level 0 writes classic Snappy
	// table files instead of zstd archives, matching the in-process GC paths.
	// The external `dolt` on PATH has no version guarantee (unlike the
	// in-process paths, which are pinned by go.mod), so an older dolt that
	// predates --archive-level would otherwise abort compact where plain
	// `dolt gc` used to work. Detect that specific unknown-flag rejection
	// and retry with plain `dolt gc` rather than fail outright; any other
	// error (a genuine GC failure) is not swallowed.
	cmd := exec.Command("dolt", "gc", "--archive-level", "0") // #nosec G204 -- fixed command, no user input
	cmd.Dir = doltPath
	output, err := cmd.CombinedOutput()
	if err != nil && isUnknownArchiveLevelFlagError(string(output)) {
		if !isJSONOutput() {
			fmt.Fprintf(os.Stderr, "Notice: external dolt does not support --archive-level; falling back to plain 'dolt gc' (new table files may still use zstd archives)\n")
		}
		fallbackCmd := exec.Command("dolt", "gc") // #nosec G204 -- fixed command, no user input
		fallbackCmd.Dir = doltPath
		output, err = fallbackCmd.CombinedOutput()
	}
	return output, err
}

func renderCompactDoltResult(doltPath string, sizeBefore, sizeAfter int64, elapsed time.Duration) error {
	freed := sizeBefore - sizeAfter
	if freed < 0 {
		freed = 0 // GC may not always reduce size
	}
	if isJSONOutput() {
		result := map[string]interface{}{
			"success":       true,
			"dolt_path":     doltPath,
			"size_before":   sizeBefore,
			"size_after":    sizeAfter,
			"freed_bytes":   freed,
			"freed_display": formatBytes(freed),
			"elapsed_ms":    elapsed.Milliseconds(),
		}
		if err := outputJSON(result); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return nil
	}
	fmt.Printf("✓ Dolt garbage collection complete\n")
	fmt.Printf("  %s → %s (freed %s)\n", formatBytes(sizeBefore), formatBytes(sizeAfter), formatBytes(freed))
	fmt.Printf("  Time: %v\n", elapsed)
	return nil
}

// isUnknownArchiveLevelFlagError reports whether output (the combined
// stdout+stderr of `dolt gc --archive-level 0`) indicates the external dolt
// binary rejected --archive-level as an unrecognized flag, rather than a
// genuine GC failure. Older Dolt releases that predate the flag report this
// via the pinned dolt module's argparser, e.g.:
//
//	error: unknown option `archive-level'
//
// (see libraries/utils/argparser/errors.go in the pinned dolthub/dolt/go
// module). The check requires both an "unknown flag" phrasing AND the flag
// name to appear in the output, so a real GC failure that happens to
// contain the word "unknown" elsewhere is never misclassified as a missing
// flag and silently swallowed — genuine failures continue to fail.
func isUnknownArchiveLevelFlagError(output string) bool {
	lower := strings.ToLower(output)
	if !strings.Contains(lower, "archive-level") && !strings.Contains(lower, "archive_level") {
		return false
	}
	return strings.Contains(lower, "unknown option") ||
		strings.Contains(lower, "unknown flag") ||
		strings.Contains(lower, "flag provided but not defined")
}

// getDirSize calculates the total size of a directory recursively
func getDirSize(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}

// formatBytes formats a byte count as a human-readable string
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// progressBar renders a text-based progress bar.
func progressBar(current, total int) string {
	const width = 40
	if total == 0 {
		return "[" + string(make([]byte, width)) + "]"
	}
	filled := (current * width) / total
	bar := ""
	for i := 0; i < width; i++ {
		if i < filled {
			bar += "█"
		} else {
			bar += " "
		}
	}
	return "[" + bar + "]"
}

func init() {
	flags := persistentFlags()
	compactCmd.Flags().Bool("dry-run", false, "Preview without compacting")
	compactCmd.Flags().Int("tier", 1, "Compaction tier (only tier 1 is implemented)")
	compactCmd.Flags().Bool("all", false, "Process all candidates")
	compactCmd.Flags().String("id", "", "Compact specific issue")
	compactCmd.Flags().Bool("force", false, "Force compact (bypass checks, requires --id)")
	compactCmd.Flags().Int("batch-size", 10, "Issues per batch")
	compactCmd.Flags().Int("workers", 5, "Parallel workers")
	compactCmd.Flags().Bool("stats", false, "Show compaction statistics")
	compactCmd.Flags().BoolVar(&flags.JSONOutput, "json", false, "Output JSON format")

	// New mode flags
	compactCmd.Flags().Bool("analyze", false, "Analyze mode: export candidates for agent review")
	compactCmd.Flags().Bool("apply", false, "Apply mode: accept agent-provided summary")
	compactCmd.Flags().Bool("auto", false, "Auto mode: AI-powered compaction")
	compactCmd.Flags().String("summary", "", "Path to summary file (use '-' for stdin)")
	compactCmd.Flags().String("actor", "agent", "Actor name for audit trail")
	compactCmd.Flags().Int("limit", 0, "Limit number of candidates (0 = no limit)")
	compactCmd.Flags().Bool("dolt", false, "Dolt mode: run Dolt garbage collection on .beads/dolt")

	// Note: compactCmd is added to adminCmd in admin.go
}
