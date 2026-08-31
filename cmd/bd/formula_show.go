package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jonbaldie/beads/internal/formula"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

func runFormulaList(cmd *cobra.Command, _ []string) error {
	evt := metrics.NewCommandEvent("formula-list")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	typeFilter, _ := cmd.Flags().GetString("type")
	searchPaths := getFormulaSearchPaths()
	entries := collectFormulaListEntries(searchPaths, typeFilter)
	return renderFormulaList(entries, searchPaths)
}

func collectFormulaListEntries(searchPaths []string, typeFilter string) []FormulaListEntry {
	seen := make(map[string]bool)
	var entries []FormulaListEntry

	for _, dir := range searchPaths {
		formulas, err := scanFormulaDir(dir)
		if err != nil {
			continue
		}

		for _, f := range formulas {
			if seen[f.Formula] {
				continue
			}
			seen[f.Formula] = true

			if typeFilter != "" && string(f.Type) != typeFilter {
				continue
			}

			entries = append(entries, FormulaListEntry{
				Name:        f.Formula,
				Type:        string(f.Type),
				Description: truncateDescription(f.Description, 60),
				Source:      f.Source,
				Steps:       countSteps(f.Steps),
				Vars:        len(f.Vars),
			})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
	return entries
}

func renderFormulaList(entries []FormulaListEntry, searchPaths []string) error {
	if isJSONOutput() {
		return outputJSON(entries)
	}

	if len(entries) == 0 {
		printEmptyFormulaList(searchPaths)
		return nil
	}

	printFormulaList(entries)
	return nil
}

func printEmptyFormulaList(searchPaths []string) {
	fmt.Println("No formulas found.")
	fmt.Println("\nFormulas are .formula.toml / .formula.json files in:")
	for _, p := range searchPaths {
		fmt.Printf("  %s\n", p)
	}
	fmt.Println("\n`bd cook --persist` writes a proto to the database, not a formula file.")
	fmt.Println("Pour a formula file directly with: bd pour <file.formula.toml>")
}

func printFormulaList(entries []FormulaListEntry) {
	fmt.Printf("Formulas (%d found)\n\n", len(entries))

	byType := make(map[string][]FormulaListEntry)
	for _, e := range entries {
		byType[e.Type] = append(byType[e.Type], e)
	}

	typeOrder := []string{"workflow", "expansion", "aspect", "convoy"}
	for _, t := range typeOrder {
		typeEntries := byType[t]
		if len(typeEntries) == 0 {
			continue
		}

		typeIcon := getTypeIcon(t)
		fmt.Printf("%s %s:\n", typeIcon, strings.Title(t))

		for _, e := range typeEntries {
			varInfo := ""
			if e.Vars > 0 {
				varInfo = fmt.Sprintf(" (%d vars)", e.Vars)
			}
			fmt.Printf("  %-25s %s%s\n", e.Name, e.Description, varInfo)
		}
		fmt.Println()
	}
}

func runFormulaShow(_ *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("formula-show")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	name := args[0]

	parser := formula.NewParser()

	f, err := parser.LoadByName(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		fmt.Fprintf(os.Stderr, "\nSearch paths:\n")
		for _, p := range getFormulaSearchPaths() {
			fmt.Fprintf(os.Stderr, "  %s\n", p)
		}
		return SilentExit()
	}

	if isJSONOutput() {
		return outputJSON(f)
	}

	printFormulaShowMetadata(f)

	printFormulaShowExtends(f)

	printFormulaShowVariables(f)

	printFormulaShowSteps(f)
	printFormulaShowTemplate(f)
	printFormulaShowAdvice(f)

	printFormulaShowComposition(f)

	printFormulaShowPointcuts(f)

	fmt.Println()
	return nil
}

func printFormulaShowMetadata(f *formula.Formula) {
	typeIcon := getTypeIcon(string(f.Type))
	fmt.Printf("\n%s %s\n", typeIcon, f.Formula)
	fmt.Printf("   Type: %s\n", f.Type)
	if f.Description != "" {
		fmt.Printf("   Description: %s\n", f.Description)
	}
	fmt.Printf("   Source: %s\n", f.Source)
}

func printFormulaShowExtends(f *formula.Formula) {
	if len(f.Extends) == 0 {
		return
	}

	fmt.Printf("\n%s Extends:\n", ui.RenderAccent("📎"))
	for _, ext := range f.Extends {
		fmt.Printf("   - %s\n", ext)
	}
}

func printFormulaShowVariables(f *formula.Formula) {
	if len(f.Vars) == 0 {
		return
	}

	fmt.Printf("\n%s Variables:\n", ui.RenderWarn("📝"))
	varNames := sortedFormulaVariableNames(f.Vars)

	for _, name := range varNames {
		fmt.Println(formatFormulaVariable(name, f.Vars[name]))
	}
}

func sortedFormulaVariableNames(vars map[string]*formula.VarDef) []string {
	varNames := make([]string, 0, len(vars))
	for name := range vars {
		varNames = append(varNames, name)
	}
	sort.Strings(varNames)
	return varNames
}

func formatFormulaVariable(name string, v *formula.VarDef) string {
	attrs := []string{}
	if v.Required {
		attrs = append(attrs, ui.RenderFail("required"))
	}
	if v.Default != nil {
		attrs = append(attrs, fmt.Sprintf("default=%q", *v.Default))
	}
	if len(v.Enum) > 0 {
		attrs = append(attrs, fmt.Sprintf("enum=[%s]", strings.Join(v.Enum, ",")))
	}
	if v.Pattern != "" {
		attrs = append(attrs, fmt.Sprintf("pattern=%q", v.Pattern))
	}
	attrStr := formatFormulaVariableAttributes(attrs)
	desc := ""
	if v.Description != "" {
		desc = fmt.Sprintf(": %s", v.Description)
	}
	return fmt.Sprintf("   {{%s}}%s%s", name, desc, attrStr)
}

func formatFormulaVariableAttributes(attrs []string) string {
	if len(attrs) == 0 {
		return ""
	}
	return fmt.Sprintf(" [%s]", strings.Join(attrs, ", "))
}

func printFormulaShowSteps(f *formula.Formula) {
	if len(f.Steps) == 0 {
		return
	}

	fmt.Printf("\n%s Steps (%d):\n", ui.RenderPass("🌲"), countSteps(f.Steps))
	printFormulaStepsTree(f.Steps, "   ")
}

func printFormulaShowTemplate(f *formula.Formula) {
	if len(f.Template) == 0 {
		return
	}

	fmt.Printf("\n%s Template (%d steps):\n", ui.RenderAccent("📐"), len(f.Template))
	printFormulaStepsTree(f.Template, "   ")
}

func printFormulaShowAdvice(f *formula.Formula) {
	if len(f.Advice) == 0 {
		return
	}

	fmt.Printf("\n%s Advice:\n", ui.RenderWarn("💡"))
	for _, a := range f.Advice {
		parts := []string{}
		if a.Before != nil {
			parts = append(parts, fmt.Sprintf("before: %s", a.Before.ID))
		}
		if a.After != nil {
			parts = append(parts, fmt.Sprintf("after: %s", a.After.ID))
		}
		if a.Around != nil {
			parts = append(parts, "around")
		}
		fmt.Printf("   %s → %s\n", a.Target, strings.Join(parts, ", "))
	}
}

func printFormulaShowComposition(f *formula.Formula) {
	if f.Compose == nil || !hasFormulaComposition(f.Compose) {
		return
	}

	fmt.Printf("\n%s Composition:\n", ui.RenderAccent("🔗"))
	printFormulaBondPoints(f.Compose.BondPoints)
	printFormulaExpansions(f.Compose.Expand)
	printFormulaMaps(f.Compose.Map)
	printFormulaAspects(f.Compose.Aspects)
}

func hasFormulaComposition(compose *formula.ComposeRules) bool {
	return len(compose.BondPoints) > 0 || len(compose.Expand) > 0 ||
		len(compose.Map) > 0 || len(compose.Aspects) > 0
}

func printFormulaBondPoints(bondPoints []*formula.BondPoint) {
	if len(bondPoints) == 0 {
		return
	}

	fmt.Printf("   Bond Points:\n")
	for _, bp := range bondPoints {
		loc := ""
		if bp.AfterStep != "" {
			loc = fmt.Sprintf("after %s", bp.AfterStep)
		} else if bp.BeforeStep != "" {
			loc = fmt.Sprintf("before %s", bp.BeforeStep)
		}
		fmt.Printf("     - %s (%s)\n", bp.ID, loc)
	}
}

func printFormulaExpansions(expansions []*formula.ExpandRule) {
	if len(expansions) == 0 {
		return
	}

	fmt.Printf("   Expansions:\n")
	for _, e := range expansions {
		fmt.Printf("     - %s → %s\n", e.Target, e.With)
	}
}

func printFormulaMaps(maps []*formula.MapRule) {
	if len(maps) == 0 {
		return
	}

	fmt.Printf("   Maps:\n")
	for _, m := range maps {
		fmt.Printf("     - %s → %s\n", m.Select, m.With)
	}
}

func printFormulaAspects(aspects []string) {
	if len(aspects) == 0 {
		return
	}

	fmt.Printf("   Aspects: %s\n", strings.Join(aspects, ", "))
}

func printFormulaShowPointcuts(f *formula.Formula) {
	if len(f.Pointcuts) == 0 {
		return
	}

	fmt.Printf("\n%s Pointcuts:\n", ui.RenderWarn("🎯"))
	for _, p := range f.Pointcuts {
		parts := []string{}
		if p.Glob != "" {
			parts = append(parts, fmt.Sprintf("glob=%q", p.Glob))
		}
		if p.Type != "" {
			parts = append(parts, fmt.Sprintf("type=%q", p.Type))
		}
		if p.Label != "" {
			parts = append(parts, fmt.Sprintf("label=%q", p.Label))
		}
		fmt.Printf("   - %s\n", strings.Join(parts, ", "))
	}
}

// getFormulaSearchPaths returns the formula search paths in priority order.
func getFormulaSearchPaths() []string {
	return formula.DefaultSearchPaths()
}

// scanFormulaDir scans a directory for formula files (both TOML and JSON).
func scanFormulaDir(dir string) ([]*formula.Formula, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	parser := formula.NewParser(dir)
	var formulas []*formula.Formula

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Support both .formula.toml and .formula.json
		name := entry.Name()
		if !strings.HasSuffix(name, formula.FormulaExtTOML) && !strings.HasSuffix(name, formula.FormulaExtJSON) {
			continue
		}

		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(path)
		if err != nil {
			continue // Skip invalid formulas
		}
		formulas = append(formulas, f)
	}

	return formulas, nil
}

// countSteps recursively counts steps including children.
func countSteps(steps []*formula.Step) int {
	count := len(steps)
	for _, s := range steps {
		count += countSteps(s.Children)
	}
	return count
}

// truncateDescription truncates a description to maxLen characters.
func truncateDescription(desc string, maxLen int) string {
	// Take first line only
	if idx := strings.Index(desc, "\n"); idx >= 0 {
		desc = desc[:idx]
	}
	return truncate(desc, maxLen)
}

// getTypeIcon returns an icon for the formula type.
func getTypeIcon(t string) string {
	switch t {
	case "workflow":
		return "📋"
	case "expansion":
		return "📐"
	case "aspect":
		return "🎯"
	case "convoy":
		return "🚐"
	default:
		return "📜"
	}
}

// printFormulaStepsTree prints steps in a tree format.
func printFormulaStepsTree(steps []*formula.Step, indent string) {
	for i, step := range steps {
		last := i == len(steps)-1
		fmt.Printf("%s%s %s: %s%s%s\n", indent, formulaStepConnector(last), step.ID, step.Title,
			formulaStepType(step), formulaStepDependencies(step))

		if len(step.Children) > 0 {
			printFormulaStepsTree(step.Children, formulaStepChildIndent(indent, last))
		}
	}
}

func formulaStepConnector(last bool) string {
	if last {
		return "└──"
	}
	return "├──"
}

func formulaStepDependencies(step *formula.Step) string {
	var depParts []string
	if len(step.DependsOn) > 0 {
		depParts = append(depParts, fmt.Sprintf("depends: %s", strings.Join(step.DependsOn, ", ")))
	}
	if len(step.Needs) > 0 {
		depParts = append(depParts, fmt.Sprintf("needs: %s", strings.Join(step.Needs, ", ")))
	}
	if step.WaitsFor != "" {
		depParts = append(depParts, fmt.Sprintf("waits_for: %s", step.WaitsFor))
	}
	if len(depParts) == 0 {
		return ""
	}
	return fmt.Sprintf(" [%s]", strings.Join(depParts, ", "))
}

func formulaStepType(step *formula.Step) string {
	if step.Type == "" || step.Type == "task" {
		return ""
	}
	return fmt.Sprintf(" (%s)", step.Type)
}

func formulaStepChildIndent(indent string, last bool) string {
	if last {
		return indent + "    "
	}
	return indent + "│   "
}

// formulaConvertCmd converts JSON formulas to TOML.
var formulaConvertCmd = &cobra.Command{
	Use:   "convert <formula-name|path> [--all]",
	Short: "Convert formula from JSON to TOML",
	Long: `Convert formula files from JSON to TOML format.

TOML format provides better ergonomics:
  - Multi-line strings without \n escaping
  - Human-readable diffs
  - Comments allowed

The convert command reads a .formula.json file and outputs .formula.toml.
The original JSON file is preserved (use --delete to remove it).

Examples:
  bd formula convert shiny              # Convert shiny.formula.json to .toml
  bd formula convert ./my.formula.json  # Convert specific file
  bd formula convert --all              # Convert all JSON formulas
  bd formula convert shiny --delete     # Convert and remove JSON file
  bd formula convert shiny --stdout     # Print TOML to stdout`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runFormulaConvert,
}
