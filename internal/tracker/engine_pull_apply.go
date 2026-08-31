package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

// applyPullIssueUpdate keeps a pulled update atomic with its labels.
func applyPullIssueUpdate(ctx context.Context, tx storage.IssueLifecycleTransaction, id string, updates map[string]interface{}, labels []string, actor string) error {
	if err := applyPullIssueFields(ctx, tx, id, updates, actor); err != nil {
		return err
	}
	return syncIssueLabels(ctx, tx, id, labels, actor)
}

// applyPullIssueFields applies a pulled issue's fields while preserving the
// caller's control over related collections such as labels.
//
// A pull always forces close policy. The remote tracker is authoritative for
// the status it reports, and it knows nothing about local-only children or
// local-only blockers — refusing an upstream close because of them would wedge
// sync on state the remote cannot see and the operator did not create. Both the
// pull and the conflict reimport route through here, so this is the one place
// that decision lives.
func applyPullIssueFields(ctx context.Context, tx storage.IssueLifecycleTransaction, id string, updates map[string]interface{}, actor string) error {
	updates[issueops.OpForceClosePolicy] = true
	return tx.UpdateIssue(ctx, id, updates, actor)
}

func pullIssueEqual(local *types.Issue, remote *types.Issue, ref string) bool {
	if local == nil || remote == nil {
		return false
	}
	if !pullCoreFieldsEqual(local, remote) {
		return false
	}
	return pullExternalRefEqual(local, ref)
}

func pullCoreFieldsEqual(local *types.Issue, remote *types.Issue) bool {
	return local.Title == remote.Title &&
		local.Description == remote.Description &&
		local.Priority == remote.Priority &&
		local.Status == remote.Status &&
		local.IssueType == remote.IssueType &&
		strings.TrimSpace(local.Assignee) == strings.TrimSpace(remote.Assignee) &&
		equalNormalizedStrings(local.Labels, remote.Labels)
}

func pullExternalRefEqual(local *types.Issue, ref string) bool {
	localRef := ""
	if local.ExternalRef != nil {
		localRef = strings.TrimSpace(*local.ExternalRef)
	}
	return localRef == strings.TrimSpace(ref)
}

func buildPullIssueUpdates(existing *types.Issue, remote *types.Issue, ref string) map[string]interface{} {
	updates := map[string]interface{}{
		"title":       remote.Title,
		"description": remote.Description,
		"priority":    remote.Priority,
		"status":      string(remote.Status),
		"issue_type":  string(remote.IssueType),
		"assignee":    remote.Assignee,
	}
	trimmedRef := strings.TrimSpace(ref)
	if trimmedRef == "" {
		return updates
	}
	if existing.ExternalRef == nil || strings.TrimSpace(*existing.ExternalRef) != trimmedRef {
		updates["external_ref"] = trimmedRef
	}
	return updates
}

func marshalTrackerMetadata(metadata interface{}) (json.RawMessage, bool) {
	if metadata == nil {
		return nil, false
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(raw), true
}

func appendFilteredDependencies(dst []DependencyInfo, deps []DependencyInfo, allowedTypes []types.DependencyType, allowedSources []DependencySource) []DependencyInfo {
	if len(deps) == 0 {
		return dst
	}
	if len(allowedTypes) == 0 && len(allowedSources) == 0 {
		return append(dst, deps...)
	}
	allowed := make(map[string]struct{}, len(allowedTypes))
	for _, depType := range allowedTypes {
		allowed[string(depType)] = struct{}{}
	}
	allowedSourceSet := make(map[DependencySource]struct{}, len(allowedSources))
	for _, source := range allowedSources {
		allowedSourceSet[source] = struct{}{}
	}
	return appendMatchingDependencies(dst, deps, allowed, allowedSourceSet)
}

func appendMatchingDependencies(dst []DependencyInfo, deps []DependencyInfo, allowed map[string]struct{}, allowedSourceSet map[DependencySource]struct{}) []DependencyInfo {
	for _, dep := range deps {
		if len(allowed) > 0 {
			if _, ok := allowed[dep.Type]; !ok {
				continue
			}
		}
		if len(allowedSourceSet) > 0 {
			if _, ok := allowedSourceSet[dep.Source]; !ok {
				continue
			}
		}
		dst = append(dst, dep)
	}
	return dst
}

func fetchPrelinkedIssues(e *Engine, ctx context.Context, fetched []TrackerIssue, localIssues []*types.Issue, lastSync *time.Time) ([]TrackerIssue, map[string]bool, error) {
	hydratedLocalIDs := make(map[string]bool)
	if lastSync == nil {
		return nil, hydratedLocalIDs, nil
	}
	seen := prelinkedSeenIdentifiers(e, fetched)
	var hydrated []TrackerIssue
	for _, local := range localIssues {
		extIssue, identifier, ok, err := fetchPrelinkedIssue(e, ctx, local, *lastSync, seen)
		if err != nil {
			return hydrated, hydratedLocalIDs, err
		}
		if !ok {
			continue
		}
		hydrated = append(hydrated, *extIssue)
		hydratedLocalIDs[local.ID] = true
		addPrelinkedIdentifier(seen, identifier)
	}
	return hydrated, hydratedLocalIDs, nil
}

func prelinkedSeenIdentifiers(e *Engine, fetched []TrackerIssue) map[string]struct{} {
	seen := make(map[string]struct{}, len(fetched))
	for _, issue := range fetched {
		addPrelinkedIdentifier(seen, strings.TrimSpace(issue.Identifier))
		ref := e.Tracker.BuildExternalRef(&issue)
		addPrelinkedIdentifier(seen, strings.TrimSpace(e.Tracker.ExtractIdentifier(ref)))
	}
	return seen
}

func addPrelinkedIdentifier(seen map[string]struct{}, identifier string) {
	if identifier == "" {
		return
	}
	seen[identifier] = struct{}{}
	seen[strings.ToLower(identifier)] = struct{}{}
}

func fetchPrelinkedIssue(e *Engine, ctx context.Context, local *types.Issue, lastSync time.Time, seen map[string]struct{}) (*TrackerIssue, string, bool, error) {
	ref, ok := prelinkedExternalRef(e, local)
	if !ok {
		return nil, "", false, nil
	}
	changedAfterLastSync, err := externalRefChangedAfter(e, ctx, local, ref, lastSync)
	if err != nil {
		return nil, "", false, fmt.Errorf("checking pre-linked local issue %s: %w", local.ID, err)
	}
	if !changedAfterLastSync {
		return nil, "", false, nil
	}
	identifier := strings.TrimSpace(e.Tracker.ExtractIdentifier(ref))
	if identifier == "" || prelinkedIdentifierSeen(seen, identifier) {
		return nil, "", false, nil
	}
	extIssue, err := fetchPrelinkedRemote(e, ctx, identifier)
	if err != nil {
		return nil, "", false, err
	}
	if extIssue == nil {
		return nil, "", false, nil
	}
	return extIssue, identifier, true, nil
}

func prelinkedExternalRef(e *Engine, local *types.Issue) (string, bool) {
	if local == nil || local.ExternalRef == nil {
		return "", false
	}
	ref := strings.TrimSpace(*local.ExternalRef)
	return ref, ref != "" && e.Tracker.IsExternalRef(ref)
}

func fetchPrelinkedRemote(e *Engine, ctx context.Context, identifier string) (*TrackerIssue, error) {
	return e.Tracker.FetchIssue(ctx, identifier)
}

func prelinkedIdentifierSeen(seen map[string]struct{}, identifier string) bool {
	if _, ok := seen[identifier]; ok {
		return true
	}
	_, ok := seen[strings.ToLower(identifier)]
	return ok
}

// externalRefChangedAfter reports whether local's external_ref differed
// from currentRef as of lastSync. When the backing store can answer this
// precisely (storage.ExternalRefHistoryQuerier — Dolt-shaped stores that
// expose dolt_history_issues), it does; otherwise it falls back to a
// coarser timestamp heuristic.
//
// The fast path is gated on the ExternalRefHistoryQuerier capability, not on
// whether the store happens to expose a raw *sql.DB: some Dolt-shaped
// backends (e.g. embeddeddolt.EmbeddedDoltStore) support the
// dolt_history_issues query without a pooled *sql.DB, and a non-Dolt SQL
// backend could expose *sql.DB without having that Dolt system table at
// all. Gating on the capability keeps this correct in both directions.
func externalRefChangedAfter(e *Engine, ctx context.Context, local *types.Issue, currentRef string, lastSync time.Time) (bool, error) {
	if local == nil {
		return false, nil
	}
	querier, ok := externalRefHistoryQuerier(e.Store)
	if !ok {
		return local.CreatedAt.After(lastSync) || local.UpdatedAt.After(lastSync), nil
	}

	previousRef, found, err := querier.PreviousExternalRef(ctx, local.ID, lastSync)
	if err != nil {
		return false, err
	}
	if !found {
		return true, nil
	}
	return strings.TrimSpace(previousRef) != strings.TrimSpace(currentRef), nil
}

// externalRefHistoryQuerier type-asserts store to storage.ExternalRefHistoryQuerier,
// unwrapping storage decorators (HookFiringStore, telemetry.InstrumentedStorage,
// etc.) first if needed. Decorators embed only the base storage.DoltStorage
// interface for passthrough, so a direct assertion on a decorated store would
// never see this optional capability even when the concrete store underneath
// implements it — the same reason cmd/bd type-asserts through
// storage.UnwrapStore for RawDBAccessor, StoreLocator, and friends.
func externalRefHistoryQuerier(store storage.Storage) (storage.ExternalRefHistoryQuerier, bool) {
	if q, ok := store.(storage.ExternalRefHistoryQuerier); ok {
		return q, true
	}
	if dolt, ok := store.(storage.DoltStorage); ok {
		q, ok := storage.UnwrapStore(dolt).(storage.ExternalRefHistoryQuerier)
		return q, ok
	}
	return nil, false
}

func syncIssueLabels(ctx context.Context, tx storage.Transaction, issueID string, desired []string, actor string) error {
	current, err := tx.GetLabels(ctx, issueID)
	if err != nil {
		return err
	}
	currentSet := normalizedStringSet(current)
	desiredSet := normalizedStringSet(desired)
	for label := range currentSet {
		if _, ok := desiredSet[label]; ok {
			continue
		}
		if err := tx.RemoveLabel(ctx, issueID, label, actor); err != nil {
			return err
		}
	}
	for label := range desiredSet {
		if _, ok := currentSet[label]; ok {
			continue
		}
		if err := tx.AddLabel(ctx, issueID, label, actor); err != nil {
			return err
		}
	}
	return nil
}

func equalNormalizedStrings(a, b []string) bool {
	an := normalizedStringSlice(a)
	bn := normalizedStringSlice(b)
	if len(an) != len(bn) {
		return false
	}
	for i := range an {
		if an[i] != bn[i] {
			return false
		}
	}
	return true
}

func normalizedStringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		result[value] = struct{}{}
	}
	return result
}

func normalizedStringSlice(values []string) []string {
	set := normalizedStringSet(values)
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func parseSyncTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, fmt.Errorf("empty sync timestamp")
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, nil
	}
	return time.Parse(time.RFC3339, value)
}
