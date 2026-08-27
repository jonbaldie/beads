package uow

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	storageissueops "github.com/jonbaldie/beads/internal/storage/issueops"
	publicops "github.com/jonbaldie/beads/issueops"
)

// BatchApplierSource is the capability accessor a unit-of-work provider offers
// for the apply-many role, the sibling of BatchCreatorSource and
// DependencyEditorSource.
type BatchApplierSource interface {
	BatchApplier() (publicops.BatchApplier, error)
}

// batchApplier applies a heterogeneous batch through one unit of work.
type batchApplier struct {
	provider UnitOfWorkProvider
}

// BatchApplier returns the guarded apply-many surface for this provider.
func (p *doltSQLProvider) BatchApplier() (publicops.BatchApplier, error) {
	return NewBatchApplier(p)
}

// NewBatchApplier constructs a public batch applier backed by provider.
func NewBatchApplier(provider UnitOfWorkProvider) (publicops.BatchApplier, error) {
	if isNilUnitOfWorkProvider(provider) {
		return nil, fmt.Errorf("new batch applier: unit-of-work provider must not be nil")
	}
	return &batchApplier{provider: provider}, nil
}

var _ publicops.BatchApplier = (*batchApplier)(nil)

// ApplyBatch applies every item in ONE unit of work and commits them together.
//
// The body is ApplyBatchInTx on the unit of work's DBTX runner — the same
// body the Dolt-backed stores wrap. A batch that changed nothing composes
// no commit message, which is RunTxResult's existing signal for a unit of
// work with nothing to version. The message is composed from what landed
// on either plane, so a wisp-only batch still commits.
func (o *batchApplier) ApplyBatch(ctx context.Context, request publicops.ApplyBatchRequest) (publicops.ApplyBatchResult, error) {
	plan, err := storage.PlanApplyBatch(request)
	if err != nil {
		return publicops.ApplyBatchResult{}, err
	}
	return RunTxResult(ctx, o.provider, func(ctx context.Context, uw UnitOfWork) (publicops.ApplyBatchResult, string, error) {
		tx, err := lifecycleStatementRunner(uw)
		if err != nil {
			return publicops.ApplyBatchResult{}, "", err
		}
		result, write, err := storageissueops.ApplyBatchInTx(ctx, tx, plan)
		if err != nil {
			return publicops.ApplyBatchResult{}, "", err
		}
		announceBatchApply(ctx, uw, result)
		return result, storageissueops.ApplyBatchCommitMessage(plan, result, write), nil
	})
}

// announceBatchApply records completion hooks for items that landed, matching
// hookBatchApplier: fire on Changed, create always lands, an idempotent
// re-close is silent, and an edge fires once per distinct source that this
// request did not itself create.
func announceBatchApply(ctx context.Context, uw UnitOfWork, result publicops.ApplyBatchResult) {
	n := notifyLifecycle(uw)
	if n == nil {
		return
	}
	created := make(map[string]struct{}, len(result.Items))
	for _, item := range result.Items {
		if !item.Changed {
			continue
		}
		switch item.Kind {
		case publicops.ItemCreate:
			created[item.IssueID] = struct{}{}
			n.recordCreate(ctx, item.IssueID, publicops.CreateRequest{})
		case publicops.ItemUpdate:
			n.recordUpdateLanded(ctx, item.IssueID)
		case publicops.ItemClose:
			n.recordClose(ctx, item.IssueID)
		}
	}
	fired := make(map[string]struct{})
	for _, item := range result.Items {
		if item.Kind != publicops.ItemDepAdd || !item.Changed {
			continue
		}
		if _, ok := created[item.IssueID]; ok {
			continue
		}
		if _, ok := fired[item.IssueID]; ok {
			continue
		}
		fired[item.IssueID] = struct{}{}
		n.recordDepAdd(ctx, item.IssueID)
	}
}
