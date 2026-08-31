package domain

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/validation"
)

func (u *issueUpdateModule) ApplyUpdate(ctx context.Context, id string, spec UpdateSpec, actor string) (*types.Issue, error) {
	if id == "" {
		return nil, fmt.Errorf("ApplyUpdate: id must not be empty")
	}

	useWisp, err := isWispID(ctx, u.issueRepo, id)
	if err != nil {
		return nil, fmt.Errorf("ApplyUpdate %s: %w", id, err)
	}

	if err := validateApplyUpdateGuards(ctx, u.lookup, id, spec, useWisp); err != nil {
		return nil, err
	}

	// Validate the requested field values before anything mutates. The claim
	// below writes assignee and status, so a spec that pairs Claim with an
	// invalid field used to leave the issue claimed by an update that never
	// applied.
	if err := validateCanonicalIssueUpdates(spec.Fields); err != nil {
		return nil, err
	}
	if err := u.validateIssueTypeUpdate(ctx, spec.Fields); err != nil {
		return nil, err
	}

	useWisp, err = applyUpdateMutations(ctx, u, id, spec, actor, useWisp)
	if err != nil {
		return nil, err
	}

	issue, err := reloadUpdatedIssue(ctx, u.lookup, id, useWisp)
	if err != nil {
		return nil, fmt.Errorf("ApplyUpdate: re-fetch %s: %w", id, err)
	}
	return issue, nil
}

func validateApplyUpdateGuards(ctx context.Context, lookup *issueLookupModule, id string, spec UpdateSpec, useWisp bool) error {
	if spec.ExpectedVersion == nil && spec.ExpectedAssignee == nil && spec.ExpectedStatus == nil {
		return nil
	}
	var (
		current *types.Issue
		err     error
	)
	if useWisp {
		current, err = lookup.GetWisp(ctx, id)
	} else {
		current, err = lookup.GetIssue(ctx, id)
	}
	if err != nil {
		return fmt.Errorf("ApplyUpdate: read %s for guards: %w", id, err)
	}
	if current == nil {
		return fmt.Errorf("%w: issue %s", storage.ErrNotFound, id)
	}
	return validateApplyUpdateGuardValues(id, current, spec)
}

func validateApplyUpdateGuardValues(id string, current *types.Issue, spec UpdateSpec) error {
	if spec.ExpectedVersion != nil && current.RowVersion != *spec.ExpectedVersion {
		return fmt.Errorf("%w: expected %d, got %d", storage.ErrVersionMismatch, *spec.ExpectedVersion, current.RowVersion)
	}
	if spec.ExpectedAssignee != nil && !validation.ActorMatches(current.Assignee, *spec.ExpectedAssignee) {
		return fmt.Errorf("%w: %s is held by %q, expected %q", storage.ErrAssigneeMismatch, id, current.Assignee, *spec.ExpectedAssignee)
	}
	if spec.ExpectedStatus != nil && string(current.Status) != *spec.ExpectedStatus {
		return fmt.Errorf("%w: %s has status %q, expected %q", storage.ErrStatusMismatch, id, current.Status, *spec.ExpectedStatus)
	}
	return nil
}

func applyUpdateMutations(ctx context.Context, u *issueUpdateModule, id string, spec UpdateSpec, actor string, useWisp bool) (bool, error) {
	if err := applyUpdateClaim(ctx, u.claims, id, actor, useWisp, spec.Claim); err != nil {
		return false, err
	}
	if err := applyUpdateFields(ctx, u.issueRepo, id, spec.Fields, actor, useWisp); err != nil {
		return false, err
	}
	if err := applyUpdateLabels(ctx, u.labelUC, id, spec, actor, useWisp); err != nil {
		return false, err
	}
	if err := applyUpdateReparent(ctx, u.depUC, id, spec.Reparent, actor, useWisp); err != nil {
		return false, err
	}
	return applyUpdatePersistence(ctx, u.issueRepo, id, spec.Persistence, useWisp)
}

func applyUpdateClaim(ctx context.Context, claims *issueClaimModule, id, actor string, useWisp, claim bool) error {
	if !claim {
		return nil
	}
	if useWisp {
		_, err := claims.ClaimWisp(ctx, id, actor)
		return err
	}
	_, err := claims.ClaimIssue(ctx, id, actor)
	return err
}

func applyUpdateFields(ctx context.Context, repo IssueSQLRepository, id string, fields map[string]any, actor string, useWisp bool) error {
	if len(fields) == 0 {
		return nil
	}
	return repo.Update(ctx, id, fields, actor, IssueTableOpts{UseWispsTable: useWisp})
}

func applyUpdateLabels(ctx context.Context, labelUC LabelUseCase, id string, spec UpdateSpec, actor string, useWisp bool) error {
	if err := applyUpdateSetLabels(ctx, labelUC, id, spec.SetLabels, actor, useWisp); err != nil {
		return err
	}
	if err := applyUpdateAddLabels(ctx, labelUC, id, spec.AddLabels, actor, useWisp); err != nil {
		return err
	}
	return applyUpdateRemoveLabels(ctx, labelUC, id, spec.RemoveLabels, actor, useWisp)

}

func applyUpdateSetLabels(ctx context.Context, labelUC LabelUseCase, id string, labels *[]string, actor string, useWisp bool) error {
	if labels == nil {
		return nil
	}
	if useWisp {
		return labelUC.SetWispLabels(ctx, id, *labels, actor)
	}
	return labelUC.SetLabels(ctx, id, *labels, actor)
}

func applyUpdateAddLabels(ctx context.Context, labelUC LabelUseCase, id string, labels []string, actor string, useWisp bool) error {
	if len(labels) == 0 {
		return nil
	}
	if useWisp {
		return labelUC.AddWispLabels(ctx, id, labels, actor)
	}
	return labelUC.AddLabels(ctx, id, labels, actor)
}

func applyUpdateRemoveLabels(ctx context.Context, labelUC LabelUseCase, id string, labels []string, actor string, useWisp bool) error {
	if len(labels) == 0 {
		return nil
	}
	if useWisp {
		return labelUC.RemoveWispLabels(ctx, id, labels, actor)
	}
	return labelUC.RemoveLabels(ctx, id, labels, actor)
}

func applyUpdateReparent(ctx context.Context, depUC DependencyUseCase, id string, parent *string, actor string, useWisp bool) error {
	if parent == nil {
		return nil
	}
	if useWisp {
		return depUC.ReparentWisp(ctx, id, *parent, actor)
	}
	return depUC.Reparent(ctx, id, *parent, actor)
}

func applyUpdatePersistence(ctx context.Context, repo IssueSQLRepository, id string, persistence *types.PersistenceMode, useWisp bool) (bool, error) {
	if persistence == nil {
		return useWisp, nil
	}
	if _, err := repo.MovePersistence(ctx, id, *persistence); err != nil {
		return false, fmt.Errorf("ApplyUpdate: move persistence for %s: %w", id, err)
	}
	updatedUseWisp, err := isWispID(ctx, repo, id)
	if err != nil {
		return false, fmt.Errorf("ApplyUpdate: re-locate %s after persistence move: %w", id, err)
	}
	return updatedUseWisp, nil
}

func reloadUpdatedIssue(ctx context.Context, lookup *issueLookupModule, id string, useWisp bool) (*types.Issue, error) {
	if useWisp {
		return lookup.GetWisp(ctx, id)
	}
	return lookup.GetIssue(ctx, id)
}

func isWispID(ctx context.Context, repo IssueSQLRepository, id string) (bool, error) {
	found, err := repo.Exists(ctx, id, IssueTableOpts{UseWispsTable: true})
	if err != nil {
		if dberrors.IsTableNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("probe wisps table: %w", err)
	}
	return found, nil
}

// PromoteWisp moves an active wisp to the Dolt-versioned issues plane,
// preserving its id, wisp_type, labels, dependencies, events, and comments.
// One-way: the promoted row is no longer ephemeral, so purge won't reclaim
// it. The repository error passes through unwrapped — the CLI surfaces it
