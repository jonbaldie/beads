package main

import (
	"bufio"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/jonbaldie/beads/cmd/bd/doctor"
	"github.com/jonbaldie/beads/cmd/bd/doctor/fix"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/ui"
	"golang.org/x/term"
)

// previewFixes shows what would be fixed without applying changes
func previewFixes(result doctorResult) {
	// Collect all fixable issues
	var fixableIssues []doctorCheck
	for _, check := range result.Checks {
		if (check.Status == statusWarning || check.Status == statusError) && check.Fix != "" {
			fixableIssues = append(fixableIssues, check)
		}
	}

	if len(fixableIssues) == 0 {
		fmt.Println("\n✓ No fixable issues found (dry-run)")
		return
	}

	fmt.Println("\n[DRY-RUN] The following issues would be fixed with --fix:")
	fmt.Println()

	for i, issue := range fixableIssues {
		// Show the issue details
		fmt.Printf("  %d. %s\n", i+1, issue.Name)
		if issue.Status == statusError {
			fmt.Printf("     Status: %s\n", ui.RenderFail("ERROR"))
		} else {
			fmt.Printf("     Status: %s\n", ui.RenderWarn("WARNING"))
		}
		fmt.Printf("     Issue:  %s\n", issue.Message)
		if issue.Detail != "" {
			fmt.Printf("     Detail: %s\n", issue.Detail)
		}
		fmt.Printf("     Fix:    %s\n", issue.Fix)
		fmt.Println()
	}

	fmt.Printf("[DRY-RUN] Would attempt to fix %d issue(s)\n", len(fixableIssues))
	fmt.Println("Run 'bd doctor --fix' to apply these fixes")
}

func applyFixes(result doctorResult) {
	applyFixesWithOptions(result, defaultDoctorOptions())
}

func applyFixesWithOptions(result doctorResult, opts doctorOptions) {
	fixableIssues := collectDoctorFixableIssues(result.Checks)

	if len(fixableIssues) == 0 {
		fmt.Println("\nNo fixable issues found.")
		return
	}

	printDoctorFixList(fixableIssues)

	// Interactive mode - confirm each fix individually
	if opts.fix.interactive {
		applyFixesInteractive(result.Path, fixableIssues, opts)
		return
	}

	// Ask for confirmation (skip if --yes flag is set or stdin is non-interactive)
	if !opts.fix.yes && !confirmDoctorFixes(len(fixableIssues)) {
		return
	}

	// Apply fixes
	fmt.Println("\nApplying fixes...")
	applyFixListWithOptions(result.Path, fixableIssues, opts)
}

func collectDoctorFixableIssues(checks []doctorCheck) []doctorCheck {
	var fixable []doctorCheck
	for _, check := range checks {
		if (check.Status == statusWarning || check.Status == statusError) && check.Fix != "" {
			fixable = append(fixable, check)
		}
	}
	return fixable
}

func printDoctorFixList(issues []doctorCheck) {
	fmt.Println("\nFixable issues:")
	for i, issue := range issues {
		fmt.Printf("  %d. %s: %s\n", i+1, issue.Name, issue.Message)
	}
}

func confirmDoctorFixes(count int) bool {
	// Detect non-interactive stdin (e.g., piped input in CI/automation).
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintf(os.Stderr, "\n%s Running in non-interactive mode\n", ui.RenderWarn("⚠"))
		fmt.Fprintf(os.Stderr, "  To auto-fix issues without prompting, use: %s\n\n", ui.RenderAccent("bd doctor --fix --yes"))
		return false
	}

	fmt.Printf("\nThis will attempt to fix %d issue(s). Continue? (Y/n): ", count)
	reader := bufio.NewReader(os.Stdin)
	response, err := reader.ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading input: %v\n", err)
		return false
	}

	response = strings.TrimSpace(strings.ToLower(response))
	if response != "" && response != "y" && response != "yes" {
		fmt.Println("Fix canceled.")
		return false
	}
	return true
}

// applyFixesInteractive prompts for each fix individually
func applyFixesInteractive(path string, issues []doctorCheck, opts doctorOptions) {
	// Detect non-interactive stdin before attempting to prompt
	isInteractive := term.IsTerminal(int(os.Stdin.Fd()))
	if !isInteractive {
		fmt.Fprintf(os.Stderr, "\n%s Interactive mode requires a terminal\n", ui.RenderWarn("⚠"))
		fmt.Fprintf(os.Stderr, "  Use 'bd doctor --fix --yes' for non-interactive mode\n\n")
		return
	}

	reader := bufio.NewReader(os.Stdin)
	applyAll := false
	var approvedFixes []doctorCheck

	fmt.Println("\nReview each fix:")
	fmt.Println("  [y]es - apply this fix")
	fmt.Println("  [n]o  - skip this fix")
	fmt.Println("  [a]ll - apply all remaining fixes")
	fmt.Println("  [q]uit - stop without applying more fixes")
	fmt.Println()

	for i, issue := range issues {
		printDoctorFixIssue(i, len(issues), issue)

		// Check if we should apply all remaining
		if applyAll {
			fmt.Println("  → Auto-approved (apply all)")
			approvedFixes = append(approvedFixes, issue)
			continue
		}

		decision, err := promptDoctorFix(reader)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading input: %v\n", err)
			if len(approvedFixes) > 0 {
				fmt.Printf("\nApplying %d previously approved fix(es) before exit...\n", len(approvedFixes))
				applyFixListWithOptions(path, approvedFixes, opts)
			}
			return
		}

		var quit bool
		approvedFixes, applyAll, quit = recordDoctorFixDecision(decision, issue, approvedFixes, applyAll)
		if quit {
			fmt.Println("  → Quit")
			if len(approvedFixes) > 0 {
				fmt.Printf("\nApplying %d approved fix(es)...\n", len(approvedFixes))
				applyFixListWithOptions(path, approvedFixes, opts)
			} else {
				fmt.Println("\nNo fixes applied.")
			}
			return
		}
		fmt.Println()
	}

	// Apply all approved fixes
	if len(approvedFixes) > 0 {
		fmt.Printf("\nApplying %d approved fix(es)...\n", len(approvedFixes))
		applyFixListWithOptions(path, approvedFixes, opts)
	} else {
		fmt.Println("\nNo fixes approved.")
	}
}

func printDoctorFixIssue(index, total int, issue doctorCheck) {
	fmt.Printf("(%d/%d) %s\n", index+1, total, issue.Name)
	if issue.Status == statusError {
		fmt.Printf("  Status: %s\n", ui.RenderFail("ERROR"))
	} else {
		fmt.Printf("  Status: %s\n", ui.RenderWarn("WARNING"))
	}
	fmt.Printf("  Issue:  %s\n", issue.Message)
	if issue.Detail != "" {
		fmt.Printf("  Detail: %s\n", issue.Detail)
	}
	fmt.Printf("  Fix:    %s\n", issue.Fix)
}

type doctorFixDecision uint8

const (
	doctorFixApprove doctorFixDecision = iota
	doctorFixSkip
	doctorFixApproveAll
	doctorFixQuit
)

func promptDoctorFix(reader *bufio.Reader) (doctorFixDecision, error) {
	fmt.Print("\n  Apply this fix? [y/n/a/q]: ")
	response, err := reader.ReadString('\n')
	if err != nil {
		return doctorFixSkip, err
	}

	switch strings.TrimSpace(strings.ToLower(response)) {
	case "y", "yes":
		return doctorFixApprove, nil
	case "a", "all":
		return doctorFixApproveAll, nil
	case "q", "quit":
		return doctorFixQuit, nil
	default:
		return doctorFixSkip, nil
	}
}

func recordDoctorFixDecision(
	decision doctorFixDecision,
	issue doctorCheck,
	approved []doctorCheck,
	applyAll bool,
) ([]doctorCheck, bool, bool) {
	switch decision {
	case doctorFixApprove:
		approved = append(approved, issue)
		fmt.Println("  → Approved")
	case doctorFixApproveAll:
		applyAll = true
		approved = append(approved, issue)
		fmt.Println("  → Approved (applying all remaining)")
	case doctorFixQuit:
		return approved, applyAll, true
	default:
		fmt.Println("  → Skipped")
	}
	return approved, applyAll, false
}

// orderDoctorFixes sorts doctor fixes in place into a dependency-aware apply
// order. Extracted from applyFixList so the ordering invariants are unit
// testable without a live database — notably that "Blocked State" (the full
// is_blocked recompute) runs after every graph-mutating fix, so it recomputes
// from the corrected graph rather than a pre-repair one (bd-6dnrw.37).
func orderDoctorFixes(fixes []doctorCheck) {
	// Apply fixes in a dependency-aware order.
	// Rough dependency chain:
	// gitignore (fast, security-critical) → permissions/lock cleanup → config sanity → DB integrity/migrations.
	order := []string{
		"Gitignore",
		"Project Gitignore",
		"Metadata Config",
		"Lock Files",
		"Circuit Breaker",
		"Permissions",
		"Database Config",
		"Config Values",
		"Database Integrity",
		"Database",
		"Fresh Clone",
		"Schema Compatibility",
		"Project Identity",
	}
	priority := make(map[string]int, len(order)+1)
	for i, name := range order {
		priority[name] = i
	}
	// "Blocked State" recomputes is_blocked from the dependency graph, so it must
	// run after every graph-mutating fix (Dependency Keys, Orphaned/Child-Parent
	// Dependencies, Cross-Table Duplicates). Those are all unlisted and share the
	// default priority below, and their relative order would otherwise be decided
	// by check-append order alone. Pin Blocked State to an explicit terminal
	// priority so it is provably last regardless of append order (bd-6dnrw.37).
	const defaultPriority = 1000
	priority["Blocked State"] = defaultPriority + 1
	slices.SortStableFunc(fixes, func(a, b doctorCheck) int {
		pa, oka := priority[a.Name]
		if !oka {
			pa = defaultPriority
		}
		pb, okb := priority[b.Name]
		if !okb {
			pb = defaultPriority
		}
		if pa < pb {
			return -1
		}
		if pa > pb {
			return 1
		}
		return 0
	})
}

// applyFixList applies a list of fixes and reports results
func applyFixList(path string, fixes []doctorCheck) {
	applyFixListWithOptions(path, fixes, defaultDoctorOptions())
}

type doctorFixAction func(path string, opts doctorOptions) (err error, applied bool)

func applyFixListWithOptions(path string, fixes []doctorCheck, opts doctorOptions) {
	orderDoctorFixes(fixes)
	actions := doctorFixActions()
	fixedCount := 0
	errorCount := 0

	for _, check := range fixes {
		fmt.Printf("\nFixing %s...\n", check.Name)
		action, ok := actions[check.Name]
		if !ok {
			fmt.Printf("  ⚠ No automatic fix available for %s\n", check.Name)
			fmt.Printf("  Manual fix: %s\n", check.Fix)
			continue
		}

		err, applied := action(path, opts)
		if !applied {
			continue
		}
		if err != nil {
			errorCount++
			fmt.Printf("  %s Error: %v\n", ui.RenderFail("✗"), err)
			fmt.Printf("  Manual fix: %s\n", check.Fix)
		} else {
			fixedCount++
			fmt.Printf("  %s Fixed\n", ui.RenderPass("✓"))
		}
	}

	fmt.Printf("\nFix summary: %d fixed, %d errors\n", fixedCount, errorCount)
	if errorCount > 0 {
		fmt.Println("\nSome fixes failed. Please review the errors above and apply manual fixes as needed.")
	}
}

func doctorFixActions() map[string]doctorFixAction {
	return map[string]doctorFixAction{
		"Metadata Config":           doctorPathFix(func(path string) error { return fix.FixMissingMetadataJSON(path) }),
		"Gitignore":                 doctorPathFix(doctor.FixGitignore),
		"Project Gitignore":         fixDoctorProjectGitignore,
		"Redirect Tracking":         doctorPathFix(doctor.FixRedirectTracking),
		"Last-Touched Tracking":     doctorPathFix(doctor.FixLastTouchedTracking),
		"Tracked Runtime Files":     doctorPathFix(doctor.FixTrackedRuntimeFiles),
		"Git Hooks":                 doctorPathFix(fix.GitHooks),
		"Hooks Path":                doctorNoPathFix(func() error { return doctor.FixHooksPath() }),
		"Sync Divergence":           skipDoctorFix("⚠ Sync divergence fix removed (Dolt-native sync)"),
		"Permissions":               doctorPathFix(fix.Permissions),
		"Database":                  fixDoctorDatabase,
		"Database Integrity":        doctorPathFix(fix.DatabaseIntegrity),
		"Schema Compatibility":      doctorPathFix(fix.SchemaCompatibility),
		"Repo Fingerprint":          fixDoctorRepoFingerprint,
		"Database Config":           doctorPathFix(fix.DatabaseConfig),
		"JSONL Config":              skipDoctorFix("⚠ JSONL config migration removed (Dolt-native sync)"),
		"Untracked Files":           skipDoctorFix("⚠ Untracked JSONL fix removed (Dolt-native storage)"),
		"Cross-Table Duplicates":    doctorVerbosePathFix(fix.CrossTableDuplicates),
		"Orphaned Dependencies":     doctorVerbosePathFix(fix.OrphanedDependencies),
		"Clone-Local FKs":           doctorVerbosePathFix(fix.CloneLocalFKEnforcement),
		"Dependency Keys":           doctorVerbosePathFix(fix.DependencyKeys),
		"Blocked State":             doctorPathFix(fix.RecomputeBlocked),
		"Child-Parent Dependencies": fixDoctorChildParent,
		"Duplicate Issues":          skipDoctorFix("⚠ Run 'bd duplicates' to review and merge duplicates"),
		"Test Pollution":            skipDoctorFix("⚠ Run 'bd doctor --check=pollution' to review and clean test issues"),
		"Git Conflicts":             skipDoctorFix("⚠ Resolve conflicts manually"),
		"Stale Closed Issues":       doctorPathFix(fix.StaleClosedIssues),
		"Compaction Candidates":     skipDoctorFix("⚠ Run 'bd compact --analyze' to review candidates"),
		"Large Database":            skipDoctorFix("⚠ Run 'bd cleanup --older-than 90' to prune old closed issues"),
		"Legacy MQ Files":           doctorPathFix(doctor.FixStaleMQFiles),
		"Patrol Pollution":          doctorPathFix(fix.PatrolPollution),
		"Lock Files":                doctorPathFix(fix.StaleLockFiles),
		"Circuit Breaker":           fixDoctorCircuitBreaker,
		"Fresh Clone":               doctorPathFix(func(path string) error { return fix.FreshCloneImport(path, Version) }),
		"Pending Migrations":        doctorPathFix(fixPendingMigrations),
		"Config Values":             doctorPathFix(fix.ConfigValues),
		"Classic Artifacts":         doctorPathFix(fix.ClassicArtifacts),
		"Btrfs NoCOW (dolt)":        fixDoctorBtrfsNoCOW,
		"Project Identity":          doctorPathFix(fix.FixProjectIdentity),
		"Dolt Schema":               doctorPathFix(fix.FixMissingDoltDatabase),
		"Dolt Format":               doctorPathFix(fix.DoltFormat),
		"Corrupt Manifest":          fixDoctorCorruptManifest,
	}
}

func doctorPathFix(fn func(string) error) doctorFixAction {
	return func(path string, _ doctorOptions) (error, bool) {
		return fn(path), true
	}
}

func doctorNoPathFix(fn func() error) doctorFixAction {
	return func(_ string, _ doctorOptions) (error, bool) {
		return fn(), true
	}
}

func doctorVerbosePathFix(fn func(string, bool) error) doctorFixAction {
	return func(path string, opts doctorOptions) (error, bool) {
		return fn(path, opts.fix.verbose), true
	}
}

func skipDoctorFix(message string) doctorFixAction {
	return func(_ string, _ doctorOptions) (error, bool) {
		fmt.Printf("  %s\n", message)
		return nil, false
	}
}

func fixDoctorProjectGitignore(path string, _ doctorOptions) (error, bool) {
	// Stealth / no-git-ops repos must not get a tracked .gitignore; route the
	// patterns into .git/info/exclude and remove any leaked tracked section.
	if isStealthRepo(path) {
		err := addProjectPatternsToGitExclude(path, doctor.ProjectGitignorePatterns, false)
		if err == nil {
			_, err = removeBeadsProjectGitignoreSection(path)
		}
		return err, true
	}
	return doctor.FixProjectGitignore(path), true
}

func fixDoctorDatabase(path string, _ doctorOptions) (error, bool) {
	err := fix.DatabaseVersionWithBdVersion(path, Version)
	if metadataErr := fix.FixMissingMetadata(path, Version); metadataErr != nil && err == nil {
		err = metadataErr
	}
	return err, true
}

func fixDoctorRepoFingerprint(path string, opts doctorOptions) (error, bool) {
	err := fix.RepoFingerprint(path, opts.fix.yes)
	if metadataErr := fix.FixMissingMetadata(path, Version); metadataErr != nil && err == nil {
		err = metadataErr
	}
	return err, true
}

func fixDoctorChildParent(path string, opts doctorOptions) (error, bool) {
	if !opts.fix.childParent {
		fmt.Printf("  ⚠ Child→parent deps require explicit opt-in: bd doctor --fix --fix-child-parent\n")
		return nil, false
	}
	return fix.ChildParentDependencies(path, opts.fix.verbose), true
}

func fixDoctorCircuitBreaker(_ string, _ doctorOptions) (error, bool) {
	dolt.CleanStaleCircuitBreakerFiles()
	fmt.Printf("  %s Cleared stale circuit breaker files\n", ui.RenderPass("✓"))
	return nil, true
}

func fixDoctorBtrfsNoCOW(path string, _ doctorOptions) (error, bool) {
	msg, err := doctor.FixBtrfsNoCOW(path)
	if err == nil && msg != "" {
		fmt.Print(msg)
		if !strings.HasSuffix(msg, "\n") {
			fmt.Println()
		}
	}
	return err, true
}

func fixDoctorCorruptManifest(path string, _ doctorOptions) (error, bool) {
	backups, err := doltserver.RecoverCorruptManifest(doctor.ResolveBeadsDirForRepo(path))
	for _, backup := range backups {
		fmt.Printf("  Backed up corrupt dolt database to %s and reinitialized\n", backup)
	}
	return err, true
}

func fixPendingMigrations(path string) error {
	pending := doctor.DetectPendingMigrations(path)
	if len(pending) == 0 {
		return nil
	}

	for _, migration := range pending {
		switch migration.Name {
		case "hooks":
			plan, err := doctor.PlanHookMigration(path)
			if err != nil {
				return fmt.Errorf("building hook migration plan: %w", err)
			}

			execPlan := buildHookMigrationExecutionPlan(plan)
			if len(execPlan.BlockingErrors) > 0 {
				return fmt.Errorf("hook migration is blocked:\n- %s", strings.Join(execPlan.BlockingErrors, "\n- "))
			}

			summary, err := applyHookMigrationExecution(execPlan)
			if err != nil {
				return fmt.Errorf("applying hook migration: %w", err)
			}

			fmt.Printf(
				"  Hook migration applied: %d hook(s) written, %d artifact(s) retired, %d artifact(s) skipped\n",
				summary.WrittenHookCount,
				summary.RetiredCount,
				summary.SkippedCount,
			)
		default:
			return fmt.Errorf("no automatic fix available for pending migration %q", migration.Name)
		}
	}

	return nil
}
