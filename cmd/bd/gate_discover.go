package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

// GHWorkflowRun represents a GitHub workflow run from `gh run list --json`
type GHWorkflowRun struct {
	DatabaseID   int64     `json:"databaseId"`
	DisplayTitle string    `json:"displayTitle"`
	HeadBranch   string    `json:"headBranch"`
	HeadSha      string    `json:"headSha"`
	Name         string    `json:"name"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	WorkflowName string    `json:"workflowName"`
	URL          string    `json:"url"`
}

// gateDiscoverCmd discovers GitHub run IDs for gh:run gates
var gateDiscoverCmd = &cobra.Command{
	Use:   "discover",
	Short: "Discover await_id for gh:run gates",
	Long: `Discovers GitHub workflow run IDs for gates awaiting CI/CD completion.

This command finds open gates with await_type="gh:run" that don't have an await_id,
queries recent GitHub workflow runs, and matches them using heuristics:
  - Branch name matching
  - Commit SHA matching
  - Time proximity (runs within 5 minutes of gate creation)

Once matched, the gate's await_id is updated with the GitHub run ID, enabling
subsequent polling to check the run's status.

A gate whose metadata.repo targets another repository is only matched
against runs queried from that repository, never against the current
repository's runs of a same-named workflow.

Examples:
  bd gate discover           # Auto-discover run IDs for all matching gates
  bd gate discover --dry-run # Preview what would be matched (no updates)
  bd gate discover --branch main --limit 10  # Only match runs on 'main' branch`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGateDiscover,
}

func init() {
	gateDiscoverCmd.Flags().BoolP("dry-run", "n", false, "Preview mode: show matches without updating")
	gateDiscoverCmd.Flags().StringP("branch", "b", "", "Filter runs by branch (default: current branch)")
	gateDiscoverCmd.Flags().IntP("limit", "l", 10, "Max runs to query from GitHub")
	gateDiscoverCmd.Flags().DurationP("max-age", "a", 30*time.Minute, "Max age for gate/run matching")

	gateCmd.AddCommand(gateDiscoverCmd)
}

func runGateDiscover(cmd *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("gate discover is not supported in proxied-server mode")
	}
	if err := CheckReadonly("gate discover"); err != nil {
		return err
	}
	evt := metrics.NewCommandEvent("gate-discover")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	branchFilter, _ := cmd.Flags().GetString("branch")
	limit, _ := cmd.Flags().GetInt("limit")
	maxAge, _ := cmd.Flags().GetDuration("max-age")
	gates, err := findPendingGates()
	if err != nil {
		return HandleError("finding gates: %v", err)
	}
	if len(gates) == 0 {
		fmt.Println("No pending gh:run gates found (all gates have numeric run IDs)")
		return nil
	}
	fmt.Printf("%s Found %d gate(s) awaiting run ID discovery\n\n", ui.RenderAccent("🔍"), len(gates))
	matches := discoverGateRuns(gates, branchFilter, limit, maxAge)
	matchCount := applyGateDiscoverMatches(getRootContext(), matches, dryRun)
	printGateDiscoverSummary(matchCount, dryRun)
	return reportGateDiscoverFailures(matches)
}

func discoverGateRuns(gates []*types.Issue, branchFilter string, limit int, maxAge time.Duration) []gateDiscoveryMatch {
	// userSpecifiedBranch must be captured BEFORE the auto-detect fallback
	// below overwrites branchFilter with the local branch - otherwise
	// branchFilterForRepo can never tell "the user explicitly asked to
	// filter by this branch" apart from "this is just the local branch we
	// defaulted to", and an explicit `--branch` is dropped for every
	// cross-repo gate exactly like the un-requested auto-detected one is.
	userSpecifiedBranch := branchFilter != ""
	if !userSpecifiedBranch {
		branchFilter = getGitBranchForGateDiscovery()
	}
	// Scope run discovery per gate's own repo selector (SF1): a gate whose
	// metadata targets another repository must only be matched against runs
	// queried FROM that repository, never against a same-named workflow run
	// from the current repo. matchGatesToRuns groups gates by their
	// validated repo and issues one query per distinct repo.
	return matchGatesToRuns(gates, maxAge, func(repo, workflowHint string) ([]GHWorkflowRun, error) {
		return queryGitHubRunsInRepo(branchFilterForRepo(branchFilter, repo, userSpecifiedBranch), limit, repo, workflowHint)
	})
}

func applyGateDiscoverMatches(ctx context.Context, matches []gateDiscoveryMatch, dryRun bool) int {
	matchCount := 0
	for _, m := range matches {
		if applied := applyOneGateDiscoverMatch(ctx, m, dryRun); applied {
			matchCount++
		}
	}
	return matchCount
}

func applyOneGateDiscoverMatch(ctx context.Context, m gateDiscoveryMatch, dryRun bool) bool {
	if m.err != nil {
		fmt.Fprintf(os.Stderr, "  %s %s - %v\n", ui.RenderFail("✗"), ui.RenderID(m.gate.ID), m.err)
		return false
	}
	if m.run == nil {
		if !isJSONOutput() {
			fmt.Printf("  %s %s - no matching run found\n", ui.RenderFail("✗"), ui.RenderID(m.gate.ID))
		}
		return false
	}
	runIDStr := strconv.FormatInt(m.run.DatabaseID, 10)
	if dryRun {
		fmt.Printf("  %s %s → run %s (%s) [dry-run]\n", ui.RenderPass("✓"), ui.RenderID(m.gate.ID), runIDStr, m.run.Status)
		return true
	}
	if err := updateGateAwaitID(ctx, m.gate.ID, runIDStr); err != nil {
		fmt.Fprintf(os.Stderr, "  %s %s - update failed: %v\n", ui.RenderFail("✗"), ui.RenderID(m.gate.ID), err)
		return false
	}
	fmt.Printf("  %s %s → run %s (%s)\n", ui.RenderPass("✓"), ui.RenderID(m.gate.ID), runIDStr, m.run.Status)
	return true
}

func printGateDiscoverSummary(matchCount int, dryRun bool) {
	fmt.Println()
	if dryRun {
		fmt.Printf("Would update %d gate(s). Run without --dry-run to apply.\n", matchCount)
		return
	}
	fmt.Printf("Updated %d gate(s) with discovered run IDs.\n", matchCount)
}

func reportGateDiscoverFailures(matches []gateDiscoveryMatch) error {
	// A GitHub query failure (gh missing, unauthenticated, rate limited, ...)
	// is fatal, matching pre-multi-repo behavior: before per-repo scoping,
	// any query error returned HandleError immediately. Per-gate detail was
	// already reported above; report a summary here and exit non-zero so a
	// wholly-failed discovery is never mistaken for "0 gates matched".
	failures := gateDiscoveryQueryFailures(matches)
	if len(failures) == 0 {
		return nil
	}
	repos := make([]string, 0, len(failures))
	for repo := range failures {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	details := make([]string, 0, len(repos))
	for _, repo := range repos {
		label := repo
		if label == "" {
			label = "current repo"
		}
		details = append(details, fmt.Sprintf("%s: %v", label, failures[repo]))
	}
	return HandleError("querying GitHub runs failed for %d repo(s): %s", len(failures), strings.Join(details, "; "))
}

// gateDiscoveryMatch pairs a gate with the run matched for it, or an error
// explaining why it could not be matched (invalid repo metadata, or the
// GitHub query for its repo failed).
type gateDiscoveryMatch struct {
	gate *types.Issue
	run  *GHWorkflowRun
	err  error
}

// gateQueryError wraps a failed GitHub query for a specific repo. It is
// distinguished from other gateDiscoveryMatch errors (e.g. invalid repo
// metadata) so runGateDiscover can tell "we couldn't even ask GitHub"
// (gh missing, unauthenticated, rate limited, ...) apart from a per-gate
// data problem: a wholly-failed discovery must exit non-zero, matching the
// pre-multi-repo behavior where a query failure was fatal.
type gateQueryError struct {
	repo string
	err  error
}

func (e *gateQueryError) Error() string { return e.err.Error() }
func (e *gateQueryError) Unwrap() error { return e.err }

// gateDiscoveryQueryFailures returns the distinct repos whose GitHub query
// failed among matches, keyed by repo ("" meaning the current repository)
// with the query error that repo produced.
func gateDiscoveryQueryFailures(matches []gateDiscoveryMatch) map[string]error {
	failures := make(map[string]error)
	for _, m := range matches {
		var qe *gateQueryError
		if errors.As(m.err, &qe) {
			failures[qe.repo] = qe.err
		}
	}
	return failures
}

// branchFilterForRepo returns the branch filter to use when querying a gate's
// repo. It always applies the branch filter to the current repo (repo ==
// ""). For a foreign repo it drops an auto-detected local branch - a
// cross-repo gate's target branch has no relationship to the branch checked
// out locally, so `gh run list --repo <other> --branch <local-branch>` would
// filter out every run in that repo and the gate would never be discoverable
// (this was the cross-repo-discovery-is-inert bug) - but keeps a branch the
// user explicitly passed via `--branch`: an explicit filter is a deliberate
// instruction about the TARGET repo's branch, not a guess about the local
// checkout, and dropping it silently for cross-repo gates would make
// `bd gate discover --branch main` appear to work while doing nothing.
func branchFilterForRepo(localBranchFilter, repo string, userSpecified bool) string {
	if repo != "" && !userSpecified {
		return ""
	}
	return localBranchFilter
}
