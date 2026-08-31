package tracker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// syncTracer is the OTel tracer for tracker sync spans.
var syncTracer = otel.Tracer("github.com/jonbaldie/beads/tracker")

// rateLimitExhaustedError is implemented by tracker errors (e.g.
// linear.ErrRateLimitExhausted) that signal the API quota floor has been
// hit and the sync loop should abort immediately rather than cascade the
// error across every remaining issue.
type rateLimitExhaustedError interface {
	RateLimitExhausted() bool
}

// isRateLimitExhausted reports whether err (or any error it wraps) signals
// that the API rate-limit circuit breaker has tripped.
func isRateLimitExhausted(err error) bool {
	var rle rateLimitExhaustedError
	return errors.As(err, &rle) && rle.RateLimitExhausted()
}

// PullHooks contains optional callbacks that customize pull (import) behavior.
// Trackers opt into behaviors by setting the hooks they need.
type PullHooks struct {
	// GenerateID assigns an ID to a newly-pulled issue before import.
	// If nil, issues keep whatever ID the storage layer assigns.
	// The hook receives the issue (with converted fields) and should set issue.ID.
	// Callers typically pre-load used IDs into the closure for collision avoidance.
	GenerateID func(ctx context.Context, issue *types.Issue) error

	// TransformIssue is called after FieldMapper.IssueToBeads() and before storage.
	// Use for description formatting, field normalization, etc.
	TransformIssue func(issue *types.Issue)

	// ShouldImport filters issues during pull. Return false to skip.
	// Called on the raw TrackerIssue before conversion to beads format.
	// If nil, all issues are imported.
	ShouldImport func(issue *TrackerIssue) bool

	// AfterConvert is called after the external issue has been converted to
	// a beads issue, transformed, and assigned an ID, but before it is stored.
	// Hooks may mutate the conversion, for example by adding dependencies that
	// should be created after all pulled issues have been saved.
	AfterConvert func(ctx context.Context, extIssue *TrackerIssue, conv *IssueConversion, ref string, existing *types.Issue, opts SyncOptions) error
}

// PushHooks contains optional callbacks that customize push (export) behavior.
// Trackers opt into behaviors by setting the hooks they need.
type PushHooks struct {
	// FormatDescription transforms the description before sending to tracker.
	// Linear uses this for BuildLinearDescription (merging structured fields).
	// If nil, issue.Description is used as-is.
	FormatDescription func(issue *types.Issue) string

	// ContentEqual compares local and remote issues to skip unnecessary API calls.
	// Returns true if content is identical (skip update). If nil, uses timestamp comparison.
	ContentEqual func(local *types.Issue, remote *TrackerIssue) bool

	// ContentHash, if set, returns a stable fingerprint of the issue's pushable
	// fields. When present, the engine binds the hash to the issue's external_ref
	// and TargetScope (when provided), and persists that fingerprint in local_metadata
	// after each successful create/update (and whenever ContentEqual reports a
	// match). It consults the fingerprint BEFORE fetching the remote issue: if the
	// stored content and target still match, the remote fetch and update are
	// skipped entirely. This avoids the per-issue GET that ContentEqual alone
	// still incurs, so a no-op `--push-only` run does not drain the API rate limit
	// (gastownhall/beads#4214). Returning "" disables the short-circuit for that
	// issue (the engine falls back to the fetch + ContentEqual path).
	ContentHash func(local *types.Issue) string

	// TargetScope returns the canonical remote namespace in which external issue
	// identifiers are resolved. Trackers whose refs may omit that namespace (for
	// example github:42 omits host and repository) should provide it so changing
	// remote configuration invalidates the push cache. If nil or empty, only the
	// external_ref identifies the target.
	TargetScope func() string

	// ShouldPush filters issues during push. Return false to skip.
	// Called in addition to type/state/ephemeral filters. Use for prefix filtering, etc.
	// If nil, all issues (matching other filters) are pushed.
	ShouldPush func(issue *types.Issue) bool

	// BuildStateCache is called once before the push loop to pre-cache workflow states.
	// Returns an opaque cache value passed to ResolveState on each issue.
	// If nil, no caching is done.
	BuildStateCache func(ctx context.Context) (interface{}, error)

	// ResolveState maps a beads status to a tracker state ID using the cached state.
	// Only called if BuildStateCache is set. Returns (stateID, ok).
	ResolveState func(cache interface{}, status types.Status) (string, bool)
}

// Engine orchestrates synchronization between beads and an external tracker.
// It implements the shared Pull→Detect→Resolve→Push pattern that all tracker
// integrations follow, eliminating duplication between Linear, GitLab, etc.
type Engine struct {
	Tracker   IssueTracker
	Store     lifecycleStorage
	Actor     string
	PullHooks *PullHooks
	PushHooks *PushHooks

	// Callbacks for UI feedback (optional).
	OnMessage func(msg string)
	OnWarning func(msg string)

	// stateCache holds the opaque value from PushHooks.BuildStateCache during a push.
	// Tracker adapters access it via ResolveState().
	stateCache interface{}

	// warnings collects warning messages during a Sync() call for inclusion in SyncResult.
	warnings []string
}

func setEngineStateCache(e *Engine, cache interface{}) { e.stateCache = cache }

func engineStateCache(e *Engine) interface{} { return e.stateCache }

type lifecycleStorage interface {
	storage.Storage
	storage.IssueLifecycleStore
}

// NewEngine creates a new sync engine for the given tracker and storage.
func NewEngine(tracker IssueTracker, store lifecycleStorage, actor string) *Engine {
	return &Engine{
		Tracker: tracker,
		Store:   store,
		Actor:   actor,
	}
}

// Sync performs a complete synchronization operation based on the given options.
func (e *Engine) Sync(ctx context.Context, opts SyncOptions) (*SyncResult, error) {
	ctx, span := syncTracer.Start(ctx, "tracker.sync",
		trace.WithAttributes(
			attribute.String("sync.tracker", e.Tracker.DisplayName()),
			attribute.Bool("sync.pull", opts.Pull || (!opts.Pull && !opts.Push)),
			attribute.Bool("sync.push", opts.Push || (!opts.Pull && !opts.Push)),
			attribute.Bool("sync.dry_run", opts.DryRun),
		),
	)
	defer span.End()

	result := &SyncResult{Success: true}
	e.warnings = nil
	opts = defaultSyncOptions(opts)

	// Track IDs to skip/force during push based on conflict resolution
	skipPushIDs := make(map[string]bool)
	forcePushIDs := make(map[string]bool)

	allowPullOverwriteIDs := make(map[string]bool)
	resolveSyncConflicts(e, ctx, opts, result, skipPushIDs, forcePushIDs, allowPullOverwriteIDs)

	// Phase 2: Pull
	if err := applyPullSync(e, ctx, opts, result, allowPullOverwriteIDs, skipPushIDs); err != nil {
		return syncFailure(result, span, "pull", err)
	}

	// Phase 3: Push
	if err := applyPushSync(e, ctx, opts, result, skipPushIDs, forcePushIDs); err != nil {
		return syncFailure(result, span, "push", err)
	}

	recordSyncSpanAttributes(span, result)
	if !opts.DryRun {
		result.LastSync = recordLastSync(e, ctx)
	}

	// Batch-push warnings were already appended above; e.warn-collected
	// warnings join them rather than replacing them.
	result.Warnings = append(result.Warnings, e.warnings...)
	return result, nil
}

func defaultSyncOptions(opts SyncOptions) SyncOptions {
	if !opts.Pull && !opts.Push {
		opts.Pull = true
		opts.Push = true
	}
	return opts
}

func resolveSyncConflicts(e *Engine, ctx context.Context, opts SyncOptions, result *SyncResult, skipPushIDs, forcePushIDs, allowPullOverwriteIDs map[string]bool) {
	if !opts.Pull || !opts.Push {
		return
	}
	conflicts, err := e.DetectConflicts(ctx)
	if err != nil {
		e.warn("Failed to detect conflicts: %v", err)
		return
	}
	if len(conflicts) == 0 {
		return
	}
	result.Stats.Conflicts = len(conflicts)
	resolveConflicts(e, opts, conflicts, skipPushIDs, forcePushIDs, allowPullOverwriteIDs)
}

func applyPullSync(e *Engine, ctx context.Context, opts SyncOptions, result *SyncResult, allowPullOverwriteIDs, skipPushIDs map[string]bool) error {
	if !opts.Pull {
		return nil
	}
	pullStats, err := doPull(e, ctx, opts, allowPullOverwriteIDs, skipPushIDs)
	if err != nil {
		return err
	}
	result.PullStats = *pullStats
	result.Stats.Pulled = pullStats.Created + pullStats.Updated
	result.Stats.Created += pullStats.Created
	result.Stats.Updated += pullStats.Updated
	result.Stats.Skipped += pullStats.Skipped
	result.Stats.Errors += pullStats.Errors
	return nil
}

func applyPushSync(e *Engine, ctx context.Context, opts SyncOptions, result *SyncResult, skipPushIDs, forcePushIDs map[string]bool) error {
	if !opts.Push {
		return nil
	}
	pushStats, err := doPush(e, ctx, opts, skipPushIDs, forcePushIDs)
	if err != nil {
		return err
	}
	result.PushStats = *pushStats
	result.Stats.Pushed = pushStats.Created + pushStats.Updated
	result.Stats.Created += pushStats.Created
	result.Stats.Updated += pushStats.Updated
	result.Stats.Skipped += pushStats.Skipped
	result.Stats.Errors += pushStats.Errors
	result.Warnings = append(result.Warnings, pushStats.Warnings...)
	return nil
}

func syncFailure(result *SyncResult, span trace.Span, phase string, err error) (*SyncResult, error) {
	result.Success = false
	result.Error = fmt.Sprintf("%s failed: %v", phase, err)
	span.RecordError(err)
	span.SetStatus(codes.Error, result.Error)
	return result, err
}

func recordSyncSpanAttributes(span trace.Span, result *SyncResult) {
	span.SetAttributes(
		attribute.Int("sync.pulled", result.Stats.Pulled),
		attribute.Int("sync.pushed", result.Stats.Pushed),
		attribute.Int("sync.conflicts", result.Stats.Conflicts),
		attribute.Int("sync.created", result.Stats.Created),
		attribute.Int("sync.updated", result.Stats.Updated),
		attribute.Int("sync.skipped", result.Stats.Skipped),
		attribute.Int("sync.errors", result.Stats.Errors),
	)
}

func recordLastSync(e *Engine, ctx context.Context) string {
	// Dolt DATETIME columns round sub-second values, so rows this sync just
	// wrote can carry updated_at values up to half a second in the future of
	// wall clock. Record last_sync at the next whole second so the engine's
	// own writes are never misread as local edits by the next pull's guard.
	lastSync := time.Now().UTC().Truncate(time.Second).Add(time.Second).Format(time.RFC3339Nano)
	key := e.Tracker.ConfigPrefix() + ".last_sync"
	if err := e.Store.SetLocalMetadata(ctx, key, lastSync); err != nil {
		e.warn("Failed to update last_sync: %v", err)
	}
	return lastSync
}

// DetectConflicts identifies issues that were modified both locally and externally
// since the last sync.
func (e *Engine) DetectConflicts(ctx context.Context) ([]Conflict, error) {
	ctx, span := syncTracer.Start(ctx, "tracker.detect_conflicts",
		trace.WithAttributes(attribute.String("sync.tracker", e.Tracker.DisplayName())),
	)
	defer span.End()

	lastSync, ok, err := loadConflictSyncTime(e, ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	conflicts, err := findConflicts(e, ctx, lastSync)
	if err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.Int("sync.conflicts", len(conflicts)))
	return conflicts, nil
}

func loadConflictSyncTime(e *Engine, ctx context.Context) (time.Time, bool, error) {
	key := e.Tracker.ConfigPrefix() + ".last_sync"
	lastSyncStr, err := e.Store.GetLocalMetadata(ctx, key)
	if err != nil || lastSyncStr == "" {
		return time.Time{}, false, nil
	}
	lastSync, err := parseSyncTime(lastSyncStr)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("invalid last_sync timestamp %q: %w", lastSyncStr, err)
	}
	return lastSync, true, nil
}

func findConflicts(e *Engine, ctx context.Context, lastSync time.Time) ([]Conflict, error) {
	issues, err := e.Store.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return nil, fmt.Errorf("searching issues: %w", err)
	}
	var conflicts []Conflict
	for _, issue := range issues {
		if conflict, ok := conflictForIssue(e, ctx, issue, lastSync); ok {
			conflicts = append(conflicts, conflict)
		}
	}
	return conflicts, nil
}

func conflictForIssue(e *Engine, ctx context.Context, issue *types.Issue, lastSync time.Time) (Conflict, bool) {
	extRef := derefStr(issue.ExternalRef)
	if extRef == "" || !e.Tracker.IsExternalRef(extRef) || !issue.UpdatedAt.After(lastSync) {
		return Conflict{}, false
	}
	extID := e.Tracker.ExtractIdentifier(extRef)
	if extID == "" {
		return Conflict{}, false
	}
	extIssue, err := e.Tracker.FetchIssue(ctx, extID)
	if err != nil || extIssue == nil || !extIssue.UpdatedAt.After(lastSync) {
		return Conflict{}, false
	}
	return Conflict{
		IssueID:            issue.ID,
		LocalUpdated:       issue.UpdatedAt,
		ExternalUpdated:    extIssue.UpdatedAt,
		ExternalRef:        extRef,
		ExternalIdentifier: extIssue.Identifier,
		ExternalInternalID: extIssue.ID,
	}, true
}
