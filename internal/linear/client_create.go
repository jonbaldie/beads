package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// issueCreateMutation is the GraphQL mutation for creating a Linear issue.
const issueCreateMutation = `
	mutation CreateIssue($input: IssueCreateInput!) {
		issueCreate(input: $input) {
			success
			issue {
				id
				identifier
				title
				description
				url
				priority
				state {
					id
					name
					type
				}
				createdAt
				updatedAt
			}
		}
	}
`

// buildIssueCreateInput constructs the GraphQL input map for an issueCreate mutation.
func (c *clientIssueCreator) buildIssueCreateInput(title, description string, priority int, stateID string, labelIDs []string) map[string]interface{} {
	input := map[string]interface{}{
		"teamId":      c.TeamID,
		"title":       title,
		"description": description,
	}
	if c.ProjectID != "" {
		input["projectId"] = c.ProjectID
	}
	if priority > 0 {
		input["priority"] = priority
	}
	if stateID != "" {
		input["stateId"] = stateID
	}
	if len(labelIDs) > 0 {
		input["labelIds"] = labelIDs
	}
	return input
}

// CreateIssue creates a new issue in Linear.
func (c *clientIssueCreator) CreateIssue(ctx context.Context, title, description string, priority int, stateID string, labelIDs []string) (*Issue, error) {
	req := &GraphQLRequest{
		Query:     issueCreateMutation,
		Variables: map[string]interface{}{"input": c.buildIssueCreateInput(title, description, priority, stateID, labelIDs)},
	}

	data, err := c.Execute(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create issue: %w", err)
	}

	var createResp IssueCreateResponse
	if err := json.Unmarshal(data, &createResp); err != nil {
		return nil, fmt.Errorf("failed to parse create response: %w", err)
	}

	if !createResp.IssueCreate.Success {
		return nil, fmt.Errorf("issue creation reported as unsuccessful")
	}

	return &createResp.IssueCreate.Issue, nil
}

// createIssueSingleAttempt executes the issueCreate mutation exactly once,
// without the retry loop used by Execute. This is intentional: retrying a
// mutation that may have already reached Linear risks creating a duplicate.
// The caller (CreateIssueIdempotent) handles retry safety by re-searching for
// the idempotency marker after any failure.
func (c *clientIssueCreator) createIssueSingleAttempt(ctx context.Context, title, description string, priority int, stateID string, labelIDs []string) (*Issue, error) {
	req := &GraphQLRequest{
		Query:     issueCreateMutation,
		Variables: map[string]interface{}{"input": c.buildIssueCreateInput(title, description, priority, stateID, labelIDs)},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := c.newRequest(ctx, body)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTPClient.Do(httpReq) //nolint:gosec // G704: endpoint is the explicitly configured Linear API endpoint.
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	respBody, err := readLinearResponseBody(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API error: %s (status %d)", string(respBody), resp.StatusCode)
	}

	data, err := parseLinearGraphQLResponse(respBody)
	if err != nil {
		return nil, err
	}
	return parseIssueCreateResponse(data)
}

func parseIssueCreateResponse(data json.RawMessage) (*Issue, error) {
	var createResp IssueCreateResponse
	if err := json.Unmarshal(data, &createResp); err != nil {
		return nil, fmt.Errorf("failed to parse create response: %w", err)
	}
	if !createResp.IssueCreate.Success {
		return nil, fmt.Errorf("issue creation reported as unsuccessful")
	}
	return &createResp.IssueCreate.Issue, nil
}

// CreateIssueIdempotent creates a new Linear issue with dedup protection.
// It embeds the given idempotency marker in the description and, before
// creating, queries Linear to see if an issue with that marker already exists.
// If a match is found (e.g., from a prior interrupted sync), the existing
// issue is returned without creating a duplicate.
//
// The create is performed as a single attempt (no internal retry) to avoid the
// following race: if issueCreate reaches Linear but the HTTP response is lost
// (network timeout, connection drop), a blind retry would create a second issue
// with the same marker. Instead, after any create failure, this function
// re-searches for the marker so that the caller can safely retry the entire
// CreateIssueIdempotent call and get a consistent result.
//
// Note: concurrent creates from multiple sources (e.g., two sync processes
// running simultaneously) cannot be made fully atomic without server-side
// uniqueness enforcement, which Linear does not provide. The dedup window is
// bounded by Linear's search-index propagation delay.
func (c *clientIssueCreator) CreateIssueIdempotent(ctx context.Context, title, description string, priority int, stateID string, labelIDs []string, marker string) (*Issue, bool, error) {
	existing, err := findIssueByDescriptionContains(c.clientTransport, ctx, marker)
	if err != nil {
		return nil, false, fmt.Errorf("idempotency check failed: %w", err)
	}
	if existing != nil {
		return existing, true, nil
	}

	description = AppendIdempotencyMarker(description, marker)
	issue, err := c.createIssueSingleAttempt(ctx, title, description, priority, stateID, labelIDs)
	if err != nil {
		// The mutation may have reached Linear despite the error. Re-check for
		// the marker so callers retrying CreateIssueIdempotent get a consistent
		// result rather than creating a duplicate.
		if found, searchErr := findIssueByDescriptionContains(c.clientTransport, ctx, marker); searchErr == nil && found != nil {
			return found, true, nil
		}
		return nil, false, err
	}
	return issue, false, nil
}

const issueBatchCreateMutation = `
	mutation BatchCreateIssues($input: IssueBatchCreateInput!) {
		issueBatchCreate(input: $input) {
			success
			issues {
				id
				identifier
				title
				url
				priority
				state {
					id
					name
					type
				}
				createdAt
				updatedAt
			}
		}
	}
`

// UpdateIssue updates an existing issue in Linear.
func (c *clientIssueUpdater) UpdateIssue(ctx context.Context, issueID string, updates map[string]interface{}) (*Issue, error) {
	return updateIssue(c.clientTransport, ctx, issueID, updates)
}

func updateIssue(c *clientTransport, ctx context.Context, issueID string, updates map[string]interface{}) (*Issue, error) {
	query := `
		mutation UpdateIssue($id: String!, $input: IssueUpdateInput!) {
			issueUpdate(id: $id, input: $input) {
				success
				issue {
					id
					identifier
					title
					description
					url
					priority
					state {
						id
						name
						type
					}
					updatedAt
				}
			}
		}
	`

	req := &GraphQLRequest{
		Query: query,
		Variables: map[string]interface{}{
			"id":    issueID,
			"input": updates,
		},
	}

	data, err := c.Execute(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to update issue: %w", err)
	}

	var updateResp IssueUpdateResponse
	if err := json.Unmarshal(data, &updateResp); err != nil {
		return nil, fmt.Errorf("failed to parse update response: %w", err)
	}

	if !updateResp.IssueUpdate.Success {
		return nil, fmt.Errorf("issue update reported as unsuccessful")
	}

	return &updateResp.IssueUpdate.Issue, nil
}

// BatchCreateIssues creates multiple issues in Linear using the issueBatchCreate mutation.
// Inputs are chunked into groups of BatchSize (50).
//
// On ambiguous failure (API error or success=false), this method does NOT blindly
// retry the full chunk—Linear may have partially applied the mutation. Instead it
// searches for each issue's idempotency marker (embedded in the description) to
// discover which issues were actually created, and returns an error for the rest.
func (c *clientIssueBatchCreator) BatchCreateIssues(ctx context.Context, inputs []IssueCreateInput) ([]Issue, error) {
	if len(inputs) == 0 {
		return nil, nil
	}

	var allIssues []Issue
	for _, batch := range linearBatchRanges(len(inputs)) {
		chunk := inputs[batch.start:batch.end]
		issues, err := c.createIssueBatch(ctx, chunk)
		allIssues = append(allIssues, issues...)
		if err != nil {
			return allIssues, err
		}
	}

	return allIssues, nil
}

type linearBatchRange struct {
	start int
	end   int
}

func linearBatchRanges(total int) []linearBatchRange {
	ranges := make([]linearBatchRange, 0, (total+BatchSize-1)/BatchSize)
	for start := 0; start < total; start += BatchSize {
		end := start + BatchSize
		if end > total {
			end = total
		}
		ranges = append(ranges, linearBatchRange{start: start, end: end})
	}
	return ranges
}

func (c *clientIssueBatchCreator) createIssueBatch(ctx context.Context, chunk []IssueCreateInput) ([]Issue, error) {
	req := &GraphQLRequest{
		Query: issueBatchCreateMutation,
		Variables: map[string]interface{}{
			"input": map[string]interface{}{
				"issues": chunk,
			},
		},
	}
	data, err := c.Execute(ctx, req)
	if err != nil {
		return c.recoverFailedIssueBatch(ctx, chunk, err, true)
	}

	var batchResp IssueBatchCreateResponse
	if err := json.Unmarshal(data, &batchResp); err != nil {
		return nil, fmt.Errorf("failed to parse batch create response: %w", err)
	}
	if batchResp.IssueBatchCreate.Success {
		return batchResp.IssueBatchCreate.Issues, nil
	}
	return c.recoverFailedIssueBatch(ctx, chunk, nil, false)
}

func (c *clientIssueBatchCreator) recoverFailedIssueBatch(ctx context.Context, chunk []IssueCreateInput, batchErr error, failedRequest bool) ([]Issue, error) {
	found, recoverErr := c.recoverAfterAmbiguousBatch(ctx, chunk)
	if recoverErr != nil {
		if failedRequest {
			return found, fmt.Errorf("batch create failed and recovery search also failed: %w (batch error: %v)", recoverErr, batchErr)
		}
		return found, fmt.Errorf("batch create unsuccessful and recovery search also failed: %w", recoverErr)
	}
	if len(found) == len(chunk) {
		return found, nil
	}
	if failedRequest {
		return found, fmt.Errorf("batch create failed; %d of %d issues unconfirmed (batch error: %v)", len(chunk)-len(found), len(chunk), batchErr)
	}
	return found, fmt.Errorf("batch create unsuccessful; %d of %d issues unconfirmed", len(chunk)-len(found), len(chunk))
}

// recoverAfterAmbiguousBatch searches Linear for each issue in a failed batch
// chunk to determine which were actually created. It looks for the idempotency
// marker (<!-- bd-idempotency: ... -->) embedded in each input's description.
// Returns only the issues confirmed to exist in Linear.
func (c *clientIssueBatchCreator) recoverAfterAmbiguousBatch(ctx context.Context, chunk []IssueCreateInput) ([]Issue, error) {
	var found []Issue
	for _, input := range chunk {
		marker := extractIdempotencyMarker(input.Description)
		if marker == "" {
			continue
		}
		existing, err := findIssueByDescriptionContains(c.clientTransport, ctx, marker)
		if err != nil {
			return found, fmt.Errorf("recovery search failed for %q: %w", input.Title, err)
		}
		if existing != nil {
			found = append(found, *existing)
		}
	}
	return found, nil
}

// extractIdempotencyMarker extracts the bd-idempotency HTML comment from a
// description string. Returns "" if no marker is found.
func extractIdempotencyMarker(description string) string {
	idx := strings.Index(description, idempotencyPrefix)
	if idx < 0 {
		return ""
	}
	end := strings.Index(description[idx:], idempotencySuffix)
	if end < 0 {
		return ""
	}
	return description[idx : idx+end+len(idempotencySuffix)]
}

const issueBatchUpdateMutation = `
	mutation BatchUpdateIssues($ids: [UUID!]!, $input: IssueUpdateInput!) {
		issueBatchUpdate(ids: $ids, input: $input) {
			success
			issues {
				id
				identifier
				title
				url
				priority
				state {
					id
					name
					type
				}
				updatedAt
			}
		}
	}
`

// BatchUpdateIssues updates multiple issues in Linear using the issueBatchUpdate mutation.
// This applies the SAME update to all specified issue IDs per call. IDs are chunked
// into groups of BatchSize (50). If a batch call fails, it falls back to per-issue
// UpdateIssue for that chunk.
func (c *clientIssueBatchUpdater) BatchUpdateIssues(ctx context.Context, ids []string, updates map[string]interface{}) ([]Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	var allIssues []Issue
	for _, batch := range linearBatchRanges(len(ids)) {
		chunk := ids[batch.start:batch.end]
		issues, err := c.updateIssueBatch(ctx, chunk, updates)
		allIssues = append(allIssues, issues...)
		if err != nil {
			return allIssues, err
		}
	}

	return allIssues, nil
}

func (c *clientIssueBatchUpdater) updateIssueBatch(ctx context.Context, chunk []string, updates map[string]interface{}) ([]Issue, error) {
	req := &GraphQLRequest{
		Query: issueBatchUpdateMutation,
		Variables: map[string]interface{}{
			"ids":   chunk,
			"input": updates,
		},
	}
	data, err := c.Execute(ctx, req)
	if err != nil {
		return c.fallbackIssueBatchUpdate(ctx, chunk, updates, err)
	}

	var batchResp IssueBatchUpdateResponse
	if err := json.Unmarshal(data, &batchResp); err != nil {
		return nil, fmt.Errorf("failed to parse batch update response: %w", err)
	}
	if batchResp.IssueBatchUpdate.Success {
		return batchResp.IssueBatchUpdate.Issues, nil
	}
	return c.fallbackIssueBatchUpdate(ctx, chunk, updates, nil)
}

func (c *clientIssueBatchUpdater) fallbackIssueBatchUpdate(ctx context.Context, ids []string, updates map[string]interface{}, batchErr error) ([]Issue, error) {
	var issues []Issue
	for _, id := range ids {
		issue, updateErr := updateIssue(c.clientTransport, ctx, id, updates)
		if updateErr != nil {
			if batchErr != nil {
				return issues, fmt.Errorf("batch update failed, single-issue fallback also failed for %s: %w (batch error: %v)", id, updateErr, batchErr)
			}
			return issues, fmt.Errorf("batch update unsuccessful, single-issue fallback also failed for %s: %w", id, updateErr)
		}
		issues = append(issues, *issue)
	}
	return issues, nil
}
