package sqlbuild

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// KeysetCreatedAtIDPredicate is the SARGABLE (created_at DESC, id ASC) keyset
// predicate emitted for IssueFilter.AfterCreatedAt/AfterID. Its three ?
// placeholders bind, in order: created_at (the sargable upper bound), created_at
// (strict, drops the same-second rows already returned), and id (the same-second
// tie-break).
//
// It is logically "(created_at, id) is strictly after (cursor)" under
// created_at DESC, id ASC — i.e. (created_at < ?) OR (created_at = ? AND id > ?)
// — but rewritten with a redundant `created_at <= ?` leading bound so the planner
// seeks idx_issues_created_at (an IndexedTableAccess range on Dolt, an index/
// BitmapOr scan on Postgres) instead of full-scanning and filtering. The two
// forms select the same rows: created_at <= C is true whenever the OR is, and
// prunes only created_at > C, which the OR already excludes. It is exported so
// the backend sargability guards EXPLAIN this exact string rather than a copy —
// a change here then breaks the guard.
const KeysetCreatedAtIDPredicate = "(created_at <= ? AND ((created_at < ?) OR (id > ?)))"

// BuildIssueFilterClauses builds WHERE clause fragments and args from a query
// string and IssueFilter. The tables parameter controls which table names are
// referenced in subqueries (issues vs wisps).
//
// Invariant: every clause must reference only main-table columns or correlated
// subqueries keyed by id — never the counts mega-query's aggregate aliases
// (labels_json, dep_count, rdep_count, comment_count, parent_id, deps_json).
// SearchCountsSQL renders this WHERE inside a pre-join subquery where those
// aliases are out of scope; a count-driven predicate (e.g. "issues with >5
// blockers") cannot live here and would need a separate outer predicate
// parameter. See the SearchCountsSQL doc comment for why a violation fails loud.
func BuildIssueFilterClauses(query string, filter types.IssueFilter, tables FilterTables) ([]string, []any, error) {
	clauses := issueFilterClauses{tables: tables}
	appendIssueSearchClauses(&clauses, query, filter)
	appendIssueStatusClauses(&clauses, filter)
	appendIssuePriorityAndIDClauses(&clauses, filter)
	appendIssueParentAndPlanClauses(&clauses, filter)
	appendIssueLabelClauses(&clauses, filter)
	appendIssueStateClauses(&clauses, filter)
	appendIssueTimeClauses(&clauses, filter)
	if err := appendIssueMetadataClauses(&clauses, filter); err != nil {
		return nil, nil, err
	}
	return clauses.where, clauses.args, nil
}

type issueFilterClauses struct {
	where  []string
	args   []any
	tables FilterTables
}

func appendIssueSearchClauses(clauses *issueFilterClauses, query string, filter types.IssueFilter) {
	if query != "" {
		lowerQuery := strings.ToLower(query)
		if LooksLikeIssueID(query) {
			clauses.where = append(clauses.where, "(id = ? OR id LIKE ? OR LOWER(title) LIKE ? OR LOWER(external_ref) LIKE ?)")
			clauses.args = append(clauses.args, lowerQuery, lowerQuery+"%", "%"+lowerQuery+"%", "%"+lowerQuery+"%")
		} else {
			clauses.where = append(clauses.where, "(LOWER(title) LIKE ? OR id LIKE ?)")
			pattern := "%" + lowerQuery + "%"
			clauses.args = append(clauses.args, pattern, pattern)
		}
	}
	appendTextFilter(clauses, "title", filter.TitleSearch)
	appendTextFilter(clauses, "title", filter.TitleContains)
	appendTextFilter(clauses, "description", filter.DescriptionContains)
	appendTextFilter(clauses, "notes", filter.NotesContains)
	appendTextFilter(clauses, "external_ref", filter.ExternalRefContains)
	if filter.ExternalRef != nil {
		clauses.where = append(clauses.where, "external_ref = ?")
		clauses.args = append(clauses.args, *filter.ExternalRef)
	}
}

func appendTextFilter(clauses *issueFilterClauses, column, value string) {
	if value == "" {
		return
	}
	clauses.where = append(clauses.where, fmt.Sprintf("LOWER(%s) LIKE ?", column))
	clauses.args = append(clauses.args, "%"+strings.ToLower(value)+"%")
}

func appendIssueStatusClauses(clauses *issueFilterClauses, filter types.IssueFilter) {
	if filter.Status != nil {
		clauses.where = append(clauses.where, "status = ?")
		clauses.args = append(clauses.args, *filter.Status)
	}
	appendStringEnumListClause(clauses, "status", "IN", filter.Statuses)
	appendStringEnumListClause(clauses, "status", "NOT IN", filter.ExcludeStatus)
	if filter.IssueType != nil {
		clauses.where = append(clauses.where, "issue_type = ?")
		clauses.args = append(clauses.args, *filter.IssueType)
	}
	appendStringEnumListClause(clauses, "issue_type", "NOT IN", filter.ExcludeTypes)
	if filter.Assignee != nil {
		clauses.where = append(clauses.where, "assignee = ?")
		clauses.args = append(clauses.args, *filter.Assignee)
	}
}

func appendStringEnumListClause[T ~string](clauses *issueFilterClauses, column, operator string, values []T) {
	if len(values) == 0 {
		return
	}
	placeholders := make([]string, len(values))
	for i, value := range values {
		placeholders[i] = "?"
		clauses.args = append(clauses.args, string(value))
	}
	clauses.where = append(clauses.where, fmt.Sprintf("%s %s (%s)", column, operator, strings.Join(placeholders, ",")))
}

func appendIssuePriorityAndIDClauses(clauses *issueFilterClauses, filter types.IssueFilter) {
	if filter.Priority != nil {
		clauses.where = append(clauses.where, "priority = ?")
		clauses.args = append(clauses.args, *filter.Priority)
	}
	if filter.PriorityMin != nil {
		clauses.where = append(clauses.where, "priority >= ?")
		clauses.args = append(clauses.args, *filter.PriorityMin)
	}
	if filter.PriorityMax != nil {
		clauses.where = append(clauses.where, "priority <= ?")
		clauses.args = append(clauses.args, *filter.PriorityMax)
	}
	appendStringListClause(clauses, "id", "IN", filter.IDs, ", ")
	if filter.IDPrefix != "" {
		clauses.where = append(clauses.where, "id LIKE ?")
		clauses.args = append(clauses.args, filter.IDPrefix+"%")
	}
	if filter.SpecIDPrefix != "" {
		clauses.where = append(clauses.where, "spec_id LIKE ?")
		clauses.args = append(clauses.args, filter.SpecIDPrefix+"%")
	}
}

func appendStringListClause(clauses *issueFilterClauses, column, operator string, values []string, separator string) {
	if len(values) == 0 {
		return
	}
	placeholders := make([]string, len(values))
	for i, value := range values {
		placeholders[i] = "?"
		clauses.args = append(clauses.args, value)
	}
	clauses.where = append(clauses.where, fmt.Sprintf("%s %s (%s)", column, operator, strings.Join(placeholders, separator)))
}

func appendIssueParentAndPlanClauses(clauses *issueFilterClauses, filter types.IssueFilter) {
	if filter.ParentID != nil {
		parentID := *filter.ParentID
		clauses.where = append(clauses.where, fmt.Sprintf("(id IN (SELECT issue_id FROM %s WHERE type = 'parent-child' AND %s = ?) OR (id LIKE CONCAT(?, '.%%') AND id NOT IN (SELECT issue_id FROM %s WHERE type = 'parent-child')))", clauses.tables.Dependencies, DepTargetExpr, clauses.tables.Dependencies))
		clauses.args = append(clauses.args, parentID, parentID)
	}
	if filter.NoParent {
		clauses.where = append(clauses.where, fmt.Sprintf("id NOT IN (SELECT issue_id FROM %s WHERE type = 'parent-child')", clauses.tables.Dependencies))
	}
	if filter.MolType != nil {
		clauses.where = append(clauses.where, "mol_type = ?")
		clauses.args = append(clauses.args, string(*filter.MolType))
	}
	if filter.WispType != nil {
		clauses.where = append(clauses.where, "wisp_type = ?")
		clauses.args = append(clauses.args, string(*filter.WispType))
	}
}

func appendIssueLabelClauses(clauses *issueFilterClauses, filter types.IssueFilter) {
	for _, label := range filter.Labels {
		clauses.where = append(clauses.where, fmt.Sprintf("id IN (SELECT issue_id FROM %s WHERE label = ?)", clauses.tables.Labels))
		clauses.args = append(clauses.args, label)
	}
	appendLabelListClause(clauses, clauses.tables.Labels, filter.LabelsAny, false)
	appendLabelListClause(clauses, clauses.tables.Labels, filter.ExcludeLabels, true)
	if filter.NoLabels {
		clauses.where = append(clauses.where, fmt.Sprintf("id NOT IN (SELECT DISTINCT issue_id FROM %s)", clauses.tables.Labels))
	}
	if filter.LabelPattern != "" {
		clauses.where = append(clauses.where, fmt.Sprintf("id IN (SELECT issue_id FROM %s WHERE label LIKE ? ESCAPE '|')", clauses.tables.Labels))
		clauses.args = append(clauses.args, globToLikePattern(filter.LabelPattern))
	}
	if filter.LabelRegex != "" {
		clauses.where = append(clauses.where, fmt.Sprintf("id IN (SELECT issue_id FROM %s WHERE label REGEXP ?)", clauses.tables.Labels))
		clauses.args = append(clauses.args, filter.LabelRegex)
	}
}

func appendLabelListClause(clauses *issueFilterClauses, table string, labels []string, exclude bool) {
	if len(labels) == 0 {
		return
	}
	placeholders := make([]string, len(labels))
	for i, label := range labels {
		placeholders[i] = "?"
		clauses.args = append(clauses.args, label)
	}
	operator := "IN"
	if exclude {
		operator = "NOT IN"
	}
	clauses.where = append(clauses.where, fmt.Sprintf("id %s (SELECT issue_id FROM %s WHERE label IN (%s))", operator, table, strings.Join(placeholders, ", ")))
}

func appendIssueStateClauses(clauses *issueFilterClauses, filter types.IssueFilter) {
	appendNullableBooleanClause(clauses, "pinned", filter.Pinned)
	if filter.SourceRepo != nil {
		clauses.where = append(clauses.where, "source_repo = ?")
		clauses.args = append(clauses.args, *filter.SourceRepo)
	}
	appendNullableBooleanClause(clauses, "ephemeral", filter.Ephemeral)
	appendNullableBooleanClause(clauses, "is_template", filter.IsTemplate)
	if filter.IsBlocked != nil {
		blocked := 0
		if *filter.IsBlocked {
			blocked = 1
		}
		clauses.where = append(clauses.where, "is_blocked = ?")
		clauses.args = append(clauses.args, blocked)
	}
	if filter.EmptyDescription {
		clauses.where = append(clauses.where, "(description IS NULL OR description = '')")
	}
	if filter.NoAssignee {
		clauses.where = append(clauses.where, "(assignee IS NULL OR assignee = '')")
	}
}

func appendNullableBooleanClause(clauses *issueFilterClauses, column string, value *bool) {
	if value == nil {
		return
	}
	if *value {
		clauses.where = append(clauses.where, column+" = 1")
		return
	}
	clauses.where = append(clauses.where, "("+column+" = 0 OR "+column+" IS NULL)")
}

func appendIssueTimeClauses(clauses *issueFilterClauses, filter types.IssueFilter) {
	for _, tc := range []struct {
		col, op string
		v       *time.Time
	}{
		{"created_at", ">", filter.CreatedAfter},
		{"created_at", "<", filter.CreatedBefore},
		{"updated_at", ">", filter.UpdatedAfter},
		{"updated_at", "<", filter.UpdatedBefore},
		{"closed_at", ">", filter.ClosedAfter},
		{"closed_at", "<", filter.ClosedBefore},
		{"started_at", ">", filter.StartedAfter},
		{"started_at", "<", filter.StartedBefore},
		{"defer_until", ">", filter.DeferAfter},
		{"defer_until", "<", filter.DeferBefore},
		{"due_at", ">", filter.DueAfter},
		{"due_at", "<", filter.DueBefore},
	} {
		if tc.v != nil {
			clauses.where = append(clauses.where, fmt.Sprintf("%s %s ?", tc.col, tc.op))
			clauses.args = append(clauses.args, tc.v.Format(time.RFC3339))
		}
	}
	if filter.AfterCreatedAt != nil {
		ac := *filter.AfterCreatedAt
		clauses.where = append(clauses.where, KeysetCreatedAtIDPredicate)
		clauses.args = append(clauses.args, ac, ac, filter.AfterID)
	}
	if filter.Deferred {
		clauses.where = append(clauses.where, "(defer_until IS NOT NULL OR status = ?)")
		clauses.args = append(clauses.args, types.StatusDeferred)
	}
	if filter.Overdue {
		clauses.where = append(clauses.where, "due_at IS NOT NULL AND due_at < ? AND status != ?")
		clauses.args = append(clauses.args, time.Now().UTC().Format(time.RFC3339), types.StatusClosed)
	}
}

func appendIssueMetadataClauses(clauses *issueFilterClauses, filter types.IssueFilter) error {
	var err error
	clauses.where, clauses.args, err = AppendMetadataClauses(clauses.where, clauses.args, filter.HasMetadataKey, filter.MetadataFields)
	return err
}

// AppendMetadataClauses appends JSON metadata predicates (has-key and exact
// field matches, keys in sorted order) to an existing clause/arg list.
func AppendMetadataClauses(where []string, args []any, hasKey string, fields map[string]string) ([]string, []any, error) {
	if hasKey != "" {
		if err := storage.ValidateMetadataKey(hasKey); err != nil {
			return nil, nil, err
		}
		where = append(where, "JSON_EXTRACT(metadata, ?) IS NOT NULL")
		args = append(args, storage.JSONMetadataPath(hasKey))
	}
	if len(fields) > 0 {
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := storage.ValidateMetadataKey(k); err != nil {
				return nil, nil, err
			}
			where = append(where, "JSON_UNQUOTE(JSON_EXTRACT(metadata, ?)) = ?")
			args = append(args, storage.JSONMetadataPath(k), fields[k])
		}
	}
	return where, args, nil
}

// globToLikePattern converts a shell-style glob (* and ?) to a SQL LIKE
// pattern. Literal % and _ in the input — and the '|' escape char itself —
// are escaped so they don't act as LIKE wildcards. The resulting SQL must
// use ESCAPE '|'.
func globToLikePattern(pattern string) string {
	var b strings.Builder
	b.Grow(len(pattern))
	for _, c := range pattern {
		switch c {
		case '%', '_', '|':
			b.WriteByte('|')
			b.WriteRune(c)
		case '*':
			b.WriteByte('%')
		case '?':
			b.WriteByte('_')
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// LooksLikeIssueID returns true if the query string looks like a beads issue ID.
func LooksLikeIssueID(query string) bool {
	idx := strings.Index(query, "-")
	if idx <= 0 || idx >= len(query)-1 {
		return false
	}
	if strings.Contains(query, " ") {
		return false
	}
	return issueIDCharsOnly(query)
}

func issueIDCharsOnly(query string) bool {
	for _, c := range query {
		if !isIssueIDChar(c) {
			return false
		}
	}
	return true
}

func isIssueIDChar(c rune) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'a' && c <= 'z':
		return true
	case c >= 'A' && c <= 'Z':
		return true
	case c == '-' || c == '.':
		return true
	default:
		return false
	}
}
