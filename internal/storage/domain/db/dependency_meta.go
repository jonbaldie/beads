package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
	publicops "github.com/jonbaldie/beads/issueops"
)

func (r *dependencyMetadataRepository) ListWithIssueMetadata(ctx context.Context, sourceID string, opts domain.DepListOpts) ([]*types.IssueWithDependencyMetadata, error) {
	var out []*types.IssueWithDependencyMetadata
	if opts.Direction == domain.DepDirectionOut || opts.Direction == domain.DepDirectionBoth {
		deps, err := issueops.GetDependenciesWithMetadataInTx(ctx, r.runner, sourceID)
		if err != nil {
			return nil, err
		}
		out = append(out, filterDepsByType(deps, opts.Types)...)
	}
	if opts.Direction == domain.DepDirectionIn || opts.Direction == domain.DepDirectionBoth {
		deps, err := issueops.GetDependentsWithMetadataInTx(ctx, r.runner, sourceID)
		if err != nil {
			return nil, err
		}
		out = append(out, filterDepsByType(deps, opts.Types)...)
	}
	return out, nil
}

func (r *dependencyMetadataRepository) IterWithIssueMetadata(ctx context.Context, sourceID string, opts domain.DepListOpts) (storage.Iter[types.IssueWithDependencyMetadata], error) {
	items, err := r.ListWithIssueMetadata(ctx, sourceID, opts)
	if err != nil {
		return nil, err
	}
	return storage.NewSliceIter(items), nil
}

func (r *dependencyMetadataRepository) CountByID(ctx context.Context, sourceID string, opts domain.DepListOpts) (int64, error) {
	return issueops.CountDependencyEdgesInTx(ctx, r.runner, sourceID, opts.Direction, opts.Types)
}

func filterDepsByType(deps []*types.IssueWithDependencyMetadata, filter []types.DependencyType) []*types.IssueWithDependencyMetadata {
	if len(filter) == 0 {
		return deps
	}
	allowed := make(map[types.DependencyType]struct{}, len(filter))
	for _, t := range filter {
		allowed[t] = struct{}{}
	}
	out := make([]*types.IssueWithDependencyMetadata, 0, len(deps))
	for _, d := range deps {
		if _, ok := allowed[d.DependencyType]; ok {
			out = append(out, d)
		}
	}
	return out
}

func (r *dependencyBlockingRepository) IsBlocked(ctx context.Context, issueID string, _ domain.DepListOpts) (bool, []string, error) {
	blocked, blockers, err := issueops.IsBlockedInTx(ctx, r.runner, issueID)
	if err != nil {
		return false, nil, fmt.Errorf("db: DependencySQLRepository.IsBlocked %s: %w", issueID, err)
	}
	return blocked, blockers, nil
}

func (r *dependencyCycleRepository) DetectCycles(ctx context.Context) ([][]*types.Issue, error) {
	out, err := issueops.DetectCyclesInTx(ctx, r.runner)
	if err != nil {
		return nil, fmt.Errorf("db: DependencySQLRepository.DetectCycles: %w", err)
	}
	return out, nil
}

func (r *dependencyCycleRepository) DetectCycleReport(ctx context.Context) (publicops.CycleReport, error) {
	out, err := issueops.DetectCycleReportInTx(ctx, r.runner)
	if err != nil {
		return publicops.CycleReport{}, fmt.Errorf("db: DependencySQLRepository.DetectCycleReport: %w", err)
	}
	return out, nil
}

// WalkDependencyTree runs the SHARED walk body, unwrapped.
//
// It does NOT wrap the error the way its siblings above do, and that is the one
// thing to keep when editing it: the body publishes issueops.ErrValidation,
// storage.ErrNotFound and *issueops.ErrTooManyRows as the role's own vocabulary,
// and every one of those is classified by errors.Is/errors.As at both front
// doors and in the HTTP problem mapping. A `fmt.Errorf("db: ...: %w")` would keep
// them matchable but would also put this repository's name into the message a
// user reads, which the direct route never does for the same refusal.
func (r *dependencyTreeRepository) WalkDependencyTree(ctx context.Context, req publicops.WalkTreeRequest) (publicops.TreeResult, error) {
	return issueops.WalkDependencyTreeInTx(ctx, r.runner, req)
}

// CountEdges runs the SHARED edge-count body, unwrapped for
// WalkDependencyTree's reason: the body publishes issueops.ErrValidation as the
// role's own vocabulary, and a `fmt.Errorf("db: ...: %w")` would keep it
// matchable while putting this repository's name into a message the direct
// route never decorates.
func (r *dependencyTreeRepository) CountEdges(ctx context.Context, req publicops.EdgeCountRequest) (publicops.EdgeCountResult, error) {
	return issueops.ExecuteEdgeCount(ctx, r.runner, req)
}

func (r *dependencyTreeRepository) GetTree(ctx context.Context, rootID string, opts domain.DepTreeOpts) ([]*types.TreeNode, error) {
	if rootID == "" {
		return nil, errors.New("db: DependencySQLRepository.GetTree: rootID must not be empty")
	}
	if opts.Direction == domain.DepDirectionBoth {
		return nil, errors.New("db: DependencySQLRepository.GetTree: DepDirectionBoth not supported; callers must invoke once per direction and merge")
	}
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 50
	}
	reverse := opts.Direction == domain.DepDirectionIn
	out, err := issueops.GetDependencyTreeInTx(ctx, r.runner, rootID, maxDepth, opts.ShowAllPaths, reverse)
	if err != nil {
		return nil, fmt.Errorf("db: DependencySQLRepository.GetTree: %w", err)
	}
	return out, nil
}

func (r *dependencyCycleRepository) CycleThroughEdges(ctx context.Context, edges [][2]string) (string, error) {
	if len(edges) == 0 {
		return "", nil
	}
	graph := make(map[string][]string)
	if err := issueops.AppendSchedulingGraphInTx(ctx, r.runner, []string{"dependencies"}, graph); err != nil {
		return "", fmt.Errorf("db: DependencySQLRepository.CycleThroughEdges: %w", err)
	}
	if err := issueops.AppendSchedulingGraphInTx(ctx, r.runner, []string{"wisp_dependencies"}, graph); err != nil && !dberrors.IsTableNotExist(err) {
		return "", fmt.Errorf("db: DependencySQLRepository.CycleThroughEdges (wisps): %w", err)
	}
	return issueops.CycleThroughEdgesInGraph(graph, edges), nil
}

// WispSourceIDs classifies a batch of ids by plane in one scoped query. It is
// the proxied twin of the in-tx probe the store-backed dependency editor runs,
// and shares its implementation so the two answer the same question — down to
// treating a missing wisps table as "no wisps" rather than an error.
func (r *dependencyMetadataRepository) WispSourceIDs(ctx context.Context, ids []string) (map[string]struct{}, error) {
	set, err := issueops.WispIDSetInTx(ctx, r.runner, ids)
	if err != nil {
		return nil, fmt.Errorf("db: DependencySQLRepository.WispSourceIDs: %w", err)
	}
	return set, nil
}

func (r *dependencyMetadataRepository) GetDependencyRecordsForIssues(ctx context.Context, issueIDs []string) (map[string][]*types.Dependency, error) {
	if len(issueIDs) == 0 {
		return map[string][]*types.Dependency{}, nil
	}
	out, err := issueops.GetDependencyRecordsForIssuesInTx(ctx, r.runner, issueIDs)
	if err != nil {
		return nil, fmt.Errorf("db: DependencySQLRepository.GetDependencyRecordsForIssues: %w", err)
	}
	return out, nil
}

func (r *dependencyMetadataRepository) GetWispDependencyRecordsForIDs(ctx context.Context, wispIDs []string) (map[string][]*types.Dependency, error) {
	if len(wispIDs) == 0 {
		return map[string][]*types.Dependency{}, nil
	}
	out, err := issueops.GetDependencyRecordsForIssuesFromTableInTx(ctx, r.runner, "wisp_dependencies", wispIDs)
	if err != nil {
		if dberrors.IsTableNotExist(err) {
			return map[string][]*types.Dependency{}, nil
		}
		return nil, fmt.Errorf("db: DependencySQLRepository.GetWispDependencyRecordsForIDs: %w", err)
	}
	return out, nil
}
