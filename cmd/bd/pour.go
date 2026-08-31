package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/formula"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

// pourCmd is a top-level command for instantiating protos as persistent mols.
//
// In the molecular chemistry metaphor:
//   - Proto (solid) -> pour -> Mol (liquid)
//   - Pour creates persistent, auditable work in .beads/
var pourCmd = &cobra.Command{
	Use:   "pour <proto-id>",
	Short: "Instantiate a proto as a persistent mol (solid -> liquid)",
	Long: `Pour a proto into a persistent mol - like pouring molten metal into a mold.

This is the chemistry-inspired command for creating PERSISTENT work from templates.
The resulting mol is stored as persistent beads in the issue database and
syncs like any other bead (bd dolt push / pull).

Phase transition: Proto (solid) -> pour -> Mol (liquid)

WHEN TO USE POUR vs WISP:
  pour (liquid): Persistent work that needs audit trail
    - Feature implementations spanning multiple sessions
    - Work you may need to reference later
    - Anything worth preserving in git history

  wisp (vapor): Ephemeral work that auto-cleans up
    - Release workflows (one-time execution)
    - Operational loops and recurring cycles
    - Health checks and diagnostics
    - Any operational workflow without audit value

TIP: Formulas can specify phase:"vapor" to recommend wisp usage.
     If you pour a vapor-phase formula, you'll get a warning.

Examples:
  bd mol pour mol-feature --var name=auth    # Persistent feature work
  bd mol pour mol-review --var pr=123        # Persistent code review`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runPour,
}

type pourInput struct {
	protoArg   string
	dryRun     bool
	varFlags   []string
	assignee   string
	attachArgs []string
	attachType string
}

func gatherPourInput(cmd *cobra.Command, args []string) pourInput {
	in := pourInput{protoArg: args[0]}
	in.dryRun, _ = cmd.Flags().GetBool("dry-run")
	in.varFlags, _ = cmd.Flags().GetStringArray("var")
	in.assignee, _ = cmd.Flags().GetString("assignee")
	in.attachArgs, _ = cmd.Flags().GetStringSlice("attach")
	in.attachType, _ = cmd.Flags().GetString("attach-type")
	return in
}

func parseVarFlags(varFlags []string) (map[string]string, error) {
	vars := make(map[string]string)
	for _, v := range varFlags {
		parts := strings.SplitN(v, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid variable format '%s', expected 'key=value'", v)
		}
		vars[parts[0]] = parts[1]
	}
	return vars, nil
}

func runPour(cmd *cobra.Command, args []string) error {
	if err := CheckReadonly("pour"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("pour")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	in := gatherPourInput(cmd, args)
	if usesProxiedServer() {
		return runPourProxiedServer(getRootContext(), in)
	}
	return runPourDirect(getRootContext(), in)
}

func runPourDirect(ctx context.Context, in pourInput) error {
	if getStore() == nil {
		return HandleError("no database connection")
	}
	vars, err := parseVarFlags(in.varFlags)
	if err != nil {
		return HandleError("%v", err)
	}
	subgraph, protoID, err := resolvePourTemplate(ctx, in, vars)
	if err != nil {
		return err
	}
	attachments, err := loadPourAttachments(ctx, in.attachArgs)
	if err != nil {
		return err
	}
	vars = applyVariableDefaults(vars, subgraph)
	if err := validatePourVars(subgraph, attachments, vars); err != nil {
		return err
	}
	if in.dryRun {
		return renderPourDirectDryRun(protoID, subgraph, vars, in, attachments)
	}
	return executePourDirect(ctx, subgraph, vars, in, attachments)
}

type pourAttachment struct {
	id       string
	issue    *types.Issue
	subgraph *TemplateSubgraph
}

func resolvePourTemplate(ctx context.Context, in pourInput, vars map[string]string) (*TemplateSubgraph, string, error) {
	sg, err := resolveAndCookFormulaWithVars(in.protoArg, nil, vars)
	if err == nil {
		if sg.Phase == "vapor" {
			warnPourVaporFormula(in.protoArg, in.varFlags)
		}
		return sg, sg.Root.ID, nil
	}
	if errors.Is(err, formula.ErrVarValidation) {
		// in.protoArg IS a formula; the --var values it was given fail
		// enum/pattern/required-empty constraints. Report that directly
		// instead of falling through to the proto-ID lookup below, which
		// would otherwise mask this as "not found as formula or proto ID".
		return nil, "", HandleError("%v", err)
	}
	return loadPourProto(ctx, in.protoArg)
}

func loadPourProto(ctx context.Context, protoArg string) (*TemplateSubgraph, string, error) {
	protoID, err := utils.ResolvePartialID(ctx, getStore(), protoArg)
	if err != nil {
		return nil, "", HandleError("%s not found as formula or proto ID", protoArg)
	}
	protoIssue, err := getStore().GetIssue(ctx, protoID)
	if err != nil {
		return nil, "", HandleError("loading proto %s: %v", protoID, err)
	}
	if !isProto(protoIssue) {
		return nil, "", HandleError("%s is not a proto (missing '%s' label)", protoID, MoleculeLabel)
	}
	subgraph, err := loadTemplateSubgraph(ctx, getStore(), protoID)
	if err != nil {
		return nil, "", HandleError("loading proto: %v", err)
	}
	return subgraph, protoID, nil
}

func loadPourAttachments(ctx context.Context, attachArgs []string) ([]pourAttachment, error) {
	var attachments []pourAttachment
	for _, attachArg := range attachArgs {
		attach, err := loadPourAttachment(ctx, attachArg)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, attach)
	}
	return attachments, nil
}

func loadPourAttachment(ctx context.Context, attachArg string) (pourAttachment, error) {
	attachID, err := utils.ResolvePartialID(ctx, getStore(), attachArg)
	if err != nil {
		return pourAttachment{}, HandleError("resolving attachment ID %s: %v", attachArg, err)
	}
	attachIssue, err := getStore().GetIssue(ctx, attachID)
	if err != nil {
		return pourAttachment{}, HandleError("loading attachment %s: %v", attachID, err)
	}
	if !isProto(attachIssue) {
		return pourAttachment{}, HandleError("%s is not a proto (missing '%s' label)", attachID, MoleculeLabel)
	}
	attachSubgraph, err := loadTemplateSubgraph(ctx, getStore(), attachID)
	if err != nil {
		return pourAttachment{}, HandleError("loading attachment subgraph %s: %v", attachID, err)
	}
	return pourAttachment{attachID, attachIssue, attachSubgraph}, nil
}

func validatePourVars(subgraph *TemplateSubgraph, attachments []pourAttachment, vars map[string]string) error {
	var attachSubgraphs []*TemplateSubgraph
	for _, a := range attachments {
		attachSubgraphs = append(attachSubgraphs, a.subgraph)
	}
	if err := checkPourVars(subgraph, attachSubgraphs, vars); err != nil {
		return HandleErrorWithHint(err.Error(), fmt.Sprintf("Provide them with: --var %s=<value>", missingVarHint(subgraph, attachSubgraphs, vars)))
	}
	return nil
}

func renderPourDirectDryRun(protoID string, subgraph *TemplateSubgraph, vars map[string]string, in pourInput, attachments []pourAttachment) error {
	var previews []pourAttachPreview
	for _, a := range attachments {
		previews = append(previews, pourAttachPreview{title: a.issue.Title, steps: len(a.subgraph.Issues)})
	}
	renderPourDryRun(protoID, subgraph, vars, in.assignee, in.attachType, previews)
	return nil
}

func executePourDirect(ctx context.Context, subgraph *TemplateSubgraph, vars map[string]string, in pourInput, attachments []pourAttachment) error {
	result, err := spawnMolecule(ctx, getStore(), subgraph, vars, in.assignee, getActor(), false, types.IDPrefixMol)
	if err != nil {
		return HandleError("pouring proto: %v", err)
	}
	totalAttached, err := attachPourMolecules(ctx, result.NewEpicID, vars, in, attachments)
	if err != nil {
		return err
	}
	return renderPourResult(result, totalAttached, len(attachments))
}

func attachPourMolecules(ctx context.Context, epicID string, vars map[string]string, in pourInput, attachments []pourAttachment) (int, error) {
	if len(attachments) == 0 {
		return 0, nil
	}
	spawnedMol, err := getStore().GetIssue(ctx, epicID)
	if err != nil {
		return 0, HandleError("loading spawned mol: %v", err)
	}
	totalAttached := 0
	for _, attach := range attachments {
		bondResult, err := bondProtoMol(ctx, getStore(), attach.issue, spawnedMol, newBondAttachmentOptions(in.attachType, vars, "", getActor(), false, true))
		if err != nil {
			return 0, HandleError("attaching %s: %v", attach.id, err)
		}
		totalAttached += bondResult.Spawned
	}
	return totalAttached, nil
}

func warnPourVaporFormula(protoArg string, varFlags []string) {
	fmt.Fprintf(os.Stderr, "%s Formula %q recommends vapor phase (ephemeral)\n", ui.RenderWarn("⚠"), protoArg)
	fmt.Fprintf(os.Stderr, "  Consider using: bd mol wisp %s", protoArg)
	for _, v := range varFlags {
		fmt.Fprintf(os.Stderr, " --var %s", v)
	}
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "  Pour creates persistent issues that sync like any other bead.\n")
	fmt.Fprintf(os.Stderr, "  Wisp creates ephemeral issues that auto-cleanup.\n\n")
}

func requiredVarsAcross(subgraph *TemplateSubgraph, attachSubgraphs []*TemplateSubgraph) []string {
	requiredVars := extractRequiredVariables(subgraph)
	for _, attachSubgraph := range attachSubgraphs {
		for _, v := range extractRequiredVariables(attachSubgraph) {
			found := false
			for _, rv := range requiredVars {
				if rv == v {
					found = true
					break
				}
			}
			if !found {
				requiredVars = append(requiredVars, v)
			}
		}
	}
	return requiredVars
}

func checkPourVars(subgraph *TemplateSubgraph, attachSubgraphs []*TemplateSubgraph, vars map[string]string) error {
	var missingVars []string
	for _, v := range requiredVarsAcross(subgraph, attachSubgraphs) {
		if _, ok := vars[v]; !ok {
			missingVars = append(missingVars, v)
		}
	}
	if len(missingVars) > 0 {
		return fmt.Errorf("missing required variables: %s", strings.Join(missingVars, ", "))
	}
	return nil
}

func missingVarHint(subgraph *TemplateSubgraph, attachSubgraphs []*TemplateSubgraph, vars map[string]string) string {
	for _, v := range requiredVarsAcross(subgraph, attachSubgraphs) {
		if _, ok := vars[v]; !ok {
			return v
		}
	}
	return ""
}

type pourAttachPreview struct {
	title string
	steps int
}

func renderPourDryRun(protoID string, subgraph *TemplateSubgraph, vars map[string]string, assignee, attachType string, attachments []pourAttachPreview) {
	fmt.Printf("\nDry run: would pour %d issues from proto %s\n\n", len(subgraph.Issues), protoID)
	fmt.Printf("Storage: permanent (.beads/)\n\n")
	for _, issue := range subgraph.Issues {
		newTitle := substituteVariables(issue.Title, vars)
		suffix := ""
		if issue.ID == subgraph.Root.ID && assignee != "" {
			suffix = fmt.Sprintf(" (assignee: %s)", assignee)
		}
		fmt.Printf("  - %s (from %s)%s\n", newTitle, issue.ID, suffix)
	}
	if len(attachments) > 0 {
		fmt.Printf("\nAttachments (%s bonding):\n", attachType)
		for _, attach := range attachments {
			fmt.Printf("  + %s (%d issues)\n", attach.title, attach.steps)
		}
	}
}

func renderPourResult(result *InstantiateResult, totalAttached, attachCount int) error {
	if isJSONOutput() {
		type pourResult struct {
			*InstantiateResult
			Attached int    `json:"attached"`
			Phase    string `json:"phase"`
		}
		return outputJSON(pourResult{result, totalAttached, "liquid"})
	}

	fmt.Printf("%s Poured mol: created %d issues\n", ui.RenderPass("✓"), result.Created)
	fmt.Printf("  Root issue: %s\n", result.NewEpicID)
	fmt.Printf("  Phase: liquid (persistent in the issue database)\n")
	if totalAttached > 0 {
		fmt.Printf("  Attached: %d issues from %d protos\n", totalAttached, attachCount)
	}
	return nil
}

func registerPourFlags(cmd *cobra.Command) {
	cmd.Flags().StringArray("var", []string{}, "Variable substitution (key=value)")
	cmd.Flags().Bool("dry-run", false, "Preview what would be created")
	cmd.Flags().String("assignee", "", "Assign the root issue to this agent/user")
	cmd.Flags().StringSlice("attach", []string{}, "Proto to attach after spawning (repeatable)")
	cmd.Flags().String("attach-type", types.BondTypeSequential, "Bond type for attachments: sequential, parallel, or conditional")
}

// pourTopLevelCmd is the `bd pour` alias. Cobra cannot attach the same command
// object to two parents, so this is a thin twin of pourCmd.
var pourTopLevelCmd = &cobra.Command{
	Use:           "pour <proto-id>",
	Short:         "Instantiate a proto as a persistent mol (alias for 'bd mol pour')",
	Long:          pourCmd.Long + "\n\nThis is an alias for `bd mol pour`.",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runPour,
}

func init() {
	registerPourFlags(pourCmd)
	registerPourFlags(pourTopLevelCmd)
	molCmd.AddCommand(pourCmd)
	rootCmd.AddCommand(pourTopLevelCmd)
}
