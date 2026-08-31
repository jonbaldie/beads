package main

import (
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/audit"
	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

var closeCmd = &cobra.Command{
	Use:               "close [id...]",
	Aliases:           []string{"done"},
	GroupID:           "issues",
	Short:             "Close one or more issues",
	ValidArgsFunction: issueIDCompletion,
	Long: `Close one or more issues.

If no issue ID is provided, closes the last touched issue (from most recent
create, update, show, or close operation). This fallback only applies in
interactive sessions (stdin is a terminal); in scripts and agent sessions a
missing ID is an error, so a command built from an empty variable cannot
silently close an unrelated issue. Set BD_LAST_TOUCHED_FALLBACK=1 to allow
the fallback anywhere, or =0 to disable it entirely.

When closing multiple issues, provide one --reason for all IDs or repeat
--reason once per ID. Reasons map positionally: the first --reason applies
to the first ID, the second --reason to the second ID, regardless of where
the flags appear in the command line.`,
	// Refuse a missing ID in argument validation, before root's
	// PersistentPreRunE can open the store, migrate, or auto-import
	// (bd-m00pb); see updateCmd for the full rationale.
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 && !AllowLastTouchedFallback() {
			return HandleErrorRespectJSON("no issue ID provided (the last-touched fallback only applies in interactive sessions; pass an explicit issue ID or set BD_LAST_TOUCHED_FALLBACK=1)")
		}
		return nil
	},
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := CheckReadonly("close"); err != nil {
			return err
		}

		evt := metrics.NewCommandEvent("close")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if usesProxiedServer() {
			return runCloseProxiedServer(cmd, getRootContext(), args)
		}

		// If no IDs provided, use last touched issue (interactive only;
		// the non-interactive case was already refused in Args validation)
		if len(args) == 0 {
			lastTouched := GetLastTouchedID()
			if lastTouched == "" {
				return HandleErrorRespectJSON("no issue ID provided and no last touched issue")
			}
			args = []string{lastTouched}
		}
		reasons, updatedArgs, err := resolveCloseReasons(cmd, args)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		args = updatedArgs

		if err := validateCloseReasons(reasons); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}

		force, _ := cmd.Flags().GetBool("force")
		continueFlag, _ := cmd.Flags().GetBool("continue")
		noAuto, _ := cmd.Flags().GetBool("no-auto")
		suggestNext, _ := cmd.Flags().GetBool("suggest-next")

		claimNext, _ := cmd.Flags().GetBool("claim-next")

		session, _ := cmd.Flags().GetString("session")
		if session == "" {
			session = os.Getenv("CLAUDE_SESSION_ID")
		}

		ctx := getRootContext()
		opsCtx, err := issueOpsContext(ctx)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}

		if continueFlag && len(args) > 1 {
			return HandleErrorRespectJSON("--continue only works when closing a single issue")
		}

		if suggestNext && len(args) > 1 {
			return HandleErrorRespectJSON("--suggest-next only works when closing a single issue")
		}

		results, cleanup, resolveErr := resolveCloseTargets(ctx, getStore(), args)
		defer cleanup()
		if resolveErr != nil {
			return HandleErrorRespectJSON("%v", resolveErr)
		}
		resolvedIDs := make([]string, 0, len(results))
		for _, r := range results {
			resolvedIDs = append(resolvedIDs, r.ResolvedID)
		}

		// Track which stores were mutated so routed closes can commit before
		// cleanup closes the routed handle. Deduped by pointer.
		mutatedStores := map[storage.DoltStorage][]string{}

		// Pick a store for post-close work (--suggest-next, --continue, --claim-next).
		// All three flags are documented as single-issue paths; for the multi-id case
		// we use the first resolved ID's store, which matches the common case where
		// every ID routes to the same place.
		postCloseStore := getStore()
		if len(results) > 0 && results[0].Store != nil {
			postCloseStore = results[0].Store
		}

		// THE BATCH. The CLI's own close policy runs first and read-only, then
		// every id it passed goes to the BatchCloser role as one request per
		// store: one transaction, one Dolt commit over N ids, and an id the
		// batch refuses skipped while the survivors commit. --claim-next rides
		// inside that transaction, and the engine's own is_blocked guard runs
		// there too (GH#962), so there is no read-then-write TOCTOU window
		// between the check and the close.
		plan := closeDirectPreflight(results, resolvedIDs, reasons, force)
		outcomes, claimedNext := closeDirectRun(opsCtx, closeDirectBatches(plan.items), len(resolvedIDs),
			session, force, postCloseStore, closeClaimNextRequest(claimNext, continueFlag))

		// Report and follow up on every argument, in the order it was typed.
		closedIssues := []*types.Issue{}
		closedCount := 0
		alreadyClosed := 0
		firstSettledID := ""

		for i, id := range resolvedIDs {
			res := outcomes[i]
			if res == nil {
				// The CLI's own close policy refused this argument, so the
				// batch never saw it.
				fmt.Fprintln(os.Stderr, plan.refusals[i])
				continue
			}
			if res.Err != nil {
				fmt.Fprintln(os.Stderr, closeDirectRefusal(id, res.Err))
				continue
			}

			// Open children only survive to here when --force waived the
			// engine's refusal. Say so, so orphaned children are never silent.
			if res.OpenChildren > 0 {
				fmt.Fprintf(os.Stderr, "warning: closing %s with %d open child issue(s) still active\n", id, res.OpenChildren)
			}

			activeStore := results[i].Store
			reason := reasonForCloseIndex(reasons, i)
			// Pre-close snapshot, for the audit entry's old status and the
			// success line's title.
			issue := results[i].Issue

			if !res.Changed {
				// Already closed: an idempotent no-op on the step's stored state. The
				// old CloseIssue path also returned nil here and still reported the
				// (already-closed) issue, so keep OUTPUT parity via the shared display
				// block below — the issue stays in --json output and the text report
				// exactly as before. Suppress the step's own real-state-change side
				// effects (the audit entry, so no spurious closed→closed; the
				// closedCount bump; step-level pending-commit tracking, since the step
				// write itself is a no-op), but still count the command as a successful
				// close for its retry-safe post-close contracts (last-touched,
				// --continue, --suggest-next, --claim-next) via alreadyClosed below.
				// Exit stays 0.
				alreadyClosed++

				// Molecule auto-close is itself a retry-safe, fully state-derived
				// post-close contract, so it must replay on an already-closed re-close
				// just like the contracts above. If the final step's real close
				// persisted but its molecule auto-close did not (a crash between the two
				// commits, or the root CloseIssue failing with only a warning), this
				// idempotent re-close is the ONLY thing that re-drives it — otherwise the
				// molecule root is stranded open forever. autoCloseCompletedMolecule
				// early-returns unless the root is genuinely open, auto-close-eligible,
				// and complete, so it heals only that case and reintroduces none of the
				// suppressed real-close side effects (no audit, no closed→closed on the
				// step). Register the store when it actually closed the root so the
				// pending-commit sweep persists it — closedCount==0 would not commit.
				if molID := autoCloseCompletedMolecule(ctx, activeStore, id, getActor(), session); molID != "" {
					mutatedStores[activeStore] = append(mutatedStores[activeStore], molID)
				}
			} else {
				mutatedStores[activeStore] = append(mutatedStores[activeStore], id)

				// Audit log the close (survives Dolt GC flatten)
				oldStatus := "open"
				if issue != nil {
					oldStatus = string(issue.Status)
				}
				audit.LogFieldChange(id, "status", oldStatus, "closed", getActor(), reason)

				closedCount++

				// Auto-close parent molecule if all steps are now complete.
				// Runs against the same store the step was closed in.
				autoCloseCompletedMolecule(ctx, activeStore, id, getActor(), session)
			}

			// First id this command settled as closed — a real close or an
			// already-closed no-op both "touch" it. Drives the retry-safe last-touched
			// contract below so a re-close still points default-target commands at it.
			if firstSettledID == "" {
				firstSettledID = id
			}

			// The operation's own post-state snapshot is what gets reported. A
			// real close and an idempotent no-op both report the closed issue
			// here, matching the historical output shape. Dependency records are
			// dropped from it because `bd close` has never printed them: the
			// re-read this replaced did not hydrate them either.
			closedIssue := res.Issue
			if closedIssue != nil {
				closedIssue.Dependencies = nil
			}

			if isJSONOutput() {
				if closedIssue != nil {
					closedIssues = append(closedIssues, closedIssue)
				}
			} else {
				debug.PrintNormal("%s Closed %s: %s\n", ui.RenderPass("✓"), formatFeedbackID(id, issueTitleOrEmpty(issue)), reason)
			}
		}

		// A close command "succeeds" for its user-facing, retry-safe contracts when it
		// settled the target as closed — whether it performed the real state change
		// (closedCount) or confirmed an already-closed idempotent no-op
		// (alreadyClosed). last-touched, --continue, --suggest-next, and --claim-next
		// all re-derive their result from current state, so they must replay on an
		// already-closed retry; `bd close --continue` in particular is a workflow-
		// advancement trigger a crash/retry has to be able to re-drive. The real
		// close-mutation side effects (audit, event, molecule auto-close) stay
		// suppressed for an already-closed no-op via the `else` branch above; the
		// pending-commit sweep is gated on mutatedStores, which a post-close claim
		// also populates.
		closedForCommand := closedCount > 0 || alreadyClosed > 0

		// Record the closed issue as last-touched so `bd close` honors its own
		// documented contract (the "last touched issue ... from create, update,
		// show, or close" behavior) and downstream write-marker consumers see the
		// close (GH#3965). Mirrors bd update's firstUpdatedID pattern. A later
		// --claim-next overwrites this with the claimed issue (the newer touch).
		if closedForCommand {
			SetLastTouchedID(firstSettledID)
		}

		if suggestNext && len(resolvedIDs) == 1 && closedForCommand {
			unblocked, err := postCloseStore.GetNewlyUnblockedByClose(ctx, resolvedIDs[0])
			if err == nil && len(unblocked) > 0 {
				if isJSONOutput() {
					return outputJSON(map[string]interface{}{
						"closed":    closedIssues,
						"unblocked": unblocked,
					})
				}
				fmt.Printf("\nNewly unblocked:\n")
				for _, issue := range unblocked {
					fmt.Printf("  • %s (P%d)\n", formatFeedbackID(issue.ID, issue.Title), issue.Priority)
				}
			}
		}

		if continueFlag && len(resolvedIDs) == 1 && closedForCommand {
			autoClaim := !noAuto
			result, err := AdvanceToNextStep(ctx, newStandaloneStoreMolWriter(postCloseStore), resolvedIDs[0], autoClaim, getActor())
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not advance to next step: %v\n", err)
			} else if result != nil {
				// Mirror --claim-next: when AdvanceToNextStep auto-claims the
				// next step, update .beads/last-touched so subsequent default-
				// target commands (e.g. bare `bd update`, `bd close`) target
				// it. Without this, last-touched stays pointed at the just-
				// closed step. See gastownhall/beads#3769.
				if result.AutoAdvanced && result.NextStep != nil {
					SetLastTouchedID(result.NextStep.ID)
					// The auto-claim mutated postCloseStore's working set. Register it
					// so the pending-commit sweep below persists the advance — parity
					// with --claim-next, and required when the close itself was an
					// already-closed no-op (closedCount==0 wouldn't otherwise commit).
					// Same-pointer key dedupes with the closed store on a real close.
					mutatedStores[postCloseStore] = append(mutatedStores[postCloseStore], result.NextStep.ID)
				}
				if isJSONOutput() {
					return outputJSON(map[string]interface{}{
						"closed":   closedIssues,
						"continue": result,
					})
				}
				PrintContinueResult(result)
			}
		}

		// Report --claim-next. The claim itself already happened, inside the
		// batch's own transaction and only when something landed, so what is
		// left here is the report and the last-touched hand-off. Register the
		// claimed store anyway: the batch committed the claim, but the sweep
		// below still has to name it if a molecule auto-close made the store
		// dirty again.
		var claimedNextIssue *types.Issue
		if claimNext && closedForCommand && !continueFlag {
			if claimedNext != nil {
				claimedNextIssue = claimedNext.Issue
				mutatedStores[postCloseStore] = append(mutatedStores[postCloseStore], claimedNextIssue.ID)
				if !isJSONOutput() {
					debug.PrintNormal("%s Auto-claimed next ready issue: %s (P%d)\n", ui.RenderPass("✓"), formatFeedbackID(claimedNextIssue.ID, claimedNextIssue.Title), claimedNextIssue.Priority)
				}
				SetLastTouchedID(claimedNextIssue.ID)
			} else if !isJSONOutput() {
				debug.PrintNormal("\n%s No ready issues available to claim.\n", ui.RenderWarn("✨"))
			}
		}

		if isJSONOutput() && len(closedIssues) > 0 {
			if claimedNextIssue != nil {
				if err := outputJSON(map[string]interface{}{
					"closed":  closedIssues,
					"claimed": claimedNextIssue,
				}); err != nil {
					return err
				}
			} else {
				if err := outputJSON(closedIssues); err != nil {
					return err
				}
			}
		}

		// Commit whenever a store was actually mutated — a real close, an auto-claimed
		// --continue advance, or a --claim-next claim. Gating on mutatedStores rather
		// than closedCount matters for an already-closed re-close that still advanced
		// or claimed via a retry-safe post-close flag: the mutation lives in the
		// working set and must be persisted, not left for a later write to sweep. For
		// existing paths this is equivalent to closedCount>0 (only real closes and
		// post-close claims populate mutatedStores). Commit is a no-op if there is
		// genuinely nothing pending.
		if len(mutatedStores) > 0 {
			for s, ids := range mutatedStores {
				if s == nil {
					continue
				}
				if err := commitPendingIfEmbedded(ctx, s, getActor(), doltAutoCommitParams{
					Command:  "close",
					IssueIDs: ids,
				}); err != nil {
					return HandleErrorRespectJSON("failed to commit: %v", err)
				}
			}
		}

		totalAttempted := len(resolvedIDs)
		if totalAttempted > 0 && closedCount == 0 && alreadyClosed == 0 {
			return SilentExit()
		}
		return nil
	},
}

func init() {
	registerCloseReasonFlag(closeCmd)
	closeCmd.Flags().String("resolution", "", "Alias for --reason (Jira CLI convention)")
	_ = closeCmd.Flags().MarkHidden("resolution") // Hidden alias for agent/CLI ergonomics
	closeCmd.Flags().StringP("message", "m", "", "Alias for --reason (git commit convention)")
	_ = closeCmd.Flags().MarkHidden("message") // Hidden alias for agent/CLI ergonomics
	closeCmd.Flags().String("comment", "", "Alias for --reason")
	_ = closeCmd.Flags().MarkHidden("comment") // Hidden alias for agent/CLI ergonomics
	closeCmd.Flags().String("reason-file", "", "Read close reason from file (use - for stdin)")
	closeCmd.Flags().BoolP("force", "f", false, "Force close pinned issues or unsatisfied gates")
	closeCmd.Flags().Bool("continue", false, "Auto-advance to next step in molecule")
	closeCmd.Flags().Bool("no-auto", false, "With --continue, show next step but don't claim it")
	closeCmd.Flags().Bool("suggest-next", false, "Show newly unblocked issues after closing")
	closeCmd.Flags().Bool("claim-next", false, "Automatically claim the next highest priority available issue")
	closeCmd.Flags().String("session", "", "Claude Code session ID (or set CLAUDE_SESSION_ID env var)")
	rootCmd.AddCommand(closeCmd)
}
