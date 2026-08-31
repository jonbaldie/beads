package types

import "time"

// IssueFilter field groups keep IssueFilter under TooManyFields while
// preserving promoted field access (filter.Status, filter.Limit, ...).

// IssueFilterCore is the identity, label, and limit group of an IssueFilter.
type IssueFilterCore struct {
	Status        *Status
	Statuses      []Status // Multiple status OR filter (from comma-separated --status)
	Priority      *int
	IssueType     *IssueType
	Assignee      *string
	Labels        []string // AND semantics: issue must have ALL these labels
	LabelsAny     []string // OR semantics: issue must have AT LEAST ONE of these labels
	ExcludeLabels []string // Exclusion: issue must NOT have ANY of these labels
	LabelPattern  string   // Glob pattern for label matching (e.g., "tech-*")
	LabelRegex    string   // Regex pattern for label matching (e.g., "tech-(debt|legacy)")
	TitleSearch   string
	IDs           []string // Filter by specific issue IDs
	IDPrefix      string   // Filter by ID prefix (e.g., "bd-" to match "bd-abc123")
	SpecIDPrefix  string   // Filter by spec_id prefix
	Limit         int
}

// IssueFilterMatch is the text-search, date-range, and keyset group of an IssueFilter.
type IssueFilterMatch struct {
	// Pattern matching
	TitleContains       string
	DescriptionContains string
	NotesContains       string
	ExternalRefContains string
	ExternalRef         *string // exact match on external_ref

	// Date ranges
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
	UpdatedAfter  *time.Time
	UpdatedBefore *time.Time
	ClosedAfter   *time.Time
	ClosedBefore  *time.Time
	StartedAfter  *time.Time
	StartedBefore *time.Time

	// Keyset pagination over the (created_at DESC, id ASC) total order.
	//
	// When AfterCreatedAt != nil the query is restricted to rows strictly after
	// the keyset position (AfterCreatedAt, AfterID) under that order — i.e.
	// (created_at < AfterCreatedAt) OR (created_at = AfterCreatedAt AND id > AfterID).
	// id is the primary key, so the tie-break is total: a same-second group
	// larger than one page still pages completely with no dropped or duplicated
	// row (unlike a created_at-only cursor, which loses same-second overflow).
	// AfterID is meaningful only when AfterCreatedAt is set; "" starts the
	// same-second group from its first id.
	//
	// This composes with every other filter (including CreatedBefore, which it
	// does not replace). Pair it with SortBy="created", SortDesc=false so the
	// ORDER BY is created_at DESC, id ASC — the order the predicate assumes.
	AfterCreatedAt *time.Time
	AfterID        string
}

// IssueFilterFlags is the emptiness, range, and classification group of an IssueFilter.
type IssueFilterFlags struct {
	// Empty/null checks
	EmptyDescription bool
	NoAssignee       bool
	NoLabels         bool

	// Numeric ranges
	PriorityMin *int
	PriorityMax *int

	// Source repo filtering (for multi-repo support)
	SourceRepo *string // Filter by source_repo field (nil = any)

	// Ephemeral filtering
	Ephemeral *bool // Filter by ephemeral flag (nil = any, true = only ephemeral, false = only persistent)

	// Pinned filtering
	Pinned *bool // Filter by pinned flag (nil = any, true = only pinned, false = only non-pinned)

	// Blocked filtering: the denormalized, transitive is_blocked column (direct ∨
	// inherited parent-child ∨ waits-for gate), maintained by the write paths and
	// index-backed by idx_issues_is_blocked(is_blocked, status). The projection
	// column alone is not a filter; this optional predicate makes it one.
	IsBlocked *bool // nil = any, true = only is_blocked, false = only unblocked

	// Template filtering
	IsTemplate *bool // Filter by template flag (nil = any, true = only templates, false = exclude templates)

	// Parent filtering: filter children by parent issue ID
	ParentID *string // Filter by parent issue (via parent-child dependency)
	NoParent bool    // Exclude issues that are children of another issue

	// Molecule type filtering
	MolType *MolType // Filter by molecule type (nil = any, swarm/patrol/work)

	// Wisp type filtering (TTL-based compaction classification)
	WispType *WispType // Filter by wisp type (nil = any, heartbeat/ping/patrol/gc_report/recovery/error/escalation)

	// Status exclusion (for default non-closed behavior)
	ExcludeStatus []Status // Exclude issues with these statuses
}

// IssueFilterHydrate is the schedule, metadata, and hydration group of an IssueFilter.
type IssueFilterHydrate struct {
	// Type exclusion (for hiding internal types like gates)
	ExcludeTypes []IssueType // Exclude issues with these types

	// Time-based scheduling filters (GH#820)
	Deferred    bool       // Filter issues that are scheduled later: defer_until set OR status is deferred
	DeferAfter  *time.Time // Filter issues with defer_until > this time
	DeferBefore *time.Time // Filter issues with defer_until < this time
	DueAfter    *time.Time // Filter issues with due_at > this time
	DueBefore   *time.Time // Filter issues with due_at < this time
	Overdue     bool       // Filter issues where due_at < now AND status != closed

	// Metadata field filtering (GH#1406)
	MetadataFields map[string]string // Top-level key=value equality; AND semantics (all must match)
	HasMetadataKey string            // Existence check: issue has this top-level key set (non-null)

	// Hydration options — control which relational data is populated on returned issues.
	// Labels are always hydrated. Dependencies are not by default (for performance).
	IncludeDependencies bool // When true, populate Issue.Dependencies with []*Dependency records

	// SkipLabels suppresses label hydration. When true, the labels JOIN is
	// skipped and Issue.Labels is left nil (callers MUST treat as empty).
	// Opt-in performance flag for the bd list --skip-labels code path.
	SkipLabels bool

	// SkipCounts suppresses cardinality hydration on the counts mega-query.
	// When true the three aggregate joins behind DependencyCount,
	// DependentCount and CommentCount are dropped and all three come back 0,
	// which callers MUST read as unknown rather than as none. The rows, their
	// order, Parent and Dependencies are unaffected. It is the counts-side
	// twin of SkipLabels and is ignored by the paths that project no counts
	// (SearchIssues, GetReadyWork).
	SkipCounts bool

	// Performance escape hatches
	SkipWisps  bool // Q2: skip wisps table merge entirely (for callers that never return ephemeral results)
	NoIDShrink bool // Q3: force Pattern A (full 47-col scan) even when Limit > 0

	Offset int
}

// IssueFilterPage is the sort and row-cap group of an IssueFilter.
type IssueFilterPage struct {
	SortBy   string
	SortDesc bool

	// MaxRows is a defensive cap on the number of rows a search may return.
	// 0 (the default) disables the cap. When >0, the storage layer issues
	// LIMIT MaxRows+1 (to detect overage) and returns *issueops.ErrTooManyRows
	// if the scan yielded more than MaxRows rows. MaxRows is independent of
	// Limit: Limit=0 still means "unlimited" at the contract level; MaxRows is
	// a safety knob layered on top. When both are set, the effective SQL LIMIT
	// is min(Limit, MaxRows+1). Library users may set MaxRows directly; the
	// CLI layer resolves it from --max-rows / BEADS_MAX_ROWS.
	MaxRows int

	// MaxRowsSource attributes which knob set MaxRows, used in error messages.
	// Expected values: "--max-rows", "BEADS_MAX_ROWS", or "" (library users
	// who set MaxRows directly without source attribution).
	MaxRowsSource string

	// Lite, when true, switches the SELECT shape to issueops.IssueSelectColumnsLite,
	// which omits heavy TEXT columns (description, design, acceptance_criteria, notes,
	// payload, waiters). Returned issues carry IsLitePartial=true; their heavy fields
	// are zero-valued. WHERE-clause filters that reference heavy columns
	// (DescriptionContains, NotesContains, EmptyDescription) keep working — they
	// reference columns in WHERE regardless of SELECT shape. Default false preserves
	// today's behavior at every call site.
	//
	// Backend coverage: honored on BOTH stacks for the COUNTED page, which is
	// every read that returns IssueWithCounts — issueops.Reader.List on either
	// implementation, and so `bd list --json` on both routes and
	// GET /v0/beads/issues. It rides the counts mega-query as
	// sqlbuild.CountsHydration.Lite, which both seams derive from this field
	// through their hydrationFor helper.
	//
	// The UNCOUNTED search is store-backed only: SearchIssuesInTx selects
	// issueLiteProjection from this field, and the domain/db SearchIssues has
	// no equivalent, so a caller on that path gets correct rows fully hydrated
	// rather than an error. That path serves the text renderings, which print
	// no body, so the gap costs bytes off the wire and no correctness.
	// See engdocs/EXTENDING.md.
	Lite bool
}

// WorkFilterCore is the status, label, and parent group of a WorkFilter.
type WorkFilterCore struct {
	Status Status
	// Statuses filters to any of the given statuses (OR semantics) in a
	// single query, so multi-status callers avoid one GetReadyWork round
	// trip per status. Ignored when Status is set; when both are empty the
	// legacy default of ('open', 'in_progress') applies.
	Statuses      []Status
	Type          string // Filter by issue type (task, bug, feature, epic, merge-request, etc.)
	Priority      *int
	Assignee      *string
	Unassigned    bool     // Filter for issues with no assignee
	Labels        []string // AND semantics: issue must have ALL these labels
	LabelsAny     []string // OR semantics: issue must have AT LEAST ONE of these labels
	ExcludeLabels []string // Exclusion: issue must NOT have ANY of these labels
	LabelPattern  string   // Glob pattern for label matching (e.g., "tech-*")
	LabelRegex    string   // Regex pattern for label matching (e.g., "tech-(debt|legacy)")
	Limit         int
	SortPolicy    SortPolicy

	// Parent filtering: filter to descendants of a bead/epic (recursive)
	ParentID *string // Show all descendants of this issue

	// Molecule filtering: filter to direct children of this molecule
	MoleculeID string // If set, only return issues that are children of this molecule
}

// WorkFilterExtra is the molecule, deferral, and paging group of a WorkFilter.
type WorkFilterExtra struct {
	// Molecule type filtering
	MolType *MolType // Filter by molecule type (nil = any, swarm/patrol/work)

	// Wisp type filtering (TTL-based compaction classification)
	WispType *WispType // Filter by wisp type (nil = any, heartbeat/ping/patrol/gc_report/recovery/error/escalation)

	// Time-based deferral filtering (GH#820)
	IncludeDeferred bool // If true, include issues with future defer_until timestamps

	// Ephemeral issue filtering
	// By default, GetReadyWork excludes ephemeral wisps but includes
	// no-history wisps because they are durable work items without Dolt history.
	// Set to true to include ephemeral wisps too (e.g., for merge-request processing).
	IncludeEphemeral bool

	// Type exclusion: exclude issues with these types from results.
	// Appended to the default exclusion list (merge-request, gate, molecule, etc.).
	// When Type is set, ExcludeTypes is ignored (explicit type inclusion wins).
	ExcludeTypes []IssueType

	// Metadata field filtering (GH#1406)
	MetadataFields map[string]string // Top-level key=value equality; AND semantics (all must match)
	HasMetadataKey string            // Existence check: issue has this top-level key set (non-null)

	Offset int

	// MaxRows enforces a hard upper bound on the row count returned. Mirrors
	// IssueFilter.MaxRows so bd ready honors --max-rows / BEADS_MAX_ROWS
	// symmetrically with bd list. 0 (the default) disables the cap.
	MaxRows int

	// MaxRowsSource attributes which knob set MaxRows. Expected values:
	// "--max-rows", "BEADS_MAX_ROWS", or "" (library users with no source).
	MaxRowsSource string

	// Lite mirrors IssueFilter.Lite for ready work: the heavy TEXT columns
	// (description, design, acceptance_criteria, notes, payload, waiters) are
	// not selected, and the returned issues carry IsLitePartial=true with those
	// fields zero-valued. It bounds the SIZE of a row, never which rows match:
	// a predicate that reads a heavy column keeps working, because WHERE is
	// independent of the SELECT shape.
	//
	// Unlike IssueFilter.Lite it is honored on BOTH backends, through the
	// counts mega-query's CountsHydration. The two knobs beside it there
	// (SkipLabels, SkipCounts) have no WorkFilter counterpart on purpose; see
	// issueops.readyHydrationFor.
	Lite bool

	// LeavesOnly excludes issues that still have open children (parent-child
	// edges or dotted descendants). Used by `bd ready --claim` so claim takes
	// a leaf like ep-7j4.2 instead of the parent epic. Listing `bd ready`
	// leaves this false so molecule roots still appear as ready work.
	LeavesOnly bool
}
