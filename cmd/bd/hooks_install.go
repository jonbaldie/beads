package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/git"
)

func guardHookWritePath(hookPath string, allowTracked bool) error {
	fi, err := os.Lstat(hookPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // creating a new file — nothing to clobber
		}
		return fmt.Errorf("failed to stat %s: %w", hookPath, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target := "unresolvable target"
		if resolved, rerr := filepath.EvalSymlinks(hookPath); rerr == nil {
			target = resolved
		} else if link, lerr := os.Readlink(hookPath); lerr == nil {
			target = link
		}
		return fmt.Errorf("%s is a symlink to %s; writing would rewrite the link target, not the hook\nRemove the symlink (or leave that hook to its owner) and re-run", hookPath, target)
	}
	if allowTracked {
		return nil
	}
	// A tracked file is refused only when bd does NOT own it: writing into a
	// foreign tracked file dirties every clone that shares it (the wy-81fnur
	// incident). A bd-owned hook the user chose to commit (e.g. a team-shared
	// .beads/hooks/) is bd's to maintain — same policy as shared installs.
	if isGitTrackedFile(hookPath) && !isBdOwnedHookFile(hookPath) {
		return fmt.Errorf("%s is tracked by git and not a bd-managed hook; bd will not modify committed files it does not own\nUntrack it (git rm --cached) or move hooks to an untracked directory and re-run", hookPath)
	}
	return nil
}

// isBdOwnedHookFile reports whether the hook file at path is bd-managed:
// either section-marker format or a legacy bd hook (shim or inline).
func isBdOwnedHookFile(path string) bool {
	content, err := os.ReadFile(path) // #nosec G304 -- path is a hook location bd resolved
	if err != nil {
		return false
	}
	if strings.Contains(string(content), hookSectionBeginPrefix) {
		return true
	}
	versionInfo, err := getHookVersion(path)
	return err == nil && versionInfo.IsBdHook
}

// isGitTrackedFile reports whether path is tracked by git in the repository
// containing it. Errors (not a repo, path inside .git/, no work tree) count
// as untracked — the guard only blocks writes it can prove are unsafe.
func isGitTrackedFile(path string) bool {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	// #nosec G204 G702 - fixed "git" command; dir/base come from the hooks
	// directory bd itself resolved, not user input
	cmd := exec.Command("git", "-C", dir, "ls-files", "--error-unmatch", "--", base)
	return cmd.Run() == nil
}

func resolveInstallHooksDir(shared, beadsHooks bool) (string, error) {
	if beadsHooks {
		// Use .beads/hooks/ directory (preferred for Dolt backend)
		beadsDir := beads.FindBeadsDir()
		if beadsDir == "" {
			return "", fmt.Errorf("%s", activeWorkspaceNotFoundError())
		}
		return filepath.Join(beadsDir, "hooks"), nil
	}
	if shared {
		// Use versioned directory for shared hooks
		if mainRoot, err := git.GetMainRepoRoot(); err == nil && mainRoot != "" {
			return filepath.Join(mainRoot, ".beads-hooks"), nil
		}
		return ".beads-hooks", nil
	}
	// Use common git directory for hooks (shared across worktrees)
	return git.GetGitHooksDir()
}

func prepareInstallHooksDir(hooksDir string, beadsHooks bool) error {
	// Directories inside .beads/ use BeadsDirPerm (0700); git-managed hook
	// dirs (.git/hooks, .beads-hooks) use 0755 so git can execute them.
	hooksDirPerm := os.FileMode(0755)
	if beadsHooks {
		hooksDirPerm = config.BeadsDirPerm
	}
	if err := os.MkdirAll(hooksDir, hooksDirPerm); err != nil {
		return fmt.Errorf("failed to create hooks directory: %w", err)
	}
	return nil
}

func guardInstallHookPaths(hooksDir string, hookNames []string, shared bool) error {
	// Refuse the whole install up front if any target is unsafe to write —
	// stopping midway through the loop would leave hooks half-installed.
	for _, hookName := range hookNames {
		if err := guardHookWritePath(filepath.Join(hooksDir, hookName), shared); err != nil {
			return fmt.Errorf("refusing to install %s hook: %w", hookName, err)
		}
	}
	return nil
}

func preserveForeignHookBackup(hookPath, hookName string, existing []byte) error {
	backupPath := hookPath + ".backup"
	if _, statErr := os.Lstat(backupPath); !os.IsNotExist(statErr) {
		return nil
	}
	// #nosec G306 -- keep executable so a rename restores a working hook
	if backupErr := os.WriteFile(backupPath, existing, 0755); backupErr != nil {
		return fmt.Errorf("failed to back up %s before injecting bd section: %w", hookName, backupErr)
	}
	return nil
}

func buildExistingHookContent(hookPath, hookName string, existing []byte, section string) (string, error) {
	existingStr := string(existing)
	if strings.Contains(existingStr, hookSectionBeginPrefix) {
		// Update only the section between markers
		return injectHookSection(existingStr, section), nil
	}
	versionInfo, _ := getHookVersion(hookPath)
	if versionInfo.IsBdHook {
		// Legacy bd hook — replace entire file with section format
		return "#!/usr/bin/env sh\n" + section, nil
	}
	// Non-bd hook — this is the one write that modifies a file bd does not
	// own. Preserve the original as a one-time .backup sidecar before injecting.
	if err := preserveForeignHookBackup(hookPath, hookName, existing); err != nil {
		return "", err
	}
	return injectHookSection(existingStr, section), nil
}

func readHookContentForInstall(hookPath, hookName, section string) (string, error) {
	// #nosec G304 -- hook path constrained to hooks directory
	existing, readErr := os.ReadFile(hookPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", fmt.Errorf("failed to read %s: %w", hookName, readErr)
	}
	if os.IsNotExist(readErr) {
		// No existing file — create with shebang + section
		return "#!/usr/bin/env sh\n" + section, nil
	}
	return buildExistingHookContent(hookPath, hookName, existing, section)
}

func installOneHook(hooksDir, hookName string) error {
	hookPath := filepath.Join(hooksDir, hookName)
	section := generateHookSection(hookName)
	newContent, err := readHookContentForInstall(hookPath, hookName, section)
	if err != nil {
		return err
	}
	// Normalize line endings to LF
	newContent = strings.ReplaceAll(newContent, "\r\n", "\n")
	// #nosec G306 -- git hooks must be executable for Git to run them
	if err := os.WriteFile(hookPath, []byte(newContent), 0755); err != nil {
		return fmt.Errorf("failed to write %s: %w", hookName, err)
	}
	return nil
}

func installHookFiles(hooksDir string, hookNames []string) error {
	// Install each hook using section markers (GH#1380).
	// Only the content between markers is managed by beads; user content
	// outside the markers is preserved across reinstalls and upgrades.
	for _, hookName := range hookNames {
		if err := installOneHook(hooksDir, hookName); err != nil {
			return err
		}
	}
	return nil
}

func configureInstalledHooksPath(shared, beadsHooks bool) error {
	if beadsHooks {
		return configureBeadsHooksPath()
	}
	if shared {
		return configureSharedHooksPath()
	}
	return nil
}

//nolint:unparam // force and chain kept for CLI flag compatibility; section markers make them no-ops
func installHooksWithOptions(hookNames []string, _ bool, shared bool, _ bool, beadsHooks bool) error {
	hooksDir, err := resolveInstallHooksDir(shared, beadsHooks)
	if err != nil {
		return err
	}
	if err := prepareInstallHooksDir(hooksDir, beadsHooks); err != nil {
		return err
	}
	// When setting a local core.hooksPath (beads or shared mode), preserve any
	// hooks from the previously effective hooks directory (e.g. a global
	// core.hooksPath or the default .git/hooks). Without this, setting a local
	// core.hooksPath silently shadows the global one and those hooks stop running.
	if beadsHooks || shared {
		preservePreexistingHooks(hooksDir)
	}
	if err := guardInstallHookPaths(hooksDir, hookNames, shared); err != nil {
		return err
	}
	if err := installHookFiles(hooksDir, hookNames); err != nil {
		return err
	}
	if err := configureInstalledHooksPath(shared, beadsHooks); err != nil {
		return fmt.Errorf("failed to configure git hooks path: %w", err)
	}
	return nil
}

// preservePreexistingHooks copies non-beads hooks from the currently effective
// hooks directory into targetDir. This prevents hooks from a global
// core.hooksPath (or the default .git/hooks/) from being silently lost when
// beads sets a local core.hooksPath override.
func resolvePreexistingHookSource(targetDir string) (string, bool) {
	// Get the hooks directory git would currently use (before we override it).
	currentDir, err := git.GetGitHooksDir()
	if err != nil {
		return "", false
	}
	// Resolve to absolute paths for reliable comparison.
	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		return "", false
	}
	absCurrent, err := filepath.Abs(currentDir)
	if err != nil {
		return "", false
	}
	// If the current dir is already our target, this is a re-install — skip.
	if absTarget == absCurrent || isBeadsHookSourceDirectory(absCurrent) {
		return "", false
	}
	return currentDir, true
}

func isBeadsHookSourceDirectory(absCurrent string) bool {
	repoRoot, _ := git.GetMainRepoRoot()
	if repoRoot == "" {
		repoRoot = git.GetRepoRoot()
	}
	if repoRoot == "" {
		return false
	}
	absBeadsHooks, _ := filepath.Abs(filepath.Join(repoRoot, ".beads", "hooks"))
	absSharedHooks, _ := filepath.Abs(filepath.Join(repoRoot, ".beads-hooks"))
	return absCurrent == absBeadsHooks || absCurrent == absSharedHooks
}

func shouldSkipPreexistingHookEntry(entry os.DirEntry, fromHusky bool) bool {
	if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || strings.HasSuffix(entry.Name(), ".sample") {
		return true
	}
	if !fromHusky {
		return false
	}
	// Husky v9's dispatcher and v8's helper are not useful after hooks are
	// copied into a beads-managed directory.
	return entry.Name() == "h" || entry.Name() == "husky.sh"
}

func preserveOnePreexistingHook(currentDir, targetDir string, entry os.DirEntry, fromHusky bool) {
	srcPath := filepath.Join(currentDir, entry.Name())
	// #nosec G304 -- hook path constrained to known hooks directories
	content, err := os.ReadFile(srcPath)
	if err != nil {
		return
	}
	newContent, keep := shouldPreserveHookContent(string(content), fromHusky)
	if !keep {
		return
	}
	// Don't overwrite existing files in target
	dstPath := filepath.Join(targetDir, entry.Name())
	if _, err := os.Stat(dstPath); err == nil {
		return
	}
	// #nosec G306 -- git hooks must be executable
	if err := os.WriteFile(dstPath, []byte(newContent), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to preserve %s hook from %s: %v\n", entry.Name(), currentDir, err) //nolint:gosec // G705: CLI stderr, not HTML.
		return
	}
	fmt.Printf("  Preserving existing %s hook from %s\n", entry.Name(), currentDir)
}

func copyPreexistingHooks(currentDir, targetDir string, fromHusky bool) {
	entries, err := os.ReadDir(currentDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if shouldSkipPreexistingHookEntry(entry, fromHusky) {
			continue
		}
		preserveOnePreexistingHook(currentDir, targetDir, entry, fromHusky)
	}
}
