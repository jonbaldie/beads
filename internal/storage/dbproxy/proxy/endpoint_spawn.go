package proxy

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/fdhygiene"
	"github.com/jonbaldie/beads/internal/procid"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/util"
)

func spawnAndHandoff(
	rootDir string,
	opts OpenOpts,
	deadline time.Time,
	stopEpoch string,
	lock *util.Lock,
	discovery adoptionResult,
) (Endpoint, error) {
	handedOff := false
	defer func() {
		if !handedOff {
			lock.Unlock()
		}
	}()

	if err := prepareProxySpawn(rootDir, stopEpoch, discovery); err != nil {
		return Endpoint{}, err
	}
	handedOff = true
	child, err := forkExecChild(rootDir, opts, opts.Port, stopEpoch, lock)
	if err != nil {
		return Endpoint{}, fmt.Errorf("fork child: %w", err)
	}
	defer func() { _ = clearOwnSpawnMarker(rootDir, child.marker) }()
	defer func() {
		if child.handle != nil {
			_ = child.handle.Close()
		}
	}()

	return waitForSpawnedProxy(rootDir, opts, deadline, stopEpoch, child)
}

// describeSpawnPort renders a requested spawn port for wait/timeout
// messages: 0 is the default OS-assigned path, not a literal "port 0".
func describeSpawnPort(port int) string {
	if port == 0 {
		return "its OS-assigned port"
	}
	return fmt.Sprintf("port %d", port)
}

type spawnedProxyChild struct {
	cmd    *exec.Cmd
	done   <-chan error
	handle *procid.Handle
	marker spawnMarker
}

func appendExternalProxyChildArgs(args []string, ext configfile.ExternalDoltConfig) []string {
	if ext.Host != "" {
		args = append(args, "--external-host", ext.Host)
	}
	if ext.Port != 0 {
		args = append(args, "--external-port", strconv.Itoa(ext.Port))
	}
	if ext.Socket != "" {
		args = append(args, "--external-socket-path", ext.Socket)
	}
	if ext.KeepAlivePeriod != 0 {
		args = append(args, "--external-keep-alive", ext.KeepAlivePeriod.String())
	}
	return args
}

func buildProxyChildArgs(rootDir string, opts OpenOpts, port int, stopEpoch string) []string {
	idleTimeout := opts.IdleTimeout
	if idleTimeout < 0 {
		idleTimeout = IdleTimeoutNever
	}
	args := []string{
		"db-proxy-child",
		"--root", rootDir,
		"--port", strconv.Itoa(port),
		"--idle-timeout", idleTimeout.String(),
		"--backend", string(opts.Backend),
		"--stop-epoch", stopEpoch,
	}
	if opts.ConfigFilePath != "" {
		args = append(args, "--config", opts.ConfigFilePath)
	}
	if opts.LogFilePath != "" {
		args = append(args, "--logpath", opts.LogFilePath)
	}
	if opts.DoltBinPath != "" {
		args = append(args, "--dolt-bin", opts.DoltBinPath)
	}
	if opts.Database != "" {
		args = append(args, "--database", opts.Database)
	}
	if opts.Backend == BackendExternal {
		args = appendExternalProxyChildArgs(args, opts.External)
	}
	return args
}

func prepareProxyChild(rootDir string, opts OpenOpts, port int, stopEpoch string) (*exec.Cmd, *os.File, spawnMarker, error) {
	self, err := ResolveExecutable()
	if err != nil {
		return nil, nil, spawnMarker{}, fmt.Errorf("locate bd executable: %w", err)
	}
	args := buildProxyChildArgs(rootDir, opts, port, stopEpoch)

	logFile, err := os.OpenFile(opts.LogFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: logFilePath is caller-derived (workspace path), not user-request input
	if err != nil {
		return nil, nil, spawnMarker{}, fmt.Errorf("open log file %q: %w", opts.LogFilePath, err)
	}

	cmd := exec.Command(self, args...)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = procAttrDetached()

	birth, err := procid.Capture(os.Getpid())
	if err != nil {
		_ = logFile.Close()
		return nil, nil, spawnMarker{}, fmt.Errorf("capture spawning process identity: %w", err)
	}
	marker := spawnMarker{
		Schema:      1,
		PID:         os.Getpid(),
		Birth:       string(birth),
		StopEpoch:   stopEpoch,
		StartedUnix: time.Now().Unix(),
	}
	if err := writeSpawnMarker(rootDir, marker); err != nil {
		_ = logFile.Close()
		return nil, nil, spawnMarker{}, err
	}
	return cmd, logFile, marker, nil
}

func captureSpawnedProxyHandle(pid int) *procid.Handle {
	var handle *procid.Handle
	childBirth, captureErr := procid.Capture(pid)
	if captureErr == nil {
		handle, captureErr = procid.Open(pid, childBirth)
	}
	if captureErr != nil && !procid.IsProcessGone(captureErr) {
		log.Printf("dbproxy: could not open verified handle for newly spawned proxy pid %d: %v", pid, captureErr)
	}
	return handle
}

func forkExecChild(rootDir string, opts OpenOpts, port int, stopEpoch string, lock *util.Lock) (*spawnedProxyChild, error) {
	released := false
	defer func() {
		if !released {
			lock.Unlock()
		}
	}()

	cmd, logFile, marker, err := prepareProxyChild(rootDir, opts, port, stopEpoch)
	if err != nil {
		return nil, err
	}

	// The marker is durable before proxy.lock is released. Shutdown treats a
	// matching live owner as an in-progress start and waits; the child removes
	// it only after acquiring proxy.lock. This closes the release-before-Start
	// window without making the child deadlock on the parent's flock.
	released = true
	lock.Unlock()
	beforeProxyChildStart()

	// GH#4634: same hazard as the direct sql-server spawn, one hop further
	// out. The proxy child is detached and long-lived, and it starts the
	// sql-server itself, so a caller's non-CLOEXEC descriptor would otherwise
	// be inherited twice over and pinned for the proxy's whole lifetime.
	if leaked := fdhygiene.MarkInheritedCloexec(); len(leaked) > 0 {
		// debug.Logf, not log.Printf: this fires in normal operation whenever
		// the caller's environment leaves any fd open, and the parent's stderr
		// may be parsed script output.
		debug.Logf("dbproxy: marked %d inherited fd(s) close-on-exec before starting proxy child: %v", len(leaked), leaked)
	}

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		_ = clearOwnSpawnMarker(rootDir, marker)
		return nil, fmt.Errorf("start proxy child: %w", err)
	}

	done := make(chan error, 1)
	go func() {
		waitErr := cmd.Wait()
		_ = logFile.Close()
		done <- waitErr
		close(done)
	}()

	// Theoretical race: the birth token is captured after Start, so the child
	// could exit and its PID be recycled before Capture runs, making the
	// handle describe the unrelated replacement. The window is a few
	// milliseconds against OS PID-reuse latency, and the OS has no primitive
	// to atomically capture identity at spawn, so this is accepted and
	// documented rather than defended.
	return &spawnedProxyChild{
		cmd:    cmd,
		done:   done,
		handle: captureSpawnedProxyHandle(cmd.Process.Pid),
		marker: marker,
	}, nil
}
