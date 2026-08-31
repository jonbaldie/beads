package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/jonbaldie/beads/internal/validation"
	"github.com/jonbaldie/beads/internal/workapi"
	"github.com/spf13/cobra"
)

type searchProxiedInput struct {
	filter     types.IssueFilter
	status     string
	longFormat bool
	sortBy     string
	reverse    bool
}

func runSearchProxiedServer(cmd *cobra.Command, ctx context.Context, args []string) error {
	query, err := resolveSearchQuery(cmd, args)
	if err != nil {
		return err
	}
	in, err := gatherSearchProxiedInput(cmd)
	if err != nil {
		return err
	}
	uw, err := openProxiedListUOW(ctx)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	defer uw.Close(ctx)
	if err := applySearchProxiedStatus(ctx, uw, in.status, &in.filter); err != nil {
		return err
	}
	if isJSONOutput() {
		return runSearchProxiedJSON(ctx, uw, query, in)
	}
	return runSearchProxiedHuman(ctx, uw, query, in)
}

func resolveSearchQuery(cmd *cobra.Command, args []string) (string, error) {
	queryFlag, _ := cmd.Flags().GetString("query")
	query := queryFlag
	if len(args) > 0 {
		query = strings.Join(args, " ")
	}
	if query != "" {
		return query, nil
	}
	if err := cmd.Help(); err != nil {
		fmt.Fprintf(os.Stderr, "Error displaying help: %v\n", err)
	}
	return "", HandleErrorRespectJSON("search query is required")
}

func gatherSearchProxiedInput(cmd *cobra.Command) (searchProxiedInput, error) {
	in := searchProxiedInput{}
	in.status, _ = cmd.Flags().GetString("status")
	in.longFormat, _ = cmd.Flags().GetBool("long")
	in.sortBy, _ = cmd.Flags().GetString("sort")
	in.reverse, _ = cmd.Flags().GetBool("reverse")
	limit, _ := cmd.Flags().GetInt("limit")
	in.filter.Limit = limit
	gatherSearchIdentityFilters(cmd, &in.filter)
	gatherSearchPatternFilters(cmd, &in.filter)
	if err := gatherSearchTimeFilters(cmd, &in.filter); err != nil {
		return in, err
	}
	if err := gatherSearchPriorityFilters(cmd, &in.filter); err != nil {
		return in, err
	}
	if err := gatherSearchMetadataFilters(cmd, &in.filter); err != nil {
		return in, err
	}
	return in, nil
}

func gatherSearchIdentityFilters(cmd *cobra.Command, filter *types.IssueFilter) {
	assignee, _ := cmd.Flags().GetString("assignee")
	issueType, _ := cmd.Flags().GetString("type")
	labels, _ := cmd.Flags().GetStringSlice("label")
	labelsAny, _ := cmd.Flags().GetStringSlice("label-any")
	labels = utils.NormalizeLabels(labels)
	labelsAny = utils.NormalizeLabels(labelsAny)
	if assignee != "" {
		filter.Assignee = &assignee
	}
	if issueType != "" {
		t := types.IssueType(issueType)
		filter.IssueType = &t
	}
	if len(labels) > 0 {
		filter.Labels = labels
	}
	if len(labelsAny) > 0 {
		filter.LabelsAny = labelsAny
	}
}

func gatherSearchPatternFilters(cmd *cobra.Command, filter *types.IssueFilter) {
	descContains, _ := cmd.Flags().GetString("desc-contains")
	notesContains, _ := cmd.Flags().GetString("notes-contains")
	externalContains, _ := cmd.Flags().GetString("external-contains")
	emptyDesc, _ := cmd.Flags().GetBool("empty-description")
	noAssignee, _ := cmd.Flags().GetBool("no-assignee")
	noLabels, _ := cmd.Flags().GetBool("no-labels")
	if descContains != "" {
		filter.DescriptionContains = descContains
	}
	if notesContains != "" {
		filter.NotesContains = notesContains
	}
	if externalContains != "" {
		filter.ExternalRefContains = externalContains
	}
	if emptyDesc {
		filter.EmptyDescription = true
	}
	if noAssignee {
		filter.NoAssignee = true
	}
	if noLabels {
		filter.NoLabels = true
	}
}

func gatherSearchTimeFilters(cmd *cobra.Command, filter *types.IssueFilter) error {
	return runListInputStages(
		func() error { return applySearchTimeFlag(cmd, "created-after", &filter.CreatedAfter) },
		func() error { return applySearchTimeFlag(cmd, "created-before", &filter.CreatedBefore) },
		func() error { return applySearchTimeFlag(cmd, "updated-after", &filter.UpdatedAfter) },
		func() error { return applySearchTimeFlag(cmd, "updated-before", &filter.UpdatedBefore) },
		func() error { return applySearchTimeFlag(cmd, "closed-after", &filter.ClosedAfter) },
		func() error { return applySearchTimeFlag(cmd, "closed-before", &filter.ClosedBefore) },
	)
}

func applySearchTimeFlag(cmd *cobra.Command, name string, dest **time.Time) error {
	raw, _ := cmd.Flags().GetString(name)
	if raw == "" {
		return nil
	}
	t, err := parseTimeFlag(raw)
	if err != nil {
		return HandleErrorRespectJSON("parsing --%s: %v", name, err)
	}
	*dest = &t
	return nil
}

func gatherSearchPriorityFilters(cmd *cobra.Command, filter *types.IssueFilter) error {
	if err := applySearchPriorityFlag(cmd, "priority-min", &filter.PriorityMin); err != nil {
		return err
	}
	return applySearchPriorityFlag(cmd, "priority-max", &filter.PriorityMax)
}

func applySearchPriorityFlag(cmd *cobra.Command, name string, dest **int) error {
	if !cmd.Flags().Changed(name) {
		return nil
	}
	raw, _ := cmd.Flags().GetString(name)
	priority, err := validation.ValidatePriority(raw)
	if err != nil {
		return HandleErrorRespectJSON("parsing --%s: %v", name, err)
	}
	*dest = &priority
	return nil
}

func gatherSearchMetadataFilters(cmd *cobra.Command, filter *types.IssueFilter) error {
	metadataFieldFlags, _ := cmd.Flags().GetStringArray("metadata-field")
	if err := applySearchMetadataFields(metadataFieldFlags, filter); err != nil {
		return err
	}
	hasMetadataKey, _ := cmd.Flags().GetString("has-metadata-key")
	if hasMetadataKey == "" {
		return nil
	}
	if err := storage.ValidateMetadataKey(hasMetadataKey); err != nil {
		return HandleErrorRespectJSON("invalid --has-metadata-key: %v", err)
	}
	filter.HasMetadataKey = hasMetadataKey
	return nil
}

func applySearchMetadataFields(flags []string, filter *types.IssueFilter) error {
	if len(flags) == 0 {
		return nil
	}
	filter.MetadataFields = make(map[string]string, len(flags))
	for _, mf := range flags {
		k, v, ok := strings.Cut(mf, "=")
		if !ok || k == "" {
			return HandleErrorRespectJSON("invalid --metadata-field: expected key=value, got %q", mf)
		}
		if err := storage.ValidateMetadataKey(k); err != nil {
			return HandleErrorRespectJSON("invalid --metadata-field key: %v", err)
		}
		filter.MetadataFields[k] = v
	}
	return nil
}

func applySearchProxiedStatus(ctx context.Context, uw uow.UnitOfWork, status string, filter *types.IssueFilter) error {
	if status == "" || status == "all" {
		return nil
	}
	cfg, err := workapi.LoadUOWListConfig(ctx, uw)
	if err != nil {
		return HandleErrorRespectJSON("loading status configuration: %v", err)
	}
	if err := workapi.ApplyStatusFilter(filter, status, cfg.CustomStatusNames()); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	return nil
}

func runSearchProxiedJSON(ctx context.Context, uw uow.UnitOfWork, query string, in searchProxiedInput) error {
	page, err := uw.IssueUseCase().SearchIssuesWithCounts(ctx, query, in.filter)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	items := page.Items
	workapi.SortIssuesWithCounts(items, in.sortBy, in.reverse)
	if items == nil {
		items = []*types.IssueWithCounts{}
	}
	return outputJSON(items)
}

func runSearchProxiedHuman(ctx context.Context, uw uow.UnitOfWork, query string, in searchProxiedInput) error {
	page, err := uw.IssueUseCase().SearchIssues(ctx, query, in.filter)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	issues := page.Items
	workapi.SortIssues(issues, in.sortBy, in.reverse)
	outputSearchResults(issues, query, in.longFormat)
	return nil
}
