package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/types"
)

// FetchIssueByIdentifier retrieves a single issue from Linear by its identifier (e.g., "TEAM-123").
// Returns nil if the issue is not found.
func (c *clientIssueLookupReader) FetchIssueByIdentifier(ctx context.Context, identifier string) (*Issue, error) {
	query := `
		query IssueByIdentifier($filter: IssueFilter!) {
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
					createdAt
					updatedAt
					completedAt
				}
			}
		}
	`

	// Build filter to search by identifier number and team prefix
	// Linear identifiers look like "TEAM-123", we filter by number
	// and validate the full identifier in the results
	variables := map[string]interface{}{
		"filter": map[string]interface{}{
			"team": map[string]interface{}{
				"id": map[string]interface{}{
					"eq": c.TeamID,
				},
			},
		},
	}

	// Extract the issue number from identifier (e.g., "123" from "TEAM-123")
	parts := strings.Split(identifier, "-")
	if len(parts) >= 2 {
		if number, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
			// Add number filter for more precise matching
			variables["filter"].(map[string]interface{})["number"] = map[string]interface{}{
				"eq": number,
			}
		}
	}

	req := &GraphQLRequest{
		Query:     query,
		Variables: variables,
	}

	data, err := c.Execute(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch issue by identifier: %w", err)
	}

	var issuesResp IssuesResponse
	if err := json.Unmarshal(data, &issuesResp); err != nil {
		return nil, fmt.Errorf("failed to parse issues response: %w", err)
	}

	// Find the exact match by identifier (in case of partial matches)
	for _, issue := range issuesResp.Issues.Nodes {
		if issue.Identifier == identifier {
			return &issue, nil
		}
	}

	return nil, nil // Issue not found
}

// BuildStateCache fetches and caches team states.
func BuildStateCache(ctx context.Context, client *Client) (*StateCache, error) {
	states, err := client.GetTeamStates(ctx)
	if err != nil {
		return nil, err
	}

	cache := &StateCache{
		States:     states,
		StatesByID: make(map[string]State),
	}

	for _, s := range states {
		cache.StatesByID[s.ID] = s
		if cache.OpenStateID == "" && (s.Type == "unstarted" || s.Type == "backlog") {
			cache.OpenStateID = s.ID
		}
	}

	return cache, nil
}

// FindStateForBeadsStatus returns the best Linear state ID for a Beads status.
func (sc *StateCache) FindStateForBeadsStatus(status types.Status) string {
	targetType := StatusToLinearStateType(status)

	for _, s := range sc.States {
		if s.Type == targetType {
			return s.ID
		}
	}

	if len(sc.States) > 0 {
		return sc.States[0].ID
	}

	return ""
}

// ExtractLinearIdentifier extracts the Linear issue identifier (e.g., "TEAM-123") from a Linear URL.
func ExtractLinearIdentifier(url string) string {
	// Linear URLs look like: https://linear.app/team/issue/TEAM-123/title
	// We want to extract "TEAM-123"
	parts := strings.Split(url, "/")
	for i, part := range parts {
		if part == "issue" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// CanonicalizeLinearExternalRef returns a stable Linear issue URL without the slug.
// Example: https://linear.app/team/issue/TEAM-123/title -> https://linear.app/team/issue/TEAM-123
// Returns ok=false if the URL isn't a recognizable Linear issue URL.
func CanonicalizeLinearExternalRef(externalRef string) (canonical string, ok bool) {
	if externalRef == "" || !IsLinearExternalRef(externalRef) {
		return "", false
	}

	parsed, err := url.Parse(externalRef)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", false
	}

	path, ok := linearIssuePath(parsed.Path)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%s://%s%s", parsed.Scheme, parsed.Host, path), true
}

func linearIssuePath(path string) (string, bool) {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if segment != "issue" {
			continue
		}
		if i+1 >= len(segments) || segments[i+1] == "" {
			continue
		}
		return "/" + strings.Join(segments[1:i+2], "/"), true
	}
	return "", false
}

// IsLinearExternalRef checks if an external_ref URL is a Linear issue URL.
func IsLinearExternalRef(externalRef string) bool {
	return strings.Contains(externalRef, "linear.app/") && strings.Contains(externalRef, "/issue/")
}

// FetchTeams retrieves all teams accessible with the current API key.
// This is useful for discovering the team ID needed for configuration.
func (c *clientTeamReader) FetchTeams(ctx context.Context) ([]Team, error) {
	query := `
		query {
			teams {
				nodes {
					id
					name
					key
				}
			}
		}
	`

	req := &GraphQLRequest{
		Query: query,
	}

	data, err := c.Execute(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch teams: %w", err)
	}

	var teamsResp TeamsResponse
	if err := json.Unmarshal(data, &teamsResp); err != nil {
		return nil, fmt.Errorf("failed to parse teams response: %w", err)
	}

	return teamsResp.Teams.Nodes, nil
}

// FetchProjects retrieves projects from Linear with optional filtering by state.
// state can be: "planned", "started", "paused", "completed", "canceled", or "all"/"".
func (c *clientProjectClient) FetchProjects(ctx context.Context, state string) ([]Project, error) {
	var allProjects []Project
	var cursor string

	filter := map[string]interface{}{
		"team": map[string]interface{}{
			"id": map[string]interface{}{
				"eq": c.TeamID,
			},
		},
	}

	if state != "all" && state != "" {
		filter["state"] = map[string]interface{}{
			"eq": state,
		}
	}

	for {
		variables := map[string]interface{}{
			"filter": filter,
			"first":  MaxPageSize,
		}
		if cursor != "" {
			variables["after"] = cursor
		}

		req := &GraphQLRequest{
			Query:     projectsQuery,
			Variables: variables,
		}

		data, err := c.Execute(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch projects: %w", err)
		}

		var projectsResp ProjectsResponse
		if err := json.Unmarshal(data, &projectsResp); err != nil {
			return nil, fmt.Errorf("failed to parse projects response: %w", err)
		}

		allProjects = append(allProjects, projectsResp.Projects.Nodes...)

		if !projectsResp.Projects.PageInfo.HasNextPage {
			break
		}
		cursor = projectsResp.Projects.PageInfo.EndCursor
	}

	return allProjects, nil
}

// CreateProject creates a new project in Linear.
func (c *clientProjectClient) CreateProject(ctx context.Context, name, description, state string) (*Project, error) {
	query := `
		mutation CreateProject($input: ProjectCreateInput!) {
			projectCreate(input: $input) {
				success
				project {
					id
					name
					description
					slugId
					url
					state
					progress
					createdAt
					updatedAt
				}
			}
		}
	`

	input := map[string]interface{}{
		"teamIds":     []string{c.TeamID},
		"name":        name,
		"description": description,
	}

	if state != "" {
		input["state"] = state
	}

	req := &GraphQLRequest{
		Query: query,
		Variables: map[string]interface{}{
			"input": input,
		},
	}

	data, err := c.Execute(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create project: %w", err)
	}

	var createResp ProjectCreateResponse
	if err := json.Unmarshal(data, &createResp); err != nil {
		return nil, fmt.Errorf("failed to parse create project response: %w", err)
	}

	if !createResp.ProjectCreate.Success {
		return nil, fmt.Errorf("project creation reported as unsuccessful")
	}

	return &createResp.ProjectCreate.Project, nil
}

// UpdateProject updates an existing project in Linear.
func (c *clientProjectClient) UpdateProject(ctx context.Context, projectID string, updates map[string]interface{}) (*Project, error) {
	query := `
		mutation UpdateProject($id: String!, $input: ProjectUpdateInput!) {
			projectUpdate(id: $id, input: $input) {
				success
				project {
					id
					name
					description
					slugId
					url
					state
					progress
					updatedAt
				}
			}
		}
	`

	req := &GraphQLRequest{
		Query: query,
		Variables: map[string]interface{}{
			"id":    projectID,
			"input": updates,
		},
	}

	data, err := c.Execute(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to update project: %w", err)
	}

	var updateResp ProjectUpdateResponse
	if err := json.Unmarshal(data, &updateResp); err != nil {
		return nil, fmt.Errorf("failed to parse update project response: %w", err)
	}

	if !updateResp.ProjectUpdate.Success {
		return nil, fmt.Errorf("project update reported as unsuccessful")
	}

	return &updateResp.ProjectUpdate.Project, nil
}
