package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/git"
)

func runChainedHook(hookName string, args []string) int {
	// Get the hooks directory from common dir (hooks are shared across worktrees)
	hooksDir, err := git.GetGitHooksDir()
	if err != nil {
		return 0 // Not a git repo, nothing to chain
	}

	oldHookPath := filepath.Join(hooksDir, hookName+".old")

	// Check if the .old hook exists and is executable
	info, err := os.Stat(oldHookPath)
	if err != nil {
		return 0 // No chained hook
	}
	if info.Mode().Perm()&0111 == 0 {
		return 0 // Not executable
	}

	// Check if .old is itself a bd hook (shim or inline) - skip to prevent infinite recursion
	// This can happen if user runs `bd hooks install --chain` multiple times,
	// renaming an existing bd hook to .old. See: GH#843, GH#1120
	versionInfo, err := getHookVersion(oldHookPath)
	if err == nil && versionInfo.IsBdHook {
		// Skip execution - .old is a bd hook which would call us again
		return 0
	}

	// Run the chained hook
	// #nosec G204 -- hookName is from controlled list, path is from git directory
	cmd := exec.Command(oldHookPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		// Other error - treat as failure
		fmt.Fprintf(os.Stderr, "Warning: chained hook %s failed: %v\n", hookName, err)
		return 1
	}

	return 0
}

// runPreCommitHook runs chained hooks before commit.
// Returns 0 on success (or if not applicable).
func runPreCommitHook() int {
	// Run chained hook first (if exists)
	if exitCode := runChainedHook("pre-commit", nil); exitCode != 0 {
		return exitCode
	}

	// GH#2489, GH#1863: Export JSONL before commit so issue state lands in
	// the same commit as code changes.  maybeAutoExport() skips when
	// BD_GIT_HOOK=1, so we invoke `bd export` as a subprocess instead.
	exportJSONLForCommit()

	return 0
}

// exportJSONLForCommit exports Dolt issue state to the git-tracked JSONL file
// when export.auto is enabled. Called from the pre-commit hook so that the
// exported file can be staged and included in the pending commit.
//
// Errors are logged as warnings but never block the commit.
func runJSONLExportForCommit(beadsDir, fullPath string) {
	debug.Logf("pre-commit: exporting JSONL to %s\n", fullPath)
	warnJSONLWithoutDoltRemote("pre-commit auto-export")

	// Shell out to `bd export` which initializes its own store.
	// Clear BD_GIT_HOOK from the subprocess env so that its
	// PersistentPostRun auto-export path does not also fire.
	//
	// NOTE: we intentionally preserve GIT_DIR et al. in the subprocess
	// env. The subprocess's PostRun eventually routes through the same
	// gitAddFile as the parent, which relies on the inherited GIT_DIR to
	// identify the hook's worktree and apply the cross-worktree staging
	// guard (GH#3311 part 2). Scrubbing here would disable that guard.
	// Run from the project root, not .beads/. Embedded Dolt discovery starts
	// from cwd, so cwd=.beads/ can make the export subprocess look for a
	// nested .beads/.beads workspace and warn on every commit (GH#3454).
	cmd := exec.Command("bd", "export", "-o", fullPath)
	cmd.Dir = exportSubprocessDir(beadsDir)
	cmd.Env = filterEnv(os.Environ(), "BD_GIT_HOOK")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "beads: pre-commit export warning: %v\n", err)
		return
	}

	// Stage the exported file if configured. Skip when no-git-ops is set
	// (GH#3314). gitAddFile scrubs the inherited git hook env vars so git
	// rediscovers the repo from cwd, and silently skips when fullPath is
	// outside the hook's worktree (the .beads/redirect case where fullPath
	// points into the main repo, not this worktree). See GH#3311.
	if config.GetBool("export.git-add") && !config.GetBool("no-git-ops") {
		if err := gitAddFile(fullPath); err != nil {
			debug.Logf("pre-commit: git add failed: %v\n", err)
		}
	}
}

func exportJSONLForCommit() {
	if !config.GetBool("export.auto") {
		return
	}
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return
	}
	exportPath := config.GetString("export.path")
	if exportPath == "" {
		exportPath = "issues.jsonl"
	}
	fullPath := filepath.Join(beadsDir, exportPath)

	// If the export file is staged for deletion (user ran `git rm`), do not
	// re-export or re-stage it. GIT_INDEX_FILE is set during an actual commit,
	// so git-diff-index reads the pending index — where the deletion lives.
	// Without this guard, git add fullPath would convert the staged deletion
	// back to a modification and the file would never be removed from the
	// repo. Reimplements gastownhall/beads#3838 (ckumar1).
	if isExportFileStagedForDeletion(fullPath) {
		debug.Logf("pre-commit: %s staged for deletion — skipping export\n", exportPath)
		return
	}
	if !preCommitHasStagedBeadsFiles(beadsDir) {
		debug.Logf("pre-commit: skipping JSONL export — no staged .beads paths\n")
		return
	}
	runJSONLExportForCommit(beadsDir, fullPath)
}

// isExportFileStagedForDeletion reports whether the beads export file at
// fullPath is staged for deletion (the user ran `git rm` on it). When true,
// exportJSONLForCommit must skip re-exporting and re-staging it: running
// `git add` on a freshly regenerated file would convert the staged deletion
// back into a modification, silently reviving a file the user intentionally
// removed.
//
// Unlike preCommitHasStagedBeadsFiles and gitAddFile, this deliberately runs
// git with the hook's inherited environment intact rather than scrubbing
// GIT_* vars. GIT_INDEX_FILE is set during an actual commit and points at
// the pending index — where the staged deletion lives — so scrubbing it
// here would make git fall back to the on-disk index and miss the
// deletion. Reimplements gastownhall/beads#3838 (ckumar1).
func isExportFileStagedForDeletion(fullPath string) bool {
	checkCmd := exec.Command("git", "diff", "--cached", "--diff-filter=D", "--name-only", "--", filepath.Base(fullPath))
	checkCmd.Dir = filepath.Dir(fullPath)
	out, _ := checkCmd.Output()
	return len(out) > 0
}

func preCommitHasStagedBeadsFiles(beadsDir string) bool {
	cmdDir := exportSubprocessDir(beadsDir)
	if hookRoot := hookWorkTreeRoot(); hookRoot != "" {
		cmdDir = hookRoot
	}
	cmd := exec.Command("git", "diff", "--cached", "--name-only", "--", ".beads")
	cmd.Dir = cmdDir
	cmd.Env = scrubGitHookEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		debug.Logf("pre-commit: failed to inspect staged .beads paths: %v\n", err)
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

func exportSubprocessDir(beadsDir string) string {
	return filepath.Dir(beadsDir)
}

// syncImportJSONLPath returns the JSONL path used by the legacy git-hook sync
// import path. Existing projects may have customized export.path before
// import.path existed, so keep importing from export.path unless import.path is
// explicitly configured.
func syncImportJSONLPath(beadsDir string) string {
	if config.GetValueSource("import.path") == config.SourceDefault {
		exportPath := config.GetString("export.path")
		if exportPath != "" {
			return filepath.Join(beadsDir, exportPath)
		}
	}
	return configuredImportJSONLPath(beadsDir)
}

// importJSONLForSync imports JSONL into Dolt after a git
// pull/merge/branch-checkout only for legacy projects with no Dolt remote.
// When sync.remote is configured, Dolt remains the source of truth and JSONL
// import is skipped because upsert-only import cannot reconcile stale exports.
//
// Errors are logged as warnings but never block the merge/checkout. The
// import is upsert; running it on an unchanged JSONL is a no-op (bd
// import returns "Error 1105: nothing to commit", which we tolerate).
//
// See GH#3729.
func importJSONLForSync(reason string) {
	if !config.GetBool("import.auto") {
		return
	}
	if resolveSyncRemote() != "" {
		debug.Logf("%s: skipping JSONL import because sync.remote is configured\n", reason)
		return
	}

	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return
	}

	fullPath := syncImportJSONLPath(beadsDir)

	if info, err := os.Stat(fullPath); err != nil || info.Size() == 0 {
		return
	}

	debug.Logf("%s: importing JSONL from %s\n", reason, fullPath)
	warnJSONLWithoutDoltRemote(reason + " JSONL import")

	// Shell out to `bd import` — same pattern as exportJSONLForCommit.
	// Clear BD_GIT_HOOK so the subprocess's own hook-detection logic
	// doesn't suppress its work.
	cmd := exec.Command("bd", "import", "--quiet", fullPath)
	cmd.Dir = exportSubprocessDir(beadsDir)
	cmd.Env = filterEnv(os.Environ(), "BD_GIT_HOOK")

	out, err := cmd.CombinedOutput()
	if err == nil {
		return
	}
	// Tolerate the no-op case: when JSONL matches Dolt exactly, bd import
	// produces "nothing to commit" from the underlying Dolt commit. That
	// is success for our purposes.
	if strings.Contains(string(out), "nothing to commit") {
		return
	}
	fmt.Fprintf(os.Stderr, "beads: %s import warning: %v\n%s", reason, err, out) //nolint:gosec // G705: CLI stderr, not HTML.
}

func warnJSONLWithoutDoltRemote(reason string) {
	if config.GetBool("no-git-ops") || resolveSyncRemote() != "" || !isGitRepo() {
		return
	}
	fmt.Fprintf(os.Stderr, "beads: %s warning: no Dolt remote configured.\n", reason) //nolint:gosec // G705: CLI stderr, not HTML.
	fmt.Fprintln(os.Stderr, "beads: .beads/issues.jsonl is an export, not cross-machine sync or source of truth.")
	if originURL, err := gitOriginGetURL(); err == nil && originURL != "" {
		fmt.Fprintf(os.Stderr, "beads: repair: bd dolt remote add origin %s && bd dolt push\n", normalizeRemoteURL(originURL))
		return
	}
	fmt.Fprintln(os.Stderr, "beads: repair: add a git origin, then run 'bd dolt remote add origin <git-remote-url>' and 'bd dolt push'.")
}

// filterEnv returns a copy of env with entries matching the given key removed.
func filterEnv(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

// runPostMergeHook runs chained hooks after merge, then runs the legacy
// JSONL import fallback only when no Dolt remote is configured. See GH#3729.
//
// Returns 0 on success (or if not applicable).
//
//nolint:unparam // Always returns 0 by design - warnings don't block merges
func runPostMergeHook() int {
	// Run chained hook first (if exists)
	if exitCode := runChainedHook("post-merge", nil); exitCode != 0 {
		return exitCode
	}
	importJSONLForSync("post-merge")
	return 0
}

// runPrePushHook runs chained hooks before push.
// Returns 0 to allow push, non-zero to block.
func runPrePushHook(args []string) int {
	// Run chained hook first (if exists)
	if exitCode := runChainedHook("pre-push", args); exitCode != 0 {
		return exitCode
	}
	return 0
}

// runPostCheckoutHook runs chained hooks after branch checkout, then runs
// the legacy JSONL import fallback when the checkout was a branch switch
// (flag=1) and no Dolt remote is configured. File-mode checkouts (flag=0)
// are skipped to avoid spurious imports on `git checkout -- <file>`. See GH#3729.
//
// args: [previous-HEAD, new-HEAD, flag] where flag=1 for branch checkout
// Returns 0 on success (or if not applicable).
//
//nolint:unparam // Always returns 0 by design - warnings don't block checkouts
func runPostCheckoutHook(args []string) int {
	// Run chained hook first (if exists)
	if exitCode := runChainedHook("post-checkout", args); exitCode != 0 {
		return exitCode
	}
	if len(args) >= 3 && args[2] == "1" {
		importJSONLForSync("post-checkout")
	}
	return 0
}

// runPrepareCommitMsgHook adds agent identity trailers to commit messages.
// args: [commit-msg-file, source, sha1]
// Returns 0 on success (or if not applicable), non-zero on error.
//
//nolint:unparam // Always returns 0 by design - we don't block commits
func prepareCommitMessageArgs(args []string) (string, string, bool) {
	if len(args) < 1 {
		return "", "", false
	}
	source := ""
	if len(args) >= 2 {
		source = args[1]
	}
	return args[0], source, true
}

func commitMessageHasActorTrailer(content []byte) bool {
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "Executed-By:") {
			return true
		}
	}
	return false
}

func addActorTrailer(content []byte, actor string) []byte {
	msg := strings.TrimRight(string(content), "\n\r\t ")
	var sb strings.Builder
	sb.WriteString(msg)
	sb.WriteString("\n\n")
	sb.WriteString(fmt.Sprintf("Executed-By: %s\n", actor))
	return []byte(sb.String())
}

func runPrepareCommitMsgHook(args []string) int {
	// Run chained hook first (if exists)
	if exitCode := runChainedHook("prepare-commit-msg", args); exitCode != 0 {
		return exitCode
	}
	msgFile, source, ok := prepareCommitMessageArgs(args)
	if !ok || source == "merge" {
		return 0
	}
	actor := os.Getenv("BD_ACTOR")
	if actor == "" {
		return 0 // Not in agent context, nothing to add
	}

	// Read current message
	content, err := os.ReadFile(msgFile) // #nosec G304 -- path from git
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not read commit message: %v\n", err)
		return 0
	}
	if commitMessageHasActorTrailer(content) {
		return 0
	}
	// Write back
	if err := os.WriteFile(msgFile, addActorTrailer(content, actor), 0600); err != nil { // Restrict permissions per gosec G306
		fmt.Fprintf(os.Stderr, "Warning: could not write commit message: %v\n", err)
	}
	return 0
}

// =============================================================================
// Hook Helper Functions
// =============================================================================

// isRebaseInProgress checks if a rebase is in progress.
func isRebaseInProgress() bool {
	if _, err := os.Stat(".git/rebase-merge"); err == nil {
		return true
	}
	if _, err := os.Stat(".git/rebase-apply"); err == nil {
		return true
	}
	return false
}
