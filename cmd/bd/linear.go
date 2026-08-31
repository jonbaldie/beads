package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/linear"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/spf13/cobra"
)

// linearCmd is the root command for Linear integration.
var linearCmd = &cobra.Command{
	Use:     "linear",
	GroupID: "advanced",
	Short:   "Linear integration commands",
	Long: `Synchronize issues between beads and Linear.

Configuration:
  bd config set linear.api_key "YOUR_API_KEY"
  bd config set linear.team_id "TEAM_ID"
  bd config set linear.team_ids "TEAM_ID1,TEAM_ID2"  # Multiple teams (comma-separated)
  bd config set linear.project_id "PROJECT_ID"  # Optional: sync only this project

Environment variables (alternative to config):
  LINEAR_API_KEY  - Linear API key (for individual developers)
  LINEAR_TEAM_ID  - Linear team ID (UUID, singular)
  LINEAR_TEAM_IDS - Linear team IDs (comma-separated UUIDs)

OAuth (for CI workers / automated sync):
  LINEAR_OAUTH_CLIENT_ID     - OAuth app client ID
  LINEAR_OAUTH_CLIENT_SECRET - OAuth app client secret

  When both OAuth env vars are set, OAuth client_credentials flow is used
  instead of the API key. This allows CI workers to authenticate as an
  application (actor=application) rather than impersonating a user.
  Precedence: OAuth > LINEAR_API_KEY > config file.

Data Mapping (optional, sensible defaults provided):
  Priority mapping (Linear 0-4 to Beads 0-4):
    bd config set linear.priority_map.0 4    # No priority -> Backlog
    bd config set linear.priority_map.1 0    # Urgent -> Critical
    bd config set linear.priority_map.2 1    # High -> High
    bd config set linear.priority_map.3 2    # Medium -> Medium
    bd config set linear.priority_map.4 3    # Low -> Low

  State mapping (Linear state type to Beads status):
    bd config set linear.state_map.backlog open
    bd config set linear.state_map.unstarted open
    bd config set linear.state_map.started in_progress
    bd config set linear.state_map.completed closed
    bd config set linear.state_map.canceled closed
    bd config set linear.state_map.my_custom_state in_progress  # Custom state names

  Label to issue type mapping:
    bd config set linear.label_type_map.bug bug
    bd config set linear.label_type_map.feature feature
    bd config set linear.label_type_map.epic epic

  Relation type mapping (Linear relations to Beads dependencies):
    bd config set linear.relation_map.blocks blocks
    bd config set linear.relation_map.blockedBy blocks
    bd config set linear.relation_map.duplicate duplicates
    bd config set linear.relation_map.related related

  ID generation (optional, hash IDs to match bd/Jira hash mode):
    bd config set linear.id_mode "hash"      # hash (default)
    bd config set linear.hash_length "6"     # hash length 3-8 (default: 6)

Examples:
  bd linear sync --pull         # Import issues from Linear
  bd linear sync --push         # Export issues to Linear
  bd linear sync                # Bidirectional sync (pull then push)
  bd linear sync --dry-run      # Preview sync without changes
  bd create "Fix login" --external-ref https://linear.app/team/issue/TEAM-123
                              # Link a local issue to an existing Linear issue
  bd linear status              # Show sync status`,
}

// linearSyncCmd handles synchronization with Linear.
var linearSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Synchronize issues with Linear",
	Long: `Synchronize issues between beads and Linear.

Modes:
  --pull              Import issues from Linear into beads
  --push              Export issues from beads to Linear
  --pull-if-stale     Pull only if data is stale (skip if fresh)
  (no flags)          Bidirectional sync: pull then push, with conflict resolution

Staleness (--pull-if-stale):
  --threshold 20m     How old data must be before pulling (default 20m)
  A 5-minute debounce prevents agent loops: if a pull completed within 5 minutes,
  data is always treated as fresh regardless of the threshold.

Team Selection:
  --team ID1,ID2  Override configured team IDs for this sync
  Multiple teams can be configured via linear.team_ids (comma-separated).
  Falls back to linear.team_id for backward compatibility.
  Push requires explicit --team when multiple teams are configured.

Pull Options:
  --milestones       Reconstruct Linear project milestones as local epic parents

Type Filtering (--push only):
  --type task,feature       Only sync issues of these types
  --exclude-type wisp       Exclude issues of these types
  --include-ephemeral       Include ephemeral issues (wisps, etc.); default is to exclude
  --parent TICKET           Only push this ticket and its descendants
  --relations               Import Linear relations as bd dependencies on pull

Persistent push-direction ID filters (workflow artifacts, sandbox beads, etc.):
  bd config set linear.exclude_id_prefix "hw-mol-"
  bd config set linear.exclude_id_patterns "-wisp-,sandbox-,scratch-"

  exclude_id_prefix is a single case-sensitive prefix on the bead ID.
  exclude_id_patterns is a comma-separated list of case-sensitive substrings
  (matched anywhere in the ID). Both are combined as a union: a bead
  matching either rule is skipped from push (no create, no update). Beads
  with an existing external_ref that NOW match are silently skipped on
  future syncs; the Linear-side issue persists — archive/delete it manually
  if desired.

Conflict Resolution:
  By default, newer timestamp wins. Override with:
  --prefer-local    Always prefer local beads version
  --prefer-linear   Always prefer Linear version

Examples:
  bd linear sync --pull                         # Import from Linear
  bd linear sync --pull-if-stale                # Pull only if data is stale
  bd linear sync --pull-if-stale --threshold 5m # Pull if older than 5 minutes
  bd linear sync --pull --relations             # Import Linear blocking relations as bd deps
  bd linear sync --push --create-only           # Push new issues only
  bd linear sync --push --type=task,feature     # Push only tasks and features
  bd linear sync --push --exclude-type=wisp     # Push all except wisps
  bd linear sync --push --parent=bd-abc123      # Push one ticket tree
  bd linear sync --dry-run                      # Preview without changes
  bd linear sync --prefer-local                 # Bidirectional, local wins`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runLinearSync,
}

// linearStatusCmd shows the current sync status.
var linearStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show Linear sync status",
	Long: `Show the current Linear sync status, including:
  - Last sync timestamp
  - Configuration status
  - Number of issues with Linear links
  - Issues pending push (no external_ref)`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runLinearStatus,
}

// linearTeamsCmd lists available teams.
var linearTeamsCmd = &cobra.Command{
	Use:   "teams",
	Short: "List available Linear teams",
	Long: `List all teams accessible with your Linear API key.

Use this to find the team ID (UUID) needed for configuration.

Example:
  bd linear teams
  bd config set linear.team_id "12345678-1234-1234-1234-123456789abc"`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runLinearTeams,
}

func init() {
	linearSyncCmd.Flags().Bool("pull", false, "Pull issues from Linear")
	linearSyncCmd.Flags().Bool("push", false, "Push issues to Linear")
	linearSyncCmd.Flags().Bool("dry-run", false, "Preview sync without making changes")
	linearSyncCmd.Flags().Bool("prefer-local", false, "Prefer local version on conflicts")
	linearSyncCmd.Flags().Bool("prefer-linear", false, "Prefer Linear version on conflicts")
	linearSyncCmd.Flags().Bool("create-only", false, "Only create new issues, don't update existing")
	linearSyncCmd.Flags().Bool("update-refs", true, "Update external_ref after creating Linear issues")
	linearSyncCmd.Flags().Bool("milestones", false, "Reconstruct Linear project milestones as local epic parents when pulling")
	linearSyncCmd.Flags().String("state", "all", "Issue state to sync: open, closed, all")
	linearSyncCmd.Flags().StringSlice("type", nil, "Only sync issues of these types (can be repeated)")
	linearSyncCmd.Flags().StringSlice("exclude-type", nil, "Exclude issues of these types (can be repeated)")
	linearSyncCmd.Flags().Bool("include-ephemeral", false, "Include ephemeral issues (wisps, etc.) when pushing to Linear")
	linearSyncCmd.Flags().String("parent", "", "Limit push to this beads ticket and its descendants")
	linearSyncCmd.Flags().StringSlice("team", nil, "Team ID(s) to sync (overrides configured team_id/team_ids)")
	linearSyncCmd.Flags().Bool("relations", false, "Import Linear relations as bd dependencies when pulling")
	linearSyncCmd.Flags().Bool("pull-if-stale", false, "Pull only if Linear data is stale (skip if fresh)")
	linearSyncCmd.Flags().Duration("threshold", linear.DefaultStaleThreshold, "Staleness threshold for --pull-if-stale (default 20m)")
	linearSyncCmd.Flags().Bool("no-wait", false, "Fail immediately if another sync is running instead of waiting")
	registerSelectiveSyncFlags(linearSyncCmd)

	linearCmd.AddCommand(linearSyncCmd)
	linearCmd.AddCommand(linearStatusCmd)
	linearCmd.AddCommand(linearTeamsCmd)
	rootCmd.AddCommand(linearCmd)
}

type linearSyncFlags struct {
	direction linearSyncDirectionFlags
	behavior  linearSyncBehaviorFlags
	filter    linearSyncFilterFlags
	staleness linearSyncStalenessFlags
	lock      linearSyncLockFlags
}

type linearSyncDirectionFlags struct {
	pull bool
	push bool
}

type linearSyncBehaviorFlags struct {
	dryRun       bool
	preferLocal  bool
	preferLinear bool
	createOnly   bool
	milestones   bool
}

type linearSyncFilterFlags struct {
	state            string
	typeFilters      []string
	excludeTypes     []string
	includeEphemeral bool
	cliTeams         []string
	relations        bool
}

type linearSyncStalenessFlags struct {
	pullIfStale bool
	threshold   time.Duration
}

type linearSyncLockFlags struct {
	noWait bool
}

type linearSyncSetup struct {
	tracker *linear.Tracker
	engine  *tracker.Engine
	opts    tracker.SyncOptions
}

func readLinearSyncFlags(cmd *cobra.Command) linearSyncFlags {
	flags := linearSyncFlags{}
	flags.direction.pull, _ = cmd.Flags().GetBool("pull")
	flags.direction.push, _ = cmd.Flags().GetBool("push")
	flags.behavior.dryRun, _ = cmd.Flags().GetBool("dry-run")
	flags.behavior.preferLocal, _ = cmd.Flags().GetBool("prefer-local")
	flags.behavior.preferLinear, _ = cmd.Flags().GetBool("prefer-linear")
	flags.behavior.createOnly, _ = cmd.Flags().GetBool("create-only")
	flags.behavior.milestones, _ = cmd.Flags().GetBool("milestones")
	flags.filter.state, _ = cmd.Flags().GetString("state")
	flags.filter.typeFilters, _ = cmd.Flags().GetStringSlice("type")
	flags.filter.excludeTypes, _ = cmd.Flags().GetStringSlice("exclude-type")
	flags.filter.includeEphemeral, _ = cmd.Flags().GetBool("include-ephemeral")
	flags.filter.cliTeams, _ = cmd.Flags().GetStringSlice("team")
	flags.filter.relations, _ = cmd.Flags().GetBool("relations")
	flags.staleness.pullIfStale, _ = cmd.Flags().GetBool("pull-if-stale")
	flags.staleness.threshold, _ = cmd.Flags().GetDuration("threshold")
	flags.lock.noWait, _ = cmd.Flags().GetBool("no-wait")
	return flags
}

func runLinearSync(cmd *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("linear sync is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("linear-sync")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	flags := readLinearSyncFlags(cmd)
	if skipped, err := maybeSkipLinearSyncForStaleness(&flags); err != nil {
		return err
	} else if skipped {
		return nil
	}

	releaseLock, err := acquireLinearSyncLock(flags.lock.noWait)
	if err != nil {
		return err
	}
	defer releaseLinearSyncLock(releaseLock)

	if err := validateLinearSyncRequest(flags); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	setup, err := prepareLinearSync(getRootContext(), cmd, flags)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	result, err := setup.engine.Sync(getRootContext(), setup.opts)
	if err := handleLinearSyncError(result, err); err != nil {
		return err
	}

	finalizeLinearSync(getRootContext(), flags, setup, result)
	return renderLinearSyncResult(result, flags)
}

func releaseLinearSyncLock(release func()) {
	if release != nil {
		release()
	}
}

func maybeSkipLinearSyncForStaleness(flags *linearSyncFlags) (bool, error) {
	if !flags.staleness.pullIfStale {
		return false, nil
	}
	beadsDir := resolveBeadsDirForStaleness()
	if beadsDir == "" {
		flags.direction.pull = true
		return false, nil
	}
	if linear.IsWithinDebounce(beadsDir) {
		info := linear.GetStalenessInfo(beadsDir, flags.staleness.threshold)
		return true, renderFreshLinearSync(info, true)
	}
	if !linear.IsPullStale(beadsDir, flags.staleness.threshold) {
		info := linear.GetStalenessInfo(beadsDir, flags.staleness.threshold)
		return true, renderFreshLinearSync(info, false)
	}
	flags.direction.pull = true
	return false, nil
}

func renderFreshLinearSync(info linear.StalenessInfo, withinDebounce bool) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"is_fresh":  true,
			"last_pull": info.LastPull.Format(time.RFC3339),
			"age":       linear.FormatAge(info.Age),
			"skipped":   true,
		})
	}
	message := "Linear data is fresh (last pull %s ago)\n"
	if withinDebounce {
		message = "Linear data is fresh (last pull %s ago, within debounce)\n"
	}
	fmt.Printf(message, linear.FormatAge(info.Age))
	return nil
}

func acquireLinearSyncLock(noWait bool) (func(), error) {
	lockDir := beads.FindBeadsDir()
	if lockDir == "" {
		return nil, nil
	}
	wait := !noWait
	if wait {
		fmt.Fprintln(os.Stderr, "Acquiring sync lock...")
	} else {
		fmt.Fprintln(os.Stderr, "Acquiring sync lock (non-blocking)...")
	}
	syncLock, err := linear.AcquireSyncLock(lockDir, wait)
	if err != nil {
		return nil, linearSyncLockError(err)
	}
	return func() {
		if err := syncLock.Release(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to release sync lock: %v\n", err)
		}
	}, nil
}

func linearSyncLockError(err error) error {
	if held, ok := err.(*linear.SyncLockHeldError); ok {
		if held.Info != nil {
			return HandleError("another bd linear sync is already running (PID %d, started %s)",
				held.Info.PID, held.Info.Started.Format("15:04:05"))
		}
		return HandleError("another bd linear sync is already running")
	}
	return HandleError("acquiring sync lock: %v", err)
}

func validateLinearSyncRequest(flags linearSyncFlags) error {
	if !flags.behavior.dryRun {
		if err := CheckReadonly("linear sync"); err != nil {
			return err
		}
	}
	if flags.behavior.preferLocal && flags.behavior.preferLinear {
		return errors.New("cannot use both --prefer-local and --prefer-linear")
	}
	if flags.behavior.milestones && flags.direction.push && !flags.direction.pull {
		return errors.New("--milestones only applies when pulling from Linear")
	}
	if err := ensureStoreActive(); err != nil {
		return fmt.Errorf("database not available: %w", err)
	}
	return validateLinearConfig(flags.filter.cliTeams)
}

func prepareLinearSync(ctx context.Context, cmd *cobra.Command, flags linearSyncFlags) (*linearSyncSetup, error) {
	teamIDs := getLinearTeamIDs(ctx, flags.filter.cliTeams)
	willPush := flags.direction.push || !flags.direction.pull
	if willPush && len(teamIDs) > 1 && len(flags.filter.cliTeams) == 0 {
		return nil, errors.New("push requires explicit --team flag when multiple teams are configured\n" +
			"Use: bd linear sync --push --team <TEAM_ID>")
	}

	lt, err := initializeLinearTracker(ctx, teamIDs, willPush)
	if err != nil {
		return nil, err
	}

	engine := tracker.NewEngine(lt, getStore(), getActor())
	engine.OnMessage = func(msg string) { fmt.Println("  " + msg) }
	engine.OnWarning = func(msg string) { fmt.Fprintf(os.Stderr, "Warning: %s\n", msg) } //nolint:gosec // G705: CLI stderr, not HTML.
	engine.PullHooks = buildLinearPullHooks(ctx, linearPullHookOptions{
		Milestones: flags.behavior.milestones,
		DryRun:     flags.behavior.dryRun,
		Actor:      getActor(),
	})

	opts, err := buildLinearSyncOptions(ctx, cmd, flags)
	if err != nil {
		return nil, err
	}
	engine.PushHooks = buildLinearPushHooks(ctx, lt, opts.ParentID != "" || len(opts.IssueIDs) > 0)
	applyLinearConflictResolution(&opts, flags)
	return &linearSyncSetup{tracker: lt, engine: engine, opts: opts}, nil
}

func initializeLinearTracker(ctx context.Context, teamIDs []string, willPush bool) (*linear.Tracker, error) {
	lt := linear.NewTracker()
	lt.SetTeamIDs(teamIDs)
	if err := lt.Init(ctx, getStore()); err != nil {
		return nil, fmt.Errorf("initializing Linear tracker: %w", err)
	}
	if willPush {
		if err := lt.ValidatePushStateMappings(ctx); err != nil {
			return nil, err
		}
	}
	return lt, nil
}

func buildLinearSyncOptions(ctx context.Context, cmd *cobra.Command, flags linearSyncFlags) (tracker.SyncOptions, error) {
	opts := tracker.SyncOptions{
		Pull:              flags.direction.pull,
		Push:              flags.direction.push,
		DryRun:            flags.behavior.dryRun,
		CreateOnly:        flags.behavior.createOnly,
		State:             flags.filter.state,
		DependencySources: linearPullDependencySources(flags.filter.relations),
	}
	for _, issueType := range flags.filter.typeFilters {
		opts.TypeFilter = append(opts.TypeFilter, types.IssueType(strings.ToLower(issueType)))
	}
	for _, issueType := range flags.filter.excludeTypes {
		opts.ExcludeTypes = append(opts.ExcludeTypes, types.IssueType(strings.ToLower(issueType)))
	}
	applyLinearExcludeIDConfig(ctx, getStore(), &opts)
	if !flags.filter.includeEphemeral {
		opts.ExcludeEphemeral = true
	}
	if err := applySelectiveSyncFlags(cmd, &opts, flags.direction.push); err != nil {
		return tracker.SyncOptions{}, err
	}
	return opts, nil
}

func applyLinearConflictResolution(opts *tracker.SyncOptions, flags linearSyncFlags) {
	if flags.behavior.preferLocal {
		opts.ConflictResolution = tracker.ConflictLocal
	} else if flags.behavior.preferLinear {
		opts.ConflictResolution = tracker.ConflictExternal
	} else {
		opts.ConflictResolution = tracker.ConflictTimestamp
	}
}

func handleLinearSyncError(result *tracker.SyncResult, err error) error {
	if err == nil {
		return nil
	}
	if isJSONOutput() {
		if jerr := outputJSON(result); jerr != nil {
			return jerr
		}
		return SilentExit()
	}
	return HandleError("%v", err)
}

func finalizeLinearSync(ctx context.Context, flags linearSyncFlags, setup *linearSyncSetup, result *tracker.SyncResult) {
	if shouldReconcileLinearSync(flags, setup, result) {
		reconcileLinearParents(ctx, setup.tracker, flags.behavior.dryRun, isJSONOutput(), &result.Warnings)
	}
	if shouldRecordLinearPull(flags) {
		recordLinearPullTimestamp()
	}
}

func shouldReconcileLinearSync(flags linearSyncFlags, setup *linearSyncSetup, result *tracker.SyncResult) bool {
	effectivePush := flags.direction.push || (!flags.direction.push && !flags.direction.pull)
	return effectivePush && result.Success && !syncIsScoped(&setup.opts)
}

func shouldRecordLinearPull(flags linearSyncFlags) bool {
	return (flags.direction.pull || !flags.direction.push) && !flags.behavior.dryRun
}

func recordLinearPullTimestamp() {
	if beadsDir := resolveBeadsDirForStaleness(); beadsDir != "" {
		_ = linear.WriteLastPullTimestamp(beadsDir)
	}
}

func renderLinearSyncResult(result *tracker.SyncResult, flags linearSyncFlags) error {
	if isJSONOutput() {
		if flags.staleness.pullIfStale {
			return outputJSON(map[string]interface{}{
				"stats":    result.Stats,
				"warnings": result.Warnings,
				"is_fresh": true,
				"skipped":  false,
			})
		}
		return outputJSON(result)
	}
	if flags.behavior.dryRun {
		fmt.Println("\n✓ Dry run complete (no changes made)")
		return nil
	}
	renderLinearSyncStats(result.Stats)
	fmt.Println("\n✓ Linear sync complete")
	renderLinearSyncWarnings(result.Warnings)
	return nil
}

func renderLinearSyncStats(stats tracker.SyncStats) {
	if stats.Pulled > 0 {
		fmt.Printf("✓ Pulled %d issues (%d created, %d updated)\n", stats.Pulled, stats.Created, stats.Updated)
	}
	if stats.Pushed > 0 {
		fmt.Printf("✓ Pushed %d issues\n", stats.Pushed)
	}
	if stats.Conflicts > 0 {
		fmt.Printf("→ Resolved %d conflicts\n", stats.Conflicts)
	}
}

func renderLinearSyncWarnings(warnings []string) {
	if len(warnings) == 0 {
		return
	}
	fmt.Println("\nWarnings:")
	for _, warning := range warnings {
		fmt.Printf("  - %s\n", warning)
	}
}

func linearPullDependencySources(includeRelations bool) []tracker.DependencySource {
	if includeRelations {
		return nil
	}
	return []tracker.DependencySource{tracker.DependencySourceParent}
}
