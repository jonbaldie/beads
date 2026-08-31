package schema

import (
	"context"
	"fmt"
	"strings"
)

// MigrateUpTo applies main-source migrations up to and including maxVersion,
// without the dirty-table guards, backfills, rekeys, or ignored-source pass
// that MigrateUp layers on. It exists so cross-upgrade-boundary tests
// (bd-6dnrw.16) can reconstruct the schema as it stood at a historical
// release and use it as a Dolt merge ancestor. Production code must use
// MigrateUp: stopping short of the latest version on a real database leaves
// it half-upgraded by design.
func MigrateUpTo(ctx context.Context, db DBConn, maxVersion int) (int, error) {
	applied, _, err := mainSource.migrate(ctx, db, maxVersion)
	return applied, err
}

func MigrateUp(ctx context.Context, db DBConn) (int, error) {
	prep, needed, err := prepareMigrateUp(ctx, db)
	if err != nil {
		return 0, err
	}
	if !needed {
		return 0, nil
	}
	result, err := runMigrateUpPass(ctx, db, prep)
	if err != nil {
		return result.applied, err
	}
	return finalizeMigrateUp(ctx, db, prep, result)

}

type migrateUpPreparation struct {
	dirtyBeforeAll        map[string]dirtyTableState
	dirtyBefore           map[string]dirtyTableState
	dirtyBeforeSignatures map[string]string
	mainVersionBefore     int
}

type migrateUpResult struct {
	applied            int
	changed            bool
	appliedIgnored     int
	mainColumnAdded    bool
	ignoredColumnAdded bool
}

func prepareMigrateUp(ctx context.Context, db DBConn) (migrateUpPreparation, bool, error) {
	seedChanged, needed, err := prepareMigrateUpNeed(ctx, db)
	if err != nil {
		return migrateUpPreparation{}, false, err
	}
	if !needed {
		return migrateUpPreparation{}, false, nil
	}

	dirtyBeforeAll, dirtyBefore, err := prepareMigrateUpDirtyTables(ctx, db, seedChanged)
	if err != nil {
		return migrateUpPreparation{}, false, err
	}
	dirtyBeforeSignatures, err := dirtyTableSignatures(ctx, db, dirtyBefore)
	if err != nil {
		return migrateUpPreparation{}, false, fmt.Errorf("reading pre-migration dirty table diffs: %w", err)
	}
	mainVersionBefore, err := mainSource.currentVersion(ctx, db)
	if err != nil {
		return migrateUpPreparation{}, false, fmt.Errorf("reading pre-migration schema version: %w", err)
	}

	return migrateUpPreparation{
		dirtyBeforeAll:        dirtyBeforeAll,
		dirtyBefore:           dirtyBefore,
		dirtyBeforeSignatures: dirtyBeforeSignatures,
		mainVersionBefore:     mainVersionBefore,
	}, true, nil
}

func prepareMigrateUpNeed(ctx context.Context, db DBConn) (bool, bool, error) {
	seedChanged, err := seedDoltIgnorePatterns(ctx, db)
	if err != nil {
		return false, false, err
	}
	needed, err := migrationWorkNeeded(ctx, db)
	if err != nil {
		return false, false, fmt.Errorf("checking schema migration work: %w", err)
	}
	if !needed {
		if seedChanged {
			if err := commitSeededDoltIgnore(ctx, db); err != nil {
				return false, false, err
			}
		}
		return false, false, nil
	}
	return seedChanged, true, nil
}

func prepareMigrateUpDirtyTables(ctx context.Context, db DBConn, seedChanged bool) (map[string]dirtyTableState, map[string]dirtyTableState, error) {
	dirtyBeforeAll, err := captureMigrateUpDirtyTables(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	if seedChanged {
		if err := commitSeededDoltIgnore(ctx, db); err != nil {
			return nil, nil, err
		}
	}

	dirtyBefore, err := captureCommittableMigrateUpDirtyTables(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	if err := exemptResumingMigrateUpTables(ctx, db, dirtyBefore); err != nil {
		return nil, nil, err
	}
	if err := checkMigrateUpDirtyTables(ctx, db, dirtyBefore); err != nil {
		return nil, nil, err
	}
	return dirtyBeforeAll, dirtyBefore, nil
}

func captureMigrateUpDirtyTables(ctx context.Context, db DBConn) (map[string]dirtyTableState, error) {
	tables, err := dirtyTables(ctx, db, false)
	if err != nil {
		return nil, fmt.Errorf("reading pre-migration status: %w", err)
	}
	delete(tables, "dolt_ignore")
	if err := unstagePreExistingTables(ctx, db, tables); err != nil {
		return nil, fmt.Errorf("unstaging pre-migration tables: %w", err)
	}
	return tables, nil
}

func captureCommittableMigrateUpDirtyTables(ctx context.Context, db DBConn) (map[string]dirtyTableState, error) {
	tables, err := committableDirtyTables(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("reading pre-migration status: %w", err)
	}
	delete(tables, "dolt_ignore")
	return tables, nil
}

func exemptResumingMigrateUpTables(ctx context.Context, db DBConn, tables map[string]dirtyTableState) error {
	resuming, err := anyAuxRekeyResumePending(ctx, db)
	if err != nil {
		return fmt.Errorf("reading aux rekey sentinel: %w", err)
	}
	if !resuming {
		return nil
	}
	for _, t := range auxRekeyTables {
		delete(tables, t.name)
	}
	return nil
}

func checkMigrateUpDirtyTables(ctx context.Context, db DBConn, dirtyBefore map[string]dirtyTableState) error {
	touchedDirtyTables, err := mainSource.pendingMigrationDirtyTables(ctx, db, dirtyBefore)
	if err != nil {
		return fmt.Errorf("checking dirty tables against pending migrations: %w", err)
	}
	if len(touchedDirtyTables) > 0 {
		return &DirtyTablesError{Tables: touchedDirtyTables}
	}
	return nil
}

func runMigrateUpPass(ctx context.Context, db DBConn, prep migrateUpPreparation) (migrateUpResult, error) {
	applied, mainColumnAdded, err := mainSource.migrate(ctx, db, 0)
	result := migrateUpResult{applied: applied, mainColumnAdded: mainColumnAdded}
	if err != nil {
		return result, err
	}

	changed, err := runMigrateUpRepairs(ctx, db, prep.mainVersionBefore)
	if err != nil {
		return result, err
	}
	result.changed = changed

	ignored, err := runIgnoredMigrateUp(ctx, db, prep.dirtyBeforeAll)
	if err != nil {
		return result, err
	}
	result.appliedIgnored = ignored.applied
	result.ignoredColumnAdded = ignored.columnAdded
	return result, nil
}

func runMigrateUpRepairs(ctx context.Context, db DBConn, mainVersionBefore int) (bool, error) {
	backfilled, err := ensureBackfilledCustomStatusesCustomTypes(ctx, db)
	if err != nil {
		return false, fmt.Errorf("backfill custom tables: %w", err)
	}

	rekeyed, err := rekeyDependencyIDs(ctx, db)
	if err != nil {
		return false, fmt.Errorf("rekey dependency ids: %w", err)
	}
	backfilled = backfilled || rekeyed

	auxRekeyed, err := rekeyAuxRowIDsAllPasses(ctx, db, mainVersionBefore)
	if err != nil {
		return false, fmt.Errorf("rekey aux row ids: %w", err)
	}
	return backfilled || auxRekeyed, nil
}

type ignoredMigrateUpResult struct {
	applied     int
	columnAdded bool
}

func runIgnoredMigrateUp(ctx context.Context, db DBConn, dirtyBeforeAll map[string]dirtyTableState) (ignoredMigrateUpResult, error) {
	touchedDirtyTables, err := ignoredSource.pendingMigrationDirtyTables(ctx, db, dirtyBeforeAll)
	if err != nil {
		return ignoredMigrateUpResult{}, fmt.Errorf("checking dirty tables against pending ignored migrations: %w", err)
	}
	if len(touchedDirtyTables) > 0 {
		return ignoredMigrateUpResult{}, fmt.Errorf("pending ignored schema migrations alter pre-existing dirty tables: %s", strings.Join(touchedDirtyTables, ", "))
	}

	applied, columnAdded, err := ignoredSource.migrate(ctx, db, 0)
	if err != nil {
		return ignoredMigrateUpResult{}, fmt.Errorf("ignored migrations: %w", err)
	}
	if err := unstageIgnoredTables(ctx, db); err != nil {
		return ignoredMigrateUpResult{}, fmt.Errorf("unstaging ignored migration tables: %w", err)
	}
	return ignoredMigrateUpResult{applied: applied, columnAdded: columnAdded}, nil
}

func finalizeMigrateUp(ctx context.Context, db DBConn, prep migrateUpPreparation, result migrateUpResult) (int, error) {
	if result.applied == 0 && !result.changed && result.appliedIgnored == 0 && !result.mainColumnAdded && !result.ignoredColumnAdded {
		return result.applied, nil
	}
	if err := verifyMigrateUpDirtyTables(ctx, db, prep.dirtyBeforeSignatures); err != nil {
		return result.applied, err
	}
	if err := commitMigrateUp(ctx, db, prep.dirtyBefore); err != nil {
		return result.applied, err
	}
	return result.applied, nil
}

func verifyMigrateUpDirtyTables(ctx context.Context, db DBConn, before map[string]string) error {
	changedDirtyTables, err := changedDirtyTableSignatures(ctx, db, before)
	if err != nil {
		return fmt.Errorf("checking pre-existing dirty table diffs: %w", err)
	}
	if len(changedDirtyTables) > 0 {
		return fmt.Errorf("pre-existing dirty tables changed during schema migration: %s", strings.Join(changedDirtyTables, ", "))
	}
	return nil
}

func commitMigrateUp(ctx context.Context, db DBConn, dirtyBefore map[string]dirtyTableState) error {
	staged, err := stageSchemaTables(ctx, db, dirtyBefore)
	if err != nil {
		return fmt.Errorf("staging migrations: %w", err)
	}
	if !staged {
		return nil
	}
	if err := DrainCall(ctx, db, "CALL DOLT_COMMIT('-m', 'schema: apply migrations')"); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "nothing to commit") {
			return fmt.Errorf("committing migrations: %w", err)
		}
	}
	return nil
}
