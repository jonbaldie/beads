// Package main provides the bd CLI commands.
package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/gitlab"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
)

// pushGitLabDependencyLinks runs the dependency-link + epic-milestone push pass:
// it converts beads dependencies among the scoped issues (per opts) into GitLab
// issue links (additive — stale remote links are left untouched) and repairs
// epic-child milestones. Shared by `bd gitlab sync` and `bd gitlab push` so both
// reach the same link parity. Dry-run plan lines are written to out (unless
// --json); warnings are delivered via warn. Returns the number of links created,
// license-skipped, and milestones updated.
func pushGitLabDependencyLinks(ctx context.Context, gt *gitlab.Tracker, st storage.Storage, opts tracker.SyncOptions, dryRun bool, out io.Writer, warn func(string)) (linksPushed, linksLicenseSkipped, milestonesUpdated int) {
	linkData, collectWarnings := collectGitLabLinkSyncData(ctx, st, opts)
	for _, warning := range collectWarnings {
		warn(warning)
	}

	client := gt.GitLabClient()
	if client == nil {
		return 0, 0, 0
	}
	linksPushed, linksLicenseSkipped = pushGitLabIssueLinks(ctx, client, linkData.DesiredLinks, dryRun, out, warn)
	milestonesUpdated = pushGitLabEpicMilestones(ctx, gt, linkData.ScopedIssues, dryRun, out, warn)
	return linksPushed, linksLicenseSkipped, milestonesUpdated
}

func pushGitLabIssueLinks(ctx context.Context, client *gitlab.Client, desiredLinks []gitlab.DependencyLink, dryRun bool, out io.Writer, warn func(string)) (int, int) {
	if len(desiredLinks) == 0 {
		return 0, 0
	}
	resolver := gitlab.NewLinkResolver(client)
	res := resolver.PushLinks(ctx, desiredLinks, gitlab.PushLinkOptions{
		DryRun: dryRun,
		OnPlan: func(link gitlab.DependencyLink) {
			if !isJSONOutput() {
				_, _ = fmt.Fprintf(out, "  [dry-run] Would create GitLab dependency link: #%d %s #%d\n",
					link.SourceIID, link.LinkType, link.TargetIID)
			}
		},
	})
	// Curated, license-aware degradation: one actionable line instead of
	// a raw per-link API error, kept distinct from genuine failures.
	if res.LicenseSkipped > 0 {
		warn(gitLabLicenseSkipMessage(res.LicenseSkipped))
	}
	for _, err := range res.Errors {
		warn(fmt.Sprintf("GitLab dependency link sync: %v", err))
	}
	return res.Created, res.LicenseSkipped
}

func pushGitLabEpicMilestones(ctx context.Context, gt *gitlab.Tracker, scopedIssues []*types.Issue, dryRun bool, out io.Writer, warn func(string)) int {
	if len(scopedIssues) == 0 {
		return 0
	}
	count, errs := gt.PushEpicMilestones(ctx, scopedIssues, gitlab.EpicMilestoneOptions{
		DryRun: dryRun,
		OnPlan: func(issueID string, issueIID int, milestoneID int) {
			if !isJSONOutput() {
				_, _ = fmt.Fprintf(out, "  [dry-run] Would set GitLab milestone %d on %s (#%d)\n",
					milestoneID, issueID, issueIID)
			}
		},
	})
	for _, err := range errs {
		warn(fmt.Sprintf("GitLab epic milestone sync: %v", err))
	}
	return count
}

type gitlabLinkSyncData struct {
	ScopedIssues []*types.Issue
	DesiredLinks []gitlab.DependencyLink
}

func collectGitLabLinkSyncData(ctx context.Context, st storage.Storage, opts tracker.SyncOptions) (gitlabLinkSyncData, []string) {
	if st == nil {
		return gitlabLinkSyncData{}, []string{"GitLab dependency link sync skipped: database not available"}
	}

	allIssues, err := st.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return gitlabLinkSyncData{}, []string{fmt.Sprintf("GitLab dependency link sync skipped: %v", err)}
	}

	descendantSet, err := gitlabDescendantSetForOptions(ctx, st, opts)
	if err != nil {
		return gitlabLinkSyncData{}, []string{fmt.Sprintf("GitLab dependency link sync skipped: resolving parent %s: %v", opts.ParentID, err)}
	}

	scopedIssues := filterGitLabLinkScopedIssues(allIssues, opts, descendantSet)
	scopedIssueIDs := gitlabScopedIssueIDSet(scopedIssues)
	desired, warnings := collectGitLabDesiredLinks(ctx, st, scopedIssues, scopedIssueIDs)
	return gitlabLinkSyncData{
		ScopedIssues: scopedIssues,
		DesiredLinks: gitlab.DeduplicateLinks(desired),
	}, warnings
}

func gitlabDescendantSetForOptions(ctx context.Context, st storage.Storage, opts tracker.SyncOptions) (map[string]bool, error) {
	if opts.ParentID == "" {
		return nil, nil
	}
	return buildGitLabDescendantSet(ctx, st, opts.ParentID)
}

func collectGitLabDesiredLinks(ctx context.Context, st storage.Storage, scopedIssues []*types.Issue, scopedIssueIDs map[string]bool) ([]gitlab.DependencyLink, []string) {
	var warnings []string
	desired := make([]gitlab.DependencyLink, 0, len(scopedIssues))
	for _, issue := range scopedIssues {
		deps, err := st.GetDependenciesWithMetadata(ctx, issue.ID)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("GitLab dependency link sync skipped dependencies for %s: %v", issue.ID, err))
			continue
		}
		for _, dep := range deps {
			if !scopedIssueIDs[dep.ID] {
				continue
			}
			link, ok := gitlab.LinkFromBeadsDependency(issue, dep)
			if ok {
				desired = append(desired, link)
			}
		}
	}
	return desired, warnings
}

func filterGitLabLinkScopedIssues(issues []*types.Issue, opts tracker.SyncOptions, descendantSet map[string]bool) []*types.Issue {
	result := make([]*types.Issue, 0, len(issues))
	issueIDSet := gitlabIssueIDSet(opts.IssueIDs)
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		if issueIDSet != nil && !issueIDSet[issue.ID] {
			continue
		}
		if descendantSet != nil && !descendantSet[issue.ID] {
			continue
		}
		if !gitlabIssueAllowedByPushFilters(issue, opts) {
			continue
		}
		result = append(result, issue)
	}
	return result
}

func gitlabIssueAllowedByPushFilters(issue *types.Issue, opts tracker.SyncOptions) bool {
	if issue == nil {
		return false
	}
	if opts.ExcludeEphemeral && issue.Ephemeral {
		return false
	}
	if !gitlabIssueMatchesTypeFilter(issue, opts.TypeFilter) {
		return false
	}
	if gitlabIssueExcludedByType(issue, opts.ExcludeTypes) {
		return false
	}
	return opts.State != "open" || issue.Status != types.StatusClosed
}

func gitlabIssueMatchesTypeFilter(issue *types.Issue, typeFilter []types.IssueType) bool {
	if len(typeFilter) == 0 {
		return true
	}
	for _, issueType := range typeFilter {
		if issue.IssueType == issueType {
			return true
		}
	}
	return false
}

func gitlabIssueExcludedByType(issue *types.Issue, excludedTypes []types.IssueType) bool {
	for _, issueType := range excludedTypes {
		if issue.IssueType == issueType {
			return true
		}
	}
	return false
}

func buildGitLabDescendantSet(ctx context.Context, st storage.Storage, parentID string) (map[string]bool, error) {
	result := map[string]bool{parentID: true}
	queue := []string{parentID}
	for {
		if len(queue) == 0 {
			break
		}
		current := queue[0]
		queue = queue[1:]
		dependents, err := st.GetDependentsWithMetadata(ctx, current)
		if err != nil {
			return nil, fmt.Errorf("getting dependents of %s: %w", current, err)
		}
		for _, dep := range dependents {
			if dep.DependencyType == types.DepParentChild && !result[dep.Issue.ID] {
				result[dep.Issue.ID] = true
				queue = append(queue, dep.Issue.ID)
			}
		}
	}
	return result, nil
}

func gitlabIssueIDSet(ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	result := make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" {
			result[id] = true
		}
	}
	return result
}

func gitlabScopedIssueIDSet(issues []*types.Issue) map[string]bool {
	result := make(map[string]bool, len(issues))
	for _, issue := range issues {
		if issue != nil && issue.ID != "" {
			result[issue.ID] = true
		}
	}
	return result
}

// buildCLIFilter constructs an IssueFilter from CLI flags.
// Returns nil if no filter flags were provided.
func buildCLIFilter(flags gitLabSyncFlags) *gitlab.IssueFilter {
	if flags.filterLabel == "" && flags.filterProject == "" &&
		flags.filterMilestone == "" && flags.filterAssignee == "" {
		return nil
	}
	filter := &gitlab.IssueFilter{
		Labels:    flags.filterLabel,
		Milestone: flags.filterMilestone,
		Assignee:  flags.filterAssignee,
	}
	if flags.filterProject != "" {
		if pid, err := strconv.Atoi(flags.filterProject); err == nil {
			filter.ProjectID = pid
		}
	}
	return filter
}

// buildGitLabPullHooks creates PullHooks for GitLab-specific pull behavior.
func buildGitLabPullHooks(ctx context.Context) *tracker.PullHooks {
	prefix := "bd"
	// YAML config takes precedence — in shared-server mode the DB
	// may belong to a different project (GH#2469).
	if p := config.GetString("issue-prefix"); p != "" {
		prefix = p
	} else if getStore() != nil {
		if p, err := getStore().GetConfig(ctx, "issue_prefix"); err == nil && p != "" {
			prefix = p
		}
	}

	return &tracker.PullHooks{
		GenerateID: func(_ context.Context, issue *types.Issue) error {
			if issue.ID == "" {
				issue.ID = generateIssueID(prefix)
			}
			return nil
		},
	}
}

// buildGitLabPushHooks creates PushHooks for GitLab-specific push behavior.
func buildGitLabPushHooks() *tracker.PushHooks {
	return &tracker.PushHooks{
		ContentEqual: gitLabPushContentEqual,
	}
}

func gitLabPushContentEqual(local *types.Issue, remote *tracker.TrackerIssue) bool {
	if local == nil || remote == nil {
		return false
	}

	// Epics are represented as GitLab milestones. GitLab can return a milestone
	// updated_at that is older than local beads bookkeeping even when pushed
	// fields are already identical, so use content equality to avoid repeat PUTs.
	if local.IssueType == types.TypeEpic && gitLabMilestonePushFieldsEqual(local, remote) {
		return true
	}

	// Preserve the engine's default skip behavior for everything this hook does
	// not handle explicitly.
	return !remote.UpdatedAt.Before(local.UpdatedAt)
}

func gitLabMilestonePushFieldsEqual(local *types.Issue, remote *tracker.TrackerIssue) bool {
	if !gitLabComparableTextEqual(local.Title, remote.Title) {
		return false
	}
	if !gitLabComparableTextEqual(local.Description, remote.Description) {
		return false
	}
	return gitLabMilestoneStateEqual(local.Status, remote.State)
}

func gitLabComparableTextEqual(a, b string) bool {
	return strings.TrimSpace(strings.ReplaceAll(a, "\r\n", "\n")) ==
		strings.TrimSpace(strings.ReplaceAll(b, "\r\n", "\n"))
}

func gitLabMilestoneStateEqual(status types.Status, remoteState interface{}) bool {
	remoteStateString, ok := remoteState.(string)
	if !ok {
		return false
	}
	remoteStateString = strings.ToLower(strings.TrimSpace(remoteStateString))
	switch status {
	case types.StatusClosed:
		return remoteStateString == "closed"
	default:
		return remoteStateString == "active"
	}
}

// parseTypeList splits a comma-separated string of issue types.
// Returns nil for empty input.
func parseTypeList(s string) []types.IssueType {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	result := make([]types.IssueType, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, types.IssueType(p))
		}
	}
	return result
}
