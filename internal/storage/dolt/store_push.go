package dolt

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Push pushes commits to the remote.
// For git-protocol remotes (SSH, git+https://, git://), uses CLI `dolt push` to avoid MySQL connection timeouts.
// For non-SSH Hosted Dolt (remoteUser set), uses CALL DOLT_PUSH with --user authentication.
// For other remotes (DoltHub, S3, GCS, file), uses CALL DOLT_PUSH via SQL.
func (s *DoltStore) Push(ctx context.Context) (retErr error) {
	return s.pushToRemote(ctx, s.remote, false)
}

// ForcePush force-pushes commits to the remote, overwriting remote changes.
// Use when the remote has uncommitted changes in its working set.
// For git-protocol remotes (SSH, git+https://, git://), uses CLI `dolt push --force` to avoid MySQL connection timeouts.
func (s *DoltStore) ForcePush(ctx context.Context) (retErr error) {
	return s.pushToRemote(ctx, s.remote, true)
}

// PushRemote pushes commits to a named remote. Unlike Push(), which always
// uses the configured default remote (s.remote), PushRemote targets an
// explicit remote name. Credentials are only applied when the target remote
// matches the default remote; otherwise nil creds are used.
func (s *DoltStore) PushRemote(ctx context.Context, remote string, force bool) error {
	return s.pushToRemote(ctx, remote, force)
}

// pushToRemote is the internal implementation for all push operations.
// It routes through CLI or SQL based on the remote's protocol and credentials.
func (s *DoltStore) pushToRemote(ctx context.Context, remote string, force bool) (retErr error) {
	spanName := "dolt.push"
	if force {
		spanName = "dolt.force_push"
	}
	ctx, span := doltTracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append(s.doltSpanAttrs(),
			attribute.String("dolt.remote", remote),
			attribute.String("dolt.branch", s.branch),
		)...),
	)
	defer func() { endSpan(span, retErr) }()
	creds := s.credentialsForRemote(remote)
	if handled, err := s.tryCLIPushRoutes(ctx, remote, force, creds); handled {
		return err
	}
	return s.pushSQLRemote(ctx, remote, force, creds)
}

func (s *DoltStore) tryCLIPushRoutes(ctx context.Context, remote string, force bool, creds *remoteCredentials) (bool, error) {
	// Git-protocol remotes: use CLI to avoid MySQL connection timeout during transfer.
	// Must check before remoteUser — Hosted Dolt SSH remotes have remoteUser set
	// but still need CLI to avoid SQL connection timeout.
	// Credentials are passed directly to the subprocess via cmd.Env, avoiding
	// process-wide env var races with concurrent goroutines.
	if useCLI, err := s.prepareCLIRouteForGitProtocol(ctx, remote); err != nil {
		return true, err
	} else if useCLI {
		return true, s.doltCLIPush(ctx, remote, force, creds)
	}
	// Credential CLI routing: when credentials are set and server is external,
	// route through CLI subprocess so credentials reach the dolt process via
	// cmd.Env (applyToCmd). The SQL path's withEnvCredentials sets process-wide
	// env vars that an external server cannot see.
	if useCLI, err := s.prepareCLIRouteForCredentials(ctx, remote, creds); err != nil {
		return true, err
	} else if useCLI {
		return true, s.doltCLIPush(ctx, remote, force, creds)
	}
	// Cloud auth CLI routing: when cloud storage env vars (AZURE_*, AWS_*,
	// etc.) are set and we're in server mode, route through CLI so the dolt
	// subprocess inherits the current env. The SQL server may not have these
	// vars if it was started in a different context (GH#6).
	if useCLI, err := s.prepareCLIRouteForCloudAuth(ctx, remote); err != nil {
		return true, err
	} else if useCLI {
		return true, s.doltCLIPush(ctx, remote, force, creds)
	}
	if useCLI, err := s.shouldUseCLIForLocalRemoteWithError(ctx, remote); err != nil {
		return true, err
	} else if useCLI {
		return true, s.doltCLIPush(ctx, remote, force, creds)
	}
	return false, nil
}

func (s *DoltStore) pushSQLRemote(ctx context.Context, remote string, force bool, creds *remoteCredentials) error {
	if s.remoteUser != "" && remote == s.remote {
		return withRemoteOperationEnv(creds, s.isS3Remote(ctx, remote), func() error {
			return s.executeSQLPush(ctx, remote, force, s.remoteUser)
		})
	}
	return withRemoteOperationEnv(nil, s.isS3Remote(ctx, remote), func() error {
		return s.executeSQLPush(ctx, remote, force, "")
	})
}

func (s *DoltStore) executeSQLPush(ctx context.Context, remote string, force bool, user string) error {
	var query string
	var args []any
	if user != "" {
		if force {
			query = "CALL DOLT_PUSH('--force', '--user', ?, ?, ?)"
		} else {
			query = "CALL DOLT_PUSH('--user', ?, ?, ?)"
		}
		args = []any{user, remote, s.branch}
	} else if force {
		query = "CALL DOLT_PUSH('--force', ?, ?)"
		args = []any{remote, s.branch}
	} else {
		query = "CALL DOLT_PUSH(?, ?)"
		args = []any{remote, s.branch}
	}
	if err := s.execWithLongTimeoutNoTx(ctx, query, args...); err != nil {
		verb := "push"
		if force {
			verb = "force push"
		}
		return fmt.Errorf("failed to %s to %s/%s: %w", verb, remote, s.branch, err)
	}
	return nil
}
