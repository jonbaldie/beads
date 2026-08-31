package issueops

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/sqlbuild"
	"github.com/jonbaldie/beads/internal/types"
)

// RestoreLeaseOnImportInTx reconciles an issue's lease row after an
// import/upsert wrote the issue row (protocol L1.2: lease fields round-trip
// the JSONL interchange, wy-urlct — bd-lrgn1 moved them from issues columns
// to the ephemeral leases table).
//
// Two duties, both keyed off the STORED row (the winner after any stale-guard
// merge), never the snapshot alone:
//
//   - restore: when the snapshot carried a lease and the stored row is a live
//     claim, upsert the lease row — but NEVER clobber a live (unexpired) local
//     lease with snapshot data. Leases are node-local; the local grant is
//     always more authoritative than a replicated timestamp.
//   - reconcile: when the accepted state ended or transferred the claim, drop
//     any now-orphaned local lease row so the UpsertLeaseInTx invariant
//     (lease row ⇔ live claim) holds.
//
// A restored lease keeps the granting replica the SNAPSHOT names
// (issue.LeaseGrantedNode), never this node — this is the one path that can
// materialize a foreign lease row locally, and mislabelling it as local is
// exactly the cross-replica reclaim hazard (wy-jpd3.7). A snapshot written by
// a pre-wy-jpd3.7 binary carries no node, which restores as "" (unknown) and
// therefore behaves exactly as it did before this change.
//
// Wisps are never leased; callers route them away before calling this.
func RestoreLeaseOnImportInTx(ctx context.Context, tx DBTX, issue *types.Issue, isNew bool) error {
	now := time.Now().UTC()

	if issue.LeaseExpiresAt != nil {
		var status, assignee string
		err := tx.QueryRowContext(ctx,
			"SELECT status, COALESCE(assignee, '') FROM issues WHERE id = ?", issue.ID,
		).Scan(&status, &assignee)
		if err != nil {
			return fmt.Errorf("read stored row for lease restore of %s: %w", issue.ID, err)
		}
		if status == string(types.StatusInProgress) && assignee != "" {
			grantedAt := now
			heartbeatAt := now
			if issue.HeartbeatAt != nil {
				grantedAt = *issue.HeartbeatAt
				heartbeatAt = *issue.HeartbeatAt
			}
			// Assignment order matters: lease_expires_at is the liveness
			// comparison column and ON DUPLICATE KEY UPDATE assignments are
			// evaluated in order, so it must be reassigned LAST.
			_, err := tx.ExecContext(ctx, `
				INSERT INTO leases (issue_id, holder, granted_at, lease_expires_at, heartbeat_at, granted_node)
				VALUES (?, ?, ?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE
					holder = IF(leases.lease_expires_at >= ?, leases.holder, VALUES(holder)),
					granted_at = IF(leases.lease_expires_at >= ?, leases.granted_at, VALUES(granted_at)),
					heartbeat_at = IF(leases.lease_expires_at >= ?, leases.heartbeat_at, VALUES(heartbeat_at)),
					granted_node = IF(leases.lease_expires_at >= ?, leases.granted_node, VALUES(granted_node)),
					lease_expires_at = IF(leases.lease_expires_at >= ?, leases.lease_expires_at, VALUES(lease_expires_at))
			`, issue.ID, assignee, grantedAt, *issue.LeaseExpiresAt, heartbeatAt, issue.LeaseGrantedNode,
				now, now, now, now, now)
			if err != nil {
				return fmt.Errorf("restore lease for %s: %w", issue.ID, err)
			}
		}
	}

	// An upsert over an existing row may have ended or transferred the claim
	// (e.g. a newer snapshot closed the issue): drop a lease row that no
	// longer matches a live claim by its holder.
	//
	// This join is a SQL equality (i.assignee = leases.holder), not
	// actorMatches — a query predicate can't canonicalize across a join the
	// way Go code can, so a lease granted under one spelling of an identity
	// can be dropped here if the issue's stored assignee is later written
	// under a different, equivalent spelling (ga-v2k49). Bounded and
	// self-healing: HeartbeatIssueInTx's disambiguation fallback re-arms the
	// lease under the caller's current spelling on the next beat, so the gap
	// is one issue briefly in_progress with no lease row (visible to
	// stale/reclaim reads), never a stuck state.
	if !isNew {
		_, err := tx.ExecContext(ctx, `
			DELETE FROM leases WHERE issue_id = ?
			  AND NOT EXISTS (
				SELECT 1 FROM issues i
				WHERE i.id = ? AND i.status = 'in_progress' AND i.assignee = leases.holder
			  )
		`, issue.ID, issue.ID)
		if err != nil {
			return fmt.Errorf("reconcile lease for %s: %w", issue.ID, err)
		}
	}
	return nil
}

// HeartbeatIssueInTx proves the lease owner is still alive: it pushes
// lease_expires_at forward by the TTL and stamps heartbeat_at = now on the
// issue's lease row. Only the current holder may heartbeat — a heartbeat from
// anyone else, or on an issue whose lease is gone (closed, unclaimed,
// reclaimed, or never leased — wisps), affects no rows and returns
// storage.ErrNotClaimable / ErrAlreadyClaimed so the caller learns its lease
// is gone.
//
// The write touches ONLY the leases table (ephemeral, dolt_ignored): a
// heartbeat mints no Dolt commit and no history, and deliberately does NOT
// stamp issues.updated_at — updated_at keeps its merge/LWW meaning and bd
// stale consults leases.heartbeat_at for in_progress rows instead (bd-lrgn1).
//
//nolint:gosec // G201: table names come from WispTableRouting (hardcoded constants)
func HeartbeatIssueInTx(ctx context.Context, tx DBTX, id, actor string) error {
	now := time.Now().UTC()
	rows, err := heartbeatLeaseRow(ctx, tx, id, actor, now)
	if err != nil {
		return err
	}
	if rows > 0 {
		// DELIBERATELY NOT JOURNALED — do not add an emit here. A heartbeat is a
		// high-frequency lease keepalive: it writes only the clone-local `leases`
		// table (lease-liveness state), never a durable bead field. Journaling it
		// would put a full-snapshot write plus the shared seq-counter serialization
		// on the hottest write in the fleet, and a replay consumer gains nothing —
		// lease state lives on the working-set plane and expires on its own. Lease
		// RECLAIM is the opposite case and does journal: it clears assignee and
		// reverts status, which is durable bead state. The decision is pinned by
		// journalExemptMutations in journal_completeness_test.go.
		return nil
	}
	return diagnoseMissingHeartbeatLease(ctx, tx, id, actor, now)
}

func heartbeatLeaseRow(ctx context.Context, tx DBTX, id, actor string, now time.Time) (int64, error) {
	// granted_node is backfilled, never overwritten: a lease row whose
	// provenance is unknown (granted before ignored migration 0016, or before
	// this replica was named) is being kept alive through THIS node's store,
	// which is the node that can enforce it. A row that already names a
	// replica keeps it — a heartbeat proves the holder is alive, not that the
	// lease moved. NodeID is "" unless an operator configured it, so on an
	// unnamed deployment this backfill writes '' over '' and changes nothing:
	// the fail-open default cannot be silently converted to fail-closed.
	result, err := tx.ExecContext(ctx, `
		UPDATE leases SET lease_expires_at = ?, heartbeat_at = ?,
			granted_node = IF(COALESCE(granted_node, '') = '', ?, granted_node)
		WHERE issue_id = ? AND holder = ?
	`, now.Add(leaseTTL(ctx)), now, NodeID(ctx), id, actor)
	if err != nil {
		return 0, fmt.Errorf("failed to heartbeat issue: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get rows affected: %w", err)
	}
	return rows, nil
}

func diagnoseMissingHeartbeatLease(ctx context.Context, tx DBTX, id, actor string, now time.Time) error {
	// No lease row. Disambiguate from the issue row: gone
	// (closed/reopened/reclaimed), not-found, owned by someone else, or a
	// wisp (never leased).
	isWisp := IsActiveWispInTx(ctx, tx, id)
	issueTable, _, _, _ := WispTableRouting(isWisp)
	var assignee, status string
	qerr := tx.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COALESCE(assignee, ''), status FROM %s WHERE id = ?", issueTable), id,
	).Scan(&assignee, &status)
	if qerr != nil {
		return fmt.Errorf("%w: %s", storage.ErrNotClaimable, id)
	}
	// Judged under actorMatches, not verbatim (ga-v2k49): the primary
	// UPDATE's own `holder = ?` predicate above stays a verbatim SQL
	// comparison deliberately — fixing it would mean pre-reading the
	// lease row on every heartbeat (the hottest write in the fleet, see
	// above) just to bind back its own holder value. A spelling
	// difference across layers (ga-wzl83) already makes that predicate
	// affect 0 rows and fall here; fixing the disambiguation instead
	// means the caller still self-heals correctly (re-arms below, under
	// its current spelling) at the cost of the slow path on a spelling
	// mismatch, never on the byte-identical common case.
	if assignee != "" && !actorMatches(assignee, actor) {
		return fmt.Errorf("%w by %s", storage.ErrAlreadyClaimed, assignee)
	}
	if !isWisp && actorMatches(assignee, actor) && status == string(types.StatusInProgress) {
		// The caller genuinely holds the claim but has no lease row — e.g.
		// the claim was hand-doled through a generic update (which never
		// arms a lease, bd-9hpgf), the worker is now opting into lease
		// semantics, or (ga-v2k49) the existing lease row's holder is a
		// different spelling of the same identity (ga-wzl83) and the
		// primary UPDATE's verbatim predicate above missed it. A real
		// worker's heartbeat re-arms recovery — under actor's current
		// spelling, which is what the next heartbeat's fast path will see.
		return UpsertLeaseInTx(ctx, tx, id, actor, now, leaseTTL(ctx))
	}
	return fmt.Errorf("%w: %s status %s", storage.ErrNotClaimable, id, status)
}

// warnReplica writes a replica-guard audit line to STDERR (never stdout —
// bd reclaim --json owns stdout and a stray line there breaks every machine
// consumer), suppressed only by quiet mode.
func warnReplica(format string, args ...any) {
	if debug.IsQuiet() {
		return
	}
	fmt.Fprintf(os.Stderr, format, args...)
}

// reportForeignSkips names the stale leases the replica guard declined.
// Best-effort and never fatal: this is an audit courtesy on the reclaim path,
// not a correctness step, so a failed query is swallowed rather than allowed
// to abort a reclaim that is otherwise fine.
func reportForeignSkips(ctx context.Context, tx DBTX, cutoff time.Time, filter types.ReclaimFilter, localNode string) {
	scopeSQL, scopeArgs := sqlbuild.ReclaimScopeSQL(filter, sqlbuild.IssuesFilterTables, "i")
	args := append([]any{cutoff, localNode}, scopeArgs...)
	rows, err := tx.QueryContext(ctx, `
		SELECT l.issue_id, COALESCE(l.granted_node, ''), COALESCE(i.assignee, '') FROM leases l
		JOIN issues i ON i.id = l.issue_id
		WHERE i.status = 'in_progress'
		  AND l.lease_expires_at < ?
		  AND COALESCE(l.granted_node, '') NOT IN ('', ?)
	`+scopeSQL, args...)
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, grantedNode, holder string
		if err := rows.Scan(&id, &grantedNode, &holder); err != nil {
			return
		}
		warnReplica("reclaim: skipping %s (held by %s) — lease granted by replica %q, not this node (%q). "+
			"Reap it on that replica, or pass --any-replica if that replica is gone.\n",
			id, holder, grantedNode, localNode)
	}
}

// reclaimReplicaSQL returns the granting-replica predicate for the reclaim
// snapshot and the per-row DELETE, plus its args. Both sites must apply the
// SAME predicate: the DELETE re-checks by id, and skipping the guard there
// would let a foreign lease slip through on the re-check path.
//
// Empty when the guard is disarmed — either explicitly (filter.AnyReplica) or
// because this deployment cannot name itself (localNode == ""), where every
// comparison would be against "" and the guard would degenerate to "reclaim
// only unknown-provenance leases", stranding this node's own work.
func reclaimReplicaSQL(filter types.ReclaimFilter, localNode string) (string, []any) {
	if filter.AnyReplica || localNode == "" {
		return "", nil
	}
	// granted_node '' is unknown provenance and stays eligible (fail-open);
	// only a lease that positively names a different replica is protected.
	//
	// granted_node is deliberately UNQUALIFIED: the snapshot splices this into
	// a `leases l JOIN issues i` (where it resolves to l, since issues has no
	// such column) but the DELETE site splices it into a bare
	// `DELETE FROM leases`, which has no alias to qualify with. Adding a
	// leases-only column to `issues` would make the snapshot ambiguous — give
	// this an alias parameter then, and pass "" from the DELETE.
	return "\n\t\t  AND (COALESCE(granted_node, '') = '' OR granted_node = ?)", []any{localNode}
}
