package main

import (
	"context"
	"fmt"
	"io"

	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
)

func runGraphProxiedServer(ctx context.Context, out io.Writer, args []string, opts graphOptions) error {
	uw, err := openProxiedListUOW(ctx)
	if err != nil {
		return HandleError("%v", err)
	}
	defer uw.Close(ctx)

	if opts.all {
		subgraphs, err := loadAllGraphSubgraphsUOW(ctx, uw)
		if err != nil {
			return HandleErrorRespectJSON("loading all issues: %v", err)
		}
		return renderGraphAllSubgraphs(out, subgraphs, opts)
	}

	root, err := uw.IssueUseCase().GetIssue(ctx, args[0])
	if err != nil || root == nil {
		return HandleErrorRespectJSON("issue '%s' not found", args[0])
	}
	subgraph, err := loadGraphSubgraphUOW(ctx, uw, root)
	if err != nil {
		return HandleErrorRespectJSON("loading graph: %v", err)
	}
	return renderGraphSingleSubgraph(out, subgraph, opts)
}

func runGraphCheckProxiedServer(ctx context.Context) error {
	uw, err := openProxiedListUOW(ctx)
	if err != nil {
		return HandleError("%v", err)
	}
	defer uw.Close(ctx)

	cycles, err := uw.DependencyUseCase().DetectCycles(ctx)
	if err != nil {
		return HandleErrorRespectJSON("cycle detection failed: %v", err)
	}
	return renderGraphCheck(cycles)
}

func loadGraphSubgraphUOW(ctx context.Context, uw uow.UnitOfWork, root *types.Issue) (*TemplateSubgraph, error) {
	subgraph := &TemplateSubgraph{
		Root:     root,
		Issues:   []*types.Issue{root},
		IssueMap: map[string]*types.Issue{root.ID: root},
	}

	queue := []string{root.ID}
	visited := map[string]bool{root.ID: true}
	for {
		currentID, ok := popGraphQueue(&queue)
		if !ok {
			break
		}
		appendGraphUOWNeighbors(ctx, uw, subgraph, visited, &queue, currentID, domain.DepDirectionIn)
		appendGraphUOWNeighbors(ctx, uw, subgraph, visited, &queue, currentID, domain.DepDirectionOut)
	}

	ids := make([]string, 0, len(subgraph.Issues))
	for _, iss := range subgraph.Issues {
		ids = append(ids, iss.ID)
	}
	recs, err := uw.DependencyUseCase().GetIssueDependencyRecords(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("loading dependencies: %w", err)
	}
	appendGraphUOWDependencies(subgraph, recs)

	return subgraph, nil
}

func popGraphQueue(queue *[]string) (string, bool) {
	if len(*queue) == 0 {
		return "", false
	}
	currentID := (*queue)[0]
	*queue = (*queue)[1:]
	return currentID, true
}

func appendGraphUOWNeighbors(ctx context.Context, uw uow.UnitOfWork, subgraph *TemplateSubgraph, visited map[string]bool, queue *[]string, currentID string, direction domain.DepDirection) {
	metas, err := uw.DependencyUseCase().ListWithIssueMetadata(ctx, currentID, domain.DepListFilter{Direction: direction})
	if err != nil {
		return
	}
	for _, d := range metas {
		iss := d.Issue
		if visited[iss.ID] {
			continue
		}
		visited[iss.ID] = true
		subgraph.Issues = append(subgraph.Issues, &iss)
		subgraph.IssueMap[iss.ID] = &iss
		*queue = append(*queue, iss.ID)
	}
}

func appendGraphUOWDependencies(subgraph *TemplateSubgraph, recs map[string][]*types.Dependency) {
	for _, iss := range subgraph.Issues {
		for _, dep := range recs[iss.ID] {
			if _, ok := subgraph.IssueMap[dep.DependsOnID]; ok {
				subgraph.Dependencies = append(subgraph.Dependencies, dep)
			}
		}
	}
}

func loadAllGraphSubgraphsUOW(ctx context.Context, uw uow.UnitOfWork) ([]*TemplateSubgraph, error) {
	var allIssues []*types.Issue
	for _, status := range []types.Status{types.StatusOpen, types.StatusInProgress, types.StatusBlocked} {
		statusCopy := status
		page, err := uw.IssueUseCase().SearchIssues(ctx, "", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Status: &statusCopy}})
		if err != nil {
			return nil, fmt.Errorf("failed to search issues: %w", err)
		}
		allIssues = append(allIssues, page.Items...)
	}

	if len(allIssues) == 0 {
		return nil, nil
	}

	issueMap := make(map[string]*types.Issue, len(allIssues))
	ids := make([]string, 0, len(allIssues))
	for _, issue := range allIssues {
		issueMap[issue.ID] = issue
		ids = append(ids, issue.ID)
	}

	recs, err := uw.DependencyUseCase().GetIssueDependencyRecords(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("failed to load dependencies: %w", err)
	}
	var allDeps []*types.Dependency
	for _, issue := range allIssues {
		for _, dep := range recs[issue.ID] {
			if _, ok := issueMap[dep.DependsOnID]; ok {
				allDeps = append(allDeps, dep)
			}
		}
	}

	return assembleAllSubgraphs(allIssues, issueMap, allDeps), nil
}
