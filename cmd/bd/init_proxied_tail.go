package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/ui"
)

func runInitProxiedServerTail(cmd *cobra.Command, ctx context.Context, in initProxiedServerInput, t runInitTailContext) error {
	isRepo := t.gitUC.IsGitRepo(ctx)
	if err := configureProxiedInitRole(ctx, in, t, isRepo); err != nil {
		return err
	}
	if err := configureProxiedInitExclude(cmd, ctx, in, t, isRepo); err != nil {
		return err
	}
	if err := installProxiedInitHooks(ctx, in, t, isRepo); err != nil {
		return err
	}
	if err := installProxiedInitAgents(cmd, ctx, in, t); err != nil {
		return err
	}
	if err := commitProxiedInitArtifacts(ctx, in, t, isRepo); err != nil {
		return err
	}
	warnProxiedInitRepository(ctx, in, t, isRepo)
	return printProxiedInitSuccess(in, t)
}

func configureProxiedInitRole(ctx context.Context, in initProxiedServerInput, t runInitTailContext, isRepo bool) error {
	if !isRepo {
		return nil
	}
	role := in.roleFlag
	if role == "" {
		role = "maintainer"
	}
	_, hasRole, _ := t.gitUC.BeadsRole(ctx)
	if hasRole && in.roleFlag == "" {
		return nil
	}
	if err := t.gitUC.SetBeadsRole(ctx, role); err != nil && !in.quiet {
		fmt.Fprintf(os.Stderr, "Warning: failed to set beads.role: %v\n", err)
	}
	return nil
}

func configureProxiedInitExclude(cmd *cobra.Command, ctx context.Context, in initProxiedServerInput, t runInitTailContext, isRepo bool) error {
	setupExclude, _ := cmd.Flags().GetBool("setup-exclude")
	if setupExclude {
		return setupProxiedInitForkExclude(ctx, in, t)
	}
	if in.stealth || !isRepo {
		return nil
	}
	isFork, upstreamURL, _ := t.gitUC.DetectFork(ctx)
	if !isFork {
		return nil
	}
	if in.nonInteractive {
		return setupProxiedInitForkExclude(ctx, in, t)
	}
	shouldExclude, err := promptForkExclude(upstreamURL, in.quiet)
	if err != nil && isCanceled(err) {
		fmt.Fprintln(os.Stderr, "Setup canceled.")
		return errCanceled()
	}
	if !shouldExclude {
		return nil
	}
	return setupProxiedInitForkExclude(ctx, in, t)
}

func setupProxiedInitForkExclude(ctx context.Context, in initProxiedServerInput, t runInitTailContext) error {
	if err := t.fsUseCase.SetupForkExclude(ctx, !in.quiet); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to configure git exclude: %v\n", err)
	}
	return nil
}

func installProxiedInitHooks(ctx context.Context, in initProxiedServerInput, t runInitTailContext, isRepo bool) error {
	installed := hooksInstalled()
	if in.skipHooks || (installed && !hooksNeedUpdate()) {
		return nil
	}
	if installed && !in.quiet {
		fmt.Printf("  Updating hooks to version %s...\n", Version)
	}
	return installProxiedInitHookMode(ctx, in, t, isRepo)
}

func installProxiedInitHookMode(ctx context.Context, in initProxiedServerInput, t runInitTailContext, isRepo bool) error {
	isJJ := t.gitUC.IsJujutsuRepo(ctx)
	isColocated := t.gitUC.IsColocatedJJGit(ctx)
	switch {
	case isJJ && !isColocated:
		if !in.quiet {
			printJJAliasInstructions()
		}
	case isColocated:
		return installProxiedInitJJHooks(ctx, in, t)
	default:
		return installProxiedInitGitHooks(ctx, in, t, isRepo)
	}
	return nil
}

func installProxiedInitJJHooks(ctx context.Context, in initProxiedServerInput, t runInitTailContext) error {
	if err := t.fsUseCase.InstallJJHooks(ctx); err != nil {
		if !in.quiet {
			fmt.Fprintf(os.Stderr, "\n%s Failed to install jj hooks: %v\n", ui.RenderWarn("⚠"), err)
		}
		return nil
	}
	if !in.quiet {
		fmt.Printf("  Hooks installed (jujutsu mode - no staging)\n")
	}
	return nil
}

func installProxiedInitGitHooks(ctx context.Context, in initProxiedServerInput, t runInitTailContext, isRepo bool) error {
	if !isRepo {
		return nil
	}
	err := t.fsUseCase.InstallGitHooks(ctx, domain.HooksInstallParams{
		HookNames:  managedHookNames,
		BeadsHooks: true,
	})
	if err != nil {
		if !in.quiet {
			fmt.Fprintf(os.Stderr, "\n%s Failed to install git hooks to .beads/hooks/: %v\n", ui.RenderWarn("⚠"), err)
		}
		return nil
	}
	if !in.quiet {
		fmt.Printf("  Hooks installed to: .beads/hooks/\n")
	}
	return nil
}

func installProxiedInitAgents(cmd *cobra.Command, ctx context.Context, in initProxiedServerInput, t runInitTailContext) error {
	if in.stealth || in.skipAgents {
		return nil
	}
	agentsTemplate, _ := cmd.Flags().GetString("agents-template")
	agentsProfile, _ := cmd.Flags().GetString("agents-profile")
	agentsFile, _ := cmd.Flags().GetString("agents-file")
	if err := persistProxiedInitAgentsFile(ctx, t, agentsFile); err != nil {
		return err
	}
	resolvedAgentsFile := agentsFile
	if resolvedAgentsFile == "" {
		resolvedAgentsFile = config.SafeAgentsFile()
	}
	if t.gitUC.IsBareGitRepo(ctx) {
		if !in.quiet {
			fmt.Printf("  Skipping %s generation in bare repository\n", resolvedAgentsFile)
		}
		return nil
	}
	_ = t.fsUseCase.AddAgentsInstructions(ctx, domain.AgentsFileParams{
		File:         resolvedAgentsFile,
		Verbose:      !in.quiet,
		TemplatePath: agentsTemplate,
		Profile:      agentsProfile,
		HasRemote:    t.remoteURL != "",
		NoPush:       config.GetBool("no-push"),
	})
	if err := t.fsUseCase.InstallClaudeProject(ctx, in.stealth); err != nil && !in.quiet {
		fmt.Fprintf(os.Stderr, "Warning: failed to setup Claude hooks: %v\n", err)
	}
	return nil
}

func persistProxiedInitAgentsFile(ctx context.Context, t runInitTailContext, agentsFile string) error {
	if agentsFile == "" {
		return nil
	}
	if err := config.ValidateAgentsFile(agentsFile); err != nil {
		return HandleError("invalid --agents-file: %v", err)
	}
	if err := t.fsUseCase.SetYAMLConfig(ctx, "agents.file", agentsFile); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to persist agents.file to config: %v\n", err)
	}
	return nil
}

func commitProxiedInitArtifacts(ctx context.Context, in initProxiedServerInput, t runInitTailContext, isRepo bool) error {
	if in.stealth || !isRepo || !t.useLocalBeads {
		return nil
	}
	commitResult, err := t.gitUC.CommitInitArtifacts(ctx, domain.CommitInitArtifactsParams{
		BeadsDir: ".beads/",
		OptionalPaths: []string{
			config.SafeAgentsFile(),
			filepath.Join(".claude", "settings.json"),
			"CLAUDE.md",
			".gitignore",
		},
		Message:   "bd init: initialize beads issue tracking",
		NoVerify:  true,
		SkipHooks: true,
	})
	if err != nil {
		if !in.quiet {
			fmt.Fprintf(os.Stderr, "Warning: failed to commit beads files: %v\n", err)
		}
		return nil
	}
	if commitResult.DidCommit && !in.quiet {
		fmt.Printf("  %s Committed beads files to git\n", ui.RenderPass("✓"))
	}
	return nil
}

func warnProxiedInitRepository(ctx context.Context, in initProxiedServerInput, t runInitTailContext, isRepo bool) {
	if !isRepo || in.quiet {
		return
	}
	if t.gitUC.HasAnyRemotes(ctx) && !t.gitUC.HasUpstream(ctx) {
		fmt.Fprintf(os.Stderr, "\n%s Git upstream not configured\n", ui.RenderWarn("⚠"))
		fmt.Fprintf(os.Stderr, "  For sync workflows, set your upstream with:\n")
		fmt.Fprintf(os.Stderr, "  %s\n\n", ui.RenderAccent("git remote add upstream <repo-url>"))
	}
	if !in.stealth && !in.initRemoteChanged && t.remoteURL == "" {
		printInitNoDoltRemoteWarning(false)
	}
}

func printProxiedInitSuccess(in initProxiedServerInput, t runInitTailContext) error {
	if in.quiet {
		return nil
	}
	fmt.Printf("\n%s bd initialized successfully!\n\n", ui.RenderPass("✓"))
	fmt.Printf("  Backend: %s\n", ui.RenderAccent(configfile.BackendDolt))
	fmt.Printf("  Mode: %s\n", ui.RenderAccent("proxied-server"))
	fmt.Printf("  Database: %s\n", ui.RenderAccent(t.dbName))
	fmt.Printf("  Issue prefix: %s\n", ui.RenderAccent(t.prefix))
	fmt.Printf("  Issues will be named: %s\n\n", ui.RenderAccent(t.prefix+"-<hash> (e.g., "+t.prefix+"-a3f2dd)"))
	fmt.Printf("Run %s to get started.\n\n", ui.RenderAccent("bd quickstart"))
	return nil
}
