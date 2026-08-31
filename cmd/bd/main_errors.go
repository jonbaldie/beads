package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// isFreshCloneError checks if the error is due to a fresh clone scenario
// where the database exists but is missing required config (like issue_prefix).
// This happens when someone clones a repo with beads but needs to initialize.
func isFreshCloneError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// Check for the specific migration invariant error pattern
	return strings.Contains(errStr, "post-migration validation failed") &&
		strings.Contains(errStr, "required config key missing: issue_prefix")
}

// handleFreshCloneError displays a helpful message when a fresh clone is detected
// and returns true if the error was handled (so caller should exit).
// If not a fresh clone error, returns false and does nothing.
func handleFreshCloneError(err error) bool {
	if !isFreshCloneError(err) {
		return false
	}

	fmt.Fprintf(os.Stderr, "Error: Database not initialized\n\n")
	fmt.Fprintf(os.Stderr, "This appears to be a fresh clone or an existing project whose database needs recovery.\n")
	fmt.Fprintf(os.Stderr, "\nTo diagnose, run:\n")
	fmt.Fprintf(os.Stderr, "  bd doctor\n\n")
	fmt.Fprintf(os.Stderr, "If this is an existing project or fresh clone, run: bd bootstrap\n")
	fmt.Fprintf(os.Stderr, "To create a brand-new database from scratch: bd init --prefix <your-prefix>\n")
	return true
}

// isWispOperation returns true if the command operates on ephemeral wisps.
// Wisp operations use direct store access (local-only).
// Detects:
//   - mol wisp subcommands (create, list, gc, or direct proto invocation)
//   - mol burn (only operates on wisps)
//   - mol squash (condenses wisps to digests)
//   - Commands with ephemeral issue IDs in args (bd-*-wisp-*, wisp-*, or legacy eph-*)
func isWispOperation(cmd *cobra.Command, args []string) bool {
	if isWispCommand(cmd) {
		return true
	}
	return hasWispArgument(args)
}

func isWispCommand(cmd *cobra.Command) bool {
	parent := cmd.Parent()
	if parent == nil {
		return false
	}
	parentName, cmdName := parent.Name(), cmd.Name()
	// Direct wisp command or subcommands under wisp.
	if parentName == "wisp" || cmdName == "wisp" {
		return true
	}
	// mol burn and mol squash are wisp-only operations.
	return parentName == "mol" && (cmdName == "burn" || cmdName == "squash")
}

func hasWispArgument(args []string) bool {
	// Ephemeral IDs have "wisp" segment: bd-wisp-xxx, gt-wisp-xxx, wisp-xxx.
	// Also detect legacy "eph" prefix for backwards compatibility.
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if isWispArgument(arg) {
			return true
		}
	}
	return false
}

func isWispArgument(arg string) bool {
	return strings.Contains(arg, "-wisp-") || strings.HasPrefix(arg, "wisp-") ||
		strings.Contains(arg, "-eph-") || strings.HasPrefix(arg, "eph-")
}
