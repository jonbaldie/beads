package dolt

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// doltCLIPush shells out to `dolt push` from the database directory.
// Used for git-protocol remotes where CALL DOLT_PUSH times out through the SQL connection.
// If creds is non-nil, credentials are set on the subprocess environment only,
// avoiding process-wide env var races with concurrent goroutines.
func (s *DoltStore) doltCLIPush(ctx context.Context, remote string, force bool, creds *remoteCredentials) error {
	if err := s.prePushFSCK(ctx); err != nil {
		return err
	}
	args := []string{"push"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, remote, s.branch)
	cmd, transferCtx, cancel := s.prepareDoltCLITransfer(ctx, remote, creds, args...)
	defer cancel()
	applyNoGitHooksToCmd(cmd) // GH#3724
	out, err := cmd.CombinedOutput()
	if err != nil {
		return cliTransferError("dolt push", remote, transferCtx, out, err)
	}
	return nil
}

// cliTransferError wraps a failed CLI transfer, distinguishing a transfer that
// hit the bounded timeout (actionable: raise BEADS_CLI_TRANSFER_TIMEOUT, or
// check what holds the database directory busy) from an ordinary failure.
func cliTransferError(op, remote string, transferCtx context.Context, out []byte, err error) error {
	if errors.Is(transferCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s to %q timed out after %s (override with %s=<duration>; large transfers to cloud remotes can run long, and a busy dolt sql-server serving the database directory can stall CLI transfers): %s: %w",
			op, remote, cliExecTimeoutDuration(), cliExecTimeoutEnv, strings.TrimSpace(string(out)), err)
	}
	return fmt.Errorf("%s failed: %s: %w", op, strings.TrimSpace(string(out)), err)
}

// doltCLIPull shells out to `dolt pull` from the database directory.
// Used for git-protocol remotes where CALL DOLT_PULL times out through the SQL connection.
// If creds is non-nil, credentials are set on the subprocess environment only.
func (s *DoltStore) doltCLIPull(ctx context.Context, remote string, creds *remoteCredentials) error {
	cmd, transferCtx, cancel := s.prepareDoltCLITransfer(ctx, remote, creds, "pull", remote, s.branch)
	defer cancel()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return cliTransferError("dolt pull", remote, transferCtx, out, err)
	}
	return nil
}
