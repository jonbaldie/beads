package main

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

type duplicateOptions struct {
	canonicalID string
}

func duplicateOptionsFromCommand(cmd *cobra.Command) duplicateOptions {
	if cmd == nil {
		return duplicateOptions{}
	}
	canonicalID, _ := cmd.Flags().GetString("of")
	return duplicateOptions{canonicalID: canonicalID}
}

func newDuplicateCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "duplicate <id> --of <canonical>",
		GroupID: "deps",
		Short:   "Mark an issue as a duplicate of another",
		Long: `Mark an issue as a duplicate of a canonical issue.

The duplicate issue is automatically closed with a reference to the canonical.
This is essential for large issue databases with many similar reports.

Examples:
  bd duplicate bd-abc --of bd-xyz    # Mark bd-abc as duplicate of bd-xyz`,
		Args: cobra.ExactArgs(1),
		RunE: runDuplicate,
	}
}

func newSupersedeCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "supersede <id> --with <new>",
		GroupID: "deps",
		Short:   "Mark an issue as superseded by a newer one",
		Long: `Mark an issue as superseded by a newer version.

The superseded issue is automatically closed with a reference to the replacement.
Useful for design docs, specs, and evolving artifacts.

Examples:
  bd supersede bd-old --with bd-new    # Mark bd-old as superseded by bd-new`,
		Args: cobra.ExactArgs(1),
		RunE: runSupersede,
	}
}

func init() {
	duplicateCmd := newDuplicateCommand()
	duplicateCmd.Flags().String("of", "", "Canonical issue ID (required)")
	_ = duplicateCmd.MarkFlagRequired("of") // Only fails if flag missing (caught in tests)
	duplicateCmd.ValidArgsFunction = issueIDCompletion
	rootCmd.AddCommand(duplicateCmd)

	supersedeCmd := newSupersedeCommand()
	supersedeCmd.Flags().String("with", "", "Replacement issue ID (required)")
	_ = supersedeCmd.MarkFlagRequired("with") // Only fails if flag missing (caught in tests)
	supersedeCmd.ValidArgsFunction = issueIDCompletion
	rootCmd.AddCommand(supersedeCmd)
}

func runDuplicate(cmd *cobra.Command, args []string) error {
	return runDuplicateWithOptions(args, duplicateOptionsFromCommand(cmd))
}

func runDuplicateWithOptions(args []string, opts duplicateOptions) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("duplicate is not supported in proxied-server mode")
	}
	if err := CheckReadonly("duplicate"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("duplicate")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	ctx := getRootContext()
	store := getStore()
	actor := getActor()

	// Resolve partial IDs
	duplicateID, canonicalID, err := resolveDuplicatePair(ctx, store, args[0], opts.canonicalID)
	if err != nil {
		return err
	}
	return executeDuplicate(ctx, store, actor, duplicateID, canonicalID)
}

func executeDuplicate(ctx context.Context, store interface {
	GetIssue(context.Context, string) (*types.Issue, error)
	AddDependency(context.Context, *types.Dependency, string) error
	CloseIssue(context.Context, string, string, string, string) error
}, actor, duplicateID, canonicalID string) error {
	if duplicateID == canonicalID {
		return fmt.Errorf("cannot mark an issue as duplicate of itself")
	}
	// Verify canonical issue exists
	var canonical *types.Issue
	canonical, err := store.GetIssue(ctx, canonicalID)
	if err != nil || canonical == nil {
		return fmt.Errorf("canonical issue not found: %s", canonicalID)
	}

	// Add a "duplicates" dependency edge (duplicate → canonical)
	dep := &types.Dependency{
		IssueID:     duplicateID,
		DependsOnID: canonicalID,
		Type:        types.DepDuplicates,
	}
	if err := store.AddDependency(ctx, dep, actor); err != nil {
		return fmt.Errorf("failed to add duplicate link: %w", err)
	}

	// Close the duplicate issue through the lifecycle operation so it records
	// the complete closure state.
	if err := store.CloseIssue(ctx, duplicateID, "", actor, ""); err != nil {
		return fmt.Errorf("failed to close duplicate: %w", err)
	}

	commandDidWrite.Store(true)

	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"duplicate": duplicateID,
			"canonical": canonicalID,
			"status":    "closed",
		})
	}

	fmt.Printf("%s Marked %s as duplicate of %s (closed)\n", ui.RenderPass("✓"), duplicateID, canonicalID)
	return nil
}

func runSupersede(cmd *cobra.Command, args []string) error {
	return runSupersedeWithOptions(args, supersedeOptionsFromCommand(cmd))
}

type supersedeOptions struct {
	replacementID string
}

func supersedeOptionsFromCommand(cmd *cobra.Command) supersedeOptions {
	if cmd == nil {
		return supersedeOptions{}
	}
	replacementID, _ := cmd.Flags().GetString("with")
	return supersedeOptions{replacementID: replacementID}
}

func runSupersedeWithOptions(args []string, opts supersedeOptions) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("supersede is not supported in proxied-server mode")
	}
	if err := CheckReadonly("supersede"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("supersede")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	ctx := getRootContext()
	store := getStore()
	actor := getActor()

	// Resolve partial IDs
	oldID, newID, err := resolveDuplicatePair(ctx, store, args[0], opts.replacementID)
	if err != nil {
		return err
	}
	return executeSupersede(ctx, store, actor, oldID, newID)
}

func executeSupersede(ctx context.Context, store interface {
	GetIssue(context.Context, string) (*types.Issue, error)
	AddDependency(context.Context, *types.Dependency, string) error
	CloseIssue(context.Context, string, string, string, string) error
}, actor, oldID, newID string) error {
	if oldID == newID {
		return fmt.Errorf("cannot mark an issue as superseded by itself")
	}
	// Verify new issue exists
	var newIssue *types.Issue
	newIssue, err := store.GetIssue(ctx, newID)
	if err != nil || newIssue == nil {
		return fmt.Errorf("replacement issue not found: %s", newID)
	}

	// Add a "supersedes" dependency edge (old → new)
	dep := &types.Dependency{
		IssueID:     oldID,
		DependsOnID: newID,
		Type:        types.DepSupersedes,
	}
	if err := store.AddDependency(ctx, dep, actor); err != nil {
		return fmt.Errorf("failed to add supersede link: %w", err)
	}

	// Close the superseded issue through the lifecycle operation so it records
	// the complete closure state.
	if err := store.CloseIssue(ctx, oldID, "", actor, ""); err != nil {
		return fmt.Errorf("failed to close superseded issue: %w", err)
	}

	commandDidWrite.Store(true)

	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"superseded":  oldID,
			"replacement": newID,
			"status":      "closed",
		})
	}

	fmt.Printf("%s Marked %s as superseded by %s (closed)\n", ui.RenderPass("✓"), oldID, newID)
	return nil
}

func resolveDuplicatePair(ctx context.Context, store utils.PartialIDResolverStore, firstInput, secondInput string) (string, string, error) {
	firstID, err := resolveDuplicateID(ctx, store, firstInput)
	if err != nil {
		return "", "", err
	}
	secondID, err := resolveDuplicateID(ctx, store, secondInput)
	if err != nil {
		return "", "", err
	}
	return firstID, secondID, nil
}

func resolveDuplicateID(ctx context.Context, store utils.PartialIDResolverStore, input string) (string, error) {
	id, err := utils.ResolvePartialID(ctx, store, input)
	if err != nil {
		return "", fmt.Errorf("failed to resolve %s: %w", input, err)
	}
	return id, nil
}
