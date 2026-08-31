package main

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

var molBondCmd = &cobra.Command{
	Use:     "bond <A> <B>",
	Aliases: []string{"fart"}, // Easter egg: molecules can produce gas
	Short:   "Bond two protos or molecules together",
	Long: `Bond two protos or molecules to create a compound.

The bond command is polymorphic - it handles different operand types:

  formula + formula → cook both, compound proto
  formula + proto   → cook formula, compound proto
  formula + mol     → cook formula, spawn and attach
  proto + proto     → compound proto (reusable template)
  proto + mol       → spawn proto, attach to molecule
  mol + proto       → spawn proto, attach to molecule
  mol + mol         → join into compound molecule

Formula names (e.g., mol-polecat-arm) are cooked inline as ephemeral protos.
This avoids needing pre-cooked proto beads in the database.

Bond types:
  sequential (default) - B runs after A completes
  parallel            - B runs alongside A
  conditional         - B runs only if A fails

Phase control:
  By default, spawned protos follow the target's phase:
  - Attaching to mol (Ephemeral=false) → spawns as persistent (Ephemeral=false)
  - Attaching to ephemeral issue (Ephemeral=true) → spawns as ephemeral (Ephemeral=true)

  Override with:
  --pour  Force spawn as liquid (persistent, Ephemeral=false)
  --ephemeral  Force spawn as vapor (ephemeral, Ephemeral=true, excluded from Dolt sync via dolt_ignore)

Dynamic bonding (Christmas Ornament pattern):
  Use --ref to specify a custom child reference with variable substitution.
  This creates IDs like "parent.child-ref" instead of random hashes.

  Example:
    bd mol bond mol-worker-arm bd-patrol --ref arm-{{worker_name}} --var worker_name=ace
    # Creates: bd-patrol.arm-ace (and children like bd-patrol.arm-ace.capture)

Use cases:
  - Found important bug during patrol? Use --pour to persist it
  - Need ephemeral diagnostic on persistent feature? Use --ephemeral
  - Spawning per-worker arms on a patrol? Use --ref for readable IDs

Examples:
  bd mol bond mol-feature mol-deploy                    # Compound proto
  bd mol bond mol-feature mol-deploy --type parallel    # Run in parallel
  bd mol bond mol-feature bd-abc123                     # Attach proto to molecule
  bd mol bond bd-abc123 bd-def456                       # Join two molecules
  bd mol bond mol-critical-bug wisp-patrol --pour       # Persist found bug
  bd mol bond mol-temp-check bd-feature --ephemeral          # Ephemeral diagnostic
  bd mol bond mol-arm bd-patrol --ref arm-{{name}} --var name=ace  # Dynamic child ID`,
	Args:          cobra.ExactArgs(2),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runMolBond,
}

// BondResult holds the result of a bond operation
type BondResult struct {
	ResultID   string            `json:"result_id"`
	ResultType string            `json:"result_type"` // "compound_proto" or "compound_molecule"
	BondType   string            `json:"bond_type"`
	Spawned    int               `json:"spawned,omitempty"`    // Number of issues spawned (if proto was involved)
	IDMapping  map[string]string `json:"id_mapping,omitempty"` // Old ID -> new ID for spawned issues
}

func runMolBond(cmd *cobra.Command, args []string) error {
	if err := CheckReadonly("mol bond"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("mol-bond")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	in, err := gatherMolBondInput(cmd, args)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	if usesProxiedServer() {
		return runMolBondProxiedServer(getRootContext(), in)
	}

	return runMolBondLocal(getRootContext(), in)
}

func runMolBondLocal(ctx context.Context, in molBondInput) error {

	if getStore() == nil {
		return HandleErrorRespectJSON("no database connection")
	}

	if in.dryRun {
		return runMolBondDryRun(ctx, in)
	}

	operands, err := resolveMolBondOperands(ctx, getStore(), in)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	result, err := bondMolOperands(ctx, getStore(), operands, in)
	if err != nil {
		return HandleErrorRespectJSON("bonding: %v", err)
	}

	return renderMolBondResult(result, operands.issueA.ID, operands.issueB.ID, in.ephemeral, in.pour)
}

func runMolBondDryRun(ctx context.Context, in molBondInput) error {
	issueA, formulaA, err := resolveOrDescribe(ctx, getStore(), in.argA, in.vars)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	issueB, formulaB, err := resolveOrDescribe(ctx, getStore(), in.argB, in.vars)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	renderMolBondDryRun(in, issueA, formulaA, issueB, formulaB)
	return nil
}

type molBondOperands struct {
	issueA    *types.Issue
	issueB    *types.Issue
	subgraphA *TemplateSubgraph
	subgraphB *TemplateSubgraph
	cookedA   bool
	cookedB   bool
}

func resolveMolBondOperands(ctx context.Context, s storage.DoltStorage, in molBondInput) (molBondOperands, error) {
	subgraphA, cookedA, err := resolveOrCookToSubgraph(ctx, s, in.argA, in.vars)
	if err != nil {
		return molBondOperands{}, err
	}
	subgraphB, cookedB, err := resolveOrCookToSubgraph(ctx, s, in.argB, in.vars)
	if err != nil {
		return molBondOperands{}, err
	}
	return molBondOperands{
		issueA:    subgraphA.Root,
		issueB:    subgraphB.Root,
		subgraphA: subgraphA,
		subgraphB: subgraphB,
		cookedA:   cookedA,
		cookedB:   cookedB,
	}, nil
}

func bondMolOperands(ctx context.Context, s storage.DoltStorage, operands molBondOperands, in molBondInput) (*BondResult, error) {
	aIsProto := operands.issueA.IsTemplate || operands.cookedA
	bIsProto := operands.issueB.IsTemplate || operands.cookedB
	attachment := in.bondAttachmentOptions(getActor())
	switch {
	case aIsProto && bIsProto:
		return bondProtoProto(ctx, s, operands.issueA, operands.issueB, in.bondType, in.customTitle, getActor())
	case aIsProto && !bIsProto:
		return bondProtoMolOperand(ctx, s, operands.subgraphA, operands.issueA, operands.issueB, operands.cookedA, attachment)
	case !aIsProto && bIsProto:
		return bondProtoMolOperand(ctx, s, operands.subgraphB, operands.issueB, operands.issueA, operands.cookedB, attachment)
	default:
		return bondMolMol(ctx, s, operands.issueA, operands.issueB, in.bondType, getActor())
	}
}

func bondProtoMolOperand(ctx context.Context, s storage.DoltStorage, subgraph *TemplateSubgraph, proto, mol *types.Issue, cooked bool, options bondAttachmentOptions) (*BondResult, error) {
	if cooked {
		return bondProtoMolWithSubgraph(ctx, s, subgraph, proto, mol, options)
	}
	return bondProtoMol(ctx, s, proto, mol, options)
}

type molBondInput struct {
	argA        string
	argB        string
	bondType    string
	customTitle string
	dryRun      bool
	vars        map[string]string
	ephemeral   bool
	pour        bool
	childRef    string
}

type bondAttachmentOptions struct {
	bondType      string
	vars          map[string]string
	childRef      string
	actorName     string
	ephemeralFlag bool
	pourFlag      bool
}

func (in molBondInput) bondAttachmentOptions(actorName string) bondAttachmentOptions {
	return bondAttachmentOptions{
		bondType:      in.bondType,
		vars:          in.vars,
		childRef:      in.childRef,
		actorName:     actorName,
		ephemeralFlag: in.ephemeral,
		pourFlag:      in.pour,
	}
}

func newBondAttachmentOptions(bondType string, vars map[string]string, childRef, actorName string, ephemeralFlag, pourFlag bool) bondAttachmentOptions {
	return bondAttachmentOptions{
		bondType:      bondType,
		vars:          vars,
		childRef:      childRef,
		actorName:     actorName,
		ephemeralFlag: ephemeralFlag,
		pourFlag:      pourFlag,
	}
}

func gatherMolBondInput(cmd *cobra.Command, args []string) (molBondInput, error) {
	in := molBondInput{argA: args[0], argB: args[1]}
	in.bondType, _ = cmd.Flags().GetString("type")
	in.customTitle, _ = cmd.Flags().GetString("as")
	in.dryRun, _ = cmd.Flags().GetBool("dry-run")
	in.ephemeral, _ = cmd.Flags().GetBool("ephemeral")
	in.pour, _ = cmd.Flags().GetBool("pour")
	in.childRef, _ = cmd.Flags().GetString("ref")

	if in.ephemeral && in.pour {
		return in, fmt.Errorf("cannot use both --ephemeral and --pour")
	}
	if in.bondType != types.BondTypeSequential && in.bondType != types.BondTypeParallel && in.bondType != types.BondTypeConditional {
		return in, fmt.Errorf("invalid bond type '%s', must be: sequential, parallel, or conditional", in.bondType)
	}

	varFlags, _ := cmd.Flags().GetStringArray("var")
	vars, err := parseVarFlags(varFlags)
	if err != nil {
		return in, err
	}
	in.vars = vars
	return in, nil
}

func renderMolBondDryRun(in molBondInput, issueA *types.Issue, formulaA string, issueB *types.Issue, formulaB string) {
	operandA := newMolBondDryRunOperand(in.argA, issueA, formulaA)
	operandB := newMolBondDryRunOperand(in.argB, issueB, formulaB)

	fmt.Printf("\nDry run: bond %s + %s\n", operandA.id, operandB.id)
	renderMolBondDryRunOperand("A", operandA)
	renderMolBondDryRunOperand("B", operandB)
	fmt.Printf("  Bond type: %s\n", in.bondType)
	renderMolBondDryRunPhase(in)
	renderMolBondDryRunChildRef(in)
	renderMolBondDryRunResult(in, operandA.isProto, operandB.isProto)
	if operandA.formula != "" || operandB.formula != "" {
		fmt.Printf("\n  Note: Cooked formulas are ephemeral and deleted after bonding.\n")
	}
}

type molBondDryRunOperand struct {
	id      string
	issue   *types.Issue
	formula string
	isProto bool
}

func newMolBondDryRunOperand(id string, issue *types.Issue, formula string) molBondDryRunOperand {
	operand := molBondDryRunOperand{id: id, issue: issue, formula: formula}
	if issue != nil {
		operand.id = issue.ID
		operand.isProto = isProto(issue)
	}
	if formula != "" {
		operand.isProto = true
	}
	return operand
}

func renderMolBondDryRunOperand(label string, operand molBondDryRunOperand) {
	if operand.formula != "" {
		fmt.Printf("  %s: %s (formula → will cook as proto)\n", label, operand.formula)
	} else if operand.issue != nil {
		fmt.Printf("  %s: %s (%s)\n", label, operand.issue.Title, operandType(operand.isProto))
	}
}

func renderMolBondDryRunPhase(in molBondInput) {
	if in.ephemeral {
		fmt.Printf("  Phase override: vapor (--ephemeral)\n")
	} else if in.pour {
		fmt.Printf("  Phase override: liquid (--pour)\n")
	}
}

func renderMolBondDryRunChildRef(in molBondInput) {
	if in.childRef == "" {
		return
	}
	resolvedRef := substituteVariables(in.childRef, in.vars)
	fmt.Printf("  Child ref: %s (resolved: %s)\n", in.childRef, resolvedRef)
}

func renderMolBondDryRunResult(in molBondInput, aIsProto, bIsProto bool) {
	switch {
	case aIsProto && bIsProto:
		fmt.Printf("  Result: compound proto\n")
		if in.customTitle != "" {
			fmt.Printf("  Custom title: %s\n", in.customTitle)
		}
	case aIsProto || bIsProto:
		fmt.Printf("  Result: spawn proto, attach to molecule\n")
	default:
		fmt.Printf("  Result: compound molecule\n")
	}
}

func renderMolBondResult(result *BondResult, idA, idB string, ephemeral, pour bool) error {
	if isJSONOutput() {
		return outputJSON(result)
	}

	fmt.Printf("%s Bonded: %s + %s\n", ui.RenderPass("✓"), idA, idB)
	fmt.Printf("  Result: %s (%s)\n", result.ResultID, result.ResultType)
	if result.Spawned > 0 {
		fmt.Printf("  Spawned: %d issues\n", result.Spawned)
	}
	if ephemeral {
		fmt.Printf("  Phase: vapor (ephemeral, Ephemeral=true)\n")
	} else if pour {
		fmt.Printf("  Phase: liquid (persistent, Ephemeral=false)\n")
	}
	return nil
}

// isProto checks if an issue is a proto (has the template label)
func isProto(issue *types.Issue) bool {
	for _, label := range issue.Labels {
		if label == MoleculeLabel {
			return true
		}
	}
	return false
}

// operandType returns a human-readable type string
func operandType(isProtoIssue bool) string {
	if isProtoIssue {
		return "proto"
	}
	return "molecule"
}
