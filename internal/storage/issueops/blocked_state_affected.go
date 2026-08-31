package issueops

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jonbaldie/beads/internal/types"
)

func AffectedByStatusChangeInTx(ctx context.Context, tx DBTX, id string) ([]string, []string, error) {
	issueSeed := []string{id}
	issueSeen := map[string]bool{id: true}
	var wispSeed []string
	wispSeen := make(map[string]bool)

	if err := loadBlockingDependersInTx(ctx, tx, "depends_on_issue_id", id, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}
	if err := loadWaitersWhoseSpawnerIsParentOfInTx(ctx, tx, id, false, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}
	// id's own status just changed, and an also_blocks waits-for edge blocks
	// while the spawner itself is open (pre-fanout window, GH#3783/GH#3875).
	// A waiter with only a DepWaitsFor edge on id (no DepBlocks edge — the
	// blocking semantics were collapsed into also_blocks) would otherwise
	// never get recomputed when its spawner closes.
	if err := loadWaitersOnSpawnerIDsInTx(ctx, tx, []string{id}, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}
	return expandByParentChildDescendantsInTx(ctx, tx, issueSeed, wispSeed, issueSeen, wispSeen)
}

func AffectedByStatusChangeForWispInTx(ctx context.Context, tx DBTX, id string) ([]string, []string, error) {
	var issueSeed []string
	issueSeen := make(map[string]bool)
	wispSeed := []string{id}
	wispSeen := map[string]bool{id: true}

	if err := loadBlockingDependersInTx(ctx, tx, "depends_on_wisp_id", id, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}
	if err := loadWaitersWhoseSpawnerIsParentOfInTx(ctx, tx, id, true, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}
	// See the issue-id sibling above: id's own status just changed, and a
	// waiter that waits directly on this wisp id as spawner needs to be
	// recomputed too.
	if err := loadWaitersOnSpawnerIDsInTx(ctx, tx, []string{id}, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}
	return expandByParentChildDescendantsInTx(ctx, tx, issueSeed, wispSeed, issueSeen, wispSeen)
}

func AffectedByDepChangeInTx(ctx context.Context, tx DBTX, source, target string, depType types.DependencyType) ([]string, []string, error) {
	switch depType {
	case types.DepBlocks, types.DepConditionalBlocks, types.DepWaitsFor, types.DepParentChild:
		issueSeed := []string{source}
		issueSeen := map[string]bool{source: true}
		var wispSeed []string
		wispSeen := map[string]bool{}
		if depType == types.DepParentChild && target != "" {
			if err := loadWaitersOnSpawnerIDsInTx(ctx, tx, []string{target}, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
				return nil, nil, err
			}
		}
		return expandByParentChildDescendantsInTx(ctx, tx, issueSeed, wispSeed, issueSeen, wispSeen)
	default:
		return nil, nil, nil
	}
}

func AffectedByDepChangeForWispInTx(ctx context.Context, tx DBTX, source, target string, depType types.DependencyType) ([]string, []string, error) {
	switch depType {
	case types.DepBlocks, types.DepConditionalBlocks, types.DepWaitsFor, types.DepParentChild:
		var issueSeed []string
		issueSeen := map[string]bool{}
		wispSeed := []string{source}
		wispSeen := map[string]bool{source: true}
		if depType == types.DepParentChild && target != "" {
			if err := loadWaitersOnSpawnerIDsInTx(ctx, tx, []string{target}, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
				return nil, nil, err
			}
		}
		return expandByParentChildDescendantsInTx(ctx, tx, issueSeed, wispSeed, issueSeen, wispSeen)
	default:
		return nil, nil, nil
	}
}

func loadBlockingDependersInTx(
	ctx context.Context, tx DBTX,
	targetCol, id string,
	issueSeed *[]string, issueSeen map[string]bool,
	wispSeed *[]string, wispSeen map[string]bool,
) error {
	return loadBlockingDependersForIDsInTx(ctx, tx, targetCol, []string{id}, issueSeed, issueSeen, wispSeed, wispSeen)
}

//nolint:gosec // G201: targetCol is one of two constant column names.
func loadBlockingDependersForIDsInTx(
	ctx context.Context, tx DBTX,
	targetCol string, ids []string,
	issueSeed *[]string, issueSeen map[string]bool,
	wispSeed *[]string, wispSeen map[string]bool,
) error {
	if len(ids) == 0 {
		return nil
	}
	tables := []struct {
		table  string
		seed   *[]string
		seen   map[string]bool
		errCtx string
	}{
		{"dependencies", issueSeed, issueSeen, "load issue dependers"},
		{"wisp_dependencies", wispSeed, wispSeen, "load wisp dependers"},
	}
	for _, id := range ids {
		for _, t := range tables {
			query := fmt.Sprintf(`
				SELECT issue_id FROM %s
				WHERE %s = ?
				  AND (type = 'blocks' OR type = 'conditional-blocks')
			`, t.table, targetCol)
			rows, err := tx.QueryContext(ctx, query, id)
			if err != nil {
				return fmt.Errorf("%s: query: %w", t.errCtx, err)
			}
			for rows.Next() {
				var dependerID string
				if err := rows.Scan(&dependerID); err != nil {
					_ = rows.Close()
					return fmt.Errorf("%s: scan: %w", t.errCtx, err)
				}
				if !t.seen[dependerID] {
					t.seen[dependerID] = true
					*t.seed = append(*t.seed, dependerID)
				}
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				return fmt.Errorf("%s: rows: %w", t.errCtx, err)
			}
		}
	}
	return nil
}

func AffectedByDeletionInTx(
	ctx context.Context, tx DBTX,
	deletedIssues, deletedWisps []string,
) ([]string, []string, error) {
	if len(deletedIssues) == 0 && len(deletedWisps) == 0 {
		return nil, nil, nil
	}

	issueSeen := make(map[string]bool, len(deletedIssues))
	wispSeen := make(map[string]bool, len(deletedWisps))
	for _, id := range deletedIssues {
		issueSeen[id] = true
	}
	for _, id := range deletedWisps {
		wispSeen[id] = true
	}
	var issueSeed, wispSeed []string

	if err := loadDeletionBlockingSeeds(ctx, tx, deletedIssues, deletedWisps, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}
	if err := loadDeletionWaiterSeeds(ctx, tx, deletedIssues, deletedWisps, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}
	if err := loadDeletionChildSeeds(ctx, tx, deletedIssues, deletedWisps, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}

	return expandByParentChildDescendantsInTx(ctx, tx, issueSeed, wispSeed, issueSeen, wispSeen)
}

func loadDeletionBlockingSeeds(ctx context.Context, tx DBTX, deletedIssues, deletedWisps []string, issueSeed *[]string, issueSeen map[string]bool, wispSeed *[]string, wispSeen map[string]bool) error {
	if err := loadBlockingDependersForIDsInTx(ctx, tx, "depends_on_issue_id", deletedIssues, issueSeed, issueSeen, wispSeed, wispSeen); err != nil {
		return err
	}
	return loadBlockingDependersForIDsInTx(ctx, tx, "depends_on_wisp_id", deletedWisps, issueSeed, issueSeen, wispSeed, wispSeen)
}

func loadDeletionWaiterSeeds(ctx context.Context, tx DBTX, deletedIssues, deletedWisps []string, issueSeed *[]string, issueSeen map[string]bool, wispSeed *[]string, wispSeen map[string]bool) error {
	if err := loadWaitersOnSpawnerIDsByColInTx(ctx, tx, "depends_on_issue_id", deletedIssues, issueSeed, issueSeen, wispSeed, wispSeen); err != nil {
		return err
	}
	if err := loadWaitersOnSpawnerIDsByColInTx(ctx, tx, "depends_on_wisp_id", deletedWisps, issueSeed, issueSeen, wispSeed, wispSeen); err != nil {
		return err
	}
	for _, id := range deletedIssues {
		if err := loadWaitersWhoseSpawnerIsParentOfInTx(ctx, tx, id, false, issueSeed, issueSeen, wispSeed, wispSeen); err != nil {
			return err
		}
	}
	for _, id := range deletedWisps {
		if err := loadWaitersWhoseSpawnerIsParentOfInTx(ctx, tx, id, true, issueSeed, issueSeen, wispSeed, wispSeen); err != nil {
			return err
		}
	}
	return nil
}

func loadDeletionChildSeeds(ctx context.Context, tx DBTX, deletedIssues, deletedWisps []string, issueSeed *[]string, issueSeen map[string]bool, wispSeed *[]string, wispSeen map[string]bool) error {
	for _, childSeed := range []struct {
		depTable, parentCol string
		parentIDs           []string
		seed                *[]string
		seen                map[string]bool
	}{
		{"dependencies", "depends_on_issue_id", deletedIssues, issueSeed, issueSeen},
		{"wisp_dependencies", "depends_on_issue_id", deletedIssues, wispSeed, wispSeen},
		{"dependencies", "depends_on_wisp_id", deletedWisps, issueSeed, issueSeen},
		{"wisp_dependencies", "depends_on_wisp_id", deletedWisps, wispSeed, wispSeen},
	} {
		if err := appendChildrenInTx(ctx, tx, childSeed.depTable, childSeed.parentCol, childSeed.parentIDs, childSeed.seen, childSeed.seed); err != nil {
			return err
		}
	}
	return nil
}

func expandByParentChildDescendantsInTx(
	ctx context.Context, tx DBTX,
	issueSeed, wispSeed []string,
	issueSeen, wispSeen map[string]bool,
) ([]string, []string, error) {
	issueQueue := issueSeed
	wispQueue := wispSeed
	issueHead, wispHead := 0, 0

	for parentChildQueuesHavePending(issueQueue, issueHead, wispQueue, wispHead) {
		if parentChildQueueHasPending(issueQueue, issueHead) {
			var err error
			issueHead, err = expandIssueParentChildBatch(ctx, tx, &issueQueue, issueHead, issueSeen, &wispQueue, wispSeen)
			if err != nil {
				return nil, nil, err
			}
		}
		if parentChildQueueHasPending(wispQueue, wispHead) {
			var err error
			wispHead, err = expandWispParentChildBatch(ctx, tx, &wispQueue, wispHead, &issueQueue, issueSeen, wispSeen)
			if err != nil {
				return nil, nil, err
			}
		}
	}
	return issueQueue, wispQueue, nil
}

func parentChildQueuesHavePending(issueQueue []string, issueHead int, wispQueue []string, wispHead int) bool {
	return issueHead < len(issueQueue) || wispHead < len(wispQueue)
}

func parentChildQueueHasPending(queue []string, head int) bool {
	return head < len(queue)
}

func expandIssueParentChildBatch(ctx context.Context, tx DBTX, queue *[]string, head int, issueSeen map[string]bool, wispQueue *[]string, wispSeen map[string]bool) (int, error) {
	end := head + queryBatchSize
	if end > len(*queue) {
		end = len(*queue)
	}
	batch := (*queue)[head:end]
	if err := appendChildrenInTx(ctx, tx, "dependencies", "depends_on_issue_id", batch, issueSeen, queue); err != nil {
		return head, err
	}
	if err := appendChildrenInTx(ctx, tx, "wisp_dependencies", "depends_on_issue_id", batch, wispSeen, wispQueue); err != nil {
		return head, err
	}
	return end, nil
}

func expandWispParentChildBatch(ctx context.Context, tx DBTX, queue *[]string, head int, issueQueue *[]string, issueSeen, wispSeen map[string]bool) (int, error) {
	end := head + queryBatchSize
	if end > len(*queue) {
		end = len(*queue)
	}
	batch := (*queue)[head:end]
	if err := appendChildrenInTx(ctx, tx, "dependencies", "depends_on_wisp_id", batch, issueSeen, issueQueue); err != nil {
		return head, err
	}
	if err := appendChildrenInTx(ctx, tx, "wisp_dependencies", "depends_on_wisp_id", batch, wispSeen, queue); err != nil {
		return head, err
	}
	return end, nil
}

//nolint:gosec // G201: depTable and parentCol come from constant call sites.
func appendChildrenInTx(
	ctx context.Context, tx DBTX,
	depTable, parentCol string,
	parentIDs []string,
	seen map[string]bool, queue *[]string,
) error {
	if len(parentIDs) == 0 {
		return nil
	}
	query := fmt.Sprintf(`
		SELECT issue_id FROM %s
		WHERE type = 'parent-child'
		  AND %s = ?
	`, depTable, parentCol)
	for _, parentID := range parentIDs {
		rows, err := tx.QueryContext(ctx, query, parentID)
		if err != nil {
			return fmt.Errorf("expand children from %s on %s: %w", depTable, parentCol, err)
		}
		for rows.Next() {
			var childID string
			if err := rows.Scan(&childID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("expand children: scan: %w", err)
			}
			if !seen[childID] {
				seen[childID] = true
				*queue = append(*queue, childID)
			}
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("expand children: rows: %w", err)
		}
	}
	return nil
}

func loadWaitersWhoseSpawnerIsParentOfInTx(
	ctx context.Context, tx DBTX,
	childID string, childIsWisp bool,
	issueSeed *[]string, issueSeen map[string]bool,
	wispSeed *[]string, wispSeen map[string]bool,
) error {
	depTable := "dependencies"
	if childIsWisp {
		depTable = "wisp_dependencies"
	}
	issueParentIDs, wispParentIDs, err := loadParentIDsForChildInTx(ctx, tx, depTable, childID)
	if err != nil {
		return err
	}
	if err := loadWaitersOnParentIDsInTx(ctx, tx, issueParentIDs, wispParentIDs, issueSeed, issueSeen, wispSeed, wispSeen); err != nil {
		return err
	}
	return nil
}

//nolint:gosec // G201: depTable is one of two constant values.
func loadParentIDsForChildInTx(ctx context.Context, tx DBTX, depTable, childID string) ([]string, []string, error) {
	//nolint:gosec // G201: depTable is one of two constant values.
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT depends_on_issue_id, depends_on_wisp_id
		FROM %s
		WHERE issue_id = ? AND type = 'parent-child'
	`, depTable), childID)
	if err != nil {
		return nil, nil, fmt.Errorf("waiters on parent of %s: load parents: %w", childID, err)
	}
	var issueParentIDs, wispParentIDs []string
	for rows.Next() {
		var ip, wp sql.NullString
		if err := rows.Scan(&ip, &wp); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("waiters on parent of %s: scan: %w", childID, err)
		}
		if ip.Valid {
			issueParentIDs = append(issueParentIDs, ip.String)
		}
		if wp.Valid {
			wispParentIDs = append(wispParentIDs, wp.String)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("waiters on parent of %s: rows: %w", childID, err)
	}
	return issueParentIDs, wispParentIDs, nil
}

func loadWaitersOnParentIDsInTx(ctx context.Context, tx DBTX, issueParentIDs, wispParentIDs []string, issueSeed *[]string, issueSeen map[string]bool, wispSeed *[]string, wispSeen map[string]bool) error {
	if len(issueParentIDs) > 0 {
		if err := loadWaitersOnSpawnerIDsByColInTx(ctx, tx, "depends_on_issue_id", issueParentIDs, issueSeed, issueSeen, wispSeed, wispSeen); err != nil {
			return err
		}
	}
	if len(wispParentIDs) > 0 {
		if err := loadWaitersOnSpawnerIDsByColInTx(ctx, tx, "depends_on_wisp_id", wispParentIDs, issueSeed, issueSeen, wispSeed, wispSeen); err != nil {
			return err
		}
	}
	return nil
}

func loadWaitersOnSpawnerIDsInTx(
	ctx context.Context, tx DBTX,
	spawnerIDs []string,
	issueSeed *[]string, issueSeen map[string]bool,
	wispSeed *[]string, wispSeen map[string]bool,
) error {
	if err := loadWaitersOnSpawnerIDsByColInTx(ctx, tx, "depends_on_issue_id", spawnerIDs, issueSeed, issueSeen, wispSeed, wispSeen); err != nil {
		return err
	}
	return loadWaitersOnSpawnerIDsByColInTx(ctx, tx, "depends_on_wisp_id", spawnerIDs, issueSeed, issueSeen, wispSeed, wispSeen)
}

//nolint:gosec // G201: targetCol is one of two constant column names.
func loadWaitersOnSpawnerIDsByColInTx(
	ctx context.Context, tx DBTX,
	targetCol string, spawnerIDs []string,
	issueSeed *[]string, issueSeen map[string]bool,
	wispSeed *[]string, wispSeen map[string]bool,
) error {
	if len(spawnerIDs) == 0 {
		return nil
	}
	tables := []struct {
		table  string
		seed   *[]string
		seen   map[string]bool
		errCtx string
	}{
		{"dependencies", issueSeed, issueSeen, "load issue waiters"},
		{"wisp_dependencies", wispSeed, wispSeen, "load wisp waiters"},
	}
	for _, spawnerID := range spawnerIDs {
		for _, t := range tables {
			if err := loadWaitersForSpawnerTableInTx(ctx, tx, targetCol, spawnerID, t.table, t.seed, t.seen, t.errCtx); err != nil {
				return err
			}
		}
	}
	return nil
}

//nolint:gosec // G201: targetCol/table are selected from fixed call-site values.
func loadWaitersForSpawnerTableInTx(ctx context.Context, tx DBTX, targetCol, spawnerID, table string, seed *[]string, seen map[string]bool, errCtx string) error {
	query := fmt.Sprintf(`
		SELECT issue_id FROM %s
		WHERE type = 'waits-for' AND %s = ?
	`, table, targetCol)
	rows, err := tx.QueryContext(ctx, query, spawnerID)
	if err != nil {
		if optionalBlockedTable(table) && isTableNotExistError(err) {
			return nil
		}
		return fmt.Errorf("%s: query: %w", errCtx, err)
	}
	for rows.Next() {
		var waiterID string
		if err := rows.Scan(&waiterID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("%s: scan: %w", errCtx, err)
		}
		if !seen[waiterID] {
			seen[waiterID] = true
			*seed = append(*seed, waiterID)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%s: rows: %w", errCtx, err)
	}
	return nil
}
