package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
)

type molReader interface {
	GetIssue(ctx context.Context, id string) (*types.Issue, error)
	GetIssuesByIDs(ctx context.Context, ids []string) ([]*types.Issue, error)
	GetIssuesByLabel(ctx context.Context, label string) ([]*types.Issue, error)
	GetDependents(ctx context.Context, issueID string) ([]*types.Issue, error)
	GetDependentsWithMetadata(ctx context.Context, issueID string) ([]*types.IssueWithDependencyMetadata, error)
	GetDependencyRecords(ctx context.Context, issueID string) ([]*types.Dependency, error)
	GetDependencyRecordsForIssues(ctx context.Context, issueIDs []string) (map[string][]*types.Dependency, error)
	SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error)
	SearchIssueIDs(ctx context.Context, query string, filter types.IssueFilter) ([]string, error)
	GetReadyWork(ctx context.Context, filter types.WorkFilter) ([]*types.Issue, error)
	GetBlockedIssues(ctx context.Context, filter types.WorkFilter) ([]*types.BlockedIssue, error)
	GetEpicsEligibleForClosure(ctx context.Context) ([]*types.EpicStatus, error)
	GetLabels(ctx context.Context, issueID string) ([]string, error)
	GetConfig(ctx context.Context, key string) (string, error)
	IsInfraTypeCtx(ctx context.Context, t types.IssueType) bool
	GetCustomStatusesDetailed(ctx context.Context) ([]types.CustomStatus, error)
	GetMoleculeProgress(ctx context.Context, moleculeID string) (*types.MoleculeProgressStats, error)
	GetMoleculeLastActivity(ctx context.Context, moleculeID string) (*types.MoleculeLastActivity, error)
	FindWispDependentsRecursive(ctx context.Context, ids []string) (map[string]bool, error)
}

var _ molReader = storage.DoltStorage(nil)

type molWriter interface {
	molReader
	// CreateIssue carries the issue's Labels. There is deliberately no AddLabel
	// verb beside it: a label written separately makes the plane decision a
	// second time, and the two implementations of this interface did not make
	// it the same way — one asked the storage layer's isActiveWisp, the other a
	// cache filled from a wisp probe. A caller that wants an issue labeled
	// says so on the issue it creates.
	CreateIssue(ctx context.Context, issue *types.Issue, actor string) error
	AddDependency(ctx context.Context, dep *types.Dependency, actor string) error
	UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error
	CloseIssue(ctx context.Context, id, reason, actor string) error
	DeleteIssue(ctx context.Context, id, actor string) error
	SetConfig(ctx context.Context, key, value string) error
	ClaimStepIfOpen(ctx context.Context, id, actor string) error
}

type storeMolWriter struct {
	storage.DoltStorage
	tx storage.Transaction
}

func (w storeMolWriter) CreateIssue(ctx context.Context, issue *types.Issue, actor string) error {
	return w.tx.CreateIssue(ctx, issue, actor)
}

func (w storeMolWriter) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	return w.tx.AddDependency(ctx, dep, actor)
}

func (w storeMolWriter) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	return w.tx.UpdateIssue(ctx, id, updates, actor)
}

func (w storeMolWriter) CloseIssue(ctx context.Context, id, reason, actor string) error {
	return w.tx.CloseIssue(ctx, id, reason, actor, "")
}

func (w storeMolWriter) DeleteIssue(ctx context.Context, id, _ string) error {
	return w.tx.DeleteIssue(ctx, id)
}

func (w storeMolWriter) SetConfig(ctx context.Context, key, value string) error {
	return w.tx.SetConfig(ctx, key, value)
}

// GetConfig reads through the transaction when one is bound. Without this,
// config readers (flattenUnregisteredIssueTypes) would go to the embedded
// store, which opens a second pool connection — a deadlock when
// MaxOpenConns=1 and the transaction already holds the only one. It also
// keeps the read consistent with any config written earlier in the same
// transaction rather than seeing the last committed value.
func (w storeMolWriter) GetConfig(ctx context.Context, key string) (string, error) {
	if w.tx != nil {
		return w.tx.GetConfig(ctx, key)
	}
	return w.DoltStorage.GetConfig(ctx, key)
}

func (w storeMolWriter) ClaimStepIfOpen(ctx context.Context, id, actor string) error {
	return w.DoltStorage.RunInTransaction(ctx, fmt.Sprintf("bd: advance to step %s", id), func(tx storage.Transaction) error {
		current, err := tx.GetIssue(ctx, id)
		if err != nil {
			return err
		}
		if current == nil {
			return fmt.Errorf("step %s not found", id)
		}
		if current.Status != types.StatusOpen {
			return fmt.Errorf("step %s already claimed (status: %s)", id, current.Status)
		}
		return tx.UpdateIssue(ctx, id, map[string]interface{}{"status": types.StatusInProgress}, actor)
	})
}

func newStandaloneStoreMolWriter(store storage.DoltStorage) storeMolWriter {
	return storeMolWriter{DoltStorage: store}
}

type uowMolReader struct {
	uw uow.UnitOfWork
}

type uowMolWriter struct {
	uowMolReader
	wispIDs    map[string]bool
	notWispIDs map[string]bool
}

func newUOWMolWriter(uw uow.UnitOfWork) *uowMolWriter {
	return &uowMolWriter{
		uowMolReader: uowMolReader{uw: uw},
		wispIDs:      make(map[string]bool),
		notWispIDs:   make(map[string]bool),
	}
}

func (w *uowMolWriter) isWisp(ctx context.Context, id string) (bool, error) {
	if w.wispIDs[id] {
		return true, nil
	}
	if w.notWispIDs[id] {
		return false, nil
	}
	_, err := w.uw.IssueUseCase().GetWisp(ctx, id)
	if err == nil {
		w.wispIDs[id] = true
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		w.notWispIDs[id] = true
		return false, nil
	}
	return false, fmt.Errorf("determining wisp status for %s: %w", id, err)
}

func (w *uowMolWriter) CreateIssue(ctx context.Context, issue *types.Issue, actor string) error {
	params := domain.CreateIssueParams{Issue: issue, ExplicitID: issue.ID, Labels: issue.Labels}
	var err error
	if issue.Ephemeral || issue.NoHistory {
		_, err = w.uw.IssueUseCase().CreateWisp(ctx, params, actor)
		if err == nil {
			w.wispIDs[issue.ID] = true
		}
	} else {
		_, err = w.uw.IssueUseCase().CreateIssue(ctx, params, actor)
		if err == nil {
			w.notWispIDs[issue.ID] = true
		}
	}
	return err
}

func (w *uowMolWriter) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	isWisp, err := w.isWisp(ctx, dep.IssueID)
	if err != nil {
		return err
	}
	if isWisp {
		return w.uw.DependencyUseCase().AddWispDependency(ctx, dep, actor)
	}
	return w.uw.DependencyUseCase().AddDependency(ctx, dep, actor)
}

func (w *uowMolWriter) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	isWisp, err := w.isWisp(ctx, id)
	if err != nil {
		return err
	}
	if isWisp {
		return w.uw.IssueUseCase().UpdateWisp(ctx, id, updates, actor)
	}
	return w.uw.IssueUseCase().UpdateIssue(ctx, id, updates, actor)
}

func (w *uowMolWriter) CloseIssue(ctx context.Context, id, reason, actor string) error {
	params := domain.CloseIssueParams{Reason: reason}
	isWisp, err := w.isWisp(ctx, id)
	if err != nil {
		return err
	}
	if isWisp {
		_, err = w.uw.IssueUseCase().CloseWisp(ctx, id, params, actor)
	} else {
		_, err = w.uw.IssueUseCase().CloseIssue(ctx, id, params, actor)
	}
	return err
}

func (w *uowMolWriter) DeleteIssue(ctx context.Context, id, actor string) error {
	_, err := w.uw.IssueUseCase().DeleteIssues(ctx, domain.DeleteIssuesParams{
		IDs:                  []string{id},
		UpdateTextReferences: true,
	}, actor)
	return err
}

func (w *uowMolWriter) SetConfig(ctx context.Context, key, value string) error {
	return w.uw.ConfigUseCase().SetConfig(ctx, key, value)
}

func (w *uowMolWriter) ClaimStepIfOpen(ctx context.Context, id, actor string) error {
	isWisp, err := w.isWisp(ctx, id)
	if err != nil {
		return err
	}
	if isWisp {
		_, err := w.uw.IssueUseCase().ClaimWispIfOpen(ctx, id, actor)
		return err
	}
	_, err = w.uw.IssueUseCase().ClaimIssueIfOpen(ctx, id, actor)
	return err
}
