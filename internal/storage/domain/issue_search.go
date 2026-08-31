package domain

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/types"
)

func (u *issueUpdateModule) PromoteWisp(ctx context.Context, id, actor string) error {
	if id == "" {
		return fmt.Errorf("promote wisp: id must not be empty")
	}
	return u.issueRepo.PromoteFromEphemeral(ctx, id, actor)
}

func (u *issueSearchModule) SearchIssues(ctx context.Context, query string, filter types.IssueFilter) (SearchPage, error) {
	out, err := u.issueRepo.SearchAcrossIssuesAndWisps(ctx, query, filter)
	if err != nil {
		return SearchPage{}, fmt.Errorf("SearchIssues: %w", err)
	}
	return out, nil
}

func (u *issueSearchModule) SearchIssueIDs(ctx context.Context, query string, filter types.IssueFilter) ([]string, error) {
	out, err := u.issueRepo.SearchIssueIDs(ctx, query, filter)
	if err != nil {
		return nil, fmt.Errorf("SearchIssueIDs: %w", err)
	}
	return out, nil
}

func (u *issueSearchModule) SearchIssuesWithCounts(ctx context.Context, query string, filter types.IssueFilter) (SearchCountsPage, error) {
	out, err := u.issueRepo.SearchAcrossIssuesAndWispsWithCounts(ctx, query, filter)
	if err != nil {
		return SearchCountsPage{}, fmt.Errorf("SearchIssuesWithCounts: %w", err)
	}
	return out, nil
}

func (u *issueSearchModule) GetReadyWork(ctx context.Context, filter types.WorkFilter) (SearchPage, error) {
	out, err := u.issueRepo.GetReadyWork(ctx, filter)
	if err != nil {
		return SearchPage{}, fmt.Errorf("GetReadyWork: %w", err)
	}
	return out, nil
}

func (u *issueSearchModule) GetReadyWorkWithCounts(ctx context.Context, filter types.WorkFilter) (SearchCountsPage, error) {
	out, err := u.issueRepo.GetReadyWorkWithCounts(ctx, filter)
	if err != nil {
		return SearchCountsPage{}, fmt.Errorf("GetReadyWorkWithCounts: %w", err)
	}
	return out, nil
}

func (u *issueSearchModule) GetDescendants(ctx context.Context, rootID string, filter types.IssueFilter) ([]*types.Issue, error) {
	if rootID == "" {
		return nil, fmt.Errorf("GetDescendants: rootID must not be empty")
	}
	out, err := u.issueRepo.GetDescendants(ctx, rootID, filter)
	if err != nil {
		return nil, fmt.Errorf("GetDescendants: %w", err)
	}
	return out, nil
}
