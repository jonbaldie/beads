package schema

import (
	"context"
	"fmt"
)

// Pre-migration repairs run immediately before a specific pending migration
// file is applied. Shipped migration files are frozen (see
// scripts/check-migration-hygiene.sh): editing one forks fresh clones from
// upgraded clones via the recorded content hash, and a bug that makes a
// migration FAIL on drifted databases cannot be fixed forward with a new
// migration either — the failing file aborts the pass before any later
// version runs. Repairing the drift in code, keyed to the pending version,
// is the only path that heals affected databases without touching shipped
// SQL. Precedent: ensureContentHashColumn, the aux row-id backfill.

// repairKey identifies a pre-migration repair by its migration source cursor
// table and the pending version it runs before.
type repairKey struct {
	cursorTable string
	version     int
}

// preMigrationRepairs is the registry of repairs that must run immediately
// before a specific migration file is applied. It is keyed rather than a switch
// so TestFrozenNonIdempotentMigrationsHaveRepairs can assert that every shipped
// migration whose frozen body cannot replay on its own (0040/0041, which write
// dolt_nonlocal_tables and self-commit) has a repair registered here.
var preMigrationRepairs = map[repairKey]func(context.Context, DBConn) error{
	{cursorTable: "schema_migrations", version: 40}: repairPartial0040NonlocalInsert,
	{cursorTable: "schema_migrations", version: 41}: repairPartial0041NonlocalDelete,
	{cursorTable: "schema_migrations", version: 47}: ensureWispTablesForMixedBlockedRecompute,
	{cursorTable: "schema_migrations", version: 53}: repairV53RigAndSplitTargets,
	{cursorTable: "schema_migrations", version: 58}: repairWispDependenciesForwardShape,
}

// preMigrationRepair dispatches any repair registered for (source, version).
func (m migrationSource) preMigrationRepair(ctx context.Context, db DBConn, version int) error {
	if repair, ok := preMigrationRepairs[repairKey{cursorTable: m.cursorTable, version: version}]; ok {
		return repair(ctx, db)
	}
	return nil
}

// repairV53RigAndSplitTargets is the pre-0053 repair: it backfills the issues
// rig/agent columns (#4502), the wisp_dependencies split-target columns
// (#4555), and the dependencies id column that 0053 reads.
func repairV53RigAndSplitTargets(ctx context.Context, db DBConn) error {
	if err := ensureIssuesRigColumns(ctx, db); err != nil {
		return err
	}
	if err := ensureWispDependenciesSplitTargets(ctx, db); err != nil {
		return err
	}
	return ensureDependenciesIDColumn(ctx, db)
}

// The four dolt_nonlocal_tables rows migration 0040 inserts (and migration 0041
// clears). Kept as a single source of truth for the version-40/41 replay
// repairs, which normalise the table to the exact state each shipped
// (content-hashed, un-editable) migration body expects before it runs.
const nonlocalTablesName = "dolt_nonlocal_tables"
const nonlocalFrozenRowsInList = "('wisps', 'wisp_*', 'repo_mtimes', 'local_metadata')"
const nonlocalFrozenRowsValues = "('wisps', 'main', 'immediate'), ('wisp_*', 'main', 'immediate'), " +
	"('repo_mtimes', 'main', 'immediate'), ('local_metadata', 'main', 'immediate')"

// commitNonlocalRepair commits a version-40/41 repair's edit to
// dolt_nonlocal_tables, staging that table BY NAME rather than with
// DOLT_COMMIT('-Am', ...). The scoping is load-bearing: on the bounded-migrate
// path (upTo != 0, where per-step commit is off) migrations 1..upTo sit
// uncommitted in the working set while the repair runs, and an "add all"
// commit here would sweep all of them into a repair-labeled commit. Staging
// only the table the repair touched leaves the rest of the working set exactly
// as the pass expects to find it. --skip-empty keeps the commit a clean no-op
// if the edit staged nothing.
func commitNonlocalRepair(ctx context.Context, db DBConn, message string) error {
	if err := DrainCall(ctx, db, "CALL DOLT_ADD(?)", nonlocalTablesName); err != nil {
		return fmt.Errorf("staging %s: %w", nonlocalTablesName, err)
	}
	return DrainCall(ctx, db, "CALL DOLT_COMMIT('-m', ?, '--skip-empty')", message)
}

// anyNonlocalFrozenRowPresent reports whether any of 0040's four
// dolt_nonlocal_tables rows currently exists (in the working set), the signal
// the version-40/41 repairs use to decide whether a heal is needed. Guarding on
// it keeps both repairs a strict no-op on the common (non-partial) path, so a
// fresh init reaches 0040/0041 having done no repair work at all — the repairs
// only ever touch a database that actually took the partial-apply brick.
func anyNonlocalFrozenRowPresent(ctx context.Context, db DBConn) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM dolt_nonlocal_tables WHERE table_name IN "+nonlocalFrozenRowsInList).Scan(&count); err != nil {
		return false, fmt.Errorf("counting nonlocal frozen rows: %w", err)
	}
	return count > 0, nil
}

// repairPartial0040NonlocalInsert heals a partially-applied migration 0040 so
// its shipped body can replay. 0040 bare-INSERTs four dolt_nonlocal_tables rows,
// each paired with its own CALL DOLT_COMMIT. Over a shared sql-server a transient
// ("busy buffer" -> "bad connection") can leave some rows committed while the
// schema_migrations version row never records, so the init retry loop re-runs
// 0040 from the top and the bare INSERT dies on "duplicate primary key given:
// [wisps]", bricking the database. 0040 is a shipped, content-hashed migration
// and cannot be edited (see the file header), so instead clear any of those four
// rows before the replay and commit the removal, leaving 0040's INSERT+COMMIT
// pairs a clean, real diff. No-op when 0040 never partially applied.
func repairPartial0040NonlocalInsert(ctx context.Context, db DBConn) error {
	present, err := anyNonlocalFrozenRowPresent(ctx, db)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		"DELETE FROM dolt_nonlocal_tables WHERE table_name IN "+nonlocalFrozenRowsInList); err != nil {
		return fmt.Errorf("clearing partial 0040 nonlocal rows: %w", err)
	}
	if err := commitNonlocalRepair(ctx, db,
		"repair: clear partial 0040 nonlocal rows before replay"); err != nil {
		return fmt.Errorf("committing 0040 nonlocal repair: %w", err)
	}
	return nil
}

// repairPartial0041NonlocalDelete heals a partially-applied migration 0041 so
// its shipped body can replay. 0041 begins by clearing dolt_nonlocal_tables and
// committing ("disable nonlocal tables for fk migrations"). If a transient
// interrupts 0041 after that commit but before its version row records, the
// retry re-runs 0041 against an already-empty table: the DELETE stages nothing
// and the paired DOLT_COMMIT (no --skip-empty in the shipped bytes) fails with
// "nothing to commit". Restore the pre-0041 invariant — the four rows 0040
// leaves — so the frozen DELETE+COMMIT has a real diff again. No-op when those
// rows are already present (the common path).
func repairPartial0041NonlocalDelete(ctx context.Context, db DBConn) error {
	present, err := anyNonlocalFrozenRowPresent(ctx, db)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		"INSERT IGNORE INTO dolt_nonlocal_tables (table_name, target_ref, options) VALUES "+nonlocalFrozenRowsValues); err != nil {
		return fmt.Errorf("restoring pre-0041 nonlocal rows: %w", err)
	}
	if err := commitNonlocalRepair(ctx, db,
		"repair: restore pre-0041 nonlocal rows before replay"); err != nil {
		return fmt.Errorf("committing 0041 nonlocal repair: %w", err)
	}
	return nil
}
