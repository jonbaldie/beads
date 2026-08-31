package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

func (s *InstrumentedStorage) Unwrap() storage.DoltStorage { return s.inner }
func (s *InstrumentedStorage) op(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span, time.Time) {
	all := append([]attribute.KeyValue{attribute.String("db.operation", name)}, attrs...)
	ctx, span := s.tracer.Start(ctx, "storage."+name,
		trace.WithAttributes(all...),
		trace.WithSpanKind(trace.SpanKindClient),
	)
	s.ops.Add(ctx, 1, metric.WithAttributes(all...))
	return ctx, span, time.Now()
}
func (s *InstrumentedStorage) done(ctx context.Context, span trace.Span, start time.Time, err error, attrs ...attribute.KeyValue) {
	ms := float64(time.Since(start).Milliseconds())
	s.dur.Record(ctx, ms, metric.WithAttributes(attrs...))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		s.errs.Add(ctx, 1, metric.WithAttributes(attrs...))
	}
	span.End()
}
func (s *InstrumentedStorage) CreateIssue(ctx context.Context, issue *types.Issue, actor string) error {
	attrs := []attribute.KeyValue{
		attribute.String("bd.actor", actor),
		attribute.String("bd.issue.type", string(issue.IssueType)),
	}
	ctx, span, t := s.op(ctx, "CreateIssue", attrs...)
	err := s.inner.CreateIssue(ctx, issue, actor)
	s.done(ctx, span, t, err, attrs...)
	return err
}
func (s *InstrumentedStorage) CreateIssues(ctx context.Context, issues []*types.Issue, actor string) error {
	attrs := []attribute.KeyValue{
		attribute.String("bd.actor", actor),
		attribute.Int("bd.issue.count", len(issues)),
	}
	ctx, span, t := s.op(ctx, "CreateIssues", attrs...)
	err := s.inner.CreateIssues(ctx, issues, actor)
	s.done(ctx, span, t, err, attrs...)
	return err
}
func (s *InstrumentedStorage) GetIssue(ctx context.Context, id string) (*types.Issue, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.issue.id", id)}
	ctx, span, t := s.op(ctx, "GetIssue", attrs...)
	v, err := s.inner.GetIssue(ctx, id)
	s.done(ctx, span, t, err, attrs...)
	return v, err
}
func (s *InstrumentedStorage) GetIssueByExternalRef(ctx context.Context, externalRef string) (*types.Issue, error) {
	ctx, span, t := s.op(ctx, "GetIssueByExternalRef")
	v, err := s.inner.GetIssueByExternalRef(ctx, externalRef)
	s.done(ctx, span, t, err)
	return v, err
}
func (s *InstrumentedStorage) GetIssuesByIDs(ctx context.Context, ids []string) ([]*types.Issue, error) {
	attrs := []attribute.KeyValue{attribute.Int("bd.issue.count", len(ids))}
	ctx, span, t := s.op(ctx, "GetIssuesByIDs", attrs...)
	v, err := s.inner.GetIssuesByIDs(ctx, ids)
	s.done(ctx, span, t, err, attrs...)
	return v, err
}
func (s *InstrumentedStorage) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	attrs := []attribute.KeyValue{
		attribute.String("bd.issue.id", id),
		attribute.String("bd.actor", actor),
		attribute.Int("bd.update.count", len(updates)),
	}
	ctx, span, t := s.op(ctx, "UpdateIssue", attrs...)
	err := s.inner.UpdateIssue(ctx, id, updates, actor)
	s.done(ctx, span, t, err, attrs...)
	return err
}
func (s *InstrumentedStorage) UpdateIssueChecked(ctx context.Context, id string, updates map[string]interface{}, actor string, opts storage.UpdateIssueOptions) error {
	attrs := []attribute.KeyValue{
		attribute.String("bd.issue.id", id),
		attribute.String("bd.actor", actor),
		attribute.Int("bd.update.count", len(updates)),
	}
	ctx, span, t := s.op(ctx, "UpdateIssueChecked", attrs...)
	err := s.inner.UpdateIssueChecked(ctx, id, updates, actor, opts)
	s.done(ctx, span, t, err, attrs...)
	return err
}
func (s *InstrumentedStorage) ReopenIssue(ctx context.Context, id string, reason string, actor string) error {
	attrs := []attribute.KeyValue{
		attribute.String("bd.issue.id", id),
		attribute.String("bd.actor", actor),
	}
	ctx, span, t := s.op(ctx, "ReopenIssue", attrs...)
	err := s.inner.ReopenIssue(ctx, id, reason, actor)
	s.done(ctx, span, t, err, attrs...)
	return err
}
func (s *InstrumentedStorage) UnclaimIssue(ctx context.Context, id string, actor string, force bool) error {
	attrs := []attribute.KeyValue{
		attribute.String("bd.issue.id", id),
		attribute.String("bd.actor", actor),
		attribute.Bool("bd.force", force),
	}
	ctx, span, t := s.op(ctx, "UnclaimIssue", attrs...)
	err := s.inner.UnclaimIssue(ctx, id, actor, force)
	s.done(ctx, span, t, err, attrs...)
	return err
}
func (s *InstrumentedStorage) UnclaimIssueIfAssignee(ctx context.Context, id string, actor string, expectedAssignee string) error {
	attrs := []attribute.KeyValue{
		attribute.String("bd.issue.id", id),
		attribute.String("bd.actor", actor),
		attribute.String("bd.issue.expected_assignee", expectedAssignee),
	}
	ctx, span, t := s.op(ctx, "UnclaimIssueIfAssignee", attrs...)
	err := s.inner.UnclaimIssueIfAssignee(ctx, id, actor, expectedAssignee)
	s.done(ctx, span, t, err, attrs...)
	return err
}
func (s *InstrumentedStorage) UpdateIssueType(ctx context.Context, id string, issueType string, actor string) error {
	attrs := []attribute.KeyValue{
		attribute.String("bd.issue.id", id),
		attribute.String("bd.issue.type", issueType),
		attribute.String("bd.actor", actor),
	}
	ctx, span, t := s.op(ctx, "UpdateIssueType", attrs...)
	err := s.inner.UpdateIssueType(ctx, id, issueType, actor)
	s.done(ctx, span, t, err, attrs...)
	return err
}
func (s *InstrumentedStorage) CloseIssue(ctx context.Context, id string, reason string, actor string, session string) error {
	attrs := []attribute.KeyValue{
		attribute.String("bd.issue.id", id),
		attribute.String("bd.actor", actor),
	}
	ctx, span, t := s.op(ctx, "CloseIssue", attrs...)
	err := s.inner.CloseIssue(ctx, id, reason, actor, session)
	s.done(ctx, span, t, err, attrs...)
	return err
}
func (s *InstrumentedStorage) CloseIssueChecked(ctx context.Context, id string, actor string, opts storage.CloseIssueOptions) (storage.CloseIssueResult, error) {
	attrs := []attribute.KeyValue{
		attribute.String("bd.issue.id", id),
		attribute.String("bd.actor", actor),
	}
	ctx, span, t := s.op(ctx, "CloseIssueChecked", attrs...)
	res, err := s.inner.CloseIssueChecked(ctx, id, actor, opts)
	s.done(ctx, span, t, err, attrs...)
	return res, err
}
func (s *InstrumentedStorage) DeleteIssue(ctx context.Context, id string) error {
	attrs := []attribute.KeyValue{attribute.String("bd.issue.id", id)}
	ctx, span, t := s.op(ctx, "DeleteIssue", attrs...)
	err := s.inner.DeleteIssue(ctx, id)
	s.done(ctx, span, t, err, attrs...)
	return err
}
func (s *InstrumentedStorage) SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.query", query)}
	ctx, span, t := s.op(ctx, "SearchIssues", attrs...)
	issues, err := s.inner.SearchIssues(ctx, query, filter)
	if err == nil {
		span.SetAttributes(attribute.Int("bd.result.count", len(issues)))
	}
	s.done(ctx, span, t, err, attrs...)
	return issues, err
}
func (s *InstrumentedStorage) SearchIssuesWithCounts(ctx context.Context, query string, filter types.IssueFilter) ([]*types.IssueWithCounts, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.query", query)}
	ctx, span, t := s.op(ctx, "SearchIssuesWithCounts", attrs...)
	v, err := s.inner.SearchIssuesWithCounts(ctx, query, filter)
	if err == nil {
		span.SetAttributes(attribute.Int("bd.result.count", len(v)))
	}
	s.done(ctx, span, t, err, attrs...)
	return v, err
}
func (s *InstrumentedStorage) SearchIssueIDs(ctx context.Context, query string, filter types.IssueFilter) ([]string, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.query", query)}
	ctx, span, t := s.op(ctx, "SearchIssueIDs", attrs...)
	ids, err := s.inner.SearchIssueIDs(ctx, query, filter)
	if err == nil {
		span.SetAttributes(attribute.Int("bd.result.count", len(ids)))
	}
	s.done(ctx, span, t, err, attrs...)
	return ids, err
}
