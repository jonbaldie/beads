package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

func (r *issueSearchRepository) SearchIssueIDs(ctx context.Context, query string, filter types.IssueFilter) ([]string, error) {
	return issueops.SearchIssueIDsInTx(ctx, r.runner, query, filter)
}

func (r *issueDeletionRepository) Delete(ctx context.Context, id string, opts domain.IssueTableOpts) error {
	table := "issues"
	if opts.UseWispsTable {
		table = "wisps"
	}
	// Edges are journaled before the row goes, while its snapshot can still be
	// read.
	if err := issueops.RecordDependencyRemovalsForIssuesInTx(ctx, r.runner, []string{id}); err != nil {
		return fmt.Errorf("db: IssueSQLRepository.Delete %s: journal dependency removals: %w", id, err)
	}
	//nolint:gosec // G201: table is a hardcoded constant.
	res, err := r.runner.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE id = ?", table), id)
	if err != nil {
		return fmt.Errorf("db: IssueSQLRepository.Delete %s from %s: %w", id, table, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("db: IssueSQLRepository.Delete rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("issue not found: %s", id)
	}
	// A deleted issue holds no lease (no-op for wisps, which are never leased).
	if err := issueops.DeleteLeaseInTx(ctx, r.runner, id); err != nil {
		return err
	}
	// The rows==0 return above keeps this actually-deleted-only.
	return issueops.RecordDeleteInTx(ctx, r.runner, id)
}

func (r *issueDependencyRepository) PartitionWispIDs(ctx context.Context, ids []string) ([]string, []string, error) {
	return issueops.PartitionWispIDsInTx(ctx, r.runner, ids)
}

func (r *issueDependencyRepository) FindAllDependents(ctx context.Context, ids []string) ([]string, error) {
	set, err := issueops.FindAllDependentsInTx(ctx, r.runner, ids)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out, nil
}

func (r *issueDependencyRepository) FindWispDependentsRecursive(ctx context.Context, ids []string) (map[string]bool, error) {
	return issueops.FindWispDependentsRecursiveInTx(ctx, r.runner, ids)
}

func (r *issueDependencyRepository) AffectedByDeletion(ctx context.Context, issueIDs, wispIDs []string) ([]string, []string, error) {
	return issueops.AffectedByDeletionInTx(ctx, r.runner, issueIDs, wispIDs)
}

func (r *issueDependencyRepository) RecomputeIsBlocked(ctx context.Context, issueIDs, wispIDs []string) error {
	return issueops.RecomputeIsBlockedInTx(ctx, r.runner, issueIDs, wispIDs)
}

func (r *issueLookupRepository) AsOf(ctx context.Context, id, ref string) (*types.Issue, error) {
	return issueops.AsOfInTx(ctx, r.runner, id, ref)
}

func (r *issueLifecycleRepository) Close(ctx context.Context, id string, params domain.CloseRowParams, actor string, _ domain.IssueTableOpts) (domain.CloseRowResult, error) {
	res, err := issueops.CloseIssueInTx(ctx, r.runner, id, params.Reason, actor, params.Session)
	if err != nil {
		return domain.CloseRowResult{}, fmt.Errorf("db: IssueSQLRepository.Close %s: %w", id, err)
	}
	return domain.CloseRowResult{
		Updated:       !res.AlreadyClosed,
		AlreadyClosed: res.AlreadyClosed,
		IsWisp:        res.IsWisp,
	}, nil
}

func (r *issueLifecycleRepository) CloseChecked(ctx context.Context, id string, params domain.CloseRowParams, actor string, force bool) (domain.CloseRowResult, error) {
	res, err := issueops.CloseIssueCheckedInTx(ctx, r.runner, id, params.Reason, actor, params.Session, force, nil)
	if err != nil {
		return domain.CloseRowResult{}, fmt.Errorf("db: IssueSQLRepository.CloseChecked %s: %w", id, err)
	}
	return domain.CloseRowResult{
		Updated:       !res.AlreadyClosed,
		AlreadyClosed: res.AlreadyClosed,
		IsWisp:        res.IsWisp,
		OpenChildren:  res.OpenChildren,
	}, nil
}

func (r *issueLifecycleRepository) Reopen(ctx context.Context, id string, params domain.ReopenRowParams, actor string, _ domain.IssueTableOpts) (domain.ReopenRowResult, error) {
	res, err := issueops.ReopenIssueInTx(ctx, r.runner, id, params.Reason, actor)
	if err != nil {
		return domain.ReopenRowResult{}, fmt.Errorf("db: IssueSQLRepository.Reopen %s: %w", id, err)
	}
	return domain.ReopenRowResult{
		Updated:     res.Changed,
		AlreadyOpen: res.AlreadyOpen,
		IsWisp:      res.IsWisp,
	}, nil
}

func (r *issueReportRepository) GetNewlyUnblockedByClose(ctx context.Context, closedID string) ([]*types.Issue, error) {
	out, err := issueops.GetNewlyUnblockedByCloseInTx(ctx, r.runner, closedID)
	if err != nil {
		return nil, fmt.Errorf("db: IssueSQLRepository.GetNewlyUnblockedByClose %s: %w", closedID, err)
	}
	return out, nil
}

func (r *issueClaimRepository) ClaimReadyIssue(ctx context.Context, filter types.WorkFilter, actor string) (*types.Issue, error) {
	out, err := issueops.ClaimReadyIssueInTx(ctx, r.runner, filter, actor)
	if err != nil {
		return nil, fmt.Errorf("db: IssueSQLRepository.ClaimReadyIssue: %w", err)
	}
	return out, nil
}

func (r *issueClaimRepository) ClaimReadyWisp(ctx context.Context, filter types.WorkFilter, actor string) (*types.Issue, error) {
	out, err := issueops.ClaimReadyIssueInTx(ctx, r.runner, filter, actor)
	if err != nil {
		return nil, fmt.Errorf("db: IssueSQLRepository.ClaimReadyWisp: %w", err)
	}
	return out, nil
}

func (r *issueReportRepository) GetBlockedIssues(ctx context.Context, filter types.WorkFilter) ([]*types.BlockedIssue, error) {
	out, err := issueops.GetBlockedIssuesInTx(ctx, r.runner, filter)
	if err != nil {
		return nil, fmt.Errorf("db: IssueSQLRepository.GetBlockedIssues: %w", err)
	}
	return out, nil
}

func (r *issueReportRepository) GetStatistics(ctx context.Context) (*types.Statistics, error) {
	stats := &types.Statistics{}
	if err := issueops.ScanIssueCountsInTx(ctx, r.runner, stats); err != nil {
		return nil, fmt.Errorf("db: IssueSQLRepository.GetStatistics: scan counts: %w", err)
	}
	var blocked int
	if err := r.runner.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM issues
		WHERE is_blocked = 1 AND status <> 'closed' AND status <> 'pinned'
	`).Scan(&blocked); err != nil {
		return nil, fmt.Errorf("db: IssueSQLRepository.GetStatistics: count blocked: %w", err)
	}
	stats.BlockedIssues = &blocked
	ready := stats.OpenIssues - blocked
	if ready < 0 {
		ready = 0
	}
	stats.ReadyIssues = &ready
	return stats, nil
}

func (r *issueReportRepository) CountIssues(ctx context.Context, query string, filter types.IssueFilter) (int64, error) {
	n, err := issueops.CountIssuesInTx(ctx, r.runner, query, filter)
	if err != nil {
		return 0, fmt.Errorf("db: IssueSQLRepository.CountIssues: %w", err)
	}
	return int64(n), nil
}

func (r *issueReportRepository) CountIssuesByGroup(ctx context.Context, filter types.IssueFilter, groupBy string) (map[string]int, error) {
	out, err := issueops.CountIssuesByGroupInTx(ctx, r.runner, filter, groupBy)
	if err != nil {
		return nil, fmt.Errorf("db: IssueSQLRepository.CountIssuesByGroup: %w", err)
	}
	return out, nil
}

func (r *issueReportRepository) History(ctx context.Context, id string) ([]*storage.HistoryEntry, error) {
	out, err := issueops.HistoryInTx(ctx, r.runner, id)
	if err != nil {
		return nil, fmt.Errorf("db: IssueSQLRepository.History: %w", err)
	}
	return out, nil
}

func (r *issueReportRepository) IterEvents(ctx context.Context, id string, limit int) (storage.Iter[types.Event], error) {
	events, err := issueops.GetEventsInTx(ctx, r.runner, id, limit)
	if err != nil {
		return nil, fmt.Errorf("db: IssueSQLRepository.IterEvents: %w", err)
	}
	return storage.NewSliceIter(events), nil
}

func (r *issueReportRepository) GetStaleIssues(ctx context.Context, filter types.StaleFilter) ([]*types.Issue, error) {
	out, err := issueops.GetStaleIssuesInTx(ctx, r.runner, filter)
	if err != nil {
		return nil, fmt.Errorf("db: IssueSQLRepository.GetStaleIssues: %w", err)
	}
	return out, nil
}

func (r *issueReportRepository) GetEpicsEligibleForClosure(ctx context.Context) ([]*types.EpicStatus, error) {
	out, err := issueops.GetEpicsEligibleForClosureInTx(ctx, r.runner)
	if err != nil {
		return nil, fmt.Errorf("db: IssueSQLRepository.GetEpicsEligibleForClosure: %w", err)
	}
	return out, nil
}

func (r *issueClaimRepository) UnclaimIssue(ctx context.Context, id, actor string, force bool) error {
	if err := issueops.UnclaimIssueInTx(ctx, r.runner, id, actor, force); err != nil {
		return fmt.Errorf("db: IssueSQLRepository.UnclaimIssue: %w", err)
	}
	return nil
}

// UnclaimIssueIfAssignee runs the classic compare-and-swap release against this
// runner. Like UnclaimIssue it takes no IssueTableOpts: issueops routes the
// write to the issues or wisps tables from the row itself, so a wisp's claim is
// released against the wisp tables on both backends. The mismatch verdict
// (storage.ErrAssigneeMismatch, nothing written) is produced by the shared
// helper, not restated here.
func (r *issueClaimRepository) UnclaimIssueIfAssignee(ctx context.Context, id, actor, expectedAssignee string) error {
	if err := issueops.UnclaimIssueIfAssigneeInTx(ctx, r.runner, id, actor, expectedAssignee); err != nil {
		return fmt.Errorf("db: IssueSQLRepository.UnclaimIssueIfAssignee: %w", err)
	}
	return nil
}

// HeartbeatIssue refreshes the lease on an issue actor holds in_progress,
// mirroring DoltStore.HeartbeatIssue: wisps are ephemeral and never leased,
// and the SQL work is the classic issueops.HeartbeatIssueInTx — same clock
// (time.Now().UTC()), same TTL resolution (issueops.LeaseTTL), and the same
// only-current-owner classification (storage.ErrAlreadyClaimed /
// ErrNotClaimable) — so classic `bd reclaim` staleness semantics see proxied
// heartbeats identically. Deliberately NO Dolt commit: the leases table is
// dolt_ignored (bd-lrgn1), and the cmd layer commits this transaction with
// uow.RunTxEphemeral (plain SQL COMMIT, nothing in dolt_log).
func (r *issueClaimRepository) HeartbeatIssue(ctx context.Context, id, actor string) error {
	if issueops.IsActiveWispInTx(ctx, r.runner, id) {
		return fmt.Errorf("db: IssueSQLRepository.HeartbeatIssue: %w: %s is ephemeral", storage.ErrNotClaimable, id)
	}
	if err := issueops.HeartbeatIssueInTx(ctx, r.runner, id, actor); err != nil {
		return fmt.Errorf("db: IssueSQLRepository.HeartbeatIssue: %w", err)
	}
	return nil
}

// WakeExpiredDefers runs the shared lazy defer-wake body against this
// repository's runner (the same DBTX-shaped seam ReclaimExpiredLeases uses)
// and reports how many rows woke per table. The issues count decides whether
// the transaction's owner mints a dolt commit; the wisps count decides
// whether it must still issue a plain SQL commit — wisp tables are
// dolt_ignored, so a wisp-only wake mints no version commit, but a caller
// that treats it as "nothing happened" rolls the wisp writes back.
func (r *issueClaimRepository) WakeExpiredDefers(ctx context.Context) (issues, wisps int, err error) {
	out, err := issueops.WakeExpiredDefersInTx(ctx, r.runner)
	if err != nil {
		return 0, 0, fmt.Errorf("db: IssueSQLRepository.WakeExpiredDefers: %w", err)
	}
	return len(out.Issues), len(out.Wisps), nil
}

func (r *issueClaimRepository) ReclaimExpiredLeases(ctx context.Context, olderThan time.Duration, filter types.ReclaimFilter, actor string) ([]types.ReclaimedLease, error) {
	cutoff := time.Now().UTC().Add(-olderThan)
	out, err := issueops.ReclaimExpiredLeasesInTx(ctx, r.runner, cutoff, filter, actor)
	if err != nil {
		return nil, fmt.Errorf("db: IssueSQLRepository.ReclaimExpiredLeases: %w", err)
	}
	return out, nil
}

const deleteBatchSize = 200
