// Package storage provides shared types for issue storage.
//
// The concrete storage implementation lives in the dolt sub-package.
// This package holds interface and value types that are referenced by
// both the dolt implementation and its consumers (cmd/bd, etc.).
package storage

import (
	"context"
	"database/sql"
	"time"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/journalops"
)

// CloseIssueOptions carries the optional inputs to CloseIssueChecked.
type CloseIssueOptions struct {
	Reason  string
	Session string
	Force   bool // bypass the is_blocked guard (mirrors `bd close --force`)
	// ExpectedVersion, when non-nil, gates the close on an optimistic-concurrency
	// check: the close proceeds only if the issue's current RowVersion (the
	// row_lock token) equals *ExpectedVersion, otherwise it refuses with
	// ErrVersionMismatch atomically (the version read and the close share one
	// transaction). nil disables the check, leaving behavior unchanged. It is a
	// pointer, not an int64, so nil ("no check") is distinct from a caller that
	// requires version 0. Force bypasses child and blocker policy, not this
	// version check.
	//
	// RowVersion tracks lifecycle/ownership writes only — it is rewritten by
	// status, assignee, and started_at changes (claim, close, reclaim, unclaim,
	// updateIssueInTx). So this is a "close only if the issue's lifecycle state
	// is unchanged" guard, NOT an all-columns check: concurrent label, dependency,
	// rename, is_blocked, or compaction-only writes intentionally do not bump
	// row_lock and are not caught here (see the freshRowLock invariant in
	// internal/storage/issueops/lease.go).
	ExpectedVersion *int64
}

// CloseIssueResult reports the outcome of CloseIssueChecked.
type CloseIssueResult struct {
	Unchanged    bool // true when the issue was ALREADY closed (idempotent no-op)
	OpenChildren int  // nonzero when Force encountered open children, including idempotent re-closes
}

// UpdateIssueOptions carries the optional inputs to UpdateIssueChecked.
type UpdateIssueOptions struct {
	// ExpectedVersion, when non-nil, makes the update a compare-and-swap: it
	// proceeds only if the issue's current RowVersion (row_lock) equals
	// *ExpectedVersion, else it refuses with ErrVersionMismatch atomically.
	// nil disables the check. A pointer so nil is distinct from requiring a
	// legacy version of 0.
	ExpectedVersion *int64

	// ExpectedAssignee and ExpectedStatus are semantic-field compare-and-swap
	// guards (bd-wsqvw, `bd update --if-assignee/--if-status`): when non-nil,
	// the update proceeds only if the issue's current assignee/status equals
	// the expected value, else it refuses atomically with
	// ErrAssigneeMismatch/ErrStatusMismatch naming the actual state. A non-nil
	// pointer to "" is a real guard meaning "expected unassigned" — nil, not
	// the empty string, disables a check. Guards present together must ALL
	// hold (conjunction), and compose with ExpectedVersion. The guard read and
	// the update share one transaction, so there is no internal TOCTOU; a
	// concurrent writer that commits mid-transaction collides on the row_lock
	// cell rewrite and is replayed by the store's retry loop, which re-reads
	// and refuses (the same invariant as ExpectedVersion).
	ExpectedAssignee *string
	ExpectedStatus   *string
}

// MergeSlotStatus is returned by MergeSlotCheck and describes the current
// state of the merge slot bead.
type MergeSlotStatus struct {
	SlotID    string
	Available bool
	Holder    string
	Waiters   []string
}

// MergeSlotResult is returned by MergeSlotAcquire.
type MergeSlotResult struct {
	// SlotID is the bead ID of the merge slot.
	SlotID string
	// Acquired is true when the slot was successfully acquired by the caller.
	Acquired bool
	// Waiting is true when --wait was passed and the caller was added to the
	// waiters queue (the slot was held by someone else).
	Waiting bool
	// Holder is the current holder of the slot. When Acquired is true this
	// is the caller; when Waiting is true this is the previous holder.
	Holder string
	// Position is the 1-based position in the waiters queue when Waiting is true.
	Position int
}

// FastStatisticsStore provides a statistics method that skips the blocked-count
// traversal for callers that don't need it (e.g. bd stats --no-blocked).
type FastStatisticsStore interface {
	// GetStatisticsNoBlocked returns aggregate counts without the blocked-set
	// computation (computeBlockedIDs). BlockedIssues is nil in the result.
	GetStatisticsNoBlocked(ctx context.Context) (*types.Statistics, error)
}

// DoltStorage is the full interface for Dolt-backed stores, composing the core
// Storage interface with all capability sub-interfaces. Both DoltStore and
// EmbeddedDoltStore satisfy this interface.
type DoltStorage interface {
	Storage
	IssueLifecycleStore
	VersionControl
	HistoryViewer
	RemoteStore
	SyncStore
	FederationStore
	BulkIssueStore
	DependencyQueryStore
	EventQueryStore
	AnnotationStore
	ConfigMetadataStore
	CompactionStore
	AdvancedQueryStore
	FastStatisticsStore
}

// RawDBAccessor provides raw *sql.DB access for diagnostics and migrations.
// Callers that need raw SQL should type-assert to this interface.
type RawDBAccessor interface {
	DB() *sql.DB
	UnderlyingDB() *sql.DB
}

// StoreLocator provides filesystem path information for the store.
// Callers that need the store's on-disk location should type-assert to this interface.
type StoreLocator interface {
	Path() string
	CLIDir() string
}

// ActiveDatabaseSizer reports the approximate on-disk size of the active
// database when the current store instance has authoritative local filesystem
// access. Implementations return *ErrUnsupported when that particular instance
// is backed by storage that is not locally measurable.
type ActiveDatabaseSizer interface {
	ActiveDatabaseSize(ctx context.Context) (int64, error)
}

// GarbageCollector provides Dolt garbage collection capability.
// Callers that need to reclaim disk space should type-assert to this interface.
type GarbageCollector interface {
	DoltGC(ctx context.Context) error
}

// Flattener squashes all Dolt commit history into a single commit.
// Callers should type-assert to this interface for history compaction.
type Flattener interface {
	Flatten(ctx context.Context) error
}

// RemoteRefPruner manages the cached remote-tracking refs that anchor Dolt
// history. After a squash (Flatten/Compact) those refs still point at the
// pre-squash chain, making the follow-up GC a silent no-op on any workspace
// that has ever pushed or fetched (bd-agctw) — callers must prune them before
// GC. Pruning only touches the local cache; the next push/fetch re-creates
// the refs at the new tip. Tags anchor history the same way but are
// user-created, so they are listed for warning rather than deleted.
type RemoteRefPruner interface {
	ListRemoteRefs(ctx context.Context) ([]string, error)
	PruneRemoteRefs(ctx context.Context) ([]string, error)
	ListTags(ctx context.Context) ([]string, error)
}

type SchemaMigrator interface {
	ApplySchemaMigrations(ctx context.Context) (applied int, err error)
}

// Compactor squashes old Dolt commits while preserving recent ones.
// Callers should type-assert to this interface for selective history compaction.
type Compactor interface {
	Compact(ctx context.Context, initialHash, boundaryHash string, oldCommits int, recentHashes []string) error
}

// BlockedRecomputer recomputes the denormalized is_blocked column for every
// issue and wisp in one full pass and reports how many rows it corrected.
// Callers should type-assert to this interface for the is_blocked repair
// (bd-6dnrw.37): unlike the scoped post-pull recompute, it does not depend on a
// merge advancing HEAD, so it can recover a column a skipped recompute (a
// recompute that failed after its merge committed, or a hand-resolved
// conflicted pull) left stale. It is idempotent — a consistent database
// corrects nothing.
type BlockedRecomputer interface {
	RecomputeAllBlocked(ctx context.Context) (int, error)
}

// StateHasher returns a hash covering committed history plus the working set.
// Unlike GetCurrentCommit (HEAD only), the hash moves on uncommitted writes.
// Change detection against a SQL server must use this when available: server
// mode runs with dolt auto-commit off, so writes sit in the working set and
// HEAD does not advance.
// Callers should type-assert to this interface and fall back to
// GetCurrentCommit when the store does not implement it.
type StateHasher interface {
	GetStateHash(ctx context.Context) (string, error)
}

// The durable mutation journal's vocabulary lives in the journalops leaf, and
// these four names are ALIASES of it — not copies, and not a compatibility
// shim to be deleted later. The canon is journalops: what a record carries,
// what a page promises and what a truncation means are stated there, once, and
// every citation in this tree points at those symbols rather than repeating
// them here.
//
// THE ALIAS DIRECTION IS THE LOAD-BEARING PART. journalops imports context and
// fmt and nothing else, so it cannot name anything in this package; this
// package can name it. Declaring the canon down there and aliasing up here is
// what makes the leaf a leaf. And because a Go alias is the SAME TYPE, every
// existing implementation, every errors.As site and every caller compiles
// unchanged across the move — which is why the journal became a role without a
// line of behavior changing.
//
// The two interfaces BELOW these aliases stay declared here on purpose. They
// are the operator's half of the plane — retention and per-instance activation
// — and journalops states why they are deliberately not on the role.
type (
	// EventsJournalRow is journalops.Row: one raw bd_events_journal record.
	EventsJournalRow = journalops.Row
	// EventsJournalPage is journalops.Page: rows plus the journal head.
	EventsJournalPage = journalops.Page
	// EventsJournalTruncatedError is journalops.TruncatedError: a checkpoint
	// below the retained window, carrying the window that can still be served.
	EventsJournalTruncatedError = journalops.TruncatedError
	// EventsJournalCursor is journalops.Journal: the role a consumer holds to
	// page through the journal from a checkpoint.
	EventsJournalCursor = journalops.Journal
)

// EventsJournalTruncatedCode is journalops.TruncatedCode: the stable wire
// spelling of a truncation.
const EventsJournalTruncatedCode = journalops.TruncatedCode

// EventsJournalAccessor reads and prunes the durable events journal
// (bd_events_journal) through the store's own transaction machinery. Unlike
// RawDBAccessor — which only the server-mode store provides — this works on the
// embedded store too, which owns its connections and exposes no stable *sql.DB.
//
// IT IS THE OPERATOR SURFACE, and it is deliberately not the role. Retention is
// a decision the workspace makes, so a caller that only READS the journal asks
// for EventsJournalCursor — journalops.Journal — instead, and a surface
// documented never to retain then cannot prune rather than merely promising not
// to. journalops' package doc states the split; this is the half it excludes.
//
// ReadEventsJournal is also a SECOND read body, not a narrowing of the role's:
// it pays for the head read only in the cases where the truncation verdict is
// ambiguous, because `bd events tail --follow` runs it every second. The two
// share the row query and the verdict (issueops.ComputeEventsTruncation) and
// differ in exactly that, so they are pinned separately.
type EventsJournalAccessor interface {
	// ReadEventsJournal returns rows with seq greater than since, ordered by
	// seq ascending, optionally capped by limit (0 = no cap). It returns
	// *EventsJournalTruncatedError when since sits below the retained window,
	// on the terms journalops.Journal.ReadEventsJournalPage states.
	ReadEventsJournal(ctx context.Context, since int64, limit int) ([]EventsJournalRow, error)
	// PruneEventsJournal deletes rows with seq below before, honoring the
	// retain-days / retain-rows floors (0 = floor disabled), and returns the
	// number of rows deleted.
	PruneEventsJournal(ctx context.Context, before int64, retainDays, retainRows int) (int64, error)
}

// EventsJournalConfigurer controls durable events journal activation on ONE
// storage instance. Implementations must never use process-global state: a
// process can hold several stores at once (multiple projects, a test binary's
// parallel fixtures), and opening one with the journal enabled must not turn it
// on for any other. Callers type-assert; a store that does not implement it
// simply cannot journal.
//
// Activation is operator surface for the reason retention is: it is the
// workspace's answer to "do we record at all", read from the workspace's own
// config, and a consumer holding the read role has no business changing it.
type EventsJournalConfigurer interface {
	SetEventsJournalEnabled(enabled bool)
}

// LifecycleManager provides lifecycle inspection beyond Close().
type LifecycleManager interface {
	IsClosed() bool
}

// PendingCommitter provides the ability to commit pending (dirty) changes.
// Used by auto-commit and auto-push flows.
type PendingCommitter interface {
	CommitPending(ctx context.Context, actor string) (bool, error)
}

// PendingChangeDetector reports whether the working set holds changes a
// commit would capture. Unlike VersionControl.Status, this excludes
// dolt_ignore'd tables (wisp and lease tables appear in dolt_status but
// cannot be staged), so it answers "would CommitPending mint a commit?"
// without committing. Callers that must refuse to act on a dirty working
// set (bd dolt remote reset-data) should type-assert to this interface.
type PendingChangeDetector interface {
	HasCommittablePending(ctx context.Context) (bool, error)
}

// BackupStore provides Dolt backup operations (CALL DOLT_BACKUP) for
// disaster recovery.
// Callers that need backup functionality should type-assert to this interface.
type BackupStore interface {
	BackupAdd(ctx context.Context, name, url string) error
	BackupSync(ctx context.Context, name string) error
	BackupRemove(ctx context.Context, name string) error
	// BackupDatabase registers dir as a file:// Dolt backup remote and syncs
	// the full database to it, preserving complete commit history.
	BackupDatabase(ctx context.Context, dir string) error
	// RestoreDatabase restores the database from a Dolt backup at dir.
	// When force is true, the existing database is dropped before restoring.
	RestoreDatabase(ctx context.Context, dir string, force bool) error
}

// ReadyWorkCounter sizes the total ready-work count for a filter without
// materializing the counts mega-query. It is identical to
// len(GetReadyWorkWithCounts(filter with Limit=0)) but computed with cheap
// indexed COUNT(*)s over the ready predicate.
//
// THAT IDENTITY IS THIS INTERFACE'S WHOLE CONTRACT. issueops.ReadyCounter
// states it for every backend, and the store-backed body behind ReadyCounter()
// (internal/workapi/storereadycounter) is this method plus the shared filter
// builder. `bd ready`'s "Showing X of N" reaches it that way on both routes; it
// no longer type-asserts for the capability itself, so a store that cannot
// answer fails to compile rather than falling back to an unbounded query.
type ReadyWorkCounter interface {
	CountReadyWork(ctx context.Context, filter types.WorkFilter) (int, error)
}

// Transaction provides atomic multi-operation support within a single database transaction.
//
// The Transaction interface exposes a subset of storage methods that execute within
// a single database transaction. This enables atomic workflows where multiple operations
// must either all succeed or all fail (e.g., creating issues with dependencies and labels).
//
// # Transaction Semantics
//
//   - All operations within the transaction share the same database connection
//   - Changes are not visible to other connections until commit
//   - If any operation returns an error, the transaction is rolled back
//   - If the callback function panics, the transaction is rolled back
//   - On successful return from the callback, the transaction is committed
//
// # Compose surface (classic path)
//
// The transaction methods are implemented by the classic Dolt and
// embedded-Dolt stores. The domain/uow plumbing (internal/storage/domain) is a
// separate compose surface that does not implement storage.Transaction today;
// that asymmetry is pre-existing and out of scope for this surface.
//
// The read methods below let a caller assemble a whole composite view — a
// bd show-style assembly of counts and relations — inside ONE transaction, so
// everything it stitches together is read from a single snapshot and cannot
// tear across separate engine reads.
//
// TWO-SESSION WISP CAVEAT (server/Dolt backend only): the classic Dolt store
// runs durable tables and dolt-ignored wisp tables on two separate SQL sessions
// within one logical transaction. Reads that span both tiers in a single query
// (the ones flagged below) therefore see this transaction's own uncommitted
// DURABLE writes and all COMMITTED wisps, but NOT wisps written in the same
// still-open transaction — those become visible after commit. Single-tier reads
// (GetIssue, GetIssueComments, GetIssueCommentsPage, GetDependencyRecords,
// IsBlocked, IsBlockedBatch, GetLabels) route to the owning session and are
// read-your-writes on both tiers. The embedded-Dolt store has no session split,
// so every read there is read-your-writes on both tiers.
//
// # Example Usage
//
//	err := store.RunInTransaction(ctx, "bd: create parent and child", func(tx storage.Transaction) error {
//	    // Create parent issue
//	    if err := tx.CreateIssue(ctx, parentIssue, actor); err != nil {
//	        return err // Triggers rollback
//	    }
//	    // Create child issue
//	    if err := tx.CreateIssue(ctx, childIssue, actor); err != nil {
//	        return err // Triggers rollback
//	    }
//	    // Add dependency between them
//	    if err := tx.AddDependency(ctx, dep, actor); err != nil {
//	        return err // Triggers rollback
//	    }
//	    return nil // Triggers commit
//	})
type Transaction interface {
	// Issue operations
	CreateIssue(ctx context.Context, issue *types.Issue, actor string) error
	CreateIssues(ctx context.Context, issues []*types.Issue, actor string) error
	UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error
	CloseIssue(ctx context.Context, id string, reason string, actor string, session string) error
	DeleteIssue(ctx context.Context, id string) error
	GetIssue(ctx context.Context, id string) (*types.Issue, error)                                    // For read-your-writes within transaction
	SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error) // For read-your-writes within transaction
	SearchIssueIDs(ctx context.Context, query string, filter types.IssueFilter) ([]string, error)     // Narrow projection: returns ids only

	// Dependency operations
	AddDependency(ctx context.Context, dep *types.Dependency, actor string) error
	AddDependencyWithOptions(ctx context.Context, dep *types.Dependency, actor string, opts DependencyAddOptions) error
	RemoveDependency(ctx context.Context, issueID, dependsOnID string, actor string) error
	// RemoveDependencyWithOptions removes a dependency with explicit options.
	// EmitEvent records a dependency_removed history event for the explicit
	// bd dep remove verb; RemoveDependency stays silent for structural teardown.
	RemoveDependencyWithOptions(ctx context.Context, issueID, dependsOnID string, actor string, opts DependencyRemoveOptions) error
	GetDependencyRecords(ctx context.Context, issueID string) ([]*types.Dependency, error)
	// CycleThroughEdges reports a rendered cycle in the static scheduling set
	// (blocks, conditional-blocks, parent-child; not waits-for) that traverses
	// one of the given new edges (issueID -> dependsOnID pairs), or
	// "" when none does. It sees the transaction's own uncommitted dependency
	// writes, which must already include the edges. Lets bulk paths that add
	// edges run one merged whole-graph check before commit and roll back instead
	// of committing cycles (bd-6dnrw.8); pre-existing
	// cycles not using any of the new edges never block (bd-578h9.9).
	CycleThroughEdges(ctx context.Context, edges [][2]string) (string, error)

	// Label operations
	AddLabel(ctx context.Context, issueID, label, actor string) error
	RemoveLabel(ctx context.Context, issueID, label, actor string) error
	GetLabels(ctx context.Context, issueID string) ([]string, error)

	// Config operations (for atomic config + issue workflows)
	SetConfig(ctx context.Context, key, value string) error
	GetConfig(ctx context.Context, key string) (string, error)

	// Metadata operations (for internal state like import hashes)
	SetMetadata(ctx context.Context, key, value string) error
	GetMetadata(ctx context.Context, key string) (string, error)

	// Local metadata operations (dolt-ignored, clone-local state).
	// Used for tip timestamps, version stamps, tracker sync cursors, etc.
	// Data is ephemeral — callers must handle ("", nil) as the normal case.
	SetLocalMetadata(ctx context.Context, key, value string) error
	GetLocalMetadata(ctx context.Context, key string) (string, error)

	// Comment operations
	AddComment(ctx context.Context, issueID, actor, comment string) error
	ImportIssueComment(ctx context.Context, issueID, author, text string, createdAt time.Time) (*types.Comment, error)
	GetIssueComments(ctx context.Context, issueID string) ([]*types.Comment, error) // For read-your-writes within transaction
	// GetIssueCommentsPage returns one keyset page of an issue's comments in the
	// stable (created_at ASC, id ASC) order, resuming strictly after the cursor
	// (the zero cursor starts at the beginning of the thread). Lets a composite
	// view page a comment thread off the same snapshot as its other reads. See
	// storage.Storage.GetIssueCommentsPage for the full ordering and
	// page-walk-equals-full-read contract.
	GetIssueCommentsPage(ctx context.Context, issueID string, after CommentPageCursor, limit int) ([]*types.Comment, error)

	// Composite-view reads.
	//
	// Each mirrors the Storage-level method of the same name; they add no new
	// query shape, only the ability to run the existing read on the
	// transaction's snapshot, so a bd show-style assembly can gather every count
	// and relation it needs inside one transaction. All see this transaction's
	// own uncommitted DURABLE writes; the wisp-tier visibility of the
	// both-tiers-spanning reads is governed by the TWO-SESSION WISP CAVEAT above.

	// CountIssuesByGroup returns per-group issue counts. groupBy is one of:
	// status, priority, type, assignee, label. SPANS BOTH TIERS (merges wisps):
	// subject to the two-session wisp caveat on the server backend. Note it merges
	// committed wisps into the buckets while the transaction's SearchIssues reads
	// the issues table only, so their totals need not agree when committed wisps
	// exist — a pre-existing count-vs-search wisp-scoping asymmetry, not a tear.
	CountIssuesByGroup(ctx context.Context, filter types.IssueFilter, groupBy string) (map[string]int, error)

	// GetDependentRecords returns the raw inbound dependency rows whose target is
	// targetID (its dependents), spanning the durable and wisp dependency tables,
	// filtered by depType ("" = all), bounded by limit and paged by afterID.
	// SPANS BOTH TIERS: subject to the two-session wisp caveat on the server backend.
	GetDependentRecords(ctx context.Context, targetID string, depType string, limit int, afterID string) ([]*types.Dependency, error)
	// GetDependentRecordsForIssues returns the raw inbound dependency rows for a
	// SET of target ids in one batched read, keyed by target id. SPANS BOTH TIERS:
	// subject to the two-session wisp caveat on the server backend.
	GetDependentRecordsForIssues(ctx context.Context, targetIDs []string) (map[string][]*types.Dependency, error)
	// CountDependentRecords returns the total inbound-edge count of targetID
	// across both dependency tables (same predicate/scope as GetDependentRecords).
	// SPANS BOTH TIERS: subject to the two-session wisp caveat on the server backend.
	CountDependentRecords(ctx context.Context, targetID string, depType string) (int, error)

	// IsBlocked reports the denormalized transitive is_blocked flag for one issue
	// plus its direct blocker ids. Single-tier (routes to the issue's own tier):
	// read-your-writes on both tiers.
	IsBlocked(ctx context.Context, issueID string) (bool, []string, error)
	// IsBlockedBatch reports the denormalized transitive is_blocked flag for a
	// page of ids in one batched read. ids present in neither the issues nor the
	// wisps table are absent from the map; callers treat absent as not-blocked.
	// Partitions ids by tier and reads each on its owning session, so it is
	// read-your-writes on both tiers even for a mixed durable/wisp batch.
	IsBlockedBatch(ctx context.Context, ids []string) (map[string]bool, error)

	// EventsSince returns durable events strictly after cursor, ordered by
	// (created_at ASC, id ASC) and bounded by limit; issueID scopes the feed to
	// one issue's history ("" = all issues). Durable events table only.
	EventsSince(ctx context.Context, cursor EventCursor, issueID string, limit int) ([]*types.Event, error)
}

// IssueLifecycleTransaction is the internal transaction lane for lifecycle
// transitions that must retain the backend's durable publication semantics.
// It deliberately extends neither Storage nor Transaction: ordinary callers
// continue to use the stable generic transaction contract.
type IssueLifecycleTransaction interface {
	Transaction
	ReopenIssueWithResult(ctx context.Context, id string, reason string, actor string) (bool, error)
}

// IssueLifecycleStore runs a lifecycle-aware transaction. It is an internal
// companion to Storage for code that must close or reopen within one durable
// operation and observe the result before committing.
type IssueLifecycleStore interface {
	RunInIssueLifecycleTransaction(ctx context.Context, commitMsg string, fn func(tx IssueLifecycleTransaction) error) error
}

// DependencyAddOptions controls dependency insertion for both the store-level
// AddDependencyWithOptions and the transaction-scoped AddDependencyWithOptions.
type DependencyAddOptions struct {
	// SkipCycleCheck bypasses the recursive pre-insert cycle check. Callers
	// that set it MUST run Transaction.CycleThroughEdges before commit and fail
	// on new blocks/conditional-blocks/parent-child cycles (waits-for is excluded) — skipping the per-edge check trades
	// per-edge cost for one whole-graph check, never graph integrity
	// (bd-6dnrw.8).
	SkipCycleCheck bool
	// EmitEvent records a dependency_added history event on the source's event
	// table for a genuine new edge. Only the explicit dependency verbs set it;
	// create-with-deps and structural edge wiring leave it unset so implicit
	// edges stay quiet, matching the proxied DepInsertOpts.EmitEvent gate.
	EmitEvent bool
}

// DependencyRemoveOptions controls dependency removal for both the store-level
// RemoveDependencyWithOptions and the transaction-scoped RemoveDependencyWithOptions.
type DependencyRemoveOptions struct {
	// EmitEvent records a dependency_removed history event on the source's event
	// table when a genuine edge is removed. Only the explicit bd dep remove verb
	// sets it; structural removals (issue delete, reparent, batch, duplicate
	// cleanup) leave it unset so they wire edges away quietly, matching the
	// proxied DepInsertOpts.EmitEvent gate so both backends record identical
	// history.
	EmitEvent bool
}
