package linear

import (
	"context"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
)

// BatchPush implements tracker.BatchPushTracker. It partitions issues into
// creates and updates, uses issueBatchCreate for new issues (chunked at 50),
// and falls back to per-issue UpdateIssue for updates (since issueBatchUpdate
// applies the same fields to all IDs, which doesn't fit per-issue field diffs).
//
// Skip semantics: existing issues are fetched and compared with PushFieldsEqual
// before updating; unchanged issues are skipped. forceIDs bypasses this check.
//
// Multi-team: state IDs are resolved using the per-team workflow state cache,
// so updates to issues belonging to different teams use the correct state list.
//
// Result mapping: batch-create results are matched by title rather than array
// index, since Linear's API does not guarantee response order matches input order.
func (t *trackerIssues) BatchPush(ctx context.Context, issues []*types.Issue, forceIDs map[string]bool) (*tracker.BatchPushResult, error) {
	batch, err := newLinearBatchContext(ctx, t.owner)
	if err != nil {
		return nil, err
	}
	return runLinearBatchPush(ctx, batch, issues, forceIDs), nil
}

type linearBatchContext struct {
	tracker           *Tracker
	client            *Client
	teamCaches        map[string]*StateCache
	primaryCache      *StateCache
	teamLabelCaches   map[string]*LabelCache
	primaryLabelCache *LabelCache
}

func newLinearBatchContext(ctx context.Context, t *Tracker) (linearBatchContext, error) {
	client := t.primaryClient()
	if client == nil {
		return linearBatchContext{}, fmt.Errorf("no Linear client available")
	}
	teamCaches, err := buildLinearStateCaches(ctx, t)
	if err != nil {
		return linearBatchContext{}, err
	}
	primaryCache := teamCaches[t.teamIDs[0]]
	if primaryCache == nil {
		return linearBatchContext{}, fmt.Errorf("building state cache: no cache for primary team %s", t.teamIDs[0])
	}
	teamLabelCaches, err := buildLinearLabelCaches(ctx, t)
	if err != nil {
		return linearBatchContext{}, err
	}
	primaryLabelCache := teamLabelCaches[t.teamIDs[0]]
	if primaryLabelCache == nil {
		return linearBatchContext{}, fmt.Errorf("building label cache: no cache for primary team %s", t.teamIDs[0])
	}
	return linearBatchContext{
		tracker:           t,
		client:            client,
		teamCaches:        teamCaches,
		primaryCache:      primaryCache,
		teamLabelCaches:   teamLabelCaches,
		primaryLabelCache: primaryLabelCache,
	}, nil
}

func buildLinearStateCaches(ctx context.Context, t *Tracker) (map[string]*StateCache, error) {
	caches := make(map[string]*StateCache, len(t.teamIDs))
	for _, teamID := range t.teamIDs {
		client := t.clients[teamID]
		if client == nil {
			continue
		}
		cache, err := BuildStateCache(ctx, client)
		if err != nil {
			return nil, fmt.Errorf("building state cache for team %s: %w", teamID, err)
		}
		caches[teamID] = cache
	}
	return caches, nil
}

func buildLinearLabelCaches(ctx context.Context, t *Tracker) (map[string]*LabelCache, error) {
	caches := make(map[string]*LabelCache, len(t.teamIDs))
	for _, teamID := range t.teamIDs {
		client := t.clients[teamID]
		if client == nil {
			continue
		}
		cache, err := BuildLabelCache(ctx, client)
		if err != nil {
			return nil, fmt.Errorf("building label cache for team %s: %w", teamID, err)
		}
		caches[teamID] = cache
	}
	return caches, nil
}

func runLinearBatchPush(ctx context.Context, batch linearBatchContext, issues []*types.Issue, forceIDs map[string]bool) *tracker.BatchPushResult {
	result := &tracker.BatchPushResult{}
	toCreate, toUpdate := partitionLinearBatchIssues(issues)
	createLinearBatchIssues(ctx, batch, toCreate, result)
	updateLinearBatchIssues(ctx, batch, toUpdate, forceIDs, result)
	return result
}

func partitionLinearBatchIssues(issues []*types.Issue) (toCreate, toUpdate []*types.Issue) {
	for _, issue := range issues {
		extRef := ""
		if issue.ExternalRef != nil {
			extRef = *issue.ExternalRef
		}
		if extRef == "" || !IsLinearExternalRef(extRef) {
			toCreate = append(toCreate, issue)
			continue
		}
		toUpdate = append(toUpdate, issue)
	}
	return toCreate, toUpdate
}

func createLinearBatchIssues(ctx context.Context, batch linearBatchContext, issues []*types.Issue, result *tracker.BatchPushResult) {
	if len(issues) == 0 {
		return
	}
	singleIssues, batchIssues := splitLinearCreateIssues(issues)
	appendLinearSingleCreates(ctx, batch, singleIssues, result)
	inputs, titleToIssue := buildLinearBatchInputs(batch, batchIssues, result)
	appendLinearBatchCreates(ctx, batch.client, inputs, titleToIssue, result)
}

func splitLinearCreateIssues(issues []*types.Issue) (singleIssues, batchIssues []*types.Issue) {
	titleCount := make(map[string]int, len(issues))
	for _, issue := range issues {
		titleCount[issue.Title]++
	}
	for _, issue := range issues {
		if titleCount[issue.Title] > 1 {
			singleIssues = append(singleIssues, issue)
			continue
		}
		batchIssues = append(batchIssues, issue)
	}
	return singleIssues, batchIssues
}

func appendLinearSingleCreates(ctx context.Context, batch linearBatchContext, issues []*types.Issue, result *tracker.BatchPushResult) {
	for _, issue := range issues {
		created, err := createSingleLinearIssue(ctx, batch, issue, result)
		if err != nil {
			result.Errors = append(result.Errors, tracker.BatchPushError{LocalID: issue.ID, Message: err.Error()})
			continue
		}
		result.Created = append(result.Created, tracker.BatchPushItem{LocalID: issue.ID, ExternalRef: created.URL})
	}
}

func createSingleLinearIssue(ctx context.Context, batch linearBatchContext, issue *types.Issue, result *tracker.BatchPushResult) (*Issue, error) {
	priority := PriorityToLinear(issue.Priority, batch.tracker.config)
	stateID, err := ResolveStateIDForBeadsStatus(batch.primaryCache, issue.Status, batch.tracker.config)
	if err != nil {
		return nil, fmt.Errorf("resolving state for status %s: %v", issue.Status, err)
	}
	labelIDs := linearBatchLabelIDs(issue, batch.primaryLabelCache, batch.tracker.config, result)
	marker := GenerateIdempotencyMarker(issue.ID, issue.CreatedBy, issue.CreatedAt.UnixNano())
	created, _, err := batch.client.CreateIssueIdempotent(ctx, issue.Title, issue.Description, priority, stateID, labelIDs, marker)
	if err != nil {
		return nil, fmt.Errorf("single create (dup title) for %q: %v", issue.Title, err)
	}
	return created, nil
}

func buildLinearBatchInputs(batch linearBatchContext, issues []*types.Issue, result *tracker.BatchPushResult) ([]IssueCreateInput, map[string]*types.Issue) {
	inputs := make([]IssueCreateInput, 0, len(issues))
	titleToIssue := make(map[string]*types.Issue, len(issues))
	for _, issue := range issues {
		priority := PriorityToLinear(issue.Priority, batch.tracker.config)
		stateID, err := ResolveStateIDForBeadsStatus(batch.primaryCache, issue.Status, batch.tracker.config)
		if err != nil {
			result.Errors = append(result.Errors, tracker.BatchPushError{
				LocalID: issue.ID,
				Message: fmt.Sprintf("resolving state for status %s: %v", issue.Status, err),
			})
			continue
		}
		labelIDs := linearBatchLabelIDs(issue, batch.primaryLabelCache, batch.tracker.config, result)
		input := IssueCreateInput{
			TeamID:      batch.client.TeamID,
			Title:       issue.Title,
			Description: AppendIdempotencyMarker(issue.Description, GenerateIdempotencyMarker(issue.ID, issue.CreatedBy, issue.CreatedAt.UnixNano())),
			Priority:    priority,
			StateID:     stateID,
			LabelIDs:    labelIDs,
		}
		if batch.client.ProjectID != "" {
			input.ProjectID = batch.client.ProjectID
		}
		titleToIssue[issue.Title] = issue
		inputs = append(inputs, input)
	}
	return inputs, titleToIssue
}

func linearBatchLabelIDs(issue *types.Issue, cache *LabelCache, cfg *MappingConfig, result *tracker.BatchPushResult) []string {
	labelIDs, unknown := ResolveLabelIDs(issue, cache, cfg)
	for _, name := range unknown {
		msg := fmt.Sprintf("linear: bead %s: label %q not found on Linear team (skipped)", issue.ID, name)
		fmt.Fprintf(os.Stderr, "%s\n", msg)
		result.Warnings = append(result.Warnings, msg)
	}
	return labelIDs
}

func appendLinearBatchCreates(ctx context.Context, client *Client, inputs []IssueCreateInput, titleToIssue map[string]*types.Issue, result *tracker.BatchPushResult) {
	if len(inputs) == 0 {
		return
	}
	created, err := client.BatchCreateIssues(ctx, inputs)
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("batch create partial error: %v", err))
	}
	matched := make(map[string]bool, len(created))
	for _, issue := range created {
		localIssue, ok := titleToIssue[issue.Title]
		if !ok {
			result.Warnings = append(result.Warnings, fmt.Sprintf("batch create: response contained unexpected title %q", issue.Title))
			continue
		}
		matched[issue.Title] = true
		result.Created = append(result.Created, tracker.BatchPushItem{LocalID: localIssue.ID, ExternalRef: issue.URL})
	}
	for title, localIssue := range titleToIssue {
		if matched[title] {
			continue
		}
		result.Errors = append(result.Errors, tracker.BatchPushError{
			LocalID: localIssue.ID,
			Message: fmt.Sprintf("not returned in batch create response (title: %q)", title),
		})
	}
}

func updateLinearBatchIssues(ctx context.Context, batch linearBatchContext, issues []*types.Issue, forceIDs map[string]bool, result *tracker.BatchPushResult) {
	for _, issue := range issues {
		updateOneLinearIssue(ctx, batch, issue, forceIDs, result)
	}
}

func updateOneLinearIssue(ctx context.Context, batch linearBatchContext, issue *types.Issue, forceIDs map[string]bool, result *tracker.BatchPushResult) {
	externalID := linearExternalID(*issue.ExternalRef)
	routeClient := batch.tracker.clientForExternalID(ctx, externalID)
	if routeClient == nil {
		result.Errors = append(result.Errors, tracker.BatchPushError{LocalID: issue.ID, Message: fmt.Sprintf("cannot determine Linear team for %s", externalID)})
		return
	}
	teamCache, teamLabelCache := linearUpdateCaches(batch, routeClient)
	remoteIssue, skipped := comparableLinearRemoteIssue(ctx, routeClient, issue, externalID, forceIDs[issue.ID], batch.tracker.config, teamLabelCache)
	if skipped {
		result.Skipped = append(result.Skipped, issue.ID)
		return
	}
	updates, err := linearIssueUpdates(issue, teamCache, teamLabelCache, batch.tracker.config)
	if err != nil {
		result.Errors = append(result.Errors, tracker.BatchPushError{LocalID: issue.ID, Message: fmt.Sprintf("resolving state for status %s: %v", issue.Status, err)})
		return
	}
	issueUUID := linearIssueUUID(ctx, routeClient, externalID, remoteIssue)
	updated, err := routeClient.UpdateIssue(ctx, issueUUID, updates)
	if err != nil {
		result.Errors = append(result.Errors, tracker.BatchPushError{LocalID: issue.ID, Message: fmt.Sprintf("updating %s: %v", externalID, err)})
		return
	}
	result.Updated = append(result.Updated, tracker.BatchPushItem{LocalID: issue.ID, ExternalRef: updated.URL})
}

func linearExternalID(ref string) string {
	if externalID := ExtractLinearIdentifier(ref); externalID != "" {
		return externalID
	}
	return ref
}

func linearUpdateCaches(batch linearBatchContext, client *Client) (*StateCache, *LabelCache) {
	teamCache := batch.teamCaches[client.TeamID]
	if teamCache == nil {
		teamCache = batch.primaryCache
	}
	teamLabelCache := batch.teamLabelCaches[client.TeamID]
	if teamLabelCache == nil {
		teamLabelCache = batch.primaryLabelCache
	}
	return teamCache, teamLabelCache
}

func comparableLinearRemoteIssue(ctx context.Context, client *Client, issue *types.Issue, externalID string, force bool, cfg *MappingConfig, labelCache *LabelCache) (*Issue, bool) {
	if force {
		return nil, false
	}
	remoteIssue, err := client.FetchIssueByIdentifier(ctx, externalID)
	if err != nil || remoteIssue == nil {
		return remoteIssue, false
	}
	comparableIssue := *issue
	comparableIssue.AcceptanceCriteria = ""
	comparableIssue.Design = ""
	comparableIssue.Notes = ""
	return remoteIssue, PushFieldsEqual(&comparableIssue, remoteIssue, cfg, labelCache)
}

func linearIssueUpdates(issue *types.Issue, teamCache *StateCache, labelCache *LabelCache, cfg *MappingConfig) (map[string]interface{}, error) {
	updates := (&linearFieldMapper{config: cfg, labelCache: labelCache}).IssueToTracker(issue)
	stateID, err := ResolveStateIDForBeadsStatus(teamCache, issue.Status, cfg)
	if err != nil {
		return nil, err
	}
	if stateID != "" {
		updates["stateId"] = stateID
	}
	return updates, nil
}

func linearIssueUUID(ctx context.Context, client *Client, externalID string, remoteIssue *Issue) string {
	if remoteIssue != nil {
		return remoteIssue.ID
	}
	if issue, err := client.FetchIssueByIdentifier(ctx, externalID); err == nil && issue != nil {
		return issue.ID
	}
	return externalID
}
