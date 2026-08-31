package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

func newRenameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rename <old-id> <new-id>",
		Short: "Rename an issue ID",
		Long: `Rename an issue from one ID to another.

This updates:
- The issue's primary ID
- All references in other issues (descriptions, titles, notes, etc.)
- Dependencies pointing to/from this issue
- Labels, comments, and events

Examples:
  bd rename bd-w382l bd-dolt     # Rename to memorable ID
  bd rename gt-abc123 gt-auth    # Use descriptive ID

Note: The new ID must use a valid prefix for this database.`,
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runRename,
	}
}

func init() {
	rootCmd.AddCommand(newRenameCmd())
}

func runRename(_ *cobra.Command, args []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("rename is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("rename")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	oldID, newID := args[0], args[1]
	if err := validateRenameIDs(oldID, newID); err != nil {
		return err
	}
	ctx := context.Background()
	if err := ensureStoreActive(); err != nil {
		return HandleError("failed to get storage: %v", err)
	}
	oldIssue, err := issueToRename(ctx, oldID, newID)
	if err != nil {
		return err
	}
	oldIssue.ID = newID
	actor := getActorWithGit()
	if err := getStore().UpdateIssueID(ctx, oldID, newID, oldIssue, actor); err != nil {
		return HandleError("failed to rename issue: %v", err)
	}

	if err := updateReferencesInAllIssues(ctx, getStore(), oldID, newID, actor); err != nil {
		fmt.Printf("Warning: failed to update some references: %v\n", err)
	}

	fmt.Printf("Renamed %s -> %s\n", ui.RenderWarn(oldID), ui.RenderAccent(newID))
	commandDidWrite.Store(true)
	return nil
}

func validateRenameIDs(oldID, newID string) error {
	if oldID == newID {
		return HandleError("old and new IDs are the same")
	}
	if !regexp.MustCompile(`^[a-z]+-[a-zA-Z0-9._-]+$`).MatchString(newID) {
		return HandleError("invalid new ID format %q: must be prefix-suffix (e.g., bd-dolt)", newID)
	}
	return nil
}

func issueToRename(ctx context.Context, oldID, newID string) (*types.Issue, error) {
	issue, err := getStore().GetIssue(ctx, oldID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, HandleError("issue %s not found", oldID)
	}
	if err != nil {
		return nil, HandleError("failed to get issue %s: %v", oldID, err)
	}
	if _, err := getStore().GetIssue(ctx, newID); err == nil {
		return nil, HandleError("issue %s already exists", newID)
	} else if !errors.Is(err, storage.ErrNotFound) {
		return nil, HandleError("failed to check for existing issue: %v", err)
	}
	return issue, nil
}

// updateReferencesInAllIssues updates text references to the old ID in all issues
func updateReferencesInAllIssues(ctx context.Context, store storage.DoltStorage, oldID, newID, actor string) error {
	// Get all issues
	issues, err := store.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return fmt.Errorf("failed to list issues: %w", err)
	}

	// Pattern to match the old ID as a word boundary
	oldPattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(oldID) + `\b`)

	for _, issue := range issues {
		if issue.ID == newID {
			continue // Skip the renamed issue itself
		}

		updates := make(map[string]interface{})
		addReferenceUpdate(updates, "title", issue.Title, oldPattern, newID)
		addReferenceUpdate(updates, "description", issue.Description, oldPattern, newID)
		addReferenceUpdate(updates, "design", issue.Design, oldPattern, newID)
		addReferenceUpdate(updates, "notes", issue.Notes, oldPattern, newID)
		addReferenceUpdate(updates, "acceptance_criteria", issue.AcceptanceCriteria, oldPattern, newID)
		if len(updates) > 0 {
			if err := store.UpdateIssue(ctx, issue.ID, updates, actor); err != nil {
				return fmt.Errorf("failed to update references in %s: %w", issue.ID, err)
			}
		}
	}

	return nil
}

func addReferenceUpdate(updates map[string]interface{}, field, value string, pattern *regexp.Regexp, replacement string) {
	if pattern.MatchString(value) {
		updates[field] = pattern.ReplaceAllString(value, replacement)
	}
}
