package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/types"
)

// ghRunStatus holds the JSON response from 'gh run view'
type ghRunStatus struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Name       string `json:"name"`
}

// ghPRStatus holds the JSON response from 'gh pr view'
type ghPRStatus struct {
	State string `json:"state"`
	Title string `json:"title"`
}

type ghCommandRunner func(args ...string) (stdout, stderr []byte, err error)

func runGHCommand(args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.Command("gh", args...) // #nosec G204 -- callers pass validated values as an argument vector, without a shell
	var stdoutBuffer, stderrBuffer bytes.Buffer
	cmd.Stdout = &stdoutBuffer
	cmd.Stderr = &stderrBuffer
	err = cmd.Run()
	return stdoutBuffer.Bytes(), stderrBuffer.Bytes(), err
}

var (
	discoverRunIDByWorkflowNameFunc = discoverRunIDByWorkflowName
	updateGateAwaitIDFunc           = updateGateAwaitID
	checkGHRunStatusFunc            = checkGHRunStatus
)

// isNumericID returns true if the string contains only digits (a GitHub run ID)
func isNumericID(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// githubRepoFromIssue returns a validated [HOST/]OWNER/REPO value from metadata.repo.
// A missing repo key, or metadata without a repo key at all, means the current
// Git repository should be used. An explicit `"repo":null` or a non-string
// repo value is rejected as malformed rather than silently falling back to
// the current repository - the docs promise malformed values are rejected,
// and a silent fallback here is the dangerous direction (it can point a
// cross-repo check at the wrong repository instead of failing loudly).
func githubRepoFromIssue(issue *types.Issue) (string, error) {
	if issue == nil || len(issue.Metadata) == 0 || string(issue.Metadata) == "null" {
		return "", nil
	}

	raw, err := parseGitHubRepoMetadata(issue.Metadata)
	if err != nil {
		return "", err
	}
	repoRaw, hasRepo := raw["repo"]
	if !hasRepo {
		return "", nil
	}

	repo, err := decodeGitHubRepoValue(repoRaw)
	if err != nil {
		return "", err
	}
	if repo == "" {
		return "", nil
	}
	return validateGitHubRepo(repo)
}

func parseGitHubRepoMetadata(metadata json.RawMessage) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &raw); err != nil {
		return nil, fmt.Errorf("metadata must be a JSON object: %w", err)
	}
	return raw, nil
}

func decodeGitHubRepoValue(repoRaw json.RawMessage) (string, error) {
	var repoValue interface{}
	if err := json.Unmarshal(repoRaw, &repoValue); err != nil {
		return "", fmt.Errorf("metadata.repo: %w", err)
	}
	if repoValue == nil {
		return "", fmt.Errorf("metadata.repo must not be null")
	}
	repo, ok := repoValue.(string)
	if !ok {
		return "", fmt.Errorf("metadata.repo must be a string, got %T", repoValue)
	}
	return repo, nil
}

func validateGitHubRepo(repo string) (string, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 && len(parts) != 3 {
		return "", fmt.Errorf("repo %q must use OWNER/REPO or HOST/OWNER/REPO", repo)
	}
	for _, part := range parts {
		if err := validateGitHubRepoPart(repo, part); err != nil {
			return "", err
		}
	}

	return repo, nil
}

func validateGitHubRepoPart(repo, part string) error {
	if part == "" {
		return fmt.Errorf("repo %q contains an empty path component", repo)
	}
	for _, char := range part {
		if strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.", char) {
			continue
		}
		return fmt.Errorf("repo %q contains invalid character %q", repo, char)
	}
	return nil
}

// isGitHubGateType returns true for gate types whose condition is checked
// against a GitHub repository (gh:run, gh:pr, and any future gh:* type).
func isGitHubGateType(gateType string) bool {
	return strings.HasPrefix(gateType, "gh:")
}

// repoMetadataForGate computes the metadata to store on a new ad-hoc gate,
// inheriting a validated GitHub repo selector from the blocked issue.
//
// This is restricted to gh:* gate types (SF4): "repo" is legal, unrelated
// metadata on any issue (the metadata contract allows arbitrary JSON), so
// running GitHub-repo validation for human/timer gates would fail ordinary
// gate creation whenever the blocked issue happened to carry a non-GitHub-
// shaped "repo" key. Only gh:run/gh:pr gates need the value at check time,
// so only they inherit and validate it here.
func repoMetadataForGate(gateType string, targetIssue *types.Issue) (json.RawMessage, error) {
	if !isGitHubGateType(gateType) {
		return nil, nil
	}
	repo, err := githubRepoFromIssue(targetIssue)
	if err != nil {
		return nil, err
	}
	if repo == "" {
		return nil, nil
	}
	metadata, err := json.Marshal(map[string]string{"repo": repo})
	if err != nil {
		return nil, err
	}
	return metadata, nil
}

// queryGitHubRunsForWorkflow queries recent runs for a specific workflow using gh CLI.
// Returns runs sorted newest-first (GitHub API default).
func queryGitHubRunsForWorkflow(workflow string, limit int) ([]GHWorkflowRun, error) {
	return queryGitHubRunsForWorkflowInRepo(workflow, limit, "")
}

func queryGitHubRunsForWorkflowInRepo(workflow string, limit int, repo string) ([]GHWorkflowRun, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, fmt.Errorf("gh CLI not found: install from https://cli.github.com")
	}
	return queryGitHubRunsForWorkflowInRepoWithRunner(workflow, limit, repo, runGHCommand)
}

func queryGitHubRunsForWorkflowInRepoWithRunner(workflow string, limit int, repo string, runGH ghCommandRunner) ([]GHWorkflowRun, error) {
	args := []string{
		"run", "list",
		"--workflow", workflow,
		"--json", "databaseId,name,status,conclusion,createdAt,workflowName",
		"--limit", fmt.Sprintf("%d", limit),
	}
	if repo != "" {
		args = append(args, "--repo", repo)
	}

	output, stderr, err := runGH(args...)
	if err != nil {
		if len(stderr) > 0 {
			return nil, fmt.Errorf("gh run list --workflow=%s failed: %s", workflow, string(stderr))
		}
		return nil, fmt.Errorf("gh run list: %w", err)
	}

	var runs []GHWorkflowRun
	if err := json.Unmarshal(output, &runs); err != nil {
		return nil, fmt.Errorf("parse gh output: %w", err)
	}

	return runs, nil
}

// discoverRunIDByWorkflowName queries GitHub for the most recent run of a workflow.
// Returns (runID, error). This is ZFC-compliant: "most recent run" is deterministic.
func discoverRunIDByWorkflowName(workflowHint string) (string, error) {
	return discoverRunIDByWorkflowNameInRepo(workflowHint, "")
}

func discoverRunIDByWorkflowNameInRepo(workflowHint, repo string) (string, error) {
	return discoverRunIDByWorkflowNameInRepoWithRunner(workflowHint, repo, runGHCommand)
}

// discoverRunIDByWorkflowNameInRepoWithRunner is the runner-injectable form of
// discoverRunIDByWorkflowNameInRepo. checkGHRunWithRunner's cross-repo branch
// must call this (not discoverRunIDByWorkflowNameInRepo directly) so the same
// injected ghCommandRunner seam used everywhere else in the gh:run/gh:pr
// checks also covers cross-repo discovery, keeping that path unit-testable
// without a live `gh` CLI (standards note on the SF1 review).
func discoverRunIDByWorkflowNameInRepoWithRunner(workflowHint, repo string, runGH ghCommandRunner) (string, error) {
	// Query GitHub directly for this workflow (efficient, avoids limit issues)
	runs, err := queryGitHubRunsForWorkflowInRepoWithRunner(workflowHint, 5, repo, runGH)
	if err != nil {
		return "", fmt.Errorf("failed to query workflow runs: %w", err)
	}

	if len(runs) == 0 {
		return "", fmt.Errorf("no runs found for workflow '%s'", workflowHint)
	}

	// Take the most recent run (gh returns newest-first)
	// This is deterministic: "most recent" is a total ordering by creation time
	return fmt.Sprintf("%d", runs[0].DatabaseID), nil
}

// checkGHRun checks a GitHub Actions workflow run gate.
// When persistAwaitID is nil, workflow-name discovery stays in-memory only.
func checkGHRun(gate *types.Issue, persistAwaitID func(gateID, runID string) error) (resolved, escalated bool, reason string, err error) {
	return checkGHRunWithRunner(gate, persistAwaitID, runGHCommand)
}

func checkGHRunWithRunner(gate *types.Issue, persistAwaitID func(gateID, runID string) error, runGH ghCommandRunner) (resolved, escalated bool, reason string, err error) {
	if gate.AwaitID == "" {
		return false, false, "no run ID specified - set await_id or use workflow name hint", nil
	}

	runID := gate.AwaitID
	repo, repoErr := githubRepoFromIssue(gate)
	if repoErr != nil {
		return false, false, "", repoErr
	}

	// If await_id is a workflow name hint (non-numeric), auto-discover the run ID
	if !isNumericID(gate.AwaitID) {
		var discoveredID string
		var discoverErr error
		if repo == "" {
			discoveredID, discoverErr = discoverRunIDByWorkflowNameFunc(gate.AwaitID)
		} else {
			discoveredID, discoverErr = discoverRunIDByWorkflowNameInRepoWithRunner(gate.AwaitID, repo, runGH)
		}
		if discoverErr != nil {
			return false, false, fmt.Sprintf("workflow hint '%s': %v", gate.AwaitID, discoverErr), nil
		}

		if persistAwaitID != nil {
			// Non-dry-run flows persist the numeric run ID for future checks.
			if updateErr := persistAwaitID(gate.ID, discoveredID); updateErr != nil {
				return false, false, "", fmt.Errorf("failed to update gate with discovered run ID: %w", updateErr)
			}
		}

		runID = discoveredID
	}

	if repo == "" {
		return checkGHRunStatusFunc(runID)
	}
	return checkGHRunStatusInRepoWithRunner(runID, repo, runGH)
}

func checkGHRunStatus(runID string) (resolved, escalated bool, reason string, err error) {
	return checkGHRunStatusInRepo(runID, "")
}

func checkGHRunStatusInRepo(runID, repo string) (resolved, escalated bool, reason string, err error) {
	return checkGHRunStatusInRepoWithRunner(runID, repo, runGHCommand)
}

func checkGHRunStatusInRepoWithRunner(runID, repo string, runGH ghCommandRunner) (resolved, escalated bool, reason string, err error) {
	// Run: gh run view <id> --json status,conclusion,name
	args := []string{"run", "view", runID, "--json", "status,conclusion,name"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	stdout, stderr, runErr := runGH(args...)
	if runErr != nil {
		return classifyGHRunStatusCommandError(stderr, runErr)
	}
	return evaluateGHRunStatus(stdout)
}

func classifyGHRunStatusCommandError(stderr []byte, runErr error) (resolved, escalated bool, reason string, err error) {
	if strings.Contains(string(stderr), "command not found") ||
		strings.Contains(runErr.Error(), "executable file not found") {
		return false, false, "", fmt.Errorf("gh CLI not installed")
	}
	if strings.Contains(string(stderr), "not found") {
		return false, true, "workflow run not found", nil
	}
	return false, false, "", fmt.Errorf("gh run view failed: %s", string(stderr))
}

func evaluateGHRunStatus(stdout []byte) (resolved, escalated bool, reason string, err error) {
	var status ghRunStatus
	if parseErr := json.Unmarshal(stdout, &status); parseErr != nil {
		return false, false, "", fmt.Errorf("failed to parse gh output: %w", parseErr)
	}

	// Evaluate status
	switch status.Status {
	case "completed":
		switch status.Conclusion {
		case "success":
			return true, false, fmt.Sprintf("workflow '%s' succeeded", status.Name), nil
		case "failure":
			return false, true, fmt.Sprintf("workflow '%s' failed", status.Name), nil
		case "canceled", "cancel" + "led":
			return false, true, fmt.Sprintf("workflow '%s' was canceled", status.Name), nil
		case "skipped":
			return true, false, fmt.Sprintf("workflow '%s' was skipped", status.Name), nil
		default:
			return false, true, fmt.Sprintf("workflow '%s' concluded with %s", status.Name, status.Conclusion), nil
		}
	case "in_progress", "queued", "pending", "waiting":
		return false, false, fmt.Sprintf("workflow '%s' is %s", status.Name, status.Status), nil
	default:
		return false, false, fmt.Sprintf("workflow '%s' status: %s", status.Name, status.Status), nil
	}
}

// checkGHPR checks a GitHub pull request gate
func checkGHPR(gate *types.Issue) (resolved, escalated bool, reason string, err error) {
	return checkGHPRWithRunner(gate, runGHCommand)
}

func checkGHPRWithRunner(gate *types.Issue, runGH ghCommandRunner) (resolved, escalated bool, reason string, err error) {
	if gate.AwaitID == "" {
		return false, false, "no PR number specified", nil
	}

	repo, repoErr := githubRepoFromIssue(gate)
	if repoErr != nil {
		return false, false, "", repoErr
	}

	// Run: gh pr view <id> --json state,title [--repo <repo>]
	args := []string{"pr", "view", gate.AwaitID, "--json", "state,title"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	stdout, stderr, runErr := runGH(args...)
	if runErr != nil {
		return classifyGHPRCommandError(stderr, runErr)
	}
	return evaluateGHPRStatus(stdout)
}

func classifyGHPRCommandError(stderr []byte, runErr error) (resolved, escalated bool, reason string, err error) {
	if strings.Contains(string(stderr), "command not found") ||
		strings.Contains(runErr.Error(), "executable file not found") {
		return false, false, "", fmt.Errorf("gh CLI not installed")
	}
	if strings.Contains(string(stderr), "not found") || strings.Contains(string(stderr), "Could not resolve") {
		return false, true, "pull request not found", nil
	}
	return false, false, "", fmt.Errorf("gh pr view failed: %s", string(stderr))
}

func evaluateGHPRStatus(stdout []byte) (resolved, escalated bool, reason string, err error) {
	var status ghPRStatus
	if parseErr := json.Unmarshal(stdout, &status); parseErr != nil {
		return false, false, "", fmt.Errorf("failed to parse gh output: %w", parseErr)
	}

	// Evaluate status
	switch status.State {
	case "MERGED":
		return true, false, fmt.Sprintf("PR '%s' was merged", status.Title), nil
	case "CLOSED":
		return false, true, fmt.Sprintf("PR '%s' was closed without merging", status.Title), nil
	case "OPEN":
		return false, false, fmt.Sprintf("PR '%s' is still open", status.Title), nil
	default:
		return false, false, fmt.Sprintf("PR '%s' state: %s", status.Title, status.State), nil
	}
}

// checkTimer checks a timer gate for expiration
// Note: timers resolve but never escalate (escalated is always false by design)
func checkTimer(gate *types.Issue, now time.Time) (resolved, escalated bool, reason string, err error) { //nolint:unparam // escalated intentionally always false
	if gate.Timeout == 0 {
		return false, false, "timer gate without timeout configured", fmt.Errorf("no timeout set")
	}

	expiresAt := gate.CreatedAt.Add(gate.Timeout)
	if now.After(expiresAt) {
		expired := now.Sub(expiresAt).Round(time.Second)
		return true, false, fmt.Sprintf("timer expired %s ago", expired), nil
	}

	remaining := expiresAt.Sub(now).Round(time.Second)
	return false, false, fmt.Sprintf("expires in %s", remaining), nil
}

// issueGetter is the one storage method checkBeadGate needs, split out so
// tests can fake the lookup without standing up a Dolt store.
type issueGetter interface {
	GetIssue(ctx context.Context, id string) (*types.Issue, error)
}

// checkBeadGate checks if a bead gate is satisfied.
// Returns (satisfied, reason).
//
// A plain await_id (no colon) names a bead in THIS rig's database: the gate
// resolves once that bead closes — the common case, an agent idle-waiting on
// local work (wy-hgms2; the old unconditional cross-rig refusal left every
// local bead gate permanently pending and its waiters asleep).
//
// The historical cross-rig form <rig>:<bead-id> cannot be evaluated since
// multi-rig routing was removed; it stays pending with a descriptive message.
func checkBeadGate(ctx context.Context, st issueGetter, awaitID string) (bool, string) {
	if awaitID == "" {
		return false, "bead gate has no await_id"
	}
	if strings.Contains(awaitID, ":") {
		return false, fmt.Sprintf("cross-rig bead gate %q cannot be checked (multi-rig routing removed)", awaitID)
	}
	if st == nil {
		return false, fmt.Sprintf("bead gate %q: no local store available", awaitID)
	}
	issue, err := st.GetIssue(ctx, awaitID)
	if err != nil {
		return false, fmt.Sprintf("bead gate %q: %v", awaitID, err)
	}
	if issue == nil {
		return false, fmt.Sprintf("bead gate %q: bead not found", awaitID)
	}
	if issue.Status == types.StatusClosed {
		return true, fmt.Sprintf("bead %s closed", awaitID)
	}
	return false, fmt.Sprintf("bead %s is %s", awaitID, issue.Status)
}

// closeGate closes a gate issue with the given reason
func closeGate(_ interface{}, gateID, reason string) error {
	if err := getStore().CloseIssue(getRootContext(), gateID, reason, getActor(), ""); err != nil {
		return err
	}
	commandDidWrite.Store(true)
	return nil
}

// escalateGate sends an escalation for a failed/expired gate
func escalateGate(gate *types.Issue, reason string) {
	topic := fmt.Sprintf("Gate escalation: %s", gate.ID)
	message := fmt.Sprintf("Gate %s needs attention.\nType: %s\nReason: %s\nCreated: %s",
		gate.ID,
		gate.AwaitType,
		reason,
		gate.CreatedAt.Format(time.RFC3339))

	// Call gt escalate if available
	escalateCmd := exec.Command("gt", "escalate", topic, "-s", "HIGH", "-m", message)
	escalateCmd.Stdout = os.Stdout
	escalateCmd.Stderr = os.Stderr
	if err := escalateCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: escalation failed for %s: %v\n", gate.ID, err)
	}
}
