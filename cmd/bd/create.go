package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/remotecache"
	"github.com/jonbaldie/beads/internal/routing"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

// validateCreateArgs runs as cobra's Args validation, which executes before
// PersistentPreRunE opens the store or runs migrations. It reuses
// resolveTitle — the same shared validator gatherCreateInput calls for the
// proxied-server create path — so a whitespace-only title (GH#4771) is
// rejected identically for both backends, and before any invocation that is
// guaranteed to fail wastes a store open/migration.
func validateCreateArgs(cmd *cobra.Command, args []string) error {
	markdownFile, _ := cmd.Flags().GetString("file")
	graphFile, _ := cmd.Flags().GetString("graph")
	titleFlag, _ := cmd.Flags().GetString("title")

	_, err := resolveTitle(args, titleFlag, markdownFile, graphFile)
	return err
}

var createCmd = &cobra.Command{
	Use:           "create [title]",
	GroupID:       "issues",
	Aliases:       []string{"new"},
	Short:         "Create a new issue (or batch from markdown/graph JSON)",
	Args:          cobra.MatchAll(cobra.MaximumNArgs(1), validateCreateArgs),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runCreate,
}

type createIssueIdentity struct {
	ID          string
	Title       string
	SpecID      string
	Assignee    string
	ExternalRef string
	CreatedBy   string
	Owner       string
}

type createIssueBody struct {
	Description        string
	Design             string
	AcceptanceCriteria string
	Notes              string
	Labels             []string
	Metadata           json.RawMessage
}

type createIssueClass struct {
	Priority      int
	IssueType     types.IssueType
	Ephemeral     bool
	NoHistory     bool
	StorageClass  types.StorageClass
	MolType       types.MolType
	WispType      types.WispType
	InitialStatus string
}

type createIssueSchedule struct {
	EstimatedMinutes *int
	DueAt            *time.Time
	DeferUntil       *time.Time
}

type createIssueEvent struct {
	EventKind string
	Actor     string
	Target    string
	Payload   string
}

type createIssueParams struct {
	ident    createIssueIdentity
	body     createIssueBody
	class    createIssueClass
	schedule createIssueSchedule
	event    createIssueEvent
}

// resolveStorageClass resolves the effective storage class at create time
// (Protocol v0.1 C1.3): the explicit --storage-class flag wins; otherwise the
// per-type config default storage-class.<type> applies; otherwise unset.
// Versioned normalizes to unset — the class marker is omitted when versioned
// (C2.4), and both spell identical semantics (C1.2). Values are validated
// wherever they came from: a bad flag is a usage error, a bad config value is
// a config bug and fails just as loudly.
func resolveStorageClass(explicit string, issueType types.IssueType) (types.StorageClass, error) {
	raw := explicit
	if raw == "" {
		raw = config.GetString("storage-class." + string(issueType))
		if raw == "" {
			return "", nil
		}
	}
	class, err := types.ParseStorageClass(raw)
	if err != nil {
		if explicit == "" {
			return "", fmt.Errorf("config storage-class.%s: %w", issueType, err)
		}
		return "", err
	}
	if class == types.StorageClassVersioned {
		return "", nil
	}
	return class, nil
}

func buildCreateIssue(params createIssueParams) *types.Issue {
	var externalRefPtr *string
	if params.ident.ExternalRef != "" {
		externalRefPtr = &params.ident.ExternalRef
	}

	status := types.StatusOpen
	if params.class.InitialStatus != "" {
		status = types.Status(params.class.InitialStatus)
	} else if params.schedule.DeferUntil != nil && params.schedule.DeferUntil.After(time.Now()) {
		status = types.StatusDeferred
	}

	return &types.Issue{
		IssueID: types.IssueID{
			ID: params.ident.ID,
		},
		IssueContent: types.IssueContent{
			Title:              params.ident.Title,
			Description:        params.body.Description,
			Design:             params.body.Design,
			AcceptanceCriteria: params.body.AcceptanceCriteria,
			Notes:              params.body.Notes,
			SpecID:             params.ident.SpecID,
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:           status,
			Priority:         params.class.Priority,
			IssueType:        params.class.IssueType,
			Assignee:         params.ident.Assignee,
			EstimatedMinutes: params.schedule.EstimatedMinutes,
			Owner:            params.ident.Owner,
		},
		IssueTimes: types.IssueTimes{
			CreatedBy: params.ident.CreatedBy,
		},
		IssueLease: types.IssueLease{
			DueAt:      params.schedule.DueAt,
			DeferUntil: params.schedule.DeferUntil,
		},
		IssueMeta: types.IssueMeta{
			ExternalRef: externalRefPtr,
			Metadata:    params.body.Metadata,
		},
		IssueGraph: types.IssueGraph{
			Labels: append([]string(nil), params.body.Labels...),
		},
		IssueWisp: types.IssueWisp{
			Ephemeral:    params.class.Ephemeral,
			NoHistory:    params.class.NoHistory,
			StorageClass: params.class.StorageClass,
			WispType:     params.class.WispType,
		},
		IssueCoord: types.IssueCoord{
			MolType: params.class.MolType,
		},
		IssueEvent: types.IssueEvent{
			EventKind: params.event.EventKind,
			Actor:     params.event.Actor,
			Target:    params.event.Target,
			Payload:   params.event.Payload,
		},
	}
}

func mergeCreateLabels(labels, inheritedLabels []string) []string {
	merged := make([]string, 0, len(labels)+len(inheritedLabels))
	seen := make(map[string]struct{}, len(labels)+len(inheritedLabels))
	for _, label := range labels {
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		merged = append(merged, label)
	}
	for _, label := range inheritedLabels {
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		merged = append(merged, label)
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// createIDPrefixOverride is the prefix an explicit --id must match, when the
// WORKSPACE knows better than the database does.
//
// config.yaml's `issue-prefix` wins over the database's, except under --global
// where the shared database is authoritative (GH#4957, selectCreateIDPrefix).
// Only a front door can read config.yaml — a shared server's database knows
// only its own prefix — so both routes resolve it here and hand it to the role
// as CreateRequest.IDPrefix. Empty means "the substrate's prefix is right",
// which is the ordinary case.
func createIDPrefixOverride() string {
	if isGlobalFlag() {
		return ""
	}
	return overlayYAMLPrefix("")
}

func selectCreateIDPrefix(global bool, yamlPrefix, storePrefix string) string {
	if global {
		return storePrefix
	}
	if yamlPrefix != "" {
		return yamlPrefix
	}
	return storePrefix
}

func renderCreateDryRunPreview(issue *types.Issue, labels, deps []string) {
	idDisplay := issue.ID
	if idDisplay == "" {
		idDisplay = "(will be generated)"
	}
	fmt.Printf("%s [DRY RUN] Would create issue:\n", ui.RenderWarn("⚠"))
	fmt.Printf("  ID: %s\n", idDisplay)
	fmt.Printf("  Title: %s\n", issue.Title)
	fmt.Printf("  Type: %s\n", issue.IssueType)
	fmt.Printf("  Priority: P%d\n", issue.Priority)
	fmt.Printf("  Status: %s\n", issue.Status)
	if issue.Assignee != "" {
		fmt.Printf("  Assignee: %s\n", issue.Assignee)
	}
	if issue.Description != "" {
		fmt.Printf("  Description: %s\n", issue.Description)
	}
	if len(labels) > 0 {
		fmt.Printf("  Labels: %s\n", strings.Join(labels, ", "))
	}
	if len(deps) > 0 {
		fmt.Printf("  Dependencies: %s\n", strings.Join(deps, ", "))
	}
	if issue.EventKind != "" {
		fmt.Printf("  Event category: %s\n", issue.EventKind)
	}
}

func shouldCommitCreatePostWrites(_ *types.Issue, _ bool) (bool, error) {
	return embeddedWritesCommitNow()
}

func createDepsAcceptedTypeList() string {
	names := []string{"blocked-by", "depends-on"}
	for _, depType := range types.WellKnownDependencyTypes() {
		names = append(names, string(depType))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func init() {
	createCmd.Flags().StringP("file", "f", "", "Create multiple issues from markdown file")
	createCmd.Flags().String("graph", "", "Create a graph of issues with dependencies from JSON plan file")
	createCmd.Flags().String("title", "", "Issue title (alternative to positional argument)")
	createCmd.Flags().Bool("silent", false, "Output only the issue ID (for scripting)")
	createCmd.Flags().Bool("dry-run", false, "Preview what would be created without actually creating")
	registerPriorityFlag(createCmd, "2")
	createCmd.Flags().StringP("type", "t", "task", "Issue type (bug|feature|task|epic|chore|decision|spike|story|milestone); custom types require types.custom config; aliases: enhancement/feat→feature, dec/adr→decision")
	createCmd.Flags().StringP("status", "s", "", "Initial status")
	registerCommonIssueFlags(createCmd)
	createCmd.Flags().String("spec-id", "", "Link to specification document")
	createCmd.Flags().StringSliceP("labels", "l", []string{}, "Labels (comma-separated)")
	createCmd.Flags().String("skills", "", "Required skills for this issue")
	createCmd.Flags().String("context", "", "Additional context for the issue")
	createCmd.Flags().StringSlice("label", []string{}, "Alias for --labels")
	_ = createCmd.Flags().MarkHidden("label") // Only fails if flag missing (caught in tests)
	createCmd.Flags().String("id", "", "Explicit issue ID (e.g., 'bd-42' for partitioning)")
	createCmd.Flags().String("parent", "", "Parent issue ID for hierarchical child (e.g., 'bd-a3f8e9')")
	createCmd.Flags().Bool("no-inherit-labels", false, "Don't inherit labels from parent issue")
	createCmd.Flags().StringSlice("deps", []string{}, "Dependencies as 'type:id' or bare 'id'. Bare 'id', 'depends-on:id', and 'blocked-by:id' all make THIS issue depend on id; 'blocks:id' reverses direction (id depends on this issue). E.g. 'blocked-by:bd-20,discovered-from:bd-15'")
	createCmd.Flags().String("waits-for", "", "Spawner issue ID to wait for (creates waits-for dependency for fanout gate)")
	createCmd.Flags().String("waits-for-gate", "all-children", "Gate type: all-children (wait for all) or any-children (wait for first)")
	createCmd.Flags().Bool("force", false, "Force creation even if prefix doesn't match database prefix")
	createCmd.Flags().String("repo", "", "Target repository for issue (overrides auto-routing)")
	createCmd.Flags().IntP("estimate", "e", 0, "Time estimate in minutes (e.g., 60 for 1 hour)")
	createCmd.Flags().Bool("ephemeral", false, "Create as ephemeral (short-lived, subject to TTL compaction)")
	createCmd.Flags().Bool("no-history", false, "Skip Dolt commit history without making GC-eligible (for permanent agent beads)")
	createCmd.Flags().String("storage-class", "", "Storage class: versioned, unversioned, or ephemeral (default: storage-class.<type> config, else versioned)")
	createCmd.Flags().String("mol-type", "", "Molecule type: swarm (multi-agent), patrol (recurring ops), work (default)")
	createCmd.Flags().String("wisp-type", "", "Wisp type for TTL-based compaction: heartbeat, ping, patrol, gc_report, recovery, error, escalation")
	createCmd.Flags().Bool("validate", false, "Validate description contains required sections for issue type")
	createCmd.Flags().Bool("allow-empty-description", false, "Allow empty description input from stdin or file")
	// Event-specific flags (only valid when --type=event)
	createCmd.Flags().String("event-category", "", "Event category (e.g., patrol.muted, agent.started) (requires --type=event)")
	createCmd.Flags().String("event-actor", "", "Entity URI who caused this event (requires --type=event)")
	createCmd.Flags().String("event-target", "", "Entity URI or bead ID affected (requires --type=event)")
	createCmd.Flags().String("event-payload", "", "Event-specific JSON data (requires --type=event)")
	// Time-based scheduling flags (GH#820)
	// Examples:
	//   --due=+6h           Due in 6 hours
	//   --due=tomorrow      Due tomorrow
	//   --due="next monday" Due next Monday
	//   --due=2025-01-15    Due on specific date
	//   --defer=+1h         Hidden from bd ready for 1 hour
	//   --defer=tomorrow    Hidden until tomorrow
	createCmd.Flags().String("due", "", "Due date/time. Formats: +6h, +1d, +2w, tomorrow, next monday, 2025-01-15")
	createCmd.Flags().String("defer", "", "Defer until date (issue hidden from bd ready until then). Same formats as --due")
	createCmd.Flags().String("metadata", "", "Set custom metadata (JSON string or @file.json to read from file)")
	// Note: --json flag is defined as a persistent flag in main.go, not here
	rootCmd.AddCommand(createCmd)
}

// formatTimeForRPC converts a *time.Time to RFC3339 string for RPC calls.
// Returns empty string if t is nil, to distinguish "not set" from "set to zero".
func formatTimeForRPC(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

// openDryRunTargetStore opens the store a `create --dry-run --repo <other>`
// resolves --parent against. It is read-only on BOTH paths and must stay that
// way: newDoltStoreFromConfig runs schema initialization on whatever it opens
// and can rename a legacy hyphenated database and rewrite the target's
// metadata.json on the way (GH#3231), so using it here would have a dry-run
// mutate a repository the user only named as a lookup target — the same
// migrate-at-open trap this preview policy exists to close, one repo over.
// newPreviewStoreFromConfig is the non-mutating factory for a foreign
// project (bd-6dnrw.32), relaxed for previews exactly as the root pre-run
// relaxes the command's own store.
func openDryRunTargetStore(ctx context.Context, repoPath string) (storage.DoltStorage, error) {
	if remotecache.IsRemoteURL(repoPath) {
		cache, err := remotecache.DefaultCache()
		if err != nil {
			return nil, fmt.Errorf("failed to initialize remote cache: %w", err)
		}
		// The dry-run parent lookup only reads from this cached remote store.
		// Do not add writes here; dry-runs must not mutate cached remotes.
		store, err := cache.OpenStore(ctx, repoPath, newPreviewStoreFromConfig)
		if err != nil {
			return nil, fmt.Errorf("dry-run parent lookup requires an existing cached remote store for %s: %w", repoPath, err)
		}
		return store, nil
	}

	targetPath := routing.ExpandPath(repoPath)
	beadsDir := filepath.Join(targetPath, ".beads")
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	if _, err := os.Stat(metadataPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("target repo %s is not initialized; refusing to initialize it during dry-run", targetPath)
		}
		return nil, fmt.Errorf("failed to inspect target repo %s: %w", targetPath, err)
	}

	store, err := newPreviewStoreFromConfig(ctx, beadsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open target store for dry-run: %w", err)
	}
	return store, nil
}

// ensureBeadsDirForPath ensures a beads directory exists at the target path.
// If the .beads directory doesn't exist, it creates it and initializes with
// the same prefix as the source store (T010, T012: prefix inheritance).
func ensureBeadsDirForPath(ctx context.Context, targetPath string, sourceStore storage.DoltStorage) error {
	beadsDir := filepath.Join(targetPath, ".beads")
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	// Check if beads directory already exists with a Dolt database.
	// metadata.json is the canonical marker for an initialized beads dir.
	if _, err := os.Stat(metadataPath); err == nil {
		return nil
	}
	if err := os.MkdirAll(beadsDir, 0750); err != nil {
		return fmt.Errorf("cannot create .beads directory: %w", err)
	}
	return inheritPrefixIntoBeadsDir(ctx, beadsDir, sourceStore)
}

func inheritPrefixIntoBeadsDir(ctx context.Context, beadsDir string, sourceStore storage.DoltStorage) error {
	if sourceStore == nil {
		return nil
	}
	sourcePrefix, err := sourceStore.GetConfig(ctx, "issue_prefix")
	if err != nil || sourcePrefix == "" {
		return nil
	}
	// Sanitize prefix for SQL database name (same as bd init).
	dbName := strings.ReplaceAll(sourcePrefix, "-", "_")
	// Open target store temporarily to set prefix.
	// Use newDoltStore with explicit config since the target .beads
	// directory was just created and has no metadata.json yet.
	tempStore, err := newDoltStore(ctx, &dolt.Config{
		BeadsDir: beadsDir,
		Database: dbName,
		RemoteOptions: dolt.RemoteOptions{
			CreateIfMissing: true,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to initialize target database: %w", err)
	}
	if err := tempStore.SetConfig(ctx, "issue_prefix", sourcePrefix); err != nil {
		_ = tempStore.Close() // Best effort cleanup on error path
		return fmt.Errorf("failed to set prefix in target store: %w", err)
	}
	if err := tempStore.Close(); err != nil {
		return fmt.Errorf("failed to close target store: %w", err)
	}
	return writeInheritedBeadsMetadata(beadsDir, dbName)
}

func writeInheritedBeadsMetadata(beadsDir, dbName string) error {
	// Write metadata.json so newDoltStoreFromConfig can find the
	// correct database name on subsequent opens (GH#2988).
	cfg := configfile.DefaultConfig()
	cfg.Backend = configfile.BackendDolt
	cfg.DoltDatabase = dbName
	cfg.DoltMode = configfile.DoltModeEmbedded
	cfg.ProjectID = configfile.GenerateProjectID()
	if err := cfg.Save(beadsDir); err != nil {
		return fmt.Errorf("failed to write metadata.json: %w", err)
	}
	return nil
}
