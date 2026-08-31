package query

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/types"
)

// buildPredicate builds a predicate function for complex queries.
func (e *Evaluator) buildPredicate(node Node) (func(*types.Issue) bool, error) {
	switch n := node.(type) {
	case *ComparisonNode:
		return e.buildComparisonPredicate(n)
	case *AndNode:
		return e.buildAndPredicate(n)
	case *OrNode:
		return e.buildOrPredicate(n)
	case *NotNode:
		return e.buildNotPredicate(n)
	default:
		return nil, fmt.Errorf("unexpected node type: %T", node)
	}
}

func (e *Evaluator) buildAndPredicate(node *AndNode) (func(*types.Issue) bool, error) {
	return e.buildCombinedPredicate(node.Left, node.Right, func(left, right bool) bool {
		return left && right
	})
}

func (e *Evaluator) buildOrPredicate(node *OrNode) (func(*types.Issue) bool, error) {
	return e.buildCombinedPredicate(node.Left, node.Right, func(left, right bool) bool {
		return left || right
	})
}

func (e *Evaluator) buildCombinedPredicate(leftNode, rightNode Node, combine func(bool, bool) bool) (func(*types.Issue) bool, error) {
	left, err := e.buildPredicate(leftNode)
	if err != nil {
		return nil, err
	}
	right, err := e.buildPredicate(rightNode)
	if err != nil {
		return nil, err
	}
	return func(issue *types.Issue) bool {
		return combine(left(issue), right(issue))
	}, nil
}

func (e *Evaluator) buildNotPredicate(node *NotNode) (func(*types.Issue) bool, error) {
	operand, err := e.buildPredicate(node.Operand)
	if err != nil {
		return nil, err
	}
	return func(issue *types.Issue) bool {
		return !operand(issue)
	}, nil
}

type comparisonPredicateBuilder func(*Evaluator, *ComparisonNode) (func(*types.Issue) bool, error)

var comparisonPredicateBuilders = map[string]comparisonPredicateBuilder{
	"status":           (*Evaluator).buildStatusPredicate,
	"priority":         (*Evaluator).buildPriorityPredicate,
	"type":             (*Evaluator).buildTypePredicate,
	"assignee":         (*Evaluator).buildAssigneePredicate,
	"owner":            (*Evaluator).buildOwnerPredicate,
	"label":            (*Evaluator).buildLabelPredicate,
	"labels":           (*Evaluator).buildLabelPredicate,
	"title":            (*Evaluator).buildTitlePredicate,
	"description":      (*Evaluator).buildDescriptionPredicate,
	"desc":             (*Evaluator).buildDescriptionPredicate,
	"notes":            (*Evaluator).buildNotesPredicate,
	"created":          (*Evaluator).buildCreatedPredicate,
	"created_at":       (*Evaluator).buildCreatedPredicate,
	"updated":          (*Evaluator).buildUpdatedPredicate,
	"updated_at":       (*Evaluator).buildUpdatedPredicate,
	"closed":           (*Evaluator).buildClosedPredicate,
	"closed_at":        (*Evaluator).buildClosedPredicate,
	"started":          (*Evaluator).buildStartedPredicate,
	"started_at":       (*Evaluator).buildStartedPredicate,
	"id":               (*Evaluator).buildIDPredicate,
	"spec":             (*Evaluator).buildSpecPredicate,
	"spec_id":          (*Evaluator).buildSpecPredicate,
	"has_metadata_key": (*Evaluator).buildHasMetadataKeyPredicate,
}

var booleanPredicateGetters = map[string]func(*types.Issue) bool{
	"pinned":    func(issue *types.Issue) bool { return issue.Pinned },
	"ephemeral": func(issue *types.Issue) bool { return issue.Ephemeral },
	"template":  func(issue *types.Issue) bool { return issue.IsTemplate },
}

// buildComparisonPredicate builds a predicate for a single comparison.
func (e *Evaluator) buildComparisonPredicate(comp *ComparisonNode) (func(*types.Issue) bool, error) {
	if build, ok := comparisonPredicateBuilders[comp.Field]; ok {
		return build(e, comp)
	}
	if getter, ok := booleanPredicateGetters[comp.Field]; ok {
		return e.buildBoolPredicate(comp, getter)
	}
	if strings.HasPrefix(comp.Field, "metadata.") {
		return e.buildMetadataPredicate(comp)
	}
	return nil, fmt.Errorf("unknown field: %s", comp.Field)
}

func (e *Evaluator) buildStatusPredicate(comp *ComparisonNode) (func(*types.Issue) bool, error) {
	status := types.Status(strings.ToLower(comp.Value))
	switch comp.Op {
	case OpEquals:
		return func(i *types.Issue) bool { return i.Status == status }, nil
	case OpNotEquals:
		return func(i *types.Issue) bool { return i.Status != status }, nil
	default:
		return nil, fmt.Errorf("status does not support %s operator", comp.Op.String())
	}
}

func (e *Evaluator) buildPriorityPredicate(comp *ComparisonNode) (func(*types.Issue) bool, error) {
	priority, err := strconv.Atoi(comp.Value)
	if err != nil {
		return nil, fmt.Errorf("invalid priority: %s", comp.Value)
	}
	switch comp.Op {
	case OpEquals:
		return func(i *types.Issue) bool { return i.Priority == priority }, nil
	case OpNotEquals:
		return func(i *types.Issue) bool { return i.Priority != priority }, nil
	case OpLess:
		return func(i *types.Issue) bool { return i.Priority < priority }, nil
	case OpLessEq:
		return func(i *types.Issue) bool { return i.Priority <= priority }, nil
	case OpGreater:
		return func(i *types.Issue) bool { return i.Priority > priority }, nil
	case OpGreaterEq:
		return func(i *types.Issue) bool { return i.Priority >= priority }, nil
	default:
		return nil, fmt.Errorf("unexpected operator: %s", comp.Op.String())
	}
}

func (e *Evaluator) buildTypePredicate(comp *ComparisonNode) (func(*types.Issue) bool, error) {
	issueType := types.IssueType(strings.ToLower(comp.Value))
	switch comp.Op {
	case OpEquals:
		return func(i *types.Issue) bool { return i.IssueType == issueType }, nil
	case OpNotEquals:
		return func(i *types.Issue) bool { return i.IssueType != issueType }, nil
	default:
		return nil, fmt.Errorf("type does not support %s operator", comp.Op.String())
	}
}

func (e *Evaluator) buildAssigneePredicate(comp *ComparisonNode) (func(*types.Issue) bool, error) {
	value := comp.Value
	isNone := value == "" || strings.ToLower(value) == "none" || strings.ToLower(value) == "null"
	switch comp.Op {
	case OpEquals:
		if isNone {
			return func(i *types.Issue) bool { return i.Assignee == "" }, nil
		}
		return func(i *types.Issue) bool { return strings.EqualFold(i.Assignee, value) }, nil
	case OpNotEquals:
		if isNone {
			return func(i *types.Issue) bool { return i.Assignee != "" }, nil
		}
		return func(i *types.Issue) bool { return !strings.EqualFold(i.Assignee, value) }, nil
	default:
		return nil, fmt.Errorf("assignee does not support %s operator", comp.Op.String())
	}
}

func (e *Evaluator) buildOwnerPredicate(comp *ComparisonNode) (func(*types.Issue) bool, error) {
	value := comp.Value
	switch comp.Op {
	case OpEquals:
		return func(i *types.Issue) bool { return strings.EqualFold(i.Owner, value) }, nil
	case OpNotEquals:
		return func(i *types.Issue) bool { return !strings.EqualFold(i.Owner, value) }, nil
	default:
		return nil, fmt.Errorf("owner does not support %s operator", comp.Op.String())
	}
}

func (e *Evaluator) buildLabelPredicate(comp *ComparisonNode) (func(*types.Issue) bool, error) {
	value := comp.Value
	isNone := value == "" || strings.ToLower(value) == "none" || strings.ToLower(value) == "null"
	switch comp.Op {
	case OpEquals:
		return labelEqualsPredicate(value, isNone), nil
	case OpNotEquals:
		return labelNotEqualsPredicate(value, isNone), nil
	default:
		return nil, fmt.Errorf("label does not support %s operator", comp.Op.String())
	}
}

func labelEqualsPredicate(value string, isNone bool) func(*types.Issue) bool {
	if isNone {
		return func(issue *types.Issue) bool { return len(issue.Labels) == 0 }
	}
	return func(issue *types.Issue) bool {
		for _, label := range issue.Labels {
			if strings.EqualFold(label, value) {
				return true
			}
		}
		return false
	}
}

func labelNotEqualsPredicate(value string, isNone bool) func(*types.Issue) bool {
	if isNone {
		return func(issue *types.Issue) bool { return len(issue.Labels) > 0 }
	}
	return func(issue *types.Issue) bool {
		for _, label := range issue.Labels {
			if strings.EqualFold(label, value) {
				return false
			}
		}
		return true
	}
}

func (e *Evaluator) buildTitlePredicate(comp *ComparisonNode) (func(*types.Issue) bool, error) {
	value := strings.ToLower(comp.Value)
	switch comp.Op {
	case OpEquals:
		return func(i *types.Issue) bool {
			return strings.Contains(strings.ToLower(i.Title), value)
		}, nil
	case OpNotEquals:
		return func(i *types.Issue) bool {
			return !strings.Contains(strings.ToLower(i.Title), value)
		}, nil
	default:
		return nil, fmt.Errorf("title does not support %s operator", comp.Op.String())
	}
}

func (e *Evaluator) buildDescriptionPredicate(comp *ComparisonNode) (func(*types.Issue) bool, error) {
	value := comp.Value
	isNone := value == "" || strings.ToLower(value) == "none" || strings.ToLower(value) == "null"
	switch comp.Op {
	case OpEquals:
		if isNone {
			return func(i *types.Issue) bool { return i.Description == "" }, nil
		}
		return func(i *types.Issue) bool {
			return strings.Contains(strings.ToLower(i.Description), strings.ToLower(value))
		}, nil
	case OpNotEquals:
		if isNone {
			return func(i *types.Issue) bool { return i.Description != "" }, nil
		}
		return func(i *types.Issue) bool {
			return !strings.Contains(strings.ToLower(i.Description), strings.ToLower(value))
		}, nil
	default:
		return nil, fmt.Errorf("description does not support %s operator", comp.Op.String())
	}
}

func (e *Evaluator) buildNotesPredicate(comp *ComparisonNode) (func(*types.Issue) bool, error) {
	value := strings.ToLower(comp.Value)
	switch comp.Op {
	case OpEquals:
		return func(i *types.Issue) bool {
			return strings.Contains(strings.ToLower(i.Notes), value)
		}, nil
	case OpNotEquals:
		return func(i *types.Issue) bool {
			return !strings.Contains(strings.ToLower(i.Notes), value)
		}, nil
	default:
		return nil, fmt.Errorf("notes does not support %s operator", comp.Op.String())
	}
}

func (e *Evaluator) buildCreatedPredicate(comp *ComparisonNode) (func(*types.Issue) bool, error) {
	t, err := e.parseTimeValue(comp)
	if err != nil {
		return nil, fmt.Errorf("invalid created time: %w", err)
	}
	return e.buildTimePredicate(comp.Op, t, func(i *types.Issue) time.Time { return i.CreatedAt })
}

func (e *Evaluator) buildUpdatedPredicate(comp *ComparisonNode) (func(*types.Issue) bool, error) {
	t, err := e.parseTimeValue(comp)
	if err != nil {
		return nil, fmt.Errorf("invalid updated time: %w", err)
	}
	return e.buildTimePredicate(comp.Op, t, func(i *types.Issue) time.Time { return i.UpdatedAt })
}

func (e *Evaluator) buildClosedPredicate(comp *ComparisonNode) (func(*types.Issue) bool, error) {
	t, err := e.parseTimeValue(comp)
	if err != nil {
		return nil, fmt.Errorf("invalid closed time: %w", err)
	}
	return func(i *types.Issue) bool {
		if i.ClosedAt == nil {
			return false
		}
		return e.compareTime(comp.Op, *i.ClosedAt, t)
	}, nil
}

func (e *Evaluator) buildStartedPredicate(comp *ComparisonNode) (func(*types.Issue) bool, error) {
	t, err := e.parseTimeValue(comp)
	if err != nil {
		return nil, fmt.Errorf("invalid started time: %w", err)
	}
	return func(i *types.Issue) bool {
		if i.StartedAt == nil {
			return false
		}
		return e.compareTime(comp.Op, *i.StartedAt, t)
	}, nil
}

func (e *Evaluator) buildTimePredicate(op ComparisonOp, t time.Time, getter func(*types.Issue) time.Time) (func(*types.Issue) bool, error) {
	return func(i *types.Issue) bool {
		return e.compareTime(op, getter(i), t)
	}, nil
}

var timeComparisons = map[ComparisonOp]func(time.Time, time.Time) bool{
	OpEquals:    sameCalendarDay,
	OpNotEquals: differentCalendarDay,
	OpLess:      time.Time.Before,
	OpLessEq:    timeBeforeOrEqual,
	OpGreater:   time.Time.After,
	OpGreaterEq: timeAfterOrEqual,
}

func (e *Evaluator) compareTime(op ComparisonOp, actual, target time.Time) bool {
	compare, ok := timeComparisons[op]
	if !ok {
		return false
	}
	return compare(actual, target)
}

func sameCalendarDay(actual, target time.Time) bool {
	return actual.Year() == target.Year() &&
		actual.Month() == target.Month() &&
		actual.Day() == target.Day()
}

func differentCalendarDay(actual, target time.Time) bool {
	return !sameCalendarDay(actual, target)
}

func timeBeforeOrEqual(actual, target time.Time) bool {
	return actual.Before(target) || actual.Equal(target)
}

func timeAfterOrEqual(actual, target time.Time) bool {
	return actual.After(target) || actual.Equal(target)
}

func (e *Evaluator) buildIDPredicate(comp *ComparisonNode) (func(*types.Issue) bool, error) {
	value := comp.Value
	hasWildcard := strings.HasSuffix(value, "*")
	if hasWildcard {
		prefix := strings.TrimSuffix(value, "*")
		switch comp.Op {
		case OpEquals:
			return func(i *types.Issue) bool { return strings.HasPrefix(i.ID, prefix) }, nil
		case OpNotEquals:
			return func(i *types.Issue) bool { return !strings.HasPrefix(i.ID, prefix) }, nil
		default:
			return nil, fmt.Errorf("id with wildcard only supports = and != operators")
		}
	}
	switch comp.Op {
	case OpEquals:
		return func(i *types.Issue) bool { return i.ID == value }, nil
	case OpNotEquals:
		return func(i *types.Issue) bool { return i.ID != value }, nil
	default:
		return nil, fmt.Errorf("id does not support %s operator", comp.Op.String())
	}
}

func (e *Evaluator) buildSpecPredicate(comp *ComparisonNode) (func(*types.Issue) bool, error) {
	value := comp.Value
	hasWildcard := strings.HasSuffix(value, "*")
	if hasWildcard {
		prefix := strings.TrimSuffix(value, "*")
		switch comp.Op {
		case OpEquals:
			return func(i *types.Issue) bool { return strings.HasPrefix(i.SpecID, prefix) }, nil
		case OpNotEquals:
			return func(i *types.Issue) bool { return !strings.HasPrefix(i.SpecID, prefix) }, nil
		default:
			return nil, fmt.Errorf("spec with wildcard only supports = and != operators")
		}
	}
	switch comp.Op {
	case OpEquals:
		return func(i *types.Issue) bool { return i.SpecID == value }, nil
	case OpNotEquals:
		return func(i *types.Issue) bool { return i.SpecID != value }, nil
	default:
		return nil, fmt.Errorf("spec does not support %s operator", comp.Op.String())
	}
}

func (e *Evaluator) buildBoolPredicate(comp *ComparisonNode, getter func(*types.Issue) bool) (func(*types.Issue) bool, error) {
	val := strings.ToLower(comp.Value)
	var boolVal bool
	switch val {
	case "true", "yes", "1":
		boolVal = true
	case "false", "no", "0":
		boolVal = false
	default:
		return nil, fmt.Errorf("invalid boolean value: %s", comp.Value)
	}

	switch comp.Op {
	case OpEquals:
		return func(i *types.Issue) bool { return getter(i) == boolVal }, nil
	case OpNotEquals:
		return func(i *types.Issue) bool { return getter(i) != boolVal }, nil
	default:
		return nil, fmt.Errorf("boolean field does not support %s operator", comp.Op.String())
	}
}

// Evaluate is a convenience function that parses and evaluates a query string.
func Evaluate(query string) (*QueryResult, error) {
	return EvaluateAt(query, time.Now())
}

// EvaluateAt parses and evaluates a query string with a specific reference time.
func EvaluateAt(query string, now time.Time) (*QueryResult, error) {
	node, err := Parse(query)
	if err != nil {
		return nil, err
	}
	eval := NewEvaluator(now)
	return eval.Evaluate(node)
}
