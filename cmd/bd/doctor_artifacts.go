package main

import (
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/cmd/bd/doctor"
	"github.com/jonbaldie/beads/cmd/bd/doctor/fix"
	"github.com/jonbaldie/beads/internal/ui"
)

// runArtifactsCheck runs detailed classic artifact detection.
// With --clean, removes safe-to-delete artifacts after confirmation.
func runArtifactsCheck(path string, clean bool, yes bool) error {
	report, err := doctor.ScanForArtifacts(path)
	if err != nil {
		return HandleError("scanning artifacts: %v", err)
	}
	if report.TotalCount == 0 {
		return reportNoArtifacts()
	}
	if isJSONOutput() {
		return reportArtifactsJSON(path, report, clean)
	}
	printArtifactsHuman(report)
	if !clean {
		fmt.Println("Run 'bd doctor --check=artifacts --clean' to remove safe-to-delete artifacts.")
		return nil
	}
	return cleanArtifactsIfConfirmed(path, report, yes)
}

func reportNoArtifacts() error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{"total_count": 0})
	}
	fmt.Println("No classic artifacts detected.")
	return nil
}

func reportArtifactsJSON(path string, report doctor.ArtifactReport, clean bool) error {
	// GH#2438: When --clean is also set, perform cleanup before outputting JSON.
	if clean && report.SafeDeleteCount > 0 {
		var err error
		report, err = cleanAndRescanArtifacts(path)
		if err != nil {
			return err
		}
	}
	return outputJSON(artifactsJSONResult(report))
}

func cleanAndRescanArtifacts(path string) (doctor.ArtifactReport, error) {
	if err := fix.ClassicArtifacts(path); err != nil {
		return doctor.ArtifactReport{}, HandleError("during cleanup: %v", err)
	}
	report, err := doctor.ScanForArtifacts(path)
	if err != nil {
		return doctor.ArtifactReport{}, HandleError("re-scanning artifacts: %v", err)
	}
	return report, nil
}

func artifactsJSONResult(report doctor.ArtifactReport) map[string]interface{} {
	result := map[string]interface{}{
		"total_count":       report.TotalCount,
		"safe_delete_count": report.SafeDeleteCount,
		"sqlite_artifacts":  len(report.SQLiteArtifacts),
		"cruft_beads_dirs":  len(report.CruftBeadsDirs),
		"redirect_issues":   len(report.RedirectIssues),
	}
	var findings []map[string]interface{}
	for _, lists := range [][]doctor.ArtifactFinding{
		report.SQLiteArtifacts,
		report.CruftBeadsDirs, report.RedirectIssues,
	} {
		for _, f := range lists {
			findings = append(findings, map[string]interface{}{
				"path":        f.Path,
				"type":        f.Type,
				"description": f.Description,
				"safe_delete": f.SafeDelete,
			})
		}
	}
	result["findings"] = findings
	return result
}

func printArtifactsHuman(report doctor.ArtifactReport) {
	fmt.Printf("Found %d classic artifact(s) (%d safe to delete):\n\n", report.TotalCount, report.SafeDeleteCount)
	printArtifactFindings("SQLite Artifacts", report.SQLiteArtifacts, true)
	printArtifactFindings("Cruft .beads Directories", report.CruftBeadsDirs, true)
	printArtifactFindings("Redirect Issues", report.RedirectIssues, false)
}

func printArtifactFindings(title string, findings []doctor.ArtifactFinding, showSafe bool) {
	if len(findings) == 0 {
		return
	}
	fmt.Printf("%s (%d):\n", title, len(findings))
	for _, f := range findings {
		safeTag := ""
		if showSafe && f.SafeDelete {
			safeTag = " [safe]"
		}
		fmt.Printf("  %s%s\n", f.Path, safeTag)
		fmt.Printf("    %s\n", ui.RenderMuted(f.Description))
	}
	fmt.Println()
}

func cleanArtifactsIfConfirmed(path string, report doctor.ArtifactReport, yes bool) error {
	if report.SafeDeleteCount == 0 {
		fmt.Println("No artifacts are safe to auto-delete. Manual review required.")
		return nil
	}
	if !yes {
		fmt.Printf("Delete %d safe-to-delete artifact(s)? [y/N] ", report.SafeDeleteCount)
		var response string
		_, _ = fmt.Scanln(&response)
		if strings.ToLower(response) != "y" {
			fmt.Println("Canceled.")
			return nil
		}
	}
	if err := fix.ClassicArtifacts(path); err != nil {
		return HandleError("during cleanup: %v", err)
	}
	return nil
}
