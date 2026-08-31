package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/workapi"
)

func runEditProxiedServer(cmd *cobra.Command, ctx context.Context, args []string) error {
	fieldToEdit := editFieldFromFlags(cmd)
	issue, err := loadEditProxiedIssue(ctx, args[0])
	if err != nil {
		return err
	}
	currentValue := issueFieldValue(issue, fieldToEdit)
	editor, err := resolveEditEditor()
	if err != nil {
		return err
	}
	return editProxiedIssueWithEditor(ctx, issue, fieldToEdit, currentValue, editor)
}

func editFieldFromFlags(cmd *cobra.Command) string {
	switch {
	case cmd.Flags().Changed("title"):
		return "title"
	case cmd.Flags().Changed("design"):
		return "design"
	case cmd.Flags().Changed("notes"):
		return "notes"
	case cmd.Flags().Changed("acceptance"):
		return "acceptance_criteria"
	default:
		return "description"
	}
}

func loadEditProxiedIssue(ctx context.Context, id string) (*types.Issue, error) {
	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return nil, err
	}
	issue, _, err := workapi.GetIssueOrWisp(ctx, workapi.NewUOWDetailSource(uw), id)
	uw.Close(ctx)
	if err == nil {
		return issue, nil
	}
	if errors.Is(err, storage.ErrNotFound) {
		return nil, HandleErrorRespectJSON("issue %s not found", id)
	}
	return nil, HandleErrorRespectJSON("resolving %s: %v", id, err)
}

func issueFieldValue(issue *types.Issue, fieldToEdit string) string {
	switch fieldToEdit {
	case "title":
		return issue.Title
	case "design":
		return issue.Design
	case "notes":
		return issue.Notes
	case "acceptance_criteria":
		return issue.AcceptanceCriteria
	default:
		return issue.Description
	}
}

func resolveEditEditor() (string, error) {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = firstAvailableEditor()
	}
	if editor == "" {
		return "", HandleErrorRespectJSON("no editor found. Set $EDITOR or $VISUAL environment variable")
	}
	return editor, nil
}

func firstAvailableEditor() string {
	for _, defaultEditor := range []string{"vim", "vi", "nano", "emacs"} {
		if _, err := exec.LookPath(defaultEditor); err == nil {
			return defaultEditor
		}
	}
	return ""
}

func editProxiedIssueWithEditor(ctx context.Context, issue *types.Issue, fieldToEdit, currentValue, editor string) error {
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
	saved, err := applyEditProxiedValue(ctx, issue, fieldToEdit, currentValue, newValue, tmpPath)
	if err != nil {
		return err
	}
	editSaved = saved
	return nil
}

func writeEditTempFile(fieldToEdit, currentValue string) (string, func(), error) {
	tmpFile, err := os.CreateTemp("", fmt.Sprintf("bd-edit-%s-*.txt", fieldToEdit))
	if err != nil {
		return "", nil, HandleErrorRespectJSON("creating temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.WriteString(currentValue); err != nil {
		_ = tmpFile.Close()
		return "", nil, HandleErrorRespectJSON("writing to temp file: %v", err)
	}
	_ = tmpFile.Close()
	return tmpPath, func() { _ = os.Remove(tmpPath) }, nil
}

func runEditEditor(editor, tmpPath string) (string, error) {
	editorParts := strings.Fields(editor)
	editorArgs := append(editorParts[1:], tmpPath)
	editorCmd := exec.Command(editorParts[0], editorArgs...) //nolint:gosec // G204: editor from trusted $EDITOR/$VISUAL env or known defaults
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr
	if err := editorCmd.Run(); err != nil {
		return "", HandleErrorRespectJSON("running editor: %v", err)
	}
	// #nosec G304 -- tmpPath was created earlier in this function
	editedContent, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", HandleErrorRespectJSON("reading edited file: %v", err)
	}
	return strings.TrimSpace(string(editedContent)), nil
}

func applyEditProxiedValue(ctx context.Context, issue *types.Issue, fieldToEdit, currentValue, newValue, tmpPath string) (bool, error) {
	if newValue == currentValue {
		fmt.Println("No changes made")
		return true, nil
	}
	if fieldToEdit == "title" && newValue == "" {
		return false, HandleErrorRespectJSON("title cannot be empty")
	}
	id := issue.ID
	updated, err := proxiedUpdateIssueFields(ctx, id, "bd: edit "+id, map[string]any{fieldToEdit: newValue}, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Your edits are preserved in: %s\n", tmpPath) //nolint:gosec // G705: stderr, not a browser context
		return false, HandleErrorRespectJSON("updating issue: %v", err)
	}
	printEditProxiedResult(issue, updated, fieldToEdit, newValue)
	return true, nil
}

func printEditProxiedResult(issue, updated *types.Issue, fieldToEdit, newValue string) {
	displayTitle := issue.Title
	if fieldToEdit == "title" {
		displayTitle = newValue
	}
	if updated != nil {
		displayTitle = updated.Title
	}
	fieldName := strings.ReplaceAll(fieldToEdit, "_", " ")
	fmt.Printf("%s Updated %s for issue: %s\n", ui.RenderPass("✓"), fieldName, formatFeedbackID(issue.ID, displayTitle))
}
