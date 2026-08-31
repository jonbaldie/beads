package main

import (
	"github.com/spf13/cobra"
)

// formulaCmd is the parent command for formula operations.
var formulaCmd = &cobra.Command{
	Use:   "formula",
	Short: "Manage workflow formulas",
	Long: `Manage workflow formulas - the source layer for molecule templates.

Formulas are TOML/JSON files that define workflows with composition rules.
Define formulas, cook them into protos, then pour or wisp them into work.

Search paths (in order):
  1. <resolved-beads-dir>/formulas/ (active project)
  2. <checkout-root>/.beads/formulas/ (repo-local formulas)
  3. ~/.beads/formulas/ (user)
  4. $GT_ROOT/.beads/formulas/ (shared workspace root, if GT_ROOT set)

Commands:
  list    List available formulas from all search paths
  show    Show formula details, steps, and composition rules
  schema  Show the formula schema index (alias: primitives)

Discovering primitives:
  bd formula schema                 # list every declared formula struct
  bd formula schema loop            # show LoopSpec fields, types, and tags
  bd formula primitives gate        # alias; same handler as 'schema'
  examples/formulas/primitives/     # curated, smoke-tested wired fixtures
  docs/workflows/formulas.md          # narrative reference`,
}

// formulaListCmd lists all available formulas.
var formulaListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available formulas",
	Long: `List all formulas from search paths.

Search paths (in order of priority):
  1. <resolved-beads-dir>/formulas/ (active project - highest priority)
  2. <checkout-root>/.beads/formulas/ (repo-local formulas)
  3. ~/.beads/formulas/ (user)
  4. $GT_ROOT/.beads/formulas/ (shared workspace root, if GT_ROOT set)

Formulas in earlier paths shadow those with the same name in later paths.

To list the declared formula schema structs an agent can write inside a .formula.toml,
use 'bd formula schema' (alias: 'bd formula primitives').

Examples:
  bd formula list
  bd formula list --json
  bd formula list --type workflow
  bd formula list --type convoy`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runFormulaList,
}

// formulaShowCmd shows details of a specific formula.
var formulaShowCmd = &cobra.Command{
	Use:   "show <formula-name>",
	Short: "Show formula details",
	Long: `Show detailed information about a formula.

Displays:
  - Formula metadata (name, type, description)
  - Variables with defaults and constraints
  - Steps with dependencies
  - Composition rules (extends, aspects, expansions)
  - Bond points for external composition

To inspect the structure of an individual primitive (e.g. LoopSpec, Gate)
rather than a user-authored formula, use 'bd formula schema <primitive>'.

Examples:
  bd formula show shiny
  bd formula show rule-of-five
  bd formula show security-audit --json`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runFormulaShow,
}

// FormulaListEntry represents a formula in the list output.
type FormulaListEntry struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Steps       int    `json:"steps"`
	Vars        int    `json:"vars"`
}
