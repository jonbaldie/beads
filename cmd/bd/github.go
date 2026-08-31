// Package main provides the bd CLI commands.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/github"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/spf13/cobra"
)

// GitHubConfig holds GitHub connection configuration.
type GitHubConfig struct {
	Token      string // Personal access token
	Owner      string // Repository owner (user or organization)
	Repo       string // Repository name
	Repository string // Combined "owner/repo" format
	URL        string // Custom API URL (for GitHub Enterprise)
}

// githubCmd is the root command for GitHub operations.
var githubCmd = &cobra.Command{
	Use:   "github",
	Short: "GitHub integration commands",
	Long: `Commands for syncing issues between beads and GitHub.

Configuration can be set via 'bd config' or environment variables:
  github.token / GITHUB_TOKEN           - Personal access token
  github.owner / GITHUB_OWNER           - Repository owner
  github.repo / GITHUB_REPO             - Repository name
  github.repository / GITHUB_REPOSITORY - Combined "owner/repo" format
  github.url / GITHUB_API_URL           - Custom API URL (GitHub Enterprise)`,
}

// githubSyncCmd synchronizes issues between beads and GitHub.
var githubSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync issues with GitHub",
	Long: `Synchronize issues between beads and GitHub.

By default, performs bidirectional sync:
- Pulls new/updated issues from GitHub to beads
- Pushes local beads issues to GitHub

Use --pull-only or --push-only to limit direction.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGitHubSync,
}

// githubStatusCmd displays GitHub configuration and sync status.
var githubStatusCmd = &cobra.Command{
	Use:           "status",
	Short:         "Show GitHub sync status",
	Long:          `Display current GitHub configuration and sync status.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGitHubStatus,
}

// githubReposCmd lists accessible GitHub repositories.
var githubReposCmd = &cobra.Command{
	Use:           "repos",
	Short:         "List accessible GitHub repositories",
	Long:          `List GitHub repositories that the configured token has access to.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGitHubRepos,
}

type githubSyncOptions struct {
	dryRun       bool
	pullOnly     bool
	pushOnly     bool
	preferLocal  bool
	preferGitHub bool
	preferNewer  bool
}

func githubSyncOptionsFromCommand(cmd *cobra.Command) githubSyncOptions {
	return githubSyncOptions{
		dryRun:       commandBoolFlag(cmd, "dry-run"),
		pullOnly:     commandBoolFlag(cmd, "pull-only"),
		pushOnly:     commandBoolFlag(cmd, "push-only"),
		preferLocal:  commandBoolFlag(cmd, "prefer-local"),
		preferGitHub: commandBoolFlag(cmd, "prefer-github"),
		preferNewer:  commandBoolFlag(cmd, "prefer-newer"),
	}
}

// GitHubConflictStrategy defines how to resolve conflicts between local and GitHub versions.
type GitHubConflictStrategy string

const (
	// GitHubConflictPreferNewer uses the most recently updated version (default).
	GitHubConflictPreferNewer GitHubConflictStrategy = "prefer-newer"
	// GitHubConflictPreferLocal always keeps the local beads version.
	GitHubConflictPreferLocal GitHubConflictStrategy = "prefer-local"
	// GitHubConflictPreferGitHub always uses the GitHub version.
	GitHubConflictPreferGitHub GitHubConflictStrategy = "prefer-github"
)

// getGitHubConflictStrategy determines the conflict strategy from flag values.
// Returns error if multiple conflicting flags are set.
func getGitHubConflictStrategy(preferLocal, preferGitHub, preferNewer bool) (GitHubConflictStrategy, error) {
	flagsSet := 0
	if preferLocal {
		flagsSet++
	}
	if preferGitHub {
		flagsSet++
	}
	if preferNewer {
		flagsSet++
	}
	if flagsSet > 1 {
		return "", fmt.Errorf("cannot use multiple conflict resolution flags")
	}

	if preferLocal {
		return GitHubConflictPreferLocal, nil
	}
	if preferGitHub {
		return GitHubConflictPreferGitHub, nil
	}
	return GitHubConflictPreferNewer, nil
}

// parseGitHubSourceSystem parses a source system string like "github:https://...:42"
// Returns the issue number and ok (whether it's a valid GitHub source).
func parseGitHubSourceSystem(sourceSystem string) (number int, ok bool) {
	if !strings.HasPrefix(sourceSystem, "github:") {
		return 0, false
	}

	// Find last ":" and parse the number after it
	lastColon := strings.LastIndex(sourceSystem, ":")
	if lastColon == -1 || lastColon == len(sourceSystem)-1 {
		return 0, false
	}

	var err error
	number, err = strconv.Atoi(sourceSystem[lastColon+1:])
	if err != nil {
		return 0, false
	}

	return number, true
}

func init() {
	// Add subcommands to github
	githubCmd.AddCommand(githubSyncCmd)
	githubCmd.AddCommand(githubStatusCmd)
	githubCmd.AddCommand(githubReposCmd)

	// Add flags to sync command
	githubSyncCmd.Flags().Bool("dry-run", false, "Show what would be synced without making changes")
	githubSyncCmd.Flags().Bool("pull-only", false, "Only pull issues from GitHub")
	githubSyncCmd.Flags().Bool("push-only", false, "Only push issues to GitHub")

	// Conflict resolution flags (mutually exclusive)
	githubSyncCmd.Flags().Bool("prefer-local", false, "On conflict, keep local beads version")
	githubSyncCmd.Flags().Bool("prefer-github", false, "On conflict, use GitHub version")
	githubSyncCmd.Flags().Bool("prefer-newer", false, "On conflict, use most recent version (default)")
	registerSelectiveSyncFlags(githubSyncCmd)

	// Register github command with root
	rootCmd.AddCommand(githubCmd)
}

// getGitHubConfig returns GitHub configuration from bd config or environment.
func getGitHubConfig() GitHubConfig {
	ctx := context.Background()
	config := GitHubConfig{}

	config.Token = getGitHubConfigValue(ctx, "github.token")
	config.Owner = getGitHubConfigValue(ctx, "github.owner")
	config.Repo = getGitHubConfigValue(ctx, "github.repo")
	config.Repository = getGitHubConfigValue(ctx, "github.repository")
	config.URL = getGitHubConfigValue(ctx, "github.url")

	// Parse combined owner/repo format if individual fields are empty
	if (config.Owner == "" || config.Repo == "") && config.Repository != "" {
		parts := strings.SplitN(config.Repository, "/", 2)
		if len(parts) == 2 {
			if config.Owner == "" {
				config.Owner = parts[0]
			}
			if config.Repo == "" {
				config.Repo = parts[1]
			}
		}
	}

	return config
}

// getGitHubConfigValue reads a GitHub configuration value from store or environment.
func getGitHubConfigValue(ctx context.Context, key string) string {
	// Secret keys (e.g. github.token) are stored in config.yaml, not the
	// Dolt database, to avoid leaking secrets when pushing to remotes.
	if config.IsYamlOnlyKey(key) {
		return getGitHubYAMLConfigValue(key)
	}

	if value := getGitHubStoreConfigValue(ctx, key); value != "" {
		return value
	}
	return getGitHubEnvironmentValue(key)
}

func getGitHubYAMLConfigValue(key string) string {
	if value := config.GetString(key); value != "" {
		return value
	}
	return getGitHubEnvironmentValue(key)
}

func getGitHubStoreConfigValue(ctx context.Context, key string) string {
	if getStore() != nil {
		value, _ := getStore().GetConfig(ctx, key)
		return value
	}

	if getDBPath() == "" {
		return ""
	}
	tempStore, err := openReadOnlyStoreForDBPath(ctx, getDBPath())
	if err != nil {
		return ""
	}
	defer func() { _ = tempStore.Close() }()
	value, _ := tempStore.GetConfig(ctx, key)
	return value
}

func getGitHubEnvironmentValue(key string) string {
	envKey := githubConfigToEnvVar(key)
	if envKey == "" {
		return ""
	}
	return os.Getenv(envKey)
}

// githubConfigToEnvVar maps GitHub config keys to their environment variable names.
func githubConfigToEnvVar(key string) string {
	switch key {
	case "github.token":
		return "GITHUB_TOKEN"
	case "github.owner":
		return "GITHUB_OWNER"
	case "github.repo":
		return "GITHUB_REPO"
	case "github.repository":
		return "GITHUB_REPOSITORY"
	case "github.url":
		return "GITHUB_API_URL"
	default:
		return ""
	}
}

// validateGitHubConfig checks that required configuration is present.
func validateGitHubConfig(config GitHubConfig) error {
	if config.Token == "" {
		return fmt.Errorf("github.token is not configured. Set via 'bd config set github.token <token>' or GITHUB_TOKEN environment variable")
	}
	if config.Owner == "" {
		return fmt.Errorf("github.owner is not configured. Set via 'bd config set github.owner <owner>' or GITHUB_OWNER environment variable")
	}
	if config.Repo == "" {
		return fmt.Errorf("github.repo is not configured. Set via 'bd config set github.repo <repo>' or GITHUB_REPO environment variable")
	}
	return nil
}

// maskGitHubToken masks a token for safe display.
// Shows only the first 4 characters to aid identification without
// revealing enough to reduce brute-force entropy.
func maskGitHubToken(token string) string {
	if token == "" {
		return "(not set)"
	}
	if len(token) <= 4 {
		return "****"
	}
	return token[:4] + "****"
}

// getGitHubClient creates a GitHub client from the current configuration.
func getGitHubClient(config GitHubConfig) *github.Client {
	client := github.NewClient(config.Token, config.Owner, config.Repo)
	if config.URL != "" {
		client = client.WithBaseURL(config.URL)
	}
	return client
}

// runGitHubStatus implements the github status command.
func runGitHubStatus(cmd *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("github status is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("github-status")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	config := getGitHubConfig()

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, "GitHub Configuration")
	_, _ = fmt.Fprintln(out, "====================")
	_, _ = fmt.Fprintf(out, "Token:      %s\n", maskGitHubToken(config.Token))
	_, _ = fmt.Fprintf(out, "Owner:      %s\n", config.Owner)
	_, _ = fmt.Fprintf(out, "Repository: %s\n", config.Repo)
	if config.URL != "" {
		_, _ = fmt.Fprintf(out, "API URL:    %s\n", config.URL)
	}

	// Validate configuration
	if err := validateGitHubConfig(config); err != nil {
		_, _ = fmt.Fprintf(out, "\nStatus: ❌ Not configured\n")
		_, _ = fmt.Fprintf(out, "Error: %v\n", err)
		return nil
	}

	_, _ = fmt.Fprintf(out, "\nStatus: ✓ Configured\n")
	return nil
}

// runGitHubRepos implements the github repos command.
func runGitHubRepos(cmd *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("github repos is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("github-repos")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	config := getGitHubConfig()
	if config.Token == "" {
		return HandleError("github.token is not configured. Set via 'bd config set github.token <token>' or GITHUB_TOKEN environment variable")
	}

	out := cmd.OutOrStdout()
	client := getGitHubClient(config)
	ctx := context.Background()

	repos, err := client.ListRepositories(ctx)
	if err != nil {
		return HandleError("failed to fetch repositories: %v", err)
	}

	_, _ = fmt.Fprintln(out, "Accessible GitHub Repositories")
	_, _ = fmt.Fprintln(out, "==============================")
	for _, r := range repos {
		_, _ = fmt.Fprintf(out, "  %s\n", r.FullName)
		if r.Description != "" {
			_, _ = fmt.Fprintf(out, "    %s\n", r.Description)
		}
		_, _ = fmt.Fprintf(out, "    %s\n", r.HTMLURL)
		_, _ = fmt.Fprintln(out)
	}

	if len(repos) == 0 {
		_, _ = fmt.Fprintln(out, "No repositories found")
	}

	return nil
}

// runGitHubSync implements the github sync command.
// Uses the tracker.Engine for all sync operations.
func runGitHubSync(cmd *cobra.Command, _ []string) error {
	opts := githubSyncOptionsFromCommand(cmd)
	if usesProxiedServer() {
		return HandleErrorRespectJSON("github sync is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("github-sync")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	return executeGitHubSync(cmd, opts)
}

func executeGitHubSync(cmd *cobra.Command, opts githubSyncOptions) error {
	conflictStrategy, err := prepareGitHubSync(opts)
	if err != nil {
		return HandleError("%v", err)
	}
	if err := ensureStoreActive(); err != nil {
		return HandleError("database not available: %v", err)
	}

	out := cmd.OutOrStdout()
	ctx := context.Background()
	engine, err := buildGitHubSyncEngine(ctx, out)
	if err != nil {
		return HandleError("initializing GitHub tracker: %v", err)
	}
	syncOpts, err := buildGitHubSyncOptions(cmd, opts, conflictStrategy)
	if err != nil {
		return HandleError("%v", err)
	}
	if opts.dryRun {
		printGitHubSyncDryRun(out)
	}
	result, err := engine.Sync(ctx, syncOpts)
	if err != nil {
		return HandleError("%v", err)
	}
	printGitHubSyncResult(out, result, opts.dryRun)
	return nil
}

func prepareGitHubSync(opts githubSyncOptions) (GitHubConflictStrategy, error) {
	if err := validateGitHubConfig(getGitHubConfig()); err != nil {
		return "", err
	}
	if !opts.dryRun {
		if err := CheckReadonly("github sync"); err != nil {
			return "", err
		}
	}
	if opts.pullOnly && opts.pushOnly {
		return "", fmt.Errorf("cannot use both --pull-only and --push-only")
	}
	strategy, err := getGitHubConflictStrategy(opts.preferLocal, opts.preferGitHub, opts.preferNewer)
	if err != nil {
		return "", fmt.Errorf("%v (--prefer-local, --prefer-github, --prefer-newer)", err)
	}
	return strategy, nil
}

func buildGitHubSyncEngine(ctx context.Context, out io.Writer) (*tracker.Engine, error) {
	gt := &github.Tracker{}
	if err := gt.Init(ctx, getStore()); err != nil {
		return nil, err
	}
	engine := tracker.NewEngine(gt, getStore(), getActor())
	engine.OnMessage = func(msg string) { _, _ = fmt.Fprintln(out, "  "+msg) }
	engine.OnWarning = func(msg string) { _, _ = fmt.Fprintf(os.Stderr, "Warning: %s\n", msg) } //nolint:gosec // G705: CLI stderr, not HTML.
	engine.PullHooks = buildGitHubPullHooks(ctx)
	engine.PushHooks = buildGitHubPushHooks(gt)
	return engine, nil
}

func buildGitHubSyncOptions(cmd *cobra.Command, opts githubSyncOptions, strategy GitHubConflictStrategy) (tracker.SyncOptions, error) {
	push := !opts.pullOnly
	syncOpts := tracker.SyncOptions{
		Pull:   !opts.pushOnly,
		Push:   push,
		DryRun: opts.dryRun,
	}
	if err := applySelectiveSyncFlags(cmd, &syncOpts, push); err != nil {
		return tracker.SyncOptions{}, err
	}
	syncOpts.ConflictResolution = githubTrackerConflictResolution(strategy)
	return syncOpts, nil
}

func githubTrackerConflictResolution(strategy GitHubConflictStrategy) tracker.ConflictResolution {
	switch strategy {
	case GitHubConflictPreferLocal:
		return tracker.ConflictLocal
	case GitHubConflictPreferGitHub:
		return tracker.ConflictExternal
	default:
		return tracker.ConflictTimestamp
	}
}

func printGitHubSyncDryRun(out io.Writer) {
	_, _ = fmt.Fprintln(out, "Dry run mode - no changes will be made")
	_, _ = fmt.Fprintln(out)
}

func printGitHubSyncResult(out io.Writer, result *tracker.SyncResult, dryRun bool) {
	if !dryRun {
		if result.Stats.Pulled > 0 {
			_, _ = fmt.Fprintf(out, "✓ Pulled %d issues (%d created, %d updated)\n",
				result.Stats.Pulled, result.Stats.Created, result.Stats.Updated)
		}
		if result.Stats.Pushed > 0 {
			_, _ = fmt.Fprintf(out, "✓ Pushed %d issues\n", result.Stats.Pushed)
		}
		if result.Stats.Conflicts > 0 {
			_, _ = fmt.Fprintf(out, "→ Resolved %d conflicts\n", result.Stats.Conflicts)
		}
	}
	if dryRun {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "Run without --dry-run to apply changes")
	}
}

// buildGitHubPushHooks creates PushHooks for GitHub-specific push behavior.
// The ContentEqual hook lets the engine skip issues whose pushable fields
// already match GitHub, so repeated `github sync --push-only` / `github push`
// runs don't re-PATCH unchanged issues (gastownhall/beads#4214).
func buildGitHubPushHooks(gt *github.Tracker) *tracker.PushHooks {
	config := gt.MappingConfig()
	if config == nil {
		config = github.DefaultMappingConfig()
	}
	return &tracker.PushHooks{
		ContentEqual: func(local *types.Issue, remote *tracker.TrackerIssue) bool {
			if remote == nil {
				return false
			}
			gh, ok := remote.Raw.(*github.Issue)
			if !ok || gh == nil {
				return false
			}
			return github.PushFieldsEqual(local, gh, config)
		},
		// ContentHash lets the engine skip the per-issue GitHub fetch entirely
		// when an issue is unchanged since its last push, so a no-op
		// `github sync --push-only` makes ~zero REST calls instead of one GET
		// per linked issue (gastownhall/beads#4214).
		ContentHash: func(local *types.Issue) string {
			return github.PushContentHash(local, config)
		},
		// TargetScope supplies the host and repository omitted by shorthand refs
		// such as github:42, so changing GitHub target configuration invalidates
		// the local no-op cache.
		TargetScope: gt.PushTargetScope,
	}
}

// buildGitHubPullHooks creates PullHooks for GitHub-specific pull behavior.
func buildGitHubPullHooks(ctx context.Context) *tracker.PullHooks {
	prefix := "bd"
	// YAML config takes precedence — in shared-server mode the DB
	// may belong to a different project (GH#2469).
	if p := config.GetString("issue-prefix"); p != "" {
		prefix = p
	} else if getStore() != nil {
		if p, err := getStore().GetConfig(ctx, "issue_prefix"); err == nil && p != "" {
			prefix = p
		}
	}

	return &tracker.PullHooks{
		GenerateID: func(_ context.Context, issue *types.Issue) error {
			if issue.ID == "" {
				issue.ID = generateIssueID(prefix)
			}
			return nil
		},
	}
}
