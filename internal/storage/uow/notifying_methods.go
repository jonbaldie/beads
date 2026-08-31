package uow

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	storageissueops "github.com/jonbaldie/beads/internal/storage/issueops"
	publicops "github.com/jonbaldie/beads/issueops"
	"github.com/jonbaldie/beads/memoryops"
)

func (p *notifyingProvider) NewUOW(ctx context.Context) (UnitOfWork, error) {
	uw, err := p.inner.NewUOW(ctx)
	if err != nil {
		return nil, err
	}
	return &notifyingUOW{UnitOfWork: uw, rec: &recorder{}, sinks: p.sinks}, nil
}
func (p *notifyingProvider) Close(ctx context.Context) error {
	return p.inner.Close(ctx)
}
func (p *notifyingProvider) Unwrap() UnitOfWorkProvider { return p.inner }
func (p *notifyingProvider) IssueLifecycle() (publicops.Lifecycle, error) {
	return NewIssueOperations(p)
}
func (p *notifyingProvider) IssueReader() (publicops.Reader, error)   { return NewIssueReader(p) }
func (p *notifyingProvider) IssueClaimer() (publicops.Claimer, error) { return NewIssueClaimer(p) }
func (p *notifyingProvider) IssueRelations() (publicops.Relations, error) {
	return NewIssueRelations(p)
}
func (p *notifyingProvider) EdgeReader() (publicops.EdgeReader, error) { return NewEdgeReader(p) }
func (p *notifyingProvider) BlockingAnnotator() (publicops.BlockingAnnotator, error) {
	return NewBlockingAnnotator(p)
}
func (p *notifyingProvider) TreeWalker() (publicops.TreeWalker, error) { return NewTreeWalker(p) }
func (p *notifyingProvider) GraphCounter() (publicops.GraphCounter, error) {
	return NewGraphCounter(p)
}
func (p *notifyingProvider) Counter() (publicops.Counter, error) { return NewCounter(p) }
func (p *notifyingProvider) ReadyCounter() (publicops.ReadyCounter, error) {
	return NewReadyCounter(p)
}
func (p *notifyingProvider) ReadyClaimer() (publicops.ReadyClaimer, error) {
	return NewReadyClaimer(p)
}
func (p *notifyingProvider) Querier() (publicops.Querier, error) { return NewQuerier(p) }
func (p *notifyingProvider) StatsReporter() (publicops.StatsReporter, error) {
	return NewStatsReporter(p)
}
func (p *notifyingProvider) CycleDetector() (publicops.CycleDetector, error) {
	return NewCycleDetector(p)
}
func (p *notifyingProvider) Commenter() (publicops.Commenter, error)     { return NewCommenter(p) }
func (p *notifyingProvider) BatchCloser() (publicops.BatchCloser, error) { return NewBatchCloser(p) }
func (p *notifyingProvider) BatchCreator() (publicops.BatchCreator, error) {
	return NewBatchCreator(p)
}
func (p *notifyingProvider) DependencyEditor() (publicops.DependencyEditor, error) {
	return NewDependencyEditor(p)
}
func (p *notifyingProvider) BatchApplier() (publicops.BatchApplier, error) {
	return NewBatchApplier(p)
}
func (p *notifyingProvider) Deleter() (publicops.Deleter, error)   { return NewDeleter(p) }
func (p *notifyingProvider) Sweeper() (publicops.Sweeper, error)   { return NewSweeper(p) }
func (p *notifyingProvider) Importer() (publicops.Importer, error) { return NewImporter(p) }
func (p *notifyingProvider) Bootstrapper() (publicops.Bootstrapper, error) {
	return NewBootstrapper(p)
}
func (p *notifyingProvider) InitVerifier() (publicops.InitVerifier, error) {
	return NewInitVerifier(p)
}
func (p *notifyingProvider) WorkspaceConfig() (publicops.WorkspaceConfig, error) {
	return NewWorkspaceConfig(p)
}
func (p *notifyingProvider) VersionReconciler() (publicops.VersionReconciler, error) {
	return NewVersionReconciler(p)
}
func (p *notifyingProvider) MetadataCAS() (publicops.MetadataCAS, error) { return NewMetadataCAS(p) }
func (p *notifyingProvider) Releaser() (publicops.Releaser, error)       { return NewReleaser(p) }
func (p *notifyingProvider) Memories() (memoryops.Memories, error)       { return NewMemories(p) }
func (p *notifyingProvider) EventsJournalCursor() (storage.EventsJournalCursor, error) {
	return NewEventsJournalCursor(p)
}
func (p *notifyingProvider) RunNonTx(ctx context.Context, fn func(ctx context.Context, conn *sql.Conn) error) error {
	mp, ok := p.inner.(MaintenanceProvider)
	if !ok {
		return fmt.Errorf("uow: provider %T does not support non-transactional maintenance", p.inner)
	}
	return mp.RunNonTx(ctx, fn)
}
func (p *notifyingProvider) SetPoolLimits(limits PoolLimits) {
	if tuner, ok := p.inner.(PoolTuner); ok {
		tuner.SetPoolLimits(limits)
	}
}
func (p *notifyingProvider) SetEventsJournalEnabled(enabled bool) {
	if configurer, ok := p.inner.(storage.EventsJournalConfigurer); ok {
		configurer.SetEventsJournalEnabled(enabled)
	}
}
func (p *notifyingProvider) RunEventsMaintenanceTx(ctx context.Context, fn func(context.Context, storageissueops.DBTX) error) error {
	runner, ok := p.inner.(eventsMaintenanceRunner)
	if !ok {
		return fmt.Errorf("uow: provider %T does not support events-journal maintenance", p.inner)
	}
	return runner.RunEventsMaintenanceTx(ctx, fn)
}
