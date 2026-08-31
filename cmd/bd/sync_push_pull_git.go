package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/jonbaldie/beads/internal/github"
	"github.com/jonbaldie/beads/internal/gitlab"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/notion"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/spf13/cobra"
)

// --- GitHub implementations ---

func runGitHubPush(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("github-push")
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
		if err := CheckReadonly("github push"); err != nil {
			return err
		}
	}

	config := getGitHubConfig()
	if err := validateGitHubConfig(config); err != nil {
		return err
	}
	if err := ensureStoreActive(); err != nil {
		return fmt.Errorf("database not available: %w", err)
	}

	ctx := cmd.Context()
	gt := &github.Tracker{}
	if err := gt.Init(ctx, getStore()); err != nil {
		return fmt.Errorf("initializing GitHub tracker: %w", err)
	}

	engine := tracker.NewEngine(gt, getStore(), getActor())
	engine.OnMessage = func(msg string) { fmt.Println("  " + msg) }
	engine.OnWarning = func(msg string) { fmt.Fprintf(os.Stderr, "Warning: %s\n", msg) } //nolint:gosec // G705: CLI stderr, not HTML.
	engine.PushHooks = buildGitHubPushHooks(gt)

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

func runGitHubPull(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("github-pull")
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
		if err := CheckReadonly("github pull"); err != nil {
			return err
		}
	}

	config := getGitHubConfig()
	if err := validateGitHubConfig(config); err != nil {
		return err
	}
	if err := ensureStoreActive(); err != nil {
		return fmt.Errorf("database not available: %w", err)
	}

	ctx := cmd.Context()
	gt := &github.Tracker{}
	if err := gt.Init(ctx, getStore()); err != nil {
		return fmt.Errorf("initializing GitHub tracker: %w", err)
	}

	engine := tracker.NewEngine(gt, getStore(), getActor())
	engine.PullHooks = buildGitHubPullHooks(ctx)
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

// --- GitLab implementations ---

func runGitLabPush(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("gitlab-push")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	gt, engine, dryRun, err := prepareGitLabPush(cmd, args)
	if err != nil {
		return err
	}
	return executeGitLabPush(cmd, gt, engine, args, dryRun)
}

func prepareGitLabPush(cmd *cobra.Command, args []string) (*gitlab.Tracker, *tracker.Engine, bool, error) {
	dryRun, err := validateGitLabPushRequest(cmd, args)
	if err != nil {
		return nil, nil, false, err
	}
	gt, engine, err := newGitLabPushEngine(cmd)
	if err != nil {
		return nil, nil, false, err
	}
	out := cmd.OutOrStdout()
	if dryRun && !isJSONOutput() {
		fmt.Fprintln(out, "Dry run mode - no changes will be made")
		fmt.Fprintln(out)
	}
	return gt, engine, dryRun, nil
}

func validateGitLabPushRequest(cmd *cobra.Command, args []string) (bool, error) {
	if len(args) == 0 {
		return false, fmt.Errorf("at least one bead ID is required")
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if !dryRun {
		if err := CheckReadonly("gitlab push"); err != nil {
			return false, err
		}
	}
	if err := validateGitLabConfig(getGitLabConfig()); err != nil {
		return false, err
	}
	if err := ensureStoreActive(); err != nil {
		return false, fmt.Errorf("database not available: %w", err)
	}
	return dryRun, nil
}

func newGitLabPushEngine(cmd *cobra.Command) (*gitlab.Tracker, *tracker.Engine, error) {
	gt := &gitlab.Tracker{}
	if err := gt.Init(cmd.Context(), getStore()); err != nil {
		return nil, nil, fmt.Errorf("initializing GitLab tracker: %w", err)
	}
	out := cmd.OutOrStdout()
	engine := tracker.NewEngine(gt, getStore(), getActor())
	engine.OnMessage = func(msg string) {
		if !isJSONOutput() {
			fmt.Fprintln(out, "  "+msg) //nolint:gosec // G705: CLI text output, not HTML.
		}
	}
	engine.OnWarning = func(msg string) { fmt.Fprintf(os.Stderr, "Warning: %s\n", msg) } //nolint:gosec // G705: CLI stderr, not HTML.
	engine.PushHooks = buildGitLabPushHooks()
	return gt, engine, nil
}

func executeGitLabPush(cmd *cobra.Command, gt *gitlab.Tracker, engine *tracker.Engine, args []string, dryRun bool) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	opts := tracker.SyncOptions{Push: true, Pull: false, DryRun: dryRun, IssueIDs: args}
	result, err := engine.Sync(ctx, opts)
	if err != nil {
		return err
	}
	// Dependency-link push parity with `bd gitlab sync`: converge beads
	// dependencies among the requested issues into GitLab issue links (and
	// repair epic-child milestones). Without this, `bd gitlab push <ids>`
	// would sync content but silently omit dependency links. Output is
	// rendered here (rather than via outputSyncResult) so the link pass runs
	// before the summary and the --json payload carries the link counts,
	// matching bd gitlab sync.
	var linkWarnings []string
	warnLink := func(msg string) {
		linkWarnings = append(linkWarnings, msg)
		fmt.Fprintf(os.Stderr, "Warning: %s\n", msg) //nolint:gosec // G705: CLI stderr, not HTML.
	}
	linksPushed, linksLicenseSkipped, milestonesUpdated := pushGitLabDependencyLinks(ctx, gt, getStore(), opts, dryRun, out, warnLink)
	if isJSONOutput() {
		return outputJSON(gitlabSyncResult{
			DryRun:              dryRun,
			Pushed:              result.Stats.Pushed,
			Created:             result.Stats.Created,
			Updated:             result.Stats.Updated,
			Skipped:             result.Stats.Skipped,
			Conflicts:           result.Stats.Conflicts,
			Errors:              result.Stats.Errors,
			LinksPushed:         linksPushed,
			LinksLicenseSkipped: linksLicenseSkipped,
			MilestonesUpdated:   milestonesUpdated,
			Warnings:            append(result.Warnings, linkWarnings...),
		})
	}
	return printGitLabPushResult(out, result, linksPushed, dryRun)
}

func printGitLabPushResult(out io.Writer, result *tracker.SyncResult, linksPushed int, dryRun bool) error {
	if dryRun {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Run without --dry-run to apply changes")
		return nil
	}
	if result.Stats.Pushed > 0 {
		fmt.Fprintf(out, "✓ Pushed %d issues\n", result.Stats.Pushed)
	}
	if linksPushed > 0 {
		fmt.Fprintf(out, "✓ Synced %d dependency links\n", linksPushed)
	}
	if result.Stats.Conflicts > 0 {
		fmt.Fprintf(out, "→ Resolved %d conflicts\n", result.Stats.Conflicts)
	}
	return nil
}

func runGitLabPull(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("gitlab-pull")
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
		if err := CheckReadonly("gitlab pull"); err != nil {
			return err
		}
	}

	config := getGitLabConfig()
	if err := validateGitLabConfig(config); err != nil {
		return err
	}
	if err := ensureStoreActive(); err != nil {
		return fmt.Errorf("database not available: %w", err)
	}

	ctx := cmd.Context()
	gt := &gitlab.Tracker{}
	if err := gt.Init(ctx, getStore()); err != nil {
		return fmt.Errorf("initializing GitLab tracker: %w", err)
	}

	engine := tracker.NewEngine(gt, getStore(), getActor())
	engine.PullHooks = buildGitLabPullHooks(ctx)
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

// --- Notion implementations ---

func runNotionPush(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("notion-push")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	ctx := cmd.Context()
	engine, dryRun, err := prepareNotionPushPull(cmd, args, "notion push", "at least one bead ID is required", false)
	if err != nil {
		return err
	}

	result, syncErr := engine.Sync(ctx, tracker.SyncOptions{
		Push:             true,
		Pull:             false,
		DryRun:           dryRun,
		ExcludeEphemeral: true,
		IssueIDs:         args,
	})
	if syncErr != nil {
		return syncErr
	}
	outputSyncResult(result, dryRun)
	return nil
}

func runNotionPull(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("notion-pull")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	ctx := cmd.Context()
	engine, dryRun, err := prepareNotionPushPull(cmd, args, "notion pull", "at least one bead ID or external reference is required", true)
	if err != nil {
		return err
	}

	result, syncErr := engine.Sync(ctx, tracker.SyncOptions{
		Pull:     true,
		Push:     false,
		DryRun:   dryRun,
		IssueIDs: args,
	})
	if syncErr != nil {
		return syncErr
	}
	outputSyncResult(result, dryRun)
	return nil
}

func prepareNotionPushPull(cmd *cobra.Command, args []string, operation, missingArgs string, pull bool) (*tracker.Engine, bool, error) {
	dryRun, err := validateNotionSyncRequest(cmd, args, operation, missingArgs)
	if err != nil {
		return nil, false, err
	}
	engine, err := newNotionSyncEngine(cmd.Context(), pull)
	if err != nil {
		return nil, false, err
	}
	return engine, dryRun, nil
}

func validateNotionSyncRequest(cmd *cobra.Command, args []string, operation, missingArgs string) (bool, error) {
	if len(args) == 0 {
		return false, fmt.Errorf("%s", missingArgs)
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if !dryRun {
		if err := CheckReadonly(operation); err != nil {
			return false, err
		}
	}
	cfg := getNotionConfig()
	auth, err := resolveNotionAuth(cmd.Context())
	if err != nil {
		return false, err
	}
	if err := validateNotionConfig(cfg, auth); err != nil {
		return false, err
	}
	if err := ensureStoreActive(); err != nil {
		return false, fmt.Errorf("database not available: %w", err)
	}
	return dryRun, nil
}

func newNotionSyncEngine(ctx context.Context, pull bool) (*tracker.Engine, error) {
	nt := notion.NewTracker()
	if err := nt.Init(ctx, getStore()); err != nil {
		return nil, fmt.Errorf("initializing Notion tracker: %w", err)
	}
	engine := tracker.NewEngine(nt, getStore(), getActor())
	if pull {
		engine.PullHooks = buildNotionPullHooks(ctx)
	} else {
		unsupportedStats := newNotionUnsupportedPushStats()
		engine.PushHooks = buildNotionPushHooks(ctx, nt, unsupportedStats)
	}
	engine.OnMessage = func(msg string) { fmt.Println("  " + msg) }
	engine.OnWarning = func(msg string) { fmt.Fprintf(os.Stderr, "Warning: %s\n", msg) } //nolint:gosec // G705: CLI stderr, not HTML.
	return engine, nil
}
