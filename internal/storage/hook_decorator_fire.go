// Package storage — hook_decorator.go
//
// HookFiringStore is a decorator around DoltStorage that automatically
// fires on_create/on_update/on_close hooks after successful mutations.
// This moves hook responsibility from individual CLI commands into the
// storage layer, ensuring ALL mutations fire hooks — including future
// commands that haven't been written yet.
//
// Usage:
//
//	store = storage.NewHookFiringStore(rawStore, hookRunner)
//
// Transaction support: mutations inside RunInTransaction are tracked
// and hooks fire only after the transaction commits successfully.
// If the transaction rolls back, no hooks fire.
package storage

import (
	"context"

	"github.com/jonbaldie/beads/internal/hooks"
	"github.com/jonbaldie/beads/internal/types"
)

// ── Internal helpers ────────────────────────────────────────────────

func (h *HookFiringStore) fireHook(event string, issue *types.Issue) {
	if h.runner == nil || issue == nil {
		return
	}
	h.runner.Run(event, issue)
}

// CompleteIssueOperationCreate fires hooks for a committed guarded create.
// dependencies is a request-neutral snapshot of the dependency changes whose
// source issues need update hooks.
func (h *HookFiringStore) CompleteIssueOperationCreate(ctx context.Context, issue *types.Issue, dependencies []*types.Dependency) {
	for _, pending := range createHookEvents(issue) {
		h.fireHook(pending.event, pending.issue)
	}
	if h.runner == nil || len(dependencies) == 0 {
		return
	}
	request := &types.Issue{IssueGraph: types.IssueGraph{Dependencies: cloneDependenciesForHook(dependencies)}}
	for _, pending := range dependencyHookEvents(ctx, []*types.Issue{request}, h.inner.GetIssue, h.inner.GetDependencyRecords) {
		h.fireHook(pending.event, pending.issue)
	}
}

// CompleteIssueOperationUpdate fires the update hook for a committed guarded
// operation. The issue is cloned because the hook runner marshals it on its
// own goroutine while callers (cmd/bd close/update/reopen) go on to mutate
// the result they handed in.
func (h *HookFiringStore) CompleteIssueOperationUpdate(issue *types.Issue) {
	h.fireHook(hooks.EventUpdate, cloneIssueForHook(issue))
}

// CompleteIssueOperationClose fires the close hook for a committed guarded
// close. Cloned for the same reason as CompleteIssueOperationUpdate.
func (h *HookFiringStore) CompleteIssueOperationClose(issue *types.Issue) {
	h.fireHook(hooks.EventClose, cloneIssueForHook(issue))
}

// CompleteIssueOperationDependency fires the update hook for an issue whose
// edges a committed guarded edit changed. It re-reads the issue with its
// dependency records, exactly as AddDependency and RemoveDependency do, so a
// hook script sees the graph the edit produced rather than the row alone.
func (h *HookFiringStore) CompleteIssueOperationDependency(ctx context.Context, issueID string) {
	h.fireDependencyHookByID(ctx, issueID)
}

// CompleteIssueOperationComment fires the update hook for a committed guarded
// comment, which is the event AddIssueComment fires: there is no on_comment
// event, and a comment is a change to the issue as far as a script is
// concerned.
func (h *HookFiringStore) CompleteIssueOperationComment(ctx context.Context, issueID string) {
	h.fireHookByID(ctx, hooks.EventUpdate, issueID)
}

// CompleteIssueOperationMetadata fires the update hook for a committed
// compare-and-set that moved a metadata key, which is the event the generic
// update path fires for the same write. It re-reads the row rather than taking
// the caller's, so a script sees the metadata the swap produced.
func (h *HookFiringStore) CompleteIssueOperationMetadata(ctx context.Context, issueID string) {
	h.fireHookByID(ctx, hooks.EventUpdate, issueID)
}

// CompleteIssueOperationRelease fires the update hook for a committed release,
// which is the event the generic update path fires for the same write —
// assignee and status are exactly the fields a release moves. It re-reads the
// row rather than taking the caller's, so a script sees the unheld issue the
// release produced rather than the claim it had.
func (h *HookFiringStore) CompleteIssueOperationRelease(ctx context.Context, issueID string) {
	h.fireHookByID(ctx, hooks.EventUpdate, issueID)
}

func (h *HookFiringStore) fireHookByID(ctx context.Context, event, id string) {
	if h.runner == nil {
		return
	}
	issue, err := h.inner.GetIssue(ctx, id)
	if err != nil {
		return // best-effort: skip hook if re-fetch fails
	}
	h.runner.Run(event, issue)
}

func (h *HookFiringStore) fireDependencyHookByID(ctx context.Context, id string) {
	if h.runner == nil {
		return
	}
	issue, err := dependencySnapshot(ctx, id, h.inner.GetIssue, h.inner.GetDependencyRecords)
	if err != nil {
		return
	}
	h.runner.Run(hooks.EventUpdate, issue)
}

// ── Hook tracking transaction ───────────────────────────────────────

// pendingHook records a hook to fire after transaction commit.
type pendingHook struct {
	event string
	issue *types.Issue
}

func createHookEvents(issue *types.Issue) []pendingHook {
	if issue == nil {
		return nil
	}
	if len(issue.Labels) == 0 {
		return []pendingHook{{event: hooks.EventCreate, issue: cloneIssueForHook(issue)}}
	}

	// Initial labels are persisted before hooks fire, but the hook stream keeps
	// the legacy post-create AddLabel shape: on_create receives a label-free
	// snapshot, then on_update receives cumulative synthetic label snapshots.
	// Hook implementations should use the issue payload for that sequence; live
	// store reads during these synthetic events observe the fully persisted issue.
	labels := make([]string, 0, len(issue.Labels))
	seen := make(map[string]struct{}, len(issue.Labels))
	for _, label := range issue.Labels {
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		labels = append(labels, label)
	}
	createSnapshot := cloneIssueForHook(issue)
	createSnapshot.Labels = nil
	events := []pendingHook{{event: hooks.EventCreate, issue: createSnapshot}}
	for i := range labels {
		updateSnapshot := cloneIssueForHook(issue)
		updateSnapshot.Labels = append([]string(nil), labels[:i+1]...)
		events = append(events, pendingHook{event: hooks.EventUpdate, issue: updateSnapshot})
	}
	return events
}

type issueGetter func(context.Context, string) (*types.Issue, error)
type dependencyRecordsGetter func(context.Context, string) ([]*types.Dependency, error)

func dependencyHookEvents(ctx context.Context, issues []*types.Issue, get issueGetter, getDeps dependencyRecordsGetter) []pendingHook {
	var events []pendingHook
	states := make(map[string]*dependencyHookState)
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		events = append(events, dependencyEventsForIssue(ctx, issue, get, getDeps, states)...)
	}
	return events
}

func dependencyEventsForIssue(ctx context.Context, issue *types.Issue, get issueGetter, getDeps dependencyRecordsGetter, states map[string]*dependencyHookState) []pendingHook {
	var events []pendingHook
	for _, dep := range issue.Dependencies {
		if event, ok := dependencyEventForRequest(ctx, issue, dep, get, getDeps, states); ok {
			events = append(events, event)
		}
	}
	return events
}

func dependencyEventForRequest(ctx context.Context, issue *types.Issue, requested *types.Dependency, get issueGetter, getDeps dependencyRecordsGetter, states map[string]*dependencyHookState) (pendingHook, bool) {
	issueID := dependencyIssueID(issue, requested)
	if issueID == "" {
		return pendingHook{}, false
	}
	state, ok := states[issueID]
	if !ok {
		snapshot, err := dependencySnapshot(ctx, issueID, get, getDeps)
		if err != nil {
			return pendingHook{}, false
		}
		state = &dependencyHookState{snapshot: snapshot, used: make([]bool, len(snapshot.Dependencies))}
		states[issueID] = state
	}
	persisted := state.take(requested, issueID)
	if persisted == nil {
		return pendingHook{}, false
	}
	state.emitted = append(state.emitted, persisted)
	updateSnapshot := cloneIssueForHook(state.snapshot)
	updateSnapshot.Dependencies = cloneDependenciesForHook(state.emitted)
	return pendingHook{event: hooks.EventUpdate, issue: updateSnapshot}, true
}

func dependencyIssueID(issue *types.Issue, requested *types.Dependency) string {
	if requested == nil {
		return ""
	}
	if requested.IssueID != "" {
		return requested.IssueID
	}
	return issue.ID
}

func dependencySnapshot(ctx context.Context, issueID string, get issueGetter, getDeps dependencyRecordsGetter) (*types.Issue, error) {
	snapshot, err := get(ctx, issueID)
	if err != nil {
		return nil, err
	}
	deps, err := getDeps(ctx, issueID)
	if err != nil {
		return nil, err
	}
	snapshot.Dependencies = cloneDependenciesForHook(deps)
	return snapshot, nil
}

type dependencyHookState struct {
	snapshot *types.Issue
	used     []bool
	emitted  []*types.Dependency
}

func (s *dependencyHookState) take(requested *types.Dependency, issueID string) *types.Dependency {
	for i, dep := range s.snapshot.Dependencies {
		if s.used[i] || !sameDependency(dep, requested, issueID) {
			continue
		}
		s.used[i] = true
		return dep
	}
	return nil
}

func sameDependency(persisted, requested *types.Dependency, issueID string) bool {
	if persisted == nil || requested == nil {
		return false
	}
	requestedIssueID := requested.IssueID
	if requestedIssueID == "" {
		requestedIssueID = issueID
	}
	return persisted.IssueID == requestedIssueID &&
		persisted.DependsOnID == requested.DependsOnID &&
		persisted.Type == requested.Type
}

func cloneIssueForHook(issue *types.Issue) *types.Issue {
	if issue == nil {
		return nil
	}
	clone := *issue
	clone.EstimatedMinutes = clonePtr(issue.EstimatedMinutes)
	clone.StartedAt = clonePtr(issue.StartedAt)
	clone.ClosedAt = clonePtr(issue.ClosedAt)
	clone.DueAt = clonePtr(issue.DueAt)
	clone.DeferUntil = clonePtr(issue.DeferUntil)
	clone.LeaseExpiresAt = clonePtr(issue.LeaseExpiresAt)
	clone.HeartbeatAt = clonePtr(issue.HeartbeatAt)
	clone.ExternalRef = clonePtr(issue.ExternalRef)
	clone.WispPlaneOverride = clonePtr(issue.WispPlaneOverride)
	clone.Labels = append([]string(nil), issue.Labels...)
	clone.Metadata = append([]byte(nil), issue.Metadata...)
	clone.CompactedAt = clonePtr(issue.CompactedAt)
	clone.CompactedAtCommit = clonePtr(issue.CompactedAtCommit)
	clone.Dependencies = cloneDependenciesForHook(issue.Dependencies)
	if issue.Comments != nil {
		clone.Comments = make([]*types.Comment, len(issue.Comments))
		for i, comment := range issue.Comments {
			if comment == nil {
				continue
			}
			commentCopy := *comment
			clone.Comments[i] = &commentCopy
		}
	}
	clone.BondedFrom = append([]types.BondRef(nil), issue.BondedFrom...)
	clone.Waiters = append([]string(nil), issue.Waiters...)
	return &clone
}

func clonePtr[T any](value *T) *T {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneDependenciesForHook(deps []*types.Dependency) []*types.Dependency {
	if deps == nil {
		return nil
	}
	cloned := make([]*types.Dependency, len(deps))
	for i, dep := range deps {
		if dep == nil {
			continue
		}
		depCopy := *dep
		cloned[i] = &depCopy
	}
	return cloned
}

// HookTrackingTransaction wraps a Transaction, recording mutations so hooks
// can fire after commit. Mutation families are embedded to keep each adapter
// focused while preserving the Transaction interface through promotion.
type HookTrackingTransaction struct {
	transactionPassthrough
	pending []pendingHook
	HookTrackingCreateOperations
	HookTrackingIssueOperations
	HookTrackingDependencyOperations
	HookTrackingLabelOperations
}

type transactionPassthrough struct {
	Transaction
}

func newHookTrackingTransaction(tx Transaction) *HookTrackingTransaction {
	tracked := &HookTrackingTransaction{transactionPassthrough: transactionPassthrough{Transaction: tx}}
	tracked.HookTrackingCreateOperations = HookTrackingCreateOperations{tx: tx, pending: &tracked.pending}
	tracked.HookTrackingIssueOperations = HookTrackingIssueOperations{tx: tx, pending: &tracked.pending}
	tracked.HookTrackingDependencyOperations = HookTrackingDependencyOperations{tx: tx, pending: &tracked.pending}
	tracked.HookTrackingLabelOperations = HookTrackingLabelOperations{tx: tx, pending: &tracked.pending}
	return tracked
}

type HookTrackingCreateOperations struct {
	tx      Transaction
	pending *[]pendingHook
}

func (t *HookTrackingCreateOperations) CreateIssue(ctx context.Context, issue *types.Issue, actor string) error {
	if err := t.tx.CreateIssue(ctx, issue, actor); err != nil {
		return err
	}
	*t.pending = append(*t.pending, createHookEvents(issue)...)
	return nil
}

func (t *HookTrackingCreateOperations) CreateIssues(ctx context.Context, issues []*types.Issue, actor string) error {
	if err := t.tx.CreateIssues(ctx, issues, actor); err != nil {
		return err
	}
	for _, issue := range issues {
		*t.pending = append(*t.pending, createHookEvents(issue)...)
	}
	*t.pending = append(*t.pending, dependencyHookEvents(ctx, issues, t.tx.GetIssue, t.tx.GetDependencyRecords)...)
	return nil
}

type HookTrackingIssueOperations struct {
	tx      Transaction
	pending *[]pendingHook
}

func (t *HookTrackingIssueOperations) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	if err := t.tx.UpdateIssue(ctx, id, updates, actor); err != nil {
		return err
	}
	// Re-fetch within the transaction to get the updated state.
	if issue, err := t.tx.GetIssue(ctx, id); err == nil {
		*t.pending = append(*t.pending, pendingHook{hooks.EventUpdate, issue})
	}
	return nil
}

func (t *HookTrackingIssueOperations) CloseIssue(ctx context.Context, id string, reason string, actor string, session string) error {
	if err := t.tx.CloseIssue(ctx, id, reason, actor, session); err != nil {
		return err
	}
	if issue, err := t.tx.GetIssue(ctx, id); err == nil {
		*t.pending = append(*t.pending, pendingHook{hooks.EventClose, issue})
	}
	return nil
}

// DeleteIssue passes through without firing hooks — delete is destructive
// and the issue no longer exists to pass to a hook.
func (t *HookTrackingIssueOperations) DeleteIssue(ctx context.Context, id string) error {
	return t.tx.DeleteIssue(ctx, id)
}

type hookTrackingLifecycleTransaction struct {
	*HookTrackingTransaction
	reopener IssueLifecycleTransaction
}

func (t *hookTrackingLifecycleTransaction) ReopenIssueWithResult(ctx context.Context, id string, reason string, actor string) (bool, error) {
	changed, err := t.reopener.ReopenIssueWithResult(ctx, id, reason, actor)
	if err != nil || !changed {
		return changed, err
	}
	if issue, getErr := t.Transaction.GetIssue(ctx, id); getErr == nil {
		t.pending = append(t.pending, pendingHook{hooks.EventUpdate, issue})
	}
	return true, nil
}

type HookTrackingDependencyOperations struct {
	tx      Transaction
	pending *[]pendingHook
}

func (t *HookTrackingDependencyOperations) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	return t.AddDependencyWithOptions(ctx, dep, actor, DependencyAddOptions{})
}

func (t *HookTrackingDependencyOperations) AddDependencyWithOptions(ctx context.Context, dep *types.Dependency, actor string, opts DependencyAddOptions) error {
	if err := t.tx.AddDependencyWithOptions(ctx, dep, actor, opts); err != nil {
		return err
	}
	if issue, err := dependencySnapshot(ctx, dep.IssueID, t.tx.GetIssue, t.tx.GetDependencyRecords); err == nil {
		*t.pending = append(*t.pending, pendingHook{hooks.EventUpdate, issue})
	}
	return nil
}

func (t *HookTrackingDependencyOperations) RemoveDependency(ctx context.Context, issueID, dependsOnID string, actor string) error {
	return t.RemoveDependencyWithOptions(ctx, issueID, dependsOnID, actor, DependencyRemoveOptions{})
}

func (t *HookTrackingDependencyOperations) RemoveDependencyWithOptions(ctx context.Context, issueID, dependsOnID string, actor string, opts DependencyRemoveOptions) error {
	if err := t.tx.RemoveDependencyWithOptions(ctx, issueID, dependsOnID, actor, opts); err != nil {
		return err
	}
	if issue, err := dependencySnapshot(ctx, issueID, t.tx.GetIssue, t.tx.GetDependencyRecords); err == nil {
		*t.pending = append(*t.pending, pendingHook{hooks.EventUpdate, issue})
	}
	return nil
}

type HookTrackingLabelOperations struct {
	tx      Transaction
	pending *[]pendingHook
}

func (t *HookTrackingLabelOperations) AddLabel(ctx context.Context, issueID, label, actor string) error {
	if err := t.tx.AddLabel(ctx, issueID, label, actor); err != nil {
		return err
	}
	if issue, err := t.tx.GetIssue(ctx, issueID); err == nil {
		*t.pending = append(*t.pending, pendingHook{hooks.EventUpdate, issue})
	}
	return nil
}

func (t *HookTrackingLabelOperations) RemoveLabel(ctx context.Context, issueID, label, actor string) error {
	if err := t.tx.RemoveLabel(ctx, issueID, label, actor); err != nil {
		return err
	}
	if issue, err := t.tx.GetIssue(ctx, issueID); err == nil {
		*t.pending = append(*t.pending, pendingHook{hooks.EventUpdate, issue})
	}
	return nil
}

func (t *HookTrackingLabelOperations) AddComment(ctx context.Context, issueID, actor, comment string) error {
	if err := t.tx.AddComment(ctx, issueID, actor, comment); err != nil {
		return err
	}
	if issue, err := t.tx.GetIssue(ctx, issueID); err == nil {
		*t.pending = append(*t.pending, pendingHook{hooks.EventUpdate, issue})
	}
	return nil
}

// Ensure compile-time interface satisfaction.
var _ DoltStorage = (*HookFiringStore)(nil)
var _ Transaction = (*HookTrackingTransaction)(nil)

// Ensure HookFiringStore's mutation methods are used (not the embedded passthrough).
// This compile-time check prevents accidentally forgetting to override a method.
var (
	_ interface {
		CreateIssue(context.Context, *types.Issue, string) error
	} = (*HookFiringStore)(nil)
	_ interface {
		UpdateIssue(context.Context, string, map[string]interface{}, string) error
	} = (*HookFiringStore)(nil)
	_ interface {
		UpdateIssueChecked(context.Context, string, map[string]interface{}, string, UpdateIssueOptions) error
	} = (*HookFiringStore)(nil)
	_ interface {
		CloseIssue(context.Context, string, string, string, string) error
	} = (*HookFiringStore)(nil)
	_ interface {
		RunInTransaction(context.Context, string, func(Transaction) error) error
	} = (*HookFiringStore)(nil)
	_ interface {
		AddDependency(context.Context, *types.Dependency, string) error
	} = (*HookFiringStore)(nil)
	_ interface {
		AddLabel(context.Context, string, string, string) error
	} = (*HookFiringStore)(nil)
	_ interface {
		AddIssueComment(context.Context, string, string, string) (*types.Comment, error)
	} = (*HookFiringStore)(nil)
	_ interface {
		ReopenIssue(context.Context, string, string, string) error
	} = (*HookFiringStore)(nil)
)
