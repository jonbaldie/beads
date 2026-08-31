// Package formula provides parsing and validation for .formula.json files.
//
// Formulas are high-level workflow templates that compile down to proto beads.
// They support:
//   - Variable definitions with defaults and validation
//   - Step definitions that become issue hierarchies
//   - Composition rules for bonding formulas together
//   - Inheritance via extends
//
// Example .formula.json:
//
//	{
//	  "formula": "mol-feature",
//	  "description": "Standard feature workflow",
//	  "version": 1,
//	  "type": "workflow",
//	  "vars": {
//	    "component": {
//	      "description": "Component name",
//	      "required": true
//	    }
//	  },
//	  "steps": [
//	    {"id": "design", "title": "Design {{component}}", "type": "task"},
//	    {"id": "implement", "title": "Implement {{component}}", "depends_on": ["design"]}
//	  ]
//	}
package formula

import (
	"fmt"
	"strings"
)

// Validate checks the formula for structural errors.
func (f *Formula) Validate() error {
	var errs []string
	f.validateHeader(&errs)
	f.validateVars(&errs)
	stepIDLocations := f.validateSteps(&errs)
	f.validateStepRefs(stepIDLocations, &errs)
	f.validateCompose(stepIDLocations, &errs)
	if len(errs) > 0 {
		return fmt.Errorf("formula validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

func (f *Formula) validateHeader(errs *[]string) {
	if f.Formula == "" {
		*errs = append(*errs, "formula: name is required")
	}
	if f.Version < 1 {
		*errs = append(*errs, "version: must be >= 1")
	}
	if f.Type != "" && !f.Type.IsValid() {
		*errs = append(*errs, fmt.Sprintf("type: invalid value %q (must be workflow, expansion, or aspect)", f.Type))
	}
}

func (f *Formula) validateVars(errs *[]string) {
	for name, v := range f.Vars {
		if name == "" {
			*errs = append(*errs, "vars: variable name cannot be empty")
			continue
		}
		if v.Required && v.Default != nil {
			*errs = append(*errs, fmt.Sprintf("vars.%s: cannot have both required:true and default", name))
		}
	}
}

func (f *Formula) validateSteps(errs *[]string) map[string]string {
	// Track where each ID was first defined for better error messages.
	stepIDLocations := make(map[string]string)
	for i, step := range f.Steps {
		prefix := fmt.Sprintf("steps[%d]", i)
		if step.ID == "" {
			*errs = append(*errs, fmt.Sprintf("%s: id is required", prefix))
			continue
		}
		if firstLoc, exists := stepIDLocations[step.ID]; exists {
			*errs = append(*errs, fmt.Sprintf("%s: duplicate id %q (first defined at %s)", prefix, step.ID, firstLoc))
		} else {
			stepIDLocations[step.ID] = prefix
		}
		if step.Title == "" && step.Expand == "" {
			*errs = append(*errs, fmt.Sprintf("%s (%s): title is required (unless using expand)", prefix, step.ID))
		}
		if step.Priority != nil && (*step.Priority < 0 || *step.Priority > 4) {
			*errs = append(*errs, fmt.Sprintf("%s (%s): priority must be 0-4", prefix, step.ID))
		}
		collectChildIDs(step.Children, stepIDLocations, errs, prefix)
	}
	return stepIDLocations
}

func (f *Formula) validateStepRefs(stepIDLocations map[string]string, errs *[]string) {
	for i, step := range f.Steps {
		for _, dep := range step.DependsOn {
			if _, exists := stepIDLocations[dep]; !exists {
				*errs = append(*errs, fmt.Sprintf("steps[%d] (%s): depends_on references unknown step %q", i, step.ID, dep))
			}
		}
		for _, need := range step.Needs {
			if _, exists := stepIDLocations[need]; !exists {
				*errs = append(*errs, fmt.Sprintf("steps[%d] (%s): needs references unknown step %q", i, step.ID, need))
			}
		}
		// Valid formats: "all-children", "any-children", "children-of(step-id)"
		if step.WaitsFor != "" {
			if err := validateWaitsFor(step.WaitsFor, stepIDLocations); err != nil {
				*errs = append(*errs, fmt.Sprintf("steps[%d] (%s): %s", i, step.ID, err.Error()))
			}
		}
		if step.OnComplete != nil {
			validateOnComplete(step.OnComplete, errs, fmt.Sprintf("steps[%d] (%s)", i, step.ID))
		}
		validateChildDependsOn(step.Children, stepIDLocations, errs, fmt.Sprintf("steps[%d]", i))
	}
}

func (f *Formula) validateCompose(stepIDLocations map[string]string, errs *[]string) {
	if f.Compose == nil {
		return
	}
	validateBondPoints(f.Compose.BondPoints, stepIDLocations, errs)
	validateComposeHooks(f.Compose.Hooks, errs)
}

func validateBondPoints(points []*BondPoint, stepIDLocations map[string]string, errs *[]string) {
	for i, bp := range points {
		prefix := fmt.Sprintf("compose.bond_points[%d] (%s)", i, bp.ID)
		if bp.ID == "" {
			*errs = append(*errs, fmt.Sprintf("compose.bond_points[%d]: id is required", i))
		}
		if bp.AfterStep != "" && bp.BeforeStep != "" {
			*errs = append(*errs, fmt.Sprintf("%s: cannot have both after_step and before_step", prefix))
		}
		validateBondPointRef(bp.AfterStep, "after_step", prefix, stepIDLocations, errs)
		validateBondPointRef(bp.BeforeStep, "before_step", prefix, stepIDLocations, errs)
	}
}

func validateBondPointRef(step, field, prefix string, stepIDLocations map[string]string, errs *[]string) {
	if step == "" {
		return
	}
	if _, exists := stepIDLocations[step]; !exists {
		*errs = append(*errs, fmt.Sprintf("%s: %s references unknown step %q", prefix, field, step))
	}
}

func validateComposeHooks(hooks []*Hook, errs *[]string) {
	for i, hook := range hooks {
		if hook.Trigger == "" {
			*errs = append(*errs, fmt.Sprintf("compose.hooks[%d]: trigger is required", i))
		}
		if hook.Attach == "" {
			*errs = append(*errs, fmt.Sprintf("compose.hooks[%d]: attach is required", i))
		}
	}
}

// collectChildIDs recursively collects step IDs from children.
// idLocations maps ID -> location where first defined (for better duplicate error messages).
func collectChildIDs(children []*Step, idLocations map[string]string, errs *[]string, prefix string) {
	for i, child := range children {
		childPrefix := fmt.Sprintf("%s.children[%d]", prefix, i)
		if child.ID == "" {
			*errs = append(*errs, fmt.Sprintf("%s: id is required", childPrefix))
			continue
		}
		if firstLoc, exists := idLocations[child.ID]; exists {
			*errs = append(*errs, fmt.Sprintf("%s: duplicate id %q (first defined at %s)", childPrefix, child.ID, firstLoc))
		} else {
			idLocations[child.ID] = childPrefix
		}

		if child.Title == "" && child.Expand == "" {
			*errs = append(*errs, fmt.Sprintf("%s (%s): title is required", childPrefix, child.ID))
		}

		// Validate priority range for children
		if child.Priority != nil && (*child.Priority < 0 || *child.Priority > 4) {
			*errs = append(*errs, fmt.Sprintf("%s (%s): priority must be 0-4", childPrefix, child.ID))
		}

		collectChildIDs(child.Children, idLocations, errs, childPrefix)
	}
}

// ParseWaitsFor parses a waits_for value into its components.
// Returns nil if the value is empty.
func ParseWaitsFor(value string) *WaitsForSpec {
	if value == "" {
		return nil
	}

	// Simple gate types - spawner inferred from needs
	if value == "all-children" || value == "any-children" {
		return &WaitsForSpec{Gate: value}
	}

	// children-of(step-id) syntax
	if strings.HasPrefix(value, "children-of(") && strings.HasSuffix(value, ")") {
		stepID := value[len("children-of(") : len(value)-1]
		return &WaitsForSpec{
			Gate:      "all-children", // Default gate type
			SpawnerID: stepID,
		}
	}

	// Invalid - return nil (validation should have caught this)
	return nil
}

// validateWaitsFor validates the waits_for field value.
// Valid formats:
//   - "all-children": wait for all dynamically-bonded children
//   - "any-children": wait for first child to complete
//   - "children-of(step-id)": wait for children of a specific step
func validateWaitsFor(value string, stepIDLocations map[string]string) error {
	// Simple gate types
	if value == "all-children" || value == "any-children" {
		return nil
	}

	// children-of(step-id) syntax
	if strings.HasPrefix(value, "children-of(") && strings.HasSuffix(value, ")") {
		stepID := value[len("children-of(") : len(value)-1]
		if stepID == "" {
			return fmt.Errorf("waits_for children-of() requires a step ID")
		}
		if _, exists := stepIDLocations[stepID]; !exists {
			return fmt.Errorf("waits_for references unknown step %q in children-of()", stepID)
		}
		return nil
	}

	return fmt.Errorf("waits_for has invalid value %q (must be all-children, any-children, or children-of(step-id))", value)
}

// validateChildDependsOn recursively validates depends_on and needs references for children.
func validateChildDependsOn(children []*Step, idLocations map[string]string, errs *[]string, prefix string) {
	for i, child := range children {
		childPrefix := fmt.Sprintf("%s.children[%d]", prefix, i)
		for _, dep := range child.DependsOn {
			if _, exists := idLocations[dep]; !exists {
				*errs = append(*errs, fmt.Sprintf("%s (%s): depends_on references unknown step %q", childPrefix, child.ID, dep))
			}
		}
		// Validate needs field
		for _, need := range child.Needs {
			if _, exists := idLocations[need]; !exists {
				*errs = append(*errs, fmt.Sprintf("%s (%s): needs references unknown step %q", childPrefix, child.ID, need))
			}
		}
		// Validate waits_for field
		if child.WaitsFor != "" {
			if err := validateWaitsFor(child.WaitsFor, idLocations); err != nil {
				*errs = append(*errs, fmt.Sprintf("%s (%s): %s", childPrefix, child.ID, err.Error()))
			}
		}
		// Validate on_complete field
		if child.OnComplete != nil {
			validateOnComplete(child.OnComplete, errs, fmt.Sprintf("%s (%s)", childPrefix, child.ID))
		}
		validateChildDependsOn(child.Children, idLocations, errs, childPrefix)
	}
}

// validateOnComplete validates an OnCompleteSpec.
func validateOnComplete(oc *OnCompleteSpec, errs *[]string, prefix string) {
	// Check that for_each and bond are both present or both absent
	if oc.ForEach != "" && oc.Bond == "" {
		*errs = append(*errs, fmt.Sprintf("%s.on_complete: bond is required when for_each is set", prefix))
	}
	if oc.ForEach == "" && oc.Bond != "" {
		*errs = append(*errs, fmt.Sprintf("%s.on_complete: for_each is required when bond is set", prefix))
	}

	// Validate for_each path format
	if oc.ForEach != "" {
		if !strings.HasPrefix(oc.ForEach, "output.") {
			*errs = append(*errs, fmt.Sprintf("%s.on_complete: for_each must start with 'output.' (got %q)", prefix, oc.ForEach))
		}
	}

	// Check parallel and sequential are mutually exclusive
	if oc.Parallel && oc.Sequential {
		*errs = append(*errs, fmt.Sprintf("%s.on_complete: cannot set both parallel and sequential", prefix))
	}
}

// GetRequiredVars returns the names of all required variables.
func (f *Formula) GetRequiredVars() []string {
	var required []string
	for name, v := range f.Vars {
		if v.Required {
			required = append(required, name)
		}
	}
	return required
}

// GetStepByID finds a step by its ID (searches recursively).
func (f *Formula) GetStepByID(id string) *Step {
	for _, step := range f.Steps {
		if found := findStepByID(step, id); found != nil {
			return found
		}
	}
	return nil
}

// findStepByID recursively searches for a step by ID.
func findStepByID(step *Step, id string) *Step {
	if step.ID == id {
		return step
	}
	for _, child := range step.Children {
		if found := findStepByID(child, id); found != nil {
			return found
		}
	}
	return nil
}

// StringPtr returns a pointer to s. Useful for constructing VarDef literals.
func StringPtr(s string) *string { return &s }

// GetBondPoint finds a bond point by ID.
func (f *Formula) GetBondPoint(id string) *BondPoint {
	if f.Compose == nil {
		return nil
	}
	for _, bp := range f.Compose.BondPoints {
		if bp.ID == id {
			return bp
		}
	}
	return nil
}
