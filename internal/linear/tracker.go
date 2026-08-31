package linear

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/tracker"
)

var _ tracker.BatchPushTracker = (*Tracker)(nil)

func init() {
	tracker.Register("linear", func() tracker.IssueTracker {
		return NewTracker()
	})
}

// Tracker implements tracker.IssueTracker for Linear. Its operation groups
// share the fields below; the groups are initialized by NewTracker and are
// embedded so the public tracker API remains unchanged.
type Tracker struct {
	trackerLifecycle
	trackerIssues
	trackerMapping
	trackerParent

	clients   map[string]*Client // keyed by team ID
	config    *MappingConfig
	store     storage.Storage
	teamIDs   []string // ordered list of configured team IDs
	projectID string
}

type trackerLifecycle struct{ owner *Tracker }
type trackerIssues struct{ owner *Tracker }
type trackerMapping struct{ owner *Tracker }
type trackerParent struct{ owner *Tracker }

// NewTracker returns an initialized Linear tracker. The zero value remains
// useful for simple value-free helpers, but an initialized tracker is required
// before calling operations that access configured clients or storage.
func NewTracker() *Tracker {
	t := &Tracker{}
	t.trackerLifecycle.owner = t
	t.trackerIssues.owner = t
	t.trackerMapping.owner = t
	t.trackerParent.owner = t
	return t
}

// SetTeamIDs sets the team IDs before Init(). When set, Init() uses these
// instead of reading from config. This supports the --team CLI flag.
func (t *trackerLifecycle) SetTeamIDs(ids []string) {
	t.owner.teamIDs = ids
}

func (t *trackerLifecycle) Name() string         { return "linear" }
func (t *trackerLifecycle) DisplayName() string  { return "Linear" }
func (t *trackerLifecycle) ConfigPrefix() string { return "linear" }

func (t *trackerLifecycle) Init(ctx context.Context, store storage.Storage) error {
	t.owner.store = store
	auth, err := resolveLinearAuth(ctx, t.owner.getConfig)
	if err != nil {
		return err
	}
	if err := t.owner.resolveTeamIDs(ctx); err != nil {
		return err
	}
	endpoint, projectID := linearOptionalSettings(ctx, store)
	if projectID != "" {
		t.owner.projectID = projectID
	}
	rateLimitFloor := t.owner.rateLimitFloor(ctx)
	t.owner.clients = buildLinearClients(t.owner.teamIDs, auth, endpoint, projectID, rateLimitFloor)

	t.owner.config = LoadMappingConfig(&configLoaderAdapter{ctx: ctx, store: store})
	return nil
}

type linearAuth struct {
	oauthClientID     string
	oauthClientSecret string
	apiKey            string
}

func resolveLinearAuth(ctx context.Context, getConfig func(context.Context, string, string) (string, error)) (linearAuth, error) {
	// OAuth client-credentials takes precedence over API key.
	auth := linearAuth{}
	auth.oauthClientID, _ = getConfig(ctx, "linear.oauth_client_id", "LINEAR_OAUTH_CLIENT_ID")
	auth.oauthClientSecret, _ = getConfig(ctx, "linear.oauth_client_secret", "LINEAR_OAUTH_CLIENT_SECRET")
	if auth.oauthClientID != "" && auth.oauthClientSecret != "" {
		return auth, nil
	}
	auth.apiKey, _ = getConfig(ctx, "linear.api_key", "LINEAR_API_KEY")
	if auth.apiKey == "" {
		return linearAuth{}, fmt.Errorf("Linear authentication not configured\n" +
			"Options:\n" +
			"  OAuth (for CI):  export LINEAR_OAUTH_CLIENT_ID=... LINEAR_OAUTH_CLIENT_SECRET=...\n" +
			"  API key (devs):  export LINEAR_API_KEY=... or bd config set linear.api_key \"YOUR_API_KEY\"")
	}
	return auth, nil
}

func (t *Tracker) resolveTeamIDs(ctx context.Context) error {
	if len(t.teamIDs) > 0 {
		return nil
	}
	pluralVal, _ := t.getConfig(ctx, "linear.team_ids", "LINEAR_TEAM_IDS")
	singularVal, _ := t.getConfig(ctx, "linear.team_id", "LINEAR_TEAM_ID")
	t.teamIDs = tracker.ResolveProjectIDs(nil, pluralVal, singularVal)
	if len(t.teamIDs) == 0 {
		return fmt.Errorf("Linear team ID not configured (set linear.team_id, linear.team_ids, or LINEAR_TEAM_ID)")
	}
	return nil
}

func linearOptionalSettings(ctx context.Context, store storage.Storage) (endpoint, projectID string) {
	if store == nil {
		return "", ""
	}
	endpoint, _ = store.GetConfig(ctx, "linear.api_endpoint")
	projectID, _ = store.GetConfig(ctx, "linear.project_id")
	return endpoint, projectID
}

func (t *Tracker) rateLimitFloor(ctx context.Context) int {
	floorStr, _ := t.getConfig(ctx, "linear.rate_limit_floor", "LINEAR_RATE_LIMIT_FLOOR")
	value, err := strconv.Atoi(strings.TrimSpace(floorStr))
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func buildLinearClients(teamIDs []string, auth linearAuth, endpoint, projectID string, rateLimitFloor int) map[string]*Client {
	clients := make(map[string]*Client, len(teamIDs))
	for _, teamID := range teamIDs {
		client := newLinearClient(teamID, auth)
		client = configureLinearClient(client, endpoint, projectID, rateLimitFloor)
		clients[teamID] = client
	}
	return clients
}

func newLinearClient(teamID string, auth linearAuth) *Client {
	if auth.oauthClientID != "" && auth.oauthClientSecret != "" {
		return NewOAuthClient(OAuthConfig{
			ClientID:     auth.oauthClientID,
			ClientSecret: auth.oauthClientSecret,
		}, teamID)
	}
	return NewClient(auth.apiKey, teamID)
}

func configureLinearClient(client *Client, endpoint, projectID string, rateLimitFloor int) *Client {
	if endpoint != "" {
		client = client.WithEndpoint(endpoint)
	}
	if projectID != "" {
		client = client.WithProjectID(projectID)
	}
	if rateLimitFloor > 0 {
		client = client.WithRateLimitFloor(rateLimitFloor)
	}
	return client
}

func (t *trackerLifecycle) Validate() error {
	if len(t.owner.clients) == 0 {
		return fmt.Errorf("Linear tracker not initialized")
	}
	return nil
}

func (t *trackerLifecycle) Close() error { return nil }
