// Package main provides the bd CLI commands.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/jonbaldie/beads/internal/gitlab"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/spf13/cobra"
)

// gitlabSyncResult holds the JSON output for the gitlab sync command.
type gitlabSyncResult struct {
	DryRun              bool     `json:"dry_run"`
	Pulled              int      `json:"pulled"`
	Pushed              int      `json:"pushed"`
	Created             int      `json:"created"`
	Updated             int      `json:"updated"`
	Skipped             int      `json:"skipped"`
	Conflicts           int      `json:"conflicts"`
	Errors              int      `json:"errors"`
	LinksPushed         int      `json:"links_pushed"`
	LinksLicenseSkipped int      `json:"links_license_skipped,omitempty"`
	MilestonesUpdated   int      `json:"milestones_updated,omitempty"`
	Warnings            []string `json:"warnings,omitempty"`
}

type gitLabSyncFlags struct {
	dryRun          bool
	pullOnly        bool
	pushOnly        bool
	preferLocal     bool
	preferGitLab    bool
	preferNewer     bool
	filterLabel     string
	filterProject   string
	filterMilestone string
	filterAssignee  string
	typeFilter      string
	excludeTypes    string
	noEphemeral     bool
}

func gitLabSyncFlagsFromCommand(cmd *cobra.Command) gitLabSyncFlags {
	flags := cmd.Flags()
	dryRun, _ := flags.GetBool("dry-run")
	pullOnly, _ := flags.GetBool("pull-only")
	pushOnly, _ := flags.GetBool("push-only")
	preferLocal, _ := flags.GetBool("prefer-local")
	preferGitLab, _ := flags.GetBool("prefer-gitlab")
	preferNewer, _ := flags.GetBool("prefer-newer")
	filterLabel, _ := flags.GetString("label")
	filterProject, _ := flags.GetString("project")
	filterMilestone, _ := flags.GetString("milestone")
	filterAssignee, _ := flags.GetString("assignee")
	typeFilter, _ := flags.GetString("type")
	excludeTypes, _ := flags.GetString("exclude-type")
	noEphemeral, _ := flags.GetBool("no-ephemeral")
	return gitLabSyncFlags{
		dryRun:          dryRun,
		pullOnly:        pullOnly,
		pushOnly:        pushOnly,
		preferLocal:     preferLocal,
		preferGitLab:    preferGitLab,
		preferNewer:     preferNewer,
		filterLabel:     filterLabel,
		filterProject:   filterProject,
		filterMilestone: filterMilestone,
		filterAssignee:  filterAssignee,
		typeFilter:      typeFilter,
		excludeTypes:    excludeTypes,
		noEphemeral:     noEphemeral,
	}
}

type gitLabSyncSetup struct {
	ctx    context.Context
	out    io.Writer
	gt     *gitlab.Tracker
	engine *tracker.Engine
	opts   tracker.SyncOptions
	flags  gitLabSyncFlags
}

// runGitLabSync implements the gitlab sync command.
// Uses the tracker.Engine for all sync operations.
func runGitLabSync(cmd *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("gitlab sync is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("gitlab-sync")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	return executeGitLabSync(cmd)
}

func executeGitLabSync(cmd *cobra.Command) error {
	flags := gitLabSyncFlagsFromCommand(cmd)
	conflictStrategy, err := validateGitLabSyncRequest(flags)
	if err != nil {
		return err
	}

	setup, err := initializeGitLabSync(cmd, flags)
	if err != nil {
		return err
	}
	opts, err := buildGitLabSyncOptions(cmd, flags, conflictStrategy)
	if err != nil {
		return err
	}
	setup.opts = opts

	if flags.dryRun && !isJSONOutput() {
		printGitLabSyncDryRunNotice(setup.out)
	}

	result, err := setup.engine.Sync(setup.ctx, setup.opts)
	if err != nil {
		return HandleError("%v", err)
	}
	return finishGitLabSync(setup, result)
}

func finishGitLabSync(setup *gitLabSyncSetup, result *tracker.SyncResult) error {
	var linkWarnings []string
	warnLink := func(msg string) {
		linkWarnings = append(linkWarnings, msg)
		_, _ = fmt.Fprintf(os.Stderr, "Warning: %s\n", msg) //nolint:gosec // G705: CLI stderr, not HTML.
	}

	// Dependency-link push pass: sync beads dependencies to GitLab issue links.
	var linksPushed int
	var linksLicenseSkipped int
	var milestonesUpdated int
	if setup.opts.Push {
		linksPushed, linksLicenseSkipped, milestonesUpdated = pushGitLabDependencyLinks(setup.ctx, setup.gt, getStore(), setup.opts, setup.flags.dryRun, setup.out, warnLink)
	}

	if isJSONOutput() {
		return outputGitLabSyncJSON(result, setup.flags.dryRun, linksPushed, linksLicenseSkipped, milestonesUpdated, linkWarnings)
	}

	renderGitLabSyncText(setup.out, result, setup.flags.dryRun, linksPushed)

	if !setup.flags.dryRun {
		commandDidWrite.Store(true)
	}

	return nil
}

func validateGitLabSyncRequest(flags gitLabSyncFlags) (ConflictStrategy, error) {
	if err := validateGitLabConfig(getGitLabConfig()); err != nil {
		return "", HandleError("%v", err)
	}
	if !flags.dryRun {
		if err := CheckReadonly("gitlab sync"); err != nil {
			return "", err
		}
	}
	if flags.pullOnly && flags.pushOnly {
		return "", HandleError("cannot use both --pull-only and --push-only")
	}
	strategy, err := getConflictStrategy(flags.preferLocal, flags.preferGitLab, flags.preferNewer)
	if err != nil {
		return "", HandleError("%v (--prefer-local, --prefer-gitlab, --prefer-newer)", err)
	}
	return strategy, nil
}

func initializeGitLabSync(cmd *cobra.Command, flags gitLabSyncFlags) (*gitLabSyncSetup, error) {
	if err := ensureStoreActive(); err != nil {
		return nil, HandleError("database not available: %v", err)
	}
	ctx := context.Background()
	gt := &gitlab.Tracker{}
	if err := gt.Init(ctx, getStore()); err != nil {
		return nil, HandleError("initializing GitLab tracker: %v", err)
	}
	if cliFilter := buildCLIFilter(flags); cliFilter != nil {
		gt.SetFilter(cliFilter)
	}
	out := cmd.OutOrStdout()
	engine := tracker.NewEngine(gt, getStore(), getActor())
	configureGitLabSyncEngine(engine, out, ctx)
	return &gitLabSyncSetup{
		ctx:    ctx,
		out:    out,
		gt:     gt,
		engine: engine,
		flags:  flags,
	}, nil
}

func configureGitLabSyncEngine(engine *tracker.Engine, out io.Writer, ctx context.Context) {
	if !isJSONOutput() {
		engine.OnMessage = func(msg string) { _, _ = fmt.Fprintln(out, "  "+msg) } //nolint:gosec // G705: CLI text output, not HTML.
	}
	engine.OnWarning = func(msg string) { _, _ = fmt.Fprintf(os.Stderr, "Warning: %s\n", msg) } //nolint:gosec // G705: CLI stderr, not HTML.
	engine.PullHooks = buildGitLabPullHooks(ctx)
	engine.PushHooks = buildGitLabPushHooks()
}

func buildGitLabSyncOptions(cmd *cobra.Command, flags gitLabSyncFlags, conflictStrategy ConflictStrategy) (tracker.SyncOptions, error) {
	excludeTypes := parseTypeList(flags.excludeTypes)
	if flags.typeFilter == "" && flags.excludeTypes == "" {
		excludeTypes = []types.IssueType{
			types.TypeMolecule,
			types.TypeMessage,
			types.TypeEvent,
		}
	}
	push := !flags.pullOnly
	opts := tracker.SyncOptions{
		Pull:             !flags.pushOnly,
		Push:             push,
		DryRun:           flags.dryRun,
		ExcludeEphemeral: flags.noEphemeral,
		TypeFilter:       parseTypeList(flags.typeFilter),
		ExcludeTypes:     excludeTypes,
	}
	if err := applySelectiveSyncFlags(cmd, &opts, push); err != nil {
		return tracker.SyncOptions{}, HandleError("%v", err)
	}
	setGitLabSyncConflictResolution(&opts, conflictStrategy)
	return opts, nil
}

func setGitLabSyncConflictResolution(opts *tracker.SyncOptions, strategy ConflictStrategy) {
	switch strategy {
	case ConflictStrategyPreferLocal:
		opts.ConflictResolution = tracker.ConflictLocal
	case ConflictStrategyPreferGitLab:
		opts.ConflictResolution = tracker.ConflictExternal
	default:
		opts.ConflictResolution = tracker.ConflictTimestamp
	}
}

func printGitLabSyncDryRunNotice(out io.Writer) {
	_, _ = fmt.Fprintln(out, "Dry run mode - no changes will be made")
	_, _ = fmt.Fprintln(out)
}

func outputGitLabSyncJSON(result *tracker.SyncResult, dryRun bool, linksPushed, linksLicenseSkipped, milestonesUpdated int, linkWarnings []string) error {
	return outputJSON(gitlabSyncResult{
		DryRun:              dryRun,
		Pulled:              result.Stats.Pulled,
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

func renderGitLabSyncText(out io.Writer, result *tracker.SyncResult, dryRun bool, linksPushed int) {
	if !dryRun {
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
	if dryRun {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "Run without --dry-run to apply changes")
	}
}

// gitLabLicenseSkipMessage returns a single curated, actionable line explaining
// that N blocks/is_blocked_by links were skipped because the GitLab instance's
// license lacks the issue-blocking feature. Used instead of raw per-link API
// errors so the degradation is transparent and not mistaken for a real failure.
func gitLabLicenseSkipMessage(n int) string {
	noun := "link"
	if n != 1 {
		noun = "links"
	}
	return fmt.Sprintf(
		"Skipped %d dependency 'blocks' %s: GitLab 'blocks'/'is_blocked_by' requires Premium/Ultimate. "+
			"'relates_to' links and milestones were applied normally.", n, noun)
}
