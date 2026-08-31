package domain

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	publicops "github.com/jonbaldie/beads/issueops"
)

func (u *issueUpdateModule) UpdateIssue(ctx context.Context, id string, updates map[string]any, actor string) error {
	return u.update(ctx, id, updates, actor, false)
}

func (u *issueUpdateModule) UpdateWisp(ctx context.Context, id string, updates map[string]any, actor string) error {
	return u.update(ctx, id, updates, actor, true)
}

// CompareAndSetMetadataKey passes the plan straight through.
//
// No pre-check and no error wrapping, for the reason WalkDependencyTree gives:
// the request's whole vocabulary was validated before the transaction opened,
// and the refusals the body raises — storage.ErrNotFound for an id on neither
// plane — are typed sentinels both front doors classify.
func (u *issueUpdateModule) CompareAndSetMetadataKey(ctx context.Context, plan storage.CompareAndSetKeyPlan) (publicops.CompareAndSetKeyResult, bool, error) {
	return u.issueRepo.CompareAndSetMetadataKey(ctx, plan)
}

// ReleaseIssue passes the request straight through.
//
// No pre-check and no error wrapping, for the reason CompareAndSetMetadataKey
// gives: the request's whole vocabulary was validated before the transaction
// opened, and every refusal the body raises is a typed sentinel both front
// doors classify.
func (u *issueUpdateModule) ReleaseIssue(ctx context.Context, req publicops.ReleaseRequest) (publicops.ReleaseResult, bool, error) {
	return u.issueRepo.ReleaseIssue(ctx, req)
}

func (u *issueUpdateModule) update(ctx context.Context, id string, updates map[string]any, actor string, useWisp bool) error {
	if id == "" {
		return fmt.Errorf("update: id must not be empty")
	}
	if len(updates) == 0 {
		return nil
	}
	if err := validateCanonicalIssueUpdates(updates); err != nil {
		return err
	}
	if err := u.validateIssueTypeUpdate(ctx, updates); err != nil {
		return err
	}
	return u.issueRepo.Update(ctx, id, updates, actor, IssueTableOpts{UseWispsTable: useWisp})
}

// validateIssueTypeUpdate rejects an issue_type the configuration does not
// define. It reads the configured custom types, so unlike
// validateCanonicalIssueUpdates it needs the use case and a context.
func (u *issueUpdateModule) validateIssueTypeUpdate(ctx context.Context, updates map[string]any) error {
	rawType, ok := updates["issue_type"]
	if !ok {
		return nil
	}
	issueType, ok := rawType.(string)
	if !ok {
		return nil
	}
	customTypes, err := u.cfgRepo.GetCustomTypes(ctx)
	if err != nil {
		return fmt.Errorf("update: read custom types: %w", err)
	}
	if !types.IssueType(issueType).IsValidWithCustom(customTypes) {
		return fmt.Errorf("%w: invalid issue type: %s", storage.ErrValidation, issueType)
	}
	return nil
}

// validateCanonicalIssueUpdates rejects out-of-range values for the canonical
// scalar columns, and rejects values whose Go type the column cannot carry. An
// unsupported type used to fall through unvalidated and reach the SQL layer,
// which coerced it silently -- a title of 7 was stored as "7", and an int64
// priority or estimate skipped its range check entirely.
func validateCanonicalIssueUpdates(updates map[string]any) error {
	if value, ok := updates["title"]; ok {
		if err := validateCanonicalTitleUpdate(value); err != nil {
			return err
		}
	}
	if value, ok := updates["priority"]; ok {
		if err := validateCanonicalPriorityUpdate(value); err != nil {
			return err
		}
	}
	if value, ok := updates["estimated_minutes"]; ok {
		if err := validateCanonicalEstimateUpdate(value); err != nil {
			return err
		}
	}
	return nil
}

func validateCanonicalTitleUpdate(value any) error {
	title, ok := value.(string)
	if !ok {
		return canonicalIssueUpdateTypeError("title", value, "string")
	}
	if err := types.ValidateIssueTitle(title); err != nil {
		return canonicalIssueUpdateValidationError("title", err)
	}
	return nil
}

func validateCanonicalPriorityUpdate(value any) error {
	priority, ok := value.(int)
	if !ok {
		return canonicalIssueUpdateTypeError("priority", value, "int")
	}
	if err := types.ValidateIssuePriority(priority); err != nil {
		return canonicalIssueUpdateValidationError("priority", err)
	}
	return nil
}

func validateCanonicalEstimateUpdate(value any) error {
	var estimatedMinutes *int
	switch value := value.(type) {
	case int:
		estimatedMinutes = &value
	case *int:
		estimatedMinutes = value
	case nil:
		// Clearing the estimate; a nil estimate validates.
	default:
		return canonicalIssueUpdateTypeError("estimated_minutes", value, "int or *int")
	}
	if err := types.ValidateIssueEstimatedMinutes(estimatedMinutes); err != nil {
		return canonicalIssueUpdateValidationError("estimated_minutes", err)
	}
	return nil
}

func canonicalIssueUpdateValidationError(field string, err error) error {
	return fmt.Errorf("%w: update field %q: %w", storage.ErrValidation, field, err)
}

func canonicalIssueUpdateTypeError(field string, value any, want string) error {
	return fmt.Errorf("%w: update field %q: expected %s, got %T", storage.ErrValidation, field, want, value)
}
