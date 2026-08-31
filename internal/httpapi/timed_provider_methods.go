package httpapi

import (
	"context"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/issueops"
	"github.com/jonbaldie/beads/memoryops"
)

func (p timedProvider) IssueReader() (issueops.Reader, error) {
	return uow.NewIssueReader(p)
}
func (p timedProvider) IssueClaimer() (issueops.Claimer, error) {
	return uow.NewIssueClaimer(p)
}
func (p timedProvider) BatchCloser() (issueops.BatchCloser, error) {
	return uow.NewBatchCloser(p)
}
func (p timedProvider) ReadyClaimer() (issueops.ReadyClaimer, error) {
	return uow.NewReadyClaimer(p)
}
func (p timedProvider) Releaser() (issueops.Releaser, error) {
	return uow.NewReleaser(p)
}
func (p timedProvider) IssueLifecycle() (issueops.Lifecycle, error) {
	return uow.NewIssueOperations(p)
}
func (p timedProvider) WorkspaceConfig() (issueops.WorkspaceConfig, error) {
	return uow.NewWorkspaceConfig(p)
}
func (p timedProvider) StatsReporter() (issueops.StatsReporter, error) {
	return uow.NewStatsReporter(p)
}
func (p timedProvider) CycleDetector() (issueops.CycleDetector, error) {
	return uow.NewCycleDetector(p)
}
func (p timedProvider) EdgeReader() (issueops.EdgeReader, error) {
	return uow.NewEdgeReader(p)
}
func (p timedProvider) GraphCounter() (issueops.GraphCounter, error) {
	return uow.NewGraphCounter(p)
}
func (p timedProvider) IssueRelations() (issueops.Relations, error) {
	return uow.NewIssueRelations(p)
}
func (p timedProvider) Commenter() (issueops.Commenter, error) {
	return uow.NewCommenter(p)
}
func (p timedProvider) BlockingAnnotator() (issueops.BlockingAnnotator, error) {
	return uow.NewBlockingAnnotator(p)
}
func (p timedProvider) TreeWalker() (issueops.TreeWalker, error) {
	return uow.NewTreeWalker(p)
}
func (p timedProvider) ReadyCounter() (issueops.ReadyCounter, error) {
	return uow.NewReadyCounter(p)
}
func (p timedProvider) Counter() (issueops.Counter, error) {
	return uow.NewCounter(p)
}
func (p timedProvider) Querier() (issueops.Querier, error) {
	return uow.NewQuerier(p)
}
func (p timedProvider) Sweeper() (issueops.Sweeper, error) {
	return uow.NewSweeper(p)
}
func (p timedProvider) Deleter() (issueops.Deleter, error) {
	return uow.NewDeleter(p)
}
func (p timedProvider) BatchCreator() (issueops.BatchCreator, error) {
	return uow.NewBatchCreator(p)
}
func (p timedProvider) DependencyEditor() (issueops.DependencyEditor, error) {
	return uow.NewDependencyEditor(p)
}
func (p timedProvider) MetadataCAS() (issueops.MetadataCAS, error) {
	return uow.NewMetadataCAS(p)
}
func (p timedProvider) BatchApplier() (issueops.BatchApplier, error) {
	return uow.NewBatchApplier(p)
}
func (p timedProvider) Memories() (memoryops.Memories, error) {
	return uow.NewMemories(p)
}
func (p timedProvider) EventsJournalCursor() (storage.EventsJournalCursor, error) {
	return uow.NewEventsJournalCursor(p)
}
func (p timedProvider) Close(context.Context) error { return nil }
