package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/jonbaldie/beads/internal/notion"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/spf13/cobra"
)

type notionConfig struct {
	DataSourceID string
	ViewURL      string
}

type notionConfigSetter interface {
	SetConfig(ctx context.Context, key, value string) error
}

type notionConfigDeleter interface {
	DeleteConfig(ctx context.Context, key string) error
}

type notionUnsupportedPushStats struct {
	counts map[types.IssueType]int
}

func newNotionUnsupportedPushStats() *notionUnsupportedPushStats {
	return &notionUnsupportedPushStats{counts: make(map[types.IssueType]int)}
}

func recordNotionUnsupportedPush(s *notionUnsupportedPushStats, issueType types.IssueType) {
	if s == nil || strings.TrimSpace(string(issueType)) == "" {
		return
	}
	s.counts[issueType]++
}

func notionUnsupportedPushWarning(s *notionUnsupportedPushStats) string {
	if s == nil || len(s.counts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.counts))
	for issueType, count := range s.counts {
		parts = append(parts, fmt.Sprintf("%s=%d", issueType, count))
	}
	sort.Strings(parts)
	return fmt.Sprintf(
		"Skipped unsupported Notion issue types: %s (supported: bug, feature, task, epic, chore)",
		strings.Join(parts, ", "),
	)
}

type notionSetupResult struct {
	Action       string `json:"action"`
	DatabaseID   string `json:"database_id,omitempty"`
	DataSourceID string `json:"data_source_id,omitempty"`
	ViewURL      string `json:"view_url,omitempty"`
	Message      string `json:"message,omitempty"`
}

type notionInitOptions struct {
	parent string
	title  string
}

func notionInitOptionsFromCommand(cmd *cobra.Command) notionInitOptions {
	if cmd == nil {
		return notionInitOptions{}
	}
	parent, _ := cmd.Flags().GetString("parent")
	title, _ := cmd.Flags().GetString("title")
	return notionInitOptions{parent: parent, title: title}
}

type notionConnectOptions struct {
	url string
}

func notionConnectOptionsFromCommand(cmd *cobra.Command) notionConnectOptions {
	if cmd == nil {
		return notionConnectOptions{}
	}
	url, _ := cmd.Flags().GetString("url")
	return notionConnectOptions{url: url}
}

type notionSyncOptions struct {
	pull         bool
	push         bool
	dryRun       bool
	preferLocal  bool
	preferNotion bool
	createOnly   bool
	state        string
}

func notionSyncOptionsFromCommand(cmd *cobra.Command) notionSyncOptions {
	if cmd == nil {
		return notionSyncOptions{}
	}
	flags := cmd.Flags()
	pull, _ := flags.GetBool("pull")
	push, _ := flags.GetBool("push")
	dryRun, _ := flags.GetBool("dry-run")
	preferLocal, _ := flags.GetBool("prefer-local")
	preferNotion, _ := flags.GetBool("prefer-notion")
	createOnly, _ := flags.GetBool("create-only")
	state, _ := flags.GetString("state")
	return notionSyncOptions{
		pull:         pull,
		push:         push,
		dryRun:       dryRun,
		preferLocal:  preferLocal,
		preferNotion: preferNotion,
		createOnly:   createOnly,
		state:        state,
	}
}

var newNotionStatusClient = notion.NewClient
var newNotionSetupClient = notion.NewClient

var notionCmd = &cobra.Command{
	Use:   "notion",
	Short: "Notion integration commands",
	Long:  "Commands for syncing issues between beads and Notion.",
}

var notionStatusCmd = &cobra.Command{
	Use:           "status",
	Short:         "Show Notion sync status",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runNotionStatus,
}

var notionInitCmd = &cobra.Command{
	Use:           "init",
	Short:         "Create a dedicated Beads database in Notion",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runNotionInit,
}

var notionConnectCmd = &cobra.Command{
	Use:           "connect",
	Short:         "Connect bd to an existing Notion database or data source",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runNotionConnect,
}

var notionSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync issues with Notion",
	Long: "Synchronize issues between beads and Notion.\n\n" +
		"By default this performs bidirectional sync. Use --pull or --push to limit direction.",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runNotionSync,
}

func init() {
	notionInitCmd.Flags().String("parent", "", "Parent page ID")
	notionInitCmd.Flags().String("title", notion.DefaultDatabaseTitle, "Database title")
	_ = notionInitCmd.MarkFlagRequired("parent")

	notionConnectCmd.Flags().String("url", "", "Existing Notion database or data source URL")
	_ = notionConnectCmd.MarkFlagRequired("url")

	notionSyncCmd.Flags().Bool("pull", false, "Only pull issues from Notion")
	notionSyncCmd.Flags().Bool("push", false, "Only push issues to Notion")
	notionSyncCmd.Flags().Bool("dry-run", false, "Preview changes without making mutations")
	notionSyncCmd.Flags().Bool("prefer-local", false, "On conflict, keep the local beads version")
	notionSyncCmd.Flags().Bool("prefer-notion", false, "On conflict, use the Notion version")
	notionSyncCmd.Flags().Bool("create-only", false, "Only create missing remote pages, do not update existing ones")
	notionSyncCmd.Flags().String("state", "all", "Issue state to sync: open, closed, or all")
	registerSelectiveSyncFlags(notionSyncCmd)

	notionCmd.AddCommand(
		notionInitCmd,
		notionConnectCmd,
		notionStatusCmd,
		notionSyncCmd,
	)
	rootCmd.AddCommand(notionCmd)
}

func getNotionConfig() notionConfig {
	ctx := context.Background()
	if getStore() != nil {
		return getNotionConfigWithReader(ctx, getStore())
	}
	if getDBPath() != "" {
		tempStore, err := openReadOnlyStoreForDBPath(ctx, getDBPath())
		if err == nil {
			defer func() { _ = tempStore.Close() }()
			return getNotionConfigWithReader(ctx, tempStore)
		}
	}
	return getNotionConfigWithReader(ctx, nil)
}

func getNotionConfigWithReader(ctx context.Context, reader notion.ConfigReader) notionConfig {
	return notionConfig{
		DataSourceID: getNotionConfigValue(ctx, reader, "notion.data_source_id", "NOTION_DATA_SOURCE_ID"),
		ViewURL:      getNotionConfigValue(ctx, reader, "notion.view_url", "NOTION_VIEW_URL"),
	}
}

func getNotionConfigValue(ctx context.Context, reader notion.ConfigReader, key, envVar string) string {
	if reader != nil {
		value, _ := reader.GetConfig(ctx, key)
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	if envVar != "" {
		return strings.TrimSpace(os.Getenv(envVar))
	}
	return ""
}

func resolveNotionAuth(ctx context.Context) (*notion.ResolvedAuth, error) {
	if getStore() != nil {
		return notion.ResolveAuth(ctx, getStore())
	}
	if getDBPath() != "" {
		tempStore, err := openReadOnlyStoreForDBPath(ctx, getDBPath())
		if err == nil {
			defer func() { _ = tempStore.Close() }()
			return notion.ResolveAuth(ctx, tempStore)
		}
	}
	if token := strings.TrimSpace(os.Getenv("NOTION_TOKEN")); token != "" {
		return &notion.ResolvedAuth{Token: token, Source: notion.AuthSourceEnv}, nil
	}
	return nil, nil
}

func validateNotionConfig(cfg notionConfig, auth *notion.ResolvedAuth) error {
	if auth == nil || strings.TrimSpace(auth.Token) == "" {
		return fmt.Errorf("Notion authentication is not configured. Set notion.token with 'bd config set notion.token <token>', or export NOTION_TOKEN")
	}
	if cfg.DataSourceID == "" {
		return fmt.Errorf("notion.data_source_id is not configured. Run 'bd notion init --parent <page-id>' or 'bd notion connect --url <notion-url>', or set it directly via bd config set notion.data_source_id <id> or NOTION_DATA_SOURCE_ID")
	}
	return nil
}

func validateNotionToken(auth *notion.ResolvedAuth) error {
	if auth == nil || strings.TrimSpace(auth.Token) == "" {
		return fmt.Errorf("Notion authentication is not configured. Set notion.token with 'bd config set notion.token <token>', or export NOTION_TOKEN")
	}
	return nil
}

func maskNotionAuth(auth *notion.ResolvedAuth) string {
	if auth == nil || strings.TrimSpace(auth.Token) == "" {
		return "(not set)"
	}
	token := auth.Token
	if len(token) <= 4 {
		return "****"
	}
	return token[:4] + "****"
}

func newNotionStatusResult(cfg notionConfig, auth *notion.ResolvedAuth) notion.StatusResponse {
	result := notion.StatusResponse{
		Configured:   auth != nil && strings.TrimSpace(auth.Token) != "" && cfg.DataSourceID != "",
		DataSourceID: cfg.DataSourceID,
		ViewURL:      cfg.ViewURL,
	}
	if auth != nil && strings.TrimSpace(auth.Token) != "" {
		result.Auth = &notion.StatusAuth{OK: true, Source: string(auth.Source)}
	} else {
		result.Auth = &notion.StatusAuth{OK: false}
	}
	return result
}

func populateNotionStatus(ctx context.Context, client *notion.Client, cfg notionConfig, result *notion.StatusResponse, auth *notion.ResolvedAuth) {
	user, err := client.GetCurrentUser(ctx)
	if err != nil {
		result.Error = err.Error()
		result.Auth = &notion.StatusAuth{OK: false, Source: string(auth.Source)}
	} else {
		result.Auth = &notion.StatusAuth{
			OK:     true,
			Source: string(auth.Source),
			User:   statusUserFromNotionUser(user),
		}
	}

	dataSource, err := client.RetrieveDataSource(ctx, cfg.DataSourceID)
	if err != nil {
		if result.Error == "" {
			result.Error = err.Error()
		}
		return
	}
	result.Database = &notion.StatusDatabase{
		ID:    dataSource.ID,
		Title: notion.DataSourceTitle(dataSource.Title),
		URL:   dataSource.URL,
	}
	result.Schema = notion.ValidateDataSourceSchema(dataSource)
	result.Ready = result.Auth != nil && result.Auth.OK && len(result.Schema.Missing) == 0
}

func renderOrWriteNotionStatus(cmd *cobra.Command, auth *notion.ResolvedAuth, cfg notionConfig, result notion.StatusResponse) error {
	if isJSONOutput() {
		return writeNotionJSON(cmd, result)
	}
	renderNotionStatus(cmd, auth, cfg, &result)
	return nil
}
