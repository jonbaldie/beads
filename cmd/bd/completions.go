package main

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/spf13/cobra"
)

// issueIDCompletion provides shell completion for issue IDs by querying the storage
// and returning a list of IDs with their titles as descriptions
func issueIDCompletion(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Initialize storage if not already initialized
	ctx := context.Background()
	if getRootContext() != nil {
		ctx = getRootContext()
	}

	// Get database path - use same logic as in PersistentPreRun
	currentDBPath := getDBPath()
	if currentDBPath == "" {
		currentDBPath = beads.FindDatabasePath()
		if currentDBPath == "" {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
	}

	// Open database if store is not initialized
	currentStore := getStore()
	if currentStore == nil {
		var err error
		currentStore, err = openReadOnlyStoreForDBPath(ctx, currentDBPath)
		if err != nil {
			// If we can't open database, return empty completion
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		defer func() { _ = currentStore.Close() }()
	}

	// Use SearchIssues with IDPrefix filter to efficiently query matching issues
	filter := types.IssueFilter{
		IssueFilterCore: types.IssueFilterCore{
			IDPrefix: toComplete,
		},
		// Filter at database level for better performance,
	}
	issues, err := currentStore.SearchIssues(ctx, "", filter)
	if err != nil {
		// If we can't list issues, return empty completion
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	// Build completion list
	completions := make([]string, 0, len(issues))
	for _, issue := range issues {
		// Format: ID\tTitle (shown during completion)
		completions = append(completions, fmt.Sprintf("%s\t%s", issue.ID, issue.Title))
	}

	return completions, cobra.ShellCompDirectiveNoFileComp
}
