// Package types defines core data structures for the bd issue tracker.
package types

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// NewGraphEdgeDependency builds the dependency record for a graph plan edge,
// shared by the embedded and domain apply paths so they cannot drift. Every
// waits-for edge gets gate metadata (empty gate defaults to all-children):
// stored rows stay self-describing rather than depending on every reader
// defaulting a missing gate (the runtime SQL predicate COALESCEs to
// all-children, but readers before migration 0059 did not, so '{}' or empty
// metadata must never be stored for graph-created waits-for dependencies).
// A plan-local spawnerKey resolves through keyToID; the spawner is recorded
// for compatibility only — gate evaluation reads the spawner from
// dependencies.depends_on_id (see ParseWaitsForGateMetadata).
func NewGraphEdgeDependency(fromID, toID string, depType DependencyType, gate, spawnerKey, spawnerID, threadID string, keyToID map[string]string) (*Dependency, error) {
	dep := &Dependency{
		IssueID:     fromID,
		DependsOnID: toID,
		Type:        depType,
		ThreadID:    threadID,
	}
	if depType == DepWaitsFor {
		if spawnerKey != "" {
			resolved, ok := keyToID[spawnerKey]
			if !ok {
				return nil, fmt.Errorf("serializing waits-for metadata: unresolved spawner key %q", spawnerKey)
			}
			spawnerID = resolved
		}
		if gate == "" {
			gate = WaitsForAllChildren
		}
		raw, err := json.Marshal(WaitsForMeta{Gate: gate, SpawnerID: spawnerID})
		if err != nil {
			return nil, fmt.Errorf("serializing waits-for metadata: %w", err)
		}
		dep.Metadata = string(raw)
	}
	return dep, nil
}

// NewWaitsForDependency builds the waits-for dependency record for a single
// issue outside a graph plan: the spawner is the depends_on target and the
// metadata carries the gate (defaulted to all-children). Shares
// NewGraphEdgeDependency so single-issue and graph-created waits-for rows
// cannot drift.
func NewWaitsForDependency(issueID, spawnerID, gate string) (*Dependency, error) {
	return NewGraphEdgeDependency(issueID, spawnerID, DepWaitsFor, gate, "", "", "", nil)
}

// NewWaitsForBlockingDependency builds a waits-for dependency that also
// carries classic blocking semantics (GH#3783): set also_blocks in the
// metadata so waitsForGateBlockedSQL additionally blocks while the spawner
// itself is open, not only while it has an open parent-child child. Use this
// instead of NewWaitsForDependency exactly when the caller is collapsing a
// would-be DepBlocks edge (from needs/depends_on) into this waits-for edge
// because the two would otherwise collide on the same (source, target) pair
// — never for a plain waits_for with no matching needs/depends_on entry.
func NewWaitsForBlockingDependency(issueID, spawnerID, gate string) (*Dependency, error) {
	dep, err := NewWaitsForDependency(issueID, spawnerID, gate)
	if err != nil {
		return nil, err
	}
	var meta WaitsForMeta
	if err := json.Unmarshal([]byte(dep.Metadata), &meta); err != nil {
		return nil, fmt.Errorf("parsing waits-for metadata to set also_blocks: %w", err)
	}
	meta.AlsoBlocks = true
	raw, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("serializing waits-for also_blocks metadata: %w", err)
	}
	dep.Metadata = string(raw)
	return dep, nil
}

// NewGraphNodeDependency builds the dependency record for a graph-plan node's
// inline dep, shared by the embedded and domain apply paths so their
// resolution semantics cannot drift: an empty type defaults to blocks, and
// the target resolves as a plan-local key first, then as a literal issue ID.
// Waits-for deps carry gate metadata like waits-for edges (all-children
// default, no explicit spawner).
func NewGraphNodeDependency(issueID string, depType DependencyType, target string, keyToID map[string]string) (*Dependency, error) {
	if depType == "" {
		depType = DepBlocks
	}
	targetID := keyToID[target]
	if targetID == "" {
		targetID = target
	}
	if targetID == "" {
		return nil, fmt.Errorf("dep target %q not found", target)
	}
	return NewGraphEdgeDependency(issueID, targetID, depType, "", "", "", "", nil)
}

// MergeMetadataRefs merges resolved metadata_refs into an issue's existing
// metadata JSON: each refs entry maps a metadata key to a plan-local node
// key, which is replaced with its minted ID from keyToID. Shared by the
// embedded and domain graph-apply paths.
func MergeMetadataRefs(existing json.RawMessage, refs map[string]string, keyToID map[string]string) (json.RawMessage, error) {
	merged := make(map[string]json.RawMessage, len(refs))
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &merged); err != nil {
			return nil, fmt.Errorf("re-parsing metadata: %w", err)
		}
	}
	for metaKey, refKey := range refs {
		resolvedID, ok := keyToID[refKey]
		if !ok {
			return nil, fmt.Errorf("metadata_ref %q references unknown key %q", metaKey, refKey)
		}
		idJSON, err := json.Marshal(resolvedID)
		if err != nil {
			return nil, fmt.Errorf("marshaling metadata ref %q: %w", metaKey, err)
		}
		merged[metaKey] = idJSON
	}
	return json.Marshal(merged)
}

// ParseWaitsForGateMetadata extracts the waits-for gate type from dependency metadata.
// Note: spawner identity comes from dependencies.depends_on_id in storage/query paths;
// metadata.spawner_id is parsed for compatibility/future explicit targeting.
// Returns WaitsForAllChildren on empty/invalid metadata for backward compatibility.
func ParseWaitsForGateMetadata(metadata string) string {
	if strings.TrimSpace(metadata) == "" {
		return WaitsForAllChildren
	}

	var meta WaitsForMeta
	if err := json.Unmarshal([]byte(metadata), &meta); err != nil {
		return WaitsForAllChildren
	}
	if meta.Gate == WaitsForAnyChildren {
		return WaitsForAnyChildren
	}
	return WaitsForAllChildren
}

// AttestsMeta holds metadata for attests dependencies (skill attestations).
// Stored as JSON in the Dependency.Metadata field.
// Enables: Entity X attests that Entity Y has skill Z at level N.
type AttestsMeta struct {
	// Skill is the identifier of the skill being attested (e.g., "go", "rust", "code-review")
	Skill string `json:"skill"`
	// Level is the proficiency level (e.g., "beginner", "intermediate", "expert", or numeric 1-5)
	Level string `json:"level"`
	// Date is when the attestation was made (RFC3339 format)
	Date string `json:"date"`
	// Evidence is optional reference to supporting evidence (e.g., issue ID, commit, PR)
	Evidence string `json:"evidence,omitempty"`
	// Notes is optional free-form notes about the attestation
	Notes string `json:"notes,omitempty"`
}

// FailureCloseKeywords are keywords that indicate an issue was closed due to failure.
// Used by conditional-blocks dependencies to determine if the condition is met.
var FailureCloseKeywords = []string{
	"failed",
	"rejected",
	"wontfix",
	"won't fix",
	"canceled",
	"cancelled", //nolint:misspell // British spelling intentionally included
	"abandoned",
	"blocked",
	"error",
	"timeout",
	"aborted",
}

// IsFailureClose returns true if the close reason indicates the issue failed.
// This is used by conditional-blocks dependencies: B runs only if A fails.
// A "failure" close reason contains one of the FailureCloseKeywords (case-insensitive).
func IsFailureClose(closeReason string) bool {
	if closeReason == "" {
		return false
	}
	lower := strings.ToLower(closeReason)
	for _, keyword := range FailureCloseKeywords {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

// Label represents a tag on an issue
type Label struct {
	IssueID string `json:"issue_id"`
	Label   string `json:"label"`
}

// Comment represents a comment on an issue
type Comment struct {
	ID        string    `json:"id"`
	IssueID   string    `json:"issue_id"`
	Author    string    `json:"author"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// UnmarshalJSON handles backward compatibility for Comment.
// Pre-v1.0 exported Comment.ID as int64; current schema uses string.
func (c *Comment) UnmarshalJSON(data []byte) error {
	type commentAlias Comment // avoid recursion
	var raw struct {
		commentAlias
		RawID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*c = Comment(raw.commentAlias)
	if len(raw.RawID) > 0 {
		// try string first, fall back to number
		var s string
		if err := json.Unmarshal(raw.RawID, &s); err == nil {
			c.ID = s
		} else {
			var n json.Number
			if err := json.Unmarshal(raw.RawID, &n); err == nil {
				c.ID = n.String()
			}
		}
	}
	return nil
}

// Event represents an audit trail entry
type Event struct {
	ID        string    `json:"id"`
	IssueID   string    `json:"issue_id"`
	EventType EventType `json:"event_type"`
	Actor     string    `json:"actor"`
	OldValue  *string   `json:"old_value,omitempty"`
	NewValue  *string   `json:"new_value,omitempty"`
	Comment   *string   `json:"comment,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// EventType categorizes audit trail events
type EventType string

// Event type constants for audit trail
const (
	EventCreated           EventType = "created"
	EventUpdated           EventType = "updated"
	EventClaimed           EventType = "claimed"
	EventStatusChanged     EventType = "status_changed"
	EventCommented         EventType = "commented"
	EventClosed            EventType = "closed"
	EventReopened          EventType = "reopened"
	EventDependencyAdded   EventType = "dependency_added"
	EventDependencyRemoved EventType = "dependency_removed"
	EventLabelAdded        EventType = "label_added"
	EventLabelRemoved      EventType = "label_removed"
	EventCompacted         EventType = "compacted"
	// EventLeaseReclaimed records that a stale lease was reverted to ready by
	// bd reclaim (dead-worker recovery). old_value is the previous owner.
	EventLeaseReclaimed EventType = "lease_reclaimed"
)

// ProvenanceEvent is one entry in the append-only provenance log: a typed
// binding from an issue to a structured external artifact (a git SHA, PR,
// work-id, transcript, or branch).
//
// Unlike Event (a field-mutation audit record), a ProvenanceEvent records that
// something happened in the world — a commit landed, a claim was made, work was
// handed off — and ties it to an opaque external Ref. bd never interprets Actor
// or Ref; only Kind and RefKind are structurally validated. This keeps the log
// a primitive usable by any runtime without baking in orchestrator semantics.
//
// OccurredAt (event-time) is distinct from CreatedAt (ingest-time): a producer
// may record a fact after it happened.
type ProvenanceEvent struct {
	ID         string     `json:"id"`
	IssueID    string     `json:"issue_id"`
	Kind       ProvKind   `json:"kind"`
	Actor      *string    `json:"actor,omitempty"`
	Ref        *string    `json:"ref,omitempty"`
	RefKind    *string    `json:"ref_kind,omitempty"`
	Payload    *string    `json:"payload,omitempty"`
	Source     string     `json:"source"`
	OccurredAt *time.Time `json:"occurred_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// ProvKind categorizes a provenance event.
type ProvKind string

// Provenance event kind constants. These are the only structurally-valid kinds;
// the record path rejects anything outside this set.
const (
	ProvCut     ProvKind = "cut"
	ProvClaim   ProvKind = "claim"
	ProvSuspend ProvKind = "suspend"
	ProvResume  ProvKind = "resume"
	ProvHandoff ProvKind = "handoff"
	ProvCommit  ProvKind = "commit"
	ProvLand    ProvKind = "land"
	ProvUsed    ProvKind = "used"
)

// BlockedIssue extends Issue with blocking information
type BlockedIssue struct {
	Issue
	BlockedByCount int      `json:"blocked_by_count"`
	BlockedBy      []string `json:"blocked_by"`
}

// ReadyExplanation provides reasoning for why issues are ready or blocked.
type ReadyExplanation struct {
	Ready   []ReadyItem    `json:"ready"`
	Blocked []BlockedItem  `json:"blocked"`
	Cycles  [][]string     `json:"cycles,omitempty"`
	Summary ExplainSummary `json:"summary"`
}

// ReadyItem explains why a specific issue is ready for work.
type ReadyItem struct {
	*Issue
	Reason           string   `json:"reason"`
	ResolvedBlockers []string `json:"resolved_blockers"`
	DependencyCount  int      `json:"dependency_count"`
	DependentCount   int      `json:"dependent_count"`
	Parent           *string  `json:"parent,omitempty"`
}

// BlockedItem explains why a specific issue is blocked.
type BlockedItem struct {
	Issue
	BlockedBy      []BlockerInfo `json:"blocked_by"`
	BlockedByCount int           `json:"blocked_by_count"`
}

// BlockerInfo provides details about a single blocker.
type BlockerInfo struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   Status `json:"status"`
	Priority int    `json:"priority"`
}

// ExplainSummary provides aggregate statistics.
type ExplainSummary struct {
	TotalReady   int `json:"total_ready"`
	TotalBlocked int `json:"total_blocked"`
	CycleCount   int `json:"cycle_count"`
}

// BuildReadyExplanation constructs a ReadyExplanation from pre-fetched data.
// This pure function is separated from CLI concerns for testability.
func BuildReadyExplanation(
	readyIssues []*Issue,
	blockedIssues []*BlockedIssue,
	depCounts map[string]*DependencyCounts,
	allDeps map[string][]*Dependency,
	blockerMap map[string]*Issue,
	cycles [][]*Issue,
) ReadyExplanation {
	readyItems := buildReadyItems(readyIssues, depCounts, allDeps)
	blockedItems := buildBlockedItems(blockedIssues, blockerMap)
	cycleIDs := buildCycleIDs(cycles)
	return ReadyExplanation{
		Ready:   readyItems,
		Blocked: blockedItems,
		Cycles:  cycleIDs,
		Summary: ExplainSummary{
			TotalReady:   len(readyItems),
			TotalBlocked: len(blockedItems),
			CycleCount:   len(cycleIDs),
		},
	}
}

func buildReadyItems(issues []*Issue, depCounts map[string]*DependencyCounts, allDeps map[string][]*Dependency) []ReadyItem {
	items := make([]ReadyItem, 0, len(issues))
	for _, issue := range issues {
		items = append(items, buildReadyItem(issue, depCounts[issue.ID], allDeps[issue.ID]))
	}
	return items
}

func buildReadyItem(issue *Issue, counts *DependencyCounts, deps []*Dependency) ReadyItem {
	if counts == nil {
		counts = &DependencyCounts{}
	}
	resolvedBlockers, reason := readyDependencyExplanation(deps)
	return ReadyItem{
		Issue:            issue,
		Reason:           reason,
		ResolvedBlockers: resolvedBlockers,
		DependencyCount:  counts.DependencyCount,
		DependentCount:   counts.DependentCount,
		Parent:           readyParent(deps),
	}
}

func readyDependencyExplanation(deps []*Dependency) ([]string, string) {
	var resolvedBlockers []string
	for _, dep := range deps {
		if dep.Type == DepBlocks || dep.Type == DepConditionalBlocks || dep.Type == DepWaitsFor {
			resolvedBlockers = append(resolvedBlockers, dep.DependsOnID)
		}
	}
	if len(resolvedBlockers) == 0 {
		return nil, "no blocking dependencies"
	}
	return resolvedBlockers, fmt.Sprintf("%d blocker(s) resolved", len(resolvedBlockers))
}

func readyParent(deps []*Dependency) *string {
	for _, dep := range deps {
		if dep.Type == DepParentChild {
			return &dep.DependsOnID
		}
	}
	return nil
}

func buildBlockedItems(issues []*BlockedIssue, blockerMap map[string]*Issue) []BlockedItem {
	items := make([]BlockedItem, 0, len(issues))
	for _, blocked := range issues {
		blockers := make([]BlockerInfo, 0, len(blocked.BlockedBy))
		for _, blockerID := range blocked.BlockedBy {
			blockers = append(blockers, blockerInfo(blockerID, blockerMap))
		}
		items = append(items, BlockedItem{
			Issue:          blocked.Issue,
			BlockedBy:      blockers,
			BlockedByCount: blocked.BlockedByCount,
		})
	}
	return items
}

func blockerInfo(id string, blockerMap map[string]*Issue) BlockerInfo {
	info := BlockerInfo{ID: id}
	if blocker, ok := blockerMap[id]; ok {
		info.Title = blocker.Title
		info.Status = blocker.Status
		info.Priority = blocker.Priority
	}
	return info
}

func buildCycleIDs(cycles [][]*Issue) [][]string {
	var cycleIDs [][]string
	for _, cycle := range cycles {
		ids := make([]string, len(cycle))
		for i, issue := range cycle {
			ids[i] = issue.ID
		}
		cycleIDs = append(cycleIDs, ids)
	}
	return cycleIDs
}

// TreeNode represents a node in a dependency tree
type TreeNode struct {
	Issue
	Depth          int            `json:"depth"`
	ParentID       string         `json:"parent_id"`
	EdgeFromParent DependencyType `json:"edge_from_parent,omitempty"`
	Truncated      bool           `json:"truncated"`
}

// MoleculeProgressStats provides efficient progress info for large molecules.
// This uses indexed queries instead of loading all steps into memory.
type MoleculeProgressStats struct {
	MoleculeID    string     `json:"molecule_id"`
	MoleculeTitle string     `json:"molecule_title"`
	Total         int        `json:"total"`           // Total steps (direct children)
	Completed     int        `json:"completed"`       // Closed steps
	InProgress    int        `json:"in_progress"`     // Steps currently in progress
	CurrentStepID string     `json:"current_step_id"` // First in_progress step ID (if any)
	FirstClosed   *time.Time `json:"first_closed,omitempty"`
	LastClosed    *time.Time `json:"last_closed,omitempty"`
}

// MoleculeLastActivity holds the most recent activity timestamp for a molecule.
type MoleculeLastActivity struct {
	MoleculeID   string    `json:"molecule_id"`
	LastActivity time.Time `json:"last_activity"`
	Source       string    `json:"source"` // "step_closed", "step_updated", "molecule_updated"
	SourceStepID string    `json:"source_step_id,omitempty"`
}

// Statistics provides aggregate metrics
type Statistics struct {
	TotalIssues             int     `json:"total_issues"`
	OpenIssues              int     `json:"open_issues"`
	InProgressIssues        int     `json:"in_progress_issues"`
	ClosedIssues            int     `json:"closed_issues"`
	BlockedIssues           *int    `json:"blocked_issues"`  // nil when --no-blocked skips computation
	DeferredIssues          int     `json:"deferred_issues"` // Issues on ice
	ReadyIssues             *int    `json:"ready_issues"`    // nil when --no-blocked skips computation (readiness needs the blocked set)
	PinnedIssues            int     `json:"pinned_issues"`   // Persistent issues
	EpicsEligibleForClosure int     `json:"epics_eligible_for_closure"`
	AverageLeadTime         float64 `json:"average_lead_time_hours"`
}
