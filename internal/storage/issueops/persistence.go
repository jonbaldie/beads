package issueops

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/types"
)

// PersistenceMoveResult reports the durable tables changed by a persistence move.
type PersistenceMoveResult struct {
	Changed       bool
	ChangedTables map[string]bool
}

// MoveIssuePersistenceInTx moves a complete issue aggregate to the requested persistence mode.
func MoveIssuePersistenceInTx(ctx context.Context, tx DBTX, current *types.Issue, mode types.PersistenceMode) (PersistenceMoveResult, error) {
	move, err := preparePersistenceMove(ctx, tx, current, mode)
	if err != nil {
		return PersistenceMoveResult{}, err
	}
	if persistenceMoveIsNoop(move) {
		return PersistenceMoveResult{}, nil
	}
	if move.sourceWisp == move.targetWisp {
		return normalizePersistenceMove(ctx, tx, move)
	}
	return executePersistenceMove(ctx, tx, move)
}

type persistenceMove struct {
	current      *types.Issue
	ephemeral    bool
	noHistory    bool
	storageClass types.StorageClass
	sourceWisp   bool
	targetWisp   bool
}

func preparePersistenceMove(ctx context.Context, tx DBTX, current *types.Issue, mode types.PersistenceMode) (persistenceMove, error) {
	if tx == nil || current == nil || current.ID == "" {
		return persistenceMove{}, fmt.Errorf("move issue persistence: issue and transaction are required")
	}
	ephemeral, noHistory, storageClass, err := types.NormalizePersistenceMode(*current, mode)
	if err != nil {
		return persistenceMove{}, err
	}
	sourceWisp, err := isActiveWispForPersistenceMove(ctx, tx, current.ID)
	if err != nil {
		return persistenceMove{}, err
	}
	return persistenceMove{
		current:      current,
		ephemeral:    ephemeral,
		noHistory:    noHistory,
		storageClass: storageClass,
		sourceWisp:   sourceWisp,
		targetWisp:   ephemeral || noHistory,
	}, nil
}

func persistenceMoveIsNoop(move persistenceMove) bool {
	return move.sourceWisp == move.targetWisp &&
		move.current.Ephemeral == move.ephemeral &&
		move.current.NoHistory == move.noHistory &&
		move.current.StorageClass.Normalize() == move.storageClass.Normalize()
}

func normalizePersistenceMove(ctx context.Context, tx DBTX, move persistenceMove) (PersistenceMoveResult, error) {
	// THE TABLE IS THE ROW'S OWN PLANE, not `wisps`. This branch runs when
	// no move is needed but the flags still disagree with the row, and that
	// state is reachable in the ISSUES plane: a durable row carrying
	// ephemeral = 1 is exactly what the pre-role proxied update produced,
	// and `bd update --persistent` is the command an operator reaches for
	// to repair it. Hardcoding `wisps` sent that repair at the wrong table,
	// where it matched nothing and still reported success.
	table := persistenceIssueTable(move.sourceWisp)
	if err := normalizePersistenceRow(ctx, tx, table, move); err != nil {
		return PersistenceMoveResult{}, err
	}
	// The flags are part of the bead snapshot, so a normalize journals as
	// an update. The no-change early return above emits nothing.
	if err := RecordEventInTx(ctx, tx, EventUpdate, move.current.ID); err != nil {
		return PersistenceMoveResult{}, err
	}
	return PersistenceMoveResult{Changed: true, ChangedTables: map[string]bool{table: true}}, nil
}

func normalizePersistenceRow(ctx context.Context, tx DBTX, table string, move persistenceMove) error {
	//nolint:gosec // G201: table comes from persistenceIssueTable, not from input.
	res, err := tx.ExecContext(ctx, `UPDATE `+table+` SET ephemeral = ?, no_history = ?, storage_class = ?, row_lock = ? WHERE id = ?`, move.ephemeral, move.noHistory, move.storageClass.Normalize(), FreshRowLock(), move.current.ID)
	if err != nil {
		return fmt.Errorf("normalize local issue persistence: %w", err)
	}
	// Changed: true is a claim about the database, so check it rather than
	// assert it. A zero-row update here means the row is not where the
	// plane probe said it was, and reporting success would hand the caller
	// a repair that did not happen.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("normalize local issue persistence: %s names no row in %s", move.current.ID, table)
	}
	return nil
}

func executePersistenceMove(ctx context.Context, tx DBTX, move persistenceMove) (PersistenceMoveResult, error) {
	result := PersistenceMoveResult{Changed: true, ChangedTables: map[string]bool{}}
	if err := preparePersistenceAggregate(ctx, tx, move); err != nil {
		return PersistenceMoveResult{}, err
	}
	auxTables, err := copyPersistenceAuxiliary(ctx, tx, move.current.ID, move.sourceWisp, move.targetWisp)
	if err != nil {
		return PersistenceMoveResult{}, err
	}
	for table := range auxTables {
		result.ChangedTables[table] = true
	}
	inboundTables, err := retargetPersistenceInbound(ctx, tx, move)
	if err != nil {
		return PersistenceMoveResult{}, err
	}
	for table := range inboundTables {
		result.ChangedTables[table] = true
	}
	if err := deletePersistenceSource(ctx, tx, move); err != nil {
		return PersistenceMoveResult{}, err
	}
	return finishPersistenceMove(ctx, tx, move, result)
}

func preparePersistenceAggregate(ctx context.Context, tx DBTX, move persistenceMove) error {
	if !move.sourceWisp && move.targetWisp {
		if err := rejectPersistenceDemotion(ctx, tx, move.current.ID); err != nil {
			return err
		}
	}
	if err := insertPersistenceTarget(ctx, tx, move); err != nil {
		return err
	}
	return nil
}

func insertPersistenceTarget(ctx context.Context, tx DBTX, move persistenceMove) error {
	moved := *move.current
	moved.Ephemeral, moved.NoHistory, moved.StorageClass, moved.RowVersion = move.ephemeral, move.noHistory, move.storageClass, FreshRowLock()
	if err := InsertIssueStrictInTx(ctx, tx, persistenceIssueTable(move.targetWisp), &moved); err != nil {
		return fmt.Errorf("insert moved issue %s: %w", move.current.ID, err)
	}
	return nil
}

func retargetPersistenceInbound(ctx context.Context, tx DBTX, move persistenceMove) (map[string]bool, error) {
	targetColumn := "depends_on_wisp_id"
	if move.targetWisp {
		targetColumn = "depends_on_issue_id"
	}
	inboundTables, err := inboundPersistenceDependencyTables(ctx, tx, move.current.ID, targetColumn)
	if err != nil {
		return nil, err
	}
	if move.targetWisp {
		if err := RetargetInboundDependenciesToWispInTx(ctx, tx, move.current.ID); err != nil {
			return nil, err
		}
		if err := DeleteLeaseInTx(ctx, tx, move.current.ID); err != nil {
			return nil, err
		}
		return inboundTables, nil
	}
	if err := RetargetInboundDependenciesToIssueInTx(ctx, tx, move.current.ID); err != nil {
		return nil, err
	}
	return inboundTables, nil
}

func deletePersistenceSource(ctx context.Context, tx DBTX, move persistenceMove) error {
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id = ?`, persistenceIssueTable(move.sourceWisp)), move.current.ID); err != nil {
		return fmt.Errorf("delete moved issue %s: %w", move.current.ID, err)
	}
	return nil
}

func finishPersistenceMove(ctx context.Context, tx DBTX, move persistenceMove, result PersistenceMoveResult) (PersistenceMoveResult, error) {
	issues, wisps, err := affectedForPersistenceMove(ctx, tx, move.current.ID, move.targetWisp)
	if err != nil {
		return PersistenceMoveResult{}, err
	}
	recompute, err := RecomputeIsBlockedInTxWithResult(ctx, tx, issues, wisps)
	if err != nil {
		return PersistenceMoveResult{}, fmt.Errorf("recompute blocked state: %w", err)
	}
	if recompute.IssueRowsChanged {
		result.ChangedTables["issues"] = true
	}
	if recompute.WispRowsChanged {
		result.ChangedTables["wisps"] = true
	}
	result.ChangedTables[persistenceIssueTable(move.sourceWisp)] = true
	result.ChangedTables[persistenceIssueTable(move.targetWisp)] = true
	// The bead keeps its ID across a plane move; only where it is stored
	// changes. Journal one update carrying the moved snapshot, after derived
	// blocked-state maintenance has settled.
	if err := RecordEventInTx(ctx, tx, EventUpdate, move.current.ID); err != nil {
		return PersistenceMoveResult{}, err
	}
	return result, nil
}

func persistenceIssueTable(wisp bool) string {
	if wisp {
		return "wisps"
	}
	return "issues"
}

func rejectPersistenceDemotion(ctx context.Context, tx DBTX, id string) error {
	for _, table := range []string{"issue_snapshots", "compaction_snapshots"} {
		var count int
		if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE issue_id = ?`, table), id).Scan(&count); err != nil {
			return fmt.Errorf("check retained snapshots in %s: %w", table, err)
		}
		if count > 0 {
			return fmt.Errorf("cannot demote issue %s with retained snapshots", id)
		}
	}
	return nil
}

func copyPersistenceAuxiliary(ctx context.Context, tx DBTX, id string, sourceWisp, targetWisp bool) (map[string]bool, error) {
	changed := map[string]bool{}
	for i, from := range persistenceAuxTables(sourceWisp) {
		to := persistenceAuxTables(targetWisp)[i]
		columns, key, err := persistenceColumns(from)
		if err != nil {
			return nil, err
		}
		copied, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM %s WHERE %s = ?`, to, columns, columns, from, key), id)
		if err != nil {
			return nil, fmt.Errorf("copy %s: %w", from, err)
		}
		if rows, err := copied.RowsAffected(); err == nil && rows > 0 {
			changed[from], changed[to] = true, true
		}
		deleted, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE %s = ?`, from, key), id)
		if err != nil {
			return nil, fmt.Errorf("delete %s: %w", from, err)
		}
		_ = deleted
	}
	return changed, nil
}

func isActiveWispForPersistenceMove(ctx context.Context, tx DBTX, id string) (bool, error) {
	var wispCount, issueCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM wisps WHERE id = ?`, id).Scan(&wispCount); err != nil {
		return false, fmt.Errorf("locate issue persistence plane: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE id = ?`, id).Scan(&issueCount); err != nil {
		return false, fmt.Errorf("locate issue persistence plane: %w", err)
	}
	if issueCount > 0 && wispCount > 0 {
		return false, fmt.Errorf("locate issue persistence plane: ID %q exists in both issues and wisps", id)
	}
	return wispCount > 0, nil
}

func persistenceAuxTables(wisp bool) []string {
	if wisp {
		return []string{"wisp_labels", "wisp_dependencies", "wisp_events", "wisp_comments", "wisp_child_counters"}
	}
	return []string{"labels", "dependencies", "events", "comments", "child_counters"}
}

func inboundPersistenceDependencyTables(ctx context.Context, tx DBTX, id, targetColumn string) (map[string]bool, error) {
	if targetColumn != "depends_on_issue_id" && targetColumn != "depends_on_wisp_id" {
		return nil, fmt.Errorf("unknown dependency target column %q", targetColumn)
	}
	changed := map[string]bool{}
	for _, table := range []string{"dependencies", "wisp_dependencies"} {
		var count int
		if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s = ?`, table, targetColumn), id).Scan(&count); err != nil {
			return nil, fmt.Errorf("find inbound dependencies in %s: %w", table, err)
		}
		if count > 0 {
			changed[table] = true
		}
	}
	return changed, nil
}

func persistenceColumns(table string) (string, string, error) {
	switch table {
	case "labels", "wisp_labels":
		return "issue_id, label", "issue_id", nil
	case "dependencies", "wisp_dependencies":
		return "id, issue_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external, type, created_at, created_by, metadata, thread_id", "issue_id", nil
	case "events", "wisp_events":
		return "id, issue_id, event_type, actor, old_value, new_value, comment, created_at", "issue_id", nil
	case "comments", "wisp_comments":
		return "id, issue_id, author, text, created_at", "issue_id", nil
	case "child_counters", "wisp_child_counters":
		return "parent_id, last_child", "parent_id", nil
	default:
		return "", "", fmt.Errorf("unknown persistence table %q", table)
	}
}

func affectedForPersistenceMove(ctx context.Context, tx DBTX, id string, targetWisp bool) ([]string, []string, error) {
	if targetWisp {
		return AffectedByStatusChangeForWispInTx(ctx, tx, id)
	}
	return AffectedByStatusChangeInTx(ctx, tx, id)
}
