package issueops

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/jonbaldie/beads/internal/types"
)

type blockingDepRecord struct {
	issueID, dependsOnID, depType string
	metadata                      sql.NullString
}

func optionalBlockedTable(table string) bool {
	return table == "wisps" || table == "wisp_dependencies"
}

func loadBlockingDepsForIssueIDsInTx(ctx context.Context, tx DBTX, depTables []string, issueIDs []string) ([]blockingDepRecord, error) {
	var deps []blockingDepRecord
	for _, depTable := range depTables {
		//nolint:gosec // G201: depTable is a hardcoded constant.
		query := fmt.Sprintf(`
			SELECT issue_id, %s AS depends_on_id, type, metadata FROM %s
			WHERE issue_id = ?
			  AND (type = 'blocks' OR type = 'waits-for' OR type = 'conditional-blocks')
		`, DepTargetExpr, depTable)
		for _, id := range issueIDs {
			rows, err := tx.QueryContext(ctx, query, id)
			if err != nil {
				if optionalBlockedTable(depTable) && isTableNotExistError(err) {
					break
				}
				return nil, fmt.Errorf("compute blocked IDs: deps from %s: %w", depTable, err)
			}
			for rows.Next() {
				var rec blockingDepRecord
				if err := rows.Scan(&rec.issueID, &rec.dependsOnID, &rec.depType, &rec.metadata); err != nil {
					_ = rows.Close()
					return nil, fmt.Errorf("compute blocked IDs: scan dep: %w", err)
				}
				deps = append(deps, rec)
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				return nil, fmt.Errorf("compute blocked IDs: dep rows from %s: %w", depTable, err)
			}
		}
	}
	return deps, nil
}

func loadParentIDsForChildrenInTx(ctx context.Context, tx DBTX, depTables []string, childIDs []string) (map[string]string, error) {
	childParents := make(map[string]string)
	for _, depTable := range depTables {
		//nolint:gosec // G201: depTable is a hardcoded constant.
		query := fmt.Sprintf(`
			SELECT issue_id, %s AS depends_on_id FROM %s
			WHERE issue_id = ?
			  AND type = 'parent-child'
		`, DepTargetExpr, depTable)
		for _, id := range childIDs {
			rows, err := tx.QueryContext(ctx, query, id)
			if err != nil {
				if optionalBlockedTable(depTable) && isTableNotExistError(err) {
					break
				}
				return nil, fmt.Errorf("candidate parents from %s: %w", depTable, err)
			}
			for rows.Next() {
				var childID, parentID string
				if err := rows.Scan(&childID, &parentID); err != nil {
					_ = rows.Close()
					return nil, fmt.Errorf("scan candidate parent: %w", err)
				}
				childParents[childID] = parentID
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				return nil, fmt.Errorf("candidate parent rows from %s: %w", depTable, err)
			}
		}
	}
	return childParents, nil
}

//nolint:gosec // G201: tables are hardcoded
func GetChildrenWithParentsInTx(ctx context.Context, tx DBTX, parentIDs []string) (map[string]string, error) {
	if len(parentIDs) == 0 {
		return nil, nil
	}
	result := make(map[string]string)
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		children, err := readChildrenWithParentsFromTable(ctx, tx, depTable, parentIDs)
		if err != nil {
			if optionalBlockedTable(depTable) && isTableNotExistError(err) {
				continue
			}
			return nil, err
		}
		for childID, parentID := range children {
			result[childID] = parentID
		}
	}
	return result, nil
}

func readChildrenWithParentsFromTable(ctx context.Context, tx DBTX, depTable string, parentIDs []string) (map[string]string, error) {
	//nolint:gosec // G201: tables are hardcoded
	query := fmt.Sprintf(`
		SELECT issue_id, %s AS depends_on_id FROM %s
		WHERE type = 'parent-child' AND %s = ?
	`, DepTargetExpr, depTable, DepTargetExpr)
	result := make(map[string]string)
	for _, parentID := range parentIDs {
		rows, err := tx.QueryContext(ctx, query, parentID)
		if err != nil {
			return nil, fmt.Errorf("get children with parents from %s: %w", depTable, err)
		}
		for rows.Next() {
			var childID, resolvedParentID string
			if err := rows.Scan(&childID, &resolvedParentID); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan children with parents: %w", err)
			}
			result[childID] = resolvedParentID
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("children with parents rows from %s: %w", depTable, err)
		}
	}
	return result, nil
}

//nolint:gosec // G201: tables are hardcoded
func GetChildrenOfIssuesInTx(ctx context.Context, tx DBTX, parentIDs []string) ([]string, error) {
	if len(parentIDs) == 0 {
		return nil, nil
	}
	var children []string
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		tableChildren, err := readChildrenOfIssuesFromTable(ctx, tx, depTable, parentIDs)
		if err != nil {
			if optionalBlockedTable(depTable) && isTableNotExistError(err) {
				continue
			}
			return nil, err
		}
		children = append(children, tableChildren...)
	}
	return children, nil
}

func readChildrenOfIssuesFromTable(ctx context.Context, tx DBTX, depTable string, parentIDs []string) ([]string, error) {
	//nolint:gosec // G201: tables are hardcoded
	query := fmt.Sprintf(`
		SELECT issue_id FROM %s
		WHERE type = 'parent-child' AND %s = ?
	`, depTable, DepTargetExpr)
	var children []string
	for _, parentID := range parentIDs {
		rows, err := tx.QueryContext(ctx, query, parentID)
		if err != nil {
			return nil, fmt.Errorf("get children of issues from %s: %w", depTable, err)
		}
		for rows.Next() {
			var childID string
			if err := rows.Scan(&childID); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan child: %w", err)
			}
			children = append(children, childID)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("children rows from %s: %w", depTable, err)
		}
	}
	return children, nil
}

func GetDescendantIDsInTx(ctx context.Context, tx DBTX, rootID string, maxDepth int) ([]string, error) {
	if rootID == "" {
		return nil, nil
	}

	result, reachedMaxDepth, err := queryDescendantsInTx(ctx, tx, rootID, maxDepth, true)
	if err != nil {
		if !isTableNotExistError(err) {
			return nil, err
		}
		result, reachedMaxDepth, err = queryDescendantsInTx(ctx, tx, rootID, maxDepth, false)
		if err != nil {
			return nil, err
		}
	}
	if reachedMaxDepth {
		return nil, fmt.Errorf("parent descendant traversal for %s reached max depth %d", rootID, maxDepth)
	}
	return result, nil
}

func queryDescendantsInTx(ctx context.Context, tx DBTX, rootID string, maxDepth int, includeWisps bool) ([]string, bool, error) {
	edgeQuery := fmt.Sprintf(`
		SELECT issue_id, %s FROM dependencies WHERE type = 'parent-child'
	`, DepTargetExpr)
	if includeWisps {
		edgeQuery += fmt.Sprintf(`
		UNION ALL
		SELECT issue_id, %s FROM wisp_dependencies WHERE type = 'parent-child'
	`, DepTargetExpr)
	}

	//nolint:gosec // G201: edgeQuery is built from hardcoded SQL plus DepTargetExpr (no user input)
	query := fmt.Sprintf(`
		WITH RECURSIVE
		parent_edges(issue_id, depends_on_id) AS (
			%s
		),
		descendants(id, depth, path) AS (
			SELECT issue_id, 1, CONCAT(',', ?, ',', issue_id, ',')
			FROM parent_edges
			WHERE depends_on_id = ?
			UNION ALL
			SELECT e.issue_id, d.depth + 1, CONCAT(d.path, e.issue_id, ',')
			FROM parent_edges e
			JOIN descendants d ON e.depends_on_id = d.id
			WHERE (? <= 0 OR d.depth < ?)
			  AND LOCATE(CONCAT(',', e.issue_id, ','), d.path) = 0
		)
		SELECT id, depth FROM descendants WHERE id <> ?
	`, edgeQuery)

	rows, err := tx.QueryContext(ctx, query, rootID, rootID, maxDepth, maxDepth, rootID)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()

	var result []string
	reachedMaxDepth := false
	for rows.Next() {
		var id string
		var depth int
		if err := rows.Scan(&id, &depth); err != nil {
			return nil, false, fmt.Errorf("scan descendant: %w", err)
		}
		result = append(result, id)
		if maxDepth > 0 && depth >= maxDepth {
			reachedMaxDepth = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("descendant rows: %w", err)
	}
	return result, reachedMaxDepth, nil
}

//nolint:gosec // G201: tables are hardcoded
func GetBlockedIssuesInTx(ctx context.Context, tx DBTX, filter types.WorkFilter) ([]*types.BlockedIssue, error) {
	blockedIDList, storedBlocked, err := loadBlockedIssueIDsInTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if len(blockedIDList) == 0 {
		return nil, nil
	}

	blockerMap, err := loadActiveBlockersInTx(ctx, tx, blockedIDList)
	if err != nil {
		return nil, err
	}

	addInheritedBlockersInTx(ctx, tx, blockedIDList, blockerMap)
	addStoredBlockers(storedBlocked, blockerMap)

	issueMap, err := loadBlockedIssueMapInTx(ctx, tx, blockerMap)
	if err != nil {
		return nil, err
	}

	parentChildSet := blockedParentChildSetInTx(ctx, tx, filter.ParentID, blockerMap)
	return buildBlockedIssueResults(blockerMap, issueMap, parentChildSet), nil
}

func loadBlockedIssueIDsInTx(ctx context.Context, tx DBTX) ([]string, map[string]bool, error) {
	blockedSet := make(map[string]bool)
	storedBlocked := make(map[string]bool)
	var blockedIDList []string
	for _, table := range []string{"issues", "wisps"} {
		ids, stored, err := readBlockedIDsFromTable(ctx, tx, table)
		if err != nil {
			if optionalBlockedTable(table) && isTableNotExistError(err) {
				continue
			}
			return nil, nil, err
		}
		for _, id := range ids {
			if !blockedSet[id] {
				blockedSet[id] = true
				blockedIDList = append(blockedIDList, id)
			}
		}
		for id := range stored {
			storedBlocked[id] = true
		}
	}
	return blockedIDList, storedBlocked, nil
}

func readBlockedIDsFromTable(ctx context.Context, tx DBTX, table string) ([]string, map[string]bool, error) {
	//nolint:gosec // G201: table is one of two hardcoded values.
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, status FROM %s
		WHERE (is_blocked = 1 OR status = 'blocked') AND status <> 'closed' AND status <> 'pinned'
	`, table))
	if err != nil {
		return nil, nil, fmt.Errorf("read blocked ids from %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	stored := make(map[string]bool)
	for rows.Next() {
		var id, issueStatus string
		if err := rows.Scan(&id, &issueStatus); err != nil {
			return nil, nil, fmt.Errorf("scan blocked id from %s: %w", table, err)
		}
		ids = append(ids, id)
		if issueStatus == string(types.StatusBlocked) {
			stored[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("blocked id rows from %s: %w", table, err)
	}
	return ids, stored, nil
}

func loadActiveBlockersInTx(ctx context.Context, tx DBTX, blockedIDList []string) (map[string][]string, error) {
	blockerMap := make(map[string][]string)
	blockingDeps, err := loadBlockingDepsForIssueIDsInTx(ctx, tx, []string{"dependencies", "wisp_dependencies"}, blockedIDList)
	if err != nil {
		return nil, fmt.Errorf("get blocking deps: %w", err)
	}
	if len(blockingDeps) == 0 {
		return blockerMap, nil
	}
	targetIDs := uniqueBlockingTargetIDs(blockingDeps)
	activeTargets, err := loadStatusByIDInTx(ctx, tx, targetIDs)
	if err != nil {
		return nil, fmt.Errorf("blocker target status: %w", err)
	}
	for _, rec := range blockingDeps {
		status, ok := activeTargets[rec.dependsOnID]
		if !ok || status == types.StatusClosed || status == types.StatusPinned {
			continue
		}
		blockerMap[rec.issueID] = append(blockerMap[rec.issueID], rec.dependsOnID)
	}
	return blockerMap, nil
}

func uniqueBlockingTargetIDs(blockingDeps []blockingDepRecord) []string {
	targetIDs := make([]string, 0, len(blockingDeps))
	seenTargets := make(map[string]bool, len(blockingDeps))
	for _, rec := range blockingDeps {
		if !seenTargets[rec.dependsOnID] {
			seenTargets[rec.dependsOnID] = true
			targetIDs = append(targetIDs, rec.dependsOnID)
		}
	}
	return targetIDs
}

func addInheritedBlockersInTx(ctx context.Context, tx DBTX, blockedIDList []string, blockerMap map[string][]string) {
	var inheritedIDs []string
	for _, id := range blockedIDList {
		if _, ok := blockerMap[id]; !ok {
			inheritedIDs = append(inheritedIDs, id)
		}
	}
	if len(inheritedIDs) == 0 {
		return
	}
	parentMap, err := loadParentIDsForChildrenInTx(ctx, tx, []string{"dependencies", "wisp_dependencies"}, inheritedIDs)
	if err != nil {
		return
	}
	for childID, parentID := range parentMap {
		if _, alreadyHas := blockerMap[childID]; !alreadyHas {
			blockerMap[childID] = []string{parentID}
		}
	}
}

func addStoredBlockers(storedBlocked map[string]bool, blockerMap map[string][]string) {
	// Stored status "blocked" is a manual hold. Include those issues even when
	// they have no remaining computed blockers so they cannot vanish from both
	// bd ready and bd blocked.
	for id := range storedBlocked {
		if _, ok := blockerMap[id]; !ok {
			blockerMap[id] = []string{}
		}
	}
}

func loadBlockedIssueMapInTx(ctx context.Context, tx DBTX, blockerMap map[string][]string) (map[string]*types.Issue, error) {
	displayIDs := make([]string, 0, len(blockerMap))
	for id := range blockerMap {
		displayIDs = append(displayIDs, id)
	}
	issues, err := GetIssuesByIDsInTx(ctx, tx, displayIDs, nil)
	if err != nil {
		return nil, fmt.Errorf("batch-fetch blocked issues: %w", err)
	}
	issueMap := make(map[string]*types.Issue, len(issues))
	for _, issue := range issues {
		issueMap[issue.ID] = issue
	}
	return issueMap, nil
}

func blockedParentChildSetInTx(ctx context.Context, tx DBTX, parentID *string, blockerMap map[string][]string) map[string]bool {
	if parentID == nil {
		return nil
	}
	parentChildSet := make(map[string]bool)
	children, err := GetChildrenOfIssuesInTx(ctx, tx, []string{*parentID})
	if err == nil {
		for _, childID := range children {
			parentChildSet[childID] = true
		}
	}
	for id := range blockerMap {
		if strings.HasPrefix(id, *parentID+".") {
			parentChildSet[id] = true
		}
	}
	return parentChildSet
}

func buildBlockedIssueResults(blockerMap map[string][]string, issueMap map[string]*types.Issue, parentChildSet map[string]bool) []*types.BlockedIssue {
	var results []*types.BlockedIssue
	for id, blockerIDs := range blockerMap {
		if parentChildSet != nil && !parentChildSet[id] {
			continue
		}
		issue, ok := issueMap[id]
		if !ok || issue == nil {
			continue
		}
		results = append(results, &types.BlockedIssue{
			Issue:          *issue,
			BlockedByCount: len(blockerIDs),
			BlockedBy:      blockerIDs,
		})
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Issue.Priority != results[j].Issue.Priority {
			return results[i].Issue.Priority < results[j].Issue.Priority
		}
		return results[i].Issue.CreatedAt.After(results[j].Issue.CreatedAt)
	})

	return results
}
