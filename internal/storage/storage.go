// Package storage provides shared types for issue storage.
//
// The concrete storage implementation lives in the dolt sub-package.
// This package holds interface and value types that are referenced by
// both the dolt implementation and its consumers (cmd/bd, etc.).
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/issueops"
	"github.com/jonbaldie/beads/memoryops"
)

// The guarded issue-operation error vocabulary is declared and documented by
// the public contract package, github.com/jonbaldie/beads/issueops. These are
// the same values, so every storage.ErrX reference and every errors.Is site
// keeps matching the identical error.
var (
	ErrAlreadyClaimed    = issueops.ErrAlreadyClaimed
	ErrNotClaimable      = issueops.ErrNotClaimable
	ErrAssigneeMismatch  = issueops.ErrAssigneeMismatch
	ErrNotFound          = issueops.ErrNotFound
	ErrValidation        = issueops.ErrValidation
	ErrNotInitialized    = issueops.ErrNotInitialized
	ErrPrefixMismatch    = issueops.ErrPrefixMismatch
	ErrCloseBlocked      = issueops.ErrCloseBlocked
	ErrCloseOpenChildren = issueops.ErrCloseOpenChildren
	ErrAlreadyExists     = issueops.ErrAlreadyExists
	ErrAlreadyIdentified = issueops.ErrAlreadyIdentified
	ErrVersionMismatch   = issueops.ErrVersionMismatch
	ErrStatusMismatch    = issueops.ErrStatusMismatch
)

// CloseOpenChildrenError reports the issue and open-child count that refused a
// guarded close. See issueops.CloseOpenChildrenError.
type CloseOpenChildrenError = issueops.CloseOpenChildrenError

// ErrNotOwner is returned when an actor tries to unclaim an issue that is claimed
// by a different actor. Releasing another actor's claim requires the force
// escape hatch (bd unclaim --force), reserved for admin/reaper use.
//
// It is an ALIAS of issueops.ErrNotOwner, which now declares it: a refusal the
// public Releaser role raises has to be classifiable by a caller that cannot
// import this package. The identity is preserved, so every errors.Is site in
// the tree keeps matching the same value.
var ErrNotOwner = issueops.ErrNotOwner

// ErrCommitIndeterminate marks a write error whose durable outcome may be
// unknown. Such errors must not be replayed because the write may already have
// landed; retry layers must check this sentinel before classifying a wrapped
// cause as transient.
var ErrCommitIndeterminate = errors.New("write commit result indeterminate")

// ClaimedByFragment and NotClaimableStatusFragment are the exact message
// fragments a claim refusal puts after the sentinel to carry the conflicting
// assignee/status: ErrAlreadyClaimed reads "<sentinel> by <assignee>" and
// ErrNotClaimable reads "<sentinel>: status <status>". They are the single
// source of truth for that format so producer and consumer
// (beads.ParseClaimConflict) cannot drift: the consumer reconstructs its marker
// as ErrAlreadyClaimed.Error()+ClaimedByFragment rather than hardcoding the
// literal.
//
// The producers are the two claim bodies —
// internal/storage/issueops.ClaimIssueInTx and
// internal/storage/domain.issueUseCaseImpl.claim — which compose the message
// before wrapping it in an issueops.ClaimConflictError. That type's Error() is
// a PASSTHROUGH, so the fragments must be in the wrapped error; a caller
// reading the type's FIELDS needs none of this. These stay for the parser, and
// the conformance suite's RunClaimRefusalMessagesCarryTheirFragments plus the
// root-package ParseClaimConflict tests are the tripwires that producer and
// consumer still agree character for character.
//
// One refusal deliberately omits the " by <assignee>" tail: an OPEN issue held
// by someone else answers with holder-steering copy instead (wy-yuclk), so its
// assignee is recovered from the typed field rather than the prose.
const (
	ClaimedByFragment          = " by "
	NotClaimableStatusFragment = ": status "
)

// CommentPageCursor is the resume position for a keyset page of an issue's
// comments: the (created_at, id) of the last comment already returned. The zero
// value starts a walk from the beginning of the thread.
//
// It lives in the storage package (rather than issueops) because issueops
// imports storage — the reverse would be an import cycle — so the shared cursor
// type is defined here and referenced from the issueops query layer.
type CommentPageCursor struct {
	CreatedAt time.Time
	ID        string
}

// Storage is the interface satisfied by *dolt.DoltStore.
// Consumers depend on this interface rather than on the concrete type so that
// alternative implementations (mocks, proxies, etc.) can be substituted.
//
// External implementers note: this contract includes the optimistic-concurrency
// helper UpdateIssueChecked and the atomic MergeMetadata method as required
// members. Adding a required method is a breaking change for out-of-tree
// implementations; such additions are called out in CHANGELOG.md and the
// examples/library-usage guide so implementers have a migration path.
type Storage interface {
	// IssueLifecycle returns the guarded issue-lifecycle surface for this
	// store. Every decorator in a store's chain answers for itself and layers
	// its own behavior onto the inner result, so the returned Lifecycle carries
	// the same hook and telemetry layers the store itself carries.
	//
	// A capability the lifecycle role does not cover gets its own role
	// interface and its own accessor here; it does not get appended to
	// issueops.Lifecycle.
	IssueLifecycle() (issueops.Lifecycle, error)

	// IssueReader returns the guarded issue-query surface for this store: the
	// read counterpart of IssueLifecycle, and its own role rather than four
	// more methods on that one. Like the lifecycle accessor, every decorator
	// in a store's chain answers for itself, so the returned Reader carries the
	// same layers the store itself carries.
	//
	// Reads fire no hooks, so the hook decorator's answer is its inner store's
	// unchanged. The accessor exists on it anyway: a seam a caller has to
	// reason about decorator-by-decorator is not a seam.
	IssueReader() (issueops.Reader, error)

	// IssueClaimer returns the guarded atomic-claim surface for this store:
	// its own role beside IssueLifecycle and IssueReader rather than a fifth
	// verb on the lifecycle. Like the other two accessors, every decorator in
	// a store's chain answers for itself and layers its own behavior onto the
	// inner result.
	IssueClaimer() (issueops.Claimer, error)

	// ReadyClaimer returns the guarded take-ready-work surface for this store.
	// It is its own role rather than a fifth verb on IssueLifecycle because
	// the caller names a question and the implementation picks the answer, so
	// selection is part of the operation and not a patch. Like the other
	// accessors, every decorator in a store's chain answers for itself.
	ReadyClaimer() (issueops.ReadyClaimer, error)

	// BatchCloser returns the guarded close-many surface for this store: the
	// role whose REQUEST is the transaction boundary, so N closes land as one
	// durable act instead of N. It is its own role rather than a Close
	// overload because a batch reports per-item outcomes that a single close
	// has nowhere to put.
	BatchCloser() (issueops.BatchCloser, error)

	// BatchCreator returns the guarded create-many surface for this store: the
	// other role whose REQUEST is the transaction boundary. It is its own role
	// rather than a Create overload because a batch is ALL OR NOTHING where a
	// batch close keeps its survivors.
	BatchCreator() (issueops.BatchCreator, error)

	// DependencyEditor returns the guarded dependency-edge surface for this
	// store. It is its own role rather than more IssueLifecycle verbs because
	// an edge has two endpoints and every refusal it raises is a statement
	// about the graph they sit in, which a patch has nowhere to put.
	DependencyEditor() (issueops.DependencyEditor, error)

	// Commenter returns the guarded add-comment surface for this store. It is
	// its own role because a comment appends to a thread the issue owns and
	// changes no field of the issue, so an IssuePatch has nothing to carry.
	Commenter() (issueops.Commenter, error)

	// Counter returns the guarded issue-count surface for this store. It is
	// its own role rather than a fourth IssueReader method because a count is
	// a NUMBER about a set where the reader answers with pages of issues:
	// there is no order, no page and no cursor in the question.
	//
	// Reads fire no hooks, as for IssueReader.
	Counter() (issueops.Counter, error)

	// StatsReporter returns the guarded summary-statistics surface for this
	// store — `bd status` and its `bd stats` alias. Its own role rather than a
	// sixth Counter dimension because its numbers are dependency-aware.
	StatsReporter() (issueops.StatsReporter, error)

	// IssueRelations returns the guarded neighbor-query surface for this
	// store: the read counterpart of DependencyEditor. It is its own role
	// rather than a fourth IssueReader method because it answers a question
	// about EDGES — anchored on one issue, directed, and carrying the edge
	// type — where the reader answers with pages of issues.
	//
	// Reads fire no hooks, so the hook decorator's answer is its inner store's
	// unchanged, as it is for IssueReader.
	IssueRelations() (issueops.Relations, error)

	// WorkspaceConfig returns the guarded workspace-settings surface for this
	// store: the durable key-value plane `bd config` reads and writes. It is
	// its own role rather than more verbs elsewhere because it answers about
	// the WORKSPACE rather than about an issue, and because a write here can
	// re-project a value into a normalized lookup table.
	WorkspaceConfig() (issueops.WorkspaceConfig, error)

	// Memories returns the guarded persistent-memory surface for this store:
	// the keyed notes `bd remember`, `bd recall`, `bd forget` and `bd memories`
	// work with, and that `bd prime` injects.
	//
	// IT IS NOT MORE WorkspaceConfig VERBS, and the rule forbidding that is the
	// least of the reasons. Memories ride in the config table but they are user
	// data in a reserved namespace, they are their own MERGE CLASS — a config
	// conflict auto-resolves with --theirs only when every conflicted key is a
	// memory — they have their own validation vocabulary (content, derived
	// keys) and they have a found/not-found user contract the settings plane
	// deliberately does not have. A settings enumeration that carried them
	// would be answering a different question in the same list.
	//
	// Its hook decorator recurses UNWRAPPED for the vocabulary reason Sweeper's
	// does: remembering is a write, but the hook vocabulary is on_create,
	// on_update and on_close and each hands a script an ISSUE. See
	// hook_memories.go.
	Memories() (memoryops.Memories, error)

	// VersionReconciler returns the clone-local version markers for this store:
	// the dolt-ignored pair recording which bd binary last opened this
	// workspace and the highest one that ever has. It is its own role rather
	// than two more keys on WorkspaceConfig because settings are durable and
	// travel with the database while these two are deliberately per-clone.
	VersionReconciler() (issueops.VersionReconciler, error)

	// CycleDetector returns the guarded cycle-report surface for this store: its
	// own role because it is asked of the WHOLE graph and answers with paths,
	// where every Relations request names one anchor and answers with that
	// anchor's neighbors. Reads fire no hooks, as for IssueReader.
	CycleDetector() (issueops.CycleDetector, error)
	// EdgeReader returns the guarded stored-edge surface for this store: raw
	// dependency rows for many anchors at once, keyed by source, with a
	// per-anchor miss. Its own role rather than a second IssueRelations method,
	// which is single-anchor, answers with hydrated issues, and refuses a
	// missing anchor outright. Reads fire no hooks, as for IssueReader.
	EdgeReader() (issueops.EdgeReader, error)
	// BlockingAnnotator returns the guarded blocking-decoration surface for this
	// store: the open blockers, the issues blocked and the parent of a page of
	// ids, which is what a listing prints beside each row. Its own role rather
	// than a second EdgeReader method because that one answers with STORED ROWS
	// where this one answers a DERIVED summary of two edge types with closed
	// blockers dropped. Reads fire no hooks, as for IssueReader.
	BlockingAnnotator() (issueops.BlockingAnnotator, error)
	// TreeWalker returns the guarded dependency-tree surface for this store: the
	// recursive walk from ONE root that `bd dep tree` renders. Its own role
	// rather than a mode of IssueRelations or EdgeReader because a recursive
	// walk has a depth, a cycle policy and a node shape of its own. Reads fire
	// no hooks, as for IssueReader.
	TreeWalker() (issueops.TreeWalker, error)
	// GraphCounter returns the guarded edge-count surface for this store: how
	// many dependency edges each of several anchors has, in one named
	// direction, spanning both dependency planes. Its own role rather than a
	// third Counter method (that one's predicate is a filter over the issues
	// table and says nothing about an edge) and rather than a counted
	// EdgeReader (that one answers with the stored ROWS, outbound only). Reads
	// fire no hooks, as for IssueReader.
	GraphCounter() (issueops.GraphCounter, error)
	// ReadyCounter returns the guarded ready-count surface for this store: the
	// size of the ready set, which is the number `bd ready`'s pagination
	// publishes and which no other role answers. Counter's predicate is a
	// filter over one table where the ready predicate is blocker-aware.
	//
	// Reads fire no hooks, as for IssueReader.
	ReadyCounter() (issueops.ReadyCounter, error)
	// Querier returns the guarded boolean-query surface for this store: `bd
	// query`'s expression language, which has OR, NOT and parentheses. Its own
	// role rather than a mode of IssueReader because a ListRequest is a
	// CONJUNCTION and expresses no disjunction. Reads fire no hooks, as for
	// IssueReader.
	Querier() (issueops.Querier, error)
	// Sweeper returns the guarded bulk-clearance surface for this store —
	// `bd purge` and `bd prune`, which are one capability over two disjoint
	// tiers rather than two. Its own role because it describes a SET and then
	// acts on it: no Lifecycle patch names a set, and composing a count with a
	// delete would reopen the window this role exists to close.
	//
	// It is the one WRITE role whose hook decorator recurses UNWRAPPED: there
	// is no on_delete hook to fire (internal/hooks publishes create, update
	// and close), and the rows a sweep would name it with are gone. See
	// hook_sweeper.go.
	Sweeper() (issueops.Sweeper, error)
	// Deleter returns the named-row erasure surface for this store —
	// `bd delete`. Its own role rather than a Sweeper mode because Sweeper
	// erases a set the caller DESCRIBED and this one erases rows the caller
	// NAMED: a named row with a dependent the request did not name is refused
	// unless the caller says cascade or force.
	//
	// Its hook decorator recurses UNWRAPPED for the same reason Sweeper's
	// does: there is no on_delete hook to fire and the rows are gone. See
	// hook_deleter.go.
	Deleter() (issueops.Deleter, error)
	// Bootstrapper returns the guarded identity-seeding surface for this store:
	// the one-time write that turns a database bd can connect to into a
	// workspace bd can use. It is its own role rather than more keys on
	// WorkspaceConfig because it REFUSES an already-identified substrate, which
	// is a guard no settings write has anywhere to express.
	//
	// Its hook decorator recurses unwrapped for the vocabulary reason Sweeper's
	// does: a bootstrap names no issue, and on a workspace this new the hooks
	// are not installed yet. See hook_bootstrapper.go.
	Bootstrapper() (issueops.Bootstrapper, error)
	// InitVerifier returns the guarded identity-read surface for this store: the
	// prefix and project id `bd init` adopts, reconciles against, or refuses to
	// invent. It is NOT Bootstrapper's read half: bd reads this identity on
	// paths where it is forbidden to write one — a bts-provisioned team
	// database, a gateway whose credential may be read-only — and a caller that
	// must not write must not be handed the writer.
	//
	// Reads fire no hooks, as for IssueReader.
	InitVerifier() (issueops.InitVerifier, error)
	// MetadataCAS returns the conditional single-key metadata write for this
	// store: set metadata[key] only if it currently holds the value the caller
	// expected. It is its own role rather than another Lifecycle guard because
	// Lifecycle's ExpectedVersion/Assignee/Status gate an ordinary edit on the
	// row's LIFECYCLE, and coordination state that is not a claim lives on keys
	// the caller invented, which no lifecycle guard can name.
	//
	// It is a WRITE role and its hook decorator WRAPS: a swap that lands is an
	// update to an issue, which is a hook the vocabulary publishes. See
	// hook_metadata_cas.go.
	MetadataCAS() (issueops.MetadataCAS, error)
	// BatchApplier returns the guarded apply-many surface for this store:
	// a HETEROGENEOUS list of creates, updates, closes and edges applied in
	// declaration order as one durable act. It is its own role rather than a
	// fifth batch verb because its unit is a PLAN — create these, wire them,
	// close the step that spawned them — and each of BatchCreator, BatchCloser
	// and DependencyEditor is one verb repeated, so composing two of them means
	// two transactions with a window in between.
	//
	// It is a WRITE role and its hook decorator WRAPS: every landed item is an
	// event the hook vocabulary publishes. See hook_batch_applier.go.
	BatchApplier() (issueops.BatchApplier, error)
	// Releaser returns the claim-release surface for this store: give up the
	// claim on one issue, optionally only while a named holder still has it.
	// It is its own role beside IssueClaimer rather than a method on it,
	// because a caller entitled to release its own work is often not entitled
	// to take new work, and a surface carrying both hands it a capability it
	// should not be able to reach.
	//
	// It is a WRITE role and its hook decorator WRAPS: a release changes
	// assignee and status, which is on_update — the same event the journal
	// already records for it. See hook_releaser.go.
	Releaser() (issueops.Releaser, error)

	// Issue CRUD
	CreateIssue(ctx context.Context, issue *types.Issue, actor string) error
	CreateIssues(ctx context.Context, issues []*types.Issue, actor string) error
	GetIssue(ctx context.Context, id string) (*types.Issue, error)
	GetIssueByExternalRef(ctx context.Context, externalRef string) (*types.Issue, error)
	GetIssuesByIDs(ctx context.Context, ids []string) ([]*types.Issue, error)
	UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error
	// UpdateIssueChecked applies the update like UpdateIssue, with an optional
	// optimistic-concurrency precondition: see UpdateIssueOptions.ExpectedVersion.
	// The version read and the update share one transaction (a true CAS).
	UpdateIssueChecked(ctx context.Context, id string, updates map[string]interface{}, actor string, opts UpdateIssueOptions) error
	ReopenIssue(ctx context.Context, id string, reason string, actor string) error
	UnclaimIssue(ctx context.Context, id string, actor string, force bool) error
	// UnclaimIssueIfAssignee releases a claim only while the issue is still
	// assigned to expectedAssignee (compare-and-swap, the inverse of
	// ClaimIssue). Returns ErrAssigneeMismatch, leaving the issue untouched,
	// when the current assignee differs.
	UnclaimIssueIfAssignee(ctx context.Context, id string, actor string, expectedAssignee string) error
	UpdateIssueType(ctx context.Context, id string, issueType string, actor string) error
	CloseIssue(ctx context.Context, id string, reason string, actor string, session string) error
	// CloseIssueChecked closes an issue, but refuses with ErrCloseOpenChildren
	// when it has open parent-child dependents, or ErrCloseBlocked when it has a
	// live direct blocker (an open blocks/waits-for/
	// conditional-blocks edge) unless opts.Force is set — the historical
	// `bd close` guard. A bare is_blocked=1 with no live direct blocker (a purely
	// transitive parent-child block, or a stale column) is not refused. The
	// blocked-check and the close run in ONE transaction, so the guard is atomic
	// (no TOCTOU). When opts.ExpectedVersion is non-nil it adds an orthogonal
	// optimistic-concurrency precondition: the close proceeds only if the issue's
	// current RowVersion still equals *opts.ExpectedVersion, else it refuses with
	// ErrVersionMismatch atomically (Force does NOT bypass this check). Already-
	// closed is an idempotent success with Unchanged=true; a missing issue returns
	// ErrNotFound.
	CloseIssueChecked(ctx context.Context, id string, actor string, opts CloseIssueOptions) (CloseIssueResult, error)
	DeleteIssue(ctx context.Context, id string) error
	SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error)
	SearchIssuesWithCounts(ctx context.Context, query string, filter types.IssueFilter) ([]*types.IssueWithCounts, error)
	// SearchIssueIDs is a narrow-projection variant of SearchIssues that
	// returns only matching issue IDs. Use when full row hydration is wasted
	// (e.g., partial-ID resolution in internal/utils/id_parser.go).
	SearchIssueIDs(ctx context.Context, query string, filter types.IssueFilter) ([]string, error)

	// Dependencies
	AddDependency(ctx context.Context, dep *types.Dependency, actor string) error
	// AddDependencyWithOptions adds a dependency with explicit options. The
	// explicit dependency verbs (bd dep add / bd link) pass EmitEvent to record
	// a dependency_added history event; AddDependency is the no-event default
	// used by create-with-deps and structural callers.
	AddDependencyWithOptions(ctx context.Context, dep *types.Dependency, actor string, opts DependencyAddOptions) error
	RemoveDependency(ctx context.Context, issueID, dependsOnID string, actor string) error
	// RemoveDependencyWithOptions removes a dependency with explicit options. The
	// explicit dependency verb (bd dep remove) passes EmitEvent to record a
	// dependency_removed history event; RemoveDependency is the no-event default
	// used by structural callers (issue delete, reparent, batch, duplicate cleanup).
	RemoveDependencyWithOptions(ctx context.Context, issueID, dependsOnID string, actor string, opts DependencyRemoveOptions) error
	GetDependencies(ctx context.Context, issueID string) ([]*types.Issue, error)
	GetDependents(ctx context.Context, issueID string) ([]*types.Issue, error)
	GetDependenciesWithMetadata(ctx context.Context, issueID string) ([]*types.IssueWithDependencyMetadata, error)
	GetDependentsWithMetadata(ctx context.Context, issueID string) ([]*types.IssueWithDependencyMetadata, error)
	GetDependencyTree(ctx context.Context, issueID string, maxDepth int, showAllPaths bool, reverse bool) ([]*types.TreeNode, error)

	// Labels
	AddLabel(ctx context.Context, issueID, label, actor string) error
	RemoveLabel(ctx context.Context, issueID, label, actor string) error
	GetLabels(ctx context.Context, issueID string) ([]string, error)
	GetIssuesByLabel(ctx context.Context, label string) ([]*types.Issue, error)

	// Work queries
	GetReadyWork(ctx context.Context, filter types.WorkFilter) ([]*types.Issue, error)
	GetReadyWorkWithCounts(ctx context.Context, filter types.WorkFilter) ([]*types.IssueWithCounts, error)
	GetBlockedIssues(ctx context.Context, filter types.WorkFilter) ([]*types.BlockedIssue, error)
	GetEpicsEligibleForClosure(ctx context.Context) ([]*types.EpicStatus, error)

	// Wisp queries
	// ListWisps returns ephemeral issues matching the filter.
	// It always restricts to Ephemeral=true; callers do not need to set that flag.
	ListWisps(ctx context.Context, filter types.WispFilter) ([]*types.Issue, error)

	// Comments and events
	AddIssueComment(ctx context.Context, issueID, author, text string) (*types.Comment, error)
	GetIssueComments(ctx context.Context, issueID string) ([]*types.Comment, error)
	// GetIssueCommentsPage returns one keyset page of an issue's comments in the
	// stable (created_at ASC, id ASC) total order, resuming strictly after the
	// after cursor (the zero cursor starts from the beginning of the thread).
	// id is the primary key, so the same-second tie-break is total: a thread
	// with several comments in the same created_at second still pages
	// completely, and concatenating every page of a full walk yields exactly the
	// same comments in the same order as GetIssueComments — no dropped or
	// duplicated comment. The resume predicate is sargable: it seeks the
	// (issue_id, created_at, id) index rather than scanning the whole thread.
	//
	// The after cursor MUST come from a comment previously returned by a read
	// (this method or GetIssueComments), whose CreatedAt matches the stored
	// DATETIME second. Feeding a cursor with a sub-second CreatedAt can skip
	// same-second rows (AddIssueComment already truncates its returned CreatedAt
	// for this reason).
	//
	// Keyset semantics, like an audit feed: a comment inserted with a backdated
	// created_at that lands behind an in-progress cursor is not seen by that
	// walk — the walk only moves forward. A whole-thread read or a fresh walk
	// still returns it.
	//
	// limit <= 0 uses a store default (100); a larger limit is capped at 500. A
	// caller that pages until len(page) < limit must therefore keep limit <= 500
	// or use empty-page termination instead: a request for limit > 500 always
	// returns at most 500 rows and would stop a len-based loop one page early.
	GetIssueCommentsPage(ctx context.Context, issueID string, after CommentPageCursor, limit int) ([]*types.Comment, error)
	GetEvents(ctx context.Context, issueID string, limit int) ([]*types.Event, error)
	GetAllEventsSince(ctx context.Context, since time.Time) ([]*types.Event, error)

	// Provenance is an append-only event log binding issues to opaque external
	// artifacts (git SHAs, PRs, work-ids). Record is idempotent on a
	// deterministic id; there is deliberately no update or delete operation.
	RecordProvenanceEvent(ctx context.Context, ev types.ProvenanceEvent) (id string, inserted bool, err error)
	GetProvenanceEvents(ctx context.Context, issueID string, kindFilter string) ([]types.ProvenanceEvent, error)
	GetProvenanceByRef(ctx context.Context, ref string) ([]types.ProvenanceEvent, error)

	// Aggregate counts — cheaper than materializing rows when only cardinality is needed.
	// Filter.Limit and Filter.Offset are ignored by CountIssues; all others apply.

	// CountIssues returns the number of issues matching query and filter.
	CountIssues(ctx context.Context, query string, filter types.IssueFilter) (int64, error)
	// CountIssuesByGroup returns per-group counts. groupBy is one of:
	// status, priority, type, assignee, label.
	CountIssuesByGroup(ctx context.Context, filter types.IssueFilter, groupBy string) (map[string]int, error)
	// CountDependents returns the number of issues that depend on issueID.
	CountDependents(ctx context.Context, issueID string) (int64, error)
	// CountDependencies returns the number of issues that issueID depends on.
	CountDependencies(ctx context.Context, issueID string) (int64, error)
	// CountIssueComments returns the number of comments on an issue.
	CountIssueComments(ctx context.Context, issueID string) (int64, error)
	// CountEvents returns the number of audit events for an issue, capped at limit
	// (or unbounded if limit == 0).
	CountEvents(ctx context.Context, issueID string, limit int) (int64, error)

	// Streaming iterators (be-jaavsb / be-yinl4d).
	//
	// IterIssues streams issues matching the filter. Use this in place of
	// SearchIssues when the result set is potentially unbounded
	// (filter.Limit == 0 or absent). For bounded queries SearchIssues
	// remains the right call.
	IterIssues(ctx context.Context, query string, filter types.IssueFilter) (Iter[types.Issue], error)
	// IterDependentsWithMetadata streams dependents (issues that depend on
	// issueID) with the relationship metadata attached. Replaces the slice
	// path for bd show --json --include-dependents on hub beads.
	IterDependentsWithMetadata(ctx context.Context, issueID string) (Iter[types.IssueWithDependencyMetadata], error)
	// IterDependenciesWithMetadata is the inverse direction — issues that
	// issueID depends on, with metadata.
	IterDependenciesWithMetadata(ctx context.Context, issueID string) (Iter[types.IssueWithDependencyMetadata], error)
	// IterIssueComments streams comments on an issue, ordered by created_at.
	IterIssueComments(ctx context.Context, issueID string) (Iter[types.Comment], error)
	// IterEvents streams the audit-trail events for an issue, ordered by
	// created_at descending. limit==0 means unbounded.
	IterEvents(ctx context.Context, issueID string, limit int) (Iter[types.Event], error)
	// IterAllEventsSince streams every audit-trail event in the rig newer
	// than `since`. There is no bounded variant — full-rig event scans are
	// inherently unbounded.
	IterAllEventsSince(ctx context.Context, since time.Time) (Iter[types.Event], error)
	// IterReadyWork streams issues that are ready for work (no open
	// blockers), matching the filter.
	IterReadyWork(ctx context.Context, filter types.WorkFilter) (Iter[types.Issue], error)
	// IterBlockedIssues streams blocked issues (with the blockers surfaced
	// in BlockedIssue), matching the filter.
	IterBlockedIssues(ctx context.Context, filter types.WorkFilter) (Iter[types.BlockedIssue], error)
	// IterWisps streams ephemeral issues matching the filter. Always
	// restricts to Ephemeral=true; callers do not need to set that flag.
	IterWisps(ctx context.Context, filter types.WispFilter) (Iter[types.Issue], error)

	// Statistics
	GetStatistics(ctx context.Context) (*types.Statistics, error)

	// Configuration
	SetConfig(ctx context.Context, key, value string) error
	GetConfig(ctx context.Context, key string) (string, error)
	GetAllConfig(ctx context.Context) (map[string]string, error)

	// Local metadata operations (dolt-ignored, clone-local state).
	// Used for tip timestamps, version stamps, tracker sync cursors, etc.
	// Data is ephemeral — callers must handle ("", nil) as the normal case.
	SetLocalMetadata(ctx context.Context, key, value string) error
	GetLocalMetadata(ctx context.Context, key string) (string, error)

	// RunInTransaction may retry setup failures before fn is entered. Once fn
	// starts, it is invoked at most once for this public call: callers retry
	// explicitly when repeating their work is safe. An error wrapping
	// ErrCommitIndeterminate means the durable outcome may be unknown and must
	// not be blindly replayed.
	RunInTransaction(ctx context.Context, commitMsg string, fn func(tx Transaction) error) error

	// MergeSlot — serialized conflict resolution primitive.
	// Each rig has one merge slot bead (<prefix>-merge-slot, labeled gt:slot).
	// The slot ID is derived from the issue_prefix config key.
	MergeSlotCreate(ctx context.Context, actor string) (*types.Issue, error)
	MergeSlotCheck(ctx context.Context) (*MergeSlotStatus, error)
	MergeSlotAcquire(ctx context.Context, holder, actor string, wait bool) (*MergeSlotResult, error)
	MergeSlotRelease(ctx context.Context, holder, actor string) error

	// Metadata slots — key-value pairs stored in issue metadata JSON.
	// Used by gt for delegation tracking, hook state, and other per-issue data.
	SlotSet(ctx context.Context, issueID, key, value, actor string) error
	SlotGet(ctx context.Context, issueID, key string) (string, error)
	SlotClear(ctx context.Context, issueID, key, actor string) error

	// MergeMetadata merges a single key into an issue's metadata JSON as a raw
	// JSON value (nested objects/arrays are preserved). The read-modify-write
	// runs in a single transaction, so two concurrent merges of DIFFERENT keys
	// both survive rather than clobbering each other. SlotSet is built on it.
	MergeMetadata(ctx context.Context, issueID, key string, value json.RawMessage, actor string) error

	// Lifecycle
	Close() error
}
