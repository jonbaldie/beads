package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func addToGitignore(ctx context.Context, repoRoot, entry string) error {
	normalizedEntry, err := normalizeGitignoreEntry(entry)
	if err != nil {
		return err
	}
	entry = normalizedEntry
	gitignorePath := filepath.Join(repoRoot, ".gitignore")

	// If git already ignores this path (e.g., via a parent pattern like
	// ".worktrees/"), avoid appending one line per worktree.
	ignored, err := isIgnoredByGit(ctx, repoRoot, entry)
	if err == nil && ignored {
		return nil
	}

	content, err := readGitignoreContent(gitignorePath)
	if err != nil {
		return err
	}

	if gitignoreContentCoversEntry(content, entry) {
		return nil
	}
	return appendGitignoreEntry(gitignorePath, content, entry)
}

func normalizeGitignoreEntry(entry string) (string, error) {
	entry = strings.TrimSuffix(filepath.ToSlash(entry), "/")
	if entry == "" {
		return "", fmt.Errorf("gitignore entry must not be empty")
	}
	return entry, nil
}

func readGitignoreContent(path string) ([]byte, error) {
	content, err := os.ReadFile(path) //nolint:gosec // G304: path is the known repo .gitignore
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return content, nil
}

func gitignoreContentCoversEntry(content []byte, entry string) bool {
	// Check if already present or covered by a parent-directory pattern.
	// e.g. if ".worktrees" is in .gitignore, ".worktrees/my-branch" is already covered.
	for _, line := range strings.Split(string(content), "\n") {
		if gitignoreLineCoversEntry(line, entry) {
			return true
		}
	}
	return false
}

func gitignoreLineCoversEntry(line, entry string) bool {
	line = strings.TrimSuffix(line, "\r")
	trimmed := strings.TrimSuffix(filepath.ToSlash(line), "/")
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return false
	}
	return trimmed == entry || strings.HasPrefix(entry+"/", trimmed+"/")
}

func appendGitignoreEntry(path string, content []byte, entry string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644) //nolint:gosec // G302: .gitignore should be world-readable
	if err != nil {
		return err
	}
	defer f.Close()

	// Add newline if file doesn't end with one
	if len(content) > 0 && content[len(content)-1] != '\n' {
		if _, err := f.WriteString("\n"); err != nil {
			return err
		}
	}

	// Add comment and entry.
	if _, err := f.WriteString(fmt.Sprintf("# bd worktree\n%s/\n", entry)); err != nil {
		return err
	}

	return nil
}

func isIgnoredByGit(ctx context.Context, repoRoot, entry string) (bool, error) {
	normalized := strings.TrimSuffix(filepath.ToSlash(entry), "/")
	if normalized == "" {
		return false, nil
	}

	gitCmd := gitCmdInDir(ctx, repoRoot, "check-ignore", "-q", "--no-index", "--", normalized)
	err := gitCmd.Run()
	if err == nil {
		return true, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}

	return false, err
}

type gitignoreCleanupPlan struct {
	repoRoot string
	path     string
	entry    string
	info     os.FileInfo
	original []byte
	updated  []byte
}

type gitignoreLine struct {
	raw  []byte
	body string
}

func prepareGitignoreCleanup(repoRoot, entry string) (*gitignoreCleanupPlan, error) {
	entry = strings.TrimSuffix(filepath.ToSlash(entry), "/")
	if entry == "" {
		return nil, fmt.Errorf("gitignore entry must not be empty")
	}
	gitignorePath := filepath.Join(repoRoot, ".gitignore")
	if _, err := os.Lstat(gitignorePath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	info, content, err := readStableRegularFile(gitignorePath)
	if err != nil {
		return nil, err
	}
	updated, changed := removeManagedGitignoreEntry(content, entry)
	if !changed {
		return nil, nil
	}
	return &gitignoreCleanupPlan{
		repoRoot: repoRoot,
		path:     gitignorePath,
		entry:    entry,
		info:     info,
		original: content,
		updated:  updated,
	}, nil
}

func readStableRegularFile(path string) (os.FileInfo, []byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !isRegularWorktreeFile(before) {
		return nil, nil, fmt.Errorf("%s is not a regular file", path)
	}

	file, openedBefore, err := openStableRegularFile(path, before)
	if err != nil {
		return nil, nil, err
	}
	content, openedAfter, err := readOpenedRegularFile(file)
	if err != nil {
		return nil, nil, err
	}

	after, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !sameStableRegularFile(after, openedBefore, openedAfter) {
		return nil, nil, fmt.Errorf("%s changed while it was being read", path)
	}
	return after, content, nil
}

func isRegularWorktreeFile(info os.FileInfo) bool {
	return info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular()
}

func openStableRegularFile(path string, before os.FileInfo) (*os.File, os.FileInfo, error) {
	file, err := os.Open(path) //nolint:gosec // path is the pinned primary worktree .gitignore
	if err != nil {
		return nil, nil, err
	}
	openedBefore, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !isRegularWorktreeFile(openedBefore) || !samePinnedFileMetadata(openedBefore, before) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%s changed while it was being opened", path)
	}
	return file, openedBefore, nil
}

func readOpenedRegularFile(file *os.File) ([]byte, os.FileInfo, error) {
	content, readErr := io.ReadAll(file)
	openedAfter, statErr := file.Stat()
	closeErr := file.Close()
	if readErr != nil {
		return nil, nil, readErr
	}
	if statErr != nil {
		return nil, nil, statErr
	}
	if closeErr != nil {
		return nil, nil, closeErr
	}
	return content, openedAfter, nil
}

func sameStableRegularFile(after, openedBefore, openedAfter os.FileInfo) bool {
	return isRegularWorktreeFile(after) &&
		samePinnedFileMetadata(openedAfter, openedBefore) &&
		samePinnedFileMetadata(after, openedAfter)
}

func removeManagedGitignoreEntry(content []byte, entry string) ([]byte, bool) {
	lines := splitGitignoreLines(content)
	updated := make([]byte, 0, len(content))
	changed := false
	lineCount := len(lines)
	for index := 0; index < lineCount; {
		if lines[index].body == "# bd worktree" &&
			index+1 < len(lines) &&
			gitignoreLineMatchesEntry(lines[index+1].body, entry) {
			changed = true
			index += 2
			continue
		}
		updated = append(updated, lines[index].raw...)
		index++
	}
	return updated, changed
}

func splitGitignoreLines(content []byte) []gitignoreLine {
	lines := make([]gitignoreLine, 0, bytes.Count(content, []byte{'\n'})+1)
	contentLength := len(content)
	for start := 0; start < contentLength; {
		end := contentLength
		if newline := bytes.IndexByte(content[start:], '\n'); newline >= 0 {
			end = start + newline + 1
		}
		bodyEnd := end
		if bodyEnd > start && content[bodyEnd-1] == '\n' {
			bodyEnd--
		}
		if bodyEnd > start && content[bodyEnd-1] == '\r' {
			bodyEnd--
		}
		lines = append(lines, gitignoreLine{
			raw:  content[start:end],
			body: string(content[start:bodyEnd]),
		})
		start = end
	}
	return lines
}

func gitignoreLineMatchesEntry(line, entry string) bool {
	normalized := strings.TrimSuffix(
		filepath.ToSlash(line),
		"/",
	)
	return normalized == entry
}

func truncate(s string, maxLen int) string {
	// Byte budget (len counts bytes), but never split a UTF-8 code point.
	// Compact prime memories and other display paths call this; a mid-rune
	// cut emits invalid UTF-8 and breaks SessionStart hosts that decode strictly.
	if maxLen <= 0 {
		return ""
	}
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		cut := maxLen
		for cut > 0 && !utf8.ValidString(s[:cut]) {
			cut--
		}
		return s[:cut]
	}
	cut := maxLen - 3
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	return s[:cut] + "..."
}
