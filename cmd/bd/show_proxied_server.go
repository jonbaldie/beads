package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/uimd"
	"github.com/jonbaldie/beads/internal/workapi"
	"github.com/jonbaldie/beads/issueops"
)

type showProxiedInput struct {
	ids             []string
	thread          bool
	shortMode       bool
	longMode        bool
	refs            bool
	children        bool
	asOfRef         string
	localTime       bool
	watchMode       bool
	currentMode     bool
	includeDepends  bool
	briefDeps       bool
	includeComments bool
}

func gatherShowProxiedInput(cmd *cobra.Command, args []string) *showProxiedInput {
	in := &showProxiedInput{}
	in.thread, _ = cmd.Flags().GetBool("thread")
	in.shortMode, _ = cmd.Flags().GetBool("short")
	in.longMode, _ = cmd.Flags().GetBool("long")
	in.refs, _ = cmd.Flags().GetBool("refs")
	in.children, _ = cmd.Flags().GetBool("children")
	in.asOfRef, _ = cmd.Flags().GetString("as-of")
	in.localTime, _ = cmd.Flags().GetBool("local-time")
	in.watchMode, _ = cmd.Flags().GetBool("watch")
	in.currentMode, _ = cmd.Flags().GetBool("current")
	in.includeDepends, _ = cmd.Flags().GetBool("include-dependents")
	in.briefDeps, _ = cmd.Flags().GetBool("brief-deps")
	in.includeComments, _ = cmd.Flags().GetBool("include-comments")

	idFlags, _ := cmd.Flags().GetStringArray("id")
	in.ids = append(in.ids, args...)
	in.ids = append(in.ids, idFlags...)
	return in
}

func proxiedOpenReadUOW(ctx context.Context) (uow.UnitOfWork, error) {
	if getUOWProvider() == nil {
		return nil, HandleError("proxied-server UOW provider not initialized")
	}
	uw, err := getUOWProvider().NewUOW(ctx)
	if err != nil {
		return nil, HandleErrorRespectJSON("open unit of work: %v", err)
	}
	return uw, nil
}

// proxiedIssueReader hands back the guarded issue-query surface for the
// proxied-server provider, through the provider's OWN capability accessor —
// the same two-step a direct command performs on a store.
//
// The accessor is the door and there is no other: the cmd-bd-role-constructors
// depguard rule keeps the shared implementation's constructor out of cmd/bd
// entirely, because a decorator adds its layer in its own accessor and a
// command that built a reader directly would get an undecorated one. A
// provider that cannot answer says so with an error rather than being wired
// around.
func proxiedIssueReader() (issueops.Reader, error) {
	if getUOWProvider() == nil {
		return nil, errors.New("proxied-server UOW provider not initialized")
	}
	src, ok := getUOWProvider().(uow.IssueReaderSource)
	if !ok {
		return nil, fmt.Errorf("proxied-server provider %T does not offer the issue-query surface", getUOWProvider())
	}
	return src.IssueReader()
}

func runShowProxiedServer(cmd *cobra.Command, ctx context.Context, args []string) error {
	in := gatherShowProxiedInput(cmd, args)

	if in.watchMode {
		return HandleErrorRespectJSON("watch mode not supported in proxied-server mode")
	}

	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return err
	}
	defer uw.Close(ctx)

	if err := prepareShowProxiedInput(ctx, uw, in); err != nil {
		return err
	}
	return dispatchShowProxied(ctx, uw, in)
}

func prepareShowProxiedInput(ctx context.Context, uw uow.UnitOfWork, in *showProxiedInput) error {
	if in.currentMode {
		if len(in.ids) > 0 {
			return HandleErrorRespectJSON("--current cannot be combined with explicit issue IDs")
		}
		currentID := resolveCurrentIssueIDProxied(ctx, uw)
		if currentID == "" {
			return HandleErrorRespectJSON("no current issue found (no in-progress, hooked, or recently touched issues)")
		}
		in.ids = []string{currentID}
	}
	if len(in.ids) == 0 {
		return HandleErrorRespectJSON("at least one issue ID is required (use positional args, --id flag, or --current)")
	}
	return nil
}

func dispatchShowProxied(ctx context.Context, uw uow.UnitOfWork, in *showProxiedInput) error {
	switch {
	case in.asOfRef != "":
		runShowProxiedAsOf(ctx, uw, in)
	case in.thread:
		return runShowProxiedThread(ctx, uw, in)
	case in.refs:
		runShowProxiedRefs(ctx, uw, in)
	case in.children:
		runShowProxiedChildren(ctx, uw, in)
	default:
		return runShowProxiedDefault(ctx, uw, in)
	}
	return nil
}

// proxiedListDeps and proxiedGetComments stay CLI-local: they feed the
// terminal rendering below, which is presentation, not the shared detail
// shape. The domain-shaped reads live in internal/workapi.
func proxiedListDeps(ctx context.Context, uw uow.UnitOfWork, id string, isWisp bool, filter domain.DepListFilter) ([]*types.IssueWithDependencyMetadata, error) {
	if isWisp {
		return uw.DependencyUseCase().ListWispWithIssueMetadata(ctx, id, filter)
	}
	return uw.DependencyUseCase().ListWithIssueMetadata(ctx, id, filter)
}

func proxiedGetComments(ctx context.Context, uw uow.UnitOfWork, id string, isWisp bool) ([]*types.Comment, error) {
	if isWisp {
		return uw.CommentUseCase().GetCommentsForWisp(ctx, id)
	}
	return uw.CommentUseCase().GetCommentsForIssue(ctx, id)
}

// reportIssueLookupFailure prints the stderr line for a failed issue lookup,
// keeping "no such issue" distinct from a backend that fell over. Before the
// lookup normalized its sentinel, proxied mode printed the raw
// "sql: no rows in result set" for a missing id and had no way to tell the
// two apart at all.
func reportIssueLookupFailure(verb, id string, err error) {
	if errors.Is(err, storage.ErrNotFound) {
		fmt.Fprintf(os.Stderr, "Issue %s not found\n", id)
		return
	}
	fmt.Fprintf(os.Stderr, "Error %s %s: %v\n", verb, id, err)
}

func runShowProxiedAsOf(ctx context.Context, uw uow.UnitOfWork, in *showProxiedInput) {
	var jsonIssues []*types.Issue
	for idx, id := range in.ids {
		issue, includeInJSON := renderShowProxiedAsOfIssue(ctx, uw, in, id, idx)
		if includeInJSON {
			jsonIssues = append(jsonIssues, issue)
		}
	}
	if isJSONOutput() && len(jsonIssues) > 0 {
		_ = outputJSON(jsonIssues)
	}
}

func renderShowProxiedAsOfIssue(ctx context.Context, uw uow.UnitOfWork, in *showProxiedInput, id string, idx int) (*types.Issue, bool) {
	issue, err := uw.IssueUseCase().AsOf(ctx, id, in.asOfRef)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching %s as of %s: %v\n", id, in.asOfRef, err)
		return nil, false
	}
	if issue == nil {
		fmt.Fprintf(os.Stderr, "Issue %s did not exist at %s\n", id, in.asOfRef)
		return nil, false
	}
	if in.shortMode {
		fmt.Println(formatShortIssue(issue))
		return nil, false
	}
	if isJSONOutput() {
		return issue, true
	}
	if idx > 0 {
		fmt.Println("\n" + ui.RenderMuted(strings.Repeat("-", 60)))
	}
	fmt.Printf("\n%s (as of %s)\n", formatIssueHeader(issue), ui.RenderMuted(in.asOfRef))
	fmt.Println(formatIssueMetadata(issue))
	if issue.Description != "" {
		fmt.Printf("\n%s\n%s\n", ui.RenderBold("DESCRIPTION"), uimd.RenderMarkdown(issue.Description))
	}
	fmt.Println()
	return nil, false
}

func runShowProxiedRefs(ctx context.Context, uw uow.UnitOfWork, in *showProxiedInput) {
	src := workapi.NewUOWDetailSource(uw)
	allRefs := make(map[string][]*types.IssueWithDependencyMetadata, len(in.ids))
	for _, id := range in.ids {
		_, isWisp, err := workapi.GetIssueOrWisp(ctx, src, id)
		if err != nil {
			reportIssueLookupFailure("resolving", id, err)
			continue
		}
		refs, err := proxiedListDeps(ctx, uw, id, isWisp, domain.DepListFilter{Direction: domain.DepDirectionIn})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting refs for %s: %v\n", id, err)
			continue
		}
		allRefs[id] = refs
	}

	if isJSONOutput() {
		_ = outputJSON(allRefs)
		return
	}
	for id, refs := range allRefs {
		if len(refs) == 0 {
			fmt.Printf("\n%s: No references found\n", ui.RenderAccent(id))
			continue
		}
		fmt.Printf("\n%s References to %s:\n", ui.RenderAccent("📎"), id)
		for _, sec := range groupDepSections(refs, false, nil) {
			displayRefGroup(sec)
		}
		fmt.Println()
	}
}

func runShowProxiedChildren(ctx context.Context, uw uow.UnitOfWork, in *showProxiedInput) {
	src := workapi.NewUOWDetailSource(uw)
	allChildren := make(map[string][]*types.IssueWithDependencyMetadata, len(in.ids))
	for _, id := range in.ids {
		children, ok := loadProxiedChildren(ctx, uw, src, id)
		if ok {
			allChildren[id] = children
		}
	}

	if isJSONOutput() {
		_ = outputJSON(allChildren)
		return
	}
	displayProxiedChildren(in, allChildren)
}

func loadProxiedChildren(ctx context.Context, uw uow.UnitOfWork, src workapi.DetailSource, id string) ([]*types.IssueWithDependencyMetadata, bool) {
	_, isWisp, err := workapi.GetIssueOrWisp(ctx, src, id)
	if err != nil {
		reportIssueLookupFailure("resolving", id, err)
		return nil, false
	}
	kids, err := proxiedListDeps(ctx, uw, id, isWisp, domain.DepListFilter{
		Types:     []types.DependencyType{types.DepParentChild},
		Direction: domain.DepDirectionIn,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting children for %s: %v\n", id, err)
		return nil, false
	}
	if kids == nil {
		kids = []*types.IssueWithDependencyMetadata{}
	}
	return kids, true
}

func displayProxiedChildren(in *showProxiedInput, allChildren map[string][]*types.IssueWithDependencyMetadata) {
	for id, kids := range allChildren {
		if len(kids) == 0 {
			fmt.Printf("%s: No children found\n", ui.RenderAccent(id))
			continue
		}
		fmt.Printf("%s Children of %s (%d):\n", ui.RenderAccent("↳"), id, len(kids))
		for _, c := range kids {
			if in.shortMode {
				fmt.Printf("  %s\n", formatShortIssue(&c.Issue))
			} else {
				fmt.Println(formatDependencyLine("↳", c))
			}
		}
		fmt.Println()
	}
}

func runShowProxiedThread(ctx context.Context, uw uow.UnitOfWork, in *showProxiedInput) error {
	if len(in.ids) == 0 {
		return nil
	}
	rootMsg, err := loadProxiedThreadRoot(ctx, uw, in.ids[0])
	if err != nil {
		return err
	}
	rootMsg = findProxiedThreadRoot(ctx, uw, rootMsg)
	threadMessages, repliesTo := collectProxiedThread(ctx, uw, rootMsg)

	slices.SortFunc(threadMessages, func(a, b *types.Issue) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})

	if isJSONOutput() {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(threadMessages)
		return nil
	}
	renderProxiedThread(rootMsg, threadMessages, repliesTo)
	return nil
}

func loadProxiedThreadRoot(ctx context.Context, uw uow.UnitOfWork, id string) (*types.Issue, error) {
	startMsg, _, err := workapi.GetIssueOrWisp(ctx, workapi.NewUOWDetailSource(uw), id)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, HandleErrorRespectJSON("message %s not found", id)
	}
	if err != nil {
		return nil, HandleErrorRespectJSON("fetching message %s: %v", id, err)
	}
	return startMsg, nil
}

func findProxiedThreadRoot(ctx context.Context, uw uow.UnitOfWork, startMsg *types.Issue) *types.Issue {
	rootMsg := startMsg
	seen := map[string]bool{rootMsg.ID: true}
	for {
		parentID := proxiedFindRepliesTo(ctx, uw, rootMsg.ID)
		if parentID == "" || seen[parentID] {
			break
		}
		seen[parentID] = true
		parentMsg, _ := uw.IssueUseCase().GetIssue(ctx, parentID)
		if parentMsg == nil {
			parentMsg, _ = uw.IssueUseCase().GetWisp(ctx, parentID)
		}
		if parentMsg == nil {
			break
		}
		rootMsg = parentMsg
	}
	return rootMsg
}

func collectProxiedThread(ctx context.Context, uw uow.UnitOfWork, rootMsg *types.Issue) ([]*types.Issue, map[string]string) {
	threadMessages := []*types.Issue{rootMsg}
	threadIDs := map[string]bool{rootMsg.ID: true}
	repliesTo := map[string]string{}
	queue := []string{rootMsg.ID}
	queueIndex := 0
	for {
		if queueIndex >= len(queue) {
			break
		}
		current := queue[queueIndex]
		queueIndex++
		replies := proxiedFindReplies(ctx, uw, current)
		for _, reply := range replies {
			if threadIDs[reply.ID] {
				continue
			}
			r := reply
			threadMessages = append(threadMessages, &r)
			threadIDs[reply.ID] = true
			repliesTo[reply.ID] = current
			queue = append(queue, reply.ID)
		}
	}
	return threadMessages, repliesTo
}

func renderProxiedThread(rootMsg *types.Issue, threadMessages []*types.Issue, repliesTo map[string]string) {
	fmt.Printf("\n%s Thread: %s\n", ui.RenderAccent("📬"), rootMsg.Title)
	fmt.Println(strings.Repeat("─", 66))
	for _, msg := range threadMessages {
		depth := 0
		parent := repliesTo[msg.ID]
		for parent != "" && depth < 5 {
			depth++
			parent = repliesTo[parent]
		}
		indent := strings.Repeat("  ", depth)
		timeStr := msg.CreatedAt.Format("2006-01-02 15:04")
		statusIcon := "📧"
		if msg.Status == types.StatusClosed {
			statusIcon = "✓"
		}
		fmt.Printf("%s%s %s %s\n", indent, statusIcon, ui.RenderAccent(msg.ID), ui.RenderMuted(timeStr))
		fmt.Printf("%s  From: %s  To: %s\n", indent, msg.Sender, msg.Assignee)
		if parentID := repliesTo[msg.ID]; parentID != "" {
			fmt.Printf("%s  Re: %s\n", indent, parentID)
		}
		fmt.Printf("%s  %s: %s\n", indent, ui.RenderMuted("Subject"), msg.Title)
		if msg.Description != "" {
			for _, line := range strings.Split(msg.Description, "\n") {
				fmt.Printf("%s  %s\n", indent, line)
			}
		}
		fmt.Println()
	}
	fmt.Printf("Total: %d messages in thread\n\n", len(threadMessages))
}

func proxiedFindRepliesTo(ctx context.Context, uw uow.UnitOfWork, id string) string {
	deps, err := uw.DependencyUseCase().ListWithIssueMetadata(ctx, id, domain.DepListFilter{
		Types:     []types.DependencyType{types.DepRepliesTo},
		Direction: domain.DepDirectionOut,
	})
	if err != nil || len(deps) == 0 {
		return ""
	}
	return deps[0].ID
}

func proxiedFindReplies(ctx context.Context, uw uow.UnitOfWork, id string) []types.Issue {
	deps, err := uw.DependencyUseCase().ListWithIssueMetadata(ctx, id, domain.DepListFilter{
		Types:     []types.DependencyType{types.DepRepliesTo},
		Direction: domain.DepDirectionIn,
	})
	if err != nil {
		return nil
	}
	out := make([]types.Issue, 0, len(deps))
	for _, d := range deps {
		out = append(out, d.Issue)
	}
	return out
}
