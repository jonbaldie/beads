package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/jonbaldie/beads/issueops"
)

// `bd dep tree` on ONE role and ONE body. What is left here is the front door's
// own work: flag vocabulary, id resolution, and rendering.

// treeTarget is a resolved root and the walker that can answer about it. The two
// travel together because on the DIRECT route resolveIDWithRouting may resolve
// the id against a prefix-ROUTED database, and the walk then has to be asked of
// that store rather than of the local one.
type treeTarget struct {
	rootID  string
	walker  issueops.TreeWalker
	cleanup func()
}

// resolveTreeTarget resolves the argument to an exact id and hands back the tree
// role for whichever route this invocation is on.
//
// RESOLUTION HAPPENS FIRST, AND THE ROLE IS ASKED FOR ONLY AFTER IT SUCCEEDS: a
// lookup that finds nothing must report the lookup failure, not a missing
// surface the command never got to use.
func resolveTreeTarget(ctx context.Context, arg string) (treeTarget, error) {
	if usesProxiedServer() {
		return proxiedTreeTarget(ctx, arg)
	}
	rootID, treeStore, cleanup, err := resolveIDWithRouting(ctx, getStore(), arg)
	if err != nil {
		return treeTarget{}, err
	}
	walker, err := treeStore.TreeWalker()
	if err != nil {
		cleanup()
		return treeTarget{}, err
	}
	return treeTarget{rootID: rootID, walker: walker, cleanup: cleanup}, nil
}

// proxiedTreeTarget resolves the argument against the proxied server and hands
// back the provider's tree surface.
//
// THIS ROUTE GAINS PARTIAL-ID RESOLUTION, which it has never had: it passed the
// argument to the use case verbatim, so `bd dep tree a1b2` worked on a direct
// workspace and failed on a team server.
func proxiedTreeTarget(ctx context.Context, arg string) (treeTarget, error) {
	uw, err := proxiedOpenReadUOW(ctx)
	if err != nil {
		return treeTarget{}, err
	}
	rootID, err := utils.ResolvePartialID(ctx, uowMolReader{uw: uw}, arg)
	uw.Close(ctx)
	if err != nil {
		return treeTarget{}, fmt.Errorf("resolving issue ID %s: %w", arg, err)
	}
	if getUOWProvider() == nil {
		return treeTarget{}, errors.New("proxied-server UOW provider not initialized")
	}
	src, ok := getUOWProvider().(uow.TreeWalkerSource)
	if !ok {
		return treeTarget{}, fmt.Errorf("proxied-server provider %T does not offer the dependency-tree surface", getUOWProvider())
	}
	walker, err := src.TreeWalker()
	if err != nil {
		return treeTarget{}, err
	}
	return treeTarget{rootID: rootID, walker: walker, cleanup: func() {}}, nil
}

// runDepTree is the whole of `bd dep tree` on both routes.
func runDepTree(cmd *cobra.Command, ctx context.Context, args []string) error {
	opts, err := gatherDepTreeOptions(cmd)
	if err != nil {
		return err
	}
	maxRows, maxRowsSource, err := resolveMaxRows(cmd)
	if err != nil {
		return err
	}
	target, err := resolveTreeTarget(ctx, args[0])
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	defer target.cleanup()
	tree, err := walkDepTree(ctx, target, opts, maxRows, maxRowsSource)
	if err != nil {
		return err
	}
	return printDepTree(tree, args[0], target.rootID, opts)
}

type depTreeOptions struct {
	maxDepth  int
	direction issueops.TreeDirection
	status    string
	formatStr string
}

func gatherDepTreeOptions(cmd *cobra.Command) (depTreeOptions, error) {
	maxDepth, _ := cmd.Flags().GetInt("max-depth")
	reverse, _ := cmd.Flags().GetBool("reverse")
	directionFlag, _ := cmd.Flags().GetString("direction")
	statusFilter, _ := cmd.Flags().GetString("status")
	formatStr, _ := cmd.Flags().GetString("format")
	if strings.EqualFold(formatStr, "json") {
		setJSONOutput(true)
		formatStr = ""
	}
	// --reverse is the deprecated spelling of --direction=up, and it loses to an
	// explicit --direction exactly as it did before.
	switch {
	case directionFlag != "":
	case reverse:
		directionFlag = string(issueops.TreeUp)
	default:
		directionFlag = string(issueops.TreeDown)
	}
	direction := issueops.TreeDirection(directionFlag)
	switch direction {
	case issueops.TreeDown, issueops.TreeUp, issueops.TreeBoth:
	default:
		return depTreeOptions{}, HandleErrorRespectJSON("--direction must be 'down', 'up', or 'both'")
	}
	if maxDepth < 1 {
		return depTreeOptions{}, HandleErrorRespectJSON("--max-depth must be >= 1")
	}
	return depTreeOptions{maxDepth: maxDepth, direction: direction, status: statusFilter, formatStr: formatStr}, nil
}

func walkDepTree(ctx context.Context, target treeTarget, opts depTreeOptions, maxRows int, maxRowsSource string) ([]*types.TreeNode, error) {
	result, err := target.walker.WalkTree(ctx, issueops.WalkTreeRequest{
		RootID:        target.rootID,
		Direction:     opts.direction,
		MaxDepth:      opts.maxDepth,
		Status:        types.Status(opts.status),
		MaxRows:       maxRows,
		MaxRowsSource: maxRowsSource,
	})
	if err != nil {
		if capErr := handleMaxRowsError(err); capErr != nil {
			return nil, capErr
		}
		return nil, HandleErrorRespectJSON("%v", err)
	}
	return result.Nodes, nil
}

func printDepTree(tree []*types.TreeNode, rawID, rootID string, opts depTreeOptions) error {
	if opts.formatStr == "mermaid" {
		outputMermaidTree(tree, rawID)
		return nil
	}
	if isJSONOutput() {
		return outputJSON(tree)
	}
	if len(tree) == 0 {
		printEmptyDepTree(rootID, opts.direction)
		return nil
	}
	printDepTreeHeader(rootID, opts.direction)
	renderTree(tree, opts.maxDepth, string(opts.direction))
	fmt.Println()
	return nil
}

func printEmptyDepTree(rootID string, direction issueops.TreeDirection) {
	switch direction {
	case issueops.TreeUp:
		fmt.Printf("\n%s has no dependents\n", rootID)
	case issueops.TreeBoth:
		fmt.Printf("\n%s has no dependencies or dependents\n", rootID)
	default:
		fmt.Printf("\n%s has no dependencies\n", rootID)
	}
}

func printDepTreeHeader(rootID string, direction issueops.TreeDirection) {
	switch direction {
	case issueops.TreeUp:
		fmt.Printf("\n%s Dependent tree for %s:\n\n", ui.RenderAccent("🌲"), rootID)
	case issueops.TreeBoth:
		fmt.Printf("\n%s Full dependency graph for %s:\n\n", ui.RenderAccent("🌲"), rootID)
	default:
		fmt.Printf("\n%s Dependency tree for %s:\n\n", ui.RenderAccent("🌲"), rootID)
	}
}
