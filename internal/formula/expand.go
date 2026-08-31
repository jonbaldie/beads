// Package formula provides expansion operators for macro-style step transformation.
//
// Expansion operators replace target steps with template-expanded steps.
// Unlike advice operators which insert steps around targets, expansion
// operators completely replace the target with the expansion template.
//
// Two operators are supported:
//   - expand: Apply template to a single target step
//   - map: Apply template to all steps matching a pattern
//
// Templates use {target} and {target.description} placeholders that are
// substituted with the target step's values during expansion.
//
// A maximum expansion depth (default 5) prevents runaway nested expansions.
// This allows massive work generation while providing a safety bound.
package formula

import (
	"fmt"
	"strings"
)

// DefaultMaxExpansionDepth is the maximum depth for recursive template expansion.
// This prevents runaway nested expansions while still allowing substantial work
// generation. The limit applies to template children, not to expansion rules.
const DefaultMaxExpansionDepth = 5

// ApplyExpansions applies all expand and map rules to a formula's steps.
// Returns a new steps slice with expansions applied.
// The original steps slice is not modified.
//
// The parser is used to load referenced expansion formulas by name.
// If parser is nil, no expansions are applied.
func ApplyExpansions(steps []*Step, compose *ComposeRules, parser *Parser) ([]*Step, error) {
	if compose == nil || parser == nil {
		return steps, nil
	}

	if len(compose.Expand) == 0 && len(compose.Map) == 0 {
		return steps, nil
	}

	result, expanded, err := applyExpandRules(steps, compose.Expand, parser)
	if err != nil {
		return nil, err
	}

	result, err = applyMapRules(result, compose.Map, parser, expanded)
	if err != nil {
		return nil, err
	}

	return validateExpandedSteps(result)
}

func applyExpandRules(steps []*Step, rules []*ExpandRule, parser *Parser) ([]*Step, map[string]bool, error) {
	result := steps
	expanded := make(map[string]bool)
	stepMap := buildStepMap(result)

	for _, rule := range rules {
		targetStep, ok := stepMap[rule.Target]
		if !ok {
			return nil, nil, fmt.Errorf("expand: target step %q not found", rule.Target)
		}
		if expanded[rule.Target] {
			continue
		}

		expFormula, err := loadExpansionFormula(parser, "expand", rule.With)
		if err != nil {
			return nil, nil, err
		}
		expandedSteps, err := expandTarget(targetStep, expFormula, rule.Vars)
		if err != nil {
			return nil, nil, fmt.Errorf("expand %q: %w", rule.Target, err)
		}

		result = replaceExpandedTarget(result, rule.Target, targetStep, expandedSteps)
		expanded[rule.Target] = true
		stepMap = buildStepMap(result)
	}

	return result, expanded, nil
}

func applyMapRules(steps []*Step, rules []*MapRule, parser *Parser, expanded map[string]bool) ([]*Step, error) {
	result := steps
	for _, rule := range rules {
		expFormula, err := loadExpansionFormula(parser, "map", rule.With)
		if err != nil {
			return nil, err
		}

		vars := mergeVars(expFormula, rule.Vars)
		stepMap := buildStepMap(result)
		toExpand := matchingExpansionTargets(stepMap, rule.Select, expanded)
		for _, targetStep := range toExpand {
			expandedSteps, err := expandTargetWithVars(targetStep, expFormula, vars)
			if err != nil {
				return nil, fmt.Errorf("map %q -> %q: %w", rule.Select, targetStep.ID, err)
			}

			result = replaceExpandedTarget(result, targetStep.ID, targetStep, expandedSteps)
			expanded[targetStep.ID] = true
		}
	}
	return result, nil
}

func loadExpansionFormula(parser *Parser, operator, name string) (*Formula, error) {
	expFormula, err := parser.LoadByName(name)
	if err != nil {
		return nil, fmt.Errorf("%s: loading %q: %w", operator, name, err)
	}
	if expFormula.Type != TypeExpansion {
		return nil, fmt.Errorf("%s: %q is not an expansion formula (type=%s)", operator, name, expFormula.Type)
	}
	if len(expFormula.Template) == 0 {
		return nil, fmt.Errorf("%s: %q has no template steps", operator, name)
	}
	return expFormula, nil
}

func expandTarget(target *Step, formula *Formula, overrides map[string]string) ([]*Step, error) {
	return expandTargetWithVars(target, formula, mergeVars(formula, overrides))
}

func expandTargetWithVars(target *Step, formula *Formula, vars map[string]string) ([]*Step, error) {
	expandedSteps, err := expandStep(target, formula.Template, 0, vars)
	if err != nil {
		return nil, err
	}
	propagateTargetDeps(target, expandedSteps)
	return expandedSteps, nil
}

func replaceExpandedTarget(result []*Step, targetID string, target *Step, expandedSteps []*Step) []*Step {
	result = replaceStep(result, targetID, expandedSteps)
	if len(expandedSteps) == 0 {
		return result
	}
	lastStepID := expandedSteps[len(expandedSteps)-1].ID
	return UpdateDependenciesForExpansion(result, target.ID, lastStepID)
}

func matchingExpansionTargets(stepMap map[string]*Step, pattern string, expanded map[string]bool) []*Step {
	var targets []*Step
	for id, step := range stepMap {
		if MatchGlob(pattern, id) && !expanded[id] {
			targets = append(targets, step)
		}
	}
	return targets
}

func validateExpandedSteps(steps []*Step) ([]*Step, error) {
	if dups := findDuplicateStepIDs(steps); len(dups) > 0 {
		return nil, fmt.Errorf("duplicate step IDs after expansion: %v", dups)
	}
	return steps, nil
}

// findDuplicateStepIDs returns any duplicate step IDs found in the steps slice.
// It recursively checks all children.
func findDuplicateStepIDs(steps []*Step) []string {
	seen := make(map[string]int)
	countStepIDs(steps, seen)

	var dups []string
	for id, count := range seen {
		if count > 1 {
			dups = append(dups, id)
		}
	}
	return dups
}

// countStepIDs counts occurrences of each step ID recursively.
func countStepIDs(steps []*Step, counts map[string]int) {
	for _, step := range steps {
		counts[step.ID]++
		if len(step.Children) > 0 {
			countStepIDs(step.Children, counts)
		}
	}
}

// expandStep expands a target step using the given template.
// Returns the expanded steps with placeholders substituted.
// The depth parameter tracks recursion depth for children; if it exceeds
// DefaultMaxExpansionDepth, an error is returned.
// The vars parameter provides variable values for {varname} substitution.
func expandStep(target *Step, template []*Step, depth int, vars map[string]string) ([]*Step, error) {
	if depth > DefaultMaxExpansionDepth {
		return nil, fmt.Errorf("expansion depth limit exceeded: max %d levels (currently at %d) - step %q",
			DefaultMaxExpansionDepth, depth, target.ID)
	}

	result := make([]*Step, 0, len(template))

	for _, tmpl := range template {
		expanded, err := expandTemplateStep(target, tmpl, depth, vars)
		if err != nil {
			return nil, err
		}
		result = append(result, expanded)
	}

	return result, nil
}

func expandTemplateStep(target, template *Step, depth int, vars map[string]string) (*Step, error) {
	expanded := &Step{
		ID:          substituteExpansionValue(template.ID, target, vars),
		Title:       substituteExpansionValue(template.Title, target, vars),
		Description: substituteExpansionValue(template.Description, target, vars),
		Type:        template.Type,
		Priority:    template.Priority,
		StepExpansion: StepExpansion{
			Assignee:       substituteVars(template.Assignee, vars),
			SourceFormula:  template.SourceFormula,  // Preserve source from template
			SourceLocation: template.SourceLocation, // Preserve source location
		},
	}
	expanded.Labels = substituteExpansionValues(template.Labels, target, vars)
	expanded.DependsOn = substituteExpansionValues(template.DependsOn, target, vars)
	expanded.Needs = substituteExpansionValues(template.Needs, target, vars)

	if len(template.Children) > 0 {
		children, err := expandStep(target, template.Children, depth+1, vars)
		if err != nil {
			return nil, err
		}
		expanded.Children = children
	}
	return expanded, nil
}

func substituteExpansionValue(value string, target *Step, vars map[string]string) string {
	return substituteVars(substituteTargetPlaceholders(value, target), vars)
}

func substituteExpansionValues(values []string, target *Step, vars map[string]string) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = substituteExpansionValue(value, target, vars)
	}
	return result
}

// substituteTargetPlaceholders replaces {target} and {target.*} placeholders.
func substituteTargetPlaceholders(s string, target *Step) string {
	if s == "" {
		return s
	}

	// Replace {target} with target step ID
	s = strings.ReplaceAll(s, "{target}", target.ID)

	// Replace {target.id} with target step ID
	s = strings.ReplaceAll(s, "{target.id}", target.ID)

	// Replace {target.title} with target step title
	s = strings.ReplaceAll(s, "{target.title}", target.Title)

	// Replace {target.description} with target step description
	s = strings.ReplaceAll(s, "{target.description}", target.Description)

	return s
}

// mergeVars merges formula default vars with rule overrides.
// Override values take precedence over defaults.
func mergeVars(formula *Formula, overrides map[string]string) map[string]string {
	result := make(map[string]string)

	// Start with formula defaults
	for name, def := range formula.Vars {
		if def.Default != nil {
			result[name] = *def.Default
		}
	}

	// Apply overrides (these win)
	for name, value := range overrides {
		result[name] = value
	}

	return result
}

// buildStepMap creates a map of step ID to step (recursive).
func buildStepMap(steps []*Step) map[string]*Step {
	result := make(map[string]*Step)
	for _, step := range steps {
		result[step.ID] = step
		// Add children recursively
		for id, child := range buildStepMap(step.Children) {
			result[id] = child
		}
	}
	return result
}

// replaceStep replaces a step with the given ID with a slice of new steps.
// Searches recursively through children to find and replace the target.
func replaceStep(steps []*Step, targetID string, replacement []*Step) []*Step {
	result := make([]*Step, 0, len(steps)+len(replacement)-1)

	for _, step := range steps {
		if step.ID == targetID {
			// Replace with expanded steps
			result = append(result, replacement...)
		} else {
			// Keep the step, but check children
			if len(step.Children) > 0 {
				// Clone step and replace in children
				clone := cloneStep(step)
				clone.Children = replaceStep(step.Children, targetID, replacement)
				result = append(result, clone)
			} else {
				result = append(result, step)
			}
		}
	}

	return result
}

// UpdateDependenciesForExpansion updates dependency references after expansion.
// When step X is expanded into X.draft, X.refine-1, etc., any step that
// depended on X should now depend on the last step in the expansion.
func UpdateDependenciesForExpansion(steps []*Step, expandedID string, lastExpandedStepID string) []*Step {
	result := make([]*Step, len(steps))

	for i, step := range steps {
		clone := cloneStep(step)

		// Update DependsOn references
		for j, dep := range clone.DependsOn {
			if dep == expandedID {
				clone.DependsOn[j] = lastExpandedStepID
			}
		}

		// Update Needs references
		for j, need := range clone.Needs {
			if need == expandedID {
				clone.Needs[j] = lastExpandedStepID
			}
		}

		// Handle children recursively
		if len(step.Children) > 0 {
			clone.Children = UpdateDependenciesForExpansion(step.Children, expandedID, lastExpandedStepID)
		}

		result[i] = clone
	}

	return result
}

// propagateTargetDeps copies the target step's Needs and DependsOn to the root
// steps of an expansion. Root steps are those whose existing dependencies only
// reference other steps within the expansion (i.e., they have no external deps
// from the template). This preserves cross-expansion dependency chains that would
// otherwise be lost when the target step is replaced.
func propagateTargetDeps(target *Step, expandedSteps []*Step) {
	if len(target.Needs) == 0 && len(target.DependsOn) == 0 {
		return
	}

	expandedIDs := make(map[string]bool, len(expandedSteps))
	for _, s := range expandedSteps {
		expandedIDs[s.ID] = true
	}

	for _, s := range expandedSteps {
		if isExpansionRoot(s, expandedIDs) {
			prependTargetDeps(target, s)
		}
	}
}

func isExpansionRoot(step *Step, expandedIDs map[string]bool) bool {
	return !hasInternalDependency(step.Needs, expandedIDs) && !hasInternalDependency(step.DependsOn, expandedIDs)
}

func hasInternalDependency(dependencies []string, expandedIDs map[string]bool) bool {
	for _, dependency := range dependencies {
		if expandedIDs[dependency] {
			return true
		}
	}
	return false
}

func prependTargetDeps(target, step *Step) {
	if len(target.Needs) > 0 {
		step.Needs = append(append([]string{}, target.Needs...), step.Needs...)
	}
	if len(target.DependsOn) > 0 {
		step.DependsOn = append(append([]string{}, target.DependsOn...), step.DependsOn...)
	}
}

// MaterializeExpansion converts a standalone expansion formula into a cookable
// form by expanding its Template into Steps. A synthetic target step is created
// using targetID as the step ID and the formula's own name/description for
// {target.title} and {target.description} placeholders.
//
// This enables expansion formulas to be directly instantiated via wisp/pour
// without requiring a Compose wrapper (bd-qzb).
//
// No-op if the formula is not an expansion type, has no Template, or already
// has Steps.
func MaterializeExpansion(f *Formula, targetID string, vars map[string]string) error {
	if f.Type != TypeExpansion || len(f.Template) == 0 || len(f.Steps) > 0 {
		return nil
	}

	target := &Step{
		ID:          targetID,
		Title:       f.Formula,
		Description: f.Description,
	}

	expandedSteps, err := expandStep(target, f.Template, 0, vars)
	if err != nil {
		return fmt.Errorf("materializing expansion %q: %w", f.Formula, err)
	}

	f.Steps = expandedSteps
	return nil
}

// ApplyInlineExpansions applies Step.Expand fields to inline expansions.
// Steps with the Expand field set are replaced by the referenced expansion template.
// The step's ExpandVars are passed as variable overrides to the expansion.
//
// This differs from compose.Expand in that the expansion is declared inline on the
// step itself rather than in a central compose section.
//
// Returns a new steps slice with inline expansions applied.
// The original steps slice is not modified.
func ApplyInlineExpansions(steps []*Step, parser *Parser) ([]*Step, error) {
	if parser == nil {
		return steps, nil
	}

	return applyInlineExpansionsRecursive(steps, parser, 0)
}

// applyInlineExpansionsRecursive handles inline expansions for a slice of steps.
// depth tracks recursion to prevent infinite expansion loops.
func applyInlineExpansionsRecursive(steps []*Step, parser *Parser, depth int) ([]*Step, error) {
	if depth > DefaultMaxExpansionDepth {
		return nil, fmt.Errorf("inline expansion depth limit exceeded: max %d levels", DefaultMaxExpansionDepth)
	}

	var result []*Step

	for _, step := range steps {
		processed, err := applyInlineExpansionStep(step, parser, depth)
		if err != nil {
			return nil, err
		}
		result = append(result, processed...)
	}

	return result, nil
}

func applyInlineExpansionStep(step *Step, parser *Parser, depth int) ([]*Step, error) {
	if step.Expand != "" {
		return expandInlineStep(step, parser, depth)
	}
	return cloneInlineStep(step, parser, depth)
}

func expandInlineStep(step *Step, parser *Parser, depth int) ([]*Step, error) {
	expFormula, err := loadInlineExpansionFormula(parser, step)
	if err != nil {
		return nil, err
	}

	vars := mergeVars(expFormula, step.ExpandVars)
	expandedSteps, err := expandStep(step, expFormula.Template, 0, vars)
	if err != nil {
		return nil, fmt.Errorf("inline expand on step %q: %w", step.ID, err)
	}
	propagateTargetDeps(step, expandedSteps)
	return applyInlineExpansionsRecursive(expandedSteps, parser, depth+1)
}

func loadInlineExpansionFormula(parser *Parser, step *Step) (*Formula, error) {
	expFormula, err := parser.LoadByName(step.Expand)
	if err != nil {
		return nil, fmt.Errorf("inline expand on step %q: loading %q: %w", step.ID, step.Expand, err)
	}
	if expFormula.Type != TypeExpansion {
		return nil, fmt.Errorf("inline expand on step %q: %q is not an expansion formula (type=%s)",
			step.ID, step.Expand, expFormula.Type)
	}
	if len(expFormula.Template) == 0 {
		return nil, fmt.Errorf("inline expand on step %q: %q has no template steps", step.ID, step.Expand)
	}
	return expFormula, nil
}

func cloneInlineStep(step *Step, parser *Parser, depth int) ([]*Step, error) {
	clone := cloneStep(step)
	if len(step.Children) > 0 {
		processedChildren, err := applyInlineExpansionsRecursive(step.Children, parser, depth)
		if err != nil {
			return nil, err
		}
		clone.Children = processedChildren
	}
	return []*Step{clone}, nil
}
