package domain

import (
	"context"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

func (u *issueCloseModule) CloseIssue(ctx context.Context, id string, params CloseIssueParams, actor string) (CloseIssueResult, error) {
	return u.close(ctx, id, params, actor, false)
}

func (u *issueCloseModule) CloseWisp(ctx context.Context, id string, params CloseIssueParams, actor string) (CloseIssueResult, error) {
	return u.close(ctx, id, params, actor, true)
}

// CloseIssueChecked closes an issue through the shared guarded close path.
func (u *issueCloseModule) CloseIssueChecked(ctx context.Context, id string, params CloseIssueParams, actor string, force bool) (CloseIssueResult, error) {
	return u.closeChecked(ctx, id, params, actor, force, false)
}

// CloseWispChecked is the wisp twin of CloseIssueChecked.
func (u *issueCloseModule) CloseWispChecked(ctx context.Context, id string, params CloseIssueParams, actor string, force bool) (CloseIssueResult, error) {
	return u.closeChecked(ctx, id, params, actor, force, true)
}

func (u *issueCloseModule) closeChecked(ctx context.Context, id string, params CloseIssueParams, actor string, force, useWisp bool) (CloseIssueResult, error) {
	if id == "" {
		return CloseIssueResult{}, fmt.Errorf("close: id must not be empty")
	}
	if actor == "" {
		return CloseIssueResult{}, fmt.Errorf("close: actor must not be empty")
	}
	row, err := u.issueRepo.CloseChecked(ctx, id, CloseRowParams{Reason: params.Reason, Session: params.Session}, actor, force)
	if err != nil {
		return CloseIssueResult{}, fmt.Errorf("close %s: %w", id, err)
	}
	issue, err := u.issueRepo.Get(ctx, id, IssueTableOpts{UseWispsTable: row.IsWisp || useWisp})
	if err != nil {
		return CloseIssueResult{}, fmt.Errorf("close %s: reload: %w", id, err)
	}
	return CloseIssueResult{Issue: issue, Closed: !row.AlreadyClosed, OpenChildren: row.OpenChildren}, nil
}

func (u *issueCloseModule) close(ctx context.Context, id string, params CloseIssueParams, actor string, useWisp bool) (CloseIssueResult, error) {
	if id == "" {
		return CloseIssueResult{}, fmt.Errorf("close: id must not be empty")
	}
	if actor == "" {
		return CloseIssueResult{}, fmt.Errorf("close: actor must not be empty")
	}
	row, err := u.issueRepo.Close(ctx, id, CloseRowParams{Reason: params.Reason, Session: params.Session}, actor, IssueTableOpts{UseWispsTable: useWisp})
	if err != nil {
		return CloseIssueResult{}, fmt.Errorf("close %s: %w", id, err)
	}
	issue, err := u.issueRepo.Get(ctx, id, IssueTableOpts{UseWispsTable: row.IsWisp})
	if err != nil {
		return CloseIssueResult{}, fmt.Errorf("close %s: reload: %w", id, err)
	}
	return CloseIssueResult{
		Issue:  issue,
		Closed: !row.AlreadyClosed,
	}, nil
}

func (u *issueCloseModule) ReopenIssue(ctx context.Context, id string, params ReopenIssueParams, actor string) (ReopenIssueResult, error) {
	return u.reopen(ctx, id, params, actor, false)
}

func (u *issueCloseModule) ReopenWisp(ctx context.Context, id string, params ReopenIssueParams, actor string) (ReopenIssueResult, error) {
	return u.reopen(ctx, id, params, actor, true)
}

func (u *issueCloseModule) reopen(ctx context.Context, id string, params ReopenIssueParams, actor string, useWisp bool) (ReopenIssueResult, error) {
	if id == "" {
		return ReopenIssueResult{}, fmt.Errorf("reopen: id must not be empty")
	}
	if actor == "" {
		return ReopenIssueResult{}, fmt.Errorf("reopen: actor must not be empty")
	}
	row, err := u.issueRepo.Reopen(ctx, id, ReopenRowParams{Reason: params.Reason}, actor, IssueTableOpts{UseWispsTable: useWisp})
	if err != nil {
		return ReopenIssueResult{}, fmt.Errorf("reopen %s: %w", id, err)
	}
	issue, err := u.issueRepo.Get(ctx, id, IssueTableOpts{UseWispsTable: row.IsWisp})
	if err != nil {
		return ReopenIssueResult{}, fmt.Errorf("reopen %s: reload: %w", id, err)
	}
	return ReopenIssueResult{
		Issue:    issue,
		Reopened: row.Updated,
	}, nil
}

func (u *issueClaimModule) ClaimIssueIfOpen(ctx context.Context, id, actor string) (ClaimResult, error) {
	return u.claim(ctx, id, actor, false)
}

func (u *issueClaimModule) ClaimWispIfOpen(ctx context.Context, id, actor string) (ClaimResult, error) {
	return u.claim(ctx, id, actor, true)
}

func (u *issueChildrenModule) CountOpenChildren(ctx context.Context, id string) (int, error) {
	return u.countOpenChildren(ctx, id, false)
}

func (u *issueChildrenModule) CountOpenWispChildren(ctx context.Context, id string) (int, error) {
	return u.countOpenChildren(ctx, id, true)
}

func (u *issueChildrenModule) countOpenChildren(ctx context.Context, id string, useWisp bool) (int, error) {
	if id == "" {
		return 0, fmt.Errorf("CountOpenChildren: id must not be empty")
	}
	children, err := u.depRepo.ListWithIssueMetadata(ctx, id, DepListOpts{
		Types:         []types.DependencyType{types.DepParentChild},
		Direction:     DepDirectionIn,
		UseWispsTable: useWisp,
	})
	if err != nil {
		return 0, fmt.Errorf("CountOpenChildren %s: %w", id, err)
	}
	open := 0
	for _, child := range children {
		if child.Status != types.StatusClosed {
			open++
		}
	}
	return open, nil
}

func (u *issueChildrenModule) GetNewlyUnblockedByClose(ctx context.Context, closedID string) ([]*types.Issue, error) {
	return u.getNewlyUnblockedByClose(ctx, closedID)
}

func (u *issueChildrenModule) GetNewlyUnblockedByCloseWisp(ctx context.Context, closedID string) ([]*types.Issue, error) {
	return u.getNewlyUnblockedByClose(ctx, closedID)
}

func (u *issueChildrenModule) getNewlyUnblockedByClose(ctx context.Context, closedID string) ([]*types.Issue, error) {
	if closedID == "" {
		return nil, fmt.Errorf("GetNewlyUnblockedByClose: closedID must not be empty")
	}
	out, err := u.issueRepo.GetNewlyUnblockedByClose(ctx, closedID)
	if err != nil {
		return nil, fmt.Errorf("GetNewlyUnblockedByClose %s: %w", closedID, err)
	}
	return out, nil
}

func (u *issueClaimReadyModule) ClaimReadyIssue(ctx context.Context, filter types.WorkFilter, actor string) (ClaimReadyResult, error) {
	return u.claimReady(ctx, filter, actor, false)
}

func (u *issueClaimReadyModule) ClaimReadyWisp(ctx context.Context, filter types.WorkFilter, actor string) (ClaimReadyResult, error) {
	return u.claimReady(ctx, filter, actor, true)
}

func (u *issueClaimReadyModule) claimReady(ctx context.Context, filter types.WorkFilter, actor string, useWisp bool) (ClaimReadyResult, error) {
	var (
		issue *types.Issue
		err   error
	)
	if useWisp {
		issue, err = u.issueRepo.ClaimReadyWisp(ctx, filter, actor)
	} else {
		issue, err = u.issueRepo.ClaimReadyIssue(ctx, filter, actor)
	}
	if err != nil {
		if useWisp {
			return ClaimReadyResult{}, fmt.Errorf("ClaimReadyWisp: %w", err)
		}
		return ClaimReadyResult{}, fmt.Errorf("ClaimReadyIssue: %w", err)
	}
	return ClaimReadyResult{Issue: issue, Claimed: issue != nil}, nil
}

func (u *issueReportModule) GetBlockedIssues(ctx context.Context, filter types.WorkFilter) ([]*types.BlockedIssue, error) {
	out, err := u.issueRepo.GetBlockedIssues(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("GetBlockedIssues: %w", err)
	}
	return out, nil
}

func (u *issueReportModule) GetStatistics(ctx context.Context) (*types.Statistics, error) {
	out, err := u.issueRepo.GetStatistics(ctx)
	if err != nil {
		return nil, fmt.Errorf("GetStatistics: %w", err)
	}
	return out, nil
}

func (u *issueReportModule) CountIssues(ctx context.Context, query string, filter types.IssueFilter) (int64, error) {
	out, err := u.issueRepo.CountIssues(ctx, query, filter)
	if err != nil {
		return 0, fmt.Errorf("CountIssues: %w", err)
	}
	return out, nil
}

func (u *issueReportModule) CountIssuesByGroup(ctx context.Context, filter types.IssueFilter, groupBy string) (map[string]int, error) {
	out, err := u.issueRepo.CountIssuesByGroup(ctx, filter, groupBy)
	if err != nil {
		return nil, fmt.Errorf("CountIssuesByGroup: %w", err)
	}
	return out, nil
}

func (u *issueReportModule) History(ctx context.Context, id string) ([]*storage.HistoryEntry, error) {
	out, err := u.issueRepo.History(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("History: %w", err)
	}
	return out, nil
}

func (u *issueReportModule) IterEvents(ctx context.Context, id string, limit int) (storage.Iter[types.Event], error) {
	out, err := u.issueRepo.IterEvents(ctx, id, limit)
	if err != nil {
		return nil, fmt.Errorf("IterEvents: %w", err)
	}
	return out, nil
}

func (u *issueReportModule) GetStaleIssues(ctx context.Context, filter types.StaleFilter) ([]*types.Issue, error) {
	out, err := u.issueRepo.GetStaleIssues(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("GetStaleIssues: %w", err)
	}
	return out, nil
}

func (u *issueReportModule) GetEpicsEligibleForClosure(ctx context.Context) ([]*types.EpicStatus, error) {
	out, err := u.issueRepo.GetEpicsEligibleForClosure(ctx)
	if err != nil {
		return nil, fmt.Errorf("GetEpicsEligibleForClosure: %w", err)
	}
	return out, nil
}

func (u *issueMaintenanceModule) Unclaim(ctx context.Context, id, actor string, force bool) error {
	if id == "" {
		return fmt.Errorf("Unclaim: id must not be empty")
	}
	if err := u.issueRepo.UnclaimIssue(ctx, id, actor, force); err != nil {
		return fmt.Errorf("Unclaim: %w", err)
	}
	return nil
}

// UnclaimIfAssignee is the compare-and-swap release: it clears the claim only
// while the issue is still assigned to expectedAssignee, and otherwise returns
// storage.ErrAssigneeMismatch having written nothing. It is the conditional
// twin of Unclaim and runs the SAME transition (assignee cleared, status
// reopened, started_at cleared, lease dropped, row_lock rewritten, "unclaimed"
// event recorded) because both reach the one classic implementation in
// issueops — which is what makes `bd unclaim --if-assignee` behave identically
// on the proxied-server and embedded backends.
func (u *issueMaintenanceModule) UnclaimIfAssignee(ctx context.Context, id, actor, expectedAssignee string) error {
	if id == "" {
		return fmt.Errorf("UnclaimIfAssignee: id must not be empty")
	}
	if err := u.issueRepo.UnclaimIssueIfAssignee(ctx, id, actor, expectedAssignee); err != nil {
		return fmt.Errorf("UnclaimIfAssignee: %w", err)
	}
	return nil
}

// Heartbeat refreshes the lease on an issue actor holds in_progress. The
// write touches ONLY the ephemeral leases table (bd-lrgn1), so the caller
// must run it under uow.RunTxEphemeral's no-Dolt-commit form — a heartbeat
// mints no Dolt commit and no history in any mode (bd-aq0ql).
func (u *issueMaintenanceModule) Heartbeat(ctx context.Context, id, actor string) error {
	if id == "" {
		return fmt.Errorf("Heartbeat: id must not be empty")
	}
	if err := u.issueRepo.HeartbeatIssue(ctx, id, actor); err != nil {
		return fmt.Errorf("Heartbeat: %w", err)
	}
	return nil
}

// WakeExpiredDefers returns every expired DATED defer to open (see
// issueops.WakeExpiredDefersInTx) and reports how many permanent issues and
// wisps woke. The caller owns persistence: commit with a wake message iff
// issues > 0, and with the ephemeral plain-COMMIT form iff only wisps woke
// (wisp tables are dolt_ignored, so their wake needs a SQL commit but must
// mint no version commit).
func (u *issueMaintenanceModule) WakeExpiredDefers(ctx context.Context) (issues, wisps int, err error) {
	issues, wisps, err = u.issueRepo.WakeExpiredDefers(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("WakeExpiredDefers: %w", err)
	}
	return issues, wisps, nil
}

func (u *issueMaintenanceModule) ReclaimExpiredLeases(ctx context.Context, olderThan time.Duration, filter types.ReclaimFilter, actor string) ([]types.ReclaimedLease, error) {
	out, err := u.issueRepo.ReclaimExpiredLeases(ctx, olderThan, filter, actor)
	if err != nil {
		return nil, fmt.Errorf("ReclaimExpiredLeases: %w", err)
	}
	return out, nil
}
