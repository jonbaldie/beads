package fix

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/types"
)

// StaleClosedIssues deletes stale closed issues.
// This is the fix handler for the "Stale Closed Issues" doctor check.
//
// This fix is DISABLED by default (stale_closed_issues_days=0). Users must
// explicitly set a positive threshold in metadata.json to enable cleanup.
func StaleClosedIssues(path string) error {
	beadsDir, thresholdDays, err := staleClosedCleanupConfig(path)
	if err != nil {
		return err
	}
	if thresholdDays == 0 {
		fmt.Println("  Stale closed issues cleanup disabled (set stale_closed_issues_days to enable)")
		return nil
	}
	ctx := context.Background()
	store, err := openBeadMutatingStore(ctx, beadsDir)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer func() { _ = store.Close() }()
	issues, err := searchStaleClosedIssues(ctx, store, thresholdDays)
	if err != nil {
		return err
	}
	deleted, skipped := deleteUnpinnedIssues(ctx, store, issues)
	reportStaleClosedCleanup(deleted, skipped, thresholdDays)
	return nil
}

func staleClosedCleanupConfig(path string) (beadsDir string, thresholdDays int, err error) {
	beadsDir, err = resolvedWorkspaceBeadsDir(path)
	if err != nil {
		return "", 0, err
	}
	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		return "", 0, fmt.Errorf("failed to load config: %w", err)
	}
	if cfg != nil {
		thresholdDays = cfg.GetStaleClosedIssuesDays()
	}
	return beadsDir, thresholdDays, nil
}

type issueMutator interface {
	SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error)
	DeleteIssue(ctx context.Context, id string) error
}

func searchStaleClosedIssues(ctx context.Context, store issueMutator, thresholdDays int) ([]*types.Issue, error) {
	cutoff := time.Now().AddDate(0, 0, -thresholdDays)
	statusClosed := types.StatusClosed
	issues, err := store.SearchIssues(ctx, "", types.IssueFilter{
		IssueFilterCore: types.IssueFilterCore{
			Status: &statusClosed,
		},
		IssueFilterMatch: types.IssueFilterMatch{
			ClosedBefore: &cutoff,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query issues: %w", err)
	}
	return issues, nil
}

func deleteUnpinnedIssues(ctx context.Context, store issueMutator, issues []*types.Issue) (deleted, skipped int) {
	for _, issue := range issues {
		if issue.Pinned {
			skipped++
			continue
		}
		if err := store.DeleteIssue(ctx, issue.ID); err != nil {
			fmt.Printf("  Warning: failed to delete %s: %v\n", issue.ID, err)
			continue
		}
		deleted++
	}
	return deleted, skipped
}

func reportStaleClosedCleanup(deleted, skipped, thresholdDays int) {
	if deleted == 0 && skipped == 0 {
		fmt.Println("  No stale closed issues to clean up")
		return
	}
	if deleted > 0 {
		fmt.Printf("  Cleaned up %d stale closed issue(s) (older than %d days)\n", deleted, thresholdDays)
	}
	if skipped > 0 {
		fmt.Printf("  Skipped %d pinned issue(s)\n", skipped)
	}
}

// PatrolPollution deletes patrol digest and session ended beads that pollute the database.
// This is the fix handler for the "Patrol Pollution" doctor check.
//
// It removes beads matching:
// - Patrol digests: titles matching "Digest: mol-*-patrol"
// - Session ended beads: titles matching "Session ended: *"
//
// After deletion, cleans up any orphaned data.
func PatrolPollution(path string) error {
	beadsDir, err := resolvedWorkspaceBeadsDir(path)
	if err != nil {
		return err
	}
	ctx := context.Background()
	store, err := openBeadMutatingStore(ctx, beadsDir)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer func() { _ = store.Close() }()
	issues, err := searchNonEphemeralIssues(ctx, store)
	if err != nil {
		return err
	}
	patrolDigestCount, sessionBeadCount, toDelete := collectPatrolPollution(issues)
	if len(toDelete) == 0 {
		fmt.Println("  No patrol pollution beads to delete")
		return nil
	}
	deleted := deleteIssueIDs(ctx, store, toDelete)
	reportPatrolPollution(patrolDigestCount, sessionBeadCount, deleted)
	return nil
}

func searchNonEphemeralIssues(ctx context.Context, store issueMutator) ([]*types.Issue, error) {
	ephemeral := false
	issues, err := store.SearchIssues(ctx, "", types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{Ephemeral: &ephemeral}})
	if err != nil {
		return nil, fmt.Errorf("failed to query issues: %w", err)
	}
	return issues, nil
}

func isPatrolPollutionTitle(title string) (patrolDigest, sessionEnded bool) {
	if strings.HasPrefix(title, "Digest: mol-") && strings.HasSuffix(title, "-patrol") {
		return true, false
	}
	return false, strings.HasPrefix(title, "Session ended:")
}

func collectPatrolPollution(issues []*types.Issue) (patrolDigestCount, sessionBeadCount int, toDelete []string) {
	for _, issue := range issues {
		patrolDigest, sessionEnded := isPatrolPollutionTitle(issue.Title)
		switch {
		case patrolDigest:
			patrolDigestCount++
			toDelete = append(toDelete, issue.ID)
		case sessionEnded:
			sessionBeadCount++
			toDelete = append(toDelete, issue.ID)
		}
	}
	return patrolDigestCount, sessionBeadCount, toDelete
}

func deleteIssueIDs(ctx context.Context, store issueMutator, ids []string) int {
	var deleted int
	for _, id := range ids {
		if err := store.DeleteIssue(ctx, id); err != nil {
			fmt.Printf("  Warning: failed to delete %s: %v\n", id, err)
			continue
		}
		deleted++
	}
	return deleted
}

func reportPatrolPollution(patrolDigestCount, sessionBeadCount, deleted int) {
	if patrolDigestCount > 0 {
		fmt.Printf("  Deleted %d patrol digest bead(s)\n", patrolDigestCount)
	}
	if sessionBeadCount > 0 {
		fmt.Printf("  Deleted %d session ended bead(s)\n", sessionBeadCount)
	}
	fmt.Printf("  Total: %d pollution bead(s) removed\n", deleted)
}
