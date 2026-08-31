package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/jira"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/spf13/cobra"
)

var jiraCmd = &cobra.Command{
	Use:     "jira",
	GroupID: "advanced",
	Short:   "Jira integration commands",
	Long: `Synchronize issues between beads and Jira.

Configuration:
  bd config set jira.url "https://company.atlassian.net"
  bd config set jira.project "PROJ"
  bd config set jira.projects "PROJ1,PROJ2"   # Multiple projects
  bd config set jira.api_token "YOUR_TOKEN"
  bd config set jira.username "your_email@company.com"  # For Jira Cloud
  bd config set jira.push_prefix "hippo"       # Only push hippo-* issues to Jira
  bd config set jira.push_prefix "proj1,proj2" # Multiple prefixes (comma-separated)

Environment variables (alternative to config):
  JIRA_API_TOKEN  - Jira API token
  JIRA_USERNAME   - Jira username/email
  JIRA_PROJECTS   - Comma-separated project keys

Examples:
  bd jira sync --pull         # Import issues from Jira
  bd jira sync --push         # Export issues to Jira
  bd jira sync                # Bidirectional sync (pull then push)
  bd jira sync --dry-run      # Preview sync without changes
  bd jira status              # Show sync status`,
}

var jiraSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Synchronize issues with Jira",
	Long: `Synchronize issues between beads and Jira.

Modes:
  --pull         Import issues from Jira into beads
  --push         Export issues from beads to Jira
  (no flags)     Bidirectional sync: pull then push, with conflict resolution

Conflict Resolution:
  By default, newer timestamp wins. Override with:
  --prefer-local   Always prefer local beads version
  --prefer-jira    Always prefer Jira version

Examples:
  bd jira sync --pull                # Import from Jira
  bd jira sync --push --create-only  # Push new issues only
  bd jira sync --dry-run             # Preview without changes
  bd jira sync --prefer-local        # Bidirectional, local wins`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runJiraSync,
}

var jiraStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show Jira sync status",
	Long: `Show the current Jira sync status, including:
  - Last sync timestamp
  - Configuration status
  - Number of issues with Jira links
  - Issues pending push (no external_ref)`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runJiraStatus,
}

func init() {
	jiraSyncCmd.Flags().Bool("pull", false, "Pull issues from Jira")
	jiraSyncCmd.Flags().Bool("push", false, "Push issues to Jira")
	jiraSyncCmd.Flags().Bool("dry-run", false, "Preview sync without making changes")
	jiraSyncCmd.Flags().Bool("prefer-local", false, "Prefer local version on conflicts")
	jiraSyncCmd.Flags().Bool("prefer-jira", false, "Prefer Jira version on conflicts")
	jiraSyncCmd.Flags().Bool("create-only", false, "Only create new issues, don't update existing")
	jiraSyncCmd.Flags().String("state", "all", "Issue state to sync: open, closed, all")
	jiraSyncCmd.Flags().StringSlice("project", nil, "Project key(s) to sync (overrides configured project/projects)")
	registerSelectiveSyncFlags(jiraSyncCmd)

	jiraCmd.AddCommand(jiraSyncCmd)
	jiraCmd.AddCommand(jiraStatusCmd)
	rootCmd.AddCommand(jiraCmd)
}

func runJiraSync(cmd *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("jira sync is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("jira-sync")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	opts, err := gatherJiraSyncOptions(cmd)
	if err != nil {
		return err
	}
	if !opts.DryRun {
		if err := CheckReadonly("jira sync"); err != nil {
			return err
		}
	}
	return executeJiraSync(cmd, opts)
}

func executeJiraSync(cmd *cobra.Command, opts tracker.SyncOptions) error {
	if err := ensureStoreActive(); err != nil {
		return HandleErrorRespectJSON("database not available: %v", err)
	}
	if err := validateJiraConfig(); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	engine, err := newJiraSyncEngine(cmd, getRootContext())
	if err != nil {
		return err
	}
	result, err := engine.Sync(getRootContext(), opts)
	if err != nil {
		return reportJiraSyncError(result, err)
	}
	return printJiraSyncResult(result, opts.DryRun)
}

func gatherJiraSyncOptions(cmd *cobra.Command) (tracker.SyncOptions, error) {
	pull, _ := cmd.Flags().GetBool("pull")
	push, _ := cmd.Flags().GetBool("push")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	preferLocal, _ := cmd.Flags().GetBool("prefer-local")
	preferJira, _ := cmd.Flags().GetBool("prefer-jira")
	createOnly, _ := cmd.Flags().GetBool("create-only")
	state, _ := cmd.Flags().GetString("state")
	if preferLocal && preferJira {
		return tracker.SyncOptions{}, HandleErrorRespectJSON("cannot use both --prefer-local and --prefer-jira")
	}
	opts := tracker.SyncOptions{
		Pull:               pull,
		Push:               push,
		DryRun:             dryRun,
		CreateOnly:         createOnly,
		State:              state,
		ConflictResolution: jiraConflictResolution(preferLocal, preferJira),
	}
	if err := applySelectiveSyncFlags(cmd, &opts, push); err != nil {
		return tracker.SyncOptions{}, HandleErrorRespectJSON("%v", err)
	}
	return opts, nil
}

func jiraConflictResolution(preferLocal, preferJira bool) tracker.ConflictResolution {
	switch {
	case preferLocal:
		return tracker.ConflictLocal
	case preferJira:
		return tracker.ConflictExternal
	default:
		return tracker.ConflictTimestamp
	}
}

func newJiraSyncEngine(cmd *cobra.Command, ctx context.Context) (*tracker.Engine, error) {
	jt := &jira.Tracker{}
	cliProjects, _ := cmd.Flags().GetStringSlice("project")
	if len(cliProjects) > 0 {
		jt.SetProjectKeys(tracker.DeduplicateStrings(cliProjects))
	}
	if err := jt.Init(ctx, getStore()); err != nil {
		return nil, HandleErrorRespectJSON("initializing Jira tracker: %v", err)
	}
	engine := tracker.NewEngine(jt, getStore(), getActor())
	engine.OnMessage = func(msg string) { fmt.Println("  " + msg) }
	engine.OnWarning = func(msg string) { fmt.Fprintf(os.Stderr, "Warning: %s\n", msg) } //nolint:gosec // G705: CLI stderr, not HTML.
	engine.PushHooks = buildJiraPushHooks(ctx)
	return engine, nil
}

func reportJiraSyncError(result *tracker.SyncResult, err error) error {
	if isJSONOutput() {
		if jerr := outputJSON(result); jerr != nil {
			return jerr
		}
		return SilentExit()
	}
	return HandleError("%v", err)
}

func printJiraSyncResult(result *tracker.SyncResult, dryRun bool) error {
	if isJSONOutput() {
		return outputJSON(result)
	}
	if dryRun {
		fmt.Println("\n✓ Dry run complete (no changes made)")
		return nil
	}
	if result.Stats.Pulled > 0 {
		fmt.Printf("✓ Pulled %d issues (%d created, %d updated)\n",
			result.Stats.Pulled, result.Stats.Created, result.Stats.Updated)
	}
	if result.Stats.Pushed > 0 {
		fmt.Printf("✓ Pushed %d issues\n", result.Stats.Pushed)
	}
	if result.Stats.Conflicts > 0 {
		fmt.Printf("→ Resolved %d conflicts\n", result.Stats.Conflicts)
	}
	fmt.Println("\n✓ Jira sync complete")
	if len(result.Warnings) > 0 {
		fmt.Println("\nWarnings:")
		for _, w := range result.Warnings {
			fmt.Printf("  - %s\n", w)
		}
	}
	return nil
}

// buildJiraPushHooks creates PushHooks for Jira-specific push behavior.
func buildJiraPushHooks(ctx context.Context) *tracker.PushHooks {
	return &tracker.PushHooks{
		ShouldPush: func(issue *types.Issue) bool {
			pushPrefix, _ := getStore().GetConfig(ctx, "jira.push_prefix")
			if pushPrefix == "" {
				return true
			}
			for _, prefix := range strings.Split(pushPrefix, ",") {
				prefix = strings.TrimSpace(prefix)
				prefix = strings.TrimSuffix(prefix, "-")
				if prefix != "" && strings.HasPrefix(issue.ID, prefix+"-") {
					return true
				}
			}
			return false
		},
	}
}

func runJiraStatus(_ *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("jira status is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("jira-status")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	if err := ensureStoreActive(); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	status, err := loadJiraStatus(getRootContext())
	if err != nil {
		return err
	}
	if isJSONOutput() {
		return outputJSON(status.json())
	}
	printJiraStatus(status)
	return nil
}

type jiraStatusInfo struct {
	jiraURL     string
	lastSync    string
	projectKeys []string
	configured  bool
	allIssues   []*types.Issue
	withJiraRef int
	pendingPush int
}

func loadJiraStatus(ctx context.Context) (jiraStatusInfo, error) {
	jiraURL, _ := getStore().GetConfig(ctx, "jira.url")
	lastSync, _ := getStore().GetConfig(ctx, "jira.last_sync")
	pluralProjects, _ := getStore().GetConfig(ctx, "jira.projects")
	singularProject, _ := getStore().GetConfig(ctx, "jira.project")
	projectKeys := tracker.ResolveProjectIDs(nil, pluralProjects, singularProject)
	// jira sync is a round-trip path — opt out of BEADS_MAX_ROWS
	// (designer §4.1) so a misconfigured env doesn't abort partway.
	allIssues, err := getStore().SearchIssues(ctx, "", types.IssueFilter{IssueFilterPage: types.IssueFilterPage{MaxRows: 0, MaxRowsSource: ""}})
	if err != nil {
		return jiraStatusInfo{}, HandleErrorRespectJSON("%v", err)
	}
	withJiraRef, pendingPush := countJiraRefs(allIssues, jiraURL)
	return jiraStatusInfo{
		jiraURL:     jiraURL,
		lastSync:    lastSync,
		projectKeys: projectKeys,
		configured:  jiraURL != "" && len(projectKeys) > 0,
		allIssues:   allIssues,
		withJiraRef: withJiraRef,
		pendingPush: pendingPush,
	}, nil
}

func countJiraRefs(allIssues []*types.Issue, jiraURL string) (withJiraRef, pendingPush int) {
	for _, issue := range allIssues {
		if issue.ExternalRef != nil && jira.IsJiraExternalRef(*issue.ExternalRef, jiraURL) {
			withJiraRef++
		} else if issue.ExternalRef == nil {
			pendingPush++
		}
	}
	return withJiraRef, pendingPush
}

func (s jiraStatusInfo) json() map[string]interface{} {
	primaryProject := ""
	if len(s.projectKeys) > 0 {
		primaryProject = s.projectKeys[0]
	}
	return map[string]interface{}{
		"configured":    s.configured,
		"jira_url":      s.jiraURL,
		"jira_project":  primaryProject,
		"jira_projects": s.projectKeys,
		"last_sync":     s.lastSync,
		"total_issues":  len(s.allIssues),
		"with_jira_ref": s.withJiraRef,
		"pending_push":  s.pendingPush,
	}
}

func printJiraStatus(s jiraStatusInfo) {
	fmt.Println("Jira Sync Status")
	fmt.Println("================")
	fmt.Println()
	if !s.configured {
		printJiraUnconfiguredHelp()
		return
	}
	fmt.Printf("Jira URL:     %s\n", s.jiraURL)
	if len(s.projectKeys) == 1 {
		fmt.Printf("Project:      %s\n", s.projectKeys[0])
	} else {
		fmt.Printf("Projects:     %s (%d projects)\n", strings.Join(s.projectKeys, ", "), len(s.projectKeys))
	}
	if s.lastSync != "" {
		fmt.Printf("Last Sync:    %s\n", s.lastSync)
	} else {
		fmt.Println("Last Sync:    Never")
	}
	fmt.Println()
	fmt.Printf("Total Issues: %d\n", len(s.allIssues))
	fmt.Printf("With Jira:    %d\n", s.withJiraRef)
	fmt.Printf("Local Only:   %d\n", s.pendingPush)
	if s.pendingPush > 0 {
		fmt.Println()
		fmt.Printf("Run 'bd jira sync --push' to push %d local issue(s) to Jira\n", s.pendingPush)
	}
}

func printJiraUnconfiguredHelp() {
	fmt.Println("Status: Not configured")
	fmt.Println()
	fmt.Println("To configure Jira integration:")
	fmt.Println("  bd config set jira.url \"https://company.atlassian.net\"")
	fmt.Println("  bd config set jira.project \"PROJ\"")
	fmt.Println("  bd config set jira.projects \"PROJ1,PROJ2\"  # multiple projects")
	fmt.Println("  bd config set jira.api_token \"YOUR_TOKEN\"")
	fmt.Println("  bd config set jira.username \"your@email.com\"")
}

// validateJiraConfig checks that required Jira configuration is present.
func validateJiraConfig() error {
	if err := ensureStoreActive(); err != nil {
		return fmt.Errorf("database not available: %w", err)
	}

	ctx := getRootContext()
	jiraURL, _ := getStore().GetConfig(ctx, "jira.url")

	if jiraURL == "" {
		return fmt.Errorf("jira.url not configured\nRun: bd config set jira.url \"https://company.atlassian.net\"")
	}

	// Check for project configuration (singular or plural).
	pluralProjects, _ := getStore().GetConfig(ctx, "jira.projects")
	singularProject, _ := getStore().GetConfig(ctx, "jira.project")
	projectKeys := tracker.ResolveProjectIDs(nil, pluralProjects, singularProject)
	if len(projectKeys) == 0 {
		return fmt.Errorf("no Jira project configured\nRun: bd config set jira.project \"PROJ\"\nOr:  bd config set jira.projects \"PROJ1,PROJ2\"")
	}

	apiToken, _ := getStore().GetConfig(ctx, "jira.api_token")
	if apiToken == "" && os.Getenv("JIRA_API_TOKEN") == "" {
		return fmt.Errorf("Jira API token not configured\nRun: bd config set jira.api_token \"YOUR_TOKEN\"\nOr: export JIRA_API_TOKEN=YOUR_TOKEN")
	}

	return nil
}
