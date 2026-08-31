package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/timeparsing"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/validation"
	"github.com/spf13/cobra"
)

func gatherCreateDirect(cmd *cobra.Command, args []string) (*createDirectState, error) {
	st := &createDirectState{cmd: cmd}
	if err := gatherCreateDirectCore(st, args); err != nil {
		return nil, err
	}
	return st, gatherCreateDirectExtras(st)
}

func gatherCreateDirectCore(st *createDirectState, args []string) error {
	if err := gatherCreateDirectTitle(st, args); err != nil {
		return err
	}
	if err := gatherCreateDirectDescription(st); err != nil {
		return err
	}
	if err := gatherCreateDirectBody(st); err != nil {
		return err
	}
	return gatherCreateDirectIDs(st)
}

func gatherCreateDirectExtras(st *createDirectState) error {
	if err := gatherCreateDirectClass(st); err != nil {
		return err
	}
	if err := gatherCreateDirectEvent(st); err != nil {
		return err
	}
	if err := gatherCreateDirectSchedule(st); err != nil {
		return err
	}
	if err := lintCreateDirect(st); err != nil {
		return err
	}
	st.ident.dryRun, _ = st.cmd.Flags().GetBool("dry-run")
	return gatherCreateDirectEstimate(st)
}

func gatherCreateDirectTitle(st *createDirectState, args []string) error {
	titleFlag, _ := st.cmd.Flags().GetString("title")
	title, err := resolveTitle(args, titleFlag, "", "")
	if err != nil {
		return err
	}
	st.ident.title = title
	st.ident.silent, _ = st.cmd.Flags().GetBool("silent")
	warnCreateTestIssue(title, st.ident.silent)
	return nil
}

func warnCreateTestIssue(title string, silent bool) {
	if getStore() == nil || !isTestIssue(title) || silent || debug.IsQuiet() {
		return
	}
	stats, err := getStore().GetStatistics(context.Background())
	if err != nil || stats == nil || stats.TotalIssues < 5 {
		return
	}
	fmt.Fprintf(os.Stderr, "%s Creating test issue in production database\n", ui.RenderWarn("⚠"))
	fmt.Fprintf(os.Stderr, "  Title: %q appears to be test data\n", title)
	fmt.Fprintf(os.Stderr, "  Recommendation: Use isolated test database with --db\n")
	fmt.Fprintf(os.Stderr, "    bd --db /tmp/test-beads create %q\n", title)
}

func gatherCreateDirectDescription(st *createDirectState) error {
	description, descriptionChanged, err := getDescriptionFlag(st.cmd)
	if err != nil {
		return err
	}
	if err := validateDescriptionUpdate(st.cmd, description, descriptionChanged); err != nil {
		return HandleError("%v", err)
	}
	description = appendCreateDescriptionSections(description, st.cmd)
	if description == "" && !isTestIssue(st.ident.title) && config.GetBool("create.require-description") {
		return HandleError("description is required (set create.require-description: false in config.yaml to disable)")
	}
	st.ident.description = description
	return nil
}

func gatherCreateDirectBody(st *createDirectState) error {
	design, _, err := getDesignFlag(st.cmd)
	if err != nil {
		return err
	}
	st.body.design = design
	st.body.acceptance, _ = st.cmd.Flags().GetString("acceptance")
	st.body.notes, _ = st.cmd.Flags().GetString("notes")
	st.body.specID, _ = st.cmd.Flags().GetString("spec-id")
	priorityStr, _ := st.cmd.Flags().GetString("priority")
	priority, err := validation.ValidatePriority(priorityStr)
	if err != nil {
		return HandleError("%v", err)
	}
	st.body.priority = priority
	st.body.issueType, _ = st.cmd.Flags().GetString("type")
	st.body.assignee, _ = st.cmd.Flags().GetString("assignee")
	st.body.status, _ = st.cmd.Flags().GetString("status")
	if st.body.status != "" && !types.Status(st.body.status).IsValidWithCustom(loadEmbeddedCustomStatuses()) {
		return HandleErrorRespectJSON("invalid status %q (built-in: open, in_progress, blocked, deferred, closed, pinned, hooked; or configure custom statuses via 'bd config set status.custom')", st.body.status)
	}
	st.body.labels = gatherCreateDirectLabels(st.cmd)
	return nil
}

func gatherCreateDirectLabels(cmd *cobra.Command) []string {
	labels, _ := cmd.Flags().GetStringSlice("labels")
	labelAlias, _ := cmd.Flags().GetStringSlice("label")
	if len(labelAlias) > 0 {
		labels = append(labels, labelAlias...)
	}
	return labels
}

func gatherCreateDirectIDs(st *createDirectState) error {
	st.ids.explicit, _ = st.cmd.Flags().GetString("id")
	st.ids.parent, _ = st.cmd.Flags().GetString("parent")
	st.ids.externalRef, _ = st.cmd.Flags().GetString("external-ref")
	st.ids.deps, _ = st.cmd.Flags().GetStringSlice("deps")
	st.ids.waitsFor, _ = st.cmd.Flags().GetString("waits-for")
	st.ids.waitsForGate, _ = st.cmd.Flags().GetString("waits-for-gate")
	st.ident.force, _ = st.cmd.Flags().GetBool("force")
	st.repo.override, _ = st.cmd.Flags().GetString("repo")
	return nil
}

func gatherCreateDirectClass(st *createDirectState) error {
	st.ident.wisp, _ = st.cmd.Flags().GetBool("ephemeral")
	st.ident.noHistory, _ = st.cmd.Flags().GetBool("no-history")
	if st.ident.wisp && st.ident.noHistory {
		return HandleError("--ephemeral and --no-history are mutually exclusive")
	}
	storageClassFlag, _ := st.cmd.Flags().GetString("storage-class")
	storageClass, err := resolveStorageClass(storageClassFlag, types.IssueType(st.body.issueType).Normalize())
	if err != nil {
		return HandleError("%v", err)
	}
	if storageClass == types.StorageClassEphemeral {
		if st.ident.noHistory {
			return HandleError("--storage-class ephemeral and --no-history are mutually exclusive")
		}
		st.ident.wisp = true
		storageClass = ""
	}
	st.class.storageClass = storageClass
	return gatherCreateDirectMolTypes(st)
}

func gatherCreateDirectMolTypes(st *createDirectState) error {
	molTypeStr, _ := st.cmd.Flags().GetString("mol-type")
	if molTypeStr != "" {
		st.class.molType = types.MolType(molTypeStr)
		if !st.class.molType.IsValid() {
			return HandleError("invalid mol-type %q (must be swarm, patrol, or work)", molTypeStr)
		}
	}
	wispTypeStr, _ := st.cmd.Flags().GetString("wisp-type")
	if wispTypeStr != "" {
		st.class.wispType = types.WispType(wispTypeStr)
		if !st.class.wispType.IsValid() {
			return HandleError("invalid wisp-type %q (must be heartbeat, ping, patrol, gc_report, recovery, error, or escalation)", wispTypeStr)
		}
	}
	return nil
}

func gatherCreateDirectEvent(st *createDirectState) error {
	st.event.category, _ = st.cmd.Flags().GetString("event-category")
	st.event.actor, _ = st.cmd.Flags().GetString("event-actor")
	st.event.target, _ = st.cmd.Flags().GetString("event-target")
	st.event.payload, _ = st.cmd.Flags().GetString("event-payload")
	if (st.event.category != "" || st.event.actor != "" || st.event.target != "" || st.event.payload != "") && st.body.issueType != "event" {
		return HandleError("--event-category, --event-actor, --event-target, and --event-payload flags require --type=event")
	}
	return nil
}

func gatherCreateDirectSchedule(st *createDirectState) error {
	if err := gatherCreateDirectDue(st); err != nil {
		return err
	}
	if err := gatherCreateDirectDefer(st); err != nil {
		return err
	}
	return gatherCreateDirectMetadata(st)
}

func gatherCreateDirectDue(st *createDirectState) error {
	dueStr, _ := st.cmd.Flags().GetString("due")
	if dueStr == "" {
		return nil
	}
	t, err := timeparsing.ParseRelativeTime(dueStr, time.Now())
	if err != nil {
		return HandleError("invalid --due format %q. Examples: +6h, tomorrow, next monday, 2025-01-15", dueStr)
	}
	st.schedule.dueAt = &t
	return nil
}

func gatherCreateDirectDefer(st *createDirectState) error {
	deferStr, _ := st.cmd.Flags().GetString("defer")
	if deferStr == "" {
		return nil
	}
	t, err := timeparsing.ParseRelativeTime(deferStr, time.Now())
	if err != nil {
		return HandleError("invalid --defer format %q. Examples: +1h, tomorrow, next monday, 2025-01-15", deferStr)
	}
	if t.Before(time.Now()) && !st.ident.silent && !debug.IsQuiet() {
		fmt.Fprintf(os.Stderr, "%s Defer date %q is in the past. Issue will appear in bd ready immediately.\n",
			ui.RenderWarn("!"), t.Local().Format("2006-01-02 15:04"))
		fmt.Fprintf(os.Stderr, "  Did you mean a future date? Use --defer=+1h or --defer=tomorrow\n")
	}
	st.schedule.deferUntil = &t
	return nil
}

func gatherCreateDirectMetadata(st *createDirectState) error {
	if !st.cmd.Flags().Changed("metadata") {
		return nil
	}
	metadataValue, _ := st.cmd.Flags().GetString("metadata")
	metadataJSON, err := readCreateMetadataJSON(metadataValue)
	if err != nil {
		return err
	}
	if !json.Valid([]byte(metadataJSON)) {
		return HandleError("invalid JSON in --metadata: must be valid JSON")
	}
	st.schedule.metadata = json.RawMessage(metadataJSON)
	return nil
}

func readCreateMetadataJSON(metadataValue string) (string, error) {
	if !strings.HasPrefix(metadataValue, "@") {
		return metadataValue, nil
	}
	filePath := metadataValue[1:]
	// #nosec G304 -- user explicitly provides file path via @file.json syntax
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", HandleError("failed to read metadata file %s: %v", filePath, err)
	}
	return string(data), nil
}

func lintCreateDirect(st *createDirectState) error {
	validateTemplate, _ := st.cmd.Flags().GetBool("validate")
	validationMode := config.GetString("validation.on-create")
	if !validateTemplate && validationMode != "error" && validationMode != "warn" {
		return nil
	}
	lintIssue := &types.Issue{
		IssueContent: types.IssueContent{
			Description:        st.ident.description,
			AcceptanceCriteria: st.body.acceptance,
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: types.IssueType(st.body.issueType).Normalize(),
		},
	}
	if err := validation.LintIssue(lintIssue); err != nil {
		if validateTemplate || validationMode == "error" {
			return HandleError("%v", err)
		}
		fmt.Fprintf(os.Stderr, "%s %v\n", ui.RenderWarn("⚠"), err)
	}
	return nil
}

func gatherCreateDirectEstimate(st *createDirectState) error {
	if !st.cmd.Flags().Changed("estimate") {
		return nil
	}
	est, _ := st.cmd.Flags().GetInt("estimate")
	if est < 0 {
		return HandleError("estimate must be a non-negative number of minutes")
	}
	st.schedule.estimatedMinutes = &est
	return nil
}
