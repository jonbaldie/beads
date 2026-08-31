package telemetry

import (
	"context"
	"encoding/json"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// ── Configuration ────────────────────────────────────────────────────────────

func (s *InstrumentedStorage) SetConfig(ctx context.Context, key, value string) error {
	attrs := []attribute.KeyValue{attribute.String("bd.config.key", key)}
	ctx, span, t := s.op(ctx, "SetConfig", attrs...)
	err := s.inner.SetConfig(ctx, key, value)
	s.done(ctx, span, t, err, attrs...)
	return err
}

func (s *InstrumentedStorage) GetConfig(ctx context.Context, key string) (string, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.config.key", key)}
	ctx, span, t := s.op(ctx, "GetConfig", attrs...)
	v, err := s.inner.GetConfig(ctx, key)
	s.done(ctx, span, t, err, attrs...)
	return v, err
}

func (s *InstrumentedStorage) GetAllConfig(ctx context.Context) (map[string]string, error) {
	ctx, span, t := s.op(ctx, "GetAllConfig")
	v, err := s.inner.GetAllConfig(ctx)
	s.done(ctx, span, t, err)
	return v, err
}

func (s *InstrumentedStorage) SetLocalMetadata(ctx context.Context, key, value string) error {
	attrs := []attribute.KeyValue{attribute.String("bd.local_metadata.key", key)}
	ctx, span, t := s.op(ctx, "SetLocalMetadata", attrs...)
	err := s.inner.SetLocalMetadata(ctx, key, value)
	s.done(ctx, span, t, err, attrs...)
	return err
}

func (s *InstrumentedStorage) GetLocalMetadata(ctx context.Context, key string) (string, error) {
	attrs := []attribute.KeyValue{attribute.String("bd.local_metadata.key", key)}
	ctx, span, t := s.op(ctx, "GetLocalMetadata", attrs...)
	v, err := s.inner.GetLocalMetadata(ctx, key)
	s.done(ctx, span, t, err, attrs...)
	return v, err
}

// ── Transactions ─────────────────────────────────────────────────────────────

func (s *InstrumentedStorage) RunInTransaction(ctx context.Context, commitMsg string, fn func(tx storage.Transaction) error) error {
	ctx, span, t := s.op(ctx, "RunInTransaction", attribute.String("db.commit_msg", commitMsg))
	err := s.inner.RunInTransaction(ctx, commitMsg, fn)
	s.done(ctx, span, t, err)
	return err
}

// RunInIssueLifecycleTransaction traces the required internal lifecycle lane.
func (s *InstrumentedStorage) RunInIssueLifecycleTransaction(ctx context.Context, commitMsg string, fn func(tx storage.IssueLifecycleTransaction) error) error {
	ctx, span, t := s.op(ctx, "RunInIssueLifecycleTransaction", attribute.String("db.commit_msg", commitMsg))
	err := s.inner.RunInIssueLifecycleTransaction(ctx, commitMsg, fn)
	s.done(ctx, span, t, err)
	return err
}

// ── Wisp queries ─────────────────────────────────────────────────────────────

func (s *InstrumentedStorage) ListWisps(ctx context.Context, filter types.WispFilter) ([]*types.Issue, error) {
	ctx, span, t := s.op(ctx, "ListWisps")
	v, err := s.inner.ListWisps(ctx, filter)
	s.done(ctx, span, t, err)
	return v, err
}

// ── Streaming iterators ─────────────────────────────────────────────────────
//
// Iter* methods record a single span covering iterator CONSTRUCTION (the
// SQL query setup). Per-row work is NOT traced — the returned iterator is
// the inner store's iterator, unwrapped. Adding per-row tracing would
// require a wrapper type that ends a long-lived span on Close; that
// optimization is intentionally deferred until callers need it.

func (s *InstrumentedStorage) IterIssues(ctx context.Context, query string, filter types.IssueFilter) (storage.Iter[types.Issue], error) {
	ctx, span, t := s.op(ctx, "IterIssues")
	it, err := s.inner.IterIssues(ctx, query, filter)
	s.done(ctx, span, t, err)
	return it, err
}

func (s *InstrumentedStorage) IterDependentsWithMetadata(ctx context.Context, issueID string) (storage.Iter[types.IssueWithDependencyMetadata], error) {
	attrs := []attribute.KeyValue{attribute.String("bd.issue.id", issueID)}
	ctx, span, t := s.op(ctx, "IterDependentsWithMetadata", attrs...)
	it, err := s.inner.IterDependentsWithMetadata(ctx, issueID)
	s.done(ctx, span, t, err, attrs...)
	return it, err
}

func (s *InstrumentedStorage) IterDependenciesWithMetadata(ctx context.Context, issueID string) (storage.Iter[types.IssueWithDependencyMetadata], error) {
	attrs := []attribute.KeyValue{attribute.String("bd.issue.id", issueID)}
	ctx, span, t := s.op(ctx, "IterDependenciesWithMetadata", attrs...)
	it, err := s.inner.IterDependenciesWithMetadata(ctx, issueID)
	s.done(ctx, span, t, err, attrs...)
	return it, err
}

func (s *InstrumentedStorage) IterIssueComments(ctx context.Context, issueID string) (storage.Iter[types.Comment], error) {
	attrs := []attribute.KeyValue{attribute.String("bd.issue.id", issueID)}
	ctx, span, t := s.op(ctx, "IterIssueComments", attrs...)
	it, err := s.inner.IterIssueComments(ctx, issueID)
	s.done(ctx, span, t, err, attrs...)
	return it, err
}

func (s *InstrumentedStorage) IterEvents(ctx context.Context, issueID string, limit int) (storage.Iter[types.Event], error) {
	attrs := []attribute.KeyValue{attribute.String("bd.issue.id", issueID)}
	ctx, span, t := s.op(ctx, "IterEvents", attrs...)
	it, err := s.inner.IterEvents(ctx, issueID, limit)
	s.done(ctx, span, t, err, attrs...)
	return it, err
}

func (s *InstrumentedStorage) IterAllEventsSince(ctx context.Context, since time.Time) (storage.Iter[types.Event], error) {
	attrs := []attribute.KeyValue{attribute.String("bd.since", since.Format(time.RFC3339))}
	ctx, span, t := s.op(ctx, "IterAllEventsSince", attrs...)
	it, err := s.inner.IterAllEventsSince(ctx, since)
	s.done(ctx, span, t, err, attrs...)
	return it, err
}

func (s *InstrumentedStorage) IterReadyWork(ctx context.Context, filter types.WorkFilter) (storage.Iter[types.Issue], error) {
	ctx, span, t := s.op(ctx, "IterReadyWork")
	it, err := s.inner.IterReadyWork(ctx, filter)
	s.done(ctx, span, t, err)
	return it, err
}

func (s *InstrumentedStorage) IterBlockedIssues(ctx context.Context, filter types.WorkFilter) (storage.Iter[types.BlockedIssue], error) {
	ctx, span, t := s.op(ctx, "IterBlockedIssues")
	it, err := s.inner.IterBlockedIssues(ctx, filter)
	s.done(ctx, span, t, err)
	return it, err
}

func (s *InstrumentedStorage) IterWisps(ctx context.Context, filter types.WispFilter) (storage.Iter[types.Issue], error) {
	ctx, span, t := s.op(ctx, "IterWisps")
	it, err := s.inner.IterWisps(ctx, filter)
	s.done(ctx, span, t, err)
	return it, err
}

// ── Count* aggregates ─────────────────────────────────────────────────────────

func (s *InstrumentedStorage) CountIssues(ctx context.Context, query string, filter types.IssueFilter) (int64, error) {
	ctx, span, t := s.op(ctx, "CountIssues")
	v, err := s.inner.CountIssues(ctx, query, filter)
	s.done(ctx, span, t, err)
	return v, err
}

func (s *InstrumentedStorage) CountIssuesByGroup(ctx context.Context, filter types.IssueFilter, groupBy string) (map[string]int, error) {
	ctx, span, t := s.op(ctx, "CountIssuesByGroup")
	v, err := s.inner.CountIssuesByGroup(ctx, filter, groupBy)
	s.done(ctx, span, t, err)
	return v, err
}

func (s *InstrumentedStorage) CountDependents(ctx context.Context, issueID string) (int64, error) {
	ctx, span, t := s.op(ctx, "CountDependents", attribute.String("issue.id", issueID))
	v, err := s.inner.CountDependents(ctx, issueID)
	s.done(ctx, span, t, err)
	return v, err
}

func (s *InstrumentedStorage) CountDependencies(ctx context.Context, issueID string) (int64, error) {
	ctx, span, t := s.op(ctx, "CountDependencies", attribute.String("issue.id", issueID))
	v, err := s.inner.CountDependencies(ctx, issueID)
	s.done(ctx, span, t, err)
	return v, err
}

func (s *InstrumentedStorage) CountIssueComments(ctx context.Context, issueID string) (int64, error) {
	ctx, span, t := s.op(ctx, "CountIssueComments", attribute.String("issue.id", issueID))
	v, err := s.inner.CountIssueComments(ctx, issueID)
	s.done(ctx, span, t, err)
	return v, err
}

func (s *InstrumentedStorage) CountEvents(ctx context.Context, issueID string, limit int) (int64, error) {
	ctx, span, t := s.op(ctx, "CountEvents", attribute.String("issue.id", issueID))
	v, err := s.inner.CountEvents(ctx, issueID, limit)
	s.done(ctx, span, t, err)
	return v, err
}

// ── MergeSlot ────────────────────────────────────────────────────────────────

func (s *InstrumentedStorage) MergeSlotCreate(ctx context.Context, actor string) (*types.Issue, error) {
	ctx, span, t := s.op(ctx, "MergeSlotCreate")
	v, err := s.inner.MergeSlotCreate(ctx, actor)
	s.done(ctx, span, t, err)
	return v, err
}

func (s *InstrumentedStorage) MergeSlotCheck(ctx context.Context) (*storage.MergeSlotStatus, error) {
	ctx, span, t := s.op(ctx, "MergeSlotCheck")
	v, err := s.inner.MergeSlotCheck(ctx)
	s.done(ctx, span, t, err)
	return v, err
}

func (s *InstrumentedStorage) MergeSlotAcquire(ctx context.Context, holder, actor string, wait bool) (*storage.MergeSlotResult, error) {
	ctx, span, t := s.op(ctx, "MergeSlotAcquire", attribute.String("slot.holder", holder))
	v, err := s.inner.MergeSlotAcquire(ctx, holder, actor, wait)
	s.done(ctx, span, t, err)
	return v, err
}

func (s *InstrumentedStorage) MergeSlotRelease(ctx context.Context, holder, actor string) error {
	ctx, span, t := s.op(ctx, "MergeSlotRelease", attribute.String("slot.holder", holder))
	err := s.inner.MergeSlotRelease(ctx, holder, actor)
	s.done(ctx, span, t, err)
	return err
}

// ── Metadata slots ─────────────────────────────────────────────────────────

func (s *InstrumentedStorage) SlotSet(ctx context.Context, issueID, key, value, actor string) error {
	ctx, span, t := s.op(ctx, "SlotSet", attribute.String("slot.key", key))
	err := s.inner.SlotSet(ctx, issueID, key, value, actor)
	s.done(ctx, span, t, err)
	return err
}

func (s *InstrumentedStorage) SlotGet(ctx context.Context, issueID, key string) (string, error) {
	ctx, span, t := s.op(ctx, "SlotGet", attribute.String("slot.key", key))
	v, err := s.inner.SlotGet(ctx, issueID, key)
	s.done(ctx, span, t, err)
	return v, err
}

func (s *InstrumentedStorage) SlotClear(ctx context.Context, issueID, key, actor string) error {
	ctx, span, t := s.op(ctx, "SlotClear", attribute.String("slot.key", key))
	err := s.inner.SlotClear(ctx, issueID, key, actor)
	s.done(ctx, span, t, err)
	return err
}

func (s *InstrumentedStorage) MergeMetadata(ctx context.Context, issueID, key string, value json.RawMessage, actor string) error {
	ctx, span, t := s.op(ctx, "MergeMetadata", attribute.String("slot.key", key))
	err := s.inner.MergeMetadata(ctx, issueID, key, value, actor)
	s.done(ctx, span, t, err)
	return err
}

// ── Lifecycle ────────────────────────────────────────────────────────────────

func (s *InstrumentedStorage) Close() error {
	return s.inner.Close()
}

// Compile-time interface satisfaction.
var (
	_ storage.DoltStorage = (*InstrumentedStorage)(nil)
	_ storage.Unwrapper   = (*InstrumentedStorage)(nil)
)
