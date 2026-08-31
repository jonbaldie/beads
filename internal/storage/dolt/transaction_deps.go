package dolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

type transactionFlags struct {
	wroteRegularDep bool
	wroteWispDep    bool
	lifecycle       bool
	// journalPinned records that ignoredTx IS regularTx because the events
	// journal collapsed the two planes into one transaction, so the finish path
	// must not commit or roll it back a second time.
	journalPinned bool
}

func markTransactionLifecycle(t *doltTransaction) { t.flags.lifecycle = true }

func transactionHasLifecycle(t *doltTransaction) bool { return t.flags.lifecycle }

func setTransactionJournalPinned(t *doltTransaction, pinned bool) { t.flags.journalPinned = pinned }

func transactionJournalPinned(t *doltTransaction) bool { return t.flags.journalPinned }

func (t *transactionRuntime) txFor(table string) *sql.Tx {
	if table == "wisps" || strings.HasPrefix(table, "wisp_") ||
		table == "local_metadata" || table == "repo_mtimes" {
		return t.resources.ignoredTx
	}
	return t.resources.regularTx
}

// isActiveWisp checks if an ID exists in the wisps table within the transaction.
// Unlike the store-level isActiveWisp, this queries within the transaction so it
// sees uncommitted wisps. Handles both -wisp- pattern and explicit-ID ephemerals (GH#2053).
func (t *transactionRuntime) isActiveWisp(ctx context.Context, id string) bool {
	var exists int
	err := t.resources.ignoredTx.QueryRowContext(ctx, "SELECT 1 FROM wisps WHERE id = ? LIMIT 1", id).Scan(&exists)
	return err == nil
}

// AddDependency adds a dependency within the transaction.
func (t *transactionDependencyAdd) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	return t.AddDependencyWithOptions(ctx, dep, actor, storage.DependencyAddOptions{})
}

type doltDependencyPlan struct {
	table           string
	eventTable      string
	targetTable     string
	kind            issueops.DepTargetKind
	crossTierTarget bool
	opts            issueops.AddDependencyOpts
}

func (t *transactionDependencyAdd) AddDependencyWithOptions(ctx context.Context, dep *types.Dependency, actor string, addOpts storage.DependencyAddOptions) error {
	plan := t.buildDoltDependencyPlan(ctx, dep, addOpts)
	if err := t.precheckDoltDependencyTarget(ctx, dep, &plan); err != nil {
		return err
	}
	if err := t.checkDoltDependencyCycle(ctx, dep, &plan); err != nil {
		return err
	}
	return t.writeDoltDependency(ctx, dep, actor, plan)
}

func (t *transactionDependencyAdd) buildDoltDependencyPlan(ctx context.Context, dep *types.Dependency, addOpts storage.DependencyAddOptions) doltDependencyPlan {
	table := "dependencies"
	sourceTable := "issues"
	eventTable := "events"
	if t.isActiveWisp(ctx, dep.IssueID) {
		table = "wisp_dependencies"
		sourceTable = "wisps"
		eventTable = "wisp_events"
	}

	isCrossPrefix := isCrossPrefixDep(dep.IssueID, dep.DependsOnID)
	targetTable := "issues"
	kind := issueops.DepTargetIssue
	switch {
	case isCrossPrefix, strings.HasPrefix(dep.DependsOnID, "external:"):
		kind = issueops.DepTargetExternal
	default:
		if t.isActiveWisp(ctx, dep.DependsOnID) {
			targetTable = "wisps"
			kind = issueops.DepTargetWisp
		}
	}

	opts := issueops.AddDependencyOpts{
		SourceTable:    sourceTable,
		TargetTable:    targetTable,
		WriteTable:     table,
		IsCrossPrefix:  isCrossPrefix,
		SkipCycleCheck: addOpts.SkipCycleCheck,
		TargetKind:     &kind,
		EmitEvent:      addOpts.EmitEvent,
	}
	return doltDependencyPlan{
		table:           table,
		eventTable:      eventTable,
		targetTable:     targetTable,
		kind:            kind,
		crossTierTarget: kind != issueops.DepTargetExternal && t.txFor(targetTable) != t.txFor(table),
		opts:            opts,
	}
}

func (t *transactionDependencyAdd) precheckDoltDependencyTarget(ctx context.Context, dep *types.Dependency, plan *doltDependencyPlan) error {
	// Regular and dolt-ignored tables run on separate SQL sessions, so when
	// the edge's write table and its target issue live in different tiers,
	// target reads on the write tx cannot see a target created earlier in
	// this same logical transaction (e.g. `bd create --deps blocks:<id>`
	// swapping the new issue into the target slot). Read the target on its
	// own tx and hand the row to AddDependencyInTx.
	if plan.crossTierTarget {
		precheck, err := t.readDepTargetForPrecheck(ctx, dep.IssueID, plan.targetTable, dep.DependsOnID)
		if err != nil {
			return err
		}
		plan.opts.PrecheckedTarget = precheck
	}
	return nil
}

func (t *transactionDependencyAdd) checkDoltDependencyCycle(ctx context.Context, dep *types.Dependency, plan *doltDependencyPlan) error {
	// The single-session in-tx cycle check only sees its own session's
	// uncommitted rows. Fall back to the merged two-session check whenever a
	// scheduling cycle could hide on the other session: either this edge itself
	// crosses tiers, or a dependency row was already written to the other tier
	// earlier in this logical transaction. The latter covers a create-time
	// batch like `blocks:<wisp>,depends-on:<regular>`, where the cross-tier
	// `blocks` edge is pending on the ignored session and the same-tier
	// `depends-on` edge would otherwise close the cycle unseen.
	if plan.opts.SkipCycleCheck || (!plan.crossTierTarget && !t.otherDepTierPending(plan.table)) {
		return nil
	}
	if err := t.checkCrossTierSchedulingCycle(ctx, dep); err != nil {
		return err
	}
	plan.opts.SkipCycleCheck = true
	return nil
}

func (t *transactionDependencyAdd) writeDoltDependency(ctx context.Context, dep *types.Dependency, actor string, plan doltDependencyPlan) error {
	var addErr error
	var eventWritten bool
	if plan.opts.PrecheckedTarget != nil && plan.table == "wisp_dependencies" && plan.kind == issueops.DepTargetIssue {
		eventWritten, addErr = t.addWispDepSuspendingIssueTargetFK(ctx, dep, actor, plan.opts)
	} else {
		eventWritten, addErr = issueops.AddDependencyInTx(ctx, t.txFor(plan.table), dep, actor, plan.opts)
	}
	if addErr != nil {
		return addErr
	}
	t.resources.dirty.MarkDirty(plan.table)
	// AddDependencyInTx records a dependency_added event on the source's event
	// table only for a genuine emit (explicit verb + new edge); stage that table
	// so StageAndCommit commits the event with the edge (a torn write otherwise
	// leaves the event in the working set, dropped on reset). A structural or
	// idempotent add writes no event, so leave eventTable unstaged.
	if eventWritten {
		t.resources.dirty.MarkDirty(plan.eventTable)
	}
	t.recordDepTierWrite(plan.table)
	return nil
}

// otherDepTierPending reports whether a dependency row was written to the tier
// opposite writeTable earlier in this logical transaction. Because the regular
// and wisp dependency tables run on separate SQL sessions, an in-tx cycle check
// on writeTable's session cannot see the other session's uncommitted scheduling
// edges; when the other tier has pending writes the caller must use the merged
// two-session cycle check instead.
func (t *transactionDependencyAdd) otherDepTierPending(writeTable string) bool {
	if writeTable == "wisp_dependencies" {
		return t.flags.wroteRegularDep
	}
	return t.flags.wroteWispDep
}

// recordDepTierWrite notes that a dependency row was written to writeTable's
// tier so a later same-tier edge on the opposite session can detect that the
// merged cycle check is required. See otherDepTierPending.
func (t *transactionDependencyAdd) recordDepTierWrite(writeTable string) {
	if writeTable == "wisp_dependencies" {
		t.flags.wroteWispDep = true
		return
	}
	t.flags.wroteRegularDep = true
}

// addWispDepSuspendingIssueTargetFK inserts a wisp-source dependency whose
// target is a regular issue created earlier in this logical transaction.
// wisp_dependencies carries a real FK (depends_on_issue_id -> issues), and
// the ignored session cannot see an issues row still uncommitted on the
// regular session, so the insert would fail FK validation even though the
// target's existence was just validated on the regular tx. The regular tx
// commits before the ignored tx, so the committed end-state always satisfies
// the FK; suspend the session's FK checks around this one statement scope.
func (t *transactionDependencyAdd) addWispDepSuspendingIssueTargetFK(ctx context.Context, dep *types.Dependency, actor string, opts issueops.AddDependencyOpts) (bool, error) {
	if _, err := t.resources.ignoredTx.ExecContext(ctx, "SET foreign_key_checks = 0"); err != nil {
		return false, fmt.Errorf("suspend foreign key checks for cross-tier dependency: %w", err)
	}
	eventWritten, addErr := issueops.AddDependencyInTx(ctx, t.resources.ignoredTx, dep, actor, opts)
	if _, err := t.resources.ignoredTx.ExecContext(ctx, "SET foreign_key_checks = 1"); err != nil && addErr == nil {
		addErr = fmt.Errorf("restore foreign key checks after cross-tier dependency: %w", err)
	}
	return eventWritten, addErr
}

// readDepTargetForPrecheck validates a dependency target on the transaction
// that owns its table and returns the row fields AddDependencyInTx needs when
// it cannot read the target itself (cross-tier edges).
func (t *transactionDependencyAdd) readDepTargetForPrecheck(ctx context.Context, sourceID, targetTable, id string) (*issueops.DepTargetPrecheck, error) {
	var p issueops.DepTargetPrecheck
	//nolint:gosec // G201: targetTable is "issues" or "wisps"
	err := t.txFor(targetTable).QueryRowContext(ctx,
		fmt.Sprintf(`SELECT issue_type, status FROM %s WHERE id = ?`, targetTable), id,
	).Scan(&p.IssueType, &p.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, issueops.MissingDependencyTarget(sourceID, id)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to check target issue existence: %w", err)
	}
	return &p, nil
}

// checkCrossTierSchedulingCycle rejects a scheduling edge that would close a
// cycle, using the merged view of both sessions' dependency tables. The in-tx
// cycle check scans both tables on the write tx and so misses edges added on
// the other session earlier in this logical transaction.
//
// The set is types.IsSchedulingEdge's, by call and not by restatement: an
// inline copy that missed a fifth scheduling type would fall through to
// "not a scheduling edge" and skip this gate entirely, which is silence rather
// than a failure — the whole reason that predicate was consolidated (ga-2ltro.10).
func (t *transactionDependencyAdd) checkCrossTierSchedulingCycle(ctx context.Context, dep *types.Dependency) error {
	if !types.IsSchedulingEdge(dep.Type) {
		return nil
	}
	cycle, err := t.CycleThroughEdges(ctx, [][2]string{{dep.IssueID, dep.DependsOnID}})
	if err != nil {
		return err
	}
	if cycle != "" {
		return domain.ErrDependencyCycle
	}
	return nil
}

// CycleThroughEdges reports a scheduling cycle through one of the new edges.
// The graph merges the regular tx's dependencies with the ignored tx's
// wisp_dependencies, so uncommitted writes on both sides are gated — the
// previous DetectCycles ran only on the regular tx and let bulk wisp edges
// commit scheduling cycles (bd-578h9.9).
func (t *transactionDependencyRead) CycleThroughEdges(ctx context.Context, edges [][2]string) (string, error) {
	graph := make(map[string][]string)
	if err := issueops.AppendSchedulingGraphInTx(ctx, t.txFor("dependencies"), []string{"dependencies"}, graph); err != nil {
		return "", err
	}
	if err := issueops.AppendSchedulingGraphInTx(ctx, t.txFor("wisp_dependencies"), []string{"wisp_dependencies"}, graph); err != nil {
		return "", err
	}
	return issueops.CycleThroughEdgesInGraph(graph, edges), nil
}

func (t *transactionDependencyRead) GetDependencyRecords(ctx context.Context, issueID string) ([]*types.Dependency, error) {
	table := "dependencies"
	if t.isActiveWisp(ctx, issueID) {
		table = "wisp_dependencies"
	}

	//nolint:gosec // G201: table is hardcoded
	rows, err := t.txFor(table).QueryContext(ctx, fmt.Sprintf(`
		SELECT issue_id, %s AS depends_on_id, type, created_at, created_by, metadata, thread_id
		FROM %s
		WHERE issue_id = ?
	`, issueops.DepTargetExpr, table), issueID)
	if err != nil {
		return nil, wrapQueryError("get dependency records in tx", err)
	}
	defer rows.Close()

	var deps []*types.Dependency
	for rows.Next() {
		var d types.Dependency
		var metadata sql.NullString
		var threadID sql.NullString
		if err := rows.Scan(&d.IssueID, &d.DependsOnID, &d.Type, &d.CreatedAt, &d.CreatedBy, &metadata, &threadID); err != nil {
			return nil, wrapScanError("get dependency records in tx", err)
		}
		if metadata.Valid {
			d.Metadata = metadata.String
		}
		if threadID.Valid {
			d.ThreadID = threadID.String
		}
		deps = append(deps, &d)
	}
	return deps, rows.Err()
}

func (t *transactionDependencyWrite) RemoveDependency(ctx context.Context, issueID, dependsOnID string, actor string) error {
	return t.RemoveDependencyWithOptions(ctx, issueID, dependsOnID, actor, storage.DependencyRemoveOptions{})
}

func (t *transactionDependencyWrite) RemoveDependencyWithOptions(ctx context.Context, issueID, dependsOnID string, actor string, rmOpts storage.DependencyRemoveOptions) error {
	table := "dependencies"
	eventTable := "events"
	if t.isActiveWisp(ctx, issueID) {
		table = "wisp_dependencies"
		eventTable = "wisp_events"
	}
	eventWritten, err := issueops.RemoveDependencyInTx(ctx, t.txFor(table), issueID, dependsOnID, actor, rmOpts.EmitEvent)
	if err != nil {
		return wrapExecError("remove dependency in tx", err)
	}
	t.resources.dirty.MarkDirty(table)
	// RemoveDependencyInTx records a dependency_removed event on the source's
	// event table only for a genuine emit (explicit verb + edge removal); stage
	// that table so it commits with the edge. A structural or missing-edge remove
	// writes no event, so leave eventTable unstaged.
	if eventWritten {
		t.resources.dirty.MarkDirty(eventTable)
	}
	return nil
}
