// Package gitlab provides client and data types for the GitLab REST API.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client provides methods to interact with the GitLab REST API.
type Client struct {
	Token      string       // GitLab personal access token or OAuth token
	BaseURL    string       // GitLab instance URL (e.g., "https://gitlab.com/api/v4")
	ProjectID  string       // Project ID or URL-encoded path (e.g., "group/project")
	GroupID    string       // Optional group ID or path for group-level issue fetching
	HTTPClient *http.Client // Optional custom HTTP client

	// taskTypeID caches the GraphQL GID for the "Task" work item type.
	// Populated lazily on first call to getTaskWorkItemTypeID.
	taskTypeID string
}

// NewClient creates a new GitLab client with the given token, base URL, and project ID.
func NewClient(token, baseURL, projectID string) *Client {
	return &Client{
		Token:     token,
		BaseURL:   baseURL,
		ProjectID: projectID,
		HTTPClient: &http.Client{
			Timeout: DefaultTimeout,
		},
	}
}

// WithGroupID returns a new client configured to fetch issues at the group level.
// When GroupID is set, FetchIssues and FetchIssuesSince use /groups/:id/issues
// instead of /projects/:id/issues. Issue creation still uses the project endpoint.
func (c *Client) WithGroupID(groupID string) *Client {
	return &Client{
		Token:      c.Token,
		BaseURL:    c.BaseURL,
		ProjectID:  c.ProjectID,
		GroupID:    groupID,
		HTTPClient: c.HTTPClient,
		taskTypeID: c.taskTypeID,
	}
}

// WithHTTPClient returns a new client configured to use the specified HTTP client.
// This is useful for testing or customizing timeouts and transport settings.
func (c *Client) WithHTTPClient(httpClient *http.Client) *Client {
	return &Client{
		Token:      c.Token,
		BaseURL:    c.BaseURL,
		ProjectID:  c.ProjectID,
		GroupID:    c.GroupID,
		HTTPClient: httpClient,
		taskTypeID: c.taskTypeID,
	}
}

// WithEndpoint returns a new client configured to use a custom API endpoint.
// This is useful for testing with mock servers or self-hosted GitLab instances.
func (c *Client) WithEndpoint(endpoint string) *Client {
	return &Client{
		Token:      c.Token,
		BaseURL:    endpoint,
		ProjectID:  c.ProjectID,
		GroupID:    c.GroupID,
		HTTPClient: c.HTTPClient,
		taskTypeID: c.taskTypeID,
	}
}

// projectPath returns the URL-encoded project path for API calls.
// This handles both numeric IDs (e.g., "123") and path-based IDs (e.g., "group/project").
func (c *Client) projectPath() string {
	return url.PathEscape(c.ProjectID)
}

// gitlabIssuesBasePath returns the API path prefix for listing issues.
// When GroupID is set, returns /groups/:id/issues (group-level).
// Otherwise, returns /projects/:id/issues (project-level).
func gitlabIssuesBasePath(c *Client) string {
	if c.GroupID != "" {
		return "/groups/" + url.PathEscape(c.GroupID) + "/issues"
	}
	return "/projects/" + c.projectPath() + "/issues"
}

// gitlabBuildURL constructs a full API URL from path and optional query parameters.
func gitlabBuildURL(c *Client, path string, params map[string]string) string {
	u := c.BaseURL + DefaultAPIEndpoint + path

	if len(params) > 0 {
		values := url.Values{}
		for k, v := range params {
			values.Set(k, v)
		}
		u += "?" + values.Encode()
	}

	return u
}

// gitlabDoRequest performs an HTTP request with authentication and retry logic.
func gitlabDoRequest(c *Client, ctx context.Context, method, urlStr string, body interface{}) ([]byte, http.Header, error) {
	var lastErr error
	for attempt := 0; attempt <= MaxRetries; attempt++ {
		result, err := doGitlabRequestAttempt(c, ctx, method, urlStr, body, attempt)
		if err != nil {
			lastErr = err
			continue
		}
		if result.status >= 200 && result.status < 300 {
			return result.body, result.header, nil
		}
		if !gitlabStatusRetriable(result.status) {
			return nil, nil, fmt.Errorf("API error: %s (status %d)", string(result.body), result.status)
		}
		lastErr = fmt.Errorf("transient error %d (attempt %d/%d)", result.status, attempt+1, MaxRetries+1)
		if err := waitGitlabRetry(ctx, result.header, attempt); err != nil {
			return nil, nil, err
		}
	}

	return nil, nil, fmt.Errorf("max retries (%d) exceeded: %w", MaxRetries+1, lastErr)
}

type gitlabAttemptResult struct {
	body   []byte
	header http.Header
	status int
}

func doGitlabRequestAttempt(c *Client, ctx context.Context, method, urlStr string, body interface{}, attempt int) (gitlabAttemptResult, error) {
	reqBody, err := gitlabRequestBody(body)
	if err != nil {
		return gitlabAttemptResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, reqBody)
	if err != nil {
		return gitlabAttemptResult{}, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return gitlabAttemptResult{}, fmt.Errorf("request failed (attempt %d/%d): %w", attempt+1, MaxRetries+1, err)
	}
	const maxResponseSize = 50 * 1024 * 1024
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	_ = resp.Body.Close() // Best effort: HTTP body close; connection may be reused regardless
	if err != nil {
		return gitlabAttemptResult{}, fmt.Errorf("failed to read response (attempt %d/%d): %w", attempt+1, MaxRetries+1, err)
	}
	return gitlabAttemptResult{body: respBody, header: resp.Header, status: resp.StatusCode}, nil
}

func gitlabRequestBody(body interface{}) (io.Reader, error) {
	if body == nil {
		return nil, nil
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}
	return bytes.NewReader(payload), nil
}

func gitlabStatusRetriable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func waitGitlabRetry(ctx context.Context, header http.Header, attempt int) error {
	delay, serverDelay := gitlabRetryDelay(header, attempt)
	if !serverDelay {
		if half := int64(delay / 2); half > 0 {
			delay += time.Duration(rand.Int64N(half)) //nolint:gosec // G404: retry jitter does not need crypto rand
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

func gitlabRetryDelay(header http.Header, attempt int) (time.Duration, bool) {
	delay := RetryDelay * time.Duration(1<<attempt)
	if retryAfter := header.Get("Retry-After"); retryAfter != "" {
		if seconds, err := strconv.Atoi(retryAfter); err == nil {
			return time.Duration(seconds) * time.Second, true
		}
	}
	return delay, false
}

// applyFilter adds IssueFilter fields as query parameters to the params map.
// ProjectID filtering is done client-side (not supported by GitLab API on group endpoints).
func applyFilter(params map[string]string, filter *IssueFilter) {
	if filter == nil {
		return
	}
	if filter.Labels != "" {
		params["labels"] = filter.Labels
	}
	if filter.Milestone != "" {
		params["milestone"] = filter.Milestone
	}
	if filter.Assignee != "" {
		params["assignee_username"] = filter.Assignee
	}
}

// filterByProject removes issues that don't match the filter's ProjectID.
// Returns the input slice unmodified if filter is nil or ProjectID is 0.
func filterByProject(issues []Issue, filter *IssueFilter) []Issue {
	if filter == nil || filter.ProjectID == 0 {
		return issues
	}
	filtered := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.ProjectID == filter.ProjectID {
			filtered = append(filtered, issue)
		}
	}
	return filtered
}
