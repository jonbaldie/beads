package versioncontrolops

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/storage/issueops"
)

// issuesRowMerge is the field-level merge decision for one conflicted issues
// row: the columns whose merged value differs from OUR working-set value, and
// the raw values to write.
type issuesRowMerge struct {
	ourKey  any
	columns []string
	values  []any
	// lww names the cells both sides changed differently, which were settled
	// by timestamp rather than merged. They are the only cells where one
	// side's edit is superseded, so the resolver names them on stderr.
	lww []string
}

type issueMergeInputs struct {
	ourKey              any
	ourUpdatedRaw       any
	theirUpdatedRaw     any
	ourUpdated          time.Time
	theirUpdated        time.Time
	ourTimeOK           bool
	theirTimeOK         bool
	lwwUsable           bool
	theirsWinLWW        bool
	cols                []string
	verdicts            map[string]cellVerdict
	theirVals           map[string]any
	closeGroupContested bool
	inCloseGroup        map[string]bool
}

// classifyCell compares one column's three sides. ok is false when the
// conflict row cannot be classified at all (a side's column is missing).
func classifyCell(row rawConflictRow, col string) (cellVerdict, any, bool) {
	ourVal, ourHas := row.value("our", col)
	theirVal, theirHas := row.value("their", col)
	if !ourHas || !theirHas {
		return 0, nil, false
	}
	if conflictCellsEqual(ourVal, theirVal) {
		return cellAgree, theirVal, true
	}
	baseVal, baseHas := row.value("base", col)
	if !baseHas {
		return 0, nil, false
	}
	switch {
	case conflictCellsEqual(theirVal, baseVal):
		return cellOursOnly, theirVal, true
	case conflictCellsEqual(ourVal, baseVal):
		return cellTheirsOnly, theirVal, true
	default:
		return cellContested, theirVal, true
	}
}

// mergeIssuesConflictRow computes the field-level three-way merge of one
// conflicted issues row. ok is false when the row must be left for the
// operator: not modify/modify, a contested cell whose LWW tiebreak is
// ambiguous (equal or unparseable updated_at), or a contested cell whose
// contents LWW cannot merge without loss (issuesNonScalarColumns).
//
// It is pure, so every merge rule is unit-testable without a database.
func mergeIssuesConflictRow(row rawConflictRow) (issuesRowMerge, bool) {
	inputs, ok := prepareIssueMerge(row)
	if !ok {
		return issuesRowMerge{}, false
	}
	merge := issuesRowMerge{ourKey: inputs.ourKey}
	for _, col := range inputs.cols {
		atomicClose := inputs.inCloseGroup[col] && inputs.closeGroupContested
		if !mergeIssueCell(&merge, col, inputs.verdicts[col], inputs.theirVals[col], atomicClose, inputs.lwwUsable, inputs.theirsWinLWW) {
			return issuesRowMerge{}, false
		}
	}
	if len(merge.columns) > 0 {
		finalizeIssueMerge(&merge, row, inputs.ourUpdatedRaw, inputs.theirUpdatedRaw, inputs.ourUpdated, inputs.theirUpdated, inputs.ourTimeOK, inputs.theirTimeOK)
	}
	return merge, true
}

func prepareIssueMerge(row rawConflictRow) (issueMergeInputs, bool) {
	baseOK, ourOK, theirOK := row.sidesPresent([]string{issuesKeyColumn})
	if !baseOK || !ourOK || !theirOK {
		return issueMergeInputs{}, false
	}
	inputs := issueMergeInputs{}
	inputs.ourKey, _ = row.value("our", issuesKeyColumn)
	inputs.ourUpdatedRaw = mustValue(row, "our", "updated_at")
	inputs.theirUpdatedRaw = mustValue(row, "their", "updated_at")
	inputs.ourUpdated, inputs.ourTimeOK = parseConflictTimestamp(inputs.ourUpdatedRaw)
	inputs.theirUpdated, inputs.theirTimeOK = parseConflictTimestamp(inputs.theirUpdatedRaw)
	inputs.lwwUsable = inputs.ourTimeOK && inputs.theirTimeOK && !inputs.ourUpdated.Equal(inputs.theirUpdated)
	inputs.theirsWinLWW = inputs.lwwUsable && inputs.theirUpdated.After(inputs.ourUpdated)
	var ok bool
	inputs.cols, inputs.verdicts, inputs.theirVals, ok = classifyIssueCells(row)
	if !ok || hasContestedNonScalar(inputs.verdicts) {
		return issueMergeInputs{}, false
	}
	inputs.closeGroupContested, inputs.inCloseGroup = classifyCloseGroup(inputs.verdicts)
	return inputs, true
}

func classifyIssueCells(row rawConflictRow) ([]string, map[string]cellVerdict, map[string]any, bool) {
	cols := row.dataColumns(issuesKeyColumn, "row_lock")
	verdicts := make(map[string]cellVerdict, len(cols))
	theirVals := make(map[string]any, len(cols))
	for _, col := range cols {
		verdict, theirVal, ok := classifyCell(row, col)
		if !ok {
			return nil, nil, nil, false
		}
		verdicts[col] = verdict
		theirVals[col] = theirVal
	}
	return cols, verdicts, theirVals, true
}

func hasContestedNonScalar(verdicts map[string]cellVerdict) bool {
	for col, verdict := range verdicts {
		if verdict == cellContested && issuesNonScalarColumns[col] {
			return true
		}
	}
	return false
}

func classifyCloseGroup(verdicts map[string]cellVerdict) (bool, map[string]bool) {
	contested := false
	members := make(map[string]bool, len(issuesCloseGroup))
	for _, col := range issuesCloseGroup {
		members[col] = true
		contested = contested || verdicts[col] == cellContested
	}
	return contested, members
}

func mergeIssueCell(merge *issuesRowMerge, col string, verdict cellVerdict, theirVal any, atomicClose, lwwUsable, theirsWin bool) bool {
	if verdict == cellAgree {
		return true
	}
	if atomicClose {
		return mergeContestedCell(merge, col, theirVal, lwwUsable, theirsWin)
	}
	switch verdict {
	case cellOursOnly:
		return true
	case cellTheirsOnly:
		merge.takeTheirs(col, theirVal)
		return true
	case cellContested:
		return mergeContestedCell(merge, col, theirVal, lwwUsable, theirsWin)
	default:
		return false
	}
}

func mergeContestedCell(merge *issuesRowMerge, col string, theirVal any, lwwUsable, theirsWin bool) bool {
	if !lwwUsable {
		return false
	}
	merge.lww = append(merge.lww, col)
	if theirsWin {
		merge.takeTheirs(col, theirVal)
	}
	return true
}

func (m *issuesRowMerge) takeTheirs(col string, val any) {
	m.columns = append(m.columns, col)
	m.values = append(m.values, val)
}

func finalizeIssueMerge(merge *issuesRowMerge, row rawConflictRow, ourRaw, theirRaw any, ourTime, theirTime time.Time, ourTimeOK, theirTimeOK bool) {
	mergedTime := ourRaw
	if theirTimeOK && (!ourTimeOK || theirTime.After(ourTime)) {
		mergedTime = theirRaw
	}
	merge.setColumn("updated_at", mergedTime)
	if _, hasRowLock := row.value("our", "row_lock"); hasRowLock {
		merge.setColumn("row_lock", freshRowLockDistinctFrom(mustValue(row, "our", "row_lock"), mustValue(row, "their", "row_lock")))
	}
}

// freshRowLockDistinctFrom mints a new row_lock token that differs from both
// parents' tokens. freshRowLock is crypto-random over int64, so a collision is
// already a ~2⁻⁶³ event; rerolling makes the "the settled row's token matches
// neither pre-merge row" guarantee exact rather than probabilistic. The raw
// conflict-table values are compared through the same normalization the merge
// rules use, so a driver handing back []byte("123") for one side and int64 for
// the candidate still compares as equal.
func freshRowLockDistinctFrom(ourLock, theirLock any) int64 {
	for {
		candidate := issueops.FreshRowLock()
		if !conflictCellsEqual(candidate, ourLock) && !conflictCellsEqual(candidate, theirLock) {
			return candidate
		}
	}
}

// setColumn adds or overwrites a column in the write-back plan.
func (m *issuesRowMerge) setColumn(col string, val any) {
	for i, c := range m.columns {
		if c == col {
			m.values[i] = val
			return
		}
	}
	m.columns = append(m.columns, col)
	m.values = append(m.values, val)
}

// mustValue reads a side's column, returning nil when the conflict table has
// no such column (the caller's rules then treat it as absent/unparseable).
func mustValue(row rawConflictRow, side, col string) any {
	v, _ := row.value(side, col)
	return v
}

// issuesConflictsAreFieldMergeable reports whether every live issues conflict
// can be settled by mergeIssuesConflictRow, and returns the merge plan so the
// resolution pass does not recompute it.
func issuesConflictsAreFieldMergeable(ctx context.Context, db DBConn) ([]issuesRowMerge, bool, error) {
	rows, err := loadConflictRows(ctx, db, "issues")
	if err != nil {
		return nil, false, err
	}
	if declineDuplicateConflictRows("issues", []string{issuesKeyColumn}, rows) {
		return nil, false, nil
	}
	plan := make([]issuesRowMerge, 0, len(rows))
	for _, row := range rows {
		merged, ok := mergeIssuesConflictRow(row)
		if !ok {
			return nil, false, nil
		}
		plan = append(plan, merged)
	}
	return plan, true, nil
}

// resolveIssuesFieldMerge applies a plan from issuesConflictsAreFieldMergeable.
//
// DOLT_CONFLICTS_RESOLVE is table-level (--ours/--theirs), which cannot express
// a per-cell merge, so this uses dolt's manual-resolution path: write the
// merged values over our working-set row, then DELETE the conflict row — the
// delete is what tells dolt the row is settled, so it must come last. A row
// whose merge equals our side needs no write at all.
func resolveIssuesFieldMerge(ctx context.Context, db DBConn, plan []issuesRowMerge) error {
	for _, m := range plan {
		reportIssuesLWW(m)
		if m.ourKey == nil {
			return fmt.Errorf("unexpected conflict row with no issue id (safety check bypassed)")
		}
		if err := applyIssuesMerge(ctx, db, m); err != nil {
			return err
		}
		if err := clearIssuesConflict(ctx, db, m.ourKey); err != nil {
			return err
		}
	}
	return nil
}

func reportIssuesLWW(merge issuesRowMerge) {
	if len(merge.lww) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr,
		"Notice: auto-merged issue %v; %s settled last-write-wins (the older side's edit was superseded)\n",
		merge.ourKey, strings.Join(merge.lww, ", "))
}

func applyIssuesMerge(ctx context.Context, db DBConn, merge issuesRowMerge) error {
	if len(merge.columns) == 0 {
		return nil
	}
	sets := make([]string, len(merge.columns))
	args := make([]any, 0, len(merge.columns)+1)
	for i, col := range merge.columns {
		if err := ValidateConflictTable(col); err != nil {
			return fmt.Errorf("refusing to write unexpected column %q of issues: %w", col, err)
		}
		sets[i] = fmt.Sprintf("`%s` = ?", col)
		args = append(args, merge.values[i])
	}
	args = append(args, merge.ourKey)
	stmt := fmt.Sprintf("UPDATE `issues` SET %s WHERE `%s` = ?", strings.Join(sets, ", "), issuesKeyColumn) //nolint:gosec // identifiers validated above
	result, err := db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return fmt.Errorf("apply merged values for issue %v: %w", merge.ourKey, err)
	}
	return confirmIssuesMergeTarget(ctx, db, merge.ourKey, result)
}

func confirmIssuesMergeTarget(ctx context.Context, db DBConn, issueKey any, result sql.Result) error {
	changed, err := result.RowsAffected()
	if err == nil && changed > 0 {
		return nil
	}
	present, err := conflictTargetStillPresent(ctx, db, "issues", issuesKeyColumn, issueKey)
	if err != nil {
		return fmt.Errorf("confirm issue %v still exists after writing merged values: %w", issueKey, err)
	}
	if !present {
		return fmt.Errorf("merged values for issue %v matched no row (was it deleted concurrently?); conflict left unresolved", issueKey)
	}
	return nil
}

func clearIssuesConflict(ctx context.Context, db DBConn, issueKey any) error {
	result, err := db.ExecContext(ctx, "DELETE FROM dolt_conflicts_issues WHERE our_"+issuesKeyColumn+" = ?", issueKey)
	if err != nil {
		return fmt.Errorf("clear conflict for issue %v: %w", issueKey, err)
	}
	if changed, err := result.RowsAffected(); err == nil && changed == 0 {
		return fmt.Errorf("conflict for issue %v was not cleared (no conflict row deleted)", issueKey)
	}
	return nil
}

// unionConflictsAreSafe reports whether every live conflict of a union-merged
// table (labels, comments, events) is the same row on both sides with matching
// columns — the only class where "union" has an unambiguous answer. A row
// missing on one side (a deletion racing an insert) or diverging columns in a
// supposedly immutable row goes to the operator.
func unionConflictsAreSafe(ctx context.Context, db DBConn, table string) ([]unionRowKey, bool, error) {
	keyCols, ok := unionConflictKeyColumns[table]
	if !ok {
		return nil, false, fmt.Errorf("table %s is not union-mergeable", table)
	}
	rows, err := loadConflictRows(ctx, db, table)
	if err != nil {
		return nil, false, err
	}
	if declineDuplicateConflictRows(table, keyCols, rows) {
		return nil, false, nil
	}
	plan := make([]unionRowKey, 0, len(rows))
	for _, row := range rows {
		key, ok := unionRowIsSafe(row, keyCols)
		if !ok {
			return nil, false, nil
		}
		plan = append(plan, key)
	}
	return plan, true, nil
}

// unionRowIsSafe decides one union-table conflict row and returns its key.
// Pure, so the safety property is unit-testable without a database: the row
// must exist on BOTH sides and every column must agree, which is what makes
// "union" unambiguous. A row missing on one side (a deletion racing an insert)
// or a supposedly immutable row whose columns diverge is refused.
func unionRowIsSafe(row rawConflictRow, keyCols []string) (unionRowKey, bool) {
	_, ourOK, theirOK := row.sidesPresent(keyCols)
	if !ourOK || !theirOK {
		return unionRowKey{}, false
	}
	for _, col := range row.dataColumns() {
		ourVal, ourHas := row.value("our", col)
		theirVal, theirHas := row.value("their", col)
		if !ourHas || !theirHas || !conflictCellsEqual(ourVal, theirVal) {
			return unionRowKey{}, false
		}
	}
	key := unionRowKey{columns: keyCols}
	for _, k := range keyCols {
		v, _ := row.value("our", k)
		key.values = append(key.values, v)
	}
	return key, true
}

// unionRowKey is one validated conflict row's primary key, carried from the
// check pass to the resolution pass so the delete is bound to the rows that
// were actually validated rather than to whatever a second query returns.
type unionRowKey struct {
	columns []string
	values  []any
}

// resolveUnionConflicts settles the conflicts unionConflictsAreSafe validated.
// Both sides hold the same row, so our working set already carries the union:
// deleting the conflict row is the whole resolution.
func resolveUnionConflicts(ctx context.Context, db DBConn, table string, plan []unionRowKey) error {
	if _, ok := unionConflictKeyColumns[table]; !ok {
		return fmt.Errorf("table %s is not union-mergeable", table)
	}
	for _, row := range plan {
		preds := make([]string, 0, len(row.columns))
		args := make([]any, 0, len(row.columns))
		for i, k := range row.columns {
			v := row.values[i]
			if v == nil {
				return fmt.Errorf("unexpected %s conflict row with no our_%s (safety check bypassed)", table, k)
			}
			preds = append(preds, "`our_"+k+"` = ?")
			args = append(args, v)
		}
		//nolint:gosec // table and key columns come from the unionConflictKeyColumns allowlist.
		stmt := "DELETE FROM `dolt_conflicts_" + table + "` WHERE " + strings.Join(preds, " AND ")
		res, err := db.ExecContext(ctx, stmt, args...)
		if err != nil {
			return fmt.Errorf("clear %s conflict: %w", table, err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return fmt.Errorf("a %s conflict was not cleared (no conflict row deleted)", table)
		}
	}
	return nil
}
