package main

import (
	"github.com/jonbaldie/beads/cmd/bd/doctor"
)

func runDoctorGitChecks(result *doctorResult, path string) {
	// Check 14: Gitignore up to date
	gitignoreCheck := convertWithCategory(doctor.CheckGitignore(path), doctor.CategoryGit)
	result.Checks = append(result.Checks, gitignoreCheck)
	// Don't fail overall check for gitignore, just warn

	// Check 14a: Project-root Dolt exclusion patterns (GH#2034). In stealth mode these live in
	// .git/info/exclude, so check that location instead to avoid recreating .gitignore.
	if isStealthRepo(path) {
		result.Checks = append(result.Checks, convertWithCategory(checkProjectExcludeStealth(path), doctor.CategoryGit))
	} else {
		projectGitignoreCheck := convertWithCategory(doctor.CheckProjectGitignore(path), doctor.CategoryGit)
		result.Checks = append(result.Checks, projectGitignoreCheck)
	}
	// Don't fail overall check for project gitignore, just warn

	// Check 14b: redirect file tracking (worktree redirect files shouldn't be committed)
	redirectTrackingCheck := convertWithCategory(doctor.CheckRedirectNotTracked(path), doctor.CategoryGit)
	result.Checks = append(result.Checks, redirectTrackingCheck)
	// Don't fail overall check for redirect tracking, just warn

	// Check 14c: redirect target validity (target exists and has valid db)
	redirectTargetCheck := convertWithCategory(doctor.CheckRedirectTargetValid(path), doctor.CategoryGit)
	result.Checks = append(result.Checks, redirectTargetCheck)
	// Don't fail overall check for redirect target, just warn

	// Check 14d: redirect target sync worktree (target has beads-sync if needed)
	redirectTargetSyncCheck := convertWithCategory(doctor.CheckRedirectTargetSyncWorktree(path), doctor.CategoryGit)
	result.Checks = append(result.Checks, redirectTargetSyncCheck)
	// Don't fail overall check for redirect target sync, just warn

	// Check 14e: vestigial sync worktrees (unused worktrees in redirected repos)
	vestigialWorktreesCheck := convertWithCategory(doctor.CheckNoVestigialSyncWorktrees(path), doctor.CategoryGit)
	result.Checks = append(result.Checks, vestigialWorktreesCheck)
	// Don't fail overall check for vestigial worktrees, just warn

	// Check 14g: last-touched file tracking (runtime state shouldn't be committed)
	lastTouchedTrackingCheck := convertWithCategory(doctor.CheckLastTouchedNotTracked(path), doctor.CategoryGit)
	result.Checks = append(result.Checks, lastTouchedTrackingCheck)
	// Don't fail overall check for last-touched tracking, just warn

	// Check 14h: tracked runtime/sensitive files (GH#2535)
	trackedRuntimeCheck := convertDoctorCheck(doctor.CheckTrackedRuntimeFiles(path))
	result.Checks = append(result.Checks, trackedRuntimeCheck)
	if trackedRuntimeCheck.Status == statusError {
		result.OverallOK = false // Sensitive files in git is a real problem
	}

	// Check 15a: Git working tree cleanliness (AGENTS.md hygiene)
	gitWorkingTreeCheck := convertWithCategory(doctor.CheckGitWorkingTree(path), doctor.CategoryGit)
	result.Checks = append(result.Checks, gitWorkingTreeCheck)
	// Don't fail overall check for dirty working tree, just warn

	// Check 15b: Git upstream sync (ahead/behind/diverged)
	gitUpstreamCheck := convertWithCategory(doctor.CheckGitUpstream(path), doctor.CategoryGit)
	result.Checks = append(result.Checks, gitUpstreamCheck)
	// Don't fail overall check for upstream drift, just warn

	// Check 16: Metadata.json version tracking
	metadataCheck := convertWithCategory(doctor.CheckMetadataVersionTracking(path, Version), doctor.CategoryMetadata)
	result.Checks = append(result.Checks, metadataCheck)
	// Don't fail overall check for metadata, just warn
}

func runDoctorMetadataChecks(result *doctorResult, path string, opts doctorOptions) {
	// Check 17b: Orphaned issues - referenced in commits but still open
	orphanedIssuesCheck := convertWithCategory(doctor.CheckOrphanedIssues(path), doctor.CategoryGit)
	result.Checks = append(result.Checks, orphanedIssuesCheck)
	// Don't fail overall check for orphaned issues, just warn

	// Check 18: Deletions manifest (legacy)
	deletionsCheck := convertWithCategory(doctor.CheckDeletionsManifest(path), doctor.CategoryMetadata)
	result.Checks = append(result.Checks, deletionsCheck)
	// Don't fail overall check for missing deletions manifest, just warn

	// Check 20: Untracked .beads/*.jsonl files
	untrackedCheck := convertWithCategory(doctor.CheckUntrackedBeadsFiles(path), doctor.CategoryData)
	result.Checks = append(result.Checks, untrackedCheck)
	// Don't fail overall check for untracked files, just warn

	// Check 21: Orphaned dependencies (from bd repair-deps, bd validate)
	orphanedDepsCheck := convertDoctorCheck(doctor.CheckOrphanedDependencies(path))
	result.Checks = append(result.Checks, orphanedDepsCheck)
	// Don't fail overall check for orphaned deps, just warn

	// Check 21b: Clone-local FKs severed by hard resets (bd-7bpkd)
	cloneLocalFKCheck := convertDoctorCheck(doctor.CheckCloneLocalFKs(path))
	result.Checks = append(result.Checks, cloneLocalFKCheck)
	// Don't fail overall check for severed clone-local FKs, just warn

	// Check 22a: Child→parent dependencies (anti-pattern)
	childParentDepsCheck := convertDoctorCheck(doctor.CheckChildParentDependencies(path))
	result.Checks = append(result.Checks, childParentDepsCheck)
	// Don't fail overall check for child→parent deps, just warn

	// Check 23: Duplicate issues (from bd validate)
	duplicatesCheck := convertDoctorCheck(doctor.CheckDuplicateIssues(path, opts.mode.orchestrator, opts.mode.duplicatesLimit))
	result.Checks = append(result.Checks, duplicatesCheck)
	// Don't fail overall check for duplicates, just warn

	// Check 24: Test pollution (from bd validate)
	pollutionCheck := convertDoctorCheck(doctor.CheckTestPollution(path))
	result.Checks = append(result.Checks, pollutionCheck)
	// Don't fail overall check for test pollution, just warn

	// Check 26: Stale closed issues (maintenance)
	staleClosedCheck := convertDoctorCheck(doctor.CheckStaleClosedIssues(path))
	result.Checks = append(result.Checks, staleClosedCheck)
	// Don't fail overall check for stale issues, just warn

	// Check 26a: Stale molecules (complete but unclosed)
	staleMoleculesCheck := convertDoctorCheck(doctor.CheckStaleMolecules(path))
	result.Checks = append(result.Checks, staleMoleculesCheck)
	// Don't fail overall check for stale molecules, just warn

	// Check 26b: Persistent mol- issues (should have been ephemeral)
	persistentMolCheck := convertDoctorCheck(doctor.CheckPersistentMolIssues(path))
	result.Checks = append(result.Checks, persistentMolCheck)
	// Don't fail overall check for persistent mol issues, just warn

	// Check 26c: Legacy merge queue files (orchestrator mrqueue remnants)
	staleMQFilesCheck := convertDoctorCheck(doctor.CheckStaleMQFiles(path))
	result.Checks = append(result.Checks, staleMQFilesCheck)
	// Don't fail overall check for legacy MQ files, just warn

	// Check 26d: Patrol pollution (patrol digests, session beads)
	patrolPollutionCheck := convertDoctorCheck(doctor.CheckPatrolPollution(path))
	result.Checks = append(result.Checks, patrolPollutionCheck)
	// Don't fail overall check for patrol pollution, just warn
}

func runDoctorMaintenanceChecks(result *doctorResult, path string, sharedStore *doctor.SharedStore) {
	// Check 29: Database size (pruning suggestion)
	// Note: This check has no auto-fix - pruning is destructive and user-controlled
	sizeCheck := convertDoctorCheck(doctor.CheckDatabaseSizeWithStore(sharedStore))
	result.Checks = append(result.Checks, sizeCheck)
	// Don't fail overall check for size warning, just inform

	// Check 30: Pending migrations (summarizes all available migrations)
	migrationsCheck := convertDoctorCheck(doctor.CheckPendingMigrations(path))
	result.Checks = append(result.Checks, migrationsCheck)
	// Status is determined by the check itself based on migration priorities
	if migrationsCheck.Status == statusError {
		result.OverallOK = false
	}

	// Check 31: KV store sync status
	kvSyncCheck := convertDoctorCheck(doctor.CheckKVSyncStatus(path))
	result.Checks = append(result.Checks, kvSyncCheck)
	// Don't fail overall check for KV sync warning, just inform

	// Check 32: Dolt locks (uncommitted changes)
	doltLocksCheck := convertDoctorCheck(doctor.CheckDoltLocks(path))
	result.Checks = append(result.Checks, doltLocksCheck)
	// Don't fail overall check for Dolt locks, just warn

	// Check 33: Classic artifacts (post-Dolt-migration cleanup)
	classicArtifactsCheck := convertDoctorCheck(doctor.CheckClassicArtifacts(path))
	result.Checks = append(result.Checks, classicArtifactsCheck)
	// Don't fail overall check for classic artifacts, just warn

	// Check 34: Linux btrfs NoCOW on .beads/ (GH nocow-beads-dolt-init)
	// Warns when the dolt data directory sits on btrfs without FS_NOCOW_FL,
	// which causes kworker thrashing on the hot append-only write path. Safe
	// no-op on non-Linux and non-btrfs filesystems.
	btrfsNoCowCheck := convertDoctorCheck(doctor.CheckBtrfsNoCOW(path))
	result.Checks = append(result.Checks, btrfsNoCowCheck)
	// Don't fail overall check for btrfs NoCOW, just warn
}

func filterSuppressedDoctorChecks(result *doctorResult, sharedStore *doctor.SharedStore) {
	suppressed := doctor.GetSuppressedChecksWithStore(sharedStore)
	if len(suppressed) > 0 {
		var suppressedCount int
		var filtered []doctorCheck
		for _, check := range result.Checks {
			slug := doctor.CheckNameToSlug(check.Name)
			if suppressed[slug] && check.Status == statusWarning {
				suppressedCount++
				continue
			}
			filtered = append(filtered, check)
		}
		if suppressedCount > 0 {
			result.Checks = filtered
			// Recompute OverallOK after filtering
			result.OverallOK = true
			for _, check := range result.Checks {
				if check.Status == statusError {
					result.OverallOK = false
					break
				}
				if check.Status == statusWarning {
					// Some warnings are informational (don't fail), but
					// replicate the per-check logic from above is complex.
					// Conservative: don't change OverallOK for warnings here.
				}
			}
			// Store suppressed count for display
			result.SuppressedCount = suppressedCount
		}
	}
}

// runInitDiagnostics runs a limited subset of diagnostics appropriate for a
// freshly-initialized project. Unlike runDiagnostics (which checks everything),
// this only validates that the init itself succeeded: the .beads directory exists,
// the database is openable with correct schema, and permissions are correct.
// Checks that require git, federation remotes, or other post-setup configuration
// are skipped since they cannot be satisfied in a fresh project.
func runInitDiagnostics(path string) doctorResult {
	result := doctorResult{
		Path:       path,
		CLIVersion: Version,
		OverallOK:  true,
	}

	// Check 1: Installation (.beads/ directory)
	installCheck := convertWithCategory(doctor.CheckInstallation(path), doctor.CategoryCore)
	result.Checks = append(result.Checks, installCheck)
	if installCheck.Status != statusOK {
		result.OverallOK = false
		return result
	}

	// Check 1b: Dolt format compatibility (GH#2137)
	doltFormatCheck := convertWithCategory(doctor.CheckDoltFormat(path), doctor.CategoryCore)
	result.Checks = append(result.Checks, doltFormatCheck)
	if doltFormatCheck.Status == statusError {
		result.OverallOK = false
	}

	// Open one shared store for the database-backed init checks. Server-mode
	// workspaces may not have a local .beads/dolt directory, so the path-only
	// checks would otherwise report a false missing database.
	sharedStore := doctor.NewSharedStore(path)
	defer sharedStore.Close()

	// Check 2: Database version
	dbCheck := convertWithCategory(doctor.CheckDatabaseVersionWithStore(sharedStore, Version), doctor.CategoryCore)
	result.Checks = append(result.Checks, dbCheck)
	if dbCheck.Status == statusError {
		result.OverallOK = false
	}

	// Check 3: Schema compatibility
	schemaCheck := convertWithCategory(doctor.CheckSchemaCompatibilityWithStore(sharedStore), doctor.CategoryCore)
	result.Checks = append(result.Checks, schemaCheck)
	if schemaCheck.Status == statusError {
		result.OverallOK = false
	}

	// Check 4: Permissions
	permCheck := convertWithCategory(doctor.CheckPermissionsWithStore(path, sharedStore), doctor.CategoryCore)
	result.Checks = append(result.Checks, permCheck)
	if permCheck.Status == statusError {
		result.OverallOK = false
	}

	// Check 5: Dolt connection — validates init actually created a working DB
	doltConnCheck := convertDoctorCheck(doctor.CheckDoltConnection(path))
	result.Checks = append(result.Checks, doltConnCheck)
	if doltConnCheck.Status == statusError {
		result.OverallOK = false
	}

	// Check 6: Dolt schema — validates tables were created
	doltSchemaCheck := convertDoctorCheck(doctor.CheckDoltSchema(path))
	result.Checks = append(result.Checks, doltSchemaCheck)
	if doltSchemaCheck.Status == statusError {
		result.OverallOK = false
	}

	return result
}
