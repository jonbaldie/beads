package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jonbaldie/beads/cmd/bd/doctor"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/spf13/cobra"
)

func runDoctor(cmd *cobra.Command, args []string) error {
	opts := doctorOptionsFromCommand(cmd)
	evt := metrics.NewCommandEvent("doctor")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	absPath, err := resolveDoctorPath(args)
	if err != nil {
		return err
	}
	if err := validateDoctorWorkspaceBackend(absPath); isLegacyUpgradeRefusal(err) {
		return printLegacyUpgradeDiagnostic(err, opts.mode.agent)
	} else if err != nil {
		return HandleError("%v", err)
	}
	if usesProxiedServer() {
		fmt.Fprintln(os.Stderr, "Note: 'bd doctor' is not yet supported in proxied-server mode.")
		return nil
	}
	if opts.fix.enabled && isOrchestratorRoot(absPath) {
		return HandleErrorWithHint(
			"refusing to run 'bd doctor --fix' at orchestrator workspace root",
			"Run the orchestrator's doctor command from workspace root, or run 'bd doctor --fix' inside a specific project clone",
		)
	}
	handled, err := runDoctorModeChecks(absPath, opts)
	if handled {
		return err
	}
	return runDoctorDiagnostics(absPath, opts)
}

func resolveDoctorPath(args []string) (string, error) {
	checkPath := "."
	if len(args) > 0 {
		checkPath = args[0]
	} else if beadsDir := os.Getenv("BEADS_DIR"); beadsDir != "" {
		checkPath = filepath.Dir(beadsDir)
	}
	absPath, err := filepath.Abs(checkPath)
	if err != nil {
		return "", HandleError("failed to resolve path: %v", err)
	}
	return absPath, nil
}

func runDoctorModeChecks(absPath string, opts doctorOptions) (bool, error) {
	// --check-health has a hook-only fallback for embedded mode
	// (doctor_health.go), so it runs before the embedded-mode gate below.
	if opts.mode.checkHealth {
		return true, runCheckHealth(absPath)
	}
	if opts.mode.perf {
		return true, runDoctorPerf(absPath, opts)
	}
	if opts.mode.check != "" {
		return true, runDoctorNamedCheck(absPath, opts)
	}
	// Bare `bd doctor` and the remaining mode-specific flags (--deep,
	// --server, --migration) aren't wired up for embedded mode yet.
	// Policy (GH#3794): embedded support is enabled one subcommand at a
	// time, each human-vetted — do not lift this gate wholesale. Checks
	// that reach into the database layer stay server-gated until the
	// storage driver interface covers them (AGENTS.md "Storage Boundary").
	if isEmbeddedMode() {
		printEmbeddedUnsupported("doctor", opts.mode.agent)
		return true, nil
	}
	if opts.mode.deep {
		return true, runDeepValidation(absPath)
	}
	if opts.mode.server {
		return true, runServerHealth(absPath)
	}
	if opts.mode.migration != "" {
		return true, runMigrationValidation(absPath, opts.mode.migration)
	}
	return false, nil
}

func runDoctorPerf(absPath string, opts doctorOptions) error {
	// --perf opens a live server-mode connection; route embedded users to
	// the structured stub instead of a hard connection error (GH#3597).
	if isEmbeddedMode() {
		printEmbeddedUnsupported("doctor --perf", opts.mode.agent)
		return nil
	}
	if err := doctor.RunPerformanceDiagnostics(absPath); err != nil {
		return HandleError("performance diagnostics: %v", err)
	}
	return nil
}

func runDoctorNamedCheck(absPath string, opts doctorOptions) error {
	// artifacts, conventions, and pollution work in embedded mode and run
	// unconditionally; validate still requires a server-mode connection
	// and stays gated (GH#3597).
	switch opts.mode.check {
	case "artifacts":
		return runArtifactsCheck(absPath, opts.mode.clean, opts.fix.yes)
	case "conventions":
		return runConventionsCheck(absPath)
	case "pollution":
		return runPollutionCheck(absPath, opts.mode.clean, opts.fix.yes)
	case "validate":
		if isEmbeddedMode() {
			printEmbeddedUnsupported("doctor --check=validate", opts.mode.agent)
			return nil
		}
		return runValidateCheckWithOptions(absPath, opts)
	default:
		return HandleErrorWithHint(fmt.Sprintf("unknown check %q", opts.mode.check), "Available checks: artifacts, conventions, pollution, validate")
	}
}

func runDoctorDiagnostics(absPath string, opts doctorOptions) error {
	result := runDiagnosticsWithOptions(absPath, opts)
	if opts.fix.dryRun {
		previewFixes(result)
	} else if opts.fix.enabled {
		applyFixesWithOptions(result, opts)
		fmt.Println("\nVerifying fixes...")
		result = runDiagnosticsWithOptions(absPath, opts)
	}
	return renderDoctorDiagnostics(absPath, opts, result)
}

func renderDoctorDiagnostics(absPath string, opts doctorOptions, result doctorResult) error {
	if opts.output != "" || isJSONOutput() {
		result.Timestamp = time.Now().UTC().Format(time.RFC3339)
		result.Platform = doctor.CollectPlatformInfo(absPath)
	}
	if opts.output != "" {
		if err := exportDiagnostics(result, opts.output); err != nil {
			return HandleError("failed to export diagnostics: %v", err)
		}
		fmt.Printf("✓ Diagnostics exported to %s\n", opts.output)
	}
	if err := printDoctorDiagnostics(opts, result); err != nil {
		return err
	}
	if !result.OverallOK {
		return SilentExit()
	}
	return nil
}

func printDoctorDiagnostics(opts doctorOptions, result doctorResult) error {
	if opts.mode.agent {
		agentResult := buildAgentResult(result)
		if isJSONOutput() {
			return outputJSON(agentResult)
		}
		printAgentDiagnostics(agentResult)
		return nil
	}
	if isJSONOutput() {
		return outputJSON(result)
	}
	if opts.output == "" {
		printDiagnosticsWithOptions(result, opts)
	}
	return nil
}

// printEmbeddedUnsupported reports that a doctor variant is not yet wired up
// for embedded mode. Emits a structured payload to stderr when --json or
// --agent is set so downstream tooling can detect the gap without parsing
// prose, and the existing prose stub otherwise (GH#3597).
//
// Follows the bd error-JSON contract (docs/JSON_SCHEMA.md): stderr, includes
// a `code` field, and is wrapped with schema_version. Exit code stays 0 - a
// benign refusal, not a failure.
func printEmbeddedUnsupported(commandLabel string, agent bool) {
	hints := []string{
		"Verify database exists:  ls -la .beads/embeddeddolt/",
		"Check bd version:        bd version",
		"Reinitialize if needed:  bd init --reinit-local",
		"Switch to server mode:   bd init --server",
	}
	supported := []string{"artifacts", "conventions", "pollution"}
	unsupported := []string{"validate"}

	if isJSONOutput() || agent {
		payload := map[string]interface{}{
			"error":                               fmt.Sprintf("'bd %s' is not yet supported in embedded mode", commandLabel),
			"code":                                "embedded_unsupported",
			"unsupported":                         true,
			"mode":                                "embedded",
			"command":                             commandLabel,
			"checks_supported_in_embedded_mode":   supported,
			"checks_unsupported_in_embedded_mode": unsupported,
			"hints":                               hints,
		}
		encoder := json.NewEncoder(os.Stderr)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(wrapWithSchemaVersion(payload))
		return
	}

	fmt.Fprintf(os.Stderr, "Note: 'bd %s' is not yet supported in embedded mode.\n\n", commandLabel)
	fmt.Fprintln(os.Stderr, "For embedded mode troubleshooting:")
	for _, h := range hints {
		fmt.Fprintf(os.Stderr, "  • %s\n", h)
	}
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Checks available in embedded mode:")
	fmt.Fprintln(os.Stderr, "  • bd doctor --check=artifacts")
	fmt.Fprintln(os.Stderr, "  • bd doctor --check=conventions")
	fmt.Fprintln(os.Stderr, "  • bd doctor --check=pollution")
}
