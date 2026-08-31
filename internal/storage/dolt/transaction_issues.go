package dolt

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

// CreateIssue creates an issue within the transaction.
// Routes ephemeral issues to the wisps table.
func (t *transactionIssueWrite) CreateIssue(ctx context.Context, issue *types.Issue, actor string) error {
	if issue == nil {
		return fmt.Errorf("issue must not be nil")
	}

	// Build the validation context on regularTx for both tiers: wisp rows
	// live on the ignored session, but the validation context (config,
	// custom_types) lives in regular dolt-tracked tables — reading it
	// through regularTx keeps types registered earlier in this transaction
	// (tx.SetConfig("types.custom", ...)) visible. Both sessions are
	// pinned to the same branch (GH#5443).
	bc, err := issueops.NewBatchContext(ctx, t.resources.regularTx, storage.BatchCreateOptions{SkipPrefixValidation: true})
	if err != nil {
		return err
	}

	if issueops.IsWisp(issue) {
		_, err = issueops.CreateIssueInTxWithResult(ctx, t.resources.ignoredTx, bc, issue, actor)
		return err
	}

	result, err := issueops.CreateIssueInTxWithResult(ctx, t.resources.regularTx, bc, issue, actor)
	if err != nil {
		return err
	}
	for table := range issueops.CreateIssueDirtyTables(ctx, issue, result) {
		t.resources.dirty.MarkDirty(table)
	}
	return nil
}

// CreateIssues creates multiple issues within the transaction
func (t *transactionIssueWrite) CreateIssues(ctx context.Context, issues []*types.Issue, actor string) error {
	if len(issues) == 0 {
		return nil
	}

	// This must run before splitting regular issues from wisps: the shared
	// create helper below only sees the regular subset.
	if err := issueops.ValidateCreateIssuesMixedBucketDependencies(issues); err != nil {
		return err
	}

	regularIssues, wispIssues := splitDoltCreateIssues(issues)

	// See CreateIssue: one validation context on regularTx serves both
	// tiers, so in-transaction custom-type registration is visible to the
	// wisp tier too (GH#5443).
	bc, err := issueops.NewBatchContext(ctx, t.resources.regularTx, storage.BatchCreateOptions{
		SkipPrefixValidation: true,
	})
	if err != nil {
		return err
	}

	if err := t.createRegularIssues(ctx, regularIssues, actor, bc); err != nil {
		return err
	}
	return t.createWispIssues(ctx, wispIssues, actor, bc)
}

func splitDoltCreateIssues(issues []*types.Issue) ([]*types.Issue, []*types.Issue) {
	var regularIssues []*types.Issue
	var wispIssues []*types.Issue
	for _, issue := range issues {
		if issueops.IsWisp(issue) {
			wispIssues = append(wispIssues, issue)
			continue
		}
		regularIssues = append(regularIssues, issue)
	}
	return regularIssues, wispIssues
}

func (t *transactionIssueWrite) createRegularIssues(ctx context.Context, issues []*types.Issue, actor string, bc *issueops.BatchContext) error {
	if len(issues) == 0 {
		return nil
	}
	result, err := issueops.CreateIssuesInTxWithContext(ctx, t.resources.regularTx, bc, issues, actor)
	if err != nil {
		return err
	}
	for table := range issueops.CreateIssuesDirtyTables(ctx, issues, result) {
		t.resources.dirty.MarkDirty(table)
	}
	return nil
}

func (t *transactionIssueWrite) createWispIssues(ctx context.Context, issues []*types.Issue, actor string, bc *issueops.BatchContext) error {
	if len(issues) == 0 {
		return nil
	}
	_, err := issueops.CreateIssuesInTxWithContext(ctx, t.resources.ignoredTx, bc, issues, actor)
	return err
}

// GetIssue retrieves an issue within the transaction.
// Checks wisps table for active wisps (including explicit-ID ephemerals).
func (t *transactionIssueRead) GetIssue(ctx context.Context, id string) (*types.Issue, error) {
	table := "issues"
	if t.isActiveWisp(ctx, id) {
		table = "wisps"
	}
	return scanIssueTxFromTable(ctx, t.txFor(table), table, id)
}

// SearchIssueIDs returns matching IDs only, projected in Go from SearchIssues.
// It skips the issueops.SearchIssueIDsInTx fast path because that merges
// issues+wisps over one *sql.Tx, while doltTransaction splits them across
// regularTx/ignoredTx (see txFor). Not worth re-implementing: partial-ID
// resolution calls the (fast) store path, never a transaction, so this is cold.
func (t *transactionIssueRead) SearchIssueIDs(ctx context.Context, query string, filter types.IssueFilter) ([]string, error) {
	issues, err := t.SearchIssues(ctx, query, filter)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
	}
	return ids, nil
}

// SearchIssues searches for issues within the transaction.
// Supports the same filter fields as DoltStore.SearchIssues (bd-v6v8).
func (t *transactionIssueRead) SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error) {
	searchQuery := newIssueSearchQuery(filter)
	if err := addIssueSearchFilters(searchQuery, query, filter); err != nil {
		return nil, err
	}

	//nolint:gosec // G201: table is hardcoded, whereSQL is parameterized
	rows, err := t.txFor(searchQuery.table).QueryContext(ctx, searchQuery.querySQL(filter.Limit), searchQuery.args...)
	if err != nil {
		return nil, wrapQueryError("search issues in tx", err)
	}
	ids, err := scanTransactionSearchIDs(rows)
	if err != nil {
		return nil, err
	}
	return loadTransactionSearchIssues(t.doltTransaction, ctx, ids)
}

type issueSearchQuery struct {
	table        string
	depTable     string
	labelTable   string
	whereClauses []string
	args         []interface{}
}

func newIssueSearchQuery(filter types.IssueFilter) *issueSearchQuery {
	table := "issues"
	if filter.Ephemeral != nil && *filter.Ephemeral {
		table = "wisps"
	}
	// If searching by IDs that are all ephemeral, use wisps table (bd-w2w)
	if len(filter.IDs) > 0 && allEphemeral(filter.IDs) {
		table = "wisps"
	}

	depTable := "dependencies"
	labelTable := "labels"
	if table == "wisps" {
		depTable = "wisp_dependencies"
		labelTable = "wisp_labels"
	}
	return &issueSearchQuery{table: table, depTable: depTable, labelTable: labelTable}
}

func (q *issueSearchQuery) addClause(clause string, args ...interface{}) {
	q.whereClauses = append(q.whereClauses, clause)
	q.args = append(q.args, args...)
}

func addIssueSearchFilters(q *issueSearchQuery, query string, filter types.IssueFilter) error {
	addIssueSearchText(q, query)
	addIssueSearchTextFieldFilters(q, filter)
	addIssueSearchStatusFilters(q, filter)
	addIssueSearchExcludedTypeFilter(q, filter)
	addIssueSearchPriorityFilters(q, filter)
	addIssueSearchTypeFilter(q, filter)
	addIssueSearchAssigneeFilter(q, filter)
	addIssueSearchDateFilters(q, filter)
	addIssueSearchEmptyFilters(q, filter)
	addIssueSearchLabelFilters(q, filter)
	addIssueSearchIDFilters(q, filter)
	addIssueSearchSourceFilter(q, filter)
	addIssueSearchBooleanFilters(q, filter)
	addIssueSearchParentFilters(q, filter)
	addIssueSearchMoleculeFilters(q, filter)
	addIssueSearchSchedulingFilters(q, filter)
	return addIssueSearchMetadataFilters(q, filter)
}

func addIssueSearchText(q *issueSearchQuery, query string) {
	if query == "" {
		return
	}
	lowerQuery := strings.ToLower(query)
	if looksLikeIssueID(query) {
		q.addClause("(id = ? OR id LIKE ? OR LOWER(title) LIKE ?)", lowerQuery, lowerQuery+"%", "%"+lowerQuery+"%")
		return
	}
	pattern := "%" + lowerQuery + "%"
	q.addClause("(LOWER(title) LIKE ? OR id LIKE ?)", pattern, pattern)
}

func addIssueSearchTextFieldFilters(q *issueSearchQuery, filter types.IssueFilter) {
	addIssueSearchTextContains(q, "title", filter.TitleSearch)
	addIssueSearchTextContains(q, "title", filter.TitleContains)
	addIssueSearchTextContains(q, "description", filter.DescriptionContains)
	addIssueSearchTextContains(q, "notes", filter.NotesContains)
	addIssueSearchTextContains(q, "external_ref", filter.ExternalRefContains)
	if filter.ExternalRef != nil {
		q.addClause("external_ref = ?", *filter.ExternalRef)
	}
}

func addIssueSearchTextContains(q *issueSearchQuery, column, value string) {
	if value == "" {
		return
	}
	q.addClause("LOWER("+column+") LIKE ?", "%"+strings.ToLower(value)+"%")
}

func addIssueSearchStatusFilters(q *issueSearchQuery, filter types.IssueFilter) {
	if filter.Status != nil {
		q.addClause("status = ?", *filter.Status)
	}
	if len(filter.ExcludeStatus) == 0 {
		return
	}
	placeholders := make([]string, len(filter.ExcludeStatus))
	args := make([]interface{}, len(filter.ExcludeStatus))
	for i, s := range filter.ExcludeStatus {
		placeholders[i] = "?"
		args[i] = string(s)
	}
	q.addClause(fmt.Sprintf("status NOT IN (%s)", strings.Join(placeholders, ",")), args...)
}

func addIssueSearchExcludedTypeFilter(q *issueSearchQuery, filter types.IssueFilter) {
	if len(filter.ExcludeTypes) == 0 {
		return
	}
	placeholders := make([]string, len(filter.ExcludeTypes))
	args := make([]interface{}, len(filter.ExcludeTypes))
	for i, tp := range filter.ExcludeTypes {
		placeholders[i] = "?"
		args[i] = string(tp)
	}
	//nolint:gosec // G201: table is hardcoded to "issues" or "wisps"
	q.addClause(fmt.Sprintf("id IN (SELECT id FROM %s WHERE issue_type NOT IN (%s))", q.table, strings.Join(placeholders, ",")), args...)
}

func addIssueSearchPriorityFilters(q *issueSearchQuery, filter types.IssueFilter) {
	if filter.Priority != nil {
		q.addClause("priority = ?", *filter.Priority)
	}
	if filter.PriorityMin != nil {
		q.addClause("priority >= ?", *filter.PriorityMin)
	}
	if filter.PriorityMax != nil {
		q.addClause("priority <= ?", *filter.PriorityMax)
	}
}

func addIssueSearchTypeFilter(q *issueSearchQuery, filter types.IssueFilter) {
	if filter.IssueType == nil {
		return
	}
	//nolint:gosec // G201: table is hardcoded to "issues" or "wisps"
	q.addClause(fmt.Sprintf("id IN (SELECT id FROM %s WHERE issue_type = ?)", q.table), *filter.IssueType)
}

func addIssueSearchAssigneeFilter(q *issueSearchQuery, filter types.IssueFilter) {
	if filter.Assignee != nil {
		q.addClause("assignee = ?", *filter.Assignee)
	}
}

type issueSearchDateFilter struct {
	column string
	op     string
	value  *time.Time
}

func addIssueSearchDateFilters(q *issueSearchQuery, filter types.IssueFilter) {
	dateFilters := []issueSearchDateFilter{
		{column: "created_at", op: ">", value: filter.CreatedAfter},
		{column: "created_at", op: "<", value: filter.CreatedBefore},
		{column: "updated_at", op: ">", value: filter.UpdatedAfter},
		{column: "updated_at", op: "<", value: filter.UpdatedBefore},
		{column: "closed_at", op: ">", value: filter.ClosedAfter},
		{column: "closed_at", op: "<", value: filter.ClosedBefore},
		{column: "defer_until", op: ">", value: filter.DeferAfter},
		{column: "defer_until", op: "<", value: filter.DeferBefore},
		{column: "due_at", op: ">", value: filter.DueAfter},
		{column: "due_at", op: "<", value: filter.DueBefore},
	}
	for _, dateFilter := range dateFilters {
		if dateFilter.value != nil {
			q.addClause(dateFilter.column+" "+dateFilter.op+" ?", dateFilter.value.Format(time.RFC3339))
		}
	}
}

func addIssueSearchEmptyFilters(q *issueSearchQuery, filter types.IssueFilter) {
	if filter.EmptyDescription {
		q.addClause("(description IS NULL OR description = '')")
	}
	if filter.NoAssignee {
		q.addClause("(assignee IS NULL OR assignee = '')")
	}
	if filter.NoLabels {
		//nolint:gosec // G201: labelTable is hardcoded to "labels" or "wisp_labels"
		q.addClause(fmt.Sprintf("id NOT IN (SELECT DISTINCT issue_id FROM %s)", q.labelTable))
	}
}

func addIssueSearchLabelFilters(q *issueSearchQuery, filter types.IssueFilter) {
	if len(filter.Labels) > 0 {
		for _, label := range filter.Labels {
			//nolint:gosec // G201: labelTable is hardcoded to "labels" or "wisp_labels"
			q.addClause(fmt.Sprintf("id IN (SELECT issue_id FROM %s WHERE label = ?)", q.labelTable), label)
		}
	}
	if len(filter.LabelsAny) == 0 {
		return
	}
	placeholders := make([]string, len(filter.LabelsAny))
	args := make([]interface{}, len(filter.LabelsAny))
	for i, label := range filter.LabelsAny {
		placeholders[i] = "?"
		args[i] = label
	}
	//nolint:gosec // G201: labelTable is hardcoded to "labels" or "wisp_labels"
	q.addClause(fmt.Sprintf("id IN (SELECT issue_id FROM %s WHERE label IN (%s))", q.labelTable, strings.Join(placeholders, ", ")), args...)
}

func addIssueSearchIDFilters(q *issueSearchQuery, filter types.IssueFilter) {
	if len(filter.IDs) > 0 {
		placeholders := make([]string, len(filter.IDs))
		args := make([]interface{}, len(filter.IDs))
		for i, id := range filter.IDs {
			placeholders[i] = "?"
			args[i] = id
		}
		q.addClause(fmt.Sprintf("id IN (%s)", strings.Join(placeholders, ",")), args...)
	}
	if filter.IDPrefix != "" {
		q.addClause("id LIKE ?", filter.IDPrefix+"%")
	}
	if filter.SpecIDPrefix != "" {
		q.addClause("spec_id LIKE ?", filter.SpecIDPrefix+"%")
	}
}

func addIssueSearchSourceFilter(q *issueSearchQuery, filter types.IssueFilter) {
	if filter.SourceRepo != nil {
		q.addClause("source_repo = ?", *filter.SourceRepo)
	}
}

func addIssueSearchBooleanFilters(q *issueSearchQuery, filter types.IssueFilter) {
	addIssueSearchNullableBooleanFilter(q, filter.Ephemeral, "ephemeral")
	addIssueSearchNullableBooleanFilter(q, filter.Pinned, "pinned")
	addIssueSearchNullableBooleanFilter(q, filter.IsTemplate, "is_template")
}

func addIssueSearchNullableBooleanFilter(q *issueSearchQuery, value *bool, column string) {
	if value == nil {
		return
	}
	if *value {
		q.addClause(column + " = 1")
		return
	}
	q.addClause(fmt.Sprintf("(%s = 0 OR %s IS NULL)", column, column))
}

func addIssueSearchParentFilters(q *issueSearchQuery, filter types.IssueFilter) {
	if filter.ParentID != nil {
		parentID := *filter.ParentID
		//nolint:gosec // G201: depTable is hardcoded to "dependencies" or "wisp_dependencies"
		q.addClause(fmt.Sprintf("(id IN (SELECT issue_id FROM %s WHERE type = 'parent-child' AND %s = ?) OR (id LIKE CONCAT(?, '.%%') AND id NOT IN (SELECT issue_id FROM %s WHERE type = 'parent-child')))", q.depTable, issueops.DepTargetExpr, q.depTable), parentID, parentID)
	}
	if filter.NoParent {
		//nolint:gosec // G201: depTable is hardcoded to "dependencies" or "wisp_dependencies"
		q.addClause(fmt.Sprintf("id NOT IN (SELECT issue_id FROM %s WHERE type = 'parent-child')", q.depTable))
	}
}

func addIssueSearchMoleculeFilters(q *issueSearchQuery, filter types.IssueFilter) {
	if filter.MolType != nil {
		q.addClause("mol_type = ?", string(*filter.MolType))
	}
	if filter.WispType != nil {
		q.addClause("wisp_type = ?", string(*filter.WispType))
	}
}

func addIssueSearchSchedulingFilters(q *issueSearchQuery, filter types.IssueFilter) {
	if filter.Deferred {
		q.addClause("(defer_until IS NOT NULL OR status = ?)", types.StatusDeferred)
	}
	if filter.Overdue {
		q.addClause("due_at IS NOT NULL AND due_at < ? AND status != ?", time.Now().UTC().Format(time.RFC3339), types.StatusClosed)
	}
}

func addIssueSearchMetadataFilters(q *issueSearchQuery, filter types.IssueFilter) error {
	if filter.HasMetadataKey != "" {
		if err := storage.ValidateMetadataKey(filter.HasMetadataKey); err != nil {
			return err
		}
		q.addClause("JSON_EXTRACT(metadata, ?) IS NOT NULL", storage.JSONMetadataPath(filter.HasMetadataKey))
	}
	if len(filter.MetadataFields) == 0 {
		return nil
	}
	metaKeys := make([]string, 0, len(filter.MetadataFields))
	for k := range filter.MetadataFields {
		metaKeys = append(metaKeys, k)
	}
	sort.Strings(metaKeys)
	for _, k := range metaKeys {
		if err := storage.ValidateMetadataKey(k); err != nil {
			return err
		}
		q.addClause("JSON_UNQUOTE(JSON_EXTRACT(metadata, ?)) = ?", storage.JSONMetadataPath(k), filter.MetadataFields[k])
	}
	return nil
}

func (q *issueSearchQuery) querySQL(limit int) string {
	whereSQL := ""
	if len(q.whereClauses) > 0 {
		whereSQL = "WHERE " + strings.Join(q.whereClauses, " AND ")
	}
	limitSQL := ""
	if limit > 0 {
		limitSQL = fmt.Sprintf(" LIMIT %d", limit)
	}
	return fmt.Sprintf("\n\t\tSELECT id FROM %s %s ORDER BY priority ASC, created_at DESC %s\n\t", q.table, whereSQL, limitSQL)
}

func scanTransactionSearchIDs(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, wrapScanError("search issues in tx", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapQueryError("search issues in tx: rows iteration", err)
	}
	return ids, nil
}

func loadTransactionSearchIssues(t *doltTransaction, ctx context.Context, ids []string) ([]*types.Issue, error) {
	var issues []*types.Issue
	for _, id := range ids {
		issue, err := t.GetIssue(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("search issues in tx: get issue %s: %w", id, err)
		}
		issues = append(issues, issue)
	}
	return issues, nil
}
func (t *transactionIssueWrite) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	table := "issues"
	if t.isActiveWisp(ctx, id) {
		table = "wisps"
	}

	if rawMeta, ok := updates["metadata"]; ok {
		metadataStr, err := storage.NormalizeMetadataValue(rawMeta)
		if err != nil {
			return fmt.Errorf("invalid metadata: %w", err)
		}
		if err := validateMetadataIfConfigured(json.RawMessage(metadataStr)); err != nil {
			return err
		}
	}

	update := issueops.UpdateIssueWithoutEventInTx
	if transactionHasLifecycle(t.doltTransaction) {
		update = issueops.UpdateIssueInTx
	}
	result, err := update(ctx, t.txFor(table), id, updates, actor)
	if err != nil {
		return wrapExecError("update issue in tx", err)
	}
	if !result.Changed {
		return nil
	}
	t.resources.dirty.MarkDirty(table)
	if transactionHasLifecycle(t.doltTransaction) {
		_, _, eventTable, _ := issueops.WispTableRouting(table == "wisps")
		t.resources.dirty.MarkDirty(eventTable)
	}
	return nil
}

func (t *transactionIssueWrite) CloseIssue(ctx context.Context, id string, reason string, actor string, session string) error {
	table := "issues"
	eventTable := "events"
	if t.isActiveWisp(ctx, id) {
		table = "wisps"
		eventTable = "wisp_events"
	}

	result, err := issueops.CloseIssueInTx(ctx, t.txFor(table), id, reason, actor, session)
	if err != nil {
		return wrapExecError("close issue in tx", err)
	}
	if result.AlreadyClosed {
		return nil
	}
	t.resources.dirty.MarkDirty(table)
	t.resources.dirty.MarkDirty(eventTable)
	if result.IssueRowsChanged {
		t.resources.dirty.MarkDirty("issues")
	}
	return nil
}

// ReopenIssueWithResult reopens an issue within this transaction and reports
// whether the lifecycle state changed.
func (t *transactionIssueWrite) ReopenIssueWithResult(ctx context.Context, id string, reason string, actor string) (bool, error) {
	table, eventTable := "issues", "events"
	if t.isActiveWisp(ctx, id) {
		table, eventTable = "wisps", "wisp_events"
	}
	result, err := issueops.ReopenIssueInTx(ctx, t.txFor(table), id, reason, actor)
	if err != nil {
		return false, wrapExecError("reopen issue in tx", err)
	}
	if result.Changed {
		t.resources.dirty.MarkDirty(table)
		t.resources.dirty.MarkDirty(eventTable)
		if result.IssueRowsChanged {
			t.resources.dirty.MarkDirty("issues")
		}
	}
	return result.Changed, nil
}

func (t *transactionIssueWrite) DeleteIssue(ctx context.Context, id string) error {
	isWisp := t.isActiveWisp(ctx, id)
	table := "issues"
	if isWisp {
		table = "wisps"
	}
	if err := issueops.DeleteIssueInTx(ctx, t.txFor(table), id); err != nil {
		return wrapExecError("delete issue in tx", err)
	}
	// Mark every table the ON DELETE CASCADE fans out to, not just the row's
	// own table: the cascaded deletions are invisible to the SQL we issue, so
	// staging only `issues` leaves them uncommitted in the working set.
	for _, cascaded := range issueops.DeleteCascadeTables(isWisp) {
		t.resources.dirty.MarkDirty(cascaded)
	}
	return nil
}
