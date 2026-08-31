package main

import (
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

// gateAddWaiterCmd adds a waiter to a gate
var gateAddWaiterCmd = &cobra.Command{
	Use:   "add-waiter <gate-id> <waiter>",
	Short: "Add a waiter to a gate",
	Long: `Register an agent as a waiter on a gate bead.

When the gate closes, the waiter will receive a wake notification via 'bd gate wake'.
The waiter is typically the worker's address (e.g., "my-project/workers/agent-1").

This is used by 'bd done --phase-complete' to register for gate wake notifications.`,
	Args:          cobra.ExactArgs(2),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if usesProxiedServer() {
			return runGateAddWaiterProxiedServer(cmd, getRootContext(), args)
		}
		if err := CheckReadonly("gate add-waiter"); err != nil {
			return err
		}

		evt := metrics.NewCommandEvent("gate-add-waiter")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		gateID := args[0]
		waiter := args[1]
		ctx := getRootContext()

		var issue *types.Issue
		var err error

		issue, err = getStore().GetIssue(ctx, gateID)
		if err != nil {
			return HandleError("gate not found: %s", gateID)
		}

		if issue.IssueType != "gate" {
			return HandleError("%s is not a gate issue (type=%s)", gateID, issue.IssueType)
		}

		for _, w := range issue.Waiters {
			if w == waiter {
				renderGateWaiterAlready(gateID)
				return nil
			}
		}

		newWaiters := append(issue.Waiters, waiter)

		updates := map[string]interface{}{
			"waiters": newWaiters,
		}
		if err := getStore().UpdateIssue(ctx, gateID, updates, getActor()); err != nil {
			return HandleError("updating gate: %v", err)
		}

		commandDidWrite.Store(true)

		renderGateWaiterAdded(gateID, waiter)
		return nil
	},
}

// renderGateWaiterAlready and renderGateWaiterAdded are shared by the direct
// and proxied-server routes so `bd gate add-waiter` prints identically on both.
func renderGateWaiterAlready(gateID string) {
	fmt.Printf("Waiter already registered on gate %s\n", gateID)
}

func renderGateWaiterAdded(gateID, waiter string) {
	fmt.Printf("%s Added waiter to gate %s: %s\n", ui.RenderPass("✓"), gateID, waiter)
}

// gateCreateCmd creates an ad-hoc gate issue that blocks another issue
var gateCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a gate that blocks an issue",
	Long: `Create an ad-hoc gate issue that blocks another issue until resolved.

The blocked issue will not appear in 'bd ready' until the gate is resolved
via 'bd gate resolve'.

Gate types:
  human   - Requires manual 'bd gate resolve' (default)
  timer   - Auto-resolves after --timeout duration
  gh:run  - Waits for GitHub Actions workflow
  gh:pr   - Waits for PR merge

Examples:
  bd gate create --blocks bd-abc
  bd gate create --type=human --blocks bd-abc --reason="Need design review"
  bd gate create --type=timer --blocks bd-abc --timeout=2h
  bd gate create --type=gh:pr --blocks bd-abc --await-id=42
  bd gate create --blocks bd-abc --title="Gate: awaiting owner sign-off"`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if usesProxiedServer() {
			return runGateCreateProxiedServer(cmd, getRootContext())
		}
		if err := CheckReadonly("gate create"); err != nil {
			return err
		}

		evt := metrics.NewCommandEvent("gate-create")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		in, err := gatherGateCreateInput(cmd)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}

		ctx := getRootContext()

		targetIssue, err := getStore().GetIssue(ctx, in.blocksID)
		if err != nil {
			return HandleErrorRespectJSON("issue not found: %s", in.blocksID)
		}

		gate := buildGateIssue(in, targetIssue.ID)
		metadata, metaErr := repoMetadataForGate(in.gateType, targetIssue)
		if metaErr != nil {
			return HandleErrorRespectJSON("invalid GitHub repository metadata on %s: %v", targetIssue.ID, metaErr)
		}
		gate.Metadata = metadata

		if err := getStore().CreateIssue(ctx, gate, getActor()); err != nil {
			return HandleErrorRespectJSON("creating gate: %v", err)
		}

		dep := &types.Dependency{
			IssueID:     targetIssue.ID,
			DependsOnID: gate.ID,
			Type:        types.DepBlocks,
		}
		if err := getStore().AddDependency(ctx, dep, getActor()); err != nil {
			return HandleErrorRespectJSON("adding blocking dependency: %v", err)
		}

		commitMsg := fmt.Sprintf("bd: create gate %s blocking %s", gate.ID, targetIssue.ID)
		if err := getStore().Commit(ctx, commitMsg); err != nil && !isDoltNothingToCommit(err) {
			return HandleErrorRespectJSON("failed to commit: %v", err)
		}

		if isJSONOutput() {
			return outputJSON(gate)
		}

		renderGateCreated(gate, targetIssue, in)
		return nil
	},
}

// gateCreateInput carries `bd gate create`'s parsed flags. Both routes gather
// it through gatherGateCreateInput so they cannot drift on flag semantics.
type gateCreateInput struct {
	blocksID  string
	gateType  string
	reason    string
	awaitID   string
	titleFlag string
	timeout   time.Duration
}

func gatherGateCreateInput(cmd *cobra.Command) (gateCreateInput, error) {
	in := gateCreateInput{}
	in.blocksID, _ = cmd.Flags().GetString("blocks")
	in.gateType, _ = cmd.Flags().GetString("type")
	in.reason, _ = cmd.Flags().GetString("reason")
	in.awaitID, _ = cmd.Flags().GetString("await-id")
	in.titleFlag, _ = cmd.Flags().GetString("title")
	timeoutStr, _ := cmd.Flags().GetString("timeout")
	if timeoutStr != "" {
		parsed, err := time.ParseDuration(timeoutStr)
		if err != nil {
			return in, fmt.Errorf("invalid timeout: %v", err)
		}
		in.timeout = parsed
	}
	return in, nil
}

// buildGateIssue constructs the ad-hoc gate issue exactly the way the direct
// route always has; the proxied route reuses it for the same reason the
// renderers are shared.
func buildGateIssue(in gateCreateInput, targetID string) *types.Issue {
	title := fmt.Sprintf("Gate: %s", in.gateType)
	if in.awaitID != "" {
		title = fmt.Sprintf("Gate: %s %s", in.gateType, in.awaitID)
	}
	if in.titleFlag != "" {
		title = in.titleFlag
	}

	desc := fmt.Sprintf("Ad-hoc gate blocking %s", targetID)
	if in.reason != "" {
		desc = fmt.Sprintf("%s\n\nReason: %s", desc, in.reason)
	}

	return &types.Issue{
		IssueContent: types.IssueContent{
			Title:       title,
			Description: desc,
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.IssueType("gate"),
			Owner:     getOwner(),
		},
		IssueTimes: types.IssueTimes{
			CreatedBy: getActorWithGit(),
		},
		IssueCoord: types.IssueCoord{
			AwaitType: in.gateType,
			AwaitID:   in.awaitID,
			Timeout:   in.timeout,
		},
	}
}

// renderGateCreated is shared by the direct and proxied-server routes; the
// first line's "Created gate <id>" is parsed by downstream scripts, so both
// routes must print it identically.
func renderGateCreated(gate, targetIssue *types.Issue, in gateCreateInput) {
	fmt.Printf("%s Created gate %s (type: %s)\n", ui.RenderPass("✓"), ui.RenderID(gate.ID), in.gateType)
	fmt.Printf("  Blocks: %s (%s)\n", targetIssue.ID, targetIssue.Title)
	if in.reason != "" {
		fmt.Printf("  Reason: %s\n", in.reason)
	}
	if in.timeout > 0 {
		fmt.Printf("  Timeout: %s\n", in.timeout)
	}
	fmt.Printf("\nResolve with: bd gate resolve %s\n", gate.ID)
}

// gateShowCmd shows a gate issue
var gateShowCmd = &cobra.Command{
	Use:   "show <gate-id>",
	Short: "Show a gate issue",
	Long: `Display details of a gate issue including its waiters.

This is similar to 'bd show' but validates that the issue is a gate.`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if usesProxiedServer() {
			return runGateShowProxiedServer(cmd, getRootContext(), args)
		}
		evt := metrics.NewCommandEvent("gate-show")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		gateID := args[0]
		ctx := getRootContext()

		var issue *types.Issue
		var err error

		issue, err = getStore().GetIssue(ctx, gateID)
		if err != nil {
			return HandleErrorRespectJSON("gate not found: %s", gateID)
		}

		if issue.IssueType != "gate" {
			return HandleErrorRespectJSON("%s is not a gate issue (type=%s)", gateID, issue.IssueType)
		}

		if isJSONOutput() {
			return outputJSON(issue)
		}

		renderGateShow(issue)
		return nil
	},
}

// renderGateShow is shared by the direct and proxied-server routes; downstream
// scripts grep this plain-text output for markers, so both routes must print
// it identically.
func renderGateShow(issue *types.Issue) {
	statusSym := "○"
	if issue.Status == types.StatusClosed {
		statusSym = "●"
	}

	fmt.Printf("%s %s - %s\n", statusSym, ui.RenderID(issue.ID), issue.Title)
	fmt.Printf("  Status: %s\n", issue.Status)
	fmt.Printf("  Await Type: %s\n", issue.AwaitType)
	if issue.AwaitID != "" {
		fmt.Printf("  Await ID: %s\n", issue.AwaitID)
	}
	if issue.Timeout > 0 {
		fmt.Printf("  Timeout: %s\n", issue.Timeout)
	}
	if len(issue.Waiters) > 0 {
		fmt.Printf("  Waiters:\n")
		for _, w := range issue.Waiters {
			fmt.Printf("    - %s\n", w)
		}
	}
	if issue.Description != "" {
		fmt.Printf("  Description: %s\n", issue.Description)
	}
}

// gateResolveCmd manually closes a gate
var gateResolveCmd = &cobra.Command{
	Use:   "resolve <gate-id>",
	Short: "Manually resolve (close) a gate",
	Long: `Close a gate issue to unblock the step waiting on it.

This is equivalent to 'bd close <gate-id>' but with a more explicit name.
Use --reason to provide context for why the gate was resolved.`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if usesProxiedServer() {
			return runGateResolveProxiedServer(cmd, getRootContext(), args)
		}
		if err := CheckReadonly("gate resolve"); err != nil {
			return err
		}

		evt := metrics.NewCommandEvent("gate-resolve")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		gateID := args[0]
		reason, _ := cmd.Flags().GetString("reason")

		ctx := getRootContext()
		var issue *types.Issue
		var err error

		issue, err = getStore().GetIssue(ctx, gateID)
		if err != nil {
			return HandleError("gate not found: %s", gateID)
		}

		if issue.IssueType != "gate" {
			return HandleError("%s is not a gate issue (type=%s)", gateID, issue.IssueType)
		}

		if err := getStore().CloseIssue(ctx, gateID, reason, getActor(), ""); err != nil {
			return HandleError("closing gate: %v", err)
		}

		commandDidWrite.Store(true)

		renderGateResolved(gateID, reason)
		return nil
	},
}

// renderGateResolved is shared by the direct and proxied-server routes so
// `bd gate resolve` prints identically on both.
func renderGateResolved(gateID, reason string) {
	fmt.Printf("%s Gate resolved: %s\n", ui.RenderPass("✓"), gateID)
	if reason != "" {
		fmt.Printf("  Reason: %s\n", reason)
	}
}

// gateCheckCmd evaluates gates and closes those that are resolved
var gateCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Evaluate gates and close resolved ones",
	Long: `Evaluate gate conditions and automatically close resolved gates.

By default, checks all open gates. Use --type to filter by gate type.

Gate types:
  gh       - Check all GitHub gates (gh:run and gh:pr)
  gh:run   - Check GitHub Actions workflow runs
  gh:pr    - Check pull request merge status
  timer    - Check timer gates (auto-expire based on timeout)
  bead     - Check cross-rig bead gates
  all      - Check all gate types

GitHub gates use the 'gh' CLI to query status:
  - gh:run checks 'gh run view <id> --json status,conclusion'
  - gh:pr checks 'gh pr view <id> --json state,title'

A gate is resolved when:
  - gh:run: status=completed AND conclusion=success
  - gh:pr: state=MERGED
  - timer: current time > created_at + timeout
  - bead: target bead status=closed

A gate is escalated when:
  - gh:run: status=completed AND conclusion in (failure, canceled)
  - gh:pr: state=CLOSED

Examples:
  bd gate check              # Check all gates
  bd gate check --type=gh    # Check only GitHub gates
  bd gate check --type=gh:run # Check only workflow run gates
  bd gate check --type=timer # Check only timer gates
  bd gate check --type=bead  # Check only cross-rig bead gates
  bd gate check --dry-run    # Show what would happen without changes
  bd gate check --escalate   # Escalate expired/failed gates`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if usesProxiedServer() {
			return runGateCheckProxiedServer(cmd, getRootContext())
		}
		if err := CheckReadonly("gate check"); err != nil {
			return err
		}

		evt := metrics.NewCommandEvent("gate-check")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		gateTypeFilter, _ := cmd.Flags().GetString("type")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		escalateFlag, _ := cmd.Flags().GetBool("escalate")
		limit, _ := cmd.Flags().GetInt("limit")

		gateType := types.IssueType("gate")
		filter := types.IssueFilter{
			IssueFilterCore: types.IssueFilterCore{
				IssueType: &gateType,
				Limit:     limit,
			},
			IssueFilterFlags: types.IssueFilterFlags{
				ExcludeStatus: []types.Status{types.StatusClosed},
			},
		}

		ctx := getRootContext()

		gates, err := getStore().SearchIssues(ctx, "", filter)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}

		filteredGates := filterCheckableGates(gates, gateTypeFilter)
		if len(filteredGates) == 0 {
			printNoOpenGates(gateTypeFilter)
			return nil
		}

		var persistAwaitID func(gateID, runID string) error
		if !dryRun {
			persistAwaitID = func(gateID, runID string) error {
				return updateGateAwaitIDFunc(nil, gateID, runID)
			}
		}

		results := evaluateGates(ctx, filteredGates, time.Now(), getStore(), persistAwaitID)

		resolvedCount, escalatedCount, errorCount := applyGateCheckResults(
			results, dryRun, escalateFlag,
			func(gate *types.Issue, reason string) error {
				return closeGate(ctx, gate.ID, reason)
			},
		)

		return printGateCheckSummary(len(results), resolvedCount, escalatedCount, errorCount, dryRun)
	},
}
