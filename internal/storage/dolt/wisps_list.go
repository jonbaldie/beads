package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/storage/sqlbuild"
	"github.com/jonbaldie/beads/internal/types"
)

// ListWisps returns ephemeral issues matching the filter.
// It always queries the wisps table (Ephemeral=true); callers do not need to set that flag.
func (s *DoltStore) ListWisps(ctx context.Context, filter types.WispFilter) ([]*types.Issue, error) {
	issueFilter := issueops.WispFilterToIssueFilter(filter)
	return s.searchWisps(ctx, "", issueFilter)
}

// searchWisps searches for issues in the wisps table.
func (s *DoltStore) searchWisps(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	whereClauses, args, err := buildIssueFilterClauses(query, filter, wispsFilterTables)
	if err != nil {
		return nil, err
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	limitSQL := ""
	if filter.Limit > 0 {
		limitSQL = fmt.Sprintf(" LIMIT %d", filter.Limit)
	}

	//nolint:gosec // G201: whereSQL contains column comparisons with ?, limitSQL is a safe integer
	querySQL := fmt.Sprintf(`
		SELECT id FROM wisps
		%s
		ORDER BY priority ASC, created_at ASC
		%s
	`, whereSQL, limitSQL)

	rows, err := s.queryContext(ctx, querySQL, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to search wisps: %w", err)
	}
	defer rows.Close()

	return s.scanWispIDs(ctx, rows)
}

// scanWispIDs collects IDs from rows and fetches full issues from the wisps table.
func (s *DoltStore) scanWispIDs(ctx context.Context, rows *sql.Rows) ([]*types.Issue, error) {
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan wisp id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapQueryError("iterate wisp IDs", err)
	}
	_ = rows.Close()

	if len(ids) == 0 {
		return nil, nil
	}

	return s.getWispsByIDs(ctx, ids)
}

// getWispsByIDs retrieves multiple wisps by ID, batching queries to avoid
// oversized IN-clauses that cause slow queries on large databases.
func (s *DoltStore) getWispsByIDs(ctx context.Context, ids []string) ([]*types.Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	issues, issueMap, err := s.loadWispsByIDBatches(ctx, ids)
	if err != nil {
		return nil, err
	}
	if err := s.hydrateWispLabels(ctx, issues, issueMap); err != nil {
		return nil, err
	}
	return orderWispsByIDs(ids, issueMap), nil
}

func (s *DoltStore) loadWispsByIDBatches(ctx context.Context, ids []string) ([]*types.Issue, map[string]*types.Issue, error) {
	// Fetch wisps in batches to keep IN-clause size bounded.
	var issues []*types.Issue
	issueMap := make(map[string]*types.Issue, len(ids))
	idCount := len(ids)
	for start := 0; start < idCount; start += queryBatchSize {
		end := start + queryBatchSize
		if end > idCount {
			end = idCount
		}
		batch := ids[start:end]

		placeholders, args := doltBuildSQLInClause(batch)

		//nolint:gosec // G201: placeholders contains only ? markers
		querySQL := fmt.Sprintf(`
			SELECT %s
			FROM wisps %s
			WHERE id IN (%s)
		`, issueSelectColumns, sqlbuild.LeaseJoin("wisps"), placeholders)

		queryRows, err := s.queryContext(ctx, querySQL, args...)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get wisps by IDs: %w", err)
		}

		for queryRows.Next() {
			issue, err := scanIssueFrom(queryRows)
			if err != nil {
				_ = queryRows.Close()
				return nil, nil, wrapScanError("scan wisp", err)
			}
			issues = append(issues, issue)
			issueMap[issue.ID] = issue
		}
		if err := queryRows.Err(); err != nil {
			_ = queryRows.Close()
			return nil, nil, wrapQueryError("iterate wisps", err)
		}
		_ = queryRows.Close()
	}
	return issues, issueMap, nil
}

func (s *DoltStore) hydrateWispLabels(ctx context.Context, issues []*types.Issue, issueMap map[string]*types.Issue) error {
	if len(issues) == 0 {
		return nil
	}
	allIDs := make([]string, len(issues))
	for i, issue := range issues {
		allIDs[i] = issue.ID
	}
	return s.hydrateWispLabelBatches(ctx, allIDs, issueMap)
}

func (s *DoltStore) hydrateWispLabelBatches(ctx context.Context, ids []string, issueMap map[string]*types.Issue) error {
	idCount := len(ids)
	for start := 0; start < idCount; start += queryBatchSize {
		end := start + queryBatchSize
		if end > idCount {
			end = idCount
		}
		if err := s.hydrateWispLabelBatch(ctx, ids[start:end], issueMap); err != nil {
			return err
		}
	}
	return nil
}

func (s *DoltStore) hydrateWispLabelBatch(ctx context.Context, ids []string, issueMap map[string]*types.Issue) error {
	placeholders, args := doltBuildSQLInClause(ids)

	//nolint:gosec // G201: placeholders contains only ? markers
	labelSQL := fmt.Sprintf(`
		SELECT issue_id, label FROM wisp_labels
		WHERE issue_id IN (%s)
		ORDER BY issue_id, label
	`, placeholders)

	labelRows, err := s.queryContext(ctx, labelSQL, args...)
	if err != nil {
		return fmt.Errorf("failed to get wisp labels: %w", err)
	}

	for labelRows.Next() {
		var issueID, label string
		if err := labelRows.Scan(&issueID, &label); err != nil {
			_ = labelRows.Close()
			return wrapScanError("scan wisp label", err)
		}
		if issue, ok := issueMap[issueID]; ok {
			issue.Labels = append(issue.Labels, label)
		}
	}
	if err := labelRows.Err(); err != nil {
		_ = labelRows.Close()
		return wrapQueryError("iterate wisp labels", err)
	}
	_ = labelRows.Close()
	return nil
}

func orderWispsByIDs(ids []string, issueMap map[string]*types.Issue) []*types.Issue {
	ordered := make([]*types.Issue, 0, len(issueMap))
	for _, id := range ids {
		if issue, ok := issueMap[id]; ok {
			ordered = append(ordered, issue)
		}
	}
	return ordered
}

// addWispDependency adds a dependency to the wisp_dependencies table.
func (s *DoltStore) addWispDependency(ctx context.Context, dep *types.Dependency, actor string, isCrossPrefix, emitEvent bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	clearJournalScope := s.scopeEventsJournalTransaction(tx)
	defer clearJournalScope()

	kind := issueops.ClassifyDepTarget(ctx, tx, dep, isCrossPrefix)
	// Wisp source/event tables are dolt_ignored (committed with the SQL tx, not
	// via selective doltAddAndCommit), so the event-written flag is not needed here.
	if _, err := issueops.AddDependencyInTx(ctx, tx, dep, actor, issueops.AddDependencyOpts{
		SourceTable:   "wisps",
		WriteTable:    "wisp_dependencies",
		IsCrossPrefix: isCrossPrefix,
		TargetKind:    &kind,
		EmitEvent:     emitEvent,
	}); err != nil {
		return err
	}

	return s.commitSQLTx(ctx, "commit add wisp dependency", tx)
}

// getWispDependencies retrieves issues that a wisp depends on.
func (s *DoltStore) getWispDependencies(ctx context.Context, issueID string) ([]*types.Issue, error) {
	rows, err := s.queryContext(ctx, fmt.Sprintf(`
		SELECT %s AS depends_on_id FROM wisp_dependencies WHERE issue_id = ?
	`, issueops.DepTargetExpr), issueID)
	if err != nil {
		return nil, fmt.Errorf("failed to get wisp dependencies: %w", err)
	}

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, wrapScanError("scan wisp dependency", err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, wrapQueryError("iterate wisp dependencies", err)
	}

	if len(ids) == 0 {
		return nil, nil
	}

	return s.GetIssuesByIDs(ctx, ids)
}

// getWispDependents retrieves issues that depend on a wisp.
func (s *DoltStore) getWispDependents(ctx context.Context, issueID string) ([]*types.Issue, error) {
	rows, err := s.queryContext(ctx, fmt.Sprintf(`
		SELECT issue_id FROM wisp_dependencies WHERE %s = ?
	`, issueops.DepTargetExpr), issueID)
	if err != nil {
		return nil, fmt.Errorf("failed to get wisp dependents: %w", err)
	}

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, wrapScanError("scan wisp dependent", err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, wrapQueryError("iterate wisp dependents", err)
	}

	if len(ids) == 0 {
		return nil, nil
	}

	return s.GetIssuesByIDs(ctx, ids)
}

// getWispDependenciesWithMetadata returns wisp dependencies with metadata.
func (s *DoltStore) getWispDependenciesWithMetadata(ctx context.Context, issueID string) ([]*types.IssueWithDependencyMetadata, error) {
	deps, err := s.loadWispDependencyMetadata(ctx, issueID)
	if err != nil {
		return nil, err
	}
	if len(deps) == 0 {
		return nil, nil
	}
	return s.buildWispDependencyMetadata(ctx, deps)
}

type wispDependencyMetadata struct {
	depID, depType string
}

func (s *DoltStore) loadWispDependencyMetadata(ctx context.Context, issueID string) ([]wispDependencyMetadata, error) {
	rows, err := s.queryContext(ctx, fmt.Sprintf(`
		SELECT %s AS depends_on_id, type FROM wisp_dependencies WHERE issue_id = ?
	`, issueops.DepTargetExpr), issueID)
	if err != nil {
		return nil, fmt.Errorf("failed to get wisp dependencies with metadata: %w", err)
	}

	var deps []wispDependencyMetadata
	for rows.Next() {
		var depID, depType string
		if err := rows.Scan(&depID, &depType); err != nil {
			_ = rows.Close()
			return nil, wrapScanError("scan wisp dependency metadata", err)
		}
		deps = append(deps, wispDependencyMetadata{depID: depID, depType: depType})
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, wrapQueryError("iterate wisp dependencies", err)
	}

	return deps, nil
}

func (s *DoltStore) buildWispDependencyMetadata(ctx context.Context, deps []wispDependencyMetadata) ([]*types.IssueWithDependencyMetadata, error) {
	ids := make([]string, len(deps))
	for i, d := range deps {
		ids[i] = d.depID
	}
	issues, err := s.GetIssuesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	issueMap := make(map[string]*types.Issue, len(issues))
	for _, iss := range issues {
		issueMap[iss.ID] = iss
	}

	var results []*types.IssueWithDependencyMetadata
	for _, d := range deps {
		issue, ok := issueMap[d.depID]
		if !ok {
			continue
		}
		results = append(results, &types.IssueWithDependencyMetadata{
			Issue:          *issue,
			DependencyType: types.DependencyType(d.depType),
		})
	}
	return results, nil
}

// FindWispDependentsRecursive finds all wisp dependents of the given IDs,
// recursively. Uses batched IN-clause queries against wisp_dependencies for
// efficiency. Returns the set of all discovered dependent IDs (excluding the
// input IDs). Capped at maxRecursiveResults to prevent runaway traversal.
func (s *DoltStore) FindWispDependentsRecursive(ctx context.Context, ids []string) (map[string]bool, error) {
	var result map[string]bool
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.FindWispDependentsRecursiveInTx(ctx, tx, ids)
		return err
	})
	return result, err
}
