package gitlab

import (
	"context"
	"fmt"
	"regexp"
	"strconv"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/tracker"
)

func init() {
	tracker.Register("gitlab", func() tracker.IssueTracker {
		return &Tracker{}
	})
}

// issueIIDPattern matches GitLab issue URLs: .../issues/42 or .../work_items/42
var issueIIDPattern = regexp.MustCompile(`/(?:issues|work_items)/(\d+)`)

// glShorthandPattern matches the "gitlab:{digits}" shorthand produced by BuildExternalRef
// when a full URL is unavailable.
var glShorthandPattern = regexp.MustCompile(`^gitlab:([1-9]\d*)$`)

// milestoneIDPattern matches GitLab milestone URLs: .../-/milestones/5
var milestoneIDPattern = regexp.MustCompile(`/-/milestones/(\d+)`)

const gitLabMilestoneIdentifierPrefix = "milestone:"

func (t *Tracker) Name() string         { return "gitlab" }
func (t *Tracker) DisplayName() string  { return "GitLab" }
func (t *Tracker) ConfigPrefix() string { return "gitlab" }

// GitLabClient returns the underlying GitLab API client.
func (t *Tracker) GitLabClient() *Client { return t.client }

func (t *Tracker) Init(ctx context.Context, store storage.Storage) error {
	t.store = store
	settings, err := t.readInitConfig(ctx)
	if err != nil {
		return err
	}

	t.client = NewClient(settings.token, settings.baseURL, settings.projectID)
	if settings.groupID != "" {
		t.client = t.client.WithGroupID(settings.groupID)
	}
	t.config = DefaultMappingConfig()
	t.projectPath = t.getConfig(ctx, "gitlab.project_path", "GITLAB_PROJECT_PATH")
	t.filter = t.loadFilterConfig(ctx)
	return nil
}

type gitlabInitConfig struct {
	token     string
	baseURL   string
	projectID string
	groupID   string
}

func (t *Tracker) readInitConfig(ctx context.Context) (gitlabInitConfig, error) {
	token := t.getConfig(ctx, "gitlab.token", "GITLAB_TOKEN")
	if token == "" {
		return gitlabInitConfig{}, fmt.Errorf("GitLab token not configured (set gitlab.token or GITLAB_TOKEN)")
	}

	baseURL := t.getConfig(ctx, "gitlab.url", "GITLAB_URL")
	if baseURL == "" {
		baseURL = "https://gitlab.com"
	}

	projectID := t.getConfig(ctx, "gitlab.project_id", "GITLAB_PROJECT_ID")
	groupID := t.getConfig(ctx, "gitlab.group_id", "GITLAB_GROUP_ID")
	defaultProjectID := t.getConfig(ctx, "gitlab.default_project_id", "GITLAB_DEFAULT_PROJECT_ID")

	// When group_id is set, default_project_id is used for creating issues.
	// When group_id is not set, project_id is required.
	if groupID == "" && projectID == "" {
		return gitlabInitConfig{}, fmt.Errorf("GitLab project ID not configured (set gitlab.project_id or GITLAB_PROJECT_ID)")
	}

	// For group mode, use default_project_id as the project for creating issues.
	// If default_project_id is not set, fall back to project_id.
	if groupID != "" && projectID == "" {
		if defaultProjectID != "" {
			projectID = defaultProjectID
		}
	}
	return gitlabInitConfig{
		token:     token,
		baseURL:   baseURL,
		projectID: projectID,
		groupID:   groupID,
	}, nil
}

// loadFilterConfig reads filter configuration from store/env.
// Returns nil if no filters are configured.
func (t *Tracker) loadFilterConfig(ctx context.Context) *IssueFilter {
	labels := t.getConfig(ctx, "gitlab.filter_labels", "GITLAB_FILTER_LABELS")
	projectStr := t.getConfig(ctx, "gitlab.filter_project", "GITLAB_FILTER_PROJECT")
	milestone := t.getConfig(ctx, "gitlab.filter_milestone", "GITLAB_FILTER_MILESTONE")
	assignee := t.getConfig(ctx, "gitlab.filter_assignee", "GITLAB_FILTER_ASSIGNEE")

	if labels == "" && projectStr == "" && milestone == "" && assignee == "" {
		return nil
	}

	filter := &IssueFilter{
		Labels:    labels,
		Milestone: milestone,
		Assignee:  assignee,
	}
	if projectStr != "" {
		if pid, err := strconv.Atoi(projectStr); err == nil {
			filter.ProjectID = pid
		}
	}
	return filter
}

// SetFilter overrides the tracker's issue filter.
// CLI flags use this to override config-based defaults.
func (t *Tracker) SetFilter(filter *IssueFilter) {
	t.filter = filter
}

func (t *Tracker) Validate() error {
	if t.client == nil {
		return fmt.Errorf("GitLab tracker not initialized")
	}
	return nil
}

func (t *Tracker) Close() error { return nil }
