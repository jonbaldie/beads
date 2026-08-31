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

// ── Issue mutations ─────────────────────────────────────────────────

// CreateIssue creates an issue and fires on_create plus synthetic on_update
// hooks for initial labels, matching the old post-create AddLabel behavior.
func (h *HookFiringStore) CreateIssue(ctx context.Context, issue *types.Issue, actor string) error {
	if err := h.inner.CreateIssue(ctx, issue, actor); err != nil {
		return err
	}
	for _, p := range createHookEvents(issue) {
		h.fireHook(p.event, p.issue)
	}
	return nil
}

// CreateIssues creates multiple issues and fires create-time hooks for each,
// followed by dependency update hooks for batch-persisted dependencies.
func (h *HookFiringStore) CreateIssues(ctx context.Context, issues []*types.Issue, actor string) error {
	if err := h.inner.CreateIssues(ctx, issues, actor); err != nil {
		return err
	}
	for _, issue := range issues {
		for _, p := range createHookEvents(issue) {
			h.fireHook(p.event, p.issue)
		}
	}
	if h.runner != nil {
		for _, p := range dependencyHookEvents(ctx, issues, h.inner.GetIssue, h.inner.GetDependencyRecords) {
			h.fireHook(p.event, p.issue)
		}
	}
	return nil
}

// UpdateIssue updates an issue and fires on_update.
func (h *HookFiringStore) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	if err := h.inner.UpdateIssue(ctx, id, updates, actor); err != nil {
		return err
	}
	h.fireHookByID(ctx, hooks.EventUpdate, id)
	return nil
}

// UpdateIssueChecked applies the guarded update (optional ExpectedVersion CAS)
// and fires on_update on success — mirroring UpdateIssue. A version mismatch
// (ErrVersionMismatch) or any other error returns without firing.
func (h *HookFiringStore) UpdateIssueChecked(ctx context.Context, id string, updates map[string]interface{}, actor string, opts UpdateIssueOptions) error {
	if err := h.inner.UpdateIssueChecked(ctx, id, updates, actor, opts); err != nil {
		return err
	}
	h.fireHookByID(ctx, hooks.EventUpdate, id)
	return nil
}

// ReopenIssue reopens an issue and fires on_update.
func (h *HookFiringStore) ReopenIssue(ctx context.Context, id string, reason string, actor string) error {
	if err := h.inner.ReopenIssue(ctx, id, reason, actor); err != nil {
		return err
	}
	h.fireHookByID(ctx, hooks.EventUpdate, id)
	return nil
}

// UpdateIssueType changes an issue's type and fires on_update.
func (h *HookFiringStore) UpdateIssueType(ctx context.Context, id string, issueType string, actor string) error {
	if err := h.inner.UpdateIssueType(ctx, id, issueType, actor); err != nil {
		return err
	}
	h.fireHookByID(ctx, hooks.EventUpdate, id)
	return nil
}

// CloseIssue closes an issue and fires on_close.
func (h *HookFiringStore) CloseIssue(ctx context.Context, id string, reason string, actor string, session string) error {
	if err := h.inner.CloseIssue(ctx, id, reason, actor, session); err != nil {
		return err
	}
	h.fireHookByID(ctx, hooks.EventClose, id)
	return nil
}

// CloseIssueChecked closes an issue under the is_blocked guard and fires
// on_close on success — mirroring CloseIssue, this includes the idempotent
// no-op when the issue was already closed (res.Unchanged). A guard rejection
// (ErrCloseBlocked) or any other error returns without firing.
//
// THE BATCH VERBS DO NOT FOLLOW THIS RULE: hookBatchCloser and hookBatchApplier
// fire per item on Changed, so a replayed teardown does not re-run on_close N
// times (ga-2yaqp.1). This single close keeps the firing because one re-close
// is one answer to one question a script asked.
func (h *HookFiringStore) CloseIssueChecked(ctx context.Context, id string, actor string, opts CloseIssueOptions) (CloseIssueResult, error) {
	res, err := h.inner.CloseIssueChecked(ctx, id, actor, opts)
	if err != nil {
		return res, err
	}
	h.fireHookByID(ctx, hooks.EventClose, id)
	return res, nil
}

// ── Dependency mutations ────────────────────────────────────────────

// AddDependency adds a dependency and fires on_update for the issue.
func (h *HookFiringStore) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	if err := h.inner.AddDependency(ctx, dep, actor); err != nil {
		return err
	}
	h.fireDependencyHookByID(ctx, dep.IssueID)
	return nil
}

// AddDependencyWithOptions adds a dependency with options and fires on_update.
func (h *HookFiringStore) AddDependencyWithOptions(ctx context.Context, dep *types.Dependency, actor string, opts DependencyAddOptions) error {
	if err := h.inner.AddDependencyWithOptions(ctx, dep, actor, opts); err != nil {
		return err
	}
	h.fireDependencyHookByID(ctx, dep.IssueID)
	return nil
}

// RemoveDependency removes a dependency and fires on_update for the issue.
func (h *HookFiringStore) RemoveDependency(ctx context.Context, issueID, dependsOnID string, actor string) error {
	if err := h.inner.RemoveDependency(ctx, issueID, dependsOnID, actor); err != nil {
		return err
	}
	h.fireDependencyHookByID(ctx, issueID)
	return nil
}

// RemoveDependencyWithOptions removes a dependency with options and fires on_update.
func (h *HookFiringStore) RemoveDependencyWithOptions(ctx context.Context, issueID, dependsOnID string, actor string, opts DependencyRemoveOptions) error {
	if err := h.inner.RemoveDependencyWithOptions(ctx, issueID, dependsOnID, actor, opts); err != nil {
		return err
	}
	h.fireDependencyHookByID(ctx, issueID)
	return nil
}

// ── Label mutations ─────────────────────────────────────────────────

// AddLabel adds a label and fires on_update.
func (h *HookFiringStore) AddLabel(ctx context.Context, issueID, label, actor string) error {
	if err := h.inner.AddLabel(ctx, issueID, label, actor); err != nil {
		return err
	}
	h.fireHookByID(ctx, hooks.EventUpdate, issueID)
	return nil
}

// RemoveLabel removes a label and fires on_update.
func (h *HookFiringStore) RemoveLabel(ctx context.Context, issueID, label, actor string) error {
	if err := h.inner.RemoveLabel(ctx, issueID, label, actor); err != nil {
		return err
	}
	h.fireHookByID(ctx, hooks.EventUpdate, issueID)
	return nil
}

// ── Comment mutations ───────────────────────────────────────────────

// AddIssueComment adds a comment and fires on_update.
func (h *HookFiringStore) AddIssueComment(ctx context.Context, issueID, author, text string) (*types.Comment, error) {
	comment, err := h.inner.AddIssueComment(ctx, issueID, author, text)
	if err != nil {
		return nil, err
	}
	h.fireHookByID(ctx, hooks.EventUpdate, issueID)
	return comment, nil
}

// ── Transaction support ─────────────────────────────────────────────

// RunInTransaction wraps the callback's transaction with hook tracking.
// Mutations inside the transaction are recorded but hooks only fire
// after the transaction commits successfully. On rollback or error,
// no hooks fire.
func (h *HookFiringStore) RunInTransaction(ctx context.Context, commitMsg string, fn func(tx Transaction) error) error {
	var tracked *HookTrackingTransaction
	err := h.inner.RunInTransaction(ctx, commitMsg, func(tx Transaction) error {
		tracked = newHookTrackingTransaction(tx)
		return fn(tracked)
	})
	if err != nil || tracked == nil {
		return err
	}
	// Transaction committed — fire all accumulated hooks.
	for _, p := range tracked.pending {
		h.fireHook(p.event, p.issue)
	}
	return nil
}

// RunInIssueLifecycleTransaction tracks lifecycle hooks and fires them only
// after the underlying lifecycle transaction commits.
func (h *HookFiringStore) RunInIssueLifecycleTransaction(ctx context.Context, commitMsg string, fn func(tx IssueLifecycleTransaction) error) error {
	var tracked *hookTrackingLifecycleTransaction
	err := h.inner.RunInIssueLifecycleTransaction(ctx, commitMsg, func(tx IssueLifecycleTransaction) error {
		tracked = &hookTrackingLifecycleTransaction{
			HookTrackingTransaction: newHookTrackingTransaction(tx),
			reopener:                tx,
		}
		return fn(tracked)
	})
	if err != nil || tracked == nil {
		return err
	}
	for _, p := range tracked.pending {
		h.fireHook(p.event, p.issue)
	}
	return nil
}
