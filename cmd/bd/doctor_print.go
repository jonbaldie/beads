package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jonbaldie/beads/cmd/bd/doctor"
	"github.com/jonbaldie/beads/internal/ui"
)

// convertDoctorCheck converts doctor package check to main package check
func convertDoctorCheck(dc doctor.DoctorCheck) doctorCheck {
	return doctorCheck{
		Name:     dc.Name,
		Status:   dc.Status,
		Message:  dc.Message,
		Detail:   dc.Detail,
		Fix:      dc.Fix,
		Category: dc.Category,
	}
}

// convertWithCategory converts a doctor check and sets its category
func convertWithCategory(dc doctor.DoctorCheck, category string) doctorCheck {
	check := convertDoctorCheck(dc)
	check.Category = category
	return check
}

// exportDiagnostics writes the doctor result to a JSON file
func exportDiagnostics(result doctorResult, outputPath string) error {
	// #nosec G304 - outputPath is a user-provided flag value for file generation
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer f.Close()

	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		return fmt.Errorf("failed to write JSON: %w", err)
	}

	return nil
}

type doctorCheckSummary struct {
	checksByCategory map[string][]doctorCheck
	issuesByCategory map[string][]doctorCheck
	passCount        int
	warnCount        int
	failCount        int
	hasIssues        bool
}

func summarizeDoctorChecks(checks []doctorCheck) doctorCheckSummary {
	summary := doctorCheckSummary{
		checksByCategory: make(map[string][]doctorCheck),
		issuesByCategory: make(map[string][]doctorCheck),
	}
	for _, check := range checks {
		category := check.Category
		if category == "" {
			category = "Other"
		}
		summary.checksByCategory[category] = append(summary.checksByCategory[category], check)
		switch check.Status {
		case statusOK:
			summary.passCount++
		case statusWarning:
			summary.warnCount++
			summary.issuesByCategory[category] = append(summary.issuesByCategory[category], check)
			summary.hasIssues = true
		case statusError:
			summary.failCount++
			summary.issuesByCategory[category] = append(summary.issuesByCategory[category], check)
			summary.hasIssues = true
		}
	}
	return summary
}

func printDiagnostics(result doctorResult) {
	printDiagnosticsWithOptions(result, defaultDoctorOptions())
}

func printDiagnosticsWithOptions(result doctorResult, opts doctorOptions) {
	summary := summarizeDoctorChecks(result.Checks)
	fmt.Printf("\nbd doctor v%s", result.CLIVersion)
	fmt.Printf("  %s  %s %d passed  %s %d warnings  %s %d errors\n",
		ui.RenderSeparator(),
		ui.RenderPassIcon(), summary.passCount,
		ui.RenderWarnIcon(), summary.warnCount,
		ui.RenderFailIcon(), summary.failCount,
	)
	if opts.fix.verbose {
		fmt.Println()
		printAllChecks(summary.checksByCategory)
	}
	if summary.hasIssues {
		fmt.Println()
		printDoctorIssues(summary)
		if !opts.fix.verbose {
			fmt.Printf("%s\n", ui.RenderMuted("Run with --verbose to see all checks"))
		}
	} else {
		fmt.Println()
		fmt.Printf("%s\n", ui.RenderPass("✓ All checks passed"))
		if !opts.fix.verbose {
			fmt.Printf("%s\n", ui.RenderMuted("Run with --verbose to see all checks"))
		}
	}
	printSuppressedDoctorNotice(result.SuppressedCount)
}

func printDoctorIssues(summary doctorCheckSummary) {
	for _, category := range doctor.CategoryOrder {
		issues, exists := summary.issuesByCategory[category]
		if !exists || len(issues) == 0 {
			continue
		}
		printDoctorCategoryIssues(category, summary.checksByCategory[category], issues)
	}
	printDoctorOtherIssues(summary.issuesByCategory["Other"])
}

func printDoctorCategoryIssues(category string, checks, issues []doctorCheck) {
	slices.SortStableFunc(issues, func(a, b doctorCheck) int {
		if a.Status == statusError && b.Status != statusError {
			return -1
		}
		if a.Status != statusError && b.Status == statusError {
			return 1
		}
		return 0
	})
	passCount := 0
	for _, check := range checks {
		if check.Status == statusOK {
			passCount++
		}
	}
	fmt.Printf("%s %s\n", ui.RenderCategory(category),
		ui.RenderMuted(fmt.Sprintf("(%d/%d passed)", passCount, len(checks))))
	for _, check := range issues {
		printDoctorIssue(check)
	}
	fmt.Println()
}

func printDoctorOtherIssues(issues []doctorCheck) {
	if len(issues) == 0 {
		return
	}
	fmt.Printf("%s\n", ui.RenderCategory("Other"))
	for _, check := range issues {
		printDoctorIssue(check)
	}
	fmt.Println()
}

func printDoctorIssue(check doctorCheck) {
	line := fmt.Sprintf("%s: %s", check.Name, check.Message)
	if check.Status == statusError {
		fmt.Printf("  %s  %s\n", ui.RenderFailIcon(), ui.RenderFail(line))
	} else {
		fmt.Printf("  %s  %s\n", ui.RenderWarnIcon(), line)
	}
	if check.Detail != "" {
		fmt.Printf("      %s\n", ui.RenderMuted(check.Detail))
	}
	if check.Fix == "" {
		return
	}
	for index, fixLine := range strings.Split(check.Fix, "\n") {
		if index == 0 {
			fmt.Printf("      %s%s\n", ui.MutedStyle().Render(ui.TreeLast), fixLine)
			continue
		}
		fmt.Printf("        %s\n", fixLine)
	}
}

func printSuppressedDoctorNotice(suppressedCount int) {
	if suppressedCount == 0 {
		return
	}
	noun := "warning"
	if suppressedCount > 1 {
		noun = "warnings"
	}
	fmt.Printf("%s\n", ui.RenderMuted(fmt.Sprintf("(%d %s suppressed via doctor.suppress config)", suppressedCount, noun)))
}

// printAllChecks prints all checks grouped by category with section headers.
func printAllChecks(checksByCategory map[string][]doctorCheck) {
	for _, category := range doctor.CategoryOrder {
		checks, exists := checksByCategory[category]
		if !exists || len(checks) == 0 {
			continue
		}
		printDoctorCategoryChecks(category, checks)
	}
	if otherChecks := checksByCategory["Other"]; len(otherChecks) > 0 {
		printDoctorCategoryChecks("Other", otherChecks)
	}
}

func printDoctorCategoryChecks(category string, checks []doctorCheck) {
	fmt.Println(ui.RenderCategory(category))
	for _, check := range checks {
		printDoctorCheck(check)
	}
	fmt.Println()
}

func printDoctorCheck(check doctorCheck) {
	statusIcon := doctorStatusIcon(check.Status)
	fmt.Printf("  %s  %s", statusIcon, check.Name)
	if check.Message != "" {
		fmt.Printf("%s", ui.RenderMuted(" "+check.Message))
	}
	fmt.Println()
	if check.Detail != "" {
		fmt.Printf("     %s%s\n", ui.MutedStyle().Render(ui.TreeLast), ui.RenderMuted(check.Detail))
	}
}

func doctorStatusIcon(status string) string {
	switch status {
	case statusOK:
		return ui.RenderPassIcon()
	case statusWarning:
		return ui.RenderWarnIcon()
	case statusError:
		return ui.RenderFailIcon()
	default:
		return ""
	}
}

// runMigrationValidation runs Dolt migration validation checks.
// Phase can be "pre" (before migration) or "post" (after migration).
// Outputs machine-parseable JSON when --json flag is set.
func runMigrationValidation(path string, phase string) error {
	check, result, err := migrationValidationForPhase(path, phase)
	if err != nil {
		return err
	}
	if isJSONOutput() {
		return outputMigrationValidation(check, result)
	}
	printMigrationValidation(phase, check, result)
	return migrationValidationExit(result.Ready)
}

func migrationValidationForPhase(
	path string,
	phase string,
) (doctorCheck, doctor.MigrationValidationResult, error) {
	switch phase {
	case "pre":
		dc, result := doctor.CheckMigrationReadiness(path)
		return convertDoctorCheck(dc), result, nil
	case "post":
		dc, result := doctor.CheckMigrationCompletion(path)
		return convertDoctorCheck(dc), result, nil
	default:
		return doctorCheck{}, doctor.MigrationValidationResult{}, HandleError(
			"invalid migration phase %q (use 'pre' or 'post')",
			phase,
		)
	}
}

func outputMigrationValidation(
	check doctorCheck,
	result doctor.MigrationValidationResult,
) error {
	output := struct {
		Check      doctorCheck                      `json:"check"`
		Validation doctor.MigrationValidationResult `json:"validation"`
		CLIVersion string                           `json:"cli_version"`
		Timestamp  string                           `json:"timestamp"`
	}{
		Check:      check,
		Validation: result,
		CLIVersion: Version,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}
	if err := outputJSON(output); err != nil {
		return err
	}
	return migrationValidationExit(result.Ready)
}

func migrationValidationExit(ready bool) error {
	if ready {
		return nil
	}
	return SilentExit()
}

func printMigrationValidation(
	phase string,
	check doctorCheck,
	result doctor.MigrationValidationResult,
) {
	fmt.Printf("\nbd doctor --migration=%s v%s\n\n", phase, Version)
	printMigrationStatus(check)
	printMigrationDetails(result)
	printMigrationMessages(result)
	printMigrationFix(check.Fix)
	printMigrationResult(result.Ready)
}

func printMigrationStatus(check doctorCheck) {
	fmt.Printf("%s  %s: %s\n", doctorStatusIcon(check.Status), check.Name, check.Message)
	if check.Detail == "" {
		return
	}
	for _, line := range strings.Split(check.Detail, "\n") {
		fmt.Printf("     %s\n", ui.RenderMuted(line))
	}
}

func printMigrationDetails(result doctor.MigrationValidationResult) {
	fmt.Println()
	fmt.Println(ui.RenderCategory("Validation Details"))
	fmt.Printf("  Backend:     %s\n", result.Backend)
	fmt.Printf("  JSONL Count: %d\n", result.JSONLCount)
	if result.SQLiteCount > 0 {
		fmt.Printf("  SQLite Count: %d\n", result.SQLiteCount)
	}
	if result.DoltCount > 0 {
		fmt.Printf("  Dolt Count:  %d\n", result.DoltCount)
	}
	fmt.Printf("  JSONL Valid: %v\n", result.JSONLValid)
	if result.JSONLMalformed > 0 {
		fmt.Printf("  Malformed Lines: %d\n", result.JSONLMalformed)
	}
}

func printMigrationMessages(result doctor.MigrationValidationResult) {
	if len(result.Warnings) > 0 {
		fmt.Println()
		fmt.Println(ui.RenderCategory("Warnings"))
		for _, warning := range result.Warnings {
			fmt.Printf("  %s  %s\n", ui.RenderWarnIcon(), warning)
		}
	}
	if len(result.Errors) > 0 {
		fmt.Println()
		fmt.Println(ui.RenderCategory("Errors"))
		for _, validationErr := range result.Errors {
			fmt.Printf("  %s  %s\n", ui.RenderFailIcon(), validationErr)
		}
	}
}

func printMigrationFix(fix string) {
	if fix == "" {
		return
	}
	fmt.Println()
	fmt.Printf("%s  %s\n", ui.RenderMuted("Fix:"), fix)
}

func printMigrationResult(ready bool) {
	fmt.Println()
	if ready {
		fmt.Printf("%s\n", ui.RenderPass("✓ Migration validation passed"))
		return
	}
	fmt.Printf("%s\n", ui.RenderFail("✗ Migration validation failed"))
}
