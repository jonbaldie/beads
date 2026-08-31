package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

// importChunkSize bounds the number of issues written per transaction during
// import. One giant transaction holds the store's write lock for the whole
// batch (a 26k-issue import measured ~2 minutes of full-store outage on
// SQLite); bounded chunks release the lock between commits so concurrent
// readers and writers interleave.
//
// 250 was picked by measurement (8k-issue import, SQLite backend): 250- and
// 500-issue chunks both cost ~13% total time over one big transaction, but
// 250 holds the write lock ~1.4s per transaction versus ~2.8s at 500 and ~6s
// at 1000 — and the SQLite connection's busy_timeout is 5s, so per-chunk lock
// holds must stay comfortably below that for concurrent bd operations to wait
// out an import instead of failing. A var, not a const, so tests can shrink it.
var importChunkSize = 250

// importInterChunkPause is slept between import transactions. Bounding the
// transactions is not enough on its own: SQLite busy-polling has no fairness
// queue, so a loop that re-issues BEGIN IMMEDIATE microseconds after each
// COMMIT re-takes the lock before any waiter's poll fires and starves every
// concurrent bd operation for the whole import — measured as an unchanged
// 100% failure pattern with zero lock acquisitions across 20 chunk commits.
// The pause is the acquisition window that lets waiters in. 150ms costs
// ~15s on a 26k-issue import (104 chunks) against the ~2-minute write time.
var importInterChunkPause = 150 * time.Millisecond

// importPause is the sleep seam for the inter-chunk pause, swappable in tests.
var importPause = time.Sleep

// importProgress is where chunked imports report per-chunk progress.
// Swappable in tests.
var importProgress io.Writer = os.Stderr

// ImportOptions configures import behavior.
type ImportOptions struct {
	DryRun                     bool
	SkipUpdate                 bool
	Strict                     bool
	RenameOnImport             bool
	ClearDuplicateExternalRefs bool
	DeletionIDs                []string
	SkipPrefixValidation       bool
	ProtectLocalExportIDs      map[string]time.Time
	// ConflictSkip makes the import insert-if-new instead of UPSERT: an
	// issue whose ID already exists is left untouched. Set only by the
	// auto-import upgrade-recovery fallback (GH#3955); explicit `bd import`
	// leaves this false and keeps UPSERT semantics.
	ConflictSkip bool
	// AllowStale imports rows even when their updated_at is older than the
	// local issue's, overwriting newer local state. Required for the
	// restore-an-older-snapshot recovery workflow, which the default stale
	// guard otherwise silently no-ops per row (bd-6dnrw.9). Only settable
	// via explicit `bd import --allow-stale`; auto-import paths never set it.
	AllowStale bool
}

// ImportResult describes what an import operation did.
type ImportResult struct {
	Created   int
	Updated   int
	Unchanged int
	Skipped   int
	Deleted   int
	IDMapping map[string]string
	ImportResultDiagnostics
	ImportedIDs         []string
	StaleSkippedIDs     []string
	SkippedDependencies []string
	// UpdatedIssues lists existing local issues whose row the import
	// rewrote (incoming strictly newer, content differs), with a
	// field-level summary, so reverts of local state are visible instead
	// of silent (bd-hj85c).
	UpdatedIssues []ImportChange
	// TieKeptLocalIDs lists incoming rows whose updated_at equals the
	// local issue's but whose content differs. The upsert keeps the local
	// row for these (second-granularity timestamp ties, bd-hj85c); their
	// aux data still merges.
	TieKeptLocalIDs []string
}

// ImportResultDiagnostics groups optional collision and prefix-validation
// details that are orthogonal to the import's row counts and ID lists.
type ImportResultDiagnostics struct {
	Collisions       int
	CollisionIDs     []string
	PrefixMismatch   bool
	ExpectedPrefix   string
	MismatchPrefixes map[string]int
}

// ImportChange describes how an import row modified an existing local issue.
type ImportChange struct {
	ID      string `json:"id"`
	Changes string `json:"changes,omitempty"`
}

// importIssueLookup is the read seam the import pre-filters and dry-run
// classifiers need. The classic storage.DoltStorage satisfies it, and so does
// the proxied unit of work's domain.IssueUseCase, so both modes classify
// incoming rows against local state with the same code.
type importIssueLookup interface {
	GetIssuesByIDs(ctx context.Context, ids []string) ([]*types.Issue, error)
}

// importIssuesCore imports issues into the Dolt store.
// This is a bridge function that delegates to the Dolt store's batch creation.
func importIssuesCore(ctx context.Context, _ string, store storage.DoltStorage, issues []*types.Issue, opts ImportOptions) (*ImportResult, error) {
	if opts.DryRun || len(issues) == 0 {
		return &ImportResult{Skipped: len(issues)}, nil
	}

	// The stale guard has two halves (bd-pkim8). This pre-filter reports the
	// rows that are already known stale (StaleSkippedIDs) and keeps their
	// labels/comments/dependencies out of the batch entirely. It is a separate
	// read, though, so a local update that commits between it and the batch
	// write would slip through — RejectStaleUpserts below closes that race by
	// re-checking updated_at inside the upsert itself.
	var staleSkippedIDs []string
	var changePlan importChangePlan
	if !opts.AllowStale {
		filtered, skipped, plan, err := filterStaleImportIssues(ctx, store, issues)
		if err != nil {
			return nil, err
		}
		issues = filtered
		staleSkippedIDs = skipped
		changePlan = plan
		if len(issues) == 0 {
			return &ImportResult{Skipped: len(staleSkippedIDs), StaleSkippedIDs: staleSkippedIDs}, nil
		}
	}

	var skippedDependencies []string
	skippedDependencySet := make(map[string]struct{})
	// In-txn half of the stale guard: rows the conditional upsert rejected
	// (local update committed between the pre-filter read and the batch
	// write). The transaction may retry, so dedup by ID.
	staleRejectedSet := make(map[string]struct{})
	actor := getActorWithGit()
	batchOpts := storage.BatchCreateOptions{
		SkipPrefixValidation:           opts.SkipPrefixValidation,
		ConflictSkip:                   opts.ConflictSkip,
		RejectStaleUpserts:             !opts.AllowStale,
		SkipDependencyValidationErrors: true,
		OnSkippedDependency: func(issueID, dependsOnID, reason string) {
			skipped := fmt.Sprintf("%s -> %s: %s", issueID, dependsOnID, reason)
			if _, ok := skippedDependencySet[skipped]; ok {
				return
			}
			skippedDependencySet[skipped] = struct{}{}
			skippedDependencies = append(skippedDependencies, skipped)
		},
		OnStaleRejected: func(issueID string) {
			staleRejectedSet[issueID] = struct{}{}
		},
	}
	var err error
	if len(issues) <= importChunkSize {
		// Small import: one transaction, dependencies inline — exactly the
		// pre-chunking behavior.
		err = store.CreateIssuesWithFullOptions(ctx, issues, actor, batchOpts)
	} else {
		err = importIssuesChunked(ctx, store, issues, actor, batchOpts)
	}
	if err != nil {
		return nil, err
	}

	return assembleImportResult(issues, staleSkippedIDs, changePlan, staleRejectedSet, skippedDependencies), nil
}

// assembleImportResult folds the batch write's in-transaction outcomes (stale
// rejections, skipped dependencies) into the pre-filter's classification,
// producing the report both the classic and the proxied import return. Kept
// as ONE function so the two modes cannot drift on how a stale-rejected row
// is attributed.
func assembleImportResult(issues []*types.Issue, staleSkippedIDs []string, changePlan importChangePlan, staleRejectedSet map[string]struct{}, skippedDependencies []string) *ImportResult {
	importedIDs := make([]string, 0, len(issues))
	for _, issue := range issues {
		if _, rejected := staleRejectedSet[issue.ID]; rejected {
			staleSkippedIDs = append(staleSkippedIDs, issue.ID)
			continue
		}
		importedIDs = append(importedIDs, issue.ID)
	}
	// Drop planned updates the in-txn guard rejected (a local update raced
	// in between the pre-filter read and the batch write).
	updatedIssues := make([]ImportChange, 0, len(changePlan.Updates))
	updatedCount := 0
	for _, change := range changePlan.Updates {
		if _, rejected := staleRejectedSet[change.ID]; rejected {
			continue
		}
		updatedIssues = append(updatedIssues, change)
		updatedCount++
	}
	return &ImportResult{
		Created:             len(importedIDs),
		Updated:             updatedCount,
		Skipped:             len(staleSkippedIDs),
		ImportedIDs:         importedIDs,
		StaleSkippedIDs:     staleSkippedIDs,
		SkippedDependencies: skippedDependencies,
		UpdatedIssues:       updatedIssues,
		TieKeptLocalIDs:     changePlan.TieKeptLocal,
	}
}

// importIssuesChunked writes a large import in bounded transactions of
// importChunkSize rows instead of one batch-wide transaction, sleeping
// importInterChunkPause between commits. One giant transaction holds the
// store's write lock for the whole import, taking every concurrent bd
// operation down with it; bounded chunks cap the per-transaction lock hold,
// and the pause between commits is the fairness window that actually lets
// waiters acquire (see importInterChunkPause).
//
// Rows are written in dependency order (orderImportIssuesForChunking): every
// readiness-affecting edge owned by a non-cycle row points at a row in the same
// or an earlier chunk — Kahn's order for the acyclic majority, plus a
// cycle-breaking fallback that still emits each cycle member before the rows it
// blocks — so that edge rides inline with its row and both commit in one
// transaction. Only a force-emitted cycle member can carry a readiness edge into
// the deferred pass, and that is the already-tolerated corrupt/legacy-cycle
// window. A concurrent reader therefore never observes a non-cycle imported bead
// without the blocking edges its import file declares —
// `bd ready` mid-import cannot offer blocked work for dispatch, and a crash
// mid-import cannot freeze a bead in a spuriously-ready state.
//
// Only edges that cannot be satisfied when their row commits are deferred to
// a final dependency pass: edges into an intra-batch dependency cycle
// (invalid for blocking types; still broken and skip-reported at wire time,
// though for a cycle of length >=3 the specific edge dropped — and thus which
// member is left spuriously ready — can differ from the single-transaction
// import, which checks the whole cycle in file order) and non-readiness edges
// (related, discovered-from) that point at a later chunk. The dependency pass submits
// row copies stripped to those deferred edges with ConflictSkip set, so an
// existing row is never rewritten: a concurrent update landing between a
// row's chunk and the dependency pass cannot stale-reject the pass and drop
// the edges (the resubmit-the-full-row alternative did exactly that,
// silently and unrecoverably). Rows whose phase-1 write was itself
// stale-rejected are excluded from the pass: a stale snapshot keeps its
// labels, comments, AND dependencies out (bd-578h9.8).
//
// Every per-row application is an idempotent upsert (conditional-update row
// write, INSERT IGNORE labels, existence-checked comments, deterministic-id
// dependency edges, created-events only for genuinely new rows), so a failure
// mid-import leaves a committed, durable prefix and re-running the same import
// converges on the full set — subject to the import's standing stale policy:
// a row a rival updated since its chunk committed is locally newer on the
// re-run, so the pre-filter stale-skips that snapshot wholesale (row and any
// still-unwired deferred edges), reported in StaleSkippedIDs. That is the
// same local-wins outcome the single-transaction import gave a rival update
// racing a crashed import, and it can only affect deferred edges (non-readiness
// edges, plus the readiness edges of force-emitted cycle members); every
// non-cycle row's readiness edges commit with their row.
func importIssuesChunked(ctx context.Context, store storage.DoltStorage, issues []*types.Issue, actor string, opts storage.BatchCreateOptions) error {
	// Apply the cross-bucket dependency policy over the full set up front:
	// the engine's per-batch filter only sees one chunk, so it could no
	// longer detect an edge whose endpoints land in different chunks.
	issues, err := issueops.FilterCreateIssuesMixedBucketDependencies(issues, opts)
	if err != nil {
		return err
	}
	ordered := orderImportIssuesForChunking(issues)

	// Partitioning narrows each issue's dependency slice to its inline subset in
	// place; always restore the full slices so the caller's issues stay intact
	// for retries and reporting.
	fullDeps := make([][]*types.Dependency, len(ordered))
	for i, issue := range ordered {
		fullDeps[i] = issue.Dependencies
	}
	defer func() {
		for i, issue := range ordered {
			issue.Dependencies = fullDeps[i]
		}
	}()
	deferred := partitionChunkedImportDeps(ordered)

	// Record phase-1 stale rejections locally (as well as forwarding them to
	// the caller): the dependency pass must skip those rows.
	phase1Stale := make(map[string]struct{})
	rowOpts := opts
	rowOpts.OnStaleRejected = func(issueID string) {
		phase1Stale[issueID] = struct{}{}
		if opts.OnStaleRejected != nil {
			opts.OnStaleRejected(issueID)
		}
	}

	pacer := &importChunkPacer{}
	if err := writeImportRowChunks(ctx, store, ordered, actor, rowOpts, pacer); err != nil {
		return err
	}
	return wireDeferredImportDeps(ctx, store, deferred, phase1Stale, len(ordered), actor, opts, pacer)
}

// importChunkPacer sleeps importInterChunkPause between import transactions. One
// shared pacer spans phase 1 and the dependency pass, so the fairness pause is
// issued before every transaction except the very first: SQLite busy-polling has
// no fairness queue, and a back-to-back BEGIN IMMEDIATE re-takes the lock before
// a waiter's poll fires (see importInterChunkPause).
type importChunkPacer struct{ transactions int }

func paceImportTransaction(p *importChunkPacer) {
	if p.transactions > 0 {
		importPause(importInterChunkPause)
	}
	p.transactions++
}
