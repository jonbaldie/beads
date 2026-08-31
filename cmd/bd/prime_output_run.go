package main

import (
	"fmt"
	"io"

	"github.com/jonbaldie/beads/internal/config"
)

// outputMCPContext outputs minimal context for MCP users
func outputMCPContext(w io.Writer, stealthMode bool) error {
	return outputMCPContextWithOptions(w, stealthMode, primeOptions{})
}

func outputMCPContextWithOptions(w io.Writer, stealthMode bool, opts primeOptions) error {
	ephemeral := isEphemeralBranch()
	noPush := primeNoPushConfigured()
	// localOnly reflects only the git-remote axis (drives git push/pull
	// hints and remote-sync authority wording). Dolt sync-remote presence
	// (doltSync, below) is a separate axis that only gates the literal
	// `bd dolt push`/`bd dolt pull` hint lines (gh#4130) — the two must not
	// be conflated (gh#4230 review).
	localOnly := !primeHasGitRemote()
	doltSync := primeHasSyncRemote()

	closeProtocol, profileRule := primeMCPPolicy(stealthMode, localOnly, ephemeral, noPush, doltSync)

	redirectNotice := getRedirectNotice(false)
	var memories string
	if !opts.noMemories {
		memories = formatMemoriesForPrimeWithOptions(true, opts)
	}

	context := primeTruncationDirective + `# Beads Issue Tracker Active

` + redirectNotice
	if memories != "" {
		context += memories + "\n"
	}

	context += `# 🚨 SESSION CLOSE PROTOCOL 🚨

` + closeProtocol + `

## Core Rules
- **Default**: Use beads for ALL task tracking (` + "`bd create`" + `, ` + "`bd ready`" + `, ` + "`bd close`" + `)
- **Prohibited**: Do NOT use TodoWrite, TaskCreate, or markdown files for task tracking
- **Workflow**: Create beads issue BEFORE writing code, mark in_progress when starting
- **Memory**: Use ` + "`bd remember`" + ` for persistent knowledge. Do NOT use MEMORY.md files.
- Persistence you don't need beats lost context
- ` + profileRule + `

Start: Check ` + "`ready`" + ` tool for available work.
`
	_, _ = fmt.Fprint(w, context)

	return nil
}

func primeMCPPolicy(stealthMode, localOnly, ephemeral, noPush, doltSync bool) (string, string) {
	if stealthMode {
		return "Before saying \"done\": bd close <completed-ids>", "Git authority: no git operations in this context"
	}
	if localOnly {
		return primeMCPRemotePolicy()
	}
	if ephemeral {
		return "Before saying \"done\": bd close <completed-ids>; run checks; report git status and proposed handoff (no push - ephemeral branch)", "Profile model: conservative by default; commit only with explicit user/orchestrator authority"
	}
	if noPush {
		return "Before saying \"done\": bd close <completed-ids>; run checks; report git status and proposed handoff (push disabled)", "Profile model: conservative by default; push only with explicit user/orchestrator authority"
	}
	if primeAgentProfile() == config.ProfileTeamMaintainer {
		return primeMCPTeamMaintainerPolicy(doltSync)
	}
	return "Before saying \"done\": bd close <completed-ids>; run checks. Then follow the active profile — conservative reports handoff; team-maintainer may commit/sync/push when explicitly enabled.", "Default: do not commit, push, or run dolt remote sync without explicit authority. Team-maintainer behavior is opt-in and still subordinate to user/orchestrator instructions."
}

func primeMCPRemotePolicy() (string, string) {
	if primeAgentProfile() == config.ProfileTeamMaintainer {
		return "Before saying \"done\": bd close <completed-ids>; run checks; run git status and commit local changes as routine work (agent.profile=team-maintainer); do not push, pull, or run remote sync.", "Git authority: local-only/no-remote. No git remote configured. Profile: team-maintainer active (agent.profile=team-maintainer) - local commits are routine; do not push, pull, or run remote sync. Explicit no-commit instructions still override."
	}
	return "Before saying \"done\": bd close <completed-ids>; run checks; report git status and proposed handoff (local-only/no remote sync)", "Git authority: local-only/no-remote. No git remote configured. Do not push, pull, or run remote sync. Local git operations follow active user, orchestrator, and repository authority."
}

func primeMCPTeamMaintainerPolicy(doltSync bool) (string, string) {
	closeProtocol := "Before saying \"done\": bd close <completed-ids>; run checks; commit and git push as part of routine work (agent.profile=team-maintainer), unless current instructions say otherwise."
	if doltSync {
		closeProtocol = "Before saying \"done\": bd close <completed-ids>; run checks; commit, bd dolt push, and git push as part of routine work (agent.profile=team-maintainer), unless current instructions say otherwise."
	}
	return closeProtocol, "Profile: team-maintainer active (agent.profile=team-maintainer) - commit, sync, and push are routine; explicit no-commit/no-push instructions still override."
}

type primeCLIContextParts struct {
	closeProtocol      string
	closeNote          string
	syncSection        string
	completingWorkflow string
	gitWorkflowRule    string
	profileRule        string
}

func primeCLIContextPartsFor(stealthMode, localOnly, ephemeral, noPush, doltSync bool) primeCLIContextParts {
	if stealthMode {
		return primeCLIStealthParts()
	}
	if localOnly {
		return primeCLILocalOnlyParts()
	}
	if ephemeral {
		return primeCLIEphemeralParts(doltSync)
	}
	if noPush {
		return primeCLINoPushParts(doltSync)
	}
	if primeAgentProfile() == config.ProfileTeamMaintainer {
		return primeCLITeamMaintainerParts(doltSync)
	}
	return primeCLIConservativeParts(doltSync)
}

func primeCLISyncSection(doltSync, pushFirst bool) string {
	return "### Sync & Collaboration\n" +
		primeDoltSyncBullets(doltSync, pushFirst) +
		"- `bd search <query>` - Search issues by keyword"
}

func primeCLIStealthParts() primeCLIContextParts {
	return primeCLIContextParts{
		closeProtocol: `[ ] bd close <id1> <id2> ...   (close completed issues)`,
		syncSection:   primeCLISyncSection(false, false),
		completingWorkflow: `**Completing work:**
` + "```bash" + `
bd close <id1> <id2> ...    # Close all completed issues at once
` + "```",
		gitWorkflowRule: "Git workflow: stealth mode (no git ops)",
		profileRule:     "Git authority: no git operations in this context",
	}
}

func primeCLILocalOnlyParts() primeCLIContextParts {
	if primeAgentProfile() == config.ProfileTeamMaintainer {
		return primeCLILocalTeamMaintainerParts()
	}
	return primeCLILocalConservativeParts()
}

func primeCLILocalTeamMaintainerParts() primeCLIContextParts {
	return primeCLIContextParts{
		closeProtocol: `[ ] 1. bd close <id1> <id2> ...   (close completed issues)
[ ] 2. run quality gates        (tests, linters, builds when relevant)
[ ] 3. git status               (check what changed)
[ ] 4. team-maintainer: commit local changes; do not push or run remote sync`,
		closeNote:   "**Note:** No git remote configured. Do not push, pull, or run remote sync. Local git operations follow active user, orchestrator, and repository authority.",
		syncSection: primeCLISyncSection(false, false),
		completingWorkflow: `**Completing work:**
` + "```bash" + `
bd close <id1> <id2> ...    # Close all completed issues at once
git status                  # Check changed files
git add <files> && git commit -m "..."
# Local-only/no-remote: do not push, pull, or run remote sync
` + "```",
		gitWorkflowRule: "Git workflow: local-only/no-remote; team-maintainer commits locally but does not push or run remote sync",
		profileRule:     "Git authority: local-only/no-remote. Profile: team-maintainer active (agent.profile=team-maintainer) - local commits are routine; explicit no-commit instructions still override.",
	}
}

func primeCLILocalConservativeParts() primeCLIContextParts {
	return primeCLIContextParts{
		closeProtocol: `[ ] 1. bd close <id1> <id2> ...   (close completed issues)
[ ] 2. run quality gates        (tests, linters, builds when relevant)
[ ] 3. git status               (check what changed)
[ ] 4. report handoff           (local-only/no remote sync; wait for authority)`,
		closeNote:   "**Note:** No git remote configured. Do not push, pull, or run remote sync. Local git operations follow active user, orchestrator, and repository authority.",
		syncSection: primeCLISyncSection(false, false),
		completingWorkflow: `**Completing work:**
` + "```bash" + `
bd close <id1> <id2> ...    # Close all completed issues at once
git status                  # Report changed files and proposed commands
# Local-only/no-remote: do not push, pull, or run remote sync
` + "```",
		gitWorkflowRule: "Git workflow: local-only/no-remote; no push, pull, or remote sync",
		profileRule:     "Git authority: local-only/no-remote. Local git operations follow active user, orchestrator, and repository authority.",
	}
}

func primeCLIEphemeralParts(doltSync bool) primeCLIContextParts {
	doltPullStep := ""
	if doltSync {
		doltPullStep = "bd dolt pull                # Pull latest beads from main\n"
	}
	return primeCLIContextParts{
		closeProtocol: `[ ] 1. bd close <id1> <id2> ...   (close completed issues)
[ ] 2. run quality gates        (tests, linters, builds when relevant)
[ ] 3. git status               (check what changed)
[ ] 4. report handoff           (changed files, validation, proposed commit if authorized)`,
		closeNote:   "**Note:** This is an ephemeral branch (no upstream). Do not push it unless the user or orchestrator explicitly says to.",
		syncSection: primeCLISyncSection(doltSync, false),
		completingWorkflow: `**Completing work:**
` + "```bash" + `
bd close <id1> <id2> ...    # Close all completed issues at once
` + doltPullStep + `git status                  # Report changed files and proposed commit; wait for authority
# Merge to main locally only when the active instructions grant that authority
` + "```",
		gitWorkflowRule: "Git workflow: conservative by default on ephemeral branches",
		profileRule:     "Profile model: conservative/minimal report handoff; team-maintainer may commit only when explicitly enabled",
	}
}

func primeCLINoPushParts(doltSync bool) primeCLIContextParts {
	return primeCLIContextParts{
		closeProtocol: `[ ] 1. bd close <id1> <id2> ...   (close completed issues)
[ ] 2. run quality gates        (tests, linters, builds when relevant)
[ ] 3. git status               (check what changed)
[ ] 4. report handoff           (push disabled; wait for explicit authority)`,
		closeNote:   "**Note:** Push disabled via config. Do not push unless the user or orchestrator explicitly says to.",
		syncSection: primeCLISyncSection(doltSync, true),
		completingWorkflow: `**Completing work:**
` + "```bash" + `
bd close <id1> <id2> ...    # Close all completed issues at once
git status                  # Report changed files and proposed commands
# Do not push unless current instructions explicitly allow it
` + "```",
		gitWorkflowRule: "Git workflow: push disabled; report handoff unless explicitly authorized",
		profileRule:     "Profile model: conservative/minimal report handoff; team-maintainer still respects no-push/user instructions",
	}
}

func primeCLITeamMaintainerParts(doltSync bool) primeCLIContextParts {
	doltPushStep := ""
	if doltSync {
		doltPushStep = "bd dolt push\n"
	}
	return primeCLIContextParts{
		closeProtocol: `[ ] 1. bd close <id1> <id2> ...   (close completed issues)
[ ] 2. run quality gates        (tests, linters, builds when relevant)
[ ] 3. git status               (check what changed)
[ ] 4. team-maintainer: commit, sync, push as part of routine work (unless current instructions say otherwise)`,
		closeNote:   "**Policy:** agent.profile=team-maintainer is active. Commit, sync, and push as part of routine work; explicit \"do not commit\"/\"do not push\" instructions still override.",
		syncSection: primeCLISyncSection(doltSync, true),
		completingWorkflow: `**Completing work:**
` + "```bash" + `
bd close <id1> <id2> ...    # Close all completed issues at once
git status                  # Check changed files
# team-maintainer: commit, sync, push are routine unless instructions forbid it
git add . && git commit -m "..."
` + doltPushStep + `git push
` + "```",
		gitWorkflowRule: "Git workflow: team-maintainer active - commit/push are routine unless explicitly restricted",
		profileRule:     "Profile: team-maintainer active (agent.profile=team-maintainer) - commit, sync, and push are routine; explicit no-commit/no-push instructions still override.",
	}
}

func primeCLIConservativeParts(doltSync bool) primeCLIContextParts {
	doltPushComment := ""
	if doltSync {
		doltPushComment = "# bd dolt push\n"
	}
	return primeCLIContextParts{
		closeProtocol: `[ ] 1. bd close <id1> <id2> ...   (close completed issues)
[ ] 2. run quality gates        (tests, linters, builds when relevant)
[ ] 3. git status               (check what changed)
[ ] 4. follow active profile    (conservative: report handoff; team-maintainer: commit/sync/push if enabled)`,
		closeNote:   "**Policy:** Conservative is the default. Commit, sync, or push only when the active user, orchestrator, or repository profile grants that authority.",
		syncSection: primeCLISyncSection(doltSync, true),
		completingWorkflow: `**Completing work:**
` + "```bash" + `
bd close <id1> <id2> ...    # Close all completed issues at once
git status                  # Check changed files
# Conservative/minimal/default: report status and proposed commands; wait for approval
# Team-maintainer opt-in only, unless current instructions forbid it:
# git add . && git commit -m "..."
` + doltPushComment + `# git push
` + "```",
		gitWorkflowRule: "Git workflow: conservative by default; commit/push only with explicit user/orchestrator or team-maintainer authority",
		profileRule:     "Default: do not commit, push, or run dolt remote sync without explicit authority. Team-maintainer behavior is opt-in and still subordinate to user/orchestrator instructions.",
	}
}

func primeCLIContextHeader(redirectNotice, memories string) string {
	context := primeTruncationDirective + `# Beads Workflow Context

> **Context Recovery**: Run ` + "`bd prime`" + ` after compaction, clear, or new session
> Hooks auto-call this in Claude Code and Codex when a beads workspace is resolved

` + redirectNotice
	if memories != "" {
		context += memories + "\n"
	}
	return context
}

func primeCLISessionRules(parts primeCLIContextParts) string {
	return `# 🚨 SESSION CLOSE PROTOCOL 🚨

**CRITICAL**: Before saying "done" or "complete", you MUST run this checklist:

` + "```" + `
` + parts.closeProtocol + `
` + "```" + `

` + parts.closeNote + `

## Core Rules
- **Default**: Use beads for ALL task tracking (` + "`bd create`" + `, ` + "`bd ready`" + `, ` + "`bd close`" + `)
- **Prohibited**: Do NOT use TodoWrite, TaskCreate, or markdown files for task tracking
- **Workflow**: Create beads issue BEFORE writing code, mark in_progress when starting
- **Memory**: Use ` + "`bd remember \"insight\"`" + ` for persistent knowledge across sessions. Do NOT use MEMORY.md files — they fragment across accounts. Search with ` + "`bd memories <keyword>`" + `.
- Persistence you don't need beats lost context
- ` + parts.profileRule + `
- ` + parts.gitWorkflowRule + `
- Session management: check ` + "`bd ready`" + ` for available work

`
}

func primeCLIEssentialCommands() string {
	return `## Essential Commands

- ` + "`bd create --title=\"Summary of this issue\" --description=\"Why this issue exists and what needs to be done\" --type=task|bug|feature --priority=2`" + ` - New issue
  - Priority: 0-4 or P0-P4 (0=critical, 2=medium, 4=backlog). NOT "high"/"medium"/"low"
- ` + "`bd create ... --parent=<id>`" + ` - Hierarchical child (task under epic, subtask under task; inherits parent labels)
- ` + "`bd update <id> --claim`" + ` - Claim work
- ` + "`bd unclaim <id>`" + ` - Release stuck issue (agent crashed)
- ` + "`bd update <id> --assignee=username`" + ` - Assign to someone
- ` + "`bd update <id> --if-assignee=<expected> --assignee=<new>`" + ` - Atomic reassign: applies only if the assignee still matches (--if-status=<expected> guards status; --if-assignee='' requires unassigned). Mismatch exits non-zero with nothing written — never retry blindly
- ` + "`bd update <id> --title/--description/--notes/--design`" + ` - Update fields inline
- ` + "`bd close <id>`" + ` - Mark complete
- ` + "`bd close <id1> <id2> ...`" + ` - Close multiple issues at once (more efficient)
- ` + "`bd close <id> --reason=\"explanation\"`" + ` - Close with reason
- **Tip**: When creating multiple issues/tasks, use parallel subagents for efficiency
- **WARNING**: Do NOT use ` + "`bd edit`" + ` - it opens $EDITOR (vim/nano) which blocks agents

### Dependencies & Blocking
- ` + "`bd dep add <issue> <depends-on>`" + ` - Add dependency (issue depends on depends-on)
- ` + "`bd blocked`" + ` - Show all blocked issues
- ` + "`bd show <id>`" + ` - See what's blocking/blocked by this issue

`
}

func primeCLIHealthAndTools(syncSection string) string {
	return syncSection + `

### Project Health
- ` + "`bd stats`" + ` - Project statistics (open/closed/blocked counts)
- ` + "`bd doctor`" + ` - Check for issues (sync problems, missing hooks)
- ` + "`bd doctor --check=conventions`" + ` - Check for convention drift (lint, stale, orphans)

### Quality Tools
- ` + "`bd create --validate`" + ` - Check description has required sections
- ` + "`bd create --acceptance=\"criteria\"`" + ` - Set acceptance criteria (checked by --validate)
- ` + "`bd create --design=\"decisions\"`" + ` - Record design decisions
- ` + "`bd create --notes=\"context\"`" + ` - Add supplementary notes
- ` + "`bd config set validation.on-create warn`" + ` - Auto-validate on every create
- ` + "`bd lint`" + ` - Check existing issues for missing sections

`
}

func primeCLIWorkflows(completingWorkflow string) string {
	return `### Lifecycle & Hygiene

**Starting work:**
` + "```bash" + `
bd ready           # Find available work
bd show <id>       # Review issue details
bd update <id> --claim  # Claim it
` + "```" + `

	` + completingWorkflow + `

**Creating dependent work:**
` + "```bash" + `
# Run bd create commands in parallel (use subagents for many items)
bd create --title="Implement feature X" --description="Why this issue exists and what needs to be done" --type=feature
bd create --title="Write tests for X" --description="Why this issue exists and what needs to be done" --type=task
bd dep add beads-yyy beads-xxx  # Tests depend on Feature (Feature blocks tests)
` + "```" + `
`
}

// outputCLIContext outputs full CLI reference for non-MCP users
func outputCLIContext(w io.Writer, stealthMode bool) error {
	return outputCLIContextWithOptions(w, stealthMode, primeOptions{})
}

func outputCLIContextWithOptions(w io.Writer, stealthMode bool, opts primeOptions) error {
	ephemeral := isEphemeralBranch()
	noPush := primeNoPushConfigured()
	// localOnly reflects only the git-remote axis (drives git push/pull
	// hints and remote-sync authority wording). Dolt sync-remote presence
	// (doltSync, below) is a separate axis that only gates the literal
	// `bd dolt push`/`bd dolt pull` hint lines (gh#4130) — the two must not
	// be conflated (gh#4230 review).
	localOnly := !primeHasGitRemote()
	doltSync := primeHasSyncRemote()

	parts := primeCLIContextPartsFor(stealthMode, localOnly, ephemeral, noPush, doltSync)

	redirectNotice := getRedirectNotice(true)
	var memories string
	if !opts.noMemories {
		memories = formatMemoriesForPrimeWithOptions(false, opts)
	}

	context := primeCLIContextHeader(redirectNotice, memories)

	context += primeCLISessionRules(parts)
	context += primeCLIEssentialCommands()
	context += primeCLIHealthAndTools(parts.syncSection)
	context += primeCLIWorkflows(parts.completingWorkflow)
	_, _ = fmt.Fprint(w, context)

	return nil
}
