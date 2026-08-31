package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/timeparsing"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/jonbaldie/beads/internal/validation"
)

type updateInput struct {
	fields           map[string]any
	addLabels        []string
	removeLabels     []string
	setLabels        *[]string
	reparent         *string
	claim            bool
	appendNotes      string
	hasAppendNotes   bool
	setMetadata      []string
	unsetMetadata    []string
	mergeMetadataIn  json.RawMessage
	clearDeferStatus bool
	// bd-wsqvw conditional-update guards; non-nil only when the flag was
	// explicitly passed (a pointer to "" is the real "expected unassigned"
	// guard).
	ifAssignee *string
	ifStatus   *string
	// bd-98s5c: --force bypasses the live-claim reassign fence (mutually
	// exclusive with --if-assignee at the flag-group level).
	force bool
}

func runUpdateInputStages(stages ...func() error) error {
	for _, stage := range stages {
		if err := stage(); err != nil {
			return err
		}
	}
	return nil
}

func gatherUpdateInput(ctx context.Context, cmd *cobra.Command) (*updateInput, error) {
	in := &updateInput{fields: map[string]any{}}
	err := runUpdateInputStages(
		func() error { return gatherUpdateStatus(ctx, cmd, in) },
		func() error { return gatherUpdateBasicFields(cmd, in) },
		func() error { return gatherUpdateDescriptionAndDesign(cmd, in) },
		func() error { return gatherUpdateNotesAndAcceptance(cmd, in) },
		func() error { return gatherUpdateReferences(cmd, in) },
		func() error { return gatherUpdateLabelsAndRelations(cmd, in) },
		func() error { return gatherUpdateSchedule(cmd, in) },
		func() error { return gatherUpdatePersistence(cmd, in) },
		func() error { return gatherUpdateMetadata(cmd, in) },
		func() error { return gatherUpdateGuards(ctx, cmd, in) },
	)
	if err != nil {
		return nil, err
	}
	return in, nil
}

func gatherUpdateStatus(ctx context.Context, cmd *cobra.Command, in *updateInput) error {
	if cmd.Flags().Changed("status") {
		status, _ := cmd.Flags().GetString("status")
		if err := validateUpdateStatus(ctx, status); err != nil {
			return err
		}
		in.fields["status"] = status
		if status == "closed" {
			session, _ := cmd.Flags().GetString("session")
			if session == "" {
				session = os.Getenv("CLAUDE_SESSION_ID")
			}
			if session != "" {
				in.fields["closed_by_session"] = session
			}
		}
	}
	return nil
}

func gatherUpdateBasicFields(cmd *cobra.Command, in *updateInput) error {
	if cmd.Flags().Changed("priority") {
		priorityStr, _ := cmd.Flags().GetString("priority")
		priority, err := validation.ValidatePriority(priorityStr)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		in.fields["priority"] = priority
	}
	if cmd.Flags().Changed("title") {
		title, _ := cmd.Flags().GetString("title")
		title = strings.TrimSpace(title)
		if title == "" {
			return HandleErrorRespectJSON("title cannot be empty")
		}
		in.fields["title"] = title
	}
	if cmd.Flags().Changed("assignee") {
		assignee, _ := cmd.Flags().GetString("assignee")
		in.fields["assignee"] = assignee
	}
	in.force, _ = cmd.Flags().GetBool("force")
	return nil
}

func gatherUpdateDescriptionAndDesign(cmd *cobra.Command, in *updateInput) error {
	description, descChanged, err := getDescriptionFlag(cmd)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	if descChanged {
		if err := validateDescriptionUpdate(cmd, description, descChanged); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		in.fields["description"] = description
	}
	design, designChanged, err := getDesignFlag(cmd)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	if designChanged {
		in.fields["design"] = design
	}
	return nil
}

func gatherUpdateNotesAndAcceptance(cmd *cobra.Command, in *updateInput) error {
	if cmd.Flags().Changed("notes") && cmd.Flags().Changed("append-notes") {
		return HandleErrorRespectJSON("cannot specify both --notes and --append-notes")
	}
	if cmd.Flags().Changed("notes") {
		notes, _ := cmd.Flags().GetString("notes")
		in.fields["notes"] = notes
	}
	if cmd.Flags().Changed("append-notes") {
		in.appendNotes, _ = cmd.Flags().GetString("append-notes")
		in.hasAppendNotes = true
	}
	if cmd.Flags().Changed("acceptance") || cmd.Flags().Changed("acceptance-criteria") {
		var ac string
		if cmd.Flags().Changed("acceptance") {
			ac, _ = cmd.Flags().GetString("acceptance")
		} else {
			ac, _ = cmd.Flags().GetString("acceptance-criteria")
		}
		in.fields["acceptance_criteria"] = ac
	}
	return nil
}

func gatherUpdateReferences(cmd *cobra.Command, in *updateInput) error {
	if cmd.Flags().Changed("external-ref") {
		externalRef, _ := cmd.Flags().GetString("external-ref")
		if externalRef == "" {
			in.fields["external_ref"] = nil
		} else {
			in.fields["external_ref"] = externalRef
		}
	}
	if cmd.Flags().Changed("spec-id") {
		specID, _ := cmd.Flags().GetString("spec-id")
		in.fields["spec_id"] = specID
	}
	if cmd.Flags().Changed("estimate") {
		estimate, _ := cmd.Flags().GetInt("estimate")
		if estimate < 0 {
			return HandleErrorRespectJSON("estimate must be a non-negative number of minutes")
		}
		in.fields["estimated_minutes"] = estimate
	}
	if cmd.Flags().Changed("type") {
		issueType, _ := cmd.Flags().GetString("type")
		in.fields["issue_type"] = utils.NormalizeIssueType(issueType)
	}
	return nil
}

func gatherUpdateLabelsAndRelations(cmd *cobra.Command, in *updateInput) error {
	if cmd.Flags().Changed("add-label") {
		in.addLabels, _ = cmd.Flags().GetStringSlice("add-label")
	}
	if cmd.Flags().Changed("remove-label") {
		in.removeLabels, _ = cmd.Flags().GetStringSlice("remove-label")
	}
	if cmd.Flags().Changed("set-labels") {
		labels, _ := cmd.Flags().GetStringSlice("set-labels")
		in.setLabels = &labels
	}
	if cmd.Flags().Changed("parent") {
		parent, _ := cmd.Flags().GetString("parent")
		in.reparent = &parent
	}
	if cmd.Flags().Changed("await-id") {
		awaitID, _ := cmd.Flags().GetString("await-id")
		in.fields["await_id"] = awaitID
	}
	return nil
}

func gatherUpdateSchedule(cmd *cobra.Command, in *updateInput) error {
	return runUpdateInputStages(
		func() error { return gatherUpdateDue(cmd, in) },
		func() error { return gatherUpdateDefer(cmd, in) },
	)
}

func gatherUpdateDue(cmd *cobra.Command, in *updateInput) error {
	if !cmd.Flags().Changed("due") {
		return nil
	}
	dueStr, _ := cmd.Flags().GetString("due")
	if dueStr == "" {
		in.fields["due_at"] = nil
		return nil
	}
	t, err := timeparsing.ParseRelativeTime(dueStr, time.Now())
	if err != nil {
		return HandleErrorRespectJSON("invalid --due format %q. Examples: +6h, tomorrow, next monday, 2025-01-15", dueStr)
	}
	in.fields["due_at"] = t
	return nil
}

func gatherUpdateDefer(cmd *cobra.Command, in *updateInput) error {
	if !cmd.Flags().Changed("defer") {
		return nil
	}
	deferStr, _ := cmd.Flags().GetString("defer")
	jsonOut, _ := cmd.Flags().GetBool("json")
	if deferStr == "" {
		in.fields["defer_until"] = nil
		if _, ok := in.fields["status"]; !ok {
			in.clearDeferStatus = true
		}
		return nil
	}
	t, err := timeparsing.ParseRelativeTime(deferStr, time.Now())
	if err != nil {
		return HandleErrorRespectJSON("invalid --defer format %q. Examples: +1h, tomorrow, next monday, 2025-01-15", deferStr)
	}
	inPast := t.Before(time.Now())
	if inPast && !jsonOut {
		fmt.Fprintf(os.Stderr, "%s Defer date %q is in the past. Issue will appear in bd ready immediately.\n",
			ui.RenderWarn("!"), t.Local().Format("2006-01-02 15:04"))
		fmt.Fprintf(os.Stderr, "  Did you mean a future date? Use --defer=+1h or --defer=tomorrow\n")
	}
	in.fields["defer_until"] = t
	if _, ok := in.fields["status"]; !ok && !inPast {
		in.fields["status"] = string(types.StatusDeferred)
	}
	return nil
}

func gatherUpdatePersistence(cmd *cobra.Command, in *updateInput) error {
	if err := validateUpdatePersistenceFlags(cmd); err != nil {
		return err
	}
	if cmd.Flags().Changed("ephemeral") {
		in.fields["wisp"] = true
	}
	if cmd.Flags().Changed("persistent") {
		in.fields["wisp"] = false
	}
	if cmd.Flags().Changed("no-history") {
		in.fields["no_history"] = true
	}
	if cmd.Flags().Changed("history") {
		in.fields["no_history"] = false
	}
	return nil
}

func validateUpdatePersistenceFlags(cmd *cobra.Command) error {
	ephemeralChanged := cmd.Flags().Changed("ephemeral")
	persistentChanged := cmd.Flags().Changed("persistent")
	noHistoryChanged := cmd.Flags().Changed("no-history")
	historyChanged := cmd.Flags().Changed("history")
	if ephemeralChanged && persistentChanged {
		return HandleErrorRespectJSON("cannot specify both --ephemeral and --persistent flags")
	}
	if noHistoryChanged && ephemeralChanged {
		return HandleErrorRespectJSON("cannot specify both --no-history and --ephemeral flags")
	}
	if noHistoryChanged && historyChanged {
		return HandleErrorRespectJSON("cannot specify both --no-history and --history flags")
	}
	return nil
}

func gatherUpdateMetadata(cmd *cobra.Command, in *updateInput) error {
	if cmd.Flags().Changed("metadata") {
		metadataValue, _ := cmd.Flags().GetString("metadata")
		var metadataJSON string
		if strings.HasPrefix(metadataValue, "@") {
			filePath := metadataValue[1:]
			data, err := os.ReadFile(filePath) //#nosec G304 -- user-supplied path via @file syntax
			if err != nil {
				return HandleErrorRespectJSON("failed to read metadata file %s: %v", filePath, err)
			}
			metadataJSON = string(data)
		} else {
			metadataJSON = metadataValue
		}
		if !json.Valid([]byte(metadataJSON)) {
			return HandleErrorRespectJSON("invalid JSON in --metadata: must be valid JSON")
		}
		in.mergeMetadataIn = json.RawMessage(metadataJSON)
	}
	setMetadataFlags, _ := cmd.Flags().GetStringArray("set-metadata")
	unsetMetadataFlags, _ := cmd.Flags().GetStringArray("unset-metadata")
	if (len(setMetadataFlags) > 0 || len(unsetMetadataFlags) > 0) && cmd.Flags().Changed("metadata") {
		return HandleErrorRespectJSON("cannot combine --metadata with --set-metadata or --unset-metadata")
	}
	in.setMetadata = setMetadataFlags
	in.unsetMetadata = unsetMetadataFlags
	return nil
}

func gatherUpdateGuards(ctx context.Context, cmd *cobra.Command, in *updateInput) error {
	in.claim, _ = cmd.Flags().GetBool("claim")

	if err := gatherUpdateGuardValues(ctx, cmd, in); err != nil {
		return err
	}
	return validateUpdateGuardCombination(in)
}

func gatherUpdateGuardValues(ctx context.Context, cmd *cobra.Command, in *updateInput) error {
	// bd-wsqvw conditional-update guards, mirroring the non-proxied path's
	// updateGuardsFromFlags rules: Changed()-detected presence (so
	// `--if-assignee ""` guards on unassigned), --if-status validated against
	// the live status set, mutually exclusive with --claim, and requiring a
	// field update to ride on.
	if cmd.Flags().Changed("if-assignee") {
		v, _ := cmd.Flags().GetString("if-assignee")
		in.ifAssignee = &v
	}
	if cmd.Flags().Changed("if-status") {
		v, _ := cmd.Flags().GetString("if-status")
		if err := validateUpdateStatus(ctx, v); err != nil {
			return err
		}
		in.ifStatus = &v
	}
	return nil
}

func validateUpdateGuardCombination(in *updateInput) error {
	if in.ifAssignee == nil && in.ifStatus == nil {
		return nil
	}
	if in.claim {
		return HandleErrorRespectJSON("cannot combine --if-assignee/--if-status with --claim (--claim is already an atomic compare-and-set)")
	}
	if !updateInputHasGuardField(in) {
		return HandleErrorRespectJSON("--if-assignee/--if-status require at least one field update (e.g. -a, -s); label and parent edits are not covered by the guard")
	}
	return nil
}

func updateInputHasGuardField(in *updateInput) bool {
	if len(in.fields) > 0 {
		return true
	}
	if in.hasAppendNotes {
		return true
	}
	if len(in.mergeMetadataIn) > 0 {
		return true
	}
	if len(in.setMetadata) > 0 {
		return true
	}
	if len(in.unsetMetadata) > 0 {
		return true
	}
	return false
}

func validateUpdateStatus(ctx context.Context, status string) error {
	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}
	uw, err := getUOWProvider().NewUOW(ctx)
	if err != nil {
		return HandleError("open unit of work: %v", err)
	}
	names, err := uw.ConfigUseCase().ListAllStatusNames(ctx)
	uw.Close(ctx)
	if err != nil {
		return HandleErrorRespectJSON("read status set: %v", err)
	}
	for _, name := range names {
		if name == status {
			return nil
		}
	}
	return HandleErrorRespectJSON("invalid status %q (allowed: %s)", status, strings.Join(names, ", "))
}

func isUpdateInputNoop(in *updateInput) bool {
	if in.claim {
		return false
	}
	return !updateInputHasFieldChanges(in) && !updateInputHasLabelChanges(in) && !updateInputHasMetadataChanges(in)
}

func updateInputHasFieldChanges(in *updateInput) bool {
	if len(in.fields) > 0 {
		return true
	}
	if in.hasAppendNotes {
		return true
	}
	if in.setLabels != nil {
		return true
	}
	if in.reparent != nil {
		return true
	}
	return false
}

func updateInputHasLabelChanges(in *updateInput) bool {
	if len(in.addLabels) > 0 {
		return true
	}
	if len(in.removeLabels) > 0 {
		return true
	}
	return false
}

func updateInputHasMetadataChanges(in *updateInput) bool {
	if len(in.mergeMetadataIn) > 0 {
		return true
	}
	if len(in.setMetadata) > 0 {
		return true
	}
	if len(in.unsetMetadata) > 0 {
		return true
	}
	return false
}
