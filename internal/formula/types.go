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
)

// FormulaType categorizes formulas by their purpose.
type FormulaType string

const (
	// TypeWorkflow is a standard workflow template (sequence of steps).
	TypeWorkflow FormulaType = "workflow"

	// TypeExpansion is a macro that expands into multiple steps.
	// Used for common patterns like "test + lint + build".
	TypeExpansion FormulaType = "expansion"

	// TypeAspect is a cross-cutting concern that can be applied to other formulas.
	// Examples: add logging steps, add approval gates.
	TypeAspect FormulaType = "aspect"

	// TypeConvoy is a multi-agent workflow that coordinates parallel workers.
	// Examples: code review with multiple reviewers, design review sessions.
	TypeConvoy FormulaType = "convoy"
)

// IsValid checks if the formula type is recognized.
func (t FormulaType) IsValid() bool {
	switch t {
	case TypeWorkflow, TypeExpansion, TypeAspect, TypeConvoy:
		return true
	}
	return false
}

// Formula is the root structure for .formula.json files.
type Formula struct {
	// Formula is the unique identifier/name for this formula.
	// Convention: mol-<name> for molecules, exp-<name> for expansions.
	Formula string `json:"formula"`

	// Description explains what this formula does.
	Description string `json:"description,omitempty"`

	// Version is the schema version (currently 1).
	Version int `json:"version"`

	// Type categorizes the formula: workflow, expansion, or aspect.
	Type FormulaType `json:"type"`

	// Extends is a list of parent formulas to inherit from.
	// The child formula inherits all vars, steps, and compose rules.
	// Child definitions override parent definitions with the same ID.
	Extends []string `json:"extends,omitempty"`

	// Vars defines template variables with defaults and validation.
	Vars map[string]*VarDef `json:"vars,omitempty"`

	// Steps defines the work items to create.
	Steps []*Step `json:"steps,omitempty"`

	// Template defines expansion template steps (for TypeExpansion formulas).
	// Template steps use {target} and {target.description} placeholders
	// that get substituted when the expansion is applied to a target step.
	Template []*Step `json:"template,omitempty"`

	// Compose defines composition/bonding rules.
	Compose *ComposeRules `json:"compose,omitempty"`

	// Advice defines step transformations (before/after/around).
	// Applied during cooking to insert steps around matching targets.
	Advice []*AdviceRule `json:"advice,omitempty"`

	// Pointcuts defines target patterns for aspect formulas.
	// Used with TypeAspect to specify which steps the aspect applies to.
	Pointcuts []*Pointcut `json:"pointcuts,omitempty"`

	// Phase indicates the recommended instantiation phase: "liquid" (pour) or "vapor" (wisp).
	// If "vapor", bd pour will warn and suggest using bd mol wisp instead.
	// Patrol and release workflows should typically use "vapor" since they're operational.
	Phase string `json:"phase,omitempty"`

	// Pour controls whether steps are materialized as individual child issues.
	// If true, each step becomes a DB row with dependency tracking (checkpoint recovery).
	// If false (default), only the root issue is created; steps are read inline at prime time.
	// Reserve pour=true for critical, infrequent work (e.g. releases) where step-level
	// tracking is worth the DB overhead. Patrol formulas should NOT set this.
	Pour bool `json:"pour,omitempty"`

	// Source tracks where this formula was loaded from (set by parser).
	Source string `json:"source,omitempty"`

	// Intent is an optional caller-defined hint about the formula's runtime
	// intent (e.g. "mail_only"). Opaque to bd; consumed by downstream tools.
	Intent string `json:"intent,omitempty" toml:"intent,omitempty"`
}

// VarDef defines a template variable with optional validation.
type VarDef struct {
	// Description explains what this variable is for.
	Description string `json:"description,omitempty"`

	// Default is the value to use if not provided.
	// nil means no default (variable must be provided if referenced).
	// Non-nil (including &"") means the variable has an explicit default.
	Default *string `json:"default,omitempty"`

	// Required indicates the variable must be provided (no default).
	Required bool `json:"required,omitempty"`

	// Enum lists the allowed values (if non-empty).
	Enum []string `json:"enum,omitempty"`

	// Pattern is a regex pattern the value must match.
	Pattern string `json:"pattern,omitempty"`

	// Type is the expected value type: string (default), int, bool.
	Type string `json:"type,omitempty"`
}

// UnmarshalTOML implements toml.Unmarshaler for VarDef.
// This allows vars to be defined as either simple strings or tables:
//
//	[vars]
//	wisp_type = "patrol"           # simple string -> Default = "patrol"
//
//	[vars.component]               # table with full definition
//	description = "Component name"
//	required = true
func (v *VarDef) UnmarshalTOML(data interface{}) error {
	switch val := data.(type) {
	case string:
		// Simple string value becomes the default
		v.Default = &val
		return nil
	case map[string]interface{}:
		return v.unmarshalTOMLTable(val)
	default:
		return fmt.Errorf("type mismatch for formula.VarDef: expected string or table but found %T", data)
	}
}

func (v *VarDef) unmarshalTOMLTable(val map[string]interface{}) error {
	if desc, ok := val["description"].(string); ok {
		v.Description = desc
	}
	if def, ok := val["default"].(string); ok {
		v.Default = &def
	}
	if req, ok := val["required"].(bool); ok {
		v.Required = req
	}
	v.Enum = append(v.Enum, tomlStringList(val["enum"])...)
	if pattern, ok := val["pattern"].(string); ok {
		v.Pattern = pattern
	}
	if typ, ok := val["type"].(string); ok {
		v.Type = typ
	}
	return nil
}

func tomlStringList(value interface{}) []string {
	enum, ok := value.([]interface{})
	if !ok {
		return nil
	}
	result := make([]string, 0, len(enum))
	for _, item := range enum {
		if s, ok := item.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

// WaitsForSpec holds the parsed waits_for field.
type WaitsForSpec struct {
	// Gate is the gate type: "all-children" or "any-children"
	Gate string
	// SpawnerID is the step ID whose children to wait for.
	// Empty means infer from context (typically first step in needs).
	SpawnerID string
}

// StepExpansion contains optional dependency, expansion, and source-tracing
// details for a formula step. It is anonymously embedded in Step so the JSON
// and TOML documents keep their existing flat shape.
type StepExpansion struct {
	// DependsOn lists step IDs this step blocks on (within the formula).
	DependsOn []string `json:"depends_on,omitempty" toml:"depends_on,omitempty"`

	// WaitsFor specifies a fanout gate type for this step.
	// Values: "all-children" (wait for all dynamic children) or "any-children" (wait for first).
	// When set, the cooked issue gets a "gate:<value>" label.
	WaitsFor string `json:"waits_for,omitempty" toml:"waits_for,omitempty"`

	// Assignee is the default assignee (supports substitution).
	Assignee string `json:"assignee,omitempty"`

	// Expand references an expansion formula to inline here.
	// When set, this step is replaced by the expansion's template steps.
	// See ApplyInlineExpansions in expand.go for implementation.
	Expand string `json:"expand,omitempty"`

	// ExpandVars are variable overrides for the expansion.
	// Merged with the expansion formula's default vars during inline expansion.
	ExpandVars map[string]string `json:"expand_vars,omitempty" toml:"expand_vars,omitempty"`

	// SourceFormula is the formula name where this step was defined.
	// For inherited steps, this is the parent formula, not the final composed formula.
	SourceFormula string `json:"-"` // Internal only, not serialized to JSON

	// SourceLocation is the path within the source formula.
	// Format: "steps[0]", "steps[2].children[1]", "advice[0].after", "loop.body[0]"
	SourceLocation string `json:"-"` // Internal only, not serialized to JSON
}

// Step defines a work item to create when the formula is instantiated.
type Step struct {
	// ID is the unique identifier within this formula.
	// Used for dependency references and bond points.
	ID string `json:"id"`

	// Title is the issue title (supports {{variable}} substitution).
	Title string `json:"title"`

	// Description is the issue description (supports substitution).
	Description string `json:"description,omitempty"`

	// Notes are additional notes for the issue (supports substitution).
	Notes string `json:"notes,omitempty"`

	// Type is the issue type: any built-in type (task by default; bug,
	// feature, epic, chore, decision, spike, story, milestone, ...) or a
	// custom type already registered in types.custom. Unregistered types
	// are flattened to task, with a warning, when the formula is cooked
	// or poured.
	Type string `json:"type,omitempty"`

	// Priority is the issue priority (0-4).
	Priority *int `json:"priority,omitempty"`

	// Labels are applied to the created issue.
	Labels []string `json:"labels,omitempty"`

	// Metadata is carried through to the created issue's Metadata field as
	// JSON. Lets formulas pre-declare keys that downstream tooling can project
	// without a post-pour compose step.
	Metadata map[string]interface{} `json:"metadata,omitempty" toml:"metadata,omitempty"`

	// Needs is a simpler alias for DependsOn - lists sibling step IDs that must complete first.
	// Either Needs or DependsOn can be used; they are merged during cooking.
	Needs []string `json:"needs,omitempty" toml:"needs,omitempty"`

	StepExpansion

	// Condition makes this step optional based on a variable.
	// Format: "{{var}}" (truthy), "!{{var}}" (negated), "{{var}} == value", "{{var}} != value".
	// Evaluated at cook/pour time via FilterStepsByCondition.
	Condition string `json:"condition,omitempty"`

	// Children are nested steps (for creating epic hierarchies).
	Children []*Step `json:"children,omitempty"`

	// Gate defines an async wait condition for this step.
	// When set, bd cook creates a gate issue that blocks this step.
	// Close the gate issue (bd close bd-xxx.gate-stepid) to unblock.
	Gate *Gate `json:"gate,omitempty"`

	// Loop defines iteration for this step.
	// When set, the step becomes a container that expands its body.
	Loop *LoopSpec `json:"loop,omitempty"`

	// OnComplete defines actions triggered when this step completes.
	// Used for runtime expansion over step output (the for-each construct).
	OnComplete *OnCompleteSpec `json:"on_complete,omitempty" toml:"on_complete,omitempty"`
}

// Gate defines an async wait condition for formula steps.
// When a step has a Gate, bd cook creates a gate issue that blocks the step.
// The gate must be closed (manually or via watchers) to unblock the step.
type Gate struct {
	// Type is the condition type: gh:run, gh:pr, timer, human, mail.
	Type string `json:"type"`

	// ID is the condition identifier (e.g., workflow name for gh:run).
	ID string `json:"id,omitempty"`

	// AwaitID is the runtime condition identifier. This is preferred by
	// formula authors because it maps directly to Issue.AwaitID.
	AwaitID string `json:"await_id,omitempty" toml:"await_id,omitempty"`

	// Timeout is how long to wait before escalation (e.g., "1h", "24h").
	Timeout string `json:"timeout,omitempty"`

	// Repo optionally selects the GitHub repository (OWNER/REPO or
	// HOST/OWNER/REPO) a gh:run or gh:pr gate's condition is checked
	// against. Empty means the current Git repository - the same default
	// as an ad-hoc `bd gate create` gate. Ignored for non-GitHub gate
	// types (human, timer, bead).
	Repo string `json:"repo,omitempty" toml:"repo,omitempty"`
}

// LoopSpec defines iteration over a body of steps.
// One of Count, Until, or Range must be specified.
type LoopSpec struct {
	// Count is the fixed number of iterations.
	// When set, the loop body is expanded Count times.
	Count int `json:"count,omitempty"`

	// Until is a condition that ends the loop.
	// Format matches condition evaluator syntax (e.g., "step.status == 'complete'").
	Until string `json:"until,omitempty"`

	// Max is the maximum iterations for conditional loops.
	// Required when Until is set, to prevent unbounded loops.
	Max int `json:"max,omitempty"`

	// Range specifies a computed range for iteration.
	// Format: "start..end" where start and end can be:
	//   - Integers: "1..10"
	//   - Expressions: "1..2^{disks}" (evaluated at cook time)
	//   - Variables: "{start}..{count}" (substituted from Vars)
	// Supports: + - * / ^ (power) and parentheses.
	Range string `json:"range,omitempty"`

	// Var is the variable name exposed to body steps.
	// For Range loops, this is set to the current iteration value.
	// Example: var: "move_num" with range: "1..7" exposes {move_num}=1,2,...,7
	Var string `json:"var,omitempty"`

	// Body contains the steps to repeat.
	Body []*Step `json:"body"`
}

// OnCompleteSpec defines actions triggered when a step completes.
// Used for runtime expansion over step output (the for-each construct).
//
// Example YAML:
//
//	step: survey-workers
//	on_complete:
//	  for_each: output.workers
//	  bond: mol-worker-arm
//	  vars:
//	    worker_name: "{item.name}"
//	    rig: "{item.rig}"
//	  parallel: true
type OnCompleteSpec struct {
	// ForEach is the path to the iterable collection in step output.
	// Format: "output.<field>" or "output.<field>.<nested>"
	// The collection must be an array at runtime.
	ForEach string `json:"for_each,omitempty" toml:"for_each,omitempty"`

	// Bond is the formula to instantiate for each item.
	// A new molecule is created for each element in the ForEach collection.
	Bond string `json:"bond,omitempty"`

	// Vars are variable bindings for each iteration.
	// Supports placeholders:
	//   - {item} - the current item value (for primitives)
	//   - {item.field} - a field from the current item (for objects)
	//   - {index} - the zero-based iteration index
	Vars map[string]string `json:"vars,omitempty"`

	// Parallel runs all bonded molecules concurrently (default behavior).
	// Set to true to make this explicit.
	Parallel bool `json:"parallel,omitempty"`

	// Sequential runs bonded molecules one at a time.
	// Each molecule starts only after the previous one completes.
	// Mutually exclusive with Parallel.
	Sequential bool `json:"sequential,omitempty"`
}

// BranchRule defines parallel execution paths that rejoin.
// Creates a fork-join pattern: from -> [parallel steps] -> join.
type BranchRule struct {
	// From is the step ID that precedes the parallel paths.
	// All branch steps will depend on this step.
	From string `json:"from"`

	// Steps are the step IDs that run in parallel.
	// These steps will all depend on From.
	Steps []string `json:"steps"`

	// Join is the step ID that follows all parallel paths.
	// This step will depend on all Steps completing.
	Join string `json:"join"`
}

// GateRule defines a condition that must be satisfied before a step proceeds.
// Gates are evaluated at runtime by the patrol executor.
type GateRule struct {
	// Before is the step ID that the gate applies to.
	// The condition must be satisfied before this step can start.
	Before string `json:"before"`

	// Condition is the expression to evaluate.
	// Format matches condition evaluator syntax (e.g., "tests.status == 'complete'").
	Condition string `json:"condition"`
}

// ComposeRules define how formulas can be bonded together.
type ComposeRules struct {
	// BondPoints are named locations where other formulas can attach.
	BondPoints []*BondPoint `json:"bond_points,omitempty" toml:"bond_points,omitempty"`

	// Hooks are automatic attachments triggered by labels or conditions.
	Hooks []*Hook `json:"hooks,omitempty"`

	// Expand applies an expansion template to a single target step.
	// The target step is replaced by the expanded template steps.
	Expand []*ExpandRule `json:"expand,omitempty"`

	// Map applies an expansion template to all steps matching a pattern.
	// Each matching step is replaced by the expanded template steps.
	Map []*MapRule `json:"map,omitempty"`

	// Branch defines fork-join parallel execution patterns.
	// Each rule creates dependencies for parallel paths that rejoin.
	Branch []*BranchRule `json:"branch,omitempty"`

	// Gate defines conditional waits before steps.
	// Each rule adds a condition that must be satisfied at runtime.
	Gate []*GateRule `json:"gate,omitempty"`

	// Aspects lists aspect formula names to apply to this formula.
	// Aspects are applied after expansions, adding before/after/around
	// steps to matching targets based on the aspect's advice rules.
	// Example: ["security-audit", "logging"]
	Aspects []string `json:"aspects,omitempty"`
}

// ExpandRule applies an expansion template to a single target step.
type ExpandRule struct {
	// Target is the step ID to expand.
	Target string `json:"target"`

	// With is the name of the expansion formula to apply.
	With string `json:"with"`

	// Vars are variable overrides for the expansion.
	Vars map[string]string `json:"vars,omitempty"`
}

// MapRule applies an expansion template to all matching steps.
type MapRule struct {
	// Select is a glob pattern matching step IDs to expand.
	// Examples: "*.implement", "shiny.*"
	Select string `json:"select"`

	// With is the name of the expansion formula to apply.
	With string `json:"with"`

	// Vars are variable overrides for the expansion.
	Vars map[string]string `json:"vars,omitempty"`
}

// BondPoint is a named attachment site for composition.
type BondPoint struct {
	// ID is the unique identifier for this bond point.
	ID string `json:"id"`

	// Description explains what should be attached here.
	Description string `json:"description,omitempty"`

	// AfterStep is the step ID after which to attach.
	// Mutually exclusive with BeforeStep.
	AfterStep string `json:"after_step,omitempty" toml:"after_step,omitempty"`

	// BeforeStep is the step ID before which to attach.
	// Mutually exclusive with AfterStep.
	BeforeStep string `json:"before_step,omitempty" toml:"before_step,omitempty"`

	// Parallel makes attached steps run in parallel with the anchor step.
	Parallel bool `json:"parallel,omitempty"`
}

// Hook defines automatic formula attachment based on conditions.
type Hook struct {
	// Trigger is what activates this hook.
	// Formats: "label:security", "type:bug", "priority:0-1".
	Trigger string `json:"trigger"`

	// Attach is the formula to attach when triggered.
	Attach string `json:"attach"`

	// At is the bond point to attach at (default: end).
	At string `json:"at,omitempty"`

	// Vars are variable overrides for the attached formula.
	Vars map[string]string `json:"vars,omitempty"`
}

// Pointcut defines a target pattern for advice application.
// Used in aspect formulas to specify which steps the advice applies to.
type Pointcut struct {
	// Glob is a glob pattern to match step IDs.
	// Examples: "*.implement", "shiny.*", "review"
	Glob string `json:"glob,omitempty"`

	// Type matches steps by their type field.
	// Examples: "task", "bug", "epic"
	Type string `json:"type,omitempty"`

	// Label matches steps that have a specific label.
	Label string `json:"label,omitempty"`
}

// AdviceRule defines a step transformation rule.
// Advice operators insert steps before, after, or around matching targets.
type AdviceRule struct {
	// Target is a glob pattern matching step IDs to apply advice to.
	// Examples: "*.implement", "design", "shiny.*"
	Target string `json:"target"`

	// Before inserts a step before the target.
	Before *AdviceStep `json:"before,omitempty"`

	// After inserts a step after the target.
	After *AdviceStep `json:"after,omitempty"`

	// Around wraps the target with before and after steps.
	Around *AroundAdvice `json:"around,omitempty"`
}

// AdviceStep defines a step to insert via advice.
type AdviceStep struct {
	// ID is the step identifier. Supports {step.id} substitution.
	ID string `json:"id"`

	// Title is the step title. Supports {step.id} substitution.
	Title string `json:"title,omitempty"`

	// Description is the step description.
	Description string `json:"description,omitempty"`

	// Type is the issue type (task, bug, etc).
	Type string `json:"type,omitempty"`

	// Args are additional context passed to the step.
	Args map[string]string `json:"args,omitempty"`

	// Output defines expected outputs from this step.
	Output map[string]string `json:"output,omitempty"`
}

// AroundAdvice wraps a target with before and after steps.
type AroundAdvice struct {
	// Before is a list of steps to insert before the target.
	Before []*AdviceStep `json:"before,omitempty"`

	// After is a list of steps to insert after the target.
	After []*AdviceStep `json:"after,omitempty"`
}
