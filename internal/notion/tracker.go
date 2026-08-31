package notion

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/jonbaldie/beads/internal/storage"
	itracker "github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
)

const defaultBatchPushWorkers = 8

type notionAPI interface {
	GetCurrentUser(ctx context.Context) (*User, error)
	RetrieveDataSource(ctx context.Context, dataSourceID string) (*DataSource, error)
	QueryDataSource(ctx context.Context, dataSourceID string) ([]Page, error)
	CreatePage(ctx context.Context, dataSourceID string, properties map[string]interface{}) (*Page, error)
	UpdatePage(ctx context.Context, pageID string, properties map[string]interface{}) (*Page, error)
	ArchivePage(ctx context.Context, pageID string, inTrash bool) (*Page, error)
}

var newNotionClient = func(token string) notionAPI {
	return NewClient(token)
}

func init() {
	itracker.Register("notion", func() itracker.IssueTracker {
		return NewTracker()
	})
}

type Tracker struct {
	trackerLifecycle
	trackerPull
	trackerIssueOps
	trackerPush
	trackerMapping
	trackerCache
	state trackerState
}

type trackerState struct {
	client       notionAPI
	store        storage.Storage
	config       *MappingConfig
	dataSourceID string
	viewURL      string
	authSource   AuthSource

	cacheMu         sync.RWMutex
	issueCache      []itracker.TrackerIssue
	remoteByPageID  map[string]itracker.TrackerIssue
	remoteByLocalID map[string]itracker.TrackerIssue
	lastQueried     int
	lastCandidates  int
}

type trackerLifecycle struct{ owner *Tracker }
type trackerPull struct{ owner *Tracker }
type trackerIssueOps struct{ owner *Tracker }
type trackerPush struct{ owner *Tracker }
type trackerMapping struct{ owner *Tracker }
type trackerCache struct{ owner *Tracker }

// NewTracker returns an initialized Notion tracker. Operations use embedded
// components so each responsibility has a focused method set while the
// public tracker API remains unchanged.
func NewTracker() *Tracker {
	t := &Tracker{}
	t.trackerLifecycle.owner = t
	t.trackerPull.owner = t
	t.trackerIssueOps.owner = t
	t.trackerPush.owner = t
	t.trackerMapping.owner = t
	t.trackerCache.owner = t
	return t
}

func (t *trackerLifecycle) Name() string         { return "notion" }
func (t *trackerLifecycle) DisplayName() string  { return "Notion" }
func (t *trackerLifecycle) ConfigPrefix() string { return "notion" }

func (t *trackerLifecycle) Init(ctx context.Context, store storage.Storage) error {
	t.owner.state.store = store
	t.owner.state.dataSourceID = t.owner.getConfig(ctx, "notion.data_source_id", "NOTION_DATA_SOURCE_ID")
	t.owner.state.viewURL = t.owner.getConfig(ctx, "notion.view_url", "NOTION_VIEW_URL")

	auth, err := ResolveAuth(ctx, store)
	if err != nil {
		return err
	}
	if auth == nil || strings.TrimSpace(auth.Token) == "" {
		return fmt.Errorf("Notion authentication is not configured (set notion.token or export NOTION_TOKEN)")
	}
	if t.owner.state.dataSourceID == "" {
		return fmt.Errorf("Notion data source not configured (run 'bd notion init --parent <page-id>', 'bd notion connect --url <notion-url>', or set notion.data_source_id)")
	}
	t.owner.state.authSource = auth.Source
	if t.owner.state.client == nil {
		t.owner.state.client = newNotionClient(auth.Token)
	}
	if t.owner.state.config == nil {
		t.owner.state.config = DefaultMappingConfig()
	}
	return nil
}

func (t *trackerLifecycle) Validate() error {
	if t.owner.state.client == nil {
		return fmt.Errorf("Notion tracker not initialized")
	}
	_, err := t.owner.state.client.RetrieveDataSource(context.Background(), t.owner.state.dataSourceID)
	if err != nil {
		return fmt.Errorf("Notion validation failed: %w", err)
	}
	return nil
}

func (t *trackerLifecycle) Close() error { return nil }

func (t *trackerPull) FetchIssues(ctx context.Context, opts itracker.FetchOptions) ([]itracker.TrackerIssue, error) {
	if err := t.owner.trackerCache.ensureRemoteIndex(ctx); err != nil {
		return nil, err
	}
	localByExternalIdentifier, localByID, err := t.buildLocalPullIndexes(ctx)
	if err != nil {
		return nil, err
	}
	t.owner.state.cacheMu.RLock()

	result := make([]itracker.TrackerIssue, 0, len(t.owner.state.issueCache))
	for _, issue := range t.owner.state.issueCache {
		candidate := cloneTrackerIssue(issue)
		if !matchesFetchState(&candidate, opts.State) {
			continue
		}
		if !matchesFetchSince(&candidate, opts.Since) && !shouldBackfillNotionIssue(&candidate, localByExternalIdentifier, localByID) {
			continue
		}
		result = append(result, candidate)
		if opts.Limit > 0 && len(result) >= opts.Limit {
			break
		}
	}
	queried := len(t.owner.state.issueCache)
	candidates := len(result)
	t.owner.state.cacheMu.RUnlock()
	t.owner.state.cacheMu.Lock()
	t.owner.state.lastQueried = queried
	t.owner.state.lastCandidates = candidates
	t.owner.state.cacheMu.Unlock()
	return result, nil
}

func (t *trackerPull) LastPullStats() (queried int, candidates int) {
	t.owner.state.cacheMu.RLock()
	defer t.owner.state.cacheMu.RUnlock()
	return t.owner.state.lastQueried, t.owner.state.lastCandidates
}

func (t *trackerPull) buildLocalPullIndexes(ctx context.Context) (map[string]struct{}, map[string]struct{}, error) {
	localByExternalIdentifier := map[string]struct{}{}
	localByID := map[string]struct{}{}
	if t.owner.state.store == nil {
		return localByExternalIdentifier, localByID, nil
	}
	localIssues, err := t.owner.state.store.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return nil, nil, fmt.Errorf("searching local issues: %w", err)
	}
	for _, issue := range localIssues {
		if issue == nil {
			continue
		}
		if id := strings.TrimSpace(issue.ID); id != "" {
			localByID[id] = struct{}{}
		}
		if issue.ExternalRef == nil {
			continue
		}
		if identifier := ExtractNotionIdentifier(strings.TrimSpace(*issue.ExternalRef)); identifier != "" {
			localByExternalIdentifier[identifier] = struct{}{}
		}
	}
	return localByExternalIdentifier, localByID, nil
}

func (t *trackerPull) FetchIssue(ctx context.Context, identifier string) (*itracker.TrackerIssue, error) {
	if err := t.owner.trackerCache.ensureRemoteIndex(ctx); err != nil {
		return nil, err
	}
	want := ExtractNotionIdentifier(identifier)
	if want == "" {
		want = strings.TrimSpace(identifier)
	}

	t.owner.state.cacheMu.RLock()
	defer t.owner.state.cacheMu.RUnlock()

	if issue, ok := t.owner.state.remoteByPageID[want]; ok {
		cloned := cloneTrackerIssue(issue)
		return &cloned, nil
	}
	if issue, ok := t.owner.state.remoteByLocalID[want]; ok {
		cloned := cloneTrackerIssue(issue)
		return &cloned, nil
	}
	for _, candidate := range t.owner.state.issueCache {
		if candidate.Identifier == want {
			cloned := cloneTrackerIssue(candidate)
			return &cloned, nil
		}
	}
	return nil, nil
}

func (t *trackerIssueOps) CreateIssue(ctx context.Context, issue *types.Issue) (*itracker.TrackerIssue, error) {
	pushIssue, err := PushIssueFromIssue(issue, t.owner.state.config)
	if err != nil {
		return nil, err
	}
	page, err := t.owner.state.client.CreatePage(ctx, t.owner.state.dataSourceID, BuildPageProperties(pushIssue))
	if err != nil {
		return nil, err
	}
	trackerIssue, err := TrackerIssueFromPullIssue(PulledIssueFromPage(*page), t.owner.state.config)
	if err != nil {
		return nil, err
	}
	t.owner.trackerCache.upsertRemoteIssue(trackerIssue)
	return trackerIssue, nil
}

func (t *trackerIssueOps) UpdateIssue(ctx context.Context, externalID string, issue *types.Issue) (*itracker.TrackerIssue, error) {
	pageID := ExtractNotionIdentifier(externalID)
	if pageID == "" && issue != nil && issue.ExternalRef != nil {
		pageID = ExtractNotionIdentifier(*issue.ExternalRef)
	}
	if pageID == "" {
		return nil, fmt.Errorf("invalid Notion page ID %q", externalID)
	}
	pushIssue, err := PushIssueFromIssue(issue, t.owner.state.config)
	if err != nil {
		return nil, err
	}
	page, err := t.owner.state.client.UpdatePage(ctx, pageID, BuildPageProperties(pushIssue))
	if err != nil {
		return nil, err
	}
	trackerIssue, err := TrackerIssueFromPullIssue(PulledIssueFromPage(*page), t.owner.state.config)
	if err != nil {
		return nil, err
	}
	t.owner.trackerCache.upsertRemoteIssue(trackerIssue)
	return trackerIssue, nil
}
