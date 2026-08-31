package dolt

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/jonbaldie/beads/internal/doltserver"
)

func establishServerEndpoint(ctx context.Context, cfg *Config) (net.Conn, string, string, *circuitBreaker, error) {
	breaker, err := allowServerEndpoint(ctx, cfg)
	if err != nil {
		return nil, "", "", breaker, err
	}
	state := prepareServerEndpoint(cfg)
	conn, addr, dialErr := dialServerEndpoint(cfg, 500*time.Millisecond)
	if dialErr != nil {
		return recoverServerEndpoint(cfg, breaker, state, addr, dialErr)
	}
	return finishServerEndpoint(breaker, state, conn, addr, "")
}

type serverEndpointState struct {
	trackAutoStartedServer bool
	resolvedBeadsDir       string
	serverDir              string
}

func allowServerEndpoint(ctx context.Context, cfg *Config) (*circuitBreaker, error) {
	breaker := initializeServerCircuitBreaker(cfg)
	if breaker == nil || breaker.Allow() {
		return breaker, nil
	}
	doltMetrics.circuitRejected.Add(ctx, 1)
	return breaker, ErrCircuitOpen
}

func prepareServerEndpoint(cfg *Config) serverEndpointState {
	resolvedBeadsDir := cfg.BeadsDir
	if resolvedBeadsDir == "" {
		resolvedBeadsDir = filepath.Dir(cfg.Path)
	}
	cfg.ServerSocket = ResolveSocketTransport(cfg.ServerSocket, cfg.ServerHost, cfg.ServerPort, 500*time.Millisecond)
	return serverEndpointState{
		trackAutoStartedServer: !cfg.ReadOnly && shouldStopAutoStartedServerOnClose(cfg),
		resolvedBeadsDir:       resolvedBeadsDir,
		serverDir:              doltserver.ResolveServerDir(resolvedBeadsDir),
	}
}

func recoverServerEndpoint(
	cfg *Config,
	breaker *circuitBreaker,
	state serverEndpointState,
	addr string,
	dialErr error,
) (net.Conn, string, string, *circuitBreaker, error) {
	if !serverOpenCanAutoStart(cfg) {
		if breaker != nil {
			breaker.RecordFailure()
		}
		return nil, "", "", breaker, unreachableServerError(cfg, addr, dialErr)
	}
	conn, addr, autoStartedDir, breaker, err := autoStartServerEndpoint(
		cfg, breaker, state.trackAutoStartedServer, state.resolvedBeadsDir, state.serverDir, addr,
	)
	if err != nil {
		return nil, "", "", breaker, err
	}
	return finishServerEndpoint(breaker, state, conn, addr, autoStartedDir)
}

func finishServerEndpoint(
	breaker *circuitBreaker,
	state serverEndpointState,
	conn net.Conn,
	addr, autoStartedDir string,
) (net.Conn, string, string, *circuitBreaker, error) {
	// Drain the MySQL handshake before closing so Close() sends FIN, not RST
	// (dolt sql-server crash risk otherwise, gastownhall/beads#4132, #4133).
	// This single close site covers both the initial successful dial above
	// and the post-auto-start retry dial in the branch just above.
	doltserver.DrainAndCloseProbe(conn)

	// If this process already owns a test-started auto-start server, later
	// stores sharing it must participate in the refcount so one Close() does
	// not stop the server out from under another open store.
	if autoStartedDir == "" && state.trackAutoStartedServer && autoStartAcquireExisting(state.serverDir) {
		autoStartedDir = state.serverDir
	}

	if breaker != nil {
		breaker.RecordSuccess()
	}
	return conn, addr, autoStartedDir, breaker, nil
}

func dialServerEndpoint(cfg *Config, timeout time.Duration) (net.Conn, string, error) {
	if cfg.ServerSocket != "" {
		return dialEndpoint("unix", cfg.ServerSocket, timeout)
	}
	addr := net.JoinHostPort(cfg.ServerHost, fmt.Sprintf("%d", cfg.ServerPort))
	return dialEndpoint("tcp", addr, timeout)
}

func dialEndpoint(network, addr string, timeout time.Duration) (net.Conn, string, error) {
	conn, err := net.DialTimeout(network, addr, timeout)
	return conn, addr, err
}

func autoStartServerEndpoint(
	cfg *Config,
	breaker *circuitBreaker,
	trackAutoStartedServer bool,
	resolvedBeadsDir, serverDir, previousAddr string,
) (net.Conn, string, string, *circuitBreaker, error) {
	// Snapshot the port file's exact pre-call state before letting
	// EnsureRunningDetailed write to it. Start() (and the adopt-existing-server
	// path) write serverDir's port file with the actual listening port inside
	// EnsureRunningDetailed. The fail-closed branches restore this snapshot
	// before returning so a retry re-triggers the same check.
	portFileSnap, snapErr := doltserver.SnapshotPortFile(serverDir)
	if snapErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not snapshot port file before auto-start: %v\n", snapErr)
	}
	port, startedByUs, startErr := ensureRunningDetailed(resolvedBeadsDir)
	if startErr != nil {
		return nil, "", "", breaker, fmt.Errorf("Dolt server unreachable at %s and auto-start failed: %w\n\n"+
			"To start manually: bd dolt start\n"+
			"To disable auto-start: set dolt.auto-start: false in .beads/config.yaml",
			previousAddr, startErr)
	}

	autoStartedDir := acquireAutoStartedServer(startedByUs, trackAutoStartedServer, serverDir)
	addr := previousAddr
	if port != cfg.ServerPort {
		if err := autoStartPortChangeError(cfg, port); err != nil {
			undoRejectedAutoStart(serverDir, startedByUs, autoStartedDir, portFileSnap, snapErr)
			if breaker != nil {
				breaker.RecordFailure()
			}
			return nil, "", "", breaker, err
		}
		warnAutoStartPortChange(cfg.ServerPort, port)
		cfg.ServerPort = port
		addr = net.JoinHostPort(cfg.ServerHost, fmt.Sprintf("%d", cfg.ServerPort))
		breaker = maybeNewCircuitBreaker(cfg.ServerHost, cfg.ServerPort, cfg.Database)
	}

	conn, _, dialErr := dialServerEndpoint(cfg, 2*time.Second)
	if dialErr != nil {
		if autoStartedDir != "" {
			_ = autoStartRelease(autoStartedDir)
		}
		if breaker != nil {
			breaker.RecordFailure()
		}
		return nil, "", "", breaker, fmt.Errorf("Dolt server auto-started but still unreachable at %s: %w\n\n"+
			"Check logs: %s", addr, dialErr, doltserver.LogPath(resolvedBeadsDir))
	}
	return conn, addr, autoStartedDir, breaker, nil
}

func acquireAutoStartedServer(startedByUs, trackAutoStartedServer bool, serverDir string) string {
	if !startedByUs || !trackAutoStartedServer {
		return ""
	}
	autoStartAcquire(serverDir)
	return serverDir
}

func autoStartPortChangeError(cfg *Config, port int) error {
	if cfg.ServerPort <= 0 {
		return nil
	}
	if cfg.ServerPortSharedServer {
		return fmt.Errorf(
			"Shared Dolt server configured at port %d (source: %s) is unreachable; "+
				"auto-start started a repo-local server on port %d instead, but bd will "+
				"not silently write to it\n\n"+
				"A repo-local server is a different database than the shared one, so "+
				"using port %d here would silently write to the wrong database.\n\n"+
				"To proceed:\n"+
				"  - Restart the shared Dolt server: bd dolt start\n"+
				"  - Or check why it stopped responding on port %d before retrying",
			cfg.ServerPort, cfg.ServerPortSource, port, port, cfg.ServerPort)
	}
	if cfg.ServerPortSource.IsAuthoritative() {
		return fmt.Errorf(
			"Dolt server configured at port %d (source: %s) is unreachable; "+
				"auto-start started a new server on port %d, but bd will not "+
				"silently use a different port than the one you configured\n\n"+
				"The configured port may be pointing at a shared-server host "+
				"serving a different project's database; using port %d instead "+
				"could silently write to the wrong database.\n\n"+
				"To proceed:\n"+
				"  - Start the configured server manually: bd dolt start\n"+
				"  - Or remove/change the pinned port (env var, .beads/config.yaml "+
				"dolt.port, or global config) if port %d is stale",
			cfg.ServerPort, cfg.ServerPortSource, port, port, cfg.ServerPort)
	}
	return nil
}

func warnAutoStartPortChange(previousPort, port int) {
	fmt.Fprintf(os.Stderr, "Warning: Dolt server endpoint changed: port %d → %d (auto-start)\n", previousPort, port)
	fmt.Fprintf(os.Stderr, "  Previous port was unreachable. If other tools expect port %d, they may see stale data.\n", previousPort)
	fmt.Fprintf(os.Stderr, "  To pin a port: set dolt.port in .beads/config.yaml\n")
}

func unreachableServerError(cfg *Config, addr string, dialErr error) error {
	return fmt.Errorf("Dolt server unreachable at %s: %w\n\n%s", addr, dialErr, unreachableServerHint(cfg))
}

func unreachableServerHint(cfg *Config) string {
	if cfg.ServerSocket != "" {
		return fmt.Sprintf("The Dolt server is not listening on socket %s.\n"+
			"Ensure the server is started with --socket:\n"+
			"  dolt sql-server --socket %s\n"+
			"Auto-start is not supported in socket mode.",
			cfg.ServerSocket, cfg.ServerSocket)
	}
	if isExternalServerHost(cfg.ServerHost) {
		// External (non-localhost) server: bd does not manage it; "bd dolt start"
		// would be wrong advice (GH#3518). Suggest verifying the external server.
		return fmt.Sprintf("Configured Dolt server at %s:%d is unreachable.\n"+
			"Verify the external server is running and reachable from this host:\n"+
			"  nc -zv %s %d  # or curl %s:%d for an HTTP-style check",
			cfg.ServerHost, cfg.ServerPort,
			cfg.ServerHost, cfg.ServerPort,
			cfg.ServerHost, cfg.ServerPort)
	}
	if !cfg.AutoStart && doltserver.IsAutoStartDisabled() {
		return "Dolt server auto-start is disabled (dolt.auto-start: false).\n" +
			"Start the server manually:\n  bd dolt start"
	}
	return "The Dolt server may not be running. Try:\n  bd dolt start"
}
