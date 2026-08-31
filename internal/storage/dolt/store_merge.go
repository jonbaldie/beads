package dolt

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

// Branch creates a new branch
func (s *DoltStore) Branch(ctx context.Context, name string) (retErr error) {
	ctx, span := doltTracer.Start(ctx, "dolt.branch",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append(s.doltSpanAttrs(),
			attribute.String("dolt.branch", name),
		)...),
	)
	defer func() { endSpan(span, retErr) }()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for branch: %w", err)
	}
	defer conn.Close()
	return versioncontrolops.CreateBranch(ctx, conn, name)
}

// Checkout switches to the specified branch
func (s *DoltStore) Checkout(ctx context.Context, branch string) (retErr error) {
	ctx, span := doltTracer.Start(ctx, "dolt.checkout",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append(s.doltSpanAttrs(),
			attribute.String("dolt.branch", branch),
		)...),
	)
	defer func() { endSpan(span, retErr) }()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for checkout: %w", err)
	}
	defer conn.Close()
	if err := versioncontrolops.CheckoutBranch(ctx, conn, branch); err != nil {
		return err
	}
	s.branch = branch
	return nil
}

// Merge merges the specified branch into the current branch.
// Returns any merge conflicts if present. Implements storage.VersionedStorage.
func (s *DoltStore) Merge(ctx context.Context, branch string) (conflicts []storage.Conflict, retErr error) {
	ctx, span := doltTracer.Start(ctx, "dolt.merge",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append(s.doltSpanAttrs(),
			attribute.String("dolt.merge_branch", branch),
		)...),
	)
	defer func() { endSpan(span, retErr) }()

	// bd-578h9.11: like every pull path, a branch merge brings in writes that
	// bypassed the local is_blocked hooks; recompute after a conflict-free
	// merge. Conflicted merges defer to the caller's post-resolution hook
	// (Sync, bd vc merge --strategy) — recomputing over unresolved rows would
	// read garbage.
	preHead := ""
	if !s.readOnly {
		if h, err := s.GetCurrentCommit(ctx); err == nil {
			preHead = h
		}
	}

	conflicts, err := versioncontrolops.Merge(ctx, s.db, branch, s.commitAuthorString())
	if len(conflicts) > 0 {
		span.SetAttributes(attribute.Int("dolt.conflicts", len(conflicts)))
	}
	if err == nil && len(conflicts) == 0 && !s.readOnly {
		if rerr := s.recomputeBlockedAfterPull(ctx, preHead); rerr != nil {
			return conflicts, fmt.Errorf("merge succeeded but is_blocked recompute failed: %w", rerr)
		}
	}
	return conflicts, err
}

// MergeWithStrategy implements storage.StrategicMerger for `bd vc merge
// --strategy` (#4992). Merge (above) runs the bare CALL DOLT_MERGE on the
// shared pool: that is enough to detect a conflict-shaped autocommit
// rejection, but not to resolve one, because Dolt's conflict-tolerant session
// flags (@@dolt_allow_commit_conflicts, @@dolt_force_transaction_commit) are
// session state and the pool may hand a later statement a different
// connection. MergeWithStrategy instead pins a single connection — the same
// pattern Branch/Checkout use for stored procedures — for the whole
// merge/resolve/repair/commit sequence versioncontrolops.MergeWithStrategy
// runs.
//
// A resolved merge (conflicted or clean) always commits, so — unlike Merge,
// which skips the recompute for a still-conflicted merge — the is_blocked
// recompute always runs on success here.
func (s *DoltStore) MergeWithStrategy(ctx context.Context, branch, strategy string) (conflicts []storage.Conflict, retErr error) {
	ctx, span := doltTracer.Start(ctx, "dolt.merge_with_strategy",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append(s.doltSpanAttrs(),
			attribute.String("dolt.merge_branch", branch),
			attribute.String("dolt.merge_strategy", strategy),
		)...),
	)
	defer func() { endSpan(span, retErr) }()

	preHead := ""
	if !s.readOnly {
		if h, err := s.GetCurrentCommit(ctx); err == nil {
			preHead = h
		}
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection for merge: %w", err)
	}
	conflicts, err = versioncontrolops.MergeWithStrategy(ctx, conn, branch, s.commitAuthorString(), strategy)
	// Release the pinned connection before the recompute: s.db's pool can be
	// configured with a single connection (setupTestStore's MaxOpenConns: 1
	// mirrors constrained production configs), and recomputeBlockedAfterPull
	// acquires its own connection — held past this point, conn would starve
	// it of the only one available.
	closeErr := conn.Close()
	if len(conflicts) > 0 {
		span.SetAttributes(attribute.Int("dolt.conflicts", len(conflicts)))
	}
	if err != nil {
		return conflicts, err
	}
	if closeErr != nil {
		return conflicts, fmt.Errorf("release merge connection: %w", closeErr)
	}
	if !s.readOnly {
		if rerr := s.recomputeBlockedAfterPull(ctx, preHead); rerr != nil {
			return conflicts, fmt.Errorf("merge succeeded but is_blocked recompute failed: %w", rerr)
		}
	}
	return conflicts, nil
}

// RecomputeBlockedAfterMerge recomputes the denormalized is_blocked column
// for the rows changed since fromCommit and commits the result — the hook a
// caller that resolved merge conflicts itself must run after committing the
// resolution (bd-578h9.11): conflicted merges skip the automatic recompute
// because unresolved rows would feed it garbage, and nothing else covers the
// merged-in writes. fromCommit is the pre-merge HEAD; empty degrades to a
// full-graph recompute.
func (s *DoltStore) RecomputeBlockedAfterMerge(ctx context.Context, fromCommit string) error {
	return s.recomputeBlockedAfterPull(ctx, fromCommit)
}
