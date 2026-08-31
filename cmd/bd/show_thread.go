package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
)

// showMessageThread displays a full conversation thread for a message
func showMessageThread(ctx context.Context, messageID string, jsonOutput bool) error {
	startMsg, err := getStore().GetIssue(ctx, messageID)
	if err != nil {
		return HandleError("fetching message %s: %v", messageID, err)
	}
	if startMsg == nil {
		return HandleError("message %s not found", messageID)
	}
	rootMsg := findMessageThreadRoot(ctx, startMsg)
	threadMessages, repliesTo := collectMessageThread(ctx, rootMsg)
	slices.SortFunc(threadMessages, func(a, b *types.Issue) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})
	if jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(threadMessages)
	}
	printMessageThread(rootMsg, threadMessages, repliesTo)
	fmt.Printf("Total: %d messages in thread\n\n", len(threadMessages))
	return nil
}

func findMessageThreadRoot(ctx context.Context, startMsg *types.Issue) *types.Issue {
	// Find the root of the thread by following replies-to dependencies upward
	// Per Decision 004, RepliesTo is now stored as a dependency, not an Issue field
	rootMsg := startMsg
	seen := map[string]bool{rootMsg.ID: true}
	for {
		parentID := findRepliesTo(ctx, rootMsg.ID, getStore())
		if parentID == "" || seen[parentID] {
			return rootMsg
		}
		seen[parentID] = true
		parentMsg, _ := getStore().GetIssue(ctx, parentID)
		if parentMsg == nil {
			return rootMsg
		}
		rootMsg = parentMsg
	}
}

func collectMessageThread(ctx context.Context, rootMsg *types.Issue) ([]*types.Issue, map[string]string) {
	threadMessages := []*types.Issue{rootMsg}
	threadIDs := map[string]bool{rootMsg.ID: true}
	repliesTo := map[string]string{}
	queue := []string{rootMsg.ID}
	for {
		if len(queue) == 0 {
			break
		}
		currentID := queue[0]
		queue = queue[1:]
		for _, reply := range findReplies(ctx, currentID, getStore()) {
			if threadIDs[reply.ID] {
				continue
			}
			threadMessages = append(threadMessages, reply)
			threadIDs[reply.ID] = true
			repliesTo[reply.ID] = currentID
			queue = append(queue, reply.ID)
		}
	}
	return threadMessages, repliesTo
}

func printMessageThread(rootMsg *types.Issue, threadMessages []*types.Issue, repliesTo map[string]string) {
	fmt.Printf("\n%s Thread: %s\n", ui.RenderAccent("📬"), rootMsg.Title)
	fmt.Println(strings.Repeat("─", 66))
	for _, msg := range threadMessages {
		printMessageThreadEntry(msg, repliesTo)
	}
}

func printMessageThreadEntry(msg *types.Issue, repliesTo map[string]string) {
	depth := 0
	parent := repliesTo[msg.ID]
	for parent != "" && depth < 5 {
		depth++
		parent = repliesTo[parent]
	}
	indent := strings.Repeat("  ", depth)
	timeStr := msg.CreatedAt.Format("2006-01-02 15:04")
	statusIcon := "📧"
	if msg.Status == types.StatusClosed {
		statusIcon = "✓"
	}
	fmt.Printf("%s%s %s %s\n", indent, statusIcon, ui.RenderAccent(msg.ID), ui.RenderMuted(timeStr))
	fmt.Printf("%s  From: %s  To: %s\n", indent, msg.Sender, msg.Assignee)
	if parentID := repliesTo[msg.ID]; parentID != "" {
		fmt.Printf("%s  Re: %s\n", indent, parentID)
	}
	fmt.Printf("%s  %s: %s\n", indent, ui.RenderMuted("Subject"), msg.Title)
	if msg.Description != "" {
		for _, line := range strings.Split(msg.Description, "\n") {
			fmt.Printf("%s  %s\n", indent, line)
		}
	}
	fmt.Println()
}

// findRepliesTo finds the parent ID that this issue replies to via replies-to dependency.
// Returns empty string if no parent found.
func findRepliesTo(ctx context.Context, issueID string, store storage.DoltStorage) string {
	deps, err := store.GetDependencyRecords(ctx, issueID)
	if err != nil {
		return ""
	}
	for _, dep := range deps {
		if dep.Type == types.DepRepliesTo {
			return dep.DependsOnID
		}
	}
	return ""
}

// findReplies finds all issues that reply to this issue via replies-to dependency.
func findReplies(ctx context.Context, issueID string, store storage.DoltStorage) []*types.Issue {
	deps, err := store.GetDependentsWithMetadata(ctx, issueID)
	if err != nil {
		return nil
	}
	var replies []*types.Issue
	for _, dep := range deps {
		if dep.DependencyType == types.DepRepliesTo {
			issue := dep.Issue // Copy to avoid aliasing
			replies = append(replies, &issue)
		}
	}
	return replies
}
