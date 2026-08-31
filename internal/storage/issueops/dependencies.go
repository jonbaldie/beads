package issueops

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage/sqlbuild"
	"github.com/jonbaldie/beads/internal/types"
)

type DepTargetKind int

const (
	DepTargetIssue DepTargetKind = iota
	DepTargetWisp
	DepTargetExternal
)

func (k DepTargetKind) Column() string {
	switch k {
	case DepTargetWisp:
		return "depends_on_wisp_id"
	case DepTargetExternal:
		return "depends_on_external"
	default:
		return "depends_on_issue_id"
	}
}

// DepTargetExpr is the SQL expression that resolves a dependency row's target
// id from its three typed columns. Use this in SELECT projections (aliased as
// depends_on_id) and in WHERE clauses when the caller doesn't know the target
// kind ahead of time.
const DepTargetExpr = sqlbuild.DepTargetExpr

func depTargetExpr(alias string) string {
	if alias == "" {
		return DepTargetExpr
	}
	return fmt.Sprintf("COALESCE(%s.depends_on_issue_id, %s.depends_on_wisp_id, %s.depends_on_external)", alias, alias, alias)
}

func depTargetEquals(alias string) string {
	return depTargetExpr(alias) + " = ?"
}

// depTargetEqualsOr returns a SARGABLE target-equality predicate as an explicit
// per-column OR — `(depends_on_issue_id = ? OR depends_on_wisp_id = ? OR
// depends_on_external = ?)` — binding the target id once per typed column (three
// args, in that column order). Unlike depTargetEquals's COALESCE wrapper (which
// wraps the columns in an expression no index can match), each disjunct is a
// bare indexed column.
//
// On an index-merging planner (e.g. Postgres) this plans as a BitmapOr
// index-merge over idx_dep_{issue,wisp,external}_target (a Bitmap Heap Scan,
// cost ~36 on a 5k-row table) where the COALESCE form is a Seq Scan (cost
// ~104). On Dolt
// (go-mysql-server) the optimizer does not index-merge an OR of distinct columns,
// so the un-ordered COUNT still scans, but the COALESCE form never had any index
// path either; the type-filtered path seeks the (type, target) composite on both.
// Use it for target-keyed reads of a single fixed target.
func depTargetEqualsOr() string {
	return "(depends_on_issue_id = ? OR depends_on_wisp_id = ? OR depends_on_external = ?)"
}

func depTargetIn(alias, placeholders string) string {
	return depTargetExpr(alias) + " IN (" + placeholders + ")"
}

// IsExternalDepTarget reports whether a dependency target is one this database
// cannot hold a row for. Two shapes qualify: an "external:" reference, which
// names something outside beads entirely, and a target whose id prefix names
// ANOTHER REPOSITORY, which lives in that rig's database and not this one.
// Both belong in depends_on_external — the one target column carrying no
// foreign key into issues — so this is the single rule every backend must
// classify by, whether it writes through a tx (ClassifyDepTarget) or through
// the domain repository (db.pickDepTargetColumn).
func IsExternalDepTarget(sourceID, targetID string) bool {
	return strings.HasPrefix(targetID, "external:") ||
		types.ExtractPrefix(sourceID) != types.ExtractPrefix(targetID)
}

// ClassifyDepTarget picks the typed target column for an edge. isCrossPrefix is
// an override for callers that already know the answer from a cached prefix
// set; leaving it false is safe, because IsExternalDepTarget re-derives the
// same comparison from the edge itself.
func ClassifyDepTarget(ctx context.Context, tx DBTX, dep *types.Dependency, isCrossPrefix bool) DepTargetKind {
	if isCrossPrefix || IsExternalDepTarget(dep.IssueID, dep.DependsOnID) {
		return DepTargetExternal
	}
	if IsActiveWispInTx(ctx, tx, dep.DependsOnID) {
		return DepTargetWisp
	}
	return DepTargetIssue
}

// AddDependencyOpts configures AddDependencyInTx behavior.
// When fields are left empty, AddDependencyInTx performs wisp routing
// automatically via IsActiveWispInTx. Callers that have already determined
// routing (e.g., DoltStore with its pre-tx wisp cache) can set fields
// explicitly to skip the redundant DB check.
type AddDependencyOpts struct {
	// SourceTable is the table to validate the source issue exists in.
	// Auto-detected via wisp routing if empty.
	SourceTable string
	// TargetTable is the table to validate the target issue exists in.
	// Auto-detected via wisp routing if empty. Ignored when target validation is skipped.
	TargetTable string
	// WriteTable is the dependency table to insert/update/check existing deps in.
	// Auto-detected from source wisp routing if empty.
	WriteTable string
	// DepTables are the tables to scan for cycle detection. Defaults to both
	// dependency tables; edge storage is source-routed, so same-class endpoints
	// can still have mixed-table interior paths.
	DepTables []string
	// IsCrossPrefix is true when source and target have different prefixes,
	// meaning the target lives in another rig's database.
	IsCrossPrefix bool
	// SkipCycleCheck skips the recursive pre-insert cycle check for callers
	// that intentionally trade validation cost for bulk graph wiring speed.
	SkipCycleCheck bool
	TargetKind     *DepTargetKind
	// PrecheckedTarget, when non-nil, replaces the in-tx target read: the
	// target is known to exist with this type and status. Callers that set
	// it must also set TargetKind, since target classification otherwise
	// queries tx too.
	PrecheckedTarget *DepTargetPrecheck
	// EmitEvent records a dependency_added event on the source's event table
	// for a genuine new edge. Only the explicit dependency verbs (bd dep add /
	// bd link and their bulk twin) set it; create-with-deps and structural
	// callers leave it unset so an implicit parent-child / --deps / waits-for
	// edge produces no event. This mirrors the proxied repository's
	// DepInsertOpts.EmitEvent gate so both backends record identical history.
	EmitEvent bool
}

// DepTargetPrecheck carries a target-issue row the caller has already read
// from the transaction that can see it. Dolt server mode splits one logical
// transaction across two SQL sessions (versioned tables vs dolt-ignored wisp
// tables); when the dependency write table lives on one session and the
// target issue on the other, a target read on tx misses rows created earlier
// in the same logical transaction. The caller reads the target on its own
// session and passes the row here; existence validation, cross-type blocking
// validation, and the direct is_blocked mark then use these values instead
// of querying the target table on tx.
type DepTargetPrecheck struct {
	IssueType string
	Status    string
}
