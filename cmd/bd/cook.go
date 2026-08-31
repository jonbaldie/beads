package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/formula"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

// stepTypeToIssueType converts a formula step type string to a types.IssueType.
// Returns types.TypeTask for empty types and for the formula step kind
// "human" (human-assigned work, not an issue type). Gates remain
// [steps.gate]. Other non-empty types pass through (trimmed and
// normalized) rather than being validated here: at pour and cook
// --persist time, flattenUnregisteredIssueTypes degrades types that are
// neither built-in nor registered in types.custom to task (with a
// warning), and the storage layer validates what remains — the same
// division of labor as bd create --type.
func stepTypeToIssueType(stepType string) types.IssueType {
	stepType = strings.TrimSpace(stepType)
	if stepType == "" {
		return types.TypeTask
	}
	if strings.EqualFold(stepType, "human") {
		return types.TypeTask
	}
	return types.IssueType(stepType).Normalize()
}

// cookCmd compiles a formula JSON into a proto bead.
var cookCmd = &cobra.Command{
	Use:   "cook <formula-file>",
	Short: "Compile a formula into a proto (ephemeral by default)",
	Long: `Cook transforms a .formula.json file into a proto.

By default, cook outputs the resolved formula as JSON to stdout for
ephemeral use. The output can be inspected, piped, or saved to a file.

Two cooking modes are available:

  COMPILE-TIME (default, --mode=compile):
    Produces a proto with {{variable}} placeholders intact.
    Use for: modeling, estimation, contractor handoff, planning.
    Variables are NOT substituted - the output shows the template structure.

  RUNTIME (--mode=runtime or when --var flags provided):
    Produces a fully-resolved proto with variables substituted.
    Use for: final validation before pour, seeing exact output.
    Requires all variables to have values (via --var or defaults).

Formulas are high-level workflow templates that support:
  - Variable definitions with defaults and validation
  - Step definitions that become issue hierarchies
  - Composition rules for bonding formulas together
  - Inheritance via extends

The --persist flag enables the legacy behavior of writing the proto
to the database. This is useful when you want to reuse the same
proto multiple times without re-cooking.

For most workflows, prefer ephemeral protos: pour and wisp commands
accept formula names directly and cook inline.

Examples:
  bd cook mol-feature.formula.json                    # Compile-time: keep {{vars}}
  bd cook mol-feature --var name=auth                 # Runtime: substitute vars
  bd cook mol-feature --mode=runtime --var name=auth  # Explicit runtime mode
  bd cook mol-feature --dry-run                       # Preview steps
  bd cook mol-release.formula.json --persist          # Write to database
  bd cook mol-release.formula.json --persist --force  # Replace existing

Output (default):
  JSON representation of the resolved formula with all steps.

Output (--persist):
  Creates a proto bead in the database with:
  - ID matching the formula name (e.g., mol-feature)
  - The "template" label for proto identification
  - Child issues for each step
  - Dependencies matching depends_on relationships`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runCook,
}

// cookResult holds the result of cooking a formula
type cookResult struct {
	ProtoID    string   `json:"proto_id"`
	Formula    string   `json:"formula"`
	Created    int      `json:"created"`
	Variables  []string `json:"variables"`
	BondPoints []string `json:"bond_points,omitempty"`
}

// cookFlags holds parsed command-line flags for the cook command
type cookFlags struct {
	dryRun      bool
	persist     bool
	force       bool
	searchPaths []string
	prefix      string
	inputVars   map[string]string
	runtimeMode bool
	formulaPath string
}

// parseCookFlags parses and validates cook command flags
func parseCookFlags(cmd *cobra.Command, args []string) (*cookFlags, error) {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	persist, _ := cmd.Flags().GetBool("persist")
	force, _ := cmd.Flags().GetBool("force")
	searchPaths, _ := cmd.Flags().GetStringSlice("search-path")
	prefix, _ := cmd.Flags().GetString("prefix")
	varFlags, _ := cmd.Flags().GetStringArray("var")
	mode, _ := cmd.Flags().GetString("mode")

	// Parse variables
	inputVars := make(map[string]string)
	for _, v := range varFlags {
		parts := strings.SplitN(v, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid variable format '%s', expected 'key=value'", v)
		}
		inputVars[parts[0]] = parts[1]
	}

	// Validate mode
	if mode != "" && mode != "compile" && mode != "runtime" {
		return nil, fmt.Errorf("invalid mode '%s', must be 'compile' or 'runtime'", mode)
	}

	// Runtime mode is triggered by: explicit --mode=runtime OR providing --var flags
	runtimeMode := mode == "runtime" || len(inputVars) > 0

	return &cookFlags{
		dryRun:      dryRun,
		persist:     persist,
		force:       force,
		searchPaths: searchPaths,
		prefix:      prefix,
		inputVars:   inputVars,
		runtimeMode: runtimeMode,
		formulaPath: args[0],
	}, nil
}

// loadAndResolveFormula parses a formula file and applies all transformations.
// It first tries to load by name from the formula registry (.beads/formulas/),
// and falls back to parsing as a file path if that fails.
func loadAndResolveFormula(formulaPath string, searchPaths []string) (*formula.Formula, error) {
	parser := formula.NewParser(searchPaths...)

	f, err := loadCookFormula(parser, formulaPath)
	if err != nil {
		return nil, err
	}

	// Resolve inheritance
	resolved, err := parser.Resolve(f)
	if err != nil {
		return nil, fmt.Errorf("resolving formula: %w", err)
	}

	// Apply control flow operators - loops, branches, gates
	controlFlowSteps, err := formula.ApplyControlFlow(resolved.Steps, resolved.Compose)
	if err != nil {
		return nil, fmt.Errorf("applying control flow: %w", err)
	}
	resolved.Steps = controlFlowSteps
	resolved.Steps = applyCookAdvice(resolved.Steps, resolved.Advice)

	// Apply inline step expansions
	inlineExpandedSteps, err := formula.ApplyInlineExpansions(resolved.Steps, parser)
	if err != nil {
		return nil, fmt.Errorf("applying inline expansions: %w", err)
	}
	resolved.Steps = inlineExpandedSteps

	if err := applyCookFormulaExpansions(resolved, parser); err != nil {
		return nil, err
	}
	if err := applyCookFormulaAspects(resolved, parser); err != nil {
		return nil, err
	}

	return resolved, nil
}

func applyCookAdvice(steps []*formula.Step, advice []*formula.AdviceRule) []*formula.Step {
	if len(advice) > 0 {
		return formula.ApplyAdvice(steps, advice)
	}
	return steps
}

func loadCookFormula(parser *formula.Parser, formulaPath string) (*formula.Formula, error) {
	// Try to load by name first (from .beads/formulas/ registry).
	f, err := parser.LoadByName(formulaPath)
	if err == nil {
		return f, nil
	}
	// Fall back to parsing as a file path.
	f, err = parser.ParseFile(formulaPath)
	if err != nil {
		return nil, fmt.Errorf("parsing formula: %w", err)
	}
	return f, nil
}

func applyCookFormulaExpansions(resolved *formula.Formula, parser *formula.Parser) error {
	if resolved.Compose == nil || (len(resolved.Compose.Expand) == 0 && len(resolved.Compose.Map) == 0) {
		return nil
	}
	expandedSteps, err := formula.ApplyExpansions(resolved.Steps, resolved.Compose, parser)
	if err != nil {
		return fmt.Errorf("applying expansions: %w", err)
	}
	resolved.Steps = expandedSteps
	return nil
}

func applyCookFormulaAspects(resolved *formula.Formula, parser *formula.Parser) error {
	if resolved.Compose == nil || len(resolved.Compose.Aspects) == 0 {
		return nil
	}
	for _, aspectName := range resolved.Compose.Aspects {
		aspectFormula, err := parser.LoadByName(aspectName)
		if err != nil {
			return fmt.Errorf("loading aspect %q: %w", aspectName, err)
		}
		if aspectFormula.Type != formula.TypeAspect {
			return fmt.Errorf("%q is not an aspect formula (type=%s)", aspectName, aspectFormula.Type)
		}
		if len(aspectFormula.Advice) > 0 {
			resolved.Steps = formula.ApplyAdvice(resolved.Steps, aspectFormula.Advice)
		}
	}
	return nil
}

// outputCookDryRun displays a dry-run preview of what would be cooked
func outputCookDryRun(resolved *formula.Formula, protoID string, runtimeMode bool, inputVars map[string]string, vars, bondPoints []string) {
	modeLabel := "compile-time"
	if runtimeMode {
		modeLabel = "runtime"
		applyCookVariableDefaults(resolved, inputVars)
	}

	fmt.Printf("\nDry run: would cook formula %s as proto %s (%s mode)\n\n", resolved.Formula, protoID, modeLabel)

	printCookDryRunSteps(resolved, runtimeMode, inputVars)
	printCookVariablesUsed(vars)
	printCookVariableValues(runtimeMode, inputVars)
	printCookBondPoints(bondPoints)
	printCookVariableDefinitions(resolved, runtimeMode)
}

func applyCookVariableDefaults(resolved *formula.Formula, inputVars map[string]string) {
	for name, def := range resolved.Vars {
		if _, provided := inputVars[name]; !provided && def.Default != nil {
			inputVars[name] = *def.Default
		}
	}
}

func printCookDryRunSteps(resolved *formula.Formula, runtimeMode bool, inputVars map[string]string) {
	if runtimeMode {
		substituteFormulaVars(resolved, inputVars)
		fmt.Printf("Steps (%d) [variables substituted]:\n", len(resolved.Steps))
	} else {
		fmt.Printf("Steps (%d) [{{variables}} shown as placeholders]:\n", len(resolved.Steps))
	}
	printFormulaSteps(resolved.Steps, "  ")
}

func printCookVariablesUsed(vars []string) {
	if len(vars) > 0 {
		fmt.Printf("\nVariables used: %s\n", strings.Join(vars, ", "))
	}
}

func printCookVariableValues(runtimeMode bool, inputVars map[string]string) {
	if !runtimeMode || len(inputVars) == 0 {
		return
	}
	fmt.Printf("\nVariable values:\n")
	for name, value := range inputVars {
		fmt.Printf("  {{%s}} = %s\n", name, value)
	}
}

func printCookBondPoints(bondPoints []string) {
	if len(bondPoints) > 0 {
		fmt.Printf("Bond points: %s\n", strings.Join(bondPoints, ", "))
	}
}

func printCookVariableDefinitions(resolved *formula.Formula, runtimeMode bool) {
	if runtimeMode || len(resolved.Vars) == 0 {
		return
	}
	fmt.Printf("\nVariable definitions:\n")
	for name, def := range resolved.Vars {
		attrs := cookVariableDefinitionAttributes(def)
		fmt.Printf("  {{%s}}: %s%s\n", name, def.Description, attrs)
	}
}

func cookVariableDefinitionAttributes(def *formula.VarDef) string {
	attrs := []string{}
	if def.Required {
		attrs = append(attrs, "required")
	}
	if def.Default != nil {
		attrs = append(attrs, fmt.Sprintf("default=%s", *def.Default))
	}
	if len(def.Enum) > 0 {
		attrs = append(attrs, fmt.Sprintf("enum=[%s]", strings.Join(def.Enum, ",")))
	}
	if len(attrs) == 0 {
		return ""
	}
	return fmt.Sprintf(" (%s)", strings.Join(attrs, ", "))
}

// outputCookEphemeral outputs the resolved formula as JSON (ephemeral mode)
func outputCookEphemeral(resolved *formula.Formula, runtimeMode bool, inputVars map[string]string, vars []string) error {
	if runtimeMode {
		// Apply defaults from formula variable definitions
		for name, def := range resolved.Vars {
			if _, provided := inputVars[name]; !provided && def.Default != nil {
				inputVars[name] = *def.Default
			}
		}

		// Check for missing required variables
		var missingVars []string
		for _, v := range vars {
			if _, ok := inputVars[v]; !ok {
				missingVars = append(missingVars, v)
			}
		}
		if len(missingVars) > 0 {
			return fmt.Errorf("runtime mode requires all variables to have values\nMissing: %s\nProvide with: --var %s=<value>",
				strings.Join(missingVars, ", "), missingVars[0])
		}

		// Substitute variables in the formula
		substituteFormulaVars(resolved, inputVars)
	}
	return outputJSON(resolved)
}

// persistCookFormula creates a proto bead in the database (persist mode)
func persistCookFormula(ctx context.Context, resolved *formula.Formula, protoID string, force bool, vars, bondPoints []string) error {
	// Check if proto already exists
	existingProto, err := getStore().GetIssue(ctx, protoID)
	if err == nil && existingProto != nil {
		if !force {
			return fmt.Errorf("proto %s already exists (use --force to replace)", protoID)
		}
		// Delete existing proto and its children
		if err := deleteProtoSubgraph(ctx, getStore(), protoID); err != nil {
			return fmt.Errorf("deleting existing proto: %w", err)
		}
	}

	// Create the proto bead from the formula
	result, err := cookFormula(ctx, getStore(), resolved, protoID)
	if err != nil {
		return fmt.Errorf("cooking formula: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(cookResult{
			ProtoID:    result.ProtoID,
			Formula:    resolved.Formula,
			Created:    result.Created,
			Variables:  vars,
			BondPoints: bondPoints,
		})
	}

	fmt.Printf("%s Cooked proto: %s\n", ui.RenderPass("✓"), result.ProtoID)
	fmt.Printf("  Created %d issues\n", result.Created)
	if len(vars) > 0 {
		fmt.Printf("  Variables: %s\n", strings.Join(vars, ", "))
	}
	if len(bondPoints) > 0 {
		fmt.Printf("  Bond points: %s\n", strings.Join(bondPoints, ", "))
	}
	fmt.Printf("\nTo use: bd mol pour %s --var <name>=<value>\n", result.ProtoID)
	return nil
}
