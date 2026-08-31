package debug

import (
	"fmt"
	"os"
	"sync/atomic"
)

var (
	enabled     = os.Getenv("BD_DEBUG") != ""
	verboseMode atomic.Bool
	quietMode   atomic.Bool
)

func Enabled() bool {
	return enabled || verboseMode.Load()
}

// SetVerbose enables verbose/debug output
func SetVerbose(verbose bool) {
	verboseMode.Store(verbose)
}

// SetQuiet enables quiet mode (suppress non-essential output)
func SetQuiet(quiet bool) {
	quietMode.Store(quiet)
}

// IsQuiet returns true if quiet mode is enabled
func IsQuiet() bool {
	return quietMode.Load()
}

func Logf(format string, args ...interface{}) {
	if enabled || verboseMode.Load() {
		fmt.Fprintf(os.Stderr, format, args...)
	}
}

func Printf(format string, args ...interface{}) {
	if enabled || verboseMode.Load() {
		fmt.Printf(format, args...)
	}
}

// PrintNormal prints output unless quiet mode is enabled.
// Use this for normal informational output that should be suppressed in quiet mode.
func PrintNormal(format string, args ...interface{}) {
	if !quietMode.Load() {
		fmt.Printf(format, args...)
	}
}

// PrintlnNormal prints a line unless quiet mode is enabled.
func PrintlnNormal(args ...interface{}) {
	if !quietMode.Load() {
		fmt.Println(args...)
	}
}
