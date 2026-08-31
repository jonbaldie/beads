package doctor

import (
	"fmt"
	"strings"
)

func enrichIDFormat(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "degraded",
		explanation: fmt.Sprintf("ID format issue: %s. Beads uses hash-based IDs (e.g. bd-a1b2c). Sequential numeric IDs indicate a very old database that should be migrated.", dc.Message),
		observed:    dc.Message,
		expected:    "All issues use hash-based IDs with configured prefix",
		commands:    []string{"bd doctor --fix"},
		sourceFiles: []string{"cmd/bd/doctor/integrity.go:CheckIDFormat"},
	}
}

func enrichCLIVersion(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("CLI version check: %s. An outdated CLI may lack bug fixes or schema migrations needed by the current database.", dc.Message),
		observed:    dc.Message,
		expected:    "CLI version matches latest GitHub release",
		commands:    []string{installScriptCommand},
		sourceFiles: []string{"cmd/bd/doctor/version.go:CheckCLIVersion"},
	}
}

func enrichGitHooks(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "degraded",
		explanation: fmt.Sprintf("Git hooks issue: %s. Beads git hooks refresh JSONL exports and run legacy fallback checks, while cross-clone sync uses Dolt remotes via bd dolt push/pull.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "Git hooks installed and version matches CLI version",
		commands:    []string{"bd hooks install"},
		sourceFiles: []string{"cmd/bd/doctor/git.go:CheckGitHooks", "cmd/bd/doctor/fix/hooks.go"},
	}
}

func enrichGitHooksDolt(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "blocking",
		explanation: fmt.Sprintf("Git hooks are incompatible with Dolt backend: %s. Hooks that predate the Dolt migration may run SQLite operations on a Dolt database, causing errors on every git operation.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "Git hooks contain Dolt-compatible commands",
		commands:    []string{"bd hooks install"},
		sourceFiles: []string{"cmd/bd/doctor/git.go:CheckGitHooksDoltCompatibility"},
	}
}

func enrichGitignore(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "degraded",
		explanation: fmt.Sprintf("Gitignore issue: %s. The .beads/.gitignore must exclude runtime files (dolt/, locks, temp files) while tracking config.yaml and metadata.json.", dc.Message),
		observed:    dc.Message,
		expected:    ".beads/.gitignore is up to date with current patterns",
		commands:    []string{"bd doctor --fix"},
		sourceFiles: []string{"cmd/bd/doctor/gitignore.go:CheckGitignore"},
	}
}

func enrichProjectGitignore(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "degraded",
		explanation: fmt.Sprintf("Project .gitignore issue: %s. The project-root .gitignore needs Dolt exclusion patterns to prevent committing large binary database files.", dc.Message),
		observed:    dc.Message,
		expected:    "Project .gitignore contains .beads/dolt/ exclusion pattern",
		commands:    []string{"bd doctor --fix"},
		sourceFiles: []string{"cmd/bd/doctor/gitignore.go:CheckProjectGitignore"},
	}
}

func enrichGitWorkingTree(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Git working tree is dirty: %s. Uncommitted changes in .beads/ files may indicate interrupted operations or manual edits.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "Git working tree clean (no uncommitted .beads/ changes)",
		commands:    []string{"git status .beads/", "git add .beads/ && git commit -m 'sync beads state'"},
		sourceFiles: []string{"cmd/bd/doctor/git.go:CheckGitWorkingTree"},
	}
}

func enrichGitUpstream(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Git upstream drift: %s. Local branch is out of sync with remote. This may mean beads changes from other clones haven't been pulled.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "Local branch is in sync with upstream tracking branch",
		commands:    []string{"git pull --rebase"},
		sourceFiles: []string{"cmd/bd/doctor/git.go:CheckGitUpstream"},
	}
}

func enrichFreshClone(dc DoctorCheck) agentEnrichment {
	commands := []string{"bd bootstrap"}
	explanation := fmt.Sprintf("Fresh clone detected: %s. The .beads/ directory exists (committed to git) but the local database has not been recovered yet. Run bd bootstrap as the safe existing-project recovery entry point.", dc.Message)

	// When the message mentions sync.remote, include it in the suggested commands.
	if strings.Contains(dc.Message, "sync.remote") {
		explanation = fmt.Sprintf("Fresh clone detected: %s. The .beads/ directory exists (committed to git) but the database is not found on the configured server. Run bd bootstrap as the safe entry point; if bootstrap cannot find the expected remote automatically, then set sync.remote in .beads/config.yaml and rerun bd bootstrap.", dc.Message)
		commands = []string{"bd bootstrap", "Set sync.remote in .beads/config.yaml if bootstrap cannot find the expected remote"}
	}

	return agentEnrichment{
		severity:    "blocking",
		explanation: explanation,
		observed:    dc.Message,
		expected:    "Local database initialized and ready for use",
		commands:    commands,
		sourceFiles: []string{"cmd/bd/doctor/legacy.go:CheckFreshClone"},
	}
}

func enrichDatabaseConfig(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "degraded",
		explanation: fmt.Sprintf("Database configuration mismatch: %s. The config.yaml backend setting doesn't match what's actually on disk. This can happen after a migration or manual file manipulation.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "config.yaml backend setting matches actual database on disk",
		commands:    []string{"bd doctor --fix"},
		sourceFiles: []string{"cmd/bd/doctor/legacy.go:CheckDatabaseConfig", "internal/configfile/config.go"},
	}
}

func enrichConfigValues(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Configuration value issue: %s. One or more config values are invalid, unknown, or using deprecated keys.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "All config values are valid and recognized",
		commands:    []string{"bd config list", "bd doctor --fix"},
		sourceFiles: []string{"cmd/bd/doctor/config_values.go:CheckConfigValues"},
	}
}

func enrichBeadsRole(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Beads role configuration: %s. The beads.role config determines how beads behaves (e.g. 'maintainer' vs 'contributor'). Missing role config falls back to URL heuristic detection.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "beads.role is configured in config.yaml",
		commands:    []string{"bd config set beads.role maintainer"},
		sourceFiles: []string{"cmd/bd/doctor/role.go:CheckBeadsRole"},
	}
}

func enrichStaleLockFiles(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "degraded",
		explanation: fmt.Sprintf("Stale lock files detected: %s. Lock files from crashed or killed bd processes prevent new operations. They are safe to remove if no bd process is currently running.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "No stale lock files in .beads/",
		commands:    []string{"bd doctor --fix"},
		sourceFiles: []string{"cmd/bd/doctor/locks.go:CheckStaleLockFiles"},
	}
}

func enrichDoltConnection(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "blocking",
		explanation: fmt.Sprintf("Cannot connect to Dolt database: %s. Either the embedded Dolt engine failed to start, or the Dolt server (if using server mode) is unreachable.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "Dolt database opens successfully (embedded) or server is reachable (server mode)",
		commands:    []string{"bd doctor --fix", "gt dolt status", "gt dolt start"},
		sourceFiles: []string{"cmd/bd/doctor/dolt.go:CheckDoltConnection"},
	}
}

func enrichDoltSchema(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "blocking",
		explanation: fmt.Sprintf("Dolt schema issue: %s. Required tables or columns are missing from the Dolt database. This usually means a migration is needed.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "All required tables and columns present in Dolt database",
		commands:    []string{"bd migrate", "bd doctor --fix"},
		sourceFiles: []string{"cmd/bd/doctor/dolt.go:CheckDoltSchema", "internal/storage/dolt/migrations.go"},
	}
}

func enrichDoltIssueCount(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Dolt issue count: %s. Reports the number of issues in the Dolt database for basic sanity checking.", dc.Message),
		observed:    dc.Message,
		expected:    "Issue count is non-zero (unless this is a new/empty database)",
		sourceFiles: []string{"cmd/bd/doctor/dolt.go:CheckDoltIssueCount"},
	}
}

func enrichDoltStatus(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    severityFromStatus(dc.Status),
		explanation: fmt.Sprintf("Dolt database status: %s. Reports uncommitted changes, dirty working set, or other Dolt-specific state issues.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "Dolt working set is clean (no uncommitted changes)",
		commands:    []string{"bd dolt commit"},
		sourceFiles: []string{"cmd/bd/doctor/dolt.go:CheckDoltStatus"},
	}
}

func enrichDependencyCycles(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "degraded",
		explanation: fmt.Sprintf("Dependency cycle detected: %s. Circular dependencies prevent topological sorting and can confuse priority calculations. The cycle must be broken by removing one dependency edge.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "Dependency graph is a DAG (no cycles)",
		commands:    []string{"bd validate", "bd dep rm <issue-a> <issue-b>"},
		sourceFiles: []string{"cmd/bd/doctor/integrity.go:CheckDependencyCycles"},
	}
}

func enrichDuplicateIssues(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Duplicate issues detected: %s. Multiple issues share the same title, which may indicate accidental creation. Review and close duplicates.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "No duplicate issue titles",
		commands:    []string{"bd validate", "bd close <duplicate-id>"},
		sourceFiles: []string{"cmd/bd/doctor/validation.go:CheckDuplicateIssues"},
	}
}
