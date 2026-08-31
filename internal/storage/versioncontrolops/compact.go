package versioncontrolops

import (
	"context"
	"fmt"
)

// Compact squashes old Dolt commits into a single base commit while preserving
// recent commits via cherry-pick. The recipe:
//  1. Create temp branch at the boundary commit (last old commit)
//  2. Checkout temp branch
//  3. Soft-reset to initial commit (collapses old history into working set)
//  4. Commit as single squashed base
//  5. Cherry-pick each recent commit on top
//  6. Checkout main, hard-reset to temp branch
//  7. Delete temp branch
//
// Callers should run PruneRemoteRefs and then DoltGC afterward to reclaim disk
// space — remote-tracking refs still anchor the pre-compact chain, and GC
// alone reclaims nothing while they exist (bd-agctw).
//
// conn must be a single database connection (not a pooled *sql.DB) since the
// stored procedures rely on session-scoped state (current branch, working set).
func Compact(ctx context.Context, conn DBConn, initialHash, boundaryHash string, oldCommits int, recentHashes []string) (retErr error) {
	branchCreated := false

	// Best-effort cleanup: if any step fails after creating the temp branch,
	// try to return to main and delete the temp branch so future compactions
	// aren't blocked by a leftover branch.
	defer func() {
		if retErr != nil && branchCreated {
			_, _ = conn.ExecContext(ctx, "CALL DOLT_CHECKOUT('main')")
			_, _ = conn.ExecContext(ctx, "CALL DOLT_BRANCH('-D', 'compact-tmp')")
		}
	}()

	if retErr = compactCreateBase(ctx, conn, initialHash, boundaryHash, oldCommits); retErr != nil {
		return retErr
	}
	branchCreated = true
	if retErr = compactReplayRecent(ctx, conn, recentHashes); retErr != nil {
		return retErr
	}
	if retErr = compactFinalize(ctx, conn); retErr != nil {
		return retErr
	}
	return nil
}

func compactStep(ctx context.Context, conn DBConn, name, query string, args ...interface{}) error {
	if _, err := conn.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("compact step %q: %w", name, err)
	}
	return nil
}

func compactCreateBase(ctx context.Context, conn DBConn, initialHash, boundaryHash string, oldCommits int) error {
	if err := compactStep(ctx, conn, "create temp branch", "CALL DOLT_BRANCH('compact-tmp', ?)", boundaryHash); err != nil {
		return err
	}
	if err := compactStep(ctx, conn, "checkout temp", "CALL DOLT_CHECKOUT('compact-tmp')"); err != nil {
		return err
	}
	if err := compactStep(ctx, conn, "soft reset to initial", "CALL DOLT_RESET('--soft', ?)", initialHash); err != nil {
		return err
	}
	msg := fmt.Sprintf("compact: squash %d commits into base snapshot", oldCommits)
	return compactStep(ctx, conn, "commit squashed base", "CALL DOLT_COMMIT('-Am', ?)", msg)
}

func compactReplayRecent(ctx context.Context, conn DBConn, recentHashes []string) error {
	// --allow-empty: the preserved window can contain empty commits (a Dolt
	// auto-commit with no table change, or a bd create double-commit whose
	// leading member has 0 changed tables). Without this flag DOLT_CHERRY_PICK
	// aborts the entire replay at the first empty commit with Error 1105
	// ("The previous cherry-pick commit is empty. Use --allow-empty ..."),
	// leaving compaction permanently blocked on active databases. See #3815.
	for _, hash := range recentHashes {
		if err := compactStep(ctx, conn, fmt.Sprintf("cherry-pick %s", hash[:min(8, len(hash))]), "CALL DOLT_CHERRY_PICK('--allow-empty', ?)", hash); err != nil {
			return err
		}
	}
	return nil
}

func compactFinalize(ctx context.Context, conn DBConn) error {
	if err := compactStep(ctx, conn, "checkout main", "CALL DOLT_CHECKOUT('main')"); err != nil {
		return err
	}
	if err := compactStep(ctx, conn, "reset main to compacted", "CALL DOLT_RESET('--hard', 'compact-tmp')"); err != nil {
		return err
	}
	return compactStep(ctx, conn, "delete temp branch", "CALL DOLT_BRANCH('-D', 'compact-tmp')")
}
