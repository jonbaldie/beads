package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jonbaldie/beads/internal/git"
	"github.com/jonbaldie/beads/internal/ui"
)

// preCommitFrameworkPattern matches pre-commit or prek framework hooks.
// Uses same patterns as hookManagerPatterns in doctor/fix/hooks.go for consistency.
// Includes all detection patterns: pre-commit run, prek run/hook-impl, config file refs, and pre-commit env vars.
var preCommitFrameworkPattern = regexp.MustCompile(`(?i)(pre-commit\s+run|prek\s+run|prek\s+hook-impl|\.pre-commit-config|INSTALL_PYTHON|PRE_COMMIT)`)

// hooksInstalled checks if bd git hooks are installed
func hooksInstalled() bool {
	hooksDir, err := git.GetGitHooksDir()
	if err != nil {
		return false
	}
	return beadHookInstalled(filepath.Join(hooksDir, "pre-commit"), "bd (beads) pre-commit hook") &&
		beadHookInstalled(filepath.Join(hooksDir, "post-merge"), "bd (beads) post-merge hook")
}

func beadHookInstalled(path, signature string) bool {
	// #nosec G304 - controlled path from git directory
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	if !strings.Contains(string(content), signature) && !strings.Contains(string(content), hookSectionBeginPrefix) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().Perm()&0111 != 0
}

// hooksNeedUpdate checks if installed bd hooks are outdated and need updating.
// Delegates to CheckGitHooks() which handles version comparison, shim detection,
// and inline hook detection consistently.
func hooksNeedUpdate() bool {
	for _, s := range CheckGitHooks() {
		if s.Outdated {
			return true
		}
	}
	return false
}

// hookInfo contains information about an existing hook
type hookInfo struct {
	name                 string
	path                 string
	exists               bool
	isBdHook             bool
	isPreCommitFramework bool // true for pre-commit or prek
	content              string
}

// detectExistingHooks scans for existing git hooks
func detectExistingHooks() []hookInfo {
	hooksDir, err := git.GetGitHooksDir()
	if err != nil {
		return nil
	}
	hooks := []hookInfo{
		{name: "pre-commit", path: filepath.Join(hooksDir, "pre-commit")},
		{name: "post-merge", path: filepath.Join(hooksDir, "post-merge")},
		{name: "pre-push", path: filepath.Join(hooksDir, "pre-push")},
	}

	for i := range hooks {
		content, err := os.ReadFile(hooks[i].path)
		if err == nil {
			hooks[i].exists = true
			hooks[i].content = string(content)
			hooks[i].isBdHook = strings.Contains(hooks[i].content, "bd (beads)") ||
				strings.Contains(hooks[i].content, hookSectionBeginPrefix)
			// Only detect pre-commit/prek framework if not a bd hook
			// Use regex for consistency with DetectActiveHookManager patterns
			if !hooks[i].isBdHook {
				hooks[i].isPreCommitFramework = preCommitFrameworkPattern.MatchString(hooks[i].content)
			}
		}
	}

	return hooks
}

// installGitHooks installs git hooks inline (no external dependencies)
func installGitHooks() error {
	hooksDir, err := git.GetGitHooksDir()
	if err != nil {
		return err
	}

	// Ensure hooks directory exists
	if err := os.MkdirAll(hooksDir, 0750); err != nil {
		return fmt.Errorf("failed to create hooks directory: %w", err)
	}

	existingHooks := detectExistingHooks()
	chainHooks := hasUnmanagedHooks(existingHooks)
	if chainHooks {
		chainExistingHooks(existingHooks)
	}
	if err := writeExecutableHook(filepath.Join(hooksDir, "pre-commit"), buildPreCommitHook(chainHooks, existingHooks)); err != nil {
		return fmt.Errorf("failed to write pre-commit hook: %w", err)
	}
	if err := writeExecutableHook(filepath.Join(hooksDir, "post-merge"), buildPostMergeHook(chainHooks, existingHooks)); err != nil {
		return fmt.Errorf("failed to write post-merge hook: %w", err)
	}
	if chainHooks {
		fmt.Printf("%s Chained bd hooks with existing hooks\n", ui.RenderPass("✓"))
	}

	return nil
}

func hasUnmanagedHooks(hooks []hookInfo) bool {
	for _, hook := range hooks {
		if hook.exists && !hook.isBdHook {
			return true
		}
	}
	return false
}

func chainExistingHooks(hooks []hookInfo) {
	for _, hook := range hooks {
		if !hook.exists || hook.isBdHook {
			continue
		}
		if err := os.Rename(hook.path, hook.path+".old"); err != nil {
			fmt.Fprintf(os.Stderr, "%s Failed to chain with existing %s hook: %v\n", ui.RenderWarn("⚠"), hook.name, err)
			if usesSQLServer() {
				fmt.Fprintf(os.Stderr, "You can resolve this with: %s\n", ui.RenderAccent("bd doctor --fix"))
			}
			continue
		}
		fmt.Printf("  Chained with existing %s hook\n", hook.name)
	}
}

func writeExecutableHook(path, content string) error {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	// #nosec G306 - git hooks must be executable
	return os.WriteFile(path, []byte(content), 0700)
}

// buildPreCommitHook generates the pre-commit hook content using section markers (GH#1380).
// If chainHooks is true, chained hooks (.old) are called before the beads section.
func buildPreCommitHook(chainHooks bool, existingHooks []hookInfo) string {
	section := generateHookSection("pre-commit")

	if chainHooks {
		var existingPreCommit string
		for _, hook := range existingHooks {
			if hook.name == "pre-commit" && hook.exists && !hook.isBdHook {
				existingPreCommit = hook.path + ".old"
				break
			}
		}

		return "#!/bin/sh\n" +
			"# Run existing hook first\n" +
			"if [ -x \"" + existingPreCommit + "\" ]; then\n" +
			"    \"" + existingPreCommit + "\" \"$@\"\n" +
			"    EXIT_CODE=$?\n" +
			"    if [ $EXIT_CODE -ne 0 ]; then\n" +
			"        exit $EXIT_CODE\n" +
			"    fi\n" +
			"fi\n\n" +
			section
	}

	return "#!/bin/sh\n" + section
}

// buildPostMergeHook generates the post-merge hook content using section markers (GH#1380).
func buildPostMergeHook(chainHooks bool, existingHooks []hookInfo) string {
	section := generateHookSection("post-merge")

	if chainHooks {
		var existingPostMerge string
		for _, hook := range existingHooks {
			if hook.name == "post-merge" && hook.exists && !hook.isBdHook {
				existingPostMerge = hook.path + ".old"
				break
			}
		}

		return "#!/bin/sh\n" +
			"# Run existing hook first\n" +
			"if [ -x \"" + existingPostMerge + "\" ]; then\n" +
			"    \"" + existingPostMerge + "\" \"$@\"\n" +
			"    EXIT_CODE=$?\n" +
			"    if [ $EXIT_CODE -ne 0 ]; then\n" +
			"        exit $EXIT_CODE\n" +
			"    fi\n" +
			"fi\n\n" +
			section
	}

	return "#!/bin/sh\n" + section
}

// installJJHooks installs marker-managed hooks for colocated jujutsu+git repos.
// This path intentionally avoids .old sidecar chaining and uses the same section
// injection behavior as regular hook installs.
func installJJHooks() error {
	// jj only needs pre-commit and post-merge hooks
	jjHookNames := []string{"pre-commit", "post-merge"}
	return installHooksWithOptions(jjHookNames, false, false, false, false)
}

// buildJJPreCommitHook generates the pre-commit hook for jujutsu repos using section markers (GH#1380).
func buildJJPreCommitHook(chainHooks bool, existingHooks []hookInfo) string {
	// jj uses the same shim as git — bd hooks run handles the differences internally
	section := generateHookSection("pre-commit")

	if chainHooks {
		var existingPreCommit string
		for _, hook := range existingHooks {
			if hook.name == "pre-commit" && hook.exists && !hook.isBdHook {
				existingPreCommit = hook.path + ".old"
				break
			}
		}

		return "#!/bin/sh\n" +
			"# Run existing hook first\n" +
			"if [ -x \"" + existingPreCommit + "\" ]; then\n" +
			"    \"" + existingPreCommit + "\" \"$@\"\n" +
			"    EXIT_CODE=$?\n" +
			"    if [ $EXIT_CODE -ne 0 ]; then\n" +
			"        exit $EXIT_CODE\n" +
			"    fi\n" +
			"fi\n\n" +
			section
	}

	return "#!/bin/sh\n" + section
}

// printJJAliasInstructions prints setup instructions for pure jujutsu repos.
// Since jj doesn't have native hooks yet, users need to set up aliases.
func printJJAliasInstructions() {
	fmt.Printf("\n%s Jujutsu repository detected (not colocated with git)\n\n", ui.RenderWarn("⚠"))
	fmt.Printf("Jujutsu doesn't support hooks yet. To auto-export beads on push,\n")
	fmt.Printf("add this alias to your jj config (~/.config/jj/config.toml):\n\n")
	fmt.Printf("  %s\n", ui.RenderAccent("[aliases]"))
	fmt.Printf("  %s\n", ui.RenderAccent(`push = ["util", "exec", "--", "sh", "-c", "bd dolt commit && bd dolt push && jj git push \"$@\"", ""]`))
	fmt.Printf("\nThen use %s instead of %s\n\n", ui.RenderAccent("jj push"), ui.RenderAccent("jj git push"))
	fmt.Printf("For more details, see: https://github.com/gastownhall/beads/blob/main/docs/reference/git-integration.md#branchless-workflows-jujutsu--jj\n\n")
}
