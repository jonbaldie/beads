package schema

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonbaldie/beads/internal/storage/dberrors"
)

type migrationFile struct {
	version int
	name    string
}

func (m migrationSource) bootstrapSQL() string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	version INT PRIMARY KEY,
	applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	content_hash CHAR(64)
)`, m.cursorTable)
}

// hasContentHashColumn reports whether the cursor table already carries the
// content_hash column. A not-yet-created table simply reports false.
//
// It probes a single table with SHOW COLUMNS rather than INFORMATION_SCHEMA.COLUMNS,
// whose predicate Dolt does not push down. The LIKE narrows the result set, but
// we still compare the Field name exactly because '_' is a LIKE single-character
// wildcard.
func (m migrationSource) hasContentHashColumn(ctx context.Context, db DBConn) (bool, error) {
	//nolint:gosec // G201: m.cursorTable is a hardcoded constant; the LIKE literal is fixed.
	rows, err := db.QueryContext(ctx, "SHOW COLUMNS FROM "+m.cursorTable+" LIKE 'content_hash'")
	if err != nil {
		// SHOW COLUMNS errors on a missing table; the old INFORMATION_SCHEMA
		// probe returned count 0 instead. Preserve that: an absent cursor table
		// has no content_hash column.
		if dberrors.IsTableNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("checking %s.content_hash: %w", m.cursorTable, err)
	}
	defer func() { _ = rows.Close() }()

	has, err := scanContentHashColumn(rows)
	if err != nil {
		return false, fmt.Errorf("checking %s.content_hash: %w", m.cursorTable, err)
	}
	return has, nil
}

func scanContentHashColumn(rows *sql.Rows) (bool, error) {
	cols, err := rows.Columns()
	if err != nil {
		return false, err
	}
	// SHOW COLUMNS returns Field, Type, Null, Key, Default, Extra (and possibly
	// more on some servers); scan every column into RawBytes and read the first
	// ("Field"), which is the column name.
	cells := make([]sql.RawBytes, len(cols))
	dest := make([]any, len(cols))
	for i := range cells {
		dest[i] = &cells[i]
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return false, err
		}
		if len(cells) > 0 && string(cells[0]) == "content_hash" {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

// ensureContentHashColumn adds the content_hash column to an existing cursor
// table that predates it (gastownhall/beads#4259 reporter fix No.2: record a
// per-migration content hash so two clones at the same MAX(version) but with
// divergent migration content are detectable). Fresh tables already have it via
// bootstrapSQL; this idempotently upgrades older databases without a numbered
// migration. Already-applied rows keep a NULL hash — their migration content is
// not re-read. It reports whether it actually added the column, so MigrateUp can
// treat that ALTER as committable schema work even when no numbered migration or
// backfill ran.
func (m migrationSource) ensureContentHashColumn(ctx context.Context, db DBConn) (bool, error) {
	has, err := m.hasContentHashColumn(ctx, db)
	if err != nil {
		return false, err
	}
	if has {
		return false, nil
	}
	//nolint:gosec // G201: m.cursorTable is a hardcoded constant.
	if _, err := db.ExecContext(ctx, "ALTER TABLE "+m.cursorTable+" ADD COLUMN content_hash CHAR(64)"); err != nil {
		return false, fmt.Errorf("adding %s.content_hash: %w", m.cursorTable, err)
	}
	return true, nil
}

func checkNoDuplicateVersions(files []migrationFile) {
	seen := make(map[int]string, len(files))
	for _, m := range files {
		if prior, ok := seen[m.version]; ok {
			panic(fmt.Sprintf(
				"schema: duplicate migration version %d: %q and %q — renumber one before commit",
				m.version, prior, m.name,
			))
		}
		seen[m.version] = m.name
	}
}

func (m migrationSource) list() []migrationFile {
	entries, err := fs.ReadDir(m.files, m.dir)
	if err != nil {
		panic(fmt.Sprintf("schema: failed to read embedded %s: %v", m.dir, err))
	}
	var files []migrationFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		v, err := parseVersion(e.Name())
		if err != nil {
			panic(fmt.Sprintf("schema: invalid migration filename %q: %v", e.Name(), err))
		}
		files = append(files, migrationFile{version: v, name: e.Name()})
	}
	checkNoDuplicateVersions(files)
	sort.Slice(files, func(i, j int) bool { return files[i].version < files[j].version })
	return files
}

func (m migrationSource) latest() int {
	files := m.list()
	if len(files) == 0 {
		return 0
	}
	return files[len(files)-1].version
}

func (m migrationSource) atLatest(ctx context.Context, db DBConn) bool {
	current, err := m.currentVersion(ctx, db)
	if err != nil {
		return false
	}
	return current >= m.latest()
}

func (m migrationSource) currentVersion(ctx context.Context, db DBConn) (int, error) {
	current, err := readMigrationCursorVersion(ctx, db, m.cursorTable)
	if err != nil {
		return 0, err
	}
	if current == 0 {
		return 0, nil
	}
	// A missing cursor TABLE already meant "nothing applied". A cursor whose
	// tables are absent means the same thing and was previously believed
	// (gh 5033, gh 4356): ignored_schema_migrations is itself dolt-ignored and
	// clone-local, so a database materialized out of band — table-by-table
	// copy, dump restore, or a clone that picked up the cursor rows without
	// the clone-local tables they describe — arrives claiming at-latest with
	// no wisps tables. atLatest() then short-circuits migrationWorkNeeded()
	// and the series never re-runs, surfacing much later and much further away
	// as "table not found: wisp_dependencies" on `bd close`.
	contradicted, cerr := m.cursorContradictedBySchema(ctx, db)
	if cerr != nil {
		return 0, cerr
	}
	if contradicted {
		return 0, nil
	}
	return current, nil
}

func readMigrationCursorVersion(ctx context.Context, db DBConn, cursorTable string) (int, error) {
	var current int
	err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM "+cursorTable).Scan(&current)
	if err != nil && err != sql.ErrNoRows {
		if dberrors.IsTableNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading %s version: %w", cursorTable, err)
	}
	return current, nil
}

// cursorContradictedBySchema reports whether this series' cursor claims work
// that the schema does not corroborate.
//
// Returning "cursor is 0" rather than an error is deliberate: the series is
// written to be re-runnable against a database that already has some of it.
// migrations/ignored/0001 builds each table as __temp__<name> and then
// `RENAME TABLE __temp__x TO x` only when x does not already exist, DROPping
// the temp otherwise; later migrations gate their ALTERs on
// INFORMATION_SCHEMA lookups. So re-running the series repairs the missing
// tables and leaves existing data untouched — which is why this can heal
// rather than merely diagnose.
func (m migrationSource) cursorContradictedBySchema(ctx context.Context, db DBConn) (bool, error) {
	for _, table := range m.sentinelTables {
		present, err := sentinelTableExists(ctx, db, table)
		if err != nil {
			return false, fmt.Errorf("checking %s sentinel table %s: %w", m.cursorTable, table, err)
		}
		if !present {
			return true, nil
		}
	}
	return false, nil
}

// sentinelTableExists is a function variable for the same reason
// issueRowCounter is: it lets the cursor-reality tests exercise the real
// decision without a live database.
var sentinelTableExists = func(ctx context.Context, db DBConn, table string) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`,
		table).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func (m migrationSource) pendingVersions(ctx context.Context, db DBConn) ([]int, error) {
	current, err := m.currentVersion(ctx, db)
	if err != nil {
		return nil, err
	}
	files := m.list()
	pending := make([]int, 0, len(files))
	for _, mf := range files {
		if mf.version > current {
			pending = append(pending, mf.version)
		}
	}
	return pending, nil
}

func (m migrationSource) pendingMigrationDirtyTables(ctx context.Context, db DBConn, dirtyBefore map[string]dirtyTableState) ([]string, error) {
	if len(dirtyBefore) == 0 {
		return nil, nil
	}
	current, err := m.currentVersion(ctx, db)
	if err != nil {
		return nil, err
	}

	dirtyNames := sortedDirtyTableNames(dirtyBefore)
	touched := make(map[string]struct{})
	for _, mf := range m.list() {
		if mf.version <= current {
			continue
		}
		data, err := m.files.ReadFile(m.dir + "/" + mf.name)
		if err != nil {
			return nil, fmt.Errorf("reading migration %s: %w", mf.name, err)
		}
		sqlText := string(data)
		for _, table := range dirtyNames {
			if migrationSQLTouchesTable(sqlText, table) {
				touched[table] = struct{}{}
			}
		}
	}

	names := make([]string, 0, len(touched))
	for table := range touched {
		names = append(names, table)
	}
	sort.Strings(names)
	return names, nil
}

func migrationSQLTouchesTable(sqlText, table string) bool {
	tableRef := "`?" + regexp.QuoteMeta(table) + "`?"
	// This intentionally scans raw migration text so PREPARE strings that run
	// DDL/DML are treated as real table touches.
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(?:alter\s+table|update|delete\s+from|insert(?:\s+ignore)?\s+into|replace\s+into|truncate\s+table|drop\s+table|create\s+table(?:\s+if\s+not\s+exists)?|rename\s+table)\s+` + tableRef + `\b`),
		regexp.MustCompile(`(?i)\brename\s+table\b[^;]*\bto\s+` + tableRef + `\b`),
		regexp.MustCompile(`(?i)\bcreate\s+(?:unique\s+)?index\b[^;]*\bon\s+` + tableRef + `\b`),
		regexp.MustCompile(`(?i)\b(?:create\s+(?:or\s+replace\s+)?view|alter\s+view)\s+` + tableRef + `\b`),
	}
	for _, pattern := range patterns {
		if pattern.MatchString(sqlText) {
			return true
		}
	}
	return false
}

// procedureCallRe matches a stored-procedure invocation (CALL ...) at a
// statement boundary, case-insensitively. It decides whether a migration body
// must have its result sets drained explicitly (see execMigrationBody).
var procedureCallRe = regexp.MustCompile(`(?i)(?:^|;|\n)\s*CALL\s`)

// DrainCall runs a statement (or multi-statement body) that invokes a Dolt
// stored procedure and fully consumes EVERY result set it returns, leaving the
// pinned connection clean for the next command.
//
// Why this matters is an ERROR-PATH asymmetry in go-sql-driver/mysql, not a
// happy-path gap: mysqlConn.exec (behind ExecContext) does end with
// handleOk.discardResults(), so a CALL that succeeds is drained already. But
// every failure before that line — readResultSetHeaderPacket, skipColumns,
// skipRows — returns early, leaving whatever the server still has queued
// unread on the wire. mysqlRows.Close() has no such exit: it skips unread rows
// and discards remaining result sets unconditionally, which is why routing
// through QueryContext + a deferred Close is drain-safe on both paths.
//
// That asymmetry is reachable here precisely because these call sites tolerate
// an error and keep using the same pinned connection: a multi-statement body
// like 0040 (four INSERT/CALL DOLT_COMMIT pairs) can fail on statement 4 with
// more results queued behind it, and MigrateUp and commitMigrationStep both
// swallow "nothing to commit" and carry on. The next command on that conn —
// the version-record INSERT, a later DOLT_ADD, or RELEASE_LOCK — then dies on
// the still-busy connection ("busy buffer" -> "driver: bad connection"). The
// same shape recurs, at far higher call volume, on every transaction-pinned
// *sql.Tx / *sql.Conn in internal/storage/dolt that runs a tolerate-and-continue
// CALL DOLT_ADD/DOLT_COMMIT/DOLT_MERGE/DOLT_BRANCH pair — unlike a pooled
// *sql.DB connection, a pinned connection can't be discarded by the pool on
// the next checkout, and database/sql does not reset a driver conn before
// handing it back to a future borrower, so an undrained buffer poisons
// whoever acquires that connection next.
//
// This is the necessary half of the fix, not the sufficient half: a
// load-induced transient (or a "nothing to commit" from a no-op commit) can
// still surface mid-migration, and the init retry loop then re-runs the whole
// migration. The frozen non-idempotent migrations (0040 bare-INSERTs its
// dolt_nonlocal_tables rows, 0041 DELETEs then commits them) are made
// replay-safe by pre-migration repairs keyed to their version, not by editing
// their shipped SQL — see preMigrationRepair and migration_repairs.go.
func DrainCall(ctx context.Context, db DBConn, query string, args ...any) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for {
		// Consume every row of the current result set. The values are
		// irrelevant — a CALL's effect comes from its side effects, not from
		// anything it returns — but reading them is what frees the connection
		// buffer for the next command.
		for rows.Next() { //nolint:revive // intentional drain, no body needed
		}
		if !rows.NextResultSet() {
			break
		}
	}
	return rows.Err()
}

// execMigrationBody applies one migration file's SQL on the pinned migration
// connection. Bodies that invoke a stored procedure (today 0040 and 0041, both
// CALL DOLT_COMMIT) are routed through DrainCall so their result sets are
// consumed; all other migrations keep the unchanged ExecContext path.
func execMigrationBody(ctx context.Context, db DBConn, sqlText string) error {
	if !procedureCallRe.MatchString(sqlText) {
		_, err := db.ExecContext(ctx, sqlText)
		return err
	}
	return DrainCall(ctx, db, sqlText)
}

// migrate brings the source up to its latest version and returns the number of
// numbered migrations applied plus whether it added the content_hash column to a
// pre-existing cursor table. The column signal lets MigrateUp stage and commit
// that ALTER as schema work even when no numbered migration was applied.
// migrate applies pending migrations from this source. upTo bounds the highest
// version applied; pass 0 for the latest (the production path — only the
// MigrateUpTo test-support path passes a real bound).
func (m migrationSource) migrate(ctx context.Context, db DBConn, upTo int) (int, bool, error) {
	if _, err := db.ExecContext(ctx, m.bootstrapSQL()); err != nil {
		return 0, false, fmt.Errorf("creating %s: %w", m.cursorTable, err)
	}
	columnAdded, err := m.ensureContentHashColumn(ctx, db)
	if err != nil {
		return 0, false, err
	}

	target := m.latest()
	if upTo > 0 && upTo < target {
		target = upTo
	}

	// The cursor is read through currentVersion, never raw: that is where the
	// cursor-reality check lives (gh 5033). A raw read here would believe the
	// contradicted cursor that migrationWorkNeeded just disbelieved — MigrateUp
	// would decide "work needed" on every open, run the whole pass, and then
	// apply nothing, leaving the missing tables missing and the pass to repeat
	// forever. The heal only happens if the applier disbelieves the cursor too.
	current, err := m.currentVersion(ctx, db)
	if err != nil {
		return 0, columnAdded, err
	}

	if current >= target {
		return 0, columnAdded, nil
	}

	// commitEachStep gates the per-migration commit to the production path
	// (upTo==0) on the main source. MigrateUpTo's test-support path (upTo>0)
	// and the dolt-ignored source (its cursor and tables are never committed to
	// shared history) both keep the single terminal commit MigrateUp runs.
	commitEachStep := upTo == 0 && m.cursorTable == mainSource.cursorTable
	count, err := runMigrations(ctx, db, m, current, target, commitEachStep)
	return count, columnAdded, err
}

// runMigrations applies migration files from src where minVersion < version <= upTo.
// Pass upTo=0 to apply through the latest version. It is package-private so
// tests can call it directly without needing a live cursor table.
//
// When commitEachStep is set, each numbered migration is committed atomically
// as it applies (see commitMigrationStep): its ALTERs and cursor row land in a
// Dolt commit before the next migration runs, so a crash/kill/timeout anywhere
// in MigrateUp's later backfill/rekey/ignored tail cannot strand this pass's
// applied migrations uncommitted for a retry to trip over (#4566). A residual
// window remains within a single step: a kill between a migration's SQL and
// its commitMigrationStep still leaves that one step's debris in the working
// set for the dirty-table guard to refuse. The window shrinks from the whole
// pass tail to one step; it does not close entirely.
func runMigrations(ctx context.Context, db DBConn, src migrationSource, minVersion, upTo int, commitEachStep bool) (int, error) {
	if upTo == 0 {
		upTo = src.latest()
	}
	announceLargeRigIfNeeded(ctx, db, src)

	count := 0
	for _, mf := range src.list() {
		if mf.version <= minVersion || mf.version > upTo {
			continue
		}
		applied, err := runMigrationStep(ctx, db, migrationStep{source: src, file: mf, commitEachStep: commitEachStep})
		if applied {
			count++
		}
		if err != nil {
			return count, err
		}
	}
	return count, nil

}

func announceLargeRigIfNeeded(ctx context.Context, db DBConn, src migrationSource) {
	if src.cursorTable != mainSource.cursorTable {
		return
	}
	rowCount, rowCountErr := issueRowCounter(ctx, db)
	emitLargeRigNotice(stderr, rowCount, rowCountErr)
}

type migrationStep struct {
	source         migrationSource
	file           migrationFile
	commitEachStep bool
}

func runMigrationStep(ctx context.Context, db DBConn, step migrationStep) (bool, error) {
	data, err := step.source.files.ReadFile(step.source.dir + "/" + step.file.name)
	if err != nil {
		return false, fmt.Errorf("reading migration %s: %w", step.file.name, err)
	}

	dirtyBeforeStep, err := snapshotMigrationStep(ctx, db, step)
	if err != nil {
		return false, err
	}
	if err := step.source.preMigrationRepair(ctx, db, step.file.version); err != nil {
		return false, fmt.Errorf("pre-repair for migration %s: %w", step.file.name, err)
	}

	fmt.Fprintf(stderr, "Applying migration %04d: %s…\n", step.file.version, humanMigrationName(step.file.name))
	start := time.Now()
	if err := execMigrationBody(ctx, db, string(data)); err != nil {
		return false, fmt.Errorf("migration %s: %w", step.file.name, err)
	}
	sum := sha256.Sum256(data)
	contentHash := hex.EncodeToString(sum[:])
	if _, err := db.ExecContext(ctx, "INSERT IGNORE INTO "+step.source.cursorTable+" (version, content_hash) VALUES (?, ?)", step.file.version, contentHash); err != nil {
		return false, fmt.Errorf("recording %s in %s: %w", step.file.name, step.source.cursorTable, err)
	}

	if err := commitMigrationStepIfNeeded(ctx, db, step, dirtyBeforeStep); err != nil {
		return true, err
	}
	fmt.Fprintf(stderr, "  done (%.1fs)\n", time.Since(start).Seconds())
	if err := runMigrationStepFaultHook(ctx, db, step.file.version); err != nil {
		return true, err
	}
	return true, nil
}

func snapshotMigrationStep(ctx context.Context, db DBConn, step migrationStep) (map[string]dirtyTableState, error) {
	if !step.commitEachStep {
		return nil, nil
	}
	dirtyBeforeStep, err := dirtyTables(ctx, db, true)
	if err != nil {
		return nil, fmt.Errorf("snapshotting dirty tables before %s: %w", step.file.name, err)
	}
	return dirtyBeforeStep, nil
}

func commitMigrationStepIfNeeded(ctx context.Context, db DBConn, step migrationStep, dirtyBeforeStep map[string]dirtyTableState) error {
	if !step.commitEachStep {
		return nil
	}
	if err := commitMigrationStep(ctx, db, step.source.cursorTable, step.file.name, dirtyBeforeStep); err != nil {
		return fmt.Errorf("committing migration %s: %w", step.file.name, err)
	}
	return nil
}

func runMigrationStepFaultHook(ctx context.Context, db DBConn, version int) error {
	if hook := migrateStepFaultHook.load(); hook != nil {
		return hook(ctx, db, version)
	}
	return nil
}

type migrationStepFaultHookState struct {
	mu sync.RWMutex
	fn func(ctx context.Context, db DBConn, version int) error
}

func (s *migrationStepFaultHookState) load() func(context.Context, DBConn, int) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fn
}

func (s *migrationStepFaultHookState) install(fn func(context.Context, DBConn, int) error) func() {
	s.mu.Lock()
	previous := s.fn
	s.fn = fn
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.fn = previous
		s.mu.Unlock()
	}
}

// migrateStepFaultHook is a test-only seam. When non-nil it runs at the end of
// each applied migration step (after the step's per-step commit on the
// production path); returning an error aborts the pass, emulating a
// crash/kill/timeout mid-migration so tests can prove the retry converges.
// Production leaves it nil.
var migrateStepFaultHook = &migrationStepFaultHookState{}

// SetMigrateStepFaultHookForTest installs fn as the per-step migration fault
// hook and returns a function that restores the previous value. Test-only.
func SetMigrateStepFaultHookForTest(fn func(ctx context.Context, db DBConn, version int) error) func() {
	return migrateStepFaultHook.install(fn)
}

// commitMigrationStep commits one numbered migration atomically: the tables it
// newly dirtied (diffed against dirtyBeforeStep, the working set snapshotted
// before the migration ran) plus the cursor row that records it. It force-
// stages only those tables, so pre-existing writes to tables the migration did
// not touch stay in the working set, and tolerates "nothing to commit" for a
// migration whose SQL was an idempotent no-op.
//
// MigrateUp's pre-flight guard proves no table a pending migration touches
// holds uncommitted user rows before the pass runs, so every table this step
// newly dirties is the migration's own DDL/DML — safe to commit on its own.
func commitMigrationStep(ctx context.Context, db DBConn, cursorTable, migrationName string, dirtyBeforeStep map[string]dirtyTableState) error {
	dirtyAfter, err := dirtyTables(ctx, db, true)
	if err != nil {
		return err
	}
	tableSet := map[string]struct{}{cursorTable: {}}
	for table := range dirtyAfter {
		if _, wasDirty := dirtyBeforeStep[table]; wasDirty {
			continue
		}
		tableSet[table] = struct{}{}
	}
	tables := make([]string, 0, len(tableSet))
	for table := range tableSet {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		if err := DrainCall(ctx, db, "CALL DOLT_ADD('-f', ?)", table); err != nil {
			return fmt.Errorf("dolt add %s: %w", table, err)
		}
	}
	if err := DrainCall(ctx, db, "CALL DOLT_COMMIT('-m', ?)", "schema: apply migration "+migrationName); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "nothing to commit") {
			return fmt.Errorf("committing migration step: %w", err)
		}
	}
	return nil
}
