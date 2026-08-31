package main

import (
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/formula"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/spf13/cobra"
)

func runCook(cmd *cobra.Command, args []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("cook is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("cook")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	flags, err := parseCookFlags(cmd, args)
	if err != nil {
		return HandleError("%v", err)
	}
	return executeCook(flags)
}

func executeCook(flags *cookFlags) error {
	if flags.persist {
		if err := CheckReadonly("cook --persist"); err != nil {
			return err
		}
		if getStore() == nil {
			return HandleError("no database connection")
		}
	}

	resolved, err := loadAndResolveFormula(flags.formulaPath, flags.searchPaths)
	if err != nil {
		return HandleError("%v", err)
	}
	if flags.runtimeMode {
		if err := formula.ValidateVars(resolved, flags.inputVars); err != nil {
			return HandleError("%v", err)
		}
	}

	protoID := resolved.Formula
	if flags.prefix != "" {
		protoID = flags.prefix + resolved.Formula
	}

	vars := formula.ExtractVariables(resolved)
	var bondPoints []string
	if resolved.Compose != nil {
		bondPoints = cookBondPointIDs(resolved.Compose.BondPoints)
	}

	return outputCookMode(flags, resolved, protoID, vars, bondPoints)
}

func cookBondPointIDs(points []*formula.BondPoint) []string {
	ids := make([]string, 0, len(points))
	for _, point := range points {
		ids = append(ids, point.ID)
	}
	return ids
}

func outputCookMode(flags *cookFlags, resolved *formula.Formula, protoID string, vars, bondPoints []string) error {
	if flags.dryRun {
		outputCookDryRun(resolved, protoID, flags.runtimeMode, flags.inputVars, vars, bondPoints)
		return nil
	}
	if flags.persist {
		if err := persistCookFormula(getRootContext(), resolved, protoID, flags.force, vars, bondPoints); err != nil {
			return HandleError("%v", err)
		}
		return nil
	}
	if err := outputCookEphemeral(resolved, flags.runtimeMode, flags.inputVars, vars); err != nil {
		return HandleError("%v", err)
	}
	if !isJSONOutput() {
		fmt.Fprintf(os.Stderr, "Note: cook is ephemeral and did not persist a proto.\n")
		fmt.Fprintf(os.Stderr, "  Pour this formula directly:  bd pour %s\n", flags.formulaPath)
		fmt.Fprintf(os.Stderr, "  Persist a proto first:       bd cook %s --persist\n", flags.formulaPath)
		fmt.Fprintf(os.Stderr, "  Then:                        bd mol pour <name>\n")
	}
	return nil
}

// cookFormulaResult holds the result of cooking
type cookFormulaResult struct {
	ProtoID string
	Created int
}
