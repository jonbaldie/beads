package workapi

import (
	"context"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/types"
)

func (u useCaseDetailSource) GetIssue(ctx context.Context, id string) (*types.Issue, error) {
	return u.issues.GetIssue(ctx, id)
}

func (u useCaseDetailSource) GetWisp(ctx context.Context, id string) (*types.Issue, error) {
	return u.issues.GetWisp(ctx, id)
}

func (u useCaseDetailSource) Labels(ctx context.Context, id string, isWisp bool) ([]string, error) {
	if isWisp {
		return u.labels.GetWispLabels(ctx, id)
	}
	return u.labels.GetLabels(ctx, id)
}

func (u useCaseDetailSource) Dependencies(ctx context.Context, id string, isWisp bool) ([]*types.IssueWithDependencyMetadata, error) {
	filter := domain.DepListFilter{Direction: domain.DepDirectionOut}
	if isWisp {
		return u.deps.ListWispWithIssueMetadata(ctx, id, filter)
	}
	return u.deps.ListWithIssueMetadata(ctx, id, filter)
}

func (u useCaseDetailSource) CountDependencies(ctx context.Context, id string, isWisp bool) (int64, error) {
	return u.countDeps(ctx, id, isWisp, domain.DepDirectionOut)
}

func (u useCaseDetailSource) CountDependents(ctx context.Context, id string, isWisp bool) (int64, error) {
	return u.countDeps(ctx, id, isWisp, domain.DepDirectionIn)
}

func (u useCaseDetailSource) countDeps(ctx context.Context, id string, isWisp bool, dir domain.DepDirection) (int64, error) {
	filter := domain.DepListFilter{Direction: dir}
	if isWisp {
		return u.deps.CountByWispID(ctx, id, filter)
	}
	return u.deps.CountByIssueID(ctx, id, filter)
}

func (u useCaseDetailSource) CountComments(ctx context.Context, id string, isWisp bool) (int64, error) {
	if isWisp {
		return u.comments.CountCommentsForWisp(ctx, id)
	}
	return u.comments.CountCommentsForIssue(ctx, id)
}

func (u useCaseDetailSource) IterDependents(ctx context.Context, id string, isWisp bool) (storage.Iter[types.IssueWithDependencyMetadata], error) {
	filter := domain.DepListFilter{Direction: domain.DepDirectionIn}
	if isWisp {
		return u.deps.IterWispWithIssueMetadata(ctx, id, filter)
	}
	return u.deps.IterWithIssueMetadata(ctx, id, filter)
}

func (u useCaseDetailSource) IterComments(ctx context.Context, id string, isWisp bool) (storage.Iter[types.Comment], error) {
	if isWisp {
		return u.comments.IterCommentsForWisp(ctx, id)
	}
	return u.comments.IterCommentsForIssue(ctx, id)
}
