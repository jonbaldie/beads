package main

import (
	"os"
	"path/filepath"

	"github.com/jonbaldie/beads/cmd/bd/doctor"
	"github.com/jonbaldie/beads/internal/configfile"
)

func runDiagnostics(path string) doctorResult {
	return runDiagnosticsWithOptions(path, defaultDoctorOptions())
}

func runDiagnosticsWithOptions(path string, opts doctorOptions) doctorResult {
	if opts.mode.duplicatesLimit == 0 {
		opts.mode.duplicatesLimit = defaultDoctorOptions().mode.duplicatesLimit
	}
	result := doctorResult{
		Path:       path,
		CLIVersion: Version,
		OverallOK:  true,
	}
	opts.mode.orchestrator = detectDoctorOrchestrator(path, opts.mode.orchestrator)
	if !runDoctorInitialChecks(&result, path) {
		return result
	}

	beadsDir := doctor.ResolveBeadsDirForRepo(path)
	runDoctorPreDatabaseChecks(&result, path, beadsDir)

	sharedStore := doctor.NewSharedStore(path)
	defer sharedStore.Close()
	runDoctorDatabaseChecks(&result, path, sharedStore)
	runDoctorFederationChecks(&result, path)
	runDoctorIntegrityChecks(&result, path, sharedStore)
	runDoctorIntegrationChecks(&result, path)
	runDoctorGitChecks(&result, path)
	runDoctorMetadataChecks(&result, path, opts)
	runDoctorMaintenanceChecks(&result, path, sharedStore)
	filterSuppressedDoctorChecks(&result, sharedStore)
	return result
}

func detectDoctorOrchestrator(path string, requested bool) bool {
	if requested {
		return true
	}
	resolvedBeadsDir := doctor.ResolveBeadsDirForRepo(path)
	routesFile := filepath.Join(resolvedBeadsDir, "routes.jsonl")
	if _, err := os.Stat(routesFile); err == nil {
		return true
	}
	return false
}

func runDoctorInitialChecks(result *doctorResult, path string) bool {
	installCheck := convertWithCategory(doctor.CheckInstallation(path), doctor.CategoryCore)
	result.Checks = append(result.Checks, installCheck)
	if installCheck.Status != statusOK {
		result.OverallOK = false
	}

	result.Checks = append(result.Checks,
		convertWithCategory(doctor.CheckGitHooks(Version), doctor.CategoryGit),
		convertWithCategory(doctor.CheckStaleLegacyHooks(), doctor.CategoryGit),
		convertWithCategory(doctor.CheckHooksPath(), doctor.CategoryGit),
	)
	doltHooksCheck := convertWithCategory(doctor.CheckGitHooksDoltCompatibility(path), doctor.CategoryGit)
	result.Checks = append(result.Checks, doltHooksCheck)
	if doltHooksCheck.Status == statusError {
		result.OverallOK = false
	}
	return installCheck.Status == statusOK
}

func runDoctorPreDatabaseChecks(result *doctorResult, path, beadsDir string) {
	// Check 1a: Fresh clone detection
	// Must come early - if this is a fresh clone, other checks may be misleading
	freshCloneCheck := convertWithCategory(doctor.CheckFreshClone(path), doctor.CategoryCore)
	result.Checks = append(result.Checks, freshCloneCheck)
	if freshCloneCheck.Status == statusWarning || freshCloneCheck.Status == statusError {
		result.OverallOK = false
	}

	// Check 1b: Metadata config file (GH#2478)
	// Must come before database checks since they depend on metadata.json.
	configPath := configfile.ConfigPath(beadsDir)
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		metaCheck := doctorCheck{
			Name:     "Metadata Config",
			Status:   statusError,
			Message:  "metadata.json is missing",
			Fix:      "Run 'bd doctor --fix' to regenerate with defaults, or 'bd init --force'",
			Category: doctor.CategoryCore,
		}
		result.Checks = append(result.Checks, metaCheck)
		result.OverallOK = false
	} else {
		result.Checks = append(result.Checks, doctorCheck{
			Name:     "Metadata Config",
			Status:   statusOK,
			Message:  "metadata.json present",
			Category: doctor.CategoryCore,
		})
	}

	// Check 1c: Managed-city handoff port conflict (GH#3926)
	managedHandoffCheck := convertDoctorCheck(doctor.CheckManagedHandoffPort(path))
	result.Checks = append(result.Checks, managedHandoffCheck)
	if managedHandoffCheck.Status == statusWarning || managedHandoffCheck.Status == statusError {
		result.OverallOK = false
	}

	// bd-jgxi: Auto-migrate database version before checking it.
	// Since doctor skips PersistentPreRun DB init (via skipStoreAnnotation),
	// trackBdVersion() and autoMigrateOnVersionBump() haven't run yet.
	//
	// Scope version tracking to the doctor target. Without this, `bd doctor <path>`
	// can accidentally touch the caller's current repo .beads state.
	origBeadsDir, hadBeadsDir := os.LookupEnv("BEADS_DIR")
	_ = os.Setenv("BEADS_DIR", beadsDir)
	trackBdVersion()
	if hadBeadsDir {
		_ = os.Setenv("BEADS_DIR", origBeadsDir)
	} else {
		_ = os.Unsetenv("BEADS_DIR")
	}

	autoMigrateOnVersionBump(beadsDir)

	// Check 1b: Dolt format compatibility (GH#2137)
	// Must run before opening the database — old noms formats cause server panics.
	doltFormatCheck := convertWithCategory(doctor.CheckDoltFormat(path), doctor.CategoryCore)
	result.Checks = append(result.Checks, doltFormatCheck)
	if doltFormatCheck.Status == statusError {
		result.OverallOK = false
	}
}

func runDoctorDatabaseChecks(result *doctorResult, path string, sharedStore *doctor.SharedStore) {
	runDoctorStoreChecks(result, path, sharedStore)
	runDoctorVersionChecks(result)
	runDoctorConfigurationChecks(result, path, sharedStore)
}

func runDoctorStoreChecks(result *doctorResult, path string, sharedStore *doctor.SharedStore) {
	// Check 2: Database version
	dbCheck := convertWithCategory(doctor.CheckDatabaseVersionWithStore(sharedStore, Version), doctor.CategoryCore)
	result.Checks = append(result.Checks, dbCheck)
	if dbCheck.Status == statusError {
		result.OverallOK = false
	}

	// Check 2a: Schema compatibility
	schemaCheck := convertWithCategory(doctor.CheckSchemaCompatibilityWithStore(sharedStore), doctor.CategoryCore)
	result.Checks = append(result.Checks, schemaCheck)
	if schemaCheck.Status == statusError {
		result.OverallOK = false
	}

	// Check 2b: Repo fingerprint (detects wrong database or URL change)
	fingerprintCheck := convertWithCategory(doctor.CheckRepoFingerprintWithStore(sharedStore, path), doctor.CategoryCore)
	result.Checks = append(result.Checks, fingerprintCheck)
	if fingerprintCheck.Status == statusError {
		result.OverallOK = false
	}

	// Check 2c: Database integrity
	integrityCheck := convertWithCategory(doctor.CheckDatabaseIntegrityWithStore(sharedStore), doctor.CategoryCore)
	result.Checks = append(result.Checks, integrityCheck)
	if integrityCheck.Status == statusError {
		result.OverallOK = false
	}

	// Check 3: ID format (hash vs sequential)
	idCheck := convertWithCategory(doctor.CheckIDFormatWithStore(sharedStore), doctor.CategoryCore)
	result.Checks = append(result.Checks, idCheck)
	if idCheck.Status == statusWarning {
		result.OverallOK = false
	}
}

func runDoctorVersionChecks(result *doctorResult) {
	// Network-based update checks are skipped in machine-readable and other
	// non-interactive contexts so doctor remains deterministic under wrappers.
	versionCheckFn := doctor.CheckCLIVersion
	pluginCheckFn := doctor.CheckClaudePlugin
	if shouldSkipDoctorNetworkChecks() {
		versionCheckFn = doctor.CheckCLIVersionLocalOnly
		pluginCheckFn = doctor.CheckClaudePluginLocalOnly
	}

	// Check 4: CLI version
	versionCheck := convertWithCategory(versionCheckFn(Version), doctor.CategoryCore)
	result.Checks = append(result.Checks, versionCheck)
	// Don't fail overall check for outdated CLI, just warn

	// Check 4.5: Claude plugin version (if running in Claude Code)
	pluginCheck := convertWithCategory(pluginCheckFn(), doctor.CategoryIntegration)
	result.Checks = append(result.Checks, pluginCheck)
	// Don't fail overall check for outdated plugin, just warn
}

func runDoctorConfigurationChecks(result *doctorResult, path string, sharedStore *doctor.SharedStore) {
	// Check 7: Database/JSONL configuration mismatch
	configCheck := convertWithCategory(doctor.CheckDatabaseConfig(path), doctor.CategoryData)
	result.Checks = append(result.Checks, configCheck)
	if configCheck.Status == statusWarning || configCheck.Status == statusError {
		result.OverallOK = false
	}

	// Check 7a: Configuration value validation
	configValuesCheck := convertWithCategory(doctor.CheckConfigValuesWithStore(path, sharedStore), doctor.CategoryData)
	result.Checks = append(result.Checks, configValuesCheck)
	// Don't fail overall check for config value warnings, just warn

	// Check 7a1: Project identity (GH#2372 backfill)
	projectIDCheck := convertWithCategory(doctor.CheckProjectIdentityWithStore(sharedStore, path), doctor.CategoryData)
	result.Checks = append(result.Checks, projectIDCheck)
	if projectIDCheck.Status == statusWarning || projectIDCheck.Status == statusError {
		result.OverallOK = false
	}
	runDoctorConfigurationHealthChecks(result, path, sharedStore)
}

func runDoctorConfigurationHealthChecks(result *doctorResult, path string, sharedStore *doctor.SharedStore) {
	// Check 7b: Multi-repo custom types discovery (bd-9ji4z)
	multiRepoTypesCheck := convertWithCategory(doctor.CheckMultiRepoTypes(path), doctor.CategoryData)
	result.Checks = append(result.Checks, multiRepoTypesCheck)
	// Don't fail overall check for multi-repo types, just informational

	// Check 7c: Role configuration (beads.role)
	roleCheck := convertDoctorCheck(doctor.CheckBeadsRoleWithStore(path, sharedStore))
	result.Checks = append(result.Checks, roleCheck)
	// Don't fail overall check for role config, just warn - URL heuristic fallback still works

	// Check 7e: Stale lock files (bootstrap, sync, startup)
	staleLockCheck := convertDoctorCheck(doctor.CheckStaleLockFiles(path))
	result.Checks = append(result.Checks, staleLockCheck)
	if staleLockCheck.Status == statusWarning || staleLockCheck.Status == statusError {
		result.OverallOK = false
	}

	// Check 7e1: Corrupt-manifest state (GH#3290). Detection only; the
	// destructive backup+reinit repair runs solely via doctor --fix (bd-6dnrw.6).
	corruptManifestCheck := convertDoctorCheck(doctor.CheckCorruptManifest(path))
	result.Checks = append(result.Checks, corruptManifestCheck)
	if corruptManifestCheck.Status == statusError {
		result.OverallOK = false
	}

	// Check 7e2: Stale circuit breaker files
	circuitCheck := convertDoctorCheck(doctor.CheckCircuitBreaker())
	result.Checks = append(result.Checks, circuitCheck)
	if circuitCheck.Status == statusWarning || circuitCheck.Status == statusError {
		result.OverallOK = false
	}

	// Check 7f1: Dolt remote URL collision with git origin (be-7eu1d)
	doltOriginCheck := convertWithCategory(doctor.CheckDoltRemoteGitOrigin(path), doctor.CategoryDolt)
	result.Checks = append(result.Checks, doltOriginCheck)

	// Check 7f: Migration content skew vs the cached remote ref (#4259). Advisory.
	skewCheck := convertWithCategory(doctor.CheckMigrationContentSkew(sharedStore), doctor.CategoryData)
	result.Checks = append(result.Checks, skewCheck)
}

func runDoctorFederationChecks(result *doctorResult, path string) {
	// Dolt health checks (connection, schema, issue count, status).
	for _, dc := range doctor.RunDoltHealthChecks(path) {
		result.Checks = append(result.Checks, convertDoctorCheck(dc))
	}

	legacyRemoteCheck := convertWithCategory(doctor.CheckLegacyCLIRemotes(path), doctor.CategoryFederation)
	result.Checks = append(result.Checks, legacyRemoteCheck)

	// Federation health checks (bd-wkumz.6)
	// Check 8d: Federation remotesapi port accessibility
	remotesAPICheck := convertWithCategory(doctor.CheckFederationRemotesAPI(path), doctor.CategoryFederation)
	result.Checks = append(result.Checks, remotesAPICheck)
	// Don't fail overall for federation issues - they're only relevant for Dolt users

	// Check 8e: Federation peer connectivity
	peerConnCheck := convertWithCategory(doctor.CheckFederationPeerConnectivity(path), doctor.CategoryFederation)
	result.Checks = append(result.Checks, peerConnCheck)

	// Check 8f: Federation sync staleness
	syncStalenessCheck := convertWithCategory(doctor.CheckFederationSyncStaleness(path), doctor.CategoryFederation)
	result.Checks = append(result.Checks, syncStalenessCheck)

	// Check 8g: Federation conflict detection
	fedConflictsCheck := convertWithCategory(doctor.CheckFederationConflicts(path), doctor.CategoryFederation)
	result.Checks = append(result.Checks, fedConflictsCheck)
	if fedConflictsCheck.Status == statusError {
		result.OverallOK = false // Unresolved conflicts are a real problem
	}

	// Check 8h: Dolt server mode configuration check
	doltModeCheck := convertWithCategory(doctor.CheckDoltServerModeMismatch(path), doctor.CategoryFederation)
	result.Checks = append(result.Checks, doltModeCheck)
}

func runDoctorIntegrityChecks(result *doctorResult, path string, sharedStore *doctor.SharedStore) {
	// Check 9: Permissions
	permCheck := convertWithCategory(doctor.CheckPermissionsWithStore(path, sharedStore), doctor.CategoryCore)
	result.Checks = append(result.Checks, permCheck)
	if permCheck.Status == statusError {
		result.OverallOK = false
	}

	// Check 10: Dependency cycles
	cycleCheck := convertWithCategory(doctor.CheckDependencyCyclesWithStore(sharedStore), doctor.CategoryMetadata)
	result.Checks = append(result.Checks, cycleCheck)
	if cycleCheck.Status == statusError || cycleCheck.Status == statusWarning {
		result.OverallOK = false
	}

	// Check 10b: Rekey-backfill leftovers — randomly-keyed or targetless
	// dependency rows that survive the #4259 migration backfill (bd-6dnrw.17).
	depKeyCheck := convertWithCategory(doctor.CheckDependencyKeysWithStore(sharedStore), doctor.CategoryMetadata)
	result.Checks = append(result.Checks, depKeyCheck)
	if depKeyCheck.Status == statusError || depKeyCheck.Status == statusWarning {
		result.OverallOK = false
	}

	// Check 10c: is_blocked consistency — derived flags a skipped post-pull
	// recompute can leave stale (bd-6dnrw.37). `bd ready` trusts is_blocked, so
	// staleness silently hides ready work; the full recompute repairs it.
	// Warn-only (does not fail OverallOK): this is a new check shipping in a
	// patch, and is_blocked is self-healing via 'bd doctor --fix' / the next
	// pull's recompute — surface it as actionable without turning doctor red
	// across the fleet if an unforeseen dependency shape trips the predicate.
	blockedConsistencyCheck := convertWithCategory(doctor.CheckBlockedConsistencyWithStore(sharedStore), doctor.CategoryData)
	result.Checks = append(result.Checks, blockedConsistencyCheck)
}

func runDoctorIntegrationChecks(result *doctorResult, path string) {
	// Check 11: Claude integration
	claudeCheck := convertWithCategory(doctor.CheckClaude(path), doctor.CategoryIntegration)
	result.Checks = append(result.Checks, claudeCheck)
	// Don't fail overall check for missing Claude integration, just warn

	// Check 11a: Claude settings file health (malformed JSON detection)
	claudeSettingsCheck := convertWithCategory(doctor.CheckClaudeSettingsHealth(path), doctor.CategoryIntegration)
	result.Checks = append(result.Checks, claudeSettingsCheck)
	if claudeSettingsCheck.Status == statusError {
		result.OverallOK = false // Malformed settings is a real problem
	}

	// Check 11b: Claude hook completeness (both SessionStart and PreCompact)
	claudeHookCheck := convertWithCategory(doctor.CheckClaudeHookCompleteness(path), doctor.CategoryIntegration)
	result.Checks = append(result.Checks, claudeHookCheck)
	// Don't fail overall check for incomplete hooks, just warn

	// Check 11c: bd prime output verification
	bdPrimeOutputCheck := convertWithCategory(doctor.VerifyPrimeOutput(path), doctor.CategoryIntegration)
	result.Checks = append(result.Checks, bdPrimeOutputCheck)
	// Don't fail overall check for prime output issues, just warn

	// Check 11d: bd in PATH (needed for Claude hooks and other integrations)
	bdPathCheck := convertWithCategory(doctor.CheckBdInPath(), doctor.CategoryIntegration)
	result.Checks = append(result.Checks, bdPathCheck)
	// Don't fail overall check for missing bd in PATH, just warn

	// Check 11e: Cursor integration (agent hooks)
	cursorCheck := convertWithCategory(doctor.CheckCursor(path), doctor.CategoryIntegration)
	result.Checks = append(result.Checks, cursorCheck)
	// Don't fail overall check for missing Cursor integration, just warn

	// Check 11f: Cursor hooks file health (malformed JSON detection)
	cursorSettingsCheck := convertWithCategory(doctor.CheckCursorSettingsHealth(path), doctor.CategoryIntegration)
	result.Checks = append(result.Checks, cursorSettingsCheck)
	if cursorSettingsCheck.Status == statusError {
		result.OverallOK = false // Malformed hooks.json is a real problem
	}

	// Check 11g: Cursor hook completeness (all three lifecycle events)
	cursorHookCheck := convertWithCategory(doctor.CheckCursorHookCompleteness(path), doctor.CategoryIntegration)
	result.Checks = append(result.Checks, cursorHookCheck)
	// Don't fail overall check for incomplete hooks, just warn

	// Check 11h: Documentation bd prime references match installed version
	bdPrimeDocsCheck := convertWithCategory(doctor.CheckDocumentationBdPrimeReference(path), doctor.CategoryIntegration)
	result.Checks = append(result.Checks, bdPrimeDocsCheck)
	// Don't fail overall check for doc mismatch, just warn

	// Check 12: Agent documentation presence
	agentDocsCheck := convertWithCategory(doctor.CheckAgentDocumentation(path), doctor.CategoryIntegration)
	result.Checks = append(result.Checks, agentDocsCheck)
	// Don't fail overall check for missing docs, just warn

	// Check 12a: AGENTS.md / CLAUDE.md user-authored divergence
	agentDocDivergenceCheck := convertWithCategory(doctor.CheckAgentDocDivergence(path), doctor.CategoryIntegration)
	result.Checks = append(result.Checks, agentDocDivergenceCheck)
	// Don't fail overall check for divergence, just warn

	// Check 13: Legacy beads slash commands in documentation
	legacyDocsCheck := convertWithCategory(doctor.CheckLegacyBeadsSlashCommands(path), doctor.CategoryMetadata)
	result.Checks = append(result.Checks, legacyDocsCheck)
	// Don't fail overall check for legacy docs, just warn

	// Check 13a: MCP tool references in documentation
	mcpToolRefsCheck := convertWithCategory(doctor.CheckLegacyMCPToolReferences(path), doctor.CategoryIntegration)
	result.Checks = append(result.Checks, mcpToolRefsCheck)
	// Don't fail overall check for MCP tool refs, just warn
}
