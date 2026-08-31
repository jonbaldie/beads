// Package gitlab provides client and data types for the GitLab REST API.
package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// GraphQL support for work item hierarchy (Issue → Task parent-child).

// graphqlRequest executes a GraphQL query against the GitLab instance.
func (c *Client) graphqlRequest(ctx context.Context, query string, variables map[string]interface{}) (json.RawMessage, error) {
	body := map[string]interface{}{"query": query}
	if len(variables) > 0 {
		body["variables"] = variables
	}

	// GraphQL endpoint is at /api/graphql (not under /api/v4/)
	urlStr := c.BaseURL + "/api/graphql"
	respBody, _, err := gitlabDoRequest(c, ctx, http.MethodPost, urlStr, body)
	if err != nil {
		return nil, fmt.Errorf("GraphQL request failed: %w", err)
	}

	var result struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to parse GraphQL response: %w", err)
	}
	if len(result.Errors) > 0 {
		return nil, fmt.Errorf("GraphQL error: %s", result.Errors[0].Message)
	}
	return result.Data, nil
}

// WorkItem represents a GitLab work item from the GraphQL API.
type WorkItem struct {
	ID    string `json:"id"`  // Global ID (gid://gitlab/WorkItem/123)
	IID   string `json:"iid"` // Project-scoped ID
	Title string `json:"title"`
	Type  string `json:"type"` // Work item type name
}

// defaultTaskTypeID is the fallback GID for older GitLab instances where the
// workItemTypes GraphQL query is unavailable.
const defaultTaskTypeID = "gid://gitlab/WorkItems::Type/5"

// getTaskWorkItemTypeID returns the GraphQL GID for the "Task" work item type.
// It queries the GitLab instance once per session and caches the result.
// Falls back to the hardcoded default if the query fails (e.g., older GitLab versions).
func (c *Client) getTaskWorkItemTypeID(ctx context.Context, projectPath string) string {
	if c.taskTypeID != "" {
		return c.taskTypeID
	}

	query := fmt.Sprintf(`{
		project(fullPath: %q) {
			workItemTypes(name: "Task") {
				nodes { id }
			}
		}
	}`, projectPath)

	data, err := c.graphqlRequest(ctx, query, nil)
	if err != nil {
		c.taskTypeID = defaultTaskTypeID
		return c.taskTypeID
	}

	var resp struct {
		Project struct {
			WorkItemTypes struct {
				Nodes []struct {
					ID string `json:"id"`
				} `json:"nodes"`
			} `json:"workItemTypes"`
		} `json:"project"`
	}
	if err := json.Unmarshal(data, &resp); err != nil || len(resp.Project.WorkItemTypes.Nodes) == 0 {
		c.taskTypeID = defaultTaskTypeID
		return c.taskTypeID
	}

	c.taskTypeID = resp.Project.WorkItemTypes.Nodes[0].ID
	return c.taskTypeID
}

// CreateTaskWorkItem creates a Task-type work item via GraphQL, optionally with a parent.
// parentGID is the global ID of the parent work item (e.g., "gid://gitlab/WorkItem/456").
// If parentGID is empty, creates a standalone task.
func (c *Client) CreateTaskWorkItem(ctx context.Context, projectPath, title, description, parentGID string) (*WorkItem, error) {
	typeID := c.getTaskWorkItemTypeID(ctx, projectPath)

	hierarchyPart := ""
	if parentGID != "" {
		hierarchyPart = fmt.Sprintf(`, hierarchyWidget: { parentId: %q }`, parentGID)
	}

	query := fmt.Sprintf(`mutation {
		workItemCreate(input: {
			projectPath: %q,
			title: %q,
			description: %q,
			workItemTypeId: %q%s
		}) {
			errors
			workItem { id iid title workItemType { name } webUrl }
		}
	}`, projectPath, title, description, typeID, hierarchyPart)

	data, err := c.graphqlRequest(ctx, query, nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		WorkItemCreate struct {
			Errors   []string `json:"errors"`
			WorkItem *struct {
				ID     string `json:"id"`
				IID    string `json:"iid"`
				Title  string `json:"title"`
				WebURL string `json:"webUrl"`
				Type   struct {
					Name string `json:"name"`
				} `json:"workItemType"`
			} `json:"workItem"`
		} `json:"workItemCreate"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse work item response: %w", err)
	}
	if len(resp.WorkItemCreate.Errors) > 0 {
		return nil, fmt.Errorf("work item creation failed: %s", resp.WorkItemCreate.Errors[0])
	}
	if resp.WorkItemCreate.WorkItem == nil {
		return nil, fmt.Errorf("work item creation returned nil")
	}

	wi := resp.WorkItemCreate.WorkItem
	return &WorkItem{
		ID:    wi.ID,
		IID:   wi.IID,
		Title: wi.Title,
		Type:  wi.Type.Name,
	}, nil
}

// GetWorkItemGID looks up the global ID of a work item by its project-scoped IID.
func (c *Client) GetWorkItemGID(ctx context.Context, projectPath string, iid int) (string, error) {
	query := fmt.Sprintf(`{
		project(fullPath: %q) {
			workItems(iid: "%d", first: 1) {
				nodes { id }
			}
		}
	}`, projectPath, iid)

	data, err := c.graphqlRequest(ctx, query, nil)
	if err != nil {
		return "", err
	}

	var resp struct {
		Project struct {
			WorkItems struct {
				Nodes []struct {
					ID string `json:"id"`
				} `json:"nodes"`
			} `json:"workItems"`
		} `json:"project"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse work item GID response: %w", err)
	}
	if len(resp.Project.WorkItems.Nodes) == 0 {
		return "", fmt.Errorf("work item with IID %d not found", iid)
	}
	return resp.Project.WorkItems.Nodes[0].ID, nil
}

// ListProjects retrieves projects accessible to the authenticated user.
func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	params := map[string]string{
		"membership": "true",
		"per_page":   "100",
	}
	urlStr := gitlabBuildURL(c, "/projects", params)
	respBody, _, err := gitlabDoRequest(c, ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list projects: %w", err)
	}

	var projects []Project
	if err := json.Unmarshal(respBody, &projects); err != nil {
		return nil, fmt.Errorf("failed to parse projects response: %w", err)
	}

	return projects, nil
}

// CreateIssueLink creates a link between two issues in the SAME project.
// Cross-project links are not supported by this function; attempting to link
// issues from different projects will result in an error from the GitLab API.
// linkType can be: "relates_to", "blocks", or "is_blocked_by".
func (c *Client) CreateIssueLink(ctx context.Context, sourceIID, targetIID int, linkType string) (*IssueLink, error) {
	body := map[string]interface{}{
		"target_project_id": c.ProjectID,
		"target_issue_iid":  targetIID,
		"link_type":         linkType,
	}

	urlStr := gitlabBuildURL(c, "/projects/"+c.projectPath()+"/issues/"+strconv.Itoa(sourceIID)+"/links", nil)
	respBody, _, err := gitlabDoRequest(c, ctx, http.MethodPost, urlStr, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create issue link: %w", err)
	}

	var link IssueLink
	if err := json.Unmarshal(respBody, &link); err != nil {
		return nil, fmt.Errorf("failed to parse issue link response: %w", err)
	}

	return &link, nil
}
