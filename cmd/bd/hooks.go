package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

// managedHookNames lists the git hooks managed by beads.
// Hook content is generated dynamically by generateHookSection().
var managedHookNames = []string{"pre-commit", "post-merge", "pre-push", "post-checkout", "prepare-commit-msg"}

const hookVersionPrefix = "# bd-hooks-version: "
const shimVersionPrefix = "# bd-shim "

// inlineHookMarker identifies inline hooks created by bd init (GH#1120)
// These hooks have the logic embedded directly rather than using shims
const inlineHookMarker = "# bd (beads)"

// Section markers for git hooks (GH#1380) — consistent with AGENTS.md pattern.
// Only content between markers is managed by beads; user content outside is preserved.
const hookSectionBeginPrefix = "# --- BEGIN BEADS INTEGRATION"
const hookSectionEndPrefix = "# --- END BEADS INTEGRATION"

// hookSectionBeginLine returns the full begin marker line with the current version.
func hookSectionBeginLine() string {
	return fmt.Sprintf("%s v%s ---", hookSectionBeginPrefix, Version)
}

// hookSectionEndLine returns the full end marker line with the current version.
func hookSectionEndLine() string {
	return fmt.Sprintf("%s v%s ---", hookSectionEndPrefix, Version)
}

// hookTimeoutSeconds is the soft deadline for a beads hook. GNU timeout sends
// SIGTERM at the deadline, while Perl's alarm terminates the direct bd process.
// Neither backend promises containment of TERM-resistant descendant work. When
// neither helper is available, the generated hook warns that it is unbounded.
// The default is 300 seconds (5 minutes) to accommodate chained hooks — e.g.
// pre-commit framework pipelines that run linters, type-checkers, and builds
// inside `bd hooks run` via the `.old` hook chain (GH#2732).
// The value can be overridden via the BEADS_HOOK_TIMEOUT environment variable.
const hookTimeoutSeconds = 300

// Cobra commands

var hooksCmd = &cobra.Command{
	Use:     "hooks",
	GroupID: "setup",
	Short:   "Manage git hooks for beads integration",
	Long: `Install, uninstall, or list git hooks for beads integration.

The hooks provide:
- pre-commit: Run chained hooks before commit
- post-merge: Run chained hooks after pull/merge
- pre-push: Run chained hooks before push
- post-checkout: Run chained hooks after branch checkout
- prepare-commit-msg: Add agent identity trailers for forensics`,
}

var hooksInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install bd git hooks",
	Long: `Install git hooks for beads integration.

By default, hooks are installed to .git/hooks/ in the current repository.
Use --beads to install to .beads/hooks/ (recommended for Dolt backend).
Use --shared to install to a versioned directory (.beads-hooks/) that can be
committed to git and shared with team members.

Hooks use section markers to coexist with existing hooks — any user content
outside the markers is preserved across installs and upgrades.

Installed hooks:
  - pre-commit: Run chained hooks before commit
  - post-merge: Run chained hooks after pull/merge
  - pre-push: Run chained hooks before push
  - post-checkout: Run chained hooks after branch checkout
  - prepare-commit-msg: Add agent identity trailers (for orchestrator agents)`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("hooks-install")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		force, _ := cmd.Flags().GetBool("force")
		shared, _ := cmd.Flags().GetBool("shared")
		chain, _ := cmd.Flags().GetBool("chain")
		beadsHooks, _ := cmd.Flags().GetBool("beads")

		if err := installHooksWithOptions(managedHookNames, force, shared, chain, beadsHooks); err != nil {
			return HandleErrorRespectJSON("installing hooks: %v", err)
		}

		if isJSONOutput() {
			output := map[string]interface{}{
				"success":    true,
				"message":    "Git hooks installed successfully",
				"shared":     shared,
				"chained":    chain,
				"beadsHooks": beadsHooks,
			}
			jsonBytes, _ := json.MarshalIndent(output, "", "  ")
			fmt.Println(string(jsonBytes))
		} else {
			fmt.Println("✓ Git hooks installed successfully")
			fmt.Println()
			if beadsHooks {
				fmt.Println("Hooks installed to: .beads/hooks/")
				fmt.Println("Git config set: core.hooksPath=.beads/hooks")
				fmt.Println()
			} else if shared {
				fmt.Println("Hooks installed to: .beads-hooks/")
				fmt.Println("Git config set: core.hooksPath=.beads-hooks")
				fmt.Println()
				fmt.Println("⚠️  Remember to commit .beads-hooks/ to share with your team!")
				fmt.Println()
			}
			fmt.Println("Installed hooks:")
			for _, hookName := range managedHookNames {
				fmt.Printf("  - %s\n", hookName)
			}
		}
		return nil
	},
}

var hooksUninstallCmd = &cobra.Command{
	Use:           "uninstall",
	Short:         "Uninstall bd git hooks",
	Long:          `Remove bd git hooks from .git/hooks/ directory.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("hooks-uninstall")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if err := uninstallHooks(); err != nil {
			return HandleErrorRespectJSON("uninstalling hooks: %v", err)
		}

		if isJSONOutput() {
			output := map[string]interface{}{
				"success": true,
				"message": "Git hooks uninstalled successfully",
			}
			jsonBytes, _ := json.MarshalIndent(output, "", "  ")
			fmt.Println(string(jsonBytes))
		} else {
			fmt.Println("✓ Git hooks uninstalled successfully")
		}
		return nil
	},
}

var hooksListCmd = &cobra.Command{
	Use:           "list",
	Short:         "List installed git hooks status",
	Long:          `Show the status of bd git hooks (installed, outdated, missing).`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("hooks-list")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		statuses := CheckGitHooks()

		if isJSONOutput() {
			output := map[string]interface{}{
				"hooks": statuses,
			}
			jsonBytes, _ := json.MarshalIndent(output, "", "  ")
			fmt.Println(string(jsonBytes))
		} else {
			fmt.Println("Git hooks status:")
			for _, status := range statuses {
				if !status.Installed {
					fmt.Printf("  ✗ %s: not installed\n", status.Name)
				} else if status.IsShim {
					fmt.Printf("  ✓ %s: installed (shim %s)\n", status.Name, status.Version)
				} else if status.Outdated {
					fmt.Printf("  ⚠ %s: installed (version %s, current: %s) - outdated\n",
						status.Name, status.Version, Version)
				} else {
					fmt.Printf("  ✓ %s: installed (version %s)\n", status.Name, status.Version)
				}
			}
		}
		return nil
	},
}

// guardHookWritePath refuses hook writes that would silently modify files
// bd does not own (bd-5vdt8):
//   - a symlinked hook path: os.WriteFile follows the link (O_TRUNC) and
//     rewrites the target inode — e.g. a repo-tracked script the hook
//     points at, dirtying every clone that shares it
//   - a hook file tracked by git: writing it dirties the working tree.
//     allowTracked exempts shared installs (.beads-hooks/ is deliberately
//     committed).
var hooksRunCmd = &cobra.Command{
	Use:   "run <hook-name> [args...]",
	Short: "Execute a git hook (called by thin shims)",
	Long: `Execute the logic for a git hook. This command is typically called by
thin shim scripts installed in .git/hooks/.

Supported hooks:
  - pre-commit: Run chained hooks before commit
  - post-merge: Run chained hooks after pull/merge
  - pre-push: Run chained hooks before push
  - post-checkout: Run chained hooks after branch checkout
  - prepare-commit-msg: Add agent identity trailers for forensics

The thin shim keeps delegated hook logic in sync with the installed bd
version. Upgrading bd updates that delegated behavior. To adopt changes to the
shim's generated shell policy, refresh it with 'bd hooks install'.`,
	Args:          cobra.MinimumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("hooks-run")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		_ = os.Setenv("BD_GIT_HOOK", "1")
		ui.DisableColors()

		hookName := args[0]
		hookArgs := args[1:]

		var exitCode int
		switch hookName {
		case "pre-commit":
			exitCode = runPreCommitHook()
		case "post-merge":
			exitCode = runPostMergeHook()
		case "pre-push":
			exitCode = runPrePushHook(hookArgs)
		case "post-checkout":
			exitCode = runPostCheckoutHook(hookArgs)
		case "prepare-commit-msg":
			exitCode = runPrepareCommitMsgHook(hookArgs)
		default:
			return HandleError("unknown hook: %s", hookName)
		}

		if exitCode != 0 {
			return &exitError{Code: exitCode}
		}
		return nil
	},
}

func init() {
	hooksInstallCmd.Flags().Bool("force", false, "No-op, kept for compatibility (section markers always preserve non-bd content)")
	hooksInstallCmd.Flags().Bool("shared", false, "Install hooks to .beads-hooks/ (versioned) instead of .git/hooks/")
	hooksInstallCmd.Flags().Bool("chain", false, "No-op, kept for compatibility (existing hook content always runs alongside the bd section)")
	hooksInstallCmd.Flags().Bool("beads", false, "Install hooks to .beads/hooks/ (recommended for Dolt backend)")

	hooksCmd.AddCommand(hooksInstallCmd)
	hooksCmd.AddCommand(hooksUninstallCmd)
	hooksCmd.AddCommand(hooksListCmd)
	hooksCmd.AddCommand(hooksRunCmd)

	rootCmd.AddCommand(hooksCmd)
}
