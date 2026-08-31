package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/uimd"
	"github.com/spf13/cobra"
)

type showOptions struct {
	showThread      bool
	shortMode       bool
	longMode        bool
	showRefs        bool
	showChildren    bool
	asOfRef         string
	idFlags         []string
	localTime       bool
	watchMode       bool
	currentMode     bool
	includeDepends  bool
	includeComments bool
	briefDeps       bool
}

func runShowCommand(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("show")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	if usesProxiedServer() {
		return runShowProxiedServer(cmd, getRootContext(), args)
	}
	opts := readShowOptions(cmd)
	args, err := prepareShowArgs(getRootContext(), args, opts)
	if err != nil {
		return err
	}
	return runDirectShow(cmd, args, opts)
}

func readShowOptions(cmd *cobra.Command) showOptions {
	flags := cmd.Flags()
	showThread, _ := flags.GetBool("thread")
	shortMode, _ := flags.GetBool("short")
	longMode, _ := flags.GetBool("long")
	showRefs, _ := flags.GetBool("refs")
	showChildren, _ := flags.GetBool("children")
	asOfRef, _ := flags.GetString("as-of")
	idFlags, _ := flags.GetStringArray("id")
	localTime, _ := flags.GetBool("local-time")
	watchMode, _ := flags.GetBool("watch")
	currentMode, _ := flags.GetBool("current")
	includeDepends, _ := flags.GetBool("include-dependents")
	includeComments, _ := flags.GetBool("include-comments")
	briefDeps, _ := flags.GetBool("brief-deps")
	return showOptions{
		showThread: showThread, shortMode: shortMode, longMode: longMode,
		showRefs: showRefs, showChildren: showChildren, asOfRef: asOfRef,
		idFlags: idFlags, localTime: localTime, watchMode: watchMode,
		currentMode: currentMode, includeDepends: includeDepends,
		includeComments: includeComments, briefDeps: briefDeps,
	}
}

func prepareShowArgs(ctx context.Context, args []string, opts showOptions) ([]string, error) {
	args = append(args, opts.idFlags...)
	if opts.currentMode {
		if len(args) > 0 {
			return nil, HandleErrorRespectJSON("--current cannot be combined with explicit issue IDs")
		}
		currentID := resolveCurrentIssueID(ctx)
		if currentID == "" {
			return nil, HandleErrorRespectJSON("no current issue found (no in-progress, hooked, or recently touched issues)")
		}
		args = []string{currentID}
	}
	if len(args) == 0 {
		return nil, HandleErrorRespectJSON("at least one issue ID is required (use positional args, --id flag, or --current)")
	}
	return args, nil
}

func runDirectShow(cmd *cobra.Command, args []string, opts showOptions) error {
	ctx := getRootContext()
	if opts.asOfRef != "" {
		return showIssueAsOf(ctx, args, opts.asOfRef, opts.shortMode)
	}
	if opts.watchMode {
		return runShowWatch(ctx, args)
	}
	if handled, err := runShowThread(cmd, ctx, args, opts.showThread); handled {
		return err
	}
	if opts.showRefs {
		return showIssueRefs(ctx, args, isJSONOutput())
	}
	if opts.showChildren {
		return showIssueChildren(ctx, args, isJSONOutput(), opts.shortMode)
	}
	return runShowDefault(ctx, args, opts)
}

func runShowWatch(ctx context.Context, args []string) error {
	if err := ensureDirectMode("watch mode requires direct database access"); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	if len(args) != 1 {
		return HandleErrorRespectJSON("watch mode requires exactly one issue ID")
	}
	watchIssue(ctx, args[0])
	return nil
}

func runShowThread(_ *cobra.Command, ctx context.Context, args []string, enabled bool) (bool, error) {
	if !enabled || len(args) == 0 {
		return false, nil
	}
	result, err := resolveAndGetIssueWithRouting(ctx, getStore(), args[0])
	if result != nil {
		defer result.Close()
	}
	if err == nil && result != nil && result.ResolvedID != "" {
		return true, showMessageThread(ctx, result.ResolvedID, isJSONOutput())
	}
	return false, nil
}

func runShowDefault(ctx context.Context, args []string, opts showOptions) error {
	var allDetails []interface{}
	foundCount := 0
	for idx, id := range args {
		detail, found, err := showOneIssue(ctx, id, idx, opts)
		if err != nil {
			return err
		}
		if found {
			foundCount++
		}
		if detail != nil {
			allDetails = append(allDetails, detail)
		}
	}
	return finishShow(args, allDetails, foundCount)
}

func showOneIssue(ctx context.Context, id string, idx int, opts showOptions) (interface{}, bool, error) {
	result, err := resolveAndGetIssueWithRouting(ctx, getStore(), id)
	if result != nil {
		defer result.Close()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching %s: %v\n", id, err)
		return nil, false, nil
	}
	if result == nil || result.Issue == nil {
		fmt.Fprintf(os.Stderr, "Issue %s not found\n", id)
		return nil, false, nil
	}
	if opts.shortMode {
		fmt.Println(formatShortIssue(result.Issue))
		return nil, true, nil
	}
	if isJSONOutput() {
		detail, err := getShowDetail(ctx, result.Store, result.Issue, opts)
		return detail, true, err
	}
	renderShowIssue(ctx, result.Store, result.Issue, idx, opts)
	return nil, true, nil
}

func getShowDetail(ctx context.Context, issueStore storage.DoltStorage, issue *types.Issue, opts showOptions) (interface{}, error) {
	rd, err := issueStore.IssueReader()
	if err != nil {
		return nil, HandleErrorRespectJSON("%v", err)
	}
	details, err := rd.Get(ctx, showGetRequest(issue.ID, opts.includeDepends, opts.includeComments, opts.briefDeps))
	if err != nil {
		return nil, HandleErrorRespectJSON("%v", err)
	}
	return details, nil
}

func renderShowIssue(ctx context.Context, issueStore storage.DoltStorage, issue *types.Issue, idx int, opts showOptions) {
	if idx > 0 {
		fmt.Println("\n" + ui.RenderMuted(strings.Repeat("─", 60)))
	}
	fmt.Printf("%s\n", formatIssueHeader(issue))
	fmt.Println(formatIssueMetadata(issue))
	renderShowContent(issue)
	renderShowLabels(ctx, issueStore, issue)
	renderShowRelations(ctx, issueStore, issue)
	renderShowComments(ctx, issueStore, issue, opts.localTime)
	if opts.longMode {
		fmt.Print(formatIssueLongExtras(issue, showTimeFormatter(opts.localTime)))
	}
	fmt.Println()
}

func renderShowContent(issue *types.Issue) {
	if issue.Description != "" {
		fmt.Printf("\n%s\n%s\n", ui.RenderBold("DESCRIPTION"), uimd.RenderMarkdown(issue.Description))
	} else {
		fmt.Printf("\n%s\n  %s\n", ui.RenderBold("DESCRIPTION"), ui.RenderMuted("(none)"))
	}
	renderShowField("DESIGN", issue.Design)
	renderShowField("NOTES", issue.Notes)
	renderShowField("ACCEPTANCE CRITERIA", issue.AcceptanceCriteria)
}

func renderShowField(label, value string) {
	if value != "" {
		fmt.Printf("\n%s\n%s\n", ui.RenderBold(label), uimd.RenderMarkdown(value))
	}
}

func renderShowLabels(ctx context.Context, issueStore storage.DoltStorage, issue *types.Issue) {
	labels, _ := issueStore.GetLabels(ctx, issue.ID)
	if len(labels) > 0 {
		fmt.Printf("\n%s %s\n", ui.RenderBold("LABELS:"), strings.Join(labels, ", "))
	}
	if metaStr := formatIssueCustomMetadata(issue); metaStr != "" {
		fmt.Printf("\n%s\n", metaStr)
	}
}

func renderShowRelations(ctx context.Context, issueStore storage.DoltStorage, issue *types.Issue) {
	relatedSeen := make(map[string]*types.IssueWithDependencyMetadata)
	deps, _ := issueStore.GetDependenciesWithMetadata(ctx, issue.ID)
	for _, sec := range groupDepSections(deps, true, relatedSeen) {
		printDepSection(sec)
	}
	dependents, _ := issueStore.GetDependentsWithMetadata(ctx, issue.ID)
	for _, sec := range groupDepSections(dependents, false, relatedSeen) {
		printDepSection(sec)
		if sec.Type == types.DepParentChild && issue.IssueType == types.TypeEpic {
			printEpicChildProgress(sec.Deps)
		}
	}
	printRelatedSection(relatedSeen)
}

func renderShowComments(ctx context.Context, issueStore storage.DoltStorage, issue *types.Issue, localTime bool) {
	comments, _ := issueStore.GetIssueComments(ctx, issue.ID)
	if len(comments) == 0 {
		return
	}
	fmt.Printf("\n%s\n", ui.RenderBold("COMMENTS"))
	formatTime := showTimeFormatter(localTime)
	for _, comment := range comments {
		fmt.Printf("  %s %s\n", ui.RenderMuted(formatTime(comment.CreatedAt)), comment.Author)
		rendered := uimd.RenderMarkdown(comment.Text)
		for _, line := range strings.Split(strings.TrimRight(rendered, "\n"), "\n") {
			fmt.Printf("    %s\n", line)
		}
	}
}

func showTimeFormatter(localTime bool) func(time.Time) string {
	return func(t time.Time) string {
		if localTime {
			t = t.Local()
		}
		return t.Format("2006-01-02 15:04")
	}
}

func finishShow(args []string, allDetails []interface{}, foundCount int) error {
	if isJSONOutput() {
		if len(allDetails) == 0 {
			return HandleErrorRespectJSON("no issues found matching the provided IDs")
		}
		return outputJSON(allDetails)
	}
	if foundCount > 0 {
		maybeShowTip(getStore())
	} else {
		markLastShowID(args)
		return SilentExit()
	}
	markLastShowID(args)
	return nil
}

func markLastShowID(args []string) {
	if len(args) > 0 {
		SetLastTouchedID(args[0])
	}
}
