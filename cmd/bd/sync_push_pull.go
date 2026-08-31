package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/spf13/cobra"
)

// --- ADO push/pull ---

var adoPushCmd = &cobra.Command{
	Use:   "push [bead-ids...]",
	Short: "Push specific beads to Azure DevOps",
	Long: `Push one or more beads issues to Azure DevOps.

Accepts bead IDs as positional arguments.
Equivalent to: bd ado sync --push-only --issues <ids>`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runADOPush,
}

var adoPullCmd = &cobra.Command{
	Use:   "pull [refs...]",
	Short: "Pull specific items from Azure DevOps",
	Long: `Pull one or more items from Azure DevOps.

Accepts bead IDs or external references as positional arguments.
Equivalent to: bd ado sync --pull-only --issues <refs>`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runADOPull,
}

// --- Jira push/pull ---

var jiraPushCmd = &cobra.Command{
	Use:   "push [bead-ids...]",
	Short: "Push specific beads to Jira",
	Long: `Push one or more beads issues to Jira.

Accepts bead IDs as positional arguments.
Equivalent to: bd jira sync --push --issues <ids>`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runJiraPush,
}

var jiraPullCmd = &cobra.Command{
	Use:   "pull [refs...]",
	Short: "Pull specific items from Jira",
	Long: `Pull one or more items from Jira.

Accepts bead IDs or external references as positional arguments.
Equivalent to: bd jira sync --pull --issues <refs>`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runJiraPull,
}

// --- Linear push/pull ---

var linearPushCmd = &cobra.Command{
	Use:   "push [bead-ids...]",
	Short: "Push specific beads to Linear",
	Long: `Push one or more beads issues to Linear.

Accepts bead IDs as positional arguments.
Equivalent to: bd linear sync --push --issues <ids>`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runLinearPush,
}

var linearPullCmd = &cobra.Command{
	Use:   "pull [refs...]",
	Short: "Pull specific items from Linear",
	Long: `Pull one or more items from Linear.

Accepts bead IDs or external references as positional arguments.
Equivalent to: bd linear sync --pull --issues <refs>`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runLinearPull,
}

// --- GitHub push/pull ---

var githubPushCmd = &cobra.Command{
	Use:   "push [bead-ids...]",
	Short: "Push specific beads to GitHub",
	Long: `Push one or more beads issues to GitHub.

Accepts bead IDs as positional arguments.
Equivalent to: bd github sync --push-only --issues <ids>`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGitHubPush,
}

var githubPullCmd = &cobra.Command{
	Use:   "pull [refs...]",
	Short: "Pull specific items from GitHub",
	Long: `Pull one or more items from GitHub.

Accepts bead IDs or external references as positional arguments.
Equivalent to: bd github sync --pull-only --issues <refs>`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGitHubPull,
}

// --- GitLab push/pull ---

var gitlabPushCmd = &cobra.Command{
	Use:   "push [bead-ids...]",
	Short: "Push specific beads to GitLab",
	Long: `Push one or more beads issues to GitLab.

Accepts bead IDs as positional arguments.
Equivalent to: bd gitlab sync --push-only --issues <ids>`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGitLabPush,
}

var gitlabPullCmd = &cobra.Command{
	Use:   "pull [refs...]",
	Short: "Pull specific items from GitLab",
	Long: `Pull one or more items from GitLab.

Accepts bead IDs or external references as positional arguments.
Equivalent to: bd gitlab sync --pull-only --issues <refs>`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGitLabPull,
}

// --- Notion push/pull ---

var notionPushCmd = &cobra.Command{
	Use:   "push [bead-ids...]",
	Short: "Push specific beads to Notion",
	Long: `Push one or more beads issues to Notion.

Accepts bead IDs as positional arguments.
Equivalent to: bd notion sync --push --issues <ids>`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runNotionPush,
}

var notionPullCmd = &cobra.Command{
	Use:   "pull [refs...]",
	Short: "Pull specific items from Notion",
	Long: `Pull one or more items from Notion.

Accepts bead IDs or external references as positional arguments.
Equivalent to: bd notion sync --pull --issues <refs>`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runNotionPull,
}

func init() {
	// ADO push/pull
	adoPushCmd.Flags().Bool("dry-run", false, "Preview push without making changes")
	adoPullCmd.Flags().Bool("dry-run", false, "Preview pull without making changes")
	adoCmd.AddCommand(adoPushCmd)
	adoCmd.AddCommand(adoPullCmd)

	// Jira push/pull
	jiraPushCmd.Flags().Bool("dry-run", false, "Preview push without making changes")
	jiraPullCmd.Flags().Bool("dry-run", false, "Preview pull without making changes")
	jiraCmd.AddCommand(jiraPushCmd)
	jiraCmd.AddCommand(jiraPullCmd)

	// Linear push/pull
	linearPushCmd.Flags().Bool("dry-run", false, "Preview push without making changes")
	linearPullCmd.Flags().Bool("dry-run", false, "Preview pull without making changes")
	linearPullCmd.Flags().Bool("relations", false, "Import Linear relations as bd dependencies when pulling")
	linearCmd.AddCommand(linearPushCmd)
	linearCmd.AddCommand(linearPullCmd)

	// GitHub push/pull
	githubPushCmd.Flags().Bool("dry-run", false, "Preview push without making changes")
	githubPullCmd.Flags().Bool("dry-run", false, "Preview pull without making changes")
	githubCmd.AddCommand(githubPushCmd)
	githubCmd.AddCommand(githubPullCmd)

	// GitLab push/pull
	gitlabPushCmd.Flags().Bool("dry-run", false, "Preview push without making changes")
	gitlabPullCmd.Flags().Bool("dry-run", false, "Preview pull without making changes")
	gitlabCmd.AddCommand(gitlabPushCmd)
	gitlabCmd.AddCommand(gitlabPullCmd)

	// Notion push/pull
	notionPushCmd.Flags().Bool("dry-run", false, "Preview push without making changes")
	notionPullCmd.Flags().Bool("dry-run", false, "Preview pull without making changes")
	notionCmd.AddCommand(notionPushCmd)
	notionCmd.AddCommand(notionPullCmd)
}

// outputSyncResult writes sync results as JSON or human-readable text.
func outputSyncResult(result *tracker.SyncResult, dryRun bool) {
	if isJSONOutput() {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(result)
		return
	}
	if dryRun {
		fmt.Println("Dry run mode - no changes will be made")
	}
	if result.Stats.Pulled > 0 {
		fmt.Printf("✓ Pulled %d issues (%d created, %d updated)\n",
			result.Stats.Pulled, result.Stats.Created, result.Stats.Updated)
	}
	if result.Stats.Pushed > 0 {
		fmt.Printf("✓ Pushed %d issues\n", result.Stats.Pushed)
	}
	if dryRun {
		fmt.Println("\nRun without --dry-run to apply changes")
	}
}
