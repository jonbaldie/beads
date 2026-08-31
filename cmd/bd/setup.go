package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jonbaldie/beads/cmd/bd/setup"
	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/recipes"
	"github.com/spf13/cobra"
)

type setupOptions struct {
	project bool
	global  bool
	check   bool
	remove  bool
	stealth bool
	print   bool
	output  string
	list    bool
	add     string
}

var setupCmd = &cobra.Command{
	Use:     "setup [recipe]",
	GroupID: "setup",
	Short:   "Setup integration with AI editors",
	Long: `Setup integration files for AI editors and coding assistants.

Recipes define where beads workflow instructions are written. Built-in recipes
include cursor, claude, copilot, gemini, aider, factory, codex, mux, opencode, junie, kiro, windsurf, cody, and kilocode.

Examples:
  bd setup cursor          # Install Cursor IDE integration (rules + agent hooks)
  bd setup cursor --global # Install global Cursor hooks (~/.cursor/hooks.json)
  bd setup kiro            # Install Kiro steering guidance
  bd setup codex           # Install Codex skill + AGENTS.md guidance + native hooks
  bd setup codex --global  # Install global Codex skill + guidance + native hooks
  bd setup copilot         # Install Copilot CLI plugin + repository instructions
  bd setup mux --project   # Install Mux workspace layer (.mux/AGENTS.md)
  bd setup mux --global    # Install Mux global layer (~/.mux/AGENTS.md)
  bd setup mux --project --global  # Install both Mux layers
  bd setup --list          # Show all available recipes
  bd setup --print         # Print the template to stdout
  bd setup -o rules.md     # Write template to custom path
  bd setup --add myeditor .myeditor/rules.md  # Add custom recipe

Use 'bd setup <recipe> --check' to verify installation status.
Use 'bd setup <recipe> --remove' to uninstall.`,
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runSetup,
}

func runSetup(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("setup")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	return dispatchSetup(cmd, args, setupOptionsFromCommand(cmd))
}

func setupOptionsFromCommand(cmd *cobra.Command) setupOptions {
	return setupOptions{
		project: setupFlagBool(cmd, "project"),
		global:  setupFlagBool(cmd, "global"),
		check:   setupFlagBool(cmd, "check"),
		remove:  setupFlagBool(cmd, "remove"),
		stealth: setupFlagBool(cmd, "stealth"),
		print:   setupFlagBool(cmd, "print"),
		output:  setupFlagString(cmd, "output"),
		list:    setupFlagBool(cmd, "list"),
		add:     setupFlagString(cmd, "add"),
	}
}

func setupFlagBool(cmd *cobra.Command, name string) bool {
	value, _ := cmd.Flags().GetBool(name)
	return value
}

func setupFlagString(cmd *cobra.Command, name string) string {
	value, _ := cmd.Flags().GetString(name)
	return value
}

func dispatchSetup(cmd *cobra.Command, args []string, opts setupOptions) error {
	if opts.list {
		return listRecipes()
	}

	if opts.print {
		fmt.Print(recipes.Template)
		return nil
	}

	if opts.output != "" {
		if err := writeToPath(opts.output); err != nil {
			return HandleError("%v", err)
		}
		fmt.Printf("✓ Wrote template to %s\n", opts.output)
		return nil
	}

	if opts.add != "" {
		if len(args) != 1 {
			return HandleErrorWithHint("--add requires a path argument", "Usage: bd setup --add <name> <path>")
		}
		if err := addRecipe(opts.add, args[0]); err != nil {
			return HandleError("%v", err)
		}
		return nil
	}

	if len(args) == 0 {
		_ = cmd.Help()
		return nil
	}

	recipeName := strings.ToLower(args[0])
	return runRecipe(recipeName, opts)
}

func setupWorkspaceError() error {
	return fmt.Errorf("%s; %s", activeWorkspaceNotFoundError(), diagHint())
}

func builtinSetupRecipes() map[string]recipes.Recipe {
	allRecipes := make(map[string]recipes.Recipe, len(recipes.BuiltinRecipes))
	for name, recipe := range recipes.BuiltinRecipes {
		allRecipes[name] = recipe
	}
	return allRecipes
}

func loadSetupRecipes() (map[string]recipes.Recipe, bool, error) {
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return builtinSetupRecipes(), false, nil
	}

	allRecipes, err := recipes.GetAllRecipes(beadsDir)
	if err != nil {
		return nil, false, err
	}
	return allRecipes, true, nil
}

func lookupSetupRecipe(name string) (*recipes.Recipe, error) {
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		normalized := strings.ToLower(strings.Trim(name, "-"))
		recipe, ok := recipes.BuiltinRecipes[normalized]
		if !ok {
			return nil, fmt.Errorf("unknown recipe: %s (workspace-local custom recipes require an active beads workspace)", normalized)
		}
		resolved := recipe
		return &resolved, nil
	}

	return recipes.GetRecipe(name, beadsDir)
}

func listRecipes() error {
	allRecipes, usingWorkspaceRecipes, err := loadSetupRecipes()
	if err != nil {
		return HandleError("loading recipes: %v", err)
	}

	names := make([]string, 0, len(allRecipes))
	for name := range allRecipes {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Println("Available recipes:")
	fmt.Println()
	for _, name := range names {
		r := allRecipes[name]
		source := "built-in"
		if !recipes.IsBuiltin(name) {
			source = "user"
		}
		fmt.Printf("  %-12s  %-25s  (%s)\n", name, r.Description, source)
	}
	fmt.Println()
	if !usingWorkspaceRecipes {
		fmt.Printf("Note: %s Showing built-in recipes only.\n", activeWorkspaceNotFoundMessage())
		fmt.Printf("Hint: %s\n", diagHint())
		fmt.Println()
	}
	fmt.Println("Use 'bd setup <recipe>' to install.")
	fmt.Println("Use 'bd setup --add <name> <path>' to add a custom recipe.")
	return nil
}

func writeToPath(path string) error {
	// Ensure parent directory exists
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory: %w", err)
		}
	}

	if err := os.WriteFile(path, []byte(recipes.Template), 0o644); err != nil { // #nosec G306 -- config files need to be readable
		return fmt.Errorf("write file: %w", err)
	}
	return nil
}

func addRecipe(name, path string) error {
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return setupWorkspaceError()
	}

	if err := recipes.SaveUserRecipe(beadsDir, name, path); err != nil {
		return err
	}

	fmt.Printf("✓ Added recipe '%s' → %s\n", name, path)
	fmt.Printf("  Config: %s/recipes.toml\n", beadsDir)
	fmt.Println()
	fmt.Printf("Install with: bd setup %s\n", name)
	return nil
}

func runRecipe(name string, opts setupOptions) error {
	if handled, err := runSpecialSetupRecipe(name, opts); handled {
		return err
	}

	recipe, err := lookupSetupRecipe(name)
	if err != nil {
		return HandleErrorWithHint(fmt.Sprintf("%v", err), "Use 'bd setup --list' to see available recipes.")
	}

	if recipe.Type != recipes.TypeFile && recipe.Type != recipes.TypeMultiFile {
		return HandleError("recipe '%s' has type '%s' which requires special handling", name, recipe.Type)
	}
	return runFileSetupRecipe(name, *recipe, opts)
}

func runSpecialSetupRecipe(name string, opts setupOptions) (bool, error) {
	handlers := map[string]func(setupOptions) error{
		"claude":   runClaudeRecipe,
		"gemini":   runGeminiRecipe,
		"factory":  runFactoryRecipe,
		"codex":    runCodexRecipe,
		"mux":      runMuxRecipe,
		"opencode": runOpenCodeRecipe,
		"aider":    runAiderRecipe,
		"cursor":   runCursorRecipe,
		"junie":    runJunieRecipe,
	}
	handler, ok := handlers[name]
	if !ok {
		return false, nil
	}
	return true, handler(opts)
}

func runFileSetupRecipe(name string, recipe recipes.Recipe, opts setupOptions) error {
	paths := recipe.Paths
	if recipe.Type == recipes.TypeFile {
		paths = []string{recipe.Path}
	}

	switch {
	case opts.check:
		return checkFileSetupRecipe(name, recipe.Name, paths)
	case opts.remove:
		return removeFileSetupRecipe(recipe.Name, paths)
	default:
		return installFileSetupRecipe(recipe.Name, recipe, paths)
	}
}

func checkFileSetupRecipe(name, recipeName string, paths []string) error {
	missing := missingSetupPaths(paths)
	if len(missing) > 0 {
		fmt.Printf("✗ %s integration not installed\n", recipeName)
		fmt.Printf("  Run: bd setup %s\n", name)
		for _, path := range missing {
			fmt.Printf("  Missing: %s\n", path)
		}
		return SilentExit()
	}
	fmt.Printf("✓ %s integration installed\n", recipeName)
	for _, path := range paths {
		fmt.Printf("  File: %s\n", path)
	}
	return nil
}

func missingSetupPaths(paths []string) []string {
	var missing []string
	for _, path := range paths {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			missing = append(missing, path)
		}
	}
	return missing
}

func removeFileSetupRecipe(recipeName string, paths []string) error {
	removed := false
	for _, path := range paths {
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return HandleError("%v", err)
		}
		removed = true
		_ = os.Remove(filepath.Dir(path))
	}
	if !removed {
		fmt.Println("No integration files found")
		return nil
	}
	fmt.Printf("✓ Removed %s integration\n", recipeName)
	return nil
}

func installFileSetupRecipe(recipeName string, recipe recipes.Recipe, paths []string) error {
	fmt.Printf("Installing %s integration...\n", recipeName)
	for _, path := range paths {
		dir := filepath.Dir(path)
		if dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return HandleError("create directory: %v", err)
			}
		}

		content, err := recipes.ContentForPath(recipe, path)
		if err != nil {
			return HandleError("%v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil { // #nosec G306 -- config files need to be readable
			return HandleError("write file: %v", err)
		}
	}

	fmt.Printf("\n✓ %s integration installed\n", recipeName)
	for _, path := range paths {
		fmt.Printf("  File: %s\n", path)
	}
	return nil
}

func translateSetupError(err error) error {
	if err == nil {
		return nil
	}
	return SilentExit()
}

func runCursorRecipe(opts setupOptions) error {
	switch {
	case opts.check:
		return translateSetupError(setup.CheckCursor(opts.global))
	case opts.remove:
		return translateSetupError(setup.RemoveCursor(opts.global))
	default:
		return translateSetupError(setup.InstallCursor(opts.global))
	}
}

func runClaudeRecipe(opts setupOptions) error {
	switch {
	case opts.check:
		return translateSetupError(setup.CheckClaude())
	case opts.remove:
		return translateSetupError(setup.RemoveClaude(opts.global))
	default:
		return translateSetupError(setup.InstallClaude(opts.global, opts.stealth))
	}
}

func runGeminiRecipe(opts setupOptions) error {
	switch {
	case opts.check:
		return translateSetupError(setup.CheckGemini())
	case opts.remove:
		return translateSetupError(setup.RemoveGemini(opts.project))
	default:
		return translateSetupError(setup.InstallGemini(opts.project, opts.stealth))
	}
}

func runFactoryRecipe(opts setupOptions) error {
	switch {
	case opts.check:
		return translateSetupError(setup.CheckFactory())
	case opts.remove:
		return translateSetupError(setup.RemoveFactory())
	default:
		return translateSetupError(setup.InstallFactory())
	}
}

func runCodexRecipe(opts setupOptions) error {
	switch {
	case opts.check:
		return translateSetupError(setup.CheckCodex(opts.global))
	case opts.remove:
		return translateSetupError(setup.RemoveCodex(opts.global))
	default:
		return translateSetupError(setup.InstallCodex(opts.global))
	}
}

func runOpenCodeRecipe(opts setupOptions) error {
	switch {
	case opts.check:
		return translateSetupError(setup.CheckOpenCode())
	case opts.remove:
		return translateSetupError(setup.RemoveOpenCode())
	default:
		return translateSetupError(setup.InstallOpenCode())
	}
}

func runMuxRecipe(opts setupOptions) error {
	switch {
	case opts.check:
		return translateSetupError(setup.CheckMux(opts.project, opts.global))
	case opts.remove:
		return translateSetupError(setup.RemoveMux(opts.project, opts.global))
	default:
		return translateSetupError(setup.InstallMux(opts.project, opts.global))
	}
}

func runAiderRecipe(opts setupOptions) error {
	switch {
	case opts.check:
		return translateSetupError(setup.CheckAider())
	case opts.remove:
		return translateSetupError(setup.RemoveAider())
	default:
		return translateSetupError(setup.InstallAider())
	}
}

func runJunieRecipe(opts setupOptions) error {
	switch {
	case opts.check:
		return translateSetupError(setup.CheckJunie())
	case opts.remove:
		return translateSetupError(setup.RemoveJunie())
	default:
		return translateSetupError(setup.InstallJunie())
	}
}

func init() {
	// Global flags for the setup command
	setupCmd.Flags().Bool("list", false, "List all available recipes")
	setupCmd.Flags().Bool("print", false, "Print the template to stdout")
	setupCmd.Flags().StringP("output", "o", "", "Write template to custom path")
	setupCmd.Flags().String("add", "", "Add a custom recipe with given name")

	// Per-recipe flags
	setupCmd.Flags().Bool("check", false, "Check if integration is installed")
	setupCmd.Flags().Bool("remove", false, "Remove the integration")
	setupCmd.Flags().Bool("project", false, "Install for this project only (gemini/mux)")
	setupCmd.Flags().Bool("global", false, "Install globally (claude/codex/cursor/mux; writes to ~/.claude/settings.json, $CODEX_HOME/AGENTS.md or ~/.codex/AGENTS.md, ~/.cursor/hooks.json, or ~/.mux/AGENTS.md)")
	setupCmd.Flags().Bool("stealth", false, "Use stealth mode (claude/gemini)")

	rootCmd.AddCommand(setupCmd)
}
