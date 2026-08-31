// Package main provides the bd CLI commands.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/jonbaldie/beads/internal/ado"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/spf13/cobra"
)

type adoSyncSession struct {
	cfg              ADOConfig
	flags            adoSyncFlags
	at               *ado.Tracker
	engine           *tracker.Engine
	opts             tracker.SyncOptions
	out              io.Writer
	warnings         []string
	bootstrapMatched int
}

// runADOSync implements the ado sync command.
// Uses the tracker.Engine for all sync operations.
func runADOSync(cmd *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("ado sync is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("ado-sync")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	flags := adoSyncFlagsFromCommand(cmd)
	cfg, conflictStrategy, err := validateADOSyncRequest(flags)
	if err != nil {
		return err
	}
	if !flags.dryRun {
		if err := CheckReadonly("ado sync"); err != nil {
			return err
		}
	}

	ctx := context.Background()
	session, err := newADOSyncSession(ctx, cmd, cfg, flags, conflictStrategy)
	if err != nil {
		return err
	}
	result, err := session.engine.Sync(ctx, session.opts)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return err
	}

	linksPushed := syncADOSyncLinks(ctx, session)
	reconcileResult := reconcileADOSync(ctx, session)
	return outputADOSync(session, result, linksPushed, reconcileResult)
}

func validateADOSyncRequest(flags adoSyncFlags) (ADOConfig, ADOConflictStrategy, error) {
	cfg := getADOConfig()
	if err := validateADOConfig(cfg); err != nil {
		return cfg, "", err
	}
	if flags.pullOnly && flags.pushOnly {
		return cfg, "", fmt.Errorf("cannot use both --pull-only and --push-only")
	}
	conflictStrategy, err := getADOConflictStrategy(flags.preferLocal, flags.preferADO, flags.preferNewer)
	if err != nil {
		return cfg, "", fmt.Errorf("%w (--prefer-local, --prefer-ado, --prefer-newer)", err)
	}
	return cfg, conflictStrategy, nil
}

func newADOSyncSession(ctx context.Context, cmd *cobra.Command, cfg ADOConfig, flags adoSyncFlags, conflictStrategy ADOConflictStrategy) (*adoSyncSession, error) {
	if err := ensureStoreActive(); err != nil {
		return nil, fmt.Errorf("database not available: %w", err)
	}

	session := &adoSyncSession{cfg: cfg, flags: flags, out: cmd.OutOrStdout()}
	session.at = &ado.Tracker{}
	cliProjects, _ := cmd.Flags().GetStringSlice("project")
	if len(cliProjects) > 0 {
		session.at.SetProjects(tracker.DeduplicateStrings(cliProjects))
	}
	if err := session.at.Init(ctx, getStore()); err != nil {
		return nil, fmt.Errorf("initializing Azure DevOps tracker: %w", err)
	}

	filters, err := configureADOSyncFilters(ctx, cmd, flags, session.at)
	if err != nil {
		return nil, err
	}
	session.engine = tracker.NewEngine(session.at, getStore(), getActor())
	configureADOSyncMessages(session)
	session.engine.PullHooks = buildADOPullHooks(ctx, session.at, flags.bootstrapMatch, flags.noCreate, &session.bootstrapMatched, session.engine.OnWarning)
	session.engine.PushHooks = buildADOPushHooks(session.at.FieldMapper(), session.at.IsExternalRef, filters, flags.noCreate)

	pull := !flags.pushOnly
	push := !flags.pullOnly
	session.opts = tracker.SyncOptions{Pull: pull, Push: push, DryRun: flags.dryRun}
	if err := applySelectiveSyncFlags(cmd, &session.opts, push); err != nil {
		return nil, err
	}
	applyADOConflictStrategy(&session.opts, conflictStrategy)
	if flags.dryRun && !isJSONOutput() {
		_, _ = fmt.Fprintln(session.out, "Dry run mode - no changes will be made")
		_, _ = fmt.Fprintln(session.out)
	}
	return session, nil
}

func configureADOSyncFilters(ctx context.Context, cmd *cobra.Command, flags adoSyncFlags, at *ado.Tracker) (*ado.PullFilters, error) {
	filters := buildADOPullFilters(ctx, cmd, flags)
	if filters == nil {
		return nil, nil
	}
	if err := filters.Validate(); err != nil {
		return nil, fmt.Errorf("invalid pull filter: %w", err)
	}
	at.SetFilters(filters)
	return filters, nil
}

func configureADOSyncMessages(session *adoSyncSession) {
	if !isJSONOutput() {
		session.engine.OnMessage = func(msg string) { _, _ = fmt.Fprintln(session.out, "  "+msg) } //nolint:gosec // G705: CLI text output, not HTML.
	}
	session.engine.OnWarning = func(msg string) {
		session.warnings = append(session.warnings, msg)
		_, _ = fmt.Fprintf(os.Stderr, "Warning: %s\n", msg) //nolint:gosec // G705: CLI stderr, not HTML.
	}
}

func applyADOConflictStrategy(opts *tracker.SyncOptions, strategy ADOConflictStrategy) {
	switch strategy {
	case ADOConflictPreferLocal:
		opts.ConflictResolution = tracker.ConflictLocal
	case ADOConflictPreferADO:
		opts.ConflictResolution = tracker.ConflictExternal
	default:
		opts.ConflictResolution = tracker.ConflictTimestamp
	}
}

func syncADOSyncLinks(ctx context.Context, session *adoSyncSession) int {
	if session.flags.dryRun || !session.opts.Push {
		return 0
	}
	adoClient := session.at.ADOClient()
	if adoClient == nil {
		return 0
	}
	linkResolver := ado.NewLinkResolver(adoClient)
	linksPushed, linkWarnings := pushADOLinks(ctx, linkResolver, session.at, getStore(), session.engine.OnWarning)
	session.warnings = append(session.warnings, linkWarnings...)
	return linksPushed
}

func reconcileADOSync(ctx context.Context, session *adoSyncSession) *ado.ReconcileResult {
	if session.flags.dryRun {
		return nil
	}
	client, err := getADOClient(session.cfg)
	if err != nil {
		session.warnings = append(session.warnings, fmt.Sprintf("Reconciliation skipped: %v", err))
		return nil
	}
	reconciler := ado.NewReconciler(client, getStore())
	if !session.flags.reconcile && !reconciler.ShouldReconcile(ctx) {
		recordADOReconcileCounter(ctx, reconciler, false)
		return nil
	}

	adoIDMap := collectADOWorkItemMap(ctx, session.at)
	workItemIDs := make([]int, 0, len(adoIDMap))
	for id := range adoIDMap {
		workItemIDs = append(workItemIDs, id)
	}
	result := reconcileADOItems(ctx, session, reconciler, adoIDMap, workItemIDs)
	recordADOReconcileCounter(ctx, reconciler, true)
	return result
}

func recordADOReconcileCounter(ctx context.Context, reconciler *ado.Reconciler, reset bool) {
	var err error
	if reset {
		err = reconciler.ResetCounter(ctx)
	} else {
		err = reconciler.IncrementCounter(ctx)
	}
	if err != nil && !isJSONOutput() {
		action := "increment"
		if reset {
			action = "reset"
		}
		_, _ = fmt.Fprintf(os.Stderr, "Warning: failed to %s reconcile counter: %v\n", action, err)
	}
}

func reconcileADOItems(ctx context.Context, session *adoSyncSession, reconciler *ado.Reconciler, adoIDMap map[int]string, workItemIDs []int) *ado.ReconcileResult {
	if len(workItemIDs) == 0 {
		return nil
	}
	rr, err := reconciler.Reconcile(ctx, workItemIDs)
	if err != nil {
		session.warnings = append(session.warnings, fmt.Sprintf("Reconciliation failed: %v", err))
		if !isJSONOutput() {
			_, _ = fmt.Fprintf(os.Stderr, "Warning: Reconciliation failed: %v\n", err)
		}
		return nil
	}
	closeDeletedADOIssues(ctx, session, adoIDMap, rr.Deleted)
	reportDeniedADOIssues(session, rr.Denied)
	return rr
}

func closeDeletedADOIssues(ctx context.Context, session *adoSyncSession, adoIDMap map[int]string, deleted []string) {
	for _, idStr := range deleted {
		adoID, err := strconv.Atoi(idStr)
		if err != nil {
			continue
		}
		localID, ok := adoIDMap[adoID]
		if !ok {
			continue
		}
		reason := fmt.Sprintf("ADO work item %s deleted", idStr)
		if err := getStore().CloseIssue(ctx, localID, reason, getActor(), ""); err != nil {
			msg := fmt.Sprintf("Failed to close %s for deleted ADO #%s: %v", localID, idStr, err)
			session.warnings = append(session.warnings, msg)
			if !isJSONOutput() {
				_, _ = fmt.Fprintf(os.Stderr, "Warning: %s\n", msg)
			}
			continue
		}
		msg := fmt.Sprintf("Closed %s: ADO work item %s deleted", localID, idStr)
		session.warnings = append(session.warnings, msg)
		if !isJSONOutput() {
			_, _ = fmt.Fprintf(session.out, "  %s\n", msg)
		}
	}
}

func reportDeniedADOIssues(session *adoSyncSession, denied []string) {
	for _, id := range denied {
		msg := fmt.Sprintf("ADO work item %s access denied (403)", id)
		session.warnings = append(session.warnings, msg)
		if !isJSONOutput() {
			_, _ = fmt.Fprintf(os.Stderr, "Warning: %s\n", msg)
		}
	}
}

func outputADOSync(session *adoSyncSession, result *tracker.SyncResult, linksPushed int, reconcileResult *ado.ReconcileResult) error {
	if isJSONOutput() {
		return outputADOSyncJSON(session, result, linksPushed, reconcileResult)
	}
	outputADOSyncText(session, result, linksPushed, reconcileResult)
	if session.flags.dryRun {
		_, _ = fmt.Fprintln(session.out)
		_, _ = fmt.Fprintln(session.out, "Run without --dry-run to apply changes")
		return nil
	}
	commandDidWrite.Store(true)
	return nil
}

func outputADOSyncJSON(session *adoSyncSession, result *tracker.SyncResult, linksPushed int, reconcileResult *ado.ReconcileResult) error {
	syncResult := adoSyncResult{
		DryRun:           session.flags.dryRun,
		Pulled:           result.Stats.Pulled,
		Pushed:           result.Stats.Pushed,
		Created:          result.Stats.Created,
		Updated:          result.Stats.Updated,
		Skipped:          result.Stats.Skipped,
		Conflicts:        result.Stats.Conflicts,
		Errors:           result.Stats.Errors,
		LinksPushed:      linksPushed,
		Warnings:         append(result.Warnings, session.warnings...),
		BootstrapMatched: session.bootstrapMatched,
	}
	if reconcileResult != nil {
		syncResult.Reconciled = true
		syncResult.ReconcileChecked = reconcileResult.Checked
		syncResult.ReconcileDeleted = len(reconcileResult.Deleted)
		syncResult.ReconcileDenied = len(reconcileResult.Denied)
	}
	return outputJSON(syncResult)
}

func outputADOSyncText(session *adoSyncSession, result *tracker.SyncResult, linksPushed int, reconcileResult *ado.ReconcileResult) {
	if session.flags.dryRun {
		return
	}
	if session.bootstrapMatched > 0 {
		_, _ = fmt.Fprintf(session.out, "✓ Bootstrap matched %d issues\n", session.bootstrapMatched)
	}
	printADOSyncStats(session.out, result, linksPushed)
	printADOReconcileSummary(session.out, reconcileResult)
}

func printADOSyncStats(out io.Writer, result *tracker.SyncResult, linksPushed int) {
	if result.Stats.Pulled > 0 {
		_, _ = fmt.Fprintf(out, "✓ Pulled %d issues (%d created, %d updated)\n",
			result.Stats.Pulled, result.Stats.Created, result.Stats.Updated)
	}
	if result.Stats.Pushed > 0 {
		_, _ = fmt.Fprintf(out, "✓ Pushed %d issues\n", result.Stats.Pushed)
	}
	if linksPushed > 0 {
		_, _ = fmt.Fprintf(out, "✓ Synced %d dependency links\n", linksPushed)
	}
	if result.Stats.Conflicts > 0 {
		_, _ = fmt.Fprintf(out, "→ Resolved %d conflicts\n", result.Stats.Conflicts)
	}
}

func printADOReconcileSummary(out io.Writer, result *ado.ReconcileResult) {
	if result == nil {
		return
	}
	_, _ = fmt.Fprintf(out, "✓ Reconciled %d work items", result.Checked)
	if len(result.Deleted) > 0 || len(result.Denied) > 0 {
		_, _ = fmt.Fprintf(out, " (%d deleted, %d denied)", len(result.Deleted), len(result.Denied))
	}
	_, _ = fmt.Fprintln(out)
}
