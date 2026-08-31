package issueops

import (
	"context"
	"database/sql"
	"fmt"
)

// DBTX is the minimal statement-execution surface the blocked-state
// recompute needs. *sql.Tx satisfies it (the classic embedded path) and so
// does the domain/db Runner (the server/proxied path): is_blocked is derived
// state shared by both stacks, so they must derive it with the same code
// (bd-6dnrw.44 item 3).
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const waitsForGateBlockedSQL = `
		(
		  (
		    EXISTS (
		      SELECT 1 FROM dependencies cd JOIN issues child ON child.id = cd.issue_id
		      WHERE cd.type = 'parent-child'
		        AND ((d.depends_on_issue_id IS NOT NULL AND cd.depends_on_issue_id = d.depends_on_issue_id)
		          OR (d.depends_on_wisp_id IS NOT NULL AND cd.depends_on_wisp_id = d.depends_on_wisp_id))
		        AND child.status <> 'closed' AND child.status <> 'pinned'
		    )
		    OR EXISTS (
		      SELECT 1 FROM wisp_dependencies cd JOIN wisps child ON child.id = cd.issue_id
		      WHERE cd.type = 'parent-child'
		        AND ((d.depends_on_issue_id IS NOT NULL AND cd.depends_on_issue_id = d.depends_on_issue_id)
		          OR (d.depends_on_wisp_id IS NOT NULL AND cd.depends_on_wisp_id = d.depends_on_wisp_id))
		        AND child.status <> 'closed' AND child.status <> 'pinned'
		    )
		  )
		  AND NOT (
		    -- COALESCE: metadata without a gate key (legacy '{}' rows) means the
		    -- all-children default; a NULL here would poison the AND/NOT chain
		    -- and unblock the gate as soon as any child closes.
		    COALESCE(JSON_UNQUOTE(JSON_EXTRACT(d.metadata, '$.gate')), 'all-children') = 'any-children'
		    AND (
		      EXISTS (
		        SELECT 1 FROM dependencies cd JOIN issues child ON child.id = cd.issue_id
		        WHERE cd.type = 'parent-child'
		          AND ((d.depends_on_issue_id IS NOT NULL AND cd.depends_on_issue_id = d.depends_on_issue_id)
		            OR (d.depends_on_wisp_id IS NOT NULL AND cd.depends_on_wisp_id = d.depends_on_wisp_id))
		          AND child.status = 'closed'
		      )
		      OR EXISTS (
		        SELECT 1 FROM wisp_dependencies cd JOIN wisps child ON child.id = cd.issue_id
		        WHERE cd.type = 'parent-child'
		          AND ((d.depends_on_issue_id IS NOT NULL AND cd.depends_on_issue_id = d.depends_on_issue_id)
		            OR (d.depends_on_wisp_id IS NOT NULL AND cd.depends_on_wisp_id = d.depends_on_wisp_id))
		          AND child.status = 'closed'
		      )
		    )
		  )
		)
		OR (
		  -- also_blocks (GH#3783/GH#3875): a waits-for edge collapsed from a
		  -- redundant needs/depends_on blocks edge onto this same spawner
		  -- (cmd/bd/cook.go collectDependencies) additionally carries classic
		  -- blocking semantics — it must block while the spawner itself is
		  -- open, not only while the spawner has an open parent-child child.
		  -- This closes the pre-fanout window where the waiter could become
		  -- ready before the spawner (and its fanout) ever completed. Legacy
		  -- rows and plain (non-collapsed) waits-for edges lack the
		  -- also_blocks key, so COALESCE defaults to 'false' and this branch
		  -- is a no-op for them (zero behavior change).
		  --
		  -- This is a top-level OR, deliberately outside (and overriding) the
		  -- any-children early-open carve-out above: a collapsed edge means
		  -- the caller's needs/depends_on required the spawner itself to
		  -- close, so an early-open child close must NOT unblock the waiter
		  -- while the spawner remains open.
		  COALESCE(JSON_UNQUOTE(JSON_EXTRACT(d.metadata, '$.also_blocks')), 'false') = 'true'
		  AND (
		    EXISTS (
		      SELECT 1 FROM issues sp
		      WHERE sp.id = d.depends_on_issue_id
		        AND sp.status <> 'closed' AND sp.status <> 'pinned'
		    )
		    OR EXISTS (
		      SELECT 1 FROM wisps sp
		      WHERE sp.id = d.depends_on_wisp_id
		        AND sp.status <> 'closed' AND sp.status <> 'pinned'
		    )
		  )
		)
`

// RecomputeIsBlockedResult reports which issue tables had rows changed while
// the blocked-state fixpoint converged.
type RecomputeIsBlockedResult struct {
	IssueRowsChanged bool
	WispRowsChanged  bool
}

// RecomputeIsBlockedInTx recomputes blocked state and discards the per-table
// change result retained by RecomputeIsBlockedInTxWithResult.
func RecomputeIsBlockedInTx(ctx context.Context, tx DBTX, issueIDs, wispIDs []string) error {
	_, err := RecomputeIsBlockedInTxWithResult(ctx, tx, issueIDs, wispIDs)
	return err
}

// RecomputeIsBlockedInTxWithResult recomputes blocked state to a fixpoint and
// reports whether an UPDATE changed rows in each issue table.
func RecomputeIsBlockedInTxWithResult(
	ctx context.Context, tx DBTX, issueIDs, wispIDs []string,
) (RecomputeIsBlockedResult, error) {
	if len(issueIDs) == 0 && len(wispIDs) == 0 {
		return RecomputeIsBlockedResult{}, nil
	}
	before, err := captureBlockedJournalSnapshot(ctx, tx, issueIDs, wispIDs)
	if err != nil {
		return RecomputeIsBlockedResult{}, err
	}
	result, err := recomputeBlockedUntilFixpoint(ctx, tx, issueIDs, wispIDs)
	if err != nil {
		return result, err
	}
	return result, recordBlockedJournalChanges(ctx, tx, before, issueIDs, wispIDs)
}

func recomputeBlockedUntilFixpoint(ctx context.Context, tx DBTX, issueIDs, wispIDs []string) (RecomputeIsBlockedResult, error) {
	var result RecomputeIsBlockedResult
	for {
		changed, issueChanged, wispChanged, err := recomputeBlockedPassInTx(ctx, tx, issueIDs, wispIDs)
		if err != nil {
			return result, err
		}
		result.IssueRowsChanged = result.IssueRowsChanged || issueChanged
		result.WispRowsChanged = result.WispRowsChanged || wispChanged
		if changed == 0 {
			return result, nil
		}
	}
}

func recomputeBlockedPassInTx(ctx context.Context, tx DBTX, issueIDs, wispIDs []string) (changed int64, issueChanged, wispChanged bool, err error) {
	n, err := recomputeIsBlockedPassForIssuesInTx(ctx, tx, issueIDs)
	if err != nil {
		return 0, false, false, err
	}
	changed += n
	issueChanged = n > 0

	n, err = recomputeIsBlockedPassForWispsInTx(ctx, tx, wispIDs)
	if err != nil {
		return changed, issueChanged, false, err
	}
	changed += n
	return changed, issueChanged, n > 0, nil
}

func MarkIsBlockedInTx(ctx context.Context, tx DBTX, issueIDs, wispIDs []string) error {
	if len(issueIDs) == 0 && len(wispIDs) == 0 {
		return nil
	}
	before, err := captureBlockedJournalSnapshot(ctx, tx, issueIDs, wispIDs)
	if err != nil {
		return err
	}
	for {
		var changed int64

		n, err := markIsBlockedPassForIssuesInTx(ctx, tx, issueIDs)
		if err != nil {
			return err
		}
		changed += n

		n, err = markIsBlockedPassForWispsInTx(ctx, tx, wispIDs)
		if err != nil {
			return err
		}
		changed += n

		if changed == 0 {
			return recordBlockedJournalChanges(ctx, tx, before, issueIDs, wispIDs)
		}
	}
}

func RecomputeIsBlockedForIDsInTx(ctx context.Context, tx DBTX, ids []string) error {
	return RecomputeIsBlockedInTx(ctx, tx, ids, nil)
}

func RecomputeIsBlockedForWispIDsInTx(ctx context.Context, tx DBTX, ids []string) error {
	return RecomputeIsBlockedInTx(ctx, tx, nil, ids)
}

//nolint:gosec // G201: SQL templates are constant; only IN-clause placeholders are formatted in.
func recomputeIsBlockedPassForIssuesInTx(ctx context.Context, tx DBTX, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	return runMarkUnmarkBatchedInTx(ctx, tx, markBlockedTemplateForIssues(), unmarkBlockedTemplateForIssues(), ids)
}

func markIsBlockedPassForIssuesInTx(ctx context.Context, tx DBTX, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	return runMarkBatchedInTx(ctx, tx, markBlockedTemplateForIssues(), ids)
}

// The mark/unmark templates explicitly assign updated_at to itself:
// issues.updated_at (and wisps.updated_at) carry ON UPDATE CURRENT_TIMESTAMP,
// and is_blocked is DERIVED state - letting a recompute bump updated_at
// plants per-clone wall clock in a synced table (merge conflicts between
// clones that recomputed the same flip at different times, bd-578h9.19) and
// makes stale-guard/conflict-guard consumers treat the row as user-edited.
// An explicit assignment suppresses the ON UPDATE clause.
func markBlockedTemplateForIssues() string {
	return fmt.Sprintf(`
		UPDATE issues i SET i.is_blocked = 1, i.updated_at = i.updated_at
		WHERE i.id IN (%%s)
		  AND i.is_blocked = 0
		  AND i.status <> 'closed' AND i.status <> 'pinned'
		  AND (
		    EXISTS (
		      SELECT 1 FROM dependencies d
		      JOIN issues t ON t.id = d.depends_on_issue_id
		      WHERE d.issue_id = i.id
		        AND (d.type = 'blocks' OR d.type = 'conditional-blocks')
		        AND t.status <> 'closed' AND t.status <> 'pinned'
		    )
		    OR EXISTS (
		      SELECT 1 FROM dependencies d
		      JOIN wisps t ON t.id = d.depends_on_wisp_id
		      WHERE d.issue_id = i.id
		        AND (d.type = 'blocks' OR d.type = 'conditional-blocks')
		        AND t.status <> 'closed' AND t.status <> 'pinned'
		    )
		    OR EXISTS (
		      SELECT 1 FROM dependencies d
		      JOIN issues p ON p.id = d.depends_on_issue_id
		      WHERE d.issue_id = i.id
		        AND d.type = 'parent-child'
		        AND p.is_blocked = 1
		    )
		    OR EXISTS (
		      SELECT 1 FROM dependencies d
		      JOIN wisps p ON p.id = d.depends_on_wisp_id
		      WHERE d.issue_id = i.id
		        AND d.type = 'parent-child'
		        AND p.is_blocked = 1
		    )
		    OR EXISTS (
		      SELECT 1 FROM dependencies d
		      WHERE d.issue_id = i.id AND d.type = 'waits-for'
		        AND (%s)
		    )
		  )
	`, waitsForGateBlockedSQL)
}

func unmarkBlockedTemplateForIssues() string {
	return fmt.Sprintf(`
		UPDATE issues i SET i.is_blocked = 0, i.updated_at = i.updated_at
		WHERE i.id IN (%%s)
		  AND i.is_blocked = 1
		  AND (
		    i.status = 'closed' OR i.status = 'pinned'
		    OR (
		      NOT EXISTS (
		        SELECT 1 FROM dependencies d
		        JOIN issues t ON t.id = d.depends_on_issue_id
		        WHERE d.issue_id = i.id
		          AND (d.type = 'blocks' OR d.type = 'conditional-blocks')
		          AND t.status <> 'closed' AND t.status <> 'pinned'
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM dependencies d
		        JOIN wisps t ON t.id = d.depends_on_wisp_id
		        WHERE d.issue_id = i.id
		          AND (d.type = 'blocks' OR d.type = 'conditional-blocks')
		          AND t.status <> 'closed' AND t.status <> 'pinned'
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM dependencies d
		        JOIN issues p ON p.id = d.depends_on_issue_id
		        WHERE d.issue_id = i.id
		          AND d.type = 'parent-child'
		          AND p.is_blocked = 1
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM dependencies d
		        JOIN wisps p ON p.id = d.depends_on_wisp_id
		        WHERE d.issue_id = i.id
		          AND d.type = 'parent-child'
		          AND p.is_blocked = 1
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM dependencies d
		        WHERE d.issue_id = i.id AND d.type = 'waits-for'
		          AND (%s)
		      )
		    )
		  )
	`, waitsForGateBlockedSQL)
}

//nolint:gosec // G201: SQL templates are constant; only IN-clause placeholders are formatted in.
func recomputeIsBlockedPassForWispsInTx(ctx context.Context, tx DBTX, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	return runMarkUnmarkBatchedInTx(ctx, tx, markBlockedTemplateForWisps(), unmarkBlockedTemplateForWisps(), ids)
}

func markIsBlockedPassForWispsInTx(ctx context.Context, tx DBTX, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	return runMarkBatchedInTx(ctx, tx, markBlockedTemplateForWisps(), ids)
}

func markBlockedTemplateForWisps() string {
	return fmt.Sprintf(`
		UPDATE wisps w SET w.is_blocked = 1, w.updated_at = w.updated_at
		WHERE w.id IN (%%s)
		  AND w.is_blocked = 0
		  AND w.status <> 'closed' AND w.status <> 'pinned'
		  AND (
		    EXISTS (
		      SELECT 1 FROM wisp_dependencies d
		      JOIN issues t ON t.id = d.depends_on_issue_id
		      WHERE d.issue_id = w.id
		        AND (d.type = 'blocks' OR d.type = 'conditional-blocks')
		        AND t.status <> 'closed' AND t.status <> 'pinned'
		    )
		    OR EXISTS (
		      SELECT 1 FROM wisp_dependencies d
		      JOIN wisps t ON t.id = d.depends_on_wisp_id
		      WHERE d.issue_id = w.id
		        AND (d.type = 'blocks' OR d.type = 'conditional-blocks')
		        AND t.status <> 'closed' AND t.status <> 'pinned'
		    )
		    OR EXISTS (
		      SELECT 1 FROM wisp_dependencies d
		      JOIN issues p ON p.id = d.depends_on_issue_id
		      WHERE d.issue_id = w.id
		        AND d.type = 'parent-child'
		        AND p.is_blocked = 1
		    )
		    OR EXISTS (
		      SELECT 1 FROM wisp_dependencies d
		      JOIN wisps p ON p.id = d.depends_on_wisp_id
		      WHERE d.issue_id = w.id
		        AND d.type = 'parent-child'
		        AND p.is_blocked = 1
		    )
		    OR EXISTS (
		      SELECT 1 FROM wisp_dependencies d
		      WHERE d.issue_id = w.id AND d.type = 'waits-for'
		        AND (%s)
		    )
		  )
	`, waitsForGateBlockedSQL)
}

func unmarkBlockedTemplateForWisps() string {
	return fmt.Sprintf(`
		UPDATE wisps w SET w.is_blocked = 0, w.updated_at = w.updated_at
		WHERE w.id IN (%%s)
		  AND w.is_blocked = 1
		  AND (
		    w.status = 'closed' OR w.status = 'pinned'
		    OR (
		      NOT EXISTS (
		        SELECT 1 FROM wisp_dependencies d
		        JOIN issues t ON t.id = d.depends_on_issue_id
		        WHERE d.issue_id = w.id
		          AND (d.type = 'blocks' OR d.type = 'conditional-blocks')
		          AND t.status <> 'closed' AND t.status <> 'pinned'
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM wisp_dependencies d
		        JOIN wisps t ON t.id = d.depends_on_wisp_id
		        WHERE d.issue_id = w.id
		          AND (d.type = 'blocks' OR d.type = 'conditional-blocks')
		          AND t.status <> 'closed' AND t.status <> 'pinned'
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM wisp_dependencies d
		        JOIN issues p ON p.id = d.depends_on_issue_id
		        WHERE d.issue_id = w.id
		          AND d.type = 'parent-child'
		          AND p.is_blocked = 1
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM wisp_dependencies d
		        JOIN wisps p ON p.id = d.depends_on_wisp_id
		        WHERE d.issue_id = w.id
		          AND d.type = 'parent-child'
		          AND p.is_blocked = 1
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM wisp_dependencies d
		        WHERE d.issue_id = w.id AND d.type = 'waits-for'
		          AND (%s)
		      )
		    )
		  )
	`, waitsForGateBlockedSQL)
}

//nolint:gosec // G201: callers pass constant templates; only IN-clause placeholders are formatted in.
func runMarkUnmarkBatchedInTx(ctx context.Context, tx DBTX, markTmpl, unmarkTmpl string, ids []string) (int64, error) {
	var changed int64
	total := len(ids)
	for start := 0; start < total; start += queryBatchSize {
		end := start + queryBatchSize
		if end > total {
			end = total
		}
		placeholders, args := buildSQLInClause(ids[start:end])

		res, err := tx.ExecContext(ctx, fmt.Sprintf(markTmpl, placeholders), args...)
		if err != nil {
			return changed, fmt.Errorf("recompute is_blocked (mark): %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return changed, fmt.Errorf("recompute is_blocked (mark rows affected): %w", err)
		}
		changed += n

		res, err = tx.ExecContext(ctx, fmt.Sprintf(unmarkTmpl, placeholders), args...)
		if err != nil {
			return changed, fmt.Errorf("recompute is_blocked (unmark): %w", err)
		}
		n, err = res.RowsAffected()
		if err != nil {
			return changed, fmt.Errorf("recompute is_blocked (unmark rows affected): %w", err)
		}
		changed += n
	}
	return changed, nil
}

//nolint:gosec // G201: callers pass constant templates; only IN-clause placeholders are formatted in.
func runMarkBatchedInTx(ctx context.Context, tx DBTX, markTmpl string, ids []string) (int64, error) {
	var changed int64
	total := len(ids)
	for start := 0; start < total; start += queryBatchSize {
		end := start + queryBatchSize
		if end > total {
			end = total
		}
		placeholders, args := buildSQLInClause(ids[start:end])

		res, err := tx.ExecContext(ctx, fmt.Sprintf(markTmpl, placeholders), args...)
		if err != nil {
			return changed, fmt.Errorf("mark is_blocked: %w", err)
		}
		n, _ := res.RowsAffected()
		changed += n
	}
	return changed, nil
}
