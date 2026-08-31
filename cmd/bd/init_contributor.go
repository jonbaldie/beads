package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/ui"
)

// contributorWizardOpts controls contributor routing setup. Non-interactive
// mode applies defaults (planning repo at ~/.beads-planning unless overridden)
// so agents can run `bd init --contributor --non-interactive`.
type contributorWizardOpts struct {
	NonInteractive bool
	PlanningRepo   string
	Quiet          bool
}

func runContributorWizard(ctx context.Context, store storage.DoltStorage, opts contributorWizardOpts) error {
	if opts.NonInteractive {
		return applyContributorSetup(ctx, store, opts)
	}
	fmt.Printf("\n%s %s\n\n", ui.RenderBold("bd"), ui.RenderBold("Contributor Workflow Setup Wizard"))
	fmt.Println("This wizard will configure beads for OSS contribution.")
	fmt.Println()

	ctx = contributorWizardContext(ctx)
	reader := bufio.NewReader(os.Stdin)

	continueSetup, isFork, err := confirmContributorWizardSetup(ctx, reader)
	if err != nil {
		return err
	}
	if !continueSetup {
		return nil
	}

	// Step 3: Configure planning repository
	fmt.Printf("\n%s Setting up planning repository...\n", ui.RenderAccent("▶"))

	planningPath, err := chooseContributorPlanningPath(ctx, reader)
	if err != nil {
		return err
	}

	// Create planning repository if it doesn't exist
	if err := prepareContributorPlanningRepo(planningPath); err != nil {
		return err
	}

	if err := configureContributorRouting(ctx, store, planningPath, isFork); err != nil {
		return err
	}

	printContributorWizardSummary(planningPath)
	return nil
}

func confirmContributorWizardSetup(ctx context.Context, reader *bufio.Reader) (bool, bool, error) {
	continueSetup, err := confirmContributorBeadsDir(ctx, reader)
	if err != nil || !continueSetup {
		return continueSetup, false, err
	}

	fmt.Printf("%s Detecting git repository setup...\n", ui.RenderAccent("▶"))
	isFork, upstreamURL := detectForkSetup()
	continueSetup, err = confirmContributorFork(ctx, reader, isFork, upstreamURL)
	if err != nil || !continueSetup {
		return continueSetup, isFork, err
	}

	fmt.Printf("\n%s Checking repository access...\n", ui.RenderAccent("▶"))
	hasPushAccess, originURL := checkPushAccess()
	continueSetup, err = confirmContributorPushAccess(ctx, reader, hasPushAccess, originURL)
	return continueSetup, isFork, err
}

func contributorWizardContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func readContributorWizardResponse(ctx context.Context, reader *bufio.Reader, prompt string) (string, error) {
	fmt.Print(prompt)
	response, err := readLineWithContext(ctx, reader, os.Stdin)
	if err != nil {
		if isCanceled(err) {
			return "", err
		}
		response = ""
	}
	return strings.TrimSpace(strings.ToLower(response)), nil
}

func confirmContributorBeadsDir(ctx context.Context, reader *bufio.Reader) (bool, error) {
	beadsDir := os.Getenv("BEADS_DIR")
	if beadsDir == "" {
		return true, nil
	}
	fmt.Printf("%s BEADS_DIR is set: %s\n", ui.RenderWarn("⚠"), beadsDir)
	fmt.Println("\n  BEADS_DIR takes precedence over contributor routing.")
	fmt.Println("  If you're using the ACF pattern (external tracking repo),")
	fmt.Println("  you likely don't need --contributor.")
	fmt.Println()
	response, err := readContributorWizardResponse(ctx, reader, "Continue anyway? [y/N]: ")
	if err != nil {
		return false, err
	}
	if response != "y" && response != "yes" {
		fmt.Println("Setup canceled.")
		return false, nil
	}
	fmt.Println()
	return true, nil
}

func confirmContributorFork(ctx context.Context, reader *bufio.Reader, isFork bool, upstreamURL string) (bool, error) {
	if isFork {
		fmt.Printf("%s Detected fork workflow (upstream: %s)\n", ui.RenderPass("✓"), upstreamURL)
		return true, nil
	}
	fmt.Printf("%s No upstream remote detected\n", ui.RenderWarn("⚠"))
	fmt.Println("\n  For fork workflows, add an 'upstream' remote:")
	fmt.Println("  git remote add upstream <original-repo-url>")
	fmt.Println()
	response, err := readContributorWizardResponse(ctx, reader, "Continue with contributor setup? [y/N]: ")
	if err != nil {
		return false, err
	}
	if response != "y" && response != "yes" {
		fmt.Println("Setup canceled.")
		return false, nil
	}
	return true, nil
}

func confirmContributorPushAccess(ctx context.Context, reader *bufio.Reader, hasPushAccess bool, originURL string) (bool, error) {
	if !hasPushAccess {
		fmt.Printf("%s Read-only access to origin (%s)\n", ui.RenderPass("✓"), originURL)
		fmt.Println("  Planning repo recommended to keep experimental work separate.")
		return true, nil
	}
	fmt.Printf("%s You have push access to origin (%s)\n", ui.RenderPass("✓"), originURL)
	fmt.Printf("  %s You can commit directly to this repository.\n", ui.RenderWarn("⚠"))
	fmt.Println()
	response, err := readContributorWizardResponse(ctx, reader, "Do you want to use a separate planning repo anyway? [Y/n]: ")
	if err != nil {
		return false, err
	}
	if response == "n" || response == "no" {
		fmt.Println("\nSetup canceled. Your issues will be stored in the current repository.")
		return false, nil
	}
	return true, nil
}

func chooseContributorPlanningPath(ctx context.Context, reader *bufio.Reader) (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	defaultPlanningRepo := filepath.Join(homeDir, ".beads-planning")
	if envBeadsDir := os.Getenv("BEADS_DIR"); envBeadsDir != "" {
		defaultPlanningRepo = envBeadsDir
	}
	fmt.Printf("\nWhere should contributor planning issues be stored?\n")
	fmt.Printf("Default: %s\n", ui.RenderAccent(defaultPlanningRepo))
	planningPath, err := readContributorWizardResponse(ctx, reader, "Planning repo path [press Enter for default]: ")
	if err != nil {
		return "", err
	}
	if planningPath == "" {
		planningPath = defaultPlanningRepo
	}
	if strings.HasPrefix(planningPath, "~/") {
		planningPath = filepath.Join(homeDir, planningPath[2:])
	}
	return planningPath, nil
}

func prepareContributorPlanningRepo(planningPath string) error {
	if _, err := os.Stat(planningPath); !os.IsNotExist(err) {
		fmt.Printf("%s Using existing planning repository\n", ui.RenderPass("✓"))
		return nil
	}
	fmt.Printf("\nCreating planning repository at %s\n", ui.RenderAccent(planningPath))
	if err := os.MkdirAll(planningPath, 0750); err != nil {
		return fmt.Errorf("failed to create planning repo directory: %w", err)
	}
	cmd := exec.Command("git", "init")
	cmd.Dir = planningPath
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to initialize git in planning repo: %w", err)
	}
	beadsDir := filepath.Join(planningPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0750); err != nil {
		return fmt.Errorf("failed to create .beads in planning repo: %w", err)
	}
	writeContributorPlanningReadme(planningPath)
	commitContributorPlanningRepo(planningPath)
	fmt.Printf("%s Planning repository created\n", ui.RenderPass("✓"))
	return nil
}

func writeContributorPlanningReadme(planningPath string) {
	readmePath := filepath.Join(planningPath, "README.md")
	readmeContent := `# Beads Planning Repository

This repository stores contributor planning issues for OSS projects.

## Purpose

- Keep experimental planning separate from upstream PRs
- Track discovered work and implementation notes
- Maintain private todos and design exploration

## Usage

Issues here are automatically created when working on forked repositories.

Created by: bd init --contributor
`
	// #nosec G306 -- README should be world-readable
	if err := os.WriteFile(readmePath, []byte(readmeContent), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to create README: %v\n", err)
	}
}

func commitContributorPlanningRepo(planningPath string) {
	cmd := exec.Command("git", "add", ".")
	cmd.Dir = planningPath
	_ = cmd.Run()
	cmd = exec.Command("git", "commit", "-m", "Initial commit: beads planning repository")
	cmd.Dir = planningPath
	_ = cmd.Run()
}

func configureContributorRouting(ctx context.Context, store storage.DoltStorage, planningPath string, isFork bool) error {
	fmt.Printf("\n%s Configuring contributor auto-routing...\n", ui.RenderAccent("▶"))
	if err := setContributorRouting(ctx, store, planningPath); err != nil {
		return err
	}
	fmt.Printf("%s Auto-routing enabled\n", ui.RenderPass("✓"))

	fmt.Printf("\n%s Configuring multi-repo hydration...\n", ui.RenderAccent("▶"))
	configPath, err := config.FindConfigYAMLPath()
	if err != nil {
		return fmt.Errorf("failed to find config.yaml: %w", err)
	}
	if err := addContributorRepo(configPath, planningPath); err != nil {
		return err
	}
	fmt.Printf("%s Hydration enabled for planning repo\n", ui.RenderPass("✓"))
	fmt.Println("  Issues from planning repo will appear in 'bd list'")

	if isFork {
		if err := setContributorSyncRemote(ctx, store); err != nil {
			return err
		}
		fmt.Printf("%s Sync configured to pull from upstream (source repo)\n", ui.RenderPass("✓"))
	}
	return nil
}

func setContributorRouting(ctx context.Context, store storage.DoltStorage, planningPath string) error {
	if err := store.SetConfig(ctx, "routing.mode", "auto"); err != nil {
		return fmt.Errorf("failed to set routing mode: %w", err)
	}
	if err := store.SetConfig(ctx, "routing.contributor", planningPath); err != nil {
		return fmt.Errorf("failed to set routing contributor path: %w", err)
	}
	return nil
}

func setContributorSyncRemote(ctx context.Context, store storage.DoltStorage) error {
	if err := store.SetConfig(ctx, "sync.remote", "upstream"); err != nil {
		return fmt.Errorf("failed to set sync remote: %w", err)
	}
	return nil
}

func addContributorRepo(configPath, planningPath string) error {
	if err := config.AddRepo(configPath, planningPath); err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("failed to configure hydration: %w", err)
	}
	return nil
}

func printContributorWizardSummary(planningPath string) {
	fmt.Printf("\n%s %s\n\n", ui.RenderPass("✓"), ui.RenderBold("Contributor setup complete!"))
	fmt.Println("Configuration:")
	fmt.Printf("  Current repo issues: %s\n", ui.RenderAccent(".beads/issues.jsonl"))
	fmt.Printf("  Planning repo issues: %s\n", ui.RenderAccent(filepath.Join(planningPath, ".beads/issues.jsonl")))
	fmt.Println()
	fmt.Println("How it works:")
	fmt.Println("  • Issues you create will route to the planning repo")
	fmt.Println("  • Planning stays out of your PRs to upstream")
	fmt.Println("  • Use 'bd list' to see issues from both repos")
	fmt.Println()
	fmt.Printf("Try it: %s\n", ui.RenderAccent("bd create \"Plan feature X\" -p 2"))
	fmt.Println()
}

// autoConfigureForkContributor configures contributor routing when bd init
// detects a fork (upstream remote present) and routing is not yet set.
// Non-interactive and idempotent. roleFlag is the --role flag value (if any).
func autoConfigureForkContributor(ctx context.Context, store storage.DoltStorage, quiet bool, roleFlag string) error {
	isFork, upstreamURL := detectForkSetup()
	if !isFork {
		return nil
	}

	// Explicit --role=maintainer on a fork: acknowledge fork, skip routing.
	if roleFlag == "maintainer" {
		reportMaintainerFork(upstreamURL, quiet)
		return nil
	}

	// Already configured: idempotent re-init.
	configured := configuredContributorRouting(ctx, store)
	if configured.path != "" {
		reportExistingContributorRouting(upstreamURL, configured.path, configured.fromConfigYAML, quiet)
		return nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}
	planningPath := filepath.Join(homeDir, ".beads-planning")

	createdPlanning, err := createForkContributorPlanningRepo(ctx, planningPath)
	if err != nil {
		return err
	}

	if err := setAutoContributorRouting(ctx, store, planningPath); err != nil {
		return err
	}

	_ = exec.Command("git", "config", "beads.role", "contributor").Run()
	configureContributorHydration(planningPath)
	reportAutoContributorRouting(upstreamURL, planningPath, createdPlanning, quiet)
	return nil
}

func reportMaintainerFork(upstreamURL string, quiet bool) {
	if quiet {
		return
	}
	fmt.Printf("\n  %s Fork detected (upstream: %s)\n", ui.RenderWarn("⚠"), upstreamURL)
	fmt.Printf("    Contributor routing skipped (--role=maintainer).\n")
	fmt.Printf("    To set up contributor routing later: bd init --contributor\n")
}

type contributorRoutingConfig struct {
	path           string
	fromConfigYAML bool
}

func configuredContributorRouting(ctx context.Context, store storage.DoltStorage) contributorRoutingConfig {
	if existing, err := store.GetConfig(ctx, "routing.contributor"); err == nil && existing != "" {
		return contributorRoutingConfig{path: existing}
	}
	if config.GetValueSource("routing.contributor") != config.SourceDefault {
		return contributorRoutingConfig{
			path:           strings.TrimSpace(config.GetString("routing.contributor")),
			fromConfigYAML: true,
		}
	}
	return contributorRoutingConfig{}
}

func reportExistingContributorRouting(upstreamURL, existing string, fromConfigYAML, quiet bool) {
	if quiet {
		return
	}
	fmt.Printf("\n  %s Fork detected (upstream: %s)\n", ui.RenderWarn("⚠"), upstreamURL)
	if fromConfigYAML {
		fmt.Printf("    Contributor routing configured via config.yaml → %s\n", existing)
	} else {
		fmt.Printf("    Contributor routing already configured → %s\n", existing)
	}
	fmt.Printf("    Skipping auto-setup. To reconfigure: bd init --contributor\n")
}

func createForkContributorPlanningRepo(ctx context.Context, planningPath string) (bool, error) {
	if _, err := os.Stat(planningPath); !os.IsNotExist(err) {
		return false, nil
	}
	if err := os.MkdirAll(planningPath, 0750); err != nil {
		return false, fmt.Errorf("failed to create planning repo: %w", err)
	}
	gitInit := exec.Command("git", "init")
	gitInit.Dir = planningPath
	if err := gitInit.Run(); err != nil {
		return false, fmt.Errorf("failed to init git in planning repo: %w", err)
	}
	planningBeadsDir := filepath.Join(planningPath, ".beads")
	if err := os.MkdirAll(planningBeadsDir, 0750); err != nil {
		return false, fmt.Errorf("failed to create .beads in planning repo: %w", err)
	}
	initializeContributorPlanningStore(ctx, planningBeadsDir)
	return true, nil
}

func initializeContributorPlanningStore(ctx context.Context, planningBeadsDir string) {
	if planningStore, storeErr := newDoltStoreFromConfig(ctx, planningBeadsDir); storeErr == nil {
		_ = planningStore.Close()
	}
}

func setAutoContributorRouting(ctx context.Context, store storage.DoltStorage, planningPath string) error {
	if err := setContributorRoutingConfig(ctx, store, planningPath); err != nil {
		return err
	}
	if err := store.SetConfig(ctx, "sync.remote", "upstream"); err != nil {
		return fmt.Errorf("failed to set sync.remote: %w", err)
	}
	return nil
}

func setContributorRoutingConfig(ctx context.Context, store storage.DoltStorage, planningPath string) error {
	if err := store.SetConfig(ctx, "routing.mode", "auto"); err != nil {
		return fmt.Errorf("failed to set routing.mode: %w", err)
	}
	if err := store.SetConfig(ctx, "routing.contributor", planningPath); err != nil {
		return fmt.Errorf("failed to set routing.contributor: %w", err)
	}
	return nil
}

func configureContributorHydration(planningPath string) {
	configPath, err := config.FindConfigYAMLPath()
	if err != nil {
		return
	}
	if err := config.AddRepo(configPath, planningPath); err == nil || strings.Contains(err.Error(), "already exists") {
		return
	}
}

func reportAutoContributorRouting(upstreamURL, planningPath string, createdPlanning, quiet bool) {
	if quiet {
		return
	}
	fmt.Printf("\n%s Fork detected — configuring contributor routing\n", ui.RenderAccent("▶"))
	fmt.Printf("  upstream: %s\n\n", upstreamURL)
	fmt.Printf("  %s Planning repo: %s\n", ui.RenderPass("✓"), planningPath)
	fmt.Printf("  %s Issues will route to planning repo (routing.mode=auto)\n", ui.RenderPass("✓"))
	fmt.Printf("  %s Sync remote set to upstream\n", ui.RenderPass("✓"))
	if createdPlanning {
		fmt.Printf("  %s Added .beads/ to planning repo\n", ui.RenderPass("✓"))
	}
	fmt.Printf("\n  To use maintainer mode instead: bd init --role=maintainer\n")
}

// detectForkSetup checks if we're in a fork by looking for upstream remote
func detectForkSetup() (isFork bool, upstreamURL string) {
	cmd := exec.Command("git", "remote", "get-url", "upstream")
	output, err := cmd.Output()
	if err != nil {
		// No upstream remote found
		return false, ""
	}

	upstreamURL = strings.TrimSpace(string(output))
	return true, upstreamURL
}

// checkPushAccess determines if we have push access to origin
func checkPushAccess() (hasPush bool, originURL string) {
	// Get origin URL
	cmd := exec.Command("git", "remote", "get-url", "origin")
	output, err := cmd.Output()
	if err != nil {
		return false, ""
	}

	originURL = strings.TrimSpace(string(output))

	// SSH URLs indicate likely push access (git@github.com:...)
	if strings.HasPrefix(originURL, "git@") {
		return true, originURL
	}

	// HTTPS URLs typically indicate read-only clone
	if strings.HasPrefix(originURL, "https://") {
		return false, originURL
	}

	// Other protocols (file://, etc.) assume push access
	return true, originURL
}

func applyContributorSetup(ctx context.Context, store storage.DoltStorage, opts contributorWizardOpts) error {
	planningPath, err := resolveContributorPlanningPath(opts)
	if err != nil {
		return err
	}
	if err := ensurePlanningRepo(ctx, planningPath); err != nil {
		return err
	}
	if err := setContributorRoutingConfig(ctx, store, planningPath); err != nil {
		return err
	}
	if isFork, _ := detectForkSetup(); isFork {
		if err := setContributorSyncConfig(ctx, store); err != nil {
			return err
		}
	}
	configureContributorHydration(planningPath)
	if !opts.Quiet {
		printContributorRoutingConfigured(planningPath)
	}
	return nil
}

func resolveContributorPlanningPath(opts contributorWizardOpts) (string, error) {
	planningPath := strings.TrimSpace(opts.PlanningRepo)
	if planningPath == "" {
		planningPath = os.Getenv("BEADS_DIR")
	}
	if planningPath != "" {
		return planningPath, nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".beads-planning"), nil
}

func setContributorSyncConfig(ctx context.Context, store storage.DoltStorage) error {
	if err := store.SetConfig(ctx, "sync.remote", "upstream"); err != nil {
		return fmt.Errorf("failed to set sync.remote: %w", err)
	}
	return nil
}

func printContributorRoutingConfigured(planningPath string) {
	fmt.Printf("\n%s Contributor routing configured\n", ui.RenderAccent("▶"))
	fmt.Printf("  %s Planning repo: %s\n", ui.RenderPass("✓"), planningPath)
	fmt.Printf("  %s Issues will route to planning repo (routing.mode=auto)\n", ui.RenderPass("✓"))
}

func ensurePlanningRepo(ctx context.Context, planningPath string) error {
	if _, err := os.Stat(planningPath); os.IsNotExist(err) {
		if err := os.MkdirAll(planningPath, 0750); err != nil {
			return fmt.Errorf("failed to create planning repo: %w", err)
		}
		gitInit := exec.Command("git", "init")
		gitInit.Dir = planningPath
		if err := gitInit.Run(); err != nil {
			return fmt.Errorf("failed to init git in planning repo: %w", err)
		}
	}
	planningBeadsDir := filepath.Join(planningPath, ".beads")
	if err := os.MkdirAll(planningBeadsDir, 0750); err != nil {
		return fmt.Errorf("failed to create .beads in planning repo: %w", err)
	}
	// Initialize the planning Dolt schema so later commands can open the store.
	// Non-fatal: no-CGO and server-mode builds skip this silently.
	if planningStore, storeErr := newDoltStoreFromConfig(ctx, planningBeadsDir); storeErr == nil {
		_ = planningStore.Close()
	}
	return nil
}
