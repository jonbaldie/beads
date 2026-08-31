// Package formula provides control flow operators for step transformation.
//
// Control flow operators enable:
//   - loop: Repeat a body of steps (fixed count or conditional)
//   - branch: Fork-join parallel execution patterns
//   - gate: Conditional waits before steps proceed
//
// These operators are applied during formula cooking to transform
// the step graph before creating the proto bead.
package formula

import (
	"fmt"
)

// ApplyLoops expands loop bodies in a formula's steps.
// Fixed-count loops expand the body N times with indexed step IDs.
// Conditional loops expand once and add a "loop:until" label for runtime evaluation.
// Returns a new steps slice with loops expanded.
func ApplyLoops(steps []*Step) ([]*Step, error) {
	result := make([]*Step, 0, len(steps))

	for _, step := range steps {
		if step.Loop == nil {
			// No loop - recursively process children
			clone := cloneStep(step)
			if len(step.Children) > 0 {
				children, err := ApplyLoops(step.Children)
				if err != nil {
					return nil, err
				}
				clone.Children = children
			}
			result = append(result, clone)
			continue
		}

		// Validate loop spec
		if err := validateLoopSpec(step.Loop, step.ID); err != nil {
			return nil, err
		}

		// Expand the loop
		expanded, err := expandLoop(step)
		if err != nil {
			return nil, err
		}
		result = append(result, expanded...)
	}

	return result, nil
}

// validateLoopSpec checks that a loop spec is valid.
func validateLoopSpec(loop *LoopSpec, stepID string) error {
	if len(loop.Body) == 0 {
		return fmt.Errorf("loop %q: body is required", stepID)
	}

	if err := validateLoopKind(loop, stepID); err != nil {
		return err
	}
	if err := validateLoopBounds(loop, stepID); err != nil {
		return err
	}
	return validateLoopExpressions(loop, stepID)
}

func validateLoopKind(loop *LoopSpec, stepID string) error {
	loopTypes := 0
	if loop.Count > 0 {
		loopTypes++
	}
	if loop.Until != "" {
		loopTypes++
	}
	if loop.Range != "" {
		loopTypes++
	}
	if loopTypes == 0 {
		return fmt.Errorf("loop %q: one of count, until, or range is required", stepID)
	}
	if loopTypes > 1 {
		return fmt.Errorf("loop %q: only one of count, until, or range can be specified", stepID)
	}
	return nil
}

func validateLoopBounds(loop *LoopSpec, stepID string) error {
	if loop.Until != "" && loop.Max == 0 {
		return fmt.Errorf("loop %q: max is required when until is set", stepID)
	}
	if loop.Count < 0 {
		return fmt.Errorf("loop %q: count must be positive", stepID)
	}
	if loop.Max < 0 {
		return fmt.Errorf("loop %q: max must be positive", stepID)
	}
	return nil
}

func validateLoopExpressions(loop *LoopSpec, stepID string) error {
	if loop.Until != "" {
		if _, err := ParseCondition(loop.Until); err != nil {
			return fmt.Errorf("loop %q: invalid until condition %q: %w", stepID, loop.Until, err)
		}
	}
	if loop.Range != "" {
		if err := ValidateRange(loop.Range); err != nil {
			return fmt.Errorf("loop %q: invalid range %q: %w", stepID, loop.Range, err)
		}
	}
	return nil
}
