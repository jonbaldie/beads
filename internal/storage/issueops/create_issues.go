package issueops

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// CreateIssuesResult reports side effects that callers need for selective
// Dolt staging after CreateIssuesInTxWithResult returns.
type CreateIssuesResult struct {
	ChangedTables             map[string]bool
	ChangedChildCounterTables map[string]bool
}

func (r *CreateIssuesResult) markChanged(table string) {
	if table == "" {
		return
	}
	if r.ChangedTables == nil {
		r.ChangedTables = map[string]bool{}
	}
	r.ChangedTables[table] = true
}

func (r *CreateIssuesResult) merge(changed map[string]bool) {
	r.ChangedTables = mergeChangedTables(r.ChangedTables, changed)
}

func CreateIssuesInTx(ctx context.Context, tx DBTX, issues []*types.Issue, actor string, opts storage.BatchCreateOptions) error {
	_, err := CreateIssuesInTxWithResult(ctx, tx, issues, actor, opts)
	return err
}

// CreateIssuesInTxWithResult creates issues and reports tables whose writes are
// only knowable after SQL reconciliation, such as child counter advances.
func CreateIssuesInTxWithResult(ctx context.Context, tx DBTX, issues []*types.Issue, actor string, opts storage.BatchCreateOptions) (CreateIssuesResult, error) {
	bc, err := NewBatchContext(ctx, tx, opts)
	if err != nil {
		return CreateIssuesResult{}, err
	}
	return CreateIssuesInTxWithContext(ctx, tx, bc, issues, actor)
}

// CreateIssuesInTxWithContext is CreateIssuesInTxWithResult with a
// caller-supplied BatchContext. Callers that split config reads from row
// writes across SQL sessions (doltTransaction's wisp tier) build the context
// on the session that sees in-transaction config writes and pass it here.
// The caller's bc is not modified, so one context can serve several calls.
func CreateIssuesInTxWithContext(ctx context.Context, tx DBTX, bc *BatchContext, issues []*types.Issue, actor string) (CreateIssuesResult, error) {
	filteredIssues, err := filterCreateIssuesMixedBucketDependencies(issues, bc.Opts)
	if err != nil {
		return CreateIssuesResult{}, err
	}
	result, accepted, err := createAcceptedIssues(ctx, tx, bc, filteredIssues, actor)
	if err != nil {
		return CreateIssuesResult{}, err
	}
	return finalizeCreatedIssues(ctx, tx, bc, accepted, actor, result)
}

func createAcceptedIssues(ctx context.Context, tx DBTX, bc *BatchContext, issues []*types.Issue, actor string) (CreateIssuesResult, []*types.Issue, error) {
	// This function already runs a slice-wide ReconcileChildCounters below,
	// covering every accepted issue; skip the redundant per-issue reconcile.
	// Set the flag on a shallow copy so the caller's context keeps its own
	// reconcile behavior.
	batch := *bc
	batch.SkipChildCounterReconcile = true

	result := CreateIssuesResult{}
	accepted := issues[:0:0]
	for _, issue := range issues {
		issueResult, err := CreateIssueInTxWithResult(ctx, tx, &batch, issue, actor)
		if err != nil {
			return CreateIssuesResult{}, nil, err
		}
		result.merge(issueResult.ChangedTables)
		if issueResult.StaleRejected {
			continue // stale snapshot: keep its deps out of the batch too
		}
		accepted = append(accepted, issue)
	}
	return result, accepted, nil
}

func finalizeCreatedIssues(ctx context.Context, tx DBTX, bc *BatchContext, issues []*types.Issue, actor string, result CreateIssuesResult) (CreateIssuesResult, error) {
	depResult, err := PersistDependenciesWithOptionsResult(ctx, tx, issues, actor, bc.Opts)
	if err != nil {
		return CreateIssuesResult{}, err
	}
	result.merge(depResult.ChangedTables)

	changedCounters, err := ReconcileChildCounters(ctx, tx, issues)
	if err != nil {
		return CreateIssuesResult{}, err
	}
	result.ChangedChildCounterTables = changedCounters
	for table := range changedCounters {
		result.markChanged(table)
	}
	issueIDs, wispIDs, err := createBlockedRecomputeIDs(ctx, tx, issues, depResult.persistedDependencies)
	if err != nil {
		return CreateIssuesResult{}, err
	}
	recomputed, err := RecomputeIsBlockedInTxWithResult(ctx, tx, issueIDs, wispIDs)
	if err != nil {
		return CreateIssuesResult{}, err
	}
	if recomputed.IssueRowsChanged {
		result.markChanged("issues")
	}
	if recomputed.WispRowsChanged {
		result.markChanged("wisps")
	}
	return result, nil
}

// CreateIssueDirtyTables returns the regular Dolt tables CreateIssueInTx may
// dirty for the given issue. Wisp tables are intentionally omitted because they
// are Dolt-ignored and cannot be staged.
func CreateIssueDirtyTables(ctx context.Context, issue *types.Issue, result CreateIssueResult) map[string]bool {
	dirty := stageableChangedTables(result.ChangedTables)
	if issue == nil {
		return dirty
	}
	if parentID, childNum, ok := ParseHierarchicalID(issue.ID); ok &&
		storage.HasReservedChildCounter(ctx, parentID, childNum) {
		dirty["child_counters"] = true
	}
	return dirty
}

// CreateIssuesDirtyTables returns the regular Dolt tables CreateIssuesInTx may
// dirty, including child counters that reconciliation actually advanced.
func CreateIssuesDirtyTables(ctx context.Context, issues []*types.Issue, result CreateIssuesResult) map[string]bool {
	dirty := stageableChangedTables(result.ChangedTables)
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		if parentID, childNum, ok := ParseHierarchicalID(issue.ID); ok &&
			storage.HasReservedChildCounter(ctx, parentID, childNum) {
			dirty["child_counters"] = true
		}
	}
	return dirty
}

func stageableChangedTables(changed map[string]bool) map[string]bool {
	dirty := map[string]bool{}
	for table := range changed {
		if table == "wisps" || strings.HasPrefix(table, "wisp_") {
			continue
		}
		dirty[table] = true
	}
	return dirty
}

// ValidateCreateIssuesMixedBucketDependencies rejects same-batch dependency
// edges between regular issues and wisps. Dependencies are stored in separate
// backing tables per bucket, so a batch cannot create both ends atomically when
// the edge crosses buckets.
func ValidateCreateIssuesMixedBucketDependencies(issues []*types.Issue) error {
	_, err := filterCreateIssuesMixedBucketDependencies(issues, storage.BatchCreateOptions{})
	return err
}

// FilterCreateIssuesMixedBucketDependencies applies the same cross-bucket
// dependency policy as CreateIssuesInTx, but over the full issue set. Callers
// that split one logical batch into bounded sub-batches (chunked import) must
// run this once up front: the per-batch filter inside the engine only sees one
// sub-batch, so it could no longer detect an edge whose endpoints land in
// different chunks. Filtered edges are reported via opts.OnSkippedDependency;
// issues whose dependency list changes are copied, never mutated.
func FilterCreateIssuesMixedBucketDependencies(issues []*types.Issue, opts storage.BatchCreateOptions) ([]*types.Issue, error) {
	return filterCreateIssuesMixedBucketDependencies(issues, opts)
}

func filterCreateIssuesMixedBucketDependencies(issues []*types.Issue, opts storage.BatchCreateOptions) ([]*types.Issue, error) {
	batchWispByID, hasRegular, hasWisp := mixedBucketMembership(issues)
	if !hasRegular || !hasWisp {
		return issues, nil
	}

	var filteredIssues []*types.Issue
	for issueIndex, issue := range issues {
		if issue == nil {
			continue
		}
		filteredIssue, filtered, err := filterMixedBucketIssue(issue, batchWispByID, opts)
		if err != nil {
			return nil, err
		}
		if filtered {
			if filteredIssues == nil {
				filteredIssues = append([]*types.Issue(nil), issues...)
			}
			filteredIssues[issueIndex] = filteredIssue
		}
	}
	if filteredIssues != nil {
		return filteredIssues, nil
	}
	return issues, nil
}

func mixedBucketMembership(issues []*types.Issue) (map[string]bool, bool, bool) {
	batchWispByID := make(map[string]bool, len(issues))
	var hasRegular, hasWisp bool
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		isWisp := IsWisp(issue)
		hasWisp = hasWisp || isWisp
		hasRegular = hasRegular || !isWisp
		if issue.ID != "" {
			batchWispByID[issue.ID] = isWisp
		}
	}
	return batchWispByID, hasRegular, hasWisp
}

func filterMixedBucketIssue(issue *types.Issue, batchWispByID map[string]bool, opts storage.BatchCreateOptions) (*types.Issue, bool, error) {
	var keptDeps []*types.Dependency
	filteredDeps := false
	for depIndex, dep := range issue.Dependencies {
		if dep == nil {
			if filteredDeps {
				keptDeps = append(keptDeps, dep)
			}
			continue
		}
		sourceID, crossBucket := mixedBucketDependency(issue, dep, batchWispByID)
		if !crossBucket {
			if filteredDeps {
				keptDeps = append(keptDeps, dep)
			}
			continue
		}
		if !opts.SkipDependencyValidationErrors {
			// Through the shared constructor, so the two bodies raise one
			// message AND one sentinel: the role promises this refusal is the
			// caller's fault, and an untyped error left callers classifying it
			// by prose.
			return nil, false, CrossPlaneBatchEdgeError(sourceID, dep.DependsOnID)
		}
		if !filteredDeps {
			keptDeps = append([]*types.Dependency(nil), issue.Dependencies[:depIndex]...)
			filteredDeps = true
		}
		recordSkippedDependencyEdge(opts, sourceID, dep.DependsOnID, "cross-bucket dependency between regular issue and wisp in the same batch")
	}
	if !filteredDeps {
		return issue, false, nil
	}
	issueCopy := *issue
	issueCopy.Dependencies = keptDeps
	return &issueCopy, true, nil
}

func mixedBucketDependency(issue *types.Issue, dep *types.Dependency, batchWispByID map[string]bool) (string, bool) {
	sourceID := issue.ID
	sourceIsWisp := IsWisp(issue)
	if dep.IssueID != "" {
		sourceID = dep.IssueID
		if isWisp, ok := batchWispByID[sourceID]; ok {
			sourceIsWisp = isWisp
		}
	}
	targetIsWisp, targetInBatch := batchWispByID[dep.DependsOnID]
	return sourceID, targetInBatch && sourceIsWisp != targetIsWisp
}

func createBlockedRecomputeIDs(ctx context.Context, tx DBTX, issues []*types.Issue, dependencies []persistedDependency) ([]string, []string, error) {
	issueSeen := make(map[string]bool, len(issues))
	wispSeen := make(map[string]bool, len(issues))
	issueIDs := make([]string, 0, len(issues))
	wispIDs := make([]string, 0, len(issues))
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		if IsWisp(issue) {
			wispIDs = appendRecomputeID(wispIDs, wispSeen, issue.ID)
		} else {
			issueIDs = appendRecomputeID(issueIDs, issueSeen, issue.ID)
		}
	}
	for _, dependency := range dependencies {
		affectedIssues, affectedWisps, err := affectedByCreatedDependency(ctx, tx, dependency)
		if err != nil {
			return nil, nil, fmt.Errorf("affected by created dependency %s -> %s: %w", dependency.source, dependency.target, err)
		}
		for _, id := range affectedIssues {
			issueIDs = appendRecomputeID(issueIDs, issueSeen, id)
		}
		for _, id := range affectedWisps {
			wispIDs = appendRecomputeID(wispIDs, wispSeen, id)
		}
	}
	return issueIDs, wispIDs, nil
}

func appendRecomputeID(ids []string, seen map[string]bool, id string) []string {
	if id == "" || seen[id] {
		return ids
	}
	seen[id] = true
	return append(ids, id)
}

func affectedByCreatedDependency(ctx context.Context, tx DBTX, dependency persistedDependency) ([]string, []string, error) {
	if dependency.sourceWisp {
		return AffectedByDepChangeForWispInTx(ctx, tx, dependency.source, dependency.target, dependency.depType)
	}
	return AffectedByDepChangeInTx(ctx, tx, dependency.source, dependency.target, dependency.depType)
}
