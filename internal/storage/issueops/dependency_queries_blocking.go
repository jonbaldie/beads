package issueops

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jonbaldie/beads/internal/types"
)

// GetBlockingInfoForIssuesInTx returns blocking dependency records for a set of issue IDs.
// Returns three maps:
//   - blockedByMap: issueID -> list of IDs blocking it
//   - blocksMap: issueID -> list of IDs it blocks
//   - parentMap: childID -> parentID (parent-child deps)
func GetBlockingInfoForIssuesInTx(ctx context.Context, tx DBTX, issueIDs []string) (
	blockedByMap map[string][]string,
	blocksMap map[string][]string,
	parentMap map[string]string,
	err error,
) {
	blockedByMap = make(map[string][]string)
	blocksMap = make(map[string][]string)
	parentMap = make(map[string]string)

	if len(issueIDs) == 0 {
		return
	}

	// Partition into wisp and perm IDs for routing. Use the batched
	// partitioner so we don't take a round-trip per ID on remote backends
	// (GH#3414).
	wispIDs, permIDs, partErr := PartitionWispIDsInTx(ctx, tx, issueIDs)
	if partErr != nil {
		return nil, nil, nil, partErr
	}

	// Process wisp IDs against wisp_dependencies.
	if len(wispIDs) > 0 {
		if err := queryBlockedByInfo(ctx, tx, wispIDs, "wisp_dependencies", blockedByMap, parentMap); err != nil {
			return nil, nil, nil, err
		}
	}

	// Process perm IDs against dependencies.
	if len(permIDs) > 0 {
		if err := queryBlockedByInfo(ctx, tx, permIDs, "dependencies", blockedByMap, parentMap); err != nil {
			return nil, nil, nil, err
		}
	}

	// "Blocks" is target-oriented, so scan both dependency tables regardless of
	// the target issue's storage class.
	if err := queryBlocksInfo(ctx, tx, issueIDs, []string{"dependencies", "wisp_dependencies"}, blocksMap); err != nil {
		return nil, nil, nil, err
	}

	return blockedByMap, blocksMap, parentMap, nil
}

type blockingInfoRow struct {
	issueID, blockerID, depType string
}

// queryBlockedByInfo queries outbound blocking info from a specific dependency
// table. Blocker status is resolved against both issue storage classes so
// cross-class closed blockers do not appear active.
// Uses batched IN clauses (queryBatchSize) to avoid query-planner spikes.
func queryBlockedByInfo(
	ctx context.Context, tx DBTX,
	issueIDs []string,
	depTable string,
	blockedByMap map[string][]string,
	parentMap map[string]string,
) error {
	total := len(issueIDs)
	for start := 0; start < total; start += queryBatchSize {
		end := start + queryBatchSize
		if end > len(issueIDs) {
			end = len(issueIDs)
		}
		if err := queryBlockedByBatch(ctx, tx, issueIDs[start:end], depTable, blockedByMap, parentMap); err != nil {
			return err
		}
	}

	return nil
}

func queryBlockedByBatch(ctx context.Context, tx DBTX, batch []string, depTable string, blockedByMap map[string][]string, parentMap map[string]string) error {
	inClause, args := buildSQLInClause(batch)
	// Query: "blocked by" — deps where issue_id is in our set.
	//nolint:gosec // G201: depTable is a caller-controlled constant.
	blockedByQuery := fmt.Sprintf(`
		SELECT d.issue_id, %s AS depends_on_id, d.type
		FROM %s d
		WHERE d.issue_id IN (%s) AND d.type IN ('blocks', 'parent-child')
	`, depTargetExpr("d"), depTable, inClause)

	rows, err := tx.QueryContext(ctx, blockedByQuery, args...)
	if err != nil {
		if optionalBlockedTable(depTable) && isTableNotExistError(err) {
			return nil
		}
		return fmt.Errorf("get blocked-by info from %s: %w", depTable, err)
	}
	depRows, blockerIDs, err := readBlockingInfoRows(rows)
	if err != nil {
		return err
	}
	statusByID, err := loadStatusByIDInTx(ctx, tx, blockerIDs)
	if err != nil {
		return fmt.Errorf("get blocking info: blocker status: %w", err)
	}
	applyBlockedByRows(depRows, statusByID, blockedByMap, parentMap)
	return nil
}

func readBlockingInfoRows(rows *sql.Rows) ([]blockingInfoRow, []string, error) {
	var depRows []blockingInfoRow
	var blockerIDs []string
	for rows.Next() {
		var row blockingInfoRow
		if scanErr := rows.Scan(&row.issueID, &row.blockerID, &row.depType); scanErr != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("get blocking info: scan blocked-by: %w", scanErr)
		}
		depRows = append(depRows, row)
		blockerIDs = append(blockerIDs, row.blockerID)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("get blocking info: blocked-by rows: %w", err)
	}
	return depRows, blockerIDs, nil
}

func applyBlockedByRows(rows []blockingInfoRow, statusByID map[string]types.Status, blockedByMap map[string][]string, parentMap map[string]string) {
	for _, row := range rows {
		if statusByID[row.blockerID] == types.StatusClosed {
			continue
		}
		if row.depType == "parent-child" {
			parentMap[row.issueID] = row.blockerID
			continue
		}
		blockedByMap[row.issueID] = append(blockedByMap[row.issueID], row.blockerID)
	}
}

// queryBlocksInfo queries inbound blocking info across dependency tables.
func queryBlocksInfo(
	ctx context.Context, tx DBTX,
	issueIDs []string,
	depTables []string,
	blocksMap map[string][]string,
) error {
	total := len(issueIDs)
	for start := 0; start < total; start += queryBatchSize {
		end := start + queryBatchSize
		if end > len(issueIDs) {
			end = len(issueIDs)
		}
		if err := queryBlocksBatch(ctx, tx, issueIDs[start:end], depTables, blocksMap); err != nil {
			return err
		}
	}

	return nil
}

func queryBlocksBatch(ctx context.Context, tx DBTX, batch []string, depTables []string, blocksMap map[string][]string) error {
	inClause, args := buildSQLInClause(batch)
	statusByID, err := loadStatusByIDInTx(ctx, tx, batch)
	if err != nil {
		return fmt.Errorf("get blocking info: blocker status: %w", err)
	}
	for _, depTable := range depTables {
		if err := queryBlocksFromTable(ctx, tx, depTable, inClause, args, statusByID, blocksMap); err != nil {
			return err
		}
	}
	return nil
}

func queryBlocksFromTable(ctx context.Context, tx DBTX, depTable, inClause string, args []any, statusByID map[string]types.Status, blocksMap map[string][]string) error {
	// Query: "blocks" — deps where depends_on_id is in our set.
	//nolint:gosec // G201: depTable is a caller-controlled constant.
	blocksQuery := fmt.Sprintf(`
		SELECT %s AS depends_on_id, d.issue_id, d.type
		FROM %s d
		WHERE %s AND d.type IN ('blocks', 'parent-child')
	`, depTargetExpr("d"), depTable, depTargetIn("d", inClause))
	rows, err := tx.QueryContext(ctx, blocksQuery, args...)
	if err != nil {
		if optionalBlockedTable(depTable) && isTableNotExistError(err) {
			return nil
		}
		return fmt.Errorf("get blocks info from %s: %w", depTable, err)
	}
	for rows.Next() {
		var blockerID, blockedID, depType string
		if scanErr := rows.Scan(&blockerID, &blockedID, &depType); scanErr != nil {
			_ = rows.Close()
			return fmt.Errorf("get blocking info: scan blocks: %w", scanErr)
		}
		if statusByID[blockerID] == types.StatusClosed || depType == "parent-child" {
			continue
		}
		blocksMap[blockerID] = append(blocksMap[blockerID], blockedID)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("get blocking info: blocks rows: %w", err)
	}
	return nil
}

func loadStatusByIDInTx(ctx context.Context, tx DBTX, ids []string) (map[string]types.Status, error) {
	statusByID := make(map[string]types.Status)
	if len(ids) == 0 {
		return statusByID, nil
	}

	sourceByID := make(map[string]string)
	for _, issueTable := range []string{"issues", "wisps"} {
		missing, err := loadStatusFromTable(ctx, tx, issueTable, ids, sourceByID, statusByID)
		if err != nil {
			return nil, err
		}
		if missing {
			continue
		}
	}
	return statusByID, nil
}

func loadStatusFromTable(ctx context.Context, tx DBTX, table string, ids []string, sourceByID map[string]string, statusByID map[string]types.Status) (bool, error) {
	total := len(ids)
	for start := 0; start < total; start += queryBatchSize {
		end := start + queryBatchSize
		if end > total {
			end = total
		}
		placeholders, args := buildSQLInClause(ids[start:end])
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
			SELECT id, status FROM %s WHERE id IN (%s)
		`, table, placeholders), args...)
		if err != nil {
			if optionalBlockedTable(table) && isTableNotExistError(err) {
				return true, nil
			}
			return false, fmt.Errorf("status from %s: %w", table, err)
		}
		if err := readStatusRows(rows, table, sourceByID, statusByID); err != nil {
			return false, err
		}
	}
	return false, nil
}

func readStatusRows(rows *sql.Rows, table string, sourceByID map[string]string, statusByID map[string]types.Status) error {
	for rows.Next() {
		var id string
		var status types.Status
		if err := rows.Scan(&id, &status); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan status: %w", err)
		}
		if _, exists := sourceByID[id]; exists {
			// Prefer wisps-table status on cross-table dup (be-iabdi).
			// Tables iterate issues→wisps so the second encounter is always wisps.
			sourceByID[id] = table
			statusByID[id] = status
			continue
		}
		sourceByID[id] = table
		statusByID[id] = status
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("status rows from %s: %w", table, err)
	}
	return nil
}
