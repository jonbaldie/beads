package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

func newEditCmd() *cobra.Command {
	editCmd := &cobra.Command{
		Use:     "edit [id]",
		GroupID: "issues",
		Short:   "Edit an issue field in $EDITOR",
		Long: `Edit an issue field using your configured $EDITOR.

By default, edits the description. Use flags to edit other fields.

Examples:
  bd edit bd-42                    # Edit description
  bd edit bd-42 --title            # Edit title
  bd edit bd-42 --design           # Edit design notes
  bd edit bd-42 --notes            # Edit notes
  bd edit bd-42 --acceptance       # Edit acceptance criteria`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runEdit,
	}
	editCmd.Flags().Bool("title", false, "Edit the title")
	editCmd.Flags().Bool("description", false, "Edit the description (default)")
	editCmd.Flags().Bool("design", false, "Edit the design notes")
	editCmd.Flags().Bool("notes", false, "Edit the notes")
	editCmd.Flags().Bool("acceptance", false, "Edit the acceptance criteria")
	editCmd.ValidArgsFunction = issueIDCompletion
	return editCmd
}

func runEdit(cmd *cobra.Command, args []string) error {
	if err := CheckReadonly("edit"); err != nil {
		return err
	}
	evt := metrics.NewCommandEvent("edit")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	if usesProxiedServer() {
		return runEditProxiedServer(cmd, getRootContext(), args)
	}
	return runDirectEdit(cmd, getRootContext(), args[0])
}

func runDirectEdit(cmd *cobra.Command, ctx context.Context, requestedID string) error {
	result, err := resolveAndGetIssueForMutation(ctx, getStore(), requestedID)
	if err != nil {
		return HandleErrorRespectJSON("resolving %s: %v", requestedID, err)
	}
	defer result.Close()
	fieldToEdit := editFieldFromFlags(cmd)
	currentValue := issueFieldValue(result.Issue, fieldToEdit)
	editor, err := resolveEditEditor()
	if err != nil {
		return err
	}
	return editDirectIssue(ctx, result, fieldToEdit, currentValue, editor)
}

func editDirectIssue(ctx context.Context, result *RoutedResult, fieldToEdit, currentValue, editor string) error {
	tmpPath, cleanup, err := writeEditTempFile(fieldToEdit, currentValue)
	if err != nil {
		return err
	}
	editSaved := false
	defer func() {
		if editSaved {
			cleanup()
		}
	}()
	newValue, err := runEditEditor(editor, tmpPath)
	if err != nil {
		return err
	}
	editSaved, err = applyDirectEditValue(ctx, result, fieldToEdit, currentValue, newValue, tmpPath)
	return err
}

func applyDirectEditValue(ctx context.Context, result *RoutedResult, fieldToEdit, currentValue, newValue, tmpPath string) (bool, error) {
	if newValue == currentValue {
		fmt.Println("No changes made")
		return true, nil
	}
	if fieldToEdit == "title" && newValue == "" {
		return false, HandleErrorRespectJSON("title cannot be empty")
	}
	updates := map[string]interface{}{fieldToEdit: newValue}
	if err := updateEditedIssue(ctx, result.Store, result.ResolvedID, updates); err != nil {
		fmt.Fprintf(os.Stderr, "Your edits are preserved in: %s\n", tmpPath)
		return false, HandleErrorRespectJSON("updating issue: %v", err)
	}
	if err := commitPendingIfEmbedded(ctx, result.Store, getActor(), doltAutoCommitParams{
		Command:  "edit",
		IssueIDs: []string{result.ResolvedID},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Your edits are preserved in: %s\n", tmpPath)
		return false, HandleErrorRespectJSON("failed to commit: %v", err)
	}
	printDirectEditResult(result.Issue, result.ResolvedID, fieldToEdit, newValue)
	return true, nil
}

func updateEditedIssue(ctx context.Context, issueStore storage.DoltStorage, id string, updates map[string]interface{}) error {
	err := issueStore.UpdateIssue(ctx, id, updates, getActor())
	if err == nil {
		return nil
	}
	if accessor, ok := storage.UnwrapStore(issueStore).(storage.RawDBAccessor); ok {
		if pingErr := accessor.DB().PingContext(ctx); pingErr != nil {
			accessor.DB().SetConnMaxIdleTime(0)
			_ = accessor.DB().PingContext(ctx)
		}
	}
	return issueStore.UpdateIssue(ctx, id, updates, getActor())
}

func printDirectEditResult(issue *types.Issue, id, fieldToEdit, newValue string) {
	displayTitle := issue.Title
	if fieldToEdit == "title" {
		displayTitle = newValue
	}
	fieldName := strings.ReplaceAll(fieldToEdit, "_", " ")
	fmt.Printf("%s Updated %s for issue: %s\n", ui.RenderPass("✓"), fieldName, formatFeedbackID(id, displayTitle))
}

func init() {
	rootCmd.AddCommand(newEditCmd())
}
