package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// FetchIssues retrieves issues from Linear with optional filtering by state.
// state can be: "open" (unstarted/started), "closed" (completed/canceled), or "all".
// If ProjectID is set on the client, only issues from that project are returned.
func (c *clientIssuePageReader) FetchIssues(ctx context.Context, state string) ([]Issue, error) {
	return c.fetchIssues(ctx, c.buildIssueFilter(state, ""), "failed to fetch issues")
}

// FetchIssuesSince retrieves issues from Linear that have been updated since the given time.
// This enables incremental sync by only fetching issues modified after the last sync.
// The state parameter can be: "open", "closed", or "all".
// If ProjectID is set on the client, only issues from that project are returned.
func (c *clientIssuePageReader) FetchIssuesSince(ctx context.Context, state string, since time.Time) ([]Issue, error) {
	sinceStr := since.UTC().Format(time.RFC3339)
	return c.fetchIssues(ctx, c.buildIssueFilter(state, sinceStr), "failed to fetch issues since "+sinceStr)
}

func (c *clientIssuePageReader) buildIssueFilter(state, since string) map[string]interface{} {
	filter := map[string]interface{}{
		"team": map[string]interface{}{
			"id": map[string]interface{}{
				"eq": c.TeamID,
			},
		},
	}
	if since != "" {
		filter["updatedAt"] = map[string]interface{}{"gte": since}
	}
	if c.ProjectID != "" {
		filter["project"] = map[string]interface{}{
			"id": map[string]interface{}{
				"eq": c.ProjectID,
			},
		}
	}
	addLinearStateFilter(filter, state)
	return filter
}

func addLinearStateFilter(filter map[string]interface{}, state string) {
	var stateTypes []string
	switch state {
	case "open":
		stateTypes = []string{"backlog", "unstarted", "started"}
	case "closed":
		stateTypes = []string{"completed", "canceled"}
	}
	if len(stateTypes) > 0 {
		filter["state"] = map[string]interface{}{
			"type": map[string]interface{}{"in": stateTypes},
		}
	}
}

func (c *clientIssuePageReader) fetchIssues(ctx context.Context, filter map[string]interface{}, errorPrefix string) ([]Issue, error) {
	var allIssues []Issue
	var cursor string
	for page := 1; ; page++ {
		select {
		case <-ctx.Done():
			return allIssues, ctx.Err()
		default:
		}
		if page > MaxPages {
			return nil, fmt.Errorf("pagination limit exceeded: stopped after %d pages", MaxPages)
		}

		issuesResp, err := c.fetchIssuePage(ctx, filter, cursor, errorPrefix)
		if err != nil {
			return nil, err
		}
		allIssues = append(allIssues, issuesResp.Issues.Nodes...)
		if !issuesResp.Issues.PageInfo.HasNextPage {
			return allIssues, nil
		}
		cursor = issuesResp.Issues.PageInfo.EndCursor
	}
}

func (c *clientIssuePageReader) fetchIssuePage(ctx context.Context, filter map[string]interface{}, cursor, errorPrefix string) (IssuesResponse, error) {
	variables := map[string]interface{}{
		"filter": filter,
		"first":  MaxPageSize,
	}
	if cursor != "" {
		variables["after"] = cursor
	}
	data, err := c.Execute(ctx, &GraphQLRequest{Query: issuesQuery, Variables: variables})
	if err != nil {
		return IssuesResponse{}, fmt.Errorf("%s: %w", errorPrefix, err)
	}
	var issuesResp IssuesResponse
	if err := json.Unmarshal(data, &issuesResp); err != nil {
		return IssuesResponse{}, fmt.Errorf("failed to parse issues response: %w", err)
	}
	return issuesResp, nil
}

// GetTeamStates fetches the workflow states for the configured team.
func (c *clientTeamReader) GetTeamStates(ctx context.Context) ([]State, error) {
	query := `
		query TeamStates($teamId: String!) {
			team(id: $teamId) {
				id
				states {
					nodes {
						id
						name
						type
					}
				}
			}
		}
	`

	req := &GraphQLRequest{
		Query: query,
		Variables: map[string]interface{}{
			"teamId": c.TeamID,
		},
	}

	data, err := c.Execute(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch team states: %w", err)
	}

	var teamResp TeamResponse
	if err := json.Unmarshal(data, &teamResp); err != nil {
		return nil, fmt.Errorf("failed to parse team states response: %w", err)
	}

	if teamResp.Team.States == nil {
		return nil, fmt.Errorf("no states found for team")
	}

	return teamResp.Team.States.Nodes, nil
}

// GetTeamLabels returns all issue labels defined for the team (paginated).
func (c *clientTeamReader) GetTeamLabels(ctx context.Context) ([]Label, error) {
	const pageSize = 250
	query := `
		query TeamLabels($teamId: String!, $first: Int!, $after: String) {
			team(id: $teamId) {
				labels(first: $first, after: $after) {
					nodes {
						id
						name
					}
					pageInfo {
						hasNextPage
						endCursor
					}
				}
			}
		}
	`

	var all []Label
	var after *string
	for {
		vars := map[string]interface{}{
			"teamId": c.TeamID,
			"first":  pageSize,
			"after":  nil,
		}
		if after != nil {
			vars["after"] = *after
		}

		req := &GraphQLRequest{
			Query:     query,
			Variables: vars,
		}

		data, err := c.Execute(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch team labels: %w", err)
		}

		var page struct {
			Team struct {
				Labels *struct {
					Nodes    []Label `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"labels"`
			} `json:"team"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, fmt.Errorf("failed to parse team labels response: %w", err)
		}
		if page.Team.Labels == nil {
			return nil, fmt.Errorf("no labels connection found for team")
		}

		all = append(all, page.Team.Labels.Nodes...)
		if !page.Team.Labels.PageInfo.HasNextPage {
			break
		}
		if page.Team.Labels.PageInfo.EndCursor == "" {
			break
		}
		cursor := page.Team.Labels.PageInfo.EndCursor
		after = &cursor
	}

	return all, nil
}

// FindIssueByDescriptionContains searches for an issue whose description
// contains the given text. This powers idempotency dedup: we embed a
// deterministic marker in the description and search for it before creating.
// Returns nil (no error) when no match is found.
func (c *clientIssueLookupReader) FindIssueByDescriptionContains(ctx context.Context, text string) (*Issue, error) {
	return findIssueByDescriptionContains(c.clientTransport, ctx, text)
}

func findIssueByDescriptionContains(c *clientTransport, ctx context.Context, text string) (*Issue, error) {
	query := `
		query FindByDescription($filter: IssueFilter!) {
			issues(filter: $filter, first: 1) {
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
					createdAt
					updatedAt
				}
			}
		}
	`

	filter := map[string]interface{}{
		"team": map[string]interface{}{
			"id": map[string]interface{}{
				"eq": c.TeamID,
			},
		},
		"description": map[string]interface{}{
			"contains": text,
		},
	}

	req := &GraphQLRequest{
		Query: query,
		Variables: map[string]interface{}{
			"filter": filter,
		},
	}

	data, err := c.Execute(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to search issues by description: %w", err)
	}

	var issuesResp IssuesResponse
	if err := json.Unmarshal(data, &issuesResp); err != nil {
		return nil, fmt.Errorf("failed to parse description search response: %w", err)
	}

	if len(issuesResp.Issues.Nodes) > 0 {
		return &issuesResp.Issues.Nodes[0], nil
	}
	return nil, nil
}
