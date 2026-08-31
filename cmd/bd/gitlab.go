// Package main provides the bd CLI commands.
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/gitlab"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/spf13/cobra"
)

// GitLabConfig holds GitLab connection configuration.
type GitLabConfig struct {
	URL              string // GitLab instance URL (e.g., "https://gitlab.com")
	Token            string // Personal access token
	ProjectID        string // Project ID or URL-encoded path
	GroupID          string // Optional group ID for group-level issue fetching
	DefaultProjectID string // Project ID for creating issues in group mode
}

// gitlabCmd is the root command for GitLab operations.
var gitlabCmd = &cobra.Command{
	Use:   "gitlab",
	Short: "GitLab integration commands",
	Long: `Commands for syncing issues between beads and GitLab.

Configuration can be set via 'bd config' or environment variables:
  gitlab.url / GITLAB_URL                         - GitLab instance URL
  gitlab.token / GITLAB_TOKEN                     - Personal access token
  gitlab.project_id / GITLAB_PROJECT_ID           - Project ID or path
  gitlab.group_id / GITLAB_GROUP_ID               - Group ID for group-level sync
  gitlab.default_project_id / GITLAB_DEFAULT_PROJECT_ID - Project for creating issues in group mode`,
}

// gitlabSyncCmd synchronizes issues between beads and GitLab.
var gitlabSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync issues with GitLab",
	Long: `Synchronize issues between beads and GitLab.

By default, performs bidirectional sync:
- Pulls new/updated issues from GitLab to beads
- Pushes local beads issues to GitLab

Use --pull-only or --push-only to limit direction.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGitLabSync,
}

// gitlabStatusCmd displays GitLab configuration and sync status.
var gitlabStatusCmd = &cobra.Command{
	Use:           "status",
	Short:         "Show GitLab sync status",
	Long:          `Display current GitLab configuration and sync status.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGitLabStatus,
}

// gitlabProjectsCmd lists accessible GitLab projects.
var gitlabProjectsCmd = &cobra.Command{
	Use:           "projects",
	Short:         "List accessible GitLab projects",
	Long:          `List GitLab projects that the configured token has access to.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGitLabProjects,
}

// ConflictStrategy defines how to resolve conflicts between local and GitLab versions.
type ConflictStrategy string

const (
	// ConflictStrategyPreferNewer uses the most recently updated version (default).
	ConflictStrategyPreferNewer ConflictStrategy = "prefer-newer"
	// ConflictStrategyPreferLocal always keeps the local beads version.
	ConflictStrategyPreferLocal ConflictStrategy = "prefer-local"
	// ConflictStrategyPreferGitLab always uses the GitLab version.
	ConflictStrategyPreferGitLab ConflictStrategy = "prefer-gitlab"
)

// getConflictStrategy determines the conflict strategy from flag values.
// Returns error if multiple conflicting flags are set.
func getConflictStrategy(preferLocal, preferGitLab, preferNewer bool) (ConflictStrategy, error) {
	flagsSet := 0
	if preferLocal {
		flagsSet++
	}
	if preferGitLab {
		flagsSet++
	}
	if preferNewer {
		flagsSet++
	}
	if flagsSet > 1 {
		return "", fmt.Errorf("cannot use multiple conflict resolution flags")
	}

	if preferLocal {
		return ConflictStrategyPreferLocal, nil
	}
	if preferGitLab {
		return ConflictStrategyPreferGitLab, nil
	}
	return ConflictStrategyPreferNewer, nil
}

// generateIssueID creates a unique issue ID with the given prefix.
// It combines a timestamp with a cryptographically random nonce so IDs remain
// independent between processes and concurrent sync operations.
func generateIssueID(prefix string) string {
	timestamp := time.Now().UnixNano() / 1000000 // milliseconds
	randomCounter := make([]byte, 8)
	_, _ = rand.Read(randomCounter)
	counter := binary.BigEndian.Uint64(randomCounter)
	randBytes := make([]byte, 4)
	_, _ = rand.Read(randBytes)
	return fmt.Sprintf("%s-%d-%d-%x", prefix, timestamp, counter, randBytes)
}

// parseGitLabSourceSystem parses a source system string like "gitlab:123:42"
// Returns projectID, iid, and ok (whether it's a valid GitLab source).
func parseGitLabSourceSystem(sourceSystem string) (projectID, iid int, ok bool) {
	if !strings.HasPrefix(sourceSystem, "gitlab:") {
		return 0, 0, false
	}

	parts := strings.Split(sourceSystem, ":")
	if len(parts) != 3 {
		return 0, 0, false
	}

	var err error
	projectID, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}

	iid, err = strconv.Atoi(parts[2])
	if err != nil {
		return 0, 0, false
	}

	return projectID, iid, true
}

func init() {
	// Add subcommands to gitlab
	gitlabCmd.AddCommand(gitlabSyncCmd)
	gitlabCmd.AddCommand(gitlabStatusCmd)
	gitlabCmd.AddCommand(gitlabProjectsCmd)

	// Add flags to sync command
	gitlabSyncCmd.Flags().Bool("dry-run", false, "Show what would be synced without making changes")
	gitlabSyncCmd.Flags().Bool("pull-only", false, "Only pull issues from GitLab")
	gitlabSyncCmd.Flags().Bool("push-only", false, "Only push issues to GitLab")

	// Conflict resolution flags (mutually exclusive)
	gitlabSyncCmd.Flags().Bool("prefer-local", false, "On conflict, keep local beads version")
	gitlabSyncCmd.Flags().Bool("prefer-gitlab", false, "On conflict, use GitLab version")
	gitlabSyncCmd.Flags().Bool("prefer-newer", false, "On conflict, use most recent version (default)")

	// Filter flags (override config defaults)
	gitlabSyncCmd.Flags().String("label", "", "Filter by labels (comma-separated, AND logic)")
	gitlabSyncCmd.Flags().String("project", "", "Filter to issues from this project ID (group mode)")
	gitlabSyncCmd.Flags().String("milestone", "", "Filter by milestone title")
	gitlabSyncCmd.Flags().String("assignee", "", "Filter by assignee username")
	registerSelectiveSyncFlags(gitlabSyncCmd)

	// Type filtering flags
	gitlabSyncCmd.Flags().String("type", "", "Only sync these issue types (comma-separated, e.g. 'epic,feature,task')")
	gitlabSyncCmd.Flags().String("exclude-type", "", "Exclude these issue types from sync (comma-separated)")
	gitlabSyncCmd.Flags().Bool("no-ephemeral", true, "Exclude ephemeral/wisp issues from push (default: true)")

	// Register gitlab command with root
	rootCmd.AddCommand(gitlabCmd)
}

// getGitLabConfig returns GitLab configuration from bd config or environment.
func getGitLabConfig() GitLabConfig {
	ctx := context.Background()
	config := GitLabConfig{}

	config.URL = getGitLabConfigValue(ctx, "gitlab.url")
	config.Token = getGitLabConfigValue(ctx, "gitlab.token")
	config.ProjectID = getGitLabConfigValue(ctx, "gitlab.project_id")
	config.GroupID = getGitLabConfigValue(ctx, "gitlab.group_id")
	config.DefaultProjectID = getGitLabConfigValue(ctx, "gitlab.default_project_id")

	return config
}

// getGitLabConfigValue reads a GitLab configuration value from store or environment.
func getGitLabConfigValue(ctx context.Context, key string) string {
	if config.IsYamlOnlyKey(key) {
		return getGitLabYAMLConfigValue(key)
	}
	if value := getGitLabStoreConfigValue(ctx, key); value != "" {
		return value
	}
	return getGitLabEnvironmentConfigValue(key)
}

func getGitLabYAMLConfigValue(key string) string {
	// Secret/yaml-only keys (e.g. gitlab.token) live in config.yaml, not the
	// Dolt database, to avoid leaking secrets when the DB is pushed to remotes.
	// Read them from config.yaml first, then env, and never touch the store.
	// Mirrors internal/gitlab/tracker.go getConfig after upstream 99653e059.
	if value := config.GetString(key); value != "" {
		return value
	}
	if envKey := gitlabConfigToEnvVar(key); envKey != "" {
		return os.Getenv(envKey)
	}
	return ""
}

func getGitLabStoreConfigValue(ctx context.Context, key string) string {
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

func getGitLabEnvironmentConfigValue(key string) string {
	envKey := gitlabConfigToEnvVar(key)
	if envKey == "" {
		return ""
	}
	return os.Getenv(envKey)
}

// gitlabConfigToEnvVar maps GitLab config keys to their environment variable names.
func gitlabConfigToEnvVar(key string) string {
	switch key {
	case "gitlab.url":
		return "GITLAB_URL"
	case "gitlab.token":
		return "GITLAB_TOKEN"
	case "gitlab.project_id":
		return "GITLAB_PROJECT_ID"
	case "gitlab.group_id":
		return "GITLAB_GROUP_ID"
	case "gitlab.default_project_id":
		return "GITLAB_DEFAULT_PROJECT_ID"
	default:
		return gitlabFilterConfigToEnvVar(key)
	}
}

func gitlabFilterConfigToEnvVar(key string) string {
	switch key {
	case "gitlab.filter_labels":
		return "GITLAB_FILTER_LABELS"
	case "gitlab.filter_project":
		return "GITLAB_FILTER_PROJECT"
	case "gitlab.filter_milestone":
		return "GITLAB_FILTER_MILESTONE"
	case "gitlab.filter_assignee":
		return "GITLAB_FILTER_ASSIGNEE"
	default:
		return ""
	}
}

// validateGitLabConfig checks that required configuration is present.
func validateGitLabConfig(config GitLabConfig) error {
	if config.URL == "" {
		return fmt.Errorf("gitlab.url is not configured. Set via 'bd config set gitlab.url <url>' or GITLAB_URL environment variable")
	}
	if config.Token == "" {
		return fmt.Errorf("gitlab.token is not configured. Set via 'bd config set gitlab.token <token>' or GITLAB_TOKEN environment variable")
	}
	if config.ProjectID == "" && config.GroupID == "" {
		return fmt.Errorf("gitlab.project_id or gitlab.group_id is not configured. Set via 'bd config' or environment variables")
	}
	// Reject non-HTTPS URLs to prevent sending tokens in cleartext.
	// Allow http://localhost and http://127.0.0.1 for local development/testing.
	if strings.HasPrefix(config.URL, "http://") &&
		!strings.HasPrefix(config.URL, "http://localhost") &&
		!strings.HasPrefix(config.URL, "http://127.0.0.1") {
		return fmt.Errorf("gitlab.url must use HTTPS (got %q). Use HTTPS to protect your access token", config.URL)
	}
	return nil
}

// maskGitLabToken masks a token for safe display.
// Shows only the first 4 characters to aid identification without
// revealing enough to reduce brute-force entropy.
func maskGitLabToken(token string) string {
	if token == "" {
		return "(not set)"
	}
	if len(token) <= 4 {
		return "****"
	}
	return token[:4] + "****"
}

// getGitLabClient creates a GitLab client from the current configuration.
func getGitLabClient(config GitLabConfig) *gitlab.Client {
	client := gitlab.NewClient(config.Token, config.URL, config.ProjectID)
	if config.GroupID != "" {
		client = client.WithGroupID(config.GroupID)
	}
	return client
}

// runGitLabStatus implements the gitlab status command.
func runGitLabStatus(cmd *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("gitlab status is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("gitlab-status")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	renderGitLabStatus(cmd.OutOrStdout(), getGitLabConfig())
	return nil
}

func renderGitLabStatus(out io.Writer, gitlabConfig GitLabConfig) {
	_, _ = fmt.Fprintln(out, "GitLab Configuration")
	_, _ = fmt.Fprintln(out, "====================")
	_, _ = fmt.Fprintf(out, "URL:        %s\n", gitlabConfig.URL)
	_, _ = fmt.Fprintf(out, "Token:      %s\n", maskGitLabToken(gitlabConfig.Token))
	_, _ = fmt.Fprintf(out, "Project ID: %s\n", gitlabConfig.ProjectID)
	renderGitLabStatusMode(out, gitlabConfig)
	renderGitLabStatusFilters(out)
	renderGitLabStatusValidation(out, gitlabConfig)
}

func renderGitLabStatusMode(out io.Writer, gitlabConfig GitLabConfig) {
	if gitlabConfig.GroupID == "" {
		_, _ = fmt.Fprintf(out, "Sync Mode:  project\n")
		return
	}
	_, _ = fmt.Fprintf(out, "Group ID:   %s\n", gitlabConfig.GroupID)
	_, _ = fmt.Fprintf(out, "Sync Mode:  group (fetches from all projects in group)\n")
	if gitlabConfig.DefaultProjectID != "" {
		_, _ = fmt.Fprintf(out, "Default Project ID: %s (for creating new issues)\n", gitlabConfig.DefaultProjectID)
	}
}

func renderGitLabStatusFilters(out io.Writer) {
	ctx := context.Background()
	filterLabels := getGitLabConfigValue(ctx, "gitlab.filter_labels")
	filterProject := getGitLabConfigValue(ctx, "gitlab.filter_project")
	filterMilestone := getGitLabConfigValue(ctx, "gitlab.filter_milestone")
	filterAssignee := getGitLabConfigValue(ctx, "gitlab.filter_assignee")
	if filterLabels == "" && filterProject == "" && filterMilestone == "" && filterAssignee == "" {
		return
	}
	_, _ = fmt.Fprintf(out, "\nFilters:\n")
	if filterLabels != "" {
		_, _ = fmt.Fprintf(out, "  Labels:    %s\n", filterLabels)
	}
	if filterProject != "" {
		_, _ = fmt.Fprintf(out, "  Project:   %s\n", filterProject)
	}
	if filterMilestone != "" {
		_, _ = fmt.Fprintf(out, "  Milestone: %s\n", filterMilestone)
	}
	if filterAssignee != "" {
		_, _ = fmt.Fprintf(out, "  Assignee:  %s\n", filterAssignee)
	}
}

func renderGitLabStatusValidation(out io.Writer, gitlabConfig GitLabConfig) {
	if err := validateGitLabConfig(gitlabConfig); err != nil {
		_, _ = fmt.Fprintf(out, "\nStatus: ❌ Not configured\n")
		_, _ = fmt.Fprintf(out, "Error: %v\n", err)
		return
	}
	_, _ = fmt.Fprintf(out, "\nStatus: ✓ Configured\n")
}

// runGitLabProjects implements the gitlab projects command.
func runGitLabProjects(cmd *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("gitlab projects is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("gitlab-projects")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	config := getGitLabConfig()
	if err := validateGitLabConfig(config); err != nil {
		return HandleError("%v", err)
	}

	out := cmd.OutOrStdout()
	client := getGitLabClient(config)
	ctx := context.Background()

	projects, err := client.ListProjects(ctx)
	if err != nil {
		return HandleError("failed to fetch projects: %v", err)
	}

	_, _ = fmt.Fprintln(out, "Accessible GitLab Projects")
	_, _ = fmt.Fprintln(out, "==========================")
	for _, p := range projects {
		_, _ = fmt.Fprintf(out, "ID: %d\n", p.ID)
		_, _ = fmt.Fprintf(out, "  Name: %s\n", p.Name)
		_, _ = fmt.Fprintf(out, "  Path: %s\n", p.PathWithNamespace)
		_, _ = fmt.Fprintf(out, "  URL:  %s\n", p.WebURL)
		_, _ = fmt.Fprintln(out)
	}

	if len(projects) == 0 {
		_, _ = fmt.Fprintln(out, "No projects found (or no membership access)")
	}

	return nil
}
