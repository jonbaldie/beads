package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/workapi"
)

func todoListProxied(ctx context.Context, filter types.IssueFilter) ([]*types.Issue, error) {
	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return nil, err
	}
	defer uw.Close(ctx)

	page, err := uw.IssueUseCase().SearchIssues(ctx, "", filter)
	if err != nil {
		return nil, HandleError("failed to list TODOs: %v", err)
	}
	return page.Items, nil
}

func runTodoAddProxiedServer(cmd *cobra.Command, ctx context.Context, args []string) error {
	title := strings.Join(args, " ")

	priority, _ := cmd.Flags().GetInt("priority")
	description, _ := cmd.Flags().GetString("description")

	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}

	issue := &types.Issue{
		IssueContent: types.IssueContent{
			Title:       title,
			Description: description,
		},
		IssueWorkflow: types.IssueWorkflow{
			Priority:  priority,
			IssueType: types.TypeTask,
			Status:    types.StatusOpen,
			Assignee:  getActorWithGit(),
			Owner:     getOwner(),
		},
		IssueTimes: types.IssueTimes{
			CreatedBy: getActorWithGit(),
		},
	}

	res, err := uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (*types.Issue, string, error) {
		result, err := uw.IssueUseCase().CreateIssue(ctx, domain.CreateIssueParams{Issue: issue}, getActorWithGit())
		if err != nil {
			return nil, "", err
		}
		return result.Issue, fmt.Sprintf("bd: create %s", result.Issue.ID), nil
	})
	if err != nil {
		return HandleError("failed to create TODO: %v", err)
	}
	commandDidWrite.Store(true)

	if isJSONOutput() {
		data, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return HandleError("failed to marshal JSON: %v", err)
		}
		fmt.Println(string(data))
		return nil
	}
	fmt.Printf("Created %s: %s\n", ui.RenderID(res.ID), res.Title)
	return nil
}

func runTodoDoneProxiedServer(cmd *cobra.Command, ctx context.Context, args []string) error {
	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}
	reason := todoDoneReason(cmd)

	var closedIDs []string
	for _, issueID := range args {
		_, err := closeTodoProxiedIssue(ctx, issueID, reason)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: failed to close %s: %v\n", issueID, err)
			continue
		}
		closedIDs = append(closedIDs, issueID)
	}

	if len(closedIDs) > 0 {
		commandDidWrite.Store(true)
	}

	return renderTodoDoneResult(closedIDs, reason)
}

func todoDoneReason(cmd *cobra.Command) string {
	reason, _ := cmd.Flags().GetString("reason")
	if reason == "" {
		return "Completed"
	}
	return reason
}

func closeTodoProxiedIssue(ctx context.Context, issueID, reason string) (struct{}, error) {
	return uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (struct{}, string, error) {
		issue, isWisp, err := workapi.GetIssueOrWisp(ctx, workapi.NewUOWDetailSource(uw), issueID)
		if errors.Is(err, storage.ErrNotFound) {
			return struct{}{}, "", fmt.Errorf("issue %s not found", issueID)
		}
		if err != nil {
			return struct{}{}, "", fmt.Errorf("resolving %s: %w", issueID, err)
		}
		params := domain.CloseIssueParams{Reason: reason}
		if isWisp {
			_, err = uw.IssueUseCase().CloseWisp(ctx, issue.ID, params, getActorWithGit())
		} else {
			_, err = uw.IssueUseCase().CloseIssue(ctx, issue.ID, params, getActorWithGit())
		}
		if err != nil {
			return struct{}{}, "", err
		}
		return struct{}{}, fmt.Sprintf("bd: close %s", issue.ID), nil
	})
}

func renderTodoDoneResult(closedIDs []string, reason string) error {
	if !isJSONOutput() {
		for _, id := range closedIDs {
			fmt.Printf("Closed %s\n", ui.RenderID(id))
		}
		return nil
	}
	data, err := json.MarshalIndent(map[string]interface{}{"closed": closedIDs, "reason": reason}, "", "  ")
	if err != nil {
		return HandleError("failed to marshal JSON: %v", err)
	}
	fmt.Println(string(data))
	return nil
}
