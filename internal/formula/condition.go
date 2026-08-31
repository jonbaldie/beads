// Package formula provides condition evaluation for gates and loops.
//
// Conditions are intentionally limited to keep evaluation decidable:
//   - Step status checks: step.status == 'complete'
//   - Step output access: step.output.approved == true
//   - Aggregates: children(step).all(status == 'complete')
//   - External checks: file.exists('go.mod'), env.CI == 'true'
//
// No arbitrary code execution is allowed.
package formula

import (
	"fmt"
	"regexp"
	"strings"
)

// ConditionResult represents the result of evaluating a condition.
type ConditionResult struct {
	// Satisfied is true if the condition is met.
	Satisfied bool

	// Reason explains why the condition is satisfied or not.
	Reason string
}

// StepState represents the runtime state of a step for condition evaluation.
type StepState struct {
	// ID is the step identifier.
	ID string

	// Status is the step status: pending, in_progress, complete, failed.
	Status string

	// Output is the structured output from the step (if complete).
	// Keys are dot-separated paths, values are the output values.
	Output map[string]interface{}

	// Children are the child step states (for aggregate conditions).
	Children []*StepState
}

// ConditionContext provides the evaluation context for conditions.
type ConditionContext struct {
	// Steps maps step ID to step state.
	Steps map[string]*StepState

	// CurrentStep is the step being gated (for relative references).
	CurrentStep string

	// Vars are the formula variables (for variable substitution).
	Vars map[string]string
}

// Operator represents a comparison operator.
type Operator string

const (
	OpEqual        Operator = "=="
	OpNotEqual     Operator = "!="
	OpGreater      Operator = ">"
	OpGreaterEqual Operator = ">="
	OpLess         Operator = "<"
	OpLessEqual    Operator = "<="
)

// Condition represents a parsed condition expression.
type Condition struct {
	// Raw is the original condition string.
	Raw string

	// Type is the condition type: field, aggregate, external.
	Type ConditionType

	// For field conditions:
	StepRef  string   // Step ID reference (e.g., "review", "step" for current)
	Field    string   // Field path (e.g., "status", "output.approved")
	Operator Operator // Comparison operator
	Value    string   // Expected value

	// For aggregate conditions:
	AggregateFunc string // Function: all, any, count
	AggregateOver string // What to aggregate: children, descendants, steps

	// For external conditions:
	ExternalType string // file.exists, env
	ExternalArg  string // Argument (path or env var name)
}

// ConditionType categorizes conditions.
type ConditionType string

const (
	ConditionTypeField     ConditionType = "field"
	ConditionTypeAggregate ConditionType = "aggregate"
	ConditionTypeExternal  ConditionType = "external"
)

// Patterns for parsing conditions.
var (
	// step.status == 'complete' or review.output.approved == true or test.output.errors.count == 0
	fieldPattern = regexp.MustCompile(`^(\w+(?:\.\w+)*)\s*([=!<>]+)\s*(.+)$`)

	// children(step).all(status == 'complete')
	aggregatePattern = regexp.MustCompile(`^(children|descendants|steps)\((\w+)\)\.(all|any|count)\((.+)\)(.*)$`)

	// file.exists('go.mod')
	fileExistsPattern = regexp.MustCompile(`^file\.exists\(['"](.+)['"]\)$`)

	// env.CI == 'true'
	envPattern = regexp.MustCompile(`^env\.(\w+)\s*([=!<>]+)\s*(.+)$`)

	// steps.complete >= 3
	stepsStatPattern = regexp.MustCompile(`^steps\.(\w+)\s*([=!<>]+)\s*(\d+)$`)

	// children(x).count(...) >= 3 (trailing comparison)
	countComparePattern = regexp.MustCompile(`\s*([=!<>]+)\s*(\d+)$`)
)

// ParseCondition parses a condition string into a Condition struct.
func ParseCondition(expr string) (*Condition, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("empty condition")
	}

	if condition, matched := parseExternalCondition(expr); matched {
		return condition, nil
	}
	if condition, matched, err := parseAggregateCondition(expr); matched {
		return condition, err
	}
	if condition, matched := parseStepsStatCondition(expr); matched {
		return condition, nil
	}
	if condition, matched := parseFieldCondition(expr); matched {
		return condition, nil
	}
	return nil, fmt.Errorf("unrecognized condition format: %s", expr)
}

func parseExternalCondition(expr string) (*Condition, bool) {
	if m := fileExistsPattern.FindStringSubmatch(expr); m != nil {
		return &Condition{
			Raw:          expr,
			Type:         ConditionTypeExternal,
			ExternalType: "file.exists",
			ExternalArg:  m[1],
		}, true
	}
	if m := envPattern.FindStringSubmatch(expr); m != nil {
		return &Condition{
			Raw:          expr,
			Type:         ConditionTypeExternal,
			ExternalType: "env",
			ExternalArg:  m[1],
			Operator:     Operator(m[2]),
			Value:        unquote(m[3]),
		}, true
	}
	return nil, false
}

func parseAggregateCondition(expr string) (*Condition, bool, error) {
	m := aggregatePattern.FindStringSubmatch(expr)
	if m == nil {
		return nil, false, nil
	}
	innerCond, err := ParseCondition(m[4])
	if err != nil {
		return nil, true, fmt.Errorf("parsing aggregate inner condition: %w", err)
	}
	condition := &Condition{
		Raw:           expr,
		Type:          ConditionTypeAggregate,
		AggregateOver: m[1],
		StepRef:       m[2],
		AggregateFunc: m[3],
		Field:         innerCond.Field,
		Operator:      innerCond.Operator,
		Value:         innerCond.Value,
	}
	if m[5] != "" {
		if countMatch := countComparePattern.FindStringSubmatch(m[5]); countMatch != nil {
			condition.AggregateFunc = "count"
			condition.Operator = Operator(countMatch[1])
			condition.Value = countMatch[2]
		}
	}
	return condition, true, nil
}

func parseStepsStatCondition(expr string) (*Condition, bool) {
	m := stepsStatPattern.FindStringSubmatch(expr)
	if m == nil {
		return nil, false
	}
	return &Condition{
		Raw:           expr,
		Type:          ConditionTypeAggregate,
		AggregateOver: "steps",
		AggregateFunc: "count",
		Field:         m[1],
		Operator:      Operator(m[2]),
		Value:         m[3],
	}, true
}

func parseFieldCondition(expr string) (*Condition, bool) {
	m := fieldPattern.FindStringSubmatch(expr)
	if m == nil {
		return nil, false
	}
	stepRef, field := parseFieldPath(m[1])
	return &Condition{
		Raw:      expr,
		Type:     ConditionTypeField,
		StepRef:  stepRef,
		Field:    field,
		Operator: Operator(m[2]),
		Value:    unquote(m[3]),
	}, true
}

func parseFieldPath(fieldPath string) (string, string) {
	parts := strings.SplitN(fieldPath, ".", 2)
	stepRef := "step"
	field := fieldPath
	if len(parts) < 2 {
		return stepRef, field
	}
	if parts[0] == "step" {
		return stepRef, parts[1]
	}
	if parts[0] == "output" {
		return stepRef, fieldPath
	}
	return parts[0], parts[1]
}
