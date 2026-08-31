package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/validation"
	"github.com/jonbaldie/beads/issueops"
	"github.com/spf13/cobra"
)

type closeReasonFlagValue struct {
	values []string
}

func registerCloseReasonFlag(cmd *cobra.Command) {
	cmd.Flags().VarP(&closeReasonFlagValue{}, "reason", "r", "Reason for closing")
}

func (v *closeReasonFlagValue) Set(s string) error {
	v.values = append(v.values, s)
	return nil
}

func (v *closeReasonFlagValue) String() string {
	if len(v.values) == 0 {
		return ""
	}
	return v.values[len(v.values)-1]
}

func (v *closeReasonFlagValue) Type() string {
	return "string"
}

func (v *closeReasonFlagValue) Values() []string {
	out := make([]string, len(v.values))
	copy(out, v.values)
	return out
}

func resolveCloseReasons(cmd *cobra.Command, args []string) ([]string, []string, error) {
	reasons, err := collectCloseReasonFlags(cmd)
	if err != nil {
		return nil, args, err
	}
	reasons, err = applyCloseReasonFile(cmd, reasons)
	if err != nil {
		return nil, args, err
	}

	// Desire-path: "bd done <id> <message>" treats last positional arg as reason
	// when no reason flag was explicitly provided (hq-pe8ce)
	reasons, args = applyCloseReasonPositional(cmd, reasons, args)
	if len(reasons) == 0 {
		reasons = []string{"Closed"}
	}
	if err := validateCloseReasonCount(reasons, args); err != nil {
		return nil, args, err
	}
	return reasons, args, nil
}

func applyCloseReasonFile(cmd *cobra.Command, reasons []string) ([]string, error) {
	fileReason, ok, err := resolveReasonFile(cmd, len(reasons) > 0)
	if err != nil {
		return nil, err
	}
	if ok {
		return []string{fileReason}, nil
	}
	return reasons, nil
}

func applyCloseReasonPositional(cmd *cobra.Command, reasons, args []string) ([]string, []string) {
	if len(reasons) == 0 && cmd.CalledAs() == "done" && len(args) >= 2 {
		return []string{args[len(args)-1]}, args[:len(args)-1]
	}
	return reasons, args
}

func validateCloseReasonCount(reasons, args []string) error {
	if len(reasons) > 1 && len(reasons) != len(args) {
		return fmt.Errorf("got %d close reasons for %d issue IDs; provide exactly one shared reason or one reason per issue", len(reasons), len(args))
	}
	return nil
}

func collectCloseReasonFlags(cmd *cobra.Command) ([]string, error) {
	if flag := cmd.Flags().Lookup("reason"); flag != nil {
		if v, ok := flag.Value.(interface{ Values() []string }); ok {
			if reasons := nonEmptyCloseReasons(v.Values()); len(reasons) > 0 {
				return reasons, nil
			}
		}
	}

	for _, name := range []string{"resolution", "message", "comment"} {
		reason, err := cmd.Flags().GetString(name)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			return []string{reason}, nil
		}
	}
	return nil, nil
}

func nonEmptyCloseReasons(reasons []string) []string {
	out := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		if reason != "" {
			out = append(out, reason)
		}
	}
	return out
}

func reasonForCloseIndex(reasons []string, i int) string {
	if len(reasons) == 1 {
		return reasons[0]
	}
	return reasons[i]
}

// closeClaimNextRequest is --claim-next as the batch expresses it. BOTH of
// `bd close`'s routes build it here, so the claim asks one question no matter
// which door it came through.
//
// The role runs it inside the closes' transaction and only when at least one
// item landed, which is the rule each route used to enforce by hand; --continue
// still wins, because the two flags have always been mutually exclusive here.
//
// It asks for priority order and no limit. The old two-step read the single top
// ready row and warned if that one row happened to be taken; the role's claim
// walks the ready order and takes the first row it can win, so a race with
// another agent now claims the next candidate instead of claiming nothing.
func closeClaimNextRequest(claimNext, continueOn bool) *issueops.ReadyRequest {
	if !claimNext || continueOn {
		return nil
	}
	return &issueops.ReadyRequest{Sort: string(types.SortPolicyPriority)}
}

func validateCloseReasons(reasons []string) error {
	closeValidation := config.GetString("validation.on-close")
	if closeValidation != "error" && closeValidation != "warn" {
		return nil
	}

	for _, reason := range reasons {
		if err := validation.ValidateCloseReason(reason); err != nil {
			if closeValidation == "error" {
				return err
			}
			// warn mode: print warning but proceed
			fmt.Fprintf(os.Stderr, "%s %v\n", ui.RenderWarn("⚠"), err)
		}
	}
	return nil
}

// isMachineCheckableGate returns true if the issue is a gate with a machine-checkable await type.
func isMachineCheckableGate(issue *types.Issue) bool {
	if issue == nil || issue.IssueType != "gate" {
		return false
	}
	switch {
	case strings.HasPrefix(issue.AwaitType, "gh:pr"):
		return true
	case strings.HasPrefix(issue.AwaitType, "gh:run"):
		return true
	case issue.AwaitType == "timer":
		return true
	case issue.AwaitType == "bead":
		return true
	default:
		return false
	}
}

// checkGateSatisfaction checks whether a gate issue's condition is satisfied.
// Returns nil if the gate is satisfied (or not a machine-checkable gate), or an error describing why it cannot be closed.
func checkGateSatisfaction(issue *types.Issue) error {
	if !isMachineCheckableGate(issue) {
		return nil
	}

	resolved, _, reason, err := evaluateGateSatisfaction(issue)

	if err != nil {
		// If we can't check the condition, allow close with a warning
		fmt.Fprintf(os.Stderr, "Warning: could not evaluate gate condition: %v\n", err)
		return nil
	}

	if resolved {
		return nil
	}

	return fmt.Errorf("gate condition not satisfied: %s (use --force to override)", reason)
}

func evaluateGateSatisfaction(issue *types.Issue) (resolved, escalated bool, reason string, err error) {
	switch {
	case strings.HasPrefix(issue.AwaitType, "gh:run"):
		return checkGHRun(issue, func(gateID, runID string) error { return updateGateAwaitIDFunc(nil, gateID, runID) })
	case strings.HasPrefix(issue.AwaitType, "gh:pr"):
		return checkGHPR(issue)
	case issue.AwaitType == "timer":
		return checkTimer(issue, time.Now())
	case issue.AwaitType == "bead":
		resolved, reason = checkBeadGate(getRootContext(), getStore(), issue.AwaitID)
		return resolved, false, reason, nil
	default:
		return false, false, "", nil
	}
}

// autoCloseCompletedMolecule checks if closing a step completed an auto-closing
// parent molecule, and if so, closes the molecule root. Ordinary epics remain
// open when all children finish so they can become explicitly close-eligible
// instead of being closed as a side effect of the final child close. It returns
// the molecule root ID when it actually closed the root (and "" otherwise) so a
// caller that did not otherwise mutate the store — an already-closed re-close in
// particular — can register the store for the pending-commit sweep. The check is
// fully state-derived and idempotent: it early-returns unless the root is open,
// auto-close-eligible, and has all steps complete, so re-invoking it never
// double-closes or reintroduces side effects.
func autoCloseCompletedMolecule(ctx context.Context, s storage.DoltStorage, closedStepID, actorName, session string) string {
	moleculeID := findParentMolecule(ctx, s, closedStepID)
	if moleculeID == "" {
		return "" // Not part of a molecule
	}

	root, ok := loadAutoCloseRoot(ctx, s, moleculeID)
	if !ok {
		return ""
	}

	// Load progress to check completion
	progress, err := getMoleculeProgress(ctx, s, moleculeID)
	if err != nil {
		return "" // Best effort — don't fail the close
	}

	if progress.Completed < progress.Total {
		return "" // Not all steps complete yet
	}

	// All steps complete — auto-close the molecule root
	if err := s.CloseIssue(ctx, moleculeID, "all steps complete", actorName, session); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not auto-close completed molecule %s: %v\n", moleculeID, err)
		return ""
	}

	if !isJSONOutput() {
		debug.PrintNormal("%s Auto-closed completed molecule %s\n", ui.RenderPass("✓"), formatFeedbackID(moleculeID, root.Title))
	}
	return moleculeID
}

func loadAutoCloseRoot(ctx context.Context, s storage.DoltStorage, moleculeID string) (*types.Issue, bool) {
	root, err := s.GetIssue(ctx, moleculeID)
	if err != nil || root == nil || root.Status == types.StatusClosed || !shouldAutoCloseCompletedRoot(root) {
		return nil, false
	}
	return root, true
}

// shouldAutoCloseCompletedRoot returns true for molecule roots that should
// auto-close when their final step closes. Regular epics stay open and become
// explicit close-eligible work, while ephemeral wisps, template-driven
// molecules, and molecule-type coordination roots keep their cleanup behavior.
func shouldAutoCloseCompletedRoot(root *types.Issue) bool {
	if root == nil {
		return false
	}

	if root.IssueType == types.TypeMolecule || root.Ephemeral {
		return true
	}

	if root.IssueType != types.TypeEpic {
		return false
	}

	for _, label := range root.Labels {
		if label == BeadsTemplateLabel {
			return true
		}
	}

	return false
}

// resolveReasonFile resolves the --reason-file flag for `bd close`.
// Returns (content, true, nil) when --reason-file was set and read successfully.
// Returns (_, false, nil) when --reason-file was not set.
// Returns an error on conflict with an existing reason, file read failure, or empty content.
// Mirrors the --body-file pattern from `bd create` so agents can pass structured close
// templates without shell-escaping hell.
func resolveReasonFile(cmd *cobra.Command, hasExistingReason bool) (string, bool, error) {
	if !cmd.Flags().Changed("reason-file") {
		return "", false, nil
	}
	if hasExistingReason {
		return "", false, fmt.Errorf("cannot specify both --reason-file and --reason/--resolution/--message/--comment")
	}
	path, _ := cmd.Flags().GetString("reason-file")
	content, err := readBodyFile(path)
	if err != nil {
		return "", false, fmt.Errorf("reading reason file %q: %w", path, err)
	}
	if strings.TrimSpace(content) == "" {
		return "", false, fmt.Errorf("--reason-file %q is empty; close reason is required", path)
	}
	return content, true, nil
}

// resolveCloseTargets resolves a batch of partial issue IDs for `bd close`,
// preserving input order. For each ID it tries the local store first, then
// explicit prefix routing via routes.jsonl, then a shared contributor-routed
// store. This matches resolveAndGetIssueWithRouting's routing precedence.
//
// The contributor-routed handle is shared across the batch so bulk close does
// not repeatedly open the same planning store and every result has a clear store
// owner for subsequent close-time checks and writes.
//
// Each returned RoutedResult.Store points to whichever store actually owns the
// issue. The caller invokes cleanup() once when done; per-result Close() is a
// no-op for routed-via-shared-handle entries because they don't own the handle.
type closeTargetResolver struct {
	ctx               context.Context
	localStore        storage.DoltStorage
	results           []*RoutedResult
	sharedRouted      storage.DoltStorage
	sharedRoutedTried bool
}

func (r *closeTargetResolver) cleanup() {
	for _, result := range r.results {
		result.Close()
	}
	if r.sharedRouted != nil {
		_ = r.sharedRouted.Close()
	}
}

func (r *closeTargetResolver) ensureShared() (storage.DoltStorage, error) {
	if r.sharedRouted != nil {
		return r.sharedRouted, nil
	}
	if r.sharedRoutedTried {
		return nil, fmt.Errorf("no auto-routed store available")
	}
	r.sharedRoutedTried = true
	store, routed, _, err := openRoutedReadStore(r.ctx, r.localStore)
	if err != nil {
		return nil, err
	}
	if !routed {
		return nil, fmt.Errorf("no auto-routed store available")
	}
	r.sharedRouted = store
	return store, nil
}

func (r *closeTargetResolver) resolve(id string) (*RoutedResult, error) {
	// Local first.
	result, err := resolveAndGetFromStore(r.ctx, r.localStore, id, false)
	if err == nil {
		return result, nil
	}
	if !isNotFoundErr(err) {
		return nil, fmt.Errorf("resolving ID %s: %w", id, err)
	}
	// Write-intent: a prefix-routed target opens writable so the close
	// commits on the target head (#4141). Contributor auto-routing below
	// stays read-only: it hydrates foreign projects that must not be mutated.
	if result, err := resolveViaPrefixRoutingWithAccess(r.ctx, id, true); err == nil {
		return result, nil
	}
	// Contributor auto-routing uses one shared store for the whole batch.
	store, err := r.ensureShared()
	if err == nil {
		// Per-id RoutedResult does not own the shared handle; cleanup() does.
		if result, resolveErr := resolveAndGetFromStore(r.ctx, store, id, true); resolveErr == nil {
			return result, nil
		}
	}
	return nil, fmt.Errorf("resolving ID %s: no issue found matching %q", id, id)
}

func resolveCloseTargets(ctx context.Context, localStore storage.DoltStorage, ids []string) ([]*RoutedResult, func(), error) {
	resolver := &closeTargetResolver{ctx: ctx, localStore: localStore, results: make([]*RoutedResult, 0, len(ids))}
	for _, id := range ids {
		result, err := resolver.resolve(id)
		if err != nil {
			resolver.cleanup()
			return nil, func() {}, err
		}
		resolver.results = append(resolver.results, result)
	}
	return resolver.results, resolver.cleanup, nil
}
