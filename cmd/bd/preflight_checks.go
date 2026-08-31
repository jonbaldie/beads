package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jonbaldie/beads/cmd/bd/doctor"
	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/git"
)

// runChecks executes all preflight checks and reports results.
func runChecks(jsonOutput, skipLint bool) error {
	results := collectPreflightChecks(skipLint)
	allPassed, passCount, skipCount, warnCount := summarizePreflightChecks(results)
	summary := formatPreflightSummary(passCount, len(results)-skipCount, warnCount, skipCount)

	if jsonOutput {
		return outputPreflightResults(results, allPassed, summary)
	}

	printPreflightResults(results)
	fmt.Println(summary)
	if !allPassed {
		return SilentExit()
	}
	return nil
}

func collectPreflightChecks(skipLint bool) []CheckResult {
	return []CheckResult{
		runTestCheck(),
		runLintCheck(skipLint),
		runFmtCheck(),
		runBeadsPollutionCheck(),
		runNixHashCheck(),
		runVersionSyncCheck(),
		runAgentDocDivergenceCheck(),
	}
}

func summarizePreflightChecks(results []CheckResult) (bool, int, int, int) {
	allPassed := true
	passCount := 0
	skipCount := 0
	warnCount := 0
	for _, result := range results {
		switch {
		case result.Skipped:
			skipCount++
		case result.Warning:
			warnCount++
		case result.Passed:
			passCount++
		default:
			allPassed = false
		}
	}
	return allPassed, passCount, skipCount, warnCount
}

func formatPreflightSummary(passCount, runCount, warnCount, skipCount int) string {
	summary := fmt.Sprintf("%d/%d checks passed", passCount, runCount)
	if warnCount > 0 {
		summary += fmt.Sprintf(", %d warning(s)", warnCount)
	}
	if skipCount > 0 {
		summary += fmt.Sprintf(" (%d skipped)", skipCount)
	}
	return summary
}

func outputPreflightResults(results []CheckResult, allPassed bool, summary string) error {
	result := PreflightResult{
		Checks:  results,
		Passed:  allPassed,
		Summary: summary,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		return HandleError("encoding preflight result: %v", err)
	}
	if !allPassed {
		return SilentExit()
	}
	return nil
}

func printPreflightResults(results []CheckResult) {
	for _, result := range results {
		printPreflightResult(result)
	}
}

func printPreflightResult(result CheckResult) {
	if result.Skipped {
		fmt.Printf("⚠ %s (skipped)\n", result.Name)
	} else if result.Warning {
		fmt.Printf("⚠ %s\n", result.Name)
	} else if result.Passed {
		fmt.Printf("✓ %s\n", result.Name)
	} else {
		fmt.Printf("✗ %s\n", result.Name)
	}
	fmt.Printf("  Command: %s\n", result.Command)
	printPreflightResultOutput(result)
	fmt.Println()
}

func printPreflightResultOutput(result CheckResult) {
	if result.Skipped && result.Output != "" {
		fmt.Printf("  Reason: %s\n", result.Output)
	} else if result.Warning && result.Output != "" {
		fmt.Printf("  Warning: %s\n", result.Output)
	} else if !result.Passed && result.Output != "" {
		fmt.Printf("  Output:\n")
		for _, line := range strings.Split(truncateOutput(result.Output, 500), "\n") {
			fmt.Printf("    %s\n", line)
		}
	}
}

// runTestCheck runs go test -short ./... and returns the result.
func runTestCheck() CheckResult {
	command := "go test -tags gms_pure_go -short ./..."
	cmd := exec.Command("go", "test", "-tags", "gms_pure_go", "-short", "./...")
	output, err := cmd.CombinedOutput()

	return CheckResult{
		Name:    "Tests pass",
		Passed:  err == nil,
		Output:  string(output),
		Command: command,
	}
}

// runLintCheck runs golangci-lint and returns the result.
func runLintCheck(skipLint bool) CheckResult {
	command := "golangci-lint run --build-tags=gms_pure_go ./..."
	if skipLint {
		return CheckResult{
			Name:    "Lint passes",
			Passed:  false,
			Skipped: true,
			Warning: true,
			Output:  "lint check explicitly skipped by --skip-lint",
			Command: command,
		}
	}

	// Check if golangci-lint is available
	if _, err := exec.LookPath("golangci-lint"); err != nil {
		return CheckResult{
			Name:    "Lint passes",
			Passed:  false,
			Output:  "golangci-lint not found in PATH (install it or rerun with --skip-lint)",
			Command: command,
		}
	}

	cmd := exec.Command("golangci-lint", "run", "--build-tags=gms_pure_go", "./...")
	output, err := cmd.CombinedOutput()

	return CheckResult{
		Name:    "Lint passes",
		Passed:  err == nil,
		Output:  string(output),
		Command: command,
	}
}

// runFmtCheck runs gofmt -l and fails if any files need formatting.
func runFmtCheck() CheckResult {
	command := "gofmt -l ."

	// Check if gofmt is available
	if _, err := exec.LookPath("gofmt"); err != nil {
		return CheckResult{
			Name:    "Formatting",
			Passed:  false,
			Output:  "gofmt not found in PATH (install Go toolchain)",
			Command: command,
		}
	}

	cmd := exec.Command("gofmt", "-l", ".")
	output, err := cmd.CombinedOutput()

	if err != nil {
		return CheckResult{
			Name:    "Formatting",
			Passed:  false,
			Output:  string(output),
			Command: command,
		}
	}

	unformatted := strings.TrimSpace(string(output))
	if unformatted != "" {
		return CheckResult{
			Name:    "Formatting",
			Passed:  false,
			Output:  fmt.Sprintf("Unformatted files:\n%s\nRun: gofmt -w .", unformatted),
			Command: command,
		}
	}

	return CheckResult{
		Name:    "Formatting",
		Passed:  true,
		Command: command,
	}
}

// runBeadsPollutionCheck detects .beads/issues.jsonl modifications vs merge base.
func runBeadsPollutionCheck() CheckResult {
	command := "git diff -- .beads/issues.jsonl"
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return passingBeadsPollutionResult(command)
	}

	issuesPath, earlyResult := resolveBeadsPollutionPath(command, beadsDir)
	if earlyResult != nil {
		return *earlyResult
	}

	branch, err := currentBeadsBranch()
	if err != nil {
		return CheckResult{
			Name:    "No beads pollution",
			Passed:  false,
			Skipped: true,
			Output:  fmt.Sprintf("Cannot determine branch: %v", err),
			Command: command,
		}
	}

	diffOutput := beadsPollutionDiff(branch, issuesPath)
	if len(strings.TrimSpace(string(diffOutput))) > 0 {
		return CheckResult{
			Name:    "No beads pollution",
			Passed:  false,
			Output:  ".beads/issues.jsonl has been modified — revert changes before pushing",
			Command: command,
		}
	}

	return CheckResult{
		Name:    "No beads pollution",
		Passed:  true,
		Command: command,
	}
}

func passingBeadsPollutionResult(command string) CheckResult {
	return CheckResult{
		Name:    "No beads pollution",
		Passed:  true,
		Command: command,
	}
}

func resolveBeadsPollutionPath(command, beadsDir string) (string, *CheckResult) {
	issuesPath := filepath.Join(beadsDir, "issues.jsonl")
	if !filepath.IsAbs(issuesPath) {
		return issuesPath, nil
	}

	repoRoot := git.GetRepoRoot()
	if repoRoot == "" {
		result := passingBeadsPollutionResult(command)
		return "", &result
	}

	rel, err := filepath.Rel(repoRoot, issuesPath)
	if err != nil || isPathOutsideRepo(rel) {
		result := passingBeadsPollutionResult(command)
		result.Output = "Skipped: .beads is outside working tree (worktree setup)"
		return "", &result
	}
	return rel, nil
}

func currentBeadsBranch() (string, error) {
	branchCmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	branchOut, err := branchCmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(branchOut)), nil
}

func beadsPollutionDiff(branch, issuesPath string) []byte {
	if branch != "main" && branch != "HEAD" {
		cmd := exec.Command("git", "diff", "origin/main...HEAD", "--", issuesPath)
		output, _ := cmd.Output()
		return output
	}

	cmd := exec.Command("git", "diff", "HEAD", "--", issuesPath)
	out1, _ := cmd.Output()
	cmd = exec.Command("git", "diff", "--cached", "--", issuesPath)
	out2, _ := cmd.Output()
	return append(out1, out2...)
}

// isPathOutsideRepo checks if a relative path (from filepath.Rel) points
// outside the base directory by inspecting the first path segment.
func isPathOutsideRepo(rel string) bool {
	if rel == "" {
		return false
	}
	first := rel
	if i := strings.IndexAny(rel, "/\\"); i > 0 {
		first = rel[:i]
	}
	return first == ".."
}

// runNixHashCheck checks if go.sum has uncommitted changes that may require vendorHash update.
func runNixHashCheck() CheckResult {
	command := "git diff HEAD -- go.sum"

	// Check for unstaged changes to go.sum
	cmd := exec.Command("git", "diff", "--name-only", "HEAD", "--", "go.sum")
	output, _ := cmd.Output()

	// Check for staged changes to go.sum
	stagedCmd := exec.Command("git", "diff", "--name-only", "--cached", "--", "go.sum")
	stagedOutput, _ := stagedCmd.Output()

	hasChanges := len(strings.TrimSpace(string(output))) > 0 || len(strings.TrimSpace(string(stagedOutput))) > 0

	if hasChanges {
		return CheckResult{
			Name:    "Nix hash current",
			Passed:  false,
			Warning: true,
			Output:  "go.sum has uncommitted changes - vendorHash in default.nix may need updating",
			Command: command,
		}
	}

	return CheckResult{
		Name:    "Nix hash current",
		Passed:  true,
		Output:  "",
		Command: command,
	}
}

// runVersionSyncCheck checks that all version files are in sync.
// Prefers scripts/check-versions.sh (matches CI) with fallback to inline logic.
func runVersionSyncCheck() CheckResult {
	command := "scripts/check-versions.sh"

	// Try using the script (matches CI's check-version-consistency job)
	if _, err := os.Stat("scripts/check-versions.sh"); err == nil {
		cmd := exec.Command("bash", "scripts/check-versions.sh")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return CheckResult{
				Name:    "Version sync",
				Passed:  false,
				Output:  string(output),
				Command: command,
			}
		}
		return CheckResult{
			Name:    "Version sync",
			Passed:  true,
			Output:  string(output),
			Command: command,
		}
	}

	// Fallback: inline comparison of version.go and default.nix
	command = "Compare cmd/bd/version.go and default.nix"

	// Read version.go
	versionGoContent, err := os.ReadFile("cmd/bd/version.go")
	if err != nil {
		return CheckResult{
			Name:    "Version sync",
			Passed:  false,
			Skipped: true,
			Output:  fmt.Sprintf("Cannot read cmd/bd/version.go: %v", err),
			Command: command,
		}
	}

	// Extract version from version.go
	versionGoRe := regexp.MustCompile(`Version\s*=\s*"([^"]+)"`)
	versionGoMatch := versionGoRe.FindSubmatch(versionGoContent)
	if versionGoMatch == nil {
		return CheckResult{
			Name:    "Version sync",
			Passed:  false,
			Skipped: true,
			Output:  "Cannot parse version from version.go",
			Command: command,
		}
	}
	goVersion := string(versionGoMatch[1])

	// Read default.nix
	nixContent, err := os.ReadFile("default.nix")
	if err != nil {
		return CheckResult{
			Name:    "Version sync",
			Passed:  true,
			Skipped: true,
			Output:  "default.nix not found (skipping nix version check)",
			Command: command,
		}
	}

	// Extract version from default.nix
	nixRe := regexp.MustCompile(`version\s*=\s*"([^"]+)"`)
	nixMatch := nixRe.FindSubmatch(nixContent)
	if nixMatch == nil {
		return CheckResult{
			Name:    "Version sync",
			Passed:  false,
			Skipped: true,
			Output:  "Cannot parse version from default.nix",
			Command: command,
		}
	}
	nixVersion := string(nixMatch[1])

	if goVersion != nixVersion {
		return CheckResult{
			Name:    "Version sync",
			Passed:  false,
			Output:  fmt.Sprintf("Version mismatch: version.go=%s, default.nix=%s", goVersion, nixVersion),
			Command: command,
		}
	}

	return CheckResult{
		Name:    "Version sync",
		Passed:  true,
		Output:  fmt.Sprintf("Versions match: %s", goVersion),
		Command: command,
	}
}

// runAgentDocDivergenceCheck flags drift between AGENTS.md and CLAUDE.md
// user-authored regions so the inconsistency is caught pre-PR rather than in
// review.
func runAgentDocDivergenceCheck() CheckResult {
	command := "bd doctor (Agent Doc Divergence)"

	repoRoot := git.GetRepoRoot()
	if repoRoot == "" {
		repoRoot = "."
	}
	check := doctor.CheckAgentDocDivergence(repoRoot)
	if check.Status == doctor.StatusOK {
		return CheckResult{
			Name:    "AGENTS.md/CLAUDE.md in sync",
			Passed:  true,
			Command: command,
		}
	}
	output := check.Message
	if check.Detail != "" {
		output += "\n" + check.Detail
	}
	if check.Fix != "" {
		output += "\n" + check.Fix
	}
	return CheckResult{
		Name:    "AGENTS.md/CLAUDE.md in sync",
		Passed:  false,
		Warning: true,
		Output:  output,
		Command: command,
	}
}

// truncateOutput truncates output to maxLen characters, adding ellipsis if truncated.
func truncateOutput(s string, maxLen int) string {
	if len(s) <= maxLen {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(s[:maxLen]) + "\n... (truncated)"
}

// fixResult reports the outcome of one auto-fix operation.
type fixResult struct {
	Name    string `json:"name"`
	Fixed   bool   `json:"fixed"`
	Skipped bool   `json:"skipped,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Error   string `json:"error,omitempty"`
}
