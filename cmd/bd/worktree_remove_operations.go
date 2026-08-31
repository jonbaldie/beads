package main

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/worktreeremove"
)

type worktreeRevalidationObservation struct {
	facts worktreeremove.RevalidationFacts
	err   error
}

type worktreeRemovalFailureObservation struct {
	facts           worktreeremove.FailureFacts
	reinspectionErr error
	state           string
}

func (git *worktreeRemovalGit) combinedOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return git.command(ctx, dir, args...).CombinedOutput()
}

func (plan *worktreeRemovalPlan) observeRevalidation(ctx context.Context) worktreeRevalidationObservation {
	facts := worktreeremove.RevalidationFacts{}
	currentEntry, obs := plan.observeRegistryRevalidation(ctx, &facts)
	if obs.err != nil {
		return obs
	}
	currentTarget, obs := plan.observeTargetStateRevalidation(ctx, currentEntry, &facts)
	if obs.err != nil {
		return obs
	}
	if obs := plan.observeIdentityRevalidation(currentTarget, &facts); obs.err != nil {
		return obs
	}
	return plan.observeComparatorRevalidation(ctx, currentTarget, facts)
}

func (plan *worktreeRemovalPlan) observeRegistryRevalidation(ctx context.Context, facts *worktreeremove.RevalidationFacts) (registeredWorktree, worktreeRevalidationObservation) {
	worktrees, err := listRegisteredWorktrees(ctx, plan.git, plan.executionRoot)
	if err != nil {
		return registeredWorktree{}, worktreeRevalidationObservation{facts: *facts, err: err}
	}
	currentEntry, found := findRegisteredWorktreeByPath(worktrees, plan.target.identity.path)
	if !found {
		facts.Registration = worktreeremove.InvariantChanged
		return registeredWorktree{}, worktreeRevalidationObservation{facts: *facts, err: fmt.Errorf("target is no longer registered at %s", plan.target.identity.path)}
	}
	if worktreeRegistryIdentity(currentEntry) != plan.target.state.registryID {
		facts.Registration = worktreeremove.InvariantChanged
		plan.observeLockPruneChange(currentEntry, facts)
		return registeredWorktree{}, worktreeRevalidationObservation{facts: *facts, err: fmt.Errorf("registered target identity changed")}
	}
	facts.Registration = worktreeremove.InvariantStable
	facts.LockPrune = worktreeremove.InvariantStable
	facts.TargetPath = worktreeremove.InvariantStable
	return currentEntry, worktreeRevalidationObservation{facts: *facts}
}

func (plan *worktreeRemovalPlan) observeLockPruneChange(currentEntry registeredWorktree, facts *worktreeremove.RevalidationFacts) {
	if currentEntry.locked != plan.target.state.locked || currentEntry.lockReason != plan.target.state.lockReason ||
		currentEntry.prunable != plan.target.state.prunable || currentEntry.pruneReason != plan.target.state.pruneReason {
		facts.LockPrune = worktreeremove.InvariantChanged
	}
}

func (plan *worktreeRemovalPlan) observeTargetStateRevalidation(ctx context.Context, currentEntry registeredWorktree, facts *worktreeremove.RevalidationFacts) (pinnedWorktreeTarget, worktreeRevalidationObservation) {
	currentTarget, err := inspectWorktreeTarget(ctx, plan.git, currentEntry)
	if err != nil {
		return pinnedWorktreeTarget{}, worktreeRevalidationObservation{facts: *facts, err: err}
	}
	if obs := plan.observeTargetPathsAndHead(currentTarget, facts); obs.err != nil {
		return pinnedWorktreeTarget{}, obs
	}
	return currentTarget, plan.observeTargetStatus(currentTarget, facts)
}

func (plan *worktreeRemovalPlan) observeTargetPathsAndHead(currentTarget pinnedWorktreeTarget, facts *worktreeremove.RevalidationFacts) worktreeRevalidationObservation {
	if !sameWorktreePath(currentTarget.identity.gitDir, plan.target.identity.gitDir) {
		facts.GitAdminDirectory = worktreeremove.InvariantChanged
		return worktreeRevalidationObservation{facts: *facts, err: fmt.Errorf("target git directory changed")}
	}
	facts.GitAdminDirectory = worktreeremove.InvariantStable
	if !sameWorktreePath(currentTarget.state.commonDir, plan.commonDir) {
		facts.CommonDirectory = worktreeremove.InvariantChanged
		return worktreeRevalidationObservation{facts: *facts, err: fmt.Errorf("target common git directory changed")}
	}
	facts.CommonDirectory = worktreeremove.InvariantStable
	if currentTarget.state.headOID != plan.target.state.headOID {
		facts.Head = worktreeremove.InvariantChanged
		return worktreeRevalidationObservation{facts: *facts, err: fmt.Errorf("target HEAD changed from %s to %s", plan.target.state.headOID, currentTarget.state.headOID)}
	}
	facts.Head = worktreeremove.InvariantStable
	return worktreeRevalidationObservation{facts: *facts}
}

func (plan *worktreeRemovalPlan) observeTargetStatus(currentTarget pinnedWorktreeTarget, facts *worktreeremove.RevalidationFacts) worktreeRevalidationObservation {
	if currentTarget.state.status != plan.target.state.status {
		if (currentTarget.state.status == "") != (plan.target.state.status == "") {
			facts.Cleanliness = worktreeremove.InvariantChanged
		} else {
			facts.Cleanliness = worktreeremove.InvariantStable
		}
		facts.StatusBytes = worktreeremove.InvariantChanged
		return worktreeRevalidationObservation{facts: *facts, err: fmt.Errorf("target cleanliness changed")}
	}
	facts.Cleanliness = worktreeremove.InvariantStable
	facts.StatusBytes = worktreeremove.InvariantStable
	if currentTarget.state.statusFingerprint != plan.target.state.statusFingerprint {
		facts.DirtyFileFingerprint = worktreeremove.InvariantChanged
		return worktreeRevalidationObservation{facts: *facts, err: fmt.Errorf("target changed files changed")}
	}
	facts.DirtyFileFingerprint = worktreeremove.InvariantStable
	if !plan.force && currentTarget.state.status != "" {
		facts.Cleanliness = worktreeremove.InvariantChanged
		return worktreeRevalidationObservation{facts: *facts, err: fmt.Errorf("target is no longer clean")}
	}
	return worktreeRevalidationObservation{facts: *facts}
}

func (plan *worktreeRemovalPlan) observeIdentityRevalidation(currentTarget pinnedWorktreeTarget, facts *worktreeremove.RevalidationFacts) worktreeRevalidationObservation {
	if obs := plan.observePinnedFileIdentity(currentTarget, facts); obs.err != nil {
		return obs
	}
	if currentTarget.identity.gitDirFingerprint != plan.target.identity.gitDirFingerprint {
		facts.GitAdminDirectoryBytes = worktreeremove.InvariantChanged
		return worktreeRevalidationObservation{facts: *facts, err: fmt.Errorf("target git directory identity changed (contents mismatch)")}
	}
	facts.GitAdminDirectoryBytes = worktreeremove.InvariantStable
	if currentTarget.identity.gitMarkerFingerprint != plan.target.identity.gitMarkerFingerprint {
		facts.GitMarkerBytes = worktreeremove.InvariantChanged
		return worktreeRevalidationObservation{facts: *facts, err: fmt.Errorf("registered target identity changed (git marker mismatch)")}
	}
	facts.GitMarkerBytes = worktreeremove.InvariantStable
	if plan.gitignoreCleanup != nil {
		if err := plan.gitignoreCleanup.validate(); err != nil {
			facts.ManagedIgnore = worktreeremove.InvariantChanged
			return worktreeRevalidationObservation{facts: *facts, err: fmt.Errorf(".gitignore changed before removal: %w", err)}
		}
	}
	facts.ManagedIgnore = worktreeremove.InvariantStable
	return worktreeRevalidationObservation{facts: *facts}
}

func (plan *worktreeRemovalPlan) observePinnedFileIdentity(currentTarget pinnedWorktreeTarget, facts *worktreeremove.RevalidationFacts) worktreeRevalidationObservation {
	if !samePinnedIdentity(currentTarget.identity.pathInfo, plan.target.identity.pathInfo) {
		facts.TargetDirectory = worktreeremove.InvariantChanged
		return worktreeRevalidationObservation{facts: *facts, err: fmt.Errorf("target directory identity changed")}
	}
	facts.TargetDirectory = worktreeremove.InvariantStable
	if !samePinnedIdentity(currentTarget.identity.gitDirInfo, plan.target.identity.gitDirInfo) {
		facts.GitAdminDirectory = worktreeremove.InvariantChanged
		return worktreeRevalidationObservation{facts: *facts, err: fmt.Errorf("target git directory identity changed")}
	}
	facts.GitAdminDirectory = worktreeremove.InvariantStable
	if !samePinnedIdentity(currentTarget.identity.gitMarkerInfo, plan.target.identity.gitMarkerInfo) {
		facts.GitMarker = worktreeremove.InvariantChanged
		return worktreeRevalidationObservation{facts: *facts, err: fmt.Errorf("target git marker identity changed")}
	}
	facts.GitMarker = worktreeremove.InvariantStable
	return worktreeRevalidationObservation{facts: *facts}
}

func samePinnedIdentity(current, previous os.FileInfo) bool {
	return os.SameFile(current, previous) && samePinnedFileMetadata(current, previous)
}

func (plan *worktreeRemovalPlan) observeComparatorRevalidation(ctx context.Context, currentTarget pinnedWorktreeTarget, facts worktreeremove.RevalidationFacts) worktreeRevalidationObservation {
	if plan.comparator == nil {
		facts.Comparator = worktreeremove.InvariantNotRequired
		facts.Containment = worktreeremove.InvariantNotRequired
		return worktreeRevalidationObservation{facts: facts}
	}
	currentComparator, err := plan.resolveComparator(ctx, currentTarget)
	if err != nil {
		return worktreeRevalidationObservation{facts: facts, err: err}
	}
	if currentComparator != *plan.comparator {
		facts.Comparator = worktreeremove.InvariantChanged
		return worktreeRevalidationObservation{facts: facts, err: fmt.Errorf(
			"comparison target changed (was %s, now %s)",
			plan.comparator.oid,
			currentComparator.oid,
		)}
	}
	facts.Comparator = worktreeremove.InvariantStable
	_, err = verifyWorktreeContainment(
		ctx,
		plan.git,
		plan.executionRoot,
		currentTarget.state.headOID,
		currentComparator,
	)
	if err != nil {
		facts.Containment = worktreeremove.InvariantChanged
		return worktreeRevalidationObservation{facts: facts, err: err}
	}
	facts.Containment = worktreeremove.InvariantStable
	return worktreeRevalidationObservation{facts: facts}
}

func (plan *worktreeRemovalPlan) resolveComparator(
	ctx context.Context,
	target pinnedWorktreeTarget,
) (pinnedWorktreeComparator, error) {
	if plan.comparator.explicit {
		return resolveExplicitWorktreeComparator(
			ctx,
			plan.git,
			plan.executionRoot,
			target,
			plan.comparator.selector,
		)
	}
	return resolveUpstreamWorktreeComparator(ctx, plan.git, plan.executionRoot, target)
}

func (plan *worktreeRemovalPlan) observeRemovalFailure(ctx context.Context) worktreeRemovalFailureObservation {
	reinspection := plan.observeRevalidation(ctx)
	registered, registration, listErr := plan.observeRemovalRegistration(ctx)
	pathExists, pathPresence, pathErr := observeRemovalPath(plan.target.identity.path)
	return worktreeRemovalFailureObservation{
		facts: worktreeremove.FailureFacts{
			Revalidation:       reinspection.facts,
			RevalidationResult: revalidationResult(reinspection.err),
			Registration:       registration,
			TargetPath:         pathPresence,
		},
		reinspectionErr: reinspection.err,
		state:           removalFailureState(registered, pathExists, listErr, pathErr),
	}
}

func (plan *worktreeRemovalPlan) observeRemovalRegistration(ctx context.Context) (bool, worktreeremove.Presence, error) {
	worktrees, listErr := listRegisteredWorktrees(ctx, plan.git, plan.executionRoot)
	if listErr != nil {
		return false, worktreeremove.PresenceUnknown, listErr
	}
	_, registered := findRegisteredWorktreeByPath(worktrees, plan.target.identity.path)
	if registered {
		return true, worktreeremove.Present, nil
	}
	return false, worktreeremove.Missing, nil
}

func observeRemovalPath(path string) (bool, worktreeremove.Presence, error) {
	_, pathErr := os.Lstat(path)
	if pathErr == nil {
		return true, worktreeremove.Present, nil
	}
	if os.IsNotExist(pathErr) {
		return false, worktreeremove.Missing, pathErr
	}
	return true, worktreeremove.PresenceUnknown, pathErr
}

func removalFailureState(registered, pathExists bool, listErr, pathErr error) string {
	state := fmt.Sprintf("registered=%t, path_exists=%t", registered, pathExists)
	if listErr != nil {
		state += fmt.Sprintf(", registry inspection failed: %v", listErr)
	}
	if pathErr != nil && !os.IsNotExist(pathErr) {
		state += fmt.Sprintf(", path inspection failed: %v", pathErr)
	}
	return state
}

func (plan *gitignoreCleanupPlan) apply() error {
	if err := plan.validate(); err != nil {
		return err
	}
	tempPath, cleanup, err := plan.writeGitignoreTemp()
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			cleanup()
		}
	}()
	if err := plan.validate(); err != nil {
		return fmt.Errorf("destination changed before atomic cleanup: %w", err)
	}
	if err := os.Rename(tempPath, plan.path); err != nil {
		return fmt.Errorf("atomically replace %s: %w", plan.path, err)
	}
	committed = true
	return nil
}

func (plan *gitignoreCleanupPlan) writeGitignoreTemp() (string, func(), error) {
	temp, err := os.CreateTemp(plan.repoRoot, ".gitignore.bd-*")
	if err != nil {
		return "", nil, err
	}
	tempPath := temp.Name()
	cleanup := func() { _ = os.Remove(tempPath) }
	if err := writeGitignoreTempContents(temp, plan); err != nil {
		cleanup()
		return "", nil, err
	}
	return tempPath, cleanup, nil
}

func writeGitignoreTempContents(temp *os.File, plan *gitignoreCleanupPlan) error {
	if err := temp.Chmod(plan.info.Mode().Perm()); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(plan.updated); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	return temp.Close()
}

func (plan *gitignoreCleanupPlan) validate() error {
	currentInfo, currentContent, err := readStableRegularFile(plan.path)
	if err != nil {
		return err
	}
	if !samePinnedFileMetadata(currentInfo, plan.info) ||
		!bytes.Equal(currentContent, plan.original) {
		return fmt.Errorf("%s changed after removal was prepared", plan.path)
	}
	return nil
}
