package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/linear"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
)

type linearPullHookOptions struct {
	Milestones bool
	DryRun     bool
	Actor      string
}

// buildLinearPullHooks creates PullHooks for Linear-specific pull behavior.
func buildLinearPullHooks(ctx context.Context, opts linearPullHookOptions) *tracker.PullHooks {
	return buildLinearPullHooksForStore(ctx, getStore(), opts)
}

func buildLinearPullHooksForStore(ctx context.Context, st storage.Storage, opts linearPullHookOptions) *tracker.PullHooks {
	hooks := &tracker.PullHooks{}
	hooks.GenerateID = buildLinearImportIDGenerator(ctx, st)
	if opts.Milestones && st != nil {
		hooks.AfterConvert = buildLinearMilestoneHook(st, opts, hooks.GenerateID)
	}
	return hooks
}

func buildLinearImportIDGenerator(ctx context.Context, st storage.Storage) func(context.Context, *types.Issue) error {
	if getLinearIDMode(ctx) != "hash" || st == nil {
		return nil
	}
	usedIDs := loadLinearUsedIDs(ctx, st)
	prefix := loadLinearIssuePrefix(ctx, st)
	hashLength := getLinearHashLength(ctx)
	return func(_ context.Context, issue *types.Issue) error {
		idOpts := linear.IDGenerationOptions{
			BaseLength: hashLength,
			MaxLength:  8,
			UsedIDs:    usedIDs,
		}
		if err := linear.GenerateIssueIDs([]*types.Issue{issue}, prefix, "linear-import", idOpts); err != nil {
			return err
		}
		usedIDs[issue.ID] = true
		return nil
	}
}

func loadLinearUsedIDs(ctx context.Context, st storage.Storage) map[string]bool {
	usedIDs := make(map[string]bool)
	existingIssues, err := st.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return usedIDs
	}
	for _, issue := range existingIssues {
		if issue.ID != "" {
			usedIDs[issue.ID] = true
		}
	}
	return usedIDs
}

func loadLinearIssuePrefix(ctx context.Context, st storage.Storage) string {
	// YAML config takes precedence — in shared-server mode the DB may belong
	// to a different project (GH#2469).
	prefix := config.GetString("issue-prefix")
	if prefix != "" {
		return prefix
	}
	prefix, err := st.GetConfig(ctx, "issue_prefix")
	if err != nil || prefix == "" {
		return "bd"
	}
	return prefix
}

func buildLinearMilestoneHook(st storage.Storage, opts linearPullHookOptions, generateID func(context.Context, *types.Issue) error) func(context.Context, *tracker.TrackerIssue, *tracker.IssueConversion, string, *types.Issue, tracker.SyncOptions) error {
	hookActor := opts.Actor
	if hookActor == "" {
		hookActor = getActor()
	}
	return func(ctx context.Context, extIssue *tracker.TrackerIssue, conv *tracker.IssueConversion, ref string, _ *types.Issue, syncOpts tracker.SyncOptions) error {
		li, ok := extIssue.Raw.(*linear.Issue)
		if !ok || li == nil || li.ProjectMilestone == nil || syncOpts.DryRun || opts.DryRun {
			return nil
		}
		milestoneRef, err := ensureLinearMilestoneEpic(ctx, st, li.ProjectMilestone, hookActor, generateID)
		if err != nil {
			return err
		}
		if strings.TrimSpace(ref) == "" {
			return fmt.Errorf("missing external ref for Linear issue %s", extIssue.Identifier)
		}
		conv.Dependencies = append(conv.Dependencies, tracker.DependencyInfo{
			FromExternalID: ref,
			ToExternalID:   milestoneRef,
			Type:           string(types.DepParentChild),
			Source:         tracker.DependencySourceParent,
		})
		return nil
	}
}

const linearMilestoneExternalRefPrefix = "linear:project-milestone:"

func linearMilestoneExternalRef(id string) string {
	return linearMilestoneExternalRefPrefix + strings.TrimSpace(id)
}

func isLinearMilestoneExternalRef(ref string) bool {
	return strings.HasPrefix(strings.TrimSpace(ref), linearMilestoneExternalRefPrefix)
}

type linearMilestoneDetails struct {
	id          string
	title       string
	description string
	ref         string
	metadata    json.RawMessage
}

func ensureLinearMilestoneEpic(ctx context.Context, st storage.Storage, ms *linear.ProjectMilestone, actor string, generateID func(context.Context, *types.Issue) error) (string, error) {
	details, err := prepareLinearMilestoneDetails(ms)
	if err != nil {
		return "", err
	}
	existing, err := findLinearMilestoneEpic(ctx, st, details.ref, details.id, details.title)
	if err != nil {
		return "", err
	}
	if existing != nil {
		return updateLinearMilestoneEpic(ctx, st, existing, ms, details, actor)
	}
	return createLinearMilestoneEpic(ctx, st, details, actor, generateID)
}

func prepareLinearMilestoneDetails(ms *linear.ProjectMilestone) (linearMilestoneDetails, error) {
	milestoneID := strings.TrimSpace(ms.ID)
	if milestoneID == "" {
		return linearMilestoneDetails{}, fmt.Errorf("Linear project milestone is missing id")
	}
	title := strings.TrimSpace(ms.Name)
	if title == "" {
		title = milestoneID
	}
	metadata, err := mergedLinearMilestoneMetadata(nil, ms)
	if err != nil {
		return linearMilestoneDetails{}, err
	}
	return linearMilestoneDetails{
		id:          milestoneID,
		title:       title,
		description: ms.Description,
		ref:         linearMilestoneExternalRef(milestoneID),
		metadata:    metadata,
	}, nil
}

func updateLinearMilestoneEpic(ctx context.Context, st storage.Storage, existing *types.Issue, ms *linear.ProjectMilestone, details linearMilestoneDetails, actor string) (string, error) {
	updates, err := linearMilestoneEpicUpdates(existing, ms, details)
	if err != nil {
		return "", err
	}
	if len(updates) > 0 {
		if err := st.UpdateIssue(ctx, existing.ID, updates, actor); err != nil {
			return "", fmt.Errorf("updating Linear milestone epic %s: %w", existing.ID, err)
		}
	}
	return details.ref, nil
}

func linearMilestoneEpicUpdates(existing *types.Issue, ms *linear.ProjectMilestone, details linearMilestoneDetails) (map[string]interface{}, error) {
	updates := map[string]interface{}{}
	if existing.Title != details.title {
		updates["title"] = details.title
	}
	if existing.Description != details.description {
		updates["description"] = details.description
	}
	if existing.IssueType != types.TypeEpic {
		updates["issue_type"] = string(types.TypeEpic)
	}
	if existing.ExternalRef == nil || strings.TrimSpace(*existing.ExternalRef) != details.ref {
		updates["external_ref"] = details.ref
	}
	mergedMetadata, err := mergedLinearMilestoneMetadata(existing.Metadata, ms)
	if err != nil {
		return nil, err
	}
	if string(existing.Metadata) != string(mergedMetadata) {
		updates["metadata"] = mergedMetadata
	}
	return updates, nil
}

func createLinearMilestoneEpic(ctx context.Context, st storage.Storage, details linearMilestoneDetails, actor string, generateID func(context.Context, *types.Issue) error) (string, error) {
	externalRef := details.ref
	epic := &types.Issue{
		IssueContent: types.IssueContent{
			Title:       details.title,
			Description: details.description,
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeEpic,
		},
		IssueMeta: types.IssueMeta{
			ExternalRef: &externalRef,
			Metadata:    details.metadata,
		},
	}
	if generateID != nil {
		if err := generateID(ctx, epic); err != nil {
			return "", fmt.Errorf("generating Linear milestone epic ID: %w", err)
		}
	}
	if err := st.CreateIssue(ctx, epic, actor); err != nil {
		return "", fmt.Errorf("creating Linear milestone epic %q: %w", details.title, err)
	}
	return details.ref, nil
}

func findLinearMilestoneEpic(ctx context.Context, st storage.Storage, ref, milestoneID, title string) (*types.Issue, error) {
	existing, err := findLinearMilestoneByRef(ctx, st, ref)
	if err != nil || existing != nil {
		return existing, err
	}
	issues, err := st.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return nil, fmt.Errorf("searching local issues for Linear milestone %s: %w", milestoneID, err)
	}
	if existing := findLinearMilestoneByMetadata(issues, milestoneID); existing != nil {
		return existing, nil
	}
	return findUnlinkedLinearMilestoneEpic(issues, title), nil
}

func findLinearMilestoneByRef(ctx context.Context, st storage.Storage, ref string) (*types.Issue, error) {
	existing, err := st.GetIssueByExternalRef(ctx, ref)
	if err == nil {
		return existing, nil
	}
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	return nil, err
}

func findLinearMilestoneByMetadata(issues []*types.Issue, milestoneID string) *types.Issue {
	for _, issue := range issues {
		if issueHasLinearMilestoneID(issue, milestoneID) {
			return issue
		}
	}
	return nil
}

func findUnlinkedLinearMilestoneEpic(issues []*types.Issue, title string) *types.Issue {
	for _, issue := range issues {
		if issue.IssueType != types.TypeEpic || !strings.EqualFold(strings.TrimSpace(issue.Title), title) {
			continue
		}
		ref := ""
		if issue.ExternalRef != nil {
			ref = strings.TrimSpace(*issue.ExternalRef)
		}
		if ref == "" {
			return issue
		}
	}
	return nil
}

func mergedLinearMilestoneMetadata(existing json.RawMessage, ms *linear.ProjectMilestone) (json.RawMessage, error) {
	data := make(map[string]interface{})
	if len(existing) > 0 {
		trimmed := strings.TrimSpace(string(existing))
		if trimmed != "" && trimmed != "null" {
			if err := json.Unmarshal(existing, &data); err != nil {
				return nil, fmt.Errorf("existing milestone metadata is not a JSON object: %w", err)
			}
		}
	}

	linearMeta, _ := data["linear"].(map[string]interface{})
	if linearMeta == nil {
		linearMeta = make(map[string]interface{})
	}
	linearMeta["kind"] = "project_milestone"
	linearMeta["project_milestone"] = map[string]interface{}{
		"id":          strings.TrimSpace(ms.ID),
		"name":        ms.Name,
		"description": ms.Description,
		"progress":    ms.Progress,
		"targetDate":  ms.TargetDate,
	}
	data["linear"] = linearMeta

	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshaling Linear milestone metadata: %w", err)
	}
	return json.RawMessage(raw), nil
}

func issueHasLinearMilestoneID(issue *types.Issue, milestoneID string) bool {
	if issue == nil || len(issue.Metadata) == 0 {
		return false
	}
	var data struct {
		Linear struct {
			Kind             string `json:"kind"`
			ProjectMilestone struct {
				ID string `json:"id"`
			} `json:"project_milestone"`
		} `json:"linear"`
	}
	if err := json.Unmarshal(issue.Metadata, &data); err != nil {
		return false
	}
	return data.Linear.Kind == "project_milestone" &&
		strings.TrimSpace(data.Linear.ProjectMilestone.ID) == strings.TrimSpace(milestoneID)
}

func isLinearMilestoneIssue(issue *types.Issue) bool {
	if issue == nil {
		return false
	}
	if issue.ExternalRef != nil && isLinearMilestoneExternalRef(*issue.ExternalRef) {
		return true
	}
	var data struct {
		Linear struct {
			Kind string `json:"kind"`
		} `json:"linear"`
	}
	if len(issue.Metadata) == 0 || json.Unmarshal(issue.Metadata, &data) != nil {
		return false
	}
	return data.Linear.Kind == "project_milestone"
}

// buildLinearPushHooks creates PushHooks for Linear-specific push behavior.
func buildLinearPushHooks(ctx context.Context, lt *linear.Tracker, allowProjectCreates bool) *tracker.PushHooks {
	config := lt.MappingConfig()
	return &tracker.PushHooks{
		FormatDescription: buildLinearDescriptionFormatter(),
		ContentEqual:      buildLinearContentEqual(ctx, lt, config),
		BuildStateCache:   buildLinearStateCache(lt),
		ResolveState:      resolveLinearState,
		ShouldPush:        buildLinearShouldPush(ctx, allowProjectCreates),
	}
}

func buildLinearDescriptionFormatter() func(*types.Issue) string {
	return linear.BuildLinearDescription
}

func buildLinearContentEqual(ctx context.Context, lt *linear.Tracker, config *linear.MappingConfig) func(*types.Issue, *tracker.TrackerIssue) bool {
	loadLabelCache := buildLinearPushLabelCacheLoader(ctx, lt)
	return func(local *types.Issue, remote *tracker.TrackerIssue) bool {
		remoteIssue, ok := remote.Raw.(*linear.Issue)
		if ok && remoteIssue != nil {
			return linear.PushFieldsEqual(local, remoteIssue, config, loadLabelCache())
		}
		remoteConv := lt.FieldMapper().IssueToBeads(remote)
		if remoteConv == nil || remoteConv.Issue == nil {
			return false
		}
		return linear.PushFieldsEqualToBeads(local, remoteConv.Issue)
	}
}

func buildLinearPushLabelCacheLoader(ctx context.Context, lt *linear.Tracker) func() *linear.LabelCache {
	var labelOnce sync.Once
	var labelCache *linear.LabelCache
	var labelCacheErr error
	return func() *linear.LabelCache {
		labelOnce.Do(func() {
			labelCache, labelCacheErr = linear.BuildLabelCacheFromTracker(ctx, lt)
		})
		if labelCacheErr != nil {
			return nil
		}
		return labelCache
	}
}

func buildLinearStateCache(lt *linear.Tracker) func(context.Context) (interface{}, error) {
	return func(ctx context.Context) (interface{}, error) {
		return linear.BuildStateCacheFromTracker(ctx, lt)
	}
}

func resolveLinearState(cache interface{}, status types.Status) (string, bool) {
	sc, ok := cache.(*linear.StateCache)
	if !ok || sc == nil {
		return "", false
	}
	id := sc.FindStateForBeadsStatus(status)
	return id, id != ""
}

func buildLinearShouldPush(ctx context.Context, allowProjectCreates bool) func(*types.Issue) bool {
	return func(issue *types.Issue) bool {
		if isLinearMilestoneIssue(issue) || !linearProjectAllowsPush(ctx, issue, allowProjectCreates) {
			return false
		}
		return linearPushPrefixMatches(ctx, issue)
	}
}

func linearProjectAllowsPush(ctx context.Context, issue *types.Issue, allowProjectCreates bool) bool {
	projectID, _ := getStore().GetConfig(ctx, "linear.project_id")
	if projectID == "" || issue.ExternalRef != nil && strings.TrimSpace(*issue.ExternalRef) != "" {
		return true
	}
	return allowProjectCreates
}

func linearPushPrefixMatches(ctx context.Context, issue *types.Issue) bool {
	pushPrefix, _ := getStore().GetConfig(ctx, "linear.push_prefix")
	if pushPrefix == "" {
		return true
	}
	for _, prefix := range strings.Split(pushPrefix, ",") {
		prefix = strings.TrimSuffix(strings.TrimSpace(prefix), "-")
		if prefix != "" && strings.HasPrefix(issue.ID, prefix+"-") {
			return true
		}
	}
	return false
}
