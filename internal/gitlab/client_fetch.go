// Package gitlab provides client and data types for the GitLab REST API.
package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// FetchIssues retrieves issues from GitLab with optional filtering by state and IssueFilter.
// state can be: "opened", "closed", or "all".
func (c *Client) FetchIssues(ctx context.Context, state string, filters ...*IssueFilter) ([]Issue, error) {
	var filter *IssueFilter
	if len(filters) > 0 {
		filter = filters[0]
	}

	var allIssues []Issue
	page := 1

	for {
		if err := ctx.Err(); err != nil {
			return allIssues, err
		}
		issues, headers, err := c.fetchIssuesPage(ctx, state, filter, page, "")
		if err != nil {
			return nil, err
		}

		allIssues = append(allIssues, issues...)
		next, more, err := nextGitlabPage(headers, page)
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
		page = next
	}

	return filterByProject(allIssues, filter), nil
}

// FetchIssuesSince retrieves issues from GitLab that have been updated since the given time.
// This enables incremental sync by only fetching issues modified after the last sync.
func (c *Client) FetchIssuesSince(ctx context.Context, state string, since time.Time, filters ...*IssueFilter) ([]Issue, error) {
	var filter *IssueFilter
	if len(filters) > 0 {
		filter = filters[0]
	}

	var allIssues []Issue
	page := 1

	sinceStr := since.UTC().Format(time.RFC3339)

	for {
		if err := ctx.Err(); err != nil {
			return allIssues, err
		}
		issues, headers, err := c.fetchIssuesPage(ctx, state, filter, page, sinceStr)
		if err != nil {
			return nil, err
		}

		allIssues = append(allIssues, issues...)
		next, more, err := nextGitlabPage(headers, page)
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
		page = next
	}

	return filterByProject(allIssues, filter), nil
}

func (c *Client) fetchIssuesPage(ctx context.Context, state string, filter *IssueFilter, page int, updatedAfter string) ([]Issue, http.Header, error) {
	params := map[string]string{
		"per_page": strconv.Itoa(MaxPageSize),
		"page":     strconv.Itoa(page),
	}
	if state != "" && state != "all" {
		params["state"] = state
	}
	if updatedAfter != "" {
		params["updated_after"] = updatedAfter
	}
	applyFilter(params, filter)

	urlStr := gitlabBuildURL(c, gitlabIssuesBasePath(c), params)
	respBody, headers, err := gitlabDoRequest(c, ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		if updatedAfter != "" {
			return nil, nil, fmt.Errorf("failed to fetch issues since %s: %w", updatedAfter, err)
		}
		return nil, nil, fmt.Errorf("failed to fetch issues: %w", err)
	}

	var issues []Issue
	if err := json.Unmarshal(respBody, &issues); err != nil {
		return nil, nil, fmt.Errorf("failed to parse issues response: %w", err)
	}
	return issues, headers, nil
}

func nextGitlabPage(headers http.Header, page int) (int, bool, error) {
	if headers.Get("X-Next-Page") == "" {
		return page, false, nil
	}
	next := page + 1
	if next > MaxPages {
		return 0, false, fmt.Errorf("pagination limit exceeded: stopped after %d pages", MaxPages)
	}
	return next, true, nil
}

// CreateIssue creates a new issue in GitLab.
func (c *Client) CreateIssue(ctx context.Context, title, description string, labels []string) (*Issue, error) {
	body := map[string]interface{}{
		"title":       title,
		"description": description,
	}
	if len(labels) > 0 {
		body["labels"] = labels
	}

	urlStr := gitlabBuildURL(c, "/projects/"+c.projectPath()+"/issues", nil)
	respBody, _, err := gitlabDoRequest(c, ctx, http.MethodPost, urlStr, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create issue: %w", err)
	}

	var issue Issue
	if err := json.Unmarshal(respBody, &issue); err != nil {
		return nil, fmt.Errorf("failed to parse create response: %w", err)
	}

	return &issue, nil
}

// UpdateIssue updates an existing issue in GitLab.
func (c *Client) UpdateIssue(ctx context.Context, iid int, updates map[string]interface{}) (*Issue, error) {
	urlStr := gitlabBuildURL(c, "/projects/"+c.projectPath()+"/issues/"+strconv.Itoa(iid), nil)
	respBody, _, err := gitlabDoRequest(c, ctx, http.MethodPut, urlStr, updates)
	if err != nil {
		return nil, fmt.Errorf("failed to update issue: %w", err)
	}

	var issue Issue
	if err := json.Unmarshal(respBody, &issue); err != nil {
		return nil, fmt.Errorf("failed to parse update response: %w", err)
	}

	return &issue, nil
}

// GetIssueLinks retrieves issue links for the specified issue IID.
func (c *Client) GetIssueLinks(ctx context.Context, iid int) ([]IssueLink, error) {
	urlStr := gitlabBuildURL(c, "/projects/"+c.projectPath()+"/issues/"+strconv.Itoa(iid)+"/links", nil)
	respBody, _, err := gitlabDoRequest(c, ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get issue links: %w", err)
	}

	var links []IssueLink
	if err := json.Unmarshal(respBody, &links); err != nil {
		return nil, fmt.Errorf("failed to parse issue links response: %w", err)
	}

	return links, nil
}

// FetchIssueByIID retrieves a single issue by its project-scoped IID.
func (c *Client) FetchIssueByIID(ctx context.Context, iid int) (*Issue, error) {
	urlStr := gitlabBuildURL(c, "/projects/"+c.projectPath()+"/issues/"+strconv.Itoa(iid), nil)
	respBody, _, err := gitlabDoRequest(c, ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch issue %d: %w", iid, err)
	}

	var issue Issue
	if err := json.Unmarshal(respBody, &issue); err != nil {
		return nil, fmt.Errorf("failed to parse issue response: %w", err)
	}

	return &issue, nil
}

// FetchMilestones retrieves milestones from the project with optional state filter.
// state can be: "active", "closed", or "" (all).
func (c *Client) FetchMilestones(ctx context.Context, state string) ([]Milestone, error) {
	params := map[string]string{
		"per_page": strconv.Itoa(MaxPageSize),
	}
	if state != "" {
		params["state"] = state
	}

	urlStr := gitlabBuildURL(c, "/projects/"+c.projectPath()+"/milestones", params)
	respBody, _, err := gitlabDoRequest(c, ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch milestones: %w", err)
	}

	var milestones []Milestone
	if err := json.Unmarshal(respBody, &milestones); err != nil {
		return nil, fmt.Errorf("failed to parse milestones response: %w", err)
	}

	return milestones, nil
}

// FetchMilestoneByIID retrieves a single milestone by its project-scoped IID.
// Returns nil if no milestone matches the given IID.
func (c *Client) FetchMilestoneByIID(ctx context.Context, iid int) (*Milestone, error) {
	params := map[string]string{
		"iids[]": strconv.Itoa(iid),
	}

	urlStr := gitlabBuildURL(c, "/projects/"+c.projectPath()+"/milestones", params)
	respBody, _, err := gitlabDoRequest(c, ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch milestone by IID %d: %w", iid, err)
	}

	var milestones []Milestone
	if err := json.Unmarshal(respBody, &milestones); err != nil {
		return nil, fmt.Errorf("failed to parse milestone response: %w", err)
	}

	if len(milestones) == 0 {
		return nil, nil
	}

	return &milestones[0], nil
}

// CreateMilestone creates a new milestone in GitLab.
func (c *Client) CreateMilestone(ctx context.Context, title, description string) (*Milestone, error) {
	body := map[string]interface{}{
		"title":       title,
		"description": description,
	}

	urlStr := gitlabBuildURL(c, "/projects/"+c.projectPath()+"/milestones", nil)
	respBody, _, err := gitlabDoRequest(c, ctx, http.MethodPost, urlStr, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create milestone: %w", err)
	}

	var milestone Milestone
	if err := json.Unmarshal(respBody, &milestone); err != nil {
		return nil, fmt.Errorf("failed to parse milestone response: %w", err)
	}

	return &milestone, nil
}

// UpdateMilestone updates an existing milestone in GitLab.
func (c *Client) UpdateMilestone(ctx context.Context, milestoneID int, updates map[string]interface{}) (*Milestone, error) {
	urlStr := gitlabBuildURL(c, "/projects/"+c.projectPath()+"/milestones/"+strconv.Itoa(milestoneID), nil)
	respBody, _, err := gitlabDoRequest(c, ctx, http.MethodPut, urlStr, updates)
	if err != nil {
		return nil, fmt.Errorf("failed to update milestone: %w", err)
	}

	var milestone Milestone
	if err := json.Unmarshal(respBody, &milestone); err != nil {
		return nil, fmt.Errorf("failed to parse milestone response: %w", err)
	}

	return &milestone, nil
}
