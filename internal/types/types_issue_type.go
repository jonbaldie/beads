// Package types defines core data structures for the bd issue tracker.
package types

import (
	"fmt"
	"slices"
	"strings"
)

// IssueType categorizes the kind of work
type IssueType string

// Core work type constants - these are the built-in types that beads validates.
// All other types require configuration via types.custom in config.yaml.
const (
	TypeBug       IssueType = "bug"
	TypeFeature   IssueType = "feature"
	TypeTask      IssueType = "task"
	TypeEpic      IssueType = "epic"
	TypeChore     IssueType = "chore"
	TypeDecision  IssueType = "decision"
	TypeMessage   IssueType = "message"
	TypeMolecule  IssueType = "molecule"  // Molecule type for swarm coordination (internal use)
	TypeGate      IssueType = "gate"      // Gate type for async coordination (bd gate, formula gates)
	TypeSpike     IssueType = "spike"     // Timeboxed investigation to reduce uncertainty
	TypeStory     IssueType = "story"     // User story describing a feature from the user's perspective
	TypeMilestone IssueType = "milestone" // Marks completion of a set of related issues (no work itself)
)

// TypeEvent is a system-internal type used by set-state for audit trail beads.
// Originally an orchestrator type, promoted to built-in internal type. It is not a
// core work type (not in IsValid) but is accepted by IsValidWithCustom /
// ValidateWithCustom and treated as built-in for hydration trust (GH#1356).
const TypeEvent IssueType = "event"

// Note: Most orchestrator types (convoy, merge-request, slot, agent, role, rig)
// were removed from beads core. They are now purely custom types with no built-in constants.
// Use string literals like types.IssueType("convoy") if needed, and configure types.custom.
// molecule, gate, and event were re-promoted to built-in because bd commands rely on them:
//   - molecule: bd mol pour/wisp/bond (swarm coordination)
//   - gate: bd gate create/check/resolve, formula gate steps (GH#3213)
//   - event: set-state audit trail beads (GH#1356)
// (message was re-promoted to built-in for inter-agent communication — GH#1347.)

// AllIssueTypes lists the built-in issue types (excludes TypeEvent and custom
// types, matching IssueType.IsValid). Single source for IsValid and the
// `bd schema` enum.
var AllIssueTypes = []IssueType{
	TypeBug, TypeFeature, TypeTask, TypeEpic, TypeChore, TypeDecision,
	TypeMessage, TypeMolecule, TypeGate, TypeSpike, TypeStory, TypeMilestone,
}

// IsValid checks if the issue type is a core work type.
// Core work types (bug, feature, task, epic, chore, decision, message, spike, story, milestone)
// and internal types (molecule, gate) are built-in. Other types require types.custom configuration.
func (t IssueType) IsValid() bool {
	return slices.Contains(AllIssueTypes, t)
}

// IsBuiltIn returns true for core work types and system-internal types
// (i.e. TypeEvent). Used during multi-repo hydration to determine trust:
// - Built-in/internal types: validate (catch typos)
// - Custom types (!IsBuiltIn): trust from source repo
func (t IssueType) IsBuiltIn() bool {
	return t.IsValid() || t == TypeEvent
}

// IsValidWithCustom checks if the issue type is valid, including custom types.
// Custom types are user-defined via bd config set types.custom "type1,type2,..."
func (t IssueType) IsValidWithCustom(customTypes []string) bool {
	if t.IsBuiltIn() {
		return true
	}
	// Check user-configured custom types
	for _, custom := range customTypes {
		if string(t) == custom {
			return true
		}
	}
	return false
}

// Normalize maps issue type aliases to their canonical form.
// For example, "enhancement" -> "feature".
// Case-insensitive to match util.NormalizeIssueType behavior.
func (t IssueType) Normalize() IssueType {
	switch strings.ToLower(string(t)) {
	case "enhancement", "feat":
		return TypeFeature
	case "dec", "adr":
		return TypeDecision
	case "investigation", "timebox":
		return TypeSpike
	case "user-story", "user_story":
		return TypeStory
	case "ms":
		return TypeMilestone
	default:
		return t
	}
}

// RequiredSection describes a recommended section for an issue type.
// Used by bd lint and bd create --validate for template validation.
type RequiredSection struct {
	Heading string // Markdown heading, e.g., "## Steps to Reproduce"
	Hint    string // Guidance for what to include
}

// RequiredSections returns the recommended sections for this issue type.
// Returns nil for types with no specific section requirements.
func (t IssueType) RequiredSections() []RequiredSection {
	switch t {
	case TypeBug:
		return []RequiredSection{
			{Heading: "## Steps to Reproduce", Hint: "Describe how to reproduce the bug"},
			{Heading: "## Acceptance Criteria", Hint: "Define criteria to verify the fix"},
		}
	case TypeTask, TypeFeature, TypeStory:
		return []RequiredSection{
			{Heading: "## Acceptance Criteria", Hint: "Define criteria to verify completion"},
		}
	case TypeEpic:
		return []RequiredSection{
			{Heading: "## Success Criteria", Hint: "Define high-level success criteria"},
		}
	case TypeDecision:
		return []RequiredSection{
			{Heading: "## Decision", Hint: "Summarize what was decided"},
			{Heading: "## Rationale", Hint: "Explain why this option was chosen"},
			{Heading: "## Alternatives Considered", Hint: "List alternatives and why they were rejected"},
		}
	case TypeSpike:
		return []RequiredSection{
			{Heading: "## Goal", Hint: "What question does this spike answer?"},
			{Heading: "## Findings", Hint: "What was learned? (fill in when complete)"},
		}
	default:
		// Chore, milestone, and custom types have no required sections
		return nil
	}
}

// MolType categorizes the molecule type for swarm coordination
type MolType string

// MolType constants
const (
	MolTypeSwarm  MolType = "swarm"  // Swarm molecule: coordinated multi-worker work
	MolTypePatrol MolType = "patrol" // Patrol molecule: recurring operational work
	MolTypeWork   MolType = "work"   // Work molecule: regular assigned work (default)
)

// validMolTypes is the canonical value list; IsValid and ValidMolTypeNames
// both derive from it so the two cannot drift.
var validMolTypes = []MolType{MolTypeSwarm, MolTypePatrol, MolTypeWork}

// IsValid checks if the mol type value is valid
func (m MolType) IsValid() bool {
	if m == "" {
		return true // empty is valid (defaults to work)
	}
	return slices.Contains(validMolTypes, m)
}

// ValidMolTypeNames enumerates the accepted mol-type values for error messages.
func ValidMolTypeNames() string {
	return joinNamesWithOr(validMolTypes)
}

// WispType categorizes ephemeral wisps for TTL-based compaction (gt-9br)
type WispType string

// WispType constants - see WISP-COMPACTION-POLICY.md for TTL assignments
const (
	// Category 1: High-churn, low forensic value (TTL: 6h)
	WispTypeHeartbeat WispType = "heartbeat" // Liveness pings
	WispTypePing      WispType = "ping"      // Health check ACKs

	// Category 2: Operational state (TTL: 24h)
	WispTypePatrol   WispType = "patrol"    // Patrol cycle reports
	WispTypeGCReport WispType = "gc_report" // Garbage collection reports

	// Category 3: Significant events (TTL: 7d)
	WispTypeRecovery   WispType = "recovery"   // Force-kill, recovery actions
	WispTypeError      WispType = "error"      // Error reports
	WispTypeEscalation WispType = "escalation" // Human escalations
)

// validWispTypes is the canonical value list; IsValid and ValidWispTypeNames
// both derive from it so the two cannot drift.
var validWispTypes = []WispType{
	WispTypeHeartbeat, WispTypePing, WispTypePatrol, WispTypeGCReport,
	WispTypeRecovery, WispTypeError, WispTypeEscalation,
}

// IsValid checks if the wisp type value is valid
func (w WispType) IsValid() bool {
	if w == "" {
		return true // empty is valid (uses default TTL)
	}
	return slices.Contains(validWispTypes, w)
}

// ValidWispTypeNames enumerates the accepted wisp-type values for error messages.
func ValidWispTypeNames() string {
	return joinNamesWithOr(validWispTypes)
}

// StorageClass is the create-selected marker for a record's history and
// replication contract. Persistence plane transitions preserve the marker
// except for normalization required by demotion or promotion. The effective
// class can change with the plane when the marker is empty.
type StorageClass string

const (
	// StorageClassVersioned is the default durable, replicated-with-history
	// class. It serializes as the empty storage_class value.
	StorageClassVersioned StorageClass = "versioned"
	// StorageClassUnversioned is durable and interchanged with no revision-
	// history promise: only current state is retained and replicated.
	StorageClassUnversioned StorageClass = "unversioned"
	// StorageClassEphemeral is operational state that is never exported and
	// never replicated — typically TTL-bounded, though not always:
	// no-history rows are class-ephemeral yet TTL-exempt. The existing
	// wisp/lease planes have this character.
	StorageClassEphemeral StorageClass = "ephemeral"
)

// PersistenceMode selects an issue's storage plane and retention behavior.
// Unlike StorageClass, it has no unset value; callers represent omission
// separately from an explicit requested mode.
type PersistenceMode string

const (
	// PersistenceModePersistent selects persistent storage. Existing
	// unversioned records remain unversioned when this mode is selected.
	PersistenceModePersistent PersistenceMode = "persistent"
	// PersistenceModeEphemeral selects ephemeral storage.
	PersistenceModeEphemeral PersistenceMode = "ephemeral"
	// PersistenceModeNoHistory selects ephemeral storage without history.
	PersistenceModeNoHistory PersistenceMode = "no_history"
)

var validPersistenceModes = []PersistenceMode{
	PersistenceModePersistent,
	PersistenceModeEphemeral,
	PersistenceModeNoHistory,
}

// IsValid reports whether a persistence mode is a known explicit value.
// Empty is invalid because it would be indistinguishable from an omitted mode.
func (m PersistenceMode) IsValid() bool {
	return slices.Contains(validPersistenceModes, m)
}

// NormalizePersistenceMode maps a requested mode from current to its exact
// persistence fields without mutating an issue. The returned values are
// Ephemeral, NoHistory, and StorageClass, respectively. Plane and retention
// changes preserve the create-selected class. Promotion clears an explicit
// ephemeral class marker to select normalized versioned storage. An
// unversioned record may remain persistent but cannot move to a wisp mode.
func NormalizePersistenceMode(current Issue, mode PersistenceMode) (bool, bool, StorageClass, error) {
	if err := validatePersistenceTransition(current, mode); err != nil {
		return false, false, "", err
	}
	if persistenceMode(current) == mode {
		return current.Ephemeral, current.NoHistory, current.StorageClass, nil
	}

	return normalizePersistenceMode(current, mode)
}

func validatePersistenceTransition(current Issue, mode PersistenceMode) error {
	if !mode.IsValid() {
		return fmt.Errorf("invalid persistence mode %q", mode)
	}
	if err := validateCurrentPersistenceState(current); err != nil {
		return err
	}
	if current.StorageClass == StorageClassUnversioned && mode != PersistenceModePersistent {
		return fmt.Errorf("cannot move unversioned record to persistence mode %q", mode)
	}
	return nil
}

func validateCurrentPersistenceState(current Issue) error {
	if current.Ephemeral && current.NoHistory {
		return fmt.Errorf("ephemeral and no_history are mutually exclusive")
	}
	return validateCurrentStorageClass(current)
}

func validateCurrentStorageClass(current Issue) error {
	if !current.StorageClass.IsValid() {
		return fmt.Errorf("invalid storage class %q", current.StorageClass)
	}
	return validateCurrentStoragePlane(current)
}

func validateCurrentStoragePlane(current Issue) error {
	wispPlane := current.Ephemeral || current.NoHistory
	if current.StorageClass == "" {
		return nil
	}
	if wispPlane && current.StorageClass != StorageClassEphemeral {
		return fmt.Errorf("storage class %q conflicts with ephemeral/no_history", current.StorageClass)
	}
	if !wispPlane && current.StorageClass == StorageClassEphemeral {
		return fmt.Errorf("storage class ephemeral requires ephemeral or no_history")
	}
	return nil
}

func normalizePersistenceMode(current Issue, mode PersistenceMode) (bool, bool, StorageClass, error) {
	switch mode {
	case PersistenceModePersistent:
		storageClass := current.StorageClass
		if storageClass == StorageClassEphemeral {
			storageClass = ""
		}
		return false, false, storageClass, nil
	case PersistenceModeEphemeral:
		storageClass := current.StorageClass
		if storageClass == StorageClassVersioned {
			storageClass = ""
		}
		return true, false, storageClass, nil
	case PersistenceModeNoHistory:
		storageClass := current.StorageClass
		if storageClass == StorageClassVersioned {
			storageClass = ""
		}
		return false, true, storageClass, nil
	default:
		return false, false, "", fmt.Errorf("invalid persistence mode %q", mode)
	}
}

// validStorageClasses is the canonical value list; IsValid,
// ParseStorageClass, and ValidStorageClassNames all derive from it.
var validStorageClasses = []StorageClass{
	StorageClassVersioned, StorageClassUnversioned, StorageClassEphemeral,
}

// IsValid reports whether the storage class value is valid. Empty is valid:
// its effective class is ephemeral for Ephemeral or NoHistory rows and
// versioned otherwise.
func (s StorageClass) IsValid() bool {
	if s == "" {
		return true
	}
	return slices.Contains(validStorageClasses, s)
}

// ValidStorageClassNames enumerates the accepted values for error messages.
func ValidStorageClassNames() string {
	return joinNamesWithOr(validStorageClasses)
}

// ParseStorageClass validates a user-supplied storage-class value (flag or
// config). Empty is rejected here — callers that allow "unset" check for
// empty before parsing.
func ParseStorageClass(v string) (StorageClass, error) {
	s := StorageClass(v)
	if s == "" || !s.IsValid() {
		return "", fmt.Errorf("invalid storage class %q (must be %s)", v, ValidStorageClassNames())
	}
	return s, nil
}

// Normalize maps the explicit versioned spelling to unset: the two are
// semantically identical and the marker is omitted when versioned in storage
// cells and serialized records alike. Both insert stacks call this so
// the database never persists the literal "versioned".
func (s StorageClass) Normalize() StorageClass {
	if s == StorageClassVersioned {
		return ""
	}
	return s
}

// EffectiveStorageClass resolves the record's class: an explicit declaration
// wins; otherwise wisp-plane records (Ephemeral or NoHistory) are ephemeral
// and everything else is versioned.
func (i *Issue) EffectiveStorageClass() StorageClass {
	if i.StorageClass != "" {
		return i.StorageClass
	}
	if i.Ephemeral || i.NoHistory {
		return StorageClassEphemeral
	}
	return StorageClassVersioned
}

func persistenceMode(i Issue) PersistenceMode {
	if i.NoHistory {
		return PersistenceModeNoHistory
	}
	if i.Ephemeral {
		return PersistenceModeEphemeral
	}
	return PersistenceModePersistent
}

// joinNamesWithOr formats a value list as "a, b, or c" for error messages.
func joinNamesWithOr[T ~string](names []T) string {
	strs := make([]string, len(names))
	for i, n := range names {
		strs[i] = string(n)
	}
	if len(strs) == 1 {
		return strs[0]
	}
	return strings.Join(strs[:len(strs)-1], ", ") + ", or " + strs[len(strs)-1]
}

// WorkType categorizes how work assignment operates for a bead (Decision 006)
type WorkType string

// WorkType constants
const (
	WorkTypeMutex           WorkType = "mutex"            // One worker, exclusive assignment (default)
	WorkTypeOpenCompetition WorkType = "open_competition" // Many submit, buyer picks
)

// IsValid checks if the work type value is valid
func (w WorkType) IsValid() bool {
	switch w {
	case WorkTypeMutex, WorkTypeOpenCompetition, "":
		return true // empty is valid (defaults to mutex)
	}
	return false
}
