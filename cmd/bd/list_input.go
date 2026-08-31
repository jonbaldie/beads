package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/jonbaldie/beads/internal/validation"
	"github.com/jonbaldie/beads/internal/workapi"
	"github.com/jonbaldie/beads/issueops"
	"github.com/spf13/cobra"
)

// listInput is everything `bd list` parsed off the command line: the
// frontend-independent query knobs (issueops.ListRequest, the request the
// reader role takes and the filter is built from) plus the presentation
// choices that never leave the CLI.
type listInput struct {
	issueops.ListRequest

	longFormat   bool
	prettyFormat bool
	flatFormat   bool
	depsMode     string
	watchMode    bool
	noPager      bool
	formatStr    string
	jsonOutput   bool

	limitChanged   bool
	effectiveLimit int

	repoOverride    string
	repoOverrideSet bool
}

func gatherListInput(cmd *cobra.Command) (listInput, error) {
	in := listInput{}
	var limit int
	var listLimitConfigured bool

	err := runListInputStages(
		func() error {
			limit, listLimitConfigured = gatherListStatusAndLimit(cmd, &in)
			return nil
		},
		func() error { return gatherListFormatAndFilters(cmd, &in) },
		func() error { return validateListSkipLabels(&in) },
		func() error { return gatherListPriorities(cmd, &in) },
		func() error { return gatherListPinned(cmd, &in) },
		func() error { return gatherListScope(cmd, &in) },
		func() error { return gatherListMolAndWispTypes(cmd, &in) },
		func() error { return gatherListTimeFilters(cmd, &in) },
		func() error { return gatherListMetadataFilters(cmd, &in) },
		func() error { return gatherListPresentation(cmd, &in) },
		func() error { return validateListBrief(&in) },
		func() error { return validateListDependencies(cmd, &in) },
		func() error { return validateListSort(&in) },
		func() error { return normalizeListLabels(&in) },
		func() error { return resolveListLimit(&in, limit, listLimitConfigured) },
		func() error { return gatherListOffset(cmd, &in) },
		func() error { return gatherListMaxRowsAndRepo(cmd, &in) },
	)
	return in, err
}

func runListInputStages(stages ...func() error) error {
	for _, stage := range stages {
		if err := stage(); err != nil {
			return err
		}
	}
	return nil
}

func gatherListStatusAndLimit(cmd *cobra.Command, in *listInput) (int, bool) {
	in.Status, _ = cmd.Flags().GetString("status")
	if in.Status == "" {
		in.Status, _ = cmd.Flags().GetString("state")
	}

	in.Assignee, _ = cmd.Flags().GetString("assignee")
	rawType, _ := cmd.Flags().GetString("type")
	in.IssueType = utils.NormalizeIssueType(rawType)

	limit, _ := cmd.Flags().GetInt("limit")
	in.limitChanged = cmd.Flags().Changed("limit")
	listLimitConfigured := false
	if !in.limitChanged {
		listLimitConfigured = config.GetValueSource("list.limit") != config.SourceDefault
		limit = config.GetInt("list.limit")
	}
	in.AllFlag, _ = cmd.Flags().GetBool("all")
	return limit, listLimitConfigured
}

func gatherListFormatAndFilters(cmd *cobra.Command, in *listInput) error {
	in.formatStr, _ = cmd.Flags().GetString("format")
	if strings.EqualFold(in.formatStr, "json") {
		setJSONOutput(true)
		in.formatStr = ""
	}
	in.jsonOutput = isJSONOutput()

	in.Labels, _ = cmd.Flags().GetStringSlice("label")
	in.LabelsAny, _ = cmd.Flags().GetStringSlice("label-any")
	in.ExcludeLabels, _ = cmd.Flags().GetStringSlice("exclude-label")
	in.LabelPattern, _ = cmd.Flags().GetString("label-pattern")
	in.LabelRegex, _ = cmd.Flags().GetString("label-regex")
	in.TitleSearch, _ = cmd.Flags().GetString("title")
	in.SpecPrefix, _ = cmd.Flags().GetString("spec")
	in.IDFilter, _ = cmd.Flags().GetString("id")
	in.longFormat, _ = cmd.Flags().GetBool("long")
	in.SortBy, _ = cmd.Flags().GetString("sort")
	in.Reverse, _ = cmd.Flags().GetBool("reverse")

	in.TitleContains, _ = cmd.Flags().GetString("title-contains")
	in.DescContains, _ = cmd.Flags().GetString("desc-contains")
	in.NotesContains, _ = cmd.Flags().GetString("notes-contains")
	in.ExternalContains, _ = cmd.Flags().GetString("external-contains")
	in.ExternalRef, _ = cmd.Flags().GetString("external-ref")

	in.EmptyDesc, _ = cmd.Flags().GetBool("empty-description")
	in.NoAssignee, _ = cmd.Flags().GetBool("no-assignee")
	in.NoLabels, _ = cmd.Flags().GetBool("no-labels")

	in.Brief, _ = cmd.Flags().GetBool("brief")
	in.SkipLabels, _ = cmd.Flags().GetBool("skip-labels")
	return nil
}

func validateListSkipLabels(in *listInput) error {
	if !in.SkipLabels {
		return nil
	}
	conflicts := skipLabelsConflicts(in.Labels, in.LabelsAny, in.LabelPattern, in.LabelRegex, in.ExcludeLabels, in.NoLabels)
	if len(conflicts) > 0 {
		fmt.Fprint(os.Stderr, formatSkipLabelsConflictError(conflicts))
		return &exitError{Code: 2}
	}
	return nil
}

func gatherListPriorities(cmd *cobra.Command, in *listInput) error {
	return runListInputStages(
		func() error {
			if !cmd.Flags().Changed("priority") {
				return nil
			}
			priorityStr, _ := cmd.Flags().GetString("priority")
			p, err := validation.ValidatePriority(priorityStr)
			if err != nil {
				return HandleError("%v", err)
			}
			in.Priority = &p
			return nil
		},
		func() error {
			if !cmd.Flags().Changed("priority-min") {
				return nil
			}
			s, _ := cmd.Flags().GetString("priority-min")
			p, err := validation.ValidatePriority(s)
			if err != nil {
				return HandleError("parsing --priority-min: %v", err)
			}
			in.PriorityMin = &p
			return nil
		},
		func() error {
			if !cmd.Flags().Changed("priority-max") {
				return nil
			}
			s, _ := cmd.Flags().GetString("priority-max")
			p, err := validation.ValidatePriority(s)
			if err != nil {
				return HandleError("parsing --priority-max: %v", err)
			}
			in.PriorityMax = &p
			return nil
		},
	)
}

func gatherListPinned(cmd *cobra.Command, in *listInput) error {
	in.PinnedFlag, _ = cmd.Flags().GetBool("pinned")
	in.NoPinnedFlag, _ = cmd.Flags().GetBool("no-pinned")
	if in.PinnedFlag && in.NoPinnedFlag {
		return HandleError("--pinned and --no-pinned are mutually exclusive")
	}
	return nil
}

func gatherListScope(cmd *cobra.Command, in *listInput) error {
	in.IncludeTemplates, _ = cmd.Flags().GetBool("include-templates")
	in.IncludeGates, _ = cmd.Flags().GetBool("include-gates")
	in.IncludeInfra, _ = cmd.Flags().GetBool("include-infra")
	in.ExcludeTypes, _ = cmd.Flags().GetStringSlice("exclude-type")

	in.ParentID, _ = cmd.Flags().GetString("parent")
	if in.ParentID == "" {
		in.ParentID, _ = cmd.Flags().GetString("filter-parent")
	}
	in.NoParent, _ = cmd.Flags().GetBool("no-parent")
	if in.ParentID != "" && in.NoParent {
		return HandleError("--parent and --no-parent are mutually exclusive")
	}
	return nil
}

func gatherListMolAndWispTypes(cmd *cobra.Command, in *listInput) error {
	if err := gatherListMolType(cmd, in); err != nil {
		return err
	}
	return gatherListWispType(cmd, in)
}

func gatherListMolType(cmd *cobra.Command, in *listInput) error {
	s, _ := cmd.Flags().GetString("mol-type")
	if s == "" {
		return nil
	}
	mt := types.MolType(s)
	if !mt.IsValid() {
		return HandleError("invalid mol-type %q (must be %s)", s, types.ValidMolTypeNames())
	}
	in.MolType = &mt
	return nil
}

func gatherListWispType(cmd *cobra.Command, in *listInput) error {
	s, _ := cmd.Flags().GetString("wisp-type")
	if s == "" {
		return nil
	}
	wt := types.WispType(s)
	if !wt.IsValid() {
		return HandleError("invalid wisp-type %q (must be %s)", s, types.ValidWispTypeNames())
	}
	in.WispType = &wt
	return nil
}

func gatherListTimeFilters(cmd *cobra.Command, in *listInput) error {
	in.DeferredFlag, _ = cmd.Flags().GetBool("deferred")
	in.OverdueFlag, _ = cmd.Flags().GetBool("overdue")

	return runListInputStages(
		func() error { return gatherListTimeFlag(cmd, "created-after", &in.CreatedAfter) },
		func() error { return gatherListTimeFlag(cmd, "created-before", &in.CreatedBefore) },
		func() error { return gatherListTimeFlag(cmd, "updated-after", &in.UpdatedAfter) },
		func() error { return gatherListTimeFlag(cmd, "updated-before", &in.UpdatedBefore) },
		func() error { return gatherListTimeFlag(cmd, "closed-after", &in.ClosedAfter) },
		func() error { return gatherListTimeFlag(cmd, "closed-before", &in.ClosedBefore) },
		func() error { return gatherListTimeFlag(cmd, "defer-after", &in.DeferAfter) },
		func() error { return gatherListTimeFlag(cmd, "defer-before", &in.DeferBefore) },
		func() error { return gatherListTimeFlag(cmd, "due-after", &in.DueAfter) },
		func() error { return gatherListTimeFlag(cmd, "due-before", &in.DueBefore) },
	)
}

func gatherListTimeFlag(cmd *cobra.Command, name string, target **time.Time) error {
	t, err := parseListTimeFlag(cmd, name)
	if err != nil {
		return err
	}
	*target = t
	return nil
}

func gatherListMetadataFilters(cmd *cobra.Command, in *listInput) error {
	metadataFieldFlags, _ := cmd.Flags().GetStringArray("metadata-field")
	if len(metadataFieldFlags) > 0 {
		in.MetadataFields = make(map[string]string, len(metadataFieldFlags))
		for _, mf := range metadataFieldFlags {
			k, v, ok := strings.Cut(mf, "=")
			if !ok || k == "" {
				return HandleErrorRespectJSON("invalid --metadata-field: expected key=value, got %q", mf)
			}
			if err := storage.ValidateMetadataKey(k); err != nil {
				return HandleErrorRespectJSON("invalid --metadata-field key: %v", err)
			}
			in.MetadataFields[k] = v
		}
	}
	return validateListMetadataKey(cmd, in)
}

func validateListMetadataKey(cmd *cobra.Command, in *listInput) error {
	k, _ := cmd.Flags().GetString("has-metadata-key")
	if k == "" {
		return nil
	}
	if err := storage.ValidateMetadataKey(k); err != nil {
		return HandleErrorRespectJSON("invalid --has-metadata-key: %v", err)
	}
	in.HasMetadataKey = k
	return nil
}

func gatherListPresentation(cmd *cobra.Command, in *listInput) error {
	prettyFormat, _ := cmd.Flags().GetBool("pretty")
	treeFormat, _ := cmd.Flags().GetBool("tree")
	in.flatFormat, _ = cmd.Flags().GetBool("flat")
	if in.flatFormat {
		treeFormat = false
	}
	in.prettyFormat = (prettyFormat || treeFormat) && !in.jsonOutput && in.formatStr == ""
	in.watchMode, _ = cmd.Flags().GetBool("watch")
	if in.watchMode {
		in.prettyFormat = true
	}
	in.noPager, _ = cmd.Flags().GetBool("no-pager")
	in.ReadyFlag, _ = cmd.Flags().GetBool("ready")
	return nil
}

func validateListBrief(in *listInput) error {
	if !in.Brief {
		return nil
	}
	// REFUSED WHERE IT CANNOT BE HONORED OR CANNOT BE SEEN. The page routes,
	// direct and proxied, JSON and text, all hand this request to
	// issueops.Reader.List, whose query reads types.IssueFilter.Lite; the three
	// below leave that query.
	//
	//   --watch re-queries on a ticker through loadWatchedIssues, whose --ready
	//   arm calls the bare GetReadyWork and whose --parent arm walks the tree;
	//   neither reads Lite.
	//
	//   --parent with --pretty is that same tree walk, an unlimited per-level
	//   query rather than a page.
	//
	//   --format hands the whole issue to a caller-written template, so
	//   `--brief --format '{{.Issue.Description}}'` would print an empty string
	//   with nothing to say it had been dropped. The long format prints one
	//   omitted field and says so; a template can print any of the six and
	//   cannot be annotated.
	switch {
	case in.watchMode:
		return HandleError("--watch cannot be combined with --brief")
	case in.formatStr != "":
		return HandleError("--format cannot be combined with --brief; a template can print a field --brief omits, with nothing to mark it")
	case in.ParentID != "" && in.prettyFormat:
		return HandleError("--parent with --pretty cannot be combined with --brief; the hierarchical walk is a different query")
	}
	return nil
}

func validateListDependencies(cmd *cobra.Command, in *listInput) error {
	in.depsMode, _ = cmd.Flags().GetString("deps")
	if in.depsMode == "" {
		return nil
	}
	if in.depsMode != "scheduling" && in.depsMode != "all" {
		return HandleErrorRespectJSON("invalid --deps value %q (valid: scheduling, all)", in.depsMode)
	}
	// --deps annotates and orders the parent-child tree, so it is meaningful
	// only in the tree view. Reject the non-tree output modes rather than
	// accept the flag and silently ignore it, then imply the tree view so a
	// bare `--deps` renders as intended (mirrors --watch implying --pretty).
	switch {
	case in.jsonOutput:
		return HandleErrorRespectJSON("--deps is not supported with --json output")
	case in.formatStr != "":
		return HandleErrorRespectJSON("--deps is not supported with --format output")
	case in.flatFormat:
		return HandleErrorRespectJSON("--deps requires the tree view and cannot be combined with --flat")
	case in.watchMode:
		return HandleErrorRespectJSON("--deps is not supported with --watch")
	}
	in.prettyFormat = true
	return nil
}

func validateListSort(in *listInput) error {
	if in.SortBy == "" {
		return nil
	}
	validSortFields := map[string]bool{
		"priority": true, "created": true, "updated": true, "closed": true,
		"status": true, "id": true, "title": true, "type": true, "assignee": true,
	}
	if !validSortFields[in.SortBy] {
		return HandleError("invalid sort field %q (valid: priority, created, updated, closed, status, id, title, type, assignee)", in.SortBy)
	}
	return nil
}

func normalizeListLabels(in *listInput) error {
	in.Labels = utils.NormalizeLabels(in.Labels)
	in.LabelsAny = utils.NormalizeLabels(in.LabelsAny)
	in.ExcludeLabels = utils.NormalizeLabels(in.ExcludeLabels)

	if !in.SkipLabels && len(in.Labels) == 0 && len(in.LabelsAny) == 0 {
		if dirLabels := config.GetDirectoryLabels(); len(dirLabels) > 0 {
			in.LabelsAny = dirLabels
		}
	}
	return nil
}

func resolveListLimit(in *listInput, limit int, listLimitConfigured bool) error {
	in.effectiveLimit = limit
	switch {
	case in.limitChanged:
		in.effectiveLimit = limit
	case in.AllFlag:
		in.effectiveLimit = 0
	case listLimitConfigured:
		in.effectiveLimit = limit
	case !ui.IsTerminal():
		in.effectiveLimit = 0 // Piped stdout should not truncate (GH#4094)
	case ui.IsAgentMode():
		in.effectiveLimit = 20
	}
	// The request carries the limit the caller receives. Which row limit that
	// implies for the query - a sort SQL cannot express fetches everything and
	// trims client-side - is workapi.SQLLimit's decision, made once, inside the
	// builder, for every frontend.
	pageLimit := in.effectiveLimit
	in.Limit = &pageLimit
	return nil
}

func gatherListOffset(cmd *cobra.Command, in *listInput) error {
	if !cmd.Flags().Changed("offset") {
		return nil
	}
	offset, _ := cmd.Flags().GetInt("offset")
	if offset < 0 {
		return HandleError("--offset must be >= 0")
	}
	// --offset only makes sense when pagination happens in SQL. Sorts
	// that fall back to Go-side (currently --sort id) fetch everything
	// regardless, so combining them with --offset is misleading — the
	// caller would think they're paging when they're really pulling
	// the whole result set.
	if offset > 0 && workapi.SQLLimit(in.ListRequest) == 0 && in.SortBy == "id" {
		return HandleError("--offset is not supported with --sort %s (sort requires fetching the full result set)", in.SortBy)
	}
	in.Offset = offset
	return nil
}

func gatherListMaxRowsAndRepo(cmd *cobra.Command, in *listInput) error {
	// The defensive cap is part of the REQUEST, not something stamped onto the
	// filter after the builder has produced it (issueops.ListRequest.MaxRows).
	//
	// Resolving it HERE also means it is resolved exactly once per invocation.
	// resolveMaxRowsEnvOnly warns on a malformed BEADS_MAX_ROWS every time it
	// runs, so a second resolve downstream would warn twice.
	maxRows, maxRowsSource, err := resolveMaxRows(cmd)
	if err != nil {
		return err
	}
	in.MaxRows = maxRows
	in.MaxRowsSource = maxRowsSource

	in.repoOverride, _ = cmd.Flags().GetString("repo")
	in.repoOverrideSet = cmd.Flags().Changed("repo")
	return nil
}
func parseListTimeFlag(cmd *cobra.Command, name string) (*time.Time, error) {
	s, _ := cmd.Flags().GetString(name)
	if s == "" {
		return nil, nil
	}
	t, err := parseTimeFlag(s)
	if err != nil {
		return nil, HandleError("parsing --%s: %v", name, err)
	}
	return &t, nil
}
