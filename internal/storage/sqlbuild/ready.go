package sqlbuild

import (
	"fmt"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/types"
)

// ReadyWorkExcludeTypes returns the issue types excluded from ready work by
// default, plus any caller extras (deduped, empty entries dropped). Infra types
// stay hidden from ready work, and rig identity beads are also hidden even
// though they are durable issues rather than infra wisps.
func ReadyWorkExcludeTypes(extra []types.IssueType) []types.IssueType {
	out := []types.IssueType{
		types.IssueType("merge-request"),
		types.TypeGate,
		types.TypeMolecule,
		types.IssueType("rig"),
	}
	for _, t := range domain.DefaultInfraTypes() {
		out = append(out, types.IssueType(t))
	}
	seen := make(map[types.IssueType]bool, len(out)+len(extra))
	for _, t := range out {
		seen[t] = true
	}
	for _, t := range extra {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// ReadyWorkOrder is an ORDER BY fragment plus any args its CASE expressions
// need (the hybrid policy parameterizes a recency cutoff).
type ReadyWorkOrder struct {
	SQL  string
	Args []any
}

// BuildReadyWorkOrder renders the ready-work ORDER BY for a sort policy.
// createdCol/priorityCol name the sortable columns: real columns
// ("created_at"/"priority") for per-table queries, or the sort_* aliases
// ("sort_created"/"sort_priority") for UNION outer queries.
func BuildReadyWorkOrder(policy types.SortPolicy, createdCol, priorityCol string) ReadyWorkOrder {
	switch policy {
	case types.SortPolicyOldest:
		return ReadyWorkOrder{SQL: fmt.Sprintf("ORDER BY %s ASC, id ASC", createdCol)}
	case types.SortPolicyPriority:
		return ReadyWorkOrder{SQL: fmt.Sprintf("ORDER BY %s ASC, %s ASC, id ASC", priorityCol, createdCol)}
	case types.SortPolicyHybrid, "":
		recentCutoff := time.Now().UTC().Add(-48 * time.Hour)
		return ReadyWorkOrder{
			SQL: fmt.Sprintf(`ORDER BY
			CASE WHEN %s >= ? THEN 0 ELSE 1 END ASC,
			CASE WHEN %s >= ? THEN %s ELSE 999 END ASC,
			%s ASC, id ASC`, createdCol, createdCol, priorityCol, createdCol),
			Args: []any{recentCutoff, recentCutoff},
		}
	default:
		return ReadyWorkOrder{SQL: fmt.Sprintf("ORDER BY %s ASC, %s ASC, id ASC", priorityCol, createdCol)}
	}
}

// ReadyWorkWhereInputs carries the precomputed ID sets the ready-work WHERE
// clause folds in. Computing them takes queries, which is execution-context
// work each stack does its own way.
type ReadyWorkWhereInputs struct {
	// DeferredChildIDs are children of future-deferred parents; consulted
	// only when !filter.IncludeDeferred.
	DeferredChildIDs []string
	// ParentDescendantIDs are the transitive descendants of *filter.ParentID;
	// consulted only when filter.ParentID != nil.
	ParentDescendantIDs []string
}

// BuildReadyWorkWhere renders the full ready-work WHERE clause for one table
// family. Both stacks must keep ready semantics identical (Seam A parity
// suite); all ready predicates live here.
//
// Invariant: every clause must reference only main-table columns or correlated
// subqueries keyed by id — never the counts mega-query's aggregate aliases
// (labels_json, dep_count, rdep_count, comment_count, parent_id, deps_json).
// SearchCountsSQL renders this WHERE inside a pre-join subquery where those
// aliases are out of scope. See the SearchCountsSQL doc comment for why a
// violation fails loud.
func BuildReadyWorkWhere(filter types.WorkFilter, tables FilterTables, in ReadyWorkWhereInputs) (string, []any, error) {
	st := newReadyWhereState(tables)
	st.addStatus(filter)
	st.addPinnedBlocked()
	st.addEphemeral(filter)
	st.addLeaves(filter)
	st.addIdentity(filter)
	st.addDefer(filter, in)
	st.addLabels(filter)
	st.addParent(filter, in)
	st.addMolecule(filter)
	return st.finish(filter)
}

type readyWhereState struct {
	clauses []string
	args    []any
	tables  FilterTables
}

func newReadyWhereState(tables FilterTables) *readyWhereState {
	return &readyWhereState{tables: tables}
}

func (st *readyWhereState) addStatus(filter types.WorkFilter) {
	switch {
	case filter.Status != "":
		st.clauses = append(st.clauses, "status = ?")
		st.args = append(st.args, string(filter.Status))
	case len(filter.Statuses) > 0:
		ph, statusArgs := InPlaceholders(filter.Statuses)
		st.clauses = append(st.clauses, fmt.Sprintf("status IN (%s)", ph))
		st.args = append(st.args, statusArgs...)
	default:
		st.clauses = append(st.clauses, "status IN ('open', 'in_progress')")
	}
}

func (st *readyWhereState) addPinnedBlocked() {
	st.clauses = append(st.clauses, "(pinned = 0 OR pinned IS NULL)", "is_blocked = 0")
}

func (st *readyWhereState) addEphemeral(filter types.WorkFilter) {
	if !filter.IncludeEphemeral {
		st.clauses = append(st.clauses, "(ephemeral = 0 OR ephemeral IS NULL)")
	}
}

func (st *readyWhereState) addLeaves(filter types.WorkFilter) {
	if !filter.LeavesOnly {
		return
	}
	// Prefer leaves: claim a child, not the parent epic, while children are open.
	// Qualify the outer id. Dolt treats a bare id as ambiguous once _leaf_c
	// and _leaf_d are in scope, and claim then fails with Error 1105.
	outerID := st.tables.Main + ".id"
	st.clauses = append(st.clauses, fmt.Sprintf(`NOT EXISTS (
			SELECT 1 FROM %s _leaf_d
			INNER JOIN %s _leaf_c ON _leaf_c.id = _leaf_d.issue_id
			WHERE _leaf_d.type = 'parent-child'
			  AND %s = %s
			  AND _leaf_c.status NOT IN ('closed', 'tombstone')
		) AND NOT EXISTS (
			SELECT 1 FROM %s _leaf_dot
			WHERE _leaf_dot.id LIKE CONCAT(%s, '.%%')
			  AND _leaf_dot.status NOT IN ('closed', 'tombstone')
		)`, st.tables.Dependencies, st.tables.Main, qualifiedDepTarget("_leaf_d"), outerID, st.tables.Main, outerID))
}

func (st *readyWhereState) addIdentity(filter types.WorkFilter) {
	if filter.Priority != nil {
		st.clauses = append(st.clauses, "priority = ?")
		st.args = append(st.args, *filter.Priority)
	}
	if filter.Type != "" {
		st.clauses = append(st.clauses, "issue_type = ?")
		st.args = append(st.args, filter.Type)
	} else {
		ph, a := InPlaceholders(ReadyWorkExcludeTypes(filter.ExcludeTypes))
		st.clauses = append(st.clauses, fmt.Sprintf("issue_type NOT IN (%s)", ph))
		st.args = append(st.args, a...)
	}
	if filter.Unassigned {
		st.clauses = append(st.clauses, "(assignee IS NULL OR assignee = '')")
	} else if filter.Assignee != nil {
		st.clauses = append(st.clauses, "assignee = ?")
		st.args = append(st.args, *filter.Assignee)
	}
}

func (st *readyWhereState) addDefer(filter types.WorkFilter, in ReadyWorkWhereInputs) {
	if filter.IncludeDeferred {
		return
	}
	st.clauses = append(st.clauses, "(defer_until IS NULL OR defer_until <= UTC_TIMESTAMP())")
	nDeferred := len(in.DeferredChildIDs)
	for start := 0; start < nDeferred; start += QueryBatchSize {
		end := start + QueryBatchSize
		if end > len(in.DeferredChildIDs) {
			end = len(in.DeferredChildIDs)
		}
		placeholders, batchArgs := InPlaceholders(in.DeferredChildIDs[start:end])
		st.args = append(st.args, batchArgs...)
		st.clauses = append(st.clauses, fmt.Sprintf("id NOT IN (%s)", placeholders))
	}
}

func (st *readyWhereState) addLabels(filter types.WorkFilter) {
	for _, label := range filter.Labels {
		st.clauses = append(st.clauses, fmt.Sprintf("id IN (SELECT issue_id FROM %s WHERE label = ?)", st.tables.Labels))
		st.args = append(st.args, label)
	}
	// LabelsAny is an OR-set: an issue qualifies if it carries AT LEAST ONE of
	// the labels. Previously this clause was absent entirely, so --label-any was
	// silently dropped on the ready/claim path (with or without --parent) — a
	// worker believed it was fenced when it was not. It AND-combines with Labels
	// (the flag help promises "Can combine with --label").
	if len(filter.LabelsAny) > 0 {
		placeholders := make([]string, len(filter.LabelsAny))
		for i, label := range filter.LabelsAny {
			placeholders[i] = "?"
			st.args = append(st.args, label)
		}
		st.clauses = append(st.clauses, fmt.Sprintf("id IN (SELECT issue_id FROM %s WHERE label IN (%s))", st.tables.Labels, strings.Join(placeholders, ", ")))
	}
	if len(filter.ExcludeLabels) > 0 {
		placeholders := make([]string, len(filter.ExcludeLabels))
		for i, label := range filter.ExcludeLabels {
			placeholders[i] = "?"
			st.args = append(st.args, label)
		}
		st.clauses = append(st.clauses, fmt.Sprintf("id NOT IN (SELECT issue_id FROM %s WHERE label IN (%s))", st.tables.Labels, strings.Join(placeholders, ", ")))
	}
	if filter.LabelPattern != "" {
		st.clauses = append(st.clauses, fmt.Sprintf("id IN (SELECT issue_id FROM %s WHERE label LIKE ? ESCAPE '|')", st.tables.Labels))
		st.args = append(st.args, globToLikePattern(filter.LabelPattern))
	}
	if filter.LabelRegex != "" {
		st.clauses = append(st.clauses, fmt.Sprintf("id IN (SELECT issue_id FROM %s WHERE label REGEXP ?)", st.tables.Labels))
		st.args = append(st.args, filter.LabelRegex)
	}
}

func (st *readyWhereState) addParent(filter types.WorkFilter, in ReadyWorkWhereInputs) {
	// Parent filtering: return all transitive descendants of parentID.
	// GH#3396: a one-hop subquery silently dropped grandchildren despite the
	// help text and WorkFilter.ParentID godoc both promising recursion.
	if filter.ParentID == nil {
		return
	}
	parentID := *filter.ParentID
	parentClauses := []string{fmt.Sprintf("(id LIKE CONCAT(?, '.%%') AND id NOT IN (SELECT issue_id FROM %s WHERE type = 'parent-child'))", st.tables.Dependencies)}
	st.args = append(st.args, parentID)
	nDescendants := len(in.ParentDescendantIDs)
	for start := 0; start < nDescendants; start += QueryBatchSize {
		end := start + QueryBatchSize
		if end > len(in.ParentDescendantIDs) {
			end = len(in.ParentDescendantIDs)
		}
		placeholders, batchArgs := InPlaceholders(in.ParentDescendantIDs[start:end])
		parentClauses = append(parentClauses, fmt.Sprintf("id IN (%s)", placeholders))
		st.args = append(st.args, batchArgs...)
	}
	st.clauses = append(st.clauses, "("+strings.Join(parentClauses, " OR ")+")")
}

func (st *readyWhereState) addMolecule(filter types.WorkFilter) {
	if filter.MoleculeID == "" {
		return
	}
	st.clauses = append(st.clauses, fmt.Sprintf("(id IN (SELECT issue_id FROM %s WHERE type = 'parent-child' AND %s = ?) OR (id LIKE CONCAT(?, '.%%') AND id NOT IN (SELECT issue_id FROM %s WHERE type = 'parent-child')))", st.tables.Dependencies, DepTargetExpr, st.tables.Dependencies))
	st.args = append(st.args, filter.MoleculeID, filter.MoleculeID)
}

func (st *readyWhereState) finish(filter types.WorkFilter) (string, []any, error) {
	whereClauses, args, err := AppendMetadataClauses(st.clauses, st.args, filter.HasMetadataKey, filter.MetadataFields)
	if err != nil {
		return "", nil, err
	}
	return "WHERE " + strings.Join(whereClauses, " AND "), args, nil
}
