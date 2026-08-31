package dolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/doltutil"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/storage/schema"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

// Sync performs a full bidirectional sync with a peer:
// 1. Fetch from peer
// 2. Merge peer's changes (handling conflicts per strategy)
// 3. Push local changes to peer
//
// Returns the sync result including any conflicts encountered.
func (s *DoltStore) Sync(ctx context.Context, peer string, strategy string) (*SyncResult, error) {
	result := &SyncResult{
		Peer:      peer,
		StartTime: time.Now(),
	}

	// GH#2474: match PullFrom — commit pending changes before the merge,
	// INCLUDING config (where kv.memory.* rows live). Plain Commit excludes
	// config (GH#2455), so federation metadata writes such as add-peer plus any
	// persistent memories would otherwise leave the working set dirty and wedge
	// DOLT_MERGE ("cannot merge with uncommitted changes").
	if err := s.commitPendingSyncChanges(ctx); err != nil {
		result.Error = err
		return result, result.Error
	}

	if err := s.mergeFederationPeer(ctx, peer, strategy, result); err != nil {
		result.Error = err
		return result, result.Error
	}

	s.pushFederationPeer(ctx, peer, result)

	// Record last sync time
	_ = s.setLastSyncTime(ctx, peer) // Best effort: sync timestamp is advisory for scheduling

	result.EndTime = time.Now()
	return result, nil
}

// commitPendingSyncChanges commits the working set before a federation merge.
// A clean working set is represented by Dolt's nothing-to-commit error and is
// deliberately treated as success.
func (s *DoltStore) commitPendingSyncChanges(ctx context.Context) error {
	if s.readOnly {
		return nil
	}
	if err := s.commitBeforePull(ctx, "auto-commit before sync"); err != nil && !isDoltNothingToCommit(err) {
		return fmt.Errorf("failed to commit pending changes before sync: %w", err)
	}
	return nil
}

// mergeFederationPeer fetches and merges a peer branch, resolving conflicts
// when a strategy was supplied. It updates the supplied result as each phase
// succeeds and returns the exact user-facing error for a failed phase.
func (s *DoltStore) mergeFederationPeer(ctx context.Context, peer, strategy string, result *SyncResult) error {
	if err := s.Fetch(ctx, peer); err != nil {
		return fmt.Errorf("fetch failed: %w", err)
	}
	result.Fetched = true

	// Best effort: an empty commit hash means diff accounting is unavailable.
	beforeCommit, _ := s.GetCurrentCommit(ctx)
	remoteBranch := fmt.Sprintf("%s/%s", peer, s.branch)
	conflicts, err := s.Merge(ctx, remoteBranch)
	if err != nil {
		return fmt.Errorf("merge failed: %w", err)
	}

	if err := s.resolveFederationConflicts(ctx, peer, strategy, beforeCommit, conflicts, result); err != nil {
		return err
	}
	result.Merged = true

	// Best effort: an empty commit hash means diff accounting is unavailable.
	afterCommit, _ := s.GetCurrentCommit(ctx)
	if beforeCommit != afterCommit {
		result.PulledCommits = 1 // Simplified - could count actual commits
	}
	return nil
}

func (s *DoltStore) resolveFederationConflicts(ctx context.Context, peer, strategy, beforeCommit string, conflicts []storage.Conflict, result *SyncResult) error {
	if len(conflicts) == 0 {
		return nil
	}
	result.Conflicts = conflicts
	if strategy == "" {
		// No strategy specified, leave conflicts for manual resolution.
		return fmt.Errorf("merge conflicts require resolution (use --strategy ours|theirs)")
	}
	for _, c := range conflicts {
		if err := s.ResolveConflicts(ctx, c.Field, strategy); err != nil {
			return fmt.Errorf("conflict resolution failed for %s: %w", c.Field, err)
		}
	}
	result.ConflictsResolved = true

	// Commit the resolution INCLUDING config: the operator chose this
	// strategy, and plain Commit excludes config (GH#2455). A config-only
	// conflict — routine now that kv.memory.* memories sync through config —
	// would otherwise resolve but never commit, leaving the merge
	// unconcluded and re-wedging the next sync.
	if err := s.CommitMergeResolution(ctx, fmt.Sprintf("Resolve conflicts from %s using %s strategy", peer, strategy)); err != nil {
		return fmt.Errorf("failed to commit conflict resolution: %w", err)
	}

	// bd-578h9.11: the conflicted merge skipped the automatic is_blocked
	// recompute (unresolved rows would have fed it garbage); now that the
	// resolution is committed, cover the whole merge+resolution window.
	if err := s.RecomputeBlockedAfterMerge(ctx, beforeCommit); err != nil {
		return fmt.Errorf("conflicts resolved but is_blocked recompute failed: %w", err)
	}
	return nil
}

// pushFederationPeer pushes local changes after a merge. Push failures are
// recorded on the result because a peer may intentionally reject pushes.
func (s *DoltStore) pushFederationPeer(ctx context.Context, peer string, result *SyncResult) {
	excludeTypes := config.GetFederationConfig().ExcludeTypes
	if err := s.filteredPushToPeer(ctx, peer, excludeTypes); err != nil {
		result.PushError = err
		return
	}
	result.Pushed = true
}

// filteredPushToPeer pushes to a peer after filtering out excluded issue types.
// When excludeTypes is empty, delegates directly to PushTo (no filtering).
//
// For non-empty excludeTypes, the method creates a temporary staging branch,
// deletes matching issues, commits the filtered state, and pushes the staging
// branch to the peer using a refspec. The staging branch is always cleaned up.
//
// The special type "wisp" matches issues with ephemeral=true in the committed
// issues table. Wisps normally live in dolt_ignore'd tables and are not pushed,
// so this acts as a defense-in-depth safety net.
func (s *DoltStore) filteredPushToPeer(ctx context.Context, peer string, excludeTypes []string) (retErr error) {
	if len(excludeTypes) == 0 {
		return s.PushTo(ctx, peer)
	}
	sourceBranch := s.branch
	operationID, err := uuid.NewRandom()
	if err != nil {
		return fmt.Errorf("federation filter: generate staging branch: %w", err)
	}
	stagingBranch := federationStagingBranchPrefix + operationID.String()

	// Pin a single long-timeout connection for the session-scoped staging
	// operation. RecomputeAllIsBlockedInTx can exceed the shared pool's
	// 10-second I/O deadline, and branch state is connection-scoped.
	db, err := s.openLongTimeoutConn()
	if err != nil {
		return fmt.Errorf("federation filter: open long-timeout connection: %w", err)
	}
	defer db.Close()
	db.SetMaxIdleConns(0)
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("federation filter: acquire long-timeout connection: %w", err)
	}
	defer conn.Close()

	// Arm cleanup before creating the branch: a lost create response can make
	// the branch exist even when the call fails, and canceling an in-flight
	// query makes go-sql-driver/mysql invalidate the operation connection, so
	// cleanup reopens a fresh one.
	defer func() {
		if cleanupErr := cleanupFilteredStaging(ctx, db, conn, sourceBranch, stagingBranch); cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("federation filter: cleanup: %w", cleanupErr))
		}
	}()

	if err := s.stageFilteredBranch(ctx, conn, sourceBranch, stagingBranch, excludeTypes); err != nil {
		return err
	}

	// Push staging branch to peer, mapped to the peer's expected branch name.
	refspec := stagingBranch + ":" + sourceBranch
	return s.pushRefToPeer(ctx, peer, refspec)
}

// stageFilteredBranch creates the UUID staging branch from sourceBranch on the
// pinned connection, deletes the excluded issue types on it, recomputes and
// commits is_blocked when any rows were removed, and restores sourceBranch so
// the caller can push the staging ref. Branch state is session-scoped, so every
// step stays on conn.
func (s *DoltStore) stageFilteredBranch(ctx context.Context, conn *sql.Conn, sourceBranch, stagingBranch string, excludeTypes []string) error {
	// Create staging branch from the current branch. Cleanup is already armed:
	// a lost response can make this call fail after Dolt created the branch.
	if _, err := conn.ExecContext(ctx, "CALL DOLT_BRANCH(?, ?)", stagingBranch, sourceBranch); err != nil {
		return fmt.Errorf("federation filter: create staging branch: %w", err)
	}

	// Checkout staging branch.
	if err := versioncontrolops.CheckoutBranch(ctx, conn, stagingBranch); err != nil {
		return fmt.Errorf("federation filter: checkout staging: %w", err)
	}

	if deleteExcludedIssues(ctx, conn, excludeTypes) {
		if err := s.commitFilteredStaging(ctx, conn); err != nil {
			return err
		}
	}

	// Restore original branch context before pushing.
	if err := versioncontrolops.CheckoutBranch(ctx, conn, sourceBranch); err != nil {
		return fmt.Errorf("federation filter: restore branch %s: %w", sourceBranch, err)
	}
	return nil
}

// deleteExcludedIssues removes issues matching excludeTypes from the committed
// issues table on conn's current (staging) branch and reports whether any rows
// were deleted. The special type "wisp" matches ephemeral rows. A DELETE error
// is intentionally ignored (deleted stays false for that type), preserving the
// pre-existing best-effort filtering behavior; the caller only recomputes and
// commits when something was actually removed.
func deleteExcludedIssues(ctx context.Context, conn *sql.Conn, excludeTypes []string) bool {
	deleted := false
	for _, excludeType := range excludeTypes {
		var result interface{ RowsAffected() (int64, error) }
		var execErr error
		if excludeType == "wisp" {
			result, execErr = conn.ExecContext(ctx, "DELETE FROM issues WHERE ephemeral = 1")
		} else {
			result, execErr = conn.ExecContext(ctx, "DELETE FROM issues WHERE issue_type = ?", excludeType)
		}
		if execErr == nil {
			if n, _ := result.RowsAffected(); n > 0 {
				deleted = true
			}
		}
	}
	return deleted
}

// cleanupFilteredStaging discards the operation connection (an in-flight cancel
// invalidates it), then restores sourceBranch and deletes stagingBranch on a
// fresh, uncanceled, bounded connection. Errors are joined; a non-nil result is
// wrapped by the caller with the "federation filter: cleanup" context.
func cleanupFilteredStaging(ctx context.Context, db *sql.DB, conn *sql.Conn, sourceBranch, stagingBranch string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), federationStagingCleanupTimeout)
	defer cancel()
	var cleanupErr error
	if err := conn.Close(); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("discard operation connection: %w", err))
	}
	cleanupConn, err := db.Conn(cleanupCtx)
	if err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("acquire cleanup connection: %w", err))
	} else {
		defer cleanupConn.Close()
		if err := schema.DrainCall(cleanupCtx, cleanupConn, "CALL DOLT_CHECKOUT(?)", sourceBranch); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("restore branch %s: %w", sourceBranch, err))
		}
		if err := deleteFederationStagingBranch(cleanupCtx, cleanupConn, stagingBranch); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete staging branch: %w", err))
		}
	}
	return cleanupErr
}

func deleteFederationStagingBranch(ctx context.Context, conn *sql.Conn, stagingBranch string) error {
	err := schema.DrainCall(ctx, conn, "CALL DOLT_BRANCH('-Df', ?)", stagingBranch)
	var mysqlErr *mysql.MySQLError
	// Dolt reports a missing branch as generic error 1105; match the message as
	// a case-insensitive substring (as RemoteRefUnavailableErr does) because
	// Dolt may append the branch/ref name to this class of error.
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1105 &&
		strings.Contains(strings.ToLower(mysqlErr.Message), "branch not found") {
		return nil
	}
	return err
}

// commitFilteredStaging repairs and commits the filtered graph on the staging
// branch selected by conn. Dolt branch state is session-scoped, so every step
// stays on that pinned connection.
func (s *DoltStore) commitFilteredStaging(ctx context.Context, conn *sql.Conn) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("federation filter: begin staged is_blocked recompute: %w", err)
	}
	if _, err := issueops.RecomputeAllIsBlockedInTx(ctx, tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("federation filter: recompute staged is_blocked: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("federation filter: commit staged is_blocked recompute: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', ?)",
		"federation: exclude private issue types"); err != nil {
		return fmt.Errorf("federation filter: commit filtered state: %w", err)
	}
	return nil
}

// prepareCLIRouteForPeerGitProtocol reports whether the SQL-visible peer
// remote uses git wire protocol and prepares the matching local CLI remote
// before routing.
func (s *DoltStore) prepareCLIRouteForPeerGitProtocol(ctx context.Context, peer string) (bool, error) {
	if s.CLIDir() == "" {
		return false, nil
	}
	if !s.hasCLIDatabase() {
		return false, nil
	}
	remotes, err := s.ListRemotes(ctx)
	if err != nil {
		return false, fmt.Errorf("list Dolt remotes before git-protocol routing for peer %q: %w", peer, err)
	}
	for _, r := range remotes {
		if r.Name == peer {
			if !doltutil.IsGitProtocolURL(r.URL) {
				return false, nil
			}
			if err := s.ensureMatchingCLIRemote(peer, r.URL); err != nil {
				return false, fmt.Errorf("peer remote %q uses git protocol and requires CLI routing: %w", peer, err)
			}
			return true, nil
		}
	}
	return false, nil
}

func (s *DoltStore) shouldUseCLIForPeerGitProtocol(ctx context.Context, peer string) (bool, error) {
	return s.prepareCLIRouteForPeerGitProtocol(ctx, peer)
}

// doltCLIPushRefToPeer shells out to `dolt push` with a specific refspec.
// The refspec can be a branch name or a "local:remote" mapping.
func (s *DoltStore) doltCLIPushRefToPeer(ctx context.Context, peer string, refspec string, creds *remoteCredentials) error {
	if err := s.prePushFSCK(ctx); err != nil {
		return err
	}
	cmd, transferCtx, cancel := s.prepareDoltCLITransfer(ctx, peer, creds, "push", peer, refspec)
	defer cancel()
	applyNoGitHooksToCmd(cmd) // GH#3724
	out, err := cmd.CombinedOutput()
	if err != nil {
		return cliTransferError(fmt.Sprintf("push to peer %s", peer), peer, transferCtx, out, err)
	}
	return nil
}

// doltCLIPullFromPeer shells out to `dolt pull` for a specific peer remote.
// Used for git-protocol remotes where CALL DOLT_PULL times out through the SQL connection.
// Credentials are set on the subprocess environment only via cmd.Env.
func (s *DoltStore) doltCLIPullFromPeer(ctx context.Context, peer string, creds *remoteCredentials) error {
	cmd, transferCtx, cancel := s.prepareDoltCLITransfer(ctx, peer, creds, "pull", peer, s.branch)
	defer cancel()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return cliTransferError(fmt.Sprintf("pull from peer %s", peer), peer, transferCtx, out, err)
	}
	return nil
}

// doltCLIFetchFromPeer shells out to `dolt fetch` for a specific peer remote.
// Used for git-protocol remotes where CALL DOLT_FETCH times out through the SQL connection.
// Credentials are set on the subprocess environment only via cmd.Env.
func (s *DoltStore) doltCLIFetchFromPeer(ctx context.Context, peer string, creds *remoteCredentials) error {
	cmd, transferCtx, cancel := s.prepareDoltCLITransfer(ctx, peer, creds, "fetch", peer)
	defer cancel()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return cliTransferError(fmt.Sprintf("fetch from peer %s", peer), peer, transferCtx, out, err)
	}
	return nil
}

// SyncResult is an alias for storage.SyncResult.
type SyncResult = storage.SyncResult
