package domain

import (
	"context"
	"fmt"
	"regexp"

	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/types"
)

func previewDelete(ctx context.Context, deps *issueUseCaseDeps, ids []string) (DeletePreview, error) {
	preview := DeletePreview{
		Issues:          map[string]*types.Issue{},
		ConnectedIssues: map[string]*types.Issue{},
		DepRecords:      map[string][]*types.Dependency{},
	}
	if len(ids) == 0 {
		return preview, nil
	}
	if err := loadPreviewIssues(ctx, deps.issueRepo, ids, preview.Issues); err != nil {
		return preview, err
	}
	preview.NotFound = previewMissingIDs(ids, preview.Issues)
	if err := loadPreviewDependencies(ctx, deps.depRepo, ids, preview.DepRecords); err != nil {
		return preview, err
	}

	allIDs, err := deps.issueRepo.FindAllDependents(ctx, ids)
	if err != nil {
		return preview, fmt.Errorf("previewDelete: cascade expansion: %w", err)
	}
	connected, _, err := collectConnectedIssues(ctx, deps, allIDs, deleteIDSet(allIDs))
	if err != nil {
		return preview, err
	}
	preview.ConnectedIssues = connected
	return preview, nil
}

func loadPreviewIssues(ctx context.Context, repo IssueSQLRepository, ids []string, issues map[string]*types.Issue) error {
	fromIssues, err := repo.GetByIDs(ctx, ids, IssueTableOpts{})
	if err != nil {
		return fmt.Errorf("previewDelete: load issues: %w", err)
	}
	addPreviewIssues(issues, fromIssues)
	fromWisps, err := repo.GetByIDs(ctx, ids, IssueTableOpts{UseWispsTable: true})
	if err != nil && !dberrors.IsTableNotExist(err) {
		return fmt.Errorf("previewDelete: load wisps: %w", err)
	}
	addPreviewIssues(issues, fromWisps)
	return nil
}

func addPreviewIssues(target map[string]*types.Issue, issues []*types.Issue) {
	for _, issue := range issues {
		target[issue.ID] = issue
	}
}

func previewMissingIDs(ids []string, issues map[string]*types.Issue) []string {
	missing := make([]string, 0)
	for _, id := range ids {
		if _, ok := issues[id]; !ok {
			missing = append(missing, id)
		}
	}
	return missing
}

func loadPreviewDependencies(ctx context.Context, repo DependencySQLRepository, ids []string, records map[string][]*types.Dependency) error {
	depRes, err := repo.ListByIssueIDs(ctx, ids, DepListOpts{Direction: DepDirectionOut})
	if err != nil {
		return fmt.Errorf("previewDelete: list deps: %w", err)
	}
	for id, deps := range depRes.Outgoing {
		records[id] = deps
	}
	wispDepRes, err := repo.ListByIssueIDs(ctx, ids, DepListOpts{Direction: DepDirectionOut, UseWispsTable: true})
	if err != nil && !dberrors.IsTableNotExist(err) {
		return fmt.Errorf("previewDelete: list wisp deps: %w", err)
	}
	for id, deps := range wispDepRes.Outgoing {
		records[id] = append(records[id], deps...)
	}
	return nil
}

func collectConnectedIssues(
	ctx context.Context, deps *issueUseCaseDeps, allIDs []string, deletedSet map[string]bool,
) (map[string]*types.Issue, map[string]bool, error) {
	out := map[string]*types.Issue{}
	isWisp := map[string]bool{}
	if len(allIDs) == 0 {
		return out, isWisp, nil
	}

	issueRes, wispRes, err := loadConnectedDependencies(ctx, deps.depRepo, allIDs)
	if err != nil {
		return nil, nil, err
	}
	neighbors := connectedDependencyNeighbors(deletedSet, issueRes, wispRes)

	if len(neighbors) == 0 {
		return out, isWisp, nil
	}
	ids := make([]string, 0, len(neighbors))
	for id := range neighbors {
		ids = append(ids, id)
	}
	return hydrateConnectedIssues(ctx, deps.issueRepo, ids)
}

func loadConnectedDependencies(ctx context.Context, repo DependencySQLRepository, ids []string) (DepBulkResult, DepBulkResult, error) {
	issueRes, err := repo.ListByIssueIDs(ctx, ids, DepListOpts{Direction: DepDirectionBoth})
	if err != nil {
		return DepBulkResult{}, DepBulkResult{}, fmt.Errorf("collectConnected (issues): %w", err)
	}
	wispRes, err := repo.ListByIssueIDs(ctx, ids, DepListOpts{Direction: DepDirectionBoth, UseWispsTable: true})
	if err != nil && !dberrors.IsTableNotExist(err) {
		return DepBulkResult{}, DepBulkResult{}, fmt.Errorf("collectConnected (wisps): %w", err)
	}
	return issueRes, wispRes, nil
}

func connectedDependencyNeighbors(deletedSet map[string]bool, results ...DepBulkResult) map[string]bool {
	neighbors := map[string]bool{}
	for _, result := range results {
		addDependencyNeighbors(neighbors, deletedSet, result.Outgoing)
		addDependencyNeighbors(neighbors, deletedSet, result.Incoming)
	}
	return neighbors
}

func addDependencyNeighbors(neighbors, deletedSet map[string]bool, dependencies map[string][]*types.Dependency) {
	for _, deps := range dependencies {
		for _, dep := range deps {
			addDependencyNeighbor(neighbors, deletedSet, dep.IssueID)
			addDependencyNeighbor(neighbors, deletedSet, dep.DependsOnID)
		}
	}
}

func addDependencyNeighbor(neighbors, deletedSet map[string]bool, candidate string) {
	if candidate == "" || deletedSet[candidate] {
		return
	}
	neighbors[candidate] = true
}

func hydrateConnectedIssues(ctx context.Context, repo IssueSQLRepository, ids []string) (map[string]*types.Issue, map[string]bool, error) {
	out := map[string]*types.Issue{}
	isWisp := map[string]bool{}
	fromIssues, err := repo.GetByIDs(ctx, ids, IssueTableOpts{})
	if err != nil {
		return nil, nil, fmt.Errorf("hydrate neighbors (issues): %w", err)
	}
	addPreviewIssues(out, fromIssues)
	fromWisps, err := repo.GetByIDs(ctx, ids, IssueTableOpts{UseWispsTable: true})
	if err != nil && !dberrors.IsTableNotExist(err) {
		return nil, nil, fmt.Errorf("hydrate neighbors (wisps): %w", err)
	}
	for _, issue := range fromWisps {
		out[issue.ID] = issue
		isWisp[issue.ID] = true
	}
	return out, isWisp, nil
}

func rewriteTextReferences(
	ctx context.Context, deps *issueUseCaseDeps, deletedIDs []string,
	connected map[string]*types.Issue, isWisp map[string]bool, actor string,
) (int, error) {
	touched := make(map[string]bool)
	for _, id := range deletedIDs {
		pattern := `(^|[^A-Za-z0-9_-])(` + regexp.QuoteMeta(id) + `)($|[^A-Za-z0-9_-])`
		re := regexp.MustCompile(pattern)
		replacement := `$1[deleted:` + id + `]$3`
		for connID, conn := range connected {
			changed, err := rewriteConnectedIssue(ctx, deps.issueRepo, connID, conn, isWisp[connID], actor, re, replacement)
			if err != nil {
				return len(touched), err
			}
			if changed {
				touched[connID] = true
			}
		}
	}
	return len(touched), nil
}

func rewriteConnectedIssue(ctx context.Context, repo IssueSQLRepository, connID string, conn *types.Issue, useWisp bool, actor string, re *regexp.Regexp, replacement string) (bool, error) {
	updates := rewrittenIssueFields(conn, re, replacement)
	if len(updates) == 0 {
		return false, nil
	}
	if err := repo.Update(ctx, connID, updates, actor, IssueTableOpts{UseWispsTable: useWisp}); err != nil {
		return false, fmt.Errorf("rewrite refs %s: %w", connID, err)
	}
	applyRewrittenIssueFields(conn, updates)
	return true, nil
}

func rewrittenIssueFields(issue *types.Issue, re *regexp.Regexp, replacement string) map[string]any {
	updates := map[string]any{}
	if re.MatchString(issue.Description) {
		updates["description"] = re.ReplaceAllString(issue.Description, replacement)
	}
	if issue.Notes != "" && re.MatchString(issue.Notes) {
		updates["notes"] = re.ReplaceAllString(issue.Notes, replacement)
	}
	if issue.Design != "" && re.MatchString(issue.Design) {
		updates["design"] = re.ReplaceAllString(issue.Design, replacement)
	}
	if issue.AcceptanceCriteria != "" && re.MatchString(issue.AcceptanceCriteria) {
		updates["acceptance_criteria"] = re.ReplaceAllString(issue.AcceptanceCriteria, replacement)
	}
	return updates
}

func applyRewrittenIssueFields(issue *types.Issue, updates map[string]any) {
	if description, ok := updates["description"].(string); ok {
		issue.Description = description
	}
	if notes, ok := updates["notes"].(string); ok {
		issue.Notes = notes
	}
	if design, ok := updates["design"].(string); ok {
		issue.Design = design
	}
	if acceptanceCriteria, ok := updates["acceptance_criteria"].(string); ok {
		issue.AcceptanceCriteria = acceptanceCriteria
	}
}

// countDeletedDependencies counts every dependency row this deletion removes,
// exactly once, across BOTH planes and BOTH ends of each edge.
//
// It replaces a pair of CountAllForIDs calls that paired each plane's ids with
// that plane's table only. Two shapes escaped them:
//
//   - a durable row depending on a deleted WISP lives in `dependencies` with
//     the wisp as the target, and the wisp was only ever checked against
//     wisp_dependencies;
//   - a surviving wisp depending on a deleted DURABLE row is the mirror.
//
// Both edges really are removed — one by the explicit cross-plane delete
// below, the other by an ON DELETE CASCADE — so the count under-reported real
// removals, and the two CLI routes printed different numbers for the same
// delete.
//
// The old predicate also DOUBLE-counted: `issue_id IN (batch) OR target IN
// (batch)` was run per 50-id batch, so an edge whose two ends fell in
// different batches matched twice. Keying by the edge itself removes that
// hazard rather than trading it for another: a row is counted once whether it
// is reached as somebody's outbound edge, somebody's inbound edge, or both.
func countDeletedDependencies(ctx context.Context, deps *issueUseCaseDeps, allIDs []string) (int, error) {
	if len(allIDs) == 0 {
		return 0, nil
	}
	seen := make(map[string]bool)
	for _, useWisps := range []bool{false, true} {
		edges, err := deps.depRepo.ListByIssueIDs(ctx, allIDs, DepListOpts{Direction: DepDirectionBoth, UseWispsTable: useWisps})
		if err != nil {
			if useWisps && dberrors.IsTableNotExist(err) {
				continue
			}
			return 0, fmt.Errorf("delete: count deps: %w", err)
		}
		addDeletedDependencyEdges(seen, useWisps, edges)
	}
	return len(seen), nil
}

func addDeletedDependencyEdges(seen map[string]bool, useWisps bool, edges DepBulkResult) {
	addDeletedDependencySide(seen, useWisps, edges.Outgoing)
	addDeletedDependencySide(seen, useWisps, edges.Incoming)
}

func addDeletedDependencySide(seen map[string]bool, useWisps bool, side map[string][]*types.Dependency) {
	for _, dependencies := range side {
		addDeletedDependencyList(seen, useWisps, dependencies)
	}
}

func addDeletedDependencyList(seen map[string]bool, useWisps bool, dependencies []*types.Dependency) {
	for _, dependency := range dependencies {
		if dependency == nil {
			continue
		}
		// (source, target) is unique per table: the writer refuses a
		// second edge for a pair, retyping included.
		seen[fmt.Sprintf("%t\x00%s\x00%s", useWisps, dependency.IssueID, dependency.DependsOnID)] = true
	}
}
