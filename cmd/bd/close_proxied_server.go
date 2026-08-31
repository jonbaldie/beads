package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/audit"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/workapi"
	"github.com/jonbaldie/beads/issueops"
)

type closeProxiedInput struct {
	force       bool
	continueOn  bool
	noAuto      bool
	suggestNext bool
	claimNext   bool
	session     string
	jsonOut     bool
}

// closeProxiedPreflight is the CLI's own close policy, decided before the
// batch runs: the template/pin/assignee fence, the epic open-children refusal
// and machine-checkable gates. None of the three is library policy — they are
// `bd close`'s, they read the issue and nothing else, and the role has no
// vocabulary for them — so they stay here and the batch is handed only the
// items that survived them.
//
// errors is indexed by ARGUMENT position, not by item, so a refusal reported
// here and one reported by the batch still print in the order the caller typed
// the ids.
type closeProxiedPreflight struct {
	items    []issueops.BatchCloseItem
	itemArgs []int
	before   map[string]*types.Issue
	errors   []string
}

type closeProxiedOutcome struct {
	id          string
	before      *types.Issue
	after       *types.Issue
	closed      bool
	auditOld    string
	auditReason string
}

// closeProxiedPostClose is the work `bd close` does AFTER the closes have
// landed: molecule auto-close, --suggest-next and --continue.
type closeProxiedPostClose struct {
	unblocked      []*types.Issue
	continueResult *ContinueResult
	autoClosedMol  *types.Issue
	warnings       []string
}

func runCloseProxiedServer(cmd *cobra.Command, ctx context.Context, args []string) error {
	reasons, args, in, err := prepareCloseProxied(cmd, args)
	if err != nil {
		return err
	}
	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}
	pre, err := closeProxiedRunPreflight(ctx, args, reasons, in)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	result, err := closeProxiedRunBatch(ctx, pre, in)
	if err != nil {
		return err
	}
	outcomes, closeReasons := closeProxiedOutcomes(&pre, result)
	post := closeProxiedRunPostClose(ctx, args, in, outcomes)
	closeProxiedReport(in, pre, post, outcomes, closeReasons, result)
	if len(args) > 0 && len(outcomes) == 0 {
		return SilentExit()
	}
	return nil
}

func prepareCloseProxied(cmd *cobra.Command, args []string) ([]string, []string, closeProxiedInput, error) {
	var zero closeProxiedInput
	if len(args) == 0 {
		return nil, nil, zero, HandleErrorRespectJSON("no issue ID provided")
	}
	reasons, updatedArgs, err := resolveCloseReasons(cmd, args)
	if err != nil {
		return nil, nil, zero, HandleErrorRespectJSON("%v", err)
	}
	if err := validateCloseReasons(reasons); err != nil {
		return nil, nil, zero, HandleErrorRespectJSON("%v", err)
	}
	in := gatherCloseProxiedInput(cmd)
	if err := validateCloseProxiedModes(in, updatedArgs); err != nil {
		return nil, nil, zero, err
	}
	return reasons, updatedArgs, in, nil
}

func validateCloseProxiedModes(in closeProxiedInput, args []string) error {
	if in.continueOn && len(args) > 1 {
		return HandleErrorRespectJSON("--continue only works when closing a single issue")
	}
	if in.suggestNext && len(args) > 1 {
		return HandleErrorRespectJSON("--suggest-next only works when closing a single issue")
	}
	return nil
}

func closeProxiedRunBatch(ctx context.Context, pre closeProxiedPreflight, in closeProxiedInput) (issueops.CloseBatchResult, error) {
	// THE BATCH. One request, one transaction, one Dolt commit over N ids —
	// and the request is the only thing that says where the transaction ends,
	// because the role hands out no handle to hold open. The commit message is
	// the role's, and it names what LANDED, which is what this route's own
	// message did: a skipped id stays out of the log.
	var result issueops.CloseBatchResult
	if len(pre.items) == 0 {
		return result, nil
	}
	closer, err := proxiedBatchCloser()
	if err != nil {
		return result, HandleErrorRespectJSON("%v", err)
	}
	result, err = closer.CloseBatch(ctx, issueops.CloseBatchRequest{
		Actor:     getActor(),
		Items:     pre.items,
		Session:   in.session,
		Force:     in.force,
		ClaimNext: closeClaimNextRequest(in.claimNext, in.continueOn),
	})
	if err != nil {
		return result, HandleErrorRespectJSON("%v", err)
	}
	return result, nil
}

func closeProxiedReport(in closeProxiedInput, pre closeProxiedPreflight, post closeProxiedPostClose, outcomes []closeProxiedOutcome, closeReasons []string, result issueops.CloseBatchResult) {
	closeProxiedPrintErrors(pre, post)
	closeProxiedAudit(outcomes)
	claimedNext := closeProxiedClaimedNext(result)
	if in.jsonOut {
		closeProxiedPrintJSON(outcomes, post, claimedNext)
		return
	}
	closeProxiedPrintHuman(outcomes, closeReasons, post, claimedNext, in)
}

func closeProxiedPrintErrors(pre closeProxiedPreflight, post closeProxiedPostClose) {
	for _, e := range pre.errors {
		if e != "" {
			fmt.Fprintln(os.Stderr, e)
		}
	}
	for _, w := range post.warnings {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
	}
}

func closeProxiedAudit(outcomes []closeProxiedOutcome) {
	for _, o := range outcomes {
		if o.closed {
			audit.LogFieldChange(o.id, "status", o.auditOld, "closed", getActor(), o.auditReason)
		}
	}
}

func closeProxiedClaimedNext(result issueops.CloseBatchResult) *types.Issue {
	if result.ClaimedNext == nil {
		return nil
	}
	return result.ClaimedNext.Issue
}

func closeProxiedPrintHuman(outcomes []closeProxiedOutcome, closeReasons []string, post closeProxiedPostClose, claimedNext *types.Issue, in closeProxiedInput) {
	for i, o := range outcomes {
		fmt.Printf("%s Closed %s: %s\n", ui.RenderPass("✓"), formatFeedbackID(o.after.ID, o.after.Title), closeReasons[i])
	}
	if post.autoClosedMol != nil {
		fmt.Printf("%s Auto-closed completed molecule %s\n", ui.RenderPass("✓"), formatFeedbackID(post.autoClosedMol.ID, post.autoClosedMol.Title))
	}
	if len(post.unblocked) > 0 {
		fmt.Printf("\nNewly unblocked:\n")
		for _, issue := range post.unblocked {
			fmt.Printf("  • %s (P%d)\n", formatFeedbackID(issue.ID, issue.Title), issue.Priority)
		}
	}
	if post.continueResult != nil {
		PrintContinueResult(post.continueResult)
	}
	closeProxiedPrintClaimedNext(claimedNext, in, len(outcomes))
}

func closeProxiedPrintClaimedNext(claimedNext *types.Issue, in closeProxiedInput, outcomeCount int) {
	if claimedNext != nil {
		fmt.Printf("%s Auto-claimed next ready issue: %s (P%d)\n", ui.RenderPass("✓"), formatFeedbackID(claimedNext.ID, claimedNext.Title), claimedNext.Priority)
		return
	}
	if in.claimNext && outcomeCount > 0 && !in.continueOn {
		fmt.Printf("\n%s No ready issues available to claim.\n", ui.RenderWarn("✨"))
	}
}

func closeProxiedPrintJSON(outcomes []closeProxiedOutcome, post closeProxiedPostClose, claimedNext *types.Issue) {
	if len(outcomes) == 0 {
		return
	}
	closedIssues := make([]*types.Issue, len(outcomes))
	for i, o := range outcomes {
		closedIssues[i] = o.after
	}
	switch {
	case len(post.unblocked) > 0:
		_ = outputJSON(map[string]interface{}{"closed": closedIssues, "unblocked": post.unblocked})
	case post.continueResult != nil:
		_ = outputJSON(map[string]interface{}{"closed": closedIssues, "continue": post.continueResult})
	default:
		closeProxiedPrintJSONDefault(closedIssues, claimedNext)
	}
}

func closeProxiedPrintJSONDefault(closedIssues []*types.Issue, claimedNext *types.Issue) {
	if claimedNext != nil {
		_ = outputJSON(map[string]interface{}{"closed": closedIssues, "claimed": claimedNext})
		return
	}
	_ = outputJSON(closedIssues)
}

func gatherCloseProxiedInput(cmd *cobra.Command) closeProxiedInput {
	in := closeProxiedInput{}
	in.force, _ = cmd.Flags().GetBool("force")
	in.continueOn, _ = cmd.Flags().GetBool("continue")
	in.noAuto, _ = cmd.Flags().GetBool("no-auto")
	in.suggestNext, _ = cmd.Flags().GetBool("suggest-next")
	in.claimNext, _ = cmd.Flags().GetBool("claim-next")
	in.session, _ = cmd.Flags().GetString("session")
	if in.session == "" {
		in.session = os.Getenv("CLAUDE_SESSION_ID")
	}
	in.jsonOut, _ = cmd.Flags().GetBool("json")
	return in
}

// proxiedBatchCloser hands back the guarded close-many surface for the
// proxied-server provider, through the provider's OWN capability accessor —
// the same two-step proxiedIssueReader performs, and for the same reason: the
// accessor is where each layer is added, so a command that reached for the
// constructor would get an unlayered closer.
func proxiedBatchCloser() (issueops.BatchCloser, error) {
	if getUOWProvider() == nil {
		return nil, errors.New("proxied-server UOW provider not initialized")
	}
	src, ok := getUOWProvider().(uow.BatchCloserSource)
	if !ok {
		return nil, fmt.Errorf("proxied-server provider %T does not offer the batch-close surface", getUOWProvider())
	}
	return src.BatchCloser()
}

// closeProxiedRunPreflight resolves every argument and applies the CLI's own
// close policy to it, in one read-only unit of work.
func closeProxiedRunPreflight(ctx context.Context, args, reasons []string, in closeProxiedInput) (closeProxiedPreflight, error) {
	pre := closeProxiedPreflight{
		errors: make([]string, len(args)),
		before: make(map[string]*types.Issue, len(args)),
	}
	_, err := uow.RunTxRead(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (struct{}, error) {
		for i, id := range args {
			refusal, current := closeProxiedCheckOne(ctx, uw, id, in)
			if refusal != "" {
				pre.errors[i] = refusal
				continue
			}
			pre.before[id] = current
			pre.items = append(pre.items, issueops.BatchCloseItem{IssueID: id, Reason: reasonForCloseIndex(reasons, i)})
			pre.itemArgs = append(pre.itemArgs, i)
		}
		return struct{}{}, nil
	})
	return pre, err
}

// closeProxiedCheckOne returns one id's refusal, or "" and the resolved
// pre-close issue.
func closeProxiedCheckOne(ctx context.Context, uw uow.UnitOfWork, id string, in closeProxiedInput) (string, *types.Issue) {
	current, _, err := workapi.GetIssueOrWisp(ctx, workapi.NewUOWDetailSource(uw), id)
	if errors.Is(err, storage.ErrNotFound) {
		return fmt.Sprintf("Issue %s not found", id), nil
	}
	if err != nil {
		return fmt.Sprintf("Error resolving %s: %v", id, err), nil
	}

	// Mirrors the ordering in closeDirectCheckOne (ga-ktn9pe.4.8): a row already at
	// literal StatusClosed has no state change for close validation to guard, so
	// the re-close skips it and reaches the engine as the idempotent no-op it has
	// always been. Both close paths must agree here — diverging is the defect
	// class #5217 closed.
	if current.Status != types.StatusClosed {
		if err := validateIssueClosable(id, current, getActor(), in.force); err != nil {
			return err.Error(), nil
		}
	}

	// The open-children guard is deliberately NOT pre-checked here. It lives in
	// the close transaction, which is the only place it can be race-free, and
	// it answers for every parent rather than only epics — this pre-check was
	// epic-only, so a non-epic parent got a different refusal on this route
	// than on the direct one. closeDirectCheckOne dropped its copy for the same
	// reason; closeProxiedRefusal surfaces the engine's CloseOpenChildrenError
	// unprefixed, so both routes now spell one refusal one way.

	if !in.force {
		if err := checkGateSatisfaction(current); err != nil {
			return fmt.Sprintf("cannot close %s: %s", id, err), nil
		}
	}

	return "", current
}

// closeProxiedOutcomes folds the batch's per-item outcomes back onto the
// argument list: a refusal lands in its argument's own error slot so the
// stderr report stays in typed order, and the survivors keep the shape the
// display block has always consumed.
func closeProxiedOutcomes(pre *closeProxiedPreflight, result issueops.CloseBatchResult) ([]closeProxiedOutcome, []string) {
	var outcomes []closeProxiedOutcome
	var reasons []string
	for j, outcome := range result.Outcomes {
		item := pre.items[j]
		if outcome.Err != nil {
			pre.errors[pre.itemArgs[j]] = closeProxiedRefusal(item.IssueID, outcome.Err)
			continue
		}
		before := pre.before[item.IssueID]
		oldStatus := "open"
		if before != nil && before.Status != "" {
			oldStatus = string(before.Status)
		}
		after := outcome.Issue
		if after != nil {
			// `bd close` has never printed dependency records, on either
			// route: the direct route drops them from the operation's own
			// snapshot for exactly this reason.
			after.Dependencies = nil
		}
		outcomes = append(outcomes, closeProxiedOutcome{
			id:          item.IssueID,
			before:      before,
			after:       after,
			closed:      outcome.Changed,
			auditOld:    oldStatus,
			auditReason: item.Reason,
		})
		reasons = append(reasons, item.Reason)
	}
	return outcomes, reasons
}

// closeProxiedRefusal spells one item's typed refusal the way this route has
// always spelled it. The vocabulary is matched with errors.Is rather than by
// reading the message, which is the point of the outcome carrying a typed
// error at all.
func closeProxiedRefusal(id string, err error) string {
	switch {
	case errors.Is(err, storage.ErrCloseBlocked):
		return fmt.Sprintf("%v (use --force to override)", err)
	// The open-children refusal is already a complete sentence naming the
	// issue and its count, so it passes through unprefixed — exactly as
	// closeDirectRefusal spells it. Without this arm the two routes answer the
	// same refusal differently, which is the drift this branch exists to
	// remove; it became newly reachable here when the guard moved into the
	// transaction.
	case errors.Is(err, storage.ErrCloseOpenChildren):
		return err.Error()
	case errors.Is(err, storage.ErrNotFound):
		return fmt.Sprintf("Issue %s not found", id)
	default:
		return fmt.Sprintf("Error closing %s: %v", id, err)
	}
}

// closeProxiedRunPostClose runs molecule auto-close, --suggest-next and
// --continue once the closes have committed.
//
// They are outside the batch's transaction because they are outside its
// contract, and outside is where the direct route has always run them: it
// calls autoCloseCompletedMolecule and AdvanceToNextStep after ops.Close
// returns, each in its own write. The visible consequence is a SECOND Dolt
// commit when a molecule actually auto-closes or --continue actually advances.
// A plain close writes nothing in this pass, names no commit message, and
// therefore still produces exactly one commit for the command.
func closeProxiedRunPostClose(ctx context.Context, args []string, in closeProxiedInput, outcomes []closeProxiedOutcome) closeProxiedPostClose {
	if len(outcomes) == 0 {
		return closeProxiedPostClose{}
	}
	post, err := uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (closeProxiedPostClose, string, error) {
		return runCloseProxiedPostCloseTx(ctx, uw, args, in, outcomes)
	})
	if err != nil {
		post.warnings = append(post.warnings, fmt.Sprintf("post-close work failed: %v", err))
	}
	return post
}

func runCloseProxiedPostCloseTx(ctx context.Context, uw uow.UnitOfWork, args []string, in closeProxiedInput, outcomes []closeProxiedOutcome) (closeProxiedPostClose, string, error) {
	var out closeProxiedPostClose
	wrote := closeProxiedAutoClose(ctx, uw, in, outcomes, &out)
	closeProxiedApplySuggestNext(ctx, uw, args, in, &out)
	wrote = append(wrote, closeProxiedApplyContinue(ctx, uw, args, in, &out)...)
	if len(wrote) == 0 {
		return out, "", nil
	}
	return out, "bd: " + strings.Join(wrote, "; "), nil
}

func closeProxiedAutoClose(ctx context.Context, uw uow.UnitOfWork, in closeProxiedInput, outcomes []closeProxiedOutcome, out *closeProxiedPostClose) []string {
	var wrote []string
	for _, o := range outcomes {
		mol := autoCloseProxiedCompletedMolecule(ctx, uw, o.id, getActor(), in.session, &out.warnings)
		if mol != nil {
			out.autoClosedMol = mol
			wrote = append(wrote, "auto-close "+mol.ID)
		}
	}
	return wrote
}

func closeProxiedApplySuggestNext(ctx context.Context, uw uow.UnitOfWork, args []string, in closeProxiedInput, out *closeProxiedPostClose) {
	if !in.suggestNext || len(args) != 1 {
		return
	}
	unblocked, warn := closeProxiedSuggestNext(ctx, uw, args[0])
	out.unblocked = unblocked
	if warn != "" {
		out.warnings = append(out.warnings, warn)
	}
}

func closeProxiedApplyContinue(ctx context.Context, uw uow.UnitOfWork, args []string, in closeProxiedInput, out *closeProxiedPostClose) []string {
	if !in.continueOn || len(args) != 1 {
		return nil
	}
	cont, warn := closeProxiedContinue(ctx, uw, args[0], !in.noAuto)
	out.continueResult = cont
	if warn != "" {
		out.warnings = append(out.warnings, warn)
	}
	if !closeProxiedContinued(cont) {
		return nil
	}
	return []string{"advance to " + cont.NextStep.ID}
}

func closeProxiedContinued(cont *ContinueResult) bool {
	return cont != nil && cont.AutoAdvanced && cont.NextStep != nil
}

func closeProxiedSuggestNext(ctx context.Context, uw uow.UnitOfWork, closedID string) ([]*types.Issue, string) {
	unblocked, err := uw.IssueUseCase().GetNewlyUnblockedByClose(ctx, closedID)
	if err != nil {
		return nil, fmt.Sprintf("could not compute newly unblocked: %v", err)
	}
	return unblocked, ""
}

func closeProxiedContinue(ctx context.Context, uw uow.UnitOfWork, closedID string, autoClaim bool) (*ContinueResult, string) {
	result, err := AdvanceToNextStep(ctx, newUOWMolWriter(uw), closedID, autoClaim, getActor())
	if err != nil {
		return nil, fmt.Sprintf("could not advance to next step: %v", err)
	}
	return result, ""
}

func autoCloseProxiedCompletedMolecule(ctx context.Context, uw uow.UnitOfWork, closedStepID string, actorName, session string, warnings *[]string) *types.Issue {
	root, moleculeID := proxiedAutoCloseRoot(ctx, uw, closedStepID)
	if root == nil {
		return nil
	}
	if !proxiedMoleculeFullyComplete(ctx, uw, moleculeID) {
		return nil
	}
	params := domain.CloseIssueParams{Reason: "all steps complete", Session: session}
	if _, err := uw.IssueUseCase().CloseIssue(ctx, moleculeID, params, actorName); err != nil {
		*warnings = append(*warnings, fmt.Sprintf("could not auto-close completed molecule %s: %v", moleculeID, err))
		return nil
	}
	return root
}

func proxiedAutoCloseRoot(ctx context.Context, uw uow.UnitOfWork, closedStepID string) (*types.Issue, string) {
	moleculeID := proxiedFindParentMolecule(ctx, uw, closedStepID)
	if moleculeID == "" {
		return nil, ""
	}
	root, err := uw.IssueUseCase().GetIssue(ctx, moleculeID)
	if err != nil || root == nil || root.Status == types.StatusClosed {
		return nil, ""
	}
	// A READ, and one that has to see this transaction. The auto-close decision
	// is made from labels written earlier in the same unit of work, and
	// issueops.Reader opens a transaction of its own, so it would answer from
	// the last committed state instead. The follow-up is a reader role bound to
	// a caller's transaction; until one exists this stays (ga-2ltro.12).
	if labels, err := uw.LabelUseCase().GetLabels(ctx, moleculeID); err == nil { //nolint:forbidigo // in-transaction read; issueops.Reader would open its own
		root.Labels = labels
	}
	if !shouldAutoCloseCompletedRoot(root) {
		return nil, ""
	}
	return root, moleculeID
}

func proxiedMoleculeFullyComplete(ctx context.Context, uw uow.UnitOfWork, moleculeID string) bool {
	progress, err := getMoleculeProgress(ctx, uowMolReader{uw: uw}, moleculeID)
	if err != nil {
		return false
	}
	return progress.Completed >= progress.Total
}

func proxiedFindParentMolecule(ctx context.Context, uw uow.UnitOfWork, issueID string) string {
	return findParentMolecule(ctx, uowMolReader{uw: uw}, issueID)
}
