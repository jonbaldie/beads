package main

import (
	"context"

	"github.com/jonbaldie/beads/internal/storage"
)

// withMutationIssue resolves one issue for a write and closes any routed store
// before returning. The mutation callback only has to implement the command's
// operation, so lookup and not-found behavior stay identical across shorthands.
func withMutationIssue(ctx context.Context, localStore storage.DoltStorage, id string, mutate func(*RoutedResult) error) error {
	result, err := resolveAndGetIssueForMutation(ctx, localStore, id)
	if err != nil {
		if result != nil {
			result.Close()
		}
		return HandleErrorRespectJSON("resolving %s: %v", id, err)
	}
	if result == nil || result.Issue == nil {
		if result != nil {
			result.Close()
		}
		return HandleErrorRespectJSON("issue %s not found", id)
	}
	defer result.Close()
	return mutate(result)
}
