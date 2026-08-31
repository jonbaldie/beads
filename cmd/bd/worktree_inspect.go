package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type pinnedWorktreeTargetIdentity struct {
	path                 string
	pathInfo             os.FileInfo
	gitDir               string
	gitDirInfo           os.FileInfo
	gitMarkerInfo        os.FileInfo
	gitDirFingerprint    string
	gitMarkerFingerprint string
}

type pinnedWorktreeTargetState struct {
	commonDir         string
	headOID           string
	branch            string
	detached          bool
	bare              bool
	locked            bool
	lockReason        string
	prunable          bool
	pruneReason       string
	status            string
	statusFingerprint string
	registryID        string
}

type pinnedWorktreeTarget struct {
	identity pinnedWorktreeTargetIdentity
	state    pinnedWorktreeTargetState
}

func inspectWorktreeTarget(
	ctx context.Context,
	git *worktreeRemovalGit,
	worktree registeredWorktree,
) (pinnedWorktreeTarget, error) {
	gitState, err := inspectWorktreeGitState(ctx, git, worktree)
	if err != nil {
		return pinnedWorktreeTarget{}, err
	}
	if gitState.headOID == "" || gitState.headOID != worktree.headOID {
		return pinnedWorktreeTarget{}, fmt.Errorf(
			"target HEAD disagrees with git worktree registry (registry %q, target %q)",
			worktree.headOID,
			gitState.headOID,
		)
	}
	identity, err := pinWorktreeTargetIdentity(worktree.path, gitState.gitDir)
	if err != nil {
		return pinnedWorktreeTarget{}, err
	}
	statusFingerprint, err := fingerprintWorktreeStatusPaths(worktree.path, gitState.status)
	if err != nil {
		return pinnedWorktreeTarget{}, fmt.Errorf("failed to fingerprint target changes: %w", err)
	}

	return pinnedWorktreeTarget{
		identity: identity,
		state: pinnedWorktreeTargetState{
			commonDir:         gitState.commonDir,
			headOID:           gitState.headOID,
			branch:            worktree.branch,
			detached:          worktree.detached,
			bare:              worktree.bare,
			locked:            worktree.locked,
			lockReason:        worktree.lockReason,
			prunable:          worktree.prunable,
			pruneReason:       worktree.pruneReason,
			status:            gitState.status,
			statusFingerprint: statusFingerprint,
			registryID:        worktreeRegistryIdentity(worktree),
		},
	}, nil
}

type inspectedWorktreeGitState struct {
	gitDir    string
	commonDir string
	headOID   string
	status    string
}

func inspectWorktreeGitState(
	ctx context.Context,
	git *worktreeRemovalGit,
	worktree registeredWorktree,
) (inspectedWorktreeGitState, error) {
	gitDirOutput, err := git.output(ctx, worktree.path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return inspectedWorktreeGitState{}, fmt.Errorf("failed to resolve target git directory: %w", err)
	}
	gitDir := filepath.Clean(strings.TrimSpace(string(gitDirOutput)))
	commonDirOutput, err := git.output(
		ctx,
		worktree.path,
		"rev-parse",
		"--path-format=absolute",
		"--git-common-dir",
	)
	if err != nil {
		return inspectedWorktreeGitState{}, fmt.Errorf("failed to resolve target common git directory: %w", err)
	}
	headOutput, err := git.output(
		ctx,
		worktree.path,
		"rev-parse",
		"--verify",
		"--quiet",
		"--end-of-options",
		"HEAD^{commit}",
	)
	if err != nil {
		return inspectedWorktreeGitState{}, fmt.Errorf("target HEAD does not resolve to a commit: %w", err)
	}
	statusOutput, err := git.output(
		ctx,
		worktree.path,
		"status",
		"--porcelain=v1",
		"-z",
		"--untracked-files=all",
		"--ignore-submodules=none",
		"--ignored=matching",
	)
	if err != nil {
		return inspectedWorktreeGitState{}, fmt.Errorf("failed to inspect target cleanliness: %w", err)
	}

	return inspectedWorktreeGitState{
		gitDir:    gitDir,
		commonDir: filepath.Clean(strings.TrimSpace(string(commonDirOutput))),
		headOID:   strings.TrimSpace(string(headOutput)),
		status:    string(statusOutput),
	}, nil
}

func pinWorktreeTargetIdentity(worktreePath, gitDir string) (pinnedWorktreeTargetIdentity, error) {
	pathInfo, err := pinWorktreeDirectory(worktreePath, "target directory identity", "target path is not a real directory")
	if err != nil {
		return pinnedWorktreeTargetIdentity{}, err
	}
	gitDirInfo, err := pinWorktreeDirectory(gitDir, "target git directory identity", "target git directory is not a real directory")
	if err != nil {
		return pinnedWorktreeTargetIdentity{}, err
	}
	gitMarkerPath := filepath.Join(worktreePath, ".git")
	gitMarkerInfo, err := pinWorktreeGitMarker(gitMarkerPath)
	if err != nil {
		return pinnedWorktreeTargetIdentity{}, err
	}
	gitDirFingerprint, err := fingerprintWorktreeFilesystem(gitDir)
	if err != nil {
		return pinnedWorktreeTargetIdentity{}, fmt.Errorf("failed to fingerprint target git directory: %w", err)
	}
	gitMarkerFingerprint, err := fingerprintWorktreeFilesystem(gitMarkerPath)
	if err != nil {
		return pinnedWorktreeTargetIdentity{}, fmt.Errorf("failed to fingerprint target git marker: %w", err)
	}
	return pinnedWorktreeTargetIdentity{
		path:                 filepath.Clean(worktreePath),
		pathInfo:             pathInfo,
		gitDir:               filepath.Clean(gitDir),
		gitDirInfo:           gitDirInfo,
		gitMarkerInfo:        gitMarkerInfo,
		gitDirFingerprint:    gitDirFingerprint,
		gitMarkerFingerprint: gitMarkerFingerprint,
	}, nil
}

func pinWorktreeDirectory(path, identityName, invalidMessage string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to pin %s: %w", identityName, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("%s: %s", invalidMessage, path)
	}
	return info, nil
}

func pinWorktreeGitMarker(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to pin target git marker identity: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("target git marker is not a regular file: %s", path)
	}
	return info, nil
}

func fingerprintWorktreeFilesystem(path string) (string, error) {
	hasher := sha256.New()
	root := filepath.Clean(path)
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		if err := writeWorktreeFilesystemMetadata(hasher, relative, info); err != nil {
			return err
		}
		if err := writeWorktreeFilesystemContents(hasher, current, info); err != nil {
			return err
		}
		return writeWorktreeFilesystemSeparator(hasher)
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hasher.Sum(nil)), nil
}

func writeWorktreeFilesystemMetadata(hasher io.Writer, relative string, info os.FileInfo) error {
	_, err := fmt.Fprintf(
		hasher,
		"%s\x00%s\x00%d\x00%d\x00",
		filepath.ToSlash(relative),
		info.Mode().String(),
		info.Size(),
		info.ModTime().UTC().UnixNano(),
	)
	return err
}

func writeWorktreeFilesystemContents(hasher io.Writer, path string, info os.FileInfo) error {
	switch {
	case info.Mode().IsRegular():
		file, err := os.Open(path) //nolint:gosec // path is rooted in a registered worktree or its gitdir
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(hasher, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		_, err = io.WriteString(hasher, target)
		return err
	default:
		return nil
	}
}

func writeWorktreeFilesystemSeparator(hasher io.Writer) error {
	_, err := hasher.Write([]byte{0})
	return err
}

func fingerprintWorktreeStatusPaths(worktreePath, status string) (string, error) {
	records := strings.Split(status, "\x00")
	pathSet, err := collectWorktreeStatusPaths(records)
	if err != nil {
		return "", err
	}

	paths := make([]string, 0, len(pathSet))
	for path := range pathSet {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	hasher := sha256.New()
	for _, gitPath := range paths {
		cleanPath, err := cleanWorktreeStatusPath(gitPath)
		if err != nil {
			return "", err
		}
		if err := writeWorktreeStatusFingerprint(hasher, worktreePath, cleanPath); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%x", hasher.Sum(nil)), nil
}

func collectWorktreeStatusPaths(records []string) (map[string]struct{}, error) {
	pathSet := make(map[string]struct{})
	recordCount := len(records)
	for index := 0; index < recordCount; index++ {
		record := records[index]
		if record == "" {
			continue
		}
		if len(record) < 4 || record[2] != ' ' {
			return nil, fmt.Errorf("unexpected porcelain status record %q", record)
		}
		code := record[:2]
		pathSet[record[3:]] = struct{}{}
		if strings.ContainsAny(code, "RC") {
			index++
			if index >= recordCount || records[index] == "" {
				return nil, fmt.Errorf("missing source path for porcelain rename/copy record %q", record)
			}
			pathSet[records[index]] = struct{}{}
		}
	}
	return pathSet, nil
}

func cleanWorktreeStatusPath(gitPath string) (string, error) {
	cleanPath := filepath.Clean(filepath.FromSlash(gitPath))
	if cleanPath == "." ||
		filepath.IsAbs(cleanPath) ||
		cleanPath == ".." ||
		strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe path %q in porcelain status", gitPath)
	}
	return cleanPath, nil
}

func writeWorktreeStatusFingerprint(hasher io.Writer, worktreePath, cleanPath string) error {
	if _, err := fmt.Fprintf(hasher, "%s\x00", filepath.ToSlash(cleanPath)); err != nil {
		return err
	}
	fingerprint, err := fingerprintWorktreeFilesystem(filepath.Join(worktreePath, cleanPath))
	if err != nil {
		if os.IsNotExist(err) {
			fingerprint = "<missing>"
		} else {
			return err
		}
	}
	_, err = fmt.Fprintf(hasher, "%s\x00", fingerprint)
	return err
}

func worktreeRegistryIdentity(worktree registeredWorktree) string {
	return strings.Join([]string{
		filepath.Clean(worktree.path),
		worktree.branch,
		strconv.FormatBool(worktree.detached),
		strconv.FormatBool(worktree.bare),
		strconv.FormatBool(worktree.locked),
		worktree.lockReason,
		strconv.FormatBool(worktree.prunable),
		worktree.pruneReason,
	}, "\x00")
}
