package linear

import (
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/types"
)

// MappingConfig holds configurable mappings between Linear and Beads.
// All maps use lowercase keys for case-insensitive matching.
type MappingConfig struct {
	// PriorityMap maps Linear priority (0-4) to Beads priority (0-4).
	// Key is Linear priority as string, value is Beads priority.
	PriorityMap map[string]int

	// StateMap maps Linear state types/names to Beads statuses.
	// Key is lowercase state type or name, value is Beads status string.
	StateMap map[string]string

	// ExplicitStateMap contains only user-configured linear.state_map.* entries.
	// Defaults are intentionally excluded so push can distinguish safe explicit
	// mappings from type-based fallbacks.
	ExplicitStateMap map[string]string

	// OutboundStateMap maps a Beads status to the Linear workflow state NAME to
	// use when pushing. Populated from linear.outbound_state_map.<beads_status>
	// config keys. Used to disambiguate cases where multiple Linear states share
	// the same state type (e.g. "In Progress" and "In Review" are both
	// "started"), which would otherwise fail the push with an ambiguity error.
	// Keys are lowercase beads status strings; values are Linear state names
	// matched case-insensitively against the workflow state cache.
	OutboundStateMap map[string]string

	// LabelTypeMap maps Linear label names to Beads issue types.
	// Key is lowercase label name, value is Beads issue type.
	LabelTypeMap map[string]string

	// RelationMap maps Linear relation types to Beads dependency types.
	// Key is Linear relation type, value is Beads dependency type.
	RelationMap map[string]string

	// CustomStatuses holds typed entries from status.custom (beads config).
	// Used for push-time state_map validation and matching non-built-in statuses.
	CustomStatuses []types.CustomStatus
}

// DefaultMappingConfig returns sensible default mappings.
func DefaultMappingConfig() *MappingConfig {
	return &MappingConfig{
		// Linear priority: 0=none, 1=urgent, 2=high, 3=medium, 4=low
		// Beads priority: 0=critical, 1=high, 2=medium, 3=low, 4=backlog
		PriorityMap: map[string]int{
			"0": 4, // No priority -> Backlog
			"1": 0, // Urgent -> Critical
			"2": 1, // High -> High
			"3": 2, // Medium -> Medium
			"4": 3, // Low -> Low
		},
		// Linear state types: backlog, unstarted, started, completed, canceled
		StateMap: map[string]string{
			"backlog":   "open",
			"unstarted": "open",
			"started":   "in_progress",
			"completed": "closed",
			"canceled":  "closed",
		},
		ExplicitStateMap: make(map[string]string),
		OutboundStateMap: make(map[string]string),
		// Label patterns for issue type inference
		LabelTypeMap: map[string]string{
			"bug":         "bug",
			"defect":      "bug",
			"feature":     "feature",
			"enhancement": "feature",
			"epic":        "epic",
			"chore":       "chore",
			"maintenance": "chore",
			"task":        "task",
			"decision":    "decision",
			"spike":       "spike",
			"story":       "story",
			"milestone":   "milestone",
		},
		// Linear relation types to Beads dependency types
		RelationMap: map[string]string{
			"blocks":    "blocks",
			"blockedBy": "blocks", // Inverse: the related issue blocks this one
			"duplicate": "duplicates",
			"related":   "related",
		},
	}
}

// ConfigLoader is an interface for loading configuration values.
// This allows the mapping package to be decoupled from the storage layer.
type ConfigLoader interface {
	GetAllConfig() (map[string]string, error)
}

// LoadMappingConfig loads mapping configuration from a config loader.
// Config keys follow the pattern: linear.<category>_map.<key> = <value>
// Examples:
//
//	linear.priority_map.0 = 4       (Linear "no priority" -> Beads backlog)
//	linear.state_map.started = in_progress
//	linear.outbound_state_map.in_progress = "In Progress"
//	linear.label_type_map.bug = bug
//	linear.relation_map.blocks = blocks
func LoadMappingConfig(loader ConfigLoader) *MappingConfig {
	config := DefaultMappingConfig()

	if loader == nil {
		return config
	}

	// Load all config keys and filter for linear mappings
	allConfig, err := loader.GetAllConfig()
	if err != nil {
		return config
	}

	for key, value := range allConfig {
		if key == "status.custom" {
			applyCustomStatusConfig(config, value)
			continue
		}
		applyMappingEntry(config, key, value)
	}

	return config
}

func applyCustomStatusConfig(config *MappingConfig, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if custom, err := types.ParseCustomStatusConfig(value); err == nil {
		config.CustomStatuses = custom
	}
}

func applyMappingEntry(config *MappingConfig, key, value string) {
	switch {
	case strings.HasPrefix(key, "linear.priority_map."):
		linearPriority := strings.TrimPrefix(key, "linear.priority_map.")
		if beadsPriority, err := parseIntValue(value); err == nil {
			config.PriorityMap[linearPriority] = beadsPriority
		}
	case strings.HasPrefix(key, "linear.state_map."):
		stateKey := strings.ToLower(strings.TrimPrefix(key, "linear.state_map."))
		config.StateMap[stateKey] = value
		config.ExplicitStateMap[stateKey] = value
	case strings.HasPrefix(key, "linear.outbound_state_map."):
		statusKey := strings.ToLower(strings.TrimPrefix(key, "linear.outbound_state_map."))
		config.OutboundStateMap[statusKey] = value
	case strings.HasPrefix(key, "linear.label_type_map."):
		labelKey := strings.ToLower(strings.TrimPrefix(key, "linear.label_type_map."))
		config.LabelTypeMap[labelKey] = value
	case strings.HasPrefix(key, "linear.relation_map."):
		relationType := strings.TrimPrefix(key, "linear.relation_map.")
		config.RelationMap[relationType] = value
	}
}

// parseIntValue safely parses an integer from a string config value.
func parseIntValue(s string) (int, error) {
	var v int
	_, err := fmt.Sscanf(s, "%d", &v)
	return v, err
}

// PriorityToBeads maps Linear priority (0-4) to Beads priority (0-4).
// Linear: 0=no priority, 1=urgent, 2=high, 3=medium, 4=low
// Beads:  0=critical, 1=high, 2=medium, 3=low, 4=backlog
// Uses configurable mapping from linear.priority_map.* config.
func PriorityToBeads(linearPriority int, config *MappingConfig) int {
	key := fmt.Sprintf("%d", linearPriority)
	if beadsPriority, ok := config.PriorityMap[key]; ok {
		return beadsPriority
	}
	// Fallback to default mapping if not configured
	return 2 // Default to Medium
}

// PriorityToLinear maps Beads priority (0-4) to Linear priority (0-4).
// Uses configurable mapping by inverting linear.priority_map.* config.
func PriorityToLinear(beadsPriority int, config *MappingConfig) int {
	// Build inverse map from config
	inverseMap := make(map[int]int)
	for linearKey, beadsVal := range config.PriorityMap {
		var linearVal int
		if _, err := fmt.Sscanf(linearKey, "%d", &linearVal); err == nil {
			inverseMap[beadsVal] = linearVal
		}
	}

	if linearPriority, ok := inverseMap[beadsPriority]; ok {
		return linearPriority
	}
	// Fallback to default mapping if not found
	return 3 // Default to Medium
}

// StateToBeadsStatus maps Linear state type to Beads status.
// Checks both state type (backlog, unstarted, etc.) and state name for custom workflows.
// Uses configurable mapping from linear.state_map.* config.
func StateToBeadsStatus(state *State, config *MappingConfig) types.Status {
	if state == nil {
		return types.StatusOpen
	}

	// First, try to match by state type (preferred)
	stateType := strings.ToLower(state.Type)
	if statusStr, ok := config.StateMap[stateType]; ok {
		return ParseBeadsStatus(statusStr)
	}

	// Then try to match by state name (for custom workflow states)
	stateName := strings.ToLower(state.Name)
	if statusStr, ok := config.StateMap[stateName]; ok {
		return ParseBeadsStatus(statusStr)
	}

	// Default fallback
	return types.StatusOpen
}

func stateMapMatchesStatus(mapped string, status types.Status) bool {
	normalizedMapped := strings.ToLower(strings.TrimSpace(mapped))
	normalizedStatus := strings.ToLower(strings.TrimSpace(string(status)))
	if normalizedMapped == normalizedStatus {
		return true
	}
	parsed := ParseBeadsStatus(mapped)
	if parsed == status {
		// ParseBeadsStatus returns StatusOpen for unrecognized strings; do not
		// treat those as matching built-in open (avoids false "ambiguous mapping"
		// when state_map values are custom status names like "review").
		if parsed == types.StatusOpen && normalizedMapped != "open" {
			return false
		}
		return true
	}
	return false
}

// ResolveStateIDForBeadsStatus returns the unique Linear workflow state ID to
// use when pushing the given beads status. Push only trusts explicit
// linear.state_map.* entries; defaults are safe for pull but too ambiguous for
// mutation.
func ResolveStateIDForBeadsStatus(cache *StateCache, status types.Status, config *MappingConfig) (string, error) {
	if cache == nil || len(cache.States) == 0 {
		return "", fmt.Errorf("no workflow states found")
	}
	if config == nil || len(config.ExplicitStateMap) == 0 {
		return "", fmt.Errorf("%s", missingExplicitStateMapMessage)
	}

	// Outbound override: an explicit linear.outbound_state_map.<status> entry
	// names the exact Linear workflow state to push to and short-circuits the
	// name/type matching below. This is the escape hatch when multiple Linear
	// states share a type (e.g. "In Progress" and "In Review" are both
	// "started") and the type-based fallback would otherwise be ambiguous.
	if stateID, handled, err := resolveOutboundStateID(cache.States, status, config); handled {
		return stateID, err
	}
	return resolveExplicitStateID(cache.States, status, config.ExplicitStateMap)
}

func resolveExplicitStateID(states []State, status types.Status, mappings map[string]string) (string, error) {
	nameMatches := findExplicitStateMatches(states, status, mappings, false)
	if stateID, handled, err := resolveNamedStateMatch(nameMatches, status); handled {
		return stateID, err
	}

	typeMatches := findExplicitStateMatches(states, status, mappings, true)
	if stateID, handled, err := resolveTypedStateMatch(typeMatches, status); handled {
		return stateID, err
	}

	return "", fmt.Errorf("linear.state_map has no configured Linear state for beads status %q", status)
}

func resolveNamedStateMatch(matches []State, status types.Status) (string, bool, error) {
	if len(matches) == 1 {
		return matches[0].ID, true, nil
	}
	if len(matches) > 1 {
		return "", true, fmt.Errorf("linear.state_map maps beads status %q to multiple Linear states: %s", status, strings.Join(stateNames(matches), ", "))
	}
	return "", false, nil
}

func resolveTypedStateMatch(matches []State, status types.Status) (string, bool, error) {
	if len(matches) == 1 {
		return matches[0].ID, true, nil
	}
	if len(matches) > 1 {
		return "", true, fmt.Errorf("linear.state_map type fallback is ambiguous for beads status %q across Linear states: %s. Set linear.outbound_state_map.%s = \"<state name>\" to disambiguate", status, strings.Join(stateNames(matches), ", "), status)
	}
	return "", false, nil
}

func stateNames(states []State) []string {
	names := make([]string, 0, len(states))
	for _, state := range states {
		names = append(names, state.Name)
	}
	return names
}

func resolveOutboundStateID(states []State, status types.Status, config *MappingConfig) (string, bool, error) {
	outboundName, ok := config.OutboundStateMap[strings.ToLower(strings.TrimSpace(string(status)))]
	if !ok {
		return "", false, nil
	}
	want := strings.ToLower(strings.TrimSpace(outboundName))
	for _, state := range states {
		if strings.ToLower(strings.TrimSpace(state.Name)) == want {
			return state.ID, true, nil
		}
	}
	return "", true, fmt.Errorf("linear.outbound_state_map.%s = %q does not match any Linear workflow state", status, outboundName)
}

func findExplicitStateMatches(states []State, status types.Status, mappings map[string]string, byType bool) []State {
	var matches []State
	for _, state := range states {
		key := state.Name
		if byType {
			key = state.Type
		}
		mapped, ok := mappings[strings.ToLower(strings.TrimSpace(key))]
		if ok && stateMapMatchesStatus(mapped, status) {
			matches = append(matches, state)
		}
	}
	return matches
}

// ParseBeadsStatus converts a status string to types.Status.
func ParseBeadsStatus(s string) types.Status {
	switch strings.ToLower(s) {
	case "open":
		return types.StatusOpen
	case "in_progress", "in-progress", "inprogress":
		return types.StatusInProgress
	case "blocked":
		return types.StatusBlocked
	case "closed", "done":
		return types.StatusClosed
	case "deferred":
		return types.StatusDeferred
	case "pinned":
		return types.StatusPinned
	case "hooked":
		return types.StatusHooked
	default:
		return types.StatusOpen
	}
}

// StatusToLinearStateType converts Beads status to Linear state type for filtering.
// This is used when pushing issues to Linear to find the appropriate state.
func StatusToLinearStateType(status types.Status) string {
	switch status {
	case types.StatusOpen:
		return "unstarted"
	case types.StatusInProgress:
		return "started"
	case types.StatusBlocked:
		return "started" // Linear doesn't have blocked state type
	case types.StatusClosed:
		return "completed"
	default:
		return "unstarted"
	}
}
