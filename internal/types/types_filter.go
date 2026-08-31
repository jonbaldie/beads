// Package types defines core data structures for the bd issue tracker.
package types

import (
	"encoding/json"
	"time"
)

// IssueFilter is used to filter issue queries.
// Nested anonymous groups keep the field count under messgo TooManyFields
// while preserving promoted access (filter.Status, filter.Limit, ...).
type IssueFilter struct {
	IssueFilterCore
	IssueFilterMatch
	IssueFilterFlags
	IssueFilterHydrate
	IssueFilterPage
}

// SortPolicy determines how ready work is ordered
type SortPolicy string

// Sort policy constants
const (
	// SortPolicyHybrid prioritizes recent issues by priority, older by age
	// Recent = created within 48 hours
	// This is the default for backwards compatibility
	SortPolicyHybrid SortPolicy = "hybrid"

	// SortPolicyPriority always sorts by priority first, then creation date
	// Use for autonomous execution, CI/CD, priority-driven workflows
	SortPolicyPriority SortPolicy = "priority"

	// SortPolicyOldest always sorts by creation date (oldest first)
	// Use for backlog clearing, preventing issue starvation
	SortPolicyOldest SortPolicy = "oldest"
)

// IsValid checks if the sort policy value is valid
func (s SortPolicy) IsValid() bool {
	switch s {
	case SortPolicyHybrid, SortPolicyPriority, SortPolicyOldest, "":
		return true
	}
	return false
}

// ReclaimedLease names an issue whose stale lease was reverted to ready by
// bd reclaim, together with the owner the lease was taken from. Returned so
// callers (the CLI, a supervisor) can report which dead workers' work was
// recovered.
type ReclaimedLease struct {
	ID            string `json:"id"`
	PreviousOwner string `json:"previous_owner"`
}

// ReclaimFilter scopes which stale-lease issues bd reclaim may revert. The
// zero value reclaims every stale lease (the historical global behavior);
// every populated field narrows the set further (fields AND-combine).
//
// Scoping matters on a federated deployment: each replica's view of another
// machine's liveness is stale by up to one sync interval, so an unscoped
// reaper can revert a unit that is very much alive on the machine that granted
// its lease. Partitioning reclaim by the same label surface the claim side is
// partitioned by (--label/--label-any/--exclude-label, plus --assignee/--id)
// turns after-the-fact revert auditing into prevention.
type ReclaimFilter struct {
	IDs           []string // Only these issue IDs are eligible
	Assignees     []string // Only leases held by one of these owners
	Labels        []string // AND semantics: issue must have ALL these labels
	LabelsAny     []string // OR semantics: issue must have AT LEAST ONE of these labels
	ExcludeLabels []string // Exclusion: issue must NOT have ANY of these labels

	// AnyReplica is the one field here that WIDENS rather than narrows: it
	// disarms the granting-replica guard, letting this reaper revert a lease
	// another replica granted (see issueops.ReclaimExpiredLeasesInTx). It is
	// an operator escape hatch for a permanently-departed replica or a
	// renamed node, never a normal setting — the machine that granted a lease
	// is the only one with a first-hand view of whether its holder is alive.
	// It does NOT widen past staleness or past the scope fields above.
	AnyReplica bool
}

// IsEmpty reports whether the filter constrains nothing, i.e. reclaim runs in
// its global, unscoped form. AnyReplica is deliberately excluded: it is an
// override, not a scope, so a reaper reporting "scoped" still means "narrowed
// to a partition".
func (f ReclaimFilter) IsEmpty() bool {
	return len(f.IDs) == 0 && len(f.Assignees) == 0 &&
		len(f.Labels) == 0 && len(f.LabelsAny) == 0 && len(f.ExcludeLabels) == 0
}

// WorkFilter is used to filter ready work queries.
// Nested anonymous groups keep the field count under messgo TooManyFields
// while preserving promoted access (filter.Status, filter.Limit, ...).
type WorkFilter struct {
	WorkFilterCore
	WorkFilterExtra
}

// StaleFilter is used to filter stale issue queries
type StaleFilter struct {
	Days   int    // Issues not updated in this many days
	Status string // Filter by status (open|in_progress|blocked), empty = all non-closed
	Limit  int    // Maximum issues to return
}

// WispFilter is used to filter ListWisps queries.
// All fields are optional (zero value = no filter).
// ListWisps always restricts to ephemeral issues (Ephemeral=true).
type WispFilter struct {
	// Type filters by issue type (e.g., "agent", "task", "patrol").
	// nil = any type.
	Type *IssueType

	// Status filters by issue status.
	// nil = non-closed only (open, in_progress, blocked).
	Status *Status

	// UpdatedAfter excludes wisps last updated before this time.
	// Use this for age-based filtering (e.g., only wisps updated in the last hour).
	UpdatedAfter *time.Time

	// UpdatedBefore excludes wisps last updated after this time.
	// Use this for staleness detection.
	UpdatedBefore *time.Time

	// IncludeClosed includes closed wisps in the results.
	// When true and Status is nil, all statuses are returned.
	IncludeClosed bool

	// Limit caps the number of results returned (0 = no limit).
	Limit int
}

// EpicStatus represents an epic with its completion status
type EpicStatus struct {
	Epic             *Issue `json:"epic"`
	TotalChildren    int    `json:"total_children"`
	ClosedChildren   int    `json:"closed_children"`
	EligibleForClose bool   `json:"eligible_for_close"`
}

// BondRef tracks compound molecule lineage.
// When protos or molecules are bonded together, BondRefs record
// which sources were combined and how they were attached.
type BondRef struct {
	SourceID  string `json:"source_id"`            // Source proto or molecule ID
	BondType  string `json:"bond_type"`            // sequential, parallel, conditional
	BondPoint string `json:"bond_point,omitempty"` // Attachment site (issue ID or empty for root)
}

// UnmarshalJSON handles backward compatibility for BondRef.
// Pre-v0.63 used "proto_id" instead of "source_id".
func (b *BondRef) UnmarshalJSON(data []byte) error {
	type bondAlias BondRef // avoid recursion
	var raw struct {
		bondAlias
		ProtoID string `json:"proto_id"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*b = BondRef(raw.bondAlias)
	if b.SourceID == "" && raw.ProtoID != "" {
		b.SourceID = raw.ProtoID
	}
	return nil
}

// Bond type constants for compound molecules
const (
	BondTypeSequential  = "sequential"  // B runs after A completes
	BondTypeParallel    = "parallel"    // B runs alongside A
	BondTypeConditional = "conditional" // B runs only if A fails
	BondTypeRoot        = "root"        // Marks the primary/root component
)

// ID prefix constants for molecule/wisp instantiation.
// These prefixes are inserted into issue IDs: <project>-<prefix>-<id>
// Used by: cmd/bd/pour.go, cmd/bd/wisp.go (ID generation)
const (
	IDPrefixMol  = "mol"  // Persistent molecules (bd-mol-xxx)
	IDPrefixWisp = "wisp" // Ephemeral wisps (bd-wisp-xxx)
)

// IsCompound returns true if this issue is a compound (bonded from multiple sources).
func (i *Issue) IsCompound() bool {
	return len(i.BondedFrom) > 0
}

// GetConstituents returns the BondRefs for this compound's constituent protos.
// Returns nil for non-compound issues.
func (i *Issue) GetConstituents() []BondRef {
	return i.BondedFrom
}
