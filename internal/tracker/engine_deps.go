package tracker

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// resolveConflicts applies the configured conflict resolution strategy.
func resolveConflicts(e *Engine, opts SyncOptions, conflicts []Conflict, skipIDs, forceIDs, allowPullOverwriteIDs map[string]bool) {
	for _, c := range conflicts {
		switch opts.ConflictResolution {
		case ConflictLocal:
			forceIDs[c.IssueID] = true
			e.msg("Conflict on %s: keeping local version", c.IssueID)

		case ConflictExternal:
			skipIDs[c.IssueID] = true
			allowPullOverwriteIDs[c.IssueID] = true
			e.msg("Conflict on %s: keeping external version", c.IssueID)

		default: // ConflictTimestamp or unset
			if c.LocalUpdated.After(c.ExternalUpdated) {
				forceIDs[c.IssueID] = true
				e.msg("Conflict on %s: local is newer, pushing", c.IssueID)
			} else {
				skipIDs[c.IssueID] = true
				allowPullOverwriteIDs[c.IssueID] = true
				e.msg("Conflict on %s: external is newer, importing", c.IssueID)
			}
		}
	}
}

// createDependencies creates dependencies from the pending list, matching
// external IDs to local issue IDs. Returns the number of dependencies that
// failed to resolve or create.
func createDependencies(e *Engine, ctx context.Context, deps []DependencyInfo) int {
	if len(deps) == 0 {
		return 0
	}

	resolveIssue, err := dependencyIssueResolver(e, ctx, nil)
	if err != nil {
		e.warn("Failed to build dependency resolver: %v", err)
		return len(deps)
	}

	errCount := 0
	for _, dep := range deps {
		fromIssue, err := resolveIssue(ctx, dep.FromExternalID)
		if err != nil {
			e.warn("Failed to resolve dependency source %s: %v", dep.FromExternalID, err)
			errCount++
			continue
		}
		toIssue, err := resolveIssue(ctx, dep.ToExternalID)
		if err != nil {
			e.warn("Failed to resolve dependency target %s: %v", dep.ToExternalID, err)
			errCount++
			continue
		}

		if fromIssue == nil || toIssue == nil {
			continue // Not found (no error) — expected if issue wasn't imported
		}

		d := &types.Dependency{
			IssueID:     fromIssue.ID,
			DependsOnID: toIssue.ID,
			Type:        types.DependencyType(dep.Type),
		}
		if err := e.Store.AddDependency(ctx, d, e.Actor); err != nil {
			e.warn("Failed to create dependency %s -> %s: %v", fromIssue.ID, toIssue.ID, err)
			errCount++
		}
	}
	return errCount
}

func previewDependencies(e *Engine, ctx context.Context, deps []DependencyInfo, dryRunIssues []*types.Issue) int {
	if len(deps) == 0 {
		return 0
	}

	resolveIssue, err := dependencyIssueResolver(e, ctx, dryRunIssues)
	if err != nil {
		e.warn("Failed to build dependency resolver: %v", err)
		return len(deps)
	}

	wouldCreate := countPreviewDependencies(e, ctx, deps, resolveIssue)
	if wouldCreate > 0 {
		e.msg("[dry-run] Would create %d dependencies", wouldCreate)
	}
	return 0
}

func countPreviewDependencies(e *Engine, ctx context.Context, deps []DependencyInfo, resolveIssue func(context.Context, string) (*types.Issue, error)) int {
	wouldCreate := 0
	pending := make(map[string]struct{}, len(deps))
	for _, dep := range deps {
		if previewDependency(e, ctx, dep, resolveIssue, pending) {
			wouldCreate++
		}
	}
	return wouldCreate
}

func previewDependency(e *Engine, ctx context.Context, dep DependencyInfo, resolveIssue func(context.Context, string) (*types.Issue, error), pending map[string]struct{}) bool {
	fromIssue, err := resolveIssue(ctx, dep.FromExternalID)
	if err != nil {
		e.warn("Failed to resolve dependency source %s: %v", dep.FromExternalID, err)
		return false
	}
	toIssue, err := resolveIssue(ctx, dep.ToExternalID)
	if err != nil {
		e.warn("Failed to resolve dependency target %s: %v", dep.ToExternalID, err)
		return false
	}
	if fromIssue == nil || toIssue == nil || dependencyExists(ctx, e.Store, fromIssue.ID, toIssue.ID, types.DependencyType(dep.Type)) {
		return false
	}
	key := pendingDependencyPreviewKey(fromIssue.ID, toIssue.ID, dep.Type)
	if _, ok := pending[key]; ok {
		return false
	}
	pending[key] = struct{}{}
	fromDisplay := firstNonEmpty(fromIssue.ID, dep.FromExternalID)
	toDisplay := firstNonEmpty(toIssue.ID, dep.ToExternalID)
	e.msg("[dry-run] Would create dependency: %s -> %s (%s)", fromDisplay, toDisplay, dep.Type)
	return true
}

func pendingDependencyPreviewKey(fromID, toID, depType string) string {
	return strings.Join([]string{
		strings.TrimSpace(fromID),
		strings.TrimSpace(toID),
		strings.TrimSpace(depType),
	}, "\x00")
}

func dependencyIssueResolver(e *Engine, ctx context.Context, extraIssues []*types.Issue) (func(context.Context, string) (*types.Issue, error), error) {
	issues, searchErr := e.Store.SearchIssues(ctx, "", types.IssueFilter{})
	if searchErr != nil {
		return nil, searchErr
	}
	issues = append(issues, extraIssues...)
	byExternal := make(map[string]*types.Issue, len(issues)*2)
	for _, candidate := range issues {
		indexDependencyIssue(e, byExternal, candidate)
	}

	return func(ctx context.Context, externalID string) (*types.Issue, error) {
		return resolveDependencyIssue(e, ctx, byExternal, externalID)
	}, nil
}

func indexDependencyIssue(e *Engine, byExternal map[string]*types.Issue, candidate *types.Issue) {
	if candidate == nil || candidate.ExternalRef == nil {
		return
	}
	ref := strings.TrimSpace(*candidate.ExternalRef)
	if ref == "" {
		return
	}
	addDependencyIndexEntry(byExternal, ref, candidate)
	if !e.Tracker.IsExternalRef(ref) {
		return
	}
	identifier := strings.TrimSpace(e.Tracker.ExtractIdentifier(ref))
	if identifier == "" {
		return
	}
	addDependencyIndexEntry(byExternal, identifier, candidate)
	addDependencyIndexEntry(byExternal, strings.ToLower(identifier), candidate)
}

func addDependencyIndexEntry(index map[string]*types.Issue, key string, issue *types.Issue) {
	if _, exists := index[key]; !exists {
		index[key] = issue
	}
}

func resolveDependencyIssue(e *Engine, ctx context.Context, byExternal map[string]*types.Issue, externalID string) (*types.Issue, error) {
	externalID = strings.TrimSpace(externalID)
	if externalID == "" {
		return nil, nil
	}
	if issue := findDependencyIndexIssue(byExternal, externalID); issue != nil {
		return issue, nil
	}
	if e.Tracker.IsExternalRef(externalID) {
		identifier := strings.TrimSpace(e.Tracker.ExtractIdentifier(externalID))
		if issue := findDependencyIndexIssue(byExternal, identifier); issue != nil {
			return issue, nil
		}
	}
	if strings.Contains(externalID, "://") {
		return e.Store.GetIssueByExternalRef(ctx, externalID)
	}
	return nil, nil
}

func findDependencyIndexIssue(index map[string]*types.Issue, key string) *types.Issue {
	if key == "" {
		return nil
	}
	if issue := index[key]; issue != nil {
		return issue
	}
	return index[strings.ToLower(key)]
}

func dependencyExists(ctx context.Context, store storage.Storage, issueID, dependsOnID string, depType types.DependencyType) bool {
	if strings.TrimSpace(issueID) == "" || strings.TrimSpace(dependsOnID) == "" {
		return false
	}
	records, err := store.GetDependenciesWithMetadata(ctx, issueID)
	if err != nil {
		return false
	}
	for _, record := range records {
		if record.ID == dependsOnID && record.DependencyType == depType {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// buildDescendantSet returns the set of issue IDs consisting of the given parent
// and all its transitive descendants via parent-child dependencies.
func buildDescendantSet(e *Engine, ctx context.Context, parentID string) (map[string]bool, error) {
	result := map[string]bool{parentID: true}
	queue := []string{parentID}
	queueIndex, queueLength := 0, len(queue)
	for queueIndex < queueLength {
		current := queue[queueIndex]
		queueIndex++
		dependents, err := e.Store.GetDependentsWithMetadata(ctx, current)
		if err != nil {
			return nil, fmt.Errorf("getting dependents of %s: %w", current, err)
		}
		for _, dep := range dependents {
			if dep.DependencyType == types.DepParentChild && !result[dep.Issue.ID] {
				result[dep.Issue.ID] = true
				queue = append(queue, dep.Issue.ID)
			}
		}
		queueLength = len(queue)
	}
	return result, nil
}

func shouldPushIssue(_ *Engine, issue *types.Issue, opts SyncOptions) bool {
	if opts.ExcludeEphemeral && issue.Ephemeral {
		return false
	}
	return matchesPushTypeFilter(issue, opts) &&
		!isExcludedPushType(issue, opts) &&
		!isExcludedPushID(issue, opts) &&
		pushStateAllowed(issue, opts)
}

func matchesPushTypeFilter(issue *types.Issue, opts SyncOptions) bool {
	if len(opts.TypeFilter) > 0 {
		for _, t := range opts.TypeFilter {
			if issue.IssueType == t {
				return true
			}
		}
		return false
	}
	return true
}

func isExcludedPushType(issue *types.Issue, opts SyncOptions) bool {
	for _, t := range opts.ExcludeTypes {
		if issue.IssueType == t {
			return true
		}
	}
	return false
}

func isExcludedPushID(issue *types.Issue, opts SyncOptions) bool {
	if opts.ExcludeIDPrefix != "" && strings.HasPrefix(issue.ID, opts.ExcludeIDPrefix) {
		return true
	}
	for _, p := range opts.ExcludeIDPatterns {
		if p != "" && strings.Contains(issue.ID, p) {
			return true
		}
	}
	return false
}

func pushStateAllowed(issue *types.Issue, opts SyncOptions) bool {
	return opts.State != "open" || issue.Status != types.StatusClosed
}

// strPtr returns a pointer to the given string.
func strPtr(s string) *string { return &s }

// derefStr safely dereferences a *string, returning "" for nil.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// isBeadID returns true if the given string looks like a local bead ID
// (i.e. it starts with the configured prefix followed by a hyphen, like "bd-123").
// External tracker refs (URLs, "EXT-1", etc.) will return false.
func isBeadID(id, prefix string) bool {
	if prefix == "" || id == "" {
		return false
	}
	return strings.HasPrefix(id, prefix+"-")
}

// buildIssueIDSet converts a slice of IDs into a set for O(1) lookup.
func buildIssueIDSet(ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}
