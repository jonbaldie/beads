package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type worktreeRemovalGit struct {
	executable string
	env        []string
}

func newWorktreeRemovalGit() (*worktreeRemovalGit, error) {
	executable, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("cannot find git executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, fmt.Errorf("cannot pin git executable path: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}

	env := scrubWorktreeRemovalGitEnv(os.Environ())
	env = append(
		env,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TEMPLATE_DIR=",
	)

	return &worktreeRemovalGit{
		executable: executable,
		env:        env,
	}, nil
}

func scrubWorktreeRemovalGitEnv(env []string) []string {
	exactKeys := map[string]struct{}{
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": {},
		"GIT_CEILING_DIRECTORIES":          {},
		"GIT_COMMON_DIR":                   {},
		"GIT_DIR":                          {},
		"GIT_DISCOVERY_ACROSS_FILESYSTEM":  {},
		"GIT_EXEC_PATH":                    {},
		"GIT_GRAFT_FILE":                   {},
		"GIT_IMPLICIT_WORK_TREE":           {},
		"GIT_INDEX_FILE":                   {},
		"GIT_INTERNAL_SUPER_PREFIX":        {},
		"GIT_NAMESPACE":                    {},
		"GIT_NO_REPLACE_OBJECTS":           {},
		"GIT_OBJECT_DIRECTORY":             {},
		"GIT_OPTIONAL_LOCKS":               {},
		"GIT_PREFIX":                       {},
		"GIT_QUARANTINE_PATH":              {},
		"GIT_REPLACE_REF_BASE":             {},
		"GIT_SHALLOW_FILE":                 {},
		"GIT_SUPER_PREFIX":                 {},
		"GIT_TEMPLATE_DIR":                 {},
		"GIT_WORK_TREE":                    {},
	}

	cleaned := make([]string, 0, len(env))
	for _, entry := range env {
		key := entry
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			key = entry[:separator]
		}
		upperKey := strings.ToUpper(key)
		if strings.HasPrefix(upperKey, "GIT_CONFIG") {
			continue
		}
		if _, blocked := exactKeys[upperKey]; blocked {
			continue
		}
		cleaned = append(cleaned, entry)
	}
	return cleaned
}

func (git *worktreeRemovalGit) command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	gitArgs := make([]string, 0, len(args)+4)
	gitArgs = append(gitArgs, "-c", "core.hooksPath=", "-c", "core.fsmonitor=false")
	gitArgs = append(gitArgs, args...)

	command := exec.CommandContext(ctx, git.executable, gitArgs...)
	command.Dir = dir
	command.Env = append([]string(nil), git.env...)
	return command
}

func (git *worktreeRemovalGit) output(ctx context.Context, dir string, args ...string) ([]byte, error) {
	command := git.command(ctx, dir, args...)
	output, err := command.Output()
	if err == nil {
		return output, nil
	}

	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		stderr := strings.TrimSpace(string(exitError.Stderr))
		if stderr != "" {
			return output, fmt.Errorf("%w: %s", err, stderr)
		}
	}
	return output, err
}

type registeredWorktree struct {
	path        string
	headOID     string
	branch      string
	detached    bool
	bare        bool
	locked      bool
	lockReason  string
	prunable    bool
	pruneReason string
	isMain      bool
}

func listRegisteredWorktrees(
	ctx context.Context,
	git *worktreeRemovalGit,
	executionRoot string,
) ([]registeredWorktree, error) {
	output, err := git.output(ctx, executionRoot, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, fmt.Errorf("failed to read git worktree registry: %w", err)
	}

	var worktrees []registeredWorktree
	var current registeredWorktree

	for _, field := range strings.Split(string(output), "\x00") {
		if field == "" {
			continue
		}
		if strings.HasPrefix(field, "worktree ") {
			appendRegisteredWorktree(&worktrees, &current)
			current.path = strings.TrimPrefix(field, "worktree ")
			continue
		}
		parseRegisteredWorktreeField(field, &current)
	}
	appendRegisteredWorktree(&worktrees, &current)

	if len(worktrees) == 0 {
		return nil, fmt.Errorf("git worktree registry is empty")
	}
	return worktrees, nil
}

func appendRegisteredWorktree(worktrees *[]registeredWorktree, current *registeredWorktree) {
	if current.path == "" {
		return
	}
	current.isMain = len(*worktrees) == 0
	*worktrees = append(*worktrees, *current)
	*current = registeredWorktree{}
}

func parseRegisteredWorktreeField(field string, current *registeredWorktree) {
	switch {
	case strings.HasPrefix(field, "HEAD "):
		current.headOID = strings.TrimPrefix(field, "HEAD ")
	case strings.HasPrefix(field, "branch "):
		current.branch = strings.TrimPrefix(field, "branch ")
	case field == "detached":
		current.detached = true
	case field == "bare":
		current.bare = true
	case field == "locked":
		current.locked = true
	case strings.HasPrefix(field, "locked "):
		current.locked = true
		current.lockReason = strings.TrimPrefix(field, "locked ")
	case field == "prunable":
		current.prunable = true
	case strings.HasPrefix(field, "prunable "):
		current.prunable = true
		current.pruneReason = strings.TrimPrefix(field, "prunable ")
	}
}

func sameWorktreePath(left, right string) bool {
	leftAbsolute, leftErr := filepath.Abs(left)
	rightAbsolute, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	leftAbsolute = filepath.Clean(leftAbsolute)
	rightAbsolute = filepath.Clean(rightAbsolute)

	// When both paths are missing, peel exact components in lockstep until
	// existing ancestors can prove physical identity. This accepts equivalent
	// ancestor spellings such as a Windows 8.3 alias without ever case-folding
	// an unresolved component.
	for {
		same, bothMissing, valid := worktreePathIdentity(leftAbsolute, rightAbsolute)
		if !valid {
			return false
		}
		if !bothMissing {
			return same
		}
		leftParent, rightParent, ok := parentWorktreePaths(leftAbsolute, rightAbsolute)
		if !ok {
			return false
		}
		leftAbsolute = leftParent
		rightAbsolute = rightParent
	}
}

func worktreePathIdentity(left, right string) (same, bothMissing, valid bool) {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr == nil || rightErr == nil {
		return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo), false, true
	}
	if !os.IsNotExist(leftErr) || !os.IsNotExist(rightErr) {
		return false, false, false
	}
	return false, true, true
}

func parentWorktreePaths(left, right string) (string, string, bool) {
	if filepath.Base(left) != filepath.Base(right) {
		return "", "", false
	}
	leftParent := filepath.Dir(left)
	rightParent := filepath.Dir(right)
	if leftParent == left || rightParent == right {
		return "", "", false
	}
	return leftParent, rightParent, true
}

func findRegisteredWorktreeByPath(
	worktrees []registeredWorktree,
	path string,
) (registeredWorktree, bool) {
	for _, worktree := range worktrees {
		if sameWorktreePath(worktree.path, path) {
			return worktree, true
		}
	}
	return registeredWorktree{}, false
}

func resolveRegisteredWorktree(
	name string,
	currentRoot string,
	mainRoot string,
	worktrees []registeredWorktree,
) (registeredWorktree, error) {
	candidatePaths := registeredWorktreeCandidatePaths(name, currentRoot, mainRoot)
	matches := matchingRegisteredWorktrees(worktrees, candidatePaths, name)

	switch len(matches) {
	case 0:
		return registeredWorktree{}, fmt.Errorf("registered worktree not found: %s", name)
	case 1:
		return matches[0], nil
	default:
		paths := make([]string, 0, len(matches))
		for _, match := range matches {
			paths = append(paths, match.path)
		}
		sort.Strings(paths)
		return registeredWorktree{}, fmt.Errorf(
			"worktree name %q is ambiguous; use an absolute path (matches: %s)",
			name,
			strings.Join(paths, ", "),
		)
	}
}

func registeredWorktreeCandidatePaths(name, currentRoot, mainRoot string) []string {
	if filepath.IsAbs(name) {
		return []string{name}
	}
	candidatePaths := make([]string, 0, 3)
	if currentCandidate, err := filepath.Abs(name); err == nil {
		candidatePaths = append(candidatePaths, currentCandidate)
	}
	candidatePaths = append(candidatePaths, filepath.Join(currentRoot, name))
	if !sameWorktreePath(currentRoot, mainRoot) {
		candidatePaths = append(candidatePaths, filepath.Join(mainRoot, name))
	}
	return candidatePaths
}

func matchingRegisteredWorktrees(
	worktrees []registeredWorktree,
	candidatePaths []string,
	name string,
) []registeredWorktree {
	allowBasename := filepath.Base(name) == name
	matches := make([]registeredWorktree, 0, 1)
	for _, worktree := range worktrees {
		if !registeredWorktreeMatches(worktree, candidatePaths, name, allowBasename) {
			continue
		}
		if !containsRegisteredWorktree(matches, worktree) {
			matches = append(matches, worktree)
		}
	}
	return matches
}

func registeredWorktreeMatches(
	worktree registeredWorktree,
	candidatePaths []string,
	name string,
	allowBasename bool,
) bool {
	for _, candidate := range candidatePaths {
		if sameWorktreePath(worktree.path, candidate) {
			return true
		}
	}
	return allowBasename && filepath.Base(worktree.path) == name
}

func containsRegisteredWorktree(worktrees []registeredWorktree, candidate registeredWorktree) bool {
	for _, worktree := range worktrees {
		if sameWorktreePath(worktree.path, candidate.path) {
			return true
		}
	}
	return false
}
