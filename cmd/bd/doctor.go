package main

import (
	"fmt"
	"os"

	"github.com/jonbaldie/beads/cmd/bd/doctor"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

// Status constants for doctor checks
const (
	statusOK      = "ok"
	statusWarning = "warning"
	statusError   = "error"
)

type doctorCheck struct {
	Name     string `json:"name"`
	Status   string `json:"status"` // statusOK, statusWarning, or statusError
	Message  string `json:"message"`
	Detail   string `json:"detail,omitempty"` // Additional detail like storage type
	Fix      string `json:"fix,omitempty"`
	Category string `json:"category,omitempty"` // category for grouping in output
}

type doctorResult struct {
	Path            string            `json:"path"`
	Checks          []doctorCheck     `json:"checks"`
	OverallOK       bool              `json:"overall_ok"`
	CLIVersion      string            `json:"cli_version"`
	Timestamp       string            `json:"timestamp,omitempty"`        // ISO8601 timestamp for historical tracking
	Platform        map[string]string `json:"platform,omitempty"`         // platform info for debugging
	SuppressedCount int               `json:"suppressed_count,omitempty"` // GH#1095: number of suppressed warnings
}

type doctorFixOptions struct {
	enabled     bool
	yes         bool
	interactive bool
	dryRun      bool
	childParent bool
	verbose     bool
}

type doctorModeOptions struct {
	perf            bool
	checkHealth     bool
	check           string
	clean           bool
	deep            bool
	orchestrator    bool
	duplicatesLimit int
	server          bool
	migration       string
	agent           bool
}

type doctorOptions struct {
	fix    doctorFixOptions
	mode   doctorModeOptions
	output string
}

func defaultDoctorOptions() doctorOptions {
	return doctorOptions{mode: doctorModeOptions{duplicatesLimit: 1000}}
}

func doctorOptionsFromCommand(cmd *cobra.Command) doctorOptions {
	opts := defaultDoctorOptions()
	opts.fix.enabled, _ = cmd.Flags().GetBool("fix")
	opts.fix.yes, _ = cmd.Flags().GetBool("yes")
	opts.fix.interactive, _ = cmd.Flags().GetBool("interactive")
	opts.fix.dryRun, _ = cmd.Flags().GetBool("dry-run")
	opts.output, _ = cmd.Flags().GetString("output")
	opts.fix.childParent, _ = cmd.Flags().GetBool("fix-child-parent")
	opts.fix.verbose, _ = cmd.Flags().GetBool("verbose")
	opts.mode.perf, _ = cmd.Flags().GetBool("perf")
	opts.mode.checkHealth, _ = cmd.Flags().GetBool("check-health")
	opts.mode.check, _ = cmd.Flags().GetString("check")
	opts.mode.clean, _ = cmd.Flags().GetBool("clean")
	opts.mode.deep, _ = cmd.Flags().GetBool("deep")
	opts.mode.orchestrator, _ = cmd.Flags().GetBool("orchestrator")
	opts.mode.duplicatesLimit, _ = cmd.Flags().GetInt("orchestrator-duplicates-threshold")
	opts.mode.server, _ = cmd.Flags().GetBool("server")
	opts.mode.migration, _ = cmd.Flags().GetString("migration")
	opts.mode.agent, _ = cmd.Flags().GetBool("agent")
	return opts
}

// ConfigKeyHintsDoctor is the config key for suppressing doctor hints
const ConfigKeyHintsDoctor = "hints.doctor"

var doctorCmd = &cobra.Command{
	Use:     "doctor [path]",
	GroupID: "maint",
	// doctor diagnoses installation health and must run without opening the
	// store. It opts out of store init via the annotation seam rather than the
	// noDbCommands list (see commandOptsOutOfStore in main.go).
	Annotations: map[string]string{skipStoreAnnotation: "1"},
	Short:       "Check and fix beads installation health (start here)",
	Long: `Sanity check the beads installation for the current directory or specified path.

This command checks:
  - If .beads/ directory exists
  - Database version and migration status
  - Schema compatibility (all required tables and columns present)
  - Whether using hash-based vs sequential IDs
  - If CLI version is current (checks GitHub releases)
  - If Claude plugin is current (when running in Claude Code)
  - File permissions
  - Circular dependencies
  - Git hooks (pre-commit, post-merge, pre-push)
  - .beads/.gitignore up to date
  - Metadata.json version tracking (LastBdVersion field)

Storage Availability:
  Full diagnostics, --perf, --deep, --server, --migration, and
  --check=validate currently require Dolt server mode. Embedded Dolt
  supports --check=artifacts, --check=conventions, and
  --check=pollution. --check-health has a limited hook-health fallback.
  Unsupported combinations return a notice without changing storage.

Performance Mode (--perf):
  Run performance diagnostics on your database:
  - Times key operations (bd ready, bd list, bd show, etc.)
  - Collects system info (OS, arch, database stats)
  - Generates CPU profile for analysis
  - Outputs shareable report for bug reports

Export Mode (--output):
  Save diagnostics to a JSON file for historical analysis and bug reporting.
  Includes timestamp and platform info for tracking intermittent issues.

Specific Check Mode (--check):
  Run a specific check in detail. Available checks:
  - artifacts: Detect and optionally clean beads classic artifacts
    (stale JSONL, SQLite files, cruft .beads dirs). Use with --clean.
  - conventions: Check for convention drift (lint warnings, stale
    issues, orphaned issues). Advisory only - warns, never blocks.
  - pollution: Detect and optionally clean test issues from database
  - validate: Run focused data-integrity checks (duplicates, orphaned
    deps, test pollution, git conflicts). Use with --fix to auto-repair.

Deep Validation Mode (--deep):
  Validate full graph integrity. May be slow on large databases.
  Additional checks:
  - Parent consistency: All parent-child deps point to existing issues
  - Dependency integrity: All deps reference valid issues
  - Epic completeness: Find epics ready to close (all children closed)
  - Agent bead integrity: Agent beads have valid state values
  - Mail thread integrity: Thread IDs reference existing issues
  - Molecule integrity: Molecules have valid parent-child structures

Server Mode (--server):
  Run health checks for Dolt server mode connections (bd-dolt.2.3):
  - Server reachable: Can connect to configured host:port?
  - Dolt version: Is it a Dolt server (not vanilla MySQL)?
  - Database exists: Does the 'beads' database exist?
  - Schema compatible: Can query beads tables?
  - Connection pool: Pool health metrics

Legacy Dolt Migration Validation Mode (--migration):
  Retained for older SQLite-to-Dolt migration workflows and available only in
  Dolt server mode. It is not the migration path for removed backends
  (PostgreSQL, MySQL, SQLite); those fail closed with export/import guidance.
  Combine
  with --json for machine-parseable diagnostic output.

Agent Mode (--agent):
  Output diagnostics designed for AI agent consumption. Instead of terse
  pass/fail messages, each issue includes:
  - Observed state: what the system actually looks like
  - Expected state: what it should look like
  - Explanation: full prose context about the issue and why it matters
  - Commands: exact remediation commands to run
  - Source files: where in the codebase to investigate further
  - Severity: blocking (prevents operation), degraded (partial function),
    or advisory (informational only)
  ZFC-compliant: Go observes and reports, the agent decides and acts.
  Combine with --json for structured agent-facing output.

Suppressing Warnings:
  Suppress specific warnings by setting doctor.suppress.<check-slug> config:
    bd config set doctor.suppress.pending-migrations true
    bd config set doctor.suppress.git-hooks true
  Check names are converted to slugs: "Git Hooks" → "git-hooks".
  Only warnings are suppressed; errors and passing checks always show.
  To unsuppress: bd config unset doctor.suppress.<slug>

Examples:
  bd doctor              # Check current directory
  bd doctor /path/to/repo # Check specific repository
  bd doctor --json       # Machine-readable output
  bd doctor --agent      # Agent-facing diagnostic output
  bd doctor --agent --json  # Structured agent diagnostics (JSON)
  bd doctor --fix        # Automatically fix issues (with confirmation)
  bd doctor --fix --yes  # Automatically fix issues (no confirmation)
  bd doctor --fix -i     # Confirm each fix individually
  bd doctor --fix --fix-child-parent  # Also fix child→parent deps (opt-in)
  bd doctor --fix --force # Force repair even when database can't be opened
  bd doctor --fix --source=jsonl # Rebuild database from a JSONL export
  bd doctor --dry-run    # Preview what --fix would do without making changes
  bd doctor --perf       # Performance diagnostics
  bd doctor --output diagnostics.json  # Export diagnostics to file
  bd doctor --check=artifacts           # Show classic artifacts (JSONL, SQLite, cruft dirs)
  bd doctor --check=artifacts --clean  # Delete safe-to-delete artifacts (with confirmation)
  bd doctor --check=conventions        # Convention drift check (lint, stale, orphans)
  bd doctor --check=pollution          # Show potential test issues
  bd doctor --check=pollution --clean  # Delete test issues (with confirmation)
  bd doctor --check=validate         # Data-integrity checks only
  bd doctor --check=validate --fix   # Auto-fix data-integrity issues
  bd doctor --deep             # Full graph integrity validation
  bd doctor --server           # Dolt server mode health checks
  bd doctor --migration=pre    # Legacy Dolt-server migration diagnostic
  bd doctor --migration=post   # Legacy Dolt-server completion diagnostic
  bd doctor --migration=pre --json  # Machine-parseable legacy diagnostic`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runDoctor,
}

func init() {
	doctorCmd.Flags().Bool("fix", false, "Automatically fix issues where possible")
	doctorCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt (for non-interactive use)")
	doctorCmd.Flags().BoolP("interactive", "i", false, "Confirm each fix individually")
	doctorCmd.Flags().Bool("dry-run", false, "Preview fixes without making changes")
	doctorCmd.Flags().Bool("fix-child-parent", false, "Remove child→parent dependencies (opt-in)")
	doctorCmd.Flags().BoolP("verbose", "v", false, "Show all checks (default shows only warnings/errors)")
	doctorCmd.Flags().Bool("orchestrator", false, "Running in orchestrator multi-workspace mode (routes.jsonl is expected, higher duplicate tolerance)")
	doctorCmd.Flags().Int("orchestrator-duplicates-threshold", 1000, "Duplicate tolerance threshold for orchestrator mode (wisps are ephemeral)")
	doctorCmd.Flags().Bool("server", false, "Run Dolt server mode health checks (connectivity, version, schema)")
	doctorCmd.Flags().String("migration", "", "Run legacy Dolt-server migration diagnostics: 'pre' or 'post'")
	doctorCmd.Flags().Bool("agent", false, "Agent-facing diagnostic mode: rich context for AI agents (ZFC-compliant)")
}

func shouldSkipDoctorNetworkChecks() bool {
	return isJSONOutput() || !ui.IsTerminal()
}

// validateDoctorWorkspaceBackend keeps doctor diagnostics read-only when metadata
// selects a removed or unknown implementation or cannot be parsed. Doctor contains
// direct diagnostic store paths and may run under shared-server mode, so corrupt
// metadata must be rejected before version tracking or any database check begins.
func validateDoctorWorkspaceBackend(path string) error {
	beadsDir := doctor.ResolveBeadsDirForRepo(path)
	if err := guardLegacyUpgradeWorkspace(beadsDir); err != nil {
		return err
	}
	cfg, err := configfile.LoadForDiscovery(beadsDir)
	if err != nil {
		return fmt.Errorf("failed to load %s: %w; no storage database was opened or modified; fix or restore metadata.json and retry", configfile.ConfigPath(beadsDir), err)
	}
	return validateConfiguredBackend(cfg)
}

// printLegacyUpgradeDiagnostic preserves doctor as a store-free repair path:
// the workspace is recognized, but no storage or metadata migration is opened.
func printLegacyUpgradeDiagnostic(err error, agent bool) error {
	if isJSONOutput() || agent {
		return outputJSON(map[string]any{
			"status":  "warning",
			"code":    "legacy_upgrade_required",
			"message": err.Error(),
			"guide":   "docs/getting-started/upgrading.md#cross-era-upgrades",
		})
	}
	_, _ = fmt.Fprintf(os.Stdout, "Warning: %v\n", err)
	_, _ = fmt.Fprintln(os.Stdout, "Follow docs/getting-started/upgrading.md#cross-era-upgrades for the layout-specific migration path.")
	return nil
}
