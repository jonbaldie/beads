package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/uimd"
)

// displayShowIssue displays a single issue (reusable for watch mode).
// Matches the full bd show output: header, metadata, content, labels, deps, comments.
func displayShowIssue(ctx context.Context, issueID string) {
	displayShowIssueReturn(ctx, issueID)
}

// singleIssueSnapshot builds a comparable string from a single issue's state
// so we can detect when the issue has changed between poll cycles.
func singleIssueSnapshot(issue *types.Issue) string {
	return fmt.Sprintf("%s:%s:%d", issue.ID, issue.Status, issue.UpdatedAt.UnixNano())
}

// watchIssue polls for changes to an issue and auto-refreshes the display (GH#654).
// Uses polling instead of fsnotify because Dolt stores data in a server-side
// database, not files — file watchers never fire.
func watchIssue(ctx context.Context, issueID string) {
	// Initial display and snapshot
	issue := displayShowIssueReturn(ctx, issueID)
	if issue == nil {
		return
	}
	lastSnapshot := singleIssueSnapshot(issue)

	fmt.Fprintf(os.Stderr, "\nWatching for changes... (Press Ctrl+C to exit)\n")

	// Handle Ctrl+C — deferred Stop prevents signal handler leak
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	pollInterval := 2 * time.Second
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-sigChan:
			fmt.Fprintf(os.Stderr, "\nStopped watching.\n")
			return
		case <-ticker.C:
			issue := fetchIssue(ctx, issueID)
			if issue == nil {
				continue
			}
			snap := singleIssueSnapshot(issue)
			if snap != lastSnapshot {
				lastSnapshot = snap
				displayShowIssue(ctx, issueID)
				fmt.Fprintf(os.Stderr, "\nWatching for changes... (Press Ctrl+C to exit)\n")
			}
		}
	}
}

// fetchIssue retrieves a single issue by ID, returning nil on error.
func fetchIssue(ctx context.Context, issueID string) *types.Issue {
	result, err := resolveAndGetIssueWithRouting(ctx, getStore(), issueID)
	if result != nil {
		defer result.Close()
	}
	if err != nil || result == nil || result.Issue == nil {
		return nil
	}
	return result.Issue
}

// displayShowIssueReturn displays a single issue and returns it for snapshot use.
func displayShowIssueReturn(ctx context.Context, issueID string) *types.Issue {
	result, err := resolveAndGetIssueWithRouting(ctx, getStore(), issueID)
	if result != nil {
		defer result.Close()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching issue: %v\n", err)
		return nil
	}
	if result == nil || result.Issue == nil {
		fmt.Printf("Issue not found: %s\n", issueID)
		return nil
	}
	renderDisplayedIssue(ctx, result.Store, result.Issue)
	return result.Issue
}

func renderDisplayedIssue(ctx context.Context, issueStore storage.DoltStorage, issue *types.Issue) {
	fmt.Println(formatIssueHeader(issue))
	fmt.Println(formatIssueMetadata(issue))
	renderDisplayedContent(issue)
	renderDisplayedLabels(issueStore, issue)
	renderDisplayedRelations(ctx, issueStore, issue)
	renderDisplayedComments(ctx, issueStore, issue)
	fmt.Println()
}

func renderDisplayedContent(issue *types.Issue) {
	renderDisplayedField("DESCRIPTION", issue.Description)
	renderDisplayedField("DESIGN", issue.Design)
	renderDisplayedField("NOTES", issue.Notes)
	renderDisplayedField("ACCEPTANCE CRITERIA", issue.AcceptanceCriteria)
}

func renderDisplayedField(label, value string) {
	if value != "" {
		fmt.Printf("\n%s\n%s\n", ui.RenderBold(label), uimd.RenderMarkdown(value))
	}
}

func renderDisplayedLabels(issueStore storage.DoltStorage, issue *types.Issue) {
	labels, _ := issueStore.GetLabels(context.Background(), issue.ID)
	if len(labels) > 0 {
		fmt.Printf("\n%s %s\n", ui.RenderBold("LABELS:"), strings.Join(labels, ", "))
	}
}

func renderDisplayedRelations(ctx context.Context, issueStore storage.DoltStorage, issue *types.Issue) {
	relatedSeen := make(map[string]*types.IssueWithDependencyMetadata)
	depsWithMeta, _ := issueStore.GetDependenciesWithMetadata(ctx, issue.ID)
	for _, sec := range groupDepSections(depsWithMeta, true, relatedSeen) {
		printDepSection(sec)
	}
	dependentsWithMeta, _ := issueStore.GetDependentsWithMetadata(ctx, issue.ID)
	for _, sec := range groupDepSections(dependentsWithMeta, false, relatedSeen) {
		printDepSection(sec)
	}
	printRelatedSection(relatedSeen)
}

func renderDisplayedComments(ctx context.Context, issueStore storage.DoltStorage, issue *types.Issue) {
	comments, _ := issueStore.GetIssueComments(ctx, issue.ID)
	if len(comments) == 0 {
		return
	}
	fmt.Printf("\n%s\n", ui.RenderBold("COMMENTS"))
	for _, comment := range comments {
		fmt.Printf("  %s %s\n", ui.RenderMuted(comment.CreatedAt.UTC().Format("2006-01-02 15:04")), comment.Author)
		rendered := uimd.RenderMarkdown(comment.Text)
		for _, line := range strings.Split(strings.TrimRight(rendered, "\n"), "\n") {
			fmt.Printf("    %s\n", line)
		}
	}
}
