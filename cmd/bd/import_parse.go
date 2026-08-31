package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
)

// parseImportRecords scans one JSONL stream into issue rows and memory
// records — the `bd import` / `bd import -` parse loop, shared by the classic
// and proxied modes. Same record vocabulary as parseJSONLFile (the bootstrap
// reader): the optional _schema header and tombstones are skipped, and the
// "wisp_plane" boolean is honored as the explicit wisps-plane marker (and
// the legacy "wisp" alias for "ephemeral") via applyImportWispPlane.
func parseImportRecords(r io.Reader) ([]*types.Issue, []memoryRecord, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)

	var issues []*types.Issue
	var memories []memoryRecord

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		record, err := parseImportRecord(line)
		if err != nil {
			return nil, nil, err
		}
		if record.memory != nil {
			memories = append(memories, *record.memory)
		}
		if record.issue != nil {
			issues = append(issues, record.issue)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("failed to scan JSONL: %w", err)
	}
	return issues, memories, nil
}

type parsedImportRecord struct {
	issue  *types.Issue
	memory *memoryRecord
}

func parseImportRecord(line string) (parsedImportRecord, error) {
	var peek map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &peek); err != nil {
		return parsedImportRecord{}, fmt.Errorf("failed to parse JSONL line: %w", err)
	}

	// Skip the optional beads-jsonl header record (§J1.3). A canonical
	// export may prepend a provenance line, e.g.
	// {"_schema":"beads-jsonl/1","_dolt_branch":"main","_sort":"stable-v1"}.
	// It carries no _type and no issue fields; without this guard it falls
	// through to the issue path, unmarshals into an empty Issue, and aborts
	// the whole import with "title is required". parseJSONLFile (the
	// bootstrap reader) has always skipped it; this loop — the one `bd
	// import` and `bd import -` run through — did not.
	if _, isHeader := peek["_schema"]; isHeader {
		return parsedImportRecord{}, nil
	}

	if record, handled, err := parseImportMemory(line, peek); handled || err != nil {
		return record, err
	}

	return parseImportIssue(line, peek)
}

func parseImportMemory(line string, peek map[string]json.RawMessage) (parsedImportRecord, bool, error) {
	rawType, ok := peek["_type"]
	if !ok {
		return parsedImportRecord{}, false, nil
	}
	var typeStr string
	if err := json.Unmarshal(rawType, &typeStr); err != nil || typeStr != "memory" {
		return parsedImportRecord{}, false, nil
	}
	var mem memoryRecord
	if err := json.Unmarshal([]byte(line), &mem); err != nil {
		return parsedImportRecord{}, true, fmt.Errorf("failed to parse memory record: %w", err)
	}
	if mem.Key == "" || mem.Value == "" {
		return parsedImportRecord{}, true, nil
	}
	return parsedImportRecord{memory: &mem}, true, nil
}

func parseImportIssue(line string, peek map[string]json.RawMessage) (parsedImportRecord, error) {
	var issue types.Issue
	if err := json.Unmarshal([]byte(line), &issue); err != nil {
		return parsedImportRecord{}, fmt.Errorf("failed to parse issue from JSONL: %w", err)
	}
	if issue.Status == "tombstone" {
		return parsedImportRecord{}, nil
	}
	applyImportWispPlane(peek, &issue)
	issue.SetDefaults()
	return parsedImportRecord{issue: &issue}, nil
}

// runImportRecordsClassic is the classic (embedded/direct store) import
// pipeline over the parsed records: dedup, dry-run classification, memory
// writes, the batch issue import, the final commit and the issue_prefix
// reconciliation.
func runImportRecordsClassic(ctx context.Context, issues []*types.Issue, memories []memoryRecord, source string, opts importOptions) error {
	issues, dedupHits := deduplicateImportIssues(ctx, getStore(), issues, opts.dedup)
	result := importResultJSON{Source: source, DedupHits: dedupHits, DryRun: opts.dryRun}

	if opts.dryRun {
		return runClassicImportDryRun(ctx, issues, memories, source, dedupHits, opts.allowStale)
	}
	if err := importClassicMemories(ctx, memories, &result); err != nil {
		return err
	}
	if err := importClassicIssues(ctx, issues, opts.allowStale, &result); err != nil {
		return err
	}
	if err := commitClassicImport(ctx, source, &result); err != nil {
		return err
	}
	syncClassicImportIssuePrefix(ctx)
	return renderImportOutcome(result, source, dedupHits)
}

func runClassicImportDryRun(ctx context.Context, issues []*types.Issue, memories []memoryRecord, source string, dedupHits int, allowStale bool) error {
	result := importResultJSON{
		Source:    source,
		DedupHits: dedupHits,
		DryRun:    true,
		Memories:  len(memories),
		Skipped:   dedupHits,
	}
	classification, err := classifyDryRunImport(ctx, getStore(), issues, allowStale)
	if err != nil {
		return fmt.Errorf("dry-run: %w", err)
	}
	applyImportDryRunClassification(&result, classification)
	return renderImportDryRun(result, len(memories), source, dedupHits)
}

func importClassicMemories(ctx context.Context, memories []memoryRecord, result *importResultJSON) error {
	for _, mem := range memories {
		storageKey := kvPrefix + memoryPrefix + mem.Key
		if err := getStore().SetConfig(ctx, storageKey, mem.Value); err != nil {
			return fmt.Errorf("failed to import memory %q: %w", mem.Key, err)
		}
		result.Memories++
	}
	return nil
}

func importClassicIssues(ctx context.Context, issues []*types.Issue, allowStale bool, result *importResultJSON) error {
	if len(issues) == 0 {
		return nil
	}
	importResult, err := importIssuesCore(ctx, "", getStore(), issues, ImportOptions{SkipPrefixValidation: true, AllowStale: allowStale})
	if err != nil {
		return fmt.Errorf("import failed: %w", err)
	}
	applyImportOutcome(result, importResult)
	return nil
}

func commitClassicImport(ctx context.Context, source string, result *importResultJSON) error {
	if result.Created == 0 && result.Memories == 0 {
		return nil
	}
	commitMsg := fmt.Sprintf("bd import: %d issues", result.Created)
	if result.Memories > 0 {
		commitMsg += fmt.Sprintf(", %d memories", result.Memories)
	}
	commitMsg += fmt.Sprintf(" from %s", filepath.Base(source))
	if err := getStore().Commit(ctx, commitMsg); err != nil && !strings.Contains(err.Error(), "nothing to commit") {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func syncClassicImportIssuePrefix(ctx context.Context) {
	// store.Commit skips the config table (GH#2455), so we use CommitWithConfig
	// for this intentional config update after the issues commit completes.
	// config.yaml is authoritative here and existing issue IDs are intentionally
	// left unchanged: this deliberately bypasses the `bd config set issue_prefix`
	// guard for the import/migration flow and is not a rename.
	yamlPrefix := config.GetString("issue-prefix")
	if yamlPrefix == "" {
		return
	}
	dbPrefix, _ := getStore().GetConfig(ctx, "issue_prefix")
	if dbPrefix == yamlPrefix {
		return
	}
	if err := getStore().SetConfig(ctx, "issue_prefix", yamlPrefix); err != nil {
		return
	}
	_ = getStore().CommitWithConfig(ctx, "bd import: sync issue_prefix from config.yaml")
}

// applyImportDryRunClassification folds a dry-run classification into the
// command's JSON result, identically in both modes.
func applyImportDryRunClassification(result *importResultJSON, classification *ImportResult) {
	result.Created = classification.Created
	result.Updated = classification.Updated
	result.Unchanged = classification.Unchanged
	result.Skipped += classification.Skipped
	result.IDs = append(result.IDs, classification.ImportedIDs...)
	result.StaleSkippedIDs = classification.StaleSkippedIDs
	result.UpdatedIssues = classification.UpdatedIssues
	result.TieKeptLocalIDs = classification.TieKeptLocalIDs
}

// applyImportOutcome folds a real import's outcome into the command's JSON
// result, identically in both modes.
func applyImportOutcome(result *importResultJSON, importResult *ImportResult) {
	result.Created = importResult.Created
	result.Updated = importResult.Updated
	result.Skipped += importResult.Skipped
	result.SkippedDependencies = append(result.SkippedDependencies, importResult.SkippedDependencies...)
	result.IDs = append(result.IDs, importResult.ImportedIDs...)
	result.UpdatedIssues = append(result.UpdatedIssues, importResult.UpdatedIssues...)
	result.TieKeptLocalIDs = append(result.TieKeptLocalIDs, importResult.TieKeptLocalIDs...)
	result.StaleSkippedIDs = append(result.StaleSkippedIDs, importResult.StaleSkippedIDs...)
}

// renderImportDryRun reports a dry run (JSON or stderr), identically in both
// modes.
func renderImportDryRun(result importResultJSON, memoriesCount int, source string, dedupHits int) error {
	if isJSONOutput() {
		return outputJSON(result)
	}
	// The leading count is the sum of the breakdown that follows it
	// (not len(issues)), which can be larger when rows were stale
	// skipped — those are reported separately below instead of being
	// folded into a total the breakdown then wouldn't add up to.
	considered := result.Created + result.Updated + result.Unchanged
	//nolint:gosec // G705: stderr, not a browser context
	fmt.Fprintf(os.Stderr, "Would import %d issues (%d new, %d updated, %d unchanged) and %d memories from %s",
		considered, result.Created, result.Updated, result.Unchanged, memoriesCount, source)
	if dedupHits > 0 {
		fmt.Fprintf(os.Stderr, " (%d duplicates skipped)", dedupHits) //nolint:gosec // G705: stderr, not a browser context
	}
	if len(result.StaleSkippedIDs) > 0 {
		fmt.Fprintf(os.Stderr, " (%d stale skipped)", len(result.StaleSkippedIDs))
	}
	fmt.Fprintln(os.Stderr)
	return nil
}

// renderImportOutcome reports a completed import (JSON or stderr),
// identically in both modes.
func renderImportOutcome(result importResultJSON, source string, dedupHits int) error {
	if isJSONOutput() {
		return outputJSON(result)
	}

	fmt.Fprintf(os.Stderr, "Imported %d issues", result.Created)
	if result.Memories > 0 {
		fmt.Fprintf(os.Stderr, " and %d memories", result.Memories)
	}
	fmt.Fprintf(os.Stderr, " from %s", source)
	if dedupHits > 0 {
		fmt.Fprintf(os.Stderr, " (%d duplicates skipped)", dedupHits) //nolint:gosec // G705: stderr, not a browser context
	}
	if staleSkipped := result.Skipped - dedupHits; staleSkipped > 0 {
		fmt.Fprintf(os.Stderr, " (%d stale skipped; use --allow-stale to restore older rows)", staleSkipped) //nolint:gosec // G705: stderr, not a browser context
	}
	fmt.Fprintln(os.Stderr)
	if len(result.UpdatedIssues) > 0 {
		fmt.Fprintf(os.Stderr, "Updated %d existing issue(s):\n", len(result.UpdatedIssues))
		for _, change := range result.UpdatedIssues {
			fmt.Fprintf(os.Stderr, "  %s: %s\n", change.ID, change.Changes)
		}
	}
	if len(result.TieKeptLocalIDs) > 0 {
		fmt.Fprintf(os.Stderr, "Kept local state for %d issue(s) with the same updated_at but different content (use --allow-stale to overwrite): %s\n",
			len(result.TieKeptLocalIDs), strings.Join(result.TieKeptLocalIDs, ", "))
	}
	for _, skipped := range result.SkippedDependencies {
		fmt.Fprintf(os.Stderr, "Skipped dependency: %s\n", skipped)
	}
	return nil
}

// importTitleSearcher is the read seam the --dedup filter needs. It lives in
// THIS file because naming types.IssueFilter is denied by default under
// cmd/bd and import.go is the named exception for the bulk-movement family
// (.golangci.yml, forbidigo): the classic storage.DoltStorage satisfies it
// directly, and uowImportTitleSearcher adapts the proxied unit of work.
type importTitleSearcher interface {
	SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error)
}

// uowImportTitleSearcher adapts a unit of work's issue use case to the
// classic []*types.Issue search shape filterDuplicatesByTitle consumes. Both
// stacks run the same issueops search underneath (issues merged with wisps),
// so --dedup sees the same title universe in both modes.
type uowImportTitleSearcher struct {
	uw uow.UnitOfWork
}

func (s uowImportTitleSearcher) SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error) {
	page, err := s.uw.IssueUseCase().SearchIssues(ctx, query, filter)
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}

func deduplicateImportIssues(ctx context.Context, searcher importTitleSearcher, issues []*types.Issue, enabled bool) ([]*types.Issue, int) {
	if !enabled || len(issues) == 0 {
		return issues, 0
	}
	return filterDuplicatesByTitle(ctx, searcher, issues)
}

// filterDuplicatesByTitle removes issues whose title matches an existing open issue.
func filterDuplicatesByTitle(ctx context.Context, st importTitleSearcher, issues []*types.Issue) ([]*types.Issue, int) {
	existing, err := st.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return issues, 0
	}

	titleSet := make(map[string]bool, len(existing))
	for _, issue := range existing {
		if issue.Status != types.StatusClosed {
			titleSet[strings.ToLower(issue.Title)] = true
		}
	}

	var kept []*types.Issue
	skipped := 0
	for _, issue := range issues {
		if titleSet[strings.ToLower(issue.Title)] {
			skipped++
			continue
		}
		kept = append(kept, issue)
	}
	return kept, skipped
}
