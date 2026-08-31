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
	"os"
	"strconv"
	"strings"
)

// Evaluate evaluates the condition against the given context.
func (c *Condition) Evaluate(ctx *ConditionContext) (*ConditionResult, error) {
	switch c.Type {
	case ConditionTypeField:
		return c.evaluateField(ctx)
	case ConditionTypeAggregate:
		return c.evaluateAggregate(ctx)
	case ConditionTypeExternal:
		return c.evaluateExternal(ctx)
	default:
		return nil, fmt.Errorf("unknown condition type: %s", c.Type)
	}
}

func (c *Condition) evaluateField(ctx *ConditionContext) (*ConditionResult, error) {
	// Resolve step reference
	stepID := c.StepRef
	if stepID == "step" {
		stepID = ctx.CurrentStep
	}

	step, ok := ctx.Steps[stepID]
	if !ok {
		return &ConditionResult{
			Satisfied: false,
			Reason:    fmt.Sprintf("step %q not found", stepID),
		}, nil
	}

	// Get the field value
	var actual interface{}
	if c.Field == "status" {
		actual = step.Status
	} else if strings.HasPrefix(c.Field, "output.") {
		path := strings.TrimPrefix(c.Field, "output.")
		actual = getNestedValue(step.Output, path)
	} else {
		return nil, fmt.Errorf("unknown field: %s", c.Field)
	}

	// Compare
	satisfied, reason := compare(actual, c.Operator, c.Value)
	return &ConditionResult{
		Satisfied: satisfied,
		Reason:    reason,
	}, nil
}

func (c *Condition) evaluateAggregate(ctx *ConditionContext) (*ConditionResult, error) {
	steps, missing := c.aggregateSteps(ctx)
	if missing != nil {
		return missing, nil
	}
	return c.applyAggregateFunction(steps)
}

func (c *Condition) aggregateSteps(ctx *ConditionContext) ([]*StepState, *ConditionResult) {
	switch c.AggregateOver {
	case "children":
		return c.aggregateReferencedSteps(ctx, false)
	case "steps":
		return allContextSteps(ctx), nil
	case "descendants":
		return c.aggregateReferencedSteps(ctx, true)
	default:
		return nil, nil
	}
}

func (c *Condition) aggregateReferencedSteps(ctx *ConditionContext, descendants bool) ([]*StepState, *ConditionResult) {
	stepID := c.StepRef
	if stepID == "step" {
		stepID = ctx.CurrentStep
	}
	parent, ok := ctx.Steps[stepID]
	if !ok {
		return nil, &ConditionResult{
			Satisfied: false,
			Reason:    fmt.Sprintf("step %q not found", stepID),
		}
	}
	if descendants {
		return collectDescendants(parent), nil
	}
	return parent.Children, nil
}

func allContextSteps(ctx *ConditionContext) []*StepState {
	var steps []*StepState
	for _, step := range ctx.Steps {
		steps = append(steps, step)
	}
	return steps
}

func (c *Condition) applyAggregateFunction(steps []*StepState) (*ConditionResult, error) {
	switch c.AggregateFunc {
	case "all":
		return c.evaluateAllAggregate(steps)
	case "any":
		return c.evaluateAnyAggregate(steps)
	case "count":
		return c.evaluateCountAggregate(steps)
	default:
		return nil, fmt.Errorf("unknown aggregate function: %s", c.AggregateFunc)
	}
}

func (c *Condition) evaluateAllAggregate(steps []*StepState) (*ConditionResult, error) {
	if len(steps) == 0 {
		return &ConditionResult{
			Satisfied: false,
			Reason:    fmt.Sprintf("no %s to evaluate", c.AggregateOver),
		}, nil
	}
	for _, step := range steps {
		satisfied, _ := matchStep(step, c.Field, c.Operator, c.Value)
		if !satisfied {
			return &ConditionResult{
				Satisfied: false,
				Reason:    fmt.Sprintf("step %q does not match: %s %s %s", step.ID, c.Field, c.Operator, c.Value),
			}, nil
		}
	}
	return &ConditionResult{
		Satisfied: true,
		Reason:    fmt.Sprintf("all %d %s match", len(steps), c.AggregateOver),
	}, nil
}

func (c *Condition) evaluateAnyAggregate(steps []*StepState) (*ConditionResult, error) {
	for _, step := range steps {
		satisfied, _ := matchStep(step, c.Field, c.Operator, c.Value)
		if satisfied {
			return &ConditionResult{
				Satisfied: true,
				Reason:    fmt.Sprintf("step %q matches: %s %s %s", step.ID, c.Field, c.Operator, c.Value),
			}, nil
		}
	}
	return &ConditionResult{
		Satisfied: false,
		Reason:    fmt.Sprintf("no steps match: %s %s %s", c.Field, c.Operator, c.Value),
	}, nil
}

func (c *Condition) evaluateCountAggregate(steps []*StepState) (*ConditionResult, error) {
	count := 0
	for _, step := range steps {
		if countAggregateMatches(c, step) {
			count++
		}
	}
	expected, err := strconv.Atoi(c.Value)
	if err != nil {
		return nil, fmt.Errorf("count comparison requires integer value, got %q: %w", c.Value, err)
	}
	satisfied, reason := compareInt(count, c.Operator, expected)
	return &ConditionResult{
		Satisfied: satisfied,
		Reason:    reason,
	}, nil
}

func countAggregateMatches(condition *Condition, step *StepState) bool {
	if condition.AggregateOver == "steps" && isStepStatusAggregateField(condition.Field) {
		return step.Status == condition.Field
	}
	satisfied, _ := matchStep(step, condition.Field, OpEqual, condition.Value)
	return satisfied
}

func isStepStatusAggregateField(field string) bool {
	switch field {
	case "complete", "failed", "pending", "in_progress":
		return true
	default:
		return false
	}
}

func (c *Condition) evaluateExternal(ctx *ConditionContext) (*ConditionResult, error) {
	switch c.ExternalType {
	case "file.exists":
		path := c.ExternalArg
		// Substitute variables
		for k, v := range ctx.Vars {
			path = strings.ReplaceAll(path, "{{"+k+"}}", v)
		}
		_, err := os.Stat(path)
		exists := err == nil
		return &ConditionResult{
			Satisfied: exists,
			Reason:    fmt.Sprintf("file %q exists: %v", path, exists),
		}, nil

	case "env":
		actual := os.Getenv(c.ExternalArg)
		satisfied, reason := compare(actual, c.Operator, c.Value)
		return &ConditionResult{
			Satisfied: satisfied,
			Reason:    reason,
		}, nil
	}

	return nil, fmt.Errorf("unknown external type: %s", c.ExternalType)
}

// Helper functions

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func getNestedValue(m map[string]interface{}, path string) interface{} {
	if m == nil {
		return nil
	}
	parts := strings.Split(path, ".")
	var current interface{} = m
	for _, part := range parts {
		if cm, ok := current.(map[string]interface{}); ok {
			current = cm[part]
		} else {
			return nil
		}
	}
	return current
}

func compare(actual interface{}, op Operator, expected string) (bool, string) {
	if actual == nil {
		return compareNil(op, expected)
	}

	if b, ok := actual.(bool); ok {
		return compareBool(b, op, expected)
	}

	actualStr := fmt.Sprintf("%v", actual)
	switch op {
	case OpEqual:
		return compareStringEquality(actualStr, op, expected, actualStr == expected)
	case OpNotEqual:
		return compareStringEquality(actualStr, op, expected, actualStr != expected)
	case OpGreater, OpGreaterEqual, OpLess, OpLessEqual:
		return compareOrdered(actualStr, op, expected)
	}
	return false, fmt.Sprintf("unknown operator: %s", op)
}

func compareNil(op Operator, expected string) (bool, string) {
	switch op {
	case OpEqual:
		satisfied := expected == ""
		return satisfied, fmt.Sprintf("nil %s %q: %v", op, expected, satisfied)
	case OpNotEqual:
		satisfied := expected != ""
		return satisfied, fmt.Sprintf("nil %s %q: %v", op, expected, satisfied)
	default:
		return false, fmt.Sprintf("nil cannot be compared with %s", op)
	}
}

func compareBool(actual bool, op Operator, expected string) (bool, string) {
	actualStr := strconv.FormatBool(actual)
	satisfied := actualStr == expected
	if op == OpNotEqual {
		satisfied = actualStr != expected
	}
	return satisfied, fmt.Sprintf("%v %s %q: %v", actual, op, expected, satisfied)
}

func compareStringEquality(actual string, op Operator, expected string, satisfied bool) (bool, string) {
	return satisfied, fmt.Sprintf("%q %s %q: %v", actual, op, expected, satisfied)
}

func compareOrdered(actual string, op Operator, expected string) (bool, string) {
	actualNum, err1 := strconv.ParseFloat(actual, 64)
	expectedNum, err2 := strconv.ParseFloat(expected, 64)
	if err1 == nil && err2 == nil {
		return compareFloat(actualNum, op, expectedNum)
	}
	return compareString(actual, op, expected)
}

func compareInt(actual int, op Operator, expected int) (bool, string) {
	var satisfied bool
	switch op {
	case OpEqual:
		satisfied = actual == expected
	case OpNotEqual:
		satisfied = actual != expected
	case OpGreater:
		satisfied = actual > expected
	case OpGreaterEqual:
		satisfied = actual >= expected
	case OpLess:
		satisfied = actual < expected
	case OpLessEqual:
		satisfied = actual <= expected
	}
	return satisfied, fmt.Sprintf("%d %s %d: %v", actual, op, expected, satisfied)
}

func compareFloat(actual float64, op Operator, expected float64) (bool, string) {
	var satisfied bool
	switch op {
	case OpEqual:
		satisfied = actual == expected
	case OpNotEqual:
		satisfied = actual != expected
	case OpGreater:
		satisfied = actual > expected
	case OpGreaterEqual:
		satisfied = actual >= expected
	case OpLess:
		satisfied = actual < expected
	case OpLessEqual:
		satisfied = actual <= expected
	}
	return satisfied, fmt.Sprintf("%v %s %v: %v", actual, op, expected, satisfied)
}

func compareString(actual string, op Operator, expected string) (bool, string) {
	var satisfied bool
	switch op {
	case OpEqual:
		satisfied = actual == expected
	case OpNotEqual:
		satisfied = actual != expected
	case OpGreater:
		satisfied = actual > expected
	case OpGreaterEqual:
		satisfied = actual >= expected
	case OpLess:
		satisfied = actual < expected
	case OpLessEqual:
		satisfied = actual <= expected
	}
	return satisfied, fmt.Sprintf("%q %s %q: %v", actual, op, expected, satisfied)
}

func matchStep(s *StepState, field string, op Operator, expected string) (bool, string) {
	var actual interface{}
	if field == "status" {
		actual = s.Status
	} else if strings.HasPrefix(field, "output.") {
		path := strings.TrimPrefix(field, "output.")
		actual = getNestedValue(s.Output, path)
	} else {
		// Direct field name might be a status shorthand
		actual = s.Status
	}
	return compare(actual, op, expected)
}

func collectDescendants(s *StepState) []*StepState {
	var result []*StepState
	for _, child := range s.Children {
		result = append(result, child)
		result = append(result, collectDescendants(child)...)
	}
	return result
}

// EvaluateCondition is a convenience function that parses and evaluates a condition.
func EvaluateCondition(expr string, ctx *ConditionContext) (*ConditionResult, error) {
	cond, err := ParseCondition(expr)
	if err != nil {
		return nil, err
	}
	return cond.Evaluate(ctx)
}
