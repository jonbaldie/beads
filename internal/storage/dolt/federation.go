package dolt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/doltutil"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

// federationStagingBranchPrefix identifies temporary filtered-push branches.
// Each operation appends a UUID so concurrent pushes cannot collide.
const federationStagingBranchPrefix = "__federation_push_staging_"

// federationStagingCleanupTimeout bounds best-effort cleanup after the caller
// has canceled or its deadline has expired.
const federationStagingCleanupTimeout = 30 * time.Second

// FederatedStorage implementation for DoltStore
// These methods enable peer-to-peer synchronization between workspaces.

// PushTo pushes commits to a specific peer remote.
// If credentials are stored for this peer, they are used automatically.
// For git-protocol remotes, uses CLI `dolt push` to avoid MySQL connection timeouts.
func (s *DoltStore) PushTo(ctx context.Context, peer string) error {
	return s.pushRefToPeer(ctx, peer, s.branch)
}

// pushRefToPeer pushes a specific refspec to a peer remote. The refspec can be
// a simple branch name ("main") or a mapping ("staging:main").
func (s *DoltStore) pushRefToPeer(ctx context.Context, peer string, refspec string) error {
	if useCLI, err := s.prepareCLIRouteForPeerGitProtocol(ctx, peer); err != nil {
		return err
	} else if useCLI {
		return s.withPeerCredentials(ctx, peer, func(creds *remoteCredentials) error {
			return s.doltCLIPushRefToPeer(ctx, peer, refspec, creds)
		})
	}
	return s.withPeerCredentials(ctx, peer, func(creds *remoteCredentials) error {
		if useCLI, err := s.prepareCLIRouteForPeerCredentials(ctx, peer, creds); err != nil {
			return err
		} else if useCLI {
			return s.doltCLIPushRefToPeer(ctx, peer, refspec, creds)
		}
		return withEnvCredentials(creds, func() error {
			if err := s.execWithLongTimeout(ctx, "CALL DOLT_PUSH(?, ?)", peer, refspec); err != nil {
				return fmt.Errorf("failed to push to peer %s: %w", peer, err)
			}
			return nil
		})
	})
}

// PullFrom pulls changes from a specific peer remote.
// If credentials are stored for this peer, they are used automatically.
// For git-protocol remotes, uses CLI `dolt pull` to avoid MySQL connection timeouts.
// Returns any merge conflicts if present.
func (s *DoltStore) PullFrom(ctx context.Context, peer string) ([]storage.Conflict, error) {
	var conflicts []storage.Conflict
	err := s.withCircuitWrite(ctx, func(ctx context.Context) error {
		var err error
		conflicts, err = s.pullFromPeer(ctx, peer)
		return err
	})
	return conflicts, err
}

func (s *DoltStore) pullFromPeer(ctx context.Context, peer string) ([]storage.Conflict, error) {
	if err := s.commitPendingPeerPull(ctx); err != nil {
		return nil, err
	}
	preHead := s.prePeerPullHead(ctx)
	var conflicts []storage.Conflict
	err := s.executePeerPull(ctx, peer, &conflicts)
	return s.finishPeerPull(ctx, conflicts, err, preHead)
}

func (s *DoltStore) commitPendingPeerPull(ctx context.Context) error {
	if s.readOnly {
		return nil
	}
	if err := s.commitBeforePull(ctx, "auto-commit before pull"); err != nil && !isDoltNothingToCommit(err) {
		return fmt.Errorf("failed to commit pending changes before pull: %w", err)
	}
	return nil
}

func (s *DoltStore) prePeerPullHead(ctx context.Context) string {
	if s.readOnly {
		return ""
	}
	head, err := s.GetCurrentCommit(ctx)
	if err != nil {
		return ""
	}
	return head
}

func (s *DoltStore) executePeerPull(ctx context.Context, peer string, conflicts *[]storage.Conflict) error {
	useCLI, err := s.prepareCLIRouteForPeerGitProtocol(ctx, peer)
	if err != nil {
		return err
	}
	if useCLI {
		return s.executePeerCLIPull(ctx, peer, conflicts)
	}
	return s.executePeerCredentialPull(ctx, peer, conflicts)
}

func (s *DoltStore) executePeerCLIPull(ctx context.Context, peer string, conflicts *[]storage.Conflict) error {
	return s.withPeerCredentials(ctx, peer, func(creds *remoteCredentials) error {
		pullErr := s.finishCLIPull(ctx, s.doltCLIPullFromPeer(ctx, peer, creds))
		return s.peerPullOutcome(ctx, peer, pullErr, conflicts)
	})
}

func (s *DoltStore) executePeerCredentialPull(ctx context.Context, peer string, conflicts *[]storage.Conflict) error {
	return s.withPeerCredentials(ctx, peer, func(creds *remoteCredentials) error {
		return s.executePeerCredentialRoute(ctx, peer, creds, conflicts)
	})
}

func (s *DoltStore) executePeerCredentialRoute(ctx context.Context, peer string, creds *remoteCredentials, conflicts *[]storage.Conflict) error {
	useCLI, err := s.prepareCLIRouteForPeerCredentials(ctx, peer, creds)
	if err != nil {
		return err
	}
	if useCLI {
		pullErr := s.finishCLIPull(ctx, s.doltCLIPullFromPeer(ctx, peer, creds))
		return s.peerPullOutcome(ctx, peer, pullErr, conflicts)
	}
	return withEnvCredentials(creds, func() error {
		pullErr := s.pullWithAutoResolve(ctx, peer, "CALL DOLT_PULL(?)", peer)
		return s.peerPullOutcome(ctx, peer, pullErr, conflicts)
	})
}

// peerPullOutcome converts a settled peer pull's result into PullFrom's
// contract: conflicts the settle machinery could not auto-resolve are returned
// as data for the caller, anything else stays an error. The SQL route rolls
// the conflicted merge back before returning, so its conflicts arrive only via
// MergeConflictsError, captured pre-rollback (bd-578h9.15); the CLI route's
// subprocess writes conflicts to the on-disk working set where GetConflicts
// still sees them.
func (s *DoltStore) peerPullOutcome(ctx context.Context, peer string, pullErr error, conflicts *[]storage.Conflict) error {
	if pullErr == nil {
		return nil
	}
	var mce *versioncontrolops.MergeConflictsError
	if errors.As(pullErr, &mce) {
		*conflicts = mce.Conflicts
		return nil
	}
	if c, conflictErr := s.GetConflicts(ctx); conflictErr == nil && len(c) > 0 {
		*conflicts = c
		return nil
	}
	return fmt.Errorf("failed to pull from peer %s: %w", peer, pullErr)
}

// finishPeerPull runs the post-merge is_blocked recompute (bd-6dnrw.3) after a
// successful, conflict-free peer pull and passes the pull result through
// otherwise. Conflicted pulls skip the recompute: the caller resolves the
// conflicts first, and the next sync picks the rows up.
func (s *DoltStore) finishPeerPull(ctx context.Context, conflicts []storage.Conflict, pullErr error, preHead string) ([]storage.Conflict, error) {
	if pullErr != nil || len(conflicts) > 0 || s.readOnly {
		return conflicts, pullErr
	}
	if err := s.recomputeBlockedAfterPull(ctx, preHead); err != nil {
		return conflicts, fmt.Errorf("pull succeeded but is_blocked recompute failed: %w", err)
	}
	return conflicts, nil
}

// Fetch fetches refs from a peer without merging.
// If credentials are stored for this peer, they are used automatically.
// For git-protocol remotes, uses CLI `dolt fetch` to avoid MySQL connection timeouts.
func (s *DoltStore) Fetch(ctx context.Context, peer string) error {
	if useCLI, err := s.prepareCLIRouteForPeerGitProtocol(ctx, peer); err != nil {
		return err
	} else if useCLI {
		return s.withPeerCredentials(ctx, peer, func(creds *remoteCredentials) error {
			return s.doltCLIFetchFromPeer(ctx, peer, creds)
		})
	}
	return s.withPeerCredentials(ctx, peer, func(creds *remoteCredentials) error {
		// Credential CLI routing: route fetch through CLI subprocess.
		if useCLI, err := s.prepareCLIRouteForPeerCredentials(ctx, peer, creds); err != nil {
			return err
		} else if useCLI {
			return s.doltCLIFetchFromPeer(ctx, peer, creds)
		}
		return withEnvCredentials(creds, func() error {
			if err := s.execWithLongTimeout(ctx, "CALL DOLT_FETCH(?)", peer); err != nil {
				return fmt.Errorf("failed to fetch from peer %s: %w", peer, err)
			}
			return nil
		})
	})
}

// ListRemotes returns configured remote names and URLs.
func (s *DoltStore) ListRemotes(ctx context.Context) ([]storage.RemoteInfo, error) {
	return versioncontrolops.ListRemotes(ctx, s.db)
}

// hasPersistedCLIRemote reports whether a Dolt remote is persisted on disk in
// .dolt/repo_state.json — in the database CLI directory (CLIDir) or the dolt
// server root (Path, per GH#2118). A freshly (auto-)started sql-server can
// report an empty dolt_remotes table at store open even though remotes are
// persisted on disk. The #4259 remote-migrate gate therefore consults this
// directly so a cold-start open cannot miss the remote and migrate the shared
// database in place.
//
// The probe reads repo_state.json itself (no dolt CLI subprocess), so a
// missing dolt binary can no longer disable the gate. A directory that is not
// a dolt repository is a definite "no remote here"; a read/parse failure still
// fails open (migration is not wedged on unrelated corruption) but is logged,
// never swallowed (bd-6dnrw.33).
func (s *DoltStore) hasPersistedCLIRemote() bool {
	return s.HasPersistedRemote()
}

// HasPersistedRemote is the exported on-disk probe for callers that must not
// trust an empty dolt_remotes table at cold start: the remote-migrate gate
// and the push/pull "no remote configured" exit-0 skip (bd-578h9.10).
func (s *DoltStore) HasPersistedRemote() bool {
	return len(s.PersistedRemoteInfos()) > 0
}

// PersistedRemoteInfos returns the remotes persisted on disk in
// .dolt/repo_state.json — names AND urls — searching the same directories in
// the same order as HasPersistedRemote: the database CLI directory first,
// then the dolt server root (GH#2118). The first directory that yields any
// remotes wins, mirroring HasPersistedRemote's first-hit semantics.
//
// Callers in the GH#2118 cold-start window use this to RECOVER the invisible
// remote rather than merely detect it: a freshly (auto-)started sql-server
// can report an empty dolt_remotes even though the remote is persisted on
// disk, so an empty listing is a reporting artifact, not an unconfigured rig
// (wy-6k7f7). Read/parse failures are logged and skipped, matching the old
// HasPersistedRemote behavior — callers only consult this after a SUCCESSFUL
// (empty) ListRemotes, so a read failure here never masquerades as evidence.
func (s *DoltStore) PersistedRemoteInfos() []storage.RemoteInfo {
	cliDir := s.CLIDir()
	dirs := []string{cliDir}
	if s.dbPath != "" && s.dbPath != cliDir {
		dirs = append(dirs, s.dbPath)
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		remotes, err := doltutil.PersistedRemotes(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"Warning: remote-migrate gate could not inspect %s for persisted remotes (assuming none): %v\n",
				dir, err)
			continue
		}
		if len(remotes) > 0 {
			return remotes
		}
	}
	return nil
}

// RemoveRemote removes a configured remote.
func (s *DoltStore) RemoveRemote(ctx context.Context, name string) error {
	return versioncontrolops.RemoveRemote(ctx, s.db, name)
}

// SyncStatus returns the sync status with a peer.
func (s *DoltStore) SyncStatus(ctx context.Context, peer string) (*storage.SyncStatus, error) {
	status := &storage.SyncStatus{
		Peer: peer,
	}

	// Get ahead/behind counts by comparing refs.
	// This requires the peer to have been fetched first.
	// Dolt's AS OF requires a literal ref: bind parameters (even inside CONCAT)
	// fail server-side with `unbound variable "v1" in query`, so validate the
	// ref and interpolate it (same pattern as embeddeddolt SyncStatus).
	remoteRef := peer + "/" + s.branch
	if err := issueops.ValidateRef(remoteRef); err != nil {
		status.LocalAhead = -1
		status.LocalBehind = -1
	} else {
		//nolint:gosec // G201: remoteRef is validated by issueops.ValidateRef above — AS OF requires a literal
		query := fmt.Sprintf(`
			SELECT
				(SELECT COUNT(*) FROM dolt_log WHERE commit_hash NOT IN
					(SELECT commit_hash FROM dolt_log AS OF '%s')) as ahead,
				(SELECT COUNT(*) FROM dolt_log AS OF '%s' WHERE commit_hash NOT IN
					(SELECT commit_hash FROM dolt_log)) as behind
		`, remoteRef, remoteRef)
		if err := s.db.QueryRowContext(ctx, query).
			Scan(&status.LocalAhead, &status.LocalBehind); err != nil {
			// If we can't get the status, return a partial result.
			// This happens when the remote branch doesn't exist locally yet.
			status.LocalAhead = -1
			status.LocalBehind = -1
		}
	}

	// Check for conflicts
	conflicts, err := s.GetConflicts(ctx)
	if err == nil && len(conflicts) > 0 {
		status.HasConflicts = true
	}

	// Get last sync time from metadata
	status.LastSync = s.getLastSyncTime(ctx, peer)

	return status, nil
}

// getLastSyncTime retrieves the last sync time for a peer from metadata.
func (s *DoltStore) getLastSyncTime(ctx context.Context, peer string) time.Time {
	key := "last_sync_" + peer
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE `key` = ?", key).Scan(&value)
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return t
}

// setLastSyncTime records the last sync time for a peer in metadata.
func (s *DoltStore) setLastSyncTime(ctx context.Context, peer string) error {
	key := "last_sync_" + peer
	value := time.Now().Format(time.RFC3339)
	_, err := s.execContext(ctx,
		"REPLACE INTO metadata (`key`, value) VALUES (?, ?)", key, value)
	return wrapExecError("set last sync time", err)
}
