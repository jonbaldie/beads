package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

func newRelateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "relate <id1> <id2>",
		Short: "Create a bidirectional relates_to link between issues",
		Long: `Create a loose 'see also' relationship between two issues.

The relates_to link is bidirectional - both issues will reference each other.
This enables knowledge graph connections without blocking or hierarchy.

Examples:
  bd relate bd-abc bd-xyz    # Link two related issues
  bd relate bd-123 bd-456    # Create see-also connection`,
		Args: cobra.ExactArgs(2),
		RunE: runRelate,
	}
	cmd.ValidArgsFunction = issueIDCompletion
	return cmd
}

func newUnrelateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unrelate <id1> <id2>",
		Short: "Remove a relates_to link between issues",
		Long: `Remove a relates_to relationship between two issues.

Removes the link in both directions.

Example:
  bd unrelate bd-abc bd-xyz`,
		Args: cobra.ExactArgs(2),
		RunE: runUnrelate,
	}
	cmd.ValidArgsFunction = issueIDCompletion
	return cmd
}

func init() {
	depCmd.AddCommand(newRelateCmd())
	depCmd.AddCommand(newUnrelateCmd())
}

func runRelate(_ *cobra.Command, args []string) error {
	if err := CheckReadonly("relate"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("relate")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	if usesProxiedServer() {
		return runRelateProxiedServer(getRootContext(), args)
	}
	id1, id2, err := resolveRelatedIssuePair(args)
	if err != nil {
		return err
	}
	if id1 == id2 {
		return fmt.Errorf("cannot relate an issue to itself")
	}
	if err := requireIssues(id1, id2); err != nil {
		return err
	}
	if err := addRelatedPair(id1, id2); err != nil {
		return err
	}
	return reportRelatedPair(id1, id2, true)
}

func runUnrelate(_ *cobra.Command, args []string) error {
	if err := CheckReadonly("unrelate"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("unrelate")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	if usesProxiedServer() {
		return runUnrelateProxiedServer(getRootContext(), args)
	}
	id1, id2, err := resolveRelatedIssuePair(args)
	if err != nil {
		return err
	}
	if err := requireIssues(id1, id2); err != nil {
		return err
	}
	if err := removeRelatedPair(id1, id2); err != nil {
		return err
	}
	return reportRelatedPair(id1, id2, false)
}

func resolveRelatedIssuePair(args []string) (string, string, error) {
	id1, err := utils.ResolvePartialID(getRootContext(), getStore(), args[0])
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve %s: %w", args[0], err)
	}
	id2, err := utils.ResolvePartialID(getRootContext(), getStore(), args[1])
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve %s: %w", args[1], err)
	}
	return id1, id2, nil
}

func requireIssues(ids ...string) error {
	for _, id := range ids {
		issue, err := getStore().GetIssue(getRootContext(), id)
		if err != nil {
			return fmt.Errorf("failed to get issue %s: %w", id, err)
		}
		if issue == nil {
			return fmt.Errorf("issue not found: %s", id)
		}
	}
	return nil
}

func addRelatedPair(id1, id2 string) error {
	for _, pair := range [][2]string{{id1, id2}, {id2, id1}} {
		dep := &types.Dependency{IssueID: pair[0], DependsOnID: pair[1], Type: types.DepRelatesTo}
		if err := getStore().AddDependencyWithOptions(getRootContext(), dep, getActor(), storage.DependencyAddOptions{EmitEvent: true}); err != nil {
			return fmt.Errorf("failed to add relates-to %s -> %s: %w", pair[0], pair[1], err)
		}
	}
	return nil
}

func removeRelatedPair(id1, id2 string) error {
	for _, pair := range [][2]string{{id1, id2}, {id2, id1}} {
		if err := getStore().RemoveDependencyWithOptions(getRootContext(), pair[0], pair[1], getActor(), storage.DependencyRemoveOptions{EmitEvent: true}); err != nil {
			return fmt.Errorf("failed to remove relates-to %s -> %s: %w", pair[0], pair[1], err)
		}
	}
	return nil
}

func reportRelatedPair(id1, id2 string, related bool) error {
	if isJSONOutput() {
		result := map[string]interface{}{"id1": id1, "id2": id2}
		if related {
			result["related"] = true
		} else {
			result["unrelated"] = true
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}
	verb := "Unlinked"
	if related {
		verb = "Linked"
	}
	fmt.Printf("%s %s %s ↔ %s\n", ui.RenderPass("✓"), verb, id1, id2)
	return nil
}

// Note: contains, remove, formatRelatesTo functions removed per Decision 004
// relates-to links now use dependencies API instead of Issue.RelatesTo field
