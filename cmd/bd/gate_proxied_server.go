package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/audit"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/workapi"
)

type proxiedGateClose struct {
	before    *types.Issue
	after     *types.Issue
	oldStatus string
	reason    string
}

type gateCheckApply struct {
	closed    []proxiedGateClose
	updated   []*types.Issue
	closeErrs map[string]error
	awaitErrs map[string]error
}

type proxiedFreshReadGetter struct{}

func (proxiedFreshReadGetter) GetIssue(ctx context.Context, id string) (*types.Issue, error) {
	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return nil, err
	}
	defer uw.Close(ctx)
	return uw.IssueUseCase().GetIssue(ctx, id)
}

func runGateCheckProxiedServer(cmd *cobra.Command, ctx context.Context) error {
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

	if getUOWProvider() == nil {
		return HandleErrorRespectJSON("proxied-server UOW provider not initialized")
	}

	discovered := map[string]string{}
	filteredGates, err := loadProxiedCheckableGates(ctx, gateTypeFilter, limit)
	if err != nil {
		return err
	}

	if len(filteredGates) == 0 {
		printNoOpenGates(gateTypeFilter)
		return nil
	}
	results := evaluateGates(ctx, filteredGates, time.Now(), proxiedFreshReadGetter{}, proxiedGateAwaitPersistence(dryRun, discovered))

	if dryRun {
		resolved, escalated, errCount := applyGateCheckResults(results, true, escalateFlag, nil)
		return printGateCheckSummary(len(results), resolved, escalated, errCount, dryRun)
	}

	applied, err := applyProxiedGateCheck(ctx, discovered, results)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	recordProxiedGateCheckChanges(applied)
	applyProxiedGateAwaitErrors(results, applied.awaitErrs)

	resolved, escalated, errCount := applyGateCheckResults(results, false, escalateFlag,
		func(gate *types.Issue, reason string) error {
			return applied.closeErrs[gate.ID]
		})
	return printGateCheckSummary(len(results), resolved, escalated, errCount, dryRun)
}

func loadProxiedCheckableGates(ctx context.Context, gateTypeFilter string, limit int) ([]*types.Issue, error) {
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

	readUW, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return nil, err
	}
	defer readUW.Close(ctx)

	page, err := readUW.IssueUseCase().SearchIssues(ctx, "", filter)
	if err != nil {
		return nil, HandleErrorRespectJSON("%v", err)
	}
	return filterCheckableGates(page.Items, gateTypeFilter), nil
}

func proxiedGateAwaitPersistence(dryRun bool, discovered map[string]string) func(gateID, runID string) error {
	if dryRun {
		return nil
	}
	return func(gateID, runID string) error {
		discovered[gateID] = runID
		return nil
	}
}

func applyProxiedGateCheck(ctx context.Context, discovered map[string]string, results []gateCheckResult) (gateCheckApply, error) {
	return uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (gateCheckApply, string, error) {
		return buildProxiedGateCheckApply(ctx, uw, discovered, results)
	})
}

func buildProxiedGateCheckApply(ctx context.Context, uw uow.UnitOfWork, discovered map[string]string, results []gateCheckResult) (gateCheckApply, string, error) {
	out := gateCheckApply{
		closeErrs: map[string]error{},
		awaitErrs: map[string]error{},
	}
	recordProxiedGateRuns(ctx, uw, discovered, &out)
	for _, result := range results {
		closed, ok, err := closeProxiedResolvedGate(ctx, uw, result, out.awaitErrs)
		if err != nil {
			out.closeErrs[result.gate.ID] = err
			continue
		}
		if ok {
			out.closed = append(out.closed, closed)
		}
	}
	return out, "bd: gate check", nil
}

func recordProxiedGateRuns(ctx context.Context, uw uow.UnitOfWork, discovered map[string]string, out *gateCheckApply) {
	for gateID, runID := range discovered {
		if err := uw.IssueUseCase().UpdateIssue(ctx, gateID, map[string]any{"await_id": runID}, getActor()); err != nil {
			out.awaitErrs[gateID] = fmt.Errorf("failed to update gate with discovered run ID: %w", err)
			continue
		}
		if after, getErr := uw.IssueUseCase().GetIssue(ctx, gateID); getErr == nil && after != nil {
			out.updated = append(out.updated, after)
		}
	}
}

func closeProxiedResolvedGate(ctx context.Context, uw uow.UnitOfWork, result gateCheckResult, awaitErrs map[string]error) (proxiedGateClose, bool, error) {
	if result.err != nil || !result.resolved {
		return proxiedGateClose{}, false, nil
	}
	if _, awaitFailed := awaitErrs[result.gate.ID]; awaitFailed {
		return proxiedGateClose{}, false, nil
	}

	before, _ := uw.IssueUseCase().GetIssue(ctx, result.gate.ID)
	if before != nil && before.Status == types.StatusClosed {
		return proxiedGateClose{}, false, nil
	}
	res, err := uw.IssueUseCase().CloseIssue(ctx, result.gate.ID, domain.CloseIssueParams{Reason: result.reason}, getActor())
	if err != nil {
		return proxiedGateClose{}, false, err
	}
	return proxiedGateClose{
		before:    before,
		after:     res.Issue,
		oldStatus: proxiedGateOldStatus(before),
		reason:    result.reason,
	}, true, nil
}

func proxiedGateOldStatus(issue *types.Issue) string {
	if issue != nil && issue.Status != "" {
		return string(issue.Status)
	}
	return "open"
}

func recordProxiedGateCheckChanges(applied gateCheckApply) {
	for _, closed := range applied.closed {
		audit.LogFieldChange(closed.after.ID, "status", closed.oldStatus, "closed", getActor(), closed.reason)
	}
	if len(applied.closed) > 0 || len(applied.updated) > 0 {
		commandDidWrite.Store(true)
	}
}

func applyProxiedGateAwaitErrors(results []gateCheckResult, awaitErrs map[string]error) {
	for i := range results {
		if awaitErr, failed := awaitErrs[results[i].gate.ID]; failed {
			results[i].resolved = false
			results[i].escalated = false
			results[i].err = awaitErr
		}
	}
}

// gateProxiedNotFound reports whether an issue lookup failed because the row
// does not exist, as opposed to the read itself failing. The distinction is
// what keeps `bd gate show` fail-closed: "no gate" and "could not read" must
// not collapse into one message.
func gateProxiedNotFound(err error) bool {
	return errors.Is(err, storage.ErrNotFound) || errors.Is(err, sql.ErrNoRows)
}

func runGateShowProxiedServer(_ *cobra.Command, ctx context.Context, args []string) error {
	evt := metrics.NewCommandEvent("gate-show")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	gateID := args[0]

	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return err
	}
	defer uw.Close(ctx)

	issue, err := uw.IssueUseCase().GetIssue(ctx, gateID)
	if gateProxiedNotFound(err) {
		return HandleErrorRespectJSON("gate not found: %s", gateID)
	}
	if err != nil {
		// A failed read is not "no gate": it exits nonzero with its own
		// message so a caller grepping the output cannot mistake an
		// unreachable server for a missing gate.
		return HandleErrorRespectJSON("reading gate %s: %v", gateID, err)
	}

	if issue.IssueType != "gate" {
		return HandleErrorRespectJSON("%s is not a gate issue (type=%s)", gateID, issue.IssueType)
	}

	if isJSONOutput() {
		return outputJSON(issue)
	}

	renderGateShow(issue)
	return nil
}

type gateAddWaiterApply struct {
	already bool
	after   *types.Issue
}

func runGateAddWaiterProxiedServer(_ *cobra.Command, ctx context.Context, args []string) error {
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

	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}

	applied, err := uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (gateAddWaiterApply, string, error) {
		return addProxiedGateWaiter(ctx, uw, gateID, waiter)
	})
	if err != nil {
		return HandleError("%v", err)
	}

	if applied.already {
		renderGateWaiterAlready(gateID)
		return nil
	}

	commandDidWrite.Store(true)

	renderGateWaiterAdded(gateID, waiter)
	return nil
}

func addProxiedGateWaiter(ctx context.Context, uw uow.UnitOfWork, gateID, waiter string) (gateAddWaiterApply, string, error) {
	var out gateAddWaiterApply
	issue, err := loadProxiedGateForMutation(ctx, uw, gateID)
	if err != nil {
		return out, "", err
	}
	if gateHasWaiter(issue, waiter) {
		out.already = true
		// Empty commit message: a registered waiter is a no-op, and a
		// no-op writes no Dolt commit.
		return out, "", nil
	}

	newWaiters := append(issue.Waiters, waiter)
	if err := uw.IssueUseCase().UpdateIssue(ctx, gateID, map[string]any{"waiters": newWaiters}, getActor()); err != nil {
		return out, "", fmt.Errorf("updating gate: %w", err)
	}
	if after, getErr := uw.IssueUseCase().GetIssue(ctx, gateID); getErr == nil {
		out.after = after
	}
	return out, fmt.Sprintf("bd: gate add-waiter %s", gateID), nil
}

func loadProxiedGateForMutation(ctx context.Context, uw uow.UnitOfWork, gateID string) (*types.Issue, error) {
	issue, err := uw.IssueUseCase().GetIssue(ctx, gateID)
	if gateProxiedNotFound(err) {
		return nil, fmt.Errorf("gate not found: %s", gateID)
	}
	if err != nil {
		return nil, fmt.Errorf("reading gate %s: %w", gateID, err)
	}
	if issue.IssueType != "gate" {
		return nil, fmt.Errorf("%s is not a gate issue (type=%s)", gateID, issue.IssueType)
	}
	return issue, nil
}

func gateHasWaiter(issue *types.Issue, waiter string) bool {
	for _, registered := range issue.Waiters {
		if registered == waiter {
			return true
		}
	}
	return false
}

type gateCreateApply struct {
	gate   *types.Issue
	target *types.Issue
}

func runGateCreateProxiedServer(cmd *cobra.Command, ctx context.Context) error {
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

	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}

	// One transaction, one Dolt commit for the whole invocation: the direct
	// route's create + add-dependency + explicit store.Commit collapse into a
	// single unit of work carrying the same commit message. Semantically
	// equivalent, minus the window where the gate exists without its edge.
	applied, err := uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (gateCreateApply, string, error) {
		return createProxiedGate(ctx, uw, in)
	})
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	commandDidWrite.Store(true)

	if isJSONOutput() {
		return outputJSON(applied.gate)
	}

	renderGateCreated(applied.gate, applied.target, in)
	return nil
}

func createProxiedGate(ctx context.Context, uw uow.UnitOfWork, in gateCreateInput) (gateCreateApply, string, error) {
	var out gateCreateApply
	target, err := uw.IssueUseCase().GetIssue(ctx, in.blocksID)
	if err != nil {
		// The direct route reports every target-lookup failure as not-found;
		// keep that message for parity.
		return out, "", fmt.Errorf("issue not found: %s", in.blocksID)
	}

	gate := buildGateIssue(in, target.ID)
	metadata, metaErr := repoMetadataForGate(in.gateType, target)
	if metaErr != nil {
		return out, "", fmt.Errorf("invalid GitHub repository metadata on %s: %v", target.ID, metaErr)
	}
	gate.Metadata = metadata

	res, err := uw.IssueUseCase().CreateIssue(ctx, domain.CreateIssueParams{Issue: gate}, getActor())
	if err != nil {
		return out, "", fmt.Errorf("creating gate: %w", err)
	}

	dep := &types.Dependency{
		IssueID:     target.ID,
		DependsOnID: res.Issue.ID,
		Type:        types.DepBlocks,
	}
	if err := uw.DependencyUseCase().AddDependency(ctx, dep, getActor()); err != nil {
		return out, "", fmt.Errorf("adding blocking dependency: %w", err)
	}

	out.gate = res.Issue
	out.target = target
	return out, fmt.Sprintf("bd: create gate %s blocking %s", res.Issue.ID, target.ID), nil
}

type gateResolveApply struct {
	before    *types.Issue
	after     *types.Issue
	oldStatus string
	closed    bool // CloseIssueResult.Closed: false when the gate was already closed
}

func runGateResolveProxiedServer(cmd *cobra.Command, ctx context.Context, args []string) error {
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

	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}

	applied, err := uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (gateResolveApply, string, error) {
		return resolveProxiedGate(ctx, uw, gateID, reason)
	})
	if err != nil {
		return HandleError("%v", err)
	}

	// Audit only when this invocation actually closed the gate — a
	// double-resolve must not re-log it (same guard as the o.closed check in
	// close_proxied_server.go).
	if applied.closed && applied.after != nil {
		audit.LogFieldChange(applied.after.ID, "status", applied.oldStatus, "closed", getActor(), reason)
	}
	commandDidWrite.Store(true)

	renderGateResolved(gateID, reason)
	return nil
}

func resolveProxiedGate(ctx context.Context, uw uow.UnitOfWork, gateID, reason string) (gateResolveApply, string, error) {
	var out gateResolveApply
	issue, err := loadProxiedGateForMutation(ctx, uw, gateID)
	if err != nil {
		return out, "", err
	}
	res, err := uw.IssueUseCase().CloseIssue(ctx, gateID, domain.CloseIssueParams{Reason: reason}, getActor())
	if err != nil {
		return out, "", fmt.Errorf("closing gate: %w", err)
	}

	out.before = issue
	out.after = res.Issue
	out.oldStatus = proxiedGateOldStatus(issue)
	out.closed = res.Closed
	return out, fmt.Sprintf("bd: gate resolve %s", gateID), nil
}

func runGateListProxiedServer(cmd *cobra.Command, ctx context.Context, args []string) error {
	allFlag, _ := cmd.Flags().GetBool("all")
	limit, _ := cmd.Flags().GetInt("limit")

	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return err
	}
	defer uw.Close(ctx)

	if len(args) == 1 {
		return listProxiedGatesForTarget(ctx, uw, args[0], allFlag, limit)
	}

	return listAllProxiedGates(ctx, uw, allFlag, limit)
}

func listProxiedGatesForTarget(ctx context.Context, uw uow.UnitOfWork, targetID string, allFlag bool, limit int) error {
	target, isWisp, err := workapi.GetIssueOrWisp(ctx, workapi.NewUOWDetailSource(uw), targetID)
	if errors.Is(err, storage.ErrNotFound) {
		return HandleErrorRespectJSON("issue not found: %s", targetID)
	}
	if err != nil {
		return HandleErrorRespectJSON("resolving %s: %v", targetID, err)
	}

	metas := proxiedGateDependencyMetadata(ctx, uw, target.ID, isWisp)
	if metas.err != nil {
		return HandleErrorRespectJSON("%v", metas.err)
	}
	deps := make([]*types.Issue, 0, len(metas.items))
	for _, meta := range metas.items {
		if meta != nil {
			deps = append(deps, &meta.Issue)
		}
	}
	gates := filterIssueGates(deps, allFlag, limit)
	if isJSONOutput() {
		return outputJSON(gates)
	}
	displayGates(gates, allFlag)
	return nil
}

type proxiedGateDependencyResult struct {
	items []*types.IssueWithDependencyMetadata
	err   error
}

func proxiedGateDependencyMetadata(ctx context.Context, uw uow.UnitOfWork, targetID string, isWisp bool) proxiedGateDependencyResult {
	filter := domain.DepListFilter{Direction: domain.DepDirectionOut}
	if isWisp {
		items, err := uw.DependencyUseCase().ListWispWithIssueMetadata(ctx, targetID, filter)
		return proxiedGateDependencyResult{items: items, err: err}
	}
	items, err := uw.DependencyUseCase().ListWithIssueMetadata(ctx, targetID, filter)
	return proxiedGateDependencyResult{items: items, err: err}
}

func listAllProxiedGates(ctx context.Context, uw uow.UnitOfWork, allFlag bool, limit int) error {
	gateType := types.IssueType("gate")
	filter := types.IssueFilter{
		IssueFilterCore: types.IssueFilterCore{
			IssueType: &gateType,
			Limit:     limit,
		},
	}
	if !allFlag {
		filter.ExcludeStatus = []types.Status{types.StatusClosed}
	}
	page, err := uw.IssueUseCase().SearchIssues(ctx, "", filter)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	if isJSONOutput() {
		return outputJSON(page.Items)
	}
	displayGates(page.Items, allFlag)
	return nil
}
