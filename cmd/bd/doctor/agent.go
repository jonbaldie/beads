package doctor

import (
	"fmt"
)

// AgentDiagnostic represents a single check result enriched for agent consumption.
// ZFC-compliant: Go observes and reports, the agent decides and acts.
type AgentDiagnostic struct {
	Name        string   `json:"name"`
	Status      string   `json:"status"`   // "error", "warning", "ok"
	Severity    string   `json:"severity"` // "blocking", "degraded", "advisory"
	Category    string   `json:"category"`
	Explanation string   `json:"explanation"`            // Full prose: what's wrong and why it matters
	Observed    string   `json:"observed"`               // What was actually found
	Expected    string   `json:"expected"`               // What should be the case
	Commands    []string `json:"commands,omitempty"`     // Exact remediation commands in order
	SourceFiles []string `json:"source_files,omitempty"` // Relevant source paths for investigation
}

// agentEnrichment holds the extra context fields an enricher adds.
type agentEnrichment struct {
	severity    string
	explanation string
	observed    string
	expected    string
	commands    []string
	sourceFiles []string
}

// enricher is a function that produces agent enrichment from a DoctorCheck.
type enricher func(dc DoctorCheck) agentEnrichment

// EnrichForAgent converts a DoctorCheck into an AgentDiagnostic with rich context.
func EnrichForAgent(dc DoctorCheck) AgentDiagnostic {
	ad := AgentDiagnostic{
		Name:     dc.Name,
		Status:   dc.Status,
		Category: dc.Category,
	}

	// Look up a specialized enricher for this check name
	if fn, ok := agentEnrichers[dc.Name]; ok {
		e := fn(dc)
		ad.Severity = e.severity
		ad.Explanation = e.explanation
		ad.Observed = e.observed
		ad.Expected = e.expected
		ad.Commands = e.commands
		ad.SourceFiles = e.sourceFiles
		return ad
	}

	// Generic fallback: synthesize from existing DoctorCheck fields
	ad.Severity = severityFromStatus(dc.Status)
	ad.Explanation = buildGenericExplanation(dc)
	ad.Observed = dc.Message
	if dc.Detail != "" {
		ad.Observed += "\n" + dc.Detail
	}
	ad.Expected = "Check passes with status OK"
	if dc.Fix != "" {
		ad.Commands = []string{dc.Fix}
	}
	return ad
}

func severityFromStatus(status string) string {
	switch status {
	case StatusError:
		return "blocking"
	case StatusWarning:
		return "degraded"
	default:
		return "advisory"
	}
}

func buildGenericExplanation(dc DoctorCheck) string {
	s := fmt.Sprintf("%s check reported %s: %s", dc.Name, dc.Status, dc.Message)
	if dc.Detail != "" {
		s += " " + dc.Detail
	}
	if dc.Fix != "" {
		s += " To fix: " + dc.Fix
	}
	return s
}

// agentEnrichers maps check names to specialized enrichment functions.
var agentEnrichers = map[string]enricher{
	"Installation":                 enrichInstallation,
	"Permissions":                  enrichPermissions,
	"Database":                     enrichDatabase,
	"Schema Compatibility":         enrichSchemaCompatibility,
	"Database Integrity":           enrichDatabaseIntegrity,
	"Large Database":               enrichLargeDatabase,
	"ID Format":                    enrichIDFormat,
	"CLI Version":                  enrichCLIVersion,
	"Git Hooks":                    enrichGitHooks,
	"Git Hooks Dolt Compatibility": enrichGitHooksDolt,
	"Gitignore":                    enrichGitignore,
	"Project Gitignore":            enrichProjectGitignore,
	"Git Working Tree":             enrichGitWorkingTree,
	"Git Upstream":                 enrichGitUpstream,
	"Fresh Clone":                  enrichFreshClone,
	"Database Config":              enrichDatabaseConfig,
	"Config Values":                enrichConfigValues,
	"Role Configuration":           enrichBeadsRole,
	"Lock Files":                   enrichStaleLockFiles,
	"Dolt Connection":              enrichDoltConnection,
	"Dolt Schema":                  enrichDoltSchema,
	"Dolt Issue Count":             enrichDoltIssueCount,
	"Dolt Status":                  enrichDoltStatus,
	"Dependency Cycles":            enrichDependencyCycles,
	"Duplicate Issues":             enrichDuplicateIssues,
	"Test Pollution":               enrichTestPollution,
	"Orphaned Dependencies":        enrichOrphanedDeps,
	"Clone-Local FKs":              enrichCloneLocalFKs,
	"Child-Parent Dependencies":    enrichChildParentDeps,
	"Classic Artifacts":            enrichClassicArtifacts,
	"Pending Migrations":           enrichPendingMigrations,
	"KV Sync Status":               enrichKVSync,
	"Stale Closed Issues":          enrichStaleClosedIssues,
	"Stale Molecules":              enrichStaleMolecules,
	"Claude Integration":           enrichClaude,
	"Claude Settings Health":       enrichClaudeSettings,
	"Claude Hook Completeness":     enrichClaudeHooks,
	"Claude Plugin":                enrichClaudePlugin,
	"Cursor Integration":           enrichCursor,
	"Cursor Settings Health":       enrichCursorSettings,
	"Cursor Hook Completeness":     enrichCursorHooks,
	"bd prime Output":              enrichBdPrimeOutput,
	"CLI Availability":             enrichBdInPath,
	"Repo Fingerprint":             enrichRepoFingerprint,
	"Version Tracking":             enrichMetadataVersion,
	"Orphaned Issues":              enrichOrphanedIssues,
	"Redirect Target Valid":        enrichRedirectTarget,
	"Redirect Tracking":            enrichRedirectTracking,
	"Redirect Target Sync":         enrichRedirectTargetSync,
	"Untracked Files":              enrichUntrackedFiles,
}

// --- Enrichment functions ---

func enrichInstallation(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "blocking",
		explanation: "The .beads/ directory does not exist in this project. Beads cannot function without it. This is the data directory that holds the issue database, configuration, and metadata.",
		observed:    dc.Message,
		expected:    ".beads/ directory exists at project root with config.yaml and dolt/ database inside",
		commands:    []string{"bd init"},
		sourceFiles: []string{"cmd/bd/doctor/installation.go:CheckInstallation"},
	}
}

func enrichPermissions(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "blocking",
		explanation: fmt.Sprintf("File permission issue in .beads/ directory: %s. The database or directory cannot be read/written, which prevents all beads operations.", dc.Message),
		observed:    dc.Message,
		expected:    ".beads/ directory and all contents are readable and writable by current user",
		commands:    []string{"bd doctor --fix", "chmod -R u+rw .beads/"},
		sourceFiles: []string{"cmd/bd/doctor/installation.go:CheckPermissions"},
	}
}

func enrichDatabase(dc DoctorCheck) agentEnrichment {
	e := agentEnrichment{
		observed:    dc.Message,
		sourceFiles: []string{"cmd/bd/doctor/database.go:CheckDatabaseVersion", "internal/storage/dolt/migrations.go"},
	}
	if dc.Status == StatusError {
		e.severity = "blocking"
		e.explanation = fmt.Sprintf("The Dolt database is broken or missing: %s. Without a working database, no beads commands will function.", dc.Message)
		e.expected = "Dolt database opens successfully and contains bd_version metadata matching CLI version"
		e.commands = []string{"bd doctor --fix", "bd dolt status"}
	} else {
		e.severity = "degraded"
		e.explanation = fmt.Sprintf("Database version mismatch: %s. The database was last written by a different bd version. Auto-migration should resolve this.", dc.Message)
		e.expected = "Database bd_version metadata matches CLI version"
		e.commands = []string{"bd doctor --fix"}
	}
	if dc.Detail != "" {
		e.observed += "\n" + dc.Detail
	}
	return e
}

func enrichSchemaCompatibility(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "blocking",
		explanation: fmt.Sprintf("The database schema is incompatible with the current CLI version: %s. Core tables (issues, dependencies, metadata, config, wisps) must be queryable. This usually means a migration hasn't been applied.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "All core tables (issues, dependencies, metadata, config, wisps) are queryable with the current schema",
		commands:    []string{"bd migrate", "bd doctor --fix"},
		sourceFiles: []string{"cmd/bd/doctor/database.go:CheckSchemaCompatibility", "internal/storage/dolt/migrations.go"},
	}
}

func enrichDatabaseIntegrity(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "blocking",
		explanation: fmt.Sprintf("Database integrity check failed: %s. Basic queries against metadata and statistics tables failed, suggesting the database may be corrupted.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "Metadata table readable, statistics query succeeds",
		commands:    []string{"bd doctor --fix", "bd dolt status"},
		sourceFiles: []string{"cmd/bd/doctor/database.go:CheckDatabaseIntegrity"},
	}
}

func enrichLargeDatabase(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("The database has accumulated many closed issues: %s. This may cause performance degradation in list/search operations. Pruning is optional and destructive.", dc.Message),
		observed:    dc.Message,
		expected:    "Closed issue count below configured threshold",
		commands:    []string{"bd cleanup --older-than 90"},
		sourceFiles: []string{"cmd/bd/doctor/database.go:CheckDatabaseSize"},
	}
}
