package issueops

import (
	"github.com/jonbaldie/beads/internal/types"
)

// IssueWithCounts is one row of a work page: the issue plus its relationship
// cardinalities.
type IssueWithCounts = types.IssueWithCounts

// IssueDetails is one issue with its labels, edges and cardinalities.
type IssueDetails = types.IssueDetails

// MolType classifies a molecule.
type MolType = types.MolType

// WispType classifies an ephemeral record.
type WispType = types.WispType

// ReadyRequest describes one ready-work query.
//
// It is a HIGH-LEVEL request, not a filter: normalization, alias expansion,
// validation and defaulting all happen inside the implementation. A caller
// that wants ready work says what it wants, never how the query is shaped —
// which is the whole reason two front doors cannot answer this question
// differently.
type ReadyRequest struct {
	// IssueType restricts the type. Only shorthand alias expansion is applied
	// (mr, feat, mol, enhancement, dec, adr); an unrecognized type matches
	// nothing rather than failing. Setting it drops the default type
	// exclusions, ExcludeTypes included.
	IssueType string
	ReadyRequestFilters

	// Labels must ALL be present; LabelsAny requires at least one;
	// ExcludeLabels must be absent. All three are raw: normalization happens
	// inside.
	Labels    []string
	LabelsAny []string

	// Priority is an exact priority. It is a pointer because 0 is a real
	// priority: a value-plus-flag pair would let one half be filled in without
	// the other, and P0 has already been lost that way once.
	Priority *int

	// ParentID restricts to recursive descendants of one issue.
	ParentID string

	// IncludeDeferred admits rows whose defer_until is still in the future;
	// IncludeEphemeral admits wisp-plane rows.
	IncludeDeferred  bool
	IncludeEphemeral bool

	// Sort is the ready ordering: hybrid, priority or oldest. Empty resolves to
	// hybrid at the storage layer, which no front door should rely on — both
	// surfaces send a concrete policy, because hybrid demotes older
	// high-priority work and therefore changes the item SET once Limit
	// truncates.
	Sort string

	// Limit bounds the page. Nil means the shared ready default; 0 means
	// unlimited. It is a pointer so that "unset" and "explicitly unlimited"
	// stay distinguishable, which is what lets one constant serve both
	// surfaces.
	Limit *int
	// Offset skips the first N matching rows, on EVERY implementation. The
	// page a caller receives is the rows at positions [Offset, Offset+Limit)
	// of the answer the same request returns unpaged, in the same order.
	//
	// WHERE the skip happens is not a backend's choice either. One seam
	// renders OFFSET and one renders LIMIT without it, so both bodies reach
	// past the skipped rows and drop them in the shared page epilogue
	// (internal/workapi.FinishPageAt) — which is also the only sequence that
	// is right for a sort SQL cannot express, since that order first exists
	// after the fetch.
	//
	// An Offset past the end of the result set is an empty page and a nil
	// error, not a failure: a pager that walks off the end has its answer.
	//
	// A ready request carries no keyset position, so there is no portable way
	// to page ready work. A caller that must page across backends pages a
	// ListRequest instead — see ListRequest.Offset.
	Offset int

	// Brief drops the free-form text from every row: Description, Design,
	// AcceptanceCriteria, Notes, Payload and Waiters come back zero-valued and
	// the row carries types.Issue.IsLitePartial. See ListRequest.Brief, which
	// is the same knob on the other operation and carries the full contract.
	Brief bool
}

// ReadyRequestFilters keeps the optional narrowing predicates together while
// the anonymous embedding preserves ReadyRequest's flat selectors and JSON
// representation.
type ReadyRequestFilters struct {
	// Assignee restricts to one actor. Unassigned wins over a stale Assignee.
	Assignee string
	// Unassigned restricts to rows with no assignee.
	Unassigned bool

	ExcludeLabels []string
	// LabelPattern is a glob and LabelRegex a regular expression, both matched
	// against labels.
	LabelPattern string
	LabelRegex   string

	// Priority is an exact priority. It is a pointer because 0 is a real
	// priority: a value-plus-flag pair would let one half be filled in without
	// the other, and P0 has already been lost that way once.
	Priority *int

	// ParentID restricts to recursive descendants of one issue.
	ParentID string
	// MolType restricts to one molecule type.
	MolType *MolType

	// IncludeDeferred admits rows whose defer_until is still in the future;
	// IncludeEphemeral admits wisp-plane rows.
	IncludeDeferred  bool
	IncludeEphemeral bool

	// ExcludeTypes names types to exclude. Entries may be comma-separated;
	// splitting and alias expansion happen inside. Ignored when IssueType is
	// set.
	ExcludeTypes []string

	// MetadataFields is a top-level metadata equality filter and
	// HasMetadataKey a top-level key-presence filter. Keys are validated
	// inside.
	MetadataFields map[string]string
	HasMetadataKey string
}
