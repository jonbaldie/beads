package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/uimd"
	"github.com/jonbaldie/beads/internal/workapi"
	"github.com/jonbaldie/beads/issueops"
)

func runShowProxiedDefault(ctx context.Context, uw uow.UnitOfWork, in *showProxiedInput) error {
	formatTime := func(t time.Time) string {
		if in.localTime {
			t = t.Local()
		}
		return t.Format("2006-01-02 15:04")
	}

	rd, err := proxiedReaderForShow(in)
	if err != nil {
		return err
	}

	src := workapi.NewUOWDetailSource(uw)
	allDetails, foundCount, err := collectProxiedDefaultResults(ctx, uw, src, rd, in, formatTime)
	if err != nil {
		return err
	}
	return finishProxiedDefaultOutput(allDetails, foundCount)
}

func proxiedReaderForShow(in *showProxiedInput) (issueops.Reader, error) {
	// Shaping the detail view belongs to the reader role, not the CLI, and
	// this route reaches it the same way the direct one does: through the
	// provider's own accessor. The count-only default (be-ijck6q), the
	// comments-omitted flag (ga-clgh) and the shallow dependent rows
	// (be-4d36f2) then read the same from every frontend by construction
	// rather than by everyone remembering to call the same helper. The
	// role opens one unit of work per call; the one this function holds stays
	// for the terminal rendering below, which is not on the contract.
	if !isJSONOutput() || in.shortMode {
		return nil, nil
	}
	rd, err := proxiedIssueReader()
	if err != nil {
		return nil, HandleErrorRespectJSON("%v", err)
	}
	return rd, nil
}

func collectProxiedDefaultResults(ctx context.Context, uw uow.UnitOfWork, src workapi.DetailSource, rd issueops.Reader, in *showProxiedInput, formatTime func(time.Time) string) ([]interface{}, int, error) {
	var allDetails []interface{}
	foundCount := 0
	for idx, id := range in.ids {
		if rd != nil {
			details, found, err := loadProxiedReaderDetails(ctx, rd, in.getRequest(id), id)
			if err != nil {
				return nil, foundCount, HandleErrorRespectJSON("%v", err)
			}
			if found {
				foundCount++
				allDetails = append(allDetails, details)
			}
			continue
		}

		found := renderProxiedDefaultIssue(ctx, uw, src, in, id, idx, formatTime)
		if found {
			foundCount++
		}
	}
	return allDetails, foundCount, nil
}

func loadProxiedReaderDetails(ctx context.Context, rd issueops.Reader, request issueops.GetRequest, id string) (interface{}, bool, error) {
	details, err := rd.Get(ctx, request)
	if err == nil {
		return details, true, nil
	}
	if errors.Is(err, storage.ErrNotFound) {
		reportIssueLookupFailure("fetching", id, err)
		return nil, false, nil
	}
	return nil, false, err
}

func renderProxiedDefaultIssue(ctx context.Context, uw uow.UnitOfWork, src workapi.DetailSource, in *showProxiedInput, id string, idx int, formatTime func(time.Time) string) bool {
	issue, isWisp, err := workapi.GetIssueOrWisp(ctx, src, id)
	if err != nil {
		reportIssueLookupFailure("fetching", id, err)
		return false
	}
	if in.shortMode {
		fmt.Println(formatShortIssue(issue))
		return true
	}
	proxiedRenderIssue(ctx, uw, issue, isWisp, in, idx, formatTime)
	return true
}

func finishProxiedDefaultOutput(allDetails []interface{}, foundCount int) error {
	if isJSONOutput() {
		if len(allDetails) > 0 {
			_ = outputJSON(allDetails)
			return nil
		}
		return HandleErrorRespectJSON("no issues found matching the provided IDs")
	}
	if foundCount == 0 {
		return SilentExit()
	}
	return nil
}

func proxiedRenderIssue(ctx context.Context, uw uow.UnitOfWork, issue *types.Issue, isWisp bool, in *showProxiedInput, idx int, formatTime func(time.Time) string) {
	renderProxiedIssueHeader(issue, idx)
	renderProxiedIssueContent(issue)
	renderProxiedIssueLabels(ctx, uw, issue, isWisp)
	renderProxiedIssueRelationships(ctx, uw, issue, isWisp)
	renderProxiedIssueComments(ctx, uw, issue, isWisp, formatTime)
	if in.longMode {
		fmt.Print(formatIssueLongExtras(issue, formatTime))
	}
	fmt.Println()
}

func renderProxiedIssueHeader(issue *types.Issue, idx int) {
	if idx > 0 {
		fmt.Println("\n" + ui.RenderMuted(strings.Repeat("─", 60)))
		fmt.Printf("\n%s\n", formatIssueHeader(issue))
	} else {
		fmt.Printf("%s\n", formatIssueHeader(issue))
	}
	fmt.Println(formatIssueMetadata(issue))
}

func renderProxiedIssueContent(issue *types.Issue) {
	if issue.Description != "" {
		fmt.Printf("\n%s\n%s\n", ui.RenderBold("DESCRIPTION"), uimd.RenderMarkdown(issue.Description))
	} else {
		fmt.Printf("\n%s\n  %s\n", ui.RenderBold("DESCRIPTION"), ui.RenderMuted("(none)"))
	}
	if issue.Design != "" {
		fmt.Printf("\n%s\n%s\n", ui.RenderBold("DESIGN"), uimd.RenderMarkdown(issue.Design))
	}
	if issue.Notes != "" {
		fmt.Printf("\n%s\n%s\n", ui.RenderBold("NOTES"), uimd.RenderMarkdown(issue.Notes))
	}
	if issue.AcceptanceCriteria != "" {
		fmt.Printf("\n%s\n%s\n", ui.RenderBold("ACCEPTANCE CRITERIA"), uimd.RenderMarkdown(issue.AcceptanceCriteria))
	}
}

func renderProxiedIssueLabels(ctx context.Context, uw uow.UnitOfWork, issue *types.Issue, isWisp bool) {
	// A READ on an ALTERNATE view. `bd show`'s detail view is on
	// issueops.Reader on both routes and gets its labels hydrated there; this
	// renderer serves --refs, --children, --thread and --as-of, which answer
	// with shapes the Reader contract does not describe, from a unit of work
	// the caller already holds and has already read the issue from. Asking the
	// role here would open a second transaction to re-fetch a row this function
	// was handed. Alternate views reaching roles of their own is the follow-up
	// (ga-2ltro.12).
	var labels []string
	if isWisp {
		labels, _ = uw.LabelUseCase().GetWispLabels(ctx, issue.ID) //nolint:forbidigo // alternate view, caller-owned UOW; the detail view is on the role
	} else {
		labels, _ = uw.LabelUseCase().GetLabels(ctx, issue.ID) //nolint:forbidigo // alternate view, caller-owned UOW; the detail view is on the role
	}
	if len(labels) > 0 {
		fmt.Printf("\n%s %s\n", ui.RenderBold("LABELS:"), strings.Join(labels, ", "))
	}

	if metaStr := formatIssueCustomMetadata(issue); metaStr != "" {
		fmt.Printf("\n%s\n", metaStr)
	}
}

func renderProxiedIssueRelationships(ctx context.Context, uw uow.UnitOfWork, issue *types.Issue, isWisp bool) {
	relatedSeen := make(map[string]*types.IssueWithDependencyMetadata)

	depsWithMeta, _ := proxiedListDeps(ctx, uw, issue.ID, isWisp, domain.DepListFilter{Direction: domain.DepDirectionOut})
	for _, sec := range groupDepSections(depsWithMeta, true, relatedSeen) {
		printDepSection(sec)
	}

	dependentsWithMeta, _ := proxiedListDeps(ctx, uw, issue.ID, isWisp, domain.DepListFilter{Direction: domain.DepDirectionIn})
	for _, sec := range groupDepSections(dependentsWithMeta, false, relatedSeen) {
		printDepSection(sec)
		if sec.Type == types.DepParentChild && issue.IssueType == types.TypeEpic {
			printEpicChildProgress(sec.Deps)
		}
	}

	printRelatedSection(relatedSeen)
}

func renderProxiedIssueComments(ctx context.Context, uw uow.UnitOfWork, issue *types.Issue, isWisp bool, formatTime func(time.Time) string) {
	comments, _ := proxiedGetComments(ctx, uw, issue.ID, isWisp)
	if len(comments) > 0 {
		fmt.Printf("\n%s\n", ui.RenderBold("COMMENTS"))
		for _, c := range comments {
			fmt.Printf("  %s %s\n", ui.RenderMuted(formatTime(c.CreatedAt)), c.Author)
			rendered := uimd.RenderMarkdown(c.Text)
			for _, line := range strings.Split(strings.TrimRight(rendered, "\n"), "\n") {
				fmt.Printf("    %s\n", line)
			}
		}
	}
}

// getRequest carries the proxied show flags onto the read contract. See
// showGetRequest: the two routes build this independently.
func (in *showProxiedInput) getRequest(id string) issueops.GetRequest {
	return showGetRequest(id, in.includeDepends, in.includeComments, in.briefDeps)
}
