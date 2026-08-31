package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/cmd/bd/doctor"
	"github.com/jonbaldie/beads/internal/git"
)

func uninstallHookSection(hookPath, hookName string, content string) (bool, error) {
	newContent, found := removeHookSection(content)
	if !found {
		return false, nil
	}
	remaining := strings.TrimSpace(newContent)
	if remaining == "" || remaining == "#!/usr/bin/env sh" || remaining == "#!/bin/sh" {
		// Only shebang left — remove the file entirely
		if err := os.Remove(hookPath); err != nil {
			return true, fmt.Errorf("failed to remove %s: %w", hookName, err)
		}
		return true, nil
	}
	// #nosec G306 -- git hooks must be executable
	if err := os.WriteFile(hookPath, []byte(newContent), 0755); err != nil {
		return true, fmt.Errorf("failed to write %s: %w", hookName, err)
	}
	return true, nil
}

func uninstallLegacyHook(hookPath, hookName string) error {
	versionInfo, err := getHookVersion(hookPath)
	if err != nil || !versionInfo.IsBdHook {
		// Not a bd hook at all — leave it alone
		return nil
	}
	if err := os.Remove(hookPath); err != nil {
		return fmt.Errorf("failed to remove %s: %w", hookName, err)
	}
	backupPath := hookPath + ".backup"
	if _, err := os.Stat(backupPath); err == nil {
		if err := os.Rename(backupPath, hookPath); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to restore backup for %s: %v\n", hookName, err)
		}
	}
	return nil
}

func uninstallOneHook(hooksDir, hookName string) error {
	hookPath := filepath.Join(hooksDir, hookName)
	// #nosec G304 -- hook path constrained to .git/hooks directory
	content, err := os.ReadFile(hookPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read %s: %w", hookName, err)
	}
	if handled, err := uninstallHookSection(hookPath, hookName, string(content)); handled {
		return err
	}
	return uninstallLegacyHook(hookPath, hookName)
}

func uninstallHooks() error {
	// Get hooks directory from common git dir (hooks are shared across worktrees)
	hooksDir, err := git.GetGitHooksDir()
	if err != nil {
		return err
	}
	hookNames := []string{"pre-commit", "post-merge", "pre-push", "post-checkout", "prepare-commit-msg"}
	for _, hookName := range hookNames {
		if err := uninstallOneHook(hooksDir, hookName); err != nil {
			return err
		}
	}

	// Reset beads-managed git config (core.hooksPath, beads.role) now that the
	// hook files themselves are removed. A failure here must not be a
	// scrolling stderr warning — bd hooks uninstall must not report success
	// while beads-managed config is still left behind (GH#4440).
	if err := resetHooksPathIfBeadsManaged(); err != nil {
		return fmt.Errorf("hook files removed, but failed to reset beads-managed git config: %w", err)
	}
	return nil
}

// resetHooksPathIfBeadsManaged unsets core.hooksPath if it points to a
// beads-managed hooks directory (.beads/hooks or .beads-hooks), and unsets
// beads.role. beads.role marks a repo as beads-managed independent of
// core.hooksPath, so it is cleared unconditionally here rather than gated on
// the hooksPath match — otherwise an uninstall that runs after core.hooksPath
// was already cleared (e.g. by `bd doctor --fix`) would leave a stale
// beads.role behind. A key that is already absent is not an error.
func resetHooksPathIfBeadsManaged() error {
	repoRoot, _ := git.GetMainRepoRoot()
	if repoRoot == "" {
		repoRoot = git.GetRepoRoot()
	}
	if repoRoot == "" {
		return nil // not in a git repo
	}

	var failures []string

	cmd := exec.Command("git", "config", "--get", "core.hooksPath")
	cmd.Dir = repoRoot
	if out, err := cmd.Output(); err == nil {
		hooksPath := strings.TrimSpace(string(out))
		// Matches both relative (legacy) and absolute (GH#2414) beads hooks
		// paths, symlink-resolving the absolute forms. Shared with
		// doctor.CheckHooksPath/FixHooksPath so uninstall and `bd doctor --fix`
		// cannot disagree about what "beads-managed" means.
		if doctor.IsBeadsManagedHooksPath(repoRoot, hooksPath) {
			unsetCmd := exec.Command("git", "config", "--unset", "core.hooksPath")
			unsetCmd.Dir = repoRoot
			if output, err := unsetCmd.CombinedOutput(); err != nil {
				failures = append(failures, fmt.Sprintf("core.hooksPath: %v (output: %s)", err, strings.TrimSpace(string(output))))
			}
		}
	}
	// core.hooksPath not set at all — nothing to reset there; still fall
	// through to beads.role below.

	// Read before unsetting rather than treating git's exit 5 as "already
	// absent". Exit 5 also means "the key has multiple values, refusing an
	// ambiguous unset" — a repo with a duplicated beads.role (bad merge, hand
	// edit) would then report a clean uninstall while leaving the key set,
	// which is the exact failure this is supposed to stop.
	getRoleCmd := exec.Command("git", "config", "--get", "beads.role")
	getRoleCmd.Dir = repoRoot
	if _, err := getRoleCmd.Output(); err == nil {
		roleCmd := exec.Command("git", "config", "--unset", "beads.role")
		roleCmd.Dir = repoRoot
		if output, err := roleCmd.CombinedOutput(); err != nil {
			failures = append(failures, fmt.Sprintf("beads.role: %v (output: %s)", err, strings.TrimSpace(string(output))))
		}
	}

	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}

	return nil
}

// =============================================================================
// Hook Implementation Functions (called by thin shims via 'bd hooks run')
// =============================================================================

// runChainedHook runs a .old hook if it exists. Returns the exit code.
// If the hook doesn't exist, returns 0 (success).
