package linear

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
)

func (t *trackerMapping) FieldMapper() tracker.FieldMapper {
	return &linearFieldMapper{config: t.owner.config}
}

// MappingConfig returns the resolved Linear mapping configuration.
func (t *trackerMapping) MappingConfig() *MappingConfig {
	return t.owner.config
}

func (t *trackerMapping) IsExternalRef(ref string) bool {
	return IsLinearExternalRef(ref)
}

func (t *trackerMapping) ExtractIdentifier(ref string) string {
	return ExtractLinearIdentifier(ref)
}

func (t *trackerMapping) BuildExternalRef(issue *tracker.TrackerIssue) string {
	if issue.URL != "" {
		if canonical, ok := CanonicalizeLinearExternalRef(issue.URL); ok {
			return canonical
		}
		return issue.URL
	}
	return fmt.Sprintf("https://linear.app/issue/%s", issue.Identifier)
}

func skipOptionalPushStateMapping(status types.Status, err error, custom []types.CustomStatus) bool {
	if !strings.Contains(err.Error(), "has no configured Linear state") {
		return false
	}
	switch status {
	case types.StatusBlocked, types.StatusDeferred, types.StatusPinned, types.StatusHooked:
		return true
	}
	for _, cs := range custom {
		if types.Status(cs.Name) == status {
			return true
		}
	}
	return false
}

// ValidatePushStateMappings ensures push has explicit, non-ambiguous status
// mappings for every configured team before any mutation occurs.
func (t *trackerLifecycle) ValidatePushStateMappings(ctx context.Context) error {
	if t.owner.config == nil || len(t.owner.config.ExplicitStateMap) == 0 {
		return fmt.Errorf("%s", missingExplicitStateMapMessage)
	}
	for _, teamID := range t.owner.teamIDs {
		if err := t.owner.validateTeamPushStateMappings(ctx, teamID); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tracker) validateTeamPushStateMappings(ctx context.Context, teamID string) error {
	client := t.clients[teamID]
	if client == nil {
		return nil
	}
	cache, err := BuildStateCache(ctx, client)
	if err != nil {
		return fmt.Errorf("fetching workflow states for team %s: %w", teamID, err)
	}
	for _, status := range linearPushStatuses() {
		if err := t.validatePushStatus(cache, status); err != nil {
			return err
		}
	}
	for _, cs := range t.config.CustomStatuses {
		if err := t.validatePushStatus(cache, types.Status(cs.Name)); err != nil {
			return err
		}
	}
	return nil
}

func linearPushStatuses() []types.Status {
	return []types.Status{
		types.StatusOpen,
		types.StatusInProgress,
		types.StatusBlocked,
		types.StatusClosed,
		types.StatusDeferred,
		types.StatusPinned,
		types.StatusHooked,
	}
}

func (t *Tracker) validatePushStatus(cache *StateCache, status types.Status) error {
	if _, err := ResolveStateIDForBeadsStatus(cache, status, t.config); err != nil {
		if skipOptionalPushStateMapping(status, err, t.config.CustomStatuses) {
			return nil
		}
		return err
	}
	return nil
}

// findStateID looks up the Linear workflow state ID for a beads status
// using the given per-team client.
func (t *Tracker) findStateID(ctx context.Context, client *Client, status types.Status) (string, error) {
	cache, err := BuildStateCache(ctx, client)
	if err != nil {
		return "", err
	}
	return ResolveStateIDForBeadsStatus(cache, status, t.config)
}

// primaryClient returns the client for the first configured team.
func (t *Tracker) primaryClient() *Client {
	if len(t.teamIDs) == 0 {
		return nil
	}
	return t.clients[t.teamIDs[0]]
}

// clientForExternalID resolves which per-team client should handle an issue
// identified by its Linear identifier (e.g., "TEAM-123").
func (t *Tracker) clientForExternalID(ctx context.Context, externalID string) *Client {
	if len(t.teamIDs) == 1 {
		return t.primaryClient()
	}

	// Try to fetch the issue from each team's client to find the owner.
	for _, teamID := range t.teamIDs {
		client := t.clients[teamID]
		if client == nil {
			continue
		}
		li, err := client.FetchIssueByIdentifier(ctx, externalID)
		if err == nil && li != nil {
			return client
		}
	}

	return t.primaryClient()
}

// TeamIDs returns the list of configured team IDs.
func (t *trackerLifecycle) TeamIDs() []string {
	return t.owner.teamIDs
}

// PrimaryClient returns the client for the first configured team.
// Exported for CLI code that needs direct client access (e.g., push hooks).
func (t *trackerLifecycle) PrimaryClient() *Client {
	return t.owner.primaryClient()
}

// getConfig reads a config value from storage, falling back to env var.
// For yaml-only keys (e.g. linear.api_key), reads from config.yaml first
// to match the behavior of cmd/bd/linear.go:getLinearConfig().
func (t *Tracker) getConfig(ctx context.Context, key, envVar string) (string, error) {
	// Secret keys are stored in config.yaml, not the Dolt database,
	// to avoid leaking secrets when pushing to remotes.
	if config.IsYamlOnlyKey(key) {
		if val := config.GetString(key); val != "" {
			return val, nil
		}
		if envVar != "" {
			if envVal := os.Getenv(envVar); envVal != "" {
				return envVal, nil
			}
		}
		return "", nil
	}

	val, err := t.store.GetConfig(ctx, key)
	if err == nil && val != "" {
		return val, nil
	}
	if envVar != "" {
		if envVal := os.Getenv(envVar); envVal != "" {
			return envVal, nil
		}
	}
	return "", nil
}

// linearToTrackerIssue converts a linear.Issue to a tracker.TrackerIssue.
func linearToTrackerIssue(li *Issue) tracker.TrackerIssue {
	ti := tracker.TrackerIssue{
		ID:          li.ID,
		Identifier:  li.Identifier,
		URL:         li.URL,
		Title:       li.Title,
		Description: li.Description,
		Priority:    li.Priority,
		Labels:      make([]string, 0),
		Raw:         li,
	}
	addLinearState(&ti, li)
	addLinearLabels(&ti, li)
	addLinearAssignee(&ti, li)
	addLinearParent(&ti, li)
	addLinearMilestone(&ti, li)
	addLinearTimestamps(&ti, li)
	return ti
}

func addLinearState(target *tracker.TrackerIssue, li *Issue) {
	if li.State != nil {
		target.State = li.State
	}
}

func addLinearLabels(target *tracker.TrackerIssue, li *Issue) {
	if li.Labels == nil {
		return
	}
	for _, label := range li.Labels.Nodes {
		target.Labels = append(target.Labels, label.Name)
	}
}

func addLinearAssignee(target *tracker.TrackerIssue, li *Issue) {
	if li.Assignee == nil {
		return
	}
	target.Assignee = li.Assignee.Name
	target.AssigneeEmail = li.Assignee.Email
	target.AssigneeID = li.Assignee.ID
}

func addLinearParent(target *tracker.TrackerIssue, li *Issue) {
	if li.Parent == nil {
		return
	}
	target.ParentID = li.Parent.Identifier
	target.ParentInternalID = li.Parent.ID
}

func addLinearMilestone(target *tracker.TrackerIssue, li *Issue) {
	if li.ProjectMilestone == nil {
		return
	}
	target.Metadata = map[string]interface{}{
		"linear": map[string]interface{}{
			"project_milestone": li.ProjectMilestone,
		},
	}
}

func addLinearTimestamps(target *tracker.TrackerIssue, li *Issue) {
	if parsed, err := time.Parse(time.RFC3339, li.CreatedAt); err == nil {
		target.CreatedAt = parsed
	}
	if parsed, err := time.Parse(time.RFC3339, li.UpdatedAt); err == nil {
		target.UpdatedAt = parsed
	}
	if li.CompletedAt == "" {
		return
	}
	if parsed, err := time.Parse(time.RFC3339, li.CompletedAt); err == nil {
		target.CompletedAt = &parsed
	}
}

// BuildStateCacheFromTracker builds a StateCache using the tracker's primary client.
// This allows CLI code to set up PushHooks.BuildStateCache without accessing the client directly.
func BuildStateCacheFromTracker(ctx context.Context, t *Tracker) (*StateCache, error) {
	client := t.primaryClient()
	if client == nil {
		return nil, fmt.Errorf("Linear tracker not initialized")
	}
	return BuildStateCache(ctx, client)
}

// BuildLabelCacheFromTracker builds a LabelCache using the tracker's primary client.
// This allows CLI push hooks to compare label sets without reaching into the client.
func BuildLabelCacheFromTracker(ctx context.Context, t *Tracker) (*LabelCache, error) {
	client := t.primaryClient()
	if client == nil {
		return nil, fmt.Errorf("Linear tracker not initialized")
	}
	return BuildLabelCache(ctx, client)
}

// configLoaderAdapter wraps storage.Storage to implement linear.ConfigLoader.
type configLoaderAdapter struct {
	ctx   context.Context
	store storage.Storage
}

func (c *configLoaderAdapter) GetAllConfig() (map[string]string, error) {
	return c.store.GetAllConfig(c.ctx)
}
