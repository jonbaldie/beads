package issueops

import (
	"context"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/storage/sqlbuild"
	"github.com/jonbaldie/beads/internal/types"
)

// ReclaimExpiredLeasesInTx reverts in_progress issues whose lease has gone stale
// back to ready: the lease row is deleted, then status → open, assignee cleared,
// started_at cleared, and a fresh row_lock so the reclaim conflicts with a
// racing close/update on the same issues row (see freshRowLock). An issue is
// stale when its lease row's lease_expires_at is strictly before cutoff.
// Callers pass cutoff = now - graceWindow (the supervisor uses graceWindow =
// 2×TTL) so only leases that expired a safe margin ago — i.e. workers that are
// almost certainly dead — are reclaimed.
//
// Leases are node-local (the leases table is dolt_ignored and does not
// replicate), so a reclaim can only recover claims granted through this node —
// which is the only place the lease was ever enforceable anyway.
//
// REPLICA GUARD (wy-jpd3.7). "Through this node" is now enforced, not merely
// implied by the transport. The one path that can materialize a foreign lease
// row locally is RestoreLeaseOnImportInTx (lease fields round-trip the JSONL
// interchange), and a lease imported from another replica carries a liveness
// view that is stale by up to one sync interval — reverting on it can rob a
// worker that is very much alive over there. So a lease whose granted_node
// POSITIVELY names another replica is skipped, and only an explicit
// filter.AnyReplica (bd reclaim --any-replica) reverts it.
//
// The guard is OPT-IN and deliberately fail-open. An empty granted_node (a lease
// row predating ignored migration 0016, or the default state of any
// deployment that has not set config.NodeID()) is treated as local, so an
// upgrade can never strand a stale lease the reaper could previously recover,
// and a single-store deployment — including many hosts sharing one dolt
// sql-server, where there is no sync interval to defend against — keeps
// exactly its old behavior. See config.NodeID for why there is no hostname
// fallback. The invariant the guard cannot enforce, and that every federated
// deployment still owes:
//
//	grace window > sync interval  (and TTL > sync interval)
//
// A TTL or grace shorter than the cadence at which replicas exchange state is
// meaningless across the bridge — the remote view is stale by a full interval
// by construction, so a reaper on the far side decides on liveness data older
// than the lease it is judging. bd reclaim's default grace is 2× the lease
// TTL; raise the TTL (or the grace) above the sync interval, not the other way
// round.
//
// Reclaim only ever touches the permanent issues table: wisps are ephemeral and
// are never leased work. Returns the issues it reverted (id + the owner it took
// the lease from) so the caller can log/emit recovery events. The caller owns
// Dolt versioning.
//
// filter narrows which stale leases are eligible (see types.ReclaimFilter); the
// zero filter keeps the historical global behavior. Scoping is applied to the
// snapshot SELECT only — the per-row DELETE/UPDATE re-checks staleness by id,
// and the ids it re-checks all came from the already-scoped snapshot, so a
// label removed mid-reclaim cannot smuggle an out-of-scope issue into the
// revert.
func ReclaimExpiredLeasesInTx(ctx context.Context, tx DBTX, cutoff time.Time, filter types.ReclaimFilter, actor string) ([]types.ReclaimedLease, error) {
	stale, staleNodes, replicaSQL, replicaArgs, err := snapshotStaleLeases(ctx, tx, cutoff, filter)
	if err != nil {
		return nil, err
	}
	localNode := NodeID(ctx)
	// Audit the other side of the guard: a stale lease this reaper declined
	// because another replica granted it. Silence here would look identical to
	// "nothing to recover" while a genuinely dead remote worker's unit sits
	// in_progress forever, so name it (the operator's remedy is to reap on the
	// granting replica, or --any-replica once that replica is gone for good).
	if replicaSQL != "" {
		reportForeignSkips(ctx, tx, cutoff, filter, localNode)
	}

	if len(stale) == 0 {
		return nil, nil
	}

	options := staleLeaseReclaimOptions{
		cutoff:      cutoff,
		replicaSQL:  replicaSQL,
		replicaArgs: replicaArgs,
		actor:       actor,
		anyReplica:  filter.AnyReplica,
		localNode:   localNode,
	}
	var reclaimed []types.ReclaimedLease
	for _, r := range stale {
		wasReclaimed, err := reclaimStaleLease(ctx, tx, r, staleNodes[r.ID], options)
		if err != nil {
			return nil, err
		}
		if wasReclaimed {
			reclaimed = append(reclaimed, r)
		}
	}
	return reclaimed, nil
}

type staleLeaseReclaimOptions struct {
	cutoff      time.Time
	replicaSQL  string
	replicaArgs []any
	actor       string
	anyReplica  bool
	localNode   string
}

func snapshotStaleLeases(ctx context.Context, tx DBTX, cutoff time.Time, filter types.ReclaimFilter) (
	[]types.ReclaimedLease, map[string]string, string, []any, error,
) {
	// Snapshot the stale set first so we can report exactly which issues we
	// reverted and record per-issue recovery events. The DELETE below repeats
	// the expiry predicate, so an issue that a concurrent heartbeat rescued
	// between the SELECT and the DELETE is simply skipped (0 rows) — it never
	// appears as reclaimed.
	scopeSQL, scopeArgs := sqlbuild.ReclaimScopeSQL(filter, sqlbuild.IssuesFilterTables, "i")
	localNode := NodeID(ctx)
	replicaSQL, replicaArgs := reclaimReplicaSQL(filter, localNode)
	args := append([]any{cutoff}, replicaArgs...)
	args = append(args, scopeArgs...)
	rows, err := tx.QueryContext(ctx, `
		SELECT l.issue_id, COALESCE(i.assignee, ''), COALESCE(l.granted_node, '') FROM leases l
		JOIN issues i ON i.id = l.issue_id
		WHERE i.status = 'in_progress'
		  AND l.lease_expires_at < ?
	`+replicaSQL+scopeSQL, args...)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("scan for stale leases: %w", err)
	}
	var stale []types.ReclaimedLease
	// Provenance of each snapshot row, parallel to stale, for the
	// --any-replica audit line below. Kept beside the slice rather than on
	// types.ReclaimedLease: it is a reclaim-time diagnostic, not part of the
	// recovery record every backend returns to callers.
	staleNodes := map[string]string{}
	for rows.Next() {
		var r types.ReclaimedLease
		var grantedNode string
		if err := rows.Scan(&r.ID, &r.PreviousOwner, &grantedNode); err != nil {
			_ = rows.Close()
			return nil, nil, "", nil, fmt.Errorf("scan stale lease row: %w", err)
		}
		staleNodes[r.ID] = grantedNode
		stale = append(stale, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, "", nil, fmt.Errorf("iterate stale leases: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, "", nil, fmt.Errorf("close stale lease rows: %w", err)
	}
	return stale, staleNodes, replicaSQL, replicaArgs, nil
}

func reclaimStaleLease(ctx context.Context, tx DBTX, lease types.ReclaimedLease, grantedNode string, options staleLeaseReclaimOptions) (bool, error) {
	reclaimed, err := applyStaleLeaseChanges(ctx, tx, lease.ID, options)
	if err != nil {
		return false, err
	}
	if !reclaimed {
		return false, nil
	}
	if err := recordStaleReclaim(ctx, tx, lease, options.actor); err != nil {
		return false, err
	}
	// Logged HERE, past both re-checks, so the audit trail records reverts
	// that actually happened: a heartbeat landing after the snapshot makes
	// the DELETE match nothing, and a "reverting X" line printed up there
	// would be a lie the operator has no way to catch.
	// localNode "" is skipped, not printed as `not this node ("")`: on an
	// unnamed deployment the guard was never armed, so there is no
	// override to audit — every lease was already eligible.
	if options.anyReplica && options.localNode != "" && grantedNode != "" && grantedNode != options.localNode {
		warnReplica("reclaim: --any-replica reverted %s (held by %s) — lease was granted by replica %q, not this node (%q)\n",
			lease.ID, lease.PreviousOwner, grantedNode, options.localNode)
	}
	return true, nil
}

func applyStaleLeaseChanges(ctx context.Context, tx DBTX, id string, options staleLeaseReclaimOptions) (bool, error) {
	// Re-check the expiry inside the DELETE so a heartbeat that landed
	// after the snapshot (pushing lease_expires_at back into the future)
	// cannot be clobbered: heartbeat and reclaim contend on this same lease
	// row, so one of a racing pair is forced to retry, and a winning
	// rescuer's pushed-out expiry makes this DELETE match nothing.
	deleted, err := deleteStaleLease(ctx, tx, id, options)
	if err != nil {
		return false, err
	}
	if !deleted {
		return false, nil
	}
	// Revert the issue itself. status is re-checked so a row that stopped
	// being in_progress under us (closed) is left alone; row_lock makes a
	// concurrent close/update conflict at commit time rather than
	// cell-merge with this write.
	updated, err := reopenStaleIssue(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if !updated {
		return false, nil
	}
	return true, nil
}

func deleteStaleLease(ctx context.Context, tx DBTX, id string, options staleLeaseReclaimOptions) (bool, error) {
	delArgs := append([]any{id, options.cutoff}, options.replicaArgs...)
	res, err := tx.ExecContext(ctx, `
			DELETE FROM leases WHERE issue_id = ? AND lease_expires_at < ?
		`+options.replicaSQL, delArgs...)
	if err != nil {
		return false, fmt.Errorf("reclaim %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reclaim %s rows affected: %w", id, err)
	}
	return n > 0, nil
}

func reopenStaleIssue(ctx context.Context, tx DBTX, id string) (bool, error) {
	res, err := tx.ExecContext(ctx, `
			UPDATE issues
			SET status = 'open', assignee = NULL, started_at = NULL,
			    updated_at = ?, row_lock = ?
			WHERE id = ? AND status = 'in_progress'
		`, time.Now().UTC(), freshRowLock(), id)
	if err != nil {
		return false, fmt.Errorf("reclaim %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reclaim %s rows affected: %w", id, err)
	}
	return n > 0, nil
}

func recordStaleReclaim(ctx context.Context, tx DBTX, lease types.ReclaimedLease, actor string) error {
	if err := RecordFullEventInTable(ctx, tx, "events", lease.ID, types.EventLeaseReclaimed, actor,
		lease.PreviousOwner, ""); err != nil {
		return fmt.Errorf("record reclaim event for %s: %w", lease.ID, err)
	}
	// Journal the lease reclaim as an update (assignee cleared, status
	// reverted to open) so a replayer sees the claim released. Emitted past
	// both re-checks, so only reverts that actually happened are recorded.
	if err := RecordEventInTx(ctx, tx, EventUpdate, lease.ID); err != nil {
		return err
	}
	return nil
}
