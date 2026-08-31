package versioncontrolops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
)

// TryAutoResolveMergeConflicts auto-resolves merge conflicts that are safe to
// resolve without operator input, and returns (true, nil) only if ALL conflicts
// were resolved. It handles these classes:
//
//   - metadata: machine-local rows (e.g. dolt_auto_push_*) that routinely diverge
//     across clones (GH#2466). Resolved with "theirs".
//
//   - dependencies: with deterministic ids (#4259) the same logical edge has the
//     same primary key on every clone, so a same-PK conflict is the SAME edge.
//     When the two sides differ only in audit columns (created_at, created_by,
//     metadata, thread_id) — same edge, same type, present on both sides — the
//     conflict is resolved with "theirs" (the remote's values win, which is
//     convergent across clones pulling from the same remote). A conflict where the
//     dependency type differs, or one side deleted the edge, is a real semantic
//     conflict and is left for the operator.
//
//   - schema_migrations: pre-#4270 binaries record (version, NULL content_hash)
//     while post-#4270 binaries record (version, sha256), so two clones applying
//     the SAME migration with mixed binary vintages conflict on the cursor row
//     (bd-6dnrw.29). When one side's hash is NULL/empty and the other has one
//     (or both are equal), the row is resolved keeping the hash — recorded
//     provenance beats its absence, and the result converges across clones.
//     Two DIFFERENT non-empty hashes are the #4259 schema fork itself and are
//     left for the operator (bd doctor reports them as Migration Content Skew).
//
//   - config: persistent memories live in config as kv.memory.* rows (the
//     pre-pull auto-commit now commits config so they sync). Like metadata,
//     same-key memory edits across clones are machine-convergent: resolved with
//     "theirs", so all clones pulling from one remote converge on the remote's
//     value. A conflict touching ANY non-memory config key (issue_prefix above
//     all) is a real semantic conflict and is left for the operator.
//
//   - issues: modify/modify conflicts are merged FIELD BY FIELD against the
//     merge base (automerge.go). A cell only one side changed keeps that
//     side's value, so disjoint edits both survive; a cell both sides changed
//     to different values is settled last-write-wins by updated_at, which also
//     merges updated_at itself to max(ours, theirs). This is the flagship of
//     the federation asks: because beads stamps updated_at on every mutation,
//     ANY two same-issue edits between syncs conflict on that cell even when
//     the semantic fields are disjoint, so the conflict rate is far higher
//     than the semantic-conflict rate. add/add (no base row), delete/modify
//     (one side removed it), and a contested cell whose two sides carry equal
//     or unparseable updated_at values are left for the operator.
//
//   - labels: set-union. The table is all key columns, so two sides adding
//     DIFFERENT labels are disjoint rows dolt already unions and a conflict
//     can only be the same (issue_id, label) on both sides — identical data,
//     resolved by keeping it.
//
//   - comments/events: append-only union. Rows are insert-only and keyed by a
//     per-machine-unique id, so creation is disjoint; a same-id conflict whose
//     columns agree is the same append on both sides and is resolved by
//     keeping it. A row missing on one side, or diverging columns in a
//     supposedly immutable row, is left for the operator.
//
// Any conflict on another table, or an unresolvable dependencies,
// schema_migrations, config, issues, labels, comments, or events conflict,
// returns (false, nil) so the caller fails the pull and the operator resolves
// it.
//
// The resolved tables are staged but NOT committed: the caller must run
// CommitResolvedConflicts after the FK cascade repair, because DOLT_COMMIT
// refuses a working set with outstanding constraint violations (bd-578h9.14).
func TryAutoResolveMergeConflicts(ctx context.Context, db DBConn) (bool, error) {
	conflicts, err := loadMergeConflictTables(ctx, db)
	if err != nil {
		return false, err
	}
	if len(conflicts) == 0 {
		return false, nil // No conflicts to resolve — error was something else
	}

	plan, safe, err := classifyAutoResolveConflicts(ctx, db, conflicts)
	if err != nil {
		return false, err
	}
	if !safe {
		return false, nil
	}
	if err := applyAutoResolvePlan(ctx, db, plan); err != nil {
		return false, err
	}
	return true, nil
}

type mergeConflictTable struct {
	table string
	count int
}

func loadMergeConflictTables(ctx context.Context, db DBConn) ([]mergeConflictTable, error) {
	rows, err := db.QueryContext(ctx, "SELECT `table`, num_conflicts FROM dolt_conflicts")
	if err != nil {
		return nil, fmt.Errorf("failed to query conflicts: %w", err)
	}
	defer rows.Close()

	var conflicts []mergeConflictTable
	for rows.Next() {
		var conflict mergeConflictTable
		if err := rows.Scan(&conflict.table, &conflict.count); err != nil {
			return nil, fmt.Errorf("failed to scan conflict: %w", err)
		}
		conflicts = append(conflicts, conflict)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return conflicts, nil
}

type autoResolvePlan struct {
	resolvable []string
	issuesPlan []issuesRowMerge
	unionPlans map[string][]unionRowKey
}

type autoResolveTablePlan struct {
	table      string
	isIssues   bool
	issuesPlan []issuesRowMerge
	isUnion    bool
	unionPlan  []unionRowKey
}

func classifyAutoResolveConflicts(ctx context.Context, db DBConn, conflicts []mergeConflictTable) (autoResolvePlan, bool, error) {
	var plan autoResolvePlan
	for _, conflict := range conflicts {
		tablePlan, safe, err := classifyAutoResolveTable(ctx, db, conflict.table)
		if err != nil {
			return autoResolvePlan{}, false, err
		}
		if !safe {
			return autoResolvePlan{}, false, nil
		}
		addAutoResolveTablePlan(&plan, tablePlan)
	}
	return plan, true, nil
}

func classifyAutoResolveTable(ctx context.Context, db DBConn, table string) (autoResolveTablePlan, bool, error) {
	switch table {
	case "metadata":
		return autoResolveTablePlan{table: table}, true, nil
	case "dependencies":
		return classifyDependencyAutoResolveTable(ctx, db)
	case "schema_migrations":
		return classifySchemaMigrationsAutoResolveTable(ctx, db)
	case "config":
		return classifyConfigAutoResolveTable(ctx, db)
	case "issues":
		return classifyIssuesAutoResolveTable(ctx, db)
	case "labels", "comments", "events":
		return classifyUnionAutoResolveTable(ctx, db, table)
	default:
		return autoResolveTablePlan{}, false, nil
	}
}

func classifyDependencyAutoResolveTable(ctx context.Context, db DBConn) (autoResolveTablePlan, bool, error) {
	safe, err := dependencyConflictsAreAuditOnly(ctx, db)
	if err != nil || !safe {
		return autoResolveTablePlan{}, false, err
	}
	return autoResolveTablePlan{table: "dependencies"}, true, nil
}

func classifySchemaMigrationsAutoResolveTable(ctx context.Context, db DBConn) (autoResolveTablePlan, bool, error) {
	safe, err := schemaMigrationsConflictsAreVintageOnly(ctx, db)
	if err != nil || !safe {
		return autoResolveTablePlan{}, false, err
	}
	return autoResolveTablePlan{table: "schema_migrations"}, true, nil
}

func classifyConfigAutoResolveTable(ctx context.Context, db DBConn) (autoResolveTablePlan, bool, error) {
	safe, err := configConflictsAreMemoryConvergent(ctx, db)
	if err != nil || !safe {
		return autoResolveTablePlan{}, false, err
	}
	return autoResolveTablePlan{table: "config"}, true, nil
}

func classifyIssuesAutoResolveTable(ctx context.Context, db DBConn) (autoResolveTablePlan, bool, error) {
	plan, safe, err := issuesConflictsAreFieldMergeable(ctx, db)
	if err != nil || !safe {
		return autoResolveTablePlan{}, false, err
	}
	return autoResolveTablePlan{table: "issues", isIssues: true, issuesPlan: plan}, true, nil
}

func classifyUnionAutoResolveTable(ctx context.Context, db DBConn, table string) (autoResolveTablePlan, bool, error) {
	plan, safe, err := unionConflictsAreSafe(ctx, db, table)
	if err != nil || !safe {
		return autoResolveTablePlan{}, false, err
	}
	return autoResolveTablePlan{table: table, isUnion: true, unionPlan: plan}, true, nil
}

func addAutoResolveTablePlan(plan *autoResolvePlan, tablePlan autoResolveTablePlan) {
	plan.resolvable = append(plan.resolvable, tablePlan.table)
	if tablePlan.isIssues {
		plan.issuesPlan = tablePlan.issuesPlan
	}
	if tablePlan.isUnion {
		if plan.unionPlans == nil {
			plan.unionPlans = make(map[string][]unionRowKey)
		}
		plan.unionPlans[tablePlan.table] = tablePlan.unionPlan
	}
}

func applyAutoResolvePlan(ctx context.Context, db DBConn, plan autoResolvePlan) error {
	for _, table := range plan.resolvable {
		if err := applyAutoResolveTable(ctx, db, table, plan); err != nil {
			return err
		}
	}
	return nil
}

func applyAutoResolveTable(ctx context.Context, db DBConn, table string, plan autoResolvePlan) error {
	var err error
	switch table {
	case "schema_migrations":
		// Row-wise: keep whichever side recorded a content hash, so the
		// table-level --ours/--theirs choice can never drop one.
		err = resolveSchemaMigrationsVintageConflicts(ctx, db)
	case "config":
		err = resolveAutoConfigConflicts(ctx, db)
	case "issues":
		// Field-level three-way merge, not a table-level --ours/--theirs.
		err = resolveIssuesFieldMerge(ctx, db, plan.issuesPlan)
	case "labels", "comments", "events":
		err = resolveUnionConflicts(ctx, db, table, plan.unionPlans[table])
	default:
		//nolint:gosec // G201: table is one of the hardcoded constants above.
		_, err = db.ExecContext(ctx, "CALL DOLT_CONFLICTS_RESOLVE('--theirs', '"+table+"')")
		if err != nil {
			err = fmt.Errorf("failed to resolve %s conflicts: %w", table, err)
		}
	}
	if err != nil {
		return err
	}
	return stageAutoResolvedTable(ctx, db, table)
}

func resolveAutoConfigConflicts(ctx context.Context, db DBConn) error {
	// --theirs makes this clone's local kv.memory.* edit lose to the remote
	// value. Name the resolved keys first; a diagnostics query failure is
	// best-effort and must not abort an otherwise-correct resolution.
	if keys, err := resolvedConfigConflictKeys(ctx, db); err == nil && len(keys) > 0 {
		fmt.Fprintf(os.Stderr,
			"Notice: auto-resolved %d memory config conflict(s) with the remote value (--theirs); "+
				"local edits to %s were superseded\n",
			len(keys), strings.Join(keys, ", "))
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_CONFLICTS_RESOLVE('--theirs', 'config')"); err != nil {
		return fmt.Errorf("failed to resolve config conflicts: %w", err)
	}
	return nil
}

func stageAutoResolvedTable(ctx context.Context, db DBConn, table string) error {
	//nolint:gosec // G201: table is one of the hardcoded constants above.
	if _, err := db.ExecContext(ctx, "CALL DOLT_ADD('"+table+"')"); err != nil {
		return fmt.Errorf("failed to stage %s: %w", table, err)
	}
	return nil
}

// CommitResolvedConflicts creates the dolt commit that concludes a merge whose
// conflicts TryAutoResolveMergeConflicts settled. Callers that saw
// resolved=true MUST call this, and only AFTER TryRepairFKCascadeViolations
// has run: DOLT_COMMIT refuses a working set with outstanding constraint
// violations, so a merge carrying both an auto-resolvable conflict and an FK
// cascade violation could never settle while the resolver committed first
// (bd-578h9.14).
func CommitResolvedConflicts(ctx context.Context, db DBConn) error {
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-m', 'auto-resolve merge conflicts: metadata, dependencies, schema_migrations, config, issues (field-level three-way merge), labels/comments/events (union)')"); err != nil {
		return fmt.Errorf("failed to commit resolved conflicts: %w", err)
	}
	return nil
}

// dependencyConflictsAreAuditOnly reports whether every conflicted row in the
// dependencies table is the SAME logical edge on both sides that differs only in
// audit columns (created_at/created_by/metadata/thread_id) — the only class safe to
// auto-resolve with --theirs.
//
// It does NOT trust the primary key as proof of a shared edge. With deterministic
// ids the same edge has the same id on every clone, but an issue rename can leave a
// row's surrogate id stale (depid.New(oldID, target)) while issue_id/target have
// already moved (#4259 finding 2), so two genuinely different edges could collide on
// one id. We therefore verify the natural identity — issue_id and the resolved
// target — matches on both sides, and that the type matches, before declaring the
// conflict audit-only. It returns false if any conflicted row differs in identity or
// type, or was deleted on one side (an add/delete conflict).
func dependencyConflictsAreAuditOnly(ctx context.Context, db DBConn) (bool, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT our_id, their_id,
		       our_issue_id, their_issue_id,
		       our_depends_on_issue_id, their_depends_on_issue_id,
		       our_depends_on_wisp_id, their_depends_on_wisp_id,
		       our_depends_on_external, their_depends_on_external,
		       our_type, their_type
		FROM dolt_conflicts_dependencies`)
	if err != nil {
		return false, fmt.Errorf("query dependency conflicts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		conflict, err := scanDependencyConflict(rows)
		if err != nil {
			return false, err
		}
		if !dependencyConflictIsAuditOnly(conflict) {
			return false, nil
		}
	}
	return true, rows.Err()
}

type dependencyConflict struct {
	ourID, theirID             sql.NullString
	ourIssue, theirIssue       sql.NullString
	ourDepIssue, theirDepIssue sql.NullString
	ourDepWisp, theirDepWisp   sql.NullString
	ourDepExt, theirDepExt     sql.NullString
	ourType, theirType         sql.NullString
}

func scanDependencyConflict(rows *sql.Rows) (dependencyConflict, error) {
	var conflict dependencyConflict
	if err := rows.Scan(&conflict.ourID, &conflict.theirID, &conflict.ourIssue, &conflict.theirIssue,
		&conflict.ourDepIssue, &conflict.theirDepIssue, &conflict.ourDepWisp, &conflict.theirDepWisp,
		&conflict.ourDepExt, &conflict.theirDepExt, &conflict.ourType, &conflict.theirType); err != nil {
		return dependencyConflict{}, fmt.Errorf("scan dependency conflict: %w", err)
	}
	return conflict, nil
}

func dependencyConflictIsAuditOnly(conflict dependencyConflict) bool {
	// One side deleted the edge (add/delete conflict): leave for the operator.
	if !conflict.ourID.Valid || !conflict.theirID.Valid {
		return false
	}
	// Same edge requires the same source issue. A differing issue_id means the
	// shared id is stale on one side (e.g. a rename), not a shared edge.
	if conflict.ourIssue.Valid != conflict.theirIssue.Valid || conflict.ourIssue.String != conflict.theirIssue.String {
		return false
	}
	// ...and the same resolved target.
	ourTarget, ourOK := resolveConflictDepTarget(conflict.ourDepIssue, conflict.ourDepWisp, conflict.ourDepExt)
	theirTarget, theirOK := resolveConflictDepTarget(conflict.theirDepIssue, conflict.theirDepWisp, conflict.theirDepExt)
	if ourOK != theirOK || ourTarget != theirTarget {
		return false
	}
	// A differing type is the only remaining way this is a real semantic conflict.
	return conflict.ourType.Valid == conflict.theirType.Valid && conflict.ourType.String == conflict.theirType.String
}

// configConflictsAreMemoryConvergent reports whether every conflicted config
// row is a persistent-memory row (key prefixed memoryConfigKeyPrefix). Memories
// are the only config class safe to auto-resolve with --theirs: like metadata,
// all clones pulling from the same remote converge on the remote's value (a
// local edit to the same memory key loses, the same convergent trade-off
// metadata makes). Any other config key in conflict — issue_prefix above all,
// whose stale-value sweep GH#2455 specifically guards against — is a real
// semantic conflict, so the whole config table is left for the operator.
//
// The key column is config's primary key, so a same-key conflict carries the
// identical key on both sides; an add/delete conflict leaves one side NULL. A
// row is convergent only if every key it presents is a memory key.
func configConflictsAreMemoryConvergent(ctx context.Context, db DBConn) (bool, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT our_key, their_key FROM dolt_conflicts_config`)
	if err != nil {
		return false, fmt.Errorf("query config conflicts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ourKey, theirKey sql.NullString
		if err := rows.Scan(&ourKey, &theirKey); err != nil {
			return false, fmt.Errorf("scan config conflict: %w", err)
		}
		for _, k := range []sql.NullString{ourKey, theirKey} {
			if k.Valid && !strings.HasPrefix(k.String, memoryConfigKeyPrefix) {
				return false, nil
			}
		}
	}
	return true, rows.Err()
}

// resolvedConfigConflictKeys returns the keys of the config rows currently in
// conflict, used only to name the kv.memory.* keys whose local value the
// --theirs auto-resolution is about to supersede. It must be called BEFORE
// DOLT_CONFLICTS_RESOLVE clears dolt_conflicts_config. config's primary key is
// `key`, so a same-key conflict carries the identical key on both sides; an
// add/delete conflict leaves one side NULL, so COALESCE picks whichever side has
// it.
func resolvedConfigConflictKeys(ctx context.Context, db DBConn) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT COALESCE(our_key, their_key) FROM dolt_conflicts_config")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var key sql.NullString
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		if key.Valid {
			keys = append(keys, key.String)
		}
	}
	return keys, rows.Err()
}

// schemaMigrationsConflictsAreVintageOnly reports whether every conflicted
// schema_migrations row is the same migration version present on BOTH sides
// whose content hashes are compatible: equal, or NULL/empty on exactly one side
// (a pre-#4270 binary recorded the version without a hash, bd-6dnrw.29). Two
// different non-empty hashes mean the clones applied different content for the
// same version — the #4259 schema fork — and are never auto-resolved. A row
// deleted on one side is not a vintage artifact either.
func schemaMigrationsConflictsAreVintageOnly(ctx context.Context, db DBConn) (bool, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT our_version, their_version, our_content_hash, their_content_hash
		FROM dolt_conflicts_schema_migrations`)
	if err != nil {
		return false, fmt.Errorf("query schema_migrations conflicts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ourVersion, theirVersion sql.NullInt64
		var ourHash, theirHash sql.NullString
		if err := rows.Scan(&ourVersion, &theirVersion, &ourHash, &theirHash); err != nil {
			return false, fmt.Errorf("scan schema_migrations conflict: %w", err)
		}
		if !schemaMigrationConflictIsVintageOnly(ourVersion, theirVersion, ourHash, theirHash) {
			return false, nil
		}
	}
	return true, rows.Err()
}

func schemaMigrationConflictIsVintageOnly(ourVersion, theirVersion sql.NullInt64, ourHash, theirHash sql.NullString) bool {
	if !ourVersion.Valid || !theirVersion.Valid || ourVersion.Int64 != theirVersion.Int64 {
		return false
	}
	ours, theirs := ourHash.String, theirHash.String
	if ours != "" && theirs != "" && ours != theirs {
		return false // real content skew (#4259) — operator decides
	}
	return true
}

// resolveSchemaMigrationsVintageConflicts resolves vintage-only cursor-row
// conflicts (validated by schemaMigrationsConflictsAreVintageOnly) keeping
// whichever side recorded a content hash: when theirs has the hash and ours is
// NULL, the working-set row is updated to theirs before the table-level
// resolve, so '--ours' never discards recorded provenance.
func resolveSchemaMigrationsVintageConflicts(ctx context.Context, db DBConn) error {
	fixes, err := loadSchemaMigrationHashFixes(ctx, db)
	if err != nil {
		return err
	}
	for _, fix := range fixes {
		if _, err := db.ExecContext(ctx,
			"UPDATE schema_migrations SET content_hash = ? WHERE version = ?", fix.hash, fix.version); err != nil {
			return fmt.Errorf("backfill content_hash for migration %d: %w", fix.version, err)
		}
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_CONFLICTS_RESOLVE('--ours', 'schema_migrations')"); err != nil {
		return fmt.Errorf("failed to resolve schema_migrations conflicts: %w", err)
	}
	return nil
}

type schemaMigrationHashFix struct {
	version int64
	hash    string
}

func loadSchemaMigrationHashFixes(ctx context.Context, db DBConn) ([]schemaMigrationHashFix, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT our_version, our_content_hash, their_content_hash
		FROM dolt_conflicts_schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("query schema_migrations conflicts: %w", err)
	}
	defer rows.Close()

	var fixes []schemaMigrationHashFix
	for rows.Next() {
		var version sql.NullInt64
		var ourHash, theirHash sql.NullString
		if err := rows.Scan(&version, &ourHash, &theirHash); err != nil {
			return nil, fmt.Errorf("scan schema_migrations conflict: %w", err)
		}
		if ourHash.String == "" && theirHash.String != "" {
			fixes = append(fixes, schemaMigrationHashFix{version: version.Int64, hash: theirHash.String})
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return fixes, nil
}

// resolveConflictDepTarget returns the single non-null dependency target from a
// conflict row's three typed target columns, following the same precedence as
// COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external).
func resolveConflictDepTarget(issueTarget, wispTarget, external sql.NullString) (string, bool) {
	switch {
	case issueTarget.Valid:
		return issueTarget.String, true
	case wispTarget.Valid:
		return wispTarget.String, true
	case external.Valid:
		return external.String, true
	default:
		return "", false
	}
}

// fkCascadeRepairDeletes maps each synced child table holding a FOREIGN KEY to
// issues(id) (migrations 0041/0042 added ON DELETE/UPDATE CASCADE; ignored
// migration 0002 covers child_counters) to the DELETE that applies the FK's
// cascade semantics by hand after a merge (bd-6dnrw.4).
//
// Dolt merges each table row-wise and never re-executes cascades, so "clone A
// deletes issue X" merged with "clone B inserts a child row referencing X"
// produces a child row whose parent is gone — a foreign-key constraint
// violation that makes the merge transaction roll back, and retrying can never
// converge. Deleting the dangling rows is the convergent repair: it is exactly
// what the cascade did on the deleting clone, and what the FK would have
// forced had the two writes been sequenced on one database.
var fkCascadeRepairDeletes = map[string]string{
	"dependencies": `DELETE FROM dependencies
		WHERE issue_id NOT IN (SELECT id FROM issues)
		   OR (depends_on_issue_id IS NOT NULL AND depends_on_issue_id NOT IN (SELECT id FROM issues))`,
	"labels":               `DELETE FROM labels WHERE issue_id NOT IN (SELECT id FROM issues)`,
	"comments":             `DELETE FROM comments WHERE issue_id NOT IN (SELECT id FROM issues)`,
	"events":               `DELETE FROM events WHERE issue_id NOT IN (SELECT id FROM issues)`,
	"issue_snapshots":      `DELETE FROM issue_snapshots WHERE issue_id NOT IN (SELECT id FROM issues)`,
	"compaction_snapshots": `DELETE FROM compaction_snapshots WHERE issue_id NOT IN (SELECT id FROM issues)`,
	"child_counters":       `DELETE FROM child_counters WHERE parent_id NOT IN (SELECT id FROM issues)`,
}
