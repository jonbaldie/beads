package dolt

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

// validateUpdateMetadata validates an inbound metadata update value against the
// configured schema (GH#1416 Phase 2) before any wisp routing. It is a no-op
// when the update carries no "metadata" key. Shared by UpdateIssue and
// UpdateIssueChecked so both apply the identical pre-write validation.
func validateUpdateMetadata(updates map[string]interface{}) error {
	rawMeta, ok := updates["metadata"]
	if !ok {
		return nil
	}
	metadataStr, err := storage.NormalizeMetadataValue(rawMeta)
	if err != nil {
		return fmt.Errorf("invalid metadata: %w", err)
	}
	return validateMetadataIfConfigured(json.RawMessage(metadataStr))
}

// checkExpectedVersionInTx enforces the optional ExpectedVersion CAS
// precondition inside tx: when expectedVersion is non-nil the row's current
// RowVersion (row_lock) must still equal it, else the caller's transaction
// returns storage.ErrVersionMismatch and rolls back with the issue unchanged. A
// nil expectedVersion disables the check (an unconditional update).
func checkExpectedVersionInTx(ctx context.Context, tx *sql.Tx, id string, expectedVersion *int64) error {
	if expectedVersion == nil {
		return nil
	}
	return issueops.CheckVersionInTx(ctx, tx, id, *expectedVersion)
}

// UpdateIssue updates fields on an issue.
// Delegates SQL work to issueops.UpdateIssueInTx; handles Dolt-specific concerns
// (metadata validation, DemoteToWisp, DOLT_ADD/COMMIT, cache invalidation).
func (s *DoltStore) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		return s.updateIssue(ctx, id, updates, actor)
	})
}

func (s *DoltStore) updateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	// Validate metadata against schema before wisp routing (GH#1416 Phase 2).
	if err := validateUpdateMetadata(updates); err != nil {
		return err
	}

	// Route ephemeral IDs to wisps table (falls through for promoted wisps).
	// Wisps skip DOLT_COMMIT since they live in dolt_ignored tables.
	if s.isActiveWisp(ctx, id) {
		return s.updateWisp(ctx, id, updates, actor)
	}

	// If updating a regular issue to no-history or ephemeral, migrate it to the
	// wisps table instead of updating in-place. Table routing only happens at
	// create time by default, so we must perform the migration here. (be-x4l)
	_, settingNoHistory := updates["no_history"]
	_, settingWisp := updates["wisp"]
	if settingNoHistory || settingWisp {
		return s.DemoteToWisp(ctx, id, updates, actor)
	}

	// Wrap in withRetryTx so a concurrent writer that loses Dolt's optimistic
	// commit-time merge (MySQL 1213/1205, guaranteed server-side rollback) is
	// retried rather than surfaced as a hard failure. Dolt has no real row
	// locking — FOR UPDATE / SKIP LOCKED are parse-only no-ops
	// (https://www.dolthub.com/blog/2023-10-23-hold-my-beer/) — so retry is the
	// only safety net. withRetryTx owns BeginTx and the final Commit.
	return s.withRetryTx(ctx, func(tx *sql.Tx) error {
		result, err := issueops.UpdateIssueInTx(ctx, tx, id, updates, actor)
		if err != nil {
			return err
		}
		if !result.Changed {
			return nil
		}

		commitMsg := fmt.Sprintf("bd: update %s", id)
		return s.doltAddAndCommitInTx(ctx, tx, []string{"issues", "events"}, commitMsg)
	})
}

// UpdateIssueChecked applies the update like UpdateIssue, adding an optional
// optimistic-concurrency precondition: when opts.ExpectedVersion is non-nil the
// update proceeds only if the issue's current RowVersion (row_lock) still equals
// *opts.ExpectedVersion, else it refuses with storage.ErrVersionMismatch. The
// version read and the update share ONE transaction, so a mismatch returns
// before any write and the transaction rolls back with the issue unchanged (a
// true compare-and-swap). nil disables the check, leaving behavior identical to
// UpdateIssue. Mirrors UpdateIssue's Dolt-specific concerns (metadata
// validation, wisp routing, DemoteToWisp, DOLT_ADD/COMMIT); UpdateIssue is the
// hot path and is left untouched.
func (s *DoltStore) UpdateIssueChecked(ctx context.Context, id string, updates map[string]interface{}, actor string, opts storage.UpdateIssueOptions) error {
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		return s.updateIssueChecked(ctx, id, updates, actor, opts)
	})
}

func (s *DoltStore) updateIssueChecked(ctx context.Context, id string, updates map[string]interface{}, actor string, opts storage.UpdateIssueOptions) error {
	// Validate metadata against schema before wisp routing (GH#1416 Phase 2).
	if err := validateUpdateMetadata(updates); err != nil {
		return err
	}

	// Route ephemeral IDs to wisps table (falls through for promoted wisps).
	// Wisps skip DOLT_COMMIT since they live in dolt_ignored tables.
	if s.isActiveWisp(ctx, id) {
		return s.updateWispChecked(ctx, id, updates, actor, opts)
	}

	// If updating a regular issue to no-history or ephemeral, migrate it to the
	// wisps table instead of updating in-place (mirrors UpdateIssue). The
	// precondition checks share the demotion transaction so the CAS stays
	// atomic on this path.
	_, settingNoHistory := updates["no_history"]
	_, settingWisp := updates["wisp"]
	if settingNoHistory || settingWisp {
		return s.updateIssueCheckedDemotion(ctx, id, updates, actor, opts)
	}

	return s.updateIssueCheckedWrite(ctx, id, updates, actor, opts)
}

func (s *DoltStore) updateIssueCheckedDemotion(ctx context.Context, id string, updates map[string]interface{}, actor string, opts storage.UpdateIssueOptions) error {
	return s.withRetryTx(ctx, func(tx *sql.Tx) error {
		if err := checkExpectedVersionInTx(ctx, tx, id, opts.ExpectedVersion); err != nil {
			return err
		}
		if err := issueops.CheckExpectedFieldsInTx(ctx, tx, id, opts.ExpectedAssignee, opts.ExpectedStatus); err != nil {
			return err
		}
		return s.demoteToWispInTx(ctx, tx, id, updates, actor)
	})
}

func (s *DoltStore) updateIssueCheckedWrite(ctx context.Context, id string, updates map[string]interface{}, actor string, opts storage.UpdateIssueOptions) error {
	// Wrap in withRetryTx exactly like UpdateIssue so a concurrent writer that
	// loses Dolt's optimistic commit-time merge (MySQL 1213/1205, guaranteed
	// server-side rollback) is retried rather than surfaced as a hard failure.
	// A precondition mismatch (storage.ErrVersionMismatch /
	// ErrAssigneeMismatch / ErrStatusMismatch) is NOT a serialization error, so
	// withRetryTx surfaces it permanently and the transaction rolls back — no
	// update and no event are written (the atomic-refuse property). A
	// concurrent write that commits DURING this tx collides on the row_lock cell
	// and is replayed by withRetryTx, which re-reads the preconditions here and
	// refuses. withRetryTx owns BeginTx and the final Commit.
	write := func() error {
		return s.withRetryTx(ctx, func(tx *sql.Tx) error {
			if err := checkExpectedVersionInTx(ctx, tx, id, opts.ExpectedVersion); err != nil {
				return err
			}
			if err := issueops.CheckExpectedFieldsInTx(ctx, tx, id, opts.ExpectedAssignee, opts.ExpectedStatus); err != nil {
				return err
			}
			result, err := issueops.UpdateIssueInTx(ctx, tx, id, updates, actor)
			if err != nil {
				return err
			}
			if !result.Changed {
				return nil
			}

			commitMsg := fmt.Sprintf("bd: update %s", id)
			return s.doltAddAndCommitInTx(ctx, tx, []string{"issues", "events"}, commitMsg)
		})
	}

	// A guarded update that writes the coordination fields (a reassign or a
	// claim-on-behalf, bd-wsqvw) is claim-family: resolve it by verify-by-re-read
	// (bd-zccb9) instead of trusting the exit status. Safe to replay after a
	// verified rollback — the guards are re-checked inside the replayed
	// transaction, so a racing writer makes the replay refuse rather than
	// clobber. Unguarded or non-coordination updates keep their exit status.
	if post, ok := guardedUpdatePostcondition(opts, updates); ok {
		return s.verifiedClaimWrite(ctx, id, post, write)
	}
	return write()
}

// ClaimIssue atomically claims an issue using compare-and-swap semantics.
// It sets the assignee to actor and status to "in_progress" only if the issue
// currently has no assignee. Returns storage.ErrAlreadyClaimed if already claimed.
// Delegates SQL work to issueops.ClaimIssueInTx; handles Dolt-specific concerns
// (wisp routing, DOLT_ADD/COMMIT, cache invalidation).
func (s *DoltStore) ClaimIssue(ctx context.Context, id string, actor string) error {
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		return s.claimIssue(ctx, id, actor)
	})
}

func (s *DoltStore) claimIssue(ctx context.Context, id string, actor string) error {
	// Route ephemeral IDs to wisps table (falls through for promoted wisps).
	// Wisps skip DOLT_COMMIT since they live in dolt_ignored tables.
	if s.isActiveWisp(ctx, id) {
		return s.claimWisp(ctx, id, actor)
	}

	// Wrap in withRetryTx so a concurrent claim that loses Dolt's optimistic
	// commit-time merge (MySQL 1213/1205, guaranteed server-side rollback) is
	// retried instead of surfaced as a hard failure. Dolt has no real row
	// locking — FOR UPDATE / SKIP LOCKED are parse-only no-ops
	// (https://www.dolthub.com/blog/2023-10-23-hold-my-beer/) — so retry is the
	// only safety net under concurrent claimants. The body stays a single tx
	// (CAS + DOLT_COMMIT); withRetryTx owns BeginTx and the final Commit.
	// The whole write is then resolved by verify-by-re-read (bd-zccb9): under a
	// degraded server the exit status is not truth in either direction.
	return s.verifiedClaimWrite(ctx, id, claimedBy(actor), func() error {
		return s.withRetryTx(ctx, func(tx *sql.Tx) error {
			if _, err := issueops.ClaimIssueInTx(ctx, tx, id, actor); err != nil {
				return err
			}

			commitMsg := fmt.Sprintf("bd: claim %s", id)
			return s.doltAddAndCommitInTx(ctx, tx, []string{"issues", "events"}, commitMsg)
		})
	})
}

// ClaimReadyIssue atomically claims the first ready issue matching filter.
func (s *DoltStore) ClaimReadyIssue(ctx context.Context, filter types.WorkFilter, actor string) (*types.Issue, error) {
	// Wrap in withRetryTx: under concurrent workers the loser of Dolt's
	// optimistic commit-time merge gets MySQL 1213/1205 (guaranteed server-side
	// rollback). Retrying re-scans the ready front from a fresh snapshot and
	// claims the next available issue instead of failing the dequeue. Dolt has
	// no real row locking — FOR UPDATE / SKIP LOCKED are parse-only no-ops
	// (https://www.dolthub.com/blog/2023-10-23-hold-my-beer/) — so retry is the
	// safety net. withRetryTx owns BeginTx and the final Commit.
	//
	// The write and its verify sit under withCircuitWrite so terminal circuit
	// success is recorded once at the boundary, only after verifiedReadyClaim
	// confirms the claim landed — not the instant withRetryTx's SQL commit
	// returns. A commit that reports success but fails verification must leave
	// the breaker untouched (matching ClaimIssue).
	var claimed *types.Issue
	err := s.withCircuitWrite(ctx, func(ctx context.Context) error {
		write := func() (*types.Issue, error) {
			var got *types.Issue
			werr := s.withRetryTx(ctx, func(tx *sql.Tx) error {
				var err error
				got, err = issueops.ClaimReadyIssueInTx(ctx, tx, filter, actor)
				if err != nil {
					return err
				}
				if got == nil {
					return nil
				}

				commitMsg := fmt.Sprintf("bd: claim ready %s", got.ID)
				return s.doltAddAndCommitInTx(ctx, tx, []string{"issues", "events"}, commitMsg)
			})
			return got, werr
		}
		var verr error
		claimed, verr = s.verifiedReadyClaim(ctx, actor, write)
		return verr
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// verifiedReadyClaim resolves a ready-claim write by verify-by-re-read
// (bd-zccb9): the claimed ID is only known once the write body has run, so
// this cannot ride verifiedClaimWrite's id parameter — but the resolution
// protocol is the same. Split from ClaimReadyIssue so injection tests can
// drive the write seam directly, the same way the verifiedClaimWrite tests do.
//
// Successful ready claims verify the winning plane because IncludeEphemeral
// may select a wisp. An indeterminate commit response remains indeterminate:
// assignee and status cannot prove the lease and actor-attributed event landed.
func (s *DoltStore) verifiedReadyClaim(ctx context.Context, actor string, write func() (*types.Issue, error)) (*types.Issue, error) {
	claimed, err := write()
	if err != nil {
		return nil, err
	}
	if claimed == nil || !s.serverMode {
		return claimed, err
	}
	assignee, status, verr := s.readReadyClaimState(ctx, claimed.ID)
	if verr != nil {
		return nil, fmt.Errorf("ready claim of %s reported success but could not be verified (server degraded?): %w — re-read the issue before trusting the claim",
			claimed.ID, verr)
	}
	post := claimedBy(actor)
	if post.want(assignee, status) {
		return claimed, nil
	}
	doltMetrics.claimVerifyLost.Add(ctx, 1, metric.WithAttributes(
		attribute.String("op", "ready-claim")))
	return nil, fmt.Errorf("ready claim of %s reported success but did not land (found assignee=%q status=%q, want %s) — server likely degraded; treat the claim as NOT applied",
		claimed.ID, assignee, status, post.desc)
}

// HeartbeatIssue refreshes the lease on an issue actor holds in_progress,
// pushing lease_expires_at forward on its row in the ephemeral leases table
// (see issueops.lease). Deliberately NO DOLT_ADD/DOLT_COMMIT: the leases
// table is dolt_ignored, so a heartbeat mints no commit and no history — this
// is the whole point of bd-lrgn1 (fleet heartbeats were the dominant source
// of unbounded reachable history). Wrapped in withRetryTx so a heartbeat that
// loses Dolt's optimistic merge to a concurrent reclaim/close on the same
// lease row is replayed against a fresh snapshot rather than surfaced.
func (s *DoltStore) HeartbeatIssue(ctx context.Context, id, actor string) error {
	if s.isActiveWisp(ctx, id) {
		// Wisps are ephemeral and never leased; nothing to heartbeat.
		return fmt.Errorf("%w: %s is ephemeral", storage.ErrNotClaimable, id)
	}
	return s.withRetryTx(ctx, func(tx *sql.Tx) error {
		return issueops.HeartbeatIssueInTx(ctx, tx, id, actor)
	})
}

// ReclaimExpiredLeases reverts in_progress issues whose lease expired more than
// olderThan ago back to ready, recovering work stranded by dead workers. The
// reclaim rewrites row_lock so it conflicts with any racing heartbeat/close on
// the same row; withRetryTx replays the loser. Returns the reclaimed issues.
func (s *DoltStore) ReclaimExpiredLeases(ctx context.Context, olderThan time.Duration, filter types.ReclaimFilter, actor string) ([]types.ReclaimedLease, error) {
	cutoff := time.Now().UTC().Add(-olderThan)
	var reclaimed []types.ReclaimedLease
	err := s.withRetryTx(ctx, func(tx *sql.Tx) error {
		var err error
		reclaimed, err = issueops.ReclaimExpiredLeasesInTx(ctx, tx, cutoff, filter, actor)
		if err != nil {
			return err
		}
		if len(reclaimed) == 0 {
			return nil
		}
		commitMsg := fmt.Sprintf("bd: reclaim %d expired lease(s)", len(reclaimed))
		return s.doltAddAndCommitInTx(ctx, tx, []string{"issues", "events"}, commitMsg)
	})
	if err != nil {
		return nil, err
	}
	return reclaimed, nil
}

// UnclaimIssue atomically unclaims an issue by clearing the assignee, resetting
// status to "open", deleting its lease row and rewriting row_lock. Records
// an "unclaimed" event. Only the current assignee may release its own claim
// unless force is set (admin/reaper override). Delegates SQL work to
// issueops.UnclaimIssueInTx; handles Dolt-specific concerns (DOLT_ADD/COMMIT).
//
// Wrapped in withRetryTx like the other claim-family writes so a concurrent
// writer that loses Dolt's optimistic commit-time merge (1213/1205) is retried
// rather than surfaced as a hard failure.
func (s *DoltStore) UnclaimIssue(ctx context.Context, id string, actor string, force bool) error {
	// verify-by-re-read (bd-zccb9): a phantom unclaim leaves the caller
	// believing the issue is released while it still holds the claim.
	//
	// withCircuitWrite wraps the write and its verification so circuit success
	// is recorded once at the boundary, only after verifiedClaimWrite confirms
	// the release landed — never the moment withRetryTx's SQL commit returns.
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		return s.verifiedClaimWrite(ctx, id, unclaimed(), func() error {
			return s.withRetryTx(ctx, func(tx *sql.Tx) error {
				if err := issueops.UnclaimIssueInTx(ctx, tx, id, actor, force); err != nil {
					return err
				}

				commitMsg := fmt.Sprintf("bd: unclaim %s", id)
				return s.doltAddAndCommitInTx(ctx, tx, []string{"issues", "events"}, commitMsg)
			})
		})
	})
}

// UnclaimIssueIfAssignee releases a claim only while the issue is still assigned
// to expectedAssignee (compare-and-swap, the inverse of ClaimIssue). Returns
// storage.ErrAssigneeMismatch, leaving the issue untouched, when the current
// assignee differs. Delegates SQL work to issueops.UnclaimIssueIfAssigneeInTx;
// handles Dolt-specific concerns (DOLT_ADD/COMMIT). Wrapped in withRetryTx like
// UnclaimIssue so a concurrent writer that loses Dolt's optimistic commit-time
// merge is retried rather than surfaced as a hard failure.
func (s *DoltStore) UnclaimIssueIfAssignee(ctx context.Context, id string, actor string, expectedAssignee string) error {
	// verify-by-re-read (bd-zccb9), same reasoning as UnclaimIssue: the write
	// and its verification sit under withCircuitWrite so circuit success is
	// recorded once at the boundary, only after verifiedClaimWrite confirms the
	// release landed.
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		return s.verifiedClaimWrite(ctx, id, unclaimed(), func() error {
			return s.withRetryTx(ctx, func(tx *sql.Tx) error {
				if err := issueops.UnclaimIssueIfAssigneeInTx(ctx, tx, id, actor, expectedAssignee); err != nil {
					return err
				}

				commitMsg := fmt.Sprintf("bd: unclaim %s", id)
				return s.doltAddAndCommitInTx(ctx, tx, []string{"issues", "events"}, commitMsg)
			})
		})
	})
}

// ReopenIssue reopens a done-category issue atomically and stages only the
// versioned tables that this transaction concretely changed.
func (s *DoltStore) ReopenIssue(ctx context.Context, id string, reason string, actor string) error {
	return s.withRetryTx(ctx, func(tx *sql.Tx) error {
		res, err := issueops.ReopenIssueInTx(ctx, tx, id, reason, actor)
		if err != nil {
			return err
		}
		if !res.Changed {
			return nil
		}
		switch {
		case !res.IsWisp:
			return s.doltAddAndCommitInTx(ctx, tx, []string{"issues", "events"}, fmt.Sprintf("bd: reopen %s", id))
		case res.IssueRowsChanged:
			return s.doltAddAndCommitInTx(ctx, tx, []string{"issues"}, fmt.Sprintf("bd: reopen %s", id))
		default:
			return nil
		}
	})
}

// UpdateIssueType changes the issue_type field of an issue.
// Wraps UpdateIssue for Dolt-specific concerns (wisp routing, DOLT_COMMIT, etc.).
func (s *DoltStore) UpdateIssueType(ctx context.Context, id string, issueType string, actor string) error {
	return s.UpdateIssue(ctx, id, map[string]interface{}{"issue_type": issueType}, actor)
}

// CloseIssue closes an issue with a reason.
// Delegates SQL work to issueops.CloseIssueInTx; handles Dolt-specific concerns
// (wisp routing, DOLT_ADD/COMMIT, cache invalidation).
func (s *DoltStore) CloseIssue(ctx context.Context, id string, reason string, actor string, session string) error {
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		return s.closeIssue(ctx, id, reason, actor, session)
	})
}

func (s *DoltStore) closeIssue(ctx context.Context, id string, reason string, actor string, session string) error {
	// Route ephemeral IDs to wisps table (falls through for promoted wisps).
	// Wisps skip DOLT_COMMIT since they live in dolt_ignored tables.
	if s.isActiveWisp(ctx, id) {
		return s.closeWisp(ctx, id, reason, actor, session)
	}

	// Wrap in withRetryTx so a concurrent writer that loses Dolt's optimistic
	// commit-time merge (MySQL 1213/1205, guaranteed server-side rollback) is
	// retried rather than surfaced as a hard failure. Dolt has no real row
	// locking — FOR UPDATE / SKIP LOCKED are parse-only no-ops
	// (https://www.dolthub.com/blog/2023-10-23-hold-my-beer/) — so retry is the
	// only safety net. withRetryTx owns BeginTx and the final Commit.
	return s.withRetryTx(ctx, func(tx *sql.Tx) error {
		if _, err := issueops.CloseIssueInTx(ctx, tx, id, reason, actor, session); err != nil {
			return err
		}

		commitMsg := fmt.Sprintf("bd: close %s", id)
		return s.doltAddAndCommitInTx(ctx, tx, []string{"issues", "events"}, commitMsg)
	})
}

// CloseIssueChecked closes an issue but refuses with storage.ErrCloseBlocked
// when it has a live direct blocker unless opts.Force is set, and — when
// opts.ExpectedVersion is non-nil — with storage.ErrVersionMismatch when the
// row's current RowVersion no longer matches (an orthogonal CAS that Force does
// not bypass). Both checks and the close share one transaction, so they are
// atomic (no TOCTOU). Mirrors CloseIssue's Dolt-specific concerns (wisp routing,
// DOLT_ADD/COMMIT).
func (s *DoltStore) CloseIssueChecked(ctx context.Context, id string, actor string, opts storage.CloseIssueOptions) (storage.CloseIssueResult, error) {
	var result storage.CloseIssueResult
	err := s.withCircuitWrite(ctx, func(ctx context.Context) error {
		var err error
		result, err = s.closeIssueChecked(ctx, id, actor, opts)
		return err
	})
	return result, err
}

func (s *DoltStore) closeIssueChecked(ctx context.Context, id string, actor string, opts storage.CloseIssueOptions) (storage.CloseIssueResult, error) {
	// Route ephemeral IDs to wisps table (falls through for promoted wisps).
	// Wisps skip DOLT_COMMIT since they live in dolt_ignored tables.
	if s.isActiveWisp(ctx, id) {
		return s.closeWispChecked(ctx, id, actor, opts)
	}

	// Wrap in withRetryTx exactly like CloseIssue so a concurrent writer that
	// loses Dolt's optimistic commit-time merge (MySQL 1213/1205, guaranteed
	// server-side rollback) is retried. A blocked-guard rejection
	// (storage.ErrCloseBlocked) is NOT a serialization error, so withRetryTx
	// surfaces it permanently and the transaction rolls back — no close and no
	// event are written (the atomic-refuse property).
	var result storage.CloseIssueResult
	if err := s.withRetryTx(ctx, func(tx *sql.Tx) error {
		res, err := issueops.CloseIssueCheckedInTx(ctx, tx, id, opts.Reason, actor, opts.Session, opts.Force, opts.ExpectedVersion)
		if err != nil {
			return err
		}
		result = storage.CloseIssueResult{Unchanged: res.AlreadyClosed, OpenChildren: res.OpenChildren}

		commitMsg := fmt.Sprintf("bd: close %s", id)
		return s.doltAddAndCommitInTx(ctx, tx, []string{"issues", "events"}, commitMsg)
	}); err != nil {
		return storage.CloseIssueResult{}, err
	}
	return result, nil
}
