// Package github provides client and data types for the GitHub REST API.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// linkNextPattern matches the "next" relation in GitHub Link headers.
var linkNextPattern = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// NewClient creates a new GitHub client with the given token, owner, and repo.
func NewClient(token, owner, repo string) *Client {
	return &Client{
		Token:   token,
		BaseURL: DefaultBaseURL,
		Owner:   owner,
		Repo:    repo,
		HTTPClient: &http.Client{
			Timeout: DefaultTimeout,
		},
		Retry: DefaultRetryConfig(),
	}
}

// WithHTTPClient returns a new client configured to use the specified HTTP client.
// This is useful for testing or customizing timeouts and transport settings.
func (c *Client) WithHTTPClient(httpClient *http.Client) *Client {
	return &Client{
		Token:      c.Token,
		BaseURL:    c.BaseURL,
		Owner:      c.Owner,
		Repo:       c.Repo,
		HTTPClient: httpClient,
		Retry:      c.Retry,
	}
}

// WithBaseURL returns a new client configured to use a custom API base URL.
// This is useful for testing with mock servers or GitHub Enterprise instances.
func (c *Client) WithBaseURL(baseURL string) *Client {
	return &Client{
		Token:      c.Token,
		BaseURL:    strings.TrimSuffix(baseURL, "/"),
		Owner:      c.Owner,
		Repo:       c.Repo,
		HTTPClient: c.HTTPClient,
		Retry:      c.Retry,
	}
}

// repoPath returns the /repos/{owner}/{repo} path prefix for API calls.
func (c *Client) repoPath() string {
	return "/repos/" + c.Owner + "/" + c.Repo
}

type githubResponseAction struct {
	body           []byte
	headers        http.Header
	err            error
	rateLimit      *RateLimitError
	retry          bool
	immediateRetry bool
	retryDelay     time.Duration
}

type githubAttemptOutcome struct {
	body        []byte
	headers     http.Header
	terminalErr error
	retry       bool
	retryDelay  time.Duration
}

func marshalGitHubRequestBody(body interface{}) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}
	return jsonBody, nil
}

func (c *Client) newGitHubRequest(ctx context.Context, method, urlStr string, jsonBody []byte) (*http.Request, error) {
	var reqBody io.Reader
	if jsonBody != nil {
		reqBody = bytes.NewReader(jsonBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set(headerAccept, "application/vnd.github+json")
	req.Header.Set(headerAPIVersion, "2022-11-28")
	if jsonBody != nil {
		req.Header.Set(headerContentType, "application/json")
	}
	return req, nil
}

func readGitHubResponse(resp *http.Response) ([]byte, error) {
	const maxResponseSize = 50 * 1024 * 1024
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	return respBody, nil
}

func (c *Client) evaluateGitHubResponse(resp *http.Response, respBody []byte, urlStr string, attempt int, retry RetryConfig) githubResponseAction {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return githubResponseAction{body: respBody, headers: resp.Header}
	}

	if rlErr := classifyGitHubRateLimit(resp, respBody, urlStr); rlErr != nil {
		return githubResponseAction{
			rateLimit:  rlErr,
			retry:      attempt < retry.MaxRetries,
			retryDelay: computeRetryDelay(rlErr, attempt, retry),
		}
	}
	if resp.StatusCode == http.StatusForbidden {
		return githubResponseAction{err: &AuthError{
			StatusCode: resp.StatusCode,
			Message:    extractGitHubMessage(respBody),
			URL:        urlStr,
		}}
	}
	if isGitHubTransientStatus(resp.StatusCode) {
		delay := exponentialBackoff(retry.BaseDelay, attempt, retry.MaxBackoff)
		if half := int64(delay / 2); half > 0 {
			delay += time.Duration(rand.Int64N(half)) //nolint:gosec // jitter does not need crypto rand
		}
		return githubResponseAction{
			err:        fmt.Errorf("transient error %d (attempt %d/%d): %s", resp.StatusCode, attempt+1, retry.MaxRetries+1, extractGitHubMessage(respBody)),
			retry:      true,
			retryDelay: delay,
		}
	}
	return githubResponseAction{err: fmt.Errorf("API error: %s (status %d)", string(respBody), resp.StatusCode)}
}

func classifyGitHubRateLimit(resp *http.Response, body []byte, urlStr string) *RateLimitError {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	return classifyRateLimit(resp.Header, body, resp.StatusCode, urlStr)
}

func isGitHubTransientStatus(status int) bool {
	switch status {
	case http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func (c *Client) performGitHubRequest(ctx context.Context, method, urlStr string, jsonBody []byte, attempt int, retry RetryConfig) githubResponseAction {
	req, err := c.newGitHubRequest(ctx, method, urlStr, jsonBody)
	if err != nil {
		return githubResponseAction{err: err}
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return githubResponseAction{
			err:            fmt.Errorf("request failed (attempt %d/%d): %w", attempt+1, retry.MaxRetries+1, err),
			retry:          true,
			immediateRetry: true,
		}
	}

	respBody, readErr := readGitHubResponse(resp)
	if readErr != nil {
		return githubResponseAction{
			err:            fmt.Errorf("failed to read response (attempt %d/%d): %w", attempt+1, retry.MaxRetries+1, readErr),
			retry:          true,
			immediateRetry: true,
		}
	}
	return c.evaluateGitHubResponse(resp, respBody, urlStr, attempt, retry)
}

func handleGitHubAttempt(action githubResponseAction, lastErr *error, lastRateLimit **RateLimitError) githubAttemptOutcome {
	if action.body != nil || action.headers != nil {
		return githubAttemptOutcome{body: action.body, headers: action.headers}
	}
	if action.rateLimit != nil {
		*lastRateLimit = action.rateLimit
		*lastErr = action.rateLimit
		if action.retry {
			return githubAttemptOutcome{retry: true, retryDelay: action.retryDelay}
		}
		return githubAttemptOutcome{}
	}
	if action.err == nil {
		return githubAttemptOutcome{}
	}
	if !action.retry {
		return githubAttemptOutcome{terminalErr: action.err}
	}
	*lastErr = action.err
	if action.immediateRetry {
		return githubAttemptOutcome{}
	}
	return githubAttemptOutcome{retry: true, retryDelay: action.retryDelay}
}

func (c *Client) doRequest(ctx context.Context, method, urlStr string, body interface{}) ([]byte, http.Header, error) {
	jsonBody, err := marshalGitHubRequestBody(body)
	if err != nil {
		return nil, nil, err
	}

	retry := c.Retry
	var lastErr error
	var lastRateLimit *RateLimitError

	for attempt := 0; attempt <= retry.MaxRetries; attempt++ {
		action := c.performGitHubRequest(ctx, method, urlStr, jsonBody, attempt, retry)
		outcome := handleGitHubAttempt(action, &lastErr, &lastRateLimit)
		if outcome.body != nil || outcome.headers != nil {
			return outcome.body, outcome.headers, nil
		}
		if outcome.terminalErr != nil {
			return nil, nil, outcome.terminalErr
		}
		if outcome.retry {
			if err := sleep(ctx, outcome.retryDelay); err != nil {
				return nil, nil, err
			}
		}
	}

	if lastRateLimit != nil {
		return nil, nil, lastRateLimit
	}
	return nil, nil, fmt.Errorf("max retries (%d) exceeded: %w", retry.MaxRetries+1, lastErr)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func computeRetryDelay(rlErr *RateLimitError, attempt int, retry RetryConfig) time.Duration {
	if rlErr.RetryAfter > 0 {
		return rlErr.RetryAfter
	}
	exp := exponentialBackoff(retry.BaseDelay, attempt, retry.MaxBackoff)
	switch rlErr.Kind {
	case RateLimitPrimary:
		if !rlErr.ResetAt.IsZero() {
			if d := time.Until(rlErr.ResetAt); d > 0 {
				return d
			}
		}
	case RateLimitSecondary:
		if exp < retry.SecondaryMinDelay {
			return retry.SecondaryMinDelay
		}
	}
	return exp
}

func exponentialBackoff(base time.Duration, attempt int, maxBackoff time.Duration) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if attempt > 30 {
		attempt = 30 // guard against int shift overflow
	}
	d := base * time.Duration(1<<attempt)
	if maxBackoff > 0 && d > maxBackoff {
		return maxBackoff
	}
	return d
}

// nextPageURL extracts the next page URL from GitHub's Link header.
// Returns empty string if there is no next page.
func nextPageURL(headers http.Header) string {
	link := headers.Get("Link")
	if link == "" {
		return ""
	}
	matches := linkNextPattern.FindStringSubmatch(link)
	if len(matches) < 2 {
		return ""
	}
	return matches[1]
}

// FetchIssues retrieves issues from GitHub with optional filtering by state.
// state can be: "open", "closed", or "all".
// Filters out pull requests (GitHub's Issues API includes PRs).
func (c *Client) FetchIssues(ctx context.Context, state string) ([]Issue, error) {
	var allIssues []Issue
	page := 1

	for {
		select {
		case <-ctx.Done():
			return allIssues, ctx.Err()
		default:
		}

		urlStr := fmt.Sprintf("%s%s/issues?per_page=%d&page=%d&state=%s&direction=asc",
			c.BaseURL, c.repoPath(), MaxPerPage, page, state)

		respBody, headers, err := c.doRequest(ctx, http.MethodGet, urlStr, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch issues: %w", err)
		}

		var issues []Issue
		if err := json.Unmarshal(respBody, &issues); err != nil {
			return nil, fmt.Errorf("failed to parse issues response: %w", err)
		}

		// Filter out pull requests
		for i := range issues {
			if !issues[i].IsPullRequest() {
				allIssues = append(allIssues, issues[i])
			}
		}

		// Check for next page
		if nextPageURL(headers) == "" {
			break
		}
		page++

		if page > MaxPages {
			return nil, fmt.Errorf("pagination limit exceeded: stopped after %d pages", MaxPages)
		}
	}

	return allIssues, nil
}

// FetchIssuesSince retrieves issues from GitHub that have been updated since the given time.
// This enables incremental sync by only fetching issues modified after the last sync.
func (c *Client) FetchIssuesSince(ctx context.Context, state string, since time.Time) ([]Issue, error) {
	var allIssues []Issue
	page := 1

	sinceStr := since.UTC().Format(time.RFC3339)

	for {
		select {
		case <-ctx.Done():
			return allIssues, ctx.Err()
		default:
		}

		urlStr := fmt.Sprintf("%s%s/issues?per_page=%d&page=%d&state=%s&since=%s&direction=asc",
			c.BaseURL, c.repoPath(), MaxPerPage, page, state, sinceStr)

		respBody, headers, err := c.doRequest(ctx, http.MethodGet, urlStr, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch issues since %s: %w", sinceStr, err)
		}

		var issues []Issue
		if err := json.Unmarshal(respBody, &issues); err != nil {
			return nil, fmt.Errorf("failed to parse issues response: %w", err)
		}

		// Filter out pull requests
		for i := range issues {
			if !issues[i].IsPullRequest() {
				allIssues = append(allIssues, issues[i])
			}
		}

		if nextPageURL(headers) == "" {
			break
		}
		page++

		if page > MaxPages {
			return nil, fmt.Errorf("pagination limit exceeded: stopped after %d pages", MaxPages)
		}
	}

	return allIssues, nil
}

// CreateIssue creates a new issue in GitHub.
func (c *Client) CreateIssue(ctx context.Context, title, body string, labels []string) (*Issue, error) {
	reqBody := map[string]interface{}{
		"title": title,
		"body":  body,
	}
	if len(labels) > 0 {
		reqBody["labels"] = labels
	}

	urlStr := fmt.Sprintf("%s%s/issues", c.BaseURL, c.repoPath())
	respBody, _, err := c.doRequest(ctx, http.MethodPost, urlStr, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create issue: %w", err)
	}

	var issue Issue
	if err := json.Unmarshal(respBody, &issue); err != nil {
		return nil, fmt.Errorf("failed to parse create response: %w", err)
	}

	return &issue, nil
}

// UpdateIssue updates an existing issue in GitHub.
func (c *Client) UpdateIssue(ctx context.Context, number int, updates map[string]interface{}) (*Issue, error) {
	urlStr := fmt.Sprintf("%s%s/issues/%d", c.BaseURL, c.repoPath(), number)
	respBody, _, err := c.doRequest(ctx, http.MethodPatch, urlStr, updates)
	if err != nil {
		return nil, fmt.Errorf("failed to update issue: %w", err)
	}

	var issue Issue
	if err := json.Unmarshal(respBody, &issue); err != nil {
		return nil, fmt.Errorf("failed to parse update response: %w", err)
	}

	return &issue, nil
}

// FetchIssueByNumber retrieves a single issue by its repository-scoped number.
func (c *Client) FetchIssueByNumber(ctx context.Context, number int) (*Issue, error) {
	urlStr := fmt.Sprintf("%s%s/issues/%d", c.BaseURL, c.repoPath(), number)
	respBody, _, err := c.doRequest(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch issue #%d: %w", number, err)
	}

	var issue Issue
	if err := json.Unmarshal(respBody, &issue); err != nil {
		return nil, fmt.Errorf("failed to parse issue response: %w", err)
	}

	return &issue, nil
}

// ListRepositories retrieves repositories accessible to the authenticated user.
func (c *Client) ListRepositories(ctx context.Context) ([]Repository, error) {
	var allRepos []Repository
	page := 1

	for {
		select {
		case <-ctx.Done():
			return allRepos, ctx.Err()
		default:
		}

		urlStr := fmt.Sprintf("%s/user/repos?per_page=%d&page=%d&sort=updated", c.BaseURL, MaxPerPage, page)
		respBody, headers, err := c.doRequest(ctx, http.MethodGet, urlStr, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to list repositories: %w", err)
		}

		var repos []Repository
		if err := json.Unmarshal(respBody, &repos); err != nil {
			return nil, fmt.Errorf("failed to parse repositories response: %w", err)
		}

		allRepos = append(allRepos, repos...)

		if nextPageURL(headers) == "" {
			break
		}
		page++

		if page > MaxPages {
			break
		}
	}

	return allRepos, nil
}

// AddLabels adds labels to an existing issue.
func (c *Client) AddLabels(ctx context.Context, number int, labels []string) error {
	urlStr := fmt.Sprintf("%s%s/issues/%d/labels", c.BaseURL, c.repoPath(), number)
	body := map[string]interface{}{
		"labels": labels,
	}
	_, _, err := c.doRequest(ctx, http.MethodPost, urlStr, body)
	if err != nil {
		return fmt.Errorf("failed to add labels to issue #%d: %w", number, err)
	}
	return nil
}

// RemoveLabel removes a label from an existing issue.
func (c *Client) RemoveLabel(ctx context.Context, number int, label string) error {
	urlStr := fmt.Sprintf("%s%s/issues/%d/labels/%s", c.BaseURL, c.repoPath(), number, label)
	_, _, err := c.doRequest(ctx, http.MethodDelete, urlStr, nil)
	if err != nil {
		return fmt.Errorf("failed to remove label %q from issue #%d: %w", label, number, err)
	}
	return nil
}

// GetAuthenticatedUser returns the authenticated user's information.
func (c *Client) GetAuthenticatedUser(ctx context.Context) (*User, error) {
	urlStr := c.BaseURL + "/user"
	respBody, _, err := c.doRequest(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get authenticated user: %w", err)
	}

	var user User
	if err := json.Unmarshal(respBody, &user); err != nil {
		return nil, fmt.Errorf("failed to parse user response: %w", err)
	}

	return &user, nil
}
