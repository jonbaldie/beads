package jira

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
)

func (t *Tracker) SetProjectKeys(keys []string) {
	t.projectKeys = keys
}
func (t *Tracker) ProjectKeys() []string {
	return t.projectKeys
}
func (t *Tracker) PrimaryProjectKey() string {
	if len(t.projectKeys) == 0 {
		return ""
	}
	return t.projectKeys[0]
}
func (t *Tracker) Name() string         { return "jira" }
func (t *Tracker) DisplayName() string  { return "Jira" }
func (t *Tracker) ConfigPrefix() string { return "jira" }
func (t *Tracker) Init(ctx context.Context, store storage.Storage) error {
	t.store = store

	config, err := loadJiraInitConfig(t, ctx)
	if err != nil {
		return err
	}
	t.jiraURL = config.jiraURL
	t.projectKeys = config.projectKeys
	t.apiVersion = config.apiVersion
	t.client = NewClient(config.jiraURL, config.username, config.apiToken)
	t.client.APIVersion = config.apiVersion
	if err := loadJiraMappingConfig(t, ctx); err != nil {
		return err
	}
	return nil
}
func (t *Tracker) Close() error { return nil }
func (t *Tracker) FetchIssues(ctx context.Context, opts tracker.FetchOptions) ([]tracker.TrackerIssue, error) {
	// Build JQL query — use IN clause for multi-project.
	var jql string
	if len(t.projectKeys) == 1 {
		jql = fmt.Sprintf("project = %q", t.projectKeys[0])
	} else {
		quoted := make([]string, len(t.projectKeys))
		for i, k := range t.projectKeys {
			quoted[i] = fmt.Sprintf("%q", k)
		}
		jql = fmt.Sprintf("project IN (%s)", strings.Join(quoted, ", "))
	}

	// User-configured pull_jql filter (e.g. 'labels = "agent-ready"')
	if pullJQL := t.getConfig(ctx, "jira.pull_jql", "JIRA_PULL_JQL"); pullJQL != "" {
		jql += " AND " + pullJQL
	}

	// State filter
	switch opts.State {
	case "open":
		jql += " AND statusCategory != Done"
	case "closed":
		jql += " AND statusCategory = Done"
	}

	// Incremental sync
	if opts.Since != nil {
		jql += fmt.Sprintf(` AND updated >= "%s"`, opts.Since.UTC().Format("2006-01-02 15:04 UTC"))
	}

	jql += " ORDER BY updated DESC"

	issues, err := t.client.SearchIssues(ctx, jql)
	if err != nil {
		return nil, err
	}

	result := make([]tracker.TrackerIssue, 0, len(issues))
	for i := range issues {
		result = append(result, jiraToTrackerIssue(&issues[i], t.priorityMap))
	}
	return result, nil
}
func (t *Tracker) FetchIssue(ctx context.Context, identifier string) (*tracker.TrackerIssue, error) {
	issue, err := t.client.GetIssue(ctx, identifier)
	if err != nil {
		return nil, err
	}
	if issue == nil {
		return nil, nil
	}
	ti := jiraToTrackerIssue(issue, t.priorityMap)
	return &ti, nil
}
func (t *Tracker) CreateIssue(ctx context.Context, issue *types.Issue) (*tracker.TrackerIssue, error) {
	mapper := t.FieldMapper()
	fields := mapper.IssueToTracker(issue)

	// Set project to primary (first) project key.
	fields["project"] = map[string]string{"key": t.PrimaryProjectKey()}

	created, err := t.client.CreateIssue(ctx, fields)
	if err != nil {
		return nil, err
	}

	ti := jiraToTrackerIssue(created, t.priorityMap)
	return &ti, nil
}
func (t *Tracker) UpdateIssue(ctx context.Context, externalID string, issue *types.Issue) (*tracker.TrackerIssue, error) {
	mapper := t.FieldMapper()
	fields := mapper.IssueToTracker(issue)

	if err := t.client.UpdateIssue(ctx, externalID, fields); err != nil {
		return nil, err
	}

	// Fetch current state to check whether a status transition is actually needed.
	current, err := t.client.GetIssue(ctx, externalID)
	if err != nil {
		return nil, err
	}

	desiredName, _ := mapper.StatusToTracker(issue.Status).(string)
	currentName := ""
	if current.Fields.Status != nil {
		currentName = current.Fields.Status.Name
	}

	if !strings.EqualFold(currentName, desiredName) {
		// Status differs — apply the workflow transition.
		if err := t.applyTransition(ctx, externalID, issue.Status); err != nil {
			return nil, err
		}
		// Re-fetch to return the state after the transition.
		current, err = t.client.GetIssue(ctx, externalID)
		if err != nil {
			return nil, err
		}
	}

	ti := jiraToTrackerIssue(current, t.priorityMap)
	return &ti, nil
}
func (t *Tracker) applyTransition(ctx context.Context, key string, status types.Status) error {
	mapper := t.FieldMapper()
	desiredName, ok := mapper.StatusToTracker(status).(string)
	if !ok || desiredName == "" {
		return nil
	}

	transitions, err := t.client.GetIssueTransitions(ctx, key)
	if err != nil {
		return err
	}

	for _, tr := range transitions {
		if strings.EqualFold(tr.To.Name, desiredName) {
			return t.client.TransitionIssue(ctx, key, tr.ID)
		}
	}

	debug.Logf("jira: no available transition to %q for %s (%d transitions checked)\n", desiredName, key, len(transitions))
	return nil
}
func (t *Tracker) FieldMapper() tracker.FieldMapper {
	return newJiraFieldMapper(t.apiVersion, t.statusMap, t.typeMap, t.priorityMap, t.customFields, t.typeCustomFields)
}
func (t *Tracker) IsExternalRef(ref string) bool {
	return IsJiraExternalRef(ref, t.jiraURL)
}
func (t *Tracker) ExtractIdentifier(ref string) string {
	return ExtractJiraKey(ref)
}
func (t *Tracker) BuildExternalRef(issue *tracker.TrackerIssue) string {
	return fmt.Sprintf("%s/browse/%s", t.jiraURL, issue.Identifier)
}
func (t *Tracker) getConfig(ctx context.Context, key, envVar string) string {
	// Secret keys are stored in config.yaml, not the Dolt database,
	// to avoid leaking secrets when pushing to remotes.
	if config.IsYamlOnlyKey(key) {
		if val := config.GetString(key); val != "" {
			return val
		}
		if envVar != "" {
			if envVal := os.Getenv(envVar); envVal != "" {
				return envVal
			}
		}
		return ""
	}

	val, err := t.store.GetConfig(ctx, key)
	if err == nil && val != "" {
		return val
	}
	if envVar != "" {
		if envVal := os.Getenv(envVar); envVal != "" {
			return envVal
		}
	}
	return ""
}
