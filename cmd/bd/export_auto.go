package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/types"
)

// exportAutoState tracks auto-export state to avoid redundant work.
type exportAutoState struct {
	LastDoltCommit string    `json:"last_dolt_commit"`
	Timestamp      time.Time `json:"timestamp"`
	Issues         int       `json:"issues"`
	Memories       int       `json:"memories"`
}

const exportAutoStateFile = "export-state.json"
const gitAddTimeout = 5 * time.Second

// maybeAutoExport writes a git-tracked JSONL file if enabled and due.
// Called from PersistentPostRun after auto-backup.
//
// This runs in server mode too: clients of a shared dolt sql-server rely on
// the JSONL export for git-durable state exactly like embedded users do — in
// topologies without a Dolt remote it is the only durability. Skipping here
// made `git push` silently publish stale issue state (wy-4ope).
func maybeAutoExport(ctx context.Context, allowEmptyOverwrite bool) error {
	if shouldSkipAutoExport() {
		return nil
	}

	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return nil
	}

	run, ok := prepareAutoExport(ctx, beadsDir)
	if !ok {
		return nil
	}
	if !allowEmptyOverwrite {
		if warning, refused := autoExportSafetyWarning(ctx, run.fullPath); refused {
			fmt.Fprintf(os.Stderr, "Warning: auto-export skipped: %s\n", warning)
			return nil
		}
	}
	return executeAutoExport(ctx, run)
}

func shouldSkipAutoExport() bool {
	// Skip when running as a git hook to avoid re-export during pre-commit.
	if os.Getenv("BD_GIT_HOOK") == "1" {
		debug.Logf("auto-export: skipping — running as git hook\n")
		return true
	}
	if !config.GetBool("export.auto") || getStore() == nil {
		return true
	}
	if lm, ok := storage.UnwrapStore(getStore()).(storage.LifecycleManager); ok && lm.IsClosed() {
		return true
	}
	return false
}

type autoExportRun struct {
	beadsDir      string
	fullPath      string
	currentCommit string
}

func prepareAutoExport(ctx context.Context, beadsDir string) (*autoExportRun, bool) {
	// Resolve the export path before throttle/check detection so all decisions
	// refer to the path that would actually be written.
	fullPath := autoExportPath(beadsDir)

	// Load state + interval.
	state := loadExportAutoState(beadsDir)
	interval := config.GetDuration("export.interval")
	if interval == 0 {
		interval = 60 * time.Second
	}

	// Change detection via Dolt state hash. This is cheap, so do it before
	// throttle: when there are no changes, there is nothing to throttle.
	currentCommit, err := storeStateHash(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: auto-export skipped: failed to get current commit: %v\n", err)
		return nil, false
	}
	if currentCommit == state.LastDoltCommit && state.LastDoltCommit != "" {
		debug.Logf("auto-export: no changes since last export\n")
		return nil, false
	}

	if !shouldExport(state, interval) {
		debug.Logf("auto-export: throttled (last export %s ago, interval %s)\n",
			time.Since(state.Timestamp).Round(time.Second), interval)
		return nil, false
	}
	return &autoExportRun{beadsDir: beadsDir, fullPath: fullPath, currentCommit: currentCommit}, true
}

func autoExportPath(beadsDir string) string {
	exportPath := config.GetString("export.path")
	if exportPath == "" {
		if isGlobalFlag() {
			exportPath = "global-issues.jsonl"
		} else {
			exportPath = "issues.jsonl"
		}
	}
	return filepath.Join(beadsDir, exportPath)
}

func autoExportSafetyWarning(ctx context.Context, fullPath string) (string, bool) {
	if skip, existingCount, err := shouldSkipEmptyAutoExport(ctx, fullPath); err != nil {
		return fmt.Sprintf("failed to check existing JSONL: %v", err), true
	} else if skip {
		return fmt.Sprintf("current database would export 0 issues, but %s already contains %d issue(s); refusing to overwrite. Run `bd init --from-jsonl` to import the JSONL file, or move it aside and retry.", fullPath, existingCount), true
	}
	missingIDs, err := missingJSONLIssueIDsInStore(ctx, fullPath)
	if err != nil {
		return fmt.Sprintf("failed to compare existing JSONL against local store: %v", err), true
	}
	if len(missingIDs) > 0 {
		return fmt.Sprintf("%s contains %d JSONL-only issue record(s) absent from the local Dolt store (%s); refusing to overwrite. Run `bd init --from-jsonl` to import the JSONL file, or move it aside and retry.", fullPath, len(missingIDs), strings.Join(sampleStrings(missingIDs, 5), ", ")), true
	}
	return "", false
}

func executeAutoExport(ctx context.Context, run *autoExportRun) error {
	// Run the export — memories are excluded from auto-export because they
	// contain private agent context that must not reach git history (GH#3650).
	issueCount, memoryCount, err := exportToFile(ctx, run.fullPath, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: auto-export failed: %v\n", err)
		return nil
	}

	debug.Logf("auto-export: wrote %d issues and %d memories to %s\n",
		issueCount, memoryCount, run.fullPath)

	// Don't prime the throttle on an empty export (e.g. immediately after
	// `bd init`). Saving state here would block the first real `bd create`
	// from exporting for up to export.interval seconds even though the data
	// has changed. Remove the empty file too so users don't see a stale 0-byte
	// issues.jsonl before any issues exist.
	if issueCount == 0 && memoryCount == 0 {
		_ = os.Remove(run.fullPath)
		saveExportAutoState(run.beadsDir, &exportAutoState{
			LastDoltCommit: run.currentCommit,
			Timestamp:      time.Now(),
			Issues:         0,
			Memories:       0,
		})
		return nil
	}
	warnJSONLWithoutDoltRemote("auto-export")

	// Optional git add — skip when no-git-ops is set (GH#3314), when not in a
	// git repo (standalone BEADS_DIR flow), or when export.git-add is false.
	if config.GetBool("export.git-add") && !config.GetBool("no-git-ops") && isGitRepo() {
		if err := gitAddFile(run.fullPath); err != nil {
			return fmt.Errorf("auto-export: git add failed: %w", err)
		}
	}

	// Save state
	newState := exportAutoState{
		LastDoltCommit: run.currentCommit,
		Timestamp:      time.Now(),
		Issues:         issueCount,
		Memories:       memoryCount,
	}
	saveExportAutoState(run.beadsDir, &newState)
	return nil
}

// storeStateHash returns the hash used for auto-export change detection.
// It prefers a working-set-aware hash (storage.StateHasher) over the HEAD
// commit: in server mode dolt auto-commit is off, so writes stay in the
// working set and HEAD does not advance — HEAD-based detection would go
// permanently quiet after the first export.
func storeStateHash(ctx context.Context) (string, error) {
	if sh, ok := storage.UnwrapStore(getStore()).(storage.StateHasher); ok {
		return sh.GetStateHash(ctx)
	}
	return getStore().GetCurrentCommit(ctx)
}

// shouldExport reports whether the throttle window has elapsed, or whether
// this is the first auto-export attempt. It returns false only when a recent
// export exists and the configured interval has not elapsed.
//
// Extracted from Jeremy Longshore's GH#4061 throttle-decision refactor.
func shouldExport(state *exportAutoState, interval time.Duration) bool {
	if state.Timestamp.IsZero() {
		return true
	}
	return time.Since(state.Timestamp) >= interval
}

func shouldSkipEmptyAutoExport(ctx context.Context, path string) (bool, int, error) {
	existingCount, err := countIssueRecordsInJSONL(path)
	if err != nil {
		return false, 0, err
	}
	if existingCount == 0 {
		return false, 0, nil
	}

	issues, err := getStore().SearchIssues(ctx, "", autoExportFilter(ctx))
	if err != nil {
		return false, 0, fmt.Errorf("failed to search issues: %w", err)
	}

	return len(issues) == 0, existingCount, nil
}

func countIssueRecordsInJSONL(path string) (int, error) {
	ids, err := issueIDsInJSONL(path)
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}

func missingJSONLIssueIDsInStore(ctx context.Context, path string) ([]string, error) {
	// GH#4988: only refuse when *in-scope* JSONL issue records are absent from
	// the store. Ephemeral wisps, templates, and infra types are outside
	// auto-export scope (buildAutoExportFilter / GH#3649). Compaction can
	// delete wisps from Dolt while an older JSONL still lists them; treating
	// those as hard orphans wedged auto-export permanently.
	existing, err := issueRecordsInJSONL(path)
	if err != nil {
		return nil, err
	}
	if len(existing) == 0 {
		return nil, nil
	}

	localIDs, infraTypeSet, err := autoExportStoreIDsAndInfraTypes(ctx)
	if err != nil {
		return nil, err
	}
	return missingAutoExportRecords(existing, localIDs, infraTypeSet), nil
}

func autoExportStoreIDsAndInfraTypes(ctx context.Context) (map[string]struct{}, map[string]bool, error) {
	// Store-side query stays unfiltered and MaxRows: 0 (opts out of
	// BEADS_MAX_ROWS). This guard's failure mode is a permanent wedge, so a
	// narrower filter here — or a row cap — can only ever manufacture
	// phantom "missing" ids, never fewer (maphew review, GH#4988 follow-up).
	// buildAutoExportFilter is still consulted for infraTypeSet, which
	// classifies the JSONL-side records below.
	_, infraTypeSet := buildAutoExportFilter(ctx)
	issues, err := getStore().SearchIssues(ctx, "", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Limit: 0}, IssueFilterPage: types.IssueFilterPage{MaxRows: 0}})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to search issues: %w", err)
	}
	localIDs := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		localIDs[issue.ID] = struct{}{}
	}
	return localIDs, infraTypeSet, nil
}

func missingAutoExportRecords(existing []jsonlIssueRecord, localIDs map[string]struct{}, infraTypeSet map[string]bool) []string {
	missing := make([]string, 0)
	for _, rec := range existing {
		if rec.Ephemeral || rec.IsTemplate || infraTypeSet[string(rec.IssueType)] {
			continue
		}
		if _, ok := localIDs[rec.ID]; !ok {
			missing = append(missing, rec.ID)
		}
	}
	return missing
}

// jsonlIssueRecord is a lightweight issue line from issues.jsonl used by
// auto-export safety guards.
type jsonlIssueRecord struct {
	ID         string
	IssueType  types.IssueType
	IsTemplate bool
	Ephemeral  bool
}

// issueRecordsInJSONL returns issue records (id + scope fields) from a JSONL
// export file. Tombstones and non-issue record types are skipped.
func issueRecordsInJSONL(path string) ([]jsonlIssueRecord, error) {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)

	seen := make(map[string]struct{})
	var records []jsonlIssueRecord
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		rec, include, err := parseAutoExportJSONLIssueRecord([]byte(line))
		if err != nil {
			return nil, err
		}
		if !include {
			continue
		}

		if _, ok := seen[rec.ID]; ok {
			continue
		}
		seen[rec.ID] = struct{}{}
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return records, nil
}

func parseAutoExportJSONLIssueRecord(line []byte) (jsonlIssueRecord, bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return jsonlIssueRecord{}, false, err
	}
	if !isJSONLIssueRecord(raw) {
		return jsonlIssueRecord{}, false, nil
	}

	rec, status := decodeJSONLIssueRecordFields(raw)
	if rec.ID == "" || status == "tombstone" {
		return jsonlIssueRecord{}, false, nil
	}
	return rec, true, nil
}

func decodeJSONLIssueRecordFields(raw map[string]json.RawMessage) (jsonlIssueRecord, string) {
	var rec jsonlIssueRecord
	if rawID, ok := raw["id"]; ok {
		_ = json.Unmarshal(rawID, &rec.ID)
	}

	var status string
	if rawStatus, ok := raw["status"]; ok {
		_ = json.Unmarshal(rawStatus, &status)
	}
	if rawIT, ok := raw["issue_type"]; ok {
		_ = json.Unmarshal(rawIT, &rec.IssueType)
	}
	if rawTpl, ok := raw["is_template"]; ok {
		_ = json.Unmarshal(rawTpl, &rec.IsTemplate)
	}
	if rawEph, ok := raw["ephemeral"]; ok {
		_ = json.Unmarshal(rawEph, &rec.Ephemeral)
	}
	return rec, status
}

func isJSONLIssueRecord(raw map[string]json.RawMessage) bool {
	rawType, ok := raw["_type"]
	if !ok {
		return true
	}
	var recordType string
	if err := json.Unmarshal(rawType, &recordType); err != nil {
		return true
	}
	return recordType == "" || recordType == "issue"
}

func issueIDsInJSONL(path string) ([]string, error) {
	records, err := issueRecordsInJSONL(path)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(records))
	for i, rec := range records {
		ids[i] = rec.ID
	}
	return ids, nil
}

func sampleStrings(values []string, limit int) []string {
	if len(values) <= limit {
		return values
	}
	out := append([]string{}, values[:limit]...)
	out = append(out, "...")
	return out
}

func autoExportFilter(ctx context.Context) types.IssueFilter {
	filter, _ := buildAutoExportFilter(ctx)
	return filter
}

func buildAutoExportFilter(ctx context.Context) (types.IssueFilter, map[string]bool) {
	// MaxRows: 0 opts out of BEADS_MAX_ROWS — auto-export is a data-integrity
	// sweep and must not be capped (designer §4.1).
	filter := types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Limit: 0}, IssueFilterPage: types.IssueFilterPage{MaxRows: 0}}
	var infraTypes []string
	if getStore() != nil {
		infraSet := getStore().GetInfraTypes(ctx)
		if len(infraSet) > 0 {
			for t := range infraSet {
				infraTypes = append(infraTypes, t)
			}
		}
	}
	if len(infraTypes) == 0 {
		infraTypes = dolt.DefaultInfraTypes()
	}
	infraTypeSet := make(map[string]bool, len(infraTypes))
	for _, t := range infraTypes {
		infraTypeSet[t] = true
	}
	sort.Strings(infraTypes)
	for _, t := range infraTypes {
		filter.ExcludeTypes = append(filter.ExcludeTypes, types.IssueType(t))
	}
	isTemplate := false
	filter.IsTemplate = &isTemplate

	// Exclude ephemeral wisps — they are private/transient and must not
	// reach git history or external integrations (GH#3649).
	persistentOnly := false
	filter.Ephemeral = &persistentOnly

	return filter, infraTypeSet
}
