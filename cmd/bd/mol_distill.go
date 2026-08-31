package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jonbaldie/beads/internal/formula"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

func newMolDistillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "distill <epic-id> [formula-name]",
		Short: "Extract a formula from an existing epic",
		Long: `Distill a molecule by extracting a reusable formula from an existing epic.

This is the reverse of pour: instead of formula → molecule, it's molecule → formula.

The distill command:
  1. Loads the existing epic and all its children
  2. Converts the structure to a .formula.json file
  3. Replaces concrete values with {{variable}} placeholders (via --var flags)

Use cases:
  - Team develops good workflow organically, wants to reuse it
  - Capture tribal knowledge as executable templates
  - Create starting point for similar future work

Variable syntax (both work - we detect which side is the concrete value):
  --var branch=feature-auth    Spawn-style: variable=value (recommended)
  --var feature-auth=branch    Substitution-style: value=variable

Output locations (first writable wins):
  1. <resolved-beads-dir>/formulas/ (project-level, default)
  2. <checkout-root>/.beads/formulas/ (repo-local formulas)
  3. ~/.beads/formulas/     (user-level, if project not writable)

Examples:
  bd mol distill bd-o5xe my-workflow
  bd mol distill bd-abc release-workflow --var feature_name=auth-refactor`,
		Args:          cobra.RangeArgs(1, 2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runMolDistill,
	}
	cmd.Flags().StringArray("var", []string{}, "Replace value with {{variable}} placeholder (variable=value)")
	cmd.Flags().Bool("dry-run", false, "Preview what would be created")
	cmd.Flags().String("output", "", "Output directory for formula file")
	return cmd
}

// DistillResult holds the result of a distill operation
type DistillResult struct {
	FormulaName string   `json:"formula_name"`
	FormulaPath string   `json:"formula_path"`
	Steps       int      `json:"steps"`     // number of steps in formula
	Variables   []string `json:"variables"` // variables introduced
}

// collectSubgraphText gathers all searchable text from a molecule subgraph
func collectSubgraphText(subgraph *MoleculeSubgraph) string {
	var parts []string
	for _, issue := range subgraph.Issues {
		parts = append(parts, issue.Title)
		parts = append(parts, issue.Description)
		parts = append(parts, issue.Design)
		parts = append(parts, issue.AcceptanceCriteria)
		parts = append(parts, issue.Notes)
	}
	return strings.Join(parts, " ")
}

// parseDistillVar parses a --var flag with smart detection of syntax.
// Accepts both spawn-style (variable=value) and substitution-style (value=variable).
// Returns (findText, varName, error).
func parseDistillVar(varFlag, searchableText string) (string, string, error) {
	parts := strings.SplitN(varFlag, "=", 2)
	if !validDistillVarParts(parts) {
		return "", "", fmt.Errorf("invalid format '%s', expected 'variable=value' or 'value=variable'", varFlag)
	}
	left, right := parts[0], parts[1]
	leftFound := strings.Contains(searchableText, left)
	rightFound := strings.Contains(searchableText, right)
	switch {
	case rightFound && !leftFound:
		// spawn-style: --var branch=feature-auth
		// left is variable name, right is the value to find
		return right, left, nil
	case leftFound && !rightFound:
		// substitution-style: --var feature-auth=branch
		// left is value to find, right is variable name
		return left, right, nil
	case leftFound && rightFound:
		// Both found - prefer spawn-style (more natural guess)
		// Agent likely typed: --var varname=concrete_value
		return right, left, nil
	default:
		return "", "", fmt.Errorf("neither '%s' nor '%s' found in epic text", left, right)
	}
}

func validDistillVarParts(parts []string) bool {
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

type molDistillInput struct {
	epicID         string
	formulaNameArg string
	varFlags       []string
	dryRun         bool
	outputDir      string
}

func gatherMolDistillInput(cmd *cobra.Command, args []string) molDistillInput {
	in := molDistillInput{epicID: args[0]}
	in.varFlags, _ = cmd.Flags().GetStringArray("var")
	in.dryRun, _ = cmd.Flags().GetBool("dry-run")
	in.outputDir, _ = cmd.Flags().GetString("output")
	if len(args) > 1 {
		in.formulaNameArg = args[1]
	}
	return in
}

func runMolDistill(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("mol-distill")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	in := gatherMolDistillInput(cmd, args)

	if usesProxiedServer() {
		return runMolDistillProxiedServer(getRootContext(), in)
	}

	ctx := getRootContext()

	if getStore() == nil {
		return HandleErrorRespectJSON("no database connection")
	}

	epicID, err := utils.ResolvePartialID(ctx, getStore(), in.epicID)
	if err != nil {
		return HandleErrorRespectJSON("'%s' not found", in.epicID)
	}

	subgraph, err := loadTemplateSubgraph(ctx, getStore(), epicID)
	if err != nil {
		return HandleErrorRespectJSON("loading epic: %v", err)
	}

	return distillSubgraph(epicID, subgraph, in)
}

// distillSubgraph converts an already-loaded subgraph into a formula file
// (dry-run preview or write-to-disk), shared by the embedded and
// proxied-server dual.
func distillSubgraph(epicID string, subgraph *TemplateSubgraph, in molDistillInput) error {
	formulaName := in.formulaNameArg
	if formulaName == "" {
		formulaName = sanitizeFormulaName(subgraph.Root.Title)
	}

	replacements, err := distillReplacements(subgraph, in.varFlags)
	if err != nil {
		return err
	}
	f := subgraphToFormula(subgraph, formulaName, replacements)
	outputPath, err := distillOutputPath(formulaName, in.outputDir)
	if err != nil {
		return err
	}
	if in.dryRun {
		renderDistillDryRun(epicID, formulaName, outputPath, replacements, f)
		return nil
	}
	if err := writeDistilledFormula(outputPath, f); err != nil {
		return err
	}
	result := &DistillResult{
		FormulaName: formulaName,
		FormulaPath: outputPath,
		Steps:       countSteps(f.Steps),
		Variables:   getVarNames(replacements),
	}

	return renderDistillResult(result)
}

func distillReplacements(subgraph *TemplateSubgraph, flags []string) (map[string]string, error) {
	replacements := make(map[string]string)
	searchableText := collectSubgraphText(subgraph)
	for _, flag := range flags {
		findText, varName, err := parseDistillVar(flag, searchableText)
		if err != nil {
			return nil, HandleErrorRespectJSON("%v", err)
		}
		replacements[findText] = varName
	}
	return replacements, nil
}

func distillOutputPath(formulaName, outputDir string) (string, error) {
	if outputDir != "" {
		return filepath.Join(outputDir, formulaName+formula.FormulaExt), nil
	}
	if outputPath := findWritableFormulaDir(formulaName); outputPath != "" {
		return outputPath, nil
	}
	hint := "Try creating one of the formula search paths"
	if searchPaths := getFormulaSearchPaths(); len(searchPaths) > 0 {
		hint = fmt.Sprintf("Try: mkdir -p %s", searchPaths[0])
	}
	return "", HandleErrorWithHint("no writable formula directory found", hint)
}

func renderDistillDryRun(epicID, formulaName, outputPath string, replacements map[string]string, f *formula.Formula) {
	fmt.Printf("\nDry run: would distill %d steps from %s into formula\n\n", countSteps(f.Steps), epicID)
	fmt.Printf("Formula: %s\nOutput: %s\n", formulaName, outputPath)
	if len(replacements) > 0 {
		fmt.Printf("\nVariables:\n")
		for value, varName := range replacements {
			fmt.Printf("  %s: \"%s\" → {{%s}}\n", varName, value, varName)
		}
	}
	fmt.Printf("\nStructure:\n")
	printFormulaStepsTree(f.Steps, "")
}

func writeDistilledFormula(outputPath string, f *formula.Formula) error {
	dir := filepath.Dir(outputPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return HandleErrorRespectJSON("creating directory %s: %v", dir, err)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return HandleErrorRespectJSON("encoding formula: %v", err)
	}
	// #nosec G306 -- Formula files are not sensitive
	if err := os.WriteFile(outputPath, data, 0644); err != nil {
		return HandleErrorRespectJSON("writing formula: %v", err)
	}
	return nil
}

func renderDistillResult(result *DistillResult) error {
	if isJSONOutput() {
		return outputJSON(result)
	}
	fmt.Printf("%s Distilled formula: %d steps\n", ui.RenderPass("✓"), result.Steps)
	fmt.Printf("  Formula: %s\n", result.FormulaName)
	fmt.Printf("  Path: %s\n", result.FormulaPath)
	if len(result.Variables) > 0 {
		fmt.Printf("  Variables: %s\n", strings.Join(result.Variables, ", "))
	}
	fmt.Printf("\nTo instantiate:\n")
	fmt.Printf("  bd mol pour %s", result.FormulaName)
	for _, v := range result.Variables {
		fmt.Printf(" --var %s=<value>", v)
	}
	fmt.Println()
	return nil
}

// sanitizeFormulaName converts a title to a valid formula name
func sanitizeFormulaName(title string) string {
	// Convert to lowercase and replace spaces/special chars with hyphens
	re := regexp.MustCompile(`[^a-zA-Z0-9-]+`)
	name := re.ReplaceAllString(strings.ToLower(title), "-")
	// Remove leading/trailing hyphens and collapse multiple hyphens
	name = regexp.MustCompile(`-+`).ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = "untitled"
	}
	return name
}

// findWritableFormulaDir finds the first writable formula directory
func findWritableFormulaDir(formulaName string) string {
	searchPaths := getFormulaSearchPaths()
	for _, dir := range searchPaths {
		// Try to create the directory if it doesn't exist
		if err := os.MkdirAll(dir, 0755); err == nil {
			// Check if we can write to it
			testPath := filepath.Join(dir, ".write-test")
			if f, err := os.Create(testPath); err == nil { //nolint:gosec // testPath is constructed from known search paths
				_ = f.Close()           // Best effort cleanup
				_ = os.Remove(testPath) // Best effort cleanup of temp file
				return filepath.Join(dir, formulaName+formula.FormulaExt)
			}
		}
	}
	return ""
}

// getVarNames extracts variable names from replacements map
func getVarNames(replacements map[string]string) []string {
	var names []string
	for _, varName := range replacements {
		names = append(names, varName)
	}
	return names
}

// subgraphToFormula converts a molecule subgraph to a formula
func subgraphToFormula(subgraph *TemplateSubgraph, name string, replacements map[string]string) *formula.Formula {
	idToStepID := formulaStepIDs(subgraph)
	depsByIssue := formulaDependencies(subgraph)
	var steps []*formula.Step
	for _, issue := range subgraph.Issues {
		if issue.ID == subgraph.Root.ID {
			continue
		}
		steps = append(steps, issueToFormulaStep(issue, subgraph.Root.ID, idToStepID, depsByIssue, replacements))
	}
	return &formula.Formula{
		Formula:     name,
		Description: replaceFormulaVars(subgraph.Root.Description, replacements),
		Version:     1,
		Type:        formula.TypeWorkflow,
		Vars:        formulaVariables(replacements),
		Steps:       steps,
	}
}

func replaceFormulaVars(text string, replacements map[string]string) string {
	result := text
	for value, varName := range replacements {
		pattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(value) + `\b`)
		result = pattern.ReplaceAllString(result, "{{"+varName+"}}")
	}
	return result
}

func formulaStepIDs(subgraph *TemplateSubgraph) map[string]string {
	ids := make(map[string]string, len(subgraph.Issues))
	for _, issue := range subgraph.Issues {
		stepID := sanitizeFormulaName(issue.Title)
		if stepID == "" {
			stepID = issue.ID
		}
		ids[issue.ID] = stepID
	}
	return ids
}

func formulaDependencies(subgraph *TemplateSubgraph) map[string][]string {
	dependencies := make(map[string][]string)
	for _, dep := range subgraph.Dependencies {
		dependencies[dep.IssueID] = append(dependencies[dep.IssueID], dep.DependsOnID)
	}
	return dependencies
}

func issueToFormulaStep(issue *types.Issue, rootID string, stepIDs map[string]string, dependencies map[string][]string, replacements map[string]string) *formula.Step {
	step := &formula.Step{
		ID:          stepIDs[issue.ID],
		Title:       replaceFormulaVars(issue.Title, replacements),
		Description: replaceFormulaVars(issue.Description, replacements),
		Type:        string(issue.IssueType),
	}
	if issue.Priority > 0 {
		priority := issue.Priority
		step.Priority = &priority
	}
	step.Labels = formulaLabels(issue.Labels)
	step.DependsOn = distilledStepDependencies(dependencies[issue.ID], rootID, stepIDs)
	return step
}

func formulaLabels(labels []string) []string {
	var result []string
	for _, label := range labels {
		if label != MoleculeLabel && !strings.HasPrefix(label, "mol:") {
			result = append(result, label)
		}
	}
	return result
}

func distilledStepDependencies(dependencies []string, rootID string, stepIDs map[string]string) []string {
	var result []string
	for _, dependencyID := range dependencies {
		if dependencyID == rootID {
			continue
		}
		if stepID, ok := stepIDs[dependencyID]; ok {
			result = append(result, stepID)
		}
	}
	return result
}

func formulaVariables(replacements map[string]string) map[string]*formula.VarDef {
	variables := make(map[string]*formula.VarDef, len(replacements))
	for _, name := range replacements {
		variables[name] = &formula.VarDef{Description: fmt.Sprintf("Value for %s", name), Required: true}
	}
	return variables
}

func init() {
	molCmd.AddCommand(newMolDistillCmd())
}
