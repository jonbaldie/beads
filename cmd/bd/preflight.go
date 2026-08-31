package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/git"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/spf13/cobra"
)

// CheckResult represents the result of a single preflight check.
type CheckResult struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Skipped bool   `json:"skipped,omitempty"`
	Warning bool   `json:"warning,omitempty"`
	Output  string `json:"output,omitempty"`
	Command string `json:"command"`
}

// PreflightResult represents the overall preflight check results.
type PreflightResult struct {
	Checks  []CheckResult `json:"checks"`
	Passed  bool          `json:"passed"`
	Summary string        `json:"summary"`
}

var preflightCmd = &cobra.Command{
	Use:     "preflight",
	GroupID: "maint",
	Short:   "Show PR readiness checklist",
	Long: `Display a checklist of common pre-PR checks for contributors.

This command helps catch common issues before pushing to CI:
- Tests not run locally
- Lint errors
- Unformatted Go files
- .beads/issues.jsonl pollution
- Stale nix vendorHash
- Version mismatches

Examples:
  bd preflight              # Show checklist
  bd preflight --check      # Run checks automatically
  bd preflight --check --json  # JSON output for programmatic use
  bd preflight --check --skip-lint  # Explicitly skip lint check
`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runPreflight,
}

func init() {
	preflightCmd.Flags().Bool("check", false, "Run checks automatically")
	preflightCmd.Flags().Bool("fix", false, "Auto-fix issues where possible (vendorHash, version sync)")
	preflightCmd.Flags().Bool("json", false, "Output results as JSON")
	preflightCmd.Flags().Bool("skip-lint", false, "Skip lint check explicitly")

	rootCmd.AddCommand(preflightCmd)
}

func runPreflight(cmd *cobra.Command, _ []string) error {
	evt := metrics.NewCommandEvent("preflight")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	check, _ := cmd.Flags().GetBool("check")
	fix, _ := cmd.Flags().GetBool("fix")
	jsonOutput, _ := cmd.Flags().GetBool("json")
	skipLint, _ := cmd.Flags().GetBool("skip-lint")

	if fix {
		return runFixes(jsonOutput)
	}

	if check {
		return runChecks(jsonOutput, skipLint)
	}

	// Static checklist mode — tailor the checklist to the detected project
	// stack so non-Go projects don't get a misleading Go/Nix checklist (GH#4364).
	root := git.GetRepoRoot()
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}

	fmt.Println("PR Readiness Checklist:")
	fmt.Println()
	for _, item := range buildPreflightChecklist(root) {
		fmt.Printf("[ ] %s\n", item)
	}
	fmt.Println()
	fmt.Println("Run 'bd preflight --check' to validate automatically.")
	return nil
}

// fileExists reports whether name exists directly under dir.
func fileExists(dir, name string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// buildPreflightChecklist returns PR-readiness checklist items tailored to the
// project's language stack, detected from standard marker files in dir. A
// project with no recognized stack gets a generic reminder rather than a
// misleading Go checklist, and the repo's own Go+Nix specific items
// (gms_pure_go build tags, nix vendorHash, version.go vs default.nix) only
// appear where they apply (GH#4364).
func buildPreflightChecklist(dir string) []string {
	// Preserve the exact rich checklist when run inside the beads repo itself
	// (its primary audience), so the gms_pure_go build tags and nix/version
	// reminders beads contributors rely on are not lost.
	if isBeadsRepo(dir) {
		return []string{
			"Tests pass: go test -tags gms_pure_go -short ./...",
			"Lint passes: golangci-lint run --build-tags=gms_pure_go ./...",
			"Formatting: gofmt -l .",
			"No beads pollution: check .beads/issues.jsonl diff",
			"Nix hash current: go.sum unchanged or vendorHash updated",
			"Version sync: version.go matches default.nix",
		}
	}

	var items []string
	switch {
	case fileExists(dir, "go.mod"):
		items = append(items,
			"Tests pass: go test ./...",
			"Lint passes: golangci-lint run ./...",
			"Formatting: gofmt -l .",
		)
	case fileExists(dir, "package.json"):
		items = append(items,
			"Tests pass: npm test",
			"Types check: tsc --noEmit",
		)
	case fileExists(dir, "pyproject.toml"), fileExists(dir, "setup.py"):
		items = append(items,
			"Tests pass: pytest",
			"Lint passes: ruff check",
		)
	case fileExists(dir, "Cargo.toml"):
		items = append(items,
			"Tests pass: cargo test",
			"Lint passes: cargo clippy",
		)
	default:
		items = append(items, "Tests pass: run your project's test suite, linter, and formatter")
	}

	// Relevant to any beads workspace, regardless of language.
	if fileExists(dir, ".beads") {
		items = append(items, "No beads pollution: check .beads/issues.jsonl diff")
	}

	return items
}

// isBeadsRepo reports whether dir is the beads source repo, detected by the
// module path in go.mod. Used to keep preflight's repo-specific checklist.
func isBeadsRepo(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod")) //nolint:gosec // path is constructed internally (repo root + fixed filename)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module ")) == "github.com/jonbaldie/beads"
		}
	}
	return false
}
