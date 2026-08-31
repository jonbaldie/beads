package main

import (
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

var molLastActivityCmd = &cobra.Command{
	Use:   "last-activity <molecule-id>",
	Short: "Show last activity timestamp for a molecule",
	Long: `Show the most recent activity timestamp for a molecule.

Returns the timestamp of the most recent change to any step in the molecule,
making it easy to detect stale or stuck molecules.

Activity sources:
  step_closed      - A step was closed
  step_updated     - A step was updated (claimed, edited, etc.)
  molecule_updated - The molecule root itself was updated

Examples:
  bd mol last-activity hq-wisp-0laki
  bd mol last-activity hq-wisp-0laki --json`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("mol-last-activity")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if usesProxiedServer() {
			return runMolLastActivityProxiedServer(getRootContext(), args[0])
		}

		ctx := getRootContext()

		if getStore() == nil {
			return HandleErrorRespectJSON("no database connection")
		}

		moleculeID, err := utils.ResolvePartialID(ctx, getStore(), args[0])
		if err != nil {
			return HandleErrorRespectJSON("molecule '%s' not found", args[0])
		}

		activity, err := getStore().GetMoleculeLastActivity(ctx, moleculeID)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}

		if isJSONOutput() {
			return outputJSON(activity)
		}

		fmt.Println(activity.LastActivity.UTC().Format(time.RFC3339))
		return nil
	},
}

func init() {
	molCmd.AddCommand(molLastActivityCmd)
}
