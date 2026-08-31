package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/jonbaldie/beads/internal/worktreeremove"
)

type pinnedWorktreeComparator struct {
	selector    string
	explicit    bool
	ref         string
	terminalRef string
	oid         string
}

func resolveExplicitWorktreeComparator(
	ctx context.Context,
	git *worktreeRemovalGit,
	executionRoot string,
	target pinnedWorktreeTarget,
	selector string,
) (pinnedWorktreeComparator, error) {
	if isWorktreeLocalPseudoref(selector) {
		return pinnedWorktreeComparator{}, fmt.Errorf(
			"--merged-into value %q is a worktree-local pseudoref; use a full shared ref or full commit object ID",
			selector,
		)
	}

	if strings.HasPrefix(selector, "refs/") {
		return resolveExplicitFullRefComparator(ctx, git, executionRoot, target, selector)
	}
	return resolveExplicitShortComparator(ctx, git, executionRoot, target, selector)
}

func resolveExplicitFullRefComparator(
	ctx context.Context,
	git *worktreeRemovalGit,
	executionRoot string,
	target pinnedWorktreeTarget,
	selector string,
) (pinnedWorktreeComparator, error) {
	if isWorktreeLocalRef(selector) {
		return pinnedWorktreeComparator{}, fmt.Errorf(
			"--merged-into value %q is in a worktree-local ref namespace; use a shared ref or full commit object ID",
			selector,
		)
	}
	valid, err := gitRefNameIsValid(ctx, git, executionRoot, selector, false)
	if err != nil {
		return pinnedWorktreeComparator{}, err
	}
	if !valid {
		return pinnedWorktreeComparator{}, fmt.Errorf("--merged-into value %q is not a valid full ref", selector)
	}
	return pinWorktreeComparatorRef(ctx, git, executionRoot, target, selector, selector, true)
}

func resolveExplicitShortComparator(
	ctx context.Context,
	git *worktreeRemovalGit,
	executionRoot string,
	target pinnedWorktreeTarget,
	selector string,
) (pinnedWorktreeComparator, error) {
	valid, err := gitRefNameIsValid(ctx, git, executionRoot, selector, true)
	if err != nil {
		return pinnedWorktreeComparator{}, err
	}
	if !valid {
		return pinnedWorktreeComparator{}, fmt.Errorf(
			"--merged-into value %q is not an accepted ref name or full commit object ID",
			selector,
		)
	}

	matches, fullOID, err := inspectExplicitShortSelector(ctx, git, executionRoot, selector)
	if err != nil {
		return pinnedWorktreeComparator{}, err
	}
	if len(matches) > 1 || fullOID && len(matches) > 0 {
		return pinnedWorktreeComparator{}, fmt.Errorf(
			"--merged-into value %q is ambiguous; use a full ref name or a non-ref full object ID (matches: %s)",
			selector,
			strings.Join(matches, ", "),
		)
	}
	if len(matches) == 1 {
		return pinWorktreeComparatorRef(ctx, git, executionRoot, target, selector, matches[0], true)
	}
	if !fullOID {
		return pinnedWorktreeComparator{}, fmt.Errorf(
			"--merged-into value %q does not name an existing unambiguous ref",
			selector,
		)
	}
	return resolveExplicitObjectComparator(ctx, git, executionRoot, target, selector)
}

func inspectExplicitShortSelector(
	ctx context.Context,
	git *worktreeRemovalGit,
	executionRoot string,
	selector string,
) ([]string, bool, error) {
	matches, err := findWorktreeComparatorRefs(ctx, git, executionRoot, selector)
	if err != nil {
		return nil, false, err
	}
	hashLength, err := repositoryObjectIDLength(ctx, git, executionRoot)
	if err != nil {
		return nil, false, err
	}
	return matches, isHexObjectID(selector, hashLength), nil
}

func resolveExplicitObjectComparator(
	ctx context.Context,
	git *worktreeRemovalGit,
	executionRoot string,
	target pinnedWorktreeTarget,
	selector string,
) (pinnedWorktreeComparator, error) {
	oid, err := resolveWorktreeCommitOID(ctx, git, executionRoot, selector)
	if err != nil {
		return pinnedWorktreeComparator{}, fmt.Errorf(
			"--merged-into object ID %q does not resolve to a commit: %w",
			selector,
			err,
		)
	}
	if oid == target.state.headOID {
		return pinnedWorktreeComparator{}, fmt.Errorf(
			"--merged-into object ID %q is the target HEAD itself; use a ref or descendant commit that independently proves containment",
			selector,
		)
	}
	return pinnedWorktreeComparator{
		selector: selector,
		explicit: true,
		oid:      oid,
	}, nil
}

func gitRefNameIsValid(
	ctx context.Context,
	git *worktreeRemovalGit,
	executionRoot string,
	ref string,
	allowOneLevel bool,
) (bool, error) {
	args := []string{"check-ref-format"}
	if allowOneLevel {
		args = append(args, "--allow-onelevel")
	}
	args = append(args, ref)
	_, err := git.output(ctx, executionRoot, args...)
	if err == nil {
		return true, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("failed to validate git ref name %q: %w", ref, err)
}

func findWorktreeComparatorRefs(
	ctx context.Context,
	git *worktreeRemovalGit,
	executionRoot string,
	selector string,
) ([]string, error) {
	candidates := []string{
		"refs/" + selector,
		"refs/tags/" + selector,
		"refs/heads/" + selector,
		"refs/remotes/" + selector,
		"refs/remotes/" + selector + "/HEAD",
	}
	candidateSet := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		candidateSet[candidate] = struct{}{}
	}

	args := []string{"for-each-ref", "--format=%(refname)", "--"}
	args = append(args, candidates...)
	output, err := git.output(ctx, executionRoot, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to enumerate --merged-into ref %q: %w", selector, err)
	}

	matchSet := make(map[string]struct{})
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		ref := strings.TrimSpace(line)
		if _, candidate := candidateSet[ref]; candidate {
			matchSet[ref] = struct{}{}
		}
	}
	matches := make([]string, 0, len(matchSet))
	for ref := range matchSet {
		matches = append(matches, ref)
	}
	sort.Strings(matches)
	return matches, nil
}

func repositoryObjectIDLength(
	ctx context.Context,
	git *worktreeRemovalGit,
	executionRoot string,
) (int, error) {
	output, err := git.output(ctx, executionRoot, "rev-parse", "--show-object-format")
	if err != nil {
		return 0, fmt.Errorf("failed to determine repository object format: %w", err)
	}
	switch strings.TrimSpace(string(output)) {
	case "sha1":
		return 40, nil
	case "sha256":
		return 64, nil
	default:
		return 0, fmt.Errorf("unsupported git object format %q", strings.TrimSpace(string(output)))
	}
}

func isHexObjectID(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if character >= '0' && character <= '9' ||
			character >= 'a' && character <= 'f' ||
			character >= 'A' && character <= 'F' {
			continue
		}
		return false
	}
	return true
}

func pinWorktreeComparatorRef(
	ctx context.Context,
	git *worktreeRemovalGit,
	executionRoot string,
	target pinnedWorktreeTarget,
	selector string,
	ref string,
	explicit bool,
) (pinnedWorktreeComparator, error) {
	terminalRef, err := resolveWorktreeTerminalRef(ctx, git, executionRoot, ref)
	if err != nil {
		return pinnedWorktreeComparator{}, err
	}
	if isWorktreeLocalRef(ref) || isWorktreeLocalRef(terminalRef) {
		return pinnedWorktreeComparator{}, fmt.Errorf(
			"comparison ref %q resolves through a worktree-local ref namespace and cannot independently prove containment",
			ref,
		)
	}
	if ref == target.state.branch || terminalRef == target.state.branch {
		return pinnedWorktreeComparator{}, fmt.Errorf(
			"comparison ref %q resolves to the target worktree branch and cannot independently prove containment",
			ref,
		)
	}
	oid, err := resolveWorktreeCommitOID(ctx, git, executionRoot, ref)
	if err != nil {
		return pinnedWorktreeComparator{}, fmt.Errorf("comparison ref %q does not resolve to a commit: %w", ref, err)
	}
	return pinnedWorktreeComparator{
		selector:    selector,
		explicit:    explicit,
		ref:         ref,
		terminalRef: terminalRef,
		oid:         oid,
	}, nil
}

func resolveWorktreeTerminalRef(
	ctx context.Context,
	git *worktreeRemovalGit,
	executionRoot string,
	ref string,
) (string, error) {
	current := ref
	seen := make(map[string]struct{})
	for range 16 {
		if _, duplicate := seen[current]; duplicate {
			return "", fmt.Errorf("symbolic ref cycle while resolving %q", ref)
		}
		seen[current] = struct{}{}

		output, err := git.output(ctx, executionRoot, "symbolic-ref", "--quiet", current)
		if err == nil {
			next := strings.TrimSpace(string(output))
			if !strings.HasPrefix(next, "refs/") {
				return "", fmt.Errorf("symbolic ref %q resolves outside refs/: %q", current, next)
			}
			current = next
			continue
		}
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
			return current, nil
		}
		return "", fmt.Errorf("failed to inspect symbolic ref %q: %w", current, err)
	}
	return "", fmt.Errorf("symbolic ref chain for %q exceeds 16 links", ref)
}

func resolveWorktreeCommitOID(
	ctx context.Context,
	git *worktreeRemovalGit,
	executionRoot string,
	refOrOID string,
) (string, error) {
	output, err := git.output(
		ctx,
		executionRoot,
		"rev-parse",
		"--verify",
		"--quiet",
		"--end-of-options",
		refOrOID+"^{commit}",
	)
	if err != nil {
		return "", err
	}
	oid := strings.TrimSpace(string(output))
	hashLength, err := repositoryObjectIDLength(ctx, git, executionRoot)
	if err != nil {
		return "", err
	}
	if !isHexObjectID(oid, hashLength) {
		return "", fmt.Errorf("git returned invalid commit object ID %q", oid)
	}
	return strings.ToLower(oid), nil
}

func resolveUpstreamWorktreeComparator(
	ctx context.Context,
	git *worktreeRemovalGit,
	executionRoot string,
	target pinnedWorktreeTarget,
) (pinnedWorktreeComparator, error) {
	if target.state.branch == "" || target.state.detached {
		return pinnedWorktreeComparator{}, fmt.Errorf(
			"cannot verify unpushed commits: target is detached; use --merged-into <ref>",
		)
	}
	output, err := git.output(
		ctx,
		executionRoot,
		"for-each-ref",
		"--format=%(upstream)",
		"--",
		target.state.branch,
	)
	if err != nil {
		return pinnedWorktreeComparator{}, fmt.Errorf("failed to inspect configured upstream: %w", err)
	}
	upstreamRef := strings.TrimSpace(string(output))
	if upstreamRef == "" || strings.Contains(upstreamRef, "\n") {
		return pinnedWorktreeComparator{}, fmt.Errorf(
			"cannot verify unpushed commits: the target branch has no single resolvable upstream; configure an upstream or use --merged-into <ref>",
		)
	}
	return pinWorktreeComparatorRef(
		ctx,
		git,
		executionRoot,
		target,
		upstreamRef,
		upstreamRef,
		false,
	)
}

func verifyWorktreeContainment(
	ctx context.Context,
	git *worktreeRemovalGit,
	executionRoot string,
	headOID string,
	comparator pinnedWorktreeComparator,
) (worktreeremove.Containment, error) {
	output, err := git.output(
		ctx,
		executionRoot,
		"merge-base",
		"--is-ancestor",
		headOID,
		comparator.oid,
	)
	if err == nil {
		return worktreeremove.Contained, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
		if comparator.explicit {
			return worktreeremove.NotContained, fmt.Errorf(
				"worktree HEAD %s is not contained in --merged-into value %q at %s",
				headOID,
				comparator.selector,
				comparator.oid,
			)
		}
		return worktreeremove.NotContained, fmt.Errorf("worktree has commits not contained in its configured upstream")
	}
	return worktreeremove.ContainmentUnknown, fmt.Errorf(
		"failed to verify worktree containment (%s in %s): %w\n%s",
		headOID,
		comparator.oid,
		err,
		strings.TrimSpace(string(output)),
	)
}

func getWorktreeCurrentBranch(ctx context.Context, dir string) string {
	gitCmd := gitCmdInDir(ctx, dir, "branch", "--show-current")
	output, err := gitCmd.CombinedOutput()
	if err != nil {
		return "(unknown)"
	}
	return strings.TrimSpace(string(output))
}
