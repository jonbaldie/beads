//go:build cgo

// Package legacysqlite reads the small, authenticated SQLite history that
// predates the current Dolt store. It intentionally has no general migration
// registry: each accepted layout is an exact, audited contract.
package legacysqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var acceptedVersions = map[string]bool{"0.49.6": true, "0.50.3": true}

// Current bounded-string widths come from the canonical Dolt schema in
// internal/storage/schema/migrations/0001_create_issues.up.sql. VARCHAR(255)
// fields use types.MaxFieldLen. Payload and waiters remain TEXT after migration
// 0049 widened the other large content columns to LONGTEXT.
const (
	currentShortVarcharRunes  = 32
	currentCommitVarcharRunes = 64
	currentTitleVarcharRunes  = 500
	currentSpecIDVarcharRunes = 1024
	currentTextBytes          = 65_535
)

var schema = map[string]string{
	"metadata":     `key|TEXT|0|-|1 value|TEXT|1|-|0`,
	"issues":       `id|TEXT|0|-|1 content_hash|TEXT|0|-|0 title|TEXT|1|-|0 description|TEXT|1|''|0 design|TEXT|1|''|0 acceptance_criteria|TEXT|1|''|0 notes|TEXT|1|''|0 status|TEXT|1|'open'|0 priority|INTEGER|1|2|0 issue_type|TEXT|1|'task'|0 assignee|TEXT|0|-|0 estimated_minutes|INTEGER|0|-|0 created_at|DATETIME|1|CURRENT_TIMESTAMP|0 created_by|TEXT|0|''|0 owner|TEXT|0|''|0 updated_at|DATETIME|1|CURRENT_TIMESTAMP|0 closed_at|DATETIME|0|-|0 closed_by_session|TEXT|0|''|0 external_ref|TEXT|0|-|0 spec_id|TEXT|0|-|0 compaction_level|INTEGER|0|0|0 compacted_at|DATETIME|0|-|0 compacted_at_commit|TEXT|0|-|0 original_size|INTEGER|0|-|0 deleted_at|DATETIME|0|-|0 deleted_by|TEXT|0|''|0 delete_reason|TEXT|0|''|0 original_type|TEXT|0|''|0 sender|TEXT|0|''|0 ephemeral|INTEGER|0|0|0 wisp_type|TEXT|0|''|0 pinned|INTEGER|0|0|0 is_template|INTEGER|0|0|0 crystallizes|INTEGER|0|0|0 mol_type|TEXT|0|''|0 work_type|TEXT|0|'mutex'|0 quality_score|REAL|0|-|0 source_system|TEXT|0|''|0 metadata|TEXT|1|'{}'|0 event_kind|TEXT|0|''|0 actor|TEXT|0|''|0 target|TEXT|0|''|0 payload|TEXT|0|''|0 source_repo|TEXT|0|'.'|0 close_reason|TEXT|0|''|0 await_type|TEXT|0|-|0 await_id|TEXT|0|-|0 timeout_ns|INTEGER|0|-|0 waiters|TEXT|0|-|0 hook_bead|TEXT|0|''|0 role_bead|TEXT|0|''|0 agent_state|TEXT|0|''|0 last_activity|DATETIME|0|-|0 role_type|TEXT|0|''|0 rig|TEXT|0|''|0 due_at|DATETIME|0|-|0 defer_until|DATETIME|0|-|0`,
	"dependencies": `issue_id|TEXT|1|-|1 depends_on_id|TEXT|1|-|2 type|TEXT|1|'blocks'|3 created_at|TIMESTAMP|0|CURRENT_TIMESTAMP|0 created_by|TEXT|1|-|0 metadata|TEXT|0|-|0 thread_id|TEXT|0|-|0`,
	"labels":       `issue_id|TEXT|1|-|1 label|TEXT|1|-|2`,
	"comments":     `id|INTEGER|0|-|1 issue_id|TEXT|1|-|0 author|TEXT|1|-|0 text|TEXT|1|-|0 created_at|DATETIME|1|CURRENT_TIMESTAMP|0`,
}

// Export emits current canonical issue JSONL. It opens only a sealed private
// copy, and renames a completed spool to a file destination.
func Export(ctx context.Context, source, output string, stdout io.Writer) error {
	sealed, err := seal(source)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(sealed.dir) }()
	if err := validateExportDestination(source, sealed.source, output); err != nil {
		return err
	}
	spool, err := createExportSpool(output)
	if err != nil {
		return err
	}
	spoolName := spool.Name()
	defer func() { _ = os.Remove(spoolName) }()
	if err := writeExportSpool(ctx, sealed.db, spool); err != nil {
		return err
	}
	return finishExport(output, stdout, spoolName)
}

func validateExportDestination(source, sealedSource, output string) error {
	if output == "-" {
		return nil
	}
	if err := rejectAlias(source, output); err != nil {
		return err
	}
	return rejectAlias(sealedSource, output)
}

func createExportSpool(output string) (*os.File, error) {
	spoolDir := ""
	if output != "-" {
		spoolDir = filepath.Dir(output)
	}
	spool, err := os.CreateTemp(spoolDir, ".bd-legacy-sqlite-*")
	if err != nil {
		return nil, fmt.Errorf("create output spool: %w", err)
	}
	return spool, nil
}

func writeExportSpool(ctx context.Context, database string, spool *os.File) error {
	if err := read(ctx, database, spool); err != nil {
		_ = spool.Close()
		return err
	}
	return spool.Close()
}

func finishExport(output string, stdout io.Writer, spoolName string) error {
	if output == "-" {
		_, err := spoolTo(stdout, spoolName)
		return err
	}
	return os.Rename(spoolName, output)
}

func spoolTo(w io.Writer, name string) (int64, error) {
	f, err := os.Open(name) //nolint:gosec // G304: name is the private spool created by Export.
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return io.Copy(w, f)
}

type sealedDB struct{ dir, db, source string }
type fingerprint struct {
	exists bool
	size   int64
	mod    time.Time
	digest string
	info   os.FileInfo
}
type sourceSet struct{ db, wal, journal fingerprint }

func seal(source string) (sealedDB, error) {
	resolved, before, err := prepareSealSource(source)
	if err != nil {
		return sealedDB{}, err
	}
	dir, err := os.MkdirTemp("", "bd-legacy-sqlite-")
	if err != nil {
		return sealedDB{}, err
	}
	fail := func(err error) (sealedDB, error) { _ = os.RemoveAll(dir); return sealedDB{}, err }
	if err := copySealedFiles(resolved, dir, before); err != nil {
		return fail(err)
	}
	if err := verifySealedFiles(dir, before); err != nil {
		return fail(err)
	}
	after, err := fingerprintSource(resolved)
	if err != nil {
		return fail(err)
	}
	if !sameSet(before, after) {
		return fail(fmt.Errorf("legacy SQLite source changed while sealing"))
	}
	return sealedDB{dir: dir, db: filepath.Join(dir, "legacy.db"), source: resolved}, nil
}

func prepareSealSource(source string) (string, sourceSet, error) {
	resolved, err := filepath.Abs(source)
	if err != nil {
		return "", sourceSet{}, fmt.Errorf("resolve legacy SQLite source: %w", err)
	}
	before, err := fingerprintSource(resolved)
	if err != nil {
		return "", sourceSet{}, err
	}
	if before.journal.exists {
		return "", sourceSet{}, fmt.Errorf("legacy SQLite source has rollback journal")
	}
	return resolved, before, nil
}

func copySealedFiles(resolved, dir string, before sourceSet) error {
	pairs := []struct {
		from, to string
		present  bool
	}{
		{resolved, filepath.Join(dir, "legacy.db"), true},
		{resolved + "-wal", filepath.Join(dir, "legacy.db-wal"), before.wal.exists},
	}
	for _, pair := range pairs {
		if pair.present {
			if err := copyFile(pair.from, pair.to); err != nil {
				return err
			}
		}
	}
	return nil
}

func verifySealedFiles(dir string, before sourceSet) error {
	if err := verifySealedFile(filepath.Join(dir, "legacy.db"), before.db.digest, "database"); err != nil {
		return err
	}
	if before.wal.exists {
		if err := verifySealedFile(filepath.Join(dir, "legacy.db-wal"), before.wal.digest, "WAL"); err != nil {
			return err
		}
	}
	return nil
}

func verifySealedFile(path, wantDigest, name string) error {
	copied, err := fingerprintFile(path, true)
	if err != nil {
		return err
	}
	if copied.digest != wantDigest {
		return fmt.Errorf("sealed legacy SQLite %s does not match source fingerprint", name)
	}
	return nil
}

func fingerprintSource(path string) (sourceSet, error) {
	db, err := fingerprintFile(path, true)
	if err != nil {
		return sourceSet{}, err
	}
	wal, err := fingerprintFile(path+"-wal", false)
	if err != nil {
		return sourceSet{}, err
	}
	if _, err := fingerprintFile(path+"-shm", false); err != nil {
		return sourceSet{}, err
	}
	journal, err := fingerprintFile(path+"-journal", false)
	if err != nil {
		return sourceSet{}, err
	}
	return sourceSet{db, wal, journal}, nil
}

func fingerprintFile(path string, required bool) (fingerprint, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) && !required {
		return fingerprint{}, nil
	}
	if err != nil {
		return fingerprint{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fingerprint{}, fmt.Errorf("legacy SQLite source %q must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fingerprint{}, fmt.Errorf("legacy SQLite source %q must be a regular file", path)
	}
	f, err := os.Open(path) //nolint:gosec // G304: source is lstat-checked and fingerprinted again after sealing.
	if err != nil {
		return fingerprint{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return fingerprint{}, err
	}
	return fingerprint{true, info.Size(), info.ModTime(), hex.EncodeToString(h.Sum(nil)), info}, nil
}

func sameSet(a, b sourceSet) bool {
	return sameFingerprint(a.db, b.db) && sameFingerprint(a.wal, b.wal) && sameFingerprint(a.journal, b.journal)
}
func sameFingerprint(a, b fingerprint) bool {
	return a.exists == b.exists && (!a.exists || (a.size == b.size && a.mod.Equal(b.mod) && a.digest == b.digest && os.SameFile(a.info, b.info)))
}
func copyFile(from, to string) error {
	in, err := os.Open(from) //nolint:gosec // G304: from is the checked legacy source or its WAL sidecar.
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: to is inside Export's private sealing directory.
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func rejectAlias(source, output string) error {
	for _, protected := range []string{source, source + "-wal", source + "-shm", source + "-journal"} {
		if samePath(protected, output) {
			return fmt.Errorf("--output must not alias legacy SQLite source or sidecar")
		}
	}
	return nil
}

func samePath(a, b string) bool {
	aa, errA := canonicalPath(a)
	bb, errB := canonicalPath(b)
	if errA != nil || errB != nil {
		return false
	}
	if aa == bb {
		return true
	}
	ai, errA := os.Stat(aa)
	bi, errB := os.Stat(bb)
	return errA == nil && errB == nil && os.SameFile(ai, bi)
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var tail []string
	for current := abs; ; current = filepath.Dir(current) {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(append([]string{resolved}, tail...)...), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return abs, nil
		}
		tail = append([]string{filepath.Base(current)}, tail...)
	}
}

func read(ctx context.Context, path string, out io.Writer) error {
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro&_query_only=1&_loc=UTC")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := verify(ctx, tx); err != nil {
		return err
	}
	issues, err := loadIssues(ctx, tx)
	if err != nil {
		return err
	}
	if err := loadChildren(ctx, tx, issues); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	for _, issue := range issues {
		if err := enc.Encode(issue); err != nil {
			return err
		}
	}
	return nil
}

func verify(ctx context.Context, db *sql.Tx) error {
	var version string
	if err := db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key = 'bd_version'").Scan(&version); err != nil {
		return fmt.Errorf("legacy SQLite release marker: %w", err)
	}
	if !acceptedVersions[version] {
		return fmt.Errorf("unsupported legacy SQLite release %q", version)
	}
	for table, want := range schema {
		if err := verifyTable(ctx, db, table, want); err != nil {
			return err
		}
	}
	for _, table := range []string{"metadata", "issues", "dependencies", "labels", "comments"} {
		if err := verifyFKs(ctx, db, table); err != nil {
			return err
		}
	}
	return nil
}

func verifyTable(ctx context.Context, db *sql.Tx, table, want string) error {
	rows, err := db.QueryContext(ctx, "PRAGMA table_xinfo("+table+")")
	if err != nil {
		return err
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var cid, hidden int
		var name, typ string
		var notNull, pk int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk, &hidden); err != nil {
			return err
		}
		if hidden != 0 {
			return fmt.Errorf("legacy SQLite schema drift in %s hidden column", table)
		}
		defaultText := "-"
		if defaultValue.Valid {
			defaultText = defaultValue.String
		}
		got = append(got, fmt.Sprintf("%s|%s|%d|%s|%d", name, typ, notNull, defaultText, pk))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if strings.Join(got, " ") != want {
		return fmt.Errorf("legacy SQLite schema drift in %s", table)
	}
	return nil
}

func verifyFKs(ctx context.Context, db *sql.Tx, table string) error {
	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_list("+table+")")
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		if err := verifyForeignKeyRow(rows, table); err != nil {
			return err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if foreignKeyCountInvalid(table, count) {
		return fmt.Errorf("legacy SQLite foreign-key drift in %s", table)
	}
	return nil
}

func verifyForeignKeyRow(rows *sql.Rows, table string) error {
	var id, seq int
	var target, from, to, update, deleteAction, match string
	if err := rows.Scan(&id, &seq, &target, &from, &to, &update, &deleteAction, &match); err != nil {
		return err
	}
	if table == "issues" || table == "metadata" {
		return fmt.Errorf("legacy SQLite foreign-key drift in %s", table)
	}
	if !foreignKeyReferenceMatches(target, from, to, update, deleteAction, match) {
		return fmt.Errorf("legacy SQLite foreign-key drift in %s", table)
	}
	return nil
}

func foreignKeyReferenceMatches(target, from, to, update, deleteAction, match string) bool {
	return target == "issues" && from == "issue_id" && to == "id" && update == "NO ACTION" && deleteAction == "CASCADE" && match == "NONE"
}

func foreignKeyCountInvalid(table string, count int) bool {
	return table != "issues" && table != "metadata" && count != 1
}

// loadIssuesProjection is the SELECT list feeding issueops.ScanIssueFrom in
// loadIssues. It must emit exactly the canonical issueops.IssueSelectColumns
// prefix — columns the legacy schema lacks are projected as NULL/0 — followed
// by the legacy trailing columns scanned via (*legacyExtras).scanDests. That
// canonical prefix is positional (ScanIssueFrom scans it slot-for-slot), so a
// new column in issueops.IssueSelectColumns needs a matching placeholder here;
// the variadic ScanIssueFrom boundary hides any count mismatch from the
// compiler, so TestLoadIssuesProjectionArity guards the invariant and makes the
// drift fail at test time instead of mid-migration.
const loadIssuesProjection = `id,content_hash,title,description,design,acceptance_criteria,notes,status,priority,issue_type,assignee,estimated_minutes,CAST(created_at AS TEXT),created_by,owner,CAST(updated_at AS TEXT),NULL,NULL,external_ref,spec_id,COALESCE(compaction_level,0),NULL,compacted_at_commit,original_size,source_repo,close_reason,NULL,sender,ephemeral,0,wisp_type,pinned,is_template,await_type,await_id,timeout_ns,NULL,mol_type,event_kind,actor,target,payload,NULL,NULL,work_type,source_system,NULL,0,NULL,NULL,NULL,NULL,closed_by_session,CAST(deleted_at AS TEXT),deleted_by,delete_reason,original_type,crystallizes,quality_score,hook_bead,role_bead,agent_state,CAST(last_activity AS TEXT),role_type,rig,metadata,waiters,ephemeral,pinned,is_template,estimated_minutes,compaction_level,original_size,CAST(closed_at AS TEXT),CAST(compacted_at AS TEXT),CAST(due_at AS TEXT),CAST(defer_until AS TEXT)`
