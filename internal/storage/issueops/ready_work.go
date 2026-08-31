package issueops

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage/sqlbuild"
	"github.com/jonbaldie/beads/internal/types"
)

type readyWorkPredicates struct {
	whereSQL string
	// whereArgs binds only the WHERE placeholders (no ORDER BY params); it is
	// what a bare COUNT(*) over the ready predicate needs. args carries the
	// WHERE params followed by the ORDER BY params for the full page query.
	whereArgs        []interface{}
	orderBySQL       string
	limitSQL         string
	args             []interface{}
	deferredChildIDs []string
}

func readyWorkPageSize(limit int) int {
	if limit <= 0 {
		return 0
	}
	const minPageSize = 100
	if limit < minPageSize {
		return minPageSize
	}
	return limit
}

func buildReadyWorkOrder(policy types.SortPolicy) sqlbuild.ReadyWorkOrder {
	return sqlbuild.BuildReadyWorkOrder(policy, "created_at", "priority")
}

// buildReadyWorkPredicates computes the ID sets the ready-work WHERE clause
// needs (children of deferred parents, parent descendants), then delegates
// the clause text to sqlbuild so both stacks share ready semantics.
func buildReadyWorkPredicates(ctx context.Context, tx DBTX, filter types.WorkFilter, tables FilterTables) (*readyWorkPredicates, error) {
	var inputs sqlbuild.ReadyWorkWhereInputs
	if !filter.IncludeDeferred {
		deferredChildIDs, dcErr := getChildrenOfDeferredParentsInTx(ctx, tx)
		if dcErr != nil {
			return nil, fmt.Errorf("get ready work: compute deferred parent children: %w", dcErr)
		}
		inputs.DeferredChildIDs = deferredChildIDs
	}
	// Parent filtering: return all transitive descendants of parentID.
	// GH#3396: previously was a one-hop subquery against dependencies, so
	// grandchildren were silently dropped despite the help text and
	// WorkFilter.ParentID godoc both promising "descendants (recursive)".
	if filter.ParentID != nil {
		descendantIDs, descErr := GetDescendantIDsInTx(ctx, tx, *filter.ParentID, 0)
		if descErr != nil {
			return nil, fmt.Errorf("get parent descendants: %w", descErr)
		}
		inputs.ParentDescendantIDs = descendantIDs
	}

	whereSQL, whereArgs, err := sqlbuild.BuildReadyWorkWhere(filter, tables, inputs)
	if err != nil {
		return nil, err
	}

	orderBy := buildReadyWorkOrder(filter.SortPolicy)
	args := make([]interface{}, 0, len(whereArgs)+len(orderBy.Args))
	args = append(args, whereArgs...)
	args = append(args, orderBy.Args...)

	var limitSQL string
	if eff := EffectiveSearchLimit(filter.Limit, filter.MaxRows); eff > 0 {
		limitSQL = fmt.Sprintf("LIMIT %d", eff)
	}

	return &readyWorkPredicates{
		whereSQL:         whereSQL,
		whereArgs:        whereArgs,
		orderBySQL:       orderBy.SQL,
		limitSQL:         limitSQL,
		args:             args,
		deferredChildIDs: inputs.DeferredChildIDs,
	}, nil
}

//nolint:gosec // G201: whereSQL/orderBySQL built from hardcoded strings and ? placeholders
func GetReadyWorkInTx(
	ctx context.Context,
	tx DBTX,
	filter types.WorkFilter,
) ([]*types.Issue, error) {
	preds, err := buildReadyWorkPredicates(ctx, tx, filter, IssuesFilterTables)
	if err != nil {
		return nil, err
	}

	//nolint:gosec // G201: fragments are hardcoded; only ? placeholders carry user input.
	query := fmt.Sprintf(`
		SELECT id FROM issues
		%s
		%s
		%s
	`, preds.whereSQL, preds.orderBySQL, preds.limitSQL)

	issueIDs, err := queryReadyIssueIDPage(ctx, tx, query, preds.args)
	if err != nil {
		return nil, err
	}

	ordered, err := getReadyIssuesInOrderInTx(ctx, tx, issueIDs)
	if err != nil {
		return nil, err
	}

	wisps, wErr := getReadyWispsInTx(ctx, tx, filter, preds.deferredChildIDs)
	if wErr != nil {
		return nil, wErr
	}
	if len(wisps) > 0 {
		ordered = mergeReadyWisps(ordered, wisps, filter)
	}

	// Apply the defensive cap on the row count returned to the caller.
	// LIMIT cap+1 was issued above so a count of cap+1 indicates overage.
	if err := EnforceMaxRowsCap(len(ordered), filter.MaxRows, filter.MaxRowsSource); err != nil {
		return nil, err
	}

	return ordered, nil
}

func getReadyIssuesInOrderInTx(ctx context.Context, tx DBTX, issueIDs []string) ([]*types.Issue, error) {
	issues, err := GetIssuesByIDsInTx(ctx, tx, issueIDs, nil)
	if err != nil {
		return nil, fmt.Errorf("get ready work: fetch issues: %w", err)
	}
	issueMap := make(map[string]*types.Issue, len(issues))
	for _, issue := range issues {
		issueMap[issue.ID] = issue
	}
	ordered := make([]*types.Issue, 0, len(issueIDs))
	for _, id := range issueIDs {
		if issue, ok := issueMap[id]; ok {
			ordered = append(ordered, issue)
		}
	}
	return ordered, nil
}

func mergeReadyWisps(ordered []*types.Issue, wisps []*types.Issue, filter types.WorkFilter) []*types.Issue {
	// Prefer the canonical wisp record when an ID exists in both tables (be-iabdi).
	wispByID := make(map[string]*types.Issue, len(wisps))
	for _, w := range wisps {
		wispByID[w.ID] = w
	}
	var kept []*types.Issue
	for _, issue := range ordered {
		if wispByID[issue.ID] == nil {
			kept = append(kept, issue)
		}
	}
	kept = append(kept, wisps...)
	sortReadyIssues(kept, filter.SortPolicy)
	if filter.Limit > 0 && len(kept) > filter.Limit {
		kept = kept[:filter.Limit]
	}
	return kept
}
