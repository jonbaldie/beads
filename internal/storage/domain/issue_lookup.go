package domain

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/types"
)

func (u *issueLookupModule) GetIssue(ctx context.Context, id string) (*types.Issue, error) {
	return u.get(ctx, id, false)
}

func (u *issueLookupModule) AsOf(ctx context.Context, id, ref string) (*types.Issue, error) {
	if id == "" {
		return nil, fmt.Errorf("as of: id must not be empty")
	}
	issue, err := u.issueRepo.AsOf(ctx, id, ref)
	if err != nil {
		return nil, fmt.Errorf("as of %s @ %s: %w", id, ref, err)
	}
	return issue, nil
}

func (u *issueLookupModule) GetWisp(ctx context.Context, id string) (*types.Issue, error) {
	return u.get(ctx, id, true)
}

func (u *issueLookupModule) get(ctx context.Context, id string, useWisp bool) (*types.Issue, error) {
	if id == "" {
		return nil, fmt.Errorf("get: id must not be empty")
	}
	issue, err := u.issueRepo.Get(ctx, id, IssueTableOpts{UseWispsTable: useWisp})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", id, err)
	}
	return issue, nil
}

func (u *issueLookupModule) GetIssuesByIDs(ctx context.Context, ids []string) ([]*types.Issue, error) {
	return u.getByIDs(ctx, ids, false)
}

func (u *issueLookupModule) FindWispDependentsRecursive(ctx context.Context, ids []string) (map[string]bool, error) {
	return u.issueRepo.FindWispDependentsRecursive(ctx, ids)
}

func (u *issueLookupModule) GetWispsByIDs(ctx context.Context, ids []string) ([]*types.Issue, error) {
	return u.getByIDs(ctx, ids, true)
}

func (u *issueLookupModule) getByIDs(ctx context.Context, ids []string, useWisp bool) ([]*types.Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out, err := u.issueRepo.GetByIDs(ctx, ids, IssueTableOpts{UseWispsTable: useWisp})
	if err != nil {
		return nil, fmt.Errorf("getByIDs: %w", err)
	}
	return out, nil
}
