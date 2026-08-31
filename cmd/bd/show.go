package main

import (
	"github.com/jonbaldie/beads/issueops"
	"github.com/spf13/cobra"
)

func showCommand() *cobra.Command {
	return &cobra.Command{
		Use:           "show [id...] [--id=<id>...] [--current]",
		Aliases:       []string{"view"},
		GroupID:       "issues",
		Short:         "Show issue details",
		Args:          cobra.ArbitraryArgs, // Allow zero positional args when --id is used
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runShowCommand,
	}
}

func init() {
	showCmd := showCommand()
	showCmd.Flags().Bool("thread", false, "Show full conversation thread (for messages)")
	showCmd.Flags().Bool("short", false, "Show compact one-line output per issue")
	showCmd.Flags().Bool("long", false, "Show all available fields (extended metadata, agent identity, gate fields, etc.)")
	showCmd.Flags().Bool("refs", false, "Show issues that reference this issue (reverse lookup)")
	showCmd.Flags().Bool("children", false, "Show only the children of this issue")
	showCmd.Flags().String("as-of", "", "Show issue as it existed at a specific commit hash or branch (requires Dolt)")
	showCmd.Flags().StringArray("id", nil, "Issue ID (use for IDs that look like flags, e.g., --id=gt--xyz)")
	showCmd.Flags().Bool("local-time", false, "Show timestamps in local time instead of UTC")
	showCmd.Flags().BoolP("watch", "w", false, "Watch for changes and auto-refresh display")
	showCmd.Flags().Bool("current", false, "Show the currently active issue (in-progress, hooked, or last touched)")
	showCmd.Flags().Bool("include-dependents", false, "Stream full dependent issues in JSON output (--json only; may be slow on hub beads)")
	showCmd.Flags().Bool("include-comments", false, "Stream full comment bodies in JSON output (--json only; may be slow on issues with many comments)")
	showCmd.Flags().Bool("brief-deps", false, "Reduce each dependency to its identity fields in JSON output (--json only; drops description, design, notes and acceptance criteria)")
	showCmd.ValidArgsFunction = issueIDCompletion
	rootCmd.AddCommand(showCmd)
}

// showGetRequest carries the show flags onto the read contract. Extracted so
// the hop is reachable from a test: both show routes build this request
// independently, and a dropped field here is invisible to every other test.
func showGetRequest(id string, includeDependents, includeComments, briefDeps bool) issueops.GetRequest {
	return issueops.GetRequest{
		ID:                id,
		IncludeDependents: includeDependents,
		IncludeComments:   includeComments,
		BriefDeps:         briefDeps,
	}
}
