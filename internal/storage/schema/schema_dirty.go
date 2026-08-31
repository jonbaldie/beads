package schema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"strings"
)

type dirtyTableState struct {
	staged bool
}

func migrationWorkNeeded(ctx context.Context, db DBConn) (bool, error) {
	if !mainSource.atLatest(ctx, db) || !ignoredSource.atLatest(ctx, db) {
		return true, nil
	}
	// A database already at the latest numbered migration still needs work if it
	// predates the content_hash column (gastownhall/beads#4259 reporter fix No.2).
	// Without this, MigrateUp short-circuits before migrate() runs the idempotent
	// ALTER, so the recording/detection surface is never installed on exactly the
	// already-upgraded databases the fix is meant to protect.
	hasMainHash, err := mainSource.hasContentHashColumn(ctx, db)
	if err != nil {
		return false, err
	}
	if !hasMainHash {
		return true, nil
	}
	hasIgnoredHash, err := ignoredSource.hasContentHashColumn(ctx, db)
	if err != nil {
		return false, err
	}
	if !hasIgnoredHash {
		return true, nil
	}
	return needsBackfilledCustomStatusesCustomTypes(ctx, db)
}

func committableDirtyTables(ctx context.Context, db DBConn) (map[string]dirtyTableState, error) {
	tables, err := dirtyTables(ctx, db, true)
	if err != nil {
		return nil, err
	}
	delete(tables, mainSource.cursorTable)
	delete(tables, ignoredSource.cursorTable)
	return tables, nil
}

func stagedDirtyTables(tables map[string]dirtyTableState) []string {
	var staged []string
	for table, state := range tables {
		if state.staged {
			staged = append(staged, table)
		}
	}
	sort.Strings(staged)
	return staged
}

func unstagePreExistingTables(ctx context.Context, db DBConn, tables map[string]dirtyTableState) error {
	staged := stagedDirtyTables(tables)
	if len(staged) > 0 {
		log.Printf("schema migration unstaging pre-existing staged tables: %s", strings.Join(staged, ", "))
	}
	for _, table := range staged {
		if err := DrainCall(ctx, db, "CALL DOLT_RESET(?)", table); err != nil {
			return fmt.Errorf("dolt reset %s: %w", table, err)
		}
	}
	return nil
}

func unstageIgnoredTables(ctx context.Context, db DBConn) error {
	tables, err := existingIgnoredTables(ctx, db)
	if err != nil {
		return err
	}
	return unstagePreExistingTables(ctx, db, tables)
}

func dirtyTableSignatures(ctx context.Context, db DBConn, tables map[string]dirtyTableState) (map[string]string, error) {
	signatures := make(map[string]string, len(tables))
	names := sortedDirtyTableNames(tables)
	for _, table := range names {
		signature, err := dirtyTableSignature(ctx, db, table)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", table, err)
		}
		signatures[table] = signature
	}
	return signatures, nil
}

func changedDirtyTableSignatures(ctx context.Context, db DBConn, before map[string]string) ([]string, error) {
	var changed []string
	names := sortedSignatureTableNames(before)
	for _, table := range names {
		signature, err := dirtyTableSignature(ctx, db, table)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", table, err)
		}
		if signature != before[table] {
			changed = append(changed, table)
		}
	}
	return changed, nil
}

func sortedDirtyTableNames(tables map[string]dirtyTableState) []string {
	names := make([]string, 0, len(tables))
	for table := range tables {
		names = append(names, table)
	}
	sort.Strings(names)
	return names
}

func sortedSignatureTableNames(signatures map[string]string) []string {
	names := make([]string, 0, len(signatures))
	for table := range signatures {
		names = append(names, table)
	}
	sort.Strings(names)
	return names
}

func dirtyTableSignature(ctx context.Context, db DBConn, table string) (string, error) {
	if !doltStatusTableNameRE.MatchString(table) {
		return "", fmt.Errorf("unsafe dolt status table name %q", table)
	}
	rowSignatures, err := readDirtyTableSignatures(ctx, db, table)
	if err != nil {
		return "", err
	}

	h := sha256.New()
	for _, row := range rowSignatures {
		_, _ = h.Write([]byte(row))
		_, _ = h.Write([]byte{0xff})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func readDirtyTableSignatures(ctx context.Context, db DBConn, table string) ([]string, error) {
	//nolint:gosec // table comes from dolt_status; dolt_diff requires a literal table argument.
	rows, err := db.QueryContext(ctx, "SELECT * FROM dolt_diff('HEAD', 'WORKING', "+sqlStringLiteral(table)+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var rowSignatures []string
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		rowSignatures = append(rowSignatures, dirtyTableRowSignature(columns, values))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(rowSignatures)
	return rowSignatures, nil
}

func dirtyTableRowSignature(columns []string, values []any) string {
	var b strings.Builder
	for i, column := range columns {
		if isDiffMetadataColumn(column) {
			continue
		}
		b.WriteString(column)
		b.WriteByte('=')
		writeSignatureValue(&b, values[i])
		b.WriteByte(0)
	}
	return b.String()
}

func isDiffMetadataColumn(column string) bool {
	switch strings.ToLower(column) {
	case "from_commit", "to_commit", "from_commit_date", "to_commit_date":
		return true
	default:
		return false
	}
}

func sqlStringLiteral(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `''`)
	return "'" + s + "'"
}

func writeSignatureValue(b *strings.Builder, v any) {
	switch typed := v.(type) {
	case nil:
		b.WriteString("<nil>")
	case []byte:
		b.Write(typed)
	default:
		b.WriteString(fmt.Sprintf("%v", typed))
	}
}

func stageSchemaTables(ctx context.Context, db DBConn, dirtyBefore map[string]dirtyTableState) (bool, error) {
	dirtyAfter, err := dirtyTables(ctx, db, true)
	if err != nil {
		return false, err
	}
	tablesAfter, err := existingCommittableTables(ctx, db)
	if err != nil {
		return false, err
	}
	tables := stageableSchemaTables(dirtyBefore, dirtyAfter, tablesAfter)

	for _, table := range tables {
		if err := DrainCall(ctx, db, "CALL DOLT_ADD('-f', ?)", table); err != nil {
			return false, fmt.Errorf("dolt add %s: %w", table, err)
		}
	}
	return len(tables) > 0, nil
}

func stageableSchemaTables(dirtyBefore map[string]dirtyTableState, dirtyAfter map[string]dirtyTableState, tablesAfter map[string]struct{}) []string {
	tableSet := make(map[string]struct{})
	for table := range dirtyAfter {
		addStageableSchemaTable(tableSet, dirtyBefore, table)
	}
	for table := range tablesAfter {
		addStageableSchemaTable(tableSet, dirtyBefore, table)
	}

	tables := make([]string, 0, len(tableSet))
	for table := range tableSet {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	return tables
}

func addStageableSchemaTable(tableSet map[string]struct{}, dirtyBefore map[string]dirtyTableState, table string) {
	if _, wasDirty := dirtyBefore[table]; wasDirty {
		return
	}
	tableSet[table] = struct{}{}
}

func existingCommittableTables(ctx context.Context, db DBConn) (map[string]struct{}, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.TABLE_NAME
		FROM INFORMATION_SCHEMA.TABLES t
		WHERE t.TABLE_SCHEMA = DATABASE()
		  AND t.TABLE_TYPE = 'BASE TABLE'
		  AND NOT EXISTS (
			SELECT 1 FROM dolt_ignore di
			WHERE di.ignored = 1
			  AND t.TABLE_NAME LIKE di.pattern
		  )
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tables := make(map[string]struct{})
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, err
		}
		tables[table] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tables, nil
}

func existingIgnoredTables(ctx context.Context, db DBConn) (map[string]dirtyTableState, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.table_name, s.staged
		FROM dolt_status s
		WHERE EXISTS (
			SELECT 1 FROM dolt_ignore di
			WHERE di.ignored = 1
			  AND s.table_name LIKE di.pattern
		)
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tables := make(map[string]dirtyTableState)
	for rows.Next() {
		var table string
		var staged bool
		if err := rows.Scan(&table, &staged); err != nil {
			return nil, err
		}
		tables[table] = dirtyTableState{staged: staged}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tables, nil
}

func dirtyTables(ctx context.Context, db DBConn, excludeIgnored bool) (map[string]dirtyTableState, error) {
	query := `
		SELECT s.table_name, s.staged
		FROM dolt_status s
	`
	if excludeIgnored {
		query += `
		WHERE NOT EXISTS (
			SELECT 1 FROM dolt_ignore di
			WHERE di.ignored = 1
			AND s.table_name LIKE di.pattern
		)
		`
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tables := make(map[string]dirtyTableState)
	for rows.Next() {
		var table string
		var staged bool
		if err := rows.Scan(&table, &staged); err != nil {
			return nil, err
		}
		state := tables[table]
		state.staged = state.staged || staged
		tables[table] = state
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tables, nil
}
