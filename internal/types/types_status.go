// Package types defines core data structures for the bd issue tracker.
package types

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Status represents the current state of an issue
type Status string

// Issue status constants
const (
	StatusOpen       Status = "open"
	StatusInProgress Status = "in_progress"
	StatusBlocked    Status = "blocked"
	StatusDeferred   Status = "deferred" // Deliberately put on ice for later
	StatusClosed     Status = "closed"
	StatusPinned     Status = "pinned" // Persistent bead that stays open indefinitely
	StatusHooked     Status = "hooked" // Work actively claimed by a worker
)

// AllStatuses lists the built-in issue statuses (excludes custom statuses). It
// is the single source consulted by Status.IsValid and the `bd schema` enum, so
// adding a status here surfaces it in both validation and the published schema.
var AllStatuses = []Status{
	StatusOpen, StatusInProgress, StatusBlocked, StatusDeferred,
	StatusClosed, StatusPinned, StatusHooked,
}

// IsValid checks if the status value is valid (built-in statuses only)
func (s Status) IsValid() bool {
	return slices.Contains(AllStatuses, s)
}

// IsValidWithCustom checks if the status is valid, including custom statuses.
// Custom statuses are user-defined via bd config set status.custom "status1,status2,..."
func (s Status) IsValidWithCustom(customStatuses []string) bool {
	// First check built-in statuses
	if s.IsValid() {
		return true
	}
	// Then check custom statuses
	for _, custom := range customStatuses {
		if string(s) == custom {
			return true
		}
	}
	return false
}

// IsValidWithCustomStatuses checks if the status is valid, including typed custom statuses.
func (s Status) IsValidWithCustomStatuses(customStatuses []CustomStatus) bool {
	if s.IsValid() {
		return true
	}
	for _, cs := range customStatuses {
		if string(s) == cs.Name {
			return true
		}
	}
	return false
}

// StatusCategory defines how a custom status behaves in views and commands.
type StatusCategory string

const (
	// CategoryActive statuses appear in bd ready and default bd list.
	CategoryActive StatusCategory = "active"
	// CategoryWIP statuses are excluded from bd ready but visible in default bd list.
	CategoryWIP StatusCategory = "wip"
	// CategoryDone statuses are excluded from bd ready and default bd list.
	CategoryDone StatusCategory = "done"
	// CategoryFrozen statuses are excluded from bd ready and default bd list.
	CategoryFrozen StatusCategory = "frozen"
	// CategoryUnspecified is assigned when no category is provided (backward compat).
	// Behaves like current behavior: valid, visible in default bd list, absent from bd ready.
	CategoryUnspecified StatusCategory = "unspecified"
)

// validCategories is the set of user-assignable categories (excludes CategoryUnspecified).
var validCategories = map[StatusCategory]bool{
	CategoryActive: true,
	CategoryWIP:    true,
	CategoryDone:   true,
	CategoryFrozen: true,
}

// CustomStatus represents a user-defined status with its behavioral category.
type CustomStatus struct {
	Name     string         `json:"name"`
	Category StatusCategory `json:"category"`
}

// statusNameRegexp validates custom status names: letter-first, lowercase alphanumeric with hyphens/underscores.
var statusNameRegexp = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// maxCustomStatuses is the maximum number of custom statuses allowed.
const maxCustomStatuses = 50

// builtInStatusNames contains all built-in status names in lowercase for collision detection.
var builtInStatusNames = map[string]bool{
	"open": true, "in_progress": true, "blocked": true,
	"deferred": true, "closed": true, "pinned": true, "hooked": true,
}

// ParseCustomStatusConfig parses a status.custom config value into typed CustomStatus entries.
// Supports both legacy flat format ("foo,bar") and category-annotated format ("foo:active,bar:wip").
// Statuses without a category annotation get CategoryUnspecified (backward compatible).
func ParseCustomStatusConfig(value string) ([]CustomStatus, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}

	parts := strings.Split(value, ",")
	var result []CustomStatus
	seen := make(map[string]bool)

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		status, err := parseCustomStatusPart(part)
		if err != nil {
			return nil, err
		}
		if seen[status.Name] {
			return nil, fmt.Errorf("duplicate custom status name %q", status.Name)
		}
		seen[status.Name] = true

		result = append(result, status)
	}

	if len(result) > maxCustomStatuses {
		return nil, fmt.Errorf("too many custom statuses (%d): maximum is %d", len(result), maxCustomStatuses)
	}

	return result, nil
}

func parseCustomStatusPart(part string) (CustomStatus, error) {
	var name string
	category := CategoryUnspecified

	// Split on first colon only.
	if idx := strings.IndexByte(part, ':'); idx >= 0 {
		name = part[:idx]
		catStr := part[idx+1:]
		if catStr == "" {
			return CustomStatus{}, fmt.Errorf("invalid custom status %q: trailing colon with empty category", part)
		}
		category = StatusCategory(catStr)
		if !validCategories[category] {
			return CustomStatus{}, fmt.Errorf("invalid category %q for status %q: must be one of active, wip, done, frozen", catStr, name)
		}
	} else {
		name = part
	}

	if !statusNameRegexp.MatchString(name) {
		return CustomStatus{}, fmt.Errorf("invalid status name %q: must match [a-z][a-z0-9_-]* (lowercase, letter-first, no spaces)", name)
	}
	if builtInStatusNames[strings.ToLower(name)] {
		return CustomStatus{}, fmt.Errorf("custom status %q collides with built-in status", name)
	}
	return CustomStatus{Name: name, Category: category}, nil
}

// CustomStatusNames extracts just the name strings from a slice of CustomStatus.
// Useful for backward-compatible callers that only need names for validation.
func CustomStatusNames(statuses []CustomStatus) []string {
	if len(statuses) == 0 {
		return nil
	}
	names := make([]string, len(statuses))
	for i, s := range statuses {
		names[i] = s.Name
	}
	return names
}

// CustomStatusesByCategory returns custom statuses filtered by the given category.
func CustomStatusesByCategory(statuses []CustomStatus, category StatusCategory) []CustomStatus {
	var result []CustomStatus
	for _, s := range statuses {
		if s.Category == category {
			result = append(result, s)
		}
	}
	return result
}

// BuiltInStatusCategory returns the category for a built-in status.
func BuiltInStatusCategory(status Status) StatusCategory {
	switch status {
	case StatusOpen:
		return CategoryActive
	case StatusInProgress, StatusBlocked, StatusHooked:
		return CategoryWIP
	case StatusClosed:
		return CategoryDone
	case StatusDeferred, StatusPinned:
		return CategoryFrozen
	default:
		return CategoryUnspecified
	}
}
