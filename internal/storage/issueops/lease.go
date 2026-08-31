package issueops

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/config"
)

// DefaultLeaseTTL is how long a fresh claim stays valid without a heartbeat.
// A worker is expected to call HeartbeatIssueInTx well within this window
// (heartbeat cadence ≫ claim cadence; see the commit-bloat note on bd heartbeat)
// so a live claim's lease_expires_at always sits in the future. A worker that
// dies stops heartbeating, its lease_expires_at goes stale, and bd reclaim
// reverts the issue to ready. Tunable per-claim via WithLeaseTTL on the
// context, falling back to this default.
const DefaultLeaseTTL = 5 * time.Minute

// leaseTTLContextKey overrides DefaultLeaseTTL for a single claim. Used by tests
// (short TTLs) and callers that know their work cadence; unset in normal use.
type leaseTTLContextKey struct{}

// WithLeaseTTL returns a context whose claims use ttl instead of DefaultLeaseTTL.
func WithLeaseTTL(ctx context.Context, ttl time.Duration) context.Context {
	return context.WithValue(ctx, leaseTTLContextKey{}, ttl)
}

// leaseTTL resolves the lease TTL for the current claim/heartbeat.
func leaseTTL(ctx context.Context) time.Duration {
	if ttl, ok := ctx.Value(leaseTTLContextKey{}).(time.Duration); ok && ttl > 0 {
		return ttl
	}
	return DefaultLeaseTTL
}

// freshRowLock returns a random non-zero int64 for the row_lock cell.
//
// row_lock is the keystone of dead-worker recovery on Dolt. Dolt has no real
// row locking and merges concurrent commits cell-by-cell, so two transactions
// that touch DIFFERENT cells of the same issue row (a reclaim writing status,
// a close writing closed_at) merge silently instead of conflicting — which
// would let a reclaim quietly revert an issue the owner just closed. By having
// every status/ownership-mutating path rewrite this one shared cell to a fresh
// random value, those writers always collide on row_lock, surfacing the
// 1213/1205 serialization conflict that withRetryTx replays. The value's only
// job is to differ from whatever a concurrent writer wrote, so any source of
// entropy works; we use crypto/rand to avoid seeding concerns. Never 0 (the
// column default) so a freshly-claimed row is always distinguishable from a
// never-touched one.
//
// INVARIANT: any path that mutates status, assignee, or started_at on an
// in_progress issue MUST rewrite row_lock — that is the set the reclaim/close
// races care about (claim, close, updateIssueInTx, reclaim, unclaim all do).
// Paths that touch only orthogonal cells (is_blocked, compaction_level,
// dependency metadata, rename, or reopen — which acts on closed rows) are safe
// to merge with a reclaim and intentionally do NOT rewrite it. Heartbeats no
// longer touch the issues row at all (bd-lrgn1): the lease lives in the
// ephemeral leases table, where a racing heartbeat and reclaim contend on the
// SAME lease row and conflict without any help. Adding a new path that sets
// status/assignee outside updateIssueInTx without rewriting row_lock would
// silently reintroduce the zombie-merge bug.
//
// The exemptions are load-bearing in the OTHER direction too (analysis
// absorbed from gastownhall/beads#4682, Julian Knutsen): row_lock doubles as
// the RowVersion optimistic-concurrency token (types.Issue.RowVersion), so a
// path that reminted it WITHOUT changing the content a CAS holder cares about
// would spuriously fail honest ExpectedVersion checks. The is_blocked
// recompute is the canonical case: it is a denormalized aux-marker refresh
// that already goes out of its way to preserve updated_at
// (blocked_state.go), and bumping the token there would clobber a concurrent
// whole-row CAS over a cell derived from OTHER rows. (The PR's second
// exemption of this kind, the lease heartbeat, no longer arises here — since
// bd-lrgn1 heartbeats never touch the issues row at all.) So the invariant
// cuts both ways: status/ownership writes MUST remint, aux-marker writes MUST
// NOT. The remint direction is enforced at build time by
// TestAllIssueRowWritesStampRowLock (row_lock_guard_test.go), whose exemption
// markers are the machine-readable form of this list — widen the stamping
// policy there first, not by ad-hoc stamps.
func freshRowLock() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is catastrophic and ~never happens; fall back to a
		// timestamp so the row_lock still changes rather than wedging the write.
		return time.Now().UnixNano() | 1
	}
	v := int64(binary.LittleEndian.Uint64(b[:]))
	if v == 0 {
		v = 1
	}
	return v
}

// RowLockClause returns the SET-clause fragment and arg that rewrite row_lock
// to a fresh value. Append to any UPDATE that mutates status/assignee/
// started_at on an issues row (see the freshRowLock invariant). Exported for
// the proxied-server (uow) claim path in internal/storage/domain/db, which
// builds its own claim UPDATE rather than calling ClaimIssueInTx.
func RowLockClause() (string, []interface{}) {
	return "row_lock = ?", []interface{}{freshRowLock()}
}

// FreshRowLock returns a fresh non-zero row_lock token for an INSERT column
// list. Exported for the proxied-server (uow) create path in
// internal/storage/domain/db, which builds its own INSERT rather than calling
// InsertIssueIntoTable. Every create must stamp a non-zero row_lock so the
// RowVersion optimistic-concurrency token is live from the first write, exactly
// like the classic insert (see the freshRowLock invariant and types.Issue.RowVersion).
func FreshRowLock() int64 {
	return freshRowLock()
}

// LeaseTTL is the exported form of leaseTTL: it resolves the lease TTL for the
// current claim from the context (WithLeaseTTL) or falls back to
// DefaultLeaseTTL.
func LeaseTTL(ctx context.Context) time.Duration {
	return leaseTTL(ctx)
}

// nodeIDContextKey overrides config.NodeID() for a single call. Used by tests
// that have to be two replicas at once (one process, one database, two
// granting nodes); unset in normal use, where every call resolves the real
// machine identity.
type nodeIDContextKey struct{}

// WithNodeID returns a context whose lease writes and reclaim guard treat node
// as this replica's identity instead of config.NodeID().
func WithNodeID(ctx context.Context, node string) context.Context {
	return context.WithValue(ctx, nodeIDContextKey{}, node)
}

// NodeID resolves the granting-replica identity for the current lease
// operation: the context override (WithNodeID) if present, else the
// deployment's node identity (config.NodeID()). "" means the provenance of a
// lease granted here is unknown — see ReclaimExpiredLeasesInTx for how the
// guard degrades.
func NodeID(ctx context.Context) string {
	if node, ok := ctx.Value(nodeIDContextKey{}).(string); ok {
		return node
	}
	return config.NodeID()
}

// UpsertLeaseInTx grants or re-grants the lease on an issue to holder: a
// future expiry, a now heartbeat. The lease row lives in the ephemeral leases
// table (dolt_ignored on the Dolt backend, bd-lrgn1), NOT on the issues row,
// so granting or renewing it mints no Dolt commit and no history. Leases are
// deliberately node-local: they are only enforceable on the replica that
// granted them; cross-machine claim VISIBILITY rides status/assignee on the
// issues row, which still commits.
//
// granted_node records WHICH replica that is (config.NodeID(), wy-jpd3.7).
// A re-grant through this node makes this node the granting replica, so the
// value is rewritten unconditionally alongside holder/granted_at.
//
// INVARIANT: a leases row exists if and only if its issue is a live claim
// (in_progress with the row's holder as assignee) on this node. Every path
// that ends or transfers a claim — close, unclaim, reclaim, delete, a generic
// update that changes status/assignee, an import that accepts a newer
// non-claimed snapshot — must delete the lease row (DeleteLeaseInTx). Wisps
// are never leased (see testHeartbeatWisp) and never get a row here.
func UpsertLeaseInTx(ctx context.Context, tx DBTX, id, holder string, now time.Time, ttl time.Duration) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO leases (issue_id, holder, granted_at, lease_expires_at, heartbeat_at, granted_node)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			holder = VALUES(holder),
			granted_at = VALUES(granted_at),
			lease_expires_at = VALUES(lease_expires_at),
			heartbeat_at = VALUES(heartbeat_at),
			granted_node = VALUES(granted_node)
	`, id, holder, now, now.Add(ttl), now, NodeID(ctx))
	if err != nil {
		return fmt.Errorf("upsert lease for %s: %w", id, err)
	}
	return nil
}

// DeleteLeaseInTx removes the lease row for an issue, if any. Call from every
// path that ends or transfers a claim (see the UpsertLeaseInTx invariant).
// Deleting a lease that does not exist is a no-op, so callers may invoke it
// unconditionally — including for wisp IDs, which never have lease rows.
func DeleteLeaseInTx(ctx context.Context, tx DBTX, id string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM leases WHERE issue_id = ?`, id); err != nil {
		return fmt.Errorf("delete lease for %s: %w", id, err)
	}
	return nil
}
