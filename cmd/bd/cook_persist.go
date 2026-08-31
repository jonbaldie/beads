package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/formula"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

type cookLabel struct {
	issueID string
	label   string
}

type cookPersistData struct {
	issues []*types.Issue
	deps   []*types.Dependency
	labels []cookLabel
}

// resolveAndCookFormula loads a formula by name, resolves it, applies all transformations,
// and returns an in-memory TemplateSubgraph ready for instantiation.
// This is the main entry point for ephemeral proto cooking.
func resolveAndCookFormula(formulaName string, searchPaths []string) (*TemplateSubgraph, error) {
	return resolveAndCookFormulaWithVars(formulaName, searchPaths, nil)
}

// resolveAndCookFormulaWithVars loads a formula and optionally filters steps by condition.
// If conditionVars is provided, steps with conditions that evaluate to false are excluded.
// Pass nil for conditionVars to include all steps (condition filtering skipped).
func resolveAndCookFormulaWithVars(formulaName string, searchPaths []string, conditionVars map[string]string) (*TemplateSubgraph, error) {
	parser := formula.NewParser(searchPaths...)
	resolved, err := loadAndValidateCookFormula(parser, formulaName, conditionVars)
	if err != nil {
		return nil, err
	}

	if err := applyResolvedCookFormula(parser, resolved, formulaName); err != nil {
		return nil, err
	}

	if err := filterCookFormulaSteps(resolved, conditionVars); err != nil {
		return nil, err
	}
	if err := materializeCookExpansion(resolved, formulaName, conditionVars); err != nil {
		return nil, err
	}

	// Cook to in-memory subgraph, including variable definitions for default handling
	return cookFormulaToSubgraphWithVars(resolved, resolved.Formula, resolved.Vars)
}

func loadAndValidateCookFormula(parser *formula.Parser, formulaName string, conditionVars map[string]string) (*formula.Formula, error) {
	f, err := parser.LoadByName(formulaName)
	if err != nil {
		return nil, fmt.Errorf("loading formula %q: %w", formulaName, err)
	}
	resolved, err := parser.Resolve(f)
	if err != nil {
		return nil, fmt.Errorf("resolving formula %q: %w", formulaName, err)
	}
	// Validate provided values, while leaving missing-variable UX to callers.
	if conditionVars != nil {
		if err := formula.ValidateProvidedVars(resolved, conditionVars); err != nil {
			return nil, fmt.Errorf("formula %q: %w", formulaName, err)
		}
	}
	return resolved, nil
}

func applyResolvedCookFormula(parser *formula.Parser, resolved *formula.Formula, formulaName string) error {
	controlFlowSteps, err := formula.ApplyControlFlow(resolved.Steps, resolved.Compose)
	if err != nil {
		return fmt.Errorf("applying control flow to %q: %w", formulaName, err)
	}
	resolved.Steps = applyCookAdvice(controlFlowSteps, resolved.Advice)
	inlineExpandedSteps, err := formula.ApplyInlineExpansions(resolved.Steps, parser)
	if err != nil {
		return fmt.Errorf("applying inline step expansions to %q: %w", formulaName, err)
	}
	resolved.Steps = inlineExpandedSteps
	if err := applyNamedCookFormulaExpansions(resolved, parser, formulaName); err != nil {
		return err
	}
	return applyNamedCookFormulaAspects(resolved, parser)
}

func applyNamedCookFormulaExpansions(resolved *formula.Formula, parser *formula.Parser, formulaName string) error {
	if resolved.Compose == nil || (len(resolved.Compose.Expand) == 0 && len(resolved.Compose.Map) == 0) {
		return nil
	}
	expandedSteps, err := formula.ApplyExpansions(resolved.Steps, resolved.Compose, parser)
	if err != nil {
		return fmt.Errorf("applying expansions to %q: %w", formulaName, err)
	}
	resolved.Steps = expandedSteps
	return nil
}

func applyNamedCookFormulaAspects(resolved *formula.Formula, parser *formula.Parser) error {
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

func filterCookFormulaSteps(resolved *formula.Formula, conditionVars map[string]string) error {
	if conditionVars == nil {
		return nil
	}
	mergedVars := make(map[string]string)
	for name, def := range resolved.Vars {
		if def != nil && def.Default != nil {
			mergedVars[name] = *def.Default
		}
	}
	for name, value := range conditionVars {
		mergedVars[name] = value
	}
	filteredSteps, err := formula.FilterStepsByCondition(resolved.Steps, mergedVars)
	if err != nil {
		return fmt.Errorf("filtering steps by condition: %w", err)
	}
	resolved.Steps = filteredSteps
	return nil
}

func materializeCookExpansion(resolved *formula.Formula, formulaName string, conditionVars map[string]string) error {
	if resolved.Type != formula.TypeExpansion || len(resolved.Template) == 0 {
		return nil
	}
	expansionVars := make(map[string]string)
	for name, def := range resolved.Vars {
		if def != nil && def.Default != nil {
			expansionVars[name] = *def.Default
		}
	}
	for name, value := range conditionVars {
		expansionVars[name] = value
	}
	if err := formula.MaterializeExpansion(resolved, "main", expansionVars); err != nil {
		return fmt.Errorf("standalone expansion %q: %w", formulaName, err)
	}
	return nil
}

// cookFormulaToSubgraphWithVars creates an in-memory subgraph with variable info attached
func cookFormulaToSubgraphWithVars(f *formula.Formula, protoID string, vars map[string]*formula.VarDef) (*TemplateSubgraph, error) {
	subgraph, err := cookFormulaToSubgraph(f, protoID)
	if err != nil {
		return nil, err
	}
	// Attach variable definitions to the subgraph for default handling during pour
	// Convert from *VarDef to VarDef for simpler handling
	if vars != nil {
		subgraph.VarDefs = make(map[string]formula.VarDef)
		for k, v := range vars {
			if v != nil {
				subgraph.VarDefs[k] = *v
			}
		}
	}
	// Attach recommended phase and pour flag from formula
	subgraph.Phase = f.Phase
	subgraph.Pour = f.Pour
	return subgraph, nil
}

// cookFormula creates a proto bead from a resolved formula.
// protoID is the final ID for the proto (may include a prefix).
func cookFormula(ctx context.Context, s storage.DoltStorage, f *formula.Formula, protoID string) (*cookFormulaResult, error) {
	if s == nil {
		return nil, fmt.Errorf("no database connection")
	}
	data := buildCookPersistData(f, protoID)
	if err := persistCookData(ctx, s, protoID, data); err != nil {
		return nil, err
	}
	return &cookFormulaResult{ProtoID: protoID, Created: len(data.issues)}, nil
}

func buildCookPersistData(f *formula.Formula, protoID string) *cookPersistData {
	rootIssue := newCookRootIssue(f, protoID)
	data := &cookPersistData{
		issues: []*types.Issue{rootIssue},
		labels: []cookLabel{{issueID: protoID, label: MoleculeLabel}},
	}

	idMapping := make(map[string]string)
	collectSteps(f.Steps, protoID, idMapping, nil, &data.issues, &data.deps, func(issueID, label string) {
		data.labels = append(data.labels, cookLabel{issueID: issueID, label: label})
	})
	for _, step := range f.Steps {
		collectDependencies(step, idMapping, &data.deps)
	}
	return data
}

func newCookRootIssue(f *formula.Formula, protoID string) *types.Issue {
	rootTitle := f.Formula
	if _, hasTitle := f.Vars["title"]; hasTitle {
		rootTitle = "{{title}}"
	}
	rootDesc := f.Description
	if _, hasDesc := f.Vars["desc"]; hasDesc {
		rootDesc = "{{desc}}"
	}
	return &types.Issue{
		IssueID: types.IssueID{
			ID: protoID,
		},
		IssueContent: types.IssueContent{
			Title:       rootTitle,
			Description: rootDesc,
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeMolecule,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
		IssueWisp: types.IssueWisp{
			IsTemplate: true,
		},
	}
}

func persistCookData(ctx context.Context, s storage.DoltStorage, protoID string, data *cookPersistData) error {
	return transact(ctx, s, fmt.Sprintf("bd: cook formula %s", protoID), func(tx storage.Transaction) error {
		// Flatten unregistered step types to task (with a warning) before
		// inserting, mirroring cloneSubgraphInto (pour). Without this,
		// PrepareIssueForInsert rejects them with "invalid issue type" and
		// the whole cook --persist transaction rolls back.
		if err := flattenUnregisteredIssueTypes(ctx, storeMolWriter{DoltStorage: s, tx: tx}, data.issues, data.deps); err != nil {
			return fmt.Errorf("checking custom types: %w", err)
		}
		if err := tx.CreateIssues(ctx, data.issues, getActor()); err != nil {
			return fmt.Errorf("failed to create issues: %w", err)
		}
		if err := addCookLabels(ctx, tx, data.labels); err != nil {
			return err
		}
		if err := addCookDependencies(ctx, tx, data.deps); err != nil {
			return err
		}
		return nil
	})
}

func addCookLabels(ctx context.Context, tx storage.Transaction, labels []cookLabel) error {
	for _, label := range labels {
		if err := tx.AddLabel(ctx, label.issueID, label.label, getActor()); err != nil {
			return fmt.Errorf("failed to add label %s to %s: %w", label.label, label.issueID, err)
		}
	}
	return nil
}

func addCookDependencies(ctx context.Context, tx storage.Transaction, deps []*types.Dependency) error {
	for _, dep := range deps {
		if err := tx.AddDependency(ctx, dep, getActor()); err != nil {
			return fmt.Errorf("failed to create dependency: %w", err)
		}
	}
	return nil
}

// collectDependencies collects blocking dependencies from depends_on, needs, and waits_for fields.
// This is the shared implementation used by both DB-persisted and in-memory subgraph cooking.
func collectDependencies(step *formula.Step, idMapping map[string]string, deps *[]*types.Dependency) {
	issueID := idMapping[step.ID]
	waitsFor := parseCookWaitsFor(step)
	collapsedBlocks := appendCookBlockingDependencies(issueID, step.DependsOn, waitsFor.spawnerStepID, idMapping, deps)
	if appendCookBlockingDependencies(issueID, step.Needs, waitsFor.spawnerStepID, idMapping, deps) {
		collapsedBlocks = true
	}
	appendCookWaitsForDependency(issueID, waitsFor, collapsedBlocks, idMapping, deps)

	// Recursively handle children
	for _, child := range step.Children {
		collectDependencies(child, idMapping, deps)
	}
}

type cookWaitsFor struct {
	spec          *formula.WaitsForSpec
	spawnerStepID string
}

func parseCookWaitsFor(step *formula.Step) cookWaitsFor {
	if step.WaitsFor == "" {
		return cookWaitsFor{}
	}
	spec := formula.ParseWaitsFor(step.WaitsFor)
	if spec == nil {
		return cookWaitsFor{}
	}
	spawnerStepID := spec.SpawnerID
	if spawnerStepID == "" && len(step.Needs) > 0 {
		spawnerStepID = step.Needs[0]
	}
	return cookWaitsFor{spec: spec, spawnerStepID: spawnerStepID}
}

func appendCookBlockingDependencies(issueID string, dependencyIDs []string, waitsForSpawnerStepID string,
	idMapping map[string]string, deps *[]*types.Dependency) bool {
	collapsed := false
	for _, dependencyID := range dependencyIDs {
		if dependencyID == waitsForSpawnerStepID {
			collapsed = true
			continue
		}
		dependencyIssueID, ok := idMapping[dependencyID]
		if !ok {
			continue
		}
		*deps = append(*deps, &types.Dependency{
			IssueID:     issueID,
			DependsOnID: dependencyIssueID,
			Type:        types.DepBlocks,
		})
	}
	return collapsed
}

func appendCookWaitsForDependency(issueID string, waitsFor cookWaitsFor, collapsedBlocks bool,
	idMapping map[string]string, deps *[]*types.Dependency) {
	if waitsFor.spec == nil || waitsFor.spawnerStepID == "" {
		return
	}
	spawnerIssueID, ok := idMapping[waitsFor.spawnerStepID]
	if !ok {
		return
	}
	var dep *types.Dependency
	var err error
	if collapsedBlocks {
		dep, err = types.NewWaitsForBlockingDependency(issueID, spawnerIssueID, waitsFor.spec.Gate)
	} else {
		dep, err = types.NewWaitsForDependency(issueID, spawnerIssueID, waitsFor.spec.Gate)
	}
	if err == nil {
		*deps = append(*deps, dep)
	}
}

// deleteProtoSubgraph deletes a proto and all its children.
func deleteProtoSubgraph(ctx context.Context, s storage.DoltStorage, protoID string) error {
	// Load the subgraph
	subgraph, err := loadTemplateSubgraph(ctx, s, protoID)
	if err != nil {
		return fmt.Errorf("load proto: %w", err)
	}

	// Delete in reverse order (children first)
	return transact(ctx, s, fmt.Sprintf("bd: delete proto subgraph %s", protoID), func(tx storage.Transaction) error {
		for i := len(subgraph.Issues) - 1; i >= 0; i-- {
			issue := subgraph.Issues[i]
			if err := tx.DeleteIssue(ctx, issue.ID); err != nil {
				return fmt.Errorf("delete %s: %w", issue.ID, err)
			}
		}
		return nil
	})
}

// printFormulaSteps prints steps in a tree format.
func printFormulaSteps(steps []*formula.Step, indent string) {
	for i, step := range steps {
		printFormulaStep(step, indent, i == len(steps)-1)
	}
}

func printFormulaStep(step *formula.Step, indent string, last bool) {
	connector := "├──"
	if last {
		connector = "└──"
	}
	depStr := formatFormulaStepDependencies(step)
	typeStr := formatFormulaStepType(step)
	sourceStr := formatFormulaStepSource(step)
	fmt.Printf("%s%s %s: %s%s%s%s\n", indent, connector, step.ID, step.Title, typeStr, depStr, sourceStr)

	if len(step.Children) > 0 {
		childIndent := indent
		if last {
			childIndent += "    "
		} else {
			childIndent += "│   "
		}
		printFormulaSteps(step.Children, childIndent)
	}
}

func formatFormulaStepDependencies(step *formula.Step) string {
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

func formatFormulaStepType(step *formula.Step) string {
	if step.Type == "" || step.Type == "task" {
		return ""
	}
	return fmt.Sprintf(" (%s)", step.Type)
}

func formatFormulaStepSource(step *formula.Step) string {
	if step.SourceFormula == "" && step.SourceLocation == "" {
		return ""
	}
	return fmt.Sprintf(" [from: %s@%s]", step.SourceFormula, step.SourceLocation)
}

// substituteFormulaVars substitutes {{variable}} placeholders in a formula.
// This is used in runtime mode to fully resolve the formula before output.
func substituteFormulaVars(f *formula.Formula, vars map[string]string) {
	// Substitute in top-level fields
	f.Description = substituteVariables(f.Description, vars)

	// Substitute in all steps recursively
	substituteStepVars(f.Steps, vars)
}

// substituteStepVars recursively substitutes variables in step fields.
func substituteStepVars(steps []*formula.Step, vars map[string]string) {
	for _, step := range steps {
		step.Title = substituteVariables(step.Title, vars)
		step.Description = substituteVariables(step.Description, vars)
		step.Notes = substituteVariables(step.Notes, vars)
		if step.Gate != nil {
			step.Gate.Type = substituteVariables(step.Gate.Type, vars)
			step.Gate.ID = substituteVariables(step.Gate.ID, vars)
			step.Gate.AwaitID = substituteVariables(step.Gate.AwaitID, vars)
			step.Gate.Timeout = substituteVariables(step.Gate.Timeout, vars)
			step.Gate.Repo = substituteVariables(step.Gate.Repo, vars)
		}
		if len(step.Children) > 0 {
			substituteStepVars(step.Children, vars)
		}
	}
}

func init() {
	cookCmd.Flags().Bool("dry-run", false, "Preview what would be created")
	cookCmd.Flags().Bool("persist", false, "Persist proto to database (legacy behavior)")
	cookCmd.Flags().Bool("force", false, "Replace existing proto if it exists (requires --persist)")
	cookCmd.Flags().StringSlice("search-path", []string{}, "Additional paths to search for formula inheritance")
	cookCmd.Flags().String("prefix", "", "Prefix to prepend to proto ID (e.g., 'gt-' creates 'gt-mol-feature')")
	cookCmd.Flags().StringArray("var", []string{}, "Variable substitution (key=value), enables runtime mode")
	cookCmd.Flags().String("mode", "", "Cooking mode: compile (keep placeholders) or runtime (substitute vars)")

	rootCmd.AddCommand(cookCmd)
}
