package main

import (
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/audit"
	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/issueops"
)

type updateBatchState struct {
	b                      updateBatchArgs
	updatedIssues          []*types.Issue
	firstUpdatedID         string
	failures               []updateIDFailure
	mutatedStores          map[storage.DoltStorage][]string
	notesOverwriteWarnings map[storage.DoltStorage][]string
	mutatedResults         map[*RoutedResult]bool
	pendingCloseResults    []*RoutedResult
}

func newUpdateBatchState(b updateBatchArgs) *updateBatchState {
	return &updateBatchState{
		b:                      b,
		updatedIssues:          []*types.Issue{},
		mutatedStores:          map[storage.DoltStorage][]string{},
		notesOverwriteWarnings: map[storage.DoltStorage][]string{},
		mutatedResults:         map[*RoutedResult]bool{},
	}
}

func recordUpdateFailure(st *updateBatchState, id, reason string) {
	st.failures = append(st.failures, updateIDFailure{ID: id, Error: reason})
}

func trackUpdateMutation(st *updateBatchState, result *RoutedResult) {
	if result == nil || result.Store == nil {
		return
	}
	if !st.mutatedResults[result] {
		st.pendingCloseResults = append(st.pendingCloseResults, result)
		st.mutatedResults[result] = true
	}
	st.mutatedStores[result.Store] = append(st.mutatedStores[result.Store], result.ResolvedID)
}

func closeUpdateIfUnmutated(st *updateBatchState, result *RoutedResult) {
	if result == nil {
		return
	}
	if st.mutatedResults[result] {
		return
	}
	result.Close()
}

func closePendingUpdateResults(st *updateBatchState) {
	for _, result := range st.pendingCloseResults {
		result.Close()
	}
	st.pendingCloseResults = nil
}

func applyUpdateBatch(b updateBatchArgs) error {
	st := newUpdateBatchState(b)
	for _, id := range b.args {
		applyOneUpdate(st, id)
	}
	return finishUpdateBatch(st)
}

func applyOneUpdate(st *updateBatchState, id string) {
	result, issue, issueStore, ok := loadUpdateTarget(st, id)
	if !ok {
		return
	}
	if !validateUpdateTarget(st, id, issue, issueStore, result) {
		return
	}
	updatedIssue, notesOverwritten, ok := mutateUpdateTarget(st, id, issue, issueStore, result)
	if !ok {
		return
	}
	collectUpdateSuccess(st, id, issue, issueStore, result, updatedIssue, notesOverwritten)
}

func loadUpdateTarget(st *updateBatchState, id string) (*RoutedResult, *types.Issue, storage.DoltStorage, bool) {
	// Resolve and get issue with routing (e.g., gt-xyz routes to another rig)
	result, err := resolveAndGetIssueForMutation(st.b.ctx, getStore(), id)
	if err != nil {
		if result != nil {
			result.Close()
		}
		fmt.Fprintf(os.Stderr, "Error resolving %s: %v\n", id, err)
		recordUpdateFailure(st, id, fmt.Sprintf("resolving issue: %v", err))
		return nil, nil, nil, false
	}
	if result == nil || result.Issue == nil {
		if result != nil {
			result.Close()
		}
		fmt.Fprintf(os.Stderr, "Issue %s not found\n", id)
		recordUpdateFailure(st, id, "issue not found")
		return nil, nil, nil, false
	}
	return result, result.Issue, result.Store, true
}

func validateUpdateTarget(st *updateBatchState, id string, issue *types.Issue, issueStore storage.DoltStorage, result *RoutedResult) bool {
	if err := validateIssueUpdatable(id, issue); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		recordUpdateFailure(st, id, err.Error())
		closeUpdateIfUnmutated(st, result)
		return false
	}
	return validateUpdateReassign(st, id, issue, issueStore, result)
}

func validateUpdateReassign(st *updateBatchState, id string, issue *types.Issue, issueStore storage.DoltStorage, result *RoutedResult) bool {
	// bd-98s5c: an unguarded assignee update must not silently
	// overwrite another actor's live claim. Skipped under
	// --if-assignee: that CAS names the holder explicitly, which is
	// how sanctioned X→Y transfers (park) stay possible without
	// --force. Also skipped under --claim: the claim CAS is itself
	// the anti-steal gate (a foreign live claim fails it with the
	// canonical "already claimed" copy before any field update runs),
	// and an assignee edit that rides a WON claim only ever touches
	// the actor's own fresh claim. A policy refusal, so it exits 1,
	// not 13.
	newAssignee, ok := st.b.updates["assignee"].(string)
	if !ok || st.b.ifAssignee != nil || st.b.claim {
		return true
	}
	if err := validateIssueReassignable(id, issue, getActor(), newAssignee,
		storeClaimPoolAliases(st.b.ctx, issueStore), st.b.force); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		recordUpdateFailure(st, id, err.Error())
		closeUpdateIfUnmutated(st, result)
		return false
	}
	return true
}

func mutateUpdateTarget(st *updateBatchState, id string, issue *types.Issue, issueStore storage.DoltStorage, result *RoutedResult) (*types.Issue, bool, bool) {
	// One atomic operation carries the claim, every field edit, the
	// label edits, the metadata edits and the reparent. Metadata edits
	// (--metadata, --set-metadata, --unset-metadata) and --append-notes
	// still resolve against the row re-read inside the mutation
	// transaction: merging against the `issue` snapshot (read in an
	// earlier transaction) silently erased concurrent writers' keys —
	// both processes exited 0, one process's committed write vanished.
	patch := st.b.patch
	// GH#3233: --defer="" restores ready visibility only if the issue
	// was actually deferred. Other statuses (blocked, in_progress, …)
	// shouldn't be clobbered just because defer_until was stale.
	if st.b.clearDefer && issue.Status == types.StatusDeferred {
		patch.Status = issueops.Field[issueops.Status]{Set: true, Value: types.StatusOpen}
	}
	notesOverwritten := replacesExistingNotes(issue.Notes, st.b.updates)

	ops, err := writeOps(issueStore)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error updating %s: %v\n", id, err)
		recordUpdateFailure(st, id, fmt.Sprintf("updating issue: %v", err))
		closeUpdateIfUnmutated(st, result)
		return nil, false, false
	}
	return runUpdateMutation(st, id, ops, patch, notesOverwritten, result)
}

func runUpdateMutation(st *updateBatchState, id string, ops commandIssueUpdater, patch issueops.IssuePatch, notesOverwritten bool, result *RoutedResult) (*types.Issue, bool, bool) {
	// Guards ride the operation itself: a stale assignee/status refuses
	// atomically with a typed mismatch error and MUST surface as a
	// non-zero exit — never collapse it to success (finding #10).
	// One --force, two overrides. The assignee half only applies to an
	// assignee edit — asserting it without one is an invalid request,
	// which is why it is conditioned here rather than passed straight
	// through: `--force -s closed` is now a legitimate way to ask for
	// the close-policy half alone.
	updateResult, updateErr := runCommandUpdateMutation(st.b.opsCtx, ops, commandUpdateMutation{
		actor:            getActor(),
		issueID:          result.ResolvedID,
		patch:            patch,
		claim:            st.b.claim,
		force:            st.b.force,
		expectedAssignee: st.b.ifAssignee,
		expectedStatus:   st.b.expectedStatus,
	})
	if updateErr != nil {
		fmt.Fprintf(os.Stderr, "Error updating %s: %v\n", id, updateErr)
		st.failures = append(st.failures, updateIDFailure{
			ID:            id,
			Error:         fmt.Sprintf("updating issue: %v", updateErr),
			GuardMismatch: isGuardMismatch(updateErr),
		})
		closeUpdateIfUnmutated(st, result)
		return nil, false, false
	}
	return updateResult.Issue, notesOverwritten, true
}

func collectUpdateSuccess(st *updateBatchState, id string, issue *types.Issue, issueStore storage.DoltStorage, result *RoutedResult, updatedIssue *types.Issue, notesOverwritten bool) {
	trackUpdateMutation(st, result)
	if notesOverwritten {
		st.notesOverwriteWarnings[issueStore] = append(st.notesOverwriteWarnings[issueStore], id)
	}
	auditUpdateFieldChanges(issue, result, st.b.patch)
	recordUpdatedIssue(st, result, updatedIssue)
	closeUpdateIfUnmutated(st, result)
}

func auditUpdateFieldChanges(issue *types.Issue, result *RoutedResult, patch issueops.IssuePatch) {
	// Audit log key field changes (survives Dolt GC flatten)
	if patch.Status.Set {
		audit.LogFieldChange(result.ResolvedID, "status", string(issue.Status), string(patch.Status.Value), getActor(), "")
	}
	if patch.Assignee.Set {
		audit.LogFieldChange(result.ResolvedID, "assignee", issue.Assignee, patch.Assignee.Value, getActor(), "")
	}
	if patch.Priority.Set {
		audit.LogFieldChange(result.ResolvedID, "priority", fmt.Sprintf("%d", issue.Priority), fmt.Sprintf("%d", patch.Priority.Value), getActor(), "")
	}
}

func recordUpdatedIssue(st *updateBatchState, result *RoutedResult, updatedIssue *types.Issue) {
	// The operation's own post-state snapshot replaces the re-read.
	// Dependency records are dropped from it because `bd update` has
	// never printed them: GetIssue did not hydrate them either.
	if updatedIssue != nil {
		updatedIssue.Dependencies = nil
	}
	updateTitle := ""
	if updatedIssue != nil {
		updateTitle = updatedIssue.Title
	}

	if isJSONOutput() {
		if updatedIssue != nil {
			st.updatedIssues = append(st.updatedIssues, updatedIssue)
		}
	} else {
		debug.PrintNormal("%s Updated issue: %s\n", ui.RenderPass("✓"), formatFeedbackID(result.ResolvedID, updateTitle))
	}

	// Track first successful update for last-touched
	if st.firstUpdatedID == "" {
		st.firstUpdatedID = result.ResolvedID
	}
}

func finishUpdateBatch(st *updateBatchState) error {
	if err := commitUpdateMutations(st); err != nil {
		return err
	}
	closePendingUpdateResults(st)

	// Set last touched after all b.updates complete
	if st.firstUpdatedID != "" {
		SetLastTouchedID(st.firstUpdatedID)
	}

	if isJSONOutput() && len(st.updatedIssues) > 0 {
		if jerr := outputJSON(st.updatedIssues); jerr != nil {
			return jerr
		}
	}

	// Updates are per-ID, not atomic across IDs: successful b.updates above
	// stay applied (and committed), but any per-ID failure must surface as
	// a nonzero exit so callers can detect a partial batch (GH audit:
	// multi-ID update used to exit 0 after mid-batch failures).
	if len(st.failures) > 0 {
		return reportUpdateFailures(st.failures, len(st.b.args))
	}
	return nil
}

func commitUpdateMutations(st *updateBatchState) error {
	if len(st.mutatedStores) == 0 {
		return nil
	}
	for s, ids := range st.mutatedStores {
		if s == nil {
			continue
		}
		if err := commitPendingIfEmbedded(st.b.ctx, s, getActor(), doltAutoCommitParams{
			Command:  "update",
			IssueIDs: ids,
		}); err != nil {
			closePendingUpdateResults(st)
			return HandleErrorRespectJSON("failed to commit: %v", err)
		}
		for _, id := range st.notesOverwriteWarnings[s] {
			warnNotesReplacement(id)
		}
	}
	return nil
}
