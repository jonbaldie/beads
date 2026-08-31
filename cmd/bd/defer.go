package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/timeparsing"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

func newDeferCmd() *cobra.Command {
	deferCmd := &cobra.Command{
		Use:   "defer [id...]",
		Short: "Defer one or more issues for later",
		Long: `Defer issues to put them on ice for later.

Deferred issues are deliberately set aside - not blocked by anything specific,
just postponed for future consideration. Unlike blocked issues, there's no
dependency keeping them from being worked. Unlike closed issues, they will
be revisited.

Deferred issues don't show in 'bd ready' but remain visible in 'bd list'.

A defer WITH a date is a snooze: once --until passes, the next ready-front
read returns the issue to open automatically (same shape as 'bd undefer').
A defer WITHOUT a date is the indefinite icebox: it stays deferred until
someone runs 'bd undefer'.

Examples:
  bd defer bd-abc                  # Icebox indefinitely (until bd undefer)
  bd defer bd-abc --until=tomorrow # Snooze: auto-wakes once the date passes
  bd defer bd-abc --reason="waiting on API access"
  bd defer bd-abc bd-def           # Defer multiple issues`,
		Args:          cobra.MinimumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runDefer,
	}
	// Time-based scheduling flag (GH#820)
	deferCmd.Flags().String("until", "", "Defer until specific time (e.g., +1h, tomorrow, next monday)")
	deferCmd.Flags().String("reason", "", "Record why this issue is being deferred (appended to notes)")
	deferCmd.ValidArgsFunction = issueIDCompletion
	return deferCmd
}

type deferOptions struct {
	until  *time.Time
	reason string
}

func runDefer(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("defer")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	opts, err := parseDeferOptions(cmd)
	if err != nil {
		return err
	}
	if err := CheckReadonly("defer"); err != nil {
		return err
	}
	if usesProxiedServer() {
		return runDeferProxiedServer(getRootContext(), args, opts.until, opts.reason)
	}
	return runDirectDefer(getRootContext(), args, opts)
}

func runDirectDefer(ctx context.Context, args []string, opts deferOptions) error {
	if _, err := utils.ResolvePartialIDs(ctx, getStore(), args); err != nil {
		return HandleError("%v", err)
	}
	if getStore() == nil {
		return HandleErrorWithHint("database not initialized", diagHint())
	}

	deferredIssues := make([]*types.Issue, 0, len(args))
	for _, id := range args {
		issue, ok := applyDefer(ctx, id, opts)
		deferredIssues = appendDeferredIssue(deferredIssues, issue, ok)
	}
	if isJSONOutput() && len(deferredIssues) > 0 {
		if err := outputJSON(deferredIssues); err != nil {
			return err
		}
	}
	if len(args) > 0 {
		commandDidWrite.Store(true)
	}
	return nil
}

func appendDeferredIssue(issues []*types.Issue, issue *types.Issue, ok bool) []*types.Issue {
	if ok && issue != nil {
		return append(issues, issue)
	}
	return issues
}

func parseDeferOptions(cmd *cobra.Command) (deferOptions, error) {
	var opts deferOptions
	untilStr, _ := cmd.Flags().GetString("until")
	if untilStr != "" {
		t, err := timeparsing.ParseRelativeTime(untilStr, time.Now())
		if err != nil {
			return deferOptions{}, HandleError("invalid --until format %q. Examples: +1h, tomorrow, next monday, 2025-01-15", untilStr)
		}
		if t.Before(time.Now()) && !isJSONOutput() {
			fmt.Fprintf(os.Stderr, "%s Defer date %q is in the past. Issue will appear in bd ready immediately.\n",
				ui.RenderWarn("!"), t.Local().Format("2006-01-02 15:04"))
			fmt.Fprintln(os.Stderr, "  Did you mean a future date? Use --until=+1h or --until=tomorrow")
		}
		opts.until = &t
	}
	opts.reason, _ = cmd.Flags().GetString("reason")
	opts.reason = strings.TrimSpace(opts.reason)
	if cmd.Flags().Changed("reason") && opts.reason == "" {
		return deferOptions{}, HandleError("reason cannot be empty")
	}
	return opts, nil
}

func applyDefer(ctx context.Context, id string, opts deferOptions) (*types.Issue, bool) {
	fullID, err := utils.ResolvePartialID(ctx, getStore(), id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving %s: %v\n", id, err)
		return nil, false
	}
	updates := map[string]interface{}{"status": string(types.StatusDeferred)}
	if opts.until != nil {
		updates["defer_until"] = *opts.until
	}
	if opts.reason != "" {
		issue, err := getStore().GetIssue(ctx, fullID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading %s: %v\n", fullID, err)
			return nil, false
		}
		if issue == nil {
			fmt.Fprintf(os.Stderr, "Issue %s not found\n", fullID)
			return nil, false
		}
		notes := issue.Notes
		if notes != "" {
			notes += "\n"
		}
		updates["notes"] = notes + opts.reason
	}
	if err := getStore().UpdateIssue(ctx, fullID, updates, getActor()); err != nil {
		fmt.Fprintf(os.Stderr, "Error deferring %s: %v\n", fullID, err)
		return nil, false
	}
	if isJSONOutput() {
		updated, _ := getStore().GetIssue(ctx, fullID)
		return updated, true
	}
	fmt.Printf("%s Deferred %s\n", ui.RenderAccent("*"), fullID)
	return nil, true
}

func init() {
	rootCmd.AddCommand(newDeferCmd())
}
