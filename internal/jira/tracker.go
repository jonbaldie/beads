package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/tracker"
)

func init() {
	tracker.Register("jira", func() tracker.IssueTracker {
		return &Tracker{}
	})
}

// Tracker implements tracker.IssueTracker for Jira.
type Tracker struct {
	client           *Client
	store            storage.Storage
	jiraURL          string
	projectKeys      []string                          // one or more project keys (first is primary)
	apiVersion       string                            // "2" or "3" (default: "3")
	statusMap        map[string]string                 // beads status → Jira status name (from jira.status_map.* config)
	typeMap          map[string]string                 // beads type → Jira type (from jira.type_map.* config)
	priorityMap      map[string]string                 // beads priority → Jira priority name (from jira.priority_map.* config)
	customFields     map[string]interface{}            // Jira field name/id → value (from jira.custom_fields.* config)
	typeCustomFields map[string]map[string]interface{} // Jira issue type → Jira field name/id → value
}

// Validate checks that the tracker is properly initialized.
func (t *Tracker) Validate() error {
	if t.client == nil {
		return fmt.Errorf("Jira tracker not initialized")
	}
	return nil
}

// SetProjectKeys sets project keys before Init(). When set, Init() uses these
// instead of reading from config. This supports the --project CLI flag.

// ProjectKeys returns the list of configured project keys.

// PrimaryProjectKey returns the first configured project key.

type jiraInitConfig struct {
	jiraURL     string
	projectKeys []string
	username    string
	apiToken    string
	apiVersion  string
}

func loadJiraInitConfig(t *Tracker, ctx context.Context) (jiraInitConfig, error) {
	jiraURL := t.getConfig(ctx, "jira.url", "JIRA_URL")
	if jiraURL == "" {
		return jiraInitConfig{}, fmt.Errorf("Jira URL not configured (set jira.url or JIRA_URL)")
	}
	projectKeys := t.projectKeys
	if len(projectKeys) == 0 {
		pluralVal := t.getConfig(ctx, "jira.projects", "JIRA_PROJECTS")
		singularVal := t.getConfig(ctx, "jira.project", "JIRA_PROJECT")
		projectKeys = tracker.ResolveProjectIDs(nil, pluralVal, singularVal)
	}
	if len(projectKeys) == 0 {
		return jiraInitConfig{}, fmt.Errorf("Jira project not configured (set jira.project, jira.projects, or JIRA_PROJECT)")
	}
	username := t.getConfig(ctx, "jira.username", "JIRA_USERNAME")
	apiToken := t.getConfig(ctx, "jira.api_token", "JIRA_API_TOKEN")
	if apiToken == "" {
		return jiraInitConfig{}, fmt.Errorf("Jira API token not configured (set jira.api_token or JIRA_API_TOKEN)")
	}
	apiVersion := t.getConfig(ctx, "jira.api_version", "JIRA_API_VERSION")
	if apiVersion == "" {
		apiVersion = "3"
	}
	return jiraInitConfig{jiraURL: jiraURL, projectKeys: projectKeys, username: username, apiToken: apiToken, apiVersion: apiVersion}, nil
}

func loadJiraMappingConfig(t *Tracker, ctx context.Context) error {
	allConfig, err := t.store.GetAllConfig(ctx)
	if err != nil {
		return nil
	}
	if statusMap := prefixedStringMap(allConfig, "jira.status_map."); len(statusMap) > 0 {
		t.statusMap = statusMap
	}
	if typeMap := prefixedStringMap(allConfig, "jira.type_map."); len(typeMap) > 0 {
		t.typeMap = typeMap
	}
	if priorityMap := prefixedStringMap(allConfig, "jira.priority_map."); len(priorityMap) > 0 {
		t.priorityMap = priorityMap
	}
	customFields, typeCustomFields, err := prefixedCustomFields(allConfig, "jira.custom_fields.")
	if err != nil {
		return err
	}
	if len(customFields) > 0 {
		t.customFields = customFields
	}
	if len(typeCustomFields) > 0 {
		t.typeCustomFields = typeCustomFields
	}
	return nil
}

func prefixedStringMap(config map[string]string, prefix string) map[string]string {
	result := make(map[string]string)
	for key, val := range config {
		if strings.HasPrefix(key, prefix) && val != "" {
			result[strings.TrimPrefix(key, prefix)] = val
		}
	}
	return result
}

func prefixedCustomFields(config map[string]string, prefix string) (map[string]interface{}, map[string]map[string]interface{}, error) {
	customFields := make(map[string]interface{})
	typeCustomFields := make(map[string]map[string]interface{})
	for key, val := range config {
		if strings.HasPrefix(key, prefix) && strings.TrimSpace(val) != "" {
			if err := addPrefixedCustomField(customFields, typeCustomFields, strings.TrimPrefix(key, prefix), val, key); err != nil {
				return nil, nil, err
			}
		}
	}
	return customFields, typeCustomFields, nil
}

func addPrefixedCustomField(customFields map[string]interface{}, typeCustomFields map[string]map[string]interface{}, suffix, value, key string) error {
	if suffix == "" {
		return nil
	}
	parsed, err := parseJiraCustomFieldValue(value)
	if err != nil {
		return fmt.Errorf("parse %s: %w", key, err)
	}
	parts := strings.SplitN(suffix, ".", 2)
	if len(parts) == 2 {
		if parts[0] == "" || parts[1] == "" {
			return nil
		}
		if typeCustomFields[parts[0]] == nil {
			typeCustomFields[parts[0]] = make(map[string]interface{})
		}
		typeCustomFields[parts[0]][parts[1]] = parsed
		return nil
	}
	customFields[suffix] = parsed
	return nil
}

// applyTransition finds and applies the Jira workflow transition matching the given beads status.
// If no matching transition is available (e.g., the issue is already in the target state or the
// workflow doesn't permit the path), it silently succeeds.

// getConfig reads a config value from storage, falling back to env var.
// For yaml-only keys (e.g. jira.api_token), reads from config.yaml first
// to avoid leaking secrets when pushing the Dolt database to remotes.

func parseJiraCustomFieldValue(value string) (interface{}, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", nil
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var parsed interface{}
		if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
			return nil, err
		}
		return parsed, nil
	}
	return trimmed, nil
}

// jiraToTrackerIssue converts a Jira API Issue to the generic TrackerIssue format.
// priorityMap is optional (nil uses hardcoded defaults).
func jiraToTrackerIssue(ji *Issue, priorityMap map[string]string) tracker.TrackerIssue {
	ti := tracker.TrackerIssue{
		ID:         ji.ID,
		Identifier: ji.Key,
		URL:        ji.Self,
		Title:      ji.Fields.Summary,
		Labels:     ji.Fields.Labels,
		Raw:        ji,
	}

	applyJiraDetails(&ti, ji, priorityMap)
	return ti
}

func applyJiraDetails(ti *tracker.TrackerIssue, ji *Issue, priorityMap map[string]string) {
	ti.Description = DescriptionToPlainText(ji.Fields.Description)
	if ji.Fields.Priority != nil {
		ti.Priority = jiraPriorityToNumeric(ji.Fields.Priority.Name, priorityMap)
	}
	if ji.Fields.Status != nil {
		ti.State = ji.Fields.Status.Name
	}
	if ji.Fields.IssueType != nil {
		ti.Type = ji.Fields.IssueType.Name
	}
	if ji.Fields.Assignee != nil {
		ti.Assignee = ji.Fields.Assignee.DisplayName
		ti.AssigneeEmail = ji.Fields.Assignee.EmailAddress
		ti.AssigneeID = ji.Fields.Assignee.AccountID
	}
	applyJiraTimestamps(ti, ji)
	applyJiraMetadata(ti, ji)
}

func applyJiraTimestamps(ti *tracker.TrackerIssue, ji *Issue) {
	if parsed, err := ParseTimestamp(ji.Fields.Created); err == nil {
		ti.CreatedAt = parsed
	}
	if parsed, err := ParseTimestamp(ji.Fields.Updated); err == nil {
		ti.UpdatedAt = parsed
	}
}

func applyJiraMetadata(ti *tracker.TrackerIssue, ji *Issue) {
	ti.Metadata = map[string]interface{}{"source_system": fmt.Sprintf("jira:%s:%s", projectKeyFromIssue(ji), ji.Key)}
	if ji.Fields.IssueType != nil {
		ti.Metadata["jira_type"] = ji.Fields.IssueType.Name
	}
}

// jiraPriorityToNumeric converts a Jira priority name to a numeric value (0=highest, 4=lowest).
// If priorityMap is non-nil, it checks the custom mapping first (inverted: find which beads
// priority key maps to a Jira name matching the input).
func jiraPriorityToNumeric(name string, priorityMap map[string]string) int {
	if priority, ok := customJiraPriority(name, priorityMap); ok {
		return priority
	}
	switch strings.ToLower(name) {
	case "highest":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	case "lowest":
		return 4
	default:
		return 2
	}
}

func customJiraPriority(name string, priorityMap map[string]string) (int, bool) {
	for beadsKey, jiraName := range priorityMap {
		if !strings.EqualFold(name, jiraName) {
			continue
		}
		priority, err := strconv.Atoi(beadsKey)
		if err == nil && priority >= 0 && priority <= 4 {
			return priority, true
		}
	}
	return 0, false
}

// projectKeyFromIssue extracts the project key from a Jira issue.
func projectKeyFromIssue(ji *Issue) string {
	if ji.Fields.Project != nil {
		return ji.Fields.Project.Key
	}
	// Fall back to extracting from issue key (e.g., "PROJ-123" → "PROJ")
	if idx := strings.LastIndex(ji.Key, "-"); idx > 0 {
		return ji.Key[:idx]
	}
	return ""
}
