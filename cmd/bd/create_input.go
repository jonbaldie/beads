package main

import (
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

type createInputSource struct {
	markdownFile string
	graphFile    string
	title        string
	explicitID   string
	parentID     string
}

type createInputIdentity struct {
	issueType   string
	status      string
	priority    int
	assignee    string
	externalRef string
	specID      string
}

type createInputBody struct {
	description        string
	design             string
	acceptanceCriteria string
	notes              string
	appendNotes        string
	labels             []string
	noInheritLabels    bool
	deps               []string
}

type createInputWaits struct {
	waitsFor        string
	waitsForGate    string
	waitsForGateSet bool // true when --waits-for-gate was explicitly passed (not relying on default)
}

type createInputFlags struct {
	silent    bool
	dryRun    bool
	force     bool
	validate  bool
	ephemeral bool
	noHistory bool
}

type createInputKind struct {
	molType  types.MolType
	wispType types.WispType
}

type createInputEvent struct {
	eventCategory string
	eventActor    string
	eventTarget   string
	eventPayload  string
}

type createInputSchedule struct {
	dueAt            *time.Time
	deferUntil       *time.Time
	metadata         json.RawMessage
	metadataSet      bool
	estimatedMinutes *int
}

type createInputRepo struct {
	repoOverride    string
	repoOverrideSet bool
	createdBy       string
	owner           string
	jsonOutput      bool
	validationMode  string
}

type createInput struct {
	createInputSource
	createInputIdentity
	createInputBody
	createInputWaits
	createInputFlags
	createInputKind
	createInputEvent
	createInputSchedule
	createInputRepo
}

// graphApplyOptions projects the plan-wide CLI flags into the options every
// graph validation/materialization helper takes.
func (in createInput) graphApplyOptions() GraphApplyOptions {
	return GraphApplyOptions{Ephemeral: in.ephemeral, NoHistory: in.noHistory, Force: in.force}
}

// graphApplyOptionsFromFlags is the embedded-path projection of the plan-wide
// flags, delegating to graphApplyOptions so both transports share one
// flags→options mapping (the embedded create path reads flags directly
// rather than through gatherCreateInput).
func graphApplyOptionsFromFlags(cmd *cobra.Command) GraphApplyOptions {
	in := newCreateInput()
	in.ephemeral, _ = cmd.Flags().GetBool("ephemeral")
	in.noHistory, _ = cmd.Flags().GetBool("no-history")
	in.force, _ = cmd.Flags().GetBool("force")
	return in.graphApplyOptions()
}

func gatherCreateInput(cmd *cobra.Command, args []string) (createInput, error) {
	in := newCreateInput()

	if err := gatherCreateSourceInput(&in, cmd, args); err != nil {
		return in, err
	}
	if err := gatherCreateContentInput(&in, cmd, args); err != nil {
		return in, err
	}
	if err := gatherCreateIssueFields(&in, cmd); err != nil {
		return in, err
	}
	if err := gatherCreateEventFields(&in, cmd); err != nil {
		return in, err
	}
	if err := gatherCreateScheduleFields(&in, cmd); err != nil {
		return in, err
	}
	if err := gatherCreateMetadataFields(&in, cmd); err != nil {
		return in, err
	}

	in.createdBy = getActorWithGit()
	in.owner = getOwner()
	in.jsonOutput = isJSONOutput()
	in.validationMode = config.GetString("validation.on-create")
	if in.validate {
		in.validationMode = "error"
	}

	return in, nil
}

func newCreateInput() createInput {
	return createInput{
		createInputSource:   createInputSource{},
		createInputIdentity: createInputIdentity{},
		createInputBody:     createInputBody{},
		createInputWaits:    createInputWaits{},
		createInputFlags:    createInputFlags{},
		createInputKind:     createInputKind{},
		createInputEvent:    createInputEvent{},
		createInputSchedule: createInputSchedule{},
		createInputRepo:     createInputRepo{},
	}
}

func gatherCreateSourceInput(in *createInput, cmd *cobra.Command, args []string) error {
	in.markdownFile, _ = cmd.Flags().GetString("file")
	in.graphFile, _ = cmd.Flags().GetString("graph")
	in.dryRun, _ = cmd.Flags().GetBool("dry-run")

	if in.markdownFile != "" && in.graphFile != "" {
		return HandleError("cannot specify both --file and --graph")
	}
	if in.markdownFile != "" {
		if len(args) > 0 {
			return HandleError("cannot specify both title and --file flag")
		}
		if in.dryRun {
			return HandleError("--dry-run is not supported with --file flag")
		}
		return rejectSingleIssueFlagsForMarkdown(cmd)
	}
	if in.graphFile != "" {
		if len(args) > 0 {
			return HandleError("cannot specify both title and --graph flag")
		}
		return rejectSingleIssueFlagsForGraph(cmd)
	}
	return nil
}

func gatherCreateContentInput(in *createInput, cmd *cobra.Command, args []string) error {
	gatherCreateBehaviorFlags(in, cmd)
	if in.ephemeral && in.noHistory {
		return HandleError("--ephemeral and --no-history are mutually exclusive")
	}
	if err := gatherCreateTitle(in, cmd, args); err != nil {
		return err
	}
	if err := gatherCreateDescription(in, cmd); err != nil {
		return err
	}
	if err := gatherCreateTextFields(in, cmd); err != nil {
		return err
	}
	if createDescriptionRequired(*in) {
		return HandleError("description is required (set create.require-description: false in config.yaml to disable)")
	}
	return nil
}

func gatherCreateBehaviorFlags(in *createInput, cmd *cobra.Command) {
	in.silent, _ = cmd.Flags().GetBool("silent")
	in.force, _ = cmd.Flags().GetBool("force")
	in.validate, _ = cmd.Flags().GetBool("validate")
	in.noInheritLabels, _ = cmd.Flags().GetBool("no-inherit-labels")
	in.ephemeral, _ = cmd.Flags().GetBool("ephemeral")
	in.noHistory, _ = cmd.Flags().GetBool("no-history")
}

func gatherCreateTitle(in *createInput, cmd *cobra.Command, args []string) error {
	titleFlag, _ := cmd.Flags().GetString("title")
	title, err := resolveTitle(args, titleFlag, in.markdownFile, in.graphFile)
	if err != nil {
		return err
	}
	in.title = title
	return nil
}

func gatherCreateDescription(in *createInput, cmd *cobra.Command) error {
	desc, descChanged, err := getDescriptionFlag(cmd)
	if err != nil {
		return err
	}
	if err := validateDescriptionUpdate(cmd, desc, descChanged); err != nil {
		return HandleError("%v", err)
	}
	in.description = appendCreateDescriptionSections(desc, cmd)
	return nil
}

func gatherCreateTextFields(in *createInput, cmd *cobra.Command) error {
	design, _, err := getDesignFlag(cmd)
	if err != nil {
		return err
	}
	in.design = design
	in.acceptanceCriteria, _ = cmd.Flags().GetString("acceptance")
	in.notes, _ = cmd.Flags().GetString("notes")
	in.appendNotes, _ = cmd.Flags().GetString("append-notes")
	in.specID, _ = cmd.Flags().GetString("spec-id")
	return nil
}

func createDescriptionRequired(in createInput) bool {
	return in.markdownFile == "" && in.graphFile == "" && in.description == "" && !isTestIssue(in.title) && config.GetBool("create.require-description")
}

func appendCreateDescriptionSections(description string, cmd *cobra.Command) string {
	for _, section := range []struct {
		flag   string
		header string
	}{
		{flag: "skills", header: "## Required Skills"},
		{flag: "context", header: "## Context"},
	} {
		value, _ := cmd.Flags().GetString(section.flag)
		if value == "" {
			continue
		}
		if description != "" {
			description += "\n\n"
		}
		description += section.header + "\n" + value
	}
	return description
}

func gatherCreateIssueFields(in *createInput, cmd *cobra.Command) error {
	priorityStr, _ := cmd.Flags().GetString("priority")
	priority, err := validation.ValidatePriority(priorityStr)
	if err != nil {
		return HandleError("%v", err)
	}
	in.priority = priority

	in.issueType, _ = cmd.Flags().GetString("type")
	in.status, _ = cmd.Flags().GetString("status")
	in.assignee, _ = cmd.Flags().GetString("assignee")
	in.externalRef, _ = cmd.Flags().GetString("external-ref")
	in.explicitID, _ = cmd.Flags().GetString("id")
	in.parentID, _ = cmd.Flags().GetString("parent")
	in.waitsFor, _ = cmd.Flags().GetString("waits-for")
	in.waitsForGate, _ = cmd.Flags().GetString("waits-for-gate")
	in.waitsForGateSet = cmd.Flags().Changed("waits-for-gate")

	if in.explicitID != "" && in.parentID != "" {
		return HandleError("cannot specify both --id and --parent flags")
	}

	in.labels, _ = cmd.Flags().GetStringSlice("labels")
	labelAlias, _ := cmd.Flags().GetStringSlice("label")
	in.labels = append(in.labels, labelAlias...)
	in.deps, _ = cmd.Flags().GetStringSlice("deps")
	in.repoOverride, _ = cmd.Flags().GetString("repo")
	in.repoOverrideSet = cmd.Flags().Changed("repo")

	if err := gatherCreatePlanTypes(in, cmd); err != nil {
		return err
	}
	return nil
}

func gatherCreatePlanTypes(in *createInput, cmd *cobra.Command) error {
	if molTypeStr, _ := cmd.Flags().GetString("mol-type"); molTypeStr != "" {
		mt := types.MolType(molTypeStr)
		if !mt.IsValid() {
			return HandleError("invalid mol-type %q (must be %s)", molTypeStr, types.ValidMolTypeNames())
		}
		in.molType = mt
	}
	if wispTypeStr, _ := cmd.Flags().GetString("wisp-type"); wispTypeStr != "" {
		wt := types.WispType(wispTypeStr)
		if !wt.IsValid() {
			return HandleError("invalid wisp-type %q (must be %s)", wispTypeStr, types.ValidWispTypeNames())
		}
		in.wispType = wt
	}
	return nil
}

func gatherCreateEventFields(in *createInput, cmd *cobra.Command) error {
	in.eventCategory, _ = cmd.Flags().GetString("event-category")
	in.eventActor, _ = cmd.Flags().GetString("event-actor")
	in.eventTarget, _ = cmd.Flags().GetString("event-target")
	in.eventPayload, _ = cmd.Flags().GetString("event-payload")
	if (in.eventCategory != "" || in.eventActor != "" || in.eventTarget != "" || in.eventPayload != "") && in.issueType != "event" {
		return HandleError("--event-category, --event-actor, --event-target, and --event-payload flags require --type=event")
	}
	return nil
}

func gatherCreateScheduleFields(in *createInput, cmd *cobra.Command) error {
	if dueStr, _ := cmd.Flags().GetString("due"); dueStr != "" {
		t, err := timeparsing.ParseRelativeTime(dueStr, time.Now())
		if err != nil {
			return HandleError("invalid --due format %q. Examples: +6h, tomorrow, next monday, 2025-01-15", dueStr)
		}
		in.dueAt = &t
	}

	if deferStr, _ := cmd.Flags().GetString("defer"); deferStr != "" {
		t, err := timeparsing.ParseRelativeTime(deferStr, time.Now())
		if err != nil {
			return HandleError("invalid --defer format %q. Examples: +1h, tomorrow, next monday, 2025-01-15", deferStr)
		}
		if t.Before(time.Now()) && !in.silent && !debug.IsQuiet() {
			fmt.Fprintf(os.Stderr, "%s Defer date %q is in the past. Issue will appear in bd ready immediately.\n",
				ui.RenderWarn("!"), t.Local().Format("2006-01-02 15:04"))
			fmt.Fprintf(os.Stderr, "  Did you mean a future date? Use --defer=+1h or --defer=tomorrow\n")
		}
		in.deferUntil = &t
	}
	return nil
}

func gatherCreateMetadataFields(in *createInput, cmd *cobra.Command) error {
	if cmd.Flags().Changed("metadata") {
		metadataValue, _ := cmd.Flags().GetString("metadata")
		metadataJSON := metadataValue
		if strings.HasPrefix(metadataValue, "@") {
			filePath := metadataValue[1:]
			// #nosec G304 -- user explicitly provides file path via @file.json syntax
			data, err := os.ReadFile(filePath)
			if err != nil {
				return HandleError("failed to read metadata file %s: %v", filePath, err)
			}
			metadataJSON = string(data)
		}
		if !json.Valid([]byte(metadataJSON)) {
			return HandleError("invalid JSON in --metadata: must be valid JSON")
		}
		in.metadata = json.RawMessage(metadataJSON)
		in.metadataSet = true
	}

	if cmd.Flags().Changed("estimate") {
		est, _ := cmd.Flags().GetInt("estimate")
		if est < 0 {
			return HandleError("estimate must be a non-negative number of minutes")
		}
		in.estimatedMinutes = &est
	}
	return nil
}

var singleIssueOnlyFlags = []string{
	"title",
	"id", "parent", "no-inherit-labels",
	"deps", "waits-for", "waits-for-gate",
	"type", "priority", "assignee", "external-ref", "spec-id",
	"status",
	"description", "body", "message", "body-file", "description-file", "stdin",
	"design", "design-file", "acceptance", "notes", "append-notes",
	"allow-empty-description",
	"labels", "label", "skills", "context",
	"event-category", "event-actor", "event-target", "event-payload",
	"due", "defer",
	"metadata", "estimate", "wisp-type",
}

func rejectSingleIssueFlagsForMarkdown(cmd *cobra.Command) error {
	for _, name := range singleIssueOnlyFlags {
		if cmd.Flags().Changed(name) {
			return HandleError("--%s is not valid with --file (markdown templates supply per-issue fields)", name)
		}
	}
	// --force is plan-wide for --graph (foreign-prefix explicit IDs) but the
	// markdown path never consults it, so reject it here rather than accept
	// and silently ignore.
	if cmd.Flags().Changed("force") {
		return HandleError("--force is not valid with --file (markdown templates supply per-issue fields)")
	}
	return nil
}

func rejectSingleIssueFlagsForGraph(cmd *cobra.Command) error {
	for _, name := range singleIssueOnlyFlags {
		if cmd.Flags().Changed(name) {
			return HandleError("--%s is not valid with --graph (graph plans supply per-node fields)", name)
		}
	}
	if cmd.Flags().Changed("mol-type") {
		return HandleError("--mol-type is not valid with --graph (set mol_type per node in the plan instead)")
	}
	return nil
}

func resolveTitle(args []string, titleFlag, markdownFile, graphFile string) (string, error) {
	if markdownFile != "" || graphFile != "" {
		return "", nil
	}
	title, err := selectCreateTitle(args, titleFlag)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(title) == "" {
		return "", HandleError("title cannot be empty or whitespace-only")
	}
	return title, nil
}

func selectCreateTitle(args []string, titleFlag string) (string, error) {
	switch {
	case len(args) > 0 && titleFlag != "":
		if args[0] != titleFlag {
			return "", HandleError("cannot specify different titles as both positional argument and --title flag\n  Positional: %q\n  --title:    %q", args[0], titleFlag)
		}
		return args[0], nil
	case len(args) > 0:
		if strings.HasPrefix(args[0], "-") {
			return "", HandleError("title %q looks like a flag (starts with '-').\n  Run 'bd create --help' for available options.\n  To use this title anyway, pass it explicitly: bd create --title=%q", args[0], args[0])
		}
		return args[0], nil
	case titleFlag != "":
		return titleFlag, nil
	default:
		return "", HandleError("title required (or use --file to create from markdown)")
	}
}
