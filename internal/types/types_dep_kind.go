// Package types defines core data structures for the bd issue tracker.
package types

// DependencyType categorizes the relationship
type DependencyType string

// Dependency type constants
const (
	// Workflow types (affect ready work calculation)
	DepBlocks            DependencyType = "blocks"
	DepParentChild       DependencyType = "parent-child"
	DepConditionalBlocks DependencyType = "conditional-blocks" // B runs only if A fails
	DepWaitsFor          DependencyType = "waits-for"          // Fanout gate: wait for dynamic children

	// Association types
	DepRelated        DependencyType = "related"
	DepDiscoveredFrom DependencyType = "discovered-from"

	// Graph link types
	DepRepliesTo  DependencyType = "replies-to" // Conversation threading
	DepRelatesTo  DependencyType = "relates-to" // Loose knowledge graph edges
	DepDuplicates DependencyType = "duplicates" // Deduplication link
	DepSupersedes DependencyType = "supersedes" // Version chain link

	// Entity types (HOP foundation - Decision 004)
	DepAuthoredBy DependencyType = "authored-by" // Creator relationship
	DepAssignedTo DependencyType = "assigned-to" // Assignment relationship
	DepApprovedBy DependencyType = "approved-by" // Approval relationship
	DepAttests    DependencyType = "attests"     // Skill attestation: X attests Y has skill Z

	// Convoy tracking (non-blocking cross-project references)
	DepTracks DependencyType = "tracks" // Convoy → issue tracking (non-blocking)

	// Reference types (cross-referencing without blocking)
	DepUntil     DependencyType = "until"     // Active until target closes (e.g., muted until issue resolved)
	DepCausedBy  DependencyType = "caused-by" // Triggered by target (audit trail)
	DepValidates DependencyType = "validates" // Approval/validation relationship

	// Delegation types (work delegation chains)
	DepDelegatedFrom DependencyType = "delegated-from" // Work delegated from parent; completion cascades up
)

// AllDependencyTypes lists the built-in dependency types, in declaration order.
// Single source for the `bd schema` enum so the published schema enumerates
// exactly the types bd recognizes.
var AllDependencyTypes = []DependencyType{
	DepBlocks, DepParentChild, DepConditionalBlocks, DepWaitsFor,
	DepRelated, DepDiscoveredFrom,
	DepRepliesTo, DepRelatesTo, DepDuplicates, DepSupersedes,
	DepAuthoredBy, DepAssignedTo, DepApprovedBy, DepAttests,
	DepTracks,
	DepUntil, DepCausedBy, DepValidates,
	DepDelegatedFrom,
}

// MaxDependencyTypeLen is the widest dependency type either dependency plane
// can store: dependencies.type and wisp_dependencies.type are both VARCHAR(32)
// (migrations 0002_create_dependencies and 0021_create_wisp_auxiliary), and no
// later migration widens either. Validators bound the type here rather than at
// some looser number of their own, so a type that passes validation is a type
// an edge can actually carry — a longer one is refused up front instead of
// becoming a filter that silently matches nothing.
const MaxDependencyTypeLen = 32

// IsValid checks if the dependency type value is valid.
// Accepts any non-empty string that fits the type column (MaxDependencyTypeLen).
// Use IsWellKnown() to check if it's a built-in type.
func (d DependencyType) IsValid() bool {
	return len(d) > 0 && len(d) <= MaxDependencyTypeLen
}

// WellKnownDependencyTypes returns the built-in dependency types accepted by
// user-facing commands that intentionally reject custom dependency types.
func WellKnownDependencyTypes() []DependencyType {
	return []DependencyType{
		DepBlocks, DepParentChild, DepConditionalBlocks, DepWaitsFor, DepRelated, DepDiscoveredFrom,
		DepRepliesTo, DepRelatesTo, DepDuplicates, DepSupersedes,
		DepAuthoredBy, DepAssignedTo, DepApprovedBy, DepAttests, DepTracks,
		DepUntil, DepCausedBy, DepValidates, DepDelegatedFrom,
	}
}

// IsWellKnown checks if the dependency type is a well-known constant.
// Returns false for custom/user-defined types (which are still valid).
func (d DependencyType) IsWellKnown() bool {
	for _, wellKnown := range WellKnownDependencyTypes() {
		if d == wellKnown {
			return true
		}
	}
	return false
}

// AffectsReadyWork returns true if this dependency type blocks work.
// Only blocking types affect the ready work calculation.
func (d DependencyType) AffectsReadyWork() bool {
	return d == DepBlocks || d == DepParentChild || d == DepConditionalBlocks || d == DepWaitsFor
}

// IsBlockingEdge returns true if this dependency type represents a hard blocker.
// Unlike AffectsReadyWork, this excludes parent-child (structural, not blocking).
// Used by dep tree rendering to decide whether the [BLOCKED] badge applies.
func (d DependencyType) IsBlockingEdge() bool {
	return d == DepBlocks || d == DepConditionalBlocks || d == DepWaitsFor
}

// WaitsForMeta holds metadata for waits-for dependencies (fanout gates).
// Stored as JSON in the Dependency.Metadata field.
type WaitsForMeta struct {
	// Gate type: "all-children" (wait for all), "any-children" (wait for first)
	Gate string `json:"gate"`
	// SpawnerID identifies which step/issue spawns the children to wait for.
	// If empty, waits for all direct children of the depends_on_id issue.
	SpawnerID string `json:"spawner_id,omitempty"`
	// AlsoBlocks marks a waits-for edge that was collapsed from a redundant
	// depends_on/needs blocks edge onto the same spawner (GH#3783): the
	// caller skipped emitting a separate DepBlocks edge because it collided
	// with this DepWaitsFor edge, so this edge must additionally carry
	// classic blocking semantics — it blocks while the spawner itself is
	// open, not only while the spawner has an open child. Omitted (and thus
	// COALESCEd to false by readers) for a plain waits_for with no matching
	// needs/depends_on entry, which must retain the original fanout-only
	// semantics.
	AlsoBlocks bool `json:"also_blocks,omitempty"`
}

// WaitsForGate constants
const (
	WaitsForAllChildren = "all-children" // Wait for all dynamic children to complete
	WaitsForAnyChildren = "any-children" // Proceed when first child completes (future)
)

// IsSchedulingEdge reports whether a dependency type belongs to the static
// COMBINED-CYCLE SET: blocks, conditional-blocks and parent-child. It is the
// set every cycle probe and every whole-graph gate walks, and parent-child is
// in it because a blocked parent propagates its blocked state to its children
// in the ready-work computation — so a chain mixing blocks and parent-child
// edges can form a livelock that leaves nothing ready.
//
// WAITS-FOR IS DELIBERATELY OUTSIDE IT. That edge also affects readiness, but
// its gate clears on the spawner's CHILDREN rather than on the spawner, so a
// waits-for edge cannot close a cycle the way a blocking one does.
//
// IT LIVES HERE, next to the Dep* constants themselves, because four packages
// walk this set and no two of them can import each other: internal/storage/
// issueops imports internal/storage/domain, so domain cannot import back, and
// internal/storage/domain/db and internal/storage/uow are third and fourth. Each
// had its own spelling of the same three types, so ADDING a fifth scheduling
// type was four edits with nothing to catch a missed one. It is one edit now.
func IsSchedulingEdge(t DependencyType) bool {
	switch t {
	case DepBlocks, DepConditionalBlocks, DepParentChild:
		return true
	default:
		return false
	}
}

// IsValidWaitsForGate reports whether gate names a known waits-for fanout gate.
func IsValidWaitsForGate(gate string) bool {
	return gate == WaitsForAllChildren || gate == WaitsForAnyChildren
}
