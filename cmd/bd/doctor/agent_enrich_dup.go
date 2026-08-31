package doctor

import (
	"fmt"
)

func enrichTestPollution(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Test pollution detected: %s. Test-created issues are present in the production database. These are artifacts from test runs that didn't clean up properly.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "No test issues in production database",
		commands:    []string{"bd doctor --check=pollution", "bd doctor --check=pollution --clean"},
		sourceFiles: []string{"cmd/bd/doctor/validation.go:CheckTestPollution"},
	}
}

func enrichOrphanedDeps(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Orphaned dependencies found: %s. Some dependency edges reference issues that no longer exist. These are harmless but noisy.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "All dependency edges reference existing issues",
		commands:    []string{"bd doctor --check=validate --fix"},
		sourceFiles: []string{"cmd/bd/doctor/validation.go:CheckOrphanedDependencies"},
	}
}

func enrichCloneLocalFKs(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Severed clone-local FK(s): %s. A hard reset (flatten/compact squash, merge abort, migration error recovery) silently drops foreign keys from dolt_ignored tables onto the tracked plane; enforcement stays off across server restarts and orphaned rows accumulate until the constraint is re-added.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "Every FK on clone-local tables (events, wisp_dependencies, wisp_labels, wisp_comments, wisp_events, wisp_child_counters) present and enforcing",
		commands:    []string{"bd doctor --fix"},
		sourceFiles: []string{"cmd/bd/doctor/clone_local_fks.go:CheckCloneLocalFKs"},
	}
}

func enrichChildParentDeps(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Child→parent dependencies found: %s. These are an anti-pattern where a child issue depends on its own parent. The parent-child relationship already implies ordering.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "No dependency edges between parent and child issues",
		commands:    []string{"bd doctor --fix --fix-child-parent"},
		sourceFiles: []string{"cmd/bd/doctor/validation.go:CheckChildParentDependencies"},
	}
}

func enrichClassicArtifacts(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Classic (pre-Dolt) artifacts found: %s. These are leftover files from the SQLite/JSONL era that are no longer needed after Dolt migration.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "No classic artifacts present (JSONL files, SQLite database, cruft directories)",
		commands:    []string{"bd doctor --check=artifacts", "bd doctor --check=artifacts --clean"},
		sourceFiles: []string{"cmd/bd/doctor/artifacts.go:CheckClassicArtifacts"},
	}
}

func enrichPendingMigrations(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    severityFromStatus(dc.Status),
		explanation: fmt.Sprintf("Pending migrations: %s. Database schema migrations are available but haven't been applied. Some may be required for correct operation.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "All available migrations have been applied",
		commands:    []string{"bd migrate"},
		sourceFiles: []string{"cmd/bd/doctor/migration.go:CheckPendingMigrations", "internal/storage/dolt/migrations.go"},
	}
}

func enrichKVSync(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("KV store sync status: %s. The key-value store may be out of sync with the main database.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "KV store is in sync with main database",
		commands:    []string{"bd dolt push"},
		sourceFiles: []string{"cmd/bd/doctor/kv.go:CheckKVSyncStatus"},
	}
}

func enrichStaleClosedIssues(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Stale closed issues: %s. Old closed issues can be pruned to reduce database size and improve query performance.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "Closed issues are within acceptable age/count thresholds",
		commands:    []string{"bd cleanup --older-than 90"},
		sourceFiles: []string{"cmd/bd/doctor/maintenance.go:CheckStaleClosedIssues"},
	}
}

func enrichStaleMolecules(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Stale molecules detected: %s. These are molecules (multi-step workflows) where all steps are complete but the molecule itself was never closed.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "Completed molecules are closed",
		commands:    []string{"bd mol list", "bd close <molecule-id>"},
		sourceFiles: []string{"cmd/bd/doctor/maintenance.go:CheckStaleMolecules"},
	}
}

func enrichClaude(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Claude integration: %s. Beads integrates with Claude Code via SessionStart hooks defined in .claude/settings.json.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "Claude Code hooks configured for beads (bd prime on SessionStart)",
		commands:    []string{"bd hooks install"},
		sourceFiles: []string{"cmd/bd/doctor/claude.go:CheckClaude"},
	}
}

func enrichClaudeSettings(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "blocking",
		explanation: fmt.Sprintf("Claude settings file is malformed: %s. A corrupted .claude/settings.json prevents Claude Code from loading hooks, which breaks beads integration entirely.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    ".claude/settings.json is valid JSON",
		commands:    []string{"cat .claude/settings.json | python3 -m json.tool", "bd hooks install"},
		sourceFiles: []string{"cmd/bd/doctor/claude.go:CheckClaudeSettingsHealth"},
	}
}

func enrichClaudeHooks(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Claude hook completeness: %s. Beads needs a SessionStart hook to inject context in Claude Code sessions, including after compaction.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "SessionStart hook configured for beads",
		commands:    []string{"bd hooks install"},
		sourceFiles: []string{"cmd/bd/doctor/claude.go:CheckClaudeHookCompleteness"},
	}
}

func enrichClaudePlugin(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Claude plugin version: %s. An outdated Claude plugin may not support current beads features.", dc.Message),
		observed:    dc.Message,
		expected:    "Claude plugin version matches CLI version",
		commands:    []string{"bd hooks install"},
		sourceFiles: []string{"cmd/bd/doctor/claude.go:CheckClaudePlugin"},
	}
}

func enrichCursor(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Cursor integration: %s. Beads integrates with Cursor via agent hooks in .cursor/hooks.json (or ~/.cursor/hooks.json) that run 'bd cursor-hook' to inject bd prime on sessionStart and recover context after compaction.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "Cursor hooks configured for beads (bd cursor-hook on sessionStart/preCompact/postToolUse)",
		commands:    []string{"bd setup cursor"},
		sourceFiles: []string{"cmd/bd/doctor/cursor.go:CheckCursor"},
	}
}

func enrichCursorSettings(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "blocking",
		explanation: fmt.Sprintf("Cursor hooks file is malformed: %s. A corrupted .cursor/hooks.json prevents Cursor from loading any hooks, which silently breaks beads context injection and post-compaction recovery.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    ".cursor/hooks.json (and ~/.cursor/hooks.json) are valid JSON",
		commands:    []string{"cat .cursor/hooks.json | python3 -m json.tool", "bd setup cursor"},
		sourceFiles: []string{"cmd/bd/doctor/cursor.go:CheckCursorSettingsHealth"},
	}
}

func enrichCursorHooks(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Cursor hook completeness: %s. Beads recovers context across compaction in Cursor using three hooks: sessionStart, preCompact, and postToolUse. A partial install degrades recovery.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "sessionStart, preCompact, and postToolUse hooks all configured for beads",
		commands:    []string{"bd setup cursor"},
		sourceFiles: []string{"cmd/bd/doctor/cursor.go:CheckCursorHookCompleteness"},
	}
}

func enrichBdPrimeOutput(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("bd prime output issue: %s. The bd prime command's output may not match expected format, which can confuse agent context loading.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "bd prime produces valid context output",
		sourceFiles: []string{"cmd/bd/doctor/claude.go:VerifyPrimeOutput"},
	}
}

func enrichBdInPath(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "degraded",
		explanation: fmt.Sprintf("bd not in PATH: %s. Claude Code hooks invoke bd commands, but bd is not found in the system PATH. Hooks will fail silently.", dc.Message),
		observed:    dc.Message,
		expected:    "'bd' executable is in PATH and runnable",
		commands:    []string{"which bd", installScriptCommand},
		sourceFiles: []string{"cmd/bd/doctor/claude.go:CheckBdInPath"},
	}
}

func enrichRepoFingerprint(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    severityFromStatus(dc.Status),
		explanation: fmt.Sprintf("Repo fingerprint mismatch: %s. The database may belong to a different repository (e.g. copied from another project or remote URL changed).", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "Database fingerprint matches current git remote URL",
		commands:    []string{"bd doctor --fix"},
		sourceFiles: []string{"cmd/bd/doctor/integrity.go:CheckRepoFingerprint"},
	}
}

func enrichMetadataVersion(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Metadata version tracking: %s. The metadata.json LastBdVersion field tracks which bd version last wrote to this database.", dc.Message),
		observed:    dc.Message,
		expected:    "metadata.json LastBdVersion matches current CLI version",
		commands:    []string{"bd doctor --fix"},
		sourceFiles: []string{"cmd/bd/doctor/version.go:CheckMetadataVersionTracking"},
	}
}

func enrichOrphanedIssues(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Orphaned issues: %s. These issues are referenced in git commits (e.g. 'fix(bd-xyz): ...') but are still open. They may have been accidentally left open after the fix was committed.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "Issues referenced in commits are closed",
		commands:    []string{"bd orphans", "bd close <issue-id>"},
		sourceFiles: []string{"cmd/bd/doctor/git.go:CheckOrphanedIssues"},
	}
}

func enrichRedirectTarget(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "degraded",
		explanation: fmt.Sprintf("Redirect target issue: %s. The .beads/redirect file points to an external directory that is missing or has an invalid database. This is used in worktree setups where multiple worktrees share one database.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "Redirect target exists and contains a valid beads database",
		commands:    []string{"cat .beads/redirect", "bd doctor --fix"},
		sourceFiles: []string{"cmd/bd/doctor/gitignore.go:CheckRedirectTargetValid"},
	}
}

func enrichRedirectTracking(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Redirect file tracking: %s. The .beads/redirect file contains an absolute local path and should not be committed to git (it's machine-specific).", dc.Message),
		observed:    dc.Message,
		expected:    ".beads/redirect is in .gitignore and not tracked by git",
		commands:    []string{"echo '.beads/redirect' >> .beads/.gitignore", "git rm --cached .beads/redirect"},
		sourceFiles: []string{"cmd/bd/doctor/gitignore.go:CheckRedirectNotTracked"},
	}
}

func enrichRedirectTargetSync(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Redirect target sync worktree: %s. The redirect target directory is missing a beads-sync worktree, which is needed for git-based synchronization of the shared database.", dc.Message),
		observed:    dc.Message + "\n" + dc.Detail,
		expected:    "Redirect target has a beads-sync worktree for database synchronization",
		commands:    []string{"bd init"},
		sourceFiles: []string{"cmd/bd/doctor/gitignore.go:CheckRedirectTargetSyncWorktree"},
	}
}

func enrichUntrackedFiles(dc DoctorCheck) agentEnrichment {
	return agentEnrichment{
		severity:    "advisory",
		explanation: fmt.Sprintf("Untracked beads files: %s. Legacy data files in .beads/ are not tracked by git. In direct-commit mode, these should be committed to propagate changes. (Dolt backends store data on the server and do not need tracked files.)", dc.Message),
		observed:    dc.Message,
		expected:    "All legacy .beads/ data files are tracked by git (or using Dolt/sync-branch mode)",
		commands:    []string{"git add .beads/*.jsonl && git commit -m 'sync beads data'"},
		sourceFiles: []string{"cmd/bd/doctor/installation.go:CheckUntrackedBeadsFiles"},
	}
}
