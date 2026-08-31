package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

func (r *dependencyBlockingRepository) GetBlockingInfo(ctx context.Context, issueIDs []string, opts domain.DepListOpts) (domain.BlockingInfo, error) {
	info := newBlockingInfo()
	if len(issueIDs) == 0 {
		return info, nil
	}

	table := pickDepTable(opts.UseWispsTable)
	idPlaceholders, idArgs := buildInPlaceholders(issueIDs)
	outRows, inRows, err := r.loadBlockingRows(ctx, table, idPlaceholders, idArgs)
	if err != nil {
		return domain.BlockingInfo{}, err
	}

	statusIDs := blockingStatusIDs(outRows, inRows)
	statusByID, err := r.loadStatusByID(ctx, statusIDs)
	if err != nil {
		return domain.BlockingInfo{}, fmt.Errorf("db: DependencySQLRepository.GetBlockingInfo: status lookup: %w", err)
	}
	populateBlockingInfo(&info, outRows, inRows, statusByID)
	return info, nil
}

func newBlockingInfo() domain.BlockingInfo {
	return domain.BlockingInfo{
		BlockedBy: make(map[string][]string),
		Blocks:    make(map[string][]string),
		Parent:    make(map[string]string),
	}
}

func (r *dependencyBlockingRepository) loadBlockingRows(ctx context.Context, table, idPlaceholders string, idArgs []any) ([]blockingRow, []blockingRow, error) {
	//nolint:gosec // G201: table and depTargetExpr are hardcoded constants
	outQ := fmt.Sprintf(
		"SELECT issue_id, %s AS depends_on_id, type FROM %s WHERE issue_id IN (%s) AND type IN ('blocks', 'parent-child')",
		depTargetExpr, table, idPlaceholders,
	)
	outRows, err := r.scanBlockingRows(ctx, outQ, idArgs)
	if err != nil {
		return nil, nil, fmt.Errorf("db: DependencySQLRepository.GetBlockingInfo: outbound: %w", err)
	}

	//nolint:gosec // G201: table and depTargetExpr are hardcoded constants
	inQ := fmt.Sprintf(
		"SELECT issue_id, %s AS depends_on_id, type FROM %s WHERE %s IN (%s) AND type = 'blocks'",
		depTargetExpr, table, depTargetExpr, idPlaceholders,
	)
	inRows, err := r.scanBlockingRows(ctx, inQ, idArgs)
	if err != nil {
		return nil, nil, fmt.Errorf("db: DependencySQLRepository.GetBlockingInfo: inbound: %w", err)
	}
	return outRows, inRows, nil
}

func blockingStatusIDs(outRows, inRows []blockingRow) map[string]struct{} {
	statusIDs := make(map[string]struct{}, len(outRows)+len(inRows))
	for _, row := range outRows {
		statusIDs[row.dependsOnID] = struct{}{}
	}
	for _, row := range inRows {
		statusIDs[row.dependsOnID] = struct{}{}
	}
	return statusIDs
}

func populateBlockingInfo(info *domain.BlockingInfo, outRows, inRows []blockingRow, statusByID map[string]types.Status) {
	for _, row := range outRows {
		if statusByID[row.dependsOnID] == types.StatusClosed {
			continue
		}
		if row.depType == "parent-child" {
			info.Parent[row.issueID] = row.dependsOnID
		} else {
			info.BlockedBy[row.issueID] = append(info.BlockedBy[row.issueID], row.dependsOnID)
		}
	}
	for _, row := range inRows {
		if statusByID[row.dependsOnID] == types.StatusClosed {
			continue
		}
		info.Blocks[row.dependsOnID] = append(info.Blocks[row.dependsOnID], row.issueID)
	}
}

func (r *dependencyBlockingRepository) GetBlockingInfoAcrossIssuesAndWisps(ctx context.Context, issueIDs []string) (domain.BlockingInfo, error) {
	perm, err := r.GetBlockingInfo(ctx, issueIDs, domain.DepListOpts{UseWispsTable: false})
	if err != nil {
		return domain.BlockingInfo{}, err
	}
	wisp, err := r.GetBlockingInfo(ctx, issueIDs, domain.DepListOpts{UseWispsTable: true})
	if err != nil {
		if !dberrors.IsTableNotExist(err) {
			return domain.BlockingInfo{}, err
		}
		wisp = domain.BlockingInfo{
			BlockedBy: map[string][]string{},
			Blocks:    map[string][]string{},
			Parent:    map[string]string{},
		}
	}
	for k, v := range wisp.BlockedBy {
		perm.BlockedBy[k] = append(perm.BlockedBy[k], v...)
	}
	for k, v := range wisp.Blocks {
		perm.Blocks[k] = append(perm.Blocks[k], v...)
	}
	for k, v := range wisp.Parent {
		if _, ok := perm.Parent[k]; !ok {
			perm.Parent[k] = v
		}
	}
	return perm, nil
}

type blockingRow struct {
	issueID, dependsOnID, depType string
}

func (r *dependencyBlockingRepository) scanBlockingRows(ctx context.Context, q string, args []any) ([]blockingRow, error) {
	rows, err := r.runner.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []blockingRow
	for rows.Next() {
		var row blockingRow
		if err := rows.Scan(&row.issueID, &row.dependsOnID, &row.depType); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (r *dependencyBlockingRepository) loadStatusByID(ctx context.Context, idSet map[string]struct{}) (map[string]types.Status, error) {
	statusByID := make(map[string]types.Status, len(idSet))
	if len(idSet) == 0 {
		return statusByID, nil
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	placeholders, args := buildInPlaceholders(ids)
	sourceByID := make(map[string]string, len(idSet))
	for _, table := range []string{"issues", "wisps"} {
		//nolint:gosec // G201: table is a hardcoded constant
		q := fmt.Sprintf("SELECT id, status FROM %s WHERE id IN (%s)", table, placeholders)
		if err := r.scanStatusRows(ctx, q, args, table, statusByID, sourceByID); err != nil {
			return nil, err
		}
	}
	return statusByID, nil
}

func (r *dependencyBlockingRepository) scanStatusRows(ctx context.Context, q string, args []any, table string, statusByID map[string]types.Status, sourceByID map[string]string) error {
	rows, err := r.runner.QueryContext(ctx, q, args...)
	if err != nil {
		if dberrors.IsTableNotExist(err) {
			return nil
		}
		return fmt.Errorf("status from %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var status types.Status
		if err := rows.Scan(&id, &status); err != nil {
			return fmt.Errorf("status from %s: scan: %w", table, err)
		}
		if existing, dup := sourceByID[id]; dup {
			return fmt.Errorf("status id %q exists in both %s and %s", id, existing, table)
		}
		sourceByID[id] = table
		statusByID[id] = status
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("status rows from %s: %w", table, err)
	}
	return nil
}

func (r *dependencyListRepository) queryDeps(ctx context.Context, q string, args []any, into map[string][]*types.Dependency, keyByIssueID bool) error {
	rows, err := r.runner.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		d, err := scanDependencyQueryRow(rows)
		if err != nil {
			return err
		}
		dd := d
		key := d.DependsOnID
		if keyByIssueID {
			key = d.IssueID
		}
		into[key] = append(into[key], &dd)
	}
	return rows.Err()
}

func scanDependencyQueryRow(rows *sql.Rows) (types.Dependency, error) {
	var d types.Dependency
	var typ string
	var createdBy, metadata, threadID sql.NullString
	var createdAt sql.NullTime
	if err := rows.Scan(&d.IssueID, &d.DependsOnID, &typ, &createdAt, &createdBy, &metadata, &threadID); err != nil {
		return types.Dependency{}, fmt.Errorf("scan: %w", err)
	}
	d.Type = types.DependencyType(typ)
	if createdAt.Valid {
		d.CreatedAt = createdAt.Time
	}
	if createdBy.Valid {
		d.CreatedBy = createdBy.String
	}
	if metadata.Valid && metadata.String != "" && metadata.String != "{}" {
		d.Metadata = metadata.String
	}
	if threadID.Valid {
		d.ThreadID = threadID.String
	}
	return d, nil
}

func scanCounts(ctx context.Context, runner Runner, q string, args []any, into map[string]*types.DependencyCounts, assign func(c *types.DependencyCounts, n int)) error {
	rows, err := runner.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		if c, ok := into[id]; ok {
			assign(c, n)
		}
	}
	return rows.Err()
}

func buildInPlaceholders[T ~string](values []T) (string, []any) {
	ph := make([]string, len(values))
	args := make([]any, len(values))
	for i, v := range values {
		ph[i] = "?"
		args[i] = string(v)
	}
	return strings.Join(ph, ","), args
}

func buildTypeFilter(depTypes []types.DependencyType) (string, []any) {
	if len(depTypes) == 0 {
		return "", nil
	}
	ph := make([]string, len(depTypes))
	args := make([]any, len(depTypes))
	for i, t := range depTypes {
		ph[i] = "?"
		args[i] = string(t)
	}
	return " AND type IN (" + strings.Join(ph, ",") + ")", args
}

func combineArgs(a, b []any) []any {
	out := make([]any, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}

func dependencyIDBatch(ids []string, start int) ([]string, string, []any) {
	end := start + deleteBatchSize
	if end > len(ids) {
		end = len(ids)
	}
	batch := ids[start:end]
	ph, args := buildInPlaceholders(batch)
	args = append(args, args...)
	return batch, ph, args
}

func deleteDependencyBatch(ctx context.Context, runner Runner, table, placeholders string, args []any, useWisps bool) (int, bool, error) {
	//nolint:gosec // G201: table is one of two hardcoded constants; placeholders are generated.
	res, err := runner.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM %s WHERE issue_id IN (%s) OR %s IN (%s)", table, placeholders, issueops.DepTargetExpr, placeholders),
		args...)
	if err != nil {
		return 0, useWisps && dberrors.IsTableNotExist(err), err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("db: DependencySQLRepository.DeleteAllForIDs rows affected: %w", err)
	}
	return int(n), false, err
}

func countDependencyBatch(ctx context.Context, runner Runner, table, placeholders string, args []any, useWisps bool) (int, bool, error) {
	var count int
	//nolint:gosec // G201: table is one of two hardcoded constants; placeholders are generated.
	err := runner.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE issue_id IN (%s) OR %s IN (%s)", table, placeholders, issueops.DepTargetExpr, placeholders),
		args...).Scan(&count)
	if err != nil {
		return 0, useWisps && dberrors.IsTableNotExist(err), err
	}
	return count, false, nil
}

func (r *dependencyBulkRepository) DeleteAllForIDs(ctx context.Context, ids []string, opts domain.DepInsertOpts) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	table := pickDepTable(opts.UseWispsTable)
	total := 0
	idsLen := len(ids)
	for start := 0; start < idsLen; start += deleteBatchSize {
		batch, ph, args := dependencyIDBatch(ids, start)
		// Journal the edges this batch is about to remove, while they and their
		// source snapshots are still readable.
		if err := issueops.RecordDependencyRemovalsForTableInTx(ctx, r.runner, table, batch); err != nil {
			return total, fmt.Errorf("db: DependencySQLRepository.DeleteAllForIDs journal removals from %s: %w", table, err)
		}
		n, missing, err := deleteDependencyBatch(ctx, r.runner, table, ph, args, opts.UseWispsTable)
		if missing {
			return total, nil
		}
		if err != nil {
			return total, fmt.Errorf("db: DependencySQLRepository.DeleteAllForIDs from %s: %w", table, err)
		}
		total += n
	}
	return total, nil
}

func (r *dependencyBulkRepository) CountAllForIDs(ctx context.Context, ids []string, opts domain.DepCountsOpts) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	table := pickDepTable(opts.UseWispsTable)
	total := 0
	idsLen := len(ids)
	for start := 0; start < idsLen; start += deleteBatchSize {
		_, ph, args := dependencyIDBatch(ids, start)
		count, missing, err := countDependencyBatch(ctx, r.runner, table, ph, args, opts.UseWispsTable)
		if missing {
			return total, nil
		}
		if err != nil {
			return total, fmt.Errorf("db: DependencySQLRepository.CountAllForIDs from %s: %w", table, err)
		}
		total += count
	}
	return total, nil
}
