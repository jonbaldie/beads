package jira

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
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/debug"
)

// Issue represents a Jira issue from the REST API.
type Issue struct {
	ID     string      `json:"id"`
	Key    string      `json:"key"`
	Self   string      `json:"self"`
	Fields IssueFields `json:"fields"`
}

// IssueFields contains the fields of a Jira issue.
type IssueFields struct {
	Summary     string           `json:"summary"`
	Description json.RawMessage  `json:"description"` // ADF (Atlassian Document Format) or plain text
	Status      *StatusField     `json:"status"`
	Priority    *PriorityField   `json:"priority"`
	IssueType   *IssueTypeField  `json:"issuetype"`
	Project     *ProjectField    `json:"project"`
	Assignee    *UserField       `json:"assignee"`
	Labels      []string         `json:"labels"`
	Created     string           `json:"created"`
	Updated     string           `json:"updated"`
	Resolution  *ResolutionField `json:"resolution"`
}

// StatusField represents a Jira issue status.
type StatusField struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// PriorityField represents a Jira issue priority.
type PriorityField struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// IssueTypeField represents a Jira issue type.
type IssueTypeField struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ProjectField represents a Jira project.
type ProjectField struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

// UserField represents a Jira user.
type UserField struct {
	AccountID    string `json:"accountId"`
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
}

// ResolutionField represents a Jira resolution.
type ResolutionField struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Transition represents a Jira workflow transition.
type Transition struct {
	ID   string      `json:"id"`
	Name string      `json:"name"`
	To   StatusField `json:"to"`
}

// TransitionsResult is the response from GET /issue/{key}/transitions.
type TransitionsResult struct {
	Transitions []Transition `json:"transitions"`
}

// SearchResult represents a Jira JQL search response.
type SearchResult struct {
	StartAt       int     `json:"startAt"`
	MaxResults    int     `json:"maxResults"`
	Total         int     `json:"total"`
	NextPageToken string  `json:"nextPageToken"`
	IsLast        bool    `json:"isLast"`
	Issues        []Issue `json:"issues"`
}

// NewClient creates a new Jira client.
func NewClient(url, username, apiToken string) *Client {
	return &Client{
		URL:        strings.TrimSuffix(url, "/"),
		Username:   username,
		APIToken:   apiToken,
		APIVersion: "3",
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// apiBase returns the versioned REST API base URL, e.g. "https://host/rest/api/3".
func (c *Client) apiBase() string {
	v := c.APIVersion
	if v == "" {
		v = "3"
	}
	return c.URL + "/rest/api/" + v
}

// FetchIssueTimestamp fetches the updated timestamp for a single Jira issue.
func (c *Client) FetchIssueTimestamp(ctx context.Context, jiraKey string) (time.Time, error) {
	var zero time.Time

	apiURL := fmt.Sprintf("%s/issue/%s?fields=updated", c.apiBase(), url.PathEscape(jiraKey))

	body, err := c.doRequest(ctx, "GET", apiURL, nil)
	if err != nil {
		return zero, fmt.Errorf("fetch issue %s: %w", jiraKey, err)
	}

	var result struct {
		Fields struct {
			Updated string `json:"updated"`
		} `json:"fields"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return zero, fmt.Errorf("parse Jira response: %w", err)
	}

	updated, err := ParseTimestamp(result.Fields.Updated)
	if err != nil {
		return zero, fmt.Errorf("parse Jira timestamp: %w", err)
	}

	return updated, nil
}

// searchFields is the default set of fields to request in search/get queries.
const searchFields = "summary,description,status,priority,issuetype,project,assignee,labels,created,updated,resolution"

// SearchIssues queries Jira using JQL and returns all matching issues, handling pagination.
func (c *Client) SearchIssues(ctx context.Context, jql string) ([]Issue, error) {
	var allIssues []Issue
	startAt := 0
	nextPageToken := ""
	maxResults := 100
	page := 0
	useV2Pagination := c.APIVersion == "2"

	for {
		if err := ctx.Err(); err != nil {
			return allIssues, err
		}

		page++
		if page > MaxPages {
			return nil, fmt.Errorf("pagination limit exceeded: stopped after %d pages", MaxPages)
		}

		apiURL := c.searchURL(jql, useV2Pagination, startAt, nextPageToken, maxResults)

		body, err := c.doRequest(ctx, "GET", apiURL, nil)
		if err != nil {
			return nil, fmt.Errorf("search issues: %w", err)
		}

		var result SearchResult
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("parse search response: %w", err)
		}

		allIssues = append(allIssues, result.Issues...)

		if searchPageDone(result, useV2Pagination, startAt) {
			break
		}
		startAt, nextPageToken = advanceSearchCursor(result, useV2Pagination, startAt)
	}

	return allIssues, nil
}

func (c *Client) searchURL(jql string, useV2Pagination bool, startAt int, nextPageToken string, maxResults int) string {
	params := url.Values{
		"jql":        {jql},
		"fields":     {searchFields},
		"maxResults": {fmt.Sprintf("%d", maxResults)},
	}
	searchPath := "search/jql"
	if useV2Pagination {
		params.Set("startAt", fmt.Sprintf("%d", startAt))
		searchPath = "search"
	} else if nextPageToken != "" {
		params.Set("nextPageToken", nextPageToken)
	}
	return fmt.Sprintf("%s/%s?%s", c.apiBase(), searchPath, params.Encode())
}

func searchPageDone(result SearchResult, useV2Pagination bool, startAt int) bool {
	if len(result.Issues) == 0 {
		return true
	}
	if useV2Pagination {
		return startAt+len(result.Issues) >= result.Total
	}
	return result.IsLast || result.NextPageToken == ""
}

func advanceSearchCursor(result SearchResult, useV2Pagination bool, startAt int) (int, string) {
	if useV2Pagination {
		return startAt + len(result.Issues), ""
	}
	return startAt, result.NextPageToken
}

// GetIssue fetches a single Jira issue by key (e.g., "PROJ-123").
func (c *Client) GetIssue(ctx context.Context, key string) (*Issue, error) {
	apiURL := fmt.Sprintf("%s/issue/%s?fields=%s", c.apiBase(), url.PathEscape(key), searchFields)

	body, err := c.doRequest(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("get issue %s: %w", key, err)
	}

	var issue Issue
	if err := json.Unmarshal(body, &issue); err != nil {
		return nil, fmt.Errorf("parse issue response: %w", err)
	}

	return &issue, nil
}

// CreateIssue creates a new issue in Jira.
// fields should include "project", "summary", "issuetype", and optionally other fields.
func (c *Client) CreateIssue(ctx context.Context, fields map[string]interface{}) (*Issue, error) {
	payload := map[string]interface{}{"fields": fields}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal create request: %w", err)
	}

	apiURL := fmt.Sprintf("%s/issue", c.apiBase())

	body, err := c.doRequest(ctx, "POST", apiURL, data)
	if err != nil {
		return nil, fmt.Errorf("create issue: %w", err)
	}

	// Create response only returns id, key, self. Fetch the full issue.
	var created struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Self string `json:"self"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		return nil, fmt.Errorf("parse create response: %w", err)
	}

	return c.GetIssue(ctx, created.Key)
}

// UpdateIssue updates an existing Jira issue by key.
func (c *Client) UpdateIssue(ctx context.Context, key string, fields map[string]interface{}) error {
	payload := map[string]interface{}{"fields": fields}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal update request: %w", err)
	}

	apiURL := fmt.Sprintf("%s/issue/%s", c.apiBase(), url.PathEscape(key))

	_, err = c.doRequest(ctx, "PUT", apiURL, data)
	if err != nil {
		return fmt.Errorf("update issue %s: %w", key, err)
	}

	return nil
}

// GetIssueTransitions fetches the available workflow transitions for a Jira issue.
func (c *Client) GetIssueTransitions(ctx context.Context, key string) ([]Transition, error) {
	apiURL := fmt.Sprintf("%s/issue/%s/transitions", c.apiBase(), url.PathEscape(key))

	body, err := c.doRequest(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("get transitions for %s: %w", key, err)
	}

	var result TransitionsResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse transitions response: %w", err)
	}

	return result.Transitions, nil
}

// TransitionIssue moves a Jira issue to a new status using the given transition ID.
func (c *Client) TransitionIssue(ctx context.Context, key, transitionID string) error {
	payload := map[string]interface{}{
		"transition": map[string]string{"id": transitionID},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal transition request: %w", err)
	}

	apiURL := fmt.Sprintf("%s/issue/%s/transitions", c.apiBase(), url.PathEscape(key))

	_, err = c.doRequest(ctx, "POST", apiURL, data)
	if err != nil {
		return fmt.Errorf("transition issue %s: %w", key, err)
	}

	return nil
}

// doRequest executes an authenticated HTTP request and returns the response body.
func (c *Client) doRequest(ctx context.Context, method, apiURL string, body []byte) ([]byte, error) {
	debug.Logf("jira: %s %s\n", method, apiURL)

	if c.URL == "" {
		return nil, fmt.Errorf("jira URL not configured")
	}
	if c.APIToken == "" {
		return nil, fmt.Errorf("jira API token not configured")
	}

	var lastErr error
	for attempt := 0; attempt <= MaxRetries; attempt++ {
		attemptResult := c.executeRequest(ctx, method, apiURL, body)
		if attemptResult.err != nil {
			if attemptResult.terminal {
				return nil, attemptResult.err
			}
			lastErr = formatJiraAttemptError(attemptResult.response, attemptResult.err, attempt)
			continue
		}

		result, resultErr, retry := evaluateJiraResponse(attemptResult.response, attemptResult.body, attempt)
		if !retry {
			return result, resultErr
		}
		lastErr = resultErr
		if err := waitJiraResponseRetry(ctx, attemptResult.response, attempt); err != nil {
			return nil, err
		}
	}

	return nil, fmt.Errorf("max retries (%d) exceeded: %w", MaxRetries+1, lastErr)
}

type jiraAttemptResult struct {
	body     []byte
	response *http.Response
	err      error
	terminal bool
}

func (c *Client) executeRequest(ctx context.Context, method, apiURL string, body []byte) jiraAttemptResult {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiURL, bodyReader)
	if err != nil {
		return jiraAttemptResult{err: fmt.Errorf("create request: %w", err), terminal: true}
	}
	c.setAuth(req)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "bd-jira-sync/1.0")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return jiraAttemptResult{err: err}
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseSize))
	_ = resp.Body.Close()
	if err != nil {
		return jiraAttemptResult{response: resp, err: err}
	}
	return jiraAttemptResult{body: respBody, response: resp}
}

func formatJiraAttemptError(resp *http.Response, err error, attempt int) error {
	if resp == nil {
		return fmt.Errorf("request failed (attempt %d/%d): %w", attempt+1, MaxRetries+1, err)
	}
	return fmt.Errorf("failed to read response (attempt %d/%d): %w", attempt+1, MaxRetries+1, err)
}

func evaluateJiraResponse(resp *http.Response, body []byte, attempt int) ([]byte, error, bool) {
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil, false
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil, false
	}
	if isPermanentJiraStatus(resp.StatusCode) {
		return nil, fmt.Errorf("jira API returned %d: %s", resp.StatusCode, string(body)), false
	}
	if isRetriableJiraStatus(resp.StatusCode) {
		return nil, fmt.Errorf("transient error %d (attempt %d/%d)", resp.StatusCode, attempt+1, MaxRetries+1), true
	}
	return nil, fmt.Errorf("jira API returned %d: %s", resp.StatusCode, string(body)), false
}

func isPermanentJiraStatus(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	default:
		return false
	}
}

func isRetriableJiraStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func waitJiraResponseRetry(ctx context.Context, resp *http.Response, attempt int) error {
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

// setAuth sets the appropriate authentication header on the request.
func (c *Client) setAuth(req *http.Request) {
	isCloud := strings.Contains(c.URL, "atlassian.net")
	if (isCloud || c.Username != "") && c.Username != "" {
		auth := base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.APIToken))
		req.Header.Set("Authorization", "Basic "+auth)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.APIToken)
	}
}

// DescriptionToPlainText extracts plain text from Jira's ADF (Atlassian Document Format).
// Jira v3 API returns descriptions as ADF JSON, not plain text.
func DescriptionToPlainText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}

	doc, err := parseDescriptionDocument(raw)
	if err != nil || doc.Type != "doc" {
		return descriptionFallback(raw)
	}
	return adfToPlainText(doc)
}

type jiraDescriptionDocument struct {
	Type    string                 `json:"type"`
	Content []jiraDescriptionBlock `json:"content"`
}

type jiraDescriptionBlock struct {
	Type    string                  `json:"type"`
	Content []jiraDescriptionInline `json:"content"`
}

type jiraDescriptionInline struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func parseDescriptionDocument(raw json.RawMessage) (jiraDescriptionDocument, error) {
	var doc jiraDescriptionDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return jiraDescriptionDocument{}, err
	}
	return doc, nil
}

func descriptionFallback(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

func adfToPlainText(doc jiraDescriptionDocument) string {
	var parts []string
	for _, block := range doc.Content {
		var line []string
		for _, inline := range block.Content {
			if inline.Text != "" {
				line = append(line, inline.Text)
			}
		}
		if len(line) > 0 {
			parts = append(parts, strings.Join(line, ""))
		}
	}

	return strings.Join(parts, "\n")
}

// PlainTextToADF converts plain text to Jira's ADF (Atlassian Document Format).
func PlainTextToADF(text string) json.RawMessage {
	if text == "" {
		return nil
	}

	paragraphs := strings.Split(text, "\n")
	var content []interface{}
	for _, para := range paragraphs {
		if para == "" {
			content = append(content, map[string]interface{}{
				"type":    "paragraph",
				"content": []interface{}{},
			})
			continue
		}
		content = append(content, map[string]interface{}{
			"type": "paragraph",
			"content": []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": para,
				},
			},
		})
	}

	doc := map[string]interface{}{
		"type":    "doc",
		"version": 1,
		"content": content,
	}

	data, _ := json.Marshal(doc)
	return data
}
