package issueops

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	publicops "github.com/jonbaldie/beads/issueops"
)

// HistoryEntry names the version-control entry a guarded mutation records: the
// caller's own provenance label when it supplied one, otherwise the
// implementation's default. It decides how the entry READS, never whether one
// is recorded — that stays a question about what the mutation wrote.
//
// Every guarded mutation whose request carries a label calls this. Create is
// the one that does not, and the reason is NOT that no surface names the
// created issue — several do (internal/storage/dolt/issues.go writes
// "bd: create <id>", reached from cmd/bd/create_atomic.go). It is that
// CreateRequest has no Provenance field for a caller to set, so there is no
// label for this function to prefer. That is a gap rather than a principle:
// the proxied route's create message changed from "bd: create <id>" to
// "create issue" for exactly this reason, and closing it means either giving
// CreateRequest the field or having the role compose an id-bearing default the
// way BatchCloser and ClaimNext do. Tracked as its own decision.
func HistoryEntry(provenance, fallback string) string {
	if provenance != "" {
		return provenance
	}
	return fallback
}

// ExecuteCreate applies a guarded create in tx and reports durable tables changed.
func ExecuteCreate(ctx context.Context, tx DBTX, request publicops.CreateRequest) (publicops.CreateResult, ChangedTables, error) {
	attempt, batch, err := prepareCreateExecution(ctx, tx, request)
	if err != nil {
		return publicops.CreateResult{}, nil, err
	}
	issue, childCounterChanged, err := preparePublicCreateIssueExecution(ctx, tx, batch, attempt)
	if err != nil {
		return publicops.CreateResult{}, nil, err
	}
	created, err := insertPublicCreateIssue(ctx, tx, issue, attempt)
	if err != nil {
		return publicops.CreateResult{}, nil, err
	}
	return finishCreateExecution(ctx, tx, issue, childCounterChanged, created)
}

func prepareCreateExecution(ctx context.Context, tx DBTX, request publicops.CreateRequest) (publicops.CreateRequest, *BatchContext, error) {
	attempt := CloneCreateRequest(request)
	if err := ValidatePublicCreateRequest(attempt); err != nil {
		return publicops.CreateRequest{}, nil, err
	}
	batch, err := NewBatchContext(ctx, tx, storage.BatchCreateOptions{CreateOnly: true, SkipPrefixValidation: attempt.ForceIDPrefix})
	if err != nil {
		return publicops.CreateRequest{}, nil, err
	}
	attempt, err = PreparePublicCreateRequest(attempt, PublicCreateContext{
		IssuePrefix: batch.ConfigPrefix, AllowedPrefixes: batch.AllowedPrefixes,
		CustomStatuses: batch.CustomStatuses, CustomTypes: batch.CustomTypes,
	})
	if err != nil {
		return publicops.CreateRequest{}, nil, err
	}
	return attempt, batch, nil
}

func preparePublicCreateIssueExecution(ctx context.Context, tx DBTX, batch *BatchContext, attempt publicops.CreateRequest) (*types.Issue, bool, error) {
	issue := attempt.Issue
	// Configured infra types live in the wisp tables, the same routing the
	// stores' own CreateIssue applies (internal/storage/dolt/issues.go). Mark
	// the issue before its ID is assigned so ID generation, the create-only
	// guard, and table routing all agree on the destination. A no-history
	// create keeps its own retention mode.
	applyCreateInfraRouting(ctx, tx, issue)
	if err := inheritCreateLabels(ctx, tx, issue, attempt); err != nil {
		return nil, false, err
	}
	childCounterChanged, err := assignCreateParentID(ctx, tx, issue, attempt.ParentID)
	if err != nil {
		return nil, false, err
	}
	if err := assignCreateIssueIDInTx(ctx, tx, batch, issue, attempt.Actor); err != nil {
		return nil, false, ClassifyPublicCreateError(err)
	}
	issue.Dependencies = storage.CreatePublicCreateDependencies(issue.ID, attempt)
	return issue, childCounterChanged, nil
}

func applyCreateInfraRouting(ctx context.Context, tx DBTX, issue *types.Issue) {
	if !issue.Ephemeral && !issue.NoHistory && ResolveInfraTypesInTx(ctx, tx)[string(issue.IssueType)] {
		issue.Ephemeral = true
	}
}

func inheritCreateLabels(ctx context.Context, tx DBTX, issue *types.Issue, attempt publicops.CreateRequest) error {
	if !attempt.InheritLabelsFromParent || attempt.ParentID == "" {
		return nil
	}
	labels, err := GetLabelsInTx(ctx, tx, "", attempt.ParentID)
	if err != nil {
		return err
	}
	issue.Labels = append(issue.Labels, labels...)
	return nil
}

func assignCreateParentID(ctx context.Context, tx DBTX, issue *types.Issue, parentID string) (bool, error) {
	if issue.ID != "" || parentID == "" {
		return false, nil
	}
	parentIsWisp := IsActiveWispInTx(ctx, tx, parentID)
	childID, err := GetNextChildIDTx(ctx, tx, parentID)
	if err != nil {
		return false, ClassifyPublicCreateError(err)
	}
	issue.ID = childID
	return !parentIsWisp, nil
}

func insertPublicCreateIssue(ctx context.Context, tx DBTX, issue *types.Issue, attempt publicops.CreateRequest) (CreateIssuesResult, error) {
	var skipped []skippedDependency
	created, err := CreateIssuesInTxWithResult(ctx, tx, []*types.Issue{issue}, attempt.Actor, storage.BatchCreateOptions{
		CreateOnly: true, SkipPrefixValidation: attempt.ForceIDPrefix,
		OnSkippedDependency: func(issueID, dependsOnID, reason string) {
			skipped = append(skipped, skippedDependency{issueID: issueID, dependsOnID: dependsOnID, reason: reason})
		},
	})
	if err != nil {
		return CreateIssuesResult{}, ClassifyPublicCreateError(err)
	}
	if len(skipped) > 0 {
		return CreateIssuesResult{}, publicCreateValidationError(skippedDependencyError(skipped))
	}
	return created, nil
}

func finishCreateExecution(ctx context.Context, tx DBTX, issue *types.Issue, childCounterChanged bool, created CreateIssuesResult) (publicops.CreateResult, ChangedTables, error) {
	tables := ChangedTables{}
	tables.Merge(CreateIssuesDirtyTables(ctx, []*types.Issue{issue}, created))
	if childCounterChanged {
		tables.Add("child_counters")
	}
	hydrated, err := HydrateIssueOperationResult(ctx, tx, issue.ID, true)
	if err != nil {
		return publicops.CreateResult{}, nil, err
	}
	return publicops.CreateResult{Issue: hydrated}, tables, nil
}

// skippedDependency records an edge the batch engine declined to write.
type skippedDependency struct{ issueID, dependsOnID, reason string }

// skippedDependencyError refuses a guarded create whose requested edges were
// not all written. The batch engine drops a dangling edge so a partial import
// still lands, but a guarded create that reported success while silently
// discarding a parent, waits-for, or explicit dependency is data loss: the
// caller has no way to learn the relationship is missing. Refusing rolls the
// whole create back with the enclosing transaction.
func skippedDependencyError(skipped []skippedDependency) error {
	edges := make([]string, 0, len(skipped))
	for _, edge := range skipped {
		edges = append(edges, fmt.Sprintf("%s -> %s (%s)", edge.issueID, edge.dependsOnID, edge.reason))
	}
	return fmt.Errorf("create: dependencies could not be created: %s: %w", strings.Join(edges, "; "), storage.ErrNotFound)
}

// ExecuteUpdate applies a guarded update in tx and reports durable tables changed.
func ExecuteUpdate(ctx context.Context, tx DBTX, request publicops.UpdateRequest) (publicops.UpdateResult, ChangedTables, error) {
	execution, err := prepareUpdateExecution(ctx, tx, request)
	if err != nil {
		return publicops.UpdateResult{}, nil, err
	}
	if err := applyUpdateClaim(ctx, tx, execution); err != nil {
		return publicops.UpdateResult{}, nil, err
	}
	if err := applyUpdateFields(ctx, tx, execution); err != nil {
		return publicops.UpdateResult{}, nil, err
	}
	if err := applyUpdateLabels(ctx, tx, execution); err != nil {
		return publicops.UpdateResult{}, nil, err
	}
	if err := applyUpdateParent(ctx, tx, execution); err != nil {
		return publicops.UpdateResult{}, nil, err
	}
	if err := applyUpdatePersistence(ctx, tx, execution); err != nil {
		return publicops.UpdateResult{}, nil, err
	}
	hydrated, err := HydrateIssueOperationResult(ctx, tx, execution.attempt.IssueID, false)
	if err != nil {
		return publicops.UpdateResult{}, nil, err
	}
	return publicops.UpdateResult{Issue: hydrated, Changed: execution.changed}, execution.tables, nil
}

type updateExecution struct {
	attempt publicops.UpdateRequest
	current *types.Issue
	tables  ChangedTables
	changed bool
}

func prepareUpdateExecution(ctx context.Context, tx DBTX, request publicops.UpdateRequest) (*updateExecution, error) {
	attempt := CloneUpdateRequest(request)
	if err := validateUpdateInput(attempt); err != nil {
		return nil, err
	}
	// The plane restriction is resolved HERE, inside the update's own
	// transaction, so a caller that serves durable issues only cannot be handed
	// a wisp by a resolve that ran earlier.
	if attempt.IssuePlaneOnly && IsActiveWispInTx(ctx, tx, attempt.IssueID) {
		return nil, fmt.Errorf("%w: issue %s", storage.ErrNotFound, attempt.IssueID)
	}
	before, err := GetIssueInTx(ctx, tx, attempt.IssueID)
	if err != nil {
		return nil, err
	}
	if attempt.ExpectedVersion != nil {
		if err := CheckVersionInTx(ctx, tx, attempt.IssueID, *attempt.ExpectedVersion); err != nil {
			return nil, err
		}
	}
	if err := validateUpdateExpectations(ctx, tx, &attempt); err != nil {
		return nil, err
	}
	if err := AuthorizeAssigneeTransfer(ctx, tx, before, attempt); err != nil {
		return nil, err
	}
	return &updateExecution{attempt: attempt, current: before, tables: ChangedTables{}}, nil
}

func validateUpdateInput(attempt publicops.UpdateRequest) error {
	if attempt.Actor == "" || attempt.IssueID == "" {
		return fmt.Errorf("%w: update requires actor and issue ID", storage.ErrValidation)
	}
	if err := ValidateUpdateRequest(attempt); err != nil {
		return err
	}
	return ValidateMetadataPatch(attempt.Patch.Metadata)
}

func validateUpdateExpectations(ctx context.Context, tx DBTX, attempt *publicops.UpdateRequest) error {
	if attempt.ExpectedStatus != nil {
		status := string(*attempt.ExpectedStatus)
		attempt.ExpectedStatus = nil
		return CheckExpectedFieldsInTx(ctx, tx, attempt.IssueID, attempt.ExpectedAssignee, &status)
	}
	return CheckExpectedFieldsInTx(ctx, tx, attempt.IssueID, attempt.ExpectedAssignee, nil)
}

func applyUpdateClaim(ctx context.Context, tx DBTX, execution *updateExecution) error {
	if !execution.attempt.Claim {
		return nil
	}
	claimed, err := ClaimIssueInTx(ctx, tx, execution.attempt.IssueID, execution.attempt.Actor)
	if err != nil {
		return err
	}
	if claimAdvancedTheRow(claimed, execution.attempt.Actor) {
		execution.changed = true
		issueTable, _, eventTable, _ := WispTableRouting(claimed.IsWisp)
		execution.tables.Add(issueTable, eventTable)
	}
	execution.current, err = GetIssueInTx(ctx, tx, execution.attempt.IssueID)
	return err
}

func applyUpdateFields(ctx context.Context, tx DBTX, execution *updateExecution) error {
	updates, err := prepareUpdateFields(execution.current, execution.attempt)
	if err != nil {
		return err
	}
	if len(updates) == 0 {
		return nil
	}
	updated, err := UpdateIssueInTx(ctx, tx, execution.attempt.IssueID, updates, execution.attempt.Actor)
	if err != nil {
		return err
	}
	if !updated.Changed {
		return nil
	}
	execution.changed = true
	issueTable, _, eventTable, _ := WispTableRouting(updated.IsWisp)
	execution.tables.Add(issueTable, eventTable)
	if updated.IssueRowsChanged {
		execution.tables.Add("issues")
	}
	return nil
}

func prepareUpdateFields(current *types.Issue, attempt publicops.UpdateRequest) (map[string]interface{}, error) {
	updates := UpdateFields(attempt.Patch)
	// A metadata patch rides the same row write as the field edits. Writing it
	// separately recorded a second event for one atomic mutation, fabricating
	// an update that never happened for every consumer of the event stream.
	// ApplyMetadataPatch already resolved the patch against the in-transaction
	// row, so the concrete value goes in alongside the other columns and its
	// no-op elision is preserved by the changed flag.
	metadata, metadataChanged, err := ApplyMetadataPatch(current.Metadata, attempt.Patch.Metadata)
	if err != nil {
		return nil, err
	}
	if metadataChanged {
		updates["metadata"] = metadata
	}
	// The override only means anything alongside a status change, so it is spelled
	// only then: an update that carries it with no status would be handing
	// the funnel a key it has no question to answer.
	if attempt.ForceClosePolicy && attempt.Patch.Status.Set {
		updates[OpForceClosePolicy] = true
	}
	return updates, nil
}

func applyUpdateLabels(ctx context.Context, tx DBTX, execution *updateExecution) error {
	labelsChanged, err := ApplyLabelPatch(ctx, tx, execution.current, execution.attempt.Patch.Labels, execution.attempt.Actor)
	if err != nil {
		return err
	}
	if labelsChanged {
		execution.changed = true
		_, labelTable, eventTable, _ := WispTableRouting(IsActiveWispInTx(ctx, tx, execution.attempt.IssueID))
		execution.tables.Add(labelTable, eventTable)
	}
	return nil
}

func applyUpdateParent(ctx context.Context, tx DBTX, execution *updateExecution) error {
	parentResult, err := ApplyParentPatch(ctx, tx, execution.current, execution.attempt.Patch.ParentID, execution.attempt.Actor)
	if err != nil {
		return err
	}
	if parentResult.Changed {
		execution.changed = true
		_, _, _, dependencyTable := WispTableRouting(IsActiveWispInTx(ctx, tx, execution.attempt.IssueID))
		execution.tables.Add(dependencyTable)
		if parentResult.IssueRowsChanged {
			execution.tables.Add("issues")
		}
	}
	return nil
}

func applyUpdatePersistence(ctx context.Context, tx DBTX, execution *updateExecution) error {
	if !execution.attempt.Patch.Persistence.Set {
		return nil
	}
	current, err := GetIssueInTx(ctx, tx, execution.attempt.IssueID)
	if err != nil {
		return err
	}
	moved, err := MoveIssuePersistenceInTx(ctx, tx, current, execution.attempt.Patch.Persistence.Value)
	if err != nil {
		return err
	}
	if moved.Changed {
		execution.changed = true
		execution.tables.Merge(moved.ChangedTables)
	}
	return nil
}

// claimAdvancedTheRow reports whether ClaimIssueInTx's CAS represents a real
// mutation from the pre-image it read, as opposed to an idempotent re-claim
// that matched the CAS but changed nothing worth staging. Comparing the
// pre-image assignee to actor verbatim (ga-v2k49, steveyegge's #5479
// re-review) mistook a holder re-claiming under a respelled identity — a
// dotted alias vs. its sanitized form, ga-wzl83's repro shape — for a fresh
// mutation: ClaimIssueInTx's own CAS already treats the two spellings as the
// same holder and writes nothing, but this check disagreed and staged a
// phantom issues+events mutation for a request that changed no bytes.
func claimAdvancedTheRow(claimed *ClaimResult, actor string) bool {
	return claimed.OldIssue.Status != types.StatusInProgress || !actorMatches(claimed.OldIssue.Assignee, actor)
}

// ExecuteClose applies a guarded close in tx and reports durable tables changed.
func ExecuteClose(ctx context.Context, tx DBTX, request publicops.CloseRequest) (publicops.CloseResult, ChangedTables, error) {
	attempt := CloneCloseRequest(request)
	if attempt.Actor == "" || attempt.IssueID == "" {
		return publicops.CloseResult{}, nil, fmt.Errorf("%w: close requires actor and issue ID", storage.ErrValidation)
	}
	closed, err := CloseIssueCheckedInTx(ctx, tx, attempt.IssueID, attempt.Reason, attempt.Actor, attempt.Session, attempt.Force, attempt.ExpectedVersion)
	if err != nil {
		return publicops.CloseResult{}, nil, err
	}
	tables := ChangedTables{}
	changed := !closed.AlreadyClosed
	if changed {
		issueTable, _, eventTable, _ := WispTableRouting(closed.IsWisp)
		tables.Add(issueTable, eventTable)
	}
	if closed.IssueRowsChanged {
		tables.Add("issues")
	}
	hydrated, err := HydrateIssueOperationResult(ctx, tx, attempt.IssueID, false)
	if err != nil {
		return publicops.CloseResult{}, nil, err
	}
	return publicops.CloseResult{Issue: hydrated, Changed: changed, OpenChildren: closed.OpenChildren}, tables, nil
}

// ExecuteReopen applies a guarded reopen in tx and reports durable tables changed.
func ExecuteReopen(ctx context.Context, tx DBTX, request publicops.ReopenRequest) (publicops.ReopenResult, ChangedTables, error) {
	attempt := CloneReopenRequest(request)
	if attempt.Actor == "" || attempt.IssueID == "" {
		return publicops.ReopenResult{}, nil, fmt.Errorf("%w: reopen requires actor and issue ID", storage.ErrValidation)
	}
	if attempt.ExpectedVersion != nil {
		if err := CheckVersionInTx(ctx, tx, attempt.IssueID, *attempt.ExpectedVersion); err != nil {
			return publicops.ReopenResult{}, nil, err
		}
	}
	reopened, err := ReopenIssueInTx(ctx, tx, attempt.IssueID, attempt.Reason, attempt.Actor)
	if err != nil {
		return publicops.ReopenResult{}, nil, err
	}
	tables := ChangedTables{}
	if reopened.Changed {
		issueTable, _, eventTable, _ := WispTableRouting(reopened.IsWisp)
		tables.Add(issueTable, eventTable)
	}
	if reopened.IssueRowsChanged {
		tables.Add("issues")
	}
	hydrated, err := HydrateIssueOperationResult(ctx, tx, attempt.IssueID, false)
	if err != nil {
		return publicops.ReopenResult{}, nil, err
	}
	return publicops.ReopenResult{Issue: hydrated, Changed: reopened.Changed}, tables, nil
}
