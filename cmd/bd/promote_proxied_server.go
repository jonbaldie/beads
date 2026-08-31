package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/jonbaldie/beads/internal/workapi"
)

// runPromoteProxiedServer is the proxied-server route for `bd promote`. It
// mirrors the classic route: resolve the id, refuse a non-wisp with the same
// error text, then promote through the domain PromoteWisp verb — which runs
// the exact issueops implementation the classic route runs, so the row is
// rewritten in place (same id, wisp_type retained, labels/deps/events/
// comments carried to the permanent tables, inbound edges retargeted).
//
// Commit semantics: the cross-plane move and the promotion comment land in
// ONE transaction under one Dolt commit ("bd: promote <id>", the classic
// dolt commit message). The wisp-side deletes touch dolt_ignored tables so
// only the issues-plane rows are versioned by that commit, but the single
// transaction still makes the wisp delete and the issues insert atomic.
//
// One deliberate tightening vs classic: classic adds the promotion comment
// in a separate write after the promote has committed, so a failed comment
// write can only degrade to a stderr warning. Here the comment shares the
// promote's transaction, so a comment failure rolls the whole promote back —
// atomic beats warn-and-continue, and a comment insert on a row this same
// transaction just promoted has no realistic standalone failure mode.
func runPromoteProxiedServer(ctx context.Context, id, reason string) error {
	if getUOWProvider() == nil {
		return HandleErrorRespectJSON("proxied-server UOW provider not initialized")
	}
	fullID, issue, err := resolvePromoteTarget(ctx, id)
	if err != nil {
		return err
	}
	if !issue.Ephemeral {
		return HandleErrorRespectJSON("%s is not a wisp (already persistent)", fullID)
	}
	if err := promoteWisp(ctx, fullID, reason); err != nil {
		return HandleErrorRespectJSON("promoting %s: %v", fullID, err)
	}
	commandDidWrite.Store(true)
	updated := readPromotedIssue(ctx, fullID)
	return printPromoteResult(fullID, updated)
}

func resolvePromoteTarget(ctx context.Context, id string) (string, *types.Issue, error) {
	ruw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return "", nil, err
	}
	defer ruw.Close(ctx)
	fullID, err := utils.ResolvePartialID(ctx, uowMolReader{uw: ruw}, id)
	if err != nil {
		return "", nil, HandleErrorRespectJSON("resolving %s: %v", id, err)
	}
	issue, _, err := workapi.GetIssueOrWisp(ctx, workapi.NewUOWDetailSource(ruw), fullID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", nil, HandleErrorRespectJSON("issue %s not found", fullID)
		}
		return "", nil, HandleErrorRespectJSON("getting issue %s: %v", fullID, err)
	}
	return fullID, issue, nil
}

func promoteWisp(ctx context.Context, fullID, reason string) error {
	return uow.RunTx(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (string, error) {
		if err := uw.IssueUseCase().PromoteWisp(ctx, fullID, getActor()); err != nil {
			return "", err
		}
		if _, err := uw.CommentUseCase().AddCommentToIssue(ctx, fullID, getActor(), promotionComment(reason)); err != nil {
			return "", fmt.Errorf("adding promotion comment: %w", err)
		}
		return fmt.Sprintf("bd: promote %s", fullID), nil
	})
}

func readPromotedIssue(ctx context.Context, fullID string) *types.Issue {
	if !isJSONOutput() {
		return nil
	}
	uwr, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return nil
	}
	defer uwr.Close(ctx)
	updated, _ := uowMolReader{uw: uwr}.GetIssue(ctx, fullID)
	return updated
}
