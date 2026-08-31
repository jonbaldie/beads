// Package types defines core data structures for the bd issue tracker.
package types

import (
	"time"
)

// Dependency represents a relationship between issues
type Dependency struct {
	// ID is the dependency row's deterministic surrogate primary key,
	// depid.New(issue_id, target) — a UUIDv5 that is stable and globally unique
	// across the durable and wisp dependency tables. Populated only by reads
	// that select it (e.g. GetDependentRecords, which keysets on it); left empty
	// by the source-keyed reads that never needed it.
	ID          string         `json:"id,omitempty"`
	IssueID     string         `json:"issue_id"`
	DependsOnID string         `json:"depends_on_id"`
	Type        DependencyType `json:"type"`
	CreatedAt   time.Time      `json:"created_at"`
	CreatedBy   string         `json:"created_by,omitempty"`
	// Metadata contains type-specific edge data (JSON blob)
	// Examples: similarity scores, approval details, skill proficiency
	Metadata string `json:"metadata,omitempty"`
	// ThreadID groups conversation edges for efficient thread queries
	// For replies-to edges, this identifies the conversation root
	ThreadID string `json:"thread_id,omitempty"`
}

// DependencyCounts holds counts for dependencies and dependents
type DependencyCounts struct {
	DependencyCount int `json:"dependency_count"` // Number of issues this issue depends on
	DependentCount  int `json:"dependent_count"`  // Number of issues that depend on this issue
}

// IssueWithDependencyMetadata extends Issue with dependency relationship type
// Note: We explicitly include all Issue fields to ensure proper JSON marshaling
type IssueWithDependencyMetadata struct {
	Issue
	DependencyType DependencyType `json:"dependency_type"`
}

// IssueWithCounts extends Issue with dependency relationship counts
type IssueWithCounts struct {
	*Issue
	DependencyCount int     `json:"dependency_count"`
	DependentCount  int     `json:"dependent_count"`
	CommentCount    int     `json:"comment_count"`
	Parent          *string `json:"parent,omitempty"` // Computed parent from parent-child dep (bd-ym8c)
}

// IssueDetails extends Issue with labels, dependencies, dependents, and comments.
// Used for JSON serialization in bd show and RPC responses.
type IssueDetails struct {
	Issue
	Labels       []string                       `json:"labels,omitempty"`
	Dependencies []*IssueWithDependencyMetadata `json:"dependencies,omitempty"`
	Dependents   []*IssueWithDependencyMetadata `json:"dependents,omitempty"`
	Comments     []*Comment                     `json:"comments,omitempty"`
	Parent       *string                        `json:"parent,omitempty"`

	// Cardinality fields — emitted by default (count-only mode).
	// Slice fields (Dependents, Comments) are nil when count-only is active.
	// Use --include-dependents / --include-comments to populate the slices.
	DependentCount  *int64 `json:"dependent_count,omitempty"`
	DependencyCount *int64 `json:"dependency_count,omitempty"`
	CommentCount    *int64 `json:"comment_count,omitempty"`

	// CommentsOmitted is set true only when CommentCount is nonzero AND
	// Comments was left nil (count-only mode, no --include-comments). Without
	// it, a positive CommentCount with no Comments key reads as a client bug,
	// and a caller checking only `.comments` sees `null`/absent and concludes
	// "no comments" — both wrong (ga-clgh). Never set alongside a populated
	// Comments slice or a zero count: a true empty stays plain omission.
	CommentsOmitted *bool `json:"comments_omitted,omitempty"`

	// Epic progress fields (populated only for issue_type=epic with children)
	EpicTotalChildren  *int  `json:"epic_total_children,omitempty"`
	EpicClosedChildren *int  `json:"epic_closed_children,omitempty"`
	EpicCloseable      *bool `json:"epic_closeable,omitempty"`

	// Revision is the detail view's projection of the embedded Issue's
	// RowVersion under a storage-neutral wire name, and it is the ONE place
	// the token is published to a client. Read RowVersion's doc for what the
	// token means: it is EQUALITY-ONLY, its coverage is deliberately PARTIAL,
	// and 0 is a real value rather than an absence.
	//
	// It exists as a projected field rather than a re-tagged RowVersion
	// because RowVersion is json:"-" for a reason that still holds — row_lock
	// is random per write, so serializing it generically would break stable
	// list and export round-trips — and the detail view is the one shape that
	// neither lists nor interchanges. NewIssueDetails is the only door: a
	// literal with an unset Revision serializes a 0 that is indistinguishable
	// from a legacy migration-0054 row, so the projection lives beside the
	// field and not at each caller.
	//
	// NO omitempty. A guarded write that expects 0 matches an un-mutated
	// legacy row and misses any current one, which is correct CAS; omitting
	// the member would leave that client unable to read the value it must
	// send, and would make an absent field mean either "legacy-zero" or "this
	// producer has no token".
	Revision int64 `json:"revision"`
}

// NewIssueDetails starts a detail view of issue with the wire-visible revision
// token projected off the row.
//
// It is the only constructor: the token is a projection, not an independent
// field, and 0 is a legal token value, so a detail view assembled by struct
// literal would publish a silently wrong token that nothing can distinguish
// from a right one. The caller fills in labels, edges and counts afterwards.
func NewIssueDetails(issue Issue) *IssueDetails {
	return &IssueDetails{Issue: issue, Revision: issue.RowVersion}
}
