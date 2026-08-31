package query

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/timeparsing"
	"github.com/jonbaldie/beads/internal/types"
)

// applyComparison applies a comparison to the filter.
func (e *Evaluator) applyComparison(comp *ComparisonNode, filter *types.IssueFilter) error {
	if apply, ok := comparisonFilterBuilders[comp.Field]; ok {
		return apply(e, comp, filter)
	}
	if field, ok := booleanFilterFields[comp.Field]; ok {
		return e.applyBoolFilter(comp, filter, field)
	}
	if strings.HasPrefix(comp.Field, "metadata.") {
		return e.applyMetadataFilter(comp, filter)
	}
	return fmt.Errorf("unknown field: %s", comp.Field)
}

func (e *Evaluator) applyStatusFilter(comp *ComparisonNode, filter *types.IssueFilter) error {
	if comp.Op != OpEquals && comp.Op != OpNotEquals {
		return fmt.Errorf("status only supports = and != operators")
	}
	status := types.Status(strings.ToLower(comp.Value))
	if !status.IsValid() {
		return fmt.Errorf("invalid status: %s", comp.Value)
	}
	if comp.Op == OpEquals {
		filter.Status = &status
	} else {
		filter.ExcludeStatus = append(filter.ExcludeStatus, status)
	}
	return nil
}

func (e *Evaluator) applyPriorityFilter(comp *ComparisonNode, filter *types.IssueFilter) error {
	priority, err := parsePriority(comp.Value)
	if err != nil {
		return err
	}
	return applyPriorityOperator(comp.Op, priority, filter)
}

func parsePriority(value string) (int, error) {
	priority, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid priority value: %s", value)
	}
	if priority < 0 || priority > 4 {
		return 0, fmt.Errorf("priority must be between 0 and 4")
	}
	return priority, nil
}

func applyPriorityOperator(op ComparisonOp, priority int, filter *types.IssueFilter) error {
	switch op {
	case OpEquals:
		filter.Priority = &priority
	case OpNotEquals:
		// For != we need predicate filtering
		return fmt.Errorf("priority != requires predicate filtering")
	case OpLess:
		return setPriorityMax(priority, filter)
	case OpLessEq:
		filter.PriorityMax = &priority
	case OpGreater:
		return setPriorityMin(priority, filter)
	case OpGreaterEq:
		filter.PriorityMin = &priority
	}
	return nil
}

func setPriorityMax(priority int, filter *types.IssueFilter) error {
	// priority < X means PriorityMax = X-1.
	max := priority - 1
	if max < 0 {
		return fmt.Errorf("priority < %d matches nothing", priority)
	}
	filter.PriorityMax = &max
	return nil
}

func setPriorityMin(priority int, filter *types.IssueFilter) error {
	// priority > X means PriorityMin = X+1.
	min := priority + 1
	if min > 4 {
		return fmt.Errorf("priority > %d matches nothing", priority)
	}
	filter.PriorityMin = &min
	return nil
}

func (e *Evaluator) applyTypeFilter(comp *ComparisonNode, filter *types.IssueFilter) error {
	if comp.Op != OpEquals && comp.Op != OpNotEquals {
		return fmt.Errorf("type only supports = and != operators")
	}
	issueType := types.IssueType(strings.ToLower(comp.Value))
	if comp.Op == OpEquals {
		filter.IssueType = &issueType
	} else {
		filter.ExcludeTypes = append(filter.ExcludeTypes, issueType)
	}
	return nil
}

func (e *Evaluator) applyAssigneeFilter(comp *ComparisonNode, filter *types.IssueFilter) error {
	if comp.Op != OpEquals {
		return fmt.Errorf("assignee only supports = operator")
	}
	if comp.Value == "" || strings.ToLower(comp.Value) == "none" || strings.ToLower(comp.Value) == "null" {
		filter.NoAssignee = true
	} else {
		filter.Assignee = &comp.Value
	}
	return nil
}

func (e *Evaluator) applyOwnerFilter(_ *ComparisonNode, _ *types.IssueFilter) error {
	// Owner filtering requires predicate
	return fmt.Errorf("owner filtering requires predicate mode")
}

func (e *Evaluator) applyLabelFilter(comp *ComparisonNode, filter *types.IssueFilter) error {
	if comp.Op != OpEquals {
		return fmt.Errorf("label only supports = operator")
	}
	if comp.Value == "" || strings.ToLower(comp.Value) == "none" || strings.ToLower(comp.Value) == "null" {
		filter.NoLabels = true
	} else {
		filter.Labels = append(filter.Labels, comp.Value)
	}
	return nil
}

func (e *Evaluator) applyTitleFilter(comp *ComparisonNode, filter *types.IssueFilter) error {
	if comp.Op != OpEquals {
		return fmt.Errorf("title only supports = operator (use title contains pattern)")
	}
	filter.TitleContains = comp.Value
	return nil
}

func (e *Evaluator) applyDescriptionFilter(comp *ComparisonNode, filter *types.IssueFilter) error {
	if comp.Op != OpEquals {
		return fmt.Errorf("description only supports = operator (use desc contains pattern)")
	}
	if comp.Value == "" || strings.ToLower(comp.Value) == "none" || strings.ToLower(comp.Value) == "null" {
		filter.EmptyDescription = true
	} else {
		filter.DescriptionContains = comp.Value
	}
	return nil
}

func (e *Evaluator) applyNotesFilter(comp *ComparisonNode, filter *types.IssueFilter) error {
	if comp.Op != OpEquals {
		return fmt.Errorf("notes only supports = operator")
	}
	filter.NotesContains = comp.Value
	return nil
}

func (e *Evaluator) applyCreatedFilter(comp *ComparisonNode, filter *types.IssueFilter) error {
	t, err := e.parseTimeValue(comp)
	if err != nil {
		return fmt.Errorf("invalid created time: %w", err)
	}
	switch comp.Op {
	case OpEquals:
		// For equals, set both before and after to bracket the day
		dayStart := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
		dayEnd := dayStart.Add(24 * time.Hour)
		filter.CreatedAfter = &dayStart
		filter.CreatedBefore = &dayEnd
	case OpGreater:
		filter.CreatedAfter = &t
	case OpGreaterEq:
		filter.CreatedAfter = &t
	case OpLess:
		filter.CreatedBefore = &t
	case OpLessEq:
		endOfDay := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 999999999, t.Location())
		filter.CreatedBefore = &endOfDay
	default:
		return fmt.Errorf("created does not support %s operator", comp.Op.String())
	}
	return nil
}

func (e *Evaluator) applyUpdatedFilter(comp *ComparisonNode, filter *types.IssueFilter) error {
	t, err := e.parseTimeValue(comp)
	if err != nil {
		return fmt.Errorf("invalid updated time: %w", err)
	}
	switch comp.Op {
	case OpEquals:
		dayStart := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
		dayEnd := dayStart.Add(24 * time.Hour)
		filter.UpdatedAfter = &dayStart
		filter.UpdatedBefore = &dayEnd
	case OpGreater:
		filter.UpdatedAfter = &t
	case OpGreaterEq:
		filter.UpdatedAfter = &t
	case OpLess:
		filter.UpdatedBefore = &t
	case OpLessEq:
		endOfDay := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 999999999, t.Location())
		filter.UpdatedBefore = &endOfDay
	default:
		return fmt.Errorf("updated does not support %s operator", comp.Op.String())
	}
	return nil
}

func (e *Evaluator) applyClosedFilter(comp *ComparisonNode, filter *types.IssueFilter) error {
	t, err := e.parseTimeValue(comp)
	if err != nil {
		return fmt.Errorf("invalid closed time: %w", err)
	}
	switch comp.Op {
	case OpGreater:
		filter.ClosedAfter = &t
	case OpGreaterEq:
		filter.ClosedAfter = &t
	case OpLess:
		filter.ClosedBefore = &t
	case OpLessEq:
		endOfDay := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 999999999, t.Location())
		filter.ClosedBefore = &endOfDay
	default:
		return fmt.Errorf("closed does not support %s operator", comp.Op.String())
	}
	return nil
}

func (e *Evaluator) applyStartedFilter(comp *ComparisonNode, filter *types.IssueFilter) error {
	t, err := e.parseTimeValue(comp)
	if err != nil {
		return fmt.Errorf("invalid started time: %w", err)
	}
	switch comp.Op {
	case OpGreater:
		filter.StartedAfter = &t
	case OpGreaterEq:
		filter.StartedAfter = &t
	case OpLess:
		filter.StartedBefore = &t
	case OpLessEq:
		endOfDay := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 999999999, t.Location())
		filter.StartedBefore = &endOfDay
	default:
		return fmt.Errorf("started does not support %s operator", comp.Op.String())
	}
	return nil
}

func (e *Evaluator) applyIDFilter(comp *ComparisonNode, filter *types.IssueFilter) error {
	if comp.Op != OpEquals {
		return fmt.Errorf("id only supports = operator")
	}
	// Check if it looks like a prefix (ends with *)
	if strings.HasSuffix(comp.Value, "*") {
		filter.IDPrefix = strings.TrimSuffix(comp.Value, "*")
	} else {
		filter.IDs = append(filter.IDs, comp.Value)
	}
	return nil
}

func (e *Evaluator) applySpecFilter(comp *ComparisonNode, filter *types.IssueFilter) error {
	if comp.Op != OpEquals {
		return fmt.Errorf("spec only supports = operator")
	}
	// Support prefix matching
	if strings.HasSuffix(comp.Value, "*") {
		filter.SpecIDPrefix = strings.TrimSuffix(comp.Value, "*")
	} else {
		filter.SpecIDPrefix = comp.Value
	}
	return nil
}

func (e *Evaluator) applyParentFilter(comp *ComparisonNode, filter *types.IssueFilter) error {
	if comp.Op != OpEquals {
		return fmt.Errorf("parent only supports = operator")
	}
	filter.ParentID = &comp.Value
	return nil
}

func (e *Evaluator) applyBoolFilter(comp *ComparisonNode, filter *types.IssueFilter, field string) error {
	if comp.Op != OpEquals {
		return fmt.Errorf("%s only supports = operator", field)
	}
	val := strings.ToLower(comp.Value)
	var boolVal bool
	switch val {
	case "true", "yes", "1":
		boolVal = true
	case "false", "no", "0":
		boolVal = false
	default:
		return fmt.Errorf("invalid boolean value for %s: %s", field, comp.Value)
	}

	switch field {
	case "pinned":
		filter.Pinned = &boolVal
	case "ephemeral":
		filter.Ephemeral = &boolVal
	case "template":
		filter.IsTemplate = &boolVal
	}
	return nil
}

func (e *Evaluator) applyMolTypeFilter(comp *ComparisonNode, filter *types.IssueFilter) error {
	if comp.Op != OpEquals {
		return fmt.Errorf("mol_type only supports = operator")
	}
	mt := types.MolType(strings.ToLower(comp.Value))
	if !mt.IsValid() {
		return fmt.Errorf("invalid mol_type: %s", comp.Value)
	}
	filter.MolType = &mt
	return nil
}

// applyMetadataFilter handles metadata.<key>=<value> queries (GH#1406).
func (e *Evaluator) applyMetadataFilter(comp *ComparisonNode, filter *types.IssueFilter) error {
	if comp.Op != OpEquals {
		return fmt.Errorf("metadata fields only support = operator")
	}
	key := strings.TrimPrefix(comp.Field, "metadata.")
	if err := storage.ValidateMetadataKey(key); err != nil {
		return err
	}
	if filter.MetadataFields == nil {
		filter.MetadataFields = make(map[string]string)
	}
	filter.MetadataFields[key] = comp.Value
	return nil
}

// applyHasMetadataKeyFilter handles has_metadata_key=<keyname> queries (GH#1406).
func (e *Evaluator) applyHasMetadataKeyFilter(comp *ComparisonNode, filter *types.IssueFilter) error {
	if comp.Op != OpEquals {
		return fmt.Errorf("has_metadata_key only supports = operator")
	}
	if err := storage.ValidateMetadataKey(comp.Value); err != nil {
		return err
	}
	filter.HasMetadataKey = comp.Value
	return nil
}

// buildMetadataPredicate builds a predicate for metadata.<key>=<value> in OR queries.
// Parses the issue's JSON metadata and compares the top-level scalar at the given key.
func (e *Evaluator) buildMetadataPredicate(comp *ComparisonNode) (func(*types.Issue) bool, error) {
	if comp.Op != OpEquals {
		return nil, fmt.Errorf("metadata fields only support = operator")
	}
	key := strings.TrimPrefix(comp.Field, "metadata.")
	if err := storage.ValidateMetadataKey(key); err != nil {
		return nil, err
	}
	value := comp.Value
	return func(i *types.Issue) bool {
		if len(i.Metadata) == 0 {
			return false
		}
		var data map[string]json.RawMessage
		if err := json.Unmarshal(i.Metadata, &data); err != nil {
			return false
		}
		raw, ok := data[key]
		if !ok {
			return false
		}
		// Try to unmarshal as a string first (most common case)
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s == value
		}
		// Fall back to comparing the raw JSON representation (numbers, bools)
		return strings.Trim(string(raw), "\"") == value
	}, nil
}

// buildHasMetadataKeyPredicate builds a predicate for has_metadata_key=<keyname> in OR queries.
func (e *Evaluator) buildHasMetadataKeyPredicate(comp *ComparisonNode) (func(*types.Issue) bool, error) {
	if comp.Op != OpEquals {
		return nil, fmt.Errorf("has_metadata_key only supports = operator")
	}
	key := comp.Value
	if err := storage.ValidateMetadataKey(key); err != nil {
		return nil, err
	}
	return func(i *types.Issue) bool {
		if len(i.Metadata) == 0 {
			return false
		}
		var data map[string]json.RawMessage
		if err := json.Unmarshal(i.Metadata, &data); err != nil {
			return false
		}
		_, ok := data[key]
		return ok
	}, nil
}

// applyNot applies a NOT expression to the filter.
func (e *Evaluator) applyNot(not *NotNode, filter *types.IssueFilter) error {
	comp, ok := not.Operand.(*ComparisonNode)
	if !ok {
		return fmt.Errorf("NOT only supports simple comparisons in filter mode")
	}

	switch comp.Field {
	case "status":
		if comp.Op != OpEquals {
			return fmt.Errorf("NOT status only supports = operator")
		}
		status := types.Status(strings.ToLower(comp.Value))
		filter.ExcludeStatus = append(filter.ExcludeStatus, status)
		return nil
	case "type":
		if comp.Op != OpEquals {
			return fmt.Errorf("NOT type only supports = operator")
		}
		issueType := types.IssueType(strings.ToLower(comp.Value))
		filter.ExcludeTypes = append(filter.ExcludeTypes, issueType)
		return nil
	default:
		return fmt.Errorf("NOT not supported for field %s in filter mode", comp.Field)
	}
}

// parseTimeValue parses a time value from a comparison node.
// Supports duration values (7d, 24h) which are interpreted as "now - duration".
func (e *Evaluator) parseTimeValue(comp *ComparisonNode) (time.Time, error) {
	if comp.ValueType == TokenDuration {
		// Duration values like 7d mean "7 days ago" for < comparisons
		// and "within the last 7 days" for > comparisons
		// We parse as relative to now, going backwards
		return e.parseDurationAgo(comp.Value)
	}
	// Otherwise use the standard time parser
	return timeparsing.ParseRelativeTime(comp.Value, e.now)
}

// parseDurationAgo parses a duration and returns now - duration.
func (e *Evaluator) parseDurationAgo(s string) (time.Time, error) {
	// Negate the duration to get time in the past
	negated := "-" + strings.TrimPrefix(s, "+")
	return timeparsing.ParseCompactDuration(negated, e.now)
}

// extractBaseFilters extracts filter-compatible portions from a complex query.
// This is used to pre-filter before applying the predicate.
func (e *Evaluator) extractBaseFilters(node Node, filter *types.IssueFilter) {
	switch n := node.(type) {
	case *ComparisonNode:
		// Try to apply, ignore errors (best-effort optimization: incompatible filters are safely skipped)
		_ = e.applyComparison(n, filter)
	case *AndNode:
		e.extractBaseFilters(n.Left, filter)
		e.extractBaseFilters(n.Right, filter)
	case *NotNode:
		_ = e.applyNot(n, filter) // Best-effort optimization: incompatible NOT filters are safely skipped
	case *OrNode:
		// For OR, we can't safely extract base filters
		// (extracting from either side would over-filter)
	}
}
