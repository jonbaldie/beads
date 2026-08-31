package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/sqlbuild"
	"github.com/jonbaldie/beads/internal/types"
)

const queryBatchSize = 200

// filterTables aliases the shared table-name config so both stacks build
// against the same definitions (bd-6dnrw.46).
type filterTables = sqlbuild.FilterTables

var (
	issuesFilterTables = sqlbuild.IssuesFilterTables
	wispsFilterTables  = sqlbuild.WispsFilterTables
)

type issueSearchRepository struct {
	*issueRepositoryCore
	hydrator *issueSearchHydrator
}

type issueSearchHydrator struct {
	*issueRepositoryCore
}

func (r *issueSearchRepository) SearchAcrossIssuesAndWisps(ctx context.Context, query string, filter types.IssueFilter) (domain.SearchPage, error) {
	return r.searchAcrossIssuesAndWisps(ctx, query, filter)
}

func (r *issueSearchRepository) searchAcrossIssuesAndWisps(ctx context.Context, query string, filter types.IssueFilter) (domain.SearchPage, error) {
	page, handled, err := r.searchEphemeral(ctx, query, filter)
	if err != nil {
		return domain.SearchPage{}, err
	}
	if handled {
		return page, nil
	}

	if filter.SkipWisps {
		return r.searchIssuesOnly(ctx, query, filter)
	}
	return r.searchIssuesAndWisps(ctx, query, filter)
}

func (r *issueSearchRepository) searchEphemeral(ctx context.Context, query string, filter types.IssueFilter) (domain.SearchPage, bool, error) {
	if filter.Ephemeral == nil || !*filter.Ephemeral {
		return domain.SearchPage{}, false, nil
	}
	page, err := r.searchTable(ctx, query, filter, wispsFilterTables)
	if err != nil {
		if dberrors.IsTableNotExist(err) {
			return domain.SearchPage{}, false, nil
		}
		return domain.SearchPage{}, false, fmt.Errorf("search wisps (ephemeral filter): %w", err)
	}
	if len(page.Items) == 0 {
		return domain.SearchPage{}, false, nil
	}
	return page, true, nil
}

func (r *issueSearchRepository) searchIssuesOnly(ctx context.Context, query string, filter types.IssueFilter) (domain.SearchPage, error) {
	page, err := r.searchTable(ctx, query, filter, issuesFilterTables)
	if err != nil {
		return domain.SearchPage{}, fmt.Errorf("search issues: %w", err)
	}
	return page, nil
}

func (r *issueSearchRepository) searchIssuesAndWisps(ctx context.Context, query string, filter types.IssueFilter) (domain.SearchPage, error) {
	empty, probeErr := wispsTableEmptyOrMissing(ctx, r.issueRepositoryCore.runner)
	if probeErr != nil {
		return domain.SearchPage{}, fmt.Errorf("search wisps (merge): probe: %w", probeErr)
	}
	if empty {
		return r.searchIssuesOnly(ctx, query, filter)
	}

	return r.searchUnion(ctx, query, filter)
}

func (r *issueSearchRepository) searchUnion(ctx context.Context, query string, filter types.IssueFilter) (domain.SearchPage, error) {
	outerOrderBy := unionOrderBySQL(filter.SortBy, filter.SortDesc)
	window := searchWindowForFilter(filter)
	legWindow := legWindowSQL(outerOrderBy, window)

	iSub, iArgs, err := r.buildUnionSubquery(query, filter, issuesFilterTables, "i", legWindow)
	if err != nil {
		return domain.SearchPage{}, fmt.Errorf("search union (issues): %w", err)
	}
	wSub, wArgs, err := r.buildUnionSubquery(query, filter, wispsFilterTables, "w", legWindow)
	if err != nil {
		return domain.SearchPage{}, fmt.Errorf("search union (wisps): %w", err)
	}

	// EACH LEG IS PARENTHESIZED, and it is not decoration. A leg that carries
	// its own ORDER BY and LIMIT (legWindowSQL) is a syntax error inside a bare
	// UNION ALL — the engine reads the clause as belonging to the union — so the
	// parentheses are what let the window be pushed down at all.
	//nolint:gosec // G201: subqueries built from hardcoded table names and ? placeholders.
	unionSQL := fmt.Sprintf("SELECT id, src FROM ((%s) UNION ALL (%s)) merged %s %s",
		iSub, wSub, outerOrderBy, window.sql)

	args := make([]any, 0, len(iArgs)+len(wArgs))
	args = append(args, iArgs...)
	args = append(args, wArgs...)

	rows, err := r.runner.QueryContext(ctx, unionSQL, args...)
	if err != nil {
		return domain.SearchPage{}, fmt.Errorf("search union: %w", err)
	}
	page, err := scanIDSrcPage(rows)
	if err != nil {
		return domain.SearchPage{}, fmt.Errorf("search union: %w", err)
	}
	page.SortGoSide(filter.SortBy, filter.SortDesc)
	hasMore, err := page.FinishWindow(window)
	if err != nil {
		return domain.SearchPage{}, err
	}

	issuesByID, err := r.hydrator.fetchIssuesByIDs(ctx, page.issueIDs, issuesFilterTables, filter)
	if err != nil {
		return domain.SearchPage{}, fmt.Errorf("search union (hydrate issues): %w", err)
	}
	wispsByID, err := r.hydrator.fetchIssuesByIDs(ctx, page.wispIDs, wispsFilterTables, filter)
	if err != nil && !dberrors.IsTableNotExist(err) {
		return domain.SearchPage{}, fmt.Errorf("search union (hydrate wisps): %w", err)
	}

	out := reassembleBySrc(page.ordered, issuesByID, wispsByID)
	return domain.SearchPage{Items: out, HasMore: hasMore}, nil
}

// buildUnionSubquery renders one leg of the wisp-inclusive UNION ALL.
//
// legWindow is the ORDER BY and LIMIT the leg carries, from legWindowSQL. It is
// the leg's whole share of the caller's window, and pushing it down is sound
// for the reason a top-N is: under a TOTAL order, a row in the union's top-N is
// in its own leg's top-N too, because anything ahead of it in its leg is ahead
// of it globally. Every ORDER BY sqlbuild renders ends in `id ASC`, so the
// order is total and there are no ties for the bound to break arbitrarily.
//
// It is empty when the sort has no SQL ORDER BY, and that is not a missed case:
// a LIMIT on an UNORDERED leg is the bug this exists to remove, one level
// further down and harder to see.
func (r *issueSearchRepository) buildUnionSubquery(query string, filter types.IssueFilter, tables filterTables, srcTag, legWindow string) (string, []any, error) {
	return buildUnionSubquery(query, filter, tables, srcTag, legWindow)
}

func buildUnionSubquery(query string, filter types.IssueFilter, tables filterTables, srcTag, legWindow string) (string, []any, error) {
	plan := buildLabelDrivenSearch(filter, tables)
	whereClauses, args, err := buildIssueFilterClauses(query, plan.Filter, tables)
	if err != nil {
		return "", nil, err
	}
	whereClauses, args = plan.MergeInto(whereClauses, args)
	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = "WHERE " + strings.Join(whereClauses, " AND ")
	}
	selectKw := "SELECT"
	if plan.Distinct {
		selectKw = "SELECT DISTINCT"
	}
	//nolint:gosec // G201: srcTag is a hardcoded 'i' or 'w'; fromSQL/whereSQL/legWindow composed from fixed table names, sort aliases and ? placeholders.
	sub := fmt.Sprintf("%s id, '%s' AS src, %s FROM %s %s %s",
		selectKw, srcTag, unionSortColumnsSQL, plan.FromSQL, whereSQL, legWindow)
	return sub, args, nil
}

// legWindowSQL is the window each UNION leg carries: the SAME ordering the
// outer query applies, plus the deepest row index that outer query can reach.
//
// THE ORDER CLAUSE IS REUSED VERBATIM rather than rebuilt, and that is what
// makes the bound safe. unionOrderBySQL names `id` and the `sort_*` aliases,
// both of which every leg projects, so one string is valid in both positions
// and the leg cannot come to rest in a different order than the one the outer
// query is about to cut against.
//
// legLimit is the outer bound PLUS any offset the engine carries, because those
// are rows the outer query reads and discards — a leg cut to the page size
// alone would starve an offset page of exactly the rows it was going to skip
// past. An offset this package keeps for itself (the capped case) is already
// inside the bound.
func legWindowSQL(outerOrderBy string, w searchWindow) string {
	if outerOrderBy == "" || w.legLimit <= 0 {
		return ""
	}
	return fmt.Sprintf("%s LIMIT %d", outerOrderBy, w.legLimit)
}

func (r *issueSearchHydrator) fetchIssuesByIDs(ctx context.Context, ids []string, tables filterTables, filter types.IssueFilter) (map[string]*types.Issue, error) {
	if len(ids) == 0 {
		return map[string]*types.Issue{}, nil
	}

	placeholders, args := buildInPlaceholders(ids)

	//nolint:gosec // G201: tables.Main is "issues" or "wisps"; placeholders are ?.
	fetchSQL := fmt.Sprintf(`SELECT %s FROM %s %s WHERE id IN (%s)`,
		issueSelectColumns, tables.Main, sqlbuild.LeaseJoin(tables.Main), placeholders)
	rows, err := r.issueRepositoryCore.runner.QueryContext(ctx, fetchSQL, args...)
	if err != nil {
		return nil, err
	}

	out := make(map[string]*types.Issue, len(ids))
	ordered := make([]*types.Issue, 0, len(ids))
	for rows.Next() {
		issue, scanErr := scanIssue(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan: %w", scanErr)
		}
		out[issue.ID] = issue
		ordered = append(ordered, issue)
	}

	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}

	if err := r.hydrateIssues(ctx, ordered, tables, filter.IncludeDependencies, filter.SkipLabels); err != nil {
		return nil, fmt.Errorf("hydrate: %w", err)
	}
	return out, nil
}

func (r *issueSearchRepository) searchTable(ctx context.Context, query string, filter types.IssueFilter, tables filterTables) (domain.SearchPage, error) {
	plan, err := buildTableSearchPlan(query, filter, tables)
	if err != nil {
		return domain.SearchPage{}, err
	}
	if filter.Limit > 0 && !filter.NoIDShrink {
		return r.searchTableByIDs(ctx, filter, tables, plan)
	}
	return r.searchTableRows(ctx, filter, tables, plan)
}

type tableSearchPlan struct {
	fromSQL  string
	whereSQL string
	selectKw string
	args     []any
}

func buildTableSearchPlan(query string, filter types.IssueFilter, tables filterTables) (tableSearchPlan, error) {
	plan := buildLabelDrivenSearch(filter, tables)
	whereClauses, args, err := buildIssueFilterClauses(query, plan.Filter, tables)
	if err != nil {
		return tableSearchPlan{}, err
	}
	whereClauses, args = plan.MergeInto(whereClauses, args)

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	selectKw := "SELECT "
	if plan.Distinct {
		selectKw = "SELECT DISTINCT "
	}
	return tableSearchPlan{fromSQL: plan.FromSQL, whereSQL: whereSQL, selectKw: selectKw, args: args}, nil
}

func (r *issueSearchRepository) searchTableByIDs(ctx context.Context, filter types.IssueFilter, tables filterTables, plan tableSearchPlan) (domain.SearchPage, error) {
	ids, hasMore, err := r.scanFilterIDs(ctx, plan.selectKw, plan.fromSQL, plan.whereSQL, plan.args, filter, tables)
	if err != nil {
		return domain.SearchPage{}, err
	}
	if len(ids) == 0 {
		return domain.SearchPage{}, nil
	}
	byID, err := r.hydrator.fetchIssuesByIDs(ctx, ids, tables, filter)
	if err != nil {
		return domain.SearchPage{}, fmt.Errorf("search %s (hydrate): %w", tables.Main, err)
	}
	return domain.SearchPage{Items: orderByIDs(ids, byID), HasMore: hasMore}, nil
}

func (r *issueSearchRepository) searchTableRows(ctx context.Context, filter types.IssueFilter, tables filterTables, plan tableSearchPlan) (domain.SearchPage, error) {
	orderBy := orderBySQL(filter.SortBy, filter.SortDesc, "")
	window := searchWindowForFilter(filter)

	//nolint:gosec // G201: SQL fragments from fixed table names and parameterized filters.
	querySQL := fmt.Sprintf(`%s%s FROM %s %s %s %s %s`,
		plan.selectKw, issueSelectColumns, plan.fromSQL, sqlbuild.LeaseJoin(tables.Main), plan.whereSQL, orderBy, window.sql)

	rows, err := r.runner.QueryContext(ctx, querySQL, plan.args...)
	if err != nil {
		return domain.SearchPage{}, fmt.Errorf("search %s: %w", tables.Main, err)
	}

	var issues []*types.Issue
	seen := make(map[string]bool)
	for rows.Next() {
		issue, scanErr := scanIssue(rows)
		if scanErr != nil {
			_ = rows.Close()
			return domain.SearchPage{}, fmt.Errorf("search %s: scan: %w", tables.Main, scanErr)
		}
		if seen[issue.ID] {
			continue
		}
		seen[issue.ID] = true
		issues = append(issues, issue)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return domain.SearchPage{}, fmt.Errorf("search %s: rows: %w", tables.Main, err)
	}

	sortRowsGoSide(issues, func(i *types.Issue) string { return i.ID }, filter.SortBy, filter.SortDesc)
	items, hasMore, err := finishWindow(issues, window)
	if err != nil {
		return domain.SearchPage{}, err
	}

	if err := r.hydrator.hydrateIssues(ctx, items, tables, filter.IncludeDependencies, filter.SkipLabels); err != nil {
		return domain.SearchPage{}, fmt.Errorf("search %s: hydrate: %w", tables.Main, err)
	}

	return domain.SearchPage{Items: items, HasMore: hasMore}, nil
}

func (r *issueSearchRepository) scanFilterIDs(ctx context.Context, selectKw, fromSQL, whereSQL string, args []any, filter types.IssueFilter, tables filterTables) ([]string, bool, error) {
	orderBy := orderBySQL(filter.SortBy, filter.SortDesc, tables.Main)
	window := searchWindowForFilter(filter)
	//nolint:gosec // G201: SQL fragments from fixed table names and parameterized filters.
	idQuery := fmt.Sprintf(`%s%s.id FROM %s %s %s %s`,
		selectKw, tables.Main, fromSQL, whereSQL, orderBy, window.sql)

	rows, err := r.runner.QueryContext(ctx, idQuery, args...)
	if err != nil {
		return nil, false, fmt.Errorf("search %s (id scan): %w", tables.Main, err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, false, fmt.Errorf("search %s (id scan): scan: %w", tables.Main, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("search %s (id scan): rows: %w", tables.Main, err)
	}

	sortRowsGoSide(ids, func(id string) string { return id }, filter.SortBy, filter.SortDesc)
	return finishWindow(ids, window)
}
