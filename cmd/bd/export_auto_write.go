package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jonbaldie/beads/internal/atomicfile"
	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/types"
)

// exportToFile atomically exports issues + memories to the given file path.
// Writes to a temp file first, then renames into place so readers never see
// a partial or truncated export. Used by both `bd export -o` and auto-export.
func exportToFile(ctx context.Context, path string, includeMemories bool) (issueCount, memoryCount int, err error) {
	w, err := atomicfile.Create(path, 0o644)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to create export file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = w.Abort()
		}
	}()

	issues, err := prepareAutoExportFile(ctx, path, includeMemories)
	if err != nil {
		return 0, 0, err
	}

	issueCount, err = writeAutoExportIssues(ctx, w, issues)
	if err != nil {
		return 0, 0, err
	}

	// Write memories
	if includeMemories {
		memoryCount, err = writeAutoExportMemories(ctx, w)
		if err != nil {
			return issueCount, memoryCount, err
		}
	}

	if err := w.Close(); err != nil {
		return issueCount, memoryCount, fmt.Errorf("failed to finalize export: %w", err)
	}

	return issueCount, memoryCount, nil
}

func prepareAutoExportFile(ctx context.Context, path string, includeMemories bool) ([]*types.Issue, error) {
	filter, infraTypeSet := buildAutoExportFilter(ctx)
	issues, err := getStore().SearchIssues(ctx, "", filter)
	if err != nil {
		return nil, fmt.Errorf("failed to search issues: %w", err)
	}

	// Owner-exclusion safety net: auto-export writes the git-committed
	// .beads/issues.jsonl, so the export.exclude_owners config (and legacy
	// export.exclude_owner) must filter here too. Otherwise contributor/personal
	// issues that the manual `bd export` path excludes can still leak into git
	// history and PRs via auto-export (maphew review, be-e2nb). Auto-export has
	// no --exclude-owner flag, so only config-sourced owners apply here.
	if ownerExcludes := buildOwnerExcludeSet(ctx, storeExportSource{}, nil); len(ownerExcludes) > 0 {
		issues = filterOutOwners(issues, ownerExcludes)
	}

	// Store-presence set for the shrink guard (#4069 vs #4988): an
	// out-of-scope row already in the JSONL only blocks the rewrite when its
	// id is STILL in the store. Computed unfiltered here — not from the
	// in-scope `issues` above — because the guard needs to see infra/
	// template/ephemeral ids too, to tell "still in Dolt" apart from
	// "compacted away".
	storeIDs, err := storeKnownIssueIDs(ctx)
	if err != nil {
		return nil, err
	}

	if err := guardAutoExportOverwrite(path, infraTypeSet, includeMemories, storeIDs); err != nil {
		return nil, err
	}
	return issues, nil
}

func writeAutoExportIssues(ctx context.Context, w *atomicfile.Writer, issues []*types.Issue) (int, error) {
	if len(issues) == 0 {
		return 0, nil
	}

	issueIDs := make([]string, len(issues))
	for i, issue := range issues {
		issueIDs[i] = issue.ID
	}
	labelsMap, _ := getStore().GetLabelsForIssues(ctx, issueIDs)
	allDeps, _ := getStore().GetDependencyRecordsForIssues(ctx, issueIDs)
	commentsMap, _ := getStore().GetCommentsForIssues(ctx, issueIDs)
	commentCounts, _ := getStore().GetCommentCounts(ctx, issueIDs)
	depCounts, _ := getStore().GetDependencyCounts(ctx, issueIDs)

	for _, issue := range issues {
		issue.Labels = labelsMap[issue.ID]
		issue.Dependencies = allDeps[issue.ID]
		issue.Comments = commentsMap[issue.ID]
	}

	enc := json.NewEncoder(w)
	issueCount := 0
	for _, issue := range issues {
		counts := depCounts[issue.ID]
		if counts == nil {
			counts = &types.DependencyCounts{}
		}
		sanitizeZeroTime(issue)
		record := &exportIssueRecord{
			RecordType: "issue",
			IssueWithCounts: &types.IssueWithCounts{
				Issue:           issue,
				DependencyCount: counts.DependencyCount,
				DependentCount:  counts.DependentCount,
				CommentCount:    commentCounts[issue.ID],
			},
		}
		if err := enc.Encode(record); err != nil {
			return 0, fmt.Errorf("failed to write issue %s: %w", issue.ID, err)
		}
		issueCount++
	}
	return issueCount, nil
}

func writeAutoExportMemories(ctx context.Context, w *atomicfile.Writer) (int, error) {
	allConfig, err := getStore().GetAllConfig(ctx)
	if err != nil {
		return 0, nil
	}

	fullPrefix := kvPrefix + memoryPrefix
	// Sort keys for deterministic output order (GH#3474).
	var memKeys []string
	for k := range allConfig {
		if strings.HasPrefix(k, fullPrefix) {
			memKeys = append(memKeys, k)
		}
	}
	sort.Strings(memKeys)
	memoryCount := 0
	for _, k := range memKeys {
		v := allConfig[k]
		userKey := strings.TrimPrefix(k, fullPrefix)
		record := map[string]string{
			"_type": "memory",
			"key":   userKey,
			"value": v,
		}
		data, err := json.Marshal(record)
		if err != nil {
			return memoryCount, fmt.Errorf("failed to marshal memory %s: %w", userKey, err)
		}
		if _, err := w.Write(data); err != nil {
			return memoryCount, fmt.Errorf("failed to write memory: %w", err)
		}
		if _, err := w.Write([]byte{'\n'}); err != nil {
			return memoryCount, fmt.Errorf("failed to write newline: %w", err)
		}
		memoryCount++
	}
	return memoryCount, nil
}

// storeKnownIssueIDs returns the set of issue ids currently present in the
// store, ignoring auto-export scope (infra/template/ephemeral rows are
// included). guardAutoExportOverwrite uses this to implement the
// store-presence rule: an out-of-scope row already in the JSONL is only
// safe to drop when its id is no longer in the store (a TTL-compacted wisp,
// GH#4988); if the store still has it, dropping it repeats #4069's data
// loss. Deliberately unfiltered + MaxRows: 0, for the same reason as
// missingJSONLIssueIDsInStore's store-side query: this guard's failure mode
// is a permanent wedge, so the query must stay maximally permissive.
func storeKnownIssueIDs(ctx context.Context) (map[string]struct{}, error) {
	issues, err := getStore().SearchIssues(ctx, "", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Limit: 0}, IssueFilterPage: types.IssueFilterPage{MaxRows: 0}})
	if err != nil {
		return nil, fmt.Errorf("failed to search issues: %w", err)
	}
	ids := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		ids[issue.ID] = struct{}{}
	}
	return ids, nil
}

func guardAutoExportOverwrite(path string, infraTypes map[string]bool, includeMemories bool, storeIDs map[string]struct{}) error {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("auto-export shrink guard: inspect existing JSONL: %w", err)
	}
	defer func() { _ = f.Close() }()

	var stats autoExportOverwriteStats
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if err := classifyExistingAutoExportRecord([]byte(line), infraTypes, includeMemories, storeIDs, &stats); err != nil {
			return fmt.Errorf("auto-export shrink guard: inspect existing JSONL line %d: %w", lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("auto-export shrink guard: inspect existing JSONL: %w", err)
	}

	// Store-presence rule (#4069 vs #4988): block on memories (when
	// excluded), unknown record types, and out-of-scope issue rows whose id
	// is STILL present in the store — those are exactly what #4069 says we
	// must not silently drop. An out-of-scope row absent from the store
	// (e.g. a TTL-compacted wisp) is safe to drop and does not block.
	if stats.FilteredRecords == 0 {
		return nil
	}
	return fmt.Errorf("auto-export shrink guard: refusing to overwrite %s because it contains %d record(s) outside auto-export scope (%d memories, %d infra/template/ephemeral issues, %d unknown); run an explicit export if you want to replace it", path, stats.FilteredRecords, stats.Memories, stats.FilteredIssues, stats.UnknownRecords)
}

type autoExportOverwriteStats struct {
	FilteredRecords int // blocking total: Memories + FilteredIssues + UnknownRecords
	Memories        int
	FilteredIssues  int // infra/template/ephemeral issues still present in the store — blocking (restores GH#4069)
	UnknownRecords  int
}

type autoExportRecord struct {
	Type       string          `json:"_type"`
	IssueType  types.IssueType `json:"issue_type"`
	IsTemplate bool            `json:"is_template"`
	Ephemeral  bool            `json:"ephemeral"`
	ID         string          `json:"id"`
}

func classifyExistingAutoExportRecord(line []byte, infraTypes map[string]bool, includeMemories bool, storeIDs map[string]struct{}, stats *autoExportOverwriteStats) error {
	var record autoExportRecord
	if err := json.Unmarshal(line, &record); err != nil {
		return err
	}
	classifyAutoExportRecord(record, infraTypes, includeMemories, storeIDs, stats)
	return nil
}

func classifyAutoExportRecord(record autoExportRecord, infraTypes map[string]bool, includeMemories bool, storeIDs map[string]struct{}, stats *autoExportOverwriteStats) {
	switch record.Type {
	case "memory":
		if !includeMemories {
			stats.FilteredRecords++
			stats.Memories++
		}
		return
	case "", "issue":
		if record.ID == "" {
			stats.FilteredRecords++
			stats.UnknownRecords++
			return
		}
		if infraTypes[string(record.IssueType)] || record.IsTemplate || record.Ephemeral {
			// Store-presence rule: only block when the row still exists in
			// Dolt (#4069's exact scenario). A row that's gone from the
			// store (TTL-compacted wisp — GH#4988) is safe to drop; the
			// rewrite doesn't lose anything the store didn't already lose.
			if _, present := storeIDs[record.ID]; present {
				stats.FilteredRecords++
				stats.FilteredIssues++
			}
		}
		return
	default:
		stats.FilteredRecords++
		stats.UnknownRecords++
	}
}

func loadExportAutoState(beadsDir string) *exportAutoState {
	path := filepath.Join(beadsDir, exportAutoStateFile)
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return &exportAutoState{}
	}
	var state exportAutoState
	if err := json.Unmarshal(data, &state); err != nil {
		return &exportAutoState{}
	}
	return &state
}

func saveExportAutoState(beadsDir string, state *exportAutoState) {
	path := filepath.Join(beadsDir, exportAutoStateFile)
	data, err := json.Marshal(state)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: auto-export: failed to marshal state: %v\n", err)
		return
	}
	if err := atomicfile.WriteFile(path, data, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: auto-export: failed to save state: %v\n", err)
	}
}

// gitAddFile stages a file in the enclosing git repo. When called from
// inside a git hook, it scrubs inherited GIT_* env vars (so git
// rediscovers the repo from cwd rather than treating cmd.Dir as the
// worktree root) and skips staging when the target is outside the hook's
// worktree (the .beads/redirect case, where staging would pollute the
// main repo's index). See GH#3311, scrubGitHookEnv, hookWorkTreeRoot.
func gitAddFile(path string) error {
	if wt := hookWorkTreeRoot(); wt != "" && !pathInsideDir(path, wt) {
		// Running inside a hook AND target is outside the hook's worktree.
		// Staging here would pollute a different repo's index; skip.
		return nil
	}

	env := scrubGitHookEnv(os.Environ())
	if err := checkGitIndexLock(path, env); err != nil {
		return err
	}
	return runGitAdd(path, env)
}

func checkGitIndexLock(path string, env []string) error {
	lockPath, err := gitIndexLockPath(path, env)
	if err != nil {
		debug.Logf("auto-export: git add lock preflight skipped: %v\n", err)
		return nil
	}
	if lockPath == "" {
		return nil
	}
	if _, statErr := os.Stat(lockPath); statErr == nil {
		return fmt.Errorf("git index is locked at %s; skipping auto-stage", lockPath)
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("failed to check git index lock %s: %w", lockPath, statErr)
	}
	return nil
}

func runGitAdd(path string, env []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), gitAddTimeout)
	defer cancel()
	// Pass the basename only, defensively: cmd.Dir is the parent of path, so
	// a full path argument would double-root (cd .beads && git add
	// .beads/issues.jsonl → pathspec looks under .beads/.beads/) if a caller
	// ever passed a relative path here. Both current callers pass absolute
	// paths, so this guards against a regression rather than fixing a live
	// failure. See GH#4351.
	// Keep cmd.Dir = parent so GH#3311 hook worktree staging still resolves
	// the index path under the repo root (not bare "issues.jsonl" at root).
	cmd := exec.CommandContext(ctx, "git", "add", "--", filepath.Base(path))
	cmd.Dir = filepath.Dir(path)
	cmd.Env = env
	// Capture combined output so the caller's warning surfaces git's stderr
	// (e.g. "paths are ignored", "Unable to create index.lock") instead of
	// just the exit-status text.
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("git add timed out after %s", gitAddTimeout)
	}
	if err != nil {
		if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
			return fmt.Errorf("%w: %s", err, trimmed)
		}
		return err
	}
	return nil
}

func gitIndexLockPath(path string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitAddTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-dir")
	cmd.Dir = filepath.Dir(path)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("git rev-parse timed out after %s", gitAddTimeout)
	}
	if err != nil {
		if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
			return "", fmt.Errorf("%w: %s", err, trimmed)
		}
		return "", err
	}
	gitDir := strings.TrimSpace(string(out))
	if gitDir == "" {
		return "", nil
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(filepath.Dir(path), gitDir)
	}
	return filepath.Join(gitDir, "index.lock"), nil
}

// scrubGitHookEnv returns env with the GIT_* variables that can poison
// git's repo/worktree auto-discovery or object-store resolution removed,
// so git falls back to auto-discovery from cwd. The scrub is
// unconditional: if a user has intentionally exported any of these vars
// for scripting purposes, they will be stripped from the git-add child
// process. That is the correct trade-off here; we never want beads'
// auto-stage to honor a GIT_DIR pointing at an unrelated repo.
//
// Covered vars:
//   - Repo/worktree discovery: GIT_DIR, GIT_WORK_TREE, GIT_COMMON_DIR,
//     GIT_PREFIX, GIT_CEILING_DIRECTORIES, GIT_DISCOVERY_ACROSS_FILESYSTEM
//   - Index routing: GIT_INDEX_FILE
//   - Object routing: GIT_OBJECT_DIRECTORY, GIT_ALTERNATE_OBJECT_DIRECTORIES
//   - Config injection (any GIT_CONFIG* — e.g. GIT_CONFIG_PARAMETERS set
//     when the parent ran `git -c core.worktree=… commit`): the whole
//     GIT_CONFIG namespace, which includes _COUNT, _KEY_n, _VALUE_n,
//     _GLOBAL, _SYSTEM, _NOSYSTEM, and the legacy GIT_CONFIG itself.
func scrubGitHookEnv(env []string) []string {
	// The GIT_CONFIG prefix (no trailing "=") is intentional: it matches
	// GIT_CONFIG=, GIT_CONFIG_COUNT=, GIT_CONFIG_KEY_n=, GIT_CONFIG_VALUE_n=,
	// GIT_CONFIG_PARAMETERS=, GIT_CONFIG_GLOBAL=, GIT_CONFIG_SYSTEM=, and
	// GIT_CONFIG_NOSYSTEM= — the whole family — in one entry. No standard
	// git env var starts with GIT_CONFIG that we want to preserve.
	prefixes := []string{
		"GIT_DIR=",
		"GIT_WORK_TREE=",
		"GIT_INDEX_FILE=",
		"GIT_COMMON_DIR=",
		"GIT_PREFIX=",
		"GIT_OBJECT_DIRECTORY=",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=",
		"GIT_CEILING_DIRECTORIES=",
		"GIT_DISCOVERY_ACROSS_FILESYSTEM=",
		"GIT_CONFIG",
	}
	out := make([]string, 0, len(env))
	for _, e := range env {
		skip := false
		for _, p := range prefixes {
			if strings.HasPrefix(e, p) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, e)
		}
	}
	return out
}

// hookWorkTreeRoot returns the root of the worktree whose git hook we
// are running inside, based on the inherited GIT_DIR env var. Returns ""
// when GIT_DIR is not set (the normal non-hook case) or cannot be
// resolved to a work-tree.
//
// Resolution rules:
//   - In a linked worktree, GIT_DIR points at main/.git/worktrees/<name>
//     and that directory contains a "gitdir" file whose contents are the
//     absolute path to the worktree's .git FILE. The worktree root is
//     the parent of that .git file.
//   - In a non-worktree, GIT_DIR is typically ".git" or "<repo>/.git";
//     the worktree root is its parent.
func hookWorkTreeRoot() string {
	gitDir := os.Getenv("GIT_DIR")
	if gitDir == "" {
		return ""
	}
	var root string
	//nolint:gosec // G304: path is GIT_DIR/gitdir, a well-known git internal file.
	if data, err := os.ReadFile(filepath.Join(gitDir, "gitdir")); err == nil {
		if dotGit := strings.TrimSpace(string(data)); dotGit != "" {
			root = filepath.Dir(dotGit)
		}
	}
	if root == "" && filepath.Base(gitDir) == ".git" {
		root = filepath.Dir(gitDir)
	}
	if root == "" {
		return ""
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	return abs
}

// pathInsideDir reports whether path is the same as dir or a descendant
// of dir, after resolving symlinks on both sides. Returns false on any
// resolution error (conservative: when in doubt, treat as outside).
//
// Resolves the PARENT of path rather than path itself, which handles the
// common "target file does not yet exist" case: on macOS /tmp is a
// symlink to /private/tmp, so asymmetric EvalSymlinks on a nonexistent
// file vs its existing parent would otherwise produce a spurious false.
// Callers (gitAddFile) always pass a path whose parent exists (either
// beadsDir, which FindBeadsDir verified, or a directory just created by
// the export write), so this single-level resolution is sufficient.
func pathInsideDir(path, dir string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	if r, err := filepath.EvalSymlinks(filepath.Dir(absPath)); err == nil {
		absPath = filepath.Join(r, filepath.Base(absPath))
	}
	if r, err := filepath.EvalSymlinks(absDir); err == nil {
		absDir = r
	}
	sep := string(filepath.Separator)
	return absPath == absDir || strings.HasPrefix(absPath, absDir+sep)
}
