package domain

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/issueops"
)

func (u *dependencyQueryUseCase) ListByIssueIDs(ctx context.Context, issueIDs []string, filter DepListFilter) (DepBulkResult, error) {
	return u.list(ctx, issueIDs, filter, false)
}

func (u *dependencyQueryUseCase) ListWithIssueMetadata(ctx context.Context, issueID string, filter DepListFilter) ([]*types.IssueWithDependencyMetadata, error) {
	return u.listWithMetadata(ctx, issueID, filter, false)
}

func (u *dependencyQueryUseCase) IterWithIssueMetadata(ctx context.Context, issueID string, filter DepListFilter) (storage.Iter[types.IssueWithDependencyMetadata], error) {
	return u.iterWithMetadata(ctx, issueID, filter, false)
}

func (u *dependencyQueryUseCase) CountByIssueID(ctx context.Context, issueID string, filter DepListFilter) (int64, error) {
	return u.countByID(ctx, issueID, filter, false)
}

func (u *dependencyQueryUseCase) GetForIssueIDs(ctx context.Context, ids []string) (map[string][]*types.Dependency, error) {
	if len(ids) == 0 {
		return map[string][]*types.Dependency{}, nil
	}
	issueRes, err := u.depRepo.ListByIssueIDs(ctx, ids, DepListOpts{Direction: DepDirectionOut})
	if err != nil {
		return nil, fmt.Errorf("GetForIssueIDs: %w", err)
	}
	out := issueRes.Outgoing
	if out == nil {
		out = make(map[string][]*types.Dependency)
	}
	wispRes, err := u.depRepo.ListByIssueIDs(ctx, ids, DepListOpts{Direction: DepDirectionOut, UseWispsTable: true})
	if err != nil && !dberrors.IsTableNotExist(err) {
		return nil, fmt.Errorf("GetForIssueIDs (wisps): %w", err)
	}
	for id, deps := range wispRes.Outgoing {
		out[id] = append(out[id], deps...)
	}
	return out, nil
}

func (u *dependencyQueryUseCase) ListByWispIDs(ctx context.Context, wispIDs []string, filter DepListFilter) (DepBulkResult, error) {
	return u.list(ctx, wispIDs, filter, true)
}

func (u *dependencyQueryUseCase) ListWispWithIssueMetadata(ctx context.Context, wispID string, filter DepListFilter) ([]*types.IssueWithDependencyMetadata, error) {
	return u.listWithMetadata(ctx, wispID, filter, true)
}

func (u *dependencyQueryUseCase) IterWispWithIssueMetadata(ctx context.Context, wispID string, filter DepListFilter) (storage.Iter[types.IssueWithDependencyMetadata], error) {
	return u.iterWithMetadata(ctx, wispID, filter, true)
}

func (u *dependencyQueryUseCase) CountByWispID(ctx context.Context, wispID string, filter DepListFilter) (int64, error) {
	return u.countByID(ctx, wispID, filter, true)
}

func (u *dependencyQueryUseCase) listWithMetadata(ctx context.Context, sourceID string, filter DepListFilter, useWisp bool) ([]*types.IssueWithDependencyMetadata, error) {
	if sourceID == "" {
		return nil, fmt.Errorf("list dep metadata: sourceID must not be empty")
	}
	out, err := u.depRepo.ListWithIssueMetadata(ctx, sourceID, DepListOpts{
		Types:         filter.Types,
		Direction:     filter.Direction,
		UseWispsTable: useWisp,
	})
	if err != nil {
		return nil, fmt.Errorf("list dep metadata: %w", err)
	}
	return out, nil
}

func (u *dependencyQueryUseCase) iterWithMetadata(ctx context.Context, sourceID string, filter DepListFilter, useWisp bool) (storage.Iter[types.IssueWithDependencyMetadata], error) {
	if sourceID == "" {
		return nil, fmt.Errorf("iter dep metadata: sourceID must not be empty")
	}
	it, err := u.depRepo.IterWithIssueMetadata(ctx, sourceID, DepListOpts{
		Types:         filter.Types,
		Direction:     filter.Direction,
		UseWispsTable: useWisp,
	})
	if err != nil {
		return nil, fmt.Errorf("iter dep metadata: %w", err)
	}
	return it, nil
}

func (u *dependencyQueryUseCase) countByID(ctx context.Context, sourceID string, filter DepListFilter, useWisp bool) (int64, error) {
	if sourceID == "" {
		return 0, fmt.Errorf("count by id: sourceID must not be empty")
	}
	n, err := u.depRepo.CountByID(ctx, sourceID, DepListOpts{
		Types:         filter.Types,
		Direction:     filter.Direction,
		UseWispsTable: useWisp,
	})
	if err != nil {
		return 0, fmt.Errorf("count by id: %w", err)
	}
	return n, nil
}

func (u *dependencyQueryUseCase) list(ctx context.Context, ids []string, filter DepListFilter, useWisp bool) (DepBulkResult, error) {
	if len(ids) == 0 {
		return DepBulkResult{
			Outgoing: map[string][]*types.Dependency{},
			Incoming: map[string][]*types.Dependency{},
		}, nil
	}
	out, err := u.depRepo.ListByIssueIDs(ctx, ids, DepListOpts{
		Types:         filter.Types,
		Direction:     filter.Direction,
		UseWispsTable: useWisp,
	})
	if err != nil {
		return DepBulkResult{}, fmt.Errorf("list deps: %w", err)
	}
	return out, nil
}

func (u *dependencyQueryUseCase) CountsByIssueIDs(ctx context.Context, issueIDs []string) (map[string]*types.DependencyCounts, error) {
	return u.counts(ctx, issueIDs, false)
}

func (u *dependencyQueryUseCase) CountsByWispIDs(ctx context.Context, wispIDs []string) (map[string]*types.DependencyCounts, error) {
	return u.counts(ctx, wispIDs, true)
}

func (u *dependencyQueryUseCase) counts(ctx context.Context, ids []string, useWisp bool) (map[string]*types.DependencyCounts, error) {
	if len(ids) == 0 {
		return map[string]*types.DependencyCounts{}, nil
	}
	out, err := u.depRepo.CountsByIssueIDs(ctx, ids, DepCountsOpts{UseWispsTable: useWisp})
	if err != nil {
		return nil, fmt.Errorf("dep counts: %w", err)
	}
	return out, nil
}

func (u *dependencyStatusUseCase) GetBlockingInfo(ctx context.Context, issueIDs []string) (BlockingInfo, error) {
	if len(issueIDs) == 0 {
		return BlockingInfo{
			BlockedBy: map[string][]string{},
			Blocks:    map[string][]string{},
			Parent:    map[string]string{},
		}, nil
	}
	out, err := u.depRepo.GetBlockingInfoAcrossIssuesAndWisps(ctx, issueIDs)
	if err != nil {
		return BlockingInfo{}, fmt.Errorf("GetBlockingInfo: %w", err)
	}
	return out, nil
}

func (u *dependencyStatusUseCase) IsBlocked(ctx context.Context, issueID string) (bool, []string, error) {
	return u.isBlocked(ctx, issueID, false)
}

func (u *dependencyStatusUseCase) IsWispBlocked(ctx context.Context, wispID string) (bool, []string, error) {
	return u.isBlocked(ctx, wispID, true)
}

func (u *dependencyStatusUseCase) isBlocked(ctx context.Context, id string, useWisp bool) (bool, []string, error) {
	if id == "" {
		return false, nil, fmt.Errorf("IsBlocked: id must not be empty")
	}
	blocked, blockers, err := u.depRepo.IsBlocked(ctx, id, DepListOpts{UseWispsTable: useWisp})
	if err != nil {
		return false, nil, fmt.Errorf("IsBlocked %s: %w", id, err)
	}
	return blocked, blockers, nil
}

func (u *dependencyGraphUseCase) DetectCycles(ctx context.Context) ([][]*types.Issue, error) {
	out, err := u.depRepo.DetectCycles(ctx)
	if err != nil {
		return nil, fmt.Errorf("DetectCycles: %w", err)
	}
	return out, nil
}

func (u *dependencyGraphUseCase) DetectCycleReport(ctx context.Context) (issueops.CycleReport, error) {
	out, err := u.depRepo.DetectCycleReport(ctx)
	if err != nil {
		return issueops.CycleReport{}, fmt.Errorf("DetectCycleReport: %w", err)
	}
	return out, nil
}

// WalkDependencyTree passes the request straight through.
//
// No pre-check and no error wrapping, unlike GetDependencyTree below: the
// request's whole vocabulary is validated inside the shared body, and its
// refusals are typed sentinels both front doors classify.
func (u *dependencyGraphUseCase) WalkDependencyTree(ctx context.Context, req issueops.WalkTreeRequest) (issueops.TreeResult, error) {
	return u.depRepo.WalkDependencyTree(ctx, req)
}

// CountEdges passes the request straight through, for WalkDependencyTree's
// reason: the request's whole vocabulary is validated inside the shared body,
// and its refusals are typed sentinels both front doors classify.
func (u *dependencyGraphUseCase) CountEdges(ctx context.Context, req issueops.EdgeCountRequest) (issueops.EdgeCountResult, error) {
	return u.depRepo.CountEdges(ctx, req)
}

func (u *dependencyGraphUseCase) GetDependencyTree(ctx context.Context, rootID string, opts DepTreeOpts) ([]*types.TreeNode, error) {
	if rootID == "" {
		return nil, fmt.Errorf("GetDependencyTree: rootID must not be empty")
	}
	out, err := u.depRepo.GetTree(ctx, rootID, opts)
	if err != nil {
		return nil, fmt.Errorf("GetDependencyTree: %w", err)
	}
	return out, nil
}
