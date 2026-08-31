package gitlab

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
)

func (t *Tracker) FetchIssues(ctx context.Context, opts tracker.FetchOptions) ([]tracker.TrackerIssue, error) {
	var issues []Issue
	var err error

	state := opts.State
	if state == "" {
		state = "all"
	}
	// GitLab uses "opened" not "open"
	if state == "open" {
		state = "opened"
	}

	if opts.Since != nil {
		issues, err = t.client.FetchIssuesSince(ctx, state, *opts.Since, t.filter)
	} else {
		issues, err = t.client.FetchIssues(ctx, state, t.filter)
	}
	if err != nil {
		return nil, err
	}

	// Enrich each issue with its links (dependencies).
	for i := range issues {
		links, err := t.client.GetIssueLinks(ctx, issues[i].IID)
		if err != nil {
			// Non-fatal: issue may lack link permissions or be in a different project.
			continue
		}
		issues[i].IssueLinksData = links
	}

	result := make([]tracker.TrackerIssue, 0, len(issues))
	for _, gl := range issues {
		result = append(result, gitlabToTrackerIssue(&gl))
	}
	return result, nil
}

func (t *Tracker) FetchIssue(ctx context.Context, identifier string) (*tracker.TrackerIssue, error) {
	if milestoneIID, ok, err := parseMilestoneIdentifier(identifier); ok || err != nil {
		if err != nil {
			return nil, err
		}
		ms, err := t.client.FetchMilestoneByIID(ctx, milestoneIID)
		if err != nil || ms == nil {
			return nil, err
		}
		ti := milestoneToTrackerIssue(ms)
		return ti, nil
	}

	iid, err := strconv.Atoi(identifier)
	if err != nil {
		return nil, fmt.Errorf("invalid GitLab IID %q: %w", identifier, err)
	}

	gl, err := t.client.FetchIssueByIID(ctx, iid)
	if err != nil {
		return nil, err
	}
	if gl == nil {
		return nil, nil
	}

	ti := gitlabToTrackerIssue(gl)
	return &ti, nil
}

func (t *Tracker) CreateIssue(ctx context.Context, issue *types.Issue) (*tracker.TrackerIssue, error) {
	if issue.IssueType == types.TypeEpic {
		return t.createMilestone(ctx, issue)
	}

	if created, handled, err := t.createTaskIfNeeded(ctx, issue); handled {
		return created, err
	}

	fields := BeadsIssueToGitLabFields(issue, t.config)
	labels, _ := fields["labels"].([]string)

	created, err := t.client.CreateIssue(ctx, issue.Title, issue.Description, labels)
	if err != nil {
		return nil, err
	}

	t.assignParentEpicMilestone(ctx, issue.ID, created.IID)
	created, warnings := t.closeCreatedIssue(ctx, issue, created)

	ti := gitlabToTrackerIssue(created)
	ti.Warnings = warnings
	return &ti, nil
}

func (t *Tracker) createTaskIfNeeded(ctx context.Context, issue *types.Issue) (*tracker.TrackerIssue, bool, error) {
	if issue.IssueType != types.TypeTask || t.projectPath == "" || t.store == nil {
		return nil, false, nil
	}
	parentGID := t.findParentStoryGID(ctx, issue.ID)
	if parentGID == "" {
		return nil, false, nil
	}
	created, err := t.createTaskWorkItem(ctx, issue, parentGID)
	return created, true, err
}

func (t *Tracker) assignParentEpicMilestone(ctx context.Context, issueID string, issueIID int) {
	if t.store == nil {
		return
	}
	milestoneID := t.findParentEpicMilestone(ctx, issueID)
	if milestoneID <= 0 {
		return
	}
	_, _ = t.client.UpdateIssue(ctx, issueIID, map[string]interface{}{
		"milestone_id": milestoneID,
	})
}

func (t *Tracker) closeCreatedIssue(ctx context.Context, issue *types.Issue, created *Issue) (*Issue, []string) {
	if issue.Status != types.StatusClosed {
		return created, nil
	}
	closed, err := t.client.UpdateIssue(ctx, created.IID, map[string]interface{}{
		"state_event": "close",
	})
	if err == nil {
		return closed, nil
	}
	warnings := []string{fmt.Sprintf("created GitLab issue %d but failed to close it (left open): %v", created.IID, err)}
	return created, warnings
}

func (t *Tracker) UpdateIssue(ctx context.Context, externalID string, issue *types.Issue) (*tracker.TrackerIssue, error) {
	// Epic → milestone
	if issue.IssueType == types.TypeEpic {
		return t.updateMilestone(ctx, externalID, issue)
	}

	iid, err := strconv.Atoi(externalID)
	if err != nil {
		return nil, fmt.Errorf("invalid GitLab IID %q: %w", externalID, err)
	}

	updates := BeadsIssueToGitLabFields(issue, t.config)

	// Assign milestone from parent epic if one exists
	if t.store != nil {
		if milestoneID := t.findParentEpicMilestone(ctx, issue.ID); milestoneID > 0 {
			updates["milestone_id"] = milestoneID
		}
	}

	updated, err := t.client.UpdateIssue(ctx, iid, updates)
	if err != nil {
		return nil, err
	}

	ti := gitlabToTrackerIssue(updated)
	return &ti, nil
}

// createMilestone creates a GitLab milestone for an epic bead.
func (t *Tracker) createMilestone(ctx context.Context, issue *types.Issue) (*tracker.TrackerIssue, error) {
	ms, err := t.client.CreateMilestone(ctx, issue.Title, issue.Description)
	if err != nil {
		return nil, fmt.Errorf("creating milestone for epic: %w", err)
	}

	// Milestones are created active; close it with a follow-up update if the
	// epic is closed so its state carries. Best effort with the same failure
	// trade-off as CreateIssue (a failed close is not retried until the epic is
	// next modified).
	var warnings []string
	if issue.Status == types.StatusClosed {
		if closed, err := t.client.UpdateMilestone(ctx, ms.ID, map[string]interface{}{
			"state_event": "close",
		}); err == nil {
			ms = closed
		} else {
			warnings = append(warnings, fmt.Sprintf("created GitLab milestone %d but failed to close it (left active): %v", ms.ID, err))
		}
	}
	ti := milestoneToTrackerIssue(ms)
	ti.Warnings = warnings
	return ti, nil
}

// updateMilestone updates a GitLab milestone for an epic bead.
func (t *Tracker) updateMilestone(ctx context.Context, externalID string, issue *types.Issue) (*tracker.TrackerIssue, error) {
	mid, ok, err := parseMilestoneIdentifier(externalID)
	if err != nil {
		return nil, err
	}
	if !ok {
		mid, err = strconv.Atoi(externalID)
		if err != nil {
			return nil, fmt.Errorf("invalid milestone ID %q: %w", externalID, err)
		}
	}
	apiID := mid
	msByIID, err := t.client.FetchMilestoneByIID(ctx, mid)
	if err != nil {
		return nil, fmt.Errorf("resolving milestone IID %d: %w", mid, err)
	}
	if msByIID != nil {
		apiID = msByIID.ID
	}

	updates := map[string]interface{}{
		"title":       issue.Title,
		"description": issue.Description,
	}
	if issue.Status == types.StatusClosed {
		updates["state_event"] = "close"
	} else {
		updates["state_event"] = "activate"
	}

	ms, err := t.client.UpdateMilestone(ctx, apiID, updates)
	if err != nil {
		return nil, fmt.Errorf("updating milestone for epic: %w", err)
	}
	return milestoneToTrackerIssue(ms), nil
}

// findParentEpicMilestone walks up the parent-child chain from an issue
// to find an ancestor epic, then returns its GitLab milestone API ID.
// Returns 0 if no epic ancestor or no milestone found.
func (t *Tracker) findParentEpicMilestone(ctx context.Context, issueID string) int {
	currentID := issueID
	for i := 0; i < 5; i++ {
		deps, err := t.store.GetDependenciesWithMetadata(ctx, currentID)
		if err != nil {
			return 0
		}
		parentID, milestoneID, isEpic, found := t.inspectParent(ctx, deps)
		if !found || isEpic {
			return milestoneID
		}
		currentID = parentID
	}
	return 0
}

func (t *Tracker) inspectParent(ctx context.Context, deps []*types.IssueWithDependencyMetadata) (string, int, bool, bool) {
	for _, dep := range deps {
		if dep.DependencyType != types.DepParentChild {
			continue
		}
		if dep.Issue.IssueType == types.TypeEpic {
			return "", t.milestoneIDForEpic(ctx, dep.Issue), true, true
		}
		return dep.Issue.ID, 0, false, true
	}
	return "", 0, false, false
}

func (t *Tracker) milestoneIDForEpic(ctx context.Context, issue types.Issue) int {
	if issue.ExternalRef == nil || *issue.ExternalRef == "" {
		return 0
	}
	matches := milestoneIDPattern.FindStringSubmatch(*issue.ExternalRef)
	if len(matches) < 2 {
		return 0
	}
	milestoneIID, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0
	}
	ms, err := t.client.FetchMilestoneByIID(ctx, milestoneIID)
	if err != nil || ms == nil {
		return 0
	}
	return ms.ID
}

// createTaskWorkItem creates a GitLab Task work item as a child of a parent Issue.
func (t *Tracker) createTaskWorkItem(ctx context.Context, issue *types.Issue, parentGID string) (*tracker.TrackerIssue, error) {
	wi, err := t.client.CreateTaskWorkItem(ctx, t.projectPath, issue.Title, issue.Description, parentGID)
	if err != nil {
		return nil, fmt.Errorf("creating task work item: %w", err)
	}

	// Build URL from project path and IID
	webURL := fmt.Sprintf("%s/%s/-/work_items/%s", t.client.BaseURL, t.projectPath, wi.IID)

	iid, _ := strconv.Atoi(wi.IID)

	// Also set milestone if there's a grandparent epic
	if milestoneID := t.findParentEpicMilestone(ctx, issue.ID); milestoneID > 0 {
		if iid > 0 {
			_, _ = t.client.UpdateIssue(ctx, iid, map[string]interface{}{
				"milestone_id": milestoneID,
			})
		}
	}

	// Work items are created open; close via the issues API (work items share
	// the project IID space, as the milestone assignment above relies on) if the
	// bead is closed. Best effort with the same failure trade-off as CreateIssue.
	var warnings []string
	if issue.Status == types.StatusClosed && iid > 0 {
		if _, err := t.client.UpdateIssue(ctx, iid, map[string]interface{}{
			"state_event": "close",
		}); err != nil {
			warnings = append(warnings, fmt.Sprintf("created GitLab work item %s but failed to close it (left open): %v", wi.IID, err))
		}
	}

	return &tracker.TrackerIssue{
		TrackerIssueDetails: tracker.TrackerIssueDetails{Warnings: warnings},
		ID:                  wi.ID,
		Identifier:          wi.IID,
		URL:                 webURL,
		Title:               wi.Title,
	}, nil
}

// findParentStoryGID finds the GitLab global ID (gid://...) of a parent story/feature.
// Returns empty string if no story/feature parent or if the parent isn't synced to GitLab.
func (t *Tracker) findParentStoryGID(ctx context.Context, issueID string) string {
	deps, err := t.store.GetDependenciesWithMetadata(ctx, issueID)
	if err != nil {
		return ""
	}
	for _, dep := range deps {
		iid, ok := storyParentIID(dep)
		if !ok {
			continue
		}
		gid, err := t.client.GetWorkItemGID(ctx, t.projectPath, iid)
		if err != nil {
			continue
		}
		return gid
	}
	return ""
}

func storyParentIID(dep *types.IssueWithDependencyMetadata) (int, bool) {
	if dep.DependencyType != types.DepParentChild || dep.Issue.IssueType == types.TypeEpic || dep.Issue.ExternalRef == nil {
		return 0, false
	}
	matches := issueIIDPattern.FindStringSubmatch(*dep.Issue.ExternalRef)
	if len(matches) < 2 {
		return 0, false
	}
	iid, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0, false
	}
	return iid, true
}

// milestoneToTrackerIssue converts a GitLab Milestone to a TrackerIssue.
func milestoneToTrackerIssue(ms *Milestone) *tracker.TrackerIssue {
	ti := &tracker.TrackerIssue{
		ID:          strconv.Itoa(ms.ID),
		Identifier:  strconv.Itoa(ms.ID),
		URL:         ms.WebURL,
		Title:       ms.Title,
		Description: ms.Description,
		State:       ms.State,
	}
	if ms.CreatedAt != nil {
		ti.CreatedAt = *ms.CreatedAt
	}
	if ms.UpdatedAt != nil {
		ti.UpdatedAt = *ms.UpdatedAt
	}
	return ti
}

func (t *Tracker) FieldMapper() tracker.FieldMapper {
	return &gitlabFieldMapper{config: t.config}
}

// IsExternalRef checks if a ref belongs to this GitLab tracker.
// It recognizes both full GitLab URLs and the "gitlab:{id}" shorthand format
// produced by BuildExternalRef when a URL is unavailable.
func (t *Tracker) IsExternalRef(ref string) bool {
	if glShorthandPattern.MatchString(ref) {
		return true
	}
	if !strings.Contains(ref, "gitlab") && !strings.Contains(ref, "milestones") {
		return false
	}
	return issueIIDPattern.MatchString(ref) || milestoneIDPattern.MatchString(ref)
}

// ExtractIdentifier extracts the issue IID from a GitLab URL or shorthand ref.
func (t *Tracker) ExtractIdentifier(ref string) string {
	if m := glShorthandPattern.FindStringSubmatch(ref); len(m) >= 2 {
		return m[1]
	}
	// Try milestone pattern first (more specific path)
	if matches := milestoneIDPattern.FindStringSubmatch(ref); len(matches) >= 2 {
		return gitLabMilestoneIdentifierPrefix + matches[1]
	}
	// Fall back to issue pattern
	if matches := issueIIDPattern.FindStringSubmatch(ref); len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

func parseMilestoneIdentifier(identifier string) (int, bool, error) {
	if !strings.HasPrefix(identifier, gitLabMilestoneIdentifierPrefix) {
		return 0, false, nil
	}
	raw := strings.TrimPrefix(identifier, gitLabMilestoneIdentifierPrefix)
	iid, err := strconv.Atoi(raw)
	if err != nil {
		return 0, true, fmt.Errorf("invalid GitLab milestone identifier %q: %w", identifier, err)
	}
	return iid, true, nil
}

// IsMilestoneRef checks if an external_ref points to a milestone (not an issue).
func (t *Tracker) IsMilestoneRef(ref string) bool {
	return milestoneIDPattern.MatchString(ref)
}

func (t *Tracker) BuildExternalRef(issue *tracker.TrackerIssue) string {
	if issue.URL != "" {
		return issue.URL
	}
	return fmt.Sprintf("gitlab:%s", issue.Identifier)
}

// getConfig reads a config value from storage, falling back to env var.
// For yaml-only keys (e.g. gitlab.token), reads from config.yaml first
// to avoid leaking secrets when pushing the Dolt database to remotes.
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

// gitlabToTrackerIssue converts a gitlab.Issue to a tracker.TrackerIssue.
func gitlabToTrackerIssue(gl *Issue) tracker.TrackerIssue {
	ti := tracker.TrackerIssue{
		ID:          strconv.Itoa(gl.ID),
		Identifier:  strconv.Itoa(gl.IID),
		URL:         gl.WebURL,
		Title:       gl.Title,
		Description: gl.Description,
		Labels:      gl.Labels,
		Raw:         gl,
	}

	if gl.State != "" {
		ti.State = gl.State
	}

	if gl.Assignee != nil {
		ti.Assignee = gl.Assignee.Username
		ti.AssigneeID = strconv.Itoa(gl.Assignee.ID)
	}

	if gl.CreatedAt != nil {
		ti.CreatedAt = *gl.CreatedAt
	}
	if gl.UpdatedAt != nil {
		ti.UpdatedAt = *gl.UpdatedAt
	}
	if gl.ClosedAt != nil {
		ti.CompletedAt = gl.ClosedAt
	}

	return ti
}
