package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// cloneSubgraph creates new issues from the template with variable substitution.
// Uses CloneOptions to control all spawn/bond behavior including dynamic bonding.
func cloneSubgraph(ctx context.Context, s storage.DoltStorage, subgraph *TemplateSubgraph, opts CloneOptions) (*InstantiateResult, error) {
	if s == nil {
		return nil, fmt.Errorf("no database connection")
	}

	var result *InstantiateResult
	err := transact(ctx, s, "bd: clone template subgraph", func(tx storage.Transaction) error {
		r, err := cloneSubgraphInto(ctx, storeMolWriter{DoltStorage: s, tx: tx}, subgraph, opts)
		if err != nil {
			return err
		}
		result = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func cloneSubgraphInto(ctx context.Context, w molWriter, subgraph *TemplateSubgraph, opts CloneOptions) (*InstantiateResult, error) {
	if err := flattenUnregisteredIssueTypes(ctx, w, subgraph.Issues, subgraph.Dependencies); err != nil {
		return nil, fmt.Errorf("checking custom types for subgraph: %w", err)
	}

	idMapping, err := cloneSubgraphIssues(ctx, w, subgraph, opts)
	if err != nil {
		return nil, err
	}

	if err := cloneSubgraphDependencies(ctx, w, subgraph, opts, idMapping); err != nil {
		return nil, err
	}

	if err := attachClonedSubgraph(ctx, w, opts, idMapping[subgraph.Root.ID]); err != nil {
		return nil, err
	}

	return &InstantiateResult{
		NewEpicID: idMapping[subgraph.Root.ID],
		IDMapping: idMapping,
		Created:   len(idMapping),
	}, nil
}

func cloneSubgraphIssues(ctx context.Context, w molWriter, subgraph *TemplateSubgraph, opts CloneOptions) (map[string]string, error) {
	idMapping := make(map[string]string)
	for _, oldIssue := range subgraph.Issues {
		if opts.RootOnly && oldIssue.ID != subgraph.Root.ID {
			continue
		}

		newIssue, err := newClonedIssue(oldIssue, subgraph.Root.ID, opts)
		if err != nil {
			return nil, err
		}
		if err := w.CreateIssue(ctx, newIssue, opts.Actor); err != nil {
			return nil, fmt.Errorf("failed to create issue from %s: %w", oldIssue.ID, err)
		}
		idMapping[oldIssue.ID] = newIssue.ID
	}
	return idMapping, nil
}

func newClonedIssue(oldIssue *types.Issue, rootID string, opts CloneOptions) (*types.Issue, error) {
	issueAssignee := oldIssue.Assignee
	if oldIssue.ID == rootID && opts.Assignee != "" {
		issueAssignee = opts.Assignee
	}

	newIssue := &types.Issue{
		IssueContent: types.IssueContent{
			// ID will be set below based on bonding options
			Title:              substituteVariables(oldIssue.Title, opts.Vars),
			Description:        substituteVariables(oldIssue.Description, opts.Vars),
			Design:             substituteVariables(oldIssue.Design, opts.Vars),
			AcceptanceCriteria: substituteVariables(oldIssue.AcceptanceCriteria, opts.Vars),
			Notes:              substituteVariables(oldIssue.Notes, opts.Vars),
		},
		IssueWorkflow: types.IssueWorkflow{
			Status: types.StatusOpen,
			// Always start fresh
			Priority:         oldIssue.Priority,
			IssueType:        oldIssue.IssueType,
			Assignee:         issueAssignee,
			EstimatedMinutes: oldIssue.EstimatedMinutes,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
		IssueMeta: types.IssueMeta{
			Metadata: substituteMetadataRepo(oldIssue.Metadata, oldIssue.AwaitType, opts.Vars),
		},
		IssueGraph: types.IssueGraph{
			// mark for cleanup when closed
			IDPrefix: opts.Prefix,
			Labels:   oldIssue.Labels,
		},
		IssueWisp: types.IssueWisp{
			Ephemeral: opts.Ephemeral,
		},
		IssueCoord: types.IssueCoord{
			// distinct prefixes for mols/wisps
			// Gate fields (for async coordination)
			AwaitType: oldIssue.AwaitType,
			AwaitID:   substituteVariables(oldIssue.AwaitID, opts.Vars),
			Timeout:   oldIssue.Timeout,
		},
	}

	if opts.ParentID == "" {
		return newIssue, nil
	}

	bondedID, err := generateBondedID(oldIssue.ID, rootID, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to generate bonded ID for %s: %w", oldIssue.ID, err)
	}
	newIssue.ID = bondedID
	return newIssue, nil
}

func cloneSubgraphDependencies(ctx context.Context, w molWriter, subgraph *TemplateSubgraph, opts CloneOptions, idMapping map[string]string) error {
	for _, dep := range subgraph.Dependencies {
		newFromID, ok := idMapping[dep.IssueID]
		if !ok {
			continue // Skip if either end is outside the subgraph
		}
		newToID, ok := idMapping[dep.DependsOnID]
		if !ok {
			continue // Skip if either end is outside the subgraph
		}

		newDep := &types.Dependency{
			IssueID:     newFromID,
			DependsOnID: newToID,
			Type:        dep.Type,
			Metadata:    dep.Metadata,
		}
		if err := w.AddDependency(ctx, newDep, opts.Actor); err != nil {
			return fmt.Errorf("failed to create dependency: %w", err)
		}
	}
	return nil
}

func attachClonedSubgraph(ctx context.Context, w molWriter, opts CloneOptions, rootID string) error {
	if opts.AttachToID == "" {
		return nil
	}

	// Atomic attachment: link spawned root to target molecule within
	// the same transaction (bd-wvplu: prevents orphaned spawns)
	attachDep := &types.Dependency{
		IssueID:     rootID,
		DependsOnID: opts.AttachToID,
		Type:        opts.AttachDepType,
	}
	if err := w.AddDependency(ctx, attachDep, opts.Actor); err != nil {
		return fmt.Errorf("attaching to molecule: %w", err)
	}
	return nil
}

// printTemplateTree prints the template structure as a tree.
// Uses a visited set to detect cycles (GH#2719) and avoid infinite recursion.
func printTemplateTree(subgraph *TemplateSubgraph, parentID string, depth int, isRoot bool) {
	visited := make(map[string]bool)
	printTemplateTreeVisited(subgraph, parentID, depth, isRoot, visited)
}

// printTemplateTreeVisited is the internal recursive implementation with cycle tracking.
func printTemplateTreeVisited(subgraph *TemplateSubgraph, parentID string, depth int, isRoot bool, visited map[string]bool) {
	indent := strings.Repeat("  ", depth)

	// Print root
	if isRoot {
		fmt.Printf("%s   %s (root)\n", indent, subgraph.Root.Title)
		visited[parentID] = true
	}
	printTemplateTreeChildren(subgraph, parentID, depth, indent, visited)
}

func printTemplateTreeChildren(subgraph *TemplateSubgraph, parentID string, depth int, indent string, visited map[string]bool) {
	children := templateTreeChildren(subgraph, parentID)
	for i, child := range children {
		printTemplateTreeChild(subgraph, child, i == len(children)-1, depth, indent, visited)
	}
}

func templateTreeChildren(subgraph *TemplateSubgraph, parentID string) []*types.Issue {
	var children []*types.Issue
	for _, dep := range subgraph.Dependencies {
		if dep.DependsOnID == parentID && dep.Type == types.DepParentChild {
			if child, ok := subgraph.IssueMap[dep.IssueID]; ok {
				children = append(children, child)
			}
		}
	}
	return children
}

func printTemplateTreeChild(subgraph *TemplateSubgraph, child *types.Issue, isLast bool, depth int, indent string, visited map[string]bool) {
	connector := "├──"
	if isLast {
		connector = "└──"
	}
	vars := extractVariables(child.Title)
	varStr := ""
	if len(vars) > 0 {
		varStr = fmt.Sprintf(" [%s]", strings.Join(vars, ", "))
	}

	// Cycle detection (GH#2719)
	if visited[child.ID] {
		fmt.Printf("%s   %s %s%s (cycle detected, skipping)\n", indent, connector, child.Title, varStr)
		return
	}
	fmt.Printf("%s   %s %s%s\n", indent, connector, child.Title, varStr)
	visited[child.ID] = true
	printTemplateTreeVisited(subgraph, child.ID, depth+1, false, visited)
}
