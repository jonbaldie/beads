package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/worktreeremove"
)

type worktreeRemovalPlan struct {
	git              *worktreeRemovalGit
	executionRoot    string
	mainWorktree     string
	commonDir        string
	target           pinnedWorktreeTarget
	comparator       *pinnedWorktreeComparator
	force            bool
	gitignoreCleanup *gitignoreCleanupPlan
	prepareFacts     worktreeremove.PrepareFacts
	prepareErr       error
}

type worktreeRemovalSelection struct {
	git          *worktreeRemovalGit
	currentRoot  string
	mainWorktree registeredWorktree
	targetEntry  registeredWorktree
}

func prepareWorktreeRemoval(
	ctx context.Context,
	name string,
	options *worktreeRemoveOptions,
	afterTargetResolution func() error,
) (*worktreeRemovalPlan, error) {
	selection, err := resolveWorktreeRemovalSelection(ctx, name)
	if err != nil {
		return nil, err
	}
	if plan, done := initialWorktreeRemovalPlan(selection, options); done {
		return plan, nil
	}
	if afterTargetResolution != nil {
		if err := afterTargetResolution(); err != nil {
			return nil, fmt.Errorf("worktree removal interrupted after target resolution: %w", err)
		}
	}
	return prepareRegisteredWorktreeRemoval(ctx, selection, options)
}

func resolveWorktreeRemovalSelection(
	ctx context.Context,
	name string,
) (worktreeRemovalSelection, error) {
	gitRunner, err := newWorktreeRemovalGit()
	if err != nil {
		return worktreeRemovalSelection{}, err
	}
	currentDirectory, err := os.Getwd()
	if err != nil {
		return worktreeRemovalSelection{}, fmt.Errorf("failed to resolve current directory: %w", err)
	}
	currentRootOutput, err := gitRunner.output(ctx, currentDirectory, "rev-parse", "--show-toplevel")
	if err != nil {
		return worktreeRemovalSelection{}, fmt.Errorf("not in a git worktree: %w", err)
	}
	currentRoot := filepath.Clean(strings.TrimSpace(string(currentRootOutput)))
	worktrees, err := listRegisteredWorktrees(ctx, gitRunner, currentRoot)
	if err != nil {
		return worktreeRemovalSelection{}, err
	}
	mainWorktree := worktrees[0]
	if mainWorktree.bare {
		return worktreeRemovalSelection{}, fmt.Errorf("cannot remove a worktree when the primary worktree is bare")
	}
	targetEntry, err := resolveRegisteredWorktree(name, currentRoot, mainWorktree.path, worktrees)
	if err != nil {
		return worktreeRemovalSelection{}, err
	}
	return worktreeRemovalSelection{
		git:          gitRunner,
		currentRoot:  currentRoot,
		mainWorktree: mainWorktree,
		targetEntry:  targetEntry,
	}, nil
}

func initialWorktreeRemovalPlan(
	selection worktreeRemovalSelection,
	options *worktreeRemoveOptions,
) (*worktreeRemovalPlan, bool) {
	plan := &worktreeRemovalPlan{
		force: options.force.value,
		prepareFacts: worktreeremove.PrepareFacts{
			Registration: worktreeremove.Present,
		},
	}
	switch {
	case selection.targetEntry.isMain:
		plan.prepareFacts.Target = worktreeremove.PrimaryWorktree
	case sameWorktreePath(selection.targetEntry.path, selection.currentRoot):
		plan.prepareFacts.Target = worktreeremove.CurrentWorktree
	default:
		return nil, false
	}
	return plan, true
}

func prepareRegisteredWorktreeRemoval(
	ctx context.Context,
	selection worktreeRemovalSelection,
	options *worktreeRemoveOptions,
) (*worktreeRemovalPlan, error) {
	mainCommonDir, err := resolveMainWorktreeCommonDir(ctx, selection.git, selection.mainWorktree)
	if err != nil {
		return nil, err
	}
	target, err := inspectWorktreeTarget(ctx, selection.git, selection.targetEntry)
	if err != nil {
		return nil, err
	}
	plan := newRegisteredWorktreeRemovalPlan(selection, target, mainCommonDir, options)
	if !sameWorktreePath(target.state.commonDir, mainCommonDir) {
		plan.prepareFacts.CommonDir = worktreeremove.Unmatched
		plan.prepareErr = fmt.Errorf(
			"target common git directory %q does not match repository %q",
			target.state.commonDir,
			mainCommonDir,
		)
		return plan, nil
	}
	if err := prepareWorktreeRemovalGitignore(plan); err != nil {
		return nil, err
	}
	return prepareWorktreeRemovalSafety(ctx, plan, selection.git, options)
}

func resolveMainWorktreeCommonDir(
	ctx context.Context,
	git *worktreeRemovalGit,
	mainWorktree registeredWorktree,
) (string, error) {
	mainCommonDirOutput, err := git.output(
		ctx,
		mainWorktree.path,
		"rev-parse",
		"--path-format=absolute",
		"--git-common-dir",
	)
	if err != nil {
		return "", fmt.Errorf("failed to resolve repository common git directory: %w", err)
	}
	return filepath.Clean(strings.TrimSpace(string(mainCommonDirOutput))), nil
}

func newRegisteredWorktreeRemovalPlan(
	selection worktreeRemovalSelection,
	target pinnedWorktreeTarget,
	mainCommonDir string,
	options *worktreeRemoveOptions,
) *worktreeRemovalPlan {
	status := worktreeremove.Clean
	if target.state.status != "" {
		status = worktreeremove.Dirty
	}
	return &worktreeRemovalPlan{
		git:           selection.git,
		executionRoot: filepath.Clean(selection.mainWorktree.path),
		mainWorktree:  filepath.Clean(selection.mainWorktree.path),
		commonDir:     mainCommonDir,
		target:        target,
		force:         options.force.value,
		prepareFacts: worktreeremove.PrepareFacts{
			Registration:   worktreeremove.Present,
			Target:         worktreeremove.RegisteredTarget,
			RegisteredPath: target.identity.path,
			TargetDir:      worktreeremove.Present,
			GitAdminDir:    worktreeremove.Present,
			GitMarker:      worktreeremove.Present,
			CommonDir:      worktreeremove.Matched,
			Head:           worktreeremove.Present,
			Status:         status,
			ManagedIgnore:  worktreeremove.IgnoreAbsent,
		},
	}
}

func prepareWorktreeRemovalGitignore(plan *worktreeRemovalPlan) error {
	if relative, inside := relativeWorktreePath(plan.mainWorktree, plan.target.identity.path); inside {
		cleanup, err := prepareGitignoreCleanup(plan.mainWorktree, relative)
		if err != nil {
			return fmt.Errorf("cannot safely prepare .gitignore cleanup: %w", err)
		}
		plan.gitignoreCleanup = cleanup
		if plan.gitignoreCleanup != nil {
			plan.prepareFacts.ManagedIgnore = worktreeremove.IgnoreManaged
			plan.prepareFacts.ManagedIgnoreEntry = plan.gitignoreCleanup.entry
		}
	}
	return nil
}

func prepareWorktreeRemovalSafety(
	ctx context.Context,
	plan *worktreeRemovalPlan,
	git *worktreeRemovalGit,
	options *worktreeRemoveOptions,
) (*worktreeRemovalPlan, error) {
	if options.force.value {
		plan.prepareFacts.Comparator = worktreeremove.ComparatorNotRequired
		plan.prepareFacts.Containment = worktreeremove.ContainmentNotRequired
		return plan, nil
	}
	if plan.target.state.status != "" {
		return plan, nil
	}

	comparator, err := resolveWorktreeRemovalComparator(ctx, plan, git, options)
	if err != nil {
		plan.prepareFacts.Comparator = worktreeremove.ComparatorMissing
		plan.prepareErr = err
		return plan, nil
	}
	plan.prepareFacts.Comparator = worktreeremove.ComparatorAvailable
	containment, err := verifyWorktreeContainment(ctx, git, plan.executionRoot, plan.target.state.headOID, comparator)
	plan.prepareFacts.Containment = containment
	if err != nil {
		if containment == worktreeremove.ContainmentUnknown {
			return nil, err
		}
		plan.prepareErr = err
		return plan, nil
	}
	plan.comparator = &comparator

	return plan, nil
}

func resolveWorktreeRemovalComparator(
	ctx context.Context,
	plan *worktreeRemovalPlan,
	git *worktreeRemovalGit,
	options *worktreeRemoveOptions,
) (pinnedWorktreeComparator, error) {
	if options.mergedInto.set {
		return resolveExplicitWorktreeComparator(
			ctx,
			git,
			plan.executionRoot,
			plan.target,
			options.mergedInto.value,
		)
	}
	return resolveUpstreamWorktreeComparator(ctx, git, plan.executionRoot, plan.target)
}

func relativeWorktreePath(root, target string) (string, bool) {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." || filepath.IsAbs(relative) {
		return "", false
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(relative), true
}

func samePinnedFileMetadata(current, pinned os.FileInfo) bool {
	return os.SameFile(current, pinned) &&
		current.Mode() == pinned.Mode() &&
		current.Size() == pinned.Size() &&
		current.ModTime().Equal(pinned.ModTime())
}

func revalidationResult(err error) worktreeremove.RevalidationResult {
	if err != nil {
		return worktreeremove.RevalidationFailed
	}
	return worktreeremove.RevalidationPassed
}

func formatWorktreeRemovalFailure(
	kind worktreeremove.FailureKind,
	observation worktreeRemovalFailureObservation,
	removeErr error,
	output []byte,
) error {
	diagnostic := strings.TrimSpace(string(output))
	if kind == worktreeremove.UnchangedFailure {
		if diagnostic == "" {
			return fmt.Errorf(
				"git worktree remove failed, but the target was revalidated unchanged: %w",
				removeErr,
			)
		}
		return fmt.Errorf(
			"git worktree remove failed, but the target was revalidated unchanged: %w\n%s",
			removeErr,
			diagnostic,
		)
	}
	if diagnostic == "" {
		return fmt.Errorf(
			"git worktree remove failed and target state is partial or indeterminate (%s): %w; reinspection: %v",
			observation.state,
			removeErr,
			observation.reinspectionErr,
		)
	}
	return fmt.Errorf(
		"git worktree remove failed and target state is partial or indeterminate (%s): %w; reinspection: %v\n%s",
		observation.state,
		removeErr,
		observation.reinspectionErr,
		diagnostic,
	)
}

func isWorktreeLocalPseudoref(selector string) bool {
	switch selector {
	case "HEAD",
		"AUTO_MERGE",
		"BISECT_EXPECTED_REV",
		"MERGE_AUTOSTASH",
		"NOTES_MERGE_PARTIAL",
		"NOTES_MERGE_REF":
		return true
	}
	if !strings.HasSuffix(selector, "_HEAD") {
		return false
	}
	return validWorktreeLocalPseudorefPrefix(strings.TrimSuffix(selector, "_HEAD"))
}

func validWorktreeLocalPseudorefPrefix(selector string) bool {
	if selector == "" {
		return false
	}
	for _, character := range selector {
		if character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '_' {
			continue
		}
		return false
	}
	return true
}

func isWorktreeLocalRef(ref string) bool {
	for _, namespace := range []string{
		"refs/bisect",
		"refs/rewritten",
		"refs/worktree",
	} {
		if ref == namespace || strings.HasPrefix(ref, namespace+"/") {
			return true
		}
	}
	return false
}
