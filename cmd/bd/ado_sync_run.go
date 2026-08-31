// Package main provides the bd CLI commands.
package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jonbaldie/beads/internal/ado"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
)

// collectADOWorkItemMap gathers ADO work item IDs from local issues that
// have ADO external refs, returning a map of ADO numeric ID → local issue ID.
func collectADOWorkItemMap(ctx context.Context, at *ado.Tracker) map[int]string {
	allIssues, err := getStore().SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return nil
	}

	m := make(map[int]string)
	for _, issue := range allIssues {
		if issue.ExternalRef == nil {
			continue
		}
		ref := *issue.ExternalRef
		if !at.IsExternalRef(ref) {
			continue
		}
		idStr := at.ExtractIdentifier(ref)
		if id, err := strconv.Atoi(idStr); err == nil {
			m[id] = issue.ID
		}
	}
	return m
}

// pushADOLinks syncs beads dependencies to ADO work item relations for all
// local issues with ADO external refs. Returns the number of links synced
// and any warnings.
func pushADOLinks(ctx context.Context, resolver *ado.LinkResolver, at *ado.Tracker, st storage.Storage, warn func(string)) (int, []string) {
	allIssues, err := st.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return 0, []string{fmt.Sprintf("Link sync skipped: %v", err)}
	}

	// Build the set of ADO work item IDs beads tracks. PushLinks only removes a
	// current relation when its target is in this set, so links to items beads
	// does not track (e.g. human-created Related / Predecessor-Successor links)
	// are preserved rather than clobbered. See GH#4522.
	managedTargets := managedADOTargets(allIssues, at)
	sync := adoLinkSync{ctx: ctx, resolver: resolver, at: at, store: st, warn: warn, managedTargets: managedTargets}
	var warnings []string
	linkCount := 0

	for _, issue := range allIssues {
		count, issueWarnings := sync.pushIssue(issue)
		linkCount += count
		warnings = append(warnings, issueWarnings...)
	}

	return linkCount, warnings
}

type adoLinkSync struct {
	ctx            context.Context
	resolver       *ado.LinkResolver
	at             *ado.Tracker
	store          storage.Storage
	warn           func(string)
	managedTargets map[int]bool
}

func managedADOTargets(issues []*types.Issue, at *ado.Tracker) map[int]bool {
	managedTargets := make(map[int]bool)
	for _, issue := range issues {
		if issue.ExternalRef == nil {
			continue
		}
		ref := *issue.ExternalRef
		if !at.IsExternalRef(ref) {
			continue
		}
		if id, err := strconv.Atoi(at.ExtractIdentifier(ref)); err == nil {
			managedTargets[id] = true
		}
	}
	return managedTargets
}

func (sync adoLinkSync) pushIssue(issue *types.Issue) (int, []string) {
	if issue.ExternalRef == nil {
		return 0, nil
	}
	ref := *issue.ExternalRef
	if !sync.at.IsExternalRef(ref) {
		return 0, nil
	}
	extIDStr := sync.at.ExtractIdentifier(ref)
	workItemID, err := strconv.Atoi(extIDStr)
	if err != nil {
		return 0, nil
	}

	deps, err := sync.store.GetDependenciesWithMetadata(sync.ctx, issue.ID)
	if err != nil {
		return 0, nil
	}
	desired := buildDesiredADOLinks(deps, sync.at, extIDStr)
	if len(desired) == 0 {
		return 0, nil
	}

	items, err := sync.at.ADOClient().FetchWorkItems(sync.ctx, []int{workItemID})
	if err != nil || len(items) == 0 {
		if sync.warn != nil {
			sync.warn(fmt.Sprintf("Failed to fetch ADO #%d for link sync: %v", workItemID, err))
		}
		return 0, nil
	}

	errs := sync.resolver.PushLinks(sync.ctx, workItemID, items[0].Relations, desired, sync.managedTargets)
	warnings := formatADOLinkWarnings(workItemID, errs, sync.warn)
	return len(desired) - len(errs), warnings
}

func buildDesiredADOLinks(deps []*types.IssueWithDependencyMetadata, at *ado.Tracker, extIDStr string) []tracker.DependencyInfo {
	var desired []tracker.DependencyInfo
	for _, dep := range deps {
		if dep.ExternalRef == nil {
			continue
		}
		depRef := *dep.ExternalRef
		if !at.IsExternalRef(depRef) {
			continue
		}
		targetExtID := at.ExtractIdentifier(depRef)
		if targetExtID == "" {
			continue
		}
		desired = append(desired, tracker.DependencyInfo{
			FromExternalID: extIDStr,
			ToExternalID:   targetExtID,
			Type:           string(dep.DependencyType),
		})
	}
	return desired
}

func formatADOLinkWarnings(workItemID int, errs []error, warn func(string)) []string {
	warnings := make([]string, 0, len(errs))
	for _, err := range errs {
		msg := fmt.Sprintf("Link sync ADO #%d: %v", workItemID, err)
		warnings = append(warnings, msg)
		if warn != nil {
			warn(msg)
		}
	}
	return warnings
}

// buildADOPullHooks creates PullHooks for ADO-specific pull behavior.
// When bootstrapMatch is true, incoming ADO items are matched against existing
// local issues by external_ref, source_system, and heuristic before creating
// duplicates. When noCreate is true, unmatched items are skipped entirely.
func buildADOPullHooks(ctx context.Context, at *ado.Tracker, bootstrapMatch, noCreate bool, matchCount *int, warn func(string)) *tracker.PullHooks {
	prefix := adoIssuePrefix(ctx)

	hooks := &tracker.PullHooks{
		GenerateID: func(_ context.Context, issue *types.Issue) error {
			if issue.ID == "" {
				issue.ID = generateIssueID(prefix)
			}
			return nil
		},
	}

	if bootstrapMatch || noCreate {
		idx, bm := loadADOBootstrapMatcher(ctx, at, bootstrapMatch)
		hooks.ShouldImport = buildADOShouldImport(ctx, at, noCreate, idx, bm, matchCount, warn)
	}

	return hooks
}

func adoIssuePrefix(ctx context.Context) string {
	// YAML config takes precedence — in shared-server mode the DB
	// may belong to a different project (GH#2469).
	if prefix := config.GetString("issue-prefix"); prefix != "" {
		return prefix
	}
	if getStore() != nil {
		if prefix, err := getStore().GetConfig(ctx, "issue_prefix"); err == nil && prefix != "" {
			return prefix
		}
	}
	return "bd"
}

func loadADOBootstrapMatcher(ctx context.Context, at *ado.Tracker, enabled bool) (*ado.BootstrapIndex, *ado.BootstrapMatcher) {
	if !enabled {
		return nil, nil
	}
	localIssues, _ := getStore().SearchIssues(ctx, "", types.IssueFilter{})
	return ado.BuildBootstrapIndex(localIssues), ado.NewBootstrapMatcher(at.FieldMapper(), true)
}

func buildADOShouldImport(ctx context.Context, at *ado.Tracker, noCreate bool, idx *ado.BootstrapIndex, bm *ado.BootstrapMatcher, matchCount *int, warn func(string)) func(*tracker.TrackerIssue) bool {
	return func(extIssue *tracker.TrackerIssue) bool {
		ref := at.BuildExternalRef(extIssue)
		existing, _ := getStore().GetIssueByExternalRef(ctx, ref)
		if existing != nil {
			return true
		}
		if tryADOBootstrapMatch(ctx, extIssue, ref, idx, bm, matchCount, warn) {
			return true
		}
		return !noCreate
	}
}

func tryADOBootstrapMatch(ctx context.Context, extIssue *tracker.TrackerIssue, ref string, idx *ado.BootstrapIndex, bm *ado.BootstrapMatcher, matchCount *int, warn func(string)) bool {
	if bm == nil {
		return false
	}
	result := bm.FindMatchIndexed(extIssue, idx)
	if result.Matched {
		updates := map[string]interface{}{
			"external_ref":  ref,
			"source_system": "ado:" + extIssue.ID,
		}
		if err := getStore().UpdateIssue(ctx, result.BeadsID, updates, getActor()); err == nil {
			*matchCount++
			if warn != nil {
				warn(fmt.Sprintf("Bootstrap matched ADO #%s → %s (%s)", extIssue.ID, result.BeadsID, result.MatchType))
			}
			return true
		}
	}
	if result.Candidates > 1 && warn != nil {
		warn(fmt.Sprintf("Ambiguous bootstrap match for ADO #%s: %d candidates", extIssue.ID, result.Candidates))
	}
	return false
}

// buildADOPushHooks creates PushHooks for ADO-specific push filtering.
// When --types or --states are set, local beads are filtered before pushing
// to ADO by mapping the ADO filter values to beads types/statuses.
// When noCreate is true, only issues already linked to ADO work items
// are pushed (no new work items are created).
func buildADOPushHooks(mapper tracker.FieldMapper, isExternalRef func(string) bool, filters *ado.PullFilters, noCreate bool) *tracker.PushHooks {
	allowedTypes := allowedADOIssueTypes(mapper, filters)
	allowedStatuses := allowedADOStatuses(mapper, filters)

	if allowedTypes == nil && allowedStatuses == nil && !noCreate {
		return nil
	}

	return &tracker.PushHooks{
		ShouldPush: func(issue *types.Issue) bool {
			return shouldPushADOIssue(issue, allowedTypes, allowedStatuses, noCreate, isExternalRef)
		},
	}
}

func allowedADOIssueTypes(mapper tracker.FieldMapper, filters *ado.PullFilters) map[types.IssueType]bool {
	if filters == nil || len(filters.WorkItemTypes) == 0 {
		return nil
	}
	allowed := make(map[types.IssueType]bool, len(filters.WorkItemTypes))
	for _, adoType := range filters.WorkItemTypes {
		allowed[mapper.TypeToBeads(adoType)] = true
	}
	return allowed
}

func allowedADOStatuses(mapper tracker.FieldMapper, filters *ado.PullFilters) map[types.Status]bool {
	if filters == nil || len(filters.States) == 0 {
		return nil
	}
	allowed := make(map[types.Status]bool, len(filters.States))
	for _, adoState := range filters.States {
		allowed[mapper.StatusToBeads(adoState)] = true
	}
	return allowed
}

func shouldPushADOIssue(issue *types.Issue, allowedTypes map[types.IssueType]bool, allowedStatuses map[types.Status]bool, noCreate bool, isExternalRef func(string) bool) bool {
	if allowedTypes != nil && !allowedTypes[issue.IssueType] {
		return false
	}
	if allowedStatuses != nil && !allowedStatuses[issue.Status] {
		return false
	}
	if noCreate && (issue.ExternalRef == nil || *issue.ExternalRef == "" || !isExternalRef(*issue.ExternalRef)) {
		return false
	}
	return true
}
