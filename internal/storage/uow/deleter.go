package uow

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/workapi"
	publicops "github.com/jonbaldie/beads/issueops"
)

// DeleterSource is the capability accessor a unit-of-work provider offers for
// the named-row erasure role, the sibling of SweeperSource and CounterSource.
type DeleterSource interface {
	Deleter() (publicops.Deleter, error)
}

// deleter erases named rows through a unit of work.
type deleter struct {
	provider UnitOfWorkProvider
}

// Deleter returns the named-row erasure surface for this provider.
func (p *doltSQLProvider) Deleter() (publicops.Deleter, error) {
	return NewDeleter(p)
}

// NewDeleter constructs a public deleter backed by provider.
func NewDeleter(provider UnitOfWorkProvider) (publicops.Deleter, error) {
	if isNilUnitOfWorkProvider(provider) {
		return nil, fmt.Errorf("new deleter: unit-of-work provider must not be nil")
	}
	return &deleter{provider: provider}, nil
}

var _ publicops.Deleter = (*deleter)(nil)

// Delete erases the request's ids inside ONE unit of work.
//
// This is the genuinely separate body: the two store backends share
// issueops.DeleteInTx, and this one reaches the same questions through the
// domain use cases. What it must NOT do differently is which rows go and which
// requests are refused — the id normalization and the request rules run through
// the same internal/workapi functions, and the conformance contract asserts the
// two equal.
//
// THE GUARD IS HERE RATHER THAN IN THE USE CASE, which selects the right SET
// for both modes but has never refused anything. The refusal belongs above it
// with the rest of the role's policy, where a future use-case caller cannot
// inherit the capability and miss it.
//
// A DRY RUN TAKES A READ-ONLY UNIT OF WORK: it writes nothing, so it must not
// take the committing path and leave a history entry describing a preview.
func (d *deleter) Delete(ctx context.Context, req publicops.DeleteRequest) (publicops.DeleteResult, error) {
	if err := workapi.ValidateDeleteRequest(req); err != nil {
		return publicops.DeleteResult{}, err
	}
	req.IDs = workapi.NormalizeDeleteIDs(req.IDs)

	if req.DryRun {
		return RunTxRead(ctx, d.provider, func(ctx context.Context, uw UnitOfWork) (publicops.DeleteResult, error) {
			return deleteInUOW(ctx, uw, req)
		})
	}
	return RunTxResult(ctx, d.provider, func(ctx context.Context, uw UnitOfWork) (publicops.DeleteResult, string, error) {
		result, err := deleteInUOW(ctx, uw, req)
		if err != nil || result.Deleted == 0 {
			// A deletion that removed nothing labels nothing: the role
			// promises at most one history entry per call and none for a
			// no-op.
			return result, "", err
		}
		return result, fmt.Sprintf("bd: delete %d issue(s)", result.Deleted), nil
	})
}

// deleteInUOW is the whole deletion on one unit of work, shared by the preview
// path and the committing one so the two cannot answer differently:
// issueops.Deleter promises that a dry run refuses exactly where the real run
// refuses.
func deleteInUOW(ctx context.Context, uw UnitOfWork, req publicops.DeleteRequest) (publicops.DeleteResult, error) {
	issueUC := uw.IssueUseCase()
	result := publicops.DeleteResult{DryRun: req.DryRun}
	present, err := resolveDeleteRows(ctx, issueUC, req.IDs)
	if err != nil {
		return publicops.DeleteResult{}, err
	}
	if err := requireDeleteRowsPresent(req.IDs, present); err != nil {
		return publicops.DeleteResult{}, err
	}
	if err := checkDeleteVersion(req, present); err != nil {
		return publicops.DeleteResult{}, err
	}
	if !req.Cascade {
		orphaned, err := checkDeleteDependents(ctx, uw, req)
		if err != nil {
			return publicops.DeleteResult{}, err
		}
		result.Orphaned = orphaned
	}
	return executeDelete(ctx, issueUC, req, result)
}

func resolveDeleteRows(ctx context.Context, issueUC domain.IssueUseCase, ids []string) (map[string]*types.Issue, error) {
	present := make(map[string]*types.Issue, len(ids))
	for _, load := range []func(context.Context, []string) ([]*types.Issue, error){
		issueUC.GetIssuesByIDs,
		issueUC.GetWispsByIDs,
	} {
		rows, err := load(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("delete: resolve ids: %w", err)
		}
		for _, row := range rows {
			if row != nil {
				present[row.ID] = row
			}
		}
	}
	return present, nil
}

func requireDeleteRowsPresent(ids []string, present map[string]*types.Issue) error {
	var missing []string
	for _, id := range ids {
		if present[id] == nil {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return &publicops.NotFoundError{IDs: missing}
	}
	return nil
}

func checkDeleteVersion(req publicops.DeleteRequest, present map[string]*types.Issue) error {
	if req.ExpectedVersion == nil {
		return nil
	}
	current := present[req.IDs[0]].RowVersion
	if current == *req.ExpectedVersion {
		return nil
	}
	return fmt.Errorf("%w: expected %d, got %d", publicops.ErrVersionMismatch, *req.ExpectedVersion, current)
}

func checkDeleteDependents(ctx context.Context, uw UnitOfWork, req publicops.DeleteRequest) ([]string, error) {
	idSet := make(map[string]bool, len(req.IDs))
	for _, id := range req.IDs {
		idSet[id] = true
	}
	external, err := externalDependentsBySourceInUOW(ctx, uw, req.IDs, idSet)
	if err != nil {
		return nil, err
	}
	if !req.Force {
		return refuseDeleteDependents(req.IDs, external)
	}
	return orphanedDeleteDependents(external), nil
}

func refuseDeleteDependents(ids []string, external map[string][]string) ([]string, error) {
	for _, id := range ids {
		if deps := external[id]; len(deps) > 0 {
			return nil, &publicops.DependentsOutsideRequestError{IssueID: id, Dependents: deps}
		}
	}
	return nil, nil
}

func orphanedDeleteDependents(external map[string][]string) []string {
	orphaned := make(map[string]bool)
	for _, deps := range external {
		for _, id := range deps {
			orphaned[id] = true
		}
	}
	return workapi.SortedDeleteIDs(orphaned)
}

func executeDelete(ctx context.Context, issueUC domain.IssueUseCase, req publicops.DeleteRequest, result publicops.DeleteResult) (publicops.DeleteResult, error) {
	deleted, err := issueUC.DeleteIssues(ctx, domain.DeleteIssuesParams{
		IDs:                  req.IDs,
		Cascade:              req.Cascade,
		DryRun:               req.DryRun,
		UpdateTextReferences: !req.DryRun,
	}, req.Actor)
	if err != nil {
		return publicops.DeleteResult{}, err
	}
	result.Deleted = deleted.DeletedCount
	result.Dependencies = deleted.DependenciesCount
	result.Labels = deleted.LabelsCount
	result.Events = deleted.EventsCount
	result.ReferencesUpdated = deleted.ReferencesUpdated
	return result, nil
}

// externalDependentsBySourceInUOW reports, for each of ids, the DIRECT
// dependents idSet does not contain — the rows a forced delete orphans and an
// unforced one refuses over.
//
// It asks both planes, the way the shared store body's
// issueops.ExternalDependentsBySourceInTx does, because an edge from a wisp
// into an issue lives in the wisp table and a guard that missed it would
// silently orphan the wisp.
func externalDependentsBySourceInUOW(
	ctx context.Context, uw UnitOfWork, ids []string, idSet map[string]bool,
) (map[string][]string, error) {
	depUC := uw.DependencyUseCase()
	bySource := make(map[string]map[string]bool)

	for _, plane := range []struct {
		list     func(context.Context, []string, domain.DepListFilter) (domain.DepBulkResult, error)
		optional bool
	}{
		{list: depUC.ListByIssueIDs},
		// The wisp plane is optional the way every other cross-plane read here
		// treats it: a workspace whose schema predates it has no table, and
		// that is not a failed guard.
		{list: depUC.ListByWispIDs, optional: true},
	} {
		res, err := listExternalDependents(ctx, ids, plane.list, plane.optional)
		if err != nil {
			return nil, err
		}
		mergeExternalDependents(bySource, res.Incoming, idSet)
	}

	out := make(map[string][]string, len(bySource))
	for target, dependents := range bySource {
		out[target] = workapi.SortedDeleteIDs(dependents)
	}
	return out, nil
}

func listExternalDependents(ctx context.Context, ids []string, list func(context.Context, []string, domain.DepListFilter) (domain.DepBulkResult, error), optional bool) (domain.DepBulkResult, error) {
	res, err := list(ctx, ids, domain.DepListFilter{Direction: domain.DepDirectionIn})
	if err != nil {
		if optional && dberrors.IsTableNotExist(err) {
			return domain.DepBulkResult{}, nil
		}
		return domain.DepBulkResult{}, fmt.Errorf("delete: check dependents: %w", err)
	}
	return res, nil
}

func mergeExternalDependents(bySource map[string]map[string]bool, incoming map[string][]*types.Dependency, idSet map[string]bool) {
	for target, edges := range incoming {
		for _, edge := range edges {
			if edge == nil || idSet[edge.IssueID] {
				continue
			}
			if bySource[target] == nil {
				bySource[target] = make(map[string]bool)
			}
			bySource[target][edge.IssueID] = true
		}
	}
}
