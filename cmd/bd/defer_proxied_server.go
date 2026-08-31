package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/workapi"
)

type deferProxiedResult struct {
	issues []*types.Issue
	errs   []string
}

func proxiedUpdateByID(ctx context.Context, uw uow.UnitOfWork, id string, isWisp bool, updates map[string]any) error {
	if isWisp {
		return uw.IssueUseCase().UpdateWisp(ctx, id, updates, getActor())
	}
	return uw.IssueUseCase().UpdateIssue(ctx, id, updates, getActor())
}

func proxiedGetByID(ctx context.Context, uw uow.UnitOfWork, id string, isWisp bool) *types.Issue {
	if isWisp {
		iss, _ := uw.IssueUseCase().GetWisp(ctx, id)
		return iss
	}
	iss, _ := uw.IssueUseCase().GetIssue(ctx, id)
	return iss
}

func runDeferProxiedServer(ctx context.Context, args []string, deferUntil *time.Time, reason string) error {
	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}
	res, err := uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (deferProxiedResult, string, error) {
		return deferProxiedTx(ctx, uw, args, deferUntil, reason)
	})
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	return printDeferProxiedResult(res, args, "Deferred", ui.RenderAccent("*"))
}

func deferProxiedTx(ctx context.Context, uw uow.UnitOfWork, args []string, deferUntil *time.Time, reason string) (deferProxiedResult, string, error) {
	var r deferProxiedResult
	for _, id := range args {
		deferOneProxiedIssue(ctx, uw, id, deferUntil, reason, &r)
	}
	if len(r.issues) == 0 {
		return r, "", nil
	}
	return r, "bd: defer", nil
}

func deferOneProxiedIssue(ctx context.Context, uw uow.UnitOfWork, id string, deferUntil *time.Time, reason string, r *deferProxiedResult) {
	issue, isWisp, rerr := workapi.GetIssueOrWisp(ctx, workapi.NewUOWDetailSource(uw), id)
	if errors.Is(rerr, storage.ErrNotFound) {
		r.errs = append(r.errs, fmt.Sprintf("Error resolving %s: not found", id))
		return
	}
	if rerr != nil {
		r.errs = append(r.errs, fmt.Sprintf("Error resolving %s: %v", id, rerr))
		return
	}
	fullID := issue.ID
	updates := map[string]interface{}{"status": string(types.StatusDeferred)}
	if deferUntil != nil {
		updates["defer_until"] = *deferUntil
	}
	if reason != "" {
		notes := issue.Notes
		if notes != "" {
			notes += "\n"
		}
		updates["notes"] = notes + reason
	}
	if uerr := proxiedUpdateByID(ctx, uw, fullID, isWisp, updates); uerr != nil {
		r.errs = append(r.errs, fmt.Sprintf("Error deferring %s: %v", fullID, uerr))
		return
	}
	if updated := proxiedGetByID(ctx, uw, fullID, isWisp); updated != nil {
		r.issues = append(r.issues, updated)
	}
}

func runUndeferProxiedServer(ctx context.Context, args []string) error {
	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}
	res, err := uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (deferProxiedResult, string, error) {
		return undeferProxiedTx(ctx, uw, args)
	})
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	return printDeferProxiedResult(res, args, "Undeferred", ui.RenderPass("*"))
}

func undeferProxiedTx(ctx context.Context, uw uow.UnitOfWork, args []string) (deferProxiedResult, string, error) {
	var r deferProxiedResult
	for _, id := range args {
		undeferOneProxiedIssue(ctx, uw, id, &r)
	}
	if len(r.issues) == 0 {
		return r, "", nil
	}
	return r, "bd: undefer", nil
}

func undeferOneProxiedIssue(ctx context.Context, uw uow.UnitOfWork, id string, r *deferProxiedResult) {
	issue, isWisp, rerr := workapi.GetIssueOrWisp(ctx, workapi.NewUOWDetailSource(uw), id)
	if errors.Is(rerr, storage.ErrNotFound) {
		r.errs = append(r.errs, fmt.Sprintf("Error getting %s: not found", id))
		return
	}
	if rerr != nil {
		r.errs = append(r.errs, fmt.Sprintf("Error getting %s: %v", id, rerr))
		return
	}
	fullID := issue.ID
	if issue.Status != types.StatusDeferred {
		r.errs = append(r.errs, fmt.Sprintf("%s is not deferred (status: %s)", fullID, string(issue.Status)))
		return
	}
	updates := map[string]interface{}{
		"status":      string(types.StatusOpen),
		"defer_until": nil,
	}
	if uerr := proxiedUpdateByID(ctx, uw, fullID, isWisp, updates); uerr != nil {
		r.errs = append(r.errs, fmt.Sprintf("Error undeferring %s: %v", fullID, uerr))
		return
	}
	if updated := proxiedGetByID(ctx, uw, fullID, isWisp); updated != nil {
		r.issues = append(r.issues, updated)
	}
}

func printDeferProxiedResult(res deferProxiedResult, args []string, verb, mark string) error {
	for _, e := range res.errs {
		fmt.Fprintln(os.Stderr, e)
	}
	if isJSONOutput() {
		if len(res.issues) > 0 {
			if e := outputJSON(res.issues); e != nil {
				return e
			}
		}
	} else {
		suffix := ""
		if verb == "Undeferred" {
			suffix = " (now open)"
		}
		for _, iss := range res.issues {
			fmt.Printf("%s %s %s%s\n", mark, verb, iss.ID, suffix)
		}
	}
	if len(args) > 0 {
		commandDidWrite.Store(true)
	}
	return nil
}
