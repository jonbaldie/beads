package ado

import (
	"context"
	"fmt"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
	"net/url"
	"strconv"
	"strings"
)

func (t *Tracker) SetProjects(projects []string) {
	t.projects = projects
}

func (t *Tracker) Projects() []string {
	return t.projects
}

func (t *Tracker) PrimaryProject() string {
	if len(t.projects) == 0 {
		return ""
	}
	return t.projects[0]
}

func (t *Tracker) Name() string { return "ado" }

func (t *Tracker) DisplayName() string { return "Azure DevOps" }

func (t *Tracker) ConfigPrefix() string { return "ado" }

func (t *Tracker) Init(ctx context.Context, store storage.Storage) error {
	t.store = store
	config, err := loadADOInitConfig(t, ctx)
	if err != nil {
		return err
	}
	configureADOMappings(t, ctx)
	return initializeADOClient(t, config)
}

func (t *Tracker) Validate() error {
	if t.client == nil {
		return fmt.Errorf("Azure DevOps tracker not initialized")
	}
	ctx := context.Background()
	_, err := t.client.ListProjects(ctx)
	if err != nil {
		return fmt.Errorf("Azure DevOps validation failed: %w", err)
	}
	return nil
}

func (t *Tracker) Close() error { return nil }

func (t *Tracker) ADOClient() *Client { return t.client }

func (t *Tracker) FetchIssues(ctx context.Context, opts tracker.FetchOptions) ([]tracker.TrackerIssue, error) {
	var items []WorkItem
	var err error

	if opts.Since != nil {
		items, err = t.client.FetchWorkItemsSinceMulti(ctx, *opts.Since, t.projects, t.filters)
	} else {
		items, err = t.client.FetchAllWorkItemsMulti(ctx, t.projects, t.filters)
	}
	if err != nil {
		return nil, err
	}

	result := make([]tracker.TrackerIssue, 0, len(items))
	for i := range items {
		result = append(result, adoWorkItemToTrackerIssue(&items[i]))
	}
	return result, nil
}

func (t *Tracker) FetchIssue(ctx context.Context, identifier string) (*tracker.TrackerIssue, error) {
	id, err := strconv.Atoi(identifier)
	if err != nil {
		return nil, fmt.Errorf("invalid ADO work item ID %q: %w", identifier, err)
	}
	if id <= 0 {
		return nil, fmt.Errorf("invalid ADO work item ID: must be positive, got %d", id)
	}

	items, err := t.client.FetchWorkItems(ctx, []int{id})
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}

	ti := adoWorkItemToTrackerIssue(&items[0])
	return &ti, nil
}

func (t *Tracker) CreateIssue(ctx context.Context, issue *types.Issue) (*tracker.TrackerIssue, error) {
	fields := t.mapper.IssueToTracker(issue)
	typeName, _ := t.mapper.TypeToTracker(issue.IssueType).(string)
	if typeName == "" {
		typeName = "Task"
	}

	// Extract and remove the target state from creation fields when it is not
	// a valid initial state. ADO rejects creating items directly in states
	// like "Closed" — they must be created in an initial state and transitioned.
	var targetState string
	if s, ok := fields[FieldState].(string); ok && !isInitialState(s) {
		targetState = s
		delete(fields, FieldState)
	}

	wi, err := t.client.CreateWorkItem(ctx, typeName, fields)
	if err != nil {
		return nil, err
	}

	// Transition to the target state if it differs from the created state.
	if targetState != "" {
		createdState := wi.GetStringField(FieldState)
		if createdState != targetState {
			transitioned, err := t.client.transitionWorkItem(ctx, wi.ID, typeName, createdState, targetState)
			if err != nil {
				// Return the created item even if transition fails — the item
				// exists in ADO but may be in the wrong state.
				ti := adoWorkItemToTrackerIssue(wi)
				return &ti, fmt.Errorf("created work item %d but failed to transition from %q to %q: %w",
					wi.ID, createdState, targetState, err)
			}
			if transitioned != nil {
				wi = transitioned
			}
		}
	}

	ti := adoWorkItemToTrackerIssue(wi)
	return &ti, nil
}

func (t *Tracker) UpdateIssue(ctx context.Context, externalID string, issue *types.Issue) (*tracker.TrackerIssue, error) {
	id, err := strconv.Atoi(externalID)
	if err != nil {
		return nil, fmt.Errorf("invalid ADO work item ID %q: %w", externalID, err)
	}
	if id <= 0 {
		return nil, fmt.Errorf("invalid ADO work item ID: must be positive, got %d", id)
	}

	fields := t.mapper.IssueToTracker(issue)
	wi, err := t.client.UpdateWorkItem(ctx, id, fields)
	if err != nil {
		return nil, err
	}

	ti := adoWorkItemToTrackerIssue(wi)
	return &ti, nil
}

func (t *Tracker) FieldMapper() tracker.FieldMapper {
	return t.mapper
}

func (t *Tracker) IsExternalRef(ref string) bool {
	if adoShorthandPattern.MatchString(ref) {
		return true
	}
	if !adoWorkItemPattern.MatchString(ref) {
		return false
	}
	if t.baseURL != "" && strings.HasPrefix(ref, t.baseURL) {
		return true
	}
	return strings.Contains(ref, "dev.azure.com") || strings.Contains(ref, "visualstudio.com")
}

func (t *Tracker) ExtractIdentifier(ref string) string {
	if m := adoShorthandPattern.FindStringSubmatch(ref); len(m) >= 2 {
		return m[1]
	}
	matches := adoWorkItemPattern.FindStringSubmatch(ref)
	if len(matches) < 2 {
		return ""
	}
	return matches[1]
}

func (t *Tracker) BuildExternalRef(issue *tracker.TrackerIssue) string {
	if issue.URL != "" {
		return issue.URL
	}
	project := t.PrimaryProject()
	if t.org != "" && project != "" {
		return fmt.Sprintf("%s/%s/%s/_workitems/edit/%s",
			DefaultBaseURL, url.PathEscape(t.org), url.PathEscape(project), issue.Identifier)
	}
	if t.baseURL != "" && project != "" {
		return fmt.Sprintf("%s/%s/_workitems/edit/%s",
			t.baseURL, url.PathEscape(project), issue.Identifier)
	}
	return fmt.Sprintf("ado:%s", issue.Identifier)
}

func (t *Tracker) readMappingConfigByPrefix(ctx context.Context, prefix string) map[string]string {
	m := make(map[string]string)
	allConfig, err := t.store.GetAllConfig(ctx)
	if err != nil {
		return m
	}
	for key, val := range allConfig {
		if strings.HasPrefix(key, prefix) && val != "" {
			m[strings.TrimPrefix(key, prefix)] = val
		}
	}
	return m
}
