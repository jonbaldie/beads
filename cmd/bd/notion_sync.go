package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/notion"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/spf13/cobra"
)

func runNotionSync(cmd *cobra.Command, _ []string) error {
	opts := notionSyncOptionsFromCommand(cmd)
	if usesProxiedServer() {
		return HandleErrorRespectJSON("notion sync is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("notion-sync")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	engine, syncOpts, unsupportedStats, err := prepareNotionSync(cmd, opts)
	if err != nil {
		return HandleError("%v", err)
	}

	result, err := engine.Sync(cmd.Context(), syncOpts)
	if err != nil {
		return HandleError("%v", err)
	}
	if warning := notionUnsupportedPushWarning(unsupportedStats); warning != "" {
		result.Warnings = append(result.Warnings, warning)
	}
	if isJSONOutput() {
		return writeNotionJSON(cmd, result)
	}
	renderNotionSyncResult(cmd, result, opts)
	return nil
}

func renderNotionSyncResult(cmd *cobra.Command, result *tracker.SyncResult, opts notionSyncOptions) {
	out := cmd.OutOrStdout()
	renderNotionSyncMode(out, opts)
	renderNotionPullStats(out, result)
	renderNotionPushStats(out, result)
	renderNotionConflictStats(out, result)
	for _, line := range summarizeNotionSyncWarnings(result.Warnings) {
		_, _ = fmt.Fprintln(out, line)
	}
	if opts.dryRun {
		_, _ = fmt.Fprintln(out, "Run without --dry-run to apply changes")
	}
}

func renderNotionSyncMode(out interface{ Write([]byte) (int, error) }, opts notionSyncOptions) {
	if opts.dryRun {
		_, _ = fmt.Fprintln(out, "Dry run mode")
	}
}

func renderNotionPullStats(out interface{ Write([]byte) (int, error) }, result *tracker.SyncResult) {
	if result.PullStats.Queried > 0 || result.PullStats.Candidates > 0 {
		_, _ = fmt.Fprintf(out, "Queried %d pages from Notion (%d pull candidates)\n",
			result.PullStats.Queried, result.PullStats.Candidates)
	}
	if result.PullStats.Created > 0 || result.PullStats.Updated > 0 {
		_, _ = fmt.Fprintf(out, "✓ Pulled %d issues (%d created, %d updated)\n",
			result.Stats.Pulled, result.PullStats.Created, result.PullStats.Updated)
	}
}

func renderNotionPushStats(out interface{ Write([]byte) (int, error) }, result *tracker.SyncResult) {
	if result.PushStats.Created > 0 || result.PushStats.Updated > 0 {
		_, _ = fmt.Fprintf(out, "✓ Pushed %d issues (%d created, %d updated)\n",
			result.Stats.Pushed, result.PushStats.Created, result.PushStats.Updated)
	}
}

func renderNotionConflictStats(out interface{ Write([]byte) (int, error) }, result *tracker.SyncResult) {
	if result.Stats.Conflicts > 0 {
		_, _ = fmt.Fprintf(out, "◐ Resolved %d conflicts\n", result.Stats.Conflicts)
	}
}

func summarizeNotionSyncWarnings(warnings []string) []string {
	staleTargetCount := 0
	for _, warning := range warnings {
		warning = strings.TrimSpace(warning)
		switch {
		case warning == "":
			continue
		case strings.HasPrefix(warning, "Skipped unsupported Notion issue types:"):
			continue
		case strings.Contains(warning, "outside the current target"):
			staleTargetCount++
		}
	}
	if staleTargetCount == 0 {
		return nil
	}
	return []string{
		fmt.Sprintf("Skipped %d linked issues that still point at a different Notion target. Clear external_ref to recreate them in this data source.", staleTargetCount),
	}
}

func buildNotionPullHooks(ctx context.Context) *tracker.PullHooks {
	prefix := "bd"
	if p := config.GetString("issue-prefix"); p != "" {
		prefix = p
	} else if getStore() != nil {
		if p, err := getStore().GetConfig(ctx, "issue_prefix"); err == nil && p != "" {
			prefix = p
		}
	}
	return &tracker.PullHooks{
		GenerateID: func(_ context.Context, issue *types.Issue) error {
			if issue.ID == "" {
				issue.ID = generateIssueID(prefix)
			}
			return nil
		},
	}
}

func buildNotionPushHooks(ctx context.Context, tr tracker.IssueTracker, stats *notionUnsupportedPushStats) *tracker.PushHooks {
	return &tracker.PushHooks{
		ShouldPush: func(issue *types.Issue) bool {
			if issue == nil || tr == nil {
				return false
			}
			if notion.SupportsIssueType(issue.IssueType, nil) {
				pushPrefix, _ := getStore().GetConfig(ctx, "notion.push_prefix")
				pushLabel, _ := getStore().GetConfig(ctx, "notion.push_label")
				return shouldPushNotionIssue(issue, tr, pushPrefix, pushLabel)
			}
			recordNotionUnsupportedPush(stats, issue.IssueType)
			return false
		},
	}
}

func shouldPushNotionIssue(issue *types.Issue, tr tracker.IssueTracker, pushPrefix, pushLabel string) bool {
	if issue == nil || tr == nil {
		return false
	}

	if issue.ExternalRef != nil && strings.TrimSpace(*issue.ExternalRef) != "" {
		return tr.IsExternalRef(*issue.ExternalRef)
	}

	if !matchesConfiguredNotionPushLabel(issue, pushLabel) {
		return false
	}

	return matchesNotionPushPrefix(issue.ID, pushPrefix)
}

func matchesConfiguredNotionPushLabel(issue *types.Issue, pushLabel string) bool {
	if strings.TrimSpace(pushLabel) == "" {
		return true
	}
	return matchesNotionPushLabel(issue, pushLabel)
}

func matchesNotionPushPrefix(issueID, pushPrefix string) bool {
	if strings.TrimSpace(pushPrefix) == "" {
		return true
	}
	for _, prefix := range strings.Split(pushPrefix, ",") {
		prefix = strings.TrimSpace(prefix)
		prefix = strings.TrimSuffix(prefix, "-")
		if prefix != "" && strings.HasPrefix(issueID, prefix+"-") {
			return true
		}
	}

	return false
}

func matchesNotionPushLabel(issue *types.Issue, pushLabel string) bool {
	if issue == nil || strings.TrimSpace(pushLabel) == "" {
		return false
	}

	configured := make(map[string]struct{})
	for _, raw := range strings.Split(pushLabel, ",") {
		label := strings.ToLower(strings.TrimSpace(raw))
		if label != "" {
			configured[label] = struct{}{}
		}
	}
	if len(configured) == 0 {
		return false
	}

	for _, raw := range issue.Labels {
		label := strings.ToLower(strings.TrimSpace(raw))
		if _, ok := configured[label]; ok {
			return true
		}
	}

	return false
}

func runNotionInitAfterValidation(ctx context.Context, client *notion.Client, parent, title string, setter notionConfigSetter, deleter notionConfigDeleter) (notionSetupResult, error) {
	db, err := client.CreateDatabase(ctx, parent, title)
	if err != nil {
		return notionSetupResult{}, err
	}
	if len(db.DataSources) == 0 || strings.TrimSpace(db.DataSources[0].ID) == "" {
		return notionSetupResult{}, fmt.Errorf("Notion create database response did not include a child data source")
	}
	result := notionSetupResult{
		Action:       "init",
		DatabaseID:   strings.TrimSpace(db.ID),
		DataSourceID: strings.TrimSpace(db.DataSources[0].ID),
		ViewURL:      strings.TrimSpace(db.URL),
		Message:      "Notion target initialized",
	}
	if err := saveNotionTargetConfigWithWriter(ctx, setter, deleter, result.DataSourceID, result.ViewURL); err != nil {
		return notionSetupResult{}, err
	}
	return result, nil
}

func runNotionConnectAfterValidation(ctx context.Context, client *notion.Client, url string, setter notionConfigSetter, deleter notionConfigDeleter) (notionSetupResult, error) {
	resolved, err := notion.ResolveDataSourceReference(ctx, client, url)
	if err != nil {
		return notionSetupResult{}, err
	}
	schema := notion.ValidateDataSourceSchema(resolved.DataSource)
	if len(schema.Missing) > 0 {
		return notionSetupResult{}, fmt.Errorf("target is missing required Notion properties: %s", strings.Join(schema.Missing, ", "))
	}
	result := notionSetupResult{
		Action:       "connect",
		DataSourceID: resolved.DataSourceID,
		ViewURL:      strings.TrimSpace(url),
		Message:      "Notion target connected",
	}
	if resolved.Database != nil {
		result.DatabaseID = strings.TrimSpace(resolved.Database.ID)
	}
	if err := saveNotionTargetConfigWithWriter(ctx, setter, deleter, result.DataSourceID, result.ViewURL); err != nil {
		return notionSetupResult{}, err
	}
	return result, nil
}

func notionConfigDeleteTarget() notionConfigDeleter {
	if getStore() == nil {
		return nil
	}
	deleter, _ := storage.UnwrapStore(getStore()).(notionConfigDeleter)
	return deleter
}

func saveNotionTargetConfigWithWriter(ctx context.Context, setter notionConfigSetter, deleter notionConfigDeleter, dataSourceID, viewURL string) error {
	if setter == nil {
		return fmt.Errorf("database not available")
	}
	if err := setter.SetConfig(ctx, "notion.data_source_id", strings.TrimSpace(dataSourceID)); err != nil {
		return fmt.Errorf("save notion.data_source_id: %w", err)
	}
	viewURL = strings.TrimSpace(viewURL)
	if viewURL == "" {
		if deleter == nil {
			return fmt.Errorf("store does not support config deletion")
		}
		if err := deleter.DeleteConfig(ctx, "notion.view_url"); err != nil {
			return fmt.Errorf("clear notion.view_url: %w", err)
		}
		return nil
	}
	if err := setter.SetConfig(ctx, "notion.view_url", viewURL); err != nil {
		return fmt.Errorf("save notion.view_url: %w", err)
	}
	return nil
}

func writeNotionJSON(cmd *cobra.Command, value interface{}) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func statusUserFromNotionUser(user *notion.User) *notion.StatusUser {
	if user == nil {
		return nil
	}
	return &notion.StatusUser{
		ID:    user.ID,
		Name:  user.Name,
		Type:  user.Type,
		Email: userEmail(user),
	}
}

func userEmail(user *notion.User) string {
	if user == nil || user.Person == nil {
		return ""
	}
	return user.Person.Email
}
