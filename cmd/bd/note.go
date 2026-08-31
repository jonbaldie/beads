package main

import (
	"fmt"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

// validateNoteArgs runs as cobra's Args validation for "note", before RunE
// and (on the local/embedded path) before the id ever reaches
// ResolvePartialID's fuzzy/substring matching. Mirror of validateCommentArgs
// (#5369): "note" has no subcommands of its own — its only job is "append a
// note to <id>" — so when the id positional argument is exactly a reserved
// word that is a common subcommand-shaped typo, the near-certain explanation
// is not that a bead is genuinely named that word (real ids always carry a
// prefix+hyphen — see looksLikePrefixedID). Left unguarded, that word
// silently resolves via ResolvePartialID's substring/prefix fallback to
// whatever existing bead or wisp id happens to contain it, and the note
// lands on the WRONG issue with no error. (GH#5370)
//
// Denylist is broader than #5369's {list, add}: "show" is invited by this
// guard's own error text ("bd show <issue-id>"); "update" is invited by the
// guard's own hints referencing "bd update <issue-id>" four times; and
// edit/rm/remove/delete are common subcommand-shaped typos for mutating
// commands. Broader-than-#5369 matches the in-repo direction of PR #5393
// for the comment sibling.
func validateNoteArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.MinimumNArgs(1)(cmd, args); err != nil {
		return err
	}
	switch args[0] {
	case "list":
		return HandleErrorRespectJSON(`"bd note list ..." is not valid — "note" takes an issue id first and has no "list" subcommand.

To append a note:
  bd note <issue-id> "text"

To read notes on an issue:
  bd show <issue-id>

See: bd note --help`)
	case "add":
		return HandleErrorRespectJSON(`"bd note add ..." is not valid — "note" already means "append a note" and takes an issue id first, not the word "add".

To append a note:
  bd note <issue-id> "text"

(Equivalent long form: bd update <issue-id> --append-notes "text".)

See: bd note --help`)
	case "show":
		return HandleErrorRespectJSON(`"bd note show ..." is not valid — "note" takes an issue id first and has no "show" subcommand.

To read notes on an issue:
  bd show <issue-id>

To append a note:
  bd note <issue-id> "text"

See: bd note --help`)
	case "edit":
		return HandleErrorRespectJSON(`"bd note edit ..." is not valid — "note" only appends and has no "edit" subcommand.

To append a note:
  bd note <issue-id> "text"

To change an issue field:
  bd update <issue-id> --notes "text"

See: bd note --help`)
	case "rm", "remove", "delete":
		return HandleErrorRespectJSON(`"bd note %s ..." is not valid — "note" only appends and has no "%s" subcommand.

To append a note:
  bd note <issue-id> "text"

To change or clear notes:
  bd update <issue-id> --notes "text"

See: bd note --help`, args[0], args[0])
	case "update":
		return HandleErrorRespectJSON(`"bd note update ..." is not valid — "note" takes an issue id first and has no "update" subcommand.

To append a note:
  bd note <issue-id> "text"

To set or append notes via the long form:
  bd update <issue-id> --notes "text"
  bd update <issue-id> --append-notes "text"

See: bd note --help`)
	}
	return nil
}

func newNoteCmd() *cobra.Command {
	noteCmd := &cobra.Command{
		Use:     "note <id> [text...]",
		GroupID: "issues",
		Short:   "Append a note to an issue",
		Long: `Append a note to an issue's notes field.

Shorthand for 'bd update <id> --append-notes "text"'.

Examples:
  bd note gt-abc "Fixed the flaky test"
  bd note gt-abc Fixed the flaky test
  echo "note from pipe" | bd note gt-abc --stdin
  bd note gt-abc --file notes.txt

Note: "note" has NO subcommands — it only appends.
To read notes on an issue, use: bd show <id>`,
		Args:          validateNoteArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runNote,
	}
	registerTextSourceFlags(noteCmd, "note text")
	noteCmd.ValidArgsFunction = issueIDCompletion
	return noteCmd
}

func runNote(cmd *cobra.Command, args []string) error {
	if err := CheckReadonly("note"); err != nil {
		return err
	}
	evt := metrics.NewCommandEvent("note")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	id := args[0]
	noteText, err := requireTextFromSources("note text", "use positional args, --stdin, or --file",
		cmdTextSources(cmd, args[1:]))
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	if usesProxiedServer() {
		return runNoteProxiedServer(getRootContext(), id, noteText)
	}
	return runNoteDirect(id, noteText)
}

func runNoteDirect(id, noteText string) error {
	result, err := resolveIssueMutationForCommand(id)
	if err != nil {
		return err
	}
	defer result.Close()
	if err := validateIssueUpdatable(id, result.Issue); err != nil {
		return HandleErrorRespectJSON("%s", err)
	}
	updates := map[string]interface{}{issueops.OpAppendNotes: noteText}
	if err := result.Store.UpdateIssue(getRootContext(), result.ResolvedID, updates, getActor()); err != nil {
		return HandleErrorRespectJSON("updating %s: %v", id, err)
	}
	if err := commitPendingIfEmbedded(getRootContext(), result.Store, getActor(), doltAutoCommitParams{
		Command:  "note",
		IssueIDs: []string{result.ResolvedID},
	}); err != nil {
		return HandleErrorRespectJSON("failed to commit: %v", err)
	}
	SetLastTouchedID(result.ResolvedID)
	return reportNoteResult(result)
}

func reportNoteResult(result *RoutedResult) error {
	updatedIssue, _ := result.Store.GetIssue(getRootContext(), result.ResolvedID)
	if isJSONOutput() {
		if updatedIssue != nil {
			return outputJSON(updatedIssue)
		}
		return nil
	}
	title := ""
	if updatedIssue != nil {
		title = updatedIssue.Title
	}
	fmt.Printf("%s Note added to %s\n", ui.RenderPass("✓"), formatFeedbackID(result.ResolvedID, title))
	return nil
}

func init() {
	rootCmd.AddCommand(newNoteCmd())
}
