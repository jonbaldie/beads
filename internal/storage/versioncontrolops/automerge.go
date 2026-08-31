package versioncontrolops

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// Domain-aware auto-merge (federation ask #1, the flagship).
//
// Dolt merges disjoint writes cleanly, but beads stamps `issues.updated_at` on
// EVERY mutation, so any two edits to the same issue on two replicas between
// syncs collide on that cell — even when the semantic fields are disjoint
// (machine A adds a comment, machine B adds a label, and the issues row
// conflicts on nothing but the timestamp both bumped). The observed conflict
// rate is therefore far higher than the semantic-conflict rate, and the
// original row-level LWW resolver could only take the safe half of it: it
// declined whenever BOTH sides had moved updated_at past the merge base,
// because taking one side's whole row would silently drop the other side's
// field-level edits.
//
// This file replaces that with a FIELD-level three-way merge, which encodes
// beads' actual write semantics:
//
//   - a column only one side changed relative to the merge base keeps that
//     side's value — no edit is dropped, whatever the timestamps say;
//   - a column both sides changed to DIFFERENT values is the only genuine
//     conflict, and it is settled last-write-wins by `updated_at` (the ask's
//     rule for status/assignee/updated_at, applied uniformly);
//   - `updated_at` itself therefore merges to max(ours, theirs), since
//     whichever side is newer either wins the cell outright (both moved) or is
//     the only side that moved it.
//
// Two carve-outs keep per-cell independence from inventing states bd's own
// write paths cannot produce (see issuesCloseGroup and issuesNonScalarColumns
// below): the close columns move atomically, and a contested `notes` or
// `metadata` declines rather than letting LWW delete an append or a JSON key.
//
// A row is left for the operator when it is not modify/modify (add/add,
// delete/modify), when a genuinely conflicting cell cannot be settled because
// the two sides' `updated_at` values are equal or unparseable — the ambiguity
// LWW has no answer for — or when one of those carve-outs applies.
//
// The companion tables merge by the semantics the ask names:
//
//   - labels: SET-UNION. `labels` is all key columns (issue_id, label), so
//     two sides adding DIFFERENT labels are disjoint rows dolt already unions,
//     and a conflict can only mean the same (issue_id, label) on both sides —
//     identical data, resolvable by keeping it.
//   - comments/events: APPEND-ONLY UNION. Rows are insert-only and keyed by a
//     per-machine-unique id, so creation is disjoint and dolt unions it; a
//     same-id conflict whose columns agree is the same append on both sides
//     and is likewise resolvable by keeping it.
//
// For all three, a conflict where the row is missing on one side (a deletion
// racing an insert — compaction, or a label removal) or where the columns of a
// supposedly immutable row disagree is NOT unioned: it goes to the operator,
// because both "presence wins" and "deletion wins" would silently discard a
// real intent.

// unionConflictKeyColumns lists the primary-key columns of the tables merged by
// union semantics. The key columns are what identify a conflicted row for the
// dolt_conflicts_<table> delete that signals resolution.
var unionConflictKeyColumns = map[string][]string{
	"labels":   {"issue_id", "label"},
	"comments": {"id"},
	"events":   {"id"},
}

// issuesKeyColumn is the issues-table primary key, used both to identify a
// conflicted row and to exclude the key from the merge write-back.
const issuesKeyColumn = "id"

// loadConflictRows reads every live conflict row of table in raw scanned form.
func loadConflictRows(ctx context.Context, db DBConn, table string) ([]rawConflictRow, error) {
	if err := ValidateConflictTable(table); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, "SELECT * FROM `dolt_conflicts_"+table+"`") //nolint:gosec // table validated as an identifier above
	if err != nil {
		return nil, fmt.Errorf("query conflicts for table %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("conflict columns for table %s: %w", table, err)
	}
	var out []rawConflictRow
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("scan conflict row for table %s: %w", table, err)
		}
		out = append(out, rawConflictRow{cols: cols, vals: vals})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conflicts for table %s: %w", table, err)
	}
	return out, nil
}

// duplicateConflictKey reports the first our-side key held by more than one
// live conflict row. Both resolvers settle a row by deleting its conflict BY
// KEY, so two rows sharing one key would both be cleared by the first delete
// and make the second iteration abort on "no conflict row deleted" — a message
// about the wrong thing entirely, after a row was resolved without ever being
// merged. loadConflictRow refuses the same shape on the operator's single-row
// path (conflicts.go); the auto-merge pre-screens instead DECLINE on it, which
// is this file's idiom and what lets the caller still build the
// MergeConflictsError that tells an operator which tables need them.
//
// Rows whose our-side key is absent are skipped: a delete/modify conflict NULLs
// our whole side, such a row is never the target of a keyed delete (the safety
// checks decline it first), and treating several of them as one repeated key
// would decline merges that are perfectly settleable.
//
// Keys are compared by their rendered bytes, which is stricter than the
// collation the DELETE itself matches under: a case-insensitive collation could
// still let two rows the guard reads as distinct be deleted together. Dolt's
// default is a binary collation and the beads schema pins no other, so the two
// agree today.
func duplicateConflictKey(keyCols []string, rows []rawConflictRow) (string, bool) {
	if len(keyCols) == 0 {
		return "", false
	}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		parts := make([]string, 0, len(keyCols))
		for _, k := range keyCols {
			v, has := row.value("our", k)
			if !has {
				break
			}
			s := formatConflictValue(v)
			if s == nil {
				break
			}
			parts = append(parts, *s)
		}
		if len(parts) != len(keyCols) {
			continue
		}
		key := strings.Join(parts, "\x00")
		if seen[key] {
			return strings.Join(parts, "/"), true
		}
		seen[key] = true
	}
	return "", false
}

// declineDuplicateConflictRows reports (and explains) a duplicate-key decline.
// The reason is otherwise undiagnosable: the caller only learns that the table
// was not auto-merged, the same courtesy the resolver pays a superseded cell.
func declineDuplicateConflictRows(table string, keyCols []string, rows []rawConflictRow) bool {
	dup, ok := duplicateConflictKey(keyCols, rows)
	if !ok {
		return false
	}
	fmt.Fprintf(os.Stderr,
		"Notice: not auto-merging %s; several live conflict rows share the key %s, which must be resolved by hand\n",
		table, dup)
	return true
}

// dataColumns returns the row's data column names (conflict metadata and the
// named excluded columns dropped), in conflict-table order and de-duplicated.
// A column is only reported when the row actually carries a value for it on
// every side that matters; callers read the sides they need with value().
func (r rawConflictRow) dataColumns(exclude ...string) []string {
	skip := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		skip[e] = true
	}
	seen := make(map[string]bool, len(r.cols))
	var out []string
	for _, c := range r.cols {
		side, field, ok := splitConflictColumn(c)
		if !ok || side != "our" || conflictMetaSuffixes[field] || skip[field] || seen[field] {
			continue
		}
		seen[field] = true
		out = append(out, field)
	}
	return out
}

// sidesPresent reports whether the row exists on the base, our, and their
// sides, judged by the key columns (a dolt conflict row NULLs out every column
// of a side that has no row).
func (r rawConflictRow) sidesPresent(keyCols []string) (base, ours, theirs bool) {
	present := func(side string) bool {
		for _, k := range keyCols {
			v, ok := r.value(side, k)
			if !ok || v == nil {
				return false
			}
		}
		return true
	}
	return present("base"), present("our"), present("their")
}

// conflictCellsEqual compares two raw conflict cell values through the same
// normalization the presentation layer uses, so a driver returning []byte for
// one side and string for the other does not read as a difference. SQL NULL is
// distinct from the empty string.
func conflictCellsEqual(a, b any) bool {
	x, y := formatConflictValue(a), formatConflictValue(b)
	if x == nil || y == nil {
		return x == nil && y == nil
	}
	return *x == *y
}

// conflictTimestampLayouts are the shapes an `updated_at` cell can arrive in:
// RFC3339 (what formatConflictValue renders a driver-parsed time.Time as) and
// the two MySQL DATETIME text forms (drivers configured without parseTime).
var conflictTimestampLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
}

// parseConflictTimestamp parses a raw conflict cell as a timestamp. ok is
// false for NULL or any unrecognized shape — an unparseable timestamp must
// make LWW decline, never guess.
func parseConflictTimestamp(v any) (time.Time, bool) {
	if t, isTime := v.(time.Time); isTime {
		return t.UTC(), true
	}
	s := formatConflictValue(v)
	if s == nil || *s == "" {
		return time.Time{}, false
	}
	for _, layout := range conflictTimestampLayouts {
		if t, err := time.Parse(layout, *s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// issuesCloseGroup are the columns beads always writes together: `bd close`
// sets status/closed_at/close_reason/closed_by_session in one statement and
// `bd reopen` clears them in one statement (issueops/close.go, reopen.go), and
// types.Issue.Validate enforces the biconditional "closed iff closed_at". Cell
// independence is wrong for them: our close and their status change would
// otherwise merge into `status='in_progress' AND closed_at=<t>`, a row no
// write path can produce and validation rejects. When any of them is
// contested the whole group is settled from the LWW winner, atomically.
var issuesCloseGroup = []string{"status", "closed_at", "close_reason", "closed_by_session"}

// issuesNonScalarColumns are columns whose contents are structurally merged by
// bd's own write paths, so per-cell LWW would silently destroy one side's
// work: `notes` is append-only (`bd note` = --append-notes) and `metadata` is
// a JSON object mutated key-wise. Comments and events get append-only union
// treatment for exactly this reason; these two live inside the issues row
// where cell-level merge cannot express a union, so a genuinely contested one
// DECLINES to the operator rather than dropping an append.
var issuesNonScalarColumns = map[string]bool{
	"notes":    true,
	"metadata": true,
}

// cellVerdict classifies one cell against the merge base.
type cellVerdict int

const (
	cellAgree      cellVerdict = iota // both sides hold the same value
	cellOursOnly                      // only we changed it
	cellTheirsOnly                    // only they changed it
	cellContested                     // both sides changed it, differently
)
