// Package main implements the bd CLI dependency management commands.
package main

import (
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

var depTreeCmd = &cobra.Command{
	Use:   "tree [issue-id]",
	Short: "Show dependency tree",
	Long: `Show dependency tree rooted at the given issue.

By default, shows dependencies (what blocks this issue). Use --direction to control:
  - down: Show dependencies (what blocks this issue) - default
  - up:   Show dependents (what this issue blocks)
  - both: Show full graph in both directions

Examples:
  bd dep tree gt-0iqq                    # Show what blocks gt-0iqq
  bd dep tree gt-0iqq --direction=up     # Show what gt-0iqq blocks
  bd dep tree gt-0iqq --status=open      # Only show open issues
  bd dep tree gt-0iqq --depth=3          # Limit to 3 levels deep

A node reached by two paths is shown ONCE, under the first path that got
there, and a cycle simply ends the descent. --show-all-paths is a deprecated
no-op; use 'bd dep cycles' to find circular dependencies.

--max-rows / BEADS_MAX_ROWS caveat: the tree walk has no query filter to
thread the cap through, so the full tree is always built first and the
node count is checked afterward (post-hoc), not during the walk. The cap is
honored on the --proxied-server route too, which it was not before.`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("dep-tree")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		// Both routes, one body: which accessor answers and how the root id is
		// resolved are both inside resolveTreeTarget.
		return runDepTree(cmd, getRootContext(), args)
	},
}

var depCyclesCmd = &cobra.Command{
	Use:           "cycles",
	Short:         "Detect dependency cycles",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("dep-cycles")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		// Both routes, one body: the only difference between them is which
		// accessor answers, and that is inside openCycleDetector.
		return runDepCycles()
	},
}

// outputMermaidTree outputs a dependency tree in Mermaid.js flowchart format
func outputMermaidTree(tree []*types.TreeNode, rootID string) {
	if len(tree) == 0 {
		fmt.Println("flowchart TD")
		fmt.Printf("  %s[\"No dependencies\"]\n", rootID)
		return
	}

	fmt.Println("flowchart TD")

	// Output nodes
	nodesSeen := make(map[string]bool)
	for _, node := range tree {
		if !nodesSeen[node.ID] {
			emoji := getStatusEmoji(node.Status)
			label := fmt.Sprintf("%s %s: %s", emoji, node.ID, node.Title)
			// Escape quotes and backslashes in label
			label = strings.ReplaceAll(label, "\\", "\\\\")
			label = strings.ReplaceAll(label, "\"", "\\\"")
			fmt.Printf("  %s[\"%s\"]\n", node.ID, label)

			nodesSeen[node.ID] = true
		}
	}

	fmt.Println()

	// Output edges - use explicit parent relationships from ParentID
	for _, node := range tree {
		if node.ParentID != "" && node.ParentID != node.ID {
			fmt.Printf("  %s --> %s\n", node.ParentID, node.ID)
		}
	}
}

// getStatusEmoji returns a symbol indicator for a given status
func getStatusEmoji(status types.Status) string {
	switch status {
	case types.StatusOpen:
		return "☐" // U+2610 Ballot Box
	case types.StatusInProgress:
		return "◧" // U+25E7 Square Left Half Black
	case types.StatusBlocked:
		return "⚠" // U+26A0 Warning Sign
	case types.StatusDeferred:
		return "❄" // U+2744 Snowflake (on ice)
	case types.StatusClosed:
		return "☑" // U+2611 Ballot Box with Check
	default:
		return "?"
	}
}

// treeRenderer holds state for rendering a tree with proper connectors
type treeRenderer struct {
	// Track which nodes we've already displayed (for "shown above" handling)
	seen map[string]bool
	// Track connector state at each depth level (true = has more siblings)
	activeConnectors []bool
	// Maximum depth reached
	maxDepth int
	// Direction of traversal
	direction string
	// Whether the root node has open children (i.e., is blocked)
	rootBlocked bool
}

// renderTree renders the tree with proper box-drawing connectors
func renderTree(tree []*types.TreeNode, maxDepth int, direction string) {
	if len(tree) == 0 {
		return
	}

	r := &treeRenderer{
		seen:             make(map[string]bool),
		activeConnectors: make([]bool, maxDepth+1),
		maxDepth:         maxDepth,
		direction:        direction,
	}

	children, root := treeChildrenAndRoot(tree)
	r.rootBlocked = treeRootHasOpenBlockers(root, children)

	// Render recursively from root
	r.renderNode(root, children, 0, true)
}

func treeChildrenAndRoot(tree []*types.TreeNode) (map[string][]*types.TreeNode, *types.TreeNode) {
	// Build a map of parent -> children for proper sibling tracking.
	children := make(map[string][]*types.TreeNode)
	var root *types.TreeNode
	for _, node := range tree {
		if node.Depth == 0 {
			root = node
		} else {
			children[node.ParentID] = append(children[node.ParentID], node)
		}
	}
	if root == nil {
		root = tree[0]
	}
	return children, root
}

func treeRootHasOpenBlockers(root *types.TreeNode, children map[string][]*types.TreeNode) bool {
	if root == nil {
		return false
	}
	// Only genuine blockers (blocks, conditional-blocks, waits-for) count;
	// parent-child, related, discovered-from, etc. do not block.
	for _, child := range children[root.ID] {
		if (child.Status == types.StatusOpen || child.Status == types.StatusInProgress) &&
			child.EdgeFromParent.IsBlockingEdge() {
			return true
		}
	}
	return false
}

// renderNode renders a single node and its children
func (r *treeRenderer) renderNode(node *types.TreeNode, children map[string][]*types.TreeNode, depth int, isLast bool) {
	if node == nil {
		return
	}

	prefix := treeNodePrefix(r.activeConnectors, depth, isLast)

	// Check if we've seen this node before (diamond dependency)
	if r.seen[node.ID] {
		fmt.Printf("%s%s (shown above)\n", prefix.String(), ui.RenderMuted(node.ID))
		return
	}
	r.seen[node.ID] = true

	// Format the node line
	line := formatTreeNode(node, depth == 0 && r.rootBlocked)
	line = addTreeNodeTruncation(line, node, children, depth, r.maxDepth)

	fmt.Printf("%s%s\n", prefix.String(), line)

	r.renderTreeChildren(children[node.ID], children, depth)
}

func treeNodePrefix(activeConnectors []bool, depth int, isLast bool) strings.Builder {
	var prefix strings.Builder
	// Add vertical lines for active parent connectors.
	for i := 0; i < depth; i++ {
		if activeConnectors[i] {
			prefix.WriteString("│   ")
		} else {
			prefix.WriteString("    ")
		}
	}
	if depth > 0 {
		if isLast {
			prefix.WriteString("└── ")
		} else {
			prefix.WriteString("├── ")
		}
	}
	return prefix
}

func addTreeNodeTruncation(line string, node *types.TreeNode, children map[string][]*types.TreeNode, depth, maxDepth int) string {
	if node.Truncated || (depth == maxDepth && len(children[node.ID]) > 0) {
		return line + ui.RenderWarn(" …")
	}
	return line
}

func (r *treeRenderer) renderTreeChildren(nodeChildren []*types.TreeNode, children map[string][]*types.TreeNode, depth int) {
	// For depth 0 (root level), never show a vertical connector since root has no siblings.
	for i, child := range nodeChildren {
		if depth > 0 {
			r.activeConnectors[depth] = (i < len(nodeChildren)-1)
		}
		r.renderNode(child, children, depth+1, i == len(nodeChildren)-1)
	}
}

// formatTreeNode formats a single tree node with status, ready indicator, etc.
// isBlocked indicates the node has open blocking dependencies and should not show [READY].
func formatTreeNode(node *types.TreeNode, isBlocked bool) string {
	if IsExternalRef(node.ID) {
		return formatExternalTreeNode(node)
	}

	idStr := formatTreeNodeID(node)
	line := fmt.Sprintf("%s: %s [P%d] (%s)",
		idStr, node.Title, node.Priority, node.Status)
	return addTreeNodeAnnotations(line, node, isBlocked)
}

func formatExternalTreeNode(node *types.TreeNode) string {
	var idStr string
	switch node.Status {
	case types.StatusClosed:
		idStr = ui.StatusClosedStyle().Render(node.Title)
	case types.StatusBlocked:
		idStr = ui.StatusBlockedStyle().Render(node.Title)
	default:
		idStr = node.Title
	}
	return fmt.Sprintf("%s (external)", idStr)
}

func formatTreeNodeID(node *types.TreeNode) string {
	var idStr string
	switch node.Status {
	case types.StatusOpen:
		idStr = ui.StatusOpenStyle().Render(node.ID)
	case types.StatusInProgress:
		idStr = ui.StatusInProgressStyle().Render(node.ID)
	case types.StatusBlocked:
		idStr = ui.StatusBlockedStyle().Render(node.ID)
	case types.StatusClosed:
		idStr = ui.StatusClosedStyle().Render(node.ID)
	default:
		idStr = node.ID
	}
	return idStr
}

func addTreeNodeAnnotations(line string, node *types.TreeNode, isBlocked bool) string {
	if node.Depth > 0 && node.EdgeFromParent != "" {
		line += " " + ui.RenderMuted(fmt.Sprintf("[%s]", node.EdgeFromParent))
	}

	// Add READY/BLOCKED indicator for root node
	if node.Status == types.StatusOpen && node.Depth == 0 {
		if isBlocked {
			line += " " + ui.FailStyle().Bold(true).Render("[BLOCKED]")
		} else {
			line += " " + ui.PassStyle().Bold(true).Render("[READY]")
		}
	}

	return line
}

// validateExternalRef validates the format of an external dependency reference.
// Valid format: external:<project>:<capability>
func validateExternalRef(ref string) error {
	if !strings.HasPrefix(ref, "external:") {
		return fmt.Errorf("external reference must start with 'external:'")
	}

	parts := strings.SplitN(ref, ":", 3)
	if len(parts) != 3 {
		return fmt.Errorf("invalid external reference format: expected 'external:<project>:<capability>', got '%s'", ref)
	}

	project := parts[1]
	capability := parts[2]

	if project == "" {
		return fmt.Errorf("external reference missing project name")
	}
	if capability == "" {
		return fmt.Errorf("external reference missing capability name")
	}

	return nil
}

// IsExternalRef returns true if the dependency reference is an external reference.
func IsExternalRef(ref string) bool {
	return strings.HasPrefix(ref, "external:")
}

// ParseExternalRef parses an external reference into project and capability.
// Returns empty strings if the format is invalid.
func ParseExternalRef(ref string) (project, capability string) {
	if !IsExternalRef(ref) {
		return "", ""
	}
	parts := strings.SplitN(ref, ":", 3)
	if len(parts) != 3 {
		return "", ""
	}
	return parts[1], parts[2]
}

func init() {
	// dep command shorthand flag
	depCmd.Flags().StringP("blocks", "b", "", "Issue ID that this issue blocks (shorthand for: bd dep add <blocked> <blocker>)")
	depCmd.Flags().Bool("no-cycle-check", false, "Skip per-edge cycle checks for speed (bulk wiring); bulk --file adds still run one final whole-graph check before commit")

	depAddCmd.Flags().StringP("type", "t", "blocks", "Dependency type (blocks|tracks|related|parent-child|discovered-from|until|caused-by|validates|relates-to|supersedes); 'blocked-by' and 'depends-on' are accepted as aliases for 'blocks'")
	depAddCmd.Flags().String("blocked-by", "", "Issue ID that blocks the first issue (alternative to positional arg)")
	depAddCmd.Flags().String("depends-on", "", "Issue ID that the first issue depends on (alias for --blocked-by)")
	depAddCmd.Flags().String("file", "", "Read dependency edges from JSONL file, or '-' for stdin")
	depAddCmd.Flags().Bool("no-cycle-check", false, "Skip per-edge cycle checks for speed (bulk wiring); bulk --file adds still run one final whole-graph check before commit")

	// DEPRECATED NO-OP, and it always was one: nothing has ever read this flag,
	// so a diamond has always been rendered under one parent only. The role's
	// contract states the first-visit rule as a promise
	// (issueops/treewalker.go, TreeResult.Nodes) and this flag stays accepted so
	// no script breaks. Same story as TreeNode.Truncated.
	depTreeCmd.Flags().Bool("show-all-paths", false, "Deprecated no-op: accepted and ignored. A node reached by two paths is shown once, under the first.")
	depTreeCmd.Flags().IntP("max-depth", "d", 50, "Maximum tree depth to display (safety limit)")
	depTreeCmd.Flags().Bool("reverse", false, "Show dependent tree (deprecated: use --direction=up)")
	depTreeCmd.Flags().String("direction", "", "Tree direction: 'down' (dependencies), 'up' (dependents), or 'both'")
	depTreeCmd.Flags().String("status", "", "Filter to only show issues with this status (open, in_progress, blocked, deferred, closed)")
	depTreeCmd.Flags().String("format", "", "Output format: 'mermaid' for Mermaid.js flowchart")
	// Defensive row cap (be-x42v): applied to the node count after the walk, by
	// the role, on BOTH routes — hence the routed variant of the flag.
	addRoutedMaxRowsFlag(depTreeCmd)
	// Note: --type flag intentionally omitted from depTreeCmd — TreeNode lacks
	// dependency type info so filtering is not possible. Use 'bd dep list --type' instead.

	depListCmd.Flags().String("direction", "down", "Direction: 'down' (dependencies), 'up' (dependents)")
	depListCmd.Flags().StringP("type", "t", "", "Filter by dependency type (e.g., tracks, blocks, parent-child)")

	// Issue ID completions for dep subcommands
	configureIssueIDCompletions(depAddCmd, depRemoveCmd, depListCmd, depTreeCmd)

	depCmd.AddCommand(depAddCmd)
	depCmd.AddCommand(depRemoveCmd)
	depCmd.AddCommand(depListCmd)
	depCmd.AddCommand(depTreeCmd)
	depCmd.AddCommand(depCyclesCmd)
	rootCmd.AddCommand(depCmd)
}

func configureIssueIDCompletions(commands ...*cobra.Command) {
	for _, command := range commands {
		command.ValidArgsFunction = issueIDCompletion
	}
}
