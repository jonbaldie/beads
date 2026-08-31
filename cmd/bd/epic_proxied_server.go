package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
)

func runEpicStatusProxiedServer(ctx context.Context, eligibleOnly bool) error {
	uw, err := openProxiedListUOW(ctx)
	if err != nil {
		return HandleError("%v", err)
	}
	defer uw.Close(ctx)

	epics, err := uw.IssueUseCase().GetEpicsEligibleForClosure(ctx)
	if err != nil {
		return HandleErrorRespectJSON("getting epic status: %v", err)
	}
	return renderEpicStatus(epics, eligibleOnly)
}

func runCloseEligibleEpicsProxiedServer(ctx context.Context, dryRun bool, reason string) error {
	eligibleEpics, err := loadEligibleEpics(ctx)
	if err != nil {
		return err
	}
	if len(eligibleEpics) == 0 {
		return outputNoEligibleEpics(reason)
	}
	if dryRun {
		return outputCloseEligibleDryRun(eligibleEpics, reason)
	}

	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}
	closedIDs, err := closeEligibleEpics(ctx, reason)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	if len(closedIDs) > 0 {
		commandDidWrite.Store(true)
	}
	return outputCloseEligibleResult(closedIDs, reason)
}

func loadEligibleEpics(ctx context.Context) ([]*types.EpicStatus, error) {
	uw, err := openProxiedListUOW(ctx)
	if err != nil {
		return nil, HandleError("%v", err)
	}
	defer uw.Close(ctx)
	epics, err := uw.IssueUseCase().GetEpicsEligibleForClosure(ctx)
	if err != nil {
		return nil, HandleErrorRespectJSON("getting eligible epics: %v", err)
	}
	return filterEligibleEpics(epics), nil
}

func closeEligibleEpics(ctx context.Context, reason string) ([]string, error) {
	return uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) ([]string, string, error) {
		epics, err := uw.IssueUseCase().GetEpicsEligibleForClosure(ctx)
		if err != nil {
			return nil, "", err
		}
		return closeEpicStatuses(ctx, uw, filterEligibleEpics(epics), reason), "epic: close eligible", nil
	})
}

func closeEpicStatuses(ctx context.Context, uw uow.UnitOfWork, epics []*types.EpicStatus, reason string) []string {
	var closed []string
	for _, epicStatus := range epics {
		if _, err := uw.IssueUseCase().CloseIssue(ctx, epicStatus.Epic.ID, domain.CloseIssueParams{Reason: reason}, "system"); err != nil {
			fmt.Fprintf(os.Stderr, "Error closing %s: %v\n", epicStatus.Epic.ID, err)
			continue
		}
		closed = append(closed, epicStatus.Epic.ID)
	}
	return closed
}
