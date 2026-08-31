package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/storage/sqlbuild"
	"github.com/jonbaldie/beads/internal/types"
)

func (r *issueSearchHydrator) hydrateIssues(ctx context.Context, issues []*types.Issue, tables filterTables, includeDeps bool, skipLabels bool) error {
	if len(issues) == 0 {
		return nil
	}

	ids := make([]string, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
	}

	if !skipLabels {
		if err := r.hydrateIssueLabels(ctx, issues, tables.Labels, ids); err != nil {
			return err
		}
	}

	if includeDeps {
		if err := r.hydrateIssueDependencies(ctx, issues, tables.Dependencies, ids); err != nil {
			return err
		}
	}

	return nil
}

func (r *issueSearchHydrator) hydrateIssueLabels(ctx context.Context, issues []*types.Issue, labelTable string, ids []string) error {
	labelMap, err := r.getLabelsFromTable(ctx, labelTable, ids)
	if err != nil {
		return fmt.Errorf("hydrate labels: %w", err)
	}
	for _, issue := range issues {
		if labels, ok := labelMap[issue.ID]; ok {
			issue.Labels = labels
		}
	}
	return nil
}

func (r *issueSearchHydrator) hydrateIssueDependencies(ctx context.Context, issues []*types.Issue, depTable string, ids []string) error {
	depMap, err := r.getDependencyRecordsFromTable(ctx, depTable, ids)
	if err != nil {
		return fmt.Errorf("hydrate dependencies: %w", err)
	}
	for _, issue := range issues {
		if deps, ok := depMap[issue.ID]; ok {
			issue.Dependencies = deps
		}
	}
	return nil
}

//nolint:gosec // G201: labelTable is "labels" or "wisp_labels" (hardcoded by callers).
func (r *issueSearchHydrator) getLabelsFromTable(ctx context.Context, labelTable string, ids []string) (map[string][]string, error) {
	result := make(map[string][]string)
	idsLen := len(ids)
	for start := 0; start < idsLen; start += queryBatchSize {
		end := start + queryBatchSize
		if end > idsLen {
			end = idsLen
		}
		placeholders, args := buildInPlaceholders(ids[start:end])
		rows, err := r.runner.QueryContext(ctx, fmt.Sprintf(
			`SELECT issue_id, label FROM %s WHERE issue_id IN (%s) ORDER BY issue_id, label`,
			labelTable, placeholders), args...)
		if err != nil {
			return nil, fmt.Errorf("get labels from %s: %w", labelTable, err)
		}
		for rows.Next() {
			var issueID, label string
			if err := rows.Scan(&issueID, &label); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("get labels: scan: %w", err)
			}
			result[issueID] = append(result[issueID], label)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get labels: rows: %w", err)
		}
	}
	return result, nil
}

//nolint:gosec // G201: depTable is "dependencies" or "wisp_dependencies" (hardcoded by callers).
func (r *issueSearchHydrator) getDependencyRecordsFromTable(ctx context.Context, depTable string, ids []string) (map[string][]*types.Dependency, error) {
	result := make(map[string][]*types.Dependency)
	idsLen := len(ids)
	for start := 0; start < idsLen; start += queryBatchSize {
		end := start + queryBatchSize
		if end > idsLen {
			end = idsLen
		}
		placeholders, args := buildInPlaceholders(ids[start:end])
		rows, err := r.runner.QueryContext(ctx, fmt.Sprintf(
			`SELECT issue_id, %s AS depends_on_id, type, created_at, created_by, metadata, thread_id
			 FROM %s WHERE issue_id IN (%s) ORDER BY issue_id`,
			depTargetExpr, depTable, placeholders), args...)
		if err != nil {
			return nil, fmt.Errorf("get dep records from %s: %w", depTable, err)
		}
		for rows.Next() {
			dep, scanErr := scanDepRow(rows)
			if scanErr != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("get dep records: scan: %w", scanErr)
			}
			result[dep.IssueID] = append(result[dep.IssueID], dep)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get dep records: rows: %w", err)
		}
	}
	return result, nil
}

func scanDepRow(rows *sql.Rows) (*types.Dependency, error) {
	var dep types.Dependency
	var createdAt sql.NullTime
	var createdBy, metadata, threadID sql.NullString
	if err := rows.Scan(&dep.IssueID, &dep.DependsOnID, &dep.Type, &createdAt, &createdBy, &metadata, &threadID); err != nil {
		return nil, err
	}
	if createdAt.Valid {
		dep.CreatedAt = createdAt.Time
	}
	if createdBy.Valid {
		dep.CreatedBy = createdBy.String
	}
	if metadata.Valid {
		dep.Metadata = metadata.String
	}
	if threadID.Valid {
		dep.ThreadID = threadID.String
	}
	return &dep, nil
}

func wispsTableEmptyOrMissing(ctx context.Context, runner Runner) (bool, error) {
	var probe int
	err := runner.QueryRowContext(ctx, "SELECT 1 FROM wisps LIMIT 1").Scan(&probe)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, sql.ErrNoRows):
		return true, nil
	case dberrors.IsTableNotExist(err):
		return true, nil
	default:
		return false, err
	}
}

func buildLabelDrivenSearch(filter types.IssueFilter, tables filterTables) sqlbuild.LabelSearchPlan {
	return sqlbuild.BuildLabelDrivenSearch(filter, tables)
}

func buildIssueFilterClauses(query string, filter types.IssueFilter, tables filterTables) ([]string, []any, error) {
	return sqlbuild.BuildIssueFilterClauses(query, filter, tables)
}

type idSrcPage struct {
	ordered  []idSrcRef
	issueIDs []string
	wispIDs  []string
}

// scanIDSrcPage reads the (id, src) projection of a UNION ALL over the issues
// and wisps legs, in the order the outer ORDER BY produced it, and collapses
// the duplicates. There are two kinds and they are not the same event.
//
// A REPEAT WITHIN ONE LEG is ordinary: a dependency or label subquery can match
// the same row twice. The first occurrence wins, exactly as the per-table scans
// resolve it.
//
// A CROSS-TABLE DUPLICATE is corruption — one id resident in both planes. No
// local write path can produce it: issueops.EnsureIssueIDAvailableInTx probes
// BOTH tables before every insert, and promotion moves a row inside one
// transaction. Replication can, by merging a durable row into a clone that
// still holds the wisp.
//
// THE WISP COPY IS CANONICAL AND THE READ STILL ANSWERS. This used to be a hard
// error for the whole query, which made a store with one corrupt id unable to
// answer any question about the other rows. Three things say that was the wrong
// verdict. The per-table seam has always resolved it the other way and says why
// — "hard-erroring breaks every lookup city-wide" (issueops.searchInTx, and the
// three sibling merges beside it, be-iabdi). `bd doctor` detects the state with
// a query of its own rather than by watching reads fail, so nothing is blinded
// by answering; and its fix removes the stale ISSUES copy, calling the wisps row
// canonical. And its own check text names the hard failure as the damage:
// "stale issues-table copies break every lookup for the affected IDs".
//
// So the durable row is dropped and the wisp row is kept at its own position,
// which is the page the per-table seam produces for the same data. A page can
// come back one row short of its limit when this fires; that is a corrupt row
// being withheld, not a paging bug, and the repair is a doctor run.
//
// THE DROP REACHES ONLY THE SCANNED WINDOW, which is narrower than "wherever it
// sits" — the phrase this used to use. Both copies have to be inside the rows
// this query returned for the shadow to be seen at all, and under a bounded
// search they need not be: legs carry the outer bound now (legWindowSQL), so a
// durable copy inside the window whose wisp twin ranks past it survives the
// scan and is reported. That is a corrupt store answering approximately, not a
// second bug; the detector is `bd doctor`, not this loop.
func scanIDSrcPage(rows *sql.Rows) (idSrcPage, error) {
	scanned, shadowed, err := scanIDSrcRefs(rows)
	if err != nil {
		return idSrcPage{}, err
	}
	return buildIDSrcPage(scanned, shadowed), nil
}

func scanIDSrcRefs(rows *sql.Rows) ([]idSrcRef, map[string]bool, error) {
	defer func() { _ = rows.Close() }()

	var scanned []idSrcRef
	seen := make(map[idSrcRef]bool)
	shadowed := make(map[string]bool)
	for rows.Next() {
		var id, src string
		if err := rows.Scan(&id, &src); err != nil {
			return nil, nil, fmt.Errorf("scan: %w", err)
		}
		ref := idSrcRef{id: id, src: src}
		if src == "w" {
			shadowed[id] = true
		}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		scanned = append(scanned, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("rows: %w", err)
	}
	return scanned, shadowed, nil
}

func buildIDSrcPage(scanned []idSrcRef, shadowed map[string]bool) idSrcPage {
	// The wisps leg is known in full only after the scan — the outer ORDER BY
	// can place either copy first — so the drop is a second pass rather than a
	// decision taken row by row.
	var page idSrcPage
	for _, ref := range scanned {
		if ref.src == "i" && shadowed[ref.id] {
			continue
		}
		page.ordered = append(page.ordered, ref)
		switch ref.src {
		case "i":
			page.issueIDs = append(page.issueIDs, ref.id)
		case "w":
			page.wispIDs = append(page.wispIDs, ref.id)
		}
	}
	return page
}

func orderByIDs[T any](ids []string, byID map[string]T) []T {
	out := make([]T, 0, len(ids))
	for _, id := range ids {
		if v, ok := byID[id]; ok {
			out = append(out, v)
		}
	}
	return out
}

func reassembleBySrc[T comparable](ordered []idSrcRef, issues, wisps map[string]T) []T {
	var zero T
	out := make([]T, 0, len(ordered))
	for _, p := range ordered {
		var v T
		switch p.src {
		case "i":
			v = issues[p.id]
		case "w":
			v = wisps[p.id]
		}
		if v != zero {
			out = append(out, v)
		}
	}
	return out
}

// sortGoSide establishes the order for a sort key SQL renders no ORDER BY for,
// BEFORE finishWindow's trim decides which rows the page keeps.
//
// It is the union seam's copy of issueops.sortMergedResults, and it exists for
// the identical reason that one gives: "a concatenation of two independently-
// ordered legs" trimmed without a re-sort "keeps an arbitrary prefix of the
// concatenation" rather than the top-n. Under a Go-side sort the legs are not
// merely independently ordered, they are UNORDERED — searchWindowFor pushes no
// bound in that case precisely so the whole matching set arrives here — and
// this is where the requested order first exists.
//
// The only such key is "id", and the id is on the ref already, so no row has to
// be hydrated to be placed. The comparison is the byte order SQL would apply
// and the byte order sqlbuild.Less applies for this key; the natural-numeric
// order a human reads (bd-9 before bd-10) is applied above, on the delivered
// page, by workapi.CompareIssuesBy. This decides MEMBERSHIP; that decides
// DISPLAY.
//
// It honors sortDesc, as sqlbuild.Less and the per-table seam's
// sortRowsGoSide now also do for this key (both were once live sibling bugs
// this comment named; bd-jao3t/bd-69c1a closed them).
func (p *idSrcPage) SortGoSide(sortBy string, sortDesc bool) {
	if !sqlbuild.IsGoSideSort(sortBy) || len(p.ordered) <= 1 {
		return
	}
	sort.SliceStable(p.ordered, func(i, j int) bool {
		return sqlbuild.LessID(p.ordered[i].id, p.ordered[j].id, sortDesc)
	})
	p.issueIDs = p.issueIDs[:0]
	p.wispIDs = p.wispIDs[:0]
	for _, r := range p.ordered {
		switch r.src {
		case "i":
			p.issueIDs = append(p.issueIDs, r.id)
		case "w":
			p.wispIDs = append(p.wispIDs, r.id)
		}
	}
}

// finishWindow is finishWindow's shape for a merged id page: the same cap,
// skip and trim, with the per-table id lists rebuilt once from whatever
// survives.
func (p *idSrcPage) FinishWindow(w searchWindow) (bool, error) {
	if err := issueops.EnforceMaxRowsCap(len(p.ordered), w.rowCap, w.capSource); err != nil {
		return false, err
	}
	ordered, hasMore := applyN1Overflow(dropFirst(p.ordered, w.skip), w.limit)
	if len(ordered) == len(p.ordered) {
		return hasMore, nil
	}
	p.ordered = ordered
	p.issueIDs = p.issueIDs[:0]
	p.wispIDs = p.wispIDs[:0]
	for _, r := range p.ordered {
		switch r.src {
		case "i":
			p.issueIDs = append(p.issueIDs, r.id)
		case "w":
			p.wispIDs = append(p.wispIDs, r.id)
		}
	}
	return hasMore, nil
}

type idSrcRef struct{ id, src string }

// sortRowsGoSide is the per-table seam's copy of idSrcPage.sortGoSide: it
// establishes the order for a sort key SQL renders no ORDER BY for, BEFORE
// finishWindow's trim decides which rows the page keeps. searchWindowFor
// pushes no page bound under a Go-side sort precisely so the whole matching
// set arrives here — this is where the requested order first exists; without
// it the trim keeps an arbitrary engine-ordered subset (order-right,
// membership-wrong once sorted downstream). The comparison is byte order,
// matching sqlbuild.Less and idSrcPage.sortGoSide; the natural-numeric
// display order is applied later, on the delivered page (see sortGoSide's
// doc for the membership/display split). No-op for SQL-rendered sort keys,
// whose ORDER BY already ran.
func sortRowsGoSide[T any](rows []T, id func(T) string, sortBy string, sortDesc bool) {
	if !sqlbuild.IsGoSideSort(sortBy) || len(rows) <= 1 {
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return sqlbuild.LessID(id(rows[i]), id(rows[j]), sortDesc)
	})
}

const unionSortColumnsSQL = sqlbuild.UnionSortColumnsSQL

func unionOrderBySQL(sortBy string, sortDesc bool) string {
	return sqlbuild.OrderByForColumns(sortBy, sortDesc, func(k string) string {
		if k == "id" {
			return "id"
		}
		return "sort_" + k
	})
}

func orderBySQL(sortBy string, sortDesc bool, prefix string) string {
	return sqlbuild.OrderBy(sortBy, sortDesc, prefix)
}
