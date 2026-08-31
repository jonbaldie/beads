package types

import (
	"encoding/json"
	"time"
)

// Issue field groups keep types.Issue under the TooManyFields threshold
// without changing promoted field access or JSON wire shape. encoding/json
// inlines anonymous structs in declaration order, so the flattened key order
// matches the historical Issue struct.

// IssueID is the identity group of an Issue.
type IssueID struct {
	// ===== Core Identification =====
	ID          string `json:"id"`
	ContentHash string `json:"-"` // Internal: SHA256 of canonical content
}

// IssueContent is the body-text group of an Issue.
type IssueContent struct {
	// ===== Issue Content =====
	Title              string `json:"title"`
	Description        string `json:"description,omitempty"`
	Design             string `json:"design,omitempty"`
	AcceptanceCriteria string `json:"acceptance_criteria,omitempty"`
	Notes              string `json:"notes,omitempty"`
	SpecID             string `json:"spec_id,omitempty"`
}

// IssueWorkflow is the status, type, and assignment group of an Issue.
type IssueWorkflow struct {
	// ===== Status & Workflow =====
	Status    Status    `json:"status,omitempty"`
	Priority  int       `json:"priority"` // No omitempty: 0 is valid (P0/critical)
	IssueType IssueType `json:"issue_type,omitempty"`
	// IsBlocked is the persisted readiness projection. It is included in journal
	// snapshots so graph deltas can be replayed without recomputing readiness.
	// omitempty keeps it out of every other serialization (export JSONL, --json
	// output): only journal snapshots, which set it explicitly, carry it.
	IsBlocked bool `json:"is_blocked,omitempty"`

	// ===== Assignment =====
	Assignee         string `json:"assignee,omitempty"`
	Owner            string `json:"owner,omitempty"` // Human owner for CV attribution (git author email)
	EstimatedMinutes *int   `json:"estimated_minutes,omitempty"`
}

// IssueTimes is the timestamp group of an Issue.
type IssueTimes struct {
	// ===== Timestamps =====
	CreatedAt       time.Time  `json:"created_at"`
	CreatedBy       string     `json:"created_by,omitempty"` // Who created this issue (GH#748)
	UpdatedAt       time.Time  `json:"updated_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"` // When this issue transitioned to in_progress (GH#2796)
	ClosedAt        *time.Time `json:"closed_at,omitempty"`
	CloseReason     string     `json:"close_reason,omitempty"`      // Reason provided when closing
	ClosedBySession string     `json:"closed_by_session,omitempty"` // Claude Code session that closed this issue
}

// IssueLease is the lease, concurrency, and schedule group of an Issue.
type IssueLease struct {
	// ===== Leasing (claim TTL + heartbeat; migrations 0054/0055) =====
	// Hydrated from the ephemeral, node-local leases table (bd-lrgn1), not
	// from issues columns. NULL when there is no active lease on this node.
	// row_lock is an internal serialization mechanism (an issues column); it is
	// surfaced read-only to Go callers as RowVersion in the Concurrency group
	// below (json:"-", never serialized).
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"` // When the current claim's lease expires
	HeartbeatAt    *time.Time `json:"heartbeat_at,omitempty"`     // Last heartbeat from the lease owner
	// LeaseGrantedNode names the replica that granted the lease
	// (config.NodeID() at claim time). A lease is only enforceable there: on
	// any other replica the liveness view is stale by up to one sync interval,
	// so reclaim refuses a positively-foreign lease unless explicitly
	// overridden. Empty means "provenance unknown" (a pre-0016 lease row, or a
	// deployment that cannot name its replicas), which is treated as local.
	// It rides the JSONL interchange so an imported lease keeps its true
	// granting replica.
	LeaseGrantedNode string `json:"lease_granted_node,omitempty"`

	// ===== Concurrency (generic Issue JSON/JSONL omits this field) =====
	// RowVersion is an opaque optimistic-concurrency token for the library's own
	// Go call sites: the issues/wisps row_lock cell, a random non-zero value the
	// engine rewrites on every status/ownership-mutating write. It is
	// EQUALITY-ONLY — compare it, never order or interpret it — and a change
	// signals the row was mutated since you read it. It is json:"-" on purpose:
	// row_lock is random per write, so generic Issue serialization would break
	// stable list/export round-trips. The detail-view DTO projects it explicitly
	// as `revision` for guarded clients (IssueDetails.Revision, set by
	// NewIssueDetails, and on the wire at GET /v0/beads/issues/{id}); Go
	// consumers read RowVersion directly.
	//
	// Coverage is deliberately partial: it changes on claim/close/unclaim and the
	// generic update path, but NOT on direct-UPDATE paths that rewrite text
	// without touching row_lock (RestoreFromSnapshotInTx, the compaction
	// text-truncation path). For a complete change-detection key, combine it with
	// updated_at (which those paths DO bump), status, and the label set
	// (label-only and reopen writes change those, not row_lock).
	//
	// 0 appears only on legacy rows backfilled by migration 0054 (DEFAULT 0) that
	// have not been mutated since; any issue created by the current code path is
	// non-zero (create stamps freshRowLock()).
	RowVersion int64 `json:"-"`

	// ===== Time-Based Scheduling (GH#820) =====
	DueAt      *time.Time `json:"due_at,omitempty"`      // When this issue should be completed
	DeferUntil *time.Time `json:"defer_until,omitempty"` // Hide from bd ready until this time
}

// IssueMeta is the external-ref, metadata, and compaction group of an Issue.
type IssueMeta struct {
	// ===== External Integration =====
	ExternalRef  *string `json:"external_ref,omitempty"`  // e.g., "gh-9", "jira-ABC"
	SourceSystem string  `json:"source_system,omitempty"` // Adapter/system that created this issue (federation)

	// ===== Custom Metadata =====
	// Metadata holds arbitrary JSON data for extension points (tool annotations, file lists, etc.)
	// Validated as well-formed JSON on create/update. See GH#1406.
	Metadata json.RawMessage `json:"metadata,omitempty"`

	// ===== Compaction Metadata =====
	CompactionLevel   int        `json:"compaction_level,omitempty"`
	CompactedAt       *time.Time `json:"compacted_at,omitempty"`
	CompactedAtCommit *string    `json:"compacted_at_commit,omitempty"` // Git commit hash when compacted
	OriginalSize      int        `json:"original_size,omitempty"`
}

// IssueGraph is the routing and relational group of an Issue.
type IssueGraph struct {
	// ===== Internal Routing (not synced via git) =====
	SourceRepo     string `json:"-"` // Which repo owns this issue (multi-repo support)
	IDPrefix       string `json:"-"` // Override prefix for ID generation (appends to config prefix)
	PrefixOverride string `json:"-"` // Completely replace config prefix (for cross-rig creation)

	// WispPlaneOverride, when non-nil, pins which storage plane this in-memory
	// record routes to (true = wisps table, false = issues table), overriding
	// the Ephemeral/NoHistory flag inference in issueops.IsWisp. Import sets it
	// from the export stream's explicit "wisp" plane marker so a promoted
	// no-history wisp — a durable issues-table row that (pre-fix, or in wild
	// data) still carries no_history=true — is never re-planed into the wisps
	// table, after which default export would treat it as wisp-plane state
	// (bd-r9uce). Never serialized, never persisted; nil means "infer from
	// flags", which is the behavior everywhere outside import.
	WispPlaneOverride *bool `json:"-"`

	// ===== Relational Data (populated for export/import) =====
	Labels       []string      `json:"labels,omitempty"`
	Dependencies []*Dependency `json:"dependencies,omitempty"`
	Comments     []*Comment    `json:"comments,omitempty"`
}

// IssueWisp is the messaging, storage-class, and marker group of an Issue.
type IssueWisp struct {
	// ===== Messaging Fields (inter-agent communication) =====
	Sender    string   `json:"sender,omitempty"`     // Who sent this (for messages)
	Ephemeral bool     `json:"ephemeral,omitempty"`  // If true, not synced via git
	NoHistory bool     `json:"no_history,omitempty"` // If true, stored in wisps table but NOT GC-eligible
	WispType  WispType `json:"wisp_type,omitempty"`  // Classification for TTL-based compaction (gt-9br)

	// StorageClass is the create-selected marker for the record's history and
	// replication contract. Persistence plane transitions preserve it except
	// where coherence requires normalizing explicit versioned to empty on
	// demotion or explicit ephemeral to empty on promotion. When the marker is
	// empty, the effective class follows the plane: ephemeral for Ephemeral or
	// NoHistory rows and versioned otherwise.
	StorageClass StorageClass `json:"storage_class,omitempty"`
	// NOTE: RepliesTo, RelatesTo, DuplicateOf, SupersededBy moved to dependencies table
	// per Decision 004 (Edge Schema Consolidation). Use dependency API instead.

	// ===== Context Markers =====
	Pinned     bool `json:"pinned,omitempty"`      // Persistent context marker, not a work item
	IsTemplate bool `json:"is_template,omitempty"` // Read-only template molecule
}

// IssueCoord is the bonding, gate, and molecule-type group of an Issue.
type IssueCoord struct {
	// ===== Bonding Fields (compound molecule lineage) =====
	BondedFrom []BondRef `json:"bonded_from,omitempty"` // For compounds: constituent protos

	// ===== Gate Fields (async coordination primitives) =====
	AwaitType string        `json:"await_type,omitempty"` // Condition type: gh:run, gh:pr, timer, human, mail
	AwaitID   string        `json:"await_id,omitempty"`   // Condition identifier (run ID, PR number, etc.)
	Timeout   time.Duration `json:"timeout,omitempty"`    // Max wait time before escalation
	Waiters   []string      `json:"waiters,omitempty"`    // Mail addresses to notify when gate clears

	// ===== Source Tracing Fields (formula cooking origin) =====
	SourceFormula  string `json:"source_formula,omitempty"`  // Formula name where step was defined
	SourceLocation string `json:"source_location,omitempty"` // Path: "steps[0]", "advice[0].after"

	// ===== Molecule Type Fields (swarm coordination) =====
	MolType MolType `json:"mol_type,omitempty"` // Molecule type: swarm|patrol|work (empty = work)

	// ===== Work Type Fields (assignment model - Decision 006) =====
	WorkType WorkType `json:"work_type,omitempty"` // Work type: mutex|open_competition (empty = mutex)
}

// IssueEvent is the event-record and lite-hydration group of an Issue.
type IssueEvent struct {
	// ===== Event Fields (operational state changes) =====
	EventKind string `json:"event_kind,omitempty"` // Namespaced event type: patrol.muted, agent.started
	Actor     string `json:"actor,omitempty"`      // Entity URI who caused this event
	Target    string `json:"target,omitempty"`     // Entity URI or bead ID affected
	Payload   string `json:"payload,omitempty"`    // Event-specific JSON data

	// ===== Internal Hydration Flags (not serialized) =====
	// IsLitePartial is set to true when this Issue was produced by a lite SELECT
	// (see issueops.ScanIssueLiteFrom). When true, the heavy text columns
	// (Description, Design, AcceptanceCriteria, Notes, Payload, Waiters) were not
	// hydrated and remain zero-valued. Callers that need the full body must call
	// store.GetIssue(ctx, id) to refetch. Internal-only — never on the wire.
	IsLitePartial bool `json:"-"`
}
