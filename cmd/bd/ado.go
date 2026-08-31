// Package main provides the bd CLI commands.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/ado"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/spf13/cobra"
)

// ADOConfig holds Azure DevOps connection configuration.
type ADOConfig struct {
	PAT      string   // Personal access token
	Org      string   // Organization name
	Project  string   // Primary project name (backward compat)
	Projects []string // All project names
	URL      string   // Custom base URL (for on-prem)
}

type adoSyncFlags struct {
	dryRun              bool
	pullOnly            bool
	pushOnly            bool
	preferLocal         bool
	preferADO           bool
	preferNewer         bool
	bootstrapMatch      bool
	noCreate            bool
	reconcile           bool
	filterAreaPath      string
	filterIterationPath string
	filterTypes         string
	filterStates        string
}

// adoCmd is the root command for Azure DevOps operations.
var adoCmd = &cobra.Command{
	Use:   "ado",
	Short: "Azure DevOps integration commands",
	Long: `Commands for syncing issues between beads and Azure DevOps.

Configuration can be set via 'bd config' or environment variables:
  ado.org / AZURE_DEVOPS_ORG              - Organization name
  ado.project / AZURE_DEVOPS_PROJECT      - Project name (single)
  ado.projects / AZURE_DEVOPS_PROJECTS    - Project names (comma-separated)
  ado.pat / AZURE_DEVOPS_PAT              - Personal access token
  ado.url / AZURE_DEVOPS_URL              - Custom base URL (on-prem)`,
}

// adoSyncCmd synchronizes issues between beads and Azure DevOps.
var adoSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync issues with Azure DevOps",
	Long: `Synchronize issues between beads and Azure DevOps.

By default, performs bidirectional sync:
- Pulls new/updated work items from Azure DevOps to beads
- Pushes local beads issues to Azure DevOps

Use --pull-only or --push-only to limit direction.

Filters (--area-path, --iteration-path, --types, --states) restrict
which work items are synced. On pull, they limit the WIQL query. On push,
--types and --states filter local beads before pushing to ADO. Use
--no-create with push to skip creating new ADO work items (only update
existing linked items). Filters can also be persisted via config:
  ado.filter.area_path, ado.filter.iteration_path,
  ado.filter.types, ado.filter.states
CLI flags override config values when both are set.`,
	RunE: runADOSync,
}

// adoStatusCmd displays Azure DevOps configuration and sync status.
var adoStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show Azure DevOps sync status",
	Long:  `Display current Azure DevOps configuration and sync status.`,
	RunE:  runADOStatus,
}

// adoProjectsCmd lists accessible Azure DevOps projects.
var adoProjectsCmd = &cobra.Command{
	Use:   "projects",
	Short: "List accessible Azure DevOps projects",
	Long:  `List Azure DevOps projects that the configured token has access to.`,
	RunE:  runADOProjects,
}

// ADOConflictStrategy defines how to resolve conflicts between local and ADO versions.
type ADOConflictStrategy string

const (
	// ADOConflictPreferNewer uses the most recently updated version (default).
	ADOConflictPreferNewer ADOConflictStrategy = "prefer-newer"
	// ADOConflictPreferLocal always keeps the local beads version.
	ADOConflictPreferLocal ADOConflictStrategy = "prefer-local"
	// ADOConflictPreferADO always uses the Azure DevOps version.
	ADOConflictPreferADO ADOConflictStrategy = "prefer-ado"
)

// getADOConflictStrategy determines the conflict strategy from flag values.
// Returns error if multiple conflicting flags are set.
func getADOConflictStrategy(preferLocal, preferADO, preferNewer bool) (ADOConflictStrategy, error) {
	flagsSet := 0
	if preferLocal {
		flagsSet++
	}
	if preferADO {
		flagsSet++
	}
	if preferNewer {
		flagsSet++
	}
	if flagsSet > 1 {
		return "", fmt.Errorf("cannot use multiple conflict resolution flags")
	}

	if preferLocal {
		return ADOConflictPreferLocal, nil
	}
	if preferADO {
		return ADOConflictPreferADO, nil
	}
	return ADOConflictPreferNewer, nil
}

func init() {
	// Add subcommands to ado
	adoCmd.AddCommand(adoSyncCmd)
	adoCmd.AddCommand(adoStatusCmd)
	adoCmd.AddCommand(adoProjectsCmd)

	// Add flags to sync command
	adoSyncCmd.Flags().Bool("dry-run", false, "Show what would be synced without making changes")
	adoSyncCmd.Flags().Bool("pull-only", false, "Only pull issues from Azure DevOps")
	adoSyncCmd.Flags().Bool("push-only", false, "Only push issues to Azure DevOps")

	// Conflict resolution flags (mutually exclusive)
	adoSyncCmd.Flags().Bool("prefer-local", false, "On conflict, keep local beads version")
	adoSyncCmd.Flags().Bool("prefer-ado", false, "On conflict, use Azure DevOps version")
	adoSyncCmd.Flags().Bool("prefer-newer", false, "On conflict, use most recent version (default)")

	// Additional sync options
	adoSyncCmd.Flags().Bool("bootstrap-match", false, "Enable heuristic matching for first sync")
	adoSyncCmd.Flags().Bool("no-create", false, "Never create new items in either direction (pull or push)")
	adoSyncCmd.Flags().Bool("reconcile", false, "Force reconciliation scan for deleted items")

	// Pull filter flags (override config keys ado.filter.*)
	adoSyncCmd.Flags().String("area-path", "", "Filter to ADO area path (e.g., \"Project\\Team\")")
	adoSyncCmd.Flags().String("iteration-path", "", "Filter to ADO iteration path (e.g., \"Project\\Sprint 1\")")
	adoSyncCmd.Flags().String("types", "", "Filter to work item types, comma-separated (e.g., \"Bug,Task,User Story\")")
	adoSyncCmd.Flags().String("states", "", "Filter to ADO states, comma-separated (e.g., \"New,Active,Resolved\")")
	adoSyncCmd.Flags().StringSlice("project", nil, "Project name(s) to sync (overrides configured project/projects)")
	registerSelectiveSyncFlags(adoSyncCmd)

	// Register ado command with root
	rootCmd.AddCommand(adoCmd)
}

func adoSyncFlagsFromCommand(cmd *cobra.Command) adoSyncFlags {
	flags := cmd.Flags()
	dryRun, _ := flags.GetBool("dry-run")
	pullOnly, _ := flags.GetBool("pull-only")
	pushOnly, _ := flags.GetBool("push-only")
	preferLocal, _ := flags.GetBool("prefer-local")
	preferADO, _ := flags.GetBool("prefer-ado")
	preferNewer, _ := flags.GetBool("prefer-newer")
	bootstrapMatch, _ := flags.GetBool("bootstrap-match")
	noCreate, _ := flags.GetBool("no-create")
	reconcile, _ := flags.GetBool("reconcile")
	filterAreaPath, _ := flags.GetString("area-path")
	filterIterationPath, _ := flags.GetString("iteration-path")
	filterTypes, _ := flags.GetString("types")
	filterStates, _ := flags.GetString("states")
	return adoSyncFlags{
		dryRun:              dryRun,
		pullOnly:            pullOnly,
		pushOnly:            pushOnly,
		preferLocal:         preferLocal,
		preferADO:           preferADO,
		preferNewer:         preferNewer,
		bootstrapMatch:      bootstrapMatch,
		noCreate:            noCreate,
		reconcile:           reconcile,
		filterAreaPath:      filterAreaPath,
		filterIterationPath: filterIterationPath,
		filterTypes:         filterTypes,
		filterStates:        filterStates,
	}
}

// getADOConfig returns Azure DevOps configuration from bd config or environment.
func getADOConfig() ADOConfig {
	ctx := context.Background()
	cfg := ADOConfig{}

	cfg.PAT = getADOConfigValue(ctx, "ado.pat")
	cfg.Org = getADOConfigValue(ctx, "ado.org")
	cfg.URL = getADOConfigValue(ctx, "ado.url")

	// Resolve projects from all sources.
	pluralVal := getADOConfigValue(ctx, "ado.projects")
	singularVal := getADOConfigValue(ctx, "ado.project")
	cfg.Projects = tracker.ResolveProjectIDs(nil, pluralVal, singularVal)
	if len(cfg.Projects) > 0 {
		cfg.Project = cfg.Projects[0]
	}

	return cfg
}

// getADOConfigValue reads an Azure DevOps configuration value from store or environment.
func getADOConfigValue(ctx context.Context, key string) string {
	// Try to read from store (works in direct mode)
	if getStore() != nil {
		value, _ := getStore().GetConfig(ctx, key)
		if value != "" {
			return value
		}
	} else if getDBPath() != "" {
		tempStore, err := dolt.New(ctx, &dolt.Config{Path: getDBPath()})
		if err == nil {
			defer func() { _ = tempStore.Close() }()
			value, _ := tempStore.GetConfig(ctx, key)
			if value != "" {
				return value
			}
		}
	}

	// Fall back to environment variable
	envKey := adoConfigToEnvVar(key)
	if envKey != "" {
		if value := os.Getenv(envKey); value != "" {
			return value
		}
	}

	return ""
}

// adoConfigToEnvVar maps Azure DevOps config keys to their environment variable names.
func adoConfigToEnvVar(key string) string {
	switch key {
	case "ado.pat":
		return "AZURE_DEVOPS_PAT"
	case "ado.org":
		return "AZURE_DEVOPS_ORG"
	case "ado.project":
		return "AZURE_DEVOPS_PROJECT"
	case "ado.projects":
		return "AZURE_DEVOPS_PROJECTS"
	case "ado.url":
		return "AZURE_DEVOPS_URL"
	default:
		return ""
	}
}

// validateADOConfig checks that required configuration is present.
func validateADOConfig(cfg ADOConfig) error {
	if cfg.PAT == "" {
		return fmt.Errorf("ado.pat not configured: set via 'bd config set ado.pat <token>' or AZURE_DEVOPS_PAT env var")
	}
	if cfg.Org == "" && cfg.URL == "" {
		return fmt.Errorf("ado.org not configured: set via 'bd config set ado.org <org>' or AZURE_DEVOPS_ORG env var")
	}
	if len(cfg.Projects) == 0 {
		return fmt.Errorf("no ADO project configured\nSet via 'bd config set ado.project <project>'\nOr:  'bd config set ado.projects \"proj1,proj2\"'\nOr: AZURE_DEVOPS_PROJECT env var")
	}
	return nil
}

// maskADOToken masks a token for safe display.
// Shows only the first 4 characters to aid identification without
// revealing enough to reduce brute-force entropy.
func maskADOToken(token string) string {
	if token == "" {
		return "(not set)"
	}
	if len(token) <= 4 {
		return "****"
	}
	return token[:4] + "****"
}

// getADOClient creates an Azure DevOps client from the current configuration.
func getADOClient(cfg ADOConfig) (*ado.Client, error) {
	client := ado.NewClient(ado.NewSecretString(cfg.PAT), cfg.Org, cfg.Project)
	if cfg.URL != "" {
		var err error
		client, err = client.WithBaseURL(cfg.URL)
		if err != nil {
			return nil, err
		}
	}
	return client, nil
}

// splitCSV splits a comma-separated string into trimmed, non-empty parts.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildADOPullFilters constructs PullFilters from CLI flags, falling back to
// config values (ado.filter.*). CLI flags override config when explicitly set.
// Returns nil when no filters are configured.
func buildADOPullFilters(ctx context.Context, cmd *cobra.Command, flags adoSyncFlags) *ado.PullFilters {
	areaPath := resolveADOFilterValue(ctx, cmd, "area-path", "ado.filter.area_path", flags.filterAreaPath)
	iterationPath := resolveADOFilterValue(ctx, cmd, "iteration-path", "ado.filter.iteration_path", flags.filterIterationPath)
	typesStr := resolveADOFilterValue(ctx, cmd, "types", "ado.filter.types", flags.filterTypes)
	statesStr := resolveADOFilterValue(ctx, cmd, "states", "ado.filter.states", flags.filterStates)

	types := splitCSV(typesStr)
	states := splitCSV(statesStr)

	if areaPath == "" && iterationPath == "" && len(types) == 0 && len(states) == 0 {
		return nil
	}

	return &ado.PullFilters{
		AreaPath:      areaPath,
		IterationPath: iterationPath,
		WorkItemTypes: types,
		States:        states,
	}
}

func resolveADOFilterValue(ctx context.Context, cmd *cobra.Command, flagName, configKey, flagValue string) string {
	if cmd.Flags().Changed(flagName) {
		return flagValue
	}
	if configValue := getADOConfigValue(ctx, configKey); configValue != "" {
		return configValue
	}
	return flagValue
}

// adoStatusResult holds the JSON output for the ado status command.
type adoStatusResult struct {
	Org        string   `json:"org"`
	Project    string   `json:"project"`
	Projects   []string `json:"projects,omitempty"`
	HasToken   bool     `json:"has_token"`
	URL        string   `json:"url,omitempty"`
	Configured bool     `json:"configured"`
	Error      string   `json:"error,omitempty"`
}

// runADOStatus implements the ado status command.
func runADOStatus(cmd *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("ado status is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("ado-status")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	cfg := getADOConfig()

	if isJSONOutput() {
		result := adoStatusResult{
			Org:      cfg.Org,
			Project:  cfg.Project,
			Projects: cfg.Projects,
			HasToken: cfg.PAT != "",
			URL:      cfg.URL,
		}
		if err := validateADOConfig(cfg); err != nil {
			result.Configured = false
			result.Error = err.Error()
		} else {
			result.Configured = true
		}
		return outputJSON(result)
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, "Azure DevOps Configuration")
	_, _ = fmt.Fprintln(out, "==========================")
	_, _ = fmt.Fprintf(out, "Organization: %s\n", cfg.Org)
	if len(cfg.Projects) <= 1 {
		_, _ = fmt.Fprintf(out, "Project:      %s\n", cfg.Project)
	} else {
		_, _ = fmt.Fprintf(out, "Projects:     %s (%d projects)\n", strings.Join(cfg.Projects, ", "), len(cfg.Projects))
	}
	_, _ = fmt.Fprintf(out, "PAT:          %s\n", maskADOToken(cfg.PAT))
	if cfg.URL != "" {
		_, _ = fmt.Fprintf(out, "Base URL:     %s\n", cfg.URL)
	}

	// Validate configuration
	if err := validateADOConfig(cfg); err != nil {
		_, _ = fmt.Fprintf(out, "\nStatus: ❌ Not configured\n")
		_, _ = fmt.Fprintf(out, "Error: %v\n", err)
		return nil
	}

	_, _ = fmt.Fprintf(out, "\nStatus: ✓ Configured\n")
	return nil
}

// runADOProjects implements the ado projects command.
func runADOProjects(cmd *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("ado projects is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("ado-projects")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	cfg := getADOConfig()
	if err := validateADOProjectsConfig(cfg); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	client, err := getADOClient(cfg)
	if err != nil {
		return fmt.Errorf("invalid ADO configuration: %w", err)
	}
	ctx := context.Background()

	projects, err := client.ListProjects(ctx)
	if err != nil {
		return fmt.Errorf("failed to list projects: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(projects)
	}

	printADOProjects(out, projects)
	return nil
}

func validateADOProjectsConfig(cfg ADOConfig) error {
	if cfg.PAT == "" {
		return fmt.Errorf("ado.pat not configured: set via 'bd config set ado.pat <token>' or AZURE_DEVOPS_PAT env var")
	}
	if cfg.Org == "" && cfg.URL == "" {
		return fmt.Errorf("ado.org not configured: set via 'bd config set ado.org <org>' or AZURE_DEVOPS_ORG env var")
	}
	return nil
}

func printADOProjects(out io.Writer, projects []ado.Project) {
	_, _ = fmt.Fprintln(out, "Azure DevOps Projects")
	_, _ = fmt.Fprintln(out, "=====================")
	for _, p := range projects {
		_, _ = fmt.Fprintf(out, "  %s\n", p.Name)
		if p.Description != "" {
			_, _ = fmt.Fprintf(out, "    %s\n", p.Description)
		}
	}
	if len(projects) == 0 {
		_, _ = fmt.Fprintln(out, "No projects found")
	}
}

// adoSyncResult holds the JSON output for the ado sync command.
type adoSyncResult struct {
	DryRun           bool     `json:"dry_run"`
	Pulled           int      `json:"pulled"`
	Pushed           int      `json:"pushed"`
	Created          int      `json:"created"`
	Updated          int      `json:"updated"`
	Skipped          int      `json:"skipped"`
	Conflicts        int      `json:"conflicts"`
	Errors           int      `json:"errors"`
	LinksPushed      int      `json:"links_pushed,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
	BootstrapMatched int      `json:"bootstrap_matched,omitempty"`
	Reconciled       bool     `json:"reconciled,omitempty"`
	ReconcileChecked int      `json:"reconcile_checked,omitempty"`
	ReconcileDeleted int      `json:"reconcile_deleted,omitempty"`
	ReconcileDenied  int      `json:"reconcile_denied,omitempty"`
}
