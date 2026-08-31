package main

import (
	"fmt"

	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/issueops"
	"github.com/spf13/cobra"
)

func runCreate(cmd *cobra.Command, args []string) error {
	if err := CheckReadonly("create"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("create")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	handled, err := dispatchCreateSpecial(cmd, args)
	if handled {
		return err
	}
	return runCreateDirect(cmd, args)
}

func dispatchCreateSpecial(cmd *cobra.Command, args []string) (bool, error) {
	if usesProxiedServer() {
		in, err := gatherCreateInput(cmd, args)
		if err != nil {
			return true, err
		}
		return true, runCreateProxiedServer(cmd, getRootContext(), in)
	}
	file, _ := cmd.Flags().GetString("file")
	graphFile, _ := cmd.Flags().GetString("graph")

	if file != "" {
		// gatherCreateInput repeats the --file argument checks and applies
		// the plan-wide flags this route used to accept and ignore
		// (--ephemeral, --no-history, --mol-type, --validate). It is the
		// same input the proxied route reads, which is what lets both build
		// one issueops.CreateBatchRequest.
		in, err := gatherCreateInput(cmd, args)
		if err != nil {
			return true, err
		}
		return true, createIssuesFromMarkdown(getRootContext(), in)
	}

	if graphFile != "" {
		if len(args) > 0 {
			return true, HandleError("cannot specify both title and --graph flag")
		}
		graphDryRun, _ := cmd.Flags().GetBool("dry-run")
		graphOpts := graphApplyOptionsFromFlags(cmd)
		if err := graphOpts.Validate(); err != nil {
			return true, HandleError("invalid graph options: %v", err)
		}
		if err := rejectSingleIssueFlagsForGraph(cmd); err != nil {
			return true, err
		}
		return true, createIssuesFromGraph(graphFile, graphDryRun, graphOpts)
	}
	return false, nil
}

func runCreateDirect(cmd *cobra.Command, args []string) error {
	st, err := gatherCreateDirect(cmd, args)
	if err != nil {
		return err
	}
	resolveCreateDirectRepo(st)
	if st.ident.dryRun && st.ids.parent == "" {
		return renderCreateDirectDryRun(st)
	}
	if err := maybeOpenCreateTargetStore(st); err != nil {
		return err
	}
	closer, err := applyCreateDirectParent(st)
	if err != nil {
		return err
	}
	defer closer()
	if st.ident.dryRun {
		return renderCreateDirectDryRun(st)
	}
	return writeCreateDirect(st)
}

func printCreateDirectResult(created *issueops.Issue, silent bool) error {
	if isJSONOutput() {
		if err := outputJSON(created); err != nil {
			return err
		}
	} else if silent {
		fmt.Println(created.ID)
	} else {
		debug.PrintNormal("%s Created issue: %s\n", ui.RenderPass("✓"), formatFeedbackID(created.ID, created.Title))
		debug.PrintNormal("  Priority: P%d\n", created.Priority)
		debug.PrintNormal("  Status: %s\n", created.Status)
		maybeShowTip(getStore())
	}
	SetLastTouchedID(created.ID)
	return nil
}
