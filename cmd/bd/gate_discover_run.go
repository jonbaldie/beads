package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/types"
)

// matchGatesToRuns scopes run discovery per gate's own repo selector (SF1).
// Gates are grouped by their validated metadata.repo (via githubRepoFromIssue;
// "" means the current repository), and queryRuns is called at most once per
// distinct (repo, workflow hint) pair among the given gates. A gate is only
// ever matched against runs queried from ITS repo - never against another
// repo's runs of a same-named workflow, which would otherwise persist the
// wrong await_id permanently (the persisted ID pins the gate).
//
// queryRuns receives a workflowHint - the gate's AwaitID workflow name hint,
// non-empty only for a foreign (cross-repo) query - so it can narrow the
// `gh run list` call with --workflow. Without that narrowing, a busy foreign
// repo's unfiltered recent-run list (capped by --limit) might never surface
// the specific workflow a gate is waiting on. The current repo's query is
// never narrowed this way (workflowHint is always "" for it), matching
// pre-existing `bd gate discover` behavior for local gates.
func matchGatesToRuns(gates []*types.Issue, maxAge time.Duration, queryRuns func(repo, workflowHint string) ([]GHWorkflowRun, error)) []gateDiscoveryMatch {
	runsByKey := make(map[string][]GHWorkflowRun)
	queryErrByKey := make(map[string]error)
	results := make([]gateDiscoveryMatch, 0, len(gates))

	for _, gate := range gates {
		repo, repoErr := githubRepoFromIssue(gate)
		if repoErr != nil {
			results = append(results, gateDiscoveryMatch{gate: gate, err: fmt.Errorf("invalid repo metadata: %w", repoErr)})
			continue
		}

		foreign := repo != ""
		hint := getWorkflowNameHint(gate)

		// Cross-repo discovery requires a workflow hint. With local-commit/
		// local-branch heuristics neutralized for a foreign repo (see
		// matchGateToRun), a hintless gate could only ever score on time
		// proximity alone and risk pinning the wrong run in another
		// repository permanently. Skip the query entirely rather than spend
		// a GitHub API call on a gate that can never match.
		if foreign && hint == "" {
			results = append(results, gateDiscoveryMatch{gate: gate})
			continue
		}

		queryHint := ""
		key := repo
		if foreign {
			queryHint = hint
			key = repo + "\x1f" + hint
		}

		runs, cached := runsByKey[key]
		if !cached {
			if qErr, queried := queryErrByKey[key]; queried {
				results = append(results, gateDiscoveryMatch{gate: gate, err: &gateQueryError{repo: repo, err: qErr}})
				continue
			}
			queried, err := queryRuns(repo, queryHint)
			if err != nil {
				queryErrByKey[key] = err
				results = append(results, gateDiscoveryMatch{gate: gate, err: &gateQueryError{repo: repo, err: err}})
				continue
			}
			runsByKey[key] = queried
			runs = queried
		}

		// A gate's local-commit/local-branch heuristics only make sense
		// against the current repo's runs; a foreign repo (repo != "") never
		// shares a commit SHA or branch name with the local checkout, so
		// those heuristics are neutralized for it (see matchGateToRun).
		results = append(results, gateDiscoveryMatch{gate: gate, run: matchGateToRun(gate, runs, maxAge, foreign)})
	}

	return results
}

// isNumericRunID returns true if the string looks like a GitHub numeric run ID.
// This is a local alias for consistency - the canonical implementation is isNumericID in gate.go.
func isNumericRunID(s string) bool {
	return isNumericID(s)
}

// needsDiscovery returns true if a gh:run gate needs run ID discovery.
// This is true when AwaitID is empty OR contains a non-numeric workflow name hint.
func needsDiscovery(g *types.Issue) bool {
	if g.AwaitType != "gh:run" {
		return false
	}
	// Empty AwaitID or non-numeric (workflow name hint) needs discovery
	return g.AwaitID == "" || !isNumericRunID(g.AwaitID)
}

// getWorkflowNameHint extracts the workflow name hint from AwaitID if present.
// Returns empty string if AwaitID is empty or numeric (already resolved).
func getWorkflowNameHint(g *types.Issue) string {
	if g.AwaitID == "" || isNumericRunID(g.AwaitID) {
		return ""
	}
	return g.AwaitID
}

// workflowNameMatches checks if a workflow hint matches a GitHub workflow run.
// It handles various naming conventions:
//   - Exact match (case-insensitive)
//   - Hint with .yml/.yaml suffix vs display name without
//   - Hint without suffix vs filename with .yml/.yaml
func workflowNameMatches(hint, workflowName, runName string) bool {
	// Normalize hint by removing .yml/.yaml suffix for comparison
	hintBase := strings.TrimSuffix(strings.TrimSuffix(hint, ".yml"), ".yaml")

	// Exact matches (case-insensitive)
	if strings.EqualFold(workflowName, hint) || strings.EqualFold(runName, hint) {
		return true
	}

	// Match hint base against workflow display name
	if strings.EqualFold(workflowName, hintBase) {
		return true
	}

	// Match hint (with suffix added) against run filename
	if strings.EqualFold(runName, hintBase+".yml") || strings.EqualFold(runName, hintBase+".yaml") {
		return true
	}

	return false
}

// findPendingGates returns open gh:run gates that need run ID discovery.
// This includes gates with empty AwaitID OR non-numeric AwaitID (workflow name hint).
func findPendingGates() ([]*types.Issue, error) {
	var gates []*types.Issue

	gateType := types.IssueType("gate")
	filter := types.IssueFilter{
		IssueFilterCore: types.IssueFilterCore{
			IssueType: &gateType,
		},
		IssueFilterFlags: types.IssueFilterFlags{
			ExcludeStatus: []types.Status{types.StatusClosed},
		},
	}

	allGates, err := getStore().SearchIssues(getRootContext(), "", filter)
	if err != nil {
		return nil, fmt.Errorf("search gates: %w", err)
	}

	for _, g := range allGates {
		if needsDiscovery(g) {
			gates = append(gates, g)
		}
	}

	return gates, nil
}

// getGitBranchForGateDiscovery returns the current git branch name
// Uses CWD repo context since this is for user's project CI discovery
func getGitBranchForGateDiscovery() string {
	rc, err := beads.GetRepoContext()
	if err != nil {
		return "main" // Default fallback
	}

	cmd := rc.GitCmdCWD(context.Background(), "rev-parse", "--abbrev-ref", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return "main" // Default fallback
	}
	return strings.TrimSpace(string(output))
}

// getGitCommitForGateDiscovery returns the current git commit SHA
// Uses CWD repo context since this is for user's project CI discovery
func getGitCommitForGateDiscovery() string {
	rc, err := beads.GetRepoContext()
	if err != nil {
		return ""
	}

	cmd := rc.GitCmdCWD(context.Background(), "rev-parse", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// queryGitHubRunsInRepo queries recent workflow runs from GitHub using gh
// CLI, scoped to repo ("" means the current repository) and optionally
// narrowed to a single workflow (workflow == "" queries all workflows). This
// is the query path for `bd gate discover`'s branch/heuristic matching (SF1)
// - distinct from queryGitHubRunsForWorkflowInRepo in gate.go, which filters
// by a specific --workflow name for the direct await_id discovery used by
// `bd gate check`. matchGatesToRuns only ever passes a non-empty workflow for
// a foreign repo, to recover the visibility --limit would otherwise cost an
// unfiltered cross-repo query (see matchGatesToRuns).
func queryGitHubRunsInRepo(branch string, limit int, repo string, workflow string) ([]GHWorkflowRun, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, fmt.Errorf("gh CLI not found: install from https://cli.github.com")
	}
	return queryGitHubRunsInRepoWithRunner(branch, limit, repo, workflow, runGHCommand)
}

func queryGitHubRunsInRepoWithRunner(branch string, limit int, repo string, workflow string, runGH ghCommandRunner) ([]GHWorkflowRun, error) {
	args := []string{
		"run", "list",
		"--json", "databaseId,displayTitle,headBranch,headSha,name,status,conclusion,createdAt,updatedAt,workflowName,url",
		"--limit", strconv.Itoa(limit),
	}

	if branch != "" {
		args = append(args, "--branch", branch)
	}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	if workflow != "" {
		args = append(args, "--workflow", workflow)
	}

	output, stderr, err := runGH(args...)
	if err != nil {
		if len(stderr) > 0 {
			return nil, fmt.Errorf("gh run list failed: %s", string(stderr))
		}
		return nil, fmt.Errorf("gh run list: %w", err)
	}

	var runs []GHWorkflowRun
	if err := json.Unmarshal(output, &runs); err != nil {
		return nil, fmt.Errorf("parse gh output: %w", err)
	}

	return runs, nil
}

// matchGateToRun finds the best matching run for a gate using heuristics.
// If the gate has a workflow name hint in AwaitID, only runs matching that workflow are considered.
//
// foreignRepo must be true when runs were queried from a repo other than the
// current one (SF1: a gate whose metadata.repo targets another repository).
// In that case the local commit SHA and branch name are meaningless - they
// describe the current checkout, not the foreign repo - so the commit/branch
// heuristics are skipped entirely rather than comparing against them anyway.
func matchGateToRun(gate *types.Issue, runs []GHWorkflowRun, maxAge time.Duration, foreignRepo bool) *GHWorkflowRun {
	workflowHint := getWorkflowNameHint(gate)
	// Cross-repo discovery requires a workflow hint. With the commit/branch
	// heuristics below neutralized for a foreign repo, a hintless gate could
	// otherwise reach bestScore >= 30 on time proximity (+ in-progress/queued
	// status) alone and pin the wrong run in another repository permanently -
	// matchGatesToRuns already skips the query for this case, but guard here
	// too so any other caller gets the same safety.
	if foreignRepo && workflowHint == "" {
		return nil
	}
	ctx := gateMatchContext(gate, foreignRepo, workflowHint)
	var bestMatch *GHWorkflowRun
	var bestScore int
	now := time.Now()
	for i := range runs {
		run := &runs[i]
		score, ok := scoreGateRun(ctx, run, now, maxAge)
		if !ok || score <= bestScore {
			continue
		}
		bestScore = score
		bestMatch = run
	}
	// Require at least some confidence in the match
	// With workflow hint, workflow match (200) alone is sufficient
	// Without workflow hint, require branch or commit match (30+ from time proximity)
	if bestScore >= 30 {
		return bestMatch
	}
	return nil
}

type gateMatchCtx struct {
	gate          *types.Issue
	foreignRepo   bool
	workflowHint  string
	currentCommit string
	currentBranch string
}

func gateMatchContext(gate *types.Issue, foreignRepo bool, workflowHint string) gateMatchCtx {
	ctx := gateMatchCtx{gate: gate, foreignRepo: foreignRepo, workflowHint: workflowHint}
	if !foreignRepo {
		ctx.currentCommit = getGitCommitForGateDiscovery()
		ctx.currentBranch = getGitBranchForGateDiscovery()
	}
	return ctx
}

func scoreGateRun(ctx gateMatchCtx, run *GHWorkflowRun, now time.Time, maxAge time.Duration) (int, bool) {
	if now.Sub(run.CreatedAt) > maxAge {
		return 0, false
	}
	score, ok := scoreGateRunWorkflow(ctx, run)
	if !ok {
		return 0, false
	}
	score += scoreGateRunLocal(ctx, run)
	score += gateRunTimeScore(run.CreatedAt.Sub(ctx.gate.CreatedAt).Abs())
	if run.Status == "in_progress" || run.Status == "queued" {
		score += 5
	}
	return score, true
}

func scoreGateRunWorkflow(ctx gateMatchCtx, run *GHWorkflowRun) (int, bool) {
	if ctx.workflowHint == "" {
		return 0, true
	}
	if !workflowNameMatches(ctx.workflowHint, run.WorkflowName, run.Name) {
		return 0, false
	}
	return 200, true
}

func scoreGateRunLocal(ctx gateMatchCtx, run *GHWorkflowRun) int {
	score := 0
	if ctx.currentCommit != "" && run.HeadSha == ctx.currentCommit {
		score += 100
	}
	// Heuristic 2: Branch match. Skipped for a foreign repo and mirrors its
	// currentCommit != "" guard: if local branch detection failed, currentBranch
	// is "" and a run with an empty HeadBranch must not accidentally "match" it.
	if !ctx.foreignRepo && ctx.currentBranch != "" && run.HeadBranch == ctx.currentBranch {
		score += 50
	}
	return score
}

func gateRunTimeScore(timeDiff time.Duration) int {
	switch {
	case timeDiff < 5*time.Minute:
		return 30
	case timeDiff < 10*time.Minute:
		return 20
	case timeDiff < 30*time.Minute:
		return 10
	default:
		return 0
	}
}

// updateGateAwaitID updates a gate's await_id field
func updateGateAwaitID(_ interface{}, gateID, runID string) error {
	updates := map[string]interface{}{
		"await_id": runID,
	}
	if err := getStore().UpdateIssue(getRootContext(), gateID, updates, getActor()); err != nil {
		return err
	}
	return nil
}
