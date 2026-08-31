package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/linear"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/spf13/cobra"
)

func runLinearTeams(_ *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("linear teams is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("linear-teams")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	ctx := getRootContext()

	client, err := buildLinearClient(ctx, "")
	if err != nil {
		return HandleError("%v", err)
	}

	teams, err := client.FetchTeams(ctx)
	if err != nil {
		return HandleError("fetching teams: %v", err)
	}

	if len(teams) == 0 {
		fmt.Println("No teams found (check your API key permissions)")
		return nil
	}

	if isJSONOutput() {
		return outputJSON(teams)
	}

	fmt.Println("Available Linear Teams")
	fmt.Println("======================")
	fmt.Println()
	fmt.Printf("%-40s  %-6s  %s\n", "ID (use this for linear.team_id)", "Key", "Name")
	fmt.Printf("%-40s  %-6s  %s\n", "----------------------------------------", "------", "----")
	for _, team := range teams {
		fmt.Printf("%-40s  %-6s  %s\n", team.ID, team.Key, team.Name)
	}
	fmt.Println()
	fmt.Println("To configure:")
	fmt.Println("  bd config set linear.team_id \"<ID>\"")
	fmt.Println("  bd config set linear.team_ids \"<ID1>,<ID2>\"  # multiple teams")
	return nil
}

// resolveBeadsDirForStaleness returns the active beads directory for
// staleness tracking. Falls back to BEADS_DIR env, then dbPath resolution.
func resolveBeadsDirForStaleness() string {
	if dir := os.Getenv("BEADS_DIR"); dir != "" {
		return dir
	}
	if getDBPath() != "" {
		return resolveCommandBeadsDir(getDBPath())
	}
	return ""
}

// uuidRegex matches valid UUID format (with or without hyphens).
var uuidRegex = regexp.MustCompile(`^[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}$`)

func isValidUUID(s string) bool {
	return uuidRegex.MatchString(s)
}

// validateLinearConfig checks that required Linear configuration is present.
// cliTeams is the list of team IDs from the --team flag (may be nil).
func validateLinearConfig(cliTeams []string) error {
	if err := ensureStoreActive(); err != nil {
		return fmt.Errorf("database not available: %w", err)
	}

	ctx := getRootContext()

	// Accept either OAuth credentials or API key.
	oauthClientID := getLinearConfig(ctx, "linear.oauth_client_id")
	oauthClientSecret := getLinearConfig(ctx, "linear.oauth_client_secret")
	hasOAuth := oauthClientID != "" && oauthClientSecret != ""

	if !hasOAuth {
		apiKey := getLinearConfig(ctx, "linear.api_key")
		if apiKey == "" {
			return fmt.Errorf("Linear authentication not configured\n" +
				"Options:\n" +
				"  OAuth (for CI):  export LINEAR_OAUTH_CLIENT_ID=... LINEAR_OAUTH_CLIENT_SECRET=...\n" +
				"  API key (devs):  export LINEAR_API_KEY=... or bd config set linear.api_key \"YOUR_API_KEY\"")
		}
	}

	teamIDs := getLinearTeamIDs(ctx, cliTeams)
	if len(teamIDs) == 0 {
		return fmt.Errorf("no Linear team ID configured\nRun: bd config set linear.team_id \"TEAM_ID\"\nOr:  bd config set linear.team_ids \"TEAM_ID1,TEAM_ID2\"\nOr: export LINEAR_TEAM_ID=TEAM_ID")
	}

	for _, id := range teamIDs {
		if !isValidUUID(id) {
			return fmt.Errorf("invalid Linear team ID (expected UUID format like '12345678-1234-1234-1234-123456789abc')\nInvalid value: %s", id)
		}
	}

	return nil
}

// maskAPIKey returns a masked version of an API key for display.
// Shows first 4 and last 4 characters, with dots in between.
func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "..." + key[len(key)-4:]
}

// getLinearConfig reads a Linear configuration value.
// Priority: environment variable > project config.
// Env vars take precedence so CI workers can override config without modifying config.yaml.
func getLinearConfig(ctx context.Context, key string) string {
	// Secret keys (e.g. linear.api_key) are stored in config.yaml, not the
	// Dolt database, to avoid leaking secrets when pushing to remotes.
	// Env vars are checked first so that LINEAR_OAUTH_CLIENT_ID/SECRET etc.
	// override whatever is in config.yaml.
	if config.IsYamlOnlyKey(key) {
		value, _ := getLinearYAMLConfig(key)
		return value
	}
	if value, ok := getLinearStoreConfig(ctx, key); ok {
		return value
	}
	value, _ := getLinearEnvironmentConfig(key)
	return value
}

func getLinearYAMLConfig(key string) (string, string) {
	envKey := linearConfigToEnvVar(key)
	if envKey != "" {
		if value := os.Getenv(envKey); value != "" {
			return value, fmt.Sprintf("environment variable (%s)", envKey)
		}
	}
	if value := config.GetString(key); value != "" {
		return value, "project config (config.yaml)"
	}
	return "", ""
}

func getLinearStoreConfig(ctx context.Context, key string) (string, bool) {
	if getStore() != nil {
		value, _ := getStore().GetConfig(ctx, key)
		return value, value != ""
	}
	if getDBPath() == "" {
		return "", false
	}
	tempStore, err := openReadOnlyStoreForDBPath(ctx, getDBPath())
	if err != nil {
		return "", false
	}
	defer func() { _ = tempStore.Close() }()
	value, _ := tempStore.GetConfig(ctx, key)
	return value, value != ""
}

func getLinearEnvironmentConfig(key string) (string, string) {
	envKey := linearConfigToEnvVar(key)
	if envKey == "" {
		return "", ""
	}
	if value := os.Getenv(envKey); value != "" {
		return value, fmt.Sprintf("environment variable (%s)", envKey)
	}
	return "", ""
}

// linearConfigToEnvVar maps Linear config keys to their environment variable names.
func linearConfigToEnvVar(key string) string {
	switch key {
	case "linear.api_key":
		return "LINEAR_API_KEY"
	case "linear.team_id":
		return "LINEAR_TEAM_ID"
	case "linear.team_ids":
		return "LINEAR_TEAM_IDS"
	case "linear.oauth_client_id":
		return "LINEAR_OAUTH_CLIENT_ID"
	case "linear.oauth_client_secret":
		return "LINEAR_OAUTH_CLIENT_SECRET"
	default:
		return ""
	}
}

// getLinearTeamIDs resolves the effective team IDs from all config sources.
// Precedence: cliTeams (--team flag) > linear.team_ids > LINEAR_TEAM_IDS > linear.team_id > LINEAR_TEAM_ID
func getLinearTeamIDs(ctx context.Context, cliTeams []string) []string {
	pluralVal := getLinearConfig(ctx, "linear.team_ids")
	singularVal := getLinearConfig(ctx, "linear.team_id")
	return tracker.ResolveProjectIDs(cliTeams, pluralVal, singularVal)
}

// getLinearClient creates a configured Linear client from beads config.
// Uses the first configured team ID for operations that require a single team.
//
// Auth precedence:
//  1. OAuth env vars (LINEAR_OAUTH_CLIENT_ID + LINEAR_OAUTH_CLIENT_SECRET)
//  2. LINEAR_API_KEY env var
//  3. linear.oauth_client_id + linear.oauth_client_secret in config
//  4. linear.api_key in config
func getLinearClient(ctx context.Context) (*linear.Client, error) {
	teamIDs := getLinearTeamIDs(ctx, nil)
	if len(teamIDs) == 0 {
		return nil, fmt.Errorf("Linear team ID not configured")
	}
	client, err := buildLinearClient(ctx, teamIDs[0])
	if err != nil {
		return nil, err
	}
	return applyLinearClientConfig(ctx, client), nil
}

func applyLinearClientConfig(ctx context.Context, client *linear.Client) *linear.Client {
	if getStore() == nil {
		return client
	}
	if endpoint, _ := getStore().GetConfig(ctx, "linear.api_endpoint"); endpoint != "" {
		client = client.WithEndpoint(endpoint)
	}
	if projectID, _ := getStore().GetConfig(ctx, "linear.project_id"); projectID != "" {
		client = client.WithProjectID(projectID)
	}
	return applyLinearRateLimitFloor(ctx, client)
}

func applyLinearRateLimitFloor(ctx context.Context, client *linear.Client) *linear.Client {
	// Readable/settable via `bd config get/set linear.rate_limit_floor`.
	// Also honored via the LINEAR_RATE_LIMIT_FLOOR environment variable.
	floorStr := getLinearConfig(ctx, "linear.rate_limit_floor")
	if floorStr == "" {
		floorStr = os.Getenv("LINEAR_RATE_LIMIT_FLOOR")
	}
	value, err := strconv.Atoi(strings.TrimSpace(floorStr))
	if floorStr != "" && err == nil && value >= 0 {
		return client.WithRateLimitFloor(value)
	}
	return client
}

// buildLinearClient resolves auth credentials and returns an appropriately
// configured Linear client. OAuth takes precedence over API key.
func buildLinearClient(ctx context.Context, teamID string) (*linear.Client, error) {
	oauthClientID := getLinearConfig(ctx, "linear.oauth_client_id")
	oauthClientSecret := getLinearConfig(ctx, "linear.oauth_client_secret")

	if oauthClientID != "" && oauthClientSecret != "" {
		debug.Logf("Linear: using OAuth client-credentials authentication")
		oauthCfg := linear.OAuthConfig{
			ClientID:     oauthClientID,
			ClientSecret: oauthClientSecret,
		}
		return linear.NewOAuthClient(oauthCfg, teamID), nil
	}

	apiKey := getLinearConfig(ctx, "linear.api_key")
	if apiKey == "" {
		return nil, fmt.Errorf("Linear authentication not configured\n" +
			"Options:\n" +
			"  OAuth (for CI):  export LINEAR_OAUTH_CLIENT_ID=... LINEAR_OAUTH_CLIENT_SECRET=...\n" +
			"  API key (devs):  export LINEAR_API_KEY=... or bd config set linear.api_key \"...\"")
	}

	return linear.NewClient(apiKey, teamID), nil
}

// storeConfigLoader adapts the store to the linear.ConfigLoader interface.
type storeConfigLoader struct {
	ctx context.Context
}

func (l *storeConfigLoader) GetAllConfig() (map[string]string, error) {
	return getStore().GetAllConfig(l.ctx)
}

// loadLinearMappingConfig loads mapping configuration from beads config.
func loadLinearMappingConfig(ctx context.Context) *linear.MappingConfig {
	if getStore() == nil {
		return linear.DefaultMappingConfig()
	}
	return linear.LoadMappingConfig(&storeConfigLoader{ctx: ctx})
}

// getLinearIDMode returns the configured ID mode for Linear imports.
// Supported values: "hash" (default) or "db".
func getLinearIDMode(ctx context.Context) string {
	mode := getLinearConfig(ctx, "linear.id_mode")
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return "hash"
	}
	return mode
}

// applyLinearExcludeIDConfig reads linear.exclude_id_prefix and
// linear.exclude_id_patterns from the given config reader and applies them
// to opts. Both keys are push-direction-only filters; see the help text on
// linearSyncCmd for the user-facing semantics.
//
// Empty values are no-ops. Patterns are comma-split, trimmed, with empty
// entries dropped. If reader is nil (no store configured), this is a no-op.
func applyLinearExcludeIDConfig(ctx context.Context, reader configReader, opts *tracker.SyncOptions) {
	if reader == nil || opts == nil {
		return
	}
	if v, _ := reader.GetConfig(ctx, "linear.exclude_id_prefix"); v != "" {
		opts.ExcludeIDPrefix = strings.TrimSpace(v)
	}
	if v, _ := reader.GetConfig(ctx, "linear.exclude_id_patterns"); v != "" {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				opts.ExcludeIDPatterns = append(opts.ExcludeIDPatterns, p)
			}
		}
	}
}

// getLinearHashLength returns the configured hash length for Linear imports.
// Values are clamped to the supported range 3-8.
func getLinearHashLength(ctx context.Context) int {
	raw := getLinearConfig(ctx, "linear.hash_length")
	if raw == "" {
		return 6
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 6
	}
	if value < 3 {
		return 3
	}
	if value > 8 {
		return 8
	}
	return value
}

// syncIsScoped returns true when the user explicitly constrained THIS
// invocation to a specific subset of beads (via --parent, --issues, or
// --type). The parent reconcile pass is skipped on scoped syncs because
// it walks the full local tree, which could mutate Linear-side state
// outside the scope the user asked for.
//
// Notably ExcludeTypes is NOT a scoping signal: it merges with persistent
// config (linear.exclude_types), and rigs that set it default-on (e.g.
// "molecule,event") would otherwise have the reconcile pass permanently
// disabled — bd-9w3 root cause. Reconcile only ever touches the parent
// field on the child issue, so excluding types from push doesn't really
// conflict with wiring up parent-child for the remaining types.
//
// TypeFilter IS kept as a scoping signal because --type is set only via
// the CLI flag for this invocation; the user's intent to push a specific
// subset is explicit.
func syncIsScoped(opts *tracker.SyncOptions) bool {
	if opts == nil {
		return false
	}
	if opts.ParentID != "" || len(opts.IssueIDs) > 0 {
		return true
	}
	if len(opts.TypeFilter) > 0 {
		return true
	}
	return false
}

// reconcileLinearParents runs as a post-sync pass to wire parent-child bead
// dependencies into Linear's parent issue field. Idempotent — no API call
// when the remote parent already matches.
//
// Two scenarios this fixes:
//
//  1. Fresh tree push: when a child is pushed before its parent in the same
//     sync, the create call has no parentId to send. After all issues have
//     external_refs, this pass closes the loop.
//  2. Orphan repair: existing Linear issues created in earlier bd versions
//     (or by interrupted syncs) without a parent get wired up retroactively.
//
// In dry-run mode the read-only fetches still run and the per-link mutation
// plan is printed as [dry-run] lines, but no IssueUpdate is issued. Lets
// users preview the orphan-repair scope before committing to a wet sync.
//
// Human-readable output is suppressed when jsonOutput is true so the
// caller's JSON serialization (in runLinearSync's output section) isn't
// polluted with stray fmt.Printf lines. Warnings and errors still go
// through the warnings slice, which IS surfaced in JSON output via
// SyncResult.Warnings.
//
// Warnings (per-link failures, missing refs) are appended to the engine's
// warning slice so the user sees them in the standard sync output.
func reconcileLinearParents(ctx context.Context, lt *linear.Tracker, dryRun, jsonOutput bool, warnings *[]string) {
	if lt == nil || getStore() == nil {
		return
	}
	links, err := buildLinearParentLinks(ctx, lt)
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("parent reconcile: building link set failed: %v", err))
		return
	}
	if len(links) == 0 {
		return
	}
	stats, err := lt.ReconcileParents(ctx, links, dryRun)
	renderLinearParentReconcile(stats, dryRun, jsonOutput)
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("parent reconcile: %v", err))
		return
	}
	appendLinearParentWarnings(warnings, stats.Errors)
}

func renderLinearParentReconcile(stats *linear.ParentReconcileStats, dryRun, jsonOutput bool) {
	if stats == nil || jsonOutput {
		return
	}
	if dryRun {
		if stats.WouldUpdate == 0 {
			return
		}
		fmt.Printf("[dry-run] Would reconcile %d Linear parent link%s\n", stats.WouldUpdate, plural(stats.WouldUpdate))
		for _, link := range stats.Mutations {
			fmt.Printf("[dry-run] Would set parent of %s → %s\n", link.ChildIdentifier, link.ParentIdentifier)
		}
		return
	}
	if stats.Updated > 0 {
		fmt.Printf("✓ Reconciled %d Linear parent link%s\n", stats.Updated, plural(stats.Updated))
	}
}

func appendLinearParentWarnings(warnings *[]string, errs []error) {
	for _, err := range errs {
		*warnings = append(*warnings, fmt.Sprintf("parent reconcile: %v", err))
	}
}

// buildLinearParentLinks enumerates local beads with a Linear external_ref
// and a parent-child dependency to a parent that also has a Linear
// external_ref. The result is the set of (child, parent) pairs whose
// Linear parent field should be set.
//
// Beads whose parent isn't yet synced to Linear are silently skipped —
// they'll get picked up on a subsequent sync once the parent has an
// external_ref.
func buildLinearParentLinks(ctx context.Context, lt *linear.Tracker) ([]linear.ParentLink, error) {
	issues, err := getStore().SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return nil, err
	}
	idToIdent := linearParentIdentifiers(issues, lt)
	if len(idToIdent) == 0 {
		return nil, nil
	}
	links := make([]linear.ParentLink, 0)
	for _, issue := range issues {
		issueLinks, err := linearParentLinksForIssue(ctx, issue, idToIdent)
		if err != nil {
			return nil, err
		}
		links = append(links, issueLinks...)
	}
	return links, nil
}

func linearParentIdentifiers(issues []*types.Issue, lt *linear.Tracker) map[string]string {
	idToIdent := make(map[string]string, len(issues))
	for _, issue := range issues {
		if issue.ExternalRef == nil {
			continue
		}
		ref := strings.TrimSpace(*issue.ExternalRef)
		if !lt.IsExternalRef(ref) {
			continue
		}
		ident := lt.ExtractIdentifier(ref)
		if ident != "" {
			idToIdent[issue.ID] = ident
		}
	}
	return idToIdent
}

func linearParentLinksForIssue(ctx context.Context, issue *types.Issue, idToIdent map[string]string) ([]linear.ParentLink, error) {
	childIdent, ok := idToIdent[issue.ID]
	if !ok {
		return nil, nil
	}
	deps, err := getStore().GetDependenciesWithMetadata(ctx, issue.ID)
	if err != nil {
		return nil, fmt.Errorf("loading deps for %s: %w", issue.ID, err)
	}
	links := make([]linear.ParentLink, 0)
	for _, dep := range deps {
		if dep == nil || dep.DependencyType != types.DepParentChild {
			continue
		}
		parentIdent, ok := idToIdent[dep.Issue.ID]
		if ok {
			links = append(links, linear.ParentLink{
				ChildIdentifier:  childIdent,
				ParentIdentifier: parentIdent,
			})
		}
	}
	return links, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
