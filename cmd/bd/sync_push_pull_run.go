package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/ado"
	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/jira"
	"github.com/jonbaldie/beads/internal/linear"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/spf13/cobra"
)

// --- ADO implementations ---

func runADOPush(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("ado-push")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	if len(args) == 0 {
		return fmt.Errorf("at least one bead ID is required")
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if !dryRun {
		if err := CheckReadonly("ado push"); err != nil {
			return err
		}
	}

	cfg := getADOConfig()
	if err := validateADOConfig(cfg); err != nil {
		return err
	}
	if err := ensureStoreActive(); err != nil {
		return fmt.Errorf("database not available: %w", err)
	}

	ctx := cmd.Context()
	at := &ado.Tracker{}
	if err := at.Init(ctx, getStore()); err != nil {
		return fmt.Errorf("initializing Azure DevOps tracker: %w", err)
	}

	engine := tracker.NewEngine(at, getStore(), getActor())
	engine.OnMessage = func(msg string) { fmt.Println("  " + msg) }
	engine.OnWarning = func(msg string) { fmt.Fprintf(os.Stderr, "Warning: %s\n", msg) } //nolint:gosec // G705: CLI stderr, not HTML.

	result, err := engine.Sync(ctx, tracker.SyncOptions{
		Push:     true,
		Pull:     false,
		DryRun:   dryRun,
		IssueIDs: args,
	})
	if err != nil {
		return err
	}
	outputSyncResult(result, dryRun)
	return nil
}

func runADOPull(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("ado-pull")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	if len(args) == 0 {
		return fmt.Errorf("at least one bead ID or external reference is required")
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if !dryRun {
		if err := CheckReadonly("ado pull"); err != nil {
			return err
		}
	}

	cfg := getADOConfig()
	if err := validateADOConfig(cfg); err != nil {
		return err
	}
	if err := ensureStoreActive(); err != nil {
		return fmt.Errorf("database not available: %w", err)
	}

	ctx := cmd.Context()
	at := &ado.Tracker{}
	if err := at.Init(ctx, getStore()); err != nil {
		return fmt.Errorf("initializing Azure DevOps tracker: %w", err)
	}

	engine := tracker.NewEngine(at, getStore(), getActor())
	engine.PullHooks = buildADOPullHooks(ctx, at, false, false, new(int), engine.OnWarning)
	engine.OnMessage = func(msg string) { fmt.Println("  " + msg) }
	engine.OnWarning = func(msg string) { fmt.Fprintf(os.Stderr, "Warning: %s\n", msg) } //nolint:gosec // G705: CLI stderr, not HTML.

	result, err := engine.Sync(ctx, tracker.SyncOptions{
		Pull:     true,
		Push:     false,
		DryRun:   dryRun,
		IssueIDs: args,
	})
	if err != nil {
		return err
	}
	outputSyncResult(result, dryRun)
	return nil
}

// --- Jira implementations ---

func runJiraPush(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("jira-push")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	if len(args) == 0 {
		return HandleError("at least one bead ID is required")
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if !dryRun {
		if err := CheckReadonly("jira push"); err != nil {
			return err
		}
	}

	if err := ensureStoreActive(); err != nil {
		return HandleError("database not available: %v", err)
	}
	if err := validateJiraConfig(); err != nil {
		return HandleError("%v", err)
	}

	ctx := getRootContext()
	jt := &jira.Tracker{}
	if err := jt.Init(ctx, getStore()); err != nil {
		return HandleError("initializing Jira tracker: %v", err)
	}

	engine := tracker.NewEngine(jt, getStore(), getActor())
	engine.OnMessage = func(msg string) { fmt.Println("  " + msg) }
	engine.OnWarning = func(msg string) { fmt.Fprintf(os.Stderr, "Warning: %s\n", msg) } //nolint:gosec // G705: CLI stderr, not HTML.
	engine.PushHooks = buildJiraPushHooks(ctx)

	result, err := engine.Sync(ctx, tracker.SyncOptions{
		Push:     true,
		Pull:     false,
		DryRun:   dryRun,
		IssueIDs: args,
	})
	if err != nil {
		return HandleError("sync failed: %v", err)
	}
	outputSyncResult(result, dryRun)
	return nil
}

func runJiraPull(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("jira-pull")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	if len(args) == 0 {
		return HandleError("at least one bead ID or external reference is required")
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if !dryRun {
		if err := CheckReadonly("jira pull"); err != nil {
			return err
		}
	}

	if err := ensureStoreActive(); err != nil {
		return HandleError("database not available: %v", err)
	}
	if err := validateJiraConfig(); err != nil {
		return HandleError("%v", err)
	}

	ctx := getRootContext()
	jt := &jira.Tracker{}
	if err := jt.Init(ctx, getStore()); err != nil {
		return HandleError("initializing Jira tracker: %v", err)
	}

	engine := tracker.NewEngine(jt, getStore(), getActor())
	engine.OnMessage = func(msg string) { fmt.Println("  " + msg) }
	engine.OnWarning = func(msg string) { fmt.Fprintf(os.Stderr, "Warning: %s\n", msg) } //nolint:gosec // G705: CLI stderr, not HTML.

	result, err := engine.Sync(ctx, tracker.SyncOptions{
		Pull:     true,
		Push:     false,
		DryRun:   dryRun,
		IssueIDs: args,
	})
	if err != nil {
		return HandleError("sync failed: %v", err)
	}
	outputSyncResult(result, dryRun)
	return nil
}

// --- Linear implementations ---

func runLinearPush(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("linear-push")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	dryRun, err := validateLinearPushPullRequest(cmd, args, "linear push", "at least one bead ID is required")
	if err != nil {
		return err
	}
	releaseLock, err := acquireLinearPushPullLock()
	if err != nil {
		return err
	}
	defer releaseLock()
	return executeLinearPush(getRootContext(), args, dryRun)
}

func runLinearPull(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("linear-pull")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	dryRun, err := validateLinearPushPullRequest(cmd, args, "linear pull", "at least one bead ID or external reference is required")
	if err != nil {
		return err
	}
	relations, _ := cmd.Flags().GetBool("relations")
	releaseLock, err := acquireLinearPushPullLock()
	if err != nil {
		return err
	}
	defer releaseLock()
	return executeLinearPull(getRootContext(), args, dryRun, relations)
}

func validateLinearPushPullRequest(cmd *cobra.Command, args []string, operation, missingArgs string) (bool, error) {
	if len(args) == 0 {
		return false, HandleError("%s", missingArgs)
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if !dryRun {
		if err := CheckReadonly(operation); err != nil {
			return false, err
		}
	}
	return dryRun, nil
}

func acquireLinearPushPullLock() (func(), error) {
	lockDir := beads.FindBeadsDir()
	if lockDir == "" {
		return func() {}, nil
	}
	syncLock, err := linear.AcquireSyncLock(lockDir, true)
	if err != nil {
		return nil, HandleError("acquiring sync lock: %v", err)
	}
	return func() {
		if err := syncLock.Release(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to release sync lock: %v\n", err)
		}
	}, nil
}

func executeLinearPush(ctx context.Context, args []string, dryRun bool) error {
	if err := ensureStoreActive(); err != nil {
		return HandleError("database not available: %v", err)
	}
	if err := validateLinearConfig(nil); err != nil {
		return HandleError("%v", err)
	}

	teamIDs := getLinearTeamIDs(ctx, nil)
	if len(teamIDs) > 1 {
		return HandleError("linear push does not support multiple configured teams\nUse: bd linear sync --push --team <TEAM_ID>")
	}

	lt := linear.NewTracker()
	lt.SetTeamIDs(teamIDs)
	if err := lt.Init(ctx, getStore()); err != nil {
		return HandleError("initializing Linear tracker: %v", err)
	}
	if err := lt.ValidatePushStateMappings(ctx); err != nil {
		return HandleError("%v", err)
	}

	engine := tracker.NewEngine(lt, getStore(), getActor())
	engine.OnMessage = func(msg string) { fmt.Println("  " + msg) }
	engine.OnWarning = func(msg string) { fmt.Fprintf(os.Stderr, "Warning: %s\n", msg) } //nolint:gosec // G705: CLI stderr, not HTML.
	engine.PushHooks = buildLinearPushHooks(ctx, lt, len(args) > 0)

	result, err := engine.Sync(ctx, tracker.SyncOptions{
		Push:             true,
		Pull:             false,
		DryRun:           dryRun,
		ExcludeEphemeral: true,
		IssueIDs:         args,
	})
	if err != nil {
		return HandleError("sync failed: %v", err)
	}
	outputSyncResult(result, dryRun)
	return nil
}

func executeLinearPull(ctx context.Context, args []string, dryRun, relations bool) error {
	if err := ensureStoreActive(); err != nil {
		return HandleError("database not available: %v", err)
	}
	if err := validateLinearConfig(nil); err != nil {
		return HandleError("%v", err)
	}

	teamIDs := getLinearTeamIDs(ctx, nil)
	lt := linear.NewTracker()
	lt.SetTeamIDs(teamIDs)
	if err := lt.Init(ctx, getStore()); err != nil {
		return HandleError("initializing Linear tracker: %v", err)
	}

	engine := tracker.NewEngine(lt, getStore(), getActor())
	engine.OnMessage = func(msg string) { fmt.Println("  " + msg) }
	engine.OnWarning = func(msg string) { fmt.Fprintf(os.Stderr, "Warning: %s\n", msg) } //nolint:gosec // G705: CLI stderr, not HTML.
	engine.PullHooks = buildLinearPullHooks(ctx, linearPullHookOptions{
		DryRun: dryRun,
		Actor:  getActor(),
	})

	result, err := engine.Sync(ctx, tracker.SyncOptions{
		Pull:              true,
		Push:              false,
		DryRun:            dryRun,
		IssueIDs:          args,
		DependencySources: linearPullDependencySources(relations),
	})
	if err != nil {
		return HandleError("sync failed: %v", err)
	}
	outputSyncResult(result, dryRun)
	return nil
}
