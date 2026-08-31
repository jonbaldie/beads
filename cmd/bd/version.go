package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/spf13/cobra"
)

var (
	// Version is the current version of bd (overridden by ldflags at build time)
	Version = "1.2.3"
	// Build can be set via ldflags at compile time
	Build = "dev"
	// Commit and branch the git revision the binary was built from (optional ldflag)
	Commit = ""
	Branch = ""
)

var versionCmd = &cobra.Command{
	Use:           "version",
	Short:         "Print version information",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("version")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		commit := resolveCommitHash()
		branch := resolveBranch()

		if isJSONOutput() {
			result := map[string]interface{}{
				"version": Version,
				"build":   Build,
			}
			if commit != "" {
				result["commit"] = commit
			}
			if branch != "" {
				result["branch"] = branch
			}
			if err := outputJSON(result); err != nil {
				return err
			}
		} else {
			if commit != "" && branch != "" {
				fmt.Printf("bd version %s (%s: %s@%s)\n", Version, Build, branch, shortCommit(commit))
			} else if commit != "" {
				fmt.Printf("bd version %s (%s: %s)\n", Version, Build, shortCommit(commit))
			} else {
				fmt.Printf("bd version %s (%s)\n", Version, Build)
			}
		}

		// Check for multiple bd binaries in PATH
		if dups := findDuplicateBinaries(); len(dups) > 1 {
			fmt.Fprintf(os.Stderr, "\nWarning: multiple 'bd' binaries found in PATH:\n")
			for _, p := range dups {
				fmt.Fprintf(os.Stderr, "  %s\n", p)
			}
			fmt.Fprintf(os.Stderr, "The first one is being used. Remove duplicates to avoid confusion.\n")
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}

func resolveCommitHash() string {
	if Commit != "" {
		return Commit
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				return setting.Value
			}
		}
	}

	return ""
}

func shortCommit(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func resolveBranch() string {
	if Branch != "" {
		return Branch
	}
	if branch := buildInfoSetting("vcs.branch"); branch != "" {
		return branch
	}
	return runtimeBranch()
}

func buildInfoSetting(key string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == key && setting.Value != "" {
			return setting.Value
		}
	}
	return ""
}

func runtimeBranch() string {
	// Use symbolic-ref to work in fresh repos without commits.
	rc, err := beads.GetRepoContext()
	if err != nil {
		return ""
	}
	cmd := rc.GitCmdCWD(context.Background(), "symbolic-ref", "--short", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	branch := strings.TrimSpace(string(output))
	if branch == "" || branch == "HEAD" {
		return ""
	}
	return branch
}

// findDuplicateBinaries searches PATH for all "bd" executables.
// Returns their full paths. If len > 1, there are duplicates.
func findDuplicateBinaries() []string {
	name := "bd"
	if runtime.GOOS == "windows" {
		name = "bd.exe"
	}

	seen := make(map[string]bool)
	var paths []string

	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		candidate := filepath.Join(dir, name)
		// Resolve symlinks so we don't double-count
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			// Try the raw path (might be a valid binary without symlinks)
			resolved = candidate
		}
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if info.IsDir() {
			continue
		}
		if !seen[resolved] {
			seen[resolved] = true
			paths = append(paths, candidate)
		}
	}
	return paths
}
