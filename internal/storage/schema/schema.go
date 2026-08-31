package schema

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/term"
)

// stderr is the writer for migration progress messages. Defaults to os.Stderr
// when stderr is a terminal so humans see progress, and to io.Discard otherwise
// so the lines don't pollute machine-parsed output (tests, CI, piped callers).
// Overridable in tests.
var stderr io.Writer = defaultStderr()

func defaultStderr() io.Writer {
	if term.IsTerminal(int(os.Stderr.Fd())) {
		return os.Stderr
	}
	return io.Discard
}

const largeRigThreshold = 10000

// issueRowCounter returns the current issues-table row count, or an error if
// the table is unreachable (fresh install → table doesn't exist yet). The
// caller uses the error as the "no warning" signal. Variable so tests in this
// package that exercise runMigrations against a non-DB mock can stub out the
// query without panicking on QueryRowContext.
var issueRowCounter = func(ctx context.Context, db DBConn) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM issues").Scan(&n)
	return n, err
}

// emitLargeRigNotice writes the one-line large-rig warning to out when the
// issues count exceeds largeRigThreshold. An error from the counter is
// treated as "fresh install / table missing" and suppresses the warning —
// see be-8ja for the UX rationale.
func emitLargeRigNotice(out io.Writer, count int64, err error) {
	if err != nil || count <= largeRigThreshold {
		return
	}
	fmt.Fprintf(out, "Large rig detected (%d issues). This migration may take up to 90 seconds; do not interrupt.\n", count)
}

// humanMigrationName turns "0033_add_date_indexes.up.sql" into
// "add_date_indexes" for the progress line.
func humanMigrationName(filename string) string {
	s := strings.TrimSuffix(filename, ".up.sql")
	parts := strings.SplitN(s, "_", 2)
	if len(parts) < 2 {
		return s
	}
	return parts[1]
}

// DBConn is the minimal interface satisfied by *sql.DB, *sql.Tx, and *sql.Conn.
// It provides query and exec methods needed by the migration runner.
type DBConn interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SchemaSkewError is returned when the DB schema version is ahead of the
// binary's known version (forward drift). Stale binary queries may fail with
// cryptic SQL errors like "column X could not be found in any table in scope".
type SchemaSkewError struct {
	DBVersion     int
	BinaryVersion int
}

func (e *SchemaSkewError) Error() string {
	delta := e.DBVersion - e.BinaryVersion
	unit := "migrations"
	if delta == 1 {
		unit = "migration"
	}
	return fmt.Sprintf("schema version mismatch: database is at v%d, binary knows up to v%d (%d %s ahead)",
		e.DBVersion, e.BinaryVersion, delta, unit)
}

// UserMessage returns the full multi-line error block for terminal output.
func (e *SchemaSkewError) UserMessage() string {
	return e.Error() + "\n" +
		"\n" +
		"  Your bd binary is stale. Queries for dropped or renamed columns will fail\n" +
		"  with cryptic SQL errors (e.g. \"column X could not be found in any table in scope\").\n" +
		"\n" +
		"  Rebuild from main:\n" +
		"    CGO_ENABLED=0 go build -tags gms_pure_go ./cmd/bd\n" +
		"\n" +
		"  Or install the latest release:\n" +
		"    CGO_ENABLED=0 go install -tags gms_pure_go github.com/jonbaldie/beads/cmd/bd@latest\n" +
		"\n" +
		"  To proceed despite the risk (some read commands may still work):\n" +
		"    BD_IGNORE_SCHEMA_SKEW=1 bd <command>\n" +
		"    bd --ignore-schema-skew <command>\n"
}

// EscapeHint returns the escape-hatch string for JSON error output.
func (e *SchemaSkewError) EscapeHint() string {
	return "BD_IGNORE_SCHEMA_SKEW=1 bd <command>  or  bd --ignore-schema-skew <command>"
}

// IsSchemaSkewError reports whether err (or any error it wraps) is a
// *SchemaSkewError.
func IsSchemaSkewError(err error) bool {
	var e *SchemaSkewError
	return errors.As(err, &e)
}

// checkSchemaSkew queries the DB's current schema version and returns a
// *SchemaSkewError if the DB is ahead of the binary. Returns nil for a fresh
// DB (version=0) or when BD_IGNORE_SCHEMA_SKEW=1 (prints a warning instead).
func checkSchemaSkew(ctx context.Context, db DBConn) error {
	// CurrentVersion treats a missing schema_migrations table as version 0, so
	// this is safe to call before migrations have created the table: a
	// brand-new database (version 0) falls through the no-op check below. That
	// matters on the writable open path, where the guard runs before initSchema
	// creates the table on a fresh database.
	currentVersion, err := CurrentVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("schema skew check: %w", err)
	}
	if currentVersion == 0 || currentVersion <= LatestVersion() {
		return nil
	}
	if os.Getenv("BD_IGNORE_SCHEMA_SKEW") == "1" {
		fmt.Fprintf(os.Stderr,
			"Warning: schema skew ignored — database (v%d) is ahead of binary (v%d); some queries may fail\n",
			currentVersion, LatestVersion())
		return nil
	}
	return &SchemaSkewError{DBVersion: currentVersion, BinaryVersion: LatestVersion()}
}

// CheckForwardDrift reports a *SchemaSkewError when the database's schema
// version is AHEAD of the binary's (forward drift). It accepts any DBConn (a
// pooled *sql.DB or a pinned *sql.Conn), so both the read-only store path
// (where MigrateUp is skipped) and the writable open path (where MigrateUp
// no-ops on a forward-drifted DB rather than erroring) can fail fast before a
// query hits a dropped or renamed column.
func CheckForwardDrift(ctx context.Context, db DBConn) error {
	return checkSchemaSkew(ctx, db)
}

// SchemaBehindError is returned when a database is opened on a path that
// cannot migrate it (read-only opens) and its schema version is behind the
// binary's. Without this check the open succeeds and queries fail later with
// cryptic unknown-column/table errors (bd-578h9.12).
type SchemaBehindError struct {
	DBVersion     int
	BinaryVersion int
}

func (e *SchemaBehindError) Error() string {
	return fmt.Sprintf("schema version mismatch: database is at v%d, binary expects v%d, and the read-only open cannot migrate it; run any bd write command in that workspace to migrate, or set BD_IGNORE_SCHEMA_SKEW=1 to read anyway (queries touching newer schema may fail)",
		e.DBVersion, e.BinaryVersion)
}

// IsSchemaBehindError reports whether err (or any error it wraps) is a
// *SchemaBehindError.
func IsSchemaBehindError(err error) bool {
	var e *SchemaBehindError
	return errors.As(err, &e)
}

// CheckBehindDrift returns a *SchemaBehindError when the database's schema
// version is behind the binary's. Used by read-only opens, which skip
// MigrateUp by design (bd-6dnrw.32) — the paths that previously auto-migrated
// foreign databases (GH#3231) now need a clear open-time failure instead of
// unknown-column errors at query time. BD_IGNORE_SCHEMA_SKEW=1 downgrades it
// to a warning, mirroring forward drift. A fresh DB (version 0) is reported
// as behind too: it has no readable schema at all.
func CheckBehindDrift(ctx context.Context, db *sql.DB) error {
	var currentVersion int
	if err := db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM schema_migrations",
	).Scan(&currentVersion); err != nil {
		return fmt.Errorf("schema behind-drift check: %w", err)
	}
	if currentVersion >= LatestVersion() {
		return nil
	}
	if os.Getenv("BD_IGNORE_SCHEMA_SKEW") == "1" {
		fmt.Fprintf(os.Stderr,
			"Warning: schema skew ignored — database (v%d) is behind binary (v%d) and was opened read-only; some queries may fail\n",
			currentVersion, LatestVersion())
		return nil
	}
	return &SchemaBehindError{DBVersion: currentVersion, BinaryVersion: LatestVersion()}
}

var doltStatusTableNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

//go:embed migrations/*.up.sql
var upMigrations embed.FS

//go:embed migrations/ignored/*.up.sql
var upIgnoredMigrations embed.FS

type migrationSource struct {
	files       embed.FS
	dir         string
	cursorTable string
	// sentinelTables are tables this series is responsible for creating. A
	// non-zero cursor is only believed while they all exist: the cursor is a
	// claim about the schema, and a claim contradicted by the schema is worth
	// less than no claim at all. See cursorContradictedBySchema.
	//
	// INVARIANT: no future migration in this series may DROP or RENAME a
	// sentinel. Older binaries in the field check their own sentinel list
	// against the live schema, so removing one would make every healthy newer
	// database read as "contradicted" to them and re-run their whole series.
	// TestSentinelTablesAreCreatedByTheSeries enforces the creating side only;
	// the dropping side is this comment.
	sentinelTables []string
}

var (
	mainSource = migrationSource{
		files:       upMigrations,
		dir:         "migrations",
		cursorTable: "schema_migrations",
	}
	ignoredSource = migrationSource{
		files:       upIgnoredMigrations,
		dir:         "migrations/ignored",
		cursorTable: "ignored_schema_migrations",
		// Created by ignored 0001 (the series' foundation) and required by
		// the wisp write path. wisp_dependencies is the one gh 5033 reports
		// missing; wisps is checked too so a partially materialized database
		// is caught by whichever is absent.
		sentinelTables: []string{"wisps", "wisp_dependencies"},
	}
)

type latestVersionCache struct {
	mainOnce    sync.Once
	main        int
	ignoredOnce sync.Once
	ignored     int
}

var latestVersions = func() func() *latestVersionCache {
	var cache latestVersionCache
	return func() *latestVersionCache { return &cache }
}()

// doltIgnorePatterns is the canonical set of clone-local table patterns every
// database must carry in dolt_ignore. Four of them are historically seeded as
// one-shot up.sql side effects of migrations 0019/0028, which never re-execute
// once the schema_migrations cursor is at-latest: a database materialized
// out-of-band (table-by-table copy/rename, dump restore) arrives with the
// cursor at-latest and misses them permanently, so wisp/local-state churn
// pollutes dolt_status and feeds the dirty-table migration gates. MigrateUp
// re-asserts the full set idempotently at the top of every write-mode open.
var doltIgnorePatterns = []string{
	// The events journal tables (bd-opisf) are seeded here rather than
	// version-gated: they have never existed on the versioned plane, so
	// asserting the pattern before 0064 runs is what keeps the CREATE from
	// landing as tracked-at-HEAD in the first place.
	"bd_events_journal",
	"bd_events_seq",
	"ignored_schema_migrations",
	"leases",
	"local_metadata",
	"repo_mtimes",
	"wisp_%",
	"wisps",
}

// versionGatedDoltIgnorePatterns are ignore patterns whose table was moved
// onto the ignored plane by a specific main-lane migration, so re-asserting
// them is only correct once the main cursor has reached that version. Seeding
// them unconditionally would strand a pre-flip database whose migration pass
// is refused (remote-migrate gate, dirty-table gate): the table would still
// be tracked-and-versioned while the pattern suppressed all staging of it, so
// its writes would silently stop being committed. The gate matters only for
// the out-of-band heal path — on a normal upgrade the flip migration itself
// registers the pattern in the same pass.
var versionGatedDoltIgnorePatterns = []struct {
	pattern        string
	minMainVersion int
}{
	{"events", 62}, // 0062_events_dolt_ignore (bd-red8u)
}

// seedDoltIgnorePatterns idempotently asserts the canonical dolt_ignore
// patterns and reports whether it actually changed anything. INSERT IGNORE
// leaves existing rows untouched, so a healthy database sees no working-set
// change and an explicit operator override (pattern present with
// ignored=false) is respected. On an under-seeded database the new rows land
// in the working set and take effect immediately; who commits them depends on
// the pass: when migration work is needed, MigrateUp exempts dolt_ignore from
// the pre-existing-dirty guards as pass-owned state (same treatment as the
// aux-rekey tables) and stageSchemaTables commits it with the pass; on the
// no-work short-circuit, MigrateUp commits the seed itself in a scoped,
// labeled commit (keyed off the changed return value) so the heal converges
// in one pass instead of riding along inside an unrelated later commit.
func seedDoltIgnorePatterns(ctx context.Context, db DBConn) (bool, error) {
	changed := false
	for _, pattern := range doltIgnorePatterns {
		seeded, err := seedDoltIgnorePattern(ctx, db, pattern)
		if err != nil {
			return changed, err
		}
		changed = changed || seeded
	}
	// Version-gated patterns seed only once the main cursor proves the flip
	// migration applied. The cursor table may not exist yet (first-ever run,
	// before mainSource.migrate bootstraps it): treat that as version 0 and
	// skip — the flip migration registers its own pattern when it applies.
	mainVersion, err := mainSource.currentVersion(ctx, db)
	if err != nil {
		mainVersion = 0
	}
	for _, gated := range versionGatedDoltIgnorePatterns {
		if mainVersion < gated.minMainVersion {
			continue
		}
		seeded, err := seedDoltIgnorePattern(ctx, db, gated.pattern)
		if err != nil {
			return changed, err
		}
		changed = changed || seeded
	}
	return changed, nil
}

func seedDoltIgnorePattern(ctx context.Context, db DBConn, pattern string) (bool, error) {
	res, err := db.ExecContext(ctx, "INSERT IGNORE INTO dolt_ignore VALUES (?, true)", pattern)
	if err != nil {
		return false, fmt.Errorf("seeding dolt_ignore pattern %q: %w", pattern, err)
	}
	// A RowsAffected error degrades to changed=false for that row: the seed
	// then stays an uncommitted working-set diff swept up by the next commit,
	// exactly the pre-scoped-commit behavior.
	n, raErr := res.RowsAffected()
	return raErr == nil && n > 0, nil
}

// commitSeededDoltIgnore stages and commits freshly seeded dolt_ignore rows
// in a scoped, labeled commit. Both MigrateUp paths use it: on the no-work
// short-circuit nothing downstream would ever commit the seed, and on the
// migration path the seed must be committed before the first step so an
// interrupted pass leaves a clean working set (#4566 self-heal contract).
func commitSeededDoltIgnore(ctx context.Context, db DBConn) error {
	if err := DrainCall(ctx, db, "CALL DOLT_ADD('dolt_ignore')"); err != nil {
		return fmt.Errorf("staging seeded dolt_ignore patterns: %w", err)
	}
	if err := DrainCall(ctx, db, "CALL DOLT_COMMIT('-m', 'schema: seed dolt_ignore patterns')"); err != nil {
		return fmt.Errorf("committing seeded dolt_ignore patterns: %w", err)
	}
	return nil
}

func LatestVersion() int {
	cache := latestVersions()
	cache.mainOnce.Do(func() {
		cache.main = mainSource.latest()
	})
	return cache.main
}

func LatestIgnoredVersion() int {
	cache := latestVersions()
	cache.ignoredOnce.Do(func() {
		cache.ignored = ignoredSource.latest()
	})
	return cache.ignored
}

func CurrentVersion(ctx context.Context, db DBConn) (int, error) {
	return mainSource.currentVersion(ctx, db)
}

func CurrentIgnoredVersion(ctx context.Context, db DBConn) (int, error) {
	return ignoredSource.currentVersion(ctx, db)
}

func PendingVersions(ctx context.Context, db DBConn) ([]int, error) {
	return mainSource.pendingVersions(ctx, db)
}

func PendingIgnoredVersions(ctx context.Context, db DBConn) ([]int, error) {
	return ignoredSource.pendingVersions(ctx, db)
}

func AllMigrationsSQL() string {
	var b strings.Builder
	b.WriteString(mainSource.bootstrapSQL())
	b.WriteString(";\n")
	for _, f := range mainSource.list() {
		data, err := mainSource.files.ReadFile(mainSource.dir + "/" + f.name)
		if err != nil {
			continue
		}
		b.WriteString(cliCompatibleMigrationSQL(f.name, string(data)))
		sum := sha256.Sum256(data)
		fmt.Fprintf(&b, "\nINSERT IGNORE INTO %s (version, content_hash) VALUES (%d, '%s');\n",
			mainSource.cursorTable, f.version, hex.EncodeToString(sum[:]))
	}
	return b.String()
}

// MigrationSQL returns the frozen SQL content of a main-source migration
// file by name (e.g. "0057_events_value_columns_idempotent_longtext.up.sql"),
// read from the same embedded FS runMigrations applies in production
// (see mainSource.files above). It exists for tests outside this package
// (internal/storage/embeddeddolt's engine-based frozen-guard tests) that need
// to execute the byte-exact frozen migration content through a real engine:
// reading the file from disk by a package-relative path does not reliably
// resolve across every CI execution context, while the embedded FS is always
// available and is, in fact, higher-fidelity -- it is the literal bytes
// runMigrations itself applies, not a copy that could drift from the build.
// Read-only; callers cannot use it to mutate or bypass the frozen-file
// hygiene guard (scripts/check-migration-hygiene.sh).
func MigrationSQL(name string) (string, error) {
	data, err := mainSource.files.ReadFile(mainSource.dir + "/" + name)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// IgnoredMigrationSQL is MigrationSQL's ignored-lane counterpart: the frozen
// bytes of an ignored-source migration file (e.g. "0019_create_events.up.sql"),
// for engine-based frozen-guard tests of clone-local DDL.
func IgnoredMigrationSQL(name string) (string, error) {
	data, err := ignoredSource.files.ReadFile(ignoredSource.dir + "/" + name)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func parseVersion(name string) (int, error) {
	parts := strings.SplitN(name, "_", 2)
	if len(parts) == 0 {
		return 0, fmt.Errorf("no version prefix")
	}
	return strconv.Atoi(parts[0])
}
