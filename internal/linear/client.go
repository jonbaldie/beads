package linear

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Client provides methods to interact with the Linear GraphQL API.
type Client struct {
	*clientCore
	*clientConfig
	*clientTransport
	*clientIssuePageReader
	*clientIssueLookupReader
	*clientIssueCreator
	*clientIssueUpdater
	*clientIssueBatchCreator
	*clientIssueBatchUpdater
	*clientTeamReader
	*clientProjectClient
}

// clientCore contains the shared connection and authentication configuration.
// The public Client type embeds this core so existing field selectors remain
// source-compatible while the API operations live in focused modules.
type clientCore struct {
	APIKey         string //nolint:gosec // G117: caller-supplied Linear credential, never an embedded secret.
	TeamID         string
	ProjectID      string // Optional: filter issues to a specific project
	Endpoint       string // GraphQL endpoint URL (defaults to DefaultAPIEndpoint)
	HTTPClient     *http.Client
	AuthMode       AuthMode
	TokenManager   *OAuthTokenManager
	RateLimitFloor int // Minimum remaining quota before circuit breaker trips (0 = use DefaultRateLimitFloor)
	rateLimitState *rateLimitState
}

type clientConfig struct {
	*clientCore
}

type clientTransport struct {
	*clientCore
}

type clientIssuePageReader struct {
	*clientTransport
}

type clientIssueLookupReader struct {
	*clientTransport
}

type clientIssueCreator struct {
	*clientTransport
}

type clientIssueUpdater struct {
	*clientTransport
}

type clientIssueBatchCreator struct {
	*clientTransport
}

type clientIssueBatchUpdater struct {
	*clientTransport
}

type clientTeamReader struct {
	*clientTransport
}

type clientProjectClient struct {
	*clientTransport
}

func newClient(core *clientCore) *Client {
	transport := &clientTransport{clientCore: core}
	return &Client{
		clientCore:              core,
		clientConfig:            &clientConfig{clientCore: core},
		clientTransport:         transport,
		clientIssuePageReader:   &clientIssuePageReader{clientTransport: transport},
		clientIssueLookupReader: &clientIssueLookupReader{clientTransport: transport},
		clientIssueCreator:      &clientIssueCreator{clientTransport: transport},
		clientIssueUpdater:      &clientIssueUpdater{clientTransport: transport},
		clientIssueBatchCreator: &clientIssueBatchCreator{clientTransport: transport},
		clientIssueBatchUpdater: &clientIssueBatchUpdater{clientTransport: transport},
		clientTeamReader:        &clientTeamReader{clientTransport: transport},
		clientProjectClient:     &clientProjectClient{clientTransport: transport},
	}
}

func cloneClientCore(core *clientCore) *clientCore {
	clone := *core
	return &clone
}

// projectsQuery is the GraphQL query for fetching projects.
const projectsQuery = `
	query Projects($filter: ProjectFilter!, $first: Int!, $after: String) {
		projects(
			first: $first
			after: $after
			filter: $filter
		) {
			nodes {
				id
				name
				description
				slugId
				url
				state
				progress
				createdAt
				updatedAt
				completedAt
			}
			pageInfo {
				hasNextPage
				endCursor
			}
		}
	}
`

// issuesQuery is the GraphQL query for fetching issues with all required fields.
// Used by both FetchIssues and FetchIssuesSince for consistency.
const issuesQuery = `
	query Issues($filter: IssueFilter!, $first: Int!, $after: String) {
		issues(
			first: $first
			after: $after
			filter: $filter
		) {
			nodes {
				id
				identifier
				title
				description
				url
				priority
				state {
					id
					name
					type
				}
				assignee {
					id
					name
					email
					displayName
				}
				labels {
					nodes {
						id
						name
					}
				}
				parent {
					id
					identifier
				}
				projectMilestone {
					id
					name
					description
					progress
					targetDate
				}
				relations {
					nodes {
						id
						type
						relatedIssue {
							id
							identifier
						}
					}
				}
				createdAt
				updatedAt
				completedAt
			}
			pageInfo {
				hasNextPage
				endCursor
			}
		}
	}
`

type rateLimitState struct {
	mu        sync.RWMutex
	remaining int
	resetsAt  time.Time
}

// maxRateLimitResetHorizon accommodates Linear's short quota windows and
// clock skew without allowing a nonsensical timestamp to block requests
// effectively forever.
const maxRateLimitResetHorizon = 24 * time.Hour

func newRateLimitState() *rateLimitState {
	return &rateLimitState{remaining: -1}
}

// NewClient creates a new Linear client with the given API key and team ID.
func NewClient(apiKey, teamID string) *Client {
	return newClient(&clientCore{
		APIKey:   apiKey,
		TeamID:   teamID,
		Endpoint: DefaultAPIEndpoint,
		AuthMode: AuthModeAPIKey,
		HTTPClient: &http.Client{
			Timeout: DefaultTimeout,
		},
		rateLimitState: newRateLimitState(),
	})
}

// NewOAuthClient creates a new Linear client that authenticates via OAuth
// client_credentials flow instead of a static API key.
func NewOAuthClient(oauthConfig OAuthConfig, teamID string) *Client {
	return newClient(&clientCore{
		TeamID:       teamID,
		Endpoint:     DefaultAPIEndpoint,
		AuthMode:     AuthModeOAuth,
		TokenManager: NewOAuthTokenManager(oauthConfig),
		HTTPClient: &http.Client{
			Timeout: DefaultTimeout,
		},
		rateLimitState: newRateLimitState(),
	})
}

// WithEndpoint returns a new client configured to use the specified endpoint.
// This is useful for testing with mock servers or connecting to self-hosted instances.
func (c *clientConfig) WithEndpoint(endpoint string) *Client {
	core := cloneClientCore(c.clientCore)
	core.Endpoint = endpoint
	return newClient(core)
}

// WithHTTPClient returns a new client configured to use the specified HTTP client.
// This is useful for testing or customizing timeouts and transport settings.
func (c *clientConfig) WithHTTPClient(httpClient *http.Client) *Client {
	core := cloneClientCore(c.clientCore)
	core.HTTPClient = httpClient
	return newClient(core)
}

// WithProjectID returns a new client configured to filter issues by the specified project.
// When set, FetchIssues and FetchIssuesSince will only return issues belonging to this project.
func (c *clientConfig) WithProjectID(projectID string) *Client {
	core := cloneClientCore(c.clientCore)
	core.ProjectID = projectID
	return newClient(core)
}

// linearAuthHeader returns the Authorization header value for this client.
func linearAuthHeader(c *clientTransport) (string, error) {
	switch c.AuthMode {
	case AuthModeOAuth:
		token, err := c.TokenManager.Token()
		if err != nil {
			return "", fmt.Errorf("failed to get OAuth token: %w", err)
		}
		return "Bearer " + token, nil
	default:
		return c.APIKey, nil
	}
}

// WithRateLimitFloor returns a new client with the specified rate-limit circuit-breaker floor.
// When remaining API quota drops below this value, Execute returns ErrRateLimitExhausted.
func (c *clientConfig) WithRateLimitFloor(floor int) *Client {
	core := cloneClientCore(c.clientCore)
	core.RateLimitFloor = floor
	return newClient(core)
}

func linearCircuitBreakerError(c *clientTransport) *ErrRateLimitExhausted {
	if c.rateLimitState == nil {
		return nil
	}
	c.rateLimitState.mu.Lock()
	defer c.rateLimitState.mu.Unlock()
	if c.rateLimitState.remaining < 0 || c.rateLimitState.remaining >= c.rateLimitFloor() {
		return nil
	}
	if !c.rateLimitState.resetsAt.IsZero() && !time.Now().Before(c.rateLimitState.resetsAt) {
		c.rateLimitState.remaining = -1
		c.rateLimitState.resetsAt = time.Time{}
		return nil
	}
	return &ErrRateLimitExhausted{
		Remaining: c.rateLimitState.remaining,
		Floor:     c.rateLimitFloor(),
		ResetsAt:  c.rateLimitState.resetsAt,
	}
}

func recordLinearRateLimitHeaders(c *clientTransport, info RateLimitInfo) {
	if c.rateLimitState == nil || info.RequestsRemaining < 0 {
		return
	}
	c.rateLimitState.mu.Lock()
	defer c.rateLimitState.mu.Unlock()
	now := time.Now()
	if info.RequestsReset.IsZero() || !now.Before(info.RequestsReset) || info.RequestsReset.After(now.Add(maxRateLimitResetHorizon)) {
		c.rateLimitState.remaining = -1
		c.rateLimitState.resetsAt = time.Time{}
		return
	}
	c.rateLimitState.remaining = info.RequestsRemaining
	c.rateLimitState.resetsAt = info.RequestsReset
}

// rateLimitFloor returns the effective circuit-breaker floor, using the
// default when the client has no explicit override.
func (c *clientCore) rateLimitFloor() int {
	if c.RateLimitFloor > 0 {
		return c.RateLimitFloor
	}
	return DefaultRateLimitFloor
}

// parseRetryAfter parses the Retry-After header value, which may be an
// integer number of seconds or an HTTP-date. Returns zero duration if
// the header is absent or unparseable.
//
// The integer form ("120") is tried first. For the HTTP-date form,
// http.ParseTime is used, which covers RFC 1123, RFC 850, and ANSI C
// formats as required by RFC 9110 §10.2.3.
func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if t, err := http.ParseTime(value); err == nil {
		if delay := time.Until(t); delay > 0 {
			return delay
		}
	}
	return 0
}

// parseRateLimitHeaders extracts rate-limit metadata from HTTP response headers.
func parseRateLimitHeaders(h http.Header) RateLimitInfo {
	info := RateLimitInfo{RequestsRemaining: -1}
	info.RetryAfter = parseRetryAfter(h.Get("Retry-After"))
	if v := h.Get("X-RateLimit-Requests-Remaining"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			info.RequestsRemaining = n
		}
	}
	if v := h.Get("X-RateLimit-Requests-Reset"); v != "" {
		if milliseconds, err := strconv.ParseInt(v, 10, 64); err == nil && milliseconds > 0 {
			info.RequestsReset = time.UnixMilli(milliseconds).UTC()
		}
	}
	return info
}
