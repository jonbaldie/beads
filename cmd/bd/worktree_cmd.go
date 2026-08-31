package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/git"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/worktreeremove"
	"github.com/spf13/cobra"
)

// WorktreeInfo contains information about a git worktree
type WorktreeInfo struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	IsMain     bool   `json:"is_main"`
	BeadsState string `json:"beads_state"` // "redirect", "shared", "none"
	RedirectTo string `json:"redirect_to,omitempty"`
}

var worktreeCmd = &cobra.Command{
	Use:         "worktree",
	Short:       "Manage git worktrees for parallel development",
	GroupID:     "maint",
	Annotations: map[string]string{skipStoreAnnotation: "1"},
	Long: `Manage git worktrees with proper beads configuration.

Worktrees allow multiple working directories sharing the same git repository,
enabling parallel development (e.g., multiple agents or features).

Worktrees automatically share the same beads database as the main repository
via git common directory discovery — no manual redirect configuration needed.

Examples:
  bd worktree create feature-auth           # Create worktree
  bd worktree create bugfix --branch fix-1  # Create with specific branch name
  bd worktree list                          # List all worktrees
  bd worktree remove feature-auth           # Remove worktree (with safety checks)
  bd worktree info                          # Show info about current worktree`,
}

var worktreeCreateCmd = &cobra.Command{
	Use:   "create <name> [--branch=<branch>]",
	Short: "Create a worktree",
	Long: `Create a git worktree for parallel development.

This command:
1. Creates a git worktree at ./<name> (or specified path)
2. Adds the worktree path to .gitignore (if inside repo root)

The worktree automatically shares the same beads database as the main
repository via git common directory discovery — no redirect file needed.

Examples:
  bd worktree create feature-auth           # Create at ./feature-auth
  bd worktree create bugfix --branch fix-1  # Create with branch name
  bd worktree create ../agents/worker-1     # Create at relative path`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runWorktreeCreate,
}

var worktreeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all git worktrees",
	Long: `List all git worktrees and their beads configuration state.

Shows each worktree with:
- Name (directory name)
- Path (full path)
- Branch
- Beads state: "redirect" (uses shared db), "shared" (is main), "none" (no beads)

Examples:
  bd worktree list          # List all worktrees
  bd worktree list --json   # JSON output`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runWorktreeList,
}

var worktreeInfoCmd = &cobra.Command{
	Use:   "info",
	Short: "Show worktree info for current directory",
	Long: `Show information about the current worktree.

If the current directory is in a git worktree, shows:
- Worktree path and name
- Branch
- Beads configuration (redirect or main)
- Main repository location

Examples:
  bd worktree info          # Show current worktree info
  bd worktree info --json   # JSON output`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runWorktreeInfo,
}

func init() {
	worktreeCreateCmd.Flags().String("branch", "", "Branch name for the worktree (default: same as name)")

	worktreeCmd.AddCommand(worktreeCreateCmd)
	worktreeCmd.AddCommand(worktreeListCmd)
	worktreeCmd.AddCommand(worktreeRemoveCmd)
	worktreeCmd.AddCommand(worktreeInfoCmd)
	rootCmd.AddCommand(worktreeCmd)
}

// repairWorktreeBeadsPermissions applies FixBeadsDirPermissions to worktreePath/.beads when
// the directory exists. Git worktree checkout can leave tracked .beads/ at permissive modes.
func repairWorktreeBeadsPermissions(worktreePath string) {
	beadsDir := filepath.Join(worktreePath, ".beads")
	if fixed, err := config.FixBeadsDirPermissions(beadsDir); err != nil {
		if !isJSONOutput() {
			fmt.Fprintf(os.Stderr, "Warning: could not fix worktree .beads permissions: %v\n", err)
		}
	} else if fixed && !isJSONOutput() {
		fmt.Fprintf(os.Stderr, "Fixed .beads permissions to %04o\n", config.BeadsDirPerm)
	}
}

func runWorktreeCreate(cmd *cobra.Command, args []string) error {
	if err := CheckReadonly("worktree create"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("worktree-create")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	ctx := context.Background()
	name := args[0]
	worktreePath, err := resolveWorktreeCreatePath(name)
	if err != nil {
		return err
	}
	repoRoot, err := worktreeCreateRepoRoot()
	if err != nil {
		return err
	}
	branch := worktreeCreateBranch(cmd, name)
	if err := createWorktree(ctx, repoRoot, branch, worktreePath); err != nil {
		return err
	}

	if err := checkCreatedWorktreeClean(ctx, worktreePath); err != nil {
		return err
	}

	// Tracked .beads/ checked out by git worktree add can inherit umask defaults (0755).
	// Align with bd init / GH#3391 so agent loops do not hit permission warnings (GH#3593).
	repairWorktreeBeadsPermissions(worktreePath)
	addWorktreeToGitignore(ctx, repoRoot, worktreePath)
	return renderWorktreeCreateResult(worktreePath, branch)
}

func resolveWorktreeCreatePath(name string) (string, error) {
	worktreePath, err := filepath.Abs(name)
	if err != nil {
		return "", fmt.Errorf("failed to resolve path: %w", err)
	}
	if _, err := os.Stat(worktreePath); err == nil {
		return "", fmt.Errorf("path already exists: %s", worktreePath)
	}
	return worktreePath, nil
}

func worktreeCreateRepoRoot() (string, error) {
	rc, err := beads.GetRepoContext()
	if err != nil {
		return "", fmt.Errorf("%s; %s: %w", activeWorkspaceNotFoundError(), diagHint(), err)
	}
	if rc.CWDRepoRoot == "" {
		return "", fmt.Errorf("not in a git repository")
	}
	return rc.CWDRepoRoot, nil
}

func worktreeCreateBranch(cmd *cobra.Command, name string) string {
	if cmd != nil {
		if branch, err := cmd.Flags().GetString("branch"); err == nil && branch != "" {
			return branch
		}
	}
	return filepath.Base(name)
}

func createWorktree(ctx context.Context, repoRoot, branch, worktreePath string) error {
	gitCmd := gitCmdInDir(ctx, repoRoot, "worktree", "add", "-b", branch, worktreePath)
	output, err := gitCmd.CombinedOutput()
	if err == nil {
		return nil
	}

	// Try without -b if branch already exists.
	gitCmd = gitCmdInDir(ctx, repoRoot, "worktree", "add", worktreePath, branch)
	output, err = gitCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to create worktree: %w\n%s", err, string(output))
	}
	return nil
}

func addWorktreeToGitignore(ctx context.Context, repoRoot, worktreePath string) {
	if !strings.HasPrefix(worktreePath, repoRoot+string(os.PathSeparator)) {
		return
	}
	relWorktreePath, err := filepath.Rel(repoRoot, worktreePath)
	if err != nil {
		relWorktreePath = filepath.Base(worktreePath)
	}
	relWorktreePath = filepath.ToSlash(relWorktreePath)
	if err := addToGitignore(ctx, repoRoot, relWorktreePath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to update .gitignore: %v\n", err)
	}
}

func renderWorktreeCreateResult(worktreePath, branch string) error {
	if isJSONOutput() {
		result := map[string]interface{}{
			"path":   worktreePath,
			"branch": branch,
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}

	fmt.Printf("%s Created worktree: %s\n", ui.RenderPass("✓"), worktreePath)
	fmt.Printf("  Branch: %s\n", branch)
	return nil
}

func runWorktreeList(_ *cobra.Command, _ []string) error {
	evt := metrics.NewCommandEvent("worktree-list")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	ctx := context.Background()

	// Get repository context
	rc, err := beads.GetRepoContext()
	if err != nil {
		// Allow listing worktrees even without .beads (but no beads state info)
		// Fall back to git.GetRepoRoot() for this case
		repoRoot := git.GetRepoRoot()
		if repoRoot == "" {
			return fmt.Errorf("not in a git repository")
		}
		return listWorktreesWithoutBeads(ctx, repoRoot)
	}

	if rc.CWDRepoRoot == "" {
		return fmt.Errorf("not in a git repository")
	}
	worktrees, err := readWorktreeList(ctx, rc.CWDRepoRoot)
	if err != nil {
		return err
	}
	enrichWorktreeList(worktrees, rc.BeadsDir)
	return renderWorktreeList(worktrees)
}

func readWorktreeList(ctx context.Context, repoRoot string) ([]WorktreeInfo, error) {
	gitCmd := gitCmdInDir(ctx, repoRoot, "worktree", "list", "--porcelain")
	output, err := gitCmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list worktrees: %w", err)
	}
	return parseWorktreeList(string(output)), nil
}

func enrichWorktreeList(worktrees []WorktreeInfo, mainBeadsDir string) {
	for i := range worktrees {
		worktrees[i].BeadsState = getBeadsState(worktrees[i].Path, mainBeadsDir)
		if worktrees[i].BeadsState == "redirect" {
			worktrees[i].RedirectTo = getRedirectTarget(worktrees[i].Path)
		}
	}
}

func renderWorktreeList(worktrees []WorktreeInfo) error {
	if isJSONOutput() {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(worktrees)
	}
	if len(worktrees) == 0 {
		fmt.Println("No worktrees found")
		return nil
	}

	fmt.Printf("%-20s %-40s %-20s %s\n", "NAME", "PATH", "BRANCH", "BEADS")
	for _, wt := range worktrees {
		fmtWorktreeListRow(wt)
	}
	return nil
}

func fmtWorktreeListRow(worktree WorktreeInfo) {
	name := filepath.Base(worktree.Path)
	if worktree.IsMain {
		name = "(main)"
	}
	beadsInfo := worktree.BeadsState
	if worktree.RedirectTo != "" {
		beadsInfo = fmt.Sprintf("redirect → %s", filepath.Base(filepath.Dir(worktree.RedirectTo)))
	}
	fmt.Printf("%-20s %-40s %-20s %s\n",
		truncate(name, 20),
		truncate(worktree.Path, 40),
		truncate(worktree.Branch, 20),
		beadsInfo)
}

func runWorktreeRemove(
	cmd *cobra.Command,
	args []string,
	options *worktreeRemoveOptions,
	hooks worktreeRemoveHooks,
) error {
	if err := CheckReadonly("worktree remove"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("worktree-remove")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	mode := worktreeremove.Normal
	if options.force.value {
		mode = worktreeremove.Force
	}
	adapter := &gitWorktreeRemovalAdapter{name: args[0], options: options, hooks: hooks}
	return runWorktreeRemovalOrchestration(
		cmd.Context(),
		worktreeRemovalRequest{mode: mode},
		adapter,
		adapter,
		cliWorktreeRemovalPresenter{},
	)
}

type worktreeRemovalPartialError struct {
	path  string
	stage string
	err   error
}

func (err *worktreeRemovalPartialError) Error() string {
	return fmt.Sprintf(
		"worktree was removed at %s, but %s failed: %v; removal was not rolled back",
		err.path,
		err.stage,
		err.err,
	)
}

func (err *worktreeRemovalPartialError) Unwrap() error {
	return err.err
}

func runWorktreeInfo(_ *cobra.Command, _ []string) error {
	evt := metrics.NewCommandEvent("worktree-info")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	ctx := context.Background()
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	isWorktree := currentDirectoryIsWorktree()
	if !isWorktree {
		return renderNotWorktree()
	}

	// Get worktree info
	mainRepoRoot, err := git.GetMainRepoRoot()
	if err != nil {
		mainRepoRoot = "(unknown)"
	}

	branch := getWorktreeCurrentBranch(ctx, cwd)
	redirectInfo := beads.GetRedirectInfo()

	return renderWorktreeInfo(cwd, branch, mainRepoRoot, redirectInfo)
}

func currentDirectoryIsWorktree() bool {
	rc, err := beads.GetRepoContext()
	if err == nil {
		return rc.IsWorktree
	}
	return git.IsWorktree()
}

func renderNotWorktree() error {
	if isJSONOutput() {
		result := map[string]interface{}{"is_worktree": false}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}
	fmt.Println("Not in a git worktree (this is the main repository)")
	return nil
}

func renderWorktreeInfo(cwd, branch, mainRepoRoot string, redirectInfo beads.RedirectInfo) error {
	if isJSONOutput() {
		result := map[string]interface{}{
			"is_worktree":      true,
			"path":             cwd,
			"name":             filepath.Base(cwd),
			"branch":           branch,
			"main_repo":        mainRepoRoot,
			"beads_redirected": redirectInfo.IsRedirected,
		}
		if redirectInfo.IsRedirected {
			result["beads_local"] = redirectInfo.LocalDir
			result["beads_target"] = redirectInfo.TargetDir
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}

	fmt.Printf("Worktree: %s\n", cwd)
	fmt.Printf("  Name: %s\n", filepath.Base(cwd))
	fmt.Printf("  Branch: %s\n", branch)
	fmt.Printf("  Main repo: %s\n", mainRepoRoot)
	if redirectInfo.IsRedirected {
		fmt.Printf("  Beads: redirects to %s\n", redirectInfo.TargetDir)
	} else {
		fmt.Printf("  Beads: local (no redirect)\n")
	}
	return nil
}

// Helper functions

var checkCreatedWorktreeClean = ensureCreatedWorktreeClean
