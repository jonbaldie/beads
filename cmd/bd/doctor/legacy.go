package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
)

// agentDocFiles returns the list of documentation files to check, including
// the configured agents file (which may differ from the default AGENTS.md).
func agentDocFiles(repoPath string) []string {
	agentsFile := config.SafeAgentsFile()
	files := []string{
		filepath.Join(repoPath, agentsFile),
		filepath.Join(repoPath, "CLAUDE.md"),
		filepath.Join(repoPath, ".github", "copilot-instructions.md"),
		filepath.Join(repoPath, ".claude", "CLAUDE.md"),
		// Local-only variants (not committed to repo)
		filepath.Join(repoPath, "claude.local.md"),
		filepath.Join(repoPath, ".claude", "claude.local.md"),
	}
	// If the configured file isn't the default, also check the default
	// to catch legacy files that haven't been migrated.
	if !strings.EqualFold(agentsFile, config.DefaultAgentsFile) {
		files = append(files, filepath.Join(repoPath, config.DefaultAgentsFile))
	}
	return files
}

// CheckLegacyBeadsSlashCommands detects old /beads:* slash commands in documentation
// and recommends migration to bd prime hooks for better token efficiency.
//
// Old pattern: /beads:quickstart, /beads:ready (~10.5k tokens per session)
// New pattern: bd prime hooks (~50-2k tokens per session)
func CheckLegacyBeadsSlashCommands(repoPath string) DoctorCheck {
	docFiles := agentDocFiles(repoPath)

	var filesWithLegacyCommands []string
	legacyPattern := "/beads:"

	for _, docFile := range docFiles {
		content, err := os.ReadFile(docFile) // #nosec G304 - controlled paths from repoPath
		if err != nil {
			continue // File doesn't exist or can't be read
		}

		if strings.Contains(string(content), legacyPattern) {
			filesWithLegacyCommands = append(filesWithLegacyCommands, filepath.Base(docFile))
		}
	}

	if len(filesWithLegacyCommands) == 0 {
		return DoctorCheck{
			Name:    "Legacy Commands",
			Status:  StatusOK,
			Message: "No legacy beads slash commands detected",
		}
	}

	return DoctorCheck{
		Name:    "Legacy Commands",
		Status:  StatusWarning,
		Message: fmt.Sprintf("Old beads integration detected in %s", strings.Join(filesWithLegacyCommands, ", ")),
		Detail: "Found: /beads:* slash command references (deprecated)\n" +
			"  These commands are token-inefficient (~10.5k tokens per session)",
		Fix: "Migrate to bd prime hooks for better token efficiency:\n" +
			"\n" +
			"Migration Steps:\n" +
			"  1. Run 'bd setup claude' to add SessionStart hooks\n" +
			"  2. Update " + config.AgentsFile() + "/CLAUDE.md:\n" +
			"     - Remove /beads:* slash command references\n" +
			"     - Add: \"Run 'bd prime' for workflow context\" (for users without hooks)\n" +
			"\n" +
			"Benefits:\n" +
			"  • MCP mode: ~50 tokens vs ~10.5k for full MCP scan (99% reduction)\n" +
			"  • CLI mode: ~1-2k tokens with automatic context recovery\n" +
			"  • Hooks auto-refresh context on session start and before compaction\n" +
			"\n" +
			"See: bd setup claude --help",
	}
}

// CheckLegacyMCPToolReferences detects direct MCP tool name references in documentation
// (e.g., mcp__beads_beads__list, mcp__plugin_beads_beads__show) and recommends
// migration to bd prime hooks for better token efficiency.
//
// Old pattern: Document MCP tool names for direct tool calls (~10.5k tokens per scan)
// New pattern: bd prime hooks with CLI commands (~50-2k tokens)
func CheckLegacyMCPToolReferences(repoPath string) DoctorCheck {
	docFiles := agentDocFiles(repoPath)

	mcpPatterns := []string{
		"mcp__beads_beads__",
		"mcp__plugin_beads_beads__",
		"mcp_beads_",
	}

	var filesWithMCPRefs []string
	for _, docFile := range docFiles {
		content, err := os.ReadFile(docFile) // #nosec G304 - controlled paths from repoPath
		if err != nil {
			continue
		}

		contentStr := string(content)
		for _, pattern := range mcpPatterns {
			if strings.Contains(contentStr, pattern) {
				filesWithMCPRefs = append(filesWithMCPRefs, filepath.Base(docFile))
				break
			}
		}
	}

	if len(filesWithMCPRefs) == 0 {
		return DoctorCheck{
			Name:    "MCP Tool References",
			Status:  StatusOK,
			Message: "No MCP tool references in documentation",
		}
	}

	return DoctorCheck{
		Name:    "MCP Tool References",
		Status:  StatusWarning,
		Message: fmt.Sprintf("MCP tool references found in %s", strings.Join(filesWithMCPRefs, ", ")),
		Detail: "Found: Direct MCP tool name references (e.g., mcp__beads_beads__list)\n" +
			"  MCP tool calls consume ~10.5k tokens per session for tool scanning",
		Fix: "Migrate to bd prime hooks for better token efficiency:\n" +
			"\n" +
			"Migration Steps:\n" +
			"  1. Run 'bd setup claude' to add SessionStart hooks\n" +
			"  2. Replace MCP tool references with CLI commands:\n" +
			"     - mcp__beads_beads__list  → bd list\n" +
			"     - mcp__beads_beads__show  → bd show <id>\n" +
			"     - mcp__beads_beads__ready → bd ready\n" +
			"  3. bd prime hooks auto-inject context on session start\n" +
			"\n" +
			"Benefits:\n" +
			"  • bd prime + hooks: ~50-2k tokens vs ~10.5k for MCP tool scan\n" +
			"  • Automatic context recovery on session start and compaction\n" +
			"\n" +
			"See: bd setup claude --help",
	}
}

// CheckAgentDocumentation checks if agent documentation (AGENTS.md or CLAUDE.md) exists
// and recommends adding it if missing, suggesting bd onboard or bd setup claude.
// Also supports local-only variants (claude.local.md) that are gitignored.
func CheckAgentDocumentation(repoPath string) DoctorCheck {
	docFiles := agentDocFiles(repoPath)

	var foundDocs []string
	for _, docFile := range docFiles {
		if _, err := os.Stat(docFile); err == nil {
			foundDocs = append(foundDocs, filepath.Base(docFile))
		}
	}

	if len(foundDocs) > 0 {
		return DoctorCheck{
			Name:    "Agent Documentation",
			Status:  StatusOK,
			Message: fmt.Sprintf("Documentation found: %s", strings.Join(foundDocs, ", ")),
		}
	}

	return DoctorCheck{
		Name:    "Agent Documentation",
		Status:  StatusWarning,
		Message: "No agent documentation found",
		Detail: "Missing: " + config.AgentsFile() + " or CLAUDE.md\n" +
			"  Documenting workflow helps AI agents work more effectively",
		Fix: "Add agent documentation:\n" +
			"  • Run 'bd onboard' to create " + config.AgentsFile() + " with workflow guidance\n" +
			"  • Or run 'bd setup claude' to add Claude-specific documentation\n" +
			"\n" +
			"For local-only documentation (not committed to repo):\n" +
			"  • Create claude.local.md or .claude/claude.local.md\n" +
			"  • Add 'claude.local.md' to your .gitignore\n" +
			"\n" +
			"Recommended: Include bd workflow in your project documentation so\n" +
			"AI agents understand how to track issues and manage dependencies",
	}
}

// stripBeadsIntegrationSection removes the <!-- BEGIN BEADS INTEGRATION ... -->
// ... <!-- END BEADS INTEGRATION --> block (plus an optional trailing newline)
// so the remaining content represents user-authored material. Returns the input
// unchanged if no well-formed marker pair is found.
func stripBeadsIntegrationSection(content string) string {
	const beginPrefix = "<!-- BEGIN BEADS INTEGRATION"
	const endMarker = "<!-- END BEADS INTEGRATION -->"

	beginIdx := strings.Index(content, beginPrefix)
	if beginIdx == -1 {
		return content
	}
	endIdx := strings.Index(content, endMarker)
	if endIdx == -1 || endIdx < beginIdx {
		return content
	}
	end := endIdx + len(endMarker)
	if end < len(content) && content[end] == '\n' {
		end++
	}
	return content[:beginIdx] + content[end:]
}

// normalizeUserAuthored canonicalizes content for divergence comparison:
// strips the managed beads section, normalizes line endings, and collapses
// trailing whitespace. Leading/trailing blank lines are removed so cosmetic
// reformatting (e.g. an extra newline at EOF) does not flag as divergence.
func normalizeUserAuthored(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = stripBeadsIntegrationSection(content)
	lines := strings.Split(content, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// sameInode reports whether two files refer to the same underlying inode
// (hard-linked or one is a symlink resolving to the other). Falls back to
// comparing resolved paths on platforms where syscall stat is unavailable.
func sameInode(a, b string) (bool, error) {
	resolvedA, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false, err
	}
	resolvedB, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false, err
	}
	if resolvedA == resolvedB {
		return true, nil
	}
	infoA, err := os.Stat(resolvedA)
	if err != nil {
		return false, err
	}
	infoB, err := os.Stat(resolvedB)
	if err != nil {
		return false, err
	}
	return os.SameFile(infoA, infoB), nil
}
