package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jonbaldie/beads/internal/formula"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/utils"
)

// configReader is the minimal slice of storage.Storage that config-reading
// helpers depend on, letting tests inject a fake without spinning up a Dolt
// server. Transaction-bound writers (storeMolWriter) satisfy it with reads
// that see in-transaction config writes.
type configReader interface {
	GetConfig(ctx context.Context, key string) (string, error)
}

// BeadsTemplateLabel is the label used to identify Beads-based templates
const BeadsTemplateLabel = "template"

// variablePattern matches {{variable}} placeholders
var variablePattern = regexp.MustCompile(`\{\{([a-zA-Z_][a-zA-Z0-9_]*)\}\}`)

// TemplateSubgraph holds a template epic and all its descendants
type TemplateSubgraph struct {
	Root         *types.Issue              // The template epic
	Issues       []*types.Issue            // All issues in the subgraph (including root)
	Dependencies []*types.Dependency       // All dependencies within the subgraph
	IssueMap     map[string]*types.Issue   // ID -> Issue for quick lookup
	VarDefs      map[string]formula.VarDef // Variable definitions from formula (for defaults)
	Phase        string                    // Recommended phase: "liquid" (pour) or "vapor" (wisp)
	Pour         bool                      // If true, steps should be materialized as sub-issues (from formula pour=true)
}

// InstantiateResult holds the result of template instantiation
type InstantiateResult struct {
	NewEpicID string            `json:"new_epic_id"`
	IDMapping map[string]string `json:"id_mapping"` // old ID -> new ID
	Created   int               `json:"created"`    // number of issues created
}

// CloneOptions controls how the subgraph is cloned during spawn/bond
type CloneOptions struct {
	Vars      map[string]string // Variable substitutions for {{key}} placeholders
	Assignee  string            // Assign the root epic to this agent/user
	Actor     string            // Actor performing the operation
	Ephemeral bool              // If true, spawned issues are marked for bulk deletion
	Prefix    string            // Override prefix for ID generation (bd-hobo: distinct prefixes)

	// Dynamic bonding fields (for Christmas Ornament pattern)
	ParentID string // Parent molecule ID to bond under (e.g., "patrol-x7k")
	ChildRef string // Child reference with variables (e.g., "arm-{{polecat_name}}")

	// Atomic attachment: if set, adds a dependency from the spawned root to
	// AttachToID within the same transaction as the clone, preventing orphans.
	AttachToID    string               // Molecule ID to attach spawned root to
	AttachDepType types.DependencyType // Dependency type for the attachment

	// RootOnly: if true, only create the root issue (no child step issues).
	// Used by patrol wisps where steps are inlined at prime time, not tracked as beads.
	RootOnly bool
}

// bondedIDPattern validates bonded IDs (alphanumeric, dash, underscore, dot)
var bondedIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

// =============================================================================
// Beads Template Functions
// =============================================================================

// loadTemplateSubgraph loads a template epic and all its descendants
func loadTemplateSubgraph(ctx context.Context, s molReader, templateID string) (*TemplateSubgraph, error) {
	if s == nil {
		return nil, fmt.Errorf("no database connection")
	}

	// Get the root issue
	root, err := s.GetIssue(ctx, templateID)
	if err != nil {
		return nil, fmt.Errorf("failed to get template: %w", err)
	}
	if root == nil {
		return nil, fmt.Errorf("template %s not found", templateID)
	}

	subgraph := &TemplateSubgraph{
		Root:     root,
		Issues:   []*types.Issue{root},
		IssueMap: map[string]*types.Issue{root.ID: root},
	}

	// Recursively load all children (with cycle detection, GH#2719)
	visited := map[string]bool{root.ID: true}
	if err := loadDescendants(ctx, s, subgraph, root.ID, visited); err != nil {
		return nil, err
	}

	// Load all dependencies within the subgraph
	for _, issue := range subgraph.Issues {
		deps, err := s.GetDependencyRecords(ctx, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to get dependencies for %s: %w", issue.ID, err)
		}
		for _, dep := range deps {
			// Only include dependencies where both ends are in the subgraph
			if _, ok := subgraph.IssueMap[dep.DependsOnID]; ok {
				subgraph.Dependencies = append(subgraph.Dependencies, dep)
			}
		}
	}

	return subgraph, nil
}

// loadDescendants recursively loads all child issues.
// It uses two strategies to find children:
// 1. Check dependency records for parent-child relationships
// 2. Check for hierarchical IDs (parent.N) to catch children with missing/wrong deps
//
// The visited set tracks IDs already expanded to detect cycles (GH#2719).
// Without this, cyclic parent-child dependencies cause unbounded recursion leading to OOM.
func loadDescendants(ctx context.Context, s molReader, subgraph *TemplateSubgraph, parentID string, visited map[string]bool) error {
	addedChildren := make(map[string]bool)
	if err := loadExplicitDescendants(ctx, s, subgraph, parentID, visited, addedChildren); err != nil {
		return err
	}

	// Strategy 2: Find hierarchical children by ID pattern
	// This catches children that have missing or incorrect dependency types.
	// Hierarchical IDs follow the pattern: parentID.N (e.g., "gt-abc.1", "gt-abc.2")
	hierarchicalChildren, err := findHierarchicalChildren(ctx, s, parentID)
	if err != nil {
		// Non-fatal: continue with what we have
		return nil
	}

	return loadHierarchicalDescendants(ctx, s, subgraph, parentID, visited, addedChildren, hierarchicalChildren)
}

func loadExplicitDescendants(ctx context.Context, s molReader, subgraph *TemplateSubgraph, parentID string, visited, addedChildren map[string]bool) error {

	// Strategy 1: Get direct parent-child dependents with relationship metadata.
	dependents, err := s.GetDependentsWithMetadata(ctx, parentID)
	if err != nil {
		return fmt.Errorf("failed to get dependents of %s: %w", parentID, err)
	}

	// Only keep explicit parent-child relationships.
	for _, dependent := range dependents {
		if dependent.DependencyType != types.DepParentChild {
			continue
		}

		if _, exists := subgraph.IssueMap[dependent.ID]; exists {
			continue // Already in subgraph
		}

		// Cycle detection (GH#2719)
		if visited[dependent.ID] {
			continue
		}

		child := dependent.Issue

		// Add to subgraph
		subgraph.Issues = append(subgraph.Issues, &child)
		subgraph.IssueMap[child.ID] = &child
		addedChildren[child.ID] = true

		// Mark visited before recursing
		visited[child.ID] = true
		if err := loadDescendants(ctx, s, subgraph, child.ID, visited); err != nil {
			return err
		}
	}

	return nil
}

func loadHierarchicalDescendants(ctx context.Context, s molReader, subgraph *TemplateSubgraph, parentID string, visited, addedChildren map[string]bool, hierarchicalChildren []*types.Issue) error {
	for _, child := range hierarchicalChildren {
		if shouldSkipHierarchicalChild(child, subgraph, visited, addedChildren) {
			continue
		}

		if isReparentedChild(ctx, s, child.ID, parentID) {
			continue
		}

		addHierarchicalChild(subgraph, child, visited, addedChildren)
		if err := loadDescendants(ctx, s, subgraph, child.ID, visited); err != nil {
			return err
		}
	}

	return nil
}

func shouldSkipHierarchicalChild(child *types.Issue, subgraph *TemplateSubgraph, visited, addedChildren map[string]bool) bool {
	if addedChildren[child.ID] {
		return true // Already added via dependency
	}
	if _, exists := subgraph.IssueMap[child.ID]; exists {
		return true // Already in subgraph
	}
	return visited[child.ID] // Cycle detection (GH#2719)
}

func isReparentedChild(ctx context.Context, s molReader, childID, parentID string) bool {
	// Check if this hierarchical child has been reparented to a different parent (GH#2476).
	// If it has an explicit parent-child dependency pointing elsewhere, skip it —
	// the ID pattern match is stale and the child belongs to another molecule.
	depRecs, err := s.GetDependencyRecords(ctx, childID)
	if err != nil {
		return false
	}
	for _, dep := range depRecs {
		if dep.Type == types.DepParentChild && dep.DependsOnID != parentID {
			return true
		}
	}
	return false
}

func addHierarchicalChild(subgraph *TemplateSubgraph, child *types.Issue, visited, addedChildren map[string]bool) {
	subgraph.Issues = append(subgraph.Issues, child)
	subgraph.IssueMap[child.ID] = child
	addedChildren[child.ID] = true
	visited[child.ID] = true
}

// findHierarchicalChildren finds issues with IDs that match the pattern parentID.N
// This catches hierarchical children that may be missing parent-child dependencies.
func findHierarchicalChildren(ctx context.Context, s molReader, parentID string) ([]*types.Issue, error) {
	pattern := parentID + "."
	candidates, err := s.SearchIssues(ctx, "", types.IssueFilter{IssueFilterCore: types.IssueFilterCore{IDPrefix: pattern}})
	if err != nil {
		return nil, err
	}

	var children []*types.Issue
	for _, issue := range candidates {
		_, directParentID, depth := types.ParseHierarchicalID(issue.ID)
		if depth > 0 && directParentID == parentID {
			children = append(children, issue)
		}
	}

	return children, nil
}

// =============================================================================
// Proto Lookup Functions
// =============================================================================

// resolveProtoIDOrTitle resolves a proto by ID or title.
// It first tries to resolve as an ID (via ResolvePartialID).
// If that fails, it searches for protos with matching titles.
// Returns the proto ID if found, or an error if not found or ambiguous.
func resolveProtoIDOrTitle(ctx context.Context, s molReader, input string) (string, error) {
	if protoID, ok := resolveProtoID(ctx, s, input); ok {
		return protoID, nil
	}

	protos, err := s.GetIssuesByLabel(ctx, BeadsTemplateLabel)
	if err != nil {
		return "", fmt.Errorf("failed to search protos: %w", err)
	}

	exactMatch, matches := findProtoTitleMatches(protos, input)

	if exactMatch != nil {
		return exactMatch.ID, nil
	}

	if len(matches) == 0 {
		return "", fmt.Errorf("no proto found matching %q (by ID or title)", input)
	}

	if len(matches) == 1 {
		return matches[0].ID, nil
	}

	// Multiple matches - show them all for disambiguation
	var matchNames []string
	for _, m := range matches {
		matchNames = append(matchNames, fmt.Sprintf("%s: %s", m.ID, m.Title))
	}
	return "", fmt.Errorf("ambiguous: %q matches %d protos:\n  %s\nUse the ID or a more specific title", input, len(matches), strings.Join(matchNames, "\n  "))
}

func resolveProtoID(ctx context.Context, s molReader, input string) (string, bool) {
	protoID, err := utils.ResolvePartialID(ctx, s, input)
	if err != nil {
		return "", false
	}

	issue, err := s.GetIssue(ctx, protoID)
	if err != nil || issue == nil {
		return "", false
	}
	labels, _ := s.GetLabels(ctx, protoID)
	for _, label := range labels {
		if label == BeadsTemplateLabel {
			return protoID, true
		}
	}
	return "", false
}

func findProtoTitleMatches(protos []*types.Issue, input string) (*types.Issue, []*types.Issue) {
	var matches []*types.Issue
	for _, proto := range protos {
		if strings.EqualFold(proto.Title, input) {
			return proto, nil
		}
		if strings.Contains(strings.ToLower(proto.Title), strings.ToLower(input)) {
			matches = append(matches, proto)
		}
	}
	return nil, matches
}
