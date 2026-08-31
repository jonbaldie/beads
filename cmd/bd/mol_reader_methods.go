package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/workapi"
)

func (r uowMolReader) GetIssue(ctx context.Context, id string) (*types.Issue, error) {
	issue, isWisp, rerr := workapi.GetIssueOrWisp(ctx, workapi.NewUOWDetailSource(r.uw), id)
	if errors.Is(rerr, storage.ErrNotFound) {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	if rerr != nil {
		return nil, fmt.Errorf("resolving %s: %w", id, rerr)
	}
	// READS, ALL OF THEM, and they stay for the reason the writes did not.
	// uowMolReader is a PORT: it adapts a caller's open unit of work to the
	// molecule commands' reader interface, and every method here must answer
	// from inside that transaction. issueops.Reader opens one of its own, so a
	// role-routed port would show the molecule the last committed state while
	// the command that owns the transaction is midway through changing it.
	// A reader role bound to a caller's transaction is the follow-up
	// (ga-2ltro.12). The wisp branch here is a read that follows the row
	// GetIssueOrWisp already found, not a front door choosing where to write.
	var labels []string
	var err error
	if isWisp {
		labels, err = r.uw.LabelUseCase().GetWispLabels(ctx, id) //nolint:forbidigo // in-transaction port read; issueops.Reader would open its own
	} else {
		labels, err = r.uw.LabelUseCase().GetLabels(ctx, id) //nolint:forbidigo // in-transaction port read; issueops.Reader would open its own
	}
	if err == nil {
		issue.Labels = labels
	}
	return issue, nil
}
func (r uowMolReader) GetIssuesByIDs(ctx context.Context, ids []string) ([]*types.Issue, error) {
	issues, err := r.uw.IssueUseCase().GetIssuesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	if labelMap, err := r.uw.LabelUseCase().GetLabelsForIssues(ctx, ids); err == nil { //nolint:forbidigo // in-transaction port read; see GetIssue above
		for _, issue := range issues {
			issue.Labels = labelMap[issue.ID]
		}
	}

	wisps, err := r.uw.IssueUseCase().GetWispsByIDs(ctx, ids)
	if err != nil {
		wisps = nil //nolint:staticcheck // wisps table may not exist; issues result still valid
	}
	if len(wisps) > 0 {
		if labelMap, err := r.uw.LabelUseCase().GetLabelsForWisps(ctx, ids); err == nil { //nolint:forbidigo // in-transaction port read; see GetIssue above
			for _, wisp := range wisps {
				wisp.Labels = labelMap[wisp.ID]
			}
		}
	}

	return append(issues, wisps...), nil
}
func (r uowMolReader) GetIssuesByLabel(ctx context.Context, label string) ([]*types.Issue, error) {
	page, err := r.uw.IssueUseCase().SearchIssues(ctx, "", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Labels: []string{label}}})
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}
func (r uowMolReader) GetDependents(ctx context.Context, issueID string) ([]*types.Issue, error) {
	withMeta, err := r.GetDependentsWithMetadata(ctx, issueID)
	if err != nil {
		return nil, err
	}
	out := make([]*types.Issue, len(withMeta))
	for i, m := range withMeta {
		issue := m.Issue
		out[i] = &issue
	}
	return out, nil
}
func (r uowMolReader) GetDependentsWithMetadata(ctx context.Context, issueID string) ([]*types.IssueWithDependencyMetadata, error) {
	filter := domain.DepListFilter{Direction: domain.DepDirectionIn}
	return r.uw.DependencyUseCase().ListWithIssueMetadata(ctx, issueID, filter)
}
func (r uowMolReader) GetDependencyRecords(ctx context.Context, issueID string) ([]*types.Dependency, error) {
	out, err := r.uw.DependencyUseCase().GetForIssueIDs(ctx, []string{issueID})
	if err != nil {
		return nil, err
	}
	return out[issueID], nil
}
func (r uowMolReader) GetDependencyRecordsForIssues(ctx context.Context, issueIDs []string) (map[string][]*types.Dependency, error) {
	return r.uw.DependencyUseCase().GetForIssueIDs(ctx, issueIDs)
}
func (r uowMolReader) SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error) {
	page, err := r.uw.IssueUseCase().SearchIssues(ctx, query, filter)
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}
func (r uowMolReader) SearchIssueIDs(ctx context.Context, query string, filter types.IssueFilter) ([]string, error) {
	return r.uw.IssueUseCase().SearchIssueIDs(ctx, query, filter)
}
func (r uowMolReader) GetReadyWork(ctx context.Context, filter types.WorkFilter) ([]*types.Issue, error) {
	page, err := r.uw.IssueUseCase().GetReadyWork(ctx, filter)
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}
func (r uowMolReader) GetBlockedIssues(ctx context.Context, filter types.WorkFilter) ([]*types.BlockedIssue, error) {
	return r.uw.IssueUseCase().GetBlockedIssues(ctx, filter)
}
func (r uowMolReader) GetEpicsEligibleForClosure(ctx context.Context) ([]*types.EpicStatus, error) {
	return r.uw.IssueUseCase().GetEpicsEligibleForClosure(ctx)
}
func (r uowMolReader) GetLabels(ctx context.Context, issueID string) ([]string, error) {
	return r.uw.LabelUseCase().GetLabels(ctx, issueID) //nolint:forbidigo // in-transaction port read; see GetIssue above
}
func (r uowMolReader) GetConfig(ctx context.Context, key string) (string, error) {
	return r.uw.ConfigUseCase().GetConfig(ctx, key)
}
func (r uowMolReader) IsInfraTypeCtx(ctx context.Context, t types.IssueType) bool {
	ok, err := r.uw.ConfigUseCase().IsInfraTypeCtx(ctx, t)
	return err == nil && ok
}
func (r uowMolReader) GetCustomStatusesDetailed(ctx context.Context) ([]types.CustomStatus, error) {
	return r.uw.ConfigUseCase().GetCustomStatuses(ctx)
}
func (r uowMolReader) GetMoleculeProgress(ctx context.Context, moleculeID string) (*types.MoleculeProgressStats, error) {
	stats := &types.MoleculeProgressStats{MoleculeID: moleculeID}

	root, err := r.GetIssue(ctx, moleculeID)
	if err != nil {
		return nil, fmt.Errorf("failed to get molecule: %w", err)
	}
	if root != nil {
		stats.MoleculeTitle = root.Title
	}

	dependents, err := r.GetDependentsWithMetadata(ctx, moleculeID)
	if err != nil {
		return nil, fmt.Errorf("failed to get molecule children: %w", err)
	}

	for _, dependent := range dependents {
		if dependent.DependencyType != types.DepParentChild {
			continue
		}
		stats.Total++
		switch dependent.Status {
		case types.StatusClosed:
			stats.Completed++
		case types.StatusInProgress:
			stats.InProgress++
			if stats.CurrentStepID == "" {
				stats.CurrentStepID = dependent.ID
			}
		}
	}
	return stats, nil
}
func (r uowMolReader) GetMoleculeLastActivity(ctx context.Context, moleculeID string) (*types.MoleculeLastActivity, error) {
	children, err := r.moleculeChildren(ctx, moleculeID)
	if err != nil {
		return nil, err
	}
	if len(children) == 0 {
		return r.rootMoleculeActivity(ctx, moleculeID)
	}
	return r.latestChildActivity(moleculeID, children), nil
}
func (r uowMolReader) moleculeChildren(ctx context.Context, moleculeID string) ([]types.Issue, error) {
	dependents, err := r.GetDependentsWithMetadata(ctx, moleculeID)
	if err != nil {
		return nil, fmt.Errorf("get molecule children: %w", err)
	}
	children := make([]types.Issue, 0, len(dependents))
	for _, dependent := range dependents {
		if dependent.DependencyType != types.DepParentChild {
			continue
		}
		children = append(children, dependent.Issue)
	}
	return children, nil
}
func (r uowMolReader) rootMoleculeActivity(ctx context.Context, moleculeID string) (*types.MoleculeLastActivity, error) {
	root, err := r.GetIssue(ctx, moleculeID)
	if err != nil {
		return nil, fmt.Errorf("molecule %s not found: %w", moleculeID, err)
	}
	return &types.MoleculeLastActivity{
		MoleculeID: moleculeID, LastActivity: root.UpdatedAt, Source: "molecule_updated",
	}, nil
}
func (r uowMolReader) latestChildActivity(moleculeID string, children []types.Issue) *types.MoleculeLastActivity {
	var lastUpdatedAt time.Time
	var lastUpdatedID string
	var lastClosedAt time.Time
	var lastClosedID string
	haveClosed := false

	for _, child := range children {
		if child.UpdatedAt.After(lastUpdatedAt) {
			lastUpdatedAt = child.UpdatedAt
			lastUpdatedID = child.ID
		}
		if child.ClosedAt != nil && (!haveClosed || child.ClosedAt.After(lastClosedAt)) {
			lastClosedAt = *child.ClosedAt
			lastClosedID = child.ID
			haveClosed = true
		}
	}

	result := &types.MoleculeLastActivity{
		MoleculeID:   moleculeID,
		LastActivity: lastUpdatedAt,
		Source:       "step_updated",
		SourceStepID: lastUpdatedID,
	}
	if haveClosed && lastClosedAt.After(lastUpdatedAt) {
		result.LastActivity = lastClosedAt
		result.Source = "step_closed"
		result.SourceStepID = lastClosedID
	}
	return result
}
func (r uowMolReader) FindWispDependentsRecursive(ctx context.Context, ids []string) (map[string]bool, error) {
	return r.uw.IssueUseCase().FindWispDependentsRecursive(ctx, ids)
}
