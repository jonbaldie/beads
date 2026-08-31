package issueops

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/storage/sqlbuild"
	"github.com/jonbaldie/beads/internal/types"
)

func getReadyWispsInTx(ctx context.Context, tx DBTX, filter types.WorkFilter, deferredChildIDs []string) ([]*types.Issue, error) {
	empty, err := wispsTableEmptyOrMissingInTx(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("search wisps (ready work): probe: %w", err)
	}
	if empty {
		return nil, nil
	}

	wispFilter := readyWorkWispIssueFilter(filter)
	if filter.Limit <= 0 {
		return getUnboundedReadyWispsInTx(ctx, tx, filter, wispFilter, deferredChildIDs)
	}
	return getPagedReadyWispsInTx(ctx, tx, filter, wispFilter, deferredChildIDs)
}

func getUnboundedReadyWispsInTx(ctx context.Context, tx DBTX, filter types.WorkFilter, wispFilter types.IssueFilter, deferredChildIDs []string) ([]*types.Issue, error) {
	wispFilter.Limit = 0
	wisps, err := searchTableInTxT(ctx, tx, "", wispFilter, WispsFilterTables, issueProjection)
	if err != nil {
		if isTableNotExistError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("search wisps (ready work): %w", err)
	}
	return filterReadyWispsInTx(ctx, tx, filter, wisps, deferredChildIDs)
}

func getPagedReadyWispsInTx(ctx context.Context, tx DBTX, filter types.WorkFilter, wispFilter types.IssueFilter, deferredChildIDs []string) ([]*types.Issue, error) {
	pageSize := readyWorkPageSize(filter.Limit)
	orderBy := buildReadyWorkOrder(filter.SortPolicy)
	ready := make([]*types.Issue, 0, filter.Limit)
	for offset := 0; ; offset += pageSize {
		if len(ready) >= filter.Limit {
			break
		}
		pageReady, done, missing, err := readyWispPageInTx(ctx, tx, filter, wispFilter, deferredChildIDs, orderBy, pageSize, offset)
		if err != nil {
			return nil, err
		}
		if missing {
			return nil, nil
		}
		ready = appendReadyWispPage(ready, pageReady, filter.Limit)
		if done {
			break
		}
	}
	return ready, nil
}

func readyWispPageInTx(ctx context.Context, tx DBTX, filter types.WorkFilter, wispFilter types.IssueFilter, deferredChildIDs []string, orderBy sqlbuild.ReadyWorkOrder, pageSize, offset int) (pageReady []*types.Issue, done, missing bool, err error) {
	pageIDs, err := queryReadyWispIssueIDPage(ctx, tx, wispFilter, !filter.IncludeDeferred, orderBy, pageSize, offset)
	if err != nil {
		if isTableNotExistError(err) {
			return nil, false, true, nil
		}
		return nil, false, false, fmt.Errorf("search wisps (ready work): %w", err)
	}
	if len(pageIDs) == 0 {
		return nil, true, false, nil
	}

	pageWisps, err := getWispIssuesByIDsInOrderInTx(ctx, tx, pageIDs)
	if err != nil {
		return nil, false, false, fmt.Errorf("search wisps (ready work): %w", err)
	}
	pageReady, err = filterReadyWispsInTx(ctx, tx, filter, pageWisps, deferredChildIDs)
	if err != nil {
		return nil, false, false, err
	}
	return pageReady, len(pageIDs) < pageSize, false, nil
}

func appendReadyWispPage(ready, page []*types.Issue, limit int) []*types.Issue {
	for _, wisp := range page {
		ready = append(ready, wisp)
		if len(ready) >= limit {
			break
		}
	}
	return ready
}

func queryReadyWispIssueIDPage(ctx context.Context, tx DBTX, filter types.IssueFilter, excludeDeferred bool, orderBy sqlbuild.ReadyWorkOrder, limit, offset int) ([]string, error) {
	plan := sqlbuild.BuildLabelDrivenSearch(filter, WispsFilterTables)
	whereClauses, args, err := BuildIssueFilterClauses("", plan.Filter, WispsFilterTables)
	if err != nil {
		return nil, err
	}
	whereClauses, args = plan.MergeInto(whereClauses, args)
	if excludeDeferred {
		whereClauses = append(whereClauses, "(defer_until IS NULL OR defer_until <= UTC_TIMESTAMP())")
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	selectSQL := "SELECT "
	if plan.Distinct {
		selectSQL = "SELECT DISTINCT "
	}
	args = append(args, orderBy.Args...)
	//nolint:gosec // G201: SQL fragments are fixed table/column names and parameterized filters; limit/offset are ints.
	query := fmt.Sprintf(`%sid FROM %s %s %s LIMIT %d OFFSET %d`,
		selectSQL, plan.FromSQL, whereSQL, orderBy.SQL, limit, offset)

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search wisps: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("search wisps: scan id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search wisps: rows: %w", err)
	}
	return ids, nil
}

func getWispIssuesByIDsInOrderInTx(ctx context.Context, tx DBTX, ids []string) ([]*types.Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	wispSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wispSet[id] = struct{}{}
	}
	issues, err := GetIssuesByIDsInTx(ctx, tx, ids, wispSet)
	if err != nil {
		return nil, err
	}
	issueMap := make(map[string]*types.Issue, len(issues))
	for _, issue := range issues {
		issueMap[issue.ID] = issue
	}
	ordered := make([]*types.Issue, 0, len(ids))
	for _, id := range ids {
		if issue, ok := issueMap[id]; ok {
			ordered = append(ordered, issue)
		}
	}
	return ordered, nil
}

func readyWorkExcludeTypes(extra []types.IssueType) []types.IssueType {
	return sqlbuild.ReadyWorkExcludeTypes(extra)
}

func readyWorkWispIssueFilter(filter types.WorkFilter) types.IssueFilter {
	pinnedFalse := false
	wispFilter := types.IssueFilter{
		IssueFilterCore: types.IssueFilterCore{
			Priority:      filter.Priority,
			Labels:        filter.Labels,
			LabelsAny:     filter.LabelsAny,
			ExcludeLabels: filter.ExcludeLabels,
			Limit:         filter.Limit,
		},
		IssueFilterFlags: types.IssueFilterFlags{
			MolType:  filter.MolType,
			WispType: filter.WispType,
			Pinned:   &pinnedFalse,
		},
		IssueFilterHydrate: types.IssueFilterHydrate{
			MetadataFields: filter.MetadataFields,
			HasMetadataKey: filter.HasMetadataKey,
		},
		IssueFilterPage: types.IssueFilterPage{
			// be-x42v.4 follow-up (review SHOULD-FIX 8): without this,
			// getReadyWispsInTx's unbounded (Limit<=0) branch called
			// searchTableInTxT with MaxRows=0, so EffectiveSearchLimit emitted
			// no SQL LIMIT at all — the entire wisps table matching the
			// predicate was scanned and hydrated before GetReadyWorkInTx's
			// post-merge EnforceMaxRowsCap ever ran. Propagating the cap here
			// lets EffectiveSearchLimit bound that query to cap+1 up front.
			MaxRows:       filter.MaxRows,
			MaxRowsSource: filter.MaxRowsSource,
		},
	}
	switch {
	case filter.Status != "":
		s := filter.Status
		wispFilter.Status = &s
	case len(filter.Statuses) > 0:
		wispFilter.Statuses = append([]types.Status(nil), filter.Statuses...)
	default:
		wispFilter.Statuses = []types.Status{types.StatusOpen, types.StatusInProgress}
	}
	if filter.Type != "" {
		t := types.IssueType(filter.Type)
		wispFilter.IssueType = &t
	} else {
		wispFilter.ExcludeTypes = readyWorkExcludeTypes(filter.ExcludeTypes)
	}
	if filter.Unassigned {
		wispFilter.NoAssignee = true
	} else if filter.Assignee != nil {
		wispFilter.Assignee = filter.Assignee
	}
	if filter.MoleculeID != "" {
		moleculeID := filter.MoleculeID
		wispFilter.ParentID = &moleculeID
	}
	if !filter.IncludeEphemeral {
		ephFalse := false
		wispFilter.Ephemeral = &ephFalse
	}
	return wispFilter
}

func filterReadyWispsInTx(ctx context.Context, tx DBTX, filter types.WorkFilter, wisps []*types.Issue, deferredChildIDs []string) ([]*types.Issue, error) {
	if len(wisps) == 0 {
		return wisps, nil
	}

	wispIDs := make([]string, 0, len(wisps))
	for _, wisp := range wisps {
		wispIDs = append(wispIDs, wisp.ID)
	}

	excluded, err := readyWispExclusionsInTx(ctx, tx, filter, wisps, wispIDs, deferredChildIDs)
	if err != nil {
		return nil, err
	}

	ready := wisps[:0]
	for _, wisp := range wisps {
		if wisp.Pinned {
			continue
		}
		if _, skip := excluded[wisp.ID]; skip {
			continue
		}
		ready = append(ready, wisp)
	}
	return ready, nil
}

func readyWispExclusionsInTx(ctx context.Context, tx DBTX, filter types.WorkFilter, wisps []*types.Issue, wispIDs, deferredChildIDs []string) (map[string]struct{}, error) {
	excluded := make(map[string]struct{})
	if filter.ParentID != nil {
		if err := excludeWispsOutsideParentInTx(ctx, tx, *filter.ParentID, wisps, wispIDs, excluded); err != nil {
			return nil, err
		}
	}
	if !filter.IncludeDeferred {
		excludeDeferredWisps(wisps, deferredChildIDs, excluded)
	}
	blockedIDs, err := blockedWispIDsInTx(ctx, tx, wispIDs)
	if err != nil {
		return nil, err
	}
	for _, id := range blockedIDs {
		excluded[id] = struct{}{}
	}
	return excluded, nil
}

func excludeWispsOutsideParentInTx(ctx context.Context, tx DBTX, parentID string, wisps []*types.Issue, wispIDs []string, excluded map[string]struct{}) error {
	descendantIDs, err := GetDescendantIDsInTx(ctx, tx, parentID, 0)
	if err != nil {
		return fmt.Errorf("get wisp parent descendants: %w", err)
	}
	descendantSet := make(map[string]struct{}, len(descendantIDs))
	for _, id := range descendantIDs {
		descendantSet[id] = struct{}{}
	}
	parentedSet, err := getParentedIDSetInTx(ctx, tx, wispIDs)
	if err != nil {
		return err
	}
	for _, wisp := range wisps {
		if _, ok := descendantSet[wisp.ID]; ok {
			continue
		}
		if strings.HasPrefix(wisp.ID, parentID+".") {
			if _, hasParent := parentedSet[wisp.ID]; !hasParent {
				continue
			}
		}
		excluded[wisp.ID] = struct{}{}
	}
	return nil
}

func excludeDeferredWisps(wisps []*types.Issue, deferredChildIDs []string, excluded map[string]struct{}) {
	now := time.Now().UTC()
	for _, wisp := range wisps {
		if wisp.DeferUntil != nil && wisp.DeferUntil.After(now) {
			excluded[wisp.ID] = struct{}{}
		}
	}
	for _, id := range deferredChildIDs {
		excluded[id] = struct{}{}
	}
}

//nolint:gosec // G201: the query only interpolates IN-clause placeholders.
func blockedWispIDsInTx(ctx context.Context, tx DBTX, wispIDs []string) ([]string, error) {
	var blocked []string
	total := len(wispIDs)
	for start := 0; start < total; start += queryBatchSize {
		end := start + queryBatchSize
		if end > total {
			end = total
		}
		placeholders, args := buildSQLInClause(wispIDs[start:end])
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
			SELECT id FROM wisps WHERE id IN (%s) AND is_blocked = 1
		`, placeholders), args...)
		if err != nil {
			return nil, fmt.Errorf("get ready work: filter blocked wisps: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan blocked wisp: %w", err)
			}
			blocked = append(blocked, id)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("blocked wisp rows: %w", err)
		}
	}
	return blocked, nil
}
