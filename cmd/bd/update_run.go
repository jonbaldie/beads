package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/metrics"
	storageissueops "github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/timeparsing"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/jonbaldie/beads/internal/validation"
	"github.com/jonbaldie/beads/issueops"
	"github.com/spf13/cobra"
)

func runUpdate(cmd *cobra.Command, args []string) error {
	if err := CheckReadonly("update"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("update")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	// Refuse a mis-typed flag value that cobra parsed as a positional issue
	// id, before any write path (direct or proxied) can apply a partial
	// update. See bd-5247.
	if err := errStrayFlagValuePositional(args); err != nil {
		return HandleErrorRespectJSON("%s", err)
	}

	if usesProxiedServer() {
		return runUpdateProxiedServer(cmd, getRootContext(), args)
	}
	return runUpdateDirect(cmd, args)
}

func runUpdateDirect(cmd *cobra.Command, args []string) error {
	batch, ok, err := prepareUpdateDirect(cmd, args)
	if err != nil || !ok {
		return err
	}
	return applyUpdateBatch(batch)
}

func prepareUpdateDirect(cmd *cobra.Command, args []string) (updateBatchArgs, bool, error) {
	args, err := resolveUpdateIDs(args)
	if err != nil {
		return updateBatchArgs{}, false, err
	}
	updates, clearDeferStatus, err := gatherUpdateFields(cmd)
	if err != nil {
		return updateBatchArgs{}, false, err
	}
	return assembleUpdateBatch(cmd, args, updates, clearDeferStatus)
}

func resolveUpdateIDs(args []string) ([]string, error) {
	// If no IDs provided, use last touched issue (interactive only;
	// the non-interactive case was already refused in Args validation)
	if len(args) > 0 {
		return args, nil
	}
	lastTouched := GetLastTouchedID()
	if lastTouched == "" {
		return nil, HandleErrorRespectJSON("no issue ID provided and no last touched issue")
	}
	return []string{lastTouched}, nil
}

func assembleUpdateBatch(cmd *cobra.Command, args []string, updates map[string]interface{}, clearDeferStatus bool) (updateBatchArgs, bool, error) {
	// Get claim flag
	claimFlag, _ := cmd.Flags().GetBool("claim")
	// --force bypasses the live-claim reassign fence (bd-98s5c); mutually
	// exclusive with --if-assignee at the flag-group level.
	forceFlag, _ := cmd.Flags().GetBool("force")

	if len(updates) == 0 && !claimFlag {
		fmt.Println("No updates specified")
		return updateBatchArgs{}, false, nil
	}

	expectedStatus, ifAssignee, err := resolveUpdateGuards(cmd, claimFlag, updates)
	if err != nil {
		return updateBatchArgs{}, false, err
	}
	basePatch, err := buildUpdatePatch(updates)
	if err != nil {
		return updateBatchArgs{}, false, HandleErrorRespectJSON("%v", err)
	}
	ctx := getRootContext()
	opsCtx, err := issueOpsContext(ctx)
	if err != nil {
		return updateBatchArgs{}, false, HandleErrorRespectJSON("%v", err)
	}
	return updateBatchArgs{
		args:           args,
		updates:        updates,
		patch:          basePatch,
		claim:          claimFlag,
		force:          forceFlag,
		clearDefer:     clearDeferStatus,
		ifAssignee:     ifAssignee,
		expectedStatus: expectedStatus,
		opsCtx:         opsCtx,
		ctx:            ctx,
	}, true, nil
}

func resolveUpdateGuards(cmd *cobra.Command, claimFlag bool, updates map[string]interface{}) (*issueops.Status, *string, error) {
	// Conditional-update guards (bd-wsqvw): validated against the same
	// status set as --status, mutually exclusive with --claim (which is
	// its own compare-and-set), and only meaningful with a field update
	// to ride on.
	ifAssignee, ifStatus, err := updateGuardsFromFlags(cmd, claimFlag, updates)
	if err != nil {
		return nil, nil, err
	}
	var expectedStatus *issueops.Status
	if ifStatus != nil {
		expected := issueops.Status(*ifStatus)
		expectedStatus = &expected
	}
	return expectedStatus, ifAssignee, nil
}

type updateBatchArgs struct {
	args           []string
	updates        map[string]interface{}
	patch          issueops.IssuePatch
	claim          bool
	force          bool
	clearDefer     bool
	ifAssignee     *string
	expectedStatus *issueops.Status
	opsCtx         context.Context
	ctx            context.Context
}

func gatherUpdateFields(cmd *cobra.Command) (map[string]interface{}, bool, error) {
	updates := make(map[string]interface{})
	if err := applyUpdateStatusFields(cmd, updates); err != nil {
		return nil, false, err
	}
	if err := applyUpdateTextFields(cmd, updates); err != nil {
		return nil, false, err
	}
	if err := applyUpdateIdentityFields(cmd, updates); err != nil {
		return nil, false, err
	}
	applyUpdateRelationFields(cmd, updates)
	clearDeferStatus, err := applyUpdateScheduleFields(cmd, updates)
	if err != nil {
		return nil, false, err
	}
	if err := applyUpdateEphemeralFields(cmd, updates); err != nil {
		return nil, false, err
	}
	if err := applyUpdateMetadataFields(cmd, updates); err != nil {
		return nil, false, err
	}
	return updates, clearDeferStatus, nil
}

func applyUpdateStatusFields(cmd *cobra.Command, updates map[string]interface{}) error {
	if err := applyUpdateStatusValue(cmd, updates); err != nil {
		return err
	}
	return applyUpdatePriorityValue(cmd, updates)
}

func applyUpdateStatusValue(cmd *cobra.Command, updates map[string]interface{}) error {
	if !cmd.Flags().Changed("status") {
		return nil
	}
	status, _ := cmd.Flags().GetString("status")
	customStatuses := loadUpdateCustomStatuses()
	if !types.Status(status).IsValidWithCustom(customStatuses) {
		return HandleErrorRespectJSON("invalid status %q (built-in: open, in_progress, blocked, deferred, closed, pinned, hooked; or configure custom statuses via 'bd config set status.custom')", status)
	}
	updates["status"] = status
	warnBlockedStatusUpdate(status)
	applyClosedSessionUpdate(cmd, status, updates)
	return nil
}

func loadUpdateCustomStatuses() []string {
	if getStore() == nil {
		return nil
	}
	cs, err := getStore().GetCustomStatuses(getRootContext())
	if err != nil {
		if !isJSONOutput() {
			fmt.Fprintf(os.Stderr, "%s Failed to get custom statuses: %v\n", ui.RenderWarn("!"), err)
		}
		return nil
	}
	return cs
}

func warnBlockedStatusUpdate(status string) {
	if status == string(types.StatusBlocked) && !isJSONOutput() {
		fmt.Fprintf(os.Stderr, "%s status %q is stored, not computed from dependencies. The issue leaves the ready queue. Add a blocker with `bd dep add` if another issue is blocking this work. Resume with `bd update --claim` or `--status open`.\n", ui.RenderWarn("!"), status) //nolint:gosec // G705: CLI stderr, not HTML.
	}
}

func applyClosedSessionUpdate(cmd *cobra.Command, status string, updates map[string]interface{}) {
	// If status is being set to closed, include session if provided
	if status != "closed" {
		return
	}
	session, _ := cmd.Flags().GetString("session")
	if session == "" {
		session = os.Getenv("CLAUDE_SESSION_ID")
	}
	if session != "" {
		updates["closed_by_session"] = session
	}
}

func applyUpdatePriorityValue(cmd *cobra.Command, updates map[string]interface{}) error {
	if !cmd.Flags().Changed("priority") {
		return nil
	}
	priorityStr, _ := cmd.Flags().GetString("priority")
	priority, err := validation.ValidatePriority(priorityStr)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	updates["priority"] = priority
	return nil
}

func applyUpdateTextFields(cmd *cobra.Command, updates map[string]interface{}) error {
	if err := applyUpdateTitleValue(cmd, updates); err != nil {
		return err
	}
	if cmd.Flags().Changed("assignee") {
		assignee, _ := cmd.Flags().GetString("assignee")
		updates["assignee"] = assignee
	}
	if err := applyUpdateDescriptionValue(cmd, updates); err != nil {
		return err
	}
	if err := applyUpdateDesignValue(cmd, updates); err != nil {
		return err
	}
	if err := applyUpdateNotesValue(cmd, updates); err != nil {
		return err
	}
	applyUpdateAcceptanceValue(cmd, updates)
	return nil
}

func applyUpdateTitleValue(cmd *cobra.Command, updates map[string]interface{}) error {
	if !cmd.Flags().Changed("title") {
		return nil
	}
	title, _ := cmd.Flags().GetString("title")
	title = strings.TrimSpace(title)
	if title == "" {
		return HandleErrorRespectJSON("title cannot be empty")
	}
	updates["title"] = title
	return nil
}

func applyUpdateDescriptionValue(cmd *cobra.Command, updates map[string]interface{}) error {
	description, descChanged, err := getDescriptionFlag(cmd)
	if err != nil {
		return err
	}
	if !descChanged {
		return nil
	}
	if err := validateDescriptionUpdate(cmd, description, descChanged); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	updates["description"] = description
	return nil
}

func applyUpdateDesignValue(cmd *cobra.Command, updates map[string]interface{}) error {
	design, designChanged, err := getDesignFlag(cmd)
	if err != nil {
		return err
	}
	if designChanged {
		updates["design"] = design
	}
	return nil
}

func applyUpdateNotesValue(cmd *cobra.Command, updates map[string]interface{}) error {
	if cmd.Flags().Changed("notes") && cmd.Flags().Changed("append-notes") {
		return HandleErrorRespectJSON("cannot specify both --notes and --append-notes")
	}
	if cmd.Flags().Changed("notes") {
		notes, _ := cmd.Flags().GetString("notes")
		updates["notes"] = notes
	}
	if cmd.Flags().Changed("append-notes") {
		appendNotes, _ := cmd.Flags().GetString("append-notes")
		updates[storageissueops.OpAppendNotes] = appendNotes
	}
	return nil
}

func applyUpdateAcceptanceValue(cmd *cobra.Command, updates map[string]interface{}) {
	if !cmd.Flags().Changed("acceptance") && !cmd.Flags().Changed("acceptance-criteria") {
		return
	}
	var acceptanceCriteria string
	if cmd.Flags().Changed("acceptance") {
		acceptanceCriteria, _ = cmd.Flags().GetString("acceptance")
	} else {
		acceptanceCriteria, _ = cmd.Flags().GetString("acceptance-criteria")
	}
	updates["acceptance_criteria"] = acceptanceCriteria
}

func applyUpdateIdentityFields(cmd *cobra.Command, updates map[string]interface{}) error {
	if cmd.Flags().Changed("external-ref") {
		externalRef, _ := cmd.Flags().GetString("external-ref")
		// Empty string clears the ref to SQL NULL, mirroring buildCreateIssue's
		// nil-when-empty pointer semantics so cleared refs round-trip as a
		// missing field (omitempty) instead of an empty string. GH#3902.
		if externalRef == "" {
			updates["external_ref"] = nil
		} else {
			updates["external_ref"] = externalRef
		}
	}
	if cmd.Flags().Changed("spec-id") {
		specID, _ := cmd.Flags().GetString("spec-id")
		updates["spec_id"] = specID
	}
	if cmd.Flags().Changed("estimate") {
		estimate, _ := cmd.Flags().GetInt("estimate")
		if estimate < 0 {
			return HandleErrorRespectJSON("estimate must be a non-negative number of minutes")
		}
		updates["estimated_minutes"] = estimate
	}
	if cmd.Flags().Changed("type") {
		issueType, _ := cmd.Flags().GetString("type")
		// Normalize aliases (e.g., "enhancement" -> "feature") before validating.
		// Type validation (including custom types) is handled by the storage
		// layer inside the transaction, matching the create path. (GH#3030)
		issueType = utils.NormalizeIssueType(issueType)
		updates["issue_type"] = issueType
	}
	return nil
}

func applyUpdateRelationFields(cmd *cobra.Command, updates map[string]interface{}) {
	if cmd.Flags().Changed("add-label") {
		addLabels, _ := cmd.Flags().GetStringSlice("add-label")
		updates["add_labels"] = addLabels
	}
	if cmd.Flags().Changed("remove-label") {
		removeLabels, _ := cmd.Flags().GetStringSlice("remove-label")
		updates["remove_labels"] = removeLabels
	}
	if cmd.Flags().Changed("set-labels") {
		setLabels, _ := cmd.Flags().GetStringSlice("set-labels")
		updates["set_labels"] = setLabels
	}
	if cmd.Flags().Changed("parent") {
		parent, _ := cmd.Flags().GetString("parent")
		updates["parent"] = parent
	}
	// Gate fields (bd-z6kw)
	if cmd.Flags().Changed("await-id") {
		awaitID, _ := cmd.Flags().GetString("await-id")
		updates["await_id"] = awaitID
	}
}

func applyUpdateScheduleFields(cmd *cobra.Command, updates map[string]interface{}) (bool, error) {
	// Time-based scheduling flags (GH#820)
	if err := applyUpdateDueValue(cmd, updates); err != nil {
		return false, err
	}
	return applyUpdateDeferValue(cmd, updates)
}

func applyUpdateDueValue(cmd *cobra.Command, updates map[string]interface{}) error {
	if !cmd.Flags().Changed("due") {
		return nil
	}
	dueStr, _ := cmd.Flags().GetString("due")
	if dueStr == "" {
		// Empty string clears the due date
		updates["due_at"] = nil
		return nil
	}
	t, err := timeparsing.ParseRelativeTime(dueStr, time.Now())
	if err != nil {
		return HandleErrorRespectJSON("invalid --due format %q. Examples: +6h, tomorrow, next monday, 2025-01-15", dueStr)
	}
	updates["due_at"] = t
	return nil
}

func applyUpdateDeferValue(cmd *cobra.Command, updates map[string]interface{}) (bool, error) {
	if !cmd.Flags().Changed("defer") {
		return false, nil
	}
	deferStr, _ := cmd.Flags().GetString("defer")
	if deferStr == "" {
		// Empty string clears the defer_until and restores ready-work
		// visibility (GH#3233). Explicit --status still wins.
		updates["defer_until"] = nil
		_, hasStatus := updates["status"]
		return !hasStatus, nil
	}
	return applyUpdateDeferUntil(deferStr, updates)
}

func applyUpdateDeferUntil(deferStr string, updates map[string]interface{}) (bool, error) {
	t, err := timeparsing.ParseRelativeTime(deferStr, time.Now())
	if err != nil {
		return false, HandleErrorRespectJSON("invalid --defer format %q. Examples: +1h, tomorrow, next monday, 2025-01-15", deferStr)
	}
	// Warn if defer date is in the past (user probably meant future)
	inPast := t.Before(time.Now())
	if inPast && !isJSONOutput() {
		fmt.Fprintf(os.Stderr, "%s Defer date %q is in the past. Issue will appear in bd ready immediately.\n",
			ui.RenderWarn("!"), t.Local().Format("2006-01-02 15:04"))
		fmt.Fprintf(os.Stderr, "  Did you mean a future date? Use --defer=+1h or --defer=tomorrow\n")
	}
	updates["defer_until"] = t
	// Align with `bd defer`: set status=deferred so the ❄ icon
	// shows and the issue leaves the ready queue (GH#3233).
	// Skip for past dates so the "appears in bd ready immediately"
	// warning stays truthful, and skip if --status was set explicitly.
	if _, ok := updates["status"]; !ok && !inPast {
		updates["status"] = string(types.StatusDeferred)
	}
	return false, nil
}

func applyUpdateEphemeralFields(cmd *cobra.Command, updates map[string]interface{}) error {
	if err := validateUpdateEphemeralFlags(cmd); err != nil {
		return err
	}
	applyUpdateEphemeralValues(cmd, updates)
	return nil
}

func validateUpdateEphemeralFlags(cmd *cobra.Command) error {
	// Ephemeral/persistent flags
	// Note: storage layer uses "wisp" field name, maps to "ephemeral" column
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

func applyUpdateEphemeralValues(cmd *cobra.Command, updates map[string]interface{}) {
	ephemeralChanged := cmd.Flags().Changed("ephemeral")
	persistentChanged := cmd.Flags().Changed("persistent")
	noHistoryChanged := cmd.Flags().Changed("no-history")
	historyChanged := cmd.Flags().Changed("history")
	if ephemeralChanged {
		updates["wisp"] = true
	}
	if persistentChanged {
		updates["wisp"] = false
	}
	if noHistoryChanged {
		updates["no_history"] = true
	}
	if historyChanged {
		updates["no_history"] = false
	}
}

func applyUpdateMetadataFields(cmd *cobra.Command, updates map[string]interface{}) error {
	if err := applyUpdateMetadataMerge(cmd, updates); err != nil {
		return err
	}
	return applyUpdateMetadataEdits(cmd, updates)
}

func applyUpdateMetadataMerge(cmd *cobra.Command, updates map[string]interface{}) error {
	// Metadata flag (GH#1413)
	if !cmd.Flags().Changed("metadata") {
		return nil
	}
	metadataValue, _ := cmd.Flags().GetString("metadata")
	metadataJSON, err := readUpdateMetadataJSON(metadataValue)
	if err != nil {
		return err
	}
	// Validate JSON
	if !json.Valid([]byte(metadataJSON)) {
		return HandleErrorRespectJSON("invalid JSON in --metadata: must be valid JSON")
	}
	// Passed as a merge OPERATION, not a pre-merged value: the storage
	// layer re-reads and merges inside the mutation transaction so a
	// concurrent writer's keys survive (lost-update fix).
	updates[storageissueops.OpMergeMetadata] = json.RawMessage(metadataJSON)
	return nil
}

func readUpdateMetadataJSON(metadataValue string) (string, error) {
	if !strings.HasPrefix(metadataValue, "@") {
		return metadataValue, nil
	}
	// Read JSON from file
	filePath := metadataValue[1:]
	// #nosec G304 -- user explicitly provides file path via @file.json syntax
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", HandleErrorRespectJSON("failed to read metadata file %s: %v", filePath, err)
	}
	return string(data), nil
}

func applyUpdateMetadataEdits(cmd *cobra.Command, updates map[string]interface{}) error {
	// Incremental metadata edits (GH#1406)
	setMetadataFlags, _ := cmd.Flags().GetStringArray("set-metadata")
	unsetMetadataFlags, _ := cmd.Flags().GetStringArray("unset-metadata")
	if (len(setMetadataFlags) > 0 || len(unsetMetadataFlags) > 0) && cmd.Flags().Changed("metadata") {
		return HandleErrorRespectJSON("cannot combine --metadata with --set-metadata or --unset-metadata")
	}
	if len(setMetadataFlags) > 0 {
		updates[storageissueops.OpSetMetadata] = setMetadataFlags
	}
	if len(unsetMetadataFlags) > 0 {
		updates[storageissueops.OpUnsetMetadata] = unsetMetadataFlags
	}
	return nil
}
