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

func newLinkCmd() *cobra.Command {
	linkCmd := &cobra.Command{
		Use:     "link <id1> <id2>",
		GroupID: "issues",
		Short:   "Link two issues with a dependency",
		Long: `Link two issues with a dependency.

Shorthand for 'bd dep add <id1> <id2>'. By default creates a "blocks"
dependency (id2 blocks id1). Use --type to specify a different relationship.

Examples:
  bd link bd-123 bd-456                    # bd-456 blocks bd-123
  bd link bd-123 bd-456 --type related     # bd-123 related to bd-456
  bd link bd-123 bd-456 --type parent-child`,
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runLink,
	}
	linkCmd.Flags().StringP("type", "t", "blocks", "Dependency type (blocks|tracks|related|parent-child|discovered-from)")
	linkCmd.ValidArgsFunction = issueIDCompletion
	return linkCmd
}

type linkInputs struct {
	fromID      string
	toID        string
	fromStore   storage.DoltStorage
	fromCleanup func()
	toCleanup   func()
}

func runLink(cmd *cobra.Command, args []string) error {
	if err := CheckReadonly("link"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("link")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	if usesProxiedServer() {
		return runLinkProxiedServer(cmd, getRootContext(), args)
	}

	depType, _ := cmd.Flags().GetString("type")
	inputs, err := resolveLinkInputs(getRootContext(), args[0], args[1], depType)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	defer inputs.fromCleanup()
	defer inputs.toCleanup()

	dep := &types.Dependency{IssueID: inputs.fromID, DependsOnID: inputs.toID, Type: types.DependencyType(depType)}
	if err := inputs.fromStore.AddDependencyWithOptions(getRootContext(), dep, getActor(), storage.DependencyAddOptions{EmitEvent: true}); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	warnIfCyclesExist(inputs.fromStore)
	if err := commitPendingIfEmbedded(getRootContext(), inputs.fromStore, getActor(), doltAutoCommitParams{
		Command:  "link",
		IssueIDs: []string{inputs.fromID, inputs.toID},
	}); err != nil {
		return HandleErrorRespectJSON("failed to commit: %v", err)
	}

	SetLastTouchedID(inputs.fromID)
	return renderLinkSuccess(inputs.fromID, inputs.toID, depType)
}

func resolveLinkInputs(ctx context.Context, id1, id2, depType string) (linkInputs, error) {
	fromID, fromStore, fromCleanup, err := resolveIDForMutation(ctx, getStore(), id1)
	if err != nil {
		return linkInputs{}, err
	}
	toID, _, toCleanup, err := resolveIDWithRouting(ctx, getStore(), id2)
	if err != nil {
		fromCleanup()
		return linkInputs{}, err
	}
	dt := types.DependencyType(depType)
	if isDisallowedHierarchicalDependency(fromID, toID, dt) {
		fromCleanup()
		toCleanup()
		return linkInputs{}, fmt.Errorf("cannot add dependency: %s is already a child of %s. Children inherit dependency on parent completion via hierarchy. Adding an explicit dependency would create a deadlock", fromID, toID)
	}
	if !dt.IsValid() {
		fromCleanup()
		toCleanup()
		return linkInputs{}, fmt.Errorf("invalid dependency type %q: must be non-empty and at most %d characters", depType, types.MaxDependencyTypeLen)
	}
	return linkInputs{fromID: fromID, toID: toID, fromStore: fromStore, fromCleanup: fromCleanup, toCleanup: toCleanup}, nil
}

func renderLinkSuccess(fromID, toID, depType string) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"status":        "added",
			"issue_id":      fromID,
			"depends_on_id": toID,
			"type":          depType,
		})
	}
	fmt.Printf("%s Linked: %s depends on %s (%s)\n",
		ui.RenderPass("✓"), formatFeedbackIDParen(fromID, lookupTitle(fromID)), formatFeedbackIDParen(toID, lookupTitle(toID)), depType)
	return nil
}

func init() {
	rootCmd.AddCommand(newLinkCmd())
}
