package ado

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// APIError represents an HTTP error response from the Azure DevOps API.
// It carries the HTTP status code so callers can use errors.As to inspect
// the status without fragile string matching.
type APIError struct {
	// StatusCode is the HTTP status code returned by the API.
	StatusCode int
	// Body is the response body text.
	Body string
}

// Error implements the error interface.
func (e *APIError) Error() string {
	return fmt.Sprintf("API error: %s (status %d)", e.Body, e.StatusCode)
}

// PullFilters configures which work items to pull from ADO.
// All filter values are validated before use in WIQL queries.
type PullFilters struct {
	AreaPath      string   // Filter to area path (uses UNDER for hierarchy), validated
	IterationPath string   // Filter to iteration path (uses UNDER for hierarchy), validated
	WorkItemTypes []string // Filter to specific work item types, validated
	States        []string // Filter to specific states, validated
}

var (
	// areaPathPattern validates ADO area/iteration path values.
	areaPathPattern = regexp.MustCompile(`^[a-zA-Z0-9 ._\\/-]+$`)
	// statePattern validates ADO state names.
	statePattern = regexp.MustCompile(`^[a-zA-Z0-9 _]+$`)
	// orgPattern validates ADO organization names.
	orgPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
	// projectPattern validates ADO project names (allows spaces, quotes, etc.).
	projectPattern = regexp.MustCompile(`^[a-zA-Z0-9 ._'-]+$`)
)

// Validate checks all filter values against their allowlist patterns.
func (f *PullFilters) Validate() error {
	if f.AreaPath != "" && !areaPathPattern.MatchString(f.AreaPath) {
		return fmt.Errorf("invalid area path: %q (must match %s)", f.AreaPath, areaPathPattern.String())
	}
	if f.IterationPath != "" && !areaPathPattern.MatchString(f.IterationPath) {
		return fmt.Errorf("invalid iteration path: %q", f.IterationPath)
	}
	for _, t := range f.WorkItemTypes {
		if !areaPathPattern.MatchString(t) {
			return fmt.Errorf("invalid work item type: %q", t)
		}
	}
	for _, s := range f.States {
		if !statePattern.MatchString(s) {
			return fmt.Errorf("invalid state filter: %q", s)
		}
	}
	return nil
}

// ValidateOrg checks the organization name against the allowlist pattern.
func ValidateOrg(org string) error {
	if !orgPattern.MatchString(org) {
		return fmt.Errorf("invalid organization name: must match %s", orgPattern.String())
	}
	return nil
}

// ValidateProject checks the project name against the allowlist pattern.
func ValidateProject(project string) error {
	if !projectPattern.MatchString(project) {
		return fmt.Errorf("invalid project name: must match %s", projectPattern.String())
	}
	return nil
}

// NewClient creates a new Azure DevOps REST API client for the given organization
// and project, authenticating with the provided Personal Access Token. The returned
// client uses DefaultTimeout for HTTP requests and DefaultBaseURL for the API endpoint.
func NewClient(pat SecretString, org, project string) *Client {
	return &Client{
		PAT:     pat,
		Org:     org,
		Project: project,
		HTTPClient: &http.Client{
			Timeout: DefaultTimeout,
		},
	}
}

// WithHTTPClient returns a copy of the client configured to use the specified HTTP client.
func (c *Client) WithHTTPClient(httpClient *http.Client) *Client {
	return &Client{
		PAT:        c.PAT,
		BaseURL:    c.BaseURL,
		Org:        c.Org,
		Project:    c.Project,
		HTTPClient: httpClient,
	}
}

// validateURLScheme rejects non-HTTPS URLs unless the host is localhost or 127.0.0.1.
func validateURLScheme(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if u.Scheme == "http" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		return fmt.Errorf("HTTPS required for ADO connections (got %s); use https:// or localhost for testing", rawURL)
	}
	return nil
}

// WithBaseURL returns a copy of the client configured to use a custom API base URL.
// This is useful for on-prem Azure DevOps Server or testing with mock servers.
// Returns an error if the URL uses plain HTTP for non-localhost hosts.
func (c *Client) WithBaseURL(baseURL string) (*Client, error) {
	if err := validateURLScheme(baseURL); err != nil {
		return nil, err
	}
	return &Client{
		PAT:        c.PAT,
		BaseURL:    strings.TrimSuffix(baseURL, "/"),
		Org:        c.Org,
		Project:    c.Project,
		HTTPClient: c.HTTPClient,
	}, nil
}

// apiBase returns the project-scoped _apis URL prefix.
func (c *Client) apiBase() string {
	if c.BaseURL != "" {
		return c.BaseURL + "/" + url.PathEscape(c.Project) + "/_apis"
	}
	return DefaultBaseURL + "/" + url.PathEscape(c.Org) + "/" + url.PathEscape(c.Project) + "/_apis"
}

// orgBase returns the org-level _apis URL prefix (not project-scoped).
func (c *Client) orgBase() string {
	if c.BaseURL != "" {
		return c.BaseURL + "/_apis"
	}
	return DefaultBaseURL + "/" + url.PathEscape(c.Org) + "/_apis"
}

// doRequest performs an HTTP request with authentication and retry logic.
// contentType controls the Content-Type header; pass empty string for GET requests.
// isIdempotent reports whether the given request is safe to retry.
// GET requests are always safe. POST to the WIQL endpoint is a read-only
// query and is also safe. Mutations (POST/PATCH to other endpoints) are
// NOT safe to retry because the server may have already applied them.
func isIdempotent(method, urlStr string) bool {
	if method == http.MethodGet {
		return true
	}
	if method == http.MethodPost && strings.Contains(urlStr, "/wit/wiql") {
		return true
	}
	return false
}

type adoRequestPlan struct {
	method      string
	url         string
	contentType string
	body        []byte
	credential  string
	maxAttempts int
}

func (c *Client) doRequest(ctx context.Context, method, urlStr, contentType string, body interface{}) ([]byte, error) {
	plan, err := c.prepareRequest(method, urlStr, contentType, body)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt <= plan.maxAttempts; attempt++ {
		respBody, resp, err := c.executeRequest(ctx, plan)
		if err != nil {
			var waitErr error
			lastErr, waitErr = handleADOExecutionError(ctx, resp, err, attempt, plan.maxAttempts)
			if waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		result, resultErr, retry := evaluateADOResponse(resp, respBody, attempt, plan.maxAttempts)
		if !retry {
			return result, resultErr
		}
		lastErr = resultErr
		if waitErr := waitADOResponseRetry(ctx, resp, attempt); waitErr != nil {
			return nil, waitErr
		}
	}

	return nil, fmt.Errorf("max retries (%d) exceeded: %w", plan.maxAttempts+1, lastErr)
}

func handleADOExecutionError(ctx context.Context, resp *http.Response, err error, attempt, maxAttempts int) (error, error) {
	if resp == nil {
		lastErr := fmt.Errorf("request failed (attempt %d/%d): %w", attempt+1, maxAttempts+1, err)
		return lastErr, waitADORequestRetry(ctx, attempt, maxAttempts)
	}
	return fmt.Errorf("failed to read response (attempt %d/%d): %w", attempt+1, maxAttempts+1, err), nil
}

func evaluateADOResponse(resp *http.Response, body []byte, attempt, maxAttempts int) ([]byte, error, bool) {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil, false
	}
	if isPermanentADOStatus(resp.StatusCode) {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(body)}, false
	}
	if isRetriableADOStatus(resp.StatusCode) && attempt < maxAttempts {
		return nil, fmt.Errorf("transient error %d (attempt %d/%d)", resp.StatusCode, attempt+1, maxAttempts+1), true
	}
	return nil, &APIError{StatusCode: resp.StatusCode, Body: string(body)}, false
}

func (c *Client) prepareRequest(method, urlStr, contentType string, body interface{}) (adoRequestPlan, error) {
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return adoRequestPlan{}, fmt.Errorf("failed to marshal request body: %w", err)
		}
	}

	maxAttempts := 0
	if isIdempotent(method, urlStr) {
		maxAttempts = MaxRetries
	}
	return adoRequestPlan{
		method:      method,
		url:         urlStr,
		contentType: contentType,
		body:        bodyBytes,
		credential:  base64.StdEncoding.EncodeToString([]byte(":" + c.PAT.Expose())),
		maxAttempts: maxAttempts,
	}, nil
}

func (c *Client) executeRequest(ctx context.Context, plan adoRequestPlan) ([]byte, *http.Response, error) {
	var reqBody io.Reader
	if plan.body != nil {
		reqBody = bytes.NewReader(plan.body)
	}
	req, err := http.NewRequestWithContext(ctx, plan.method, plan.url, reqBody)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+plan.credential)
	if plan.contentType != "" {
		req.Header.Set("Content-Type", plan.contentType)
	}

	resp, err := c.HTTPClient.Do(req) //nolint:gosec // G704: URL is from admin-configured ADO endpoint, not untrusted input
	if err != nil {
		return nil, nil, err
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseSize))
	_ = resp.Body.Close()
	if err != nil {
		return nil, resp, err
	}
	return respBody, resp, nil
}

func isPermanentADOStatus(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	default:
		return false
	}
}

func isRetriableADOStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

func waitADORequestRetry(ctx context.Context, attempt, maxAttempts int) error {
	if attempt >= maxAttempts {
		return nil
	}
	delay := RetryDelay * time.Duration(1<<uint(attempt))
	if half := int64(delay / 2); half > 0 {
		delay += time.Duration(rand.Int64N(half)) //nolint:gosec // G404: jitter for retry backoff does not need crypto rand
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

func waitADOResponseRetry(ctx context.Context, resp *http.Response, attempt int) error {
	delay := RetryDelay * time.Duration(1<<uint(attempt))
	useServerDelay := false
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
		if seconds, parseErr := strconv.Atoi(retryAfter); parseErr == nil {
			delay = time.Duration(seconds) * time.Second
			useServerDelay = true
		}
	}
	if !useServerDelay {
		if half := int64(delay / 2); half > 0 {
			delay += time.Duration(rand.Int64N(half)) //nolint:gosec // G404: jitter for retry backoff does not need crypto rand
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

// addAPIVersion appends the api-version query parameter to a URL string.
func addAPIVersion(urlStr string) string {
	if strings.Contains(urlStr, "?") {
		return urlStr + "&api-version=" + APIVersion
	}
	return urlStr + "?api-version=" + APIVersion
}

// listResponse is a generic envelope for ADO list API responses.
type listResponse struct {
	Count int             `json:"count"`
	Value json.RawMessage `json:"value"`
}

// escapeWIQL escapes a string for safe inclusion in a WIQL query literal.
func escapeWIQL(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	return strings.ReplaceAll(s, "'", "''")
}

// formatWIQLDate formats a time.Time for use in WIQL datetime literals.
// Azure DevOps date-precision fields (e.g. System.ChangedDate) reject any
// time component, so we output date-only format: 'YYYY-MM-DD'.
// The time is converted to UTC before truncating to date.
func formatWIQLDate(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// buildPatchOps converts a field map into sorted JSON Patch operations.
func buildPatchOps(fields map[string]interface{}) []PatchOperation {
	var ops []PatchOperation
	for field, value := range fields {
		ops = append(ops, PatchOperation{
			Op:    "add",
			Path:  "/fields/" + field,
			Value: value,
		})
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].Path < ops[j].Path })
	return ops
}
