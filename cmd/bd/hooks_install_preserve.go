package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/git"
)

func preservePreexistingHooks(targetDir string) {
	currentDir, ok := resolvePreexistingHookSource(targetDir)
	if !ok {
		return
	}

	// Detect whether the source hooks live inside a husky directory. Husky v8
	// hooks source `.husky/_/husky.sh`; husky v9 hooks source `.husky/_/h`.
	// When the copy target is a beads-managed directory (e.g. .beads/hooks/),
	// those sourced helpers are not present relative to the copied hook, so
	// we must either also copy the helpers or rewrite the hooks to not need
	// them. We choose the latter: inline-sanitize the hook body and skip the
	// dispatcher files entirely. (GH#3132)
	fromHusky := isHuskyDir(currentDir)

	copyPreexistingHooks(currentDir, targetDir, fromHusky)

	// GH#3132: Fix husky hook layout after copying.
	fixHuskyHookLayout(currentDir, targetDir)
}

// fixHuskyHookLayout handles two husky-specific issues when hooks are copied
// from a husky-managed directory into .beads/hooks/.
//
// Bug 1 (v8): Husky v8 hooks source "$(dirname "$0")/_/husky.sh", but the
// _/ subdirectory is not copied because preservePreexistingHooks skips
// directories. Fix: create a relative symlink to the original _/ directory.
//
// Bug 2 (v9): Husky v9 uses a "h" dispatcher that resolves user hooks via
// dirname(dirname($0)), which breaks when relocated. The shims in .husky/_/
// are wrappers, not actual user hooks. Fix: replace copied shims with the
// real user hook content from the parent directory (.husky/).
func linkHuskyHelperDirectory(sourceDir, targetDir string) {
	srcHelper := filepath.Join(sourceDir, "_")
	info, err := os.Stat(srcHelper)
	if err != nil || !info.IsDir() {
		return
	}
	tgtHelper := filepath.Join(targetDir, "_")
	if _, err := os.Lstat(tgtHelper); !os.IsNotExist(err) {
		return
	}
	relPath, err := filepath.Rel(targetDir, srcHelper)
	if err != nil {
		return
	}
	if err := os.Symlink(relPath, tgtHelper); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to symlink husky helper directory: %v\n", err)
	}
}

func replaceOneHuskyV9Shim(sourceDir, targetDir string, entry os.DirEntry) {
	if entry.IsDir() || entry.Name() == "h" {
		return
	}
	hookPath := filepath.Join(targetDir, entry.Name())
	if _, err := os.Stat(hookPath); err != nil {
		return
	}
	userHookPath := filepath.Join(filepath.Dir(sourceDir), entry.Name())
	userContent, err := os.ReadFile(userHookPath) // #nosec G304 -- constrained to husky dir
	if err != nil {
		return
	}
	// Ensure the content has a shebang (user hooks in .husky/ often omit it)
	replacement := string(userContent)
	if !strings.HasPrefix(replacement, "#!") {
		replacement = "#!/usr/bin/env sh\n" + replacement
	}
	// #nosec G306 -- git hooks must be executable
	if err := os.WriteFile(hookPath, []byte(replacement), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to replace husky v9 shim %s: %v\n", entry.Name(), err) //nolint:gosec // G705: CLI stderr, not HTML.
	}
}

func replaceHuskyV9Shims(sourceDir, targetDir string) {
	// Detect v9 by checking for the dispatcher in the source directory.
	srcH := filepath.Join(sourceDir, "h")
	hContent, err := os.ReadFile(srcH) // #nosec G304 -- path is in known hooks directory
	if err != nil || !strings.Contains(string(hContent), `dirname "$(dirname`) {
		return
	}
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		replaceOneHuskyV9Shim(sourceDir, targetDir, entry)
	}
}

func fixHuskyHookLayout(sourceDir, targetDir string) {
	// Bug 1: Symlink _/ helper directory for husky v8 compatibility.
	linkHuskyHelperDirectory(sourceDir, targetDir)

	// Bug 2: Replace husky v9 shims with actual user hook content.
	replaceHuskyV9Shims(sourceDir, targetDir)
}

// isHuskyDir reports whether dir looks like a husky-managed hooks directory
// (either `.husky` itself or `.husky/_`, the helper dir used by v9).
func isHuskyDir(dir string) bool {
	if dir == "" {
		return false
	}
	base := filepath.Base(dir)
	parent := filepath.Base(filepath.Dir(dir))
	if base == ".husky" {
		return true
	}
	// .husky/_  (husky v9 helper directory that is sometimes set as
	// core.hooksPath directly).
	if base == "_" && parent == ".husky" {
		return true
	}
	return false
}

// sanitizeHuskyHook rewrites a husky hook body so it can run standalone
// without the `.husky/_/husky.sh` (v8) or `.husky/_/h` (v9) helper being
// reachable relative to $0. It removes the helper-source line and prepends
// `node_modules/.bin` to PATH so that tools like `npx`, `lint-staged`, and
// project-local binaries continue to resolve — which is what husky v9's `h`
// normally does for the user. (GH#3132)
//
// Hooks that don't look like husky hooks are returned unchanged.
func sanitizeHuskyHook(content string) string {
	// Normalize CRLF first so our line-by-line rewrite works on
	// Windows-authored hooks too.
	normalized := strings.ReplaceAll(content, "\r\n", "\n")

	lines := strings.Split(normalized, "\n")
	out := make([]string, 0, len(lines)+2)
	sourcedHelper := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Match husky v8 helper: `. "$(dirname -- "$0")/_/husky.sh"` and
		// common variants (single-quoted, no `--`, `source` instead of `.`).
		if isHuskyHelperSourceLine(trimmed) {
			sourcedHelper = true
			// Drop the line entirely.
			continue
		}
		out = append(out, line)
	}

	if !sourcedHelper {
		// Not recognizably a husky-sourcing hook — leave it alone.
		return content
	}

	// Rebuild, injecting a PATH export right after the shebang (if any) so
	// that `npx`, `lint-staged`, etc. keep working. Husky v9's `h` normally
	// does this for the user.
	result := make([]string, 0, len(out)+2)
	injected := false
	pathLine := `export PATH="$PWD/node_modules/.bin:$PATH"`

	for i, line := range out {
		result = append(result, line)
		if !injected && i == 0 && strings.HasPrefix(strings.TrimSpace(line), "#!") {
			result = append(result, "# Injected by beads (GH#3132): husky helper layout not mirrored into this dir.")
			result = append(result, pathLine)
			injected = true
		}
	}
	if !injected {
		// No shebang — inject at the top.
		result = append([]string{pathLine}, result...)
	}

	return strings.Join(result, "\n")
}

// isHuskyHelperSourceLine reports whether line (already trimmed) sources one
// of the husky helper scripts. Matches husky v8 (`_/husky.sh`) and husky v9
// (`/h`) dispatchers, tolerating quoting and `source` vs `.` variants.
func isHuskyV8HelperSourceLine(line string) bool {
	return strings.Contains(line, "/_/husky.sh") || strings.Contains(line, `\_\husky.sh`)
}

func isHuskyV9HelperSourceLine(line string) bool {
	return strings.Contains(line, "dirname") &&
		(strings.HasSuffix(line, `/h"`) || strings.HasSuffix(line, `/h'`) || strings.HasSuffix(line, "/h"))
}

func isHuskyHelperSourceLine(line string) bool {
	if line == "" {
		return false
	}
	// Must start with POSIX source (`. `) or bash `source `.
	if !strings.HasPrefix(line, ". ") && !strings.HasPrefix(line, "source ") {
		return false
	}
	return isHuskyV8HelperSourceLine(line) || isHuskyV9HelperSourceLine(line)
}

func configureSharedHooksPath() error {
	// Set git config core.hooksPath to an absolute path pointing to .beads-hooks.
	// Using an absolute path is critical for git worktrees (GH#2414):
	// git resolves relative core.hooksPath relative to the working tree root.
	repoRoot, _ := git.GetMainRepoRoot()
	if repoRoot == "" {
		repoRoot = git.GetRepoRoot()
	}
	if repoRoot == "" {
		return fmt.Errorf("not in a git repository")
	}
	absHooksPath := filepath.Join(repoRoot, ".beads-hooks")
	cmd := exec.Command("git", "config", "core.hooksPath", absHooksPath)
	cmd.Dir = repoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git config failed: %w (output: %s)", err, string(output))
	}
	return nil
}

func configureBeadsHooksPath() error {
	// Set git config core.hooksPath to an absolute path pointing to .beads/hooks.
	// Using an absolute path is critical for git worktrees (GH#2414):
	// git resolves relative core.hooksPath relative to the working tree root,
	// so in a worktree ".beads/hooks" would resolve to <worktree>/.beads/hooks/
	// which doesn't exist — the hooks live in the main repo's .beads/hooks/.
	repoRoot, _ := git.GetMainRepoRoot()
	if repoRoot == "" {
		repoRoot = git.GetRepoRoot()
	}
	if repoRoot == "" {
		return fmt.Errorf("not in a git repository")
	}
	absHooksPath := filepath.Join(repoRoot, ".beads", "hooks")
	cmd := exec.Command("git", "config", "core.hooksPath", absHooksPath)
	cmd.Dir = repoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git config failed: %w (output: %s)", err, string(output))
	}
	return nil
}
