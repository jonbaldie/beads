package dolt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/jonbaldie/beads/internal/gittraceenv"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/doltutil"
)

// hasMatchingCLIRemote reports whether the local CLI directory contains the
// same remote URL that SQL reports. CLI push/pull/fetch run from CLIDir, so
// SQL visibility alone is not enough to route safely.
func (s *DoltStore) hasMatchingCLIRemote(remote, expectedURL string) bool {
	if expectedURL == "" {
		return false
	}
	cliDir := s.CLIDir()
	if cliDir == "" {
		return false
	}
	if !s.hasCLIDatabase() {
		return false
	}
	return doltutil.RemoteURLsMatch(doltutil.FindCLIRemote(cliDir, remote), expectedURL)
}

// hasCLIDatabase reports whether CLIDir points at an initialized Dolt database.
// SQL-capable routes use this as a CLI availability check and fall back to SQL
// when an external-server client has only a placeholder local directory.
func (s *DoltStore) hasCLIDatabase() bool {
	cliDir := s.CLIDir()
	if cliDir == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(cliDir, ".dolt"))
	return err == nil && info.IsDir()
}

// ensureMatchingCLIRemote materializes the local CLI remote needed before
// subprocess push/pull/fetch routing. SQL remains the source of truth; the CLI
// remote is only the local transport surface that dolt subprocesses read.
func (s *DoltStore) ensureMatchingCLIRemote(remote, expectedURL string) error {
	if s.hasMatchingCLIRemote(remote, expectedURL) {
		return nil
	}
	cliDir := s.CLIDir()
	if expectedURL == "" {
		return fmt.Errorf("remote %q has an empty SQL URL", remote)
	}
	if cliDir == "" {
		return fmt.Errorf("remote %q (%s) requires CLI routing but no CLI directory is configured", remote, expectedURL)
	}
	if err := doltutil.EnsureCLIRemote(cliDir, remote, expectedURL); err != nil {
		return fmt.Errorf("materialize CLI remote %q (%s) in %s: %w", remote, expectedURL, cliDir, err)
	}
	if !s.hasMatchingCLIRemote(remote, expectedURL) {
		return fmt.Errorf("materialized CLI remote %q in %s, but its URL does not match SQL URL %q", remote, cliDir, expectedURL)
	}
	return nil
}

func (s *DoltStore) prepareDoltCLITransfer(ctx context.Context, remote string, creds *remoteCredentials, args ...string) (*exec.Cmd, context.Context, context.CancelFunc) {
	return prepareDoltCLITransferCommand(ctx, s.CLIDir(), creds, s.isS3Remote(ctx, remote), args...)
}

func prepareDoltCLITransferCommand(ctx context.Context, cliDir string, creds *remoteCredentials, s3Remote bool, args ...string) (*exec.Cmd, context.Context, context.CancelFunc) {
	ctx, cancel := withCLIExecTimeout(ctx)
	cmd := exec.CommandContext(ctx, "dolt", args...) // #nosec G204 -- fixed command with validated remote/ref args
	// CommandContext kills only the direct dolt child on expiry; a grandchild
	// (e.g. a cloud credential helper) holding the inherited output pipes
	// would otherwise keep Wait/CombinedOutput blocked forever after the kill.
	cmd.WaitDelay = cliExecWaitDelay
	cmd.Dir = cliDir
	creds.applyToCmd(cmd)
	if s3Remote {
		applyS3ChecksumEnvToCmd(cmd)
	}
	// Stderr-directed git tracing corrupts the transfer (see internal/gittraceenv);
	// mirrors withRemoteEnvGuards on the in-process path.
	base := cmd.Env
	if base == nil {
		base = os.Environ()
	}
	cmd.Env = gittraceenv.ScrubEnv(base)
	return cmd, ctx, cancel
}

// prepareCLIRouteForGitProtocol reports whether the SQL-visible remote uses
// git wire protocol and prepares the matching local CLI remote before routing.
func (s *DoltStore) prepareCLIRouteForGitProtocol(ctx context.Context, remote string) (bool, error) {
	if s.CLIDir() == "" || !s.hasCLIDatabase() {
		return false, nil
	}
	remotes, err := s.ListRemotes(ctx)
	if err != nil {
		return false, fmt.Errorf("list Dolt remotes before git-protocol routing: %w", err)
	}
	if found, useCLI, err := s.prepareVisibleCLIRoute(remote, remotes); found || err != nil {
		return useCLI, err
	}
	return s.preparePersistedCLIRoute(remote)
}

func (s *DoltStore) prepareVisibleCLIRoute(remote string, remotes []storage.RemoteInfo) (bool, bool, error) {
	for _, r := range remotes {
		if r.Name != remote {
			continue
		}
		if !doltutil.IsGitProtocolURL(r.URL) {
			return true, false, nil
		}
		if err := s.ensureMatchingCLIRemote(remote, r.URL); err != nil {
			return true, false, fmt.Errorf("remote %q uses git protocol and requires CLI routing: %w", remote, err)
		}
		return true, true, nil
	}
	return false, false, nil
}

func (s *DoltStore) preparePersistedCLIRoute(remote string) (bool, error) {
	// Not visible in dolt_remotes — but that is not proof it is absent: a
	// freshly (auto-)started sql-server can report an empty dolt_remotes
	// while the remote is persisted on disk (GH#2118, wy-6k7f7). Recover the
	// persisted truth: a git-protocol remote routes over the CLI anyway, so
	// the push can proceed; a non-git remote would need the SQL route, which
	// the cold server would refuse with a bare "remote not found" — fail
	// with the cold-start explanation instead.
	for _, r := range s.PersistedRemoteInfos() {
		if r.Name != remote {
			continue
		}
		if !doltutil.IsGitProtocolURL(r.URL) {
			return false, fmt.Errorf("remote %q (%s) is persisted on disk but not yet visible to this sql-server (GH#2118 cold start); retry shortly, or restart the dolt sql-server if it persists", remote, r.URL)
		}
		if err := s.ensureMatchingCLIRemote(remote, r.URL); err != nil {
			return false, fmt.Errorf("remote %q uses git protocol and requires CLI routing: %w", remote, err)
		}
		return true, nil
	}
	return false, nil
}

// mainRemoteCredentials returns credentials for the main remote, or nil if none.
func (s *DoltStore) mainRemoteCredentials() *remoteCredentials {
	if s.remoteUser == "" && s.remotePassword == "" {
		return nil
	}
	return &remoteCredentials{username: s.remoteUser, password: s.remotePassword}
}

// credentialsForRemote returns credentials only when the target remote is the
// default remote (s.remote). Non-default remotes get nil creds to avoid sending
// the wrong credentials to the wrong host.
func (s *DoltStore) credentialsForRemote(remote string) *remoteCredentials {
	if remote == s.remote {
		return s.mainRemoteCredentials()
	}
	return nil
}
