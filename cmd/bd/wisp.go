package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/formula"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

// Wisp commands - manage ephemeral molecules
//
// Wisps are ephemeral issues with Ephemeral=true in the main database.
// They're used for patrol cycles and operational loops that shouldn't
// be synced via git.
//
// Commands:
//   bd mol wisp list    - List all wisps in current context
//   bd mol wisp gc      - Garbage collect orphaned wisps

var wispCmd = &cobra.Command{
	Use:   "wisp [proto-id]",
	Short: "Create or manage wisps (ephemeral molecules)",
	Long: `Create or manage wisps - EPHEMERAL molecules for operational workflows.

When called with a proto-id argument, creates a wisp from that proto.
When called with a subcommand (list, gc), manages existing wisps.

Wisps are issues with Ephemeral=true in the main database. They're stored
locally but NOT synced via git.

WHEN TO USE WISP vs POUR:
  wisp (vapor): Ephemeral work that auto-cleans up
    - Release workflows (one-time execution)
    - Operational loops and recurring cycles
    - Health checks and diagnostics
    - Any operational workflow without audit value

  pour (liquid): Persistent work that needs audit trail
    - Feature implementations spanning multiple sessions
    - Work you may need to reference later
    - Anything worth preserving in git history

TIP: Formulas can specify phase:"vapor" to recommend wisp usage.
     If you use pour on a vapor-phase formula, you'll get a warning.

The wisp lifecycle:
  1. Create: bd mol wisp <proto> or bd create --ephemeral
  2. Execute: Normal bd operations work on wisp issues
  3. Squash: bd mol squash <id> (clears Ephemeral flag, promotes to persistent)
  4. Or burn: bd mol burn <id> (deletes without creating digest)

Examples:
  bd mol wisp beads-release --var version=1.0  # Release workflow
  bd mol wisp mol-my-workflow                  # Ephemeral operational cycle
  bd mol wisp list                             # List all wisps
  bd mol wisp gc                               # Garbage collect old wisps

Subcommands:
  list  List all wisps in current context
  gc    Garbage collect orphaned wisps`,
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runWisp,
}

// WispListItem represents a wisp in list output
type WispListItem struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Priority  int       `json:"priority"`
	Type      string    `json:"type"`
	Labels    []string  `json:"labels,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Old       bool      `json:"old,omitempty"` // Not updated in 24+ hours
}

// WispListResult is the JSON output for wisp list
type WispListResult struct {
	Wisps    []WispListItem `json:"wisps"`
	Count    int            `json:"count"`
	OldCount int            `json:"old_count,omitempty"`
}

// OldThreshold is how old a wisp must be to be flagged as old (time-based, for ephemeral cleanup)
const OldThreshold = 24 * time.Hour

func runWisp(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("wisp")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	if len(args) == 0 {
		_ = cmd.Help()
		return nil
	}
	// Delegate to the non-emitting core so `bd wisp <name>` records exactly one
	// cli_command event ("wisp"), not also "wisp-create".
	return runWispCreateCore(cmd, args)
}

// wispCreateCmd instantiates a proto as an ephemeral wisp (kept for backwards compat)
var wispCreateCmd = &cobra.Command{
	Use:   "create <proto-id>",
	Short: "Instantiate a proto as a wisp (solid -> vapor)",
	Long: `Create a wisp from a proto - sublimation from solid to vapor.

This is the chemistry-inspired command for creating ephemeral work from templates.
The resulting wisp is stored in the main database with Ephemeral=true and NOT synced via git.

Phase transition: Proto (solid) -> Wisp (vapor)

Use wisp for:
  - Operational loops and recurring cycles
  - Health checks and monitoring
  - One-shot orchestration runs
  - Routine operations with no audit value

The wisp will:
  - Be stored in main database with Ephemeral=true flag
  - NOT be synced via git
  - Either evaporate (burn) or condense to digest (squash)

Examples:
  bd mol wisp create mol-patrol                    # Ephemeral patrol cycle
  bd mol wisp create mol-health-check              # One-time health check
  bd mol wisp create mol-diagnostics --var target=db  # Diagnostic run`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runWispCreate,
}

func runWispCreate(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("wisp-create")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	return runWispCreateCore(cmd, args)
}

// runWispCreateCore instantiates a proto as a wisp without emitting a metrics
// event, so the caller owns emission: the standalone `bd mol wisp create`
// entrypoint records "wisp-create", while the bare `bd wisp <name>` alias records
// "wisp". This keeps each invocation to exactly one cli_command event.
type wispCreateInput struct {
	protoArg string
	dryRun   bool
	rootOnly bool
	varFlags []string
}

func gatherWispCreateInput(cmd *cobra.Command, args []string) wispCreateInput {
	in := wispCreateInput{protoArg: args[0]}
	in.dryRun, _ = cmd.Flags().GetBool("dry-run")
	in.rootOnly, _ = cmd.Flags().GetBool("root-only")
	in.varFlags, _ = cmd.Flags().GetStringArray("var")
	return in
}

func runWispCreateCore(cmd *cobra.Command, args []string) error {
	if err := CheckReadonly("wisp create"); err != nil {
		return err
	}

	in := gatherWispCreateInput(cmd, args)

	if usesProxiedServer() {
		return runWispCreateProxiedServer(getRootContext(), in)
	}

	if getStore() == nil {
		return HandleErrorWithHint("no database connection", diagHint())
	}

	prepared, err := prepareWispCreate(getRootContext(), in)
	if err != nil {
		return err
	}

	if in.dryRun {
		renderWispCreateDryRun(prepared.protoID, prepared.subgraph, prepared.vars, in.rootOnly)
		return nil
	}

	result, err := spawnMoleculeWithOptions(getRootContext(), getStore(), prepared.subgraph, CloneOptions{
		Vars:      prepared.vars,
		Actor:     getActor(),
		Ephemeral: true,
		Prefix:    types.IDPrefixWisp,
		RootOnly:  in.rootOnly,
	})
	if err != nil {
		return HandleError("creating wisp: %v", err)
	}

	return renderWispCreateResult(result)
}

type preparedWispCreate struct {
	subgraph *TemplateSubgraph
	protoID  string
	vars     map[string]string
}

func prepareWispCreate(ctx context.Context, in wispCreateInput) (preparedWispCreate, error) {
	vars, err := parseVarFlags(in.varFlags)
	if err != nil {
		return preparedWispCreate{}, HandleError("%v", err)
	}

	subgraph, protoID, err := resolveWispCreateSource(in.protoArg, vars)
	if err != nil {
		return preparedWispCreate{}, err
	}
	if subgraph == nil {
		subgraph, protoID, err = loadLegacyWispSource(ctx, in.protoArg)
		if err != nil {
			return preparedWispCreate{}, err
		}
	}

	vars = applyVariableDefaults(vars, subgraph)
	if err := checkRequiredVars(subgraph, vars); err != nil {
		return preparedWispCreate{}, HandleErrorWithHint(err.Error(), fmt.Sprintf("Provide them with: --var %s=<value>", firstMissingVar(subgraph, vars)))
	}

	return preparedWispCreate{subgraph: subgraph, protoID: protoID, vars: vars}, nil
}

func resolveWispCreateSource(protoArg string, vars map[string]string) (*TemplateSubgraph, string, error) {
	// Try to cook formula inline (ephemeral protos). This works for any valid
	// formula name, not just "mol-" prefixed ones, and passes vars for step
	// condition filtering (bd-7zka.1).
	subgraph, err := resolveAndCookFormulaWithVars(protoArg, nil, vars)
	if err == nil {
		return subgraph, subgraph.Root.ID, nil
	}
	if errors.Is(err, formula.ErrVarValidation) {
		// protoArg IS a formula; report invalid vars directly instead of
		// masking the validation error as "not found as formula or proto".
		return nil, "", HandleError("%v", err)
	}
	return nil, "", nil
}

func loadLegacyWispSource(ctx context.Context, protoArg string) (*TemplateSubgraph, string, error) {
	protoID := protoArg
	if !looksLikeFullWispID(protoID) {
		if resolved, err := resolvePartialIDDirect(ctx, protoID); err == nil {
			protoID = resolved
		}
	}

	if strings.HasPrefix(protoID, "mol-") {
		resolved, err := resolveWispProtoTitle(ctx, protoID)
		if err != nil {
			return nil, "", err
		}
		protoID = resolved
	}

	protoIssue, err := getStore().GetIssue(ctx, protoID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, "", HandleError("proto not found: %s", protoID)
		}
		return nil, "", HandleError("loading proto %s: %v", protoID, err)
	}
	if !isProtoIssue(protoIssue) {
		return nil, "", HandleError("%s is not a proto (missing '%s' label)", protoID, MoleculeLabel)
	}

	subgraph, err := loadTemplateSubgraph(ctx, getStore(), protoID)
	if err != nil {
		return nil, "", HandleError("loading proto: %v", err)
	}
	return subgraph, protoID, nil
}

func looksLikeFullWispID(id string) bool {
	return strings.HasPrefix(id, "bd-") || strings.HasPrefix(id, "gt-") || strings.HasPrefix(id, "mol-")
}

func resolveWispProtoTitle(ctx context.Context, protoID string) (string, error) {
	issues, err := getStore().SearchIssues(ctx, "", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Labels: []string{MoleculeLabel}}})
	if err != nil {
		return "", HandleError("searching for proto: %v", err)
	}
	for _, issue := range issues {
		if strings.Contains(issue.Title, protoID) || issue.ID == protoID {
			return issue.ID, nil
		}
	}
	return "", HandleErrorWithHint(fmt.Sprintf("'%s' not found as formula or proto", protoID), "run 'bd formula list' to see available formulas")
}

func checkRequiredVars(subgraph *TemplateSubgraph, vars map[string]string) error {
	var missingVars []string
	for _, v := range extractRequiredVariables(subgraph) {
		if _, ok := vars[v]; !ok {
			missingVars = append(missingVars, v)
		}
	}
	if len(missingVars) > 0 {
		return fmt.Errorf("missing required variables: %s", strings.Join(missingVars, ", "))
	}
	return nil
}

func firstMissingVar(subgraph *TemplateSubgraph, vars map[string]string) string {
	for _, v := range extractRequiredVariables(subgraph) {
		if _, ok := vars[v]; !ok {
			return v
		}
	}
	return ""
}

func renderWispCreateDryRun(protoID string, subgraph *TemplateSubgraph, vars map[string]string, rootOnly bool) {
	if rootOnly {
		skipped := len(subgraph.Issues) - 1
		fmt.Printf("\nDry run: would create wisp with 1 issue (root only) from proto %s\n", protoID)
		if skipped > 0 {
			fmt.Printf("  Note: %d child step(s) skipped (--root-only)\n", skipped)
		}
	} else {
		fmt.Printf("\nDry run: would create wisp with %d issues from proto %s\n\n", len(subgraph.Issues), protoID)
	}
	fmt.Printf("Storage: main database (ephemeral=true, not synced via git)\n\n")
	issuesToShow := subgraph.Issues
	if rootOnly && len(issuesToShow) > 0 {
		issuesToShow = issuesToShow[:1]
	}
	for _, issue := range issuesToShow {
		newTitle := substituteVariables(issue.Title, vars)
		fmt.Printf("  - %s (from %s)\n", newTitle, issue.ID)
	}
}

func renderWispCreateResult(result *InstantiateResult) error {
	if isJSONOutput() {
		type wispCreateResult struct {
			*InstantiateResult
			Phase string `json:"phase"`
		}
		return outputJSON(wispCreateResult{result, "vapor"})
	}

	fmt.Printf("%s Created wisp: %d issues\n", ui.RenderPass("✓"), result.Created)
	fmt.Printf("  Root issue: %s\n", result.NewEpicID)
	fmt.Printf("  Phase: vapor (ephemeral, not synced via git)\n")
	fmt.Printf("\nNext steps:\n")
	fmt.Printf("  bd close %s.<step>       # Complete steps\n", result.NewEpicID)
	fmt.Printf("  bd mol squash %s         # Condense to digest (promotes to persistent)\n", result.NewEpicID)
	fmt.Printf("  bd mol burn %s           # Discard without creating digest\n", result.NewEpicID)
	return nil
}

// isProtoIssue checks if an issue is a proto (has the template label)
func isProtoIssue(issue *types.Issue) bool {
	for _, label := range issue.Labels {
		if label == MoleculeLabel {
			return true
		}
	}
	return false
}

// resolvePartialIDDirect resolves a partial ID directly from store
func resolvePartialIDDirect(ctx context.Context, partial string) (string, error) {
	// Try direct lookup first
	if issue, err := getStore().GetIssue(ctx, partial); err == nil {
		return issue.ID, nil
	}
	// Search by prefix
	issues, err := getStore().SearchIssues(ctx, "", types.IssueFilter{
		IssueFilterCore: types.IssueFilterCore{
			IDs: []string{partial + "*"},
		},
	})
	if err != nil {
		return "", err
	}
	if len(issues) == 1 {
		return issues[0].ID, nil
	}
	if len(issues) > 1 {
		return "", fmt.Errorf("ambiguous ID: %s matches %d issues", partial, len(issues))
	}
	return "", fmt.Errorf("not found: %s", partial)
}
