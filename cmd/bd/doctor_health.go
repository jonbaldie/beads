package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/beads/cmd/bd/doctor"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage/doltutil"
	"github.com/jonbaldie/beads/internal/ui"
)

// runCheckHealth runs lightweight health checks for git hooks.
// Silent on success, prints a hint if issues detected.
// Respects hints.doctor config setting.
func runCheckHealth(path string) error {
	beadsDir := doctor.ResolveBeadsDirForRepo(path)
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		return nil
	}
	cfg, err := configfile.Load(beadsDir)
	if err != nil || cfg == nil {
		return checkHooksHealthHint()
	}
	port := doltserver.DefaultConfig(beadsDir).Port
	if port == 0 {
		return checkHooksHealthHint()
	}
	issues, disabled := checkDatabaseHealth(cfg, port)
	if disabled {
		return nil
	}
	if issue := doctor.CheckHooksQuick(Version); issue != "" {
		issues = append(issues, issue)
	}
	if len(issues) > 0 {
		return printCheckHealthHint(issues)
	}
	return nil
}

func checkHooksHealthHint() error {
	if issue := doctor.CheckHooksQuick(Version); issue != "" {
		return printCheckHealthHint([]string{issue})
	}
	return nil
}

func checkDatabaseHealth(cfg *configfile.Config, port int) ([]string, bool) {
	dsn := doltutil.ServerDSN{
		Host:     cfg.GetDoltServerHost(),
		Port:     port,
		User:     cfg.GetDoltServerUser(),
		Password: cfg.GetDoltServerPasswordForPort(port),
		Database: cfg.GetDoltDatabase(),
		Timeout:  2 * time.Second,
		TLS:      cfg.GetDoltServerTLS(),
	}.String()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, false
	}
	defer db.Close()
	if db.Ping() != nil {
		return nil, false
	}
	if hintsDisabledDB(db) {
		return nil, true
	}
	if issue := checkVersionMismatchDB(db); issue != "" {
		return []string{issue}, false
	}
	return nil, false
}

// runDeepValidation runs full graph integrity validation
func runDeepValidation(path string) error {
	fmt.Println("Running deep validation (may be slow on large databases)...")
	fmt.Println()

	result := doctor.RunDeepValidation(path)

	if isJSONOutput() {
		jsonBytes, err := doctor.DeepValidationResultJSON(result)
		if err != nil {
			return HandleError("%v", err)
		}
		fmt.Println(string(jsonBytes))
	} else {
		doctor.PrintDeepValidationResult(result)
	}

	if !result.OverallOK {
		return SilentExit()
	}
	return nil
}

// runServerHealth runs Dolt server mode health checks
func runServerHealth(path string) error {
	result := doctor.RunServerHealthChecks(path)

	if isJSONOutput() {
		jsonBytes, err := json.Marshal(result)
		if err != nil {
			return HandleError("failed to marshal health check result: %v", err)
		}
		fmt.Println(string(jsonBytes))
	} else {
		fmt.Println("Dolt Server Mode Health Check")
		fmt.Println()
		printServerHealthResult(result)
	}

	if !result.OverallOK {
		return SilentExit()
	}
	return nil
}

// printServerHealthResult prints the server health check results
func printServerHealthResult(result doctor.ServerHealthResult) {
	counts := serverHealthCounts{}
	for _, check := range result.Checks {
		printServerHealthCheck(check, &counts)
	}
	fmt.Println()
	fmt.Println(ui.RenderSeparator())
	summary := fmt.Sprintf("%s %d passed  %s %d warnings  %s %d failed",
		ui.RenderPassIcon(), counts.passed,
		ui.RenderWarnIcon(), counts.warned,
		ui.RenderFailIcon(), counts.failed,
	)
	fmt.Println(summary)
	printServerHealthFixes(counts.warnings, result.OverallOK)
}

type serverHealthCounts struct {
	passed, warned, failed int
	warnings               []doctor.DoctorCheck
}

func printServerHealthCheck(check doctor.DoctorCheck, counts *serverHealthCounts) {
	statusIcon := ui.RenderPassIcon()
	switch check.Status {
	case statusOK:
		counts.passed++
	case statusWarning:
		statusIcon = ui.RenderWarnIcon()
		counts.warned++
		counts.warnings = append(counts.warnings, check)
	case statusError:
		statusIcon = ui.RenderFailIcon()
		counts.failed++
		counts.warnings = append(counts.warnings, check)
	}
	fmt.Printf("  %s  %s", statusIcon, check.Name)
	if check.Message != "" {
		fmt.Printf("%s", ui.RenderMuted(" "+check.Message))
	}
	fmt.Println()
	if check.Detail != "" {
		for _, line := range strings.Split(check.Detail, "\n") {
			fmt.Printf("     %s%s\n", ui.MutedStyle().Render(ui.TreeLast), ui.RenderMuted(line))
		}
	}
}

func printServerHealthFixes(warnings []doctor.DoctorCheck, overallOK bool) {
	if len(warnings) > 0 {
		fmt.Println()
		fmt.Println(ui.RenderWarn(ui.IconWarn + "  FIXES NEEDED"))
		for i, check := range warnings {
			if check.Fix == "" {
				continue
			}
			line := fmt.Sprintf("%s: %s", check.Name, check.Message)
			if check.Status == statusError {
				fmt.Printf("  %s  %s %s\n", ui.RenderFailIcon(), ui.RenderFail(fmt.Sprintf("%d.", i+1)), ui.RenderFail(line))
			} else {
				fmt.Printf("  %s  %s %s\n", ui.RenderWarnIcon(), ui.RenderWarn(fmt.Sprintf("%d.", i+1)), line)
			}
			fmt.Printf("        %s%s\n", ui.MutedStyle().Render(ui.TreeLast), check.Fix)
		}
	} else if overallOK {
		fmt.Println()
		fmt.Printf("%s\n", ui.RenderPass("✓ All server health checks passed"))
	}
}

func printCheckHealthHint(issues []string) error {
	fmt.Fprintf(os.Stderr, "💡 bd doctor recommends a health check:\n")
	for _, issue := range issues {
		fmt.Fprintf(os.Stderr, "   • %s\n", issue)
	}
	fmt.Fprintf(os.Stderr, "   Run 'bd doctor' for details, or 'bd doctor --fix' to auto-repair\n")
	fmt.Fprintf(os.Stderr, "   (Suppress with: bd config set %s false)\n", ConfigKeyHintsDoctor)
	return SilentExit()
}

// hintsDisabledDB checks if hints.doctor is set to "false" using an existing DB connection.
// Used by runCheckHealth to avoid multiple DB opens.
func hintsDisabledDB(db *sql.DB) bool {
	var value string
	err := db.QueryRow("SELECT value FROM config WHERE `key` = ?", ConfigKeyHintsDoctor).Scan(&value)
	if err != nil {
		return false // Key not set, assume hints enabled
	}
	return strings.ToLower(value) == "false"
}

// checkVersionMismatchDB checks if CLI version differs from database bd_version.
// Uses an existing DB connection.
func checkVersionMismatchDB(db *sql.DB) string {
	var dbVersion string
	err := db.QueryRow("SELECT value FROM local_metadata WHERE `key` = 'bd_version'").Scan(&dbVersion)
	if err != nil {
		return "" // Can't read version, skip
	}

	if dbVersion != "" && dbVersion != Version {
		return fmt.Sprintf("Version mismatch (CLI: %s, database: %s)", Version, dbVersion)
	}

	return ""
}
