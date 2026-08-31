package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/utils"
)

// importLocalResult holds counts from a local JSONL import.
type importLocalResult struct {
	Issues   int
	Memories int
}

// memoryRecord represents a memory entry in the JSONL export.
type memoryRecord struct {
	Type  string `json:"_type"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

type parsedJSONLRecord struct {
	issue  *types.Issue
	memory *memoryRecord
}

// importFromLocalJSONL imports issues (and memories) from a local JSONL file on disk
// into the Dolt store. Returns the number of issues imported and any error.
// This is a convenience wrapper around importFromLocalJSONLFull.
func importFromLocalJSONL(ctx context.Context, store storage.DoltStorage, localPath string) (int, error) {
	result, err := importFromLocalJSONLFull(ctx, store, localPath)
	if err != nil {
		return 0, err
	}
	return result.Issues, nil
}

// parseJSONLFile reads a JSONL file and returns parsed issues and config
// entries (memories). Pure function — no store I/O.
func parseJSONLFile(path string) ([]*types.Issue, map[string]string, error) {
	//nolint:gosec // G304: path from user-provided CLI argument
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read JSONL file %s: %w", path, err)
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	// Allow up to 64MB per line for large descriptions
	scanner.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	var issues []*types.Issue
	configEntries := make(map[string]string)

	for scanner.Scan() {
		record, err := parseJSONLRecord(scanner.Text())
		if err != nil {
			return nil, nil, err
		}
		if record.memory != nil && record.memory.Key != "" && record.memory.Value != "" {
			configEntries[kvPrefix+memoryPrefix+record.memory.Key] = record.memory.Value
		}
		if record.issue != nil {
			issues = append(issues, record.issue)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("failed to scan JSONL: %w", err)
	}

	return issues, configEntries, nil
}

func parseJSONLRecord(line string) (parsedJSONLRecord, error) {
	if line == "" {
		return parsedJSONLRecord{}, nil
	}

	// Peek at the record to check for _type field.
	var peek map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &peek); err != nil {
		return parsedJSONLRecord{}, fmt.Errorf("failed to parse JSONL line: %w", err)
	}

	// Skip the optional beads-jsonl metadata/header record.
	// Canonical exports produced by the stable-ordering / git-merge convention
	// prepend a schema+provenance line. It carries no _type and no issue fields;
	// without this guard it would fall through to the issue path and unmarshal
	// into an empty Issue. Real issue and memory records never carry _schema.
	if _, isHeader := peek["_schema"]; isHeader {
		return parsedJSONLRecord{}, nil
	}

	mem, err := parseJSONLMemoryRecord(line, peek)
	if err != nil {
		return parsedJSONLRecord{}, err
	}
	if mem != nil {
		return parsedJSONLRecord{memory: mem}, nil
	}

	issue, err := parseJSONLIssueRecord(line, peek)
	if err != nil {
		return parsedJSONLRecord{}, err
	}
	return parsedJSONLRecord{issue: issue}, nil
}

func parseJSONLMemoryRecord(line string, peek map[string]json.RawMessage) (*memoryRecord, error) {
	rawType, ok := peek["_type"]
	if !ok {
		return nil, nil
	}
	var typeStr string
	if err := json.Unmarshal(rawType, &typeStr); err != nil || typeStr != "memory" {
		return nil, nil
	}
	var mem memoryRecord
	if err := json.Unmarshal([]byte(line), &mem); err != nil {
		return nil, fmt.Errorf("failed to parse memory record: %w", err)
	}
	return &mem, nil
}

func parseJSONLIssueRecord(line string, peek map[string]json.RawMessage) (*types.Issue, error) {
	var issue types.Issue
	if err := json.Unmarshal([]byte(line), &issue); err != nil {
		return nil, fmt.Errorf("failed to parse issue from JSONL: %w", err)
	}
	// Skip tombstone entries: these are deleted issues exported by older
	// versions (pre-v0.50) with status "tombstone" and deleted_at set.
	if issue.Status == "tombstone" {
		return nil, nil
	}

	applyImportWispPlane(peek, &issue)
	issue.SetDefaults()
	return &issue, nil
}

// applyImportWispPlane resolves which storage plane (wisps vs issues table) a
// parsed import record routes to, shared by every JSONL parse loop
// (parseImportRecords for `bd import` in both storage modes, parseJSONLFile
// for bootstrap / init --from-jsonl / auto-import).
//
// The "wisp_plane" peek key is the EXPLICIT wisps-plane marker (bd-r9uce):
// export writes it for rows that live in the wisps table, precisely because
// row flags cannot be trusted for the plane decision — a promoted no-history
// wisp is a durable issues-table row that may still carry no_history=true
// (PromoteFromEphemeralInTx used to clear only Ephemeral, and wild data
// with that shape persists). Routing such a record by flags re-planes it
// into the wisps table, after which its cross-plane relations are dropped
// by the batch import and the row itself is no longer durable — silent data
// loss across export→import→export.
//
// The marker is deliberately a FRESH key, not a reuse of the legacy "wisp"
// boolean (lion, #5368 review): every pre-fix v0.38+ binary's alias branch
// is `hasWisp && !Ephemeral => Ephemeral=true`, so stamping "wisp" on a
// genuine no-history wisp would make every current binary import it as
// ephemeral — purge-eligible and excluded from that rig's next default
// export — turning the common rollout-skew case lossy. Readers that predate
// the fresh key simply ignore it and fall back to flag routing, the
// data-safe degradation in both skew directions. So:
//
//   - "wisp_plane": true      => wisps plane, whatever the flags say.
//   - key absent or false     => a no_history=true record is pinned to the
//     ISSUES plane (the promoted shape). The flag itself is preserved on the
//     row — clearing it would change the content hash and break the
//     byte-identity of export→import→export — only the routing is pinned.
//   - legacy "wisp": true     => the v0.35–v0.37 spelling of "ephemeral"
//     (those exports predate no_history): Ephemeral is restored — the alias
//     behavior import has always had, preserved verbatim.
func applyImportWispPlane(peek map[string]json.RawMessage, issue *types.Issue) {
	// A malformed marker is treated as absent (best-effort, like the
	// legacy-alias parse always was).
	planeMarker := false
	if raw, ok := peek["wisp_plane"]; ok {
		_ = json.Unmarshal(raw, &planeMarker)
	}
	if planeMarker {
		wisp := true
		issue.WispPlaneOverride = &wisp
		return
	}
	if legacy, ok := peek["wisp"]; ok && !issue.Ephemeral {
		// Legacy v0.35–v0.37 alias for "ephemeral", preserved verbatim.
		var wisp bool
		if err := json.Unmarshal(legacy, &wisp); err == nil && wisp {
			issue.Ephemeral = true
			return
		}
	}
	if issue.NoHistory && !issue.Ephemeral {
		// Promoted no-history wisp: durable row, stray flag. Pin it durable.
		durable := false
		issue.WispPlaneOverride = &durable
	}
}

// importFromLocalJSONLFull imports issues and memories from a local JSONL file
// using UPSERT semantics (an existing issue row is overwritten). Used by the
// explicit recovery paths: `bd bootstrap` and `bd init --from-jsonl`.
func importFromLocalJSONLFull(ctx context.Context, store storage.DoltStorage, localPath string) (*importLocalResult, error) {
	return importFromLocalJSONLWithOpts(ctx, store, localPath, false)
}

// importFromLocalJSONLConflictSkip is the auto-import upgrade-recovery
// fallback (GH#3955; the fallbackImporter seam in auto_import_upgrade.go).
// It is identical to importFromLocalJSONLFull except that an issue whose ID
// already exists is left untouched instead of being overwritten, so a
// regressed emptiness guard can never clobber live rows — worst case is a
// no-op.
func importFromLocalJSONLConflictSkip(ctx context.Context, store storage.DoltStorage, localPath string) (*importLocalResult, error) {
	return importFromLocalJSONLWithOpts(ctx, store, localPath, true)
}

// importFromLocalJSONLWithOpts is the shared implementation. It detects
// memory records (lines with "_type":"memory") and imports them via
// SetConfig, while routing regular issue records through the normal path.
// conflictSkip selects insert-if-new (true) vs UPSERT (false) for issue rows.
func importFromLocalJSONLWithOpts(ctx context.Context, store storage.DoltStorage, localPath string, conflictSkip bool) (*importLocalResult, error) {
	issues, configEntries, err := parseJSONLFile(localPath)
	if err != nil {
		return nil, err
	}

	memories, err := importLocalConfigEntries(ctx, store, configEntries)
	if err != nil {
		return nil, err
	}
	importedIssues, err := importLocalIssues(ctx, store, issues, conflictSkip)
	if err != nil {
		return nil, err
	}
	return &importLocalResult{Issues: importedIssues, Memories: memories}, nil
}

func importLocalConfigEntries(ctx context.Context, store storage.DoltStorage, configEntries map[string]string) (int, error) {
	memories := 0
	for key, value := range configEntries {
		if err := store.SetConfig(ctx, key, value); err != nil {
			return 0, fmt.Errorf("failed to import config %q: %w", key, err)
		}
		memories++
	}
	return memories, nil
}

func importLocalIssues(ctx context.Context, store storage.DoltStorage, issues []*types.Issue, conflictSkip bool) (int, error) {
	if len(issues) == 0 {
		return 0, nil
	}
	if err := configureImportedIssuePrefix(ctx, store, issues[0]); err != nil {
		return 0, err
	}

	opts := ImportOptions{
		SkipPrefixValidation: true,
		ConflictSkip:         conflictSkip,
	}
	importResult, err := importIssuesCore(ctx, "", store, issues, opts)
	if err != nil {
		return 0, err
	}
	return importResult.Created, nil
}

func configureImportedIssuePrefix(ctx context.Context, store storage.DoltStorage, firstIssue *types.Issue) error {
	// Auto-detect prefix from the first issue if not already configured.
	configuredPrefix, err := store.GetConfig(ctx, "issue_prefix")
	if err != nil || strings.TrimSpace(configuredPrefix) != "" {
		return nil
	}
	firstPrefix := utils.ExtractIssuePrefix(firstIssue.ID)
	if firstPrefix == "" {
		return nil
	}
	if err := store.SetConfig(ctx, "issue_prefix", firstPrefix); err != nil {
		return fmt.Errorf("failed to set issue_prefix from imported issues: %w", err)
	}
	return nil
}
