package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/notion"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/spf13/cobra"
)

func runNotionStatus(cmd *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("notion status is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("notion-status")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	cfg := getNotionConfig()
	auth, err := resolveNotionAuth(cmd.Context())
	if err != nil {
		return HandleError("%v", err)
	}
	result := newNotionStatusResult(cfg, auth)
	if !result.Configured {
		if err := validateNotionConfig(cfg, auth); err != nil {
			result.Error = err.Error()
		}
		return renderOrWriteNotionStatus(cmd, auth, cfg, result)
	}

	populateNotionStatus(cmd.Context(), newNotionStatusClient(auth.Token), cfg, &result, auth)
	return renderOrWriteNotionStatus(cmd, auth, cfg, result)
}

func runNotionInit(cmd *cobra.Command, _ []string) error {
	opts := notionInitOptionsFromCommand(cmd)
	if err := checkNotionMutationRoute("notion init", "notion init is not supported in proxied-server mode"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("notion-init")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	auth, err := resolveNotionMutationAuth(cmd)
	if err != nil {
		return err
	}

	result, err := runNotionInitAfterValidation(cmd.Context(), newNotionSetupClient(auth.Token), opts.parent, opts.title, getStore(), notionConfigDeleteTarget())
	if err != nil {
		return HandleError("%v", err)
	}
	if isJSONOutput() {
		return writeNotionJSON(cmd, result)
	}
	writeNotionInitOutput(cmd, result)
	return nil
}

func runNotionConnect(cmd *cobra.Command, _ []string) error {
	opts := notionConnectOptionsFromCommand(cmd)
	if err := checkNotionMutationRoute("notion connect", "notion connect is not supported in proxied-server mode"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("notion-connect")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	auth, err := resolveNotionMutationAuth(cmd)
	if err != nil {
		return err
	}

	result, err := runNotionConnectAfterValidation(cmd.Context(), newNotionSetupClient(auth.Token), opts.url, getStore(), notionConfigDeleteTarget())
	if err != nil {
		return HandleError("%v", err)
	}
	if isJSONOutput() {
		return writeNotionJSON(cmd, result)
	}
	writeNotionConnectOutput(cmd, result)
	return nil
}

func checkNotionMutationRoute(operation, unsupportedMessage string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("%s", unsupportedMessage)
	}
	return CheckReadonly(operation)
}

func resolveNotionMutationAuth(cmd *cobra.Command) (*notion.ResolvedAuth, error) {
	if err := ensureStoreActive(); err != nil {
		return nil, HandleError("database not available: %v", err)
	}
	auth, err := resolveNotionAuth(cmd.Context())
	if err != nil {
		return nil, HandleError("%v", err)
	}
	if err := validateNotionToken(auth); err != nil {
		return nil, HandleError("%v", err)
	}
	return auth, nil
}

func writeNotionInitOutput(cmd *cobra.Command, result notionSetupResult) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "✓ Created Notion database %s\n", firstNonEmpty(result.DatabaseID, "(unknown)"))
	_, _ = fmt.Fprintf(out, "Saved data source: %s\n", result.DataSourceID)
	writeNotionViewURL(out, result.ViewURL)
}

func writeNotionConnectOutput(cmd *cobra.Command, result notionSetupResult) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "✓ Connected Notion data source %s\n", result.DataSourceID)
	writeNotionViewURL(out, result.ViewURL)
}

func writeNotionViewURL(out interface{ Write([]byte) (int, error) }, viewURL string) {
	if viewURL != "" {
		_, _ = fmt.Fprintf(out, "Launch URL: %s\n", viewURL)
	}
}

func renderNotionStatus(cmd *cobra.Command, auth *notion.ResolvedAuth, cfg notionConfig, result *notion.StatusResponse) {
	out := cmd.OutOrStdout()
	renderNotionStatusHeader(out, auth, cfg, result)
	renderNotionStatusSummary(out, result)
}

func renderNotionStatusHeader(out interface{ Write([]byte) (int, error) }, auth *notion.ResolvedAuth, cfg notionConfig, result *notion.StatusResponse) {
	_, _ = fmt.Fprintln(out, "Notion Configuration")
	_, _ = fmt.Fprintln(out, "====================")
	_, _ = fmt.Fprintf(out, "Auth:        %s\n", maskNotionAuth(auth))
	if auth != nil && auth.Source != "" {
		_, _ = fmt.Fprintf(out, "Auth source: %s\n", auth.Source)
	}
	_, _ = fmt.Fprintf(out, "Data source: %s\n", cfg.DataSourceID)
	if cfg.ViewURL != "" {
		_, _ = fmt.Fprintf(out, "View URL:    %s\n", cfg.ViewURL)
	}
	if result.Database != nil {
		_, _ = fmt.Fprintf(out, "Database:    %s\n", result.Database.Title)
	}
}

func notionStatusLine(result *notion.StatusResponse) string {
	if result.Ready {
		return "✓ Ready"
	}
	if result.Configured {
		return "◐ Not ready"
	}
	return "○ Not configured"
}

func renderNotionStatusSummary(out interface{ Write([]byte) (int, error) }, result *notion.StatusResponse) {
	_, _ = fmt.Fprintf(out, "\nStatus: %s\n", notionStatusLine(result))
	if result.Error != "" {
		_, _ = fmt.Fprintf(out, "Error: %s\n", result.Error)
	}
	renderNotionStatusSchema(out, result)
}

func renderNotionStatusSchema(out interface{ Write([]byte) (int, error) }, result *notion.StatusResponse) {
	if result.Schema != nil {
		if len(result.Schema.Missing) == 0 {
			_, _ = fmt.Fprintln(out, "Schema: ✓ Required properties present")
		} else {
			_, _ = fmt.Fprintf(out, "Schema: missing %s\n", strings.Join(result.Schema.Missing, ", "))
		}
	}
}

func validateNotionSyncFlags(opts notionSyncOptions) error {
	if opts.preferLocal && opts.preferNotion {
		return fmt.Errorf("cannot use both --prefer-local and --prefer-notion")
	}
	if opts.pull && opts.push {
		return fmt.Errorf("cannot use both --pull and --push")
	}
	return nil
}

func notionSyncDirections(opts notionSyncOptions) (pull, push bool) {
	pull = !opts.push
	push = !opts.pull
	return pull, push
}

func notionSyncConflictResolution(opts notionSyncOptions) tracker.ConflictResolution {
	if opts.preferLocal {
		return tracker.ConflictLocal
	}
	if opts.preferNotion {
		return tracker.ConflictExternal
	}
	return tracker.ConflictTimestamp
}

func buildNotionSyncOptions(cmd *cobra.Command, opts notionSyncOptions) (tracker.SyncOptions, error) {
	pull, push := notionSyncDirections(opts)
	syncOpts := tracker.SyncOptions{
		Pull:               pull,
		Push:               push,
		DryRun:             opts.dryRun,
		CreateOnly:         opts.createOnly,
		State:              opts.state,
		ExcludeEphemeral:   true,
		ConflictResolution: notionSyncConflictResolution(opts),
	}
	if err := applySelectiveSyncFlags(cmd, &syncOpts, push); err != nil {
		return tracker.SyncOptions{}, err
	}
	return syncOpts, nil
}

func configureNotionSyncEngine(cmd *cobra.Command, ctx context.Context, nt *notion.Tracker, unsupportedStats *notionUnsupportedPushStats) *tracker.Engine {
	engine := tracker.NewEngine(nt, getStore(), getActor())
	engine.PullHooks = buildNotionPullHooks(ctx)
	engine.PushHooks = buildNotionPushHooks(ctx, nt, unsupportedStats)
	if isJSONOutput() {
		engine.OnMessage = func(msg string) { _, _ = fmt.Fprintln(cmd.ErrOrStderr(), "  "+msg) } //nolint:gosec // G705: CLI stderr, not HTML.
	} else {
		engine.OnMessage = func(msg string) { _, _ = fmt.Fprintln(cmd.OutOrStdout(), "  "+msg) } //nolint:gosec // G705: CLI text output, not HTML.
	}
	engine.OnWarning = func(msg string) { _, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s\n", msg) } //nolint:gosec // G705: CLI stderr, not HTML.
	return engine
}

func prepareNotionSync(cmd *cobra.Command, opts notionSyncOptions) (*tracker.Engine, tracker.SyncOptions, *notionUnsupportedPushStats, error) {
	cfg := getNotionConfig()
	auth, err := resolveNotionAuth(cmd.Context())
	if err != nil {
		return nil, tracker.SyncOptions{}, nil, err
	}
	if err := validateNotionConfig(cfg, auth); err != nil {
		return nil, tracker.SyncOptions{}, nil, err
	}
	if !opts.dryRun {
		if err := CheckReadonly("notion sync"); err != nil {
			return nil, tracker.SyncOptions{}, nil, err
		}
	}
	if err := validateNotionSyncFlags(opts); err != nil {
		return nil, tracker.SyncOptions{}, nil, err
	}
	if err := ensureStoreActive(); err != nil {
		return nil, tracker.SyncOptions{}, nil, fmt.Errorf("database not available: %w", err)
	}

	ctx := cmd.Context()
	nt := notion.NewTracker()
	if err := nt.Init(ctx, getStore()); err != nil {
		return nil, tracker.SyncOptions{}, nil, fmt.Errorf("initializing Notion tracker: %w", err)
	}
	unsupportedStats := newNotionUnsupportedPushStats()
	engine := configureNotionSyncEngine(cmd, ctx, nt, unsupportedStats)
	syncOpts, err := buildNotionSyncOptions(cmd, opts)
	if err != nil {
		return nil, tracker.SyncOptions{}, nil, err
	}
	return engine, syncOpts, unsupportedStats, nil
}
