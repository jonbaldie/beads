package versioncontrolops

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// TryRepairFKCascadeViolations repairs the post-merge foreign-key constraint
// violations produced by the delete-vs-insert cascade hazard (bd-6dnrw.4): for
// every violating table it deletes the rows whose issue reference dangles,
// clears that table's dolt_constraint_violations entries, and stages the
// table. The caller's session must run with
// @@dolt_force_transaction_commit=1 for the merge to survive long enough to be
// repaired, and must NOT keep the merge when (repaired=false, had=true) —
// unrepaired violations are the operator's.
//
// Returns (repaired, had):
//   - (false, false): no violations — nothing to do.
//   - (true, true): every violation was an issues-FK violation on a known
//     synced child table, and all were repaired and cleared.
//   - (false, true): violations of another shape (different constraint type,
//     unknown table, FK to a different parent) — nothing was touched.
func TryRepairFKCascadeViolations(ctx context.Context, db DBConn) (repaired, had bool, err error) {
	tables, err := constraintViolationTables(ctx, db)
	if err != nil {
		return false, false, err
	}
	if len(tables) == 0 {
		return false, false, nil
	}

	// Validate every violating table before touching any of them.
	safe, err := validateFKCascadeRepairTables(ctx, db, tables)
	if err != nil {
		return false, true, err
	}
	if !safe {
		return false, true, nil
	}

	for _, t := range tables {
		if err := repairFKCascadeTable(ctx, db, t); err != nil {
			return false, true, err
		}
	}

	// The repair must leave nothing behind: a residual violation here means the
	// deletes above did not cover the constraint that fired, and committing
	// would persist a violated working set.
	remaining, err := constraintViolationTables(ctx, db)
	if err != nil {
		return false, true, err
	}
	if len(remaining) > 0 {
		return false, true, nil
	}
	return true, true, nil
}

func validateFKCascadeRepairTables(ctx context.Context, db DBConn, tables []string) (bool, error) {
	for _, table := range tables {
		if _, ok := fkCascadeRepairDeletes[table]; !ok {
			return false, nil
		}
		issueFKOnly, err := violationsAreIssueFKOnly(ctx, db, table)
		if err != nil {
			return false, err
		}
		if !issueFKOnly {
			return false, nil
		}
	}
	return true, nil
}

func repairFKCascadeTable(ctx context.Context, db DBConn, table string) error {
	res, err := db.ExecContext(ctx, fkCascadeRepairDeletes[table])
	if err != nil {
		return fmt.Errorf("cascade-repair %s: %w", table, err)
	}
	n, _ := res.RowsAffected()
	// table is from the fixed fkCascadeRepairDeletes allowlist, never user input.
	//nolint:gosec // G201/G202: hardcoded table name.
	if _, err := db.ExecContext(ctx, "DELETE FROM dolt_constraint_violations_"+table); err != nil {
		return fmt.Errorf("clear %s constraint violations: %w", table, err)
	}
	//nolint:gosec // G202: hardcoded table name.
	if _, err := db.ExecContext(ctx, "CALL DOLT_ADD('"+table+"')"); err != nil {
		return fmt.Errorf("stage repaired %s: %w", table, err)
	}
	fmt.Fprintf(os.Stderr,
		"Notice: pull merged %s row(s) referencing issue(s) deleted on another clone; applied the foreign key's cascade delete (%d row(s) removed)\n",
		table, n)
	return nil
}

// constraintViolationTables lists the tables with outstanding constraint
// violations in the working set.
func constraintViolationTables(ctx context.Context, db DBConn) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT `table` FROM dolt_constraint_violations WHERE num_violations > 0")
	if err != nil {
		return nil, fmt.Errorf("query constraint violations: %w", err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("scan constraint violation: %w", err)
		}
		tables = append(tables, t)
	}
	return tables, rows.Err()
}

// violationsAreIssueFKOnly reports whether every constraint violation recorded
// for table is a foreign-key violation referencing issues — the only class the
// cascade repair understands. violation_info is Dolt's JSON descriptor; its
// ReferencedTable names the FK's parent.
func violationsAreIssueFKOnly(ctx context.Context, db DBConn, table string) (bool, error) {
	// table is from the fixed fkCascadeRepairDeletes allowlist, never user input.
	//nolint:gosec // G202: hardcoded table name.
	rows, err := db.QueryContext(ctx,
		"SELECT violation_type, violation_info FROM dolt_constraint_violations_"+table)
	if err != nil {
		return false, fmt.Errorf("query %s constraint violations: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var vtype string
		var vinfo any
		if err := rows.Scan(&vtype, &vinfo); err != nil {
			return false, fmt.Errorf("scan %s constraint violation: %w", table, err)
		}
		if !violationReferencesIssues(vtype, vinfo) {
			return false, nil
		}
	}
	return true, rows.Err()
}

func violationReferencesIssues(violationType string, violationInfo any) bool {
	if violationType != "foreign key" {
		return false
	}
	// Server mode returns violation_info as JSON text; the embedded engine hands
	// back the driver's native value (e.g. merge.FkCVMeta), which marshals to the
	// same JSON.
	infoJSON, ok := violationInfoJSON(violationInfo)
	if !ok {
		return false // unknown descriptor shape — operator decides
	}
	var info struct {
		ReferencedTable string `json:"ReferencedTable"`
	}
	if err := json.Unmarshal(infoJSON, &info); err != nil {
		return false // unknown descriptor shape — operator decides
	}
	return info.ReferencedTable == "issues"
}

func violationInfoJSON(violationInfo any) ([]byte, bool) {
	switch value := violationInfo.(type) {
	case []byte:
		return value, true
	case string:
		return []byte(value), true
	default:
		valueJSON, err := json.Marshal(value)
		if err != nil {
			return nil, false
		}
		return valueJSON, true
	}
}
