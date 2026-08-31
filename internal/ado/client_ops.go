package ado

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// FetchWorkItems retrieves work items by ID in batches of MaxBatchSize.
func (c *Client) FetchWorkItems(ctx context.Context, ids []int) ([]WorkItem, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	var all []WorkItem
	total := len(ids)
	for start := 0; start < total; start += MaxBatchSize {
		end := start + MaxBatchSize
		if end > total {
			end = total
		}
		chunk := ids[start:end]

		parts := make([]string, len(chunk))
		for i, id := range chunk {
			parts[i] = strconv.Itoa(id)
		}

		urlStr := addAPIVersion(c.apiBase() + "/wit/workitems?ids=" + strings.Join(parts, ",") + "&$expand=All")
		respBody, err := c.doRequest(ctx, http.MethodGet, urlStr, "", nil)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch work items: %w", err)
		}

		var envelope listResponse
		if err := json.Unmarshal(respBody, &envelope); err != nil {
			return nil, fmt.Errorf("failed to parse work items response: %w", err)
		}

		var items []WorkItem
		if err := json.Unmarshal(envelope.Value, &items); err != nil {
			return nil, fmt.Errorf("failed to parse work items value: %w", err)
		}
		all = append(all, items...)
	}

	return all, nil
}

// buildPullWIQLMulti builds a WIQL query that can filter across multiple projects.
func (c *Client) buildPullWIQLMulti(projects []string, since *time.Time, filters *PullFilters) string {
	clauses := []string{
		buildProjectClause(projects),
		"[System.IsDeleted] = false",
	}
	if since != nil {
		clauses = append(clauses, fmt.Sprintf(
			"[System.ChangedDate] >= '%s'",
			formatWIQLDate(*since),
		))
	}
	clauses = append(clauses, buildFilterClauses(filters)...)
	return "SELECT [System.Id] FROM WorkItems WHERE " +
		strings.Join(clauses, " AND ") +
		" ORDER BY [System.ChangedDate] ASC"
}

func buildProjectClause(projects []string) string {
	if len(projects) == 1 {
		return fmt.Sprintf("[System.TeamProject] = '%s'", escapeWIQL(projects[0]))
	}
	return fmt.Sprintf("[System.TeamProject] IN (%s)", quoteWIQLValues(projects))
}

func buildFilterClauses(filters *PullFilters) []string {
	if filters == nil {
		return nil
	}
	var clauses []string
	if filters.AreaPath != "" {
		clauses = append(clauses, fmt.Sprintf(
			"[System.AreaPath] UNDER '%s'", escapeWIQL(filters.AreaPath),
		))
	}
	if filters.IterationPath != "" {
		clauses = append(clauses, fmt.Sprintf(
			"[System.IterationPath] UNDER '%s'", escapeWIQL(filters.IterationPath),
		))
	}
	if len(filters.WorkItemTypes) > 0 {
		clauses = append(clauses, fmt.Sprintf(
			"[System.WorkItemType] IN (%s)", quoteWIQLValues(filters.WorkItemTypes),
		))
	}
	if len(filters.States) > 0 {
		clauses = append(clauses, fmt.Sprintf(
			"[System.State] IN (%s)", quoteWIQLValues(filters.States),
		))
	}
	return clauses
}

func quoteWIQLValues(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = "'" + escapeWIQL(value) + "'"
	}
	return strings.Join(quoted, ", ")
}

// fetchWorkItemsByWIQL executes the given WIQL query and fetches full work items.
func (c *Client) fetchWorkItemsByWIQL(ctx context.Context, query string) ([]WorkItem, error) {
	urlStr := addAPIVersion(c.apiBase() + "/wit/wiql")
	reqBody := WIQLRequest{Query: query}
	respBody, err := c.doRequest(ctx, http.MethodPost, urlStr, "application/json", reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to execute WIQL query: %w", err)
	}

	var result WIQLResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to parse WIQL response: %w", err)
	}

	if len(result.WorkItems) == 0 {
		return nil, nil
	}

	ids := make([]int, len(result.WorkItems))
	for i, ref := range result.WorkItems {
		ids[i] = ref.ID
	}

	return c.FetchWorkItems(ctx, ids)
}

// FetchWorkItemsSince retrieves work items changed since the given time using WIQL.
// Pass nil for filters to fetch all work item types and states.
func (c *Client) FetchWorkItemsSince(ctx context.Context, since time.Time, filters *PullFilters) ([]WorkItem, error) {
	return c.FetchWorkItemsSinceMulti(ctx, since, []string{c.Project}, filters)
}

// FetchWorkItemsSinceMulti retrieves work items from multiple projects changed since the given time.
func (c *Client) FetchWorkItemsSinceMulti(ctx context.Context, since time.Time, projects []string, filters *PullFilters) ([]WorkItem, error) {
	if filters != nil {
		if err := filters.Validate(); err != nil {
			return nil, fmt.Errorf("invalid pull filters: %w", err)
		}
	}
	query := c.buildPullWIQLMulti(projects, &since, filters)
	return c.fetchWorkItemsByWIQL(ctx, query)
}

// FetchAllWorkItems retrieves all work items matching the given filters.
// Used for initial sync or reconciliation.
func (c *Client) FetchAllWorkItems(ctx context.Context, filters *PullFilters) ([]WorkItem, error) {
	return c.FetchAllWorkItemsMulti(ctx, []string{c.Project}, filters)
}

// FetchAllWorkItemsMulti retrieves all work items from multiple projects.
func (c *Client) FetchAllWorkItemsMulti(ctx context.Context, projects []string, filters *PullFilters) ([]WorkItem, error) {
	if filters != nil {
		if err := filters.Validate(); err != nil {
			return nil, fmt.Errorf("invalid pull filters: %w", err)
		}
	}
	query := c.buildPullWIQLMulti(projects, nil, filters)
	return c.fetchWorkItemsByWIQL(ctx, query)
}

// CreateWorkItem creates a new work item of the given type with the specified fields.
func (c *Client) CreateWorkItem(ctx context.Context, typeName string, fields map[string]interface{}) (*WorkItem, error) {
	ops := buildPatchOps(fields)
	urlStr := addAPIVersion(c.apiBase() + "/wit/workitems/$" + url.PathEscape(typeName))
	respBody, err := c.doRequest(ctx, http.MethodPost, urlStr, "application/json-patch+json", ops)
	if err != nil {
		return nil, fmt.Errorf("failed to create work item: %w", err)
	}

	var item WorkItem
	if err := json.Unmarshal(respBody, &item); err != nil {
		return nil, fmt.Errorf("failed to parse create response: %w", err)
	}
	return &item, nil
}

// UpdateWorkItem updates an existing work item's fields.
func (c *Client) UpdateWorkItem(ctx context.Context, id int, fields map[string]interface{}) (*WorkItem, error) {
	ops := buildPatchOps(fields)
	urlStr := addAPIVersion(fmt.Sprintf("%s/wit/workitems/%d", c.apiBase(), id))
	respBody, err := c.doRequest(ctx, http.MethodPatch, urlStr, "application/json-patch+json", ops)
	if err != nil {
		return nil, fmt.Errorf("failed to update work item: %w", err)
	}

	var item WorkItem
	if err := json.Unmarshal(respBody, &item); err != nil {
		return nil, fmt.Errorf("failed to parse update response: %w", err)
	}
	return &item, nil
}

// AddWorkItemLink adds a relation link from sourceID to the target work item URL.
// The comment parameter sets the relation comment attribute; pass "" for no comment.
func (c *Client) AddWorkItemLink(ctx context.Context, sourceID int, targetURL, linkType, comment string) error {
	ops := []PatchOperation{
		{
			Op:   "add",
			Path: "/relations/-",
			Value: map[string]interface{}{
				"rel": linkType,
				"url": targetURL,
				"attributes": map[string]interface{}{
					"comment": comment,
				},
			},
		},
	}
	urlStr := addAPIVersion(fmt.Sprintf("%s/wit/workitems/%d", c.apiBase(), sourceID))
	_, err := c.doRequest(ctx, http.MethodPatch, urlStr, "application/json-patch+json", ops)
	if err != nil {
		return fmt.Errorf("failed to add work item link: %w", err)
	}
	return nil
}

// RemoveWorkItemLink removes a relation link by index from the given work item.
func (c *Client) RemoveWorkItemLink(ctx context.Context, sourceID, relationIndex int) error {
	ops := []PatchOperation{
		{
			Op:   "remove",
			Path: fmt.Sprintf("/relations/%d", relationIndex),
		},
	}
	urlStr := addAPIVersion(fmt.Sprintf("%s/wit/workitems/%d", c.apiBase(), sourceID))
	_, err := c.doRequest(ctx, http.MethodPatch, urlStr, "application/json-patch+json", ops)
	if err != nil {
		return fmt.Errorf("failed to remove work item link: %w", err)
	}
	return nil
}

// ListProjects returns all team projects in the organization.
// This is an org-level endpoint, not project-scoped.
func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	urlStr := addAPIVersion(c.orgBase() + "/projects")
	respBody, err := c.doRequest(ctx, http.MethodGet, urlStr, "", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list projects: %w", err)
	}

	var envelope listResponse
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("failed to parse projects response: %w", err)
	}

	var projects []Project
	if err := json.Unmarshal(envelope.Value, &projects); err != nil {
		return nil, fmt.Errorf("failed to parse projects value: %w", err)
	}
	return projects, nil
}

// GetWorkItemTypes returns the work item types available in the project.
func (c *Client) GetWorkItemTypes(ctx context.Context) ([]WorkItemType, error) {
	urlStr := addAPIVersion(c.apiBase() + "/wit/workitemtypes")
	respBody, err := c.doRequest(ctx, http.MethodGet, urlStr, "", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get work item types: %w", err)
	}

	var envelope listResponse
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("failed to parse work item types response: %w", err)
	}

	var types []WorkItemType
	if err := json.Unmarshal(envelope.Value, &types); err != nil {
		return nil, fmt.Errorf("failed to parse work item types value: %w", err)
	}
	return types, nil
}

// GetWorkItemStates returns the states for a given work item type.
func (c *Client) GetWorkItemStates(ctx context.Context, typeName string) ([]WorkItemState, error) {
	urlStr := addAPIVersion(c.apiBase() + "/wit/workitemtypes/" + url.PathEscape(typeName) + "/states")
	respBody, err := c.doRequest(ctx, http.MethodGet, urlStr, "", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get work item states: %w", err)
	}

	var envelope listResponse
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("failed to parse work item states response: %w", err)
	}

	var states []WorkItemState
	if err := json.Unmarshal(envelope.Value, &states); err != nil {
		return nil, fmt.Errorf("failed to parse work item states value: %w", err)
	}
	return states, nil
}
