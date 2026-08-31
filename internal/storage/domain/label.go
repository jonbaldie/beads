package domain

import (
	"context"
	"fmt"
)

type LabelOpts struct {
	UseWispsTable bool
}

type LabelSQLRepository interface {
	Insert(ctx context.Context, issueID, label, actor string, opts LabelOpts) error
	Delete(ctx context.Context, issueID, label, actor string, opts LabelOpts) error
	List(ctx context.Context, issueID string, opts LabelOpts) ([]string, error)
	ListByIssueIDs(ctx context.Context, issueIDs []string, opts LabelOpts) (map[string][]string, error)
	DeleteAllForIDs(ctx context.Context, ids []string, opts LabelOpts) (int, error)
	CountAllForIDs(ctx context.Context, ids []string, opts LabelOpts) (int, error)
}

type LabelUseCase interface {
	AddLabel(ctx context.Context, issueID, label, actor string) error
	RemoveLabel(ctx context.Context, issueID, label, actor string) error
	AddLabels(ctx context.Context, issueID string, labels []string, actor string) error
	RemoveLabels(ctx context.Context, issueID string, labels []string, actor string) error
	SetLabels(ctx context.Context, issueID string, labels []string, actor string) error
	GetLabels(ctx context.Context, issueID string) ([]string, error)
	GetLabelsForIssues(ctx context.Context, issueIDs []string) (map[string][]string, error)
	InheritFromParent(ctx context.Context, childID, parentID, actor string, skipExisting []string) ([]string, error)

	AddWispLabel(ctx context.Context, wispID, label, actor string) error
	RemoveWispLabel(ctx context.Context, wispID, label, actor string) error
	AddWispLabels(ctx context.Context, wispID string, labels []string, actor string) error
	RemoveWispLabels(ctx context.Context, wispID string, labels []string, actor string) error
	SetWispLabels(ctx context.Context, wispID string, labels []string, actor string) error
	GetWispLabels(ctx context.Context, wispID string) ([]string, error)
	GetLabelsForWisps(ctx context.Context, wispIDs []string) (map[string][]string, error)
	InheritFromWispParent(ctx context.Context, childWispID, parentWispID, actor string, skipExisting []string) ([]string, error)
}

func NewLabelUseCase(labelRepo LabelSQLRepository) LabelUseCase {
	return &labelUseCaseImpl{
		LabelMutations:   LabelMutations{labelRepo: labelRepo},
		LabelQueries:     LabelQueries{labelRepo: labelRepo},
		LabelInheritance: LabelInheritance{labelRepo: labelRepo},
	}
}

type labelUseCaseImpl struct {
	LabelMutations
	LabelQueries
	LabelInheritance
}

var _ LabelUseCase = (*labelUseCaseImpl)(nil)

// LabelMutations owns operations that add, remove, or replace labels. It is
// embedded by labelUseCaseImpl so the public use-case surface remains one
// cohesive interface while each mutation family stays small enough to reason
// about independently.
type LabelMutations struct {
	labelRepo LabelSQLRepository
}

func (u *LabelMutations) AddLabel(ctx context.Context, issueID, label, actor string) error {
	return u.add(ctx, issueID, label, actor, false)
}

func (u *LabelMutations) AddWispLabel(ctx context.Context, wispID, label, actor string) error {
	return u.add(ctx, wispID, label, actor, true)
}

func (u *LabelMutations) add(ctx context.Context, id, label, actor string, useWisp bool) error {
	if id == "" {
		return fmt.Errorf("add label: id must not be empty")
	}
	if label == "" {
		return fmt.Errorf("add label: label must not be empty")
	}
	if err := u.labelRepo.Insert(ctx, id, label, actor, LabelOpts{UseWispsTable: useWisp}); err != nil {
		return fmt.Errorf("add label %s/%s: %w", id, label, err)
	}
	return nil
}

func (u *LabelMutations) RemoveLabel(ctx context.Context, issueID, label, actor string) error {
	return u.remove(ctx, issueID, label, actor, false)
}

func (u *LabelMutations) RemoveWispLabel(ctx context.Context, wispID, label, actor string) error {
	return u.remove(ctx, wispID, label, actor, true)
}

func (u *LabelMutations) remove(ctx context.Context, id, label, actor string, useWisp bool) error {
	if id == "" {
		return fmt.Errorf("remove label: id must not be empty")
	}
	if label == "" {
		return fmt.Errorf("remove label: label must not be empty")
	}
	if err := u.labelRepo.Delete(ctx, id, label, actor, LabelOpts{UseWispsTable: useWisp}); err != nil {
		return fmt.Errorf("remove label %s/%s: %w", id, label, err)
	}
	return nil
}

func (u *LabelMutations) AddLabels(ctx context.Context, issueID string, labels []string, actor string) error {
	return u.addMany(ctx, issueID, labels, actor, false)
}

func (u *LabelMutations) AddWispLabels(ctx context.Context, wispID string, labels []string, actor string) error {
	return u.addMany(ctx, wispID, labels, actor, true)
}

func (u *LabelMutations) addMany(ctx context.Context, id string, labels []string, actor string, useWisp bool) error {
	if id == "" {
		return fmt.Errorf("add labels: id must not be empty")
	}
	opts := LabelOpts{UseWispsTable: useWisp}
	for _, label := range labels {
		if label == "" {
			continue
		}
		if err := u.labelRepo.Insert(ctx, id, label, actor, opts); err != nil {
			return fmt.Errorf("add labels: %s: %w", label, err)
		}
	}
	return nil
}

func (u *LabelMutations) RemoveLabels(ctx context.Context, issueID string, labels []string, actor string) error {
	return u.removeMany(ctx, issueID, labels, actor, false)
}

func (u *LabelMutations) RemoveWispLabels(ctx context.Context, wispID string, labels []string, actor string) error {
	return u.removeMany(ctx, wispID, labels, actor, true)
}

func (u *LabelMutations) removeMany(ctx context.Context, id string, labels []string, actor string, useWisp bool) error {
	if id == "" {
		return fmt.Errorf("remove labels: id must not be empty")
	}
	opts := LabelOpts{UseWispsTable: useWisp}
	for _, label := range labels {
		if label == "" {
			continue
		}
		if err := u.labelRepo.Delete(ctx, id, label, actor, opts); err != nil {
			return fmt.Errorf("remove labels: %s: %w", label, err)
		}
	}
	return nil
}

func (u *LabelMutations) SetLabels(ctx context.Context, issueID string, labels []string, actor string) error {
	return u.setMany(ctx, issueID, labels, actor, false)
}

func (u *LabelMutations) SetWispLabels(ctx context.Context, wispID string, labels []string, actor string) error {
	return u.setMany(ctx, wispID, labels, actor, true)
}

func (u *LabelMutations) setMany(ctx context.Context, id string, labels []string, actor string, useWisp bool) error {
	if id == "" {
		return fmt.Errorf("set labels: id must not be empty")
	}
	opts := LabelOpts{UseWispsTable: useWisp}
	current, err := u.labelRepo.List(ctx, id, opts)
	if err != nil {
		return fmt.Errorf("set labels: list current: %w", err)
	}
	desired := make(map[string]bool, len(labels))
	for _, l := range labels {
		if l != "" {
			desired[l] = true
		}
	}
	existing := make(map[string]bool, len(current))
	if err := u.removeStaleLabels(ctx, id, current, desired, actor, opts, existing); err != nil {
		return err
	}
	return u.addMissingLabels(ctx, id, desired, existing, actor, opts)
}

func (u *LabelMutations) removeStaleLabels(ctx context.Context, id string, current []string, desired map[string]bool, actor string, opts LabelOpts, existing map[string]bool) error {
	for _, label := range current {
		existing[label] = true
		if desired[label] {
			continue
		}
		if err := u.labelRepo.Delete(ctx, id, label, actor, opts); err != nil {
			return fmt.Errorf("set labels: remove %s: %w", label, err)
		}
	}
	return nil
}

func (u *LabelMutations) addMissingLabels(ctx context.Context, id string, desired, existing map[string]bool, actor string, opts LabelOpts) error {
	for l := range desired {
		if !existing[l] {
			if err := u.labelRepo.Insert(ctx, id, l, actor, opts); err != nil {
				return fmt.Errorf("set labels: add %s: %w", l, err)
			}
		}
	}
	return nil
}

// LabelQueries owns read-only label operations.
type LabelQueries struct {
	labelRepo LabelSQLRepository
}

func (u *LabelQueries) GetLabels(ctx context.Context, issueID string) ([]string, error) {
	return u.list(ctx, issueID, false)
}

func (u *LabelQueries) GetWispLabels(ctx context.Context, wispID string) ([]string, error) {
	return u.list(ctx, wispID, true)
}

func (u *LabelQueries) list(ctx context.Context, id string, useWisp bool) ([]string, error) {
	if id == "" {
		return nil, fmt.Errorf("get labels: id must not be empty")
	}
	out, err := u.labelRepo.List(ctx, id, LabelOpts{UseWispsTable: useWisp})
	if err != nil {
		return nil, fmt.Errorf("get labels %s: %w", id, err)
	}
	return out, nil
}

func (u *LabelQueries) GetLabelsForIssues(ctx context.Context, issueIDs []string) (map[string][]string, error) {
	return u.listBulk(ctx, issueIDs, false)
}

func (u *LabelQueries) GetLabelsForWisps(ctx context.Context, wispIDs []string) (map[string][]string, error) {
	return u.listBulk(ctx, wispIDs, true)
}

func (u *LabelQueries) listBulk(ctx context.Context, ids []string, useWisp bool) (map[string][]string, error) {
	if len(ids) == 0 {
		return map[string][]string{}, nil
	}
	out, err := u.labelRepo.ListByIssueIDs(ctx, ids, LabelOpts{UseWispsTable: useWisp})
	if err != nil {
		return nil, fmt.Errorf("get labels bulk: %w", err)
	}
	return out, nil
}

// LabelInheritance owns copying labels from a parent to a child.
type LabelInheritance struct {
	labelRepo LabelSQLRepository
}

func (u *LabelInheritance) InheritFromParent(ctx context.Context, childID, parentID, actor string, skipExisting []string) ([]string, error) {
	return u.inherit(ctx, childID, parentID, actor, skipExisting, false)
}

func (u *LabelInheritance) InheritFromWispParent(ctx context.Context, childWispID, parentWispID, actor string, skipExisting []string) ([]string, error) {
	return u.inherit(ctx, childWispID, parentWispID, actor, skipExisting, true)
}

func (u *LabelInheritance) inherit(ctx context.Context, childID, parentID, actor string, skipExisting []string, useWisp bool) ([]string, error) {
	if childID == "" {
		return nil, fmt.Errorf("inherit labels: childID must not be empty")
	}
	if parentID == "" {
		return nil, fmt.Errorf("inherit labels: parentID must not be empty")
	}
	parentLabels, err := u.labelRepo.List(ctx, parentID, LabelOpts{UseWispsTable: useWisp})
	if err != nil {
		return nil, fmt.Errorf("inherit labels: list parent %s: %w", parentID, err)
	}
	if len(parentLabels) == 0 {
		return nil, nil
	}
	skip := make(map[string]bool, len(skipExisting))
	for _, s := range skipExisting {
		skip[s] = true
	}
	var inherited []string
	for _, label := range parentLabels {
		if skip[label] {
			continue
		}
		if err := u.labelRepo.Insert(ctx, childID, label, actor, LabelOpts{UseWispsTable: useWisp}); err != nil {
			return inherited, fmt.Errorf("inherit labels: insert %s on %s: %w", label, childID, err)
		}
		inherited = append(inherited, label)
	}
	return inherited, nil
}
