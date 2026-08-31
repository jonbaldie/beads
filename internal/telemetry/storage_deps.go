package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// ── Dependencies ────────────────────────────────────────────────────────────

func (s *InstrumentedStorage) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	attrs := []attribute.KeyValue{
		attribute.String("bd.dep.from", dep.IssueID),
		attribute.String("bd.dep.to", dep.DependsOnID),
		attribute.String("bd.dep.type", string(dep.Type)),
	}
	ctx, span, t := s.op(ctx, "AddDependency", attrs...)
	err := s.inner.AddDependency(ctx, dep, actor)
	s.done(ctx, span, t, err, attrs...)
	return err
}

func (s *InstrumentedStorage) AddDependencyWithOptions(ctx context.Context, dep *types.Dependency, actor string, opts storage.DependencyAddOptions) error {
	attrs := []attribute.KeyValue{
		attribute.String("bd.dep.from", dep.IssueID),
		attribute.String("bd.dep.to", dep.DependsOnID),
		attribute.String("bd.dep.type", string(dep.Type)),
	}
	ctx, span, t := s.op(ctx, "AddDependency", attrs...)
	err := s.inner.AddDependencyWithOptions(ctx, dep, actor, opts)
	s.done(ctx, span, t, err, attrs...)
	return err
}

func (s *InstrumentedStorage) RemoveDependency(ctx context.Context, issueID, dependsOnID string, actor string) error {
	attrs := []attribute.KeyValue{
		attribute.String("bd.dep.from", issueID),
		attribute.String("bd.dep.to", dependsOnID),
	}
	ctx, span, t := s.op(ctx, "RemoveDependency", attrs...)
	err := s.inner.RemoveDependency(ctx, issueID, dependsOnID, actor)
	s.done(ctx, span, t, err, attrs...)
	return err
}

func (s *InstrumentedStorage) RemoveDependencyWithOptions(ctx context.Context, issueID, dependsOnID string, actor string, opts storage.DependencyRemoveOptions) error {
	attrs := []attribute.KeyValue{
		attribute.String("bd.dep.from", issueID),
		attribute.String("bd.dep.to", dependsOnID),
	}
	ctx, span, t := s.op(ctx, "RemoveDependency", attrs...)
	err := s.inner.RemoveDependencyWithOptions(ctx, issueID, dependsOnID, actor, opts)
	s.done(ctx, span, t, err, attrs...)
	return err
}

func (s *InstrumentedStorage) GetDependencies(ctx context.Context, issueID string) ([]*types.Issue, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.issue.id", issueID)}
	ctx, span, t := s.op(ctx, "GetDependencies", attrs...)
	v, err := s.inner.GetDependencies(ctx, issueID)
	s.done(ctx, span, t, err, attrs...)
	return v, err
}

func (s *InstrumentedStorage) GetDependents(ctx context.Context, issueID string) ([]*types.Issue, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.issue.id", issueID)}
	ctx, span, t := s.op(ctx, "GetDependents", attrs...)
	v, err := s.inner.GetDependents(ctx, issueID)
	s.done(ctx, span, t, err, attrs...)
	return v, err
}

func (s *InstrumentedStorage) GetDependenciesWithMetadata(ctx context.Context, issueID string) ([]*types.IssueWithDependencyMetadata, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.issue.id", issueID)}
	ctx, span, t := s.op(ctx, "GetDependenciesWithMetadata", attrs...)
	v, err := s.inner.GetDependenciesWithMetadata(ctx, issueID)
	s.done(ctx, span, t, err, attrs...)
	return v, err
}

func (s *InstrumentedStorage) GetDependentsWithMetadata(ctx context.Context, issueID string) ([]*types.IssueWithDependencyMetadata, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.issue.id", issueID)}
	ctx, span, t := s.op(ctx, "GetDependentsWithMetadata", attrs...)
	v, err := s.inner.GetDependentsWithMetadata(ctx, issueID)
	s.done(ctx, span, t, err, attrs...)
	return v, err
}

func (s *InstrumentedStorage) GetDependencyTree(ctx context.Context, issueID string, maxDepth int, showAllPaths bool, reverse bool) ([]*types.TreeNode, error) {
	attrs := []attribute.KeyValue{
		attribute.String("bd.issue.id", issueID),
		attribute.Int("bd.max_depth", maxDepth),
	}
	ctx, span, t := s.op(ctx, "GetDependencyTree", attrs...)
	v, err := s.inner.GetDependencyTree(ctx, issueID, maxDepth, showAllPaths, reverse)
	s.done(ctx, span, t, err, attrs...)
	return v, err
}

// ── Labels ──────────────────────────────────────────────────────────────────

func (s *InstrumentedStorage) AddLabel(ctx context.Context, issueID, label, actor string) error {
	attrs := []attribute.KeyValue{
		attribute.String("bd.issue.id", issueID),
		attribute.String("bd.label", label),
	}
	ctx, span, t := s.op(ctx, "AddLabel", attrs...)
	err := s.inner.AddLabel(ctx, issueID, label, actor)
	s.done(ctx, span, t, err, attrs...)
	return err
}

func (s *InstrumentedStorage) RemoveLabel(ctx context.Context, issueID, label, actor string) error {
	attrs := []attribute.KeyValue{
		attribute.String("bd.issue.id", issueID),
		attribute.String("bd.label", label),
	}
	ctx, span, t := s.op(ctx, "RemoveLabel", attrs...)
	err := s.inner.RemoveLabel(ctx, issueID, label, actor)
	s.done(ctx, span, t, err, attrs...)
	return err
}

func (s *InstrumentedStorage) GetLabels(ctx context.Context, issueID string) ([]string, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.issue.id", issueID)}
	ctx, span, t := s.op(ctx, "GetLabels", attrs...)
	v, err := s.inner.GetLabels(ctx, issueID)
	s.done(ctx, span, t, err, attrs...)
	return v, err
}

func (s *InstrumentedStorage) GetIssuesByLabel(ctx context.Context, label string) ([]*types.Issue, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.label", label)}
	ctx, span, t := s.op(ctx, "GetIssuesByLabel", attrs...)
	v, err := s.inner.GetIssuesByLabel(ctx, label)
	s.done(ctx, span, t, err, attrs...)
	return v, err
}

// ── Work queries ─────────────────────────────────────────────────────────────

func (s *InstrumentedStorage) GetReadyWork(ctx context.Context, filter types.WorkFilter) ([]*types.Issue, error) {
	ctx, span, t := s.op(ctx, "GetReadyWork")
	v, err := s.inner.GetReadyWork(ctx, filter)
	if err == nil {
		span.SetAttributes(attribute.Int("bd.result.count", len(v)))
	}
	s.done(ctx, span, t, err)
	return v, err
}

func (s *InstrumentedStorage) GetReadyWorkWithCounts(ctx context.Context, filter types.WorkFilter) ([]*types.IssueWithCounts, error) {
	ctx, span, t := s.op(ctx, "GetReadyWorkWithCounts")
	v, err := s.inner.GetReadyWorkWithCounts(ctx, filter)
	if err == nil {
		span.SetAttributes(attribute.Int("bd.result.count", len(v)))
	}
	s.done(ctx, span, t, err)
	return v, err
}

func (s *InstrumentedStorage) GetBlockedIssues(ctx context.Context, filter types.WorkFilter) ([]*types.BlockedIssue, error) {
	ctx, span, t := s.op(ctx, "GetBlockedIssues")
	v, err := s.inner.GetBlockedIssues(ctx, filter)
	if err == nil {
		span.SetAttributes(attribute.Int("bd.result.count", len(v)))
	}
	s.done(ctx, span, t, err)
	return v, err
}

func (s *InstrumentedStorage) GetEpicsEligibleForClosure(ctx context.Context) ([]*types.EpicStatus, error) {
	ctx, span, t := s.op(ctx, "GetEpicsEligibleForClosure")
	v, err := s.inner.GetEpicsEligibleForClosure(ctx)
	s.done(ctx, span, t, err)
	return v, err
}

// ── Comments & events ────────────────────────────────────────────────────────

func (s *InstrumentedStorage) AddIssueComment(ctx context.Context, issueID, author, text string) (*types.Comment, error) {
	attrs := []attribute.KeyValue{
		attribute.String("bd.issue.id", issueID),
		attribute.String("bd.actor", author),
	}
	ctx, span, t := s.op(ctx, "AddIssueComment", attrs...)
	v, err := s.inner.AddIssueComment(ctx, issueID, author, text)
	s.done(ctx, span, t, err, attrs...)
	return v, err
}

func (s *InstrumentedStorage) GetIssueComments(ctx context.Context, issueID string) ([]*types.Comment, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.issue.id", issueID)}
	ctx, span, t := s.op(ctx, "GetIssueComments", attrs...)
	v, err := s.inner.GetIssueComments(ctx, issueID)
	s.done(ctx, span, t, err, attrs...)
	return v, err
}

func (s *InstrumentedStorage) GetIssueCommentsPage(ctx context.Context, issueID string, after storage.CommentPageCursor, limit int) ([]*types.Comment, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.issue.id", issueID)}
	ctx, span, t := s.op(ctx, "GetIssueCommentsPage", attrs...)
	v, err := s.inner.GetIssueCommentsPage(ctx, issueID, after, limit)
	s.done(ctx, span, t, err, attrs...)
	return v, err
}

func (s *InstrumentedStorage) GetEvents(ctx context.Context, issueID string, limit int) ([]*types.Event, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.issue.id", issueID)}
	ctx, span, t := s.op(ctx, "GetEvents", attrs...)
	v, err := s.inner.GetEvents(ctx, issueID, limit)
	s.done(ctx, span, t, err, attrs...)
	return v, err
}

func (s *InstrumentedStorage) GetAllEventsSince(ctx context.Context, since time.Time) ([]*types.Event, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.since", since.Format(time.RFC3339))}
	ctx, span, t := s.op(ctx, "GetAllEventsSince", attrs...)
	v, err := s.inner.GetAllEventsSince(ctx, since)
	s.done(ctx, span, t, err, attrs...)
	return v, err
}

func (s *InstrumentedStorage) RecordProvenanceEvent(ctx context.Context, ev types.ProvenanceEvent) (string, bool, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.issue.id", ev.IssueID), attribute.String("bd.prov.kind", string(ev.Kind))}
	ctx, span, t := s.op(ctx, "RecordProvenanceEvent", attrs...)
	id, inserted, err := s.inner.RecordProvenanceEvent(ctx, ev)
	s.done(ctx, span, t, err, attrs...)
	return id, inserted, err
}

func (s *InstrumentedStorage) GetProvenanceEvents(ctx context.Context, issueID, kindFilter string) ([]types.ProvenanceEvent, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.issue.id", issueID)}
	ctx, span, t := s.op(ctx, "GetProvenanceEvents", attrs...)
	v, err := s.inner.GetProvenanceEvents(ctx, issueID, kindFilter)
	s.done(ctx, span, t, err, attrs...)
	return v, err
}

func (s *InstrumentedStorage) GetProvenanceByRef(ctx context.Context, ref string) ([]types.ProvenanceEvent, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.prov.ref", ref)}
	ctx, span, t := s.op(ctx, "GetProvenanceByRef", attrs...)
	v, err := s.inner.GetProvenanceByRef(ctx, ref)
	s.done(ctx, span, t, err, attrs...)
	return v, err
}

// ── Statistics ───────────────────────────────────────────────────────────────

func (s *InstrumentedStorage) GetStatistics(ctx context.Context) (*types.Statistics, error) {
	ctx, span, t := s.op(ctx, "GetStatistics")
	v, err := s.inner.GetStatistics(ctx)
	s.done(ctx, span, t, err)
	if err == nil && v != nil {
		// Record current issue counts as gauge snapshots, broken down by status.
		statusAttr := func(status string) metric.MeasurementOption {
			return metric.WithAttributes(attribute.String("status", status))
		}
		s.issueGauge.Record(ctx, int64(v.OpenIssues), statusAttr("open"))
		s.issueGauge.Record(ctx, int64(v.InProgressIssues), statusAttr("in_progress"))
		s.issueGauge.Record(ctx, int64(v.ClosedIssues), statusAttr("closed"))
		s.issueGauge.Record(ctx, int64(v.DeferredIssues), statusAttr("deferred"))
	}
	return v, err
}
