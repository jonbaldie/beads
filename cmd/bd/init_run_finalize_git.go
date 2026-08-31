package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/cmd/bd/setup"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/git"
	"github.com/jonbaldie/beads/internal/templates/agents"
	"github.com/jonbaldie/beads/internal/ui"
)

func setupInitGitIntegrations(args initFinalizeArgs) error {
	if err := setupInitForkExclude(args); err != nil {
		return err
	}
	return promptInitAutoExport(args)
}

func setupInitForkExclude(args initFinalizeArgs) error {
	setupExclude, _ := args.cmd.Flags().GetBool("setup-exclude")
	if setupExclude {
		if err := setupForkExclude(!args.mode.quiet); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to configure git exclude: %v\n", err)
		}
		return nil
	}
	if args.mode.stealth || !isGitRepo() {
		return nil
	}
	isFork, upstreamURL := detectForkSetup()
	if !isFork {
		return nil
	}
	return applyInitForkExclude(args, upstreamURL)
}

func applyInitForkExclude(args initFinalizeArgs, upstreamURL string) error {
	if args.mode.nonInteractive {
		if err := setupForkExclude(!args.mode.quiet); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to configure git exclude: %v\n", err)
		}
		return nil
	}
	shouldExclude, err := promptForkExclude(upstreamURL, args.mode.quiet)
	if err != nil && isCanceled(err) {
		fmt.Fprintln(os.Stderr, "Setup canceled.")
		return errCanceled()
	}
	if shouldExclude {
		if err := setupForkExclude(!args.mode.quiet); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to configure git exclude: %v\n", err)
		}
	}
	return nil
}

func promptInitAutoExport(args initFinalizeArgs) error {
	if args.mode.nonInteractive || args.mode.quiet {
		return nil
	}
	wantExport, err := promptAutoExport()
	if err != nil && isCanceled(err) {
		fmt.Fprintln(os.Stderr, "Setup canceled.")
		return errCanceled()
	}
	if wantExport {
		if err := config.SetYamlConfig("export.auto", "true"); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to enable auto-export: %v\n", err)
		} else {
			fmt.Printf("  %s Auto-export enabled -> .beads/issues.jsonl\n", ui.RenderPass("✓"))
		}
		return nil
	}
	fmt.Printf("  %s Auto-export disabled (enable later with: bd config set export.auto true)\n", ui.RenderPass("✓"))
	return nil
}

func installInitHooksAndAgents(args initFinalizeArgs) error {
	installInitHooks(args)
	writeInitLocalVersion(args)
	if err := addInitAgentsInstructions(args); err != nil {
		return err
	}
	setupInitAgentProjects(args)
	return nil
}

func installInitHooks(args initFinalizeArgs) {
	hooksExist := hooksInstalled()
	if args.mode.skipHooks || (hooksExist && !hooksNeedUpdate()) {
		return
	}
	if hooksExist && !args.mode.quiet {
		fmt.Printf("  Updating hooks to version %s...\n", Version)
	}
	installInitHooksForRepo(args.mode.quiet)
}

func installInitHooksForRepo(quiet bool) {
	isJJ := git.IsJujutsuRepo()
	isColocated := git.IsColocatedJJGit()
	if isJJ && !isColocated {
		if !quiet {
			printJJAliasInstructions()
		}
		return
	}
	if isColocated {
		installInitJJHooks(quiet)
		return
	}
	if isGitRepo() {
		installInitGitHooks(quiet)
	}
}

func installInitJJHooks(quiet bool) {
	if err := installJJHooks(); err != nil && !quiet {
		fmt.Fprintf(os.Stderr, "\n%s Failed to install jj hooks: %v\n", ui.RenderWarn("⚠"), err)
		fmt.Fprintf(os.Stderr, "You can try again with: %s\n\n", ui.RenderAccent("bd doctor --fix"))
		return
	}
	if !quiet {
		fmt.Printf("  Hooks installed (jujutsu mode - no staging)\n")
	}
}

func installInitGitHooks(quiet bool) {
	if err := installHooksWithOptions(managedHookNames, false, false, false, true); err != nil && !quiet {
		fmt.Fprintf(os.Stderr, "\n%s Failed to install git hooks to .beads/hooks/: %v\n", ui.RenderWarn("⚠"), err)
		fmt.Fprintf(os.Stderr, "You can try again with: %s\n\n", ui.RenderAccent("bd hooks install --beads"))
		return
	}
	if !quiet {
		fmt.Printf("  Hooks installed to: .beads/hooks/\n")
	}
}

func writeInitLocalVersion(args initFinalizeArgs) {
	if !args.ident.useLocalBeads {
		return
	}
	localVersionPath := filepath.Join(args.ident.beadsDir, ".local_version")
	if err := writeLocalVersion(localVersionPath, Version); err != nil && !args.mode.quiet {
		fmt.Fprintf(os.Stderr, "Warning: failed to initialize version tracking: %v\n", err)
	}
}

func addInitAgentsInstructions(args initFinalizeArgs) error {
	if args.mode.stealth || args.mode.skipAgents {
		return nil
	}
	agentsTemplate, _ := args.cmd.Flags().GetString("agents-template")
	agentsProfileStr, _ := args.cmd.Flags().GetString("agents-profile")
	agentsFile, _ := args.cmd.Flags().GetString("agents-file")
	resolved, err := persistInitAgentsFile(agentsFile)
	if err != nil {
		return err
	}
	if isBareGitRepo() {
		if !args.mode.quiet {
			fmt.Printf("  Skipping %s generation in bare repository\n", resolved)
		}
		return nil
	}
	renderOpts := agents.RenderOpts{
		HasRemote: shouldConfigureInitDoltRemote(args.remote.syncURL, args.remote.syncFromRemote, args.remote.syncURLFromConfig, args.remote.syncURLFromGitOrigin, isDoltLocalOnly()),
		NoPush:    config.GetBool("no-push"),
	}
	addAgentsInstructions(resolved, !args.mode.quiet, agentsTemplate, agents.Profile(agentsProfileStr), renderOpts)
	return nil
}

func persistInitAgentsFile(agentsFile string) (string, error) {
	if agentsFile != "" {
		if err := config.ValidateAgentsFile(agentsFile); err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid --agents-file: %v\n", err)
			return "", errInitStopped
		}
		if err := config.SetYamlConfig("agents.file", agentsFile); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to persist agents.file to config: %v\n", err)
		}
		return agentsFile, nil
	}
	return config.SafeAgentsFile(), nil
}

func setupInitAgentProjects(args initFinalizeArgs) {
	if skipInitAgentProjects(args) {
		return
	}
	warnInitAgentSetup("Claude hooks", setup.InstallClaudeProject(args.mode.stealth), args.mode.quiet)
	warnInitAgentSetup("Codex integration", setup.InstallCodexProject(), args.mode.quiet)
	warnInitAgentSetup("Cursor integration", setup.InstallCursorProject(), args.mode.quiet)
}

func skipInitAgentProjects(args initFinalizeArgs) bool {
	return args.mode.stealth || args.mode.skipAgents || isBareGitRepo()
}

func warnInitAgentSetup(name string, err error, quiet bool) {
	if err == nil || quiet {
		return
	}
	fmt.Fprintf(os.Stderr, "Warning: failed to setup %s: %v\n", name, err)
}

func commitInitBeadsFiles(args initFinalizeArgs) {
	if args.mode.stealth || !isGitRepo() || !args.ident.useLocalBeads {
		return
	}
	gitAddCmd := exec.Command("git", "add", ".beads/")
	if _, addErr := gitAddCmd.CombinedOutput(); addErr != nil {
		return
	}
	stageInitGeneratedFiles()
	commitInitGeneratedFiles(args.mode.quiet)
}

func stageInitGeneratedFiles() {
	stageInitPathIfExists(config.SafeAgentsFile())
	stageInitPathIfExists(filepath.Join(".claude", "settings.json"))
	stageInitPathIfExists("CLAUDE.md")
	for _, path := range []string{".agents", ".codex", ".cursor"} {
		stageInitPathIfExists(path)
	}
	stageInitPathIfExists(".gitignore")
}

func stageInitPathIfExists(path string) {
	if _, statErr := os.Stat(path); statErr == nil {
		cmd := exec.Command("git", "add", path) //nolint:gosec // G702: callers pass only the fixed initialization paths above.
		_ = cmd.Run()
	}
}

func commitInitGeneratedFiles(quiet bool) {
	commitArgs := []string{"-c", "core.hooksPath=", "commit", "--no-verify", "-m", "bd init: initialize beads issue tracking"}
	commitCmd := exec.Command("git", commitArgs...)
	commitOut, commitErr := commitCmd.CombinedOutput()
	if commitErr != nil {
		if !quiet && !strings.Contains(string(commitOut), "nothing to commit") {
			fmt.Fprintf(os.Stderr, "Warning: failed to commit beads files: %v\n", commitErr)
		}
		return
	}
	if !quiet {
		fmt.Printf("  %s Committed beads files to git\n", ui.RenderPass("✓"))
	}
}

func warnInitGitUpstream(args initFinalizeArgs) {
	if !isGitRepo() || args.mode.quiet {
		return
	}
	if gitHasAnyRemotes() && !gitHasUpstream() {
		fmt.Fprintf(os.Stderr, "\n%s Git upstream not configured\n", ui.RenderWarn("⚠"))
		fmt.Fprintf(os.Stderr, "  For sync workflows, set your upstream with:\n")
		fmt.Fprintf(os.Stderr, "  %s\n\n", ui.RenderAccent("git remote add upstream <repo-url>"))
	}
	if !args.mode.stealth && !args.remote.initRemoteChanged && !shouldWireInitRemote(args.remote.syncURL, args.remote.syncFromRemote, args.remote.syncURLFromConfig, args.remote.syncURLFromGitOrigin) {
		printInitNoDoltRemoteWarning(true)
	}
}
