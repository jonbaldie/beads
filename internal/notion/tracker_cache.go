package notion

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/config"
	itracker "github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
)

func (t *trackerCache) ensureRemoteIndex(ctx context.Context) error {
	if t.remoteIndexReady() {
		return nil
	}

	pages, err := t.owner.state.client.QueryDataSource(ctx, t.owner.state.dataSourceID)
	if err != nil {
		return err
	}
	cache, byPageID, byLocalID, err := buildNotionRemoteIndex(pages, t.owner.state.config)
	if err != nil {
		return err
	}

	t.owner.state.cacheMu.Lock()
	t.owner.state.issueCache = cache
	t.owner.state.remoteByPageID = byPageID
	t.owner.state.remoteByLocalID = byLocalID
	t.owner.state.cacheMu.Unlock()
	return nil
}

func (t *trackerCache) remoteIndexReady() bool {
	t.owner.state.cacheMu.RLock()
	defer t.owner.state.cacheMu.RUnlock()
	return t.owner.state.issueCache != nil && t.owner.state.remoteByPageID != nil && t.owner.state.remoteByLocalID != nil
}

func buildNotionRemoteIndex(pages []Page, config *MappingConfig) ([]itracker.TrackerIssue, map[string]itracker.TrackerIssue, map[string]itracker.TrackerIssue, error) {
	cache := make([]itracker.TrackerIssue, 0, len(pages))
	byPageID := make(map[string]itracker.TrackerIssue, len(pages))
	byLocalID := make(map[string]itracker.TrackerIssue, len(pages))
	for _, page := range pages {
		if page.InTrash || page.Archived {
			continue
		}
		pulled := PulledIssueFromPage(page)
		trackerIssue, err := TrackerIssueFromPullIssue(pulled, config)
		if err != nil {
			return nil, nil, nil, err
		}
		cache = append(cache, *trackerIssue)
		if id := strings.TrimSpace(trackerIssue.ID); id != "" {
			byPageID[id] = *trackerIssue
		}
		if identifier := strings.TrimSpace(pulled.ID); identifier != "" {
			byLocalID[identifier] = *trackerIssue
		}
	}
	return cache, byPageID, byLocalID, nil
}

func (t *trackerCache) lookupRemoteByPageID(pageID string) (*itracker.TrackerIssue, bool) {
	return t.lookupRemoteIssue(t.owner.state.remoteByPageID, pageID)
}

func (t *trackerCache) lookupRemoteByLocalID(localID string) (*itracker.TrackerIssue, bool) {
	return t.lookupRemoteIssue(t.owner.state.remoteByLocalID, localID)
}

func (t *trackerCache) lookupRemoteIssue(index map[string]itracker.TrackerIssue, identifier string) (*itracker.TrackerIssue, bool) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return nil, false
	}
	t.owner.state.cacheMu.RLock()
	defer t.owner.state.cacheMu.RUnlock()
	issue, ok := index[identifier]
	if !ok {
		return nil, false
	}
	cloned := cloneTrackerIssue(issue)
	return &cloned, true
}

func (t *trackerCache) upsertRemoteIssue(issue *itracker.TrackerIssue) {
	if issue == nil {
		return
	}
	cloned := cloneTrackerIssue(*issue)
	t.owner.state.cacheMu.Lock()
	defer t.owner.state.cacheMu.Unlock()

	t.replaceCachedIssue(*issue, cloned)
	t.ensureRemoteMaps()
	t.indexRemoteIssue(*issue, cloned)
}

func (t *trackerCache) replaceCachedIssue(issue, cloned itracker.TrackerIssue) {
	for i := range t.owner.state.issueCache {
		if sameTrackerIssue(t.owner.state.issueCache[i], issue) {
			t.owner.state.issueCache[i] = cloned
			return
		}
	}
	t.owner.state.issueCache = append(t.owner.state.issueCache, cloned)
}

func (t *trackerCache) ensureRemoteMaps() {
	if t.owner.state.remoteByPageID == nil {
		t.owner.state.remoteByPageID = make(map[string]itracker.TrackerIssue)
	}
	if t.owner.state.remoteByLocalID == nil {
		t.owner.state.remoteByLocalID = make(map[string]itracker.TrackerIssue)
	}
}

func (t *trackerCache) indexRemoteIssue(issue, cloned itracker.TrackerIssue) {
	if id := strings.TrimSpace(issue.ID); id != "" {
		t.owner.state.remoteByPageID[id] = cloned
	}
	if identifier := strings.TrimSpace(ExtractNotionIdentifier(issue.URL)); identifier != "" {
		t.owner.state.remoteByPageID[identifier] = cloned
	}
	if raw, ok := issue.Raw.(*PulledIssue); ok && raw != nil && strings.TrimSpace(raw.ID) != "" {
		t.owner.state.remoteByLocalID[strings.TrimSpace(raw.ID)] = cloned
	}
}

// getConfig reads a config value from storage, falling back to env var.
// For yaml-only keys, reads from config.yaml first to avoid leaking
// secrets when pushing the Dolt database to remotes.
func (t *Tracker) getConfig(ctx context.Context, key, envVar string) string {
	// Secret keys are stored in config.yaml, not the Dolt database,
	// to avoid leaking secrets when pushing to remotes.
	if config.IsYamlOnlyKey(key) {
		if val := config.GetString(key); strings.TrimSpace(val) != "" {
			return strings.TrimSpace(val)
		}
		if envVar != "" {
			if envVal := strings.TrimSpace(os.Getenv(envVar)); envVal != "" {
				return envVal
			}
		}
		return ""
	}

	if t.state.store != nil {
		if value, err := t.state.store.GetConfig(ctx, key); err == nil && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	if envVar != "" {
		return strings.TrimSpace(os.Getenv(envVar))
	}
	return ""
}

func trackerIssueEqual(local *types.Issue, remote *itracker.TrackerIssue) bool {
	if local == nil {
		return false
	}
	if remote == nil {
		return false
	}
	if !equalNotionIssueText(local, remote) {
		return false
	}
	if !equalNotionIssueMetadata(local, remote) {
		return false
	}
	if !equalNotionIssueAssignee(local, remote) {
		return false
	}
	return equalStringSets(local.Labels, remote.Labels)
}

func equalNotionIssueText(local *types.Issue, remote *itracker.TrackerIssue) bool {
	if strings.TrimSpace(local.Title) != strings.TrimSpace(remote.Title) {
		return false
	}
	if strings.TrimSpace(local.Description) != strings.TrimSpace(remote.Description) {
		return false
	}
	return true
}

func equalNotionIssueMetadata(local *types.Issue, remote *itracker.TrackerIssue) bool {
	if local.Priority != remote.Priority {
		return false
	}
	if state, ok := remote.State.(types.Status); !ok || state != local.Status {
		return false
	}
	if issueType, ok := remote.Type.(types.IssueType); !ok || issueType != local.IssueType {
		return false
	}
	return true
}

func equalNotionIssueAssignee(local *types.Issue, remote *itracker.TrackerIssue) bool {
	if strings.TrimSpace(local.Assignee) != strings.TrimSpace(remote.Assignee) {
		return false
	}
	return true
}

func equalStringSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := normalizeStringSlice(left)
	rightCopy := normalizeStringSlice(right)
	for i := range leftCopy {
		if leftCopy[i] != rightCopy[i] {
			return false
		}
	}
	return true
}

func normalizeStringSlice(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func sameTrackerIssue(left, right itracker.TrackerIssue) bool {
	leftIDs := []string{
		ExtractNotionIdentifier(left.ID),
		ExtractNotionIdentifier(left.Identifier),
		ExtractNotionIdentifier(left.URL),
	}
	rightIDs := []string{
		ExtractNotionIdentifier(right.ID),
		ExtractNotionIdentifier(right.Identifier),
		ExtractNotionIdentifier(right.URL),
	}
	for _, leftID := range leftIDs {
		if leftID == "" {
			continue
		}
		for _, rightID := range rightIDs {
			if rightID != "" && leftID == rightID {
				return true
			}
		}
	}
	return false
}

func cloneTrackerIssue(issue itracker.TrackerIssue) itracker.TrackerIssue {
	cloned := issue
	if issue.Labels != nil {
		cloned.Labels = append([]string(nil), issue.Labels...)
	}
	return cloned
}

func matchesFetchSince(issue *itracker.TrackerIssue, since *time.Time) bool {
	if issue == nil {
		return false
	}
	if since != nil && !issue.UpdatedAt.IsZero() {
		// Notion page timestamps are minute-precision. Revisit the boundary minute
		// so edits made later in the same minute as last_sync are not lost.
		cutoff := since.UTC().Truncate(time.Minute)
		if issue.UpdatedAt.Before(cutoff) {
			return false
		}
	}
	return true
}

func matchesFetchState(issue *itracker.TrackerIssue, stateFilter string) bool {
	if issue == nil {
		return false
	}
	switch strings.TrimSpace(strings.ToLower(stateFilter)) {
	case "", "all":
		return true
	case "open":
		status, _ := issue.State.(types.Status)
		return status != types.StatusClosed
	case "closed":
		status, _ := issue.State.(types.Status)
		return status == types.StatusClosed
	default:
		return true
	}
}

func shouldBackfillNotionIssue(issue *itracker.TrackerIssue, localByExternalIdentifier, localByID map[string]struct{}) bool {
	if issue == nil {
		return false
	}
	for _, ref := range []string{issue.URL, issue.ID, issue.Identifier} {
		if identifier := ExtractNotionIdentifier(ref); identifier != "" {
			if _, ok := localByExternalIdentifier[identifier]; ok {
				return false
			}
		}
	}
	raw, ok := issue.Raw.(*PulledIssue)
	if !ok || raw == nil {
		return false
	}
	localID := strings.TrimSpace(raw.ID)
	if localID == "" {
		return false
	}
	_, ok = localByID[localID]
	return !ok
}

func derefStr(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}
