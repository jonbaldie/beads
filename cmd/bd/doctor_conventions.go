package main

import (
	"fmt"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/validation"
)

// runConventionsCheck runs a composite conventions check: lint, stale, and orphans.
// All findings are advisory (warning, never error) - conventions are a choice.
func runConventionsCheck(path string) error {
	checks, err := loadConventionsChecks(path)
	if err != nil {
		return err
	}
	if isJSONOutput() {
		return outputConventionsJSON(path, checks)
	}
	renderConventionsText(checks)
	return nil
}

func loadConventionsChecks(path string) ([]doctorCheck, error) {
	// doctor opts out of PersistentPreRun DB init via skipStoreAnnotation, so
	// PersistentPreRun doesn't open the store.
	// The lint/stale/orphans primitives all require the global store, so
	// initialize it lazily here; ensureDirectMode routes to embedded or
	// server based on metadata.json (GH#3597).
	if err := ensureDirectMode("conventions check requires direct mode"); err != nil {
		return nil, HandleError("%v", err)
	}

	var checks []doctorCheck
	lintChecks, err := runConventionsLint()
	if err != nil {
		return nil, err
	}
	checks = append(checks, lintChecks...)
	checks = append(checks, runConventionsStale()...)
	checks = append(checks, runConventionsOrphans(path)...)
	return checks, nil
}

func outputConventionsJSON(path string, checks []doctorCheck) error {
	overallOK := true
	for _, c := range checks {
		if c.Status != statusOK {
			overallOK = false
			break
		}
	}
	return outputJSON(struct {
		Path      string        `json:"path"`
		Checks    []doctorCheck `json:"checks"`
		OverallOK bool          `json:"overall_ok"`
	}{
		Path:      path,
		Checks:    checks,
		OverallOK: overallOK,
	})
}

func renderConventionsText(checks []doctorCheck) {
	fmt.Println()
	fmt.Println(ui.RenderCategory("Conventions"))

	var passCount, warnCount int
	for _, c := range checks {
		pass, warning := renderConventionsCheck(c)
		passCount += pass
		warnCount += warning
	}

	fmt.Println()
	fmt.Println(ui.RenderSeparator())
	fmt.Printf("%s %d passed  %s %d warnings\n",
		ui.RenderPassIcon(), passCount,
		ui.RenderWarnIcon(), warnCount,
	)

	if warnCount == 0 {
		fmt.Println()
		fmt.Printf("%s\n", ui.RenderPass("✓ All convention checks passed"))
	}
}

func renderConventionsCheck(c doctorCheck) (pass, warning int) {
	statusIcon := ""
	switch c.Status {
	case statusOK:
		statusIcon = ui.RenderPassIcon()
		pass = 1
	case statusWarning:
		statusIcon = ui.RenderWarnIcon()
		warning = 1
	}
	fmt.Printf("  %s  %s", statusIcon, c.Name)
	if c.Message != "" {
		fmt.Printf("%s", ui.RenderMuted(" "+c.Message))
	}
	fmt.Println()
	if c.Detail != "" {
		fmt.Printf("     %s%s\n", ui.MutedStyle().Render(ui.TreeLast), ui.RenderMuted(c.Detail))
	}
	if c.Fix != "" {
		fmt.Printf("     %s\n", ui.RenderMuted("Fix: "+c.Fix))
	}
	return pass, warning
}

// runConventionsLint checks open issues for missing template sections.
// runConventionsLint's doctorCheck results are advisory (per the composite
// comment above), but a MaxRows cap violation is a different kind of
// failure — an infrastructure circuit breaker, not a data-quality finding —
// so it returns a non-nil error instead of a doctorCheck in that one case.
// The caller must propagate it rather than folding it into the checks list.
func runConventionsLint() ([]doctorCheck, error) {
	if getStore() == nil {
		return []doctorCheck{{
			Name:     "conventions.lint",
			Status:   statusWarning,
			Message:  "database not available",
			Category: "Conventions",
		}}, nil
	}

	ctx := getRootContext()
	openStatus := types.StatusOpen
	// Env-only cap (designer §4): operator opt-in via BEADS_MAX_ROWS.
	maxRows, maxRowsSource := resolveMaxRowsEnvOnly()
	issues, err := getStore().SearchIssues(ctx, "", types.IssueFilter{
		IssueFilterCore: types.IssueFilterCore{
			Status: &openStatus,
		},
		IssueFilterPage: types.IssueFilterPage{
			MaxRows:       maxRows,
			MaxRowsSource: maxRowsSource,
		},
	})
	if err != nil {
		if capErr := handleMaxRowsError(err); capErr != nil {
			return nil, capErr
		}
		return []doctorCheck{{
			Name:     "conventions.lint",
			Status:   statusWarning,
			Message:  fmt.Sprintf("error reading issues: %v", err),
			Category: "Conventions",
		}}, nil
	}

	warningCount := 0
	for _, issue := range issues {
		if err := validation.LintIssue(issue); err != nil {
			warningCount++
		}
	}

	if warningCount == 0 {
		return []doctorCheck{{
			Name:     "conventions.lint",
			Status:   statusOK,
			Message:  fmt.Sprintf("all %d open issues pass template checks", len(issues)),
			Category: "Conventions",
		}}, nil
	}

	return []doctorCheck{{
		Name:     "conventions.lint",
		Status:   statusWarning,
		Message:  fmt.Sprintf("%d of %d open issues missing recommended sections", warningCount, len(issues)),
		Fix:      "bd lint",
		Category: "Conventions",
	}}, nil
}

// runConventionsStale checks for issues with no recent activity.
func runConventionsStale() []doctorCheck {
	if getStore() == nil {
		return []doctorCheck{{
			Name:     "conventions.stale",
			Status:   statusWarning,
			Message:  "database not available",
			Category: "Conventions",
		}}
	}

	ctx := getRootContext()
	filter := types.StaleFilter{Days: 14, Limit: 100}
	staleIssues, err := getStore().GetStaleIssues(ctx, filter)
	if err != nil {
		return []doctorCheck{{
			Name:     "conventions.stale",
			Status:   statusWarning,
			Message:  fmt.Sprintf("error checking stale issues: %v", err),
			Category: "Conventions",
		}}
	}

	if len(staleIssues) == 0 {
		return []doctorCheck{{
			Name:     "conventions.stale",
			Status:   statusOK,
			Message:  "no issues inactive for 14+ days",
			Category: "Conventions",
		}}
	}

	return []doctorCheck{{
		Name:     "conventions.stale",
		Status:   statusWarning,
		Message:  fmt.Sprintf("%d issues inactive for 14+ days", len(staleIssues)),
		Fix:      "bd stale",
		Category: "Conventions",
	}}
}

// runConventionsOrphans checks for issues referenced in commits but still open.
func runConventionsOrphans(path string) []doctorCheck {
	orphans, err := findOrphanedIssues(path, nil, nil)
	if err != nil {
		// Not an error - orphan detection may fail in non-git repos
		return []doctorCheck{{
			Name:     "conventions.orphans",
			Status:   statusOK,
			Message:  "orphan check skipped (no git history)",
			Category: "Conventions",
		}}
	}

	if len(orphans) == 0 {
		return []doctorCheck{{
			Name:     "conventions.orphans",
			Status:   statusOK,
			Message:  "no orphaned issues found",
			Category: "Conventions",
		}}
	}

	return []doctorCheck{{
		Name:     "conventions.orphans",
		Status:   statusWarning,
		Message:  fmt.Sprintf("%d issues referenced in commits but still open", len(orphans)),
		Fix:      "bd orphans",
		Category: "Conventions",
	}}
}
