package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
	publicops "github.com/jonbaldie/beads/issueops"
)

// proxiedImporter hands back the guarded bulk-import surface for the
// proxied-server provider, through the provider's OWN capability accessor —
// the same two-step proxiedIssueReader and proxiedBatchCloser perform, and
// for the same reason: the accessor is where each layer is added, so a
// command that reached for the constructor would get an unlayered importer.
func proxiedImporter() (publicops.Importer, error) {
	if getUOWProvider() == nil {
		return nil, errors.New("proxied-server UOW provider not initialized")
	}
	src, ok := getUOWProvider().(uow.ImporterSource)
	if !ok {
		return nil, fmt.Errorf("proxied-server provider %T does not offer the import surface", getUOWProvider())
	}
	return src.Importer()
}

// runImportRecordsProxied is the proxied-server import pipeline over the
// parsed records. It mirrors runImportRecordsClassic stage for stage — dedup,
// dry-run classification, stale pre-filter, batch write, issue_prefix
// reconciliation — through the SAME classification and reporting code, with
// two deliberate structural differences:
//
//   - ONE COMMIT PER INVOCATION. The classic path chunks a large import into
//     bounded transactions (a SQLite write-lock fairness measure) and commits
//     the issue_prefix sync separately; the proxied path has no PostRun
//     auto-commit and the whole import — rows, memories, prefix sync — lands
//     in ONE unit of work with ONE history entry, the Importer capability's
//     contract.
//
//   - The stale guard keeps its classic two-half shape: the pre-filter runs in
//     its own read (reporting StaleSkippedIDs and keeping stale rows' aux data
//     out of the batch), and RejectStaleUpserts re-checks updated_at inside
//     the write transaction, closing the same race the classic path closes.
func runImportRecordsProxied(ctx context.Context, issues []*types.Issue, memories []memoryRecord, source string, opts importOptions) error {
	if getUOWProvider() == nil {
		return fmt.Errorf("proxied-server UOW provider not initialized")
	}

	issues, dedupHits, err := deduplicateProxiedImportIssues(ctx, issues, opts.dedup)
	if err != nil {
		return err
	}

	if opts.dryRun {
		return runProxiedImportDryRun(ctx, issues, memories, source, dedupHits, opts.allowStale)
	}

	issues, staleSkippedIDs, changePlan, err := prepareProxiedStaleImport(ctx, issues, opts.allowStale)
	if err != nil {
		return err
	}

	importer, err := proxiedImporter()
	if err != nil {
		return err
	}

	// THE BATCH: rows, memories and the issue_prefix reconciliation in one
	// transaction, one history entry. The prefix sync runs even when the
	// batch is otherwise empty, exactly as the classic path's post-commit
	// sync does (be-llaf; config.yaml is authoritative, not a rename).
	batch, err := importer.ImportBatch(ctx, publicops.ImportBatchRequest{
		Actor:                getActorWithGit(),
		Issues:               issues,
		Memories:             importMemoryEntries(memories),
		AllowStale:           opts.allowStale,
		SkipPrefixValidation: true,
		SyncIssuePrefix:      config.GetString("issue-prefix"),
		Source:               filepath.Base(source),
	})
	if err != nil {
		return fmt.Errorf("import failed: %w", err)
	}
	result := importResultJSON{Source: source, DedupHits: dedupHits, DryRun: opts.dryRun, Memories: batch.MemoriesImported}
	applyProxiedImportOutcome(&result, issues, staleSkippedIDs, changePlan, batch)
	return renderImportOutcome(result, source, dedupHits)
}

func deduplicateProxiedImportIssues(ctx context.Context, issues []*types.Issue, enabled bool) ([]*types.Issue, int, error) {
	if !enabled || len(issues) == 0 {
		return issues, 0, nil
	}
	type dedupOutcome struct {
		kept []*types.Issue
		hits int
	}
	out, err := uow.RunTxRead(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (dedupOutcome, error) {
		kept, hits := deduplicateImportIssues(ctx, uowImportTitleSearcher{uw: uw}, issues, true)
		return dedupOutcome{kept: kept, hits: hits}, nil
	})
	if err != nil {
		return nil, 0, err
	}
	return out.kept, out.hits, nil
}

func runProxiedImportDryRun(ctx context.Context, issues []*types.Issue, memories []memoryRecord, source string, dedupHits int, allowStale bool) error {
	result := importResultJSON{
		Source:    source,
		DedupHits: dedupHits,
		DryRun:    true,
		Memories:  len(memories),
		Skipped:   dedupHits,
	}
	classification, err := uow.RunTxRead(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (*ImportResult, error) {
		return classifyDryRunImport(ctx, uw.IssueUseCase(), issues, allowStale)
	})
	if err != nil {
		return fmt.Errorf("dry-run: %w", err)
	}
	applyImportDryRunClassification(&result, classification)
	return renderImportDryRun(result, len(memories), source, dedupHits)
}

func prepareProxiedStaleImport(ctx context.Context, issues []*types.Issue, allowStale bool) ([]*types.Issue, []string, importChangePlan, error) {
	if allowStale || len(issues) == 0 {
		return issues, nil, importChangePlan{}, nil
	}
	type staleOutcome struct {
		filtered []*types.Issue
		skipped  []string
		plan     importChangePlan
	}
	out, err := uow.RunTxRead(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (staleOutcome, error) {
		filtered, skipped, plan, err := filterStaleImportIssues(ctx, uw.IssueUseCase(), issues)
		if err != nil {
			return staleOutcome{}, err
		}
		return staleOutcome{filtered: filtered, skipped: skipped, plan: plan}, nil
	})
	if err != nil {
		return nil, nil, importChangePlan{}, err
	}
	return out.filtered, out.skipped, out.plan, nil
}

func importMemoryEntries(memories []memoryRecord) []publicops.ImportMemory {
	entries := make([]publicops.ImportMemory, 0, len(memories))
	for _, mem := range memories {
		entries = append(entries, publicops.ImportMemory{
			Key:   kvPrefix + memoryPrefix + mem.Key,
			Value: mem.Value,
		})
	}
	return entries
}

func applyProxiedImportOutcome(result *importResultJSON, issues []*types.Issue, staleSkippedIDs []string, changePlan importChangePlan, batch publicops.ImportBatchResult) {
	if len(issues) == 0 {
		result.Skipped += len(staleSkippedIDs)
		result.StaleSkippedIDs = append(result.StaleSkippedIDs, staleSkippedIDs...)
		return
	}
	staleRejectedSet := make(map[string]struct{}, len(batch.StaleRejectedIDs))
	for _, id := range batch.StaleRejectedIDs {
		staleRejectedSet[id] = struct{}{}
	}
	skippedDependencies := make([]string, 0, len(batch.SkippedDependencies))
	for _, dep := range batch.SkippedDependencies {
		skippedDependencies = append(skippedDependencies, fmt.Sprintf("%s -> %s: %s", dep.IssueID, dep.DependsOnID, dep.Reason))
	}
	applyImportOutcome(result, assembleImportResult(issues, staleSkippedIDs, changePlan, staleRejectedSet, skippedDependencies))
}
