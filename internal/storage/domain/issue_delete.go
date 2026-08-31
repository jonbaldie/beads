package domain

import (
	"context"
	"fmt"
	"sort"

	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/types"
)

// DeleteBlockedError is the refusal returned by deleteMany when
// EnforceCascadePolicy is on, Cascade and Force are both off, and an issue in
// the deletion set has dependents outside it. The message mirrors classic
// (embedded) delete's refusal so both planes speak the same language.
type DeleteBlockedError struct {
	// IssueID is the first issue in the requested deletion set (request order)
	// found to have external dependents.
	IssueID string
	// Dependents are that issue's dependents outside the deletion set, sorted.
	Dependents []string
}

func (e *DeleteBlockedError) Error() string {
	return fmt.Sprintf("issue %s has dependents not in deletion set; use --cascade to delete them or --force to orphan them", e.IssueID)
}

func (u *issueDeleteModule) DeleteIssue(ctx context.Context, id, actor string) (DeleteIssuesResult, error) {
	if id == "" {
		return DeleteIssuesResult{}, fmt.Errorf("DeleteIssue: id must not be empty")
	}
	return deleteMany(ctx, u.deps, DeleteIssuesParams{
		IDs:                  []string{id},
		Cascade:              true,
		UpdateTextReferences: true,
	}, actor)
}

func (u *issueDeleteModule) DeleteWisp(ctx context.Context, id, actor string) (DeleteIssuesResult, error) {
	if id == "" {
		return DeleteIssuesResult{}, fmt.Errorf("DeleteWisp: id must not be empty")
	}
	return deleteMany(ctx, u.deps, DeleteIssuesParams{
		IDs:                  []string{id},
		Cascade:              true,
		UpdateTextReferences: true,
	}, actor)
}

func (u *issueDeleteModule) DeleteIssues(ctx context.Context, params DeleteIssuesParams, actor string) (DeleteIssuesResult, error) {
	return deleteMany(ctx, u.deps, params, actor)
}

func (u *issueDeleteModule) DeleteWisps(ctx context.Context, params DeleteIssuesParams, actor string) (DeleteIssuesResult, error) {
	return deleteMany(ctx, u.deps, params, actor)
}

func (u *issueDeleteModule) PreviewDelete(ctx context.Context, ids []string) (DeletePreview, error) {
	return previewDelete(ctx, u.deps, ids)
}

func (u *issueDeleteModule) PreviewDeleteWisp(ctx context.Context, ids []string) (DeletePreview, error) {
	return previewDelete(ctx, u.deps, ids)
}

type issueDeleteModule struct {
	deps *issueUseCaseDeps
}

func deleteMany(ctx context.Context, deps *issueUseCaseDeps, params DeleteIssuesParams, actor string) (DeleteIssuesResult, error) {
	if len(params.IDs) == 0 {
		return DeleteIssuesResult{}, nil
	}

	result := DeleteIssuesResult{}
	allIDs, orphaned, err := prepareDeleteIDs(ctx, deps, params)
	result.OrphanedIssues = orphaned
	if err != nil {
		return result, err
	}
	if len(allIDs) == 0 {
		return DeleteIssuesResult{}, nil
	}

	wispIDs, regularIDs, err := partitionDeleteIDs(ctx, deps.issueRepo, allIDs)
	if err != nil {
		return DeleteIssuesResult{}, fmt.Errorf("delete: partition: %w", err)
	}

	if err := populateDeleteCounts(ctx, deps, allIDs, regularIDs, wispIDs, &result); err != nil {
		return DeleteIssuesResult{}, err
	}

	if params.DryRun {
		result.DeletedCount = len(regularIDs) + len(wispIDs)
		return result, nil
	}
	return executeDelete(ctx, deps, params, actor, allIDs, regularIDs, wispIDs, result)
}

func partitionDeleteIDs(ctx context.Context, repo IssueSQLRepository, allIDs []string) ([]string, []string, error) {
	wispIDs, regularIDs, err := repo.PartitionWispIDs(ctx, allIDs)
	if err != nil {
		return nil, nil, err
	}
	return wispIDs, regularIDs, nil
}

func executeDelete(ctx context.Context, deps *issueUseCaseDeps, params DeleteIssuesParams, actor string, allIDs, regularIDs, wispIDs []string, result DeleteIssuesResult) (DeleteIssuesResult, error) {
	var connected map[string]*types.Issue
	var connectedIsWisp map[string]bool
	var err error
	if params.UpdateTextReferences {
		connected, connectedIsWisp, err = connectedDeleteIssues(ctx, deps, allIDs)
		if err != nil {
			return result, err
		}
	}

	affectedIssues, affectedWisps, err := deps.issueRepo.AffectedByDeletion(ctx, regularIDs, wispIDs)
	if err != nil {
		return result, fmt.Errorf("delete: affected by deletion: %w", err)
	}

	if err := deleteRelatedRows(ctx, deps, regularIDs, wispIDs); err != nil {
		return result, err
	}
	deleted, err := deleteIssueRows(ctx, deps, regularIDs, wispIDs)
	if err != nil {
		return result, err
	}
	result.DeletedCount = deleted

	refs, err := rewriteDeleteReferences(ctx, deps, params.UpdateTextReferences, allIDs, connected, connectedIsWisp, actor)
	if err != nil {
		return result, err
	}
	result.ReferencesUpdated = refs

	if err := deps.issueRepo.RecomputeIsBlocked(ctx, affectedIssues, affectedWisps); err != nil {
		return result, fmt.Errorf("delete: recompute is_blocked: %w", err)
	}

	return result, nil
}

func rewriteDeleteReferences(ctx context.Context, deps *issueUseCaseDeps, enabled bool, allIDs []string, connected map[string]*types.Issue, connectedIsWisp map[string]bool, actor string) (int, error) {
	if !enabled || len(connected) == 0 {
		return 0, nil
	}
	refs, err := rewriteTextReferences(ctx, deps, allIDs, connected, connectedIsWisp, actor)
	if err != nil {
		return 0, fmt.Errorf("delete: rewrite text references: %w", err)
	}
	return refs, nil
}

func prepareDeleteIDs(ctx context.Context, deps *issueUseCaseDeps, params DeleteIssuesParams) ([]string, []string, error) {
	if params.Cascade {
		allIDs, err := deps.issueRepo.FindAllDependents(ctx, params.IDs)
		if err != nil {
			return nil, nil, fmt.Errorf("delete: cascade expansion: %w", err)
		}
		return allIDs, nil, nil
	}
	if !params.EnforceCascadePolicy {
		return params.IDs, nil, nil
	}
	return applyDeleteCascadePolicy(ctx, deps, params)
}

func applyDeleteCascadePolicy(ctx context.Context, deps *issueUseCaseDeps, params DeleteIssuesParams) ([]string, []string, error) {
	// Embedded-parity dependent handling (see DeleteIssuesParams): without
	// Cascade, an external dependent either blocks the delete (no Force) or
	// is orphaned (Force), never silently swept.
	externalBySource, err := externalDependents(ctx, deps, params.IDs)
	if err != nil {
		return nil, nil, err
	}
	if params.Force {
		return params.IDs, orphanedDeleteDependents(externalBySource), nil
	}
	for _, id := range params.IDs {
		if dependents := externalBySource[id]; len(dependents) > 0 {
			sort.Strings(dependents)
			return params.IDs, dependents, &DeleteBlockedError{IssueID: id, Dependents: dependents}
		}
	}
	return params.IDs, nil, nil
}

func orphanedDeleteDependents(externalBySource map[string][]string) []string {
	orphanSet := map[string]bool{}
	for _, dependents := range externalBySource {
		for _, dependent := range dependents {
			orphanSet[dependent] = true
		}
	}
	return sortedStringSet(orphanSet)
}

func populateDeleteCounts(ctx context.Context, deps *issueUseCaseDeps, allIDs, regularIDs, wispIDs []string, result *DeleteIssuesResult) error {
	depCount, err := countDeletedDependencies(ctx, deps, allIDs)
	if err != nil {
		return err
	}
	result.DependenciesCount = depCount
	labels, err := countDeleteLabels(ctx, deps, regularIDs, wispIDs)
	if err != nil {
		return err
	}
	result.LabelsCount = labels
	events, err := countDeleteEvents(ctx, deps, regularIDs, wispIDs)
	if err != nil {
		return err
	}
	result.EventsCount = events
	return nil
}

func countDeleteLabels(ctx context.Context, deps *issueUseCaseDeps, regularIDs, wispIDs []string) (int, error) {
	regular, err := deps.labelRepo.CountAllForIDs(ctx, regularIDs, LabelOpts{})
	if err != nil {
		return 0, fmt.Errorf("delete: count labels: %w", err)
	}
	wisps, err := deps.labelRepo.CountAllForIDs(ctx, wispIDs, LabelOpts{UseWispsTable: true})
	if err != nil {
		return 0, fmt.Errorf("delete: count wisp labels: %w", err)
	}
	return regular + wisps, nil
}

func countDeleteEvents(ctx context.Context, deps *issueUseCaseDeps, regularIDs, wispIDs []string) (int, error) {
	regular, err := deps.eventsRepo.CountAllForIDs(ctx, regularIDs, RecordEventOpts{})
	if err != nil {
		return 0, fmt.Errorf("delete: count events: %w", err)
	}
	wisps, err := deps.eventsRepo.CountAllForIDs(ctx, wispIDs, RecordEventOpts{UseWispsTable: true})
	if err != nil {
		return 0, fmt.Errorf("delete: count wisp events: %w", err)
	}
	return regular + wisps, nil
}

func connectedDeleteIssues(ctx context.Context, deps *issueUseCaseDeps, allIDs []string) (map[string]*types.Issue, map[string]bool, error) {
	deletedSet := make(map[string]bool, len(allIDs))
	for _, id := range allIDs {
		deletedSet[id] = true
	}
	return collectConnectedIssues(ctx, deps, allIDs, deletedSet)
}

func deleteRelatedRows(ctx context.Context, deps *issueUseCaseDeps, regularIDs, wispIDs []string) error {
	if err := deleteDependencyRows(ctx, deps.depRepo, regularIDs, wispIDs); err != nil {
		return err
	}
	if err := deleteLabelRows(ctx, deps.labelRepo, regularIDs, wispIDs); err != nil {
		return err
	}
	return deleteEventRows(ctx, deps.eventsRepo, regularIDs, wispIDs)
}

func deleteDependencyRows(ctx context.Context, repo DependencySQLRepository, regularIDs, wispIDs []string) error {
	if _, err := repo.DeleteAllForIDs(ctx, regularIDs, DepInsertOpts{}); err != nil {
		return fmt.Errorf("delete: drop deps: %w", err)
	}
	if _, err := repo.DeleteAllForIDs(ctx, wispIDs, DepInsertOpts{UseWispsTable: true}); err != nil {
		return fmt.Errorf("delete: drop wisp deps: %w", err)
	}
	// The SYNC-PLANE edges pointing at a deleted wisp, which are not the same
	// rows as the line above and are not reached by a foreign key: there is no
	// FK from dependencies to wisps, so `dependencies.depends_on_wisp_id` rows
	// survive their target unless they are deleted explicitly. Without this a
	// forced delete of a wisp left its durable dependent holding an edge into
	// a row that no longer exists — dangling, not orphaned, which is not what
	// issueops.DeleteRequest.Force promises. The store body has always done
	// this (issueops.deleteIssueRowInTx -> DeleteWispFromDependenciesInTx).
	if _, err := repo.DeleteAllForIDs(ctx, wispIDs, DepInsertOpts{}); err != nil {
		return fmt.Errorf("delete: drop sync-plane edges into deleted wisps: %w", err)
	}
	return nil
}

func deleteLabelRows(ctx context.Context, repo LabelSQLRepository, regularIDs, wispIDs []string) error {
	if _, err := repo.DeleteAllForIDs(ctx, regularIDs, LabelOpts{}); err != nil {
		return fmt.Errorf("delete: drop labels: %w", err)
	}
	if _, err := repo.DeleteAllForIDs(ctx, wispIDs, LabelOpts{UseWispsTable: true}); err != nil {
		return fmt.Errorf("delete: drop wisp labels: %w", err)
	}
	return nil
}

func deleteEventRows(ctx context.Context, repo EventsSQLRepository, regularIDs, wispIDs []string) error {
	if _, err := repo.DeleteAllForIDs(ctx, regularIDs, RecordEventOpts{}); err != nil {
		return fmt.Errorf("delete: drop events: %w", err)
	}
	if _, err := repo.DeleteAllForIDs(ctx, wispIDs, RecordEventOpts{UseWispsTable: true}); err != nil {
		return fmt.Errorf("delete: drop wisp events: %w", err)
	}
	return nil
}

func deleteIssueRows(ctx context.Context, deps *issueUseCaseDeps, regularIDs, wispIDs []string) (int, error) {
	issuesDeleted, err := deps.issueRepo.DeleteByIDs(ctx, regularIDs, IssueTableOpts{})
	if err != nil {
		return 0, fmt.Errorf("delete: drop issue rows: %w", err)
	}
	wispsDeleted, err := deps.issueRepo.DeleteByIDs(ctx, wispIDs, IssueTableOpts{UseWispsTable: true})
	if err != nil {
		return 0, fmt.Errorf("delete: drop wisp rows: %w", err)
	}
	return issuesDeleted + wispsDeleted, nil
}

// externalDependents finds the direct dependents of each id in ids that are
// not themselves in ids, across both the issue and wisp dependency tables.
// The result maps deletion-set id -> external dependent ids (unsorted).
func externalDependents(ctx context.Context, deps *issueUseCaseDeps, ids []string) (map[string][]string, error) {
	idSet := deleteIDSet(ids)

	issueRes, err := deps.depRepo.ListByIssueIDs(ctx, ids, DepListOpts{Direction: DepDirectionIn})
	if err != nil {
		return nil, fmt.Errorf("delete: list dependents: %w", err)
	}
	wispRes, err := deps.depRepo.ListByIssueIDs(ctx, ids, DepListOpts{Direction: DepDirectionIn, UseWispsTable: true})
	if err != nil && !dberrors.IsTableNotExist(err) {
		return nil, fmt.Errorf("delete: list wisp dependents: %w", err)
	}

	out := map[string][]string{}
	seen := map[string]map[string]bool{}
	mergeExternalDependents(out, seen, issueRes.Incoming, idSet)
	mergeExternalDependents(out, seen, wispRes.Incoming, idSet)
	return out, nil
}

func deleteIDSet(ids []string) map[string]bool {
	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	return idSet
}

func mergeExternalDependents(out map[string][]string, seen map[string]map[string]bool, incoming map[string][]*types.Dependency, idSet map[string]bool) {
	for target, dependents := range incoming {
		for _, dependent := range dependents {
			mergeExternalDependent(out, seen, target, dependent, idSet)
		}
	}
}

func mergeExternalDependent(out map[string][]string, seen map[string]map[string]bool, target string, dependent *types.Dependency, idSet map[string]bool) {
	if dependent.IssueID == "" || idSet[dependent.IssueID] {
		return
	}
	if seen[target] == nil {
		seen[target] = map[string]bool{}
	}
	if seen[target][dependent.IssueID] {
		return
	}
	seen[target][dependent.IssueID] = true
	out[target] = append(out[target], dependent.IssueID)
}

func sortedStringSet(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
