package proxy

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/lockfile"
	"github.com/jonbaldie/beads/internal/procid"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/identity"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/pidfile"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/server"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/util"
)

type ErrUpstreamMismatch struct {
	RootDir string
	Want    string
	Have    string
}

func (e *ErrUpstreamMismatch) Error() string {
	return fmt.Sprintf("proxy at %s fronts upstream %s, not %s", e.RootDir, e.Have, e.Want)
}

func IsUpstreamMismatch(err error) bool {
	var m *ErrUpstreamMismatch
	return errors.As(err, &m)
}

func intendedUpstreamID(opts OpenOpts) string {
	if opts.Backend == BackendExternal {
		return server.ExternalDoltServerID(opts.External)
	}
	return ""
}

type Endpoint struct {
	Host string
	Port int
}

func (e Endpoint) Address() string {
	return net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
}

type OpenOpts struct {
	IdleTimeout    time.Duration
	Backend        Backend
	ConfigFilePath string
	LogFilePath    string
	DoltBinPath    string
	Database       string
	Port           int
	External       configfile.ExternalDoltConfig
}

const (
	openDeadline          = 15 * time.Second
	spawnReadyHardTimeout = 2 * time.Minute
	openPollInterval      = 100 * time.Millisecond
	identityProbeTimeout  = 500 * time.Millisecond
	backendExitTimeout    = 5 * time.Second
	// quarantineRetention bounds forensic pidfile accumulation; successful
	// spawns sweep only records older than 30 days.
	quarantineRetention = 30 * 24 * time.Hour

	spawnMarkerFileName = "proxy.spawn"
	stopEpochFileName   = "proxy.stop-epoch"
)

var ResolveExecutable = os.Executable

var (
	verifyProcessIdentity = procid.Verify
	resolveRootIdentity   = identity.RootID
	readControlSecret     = identity.ReadSecret
)

// ErrUnverifiableProcess marks lifecycle operations that refuse to signal a
// process because its workspace-scoped identity cannot be verified. Callers
// may use errors.Is to offer a narrower recovery path without treating
// unrelated shutdown failures as identity failures.
var ErrUnverifiableProcess = errors.New("proxy process identity is unverifiable")

type unverifiableLifecycleError struct {
	message string
}

func (e *unverifiableLifecycleError) Error() string {
	return e.message
}

func (e *unverifiableLifecycleError) Unwrap() error {
	return ErrUnverifiableProcess
}

var stopEpochSequence atomic.Uint64

// beforeProxyChildStart is a deterministic test hook for the otherwise tiny
// release-lock-before-exec scheduling window. Production leaves it as a no-op.
var beforeProxyChildStart = func() {}

type adoptionStatus uint8

const (
	adoptionAdopted adoptionStatus = iota
	adoptionNoRecord
	adoptionStaleDead
	adoptionIdentityMismatch
	adoptionUnverifiable
	adoptionLegacy
	adoptionMalformed
	adoptionIOErr
)

func (s adoptionStatus) String() string {
	switch s {
	case adoptionAdopted:
		return "adopted"
	case adoptionNoRecord:
		return "no record"
	case adoptionStaleDead:
		return "stale dead"
	case adoptionIdentityMismatch:
		return "identity mismatch"
	case adoptionUnverifiable:
		return "identity unverifiable"
	case adoptionLegacy:
		return "legacy"
	case adoptionMalformed:
		return "malformed"
	case adoptionIOErr:
		return "I/O error"
	default:
		return fmt.Sprintf("unknown adoption status %d", s)
	}
}

type adoptionResult struct {
	status   adoptionStatus
	endpoint Endpoint
	pidfile  *pidfile.PidFile
	err      error
}

type spawnMarker struct {
	Schema      int    `json:"schema"`
	PID         int    `json:"pid"`
	Birth       string `json:"birth"`
	StopEpoch   string `json:"stop_epoch"`
	StartedUnix int64  `json:"started_unix"`
}

var errStartInterrupted = errors.New("proxy startup interrupted by concurrent shutdown")

func PickFreePort() (int, error) {
	// The managed proxy no longer uses this bind-close allocator: its child
	// binds port 0 and publishes the kernel-assigned port. The remaining
	// production caller allocates the Dolt config port; that race requires
	// the managed-config ownership/retry contract deferred to the PR-C RFC.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}

func validateProxyOpenOpts(opts OpenOpts) error {
	if err := opts.Backend.Validate(); err != nil {
		return fmt.Errorf("OpenOpts.Backend: %w", err)
	}
	if opts.Port != 0 && (opts.Port < 1 || opts.Port > 65535) {
		return fmt.Errorf("OpenOpts.Port: must be 0 or 1-65535, got %d", opts.Port)
	}
	return validateProxyBackendOpts(opts)
}

func validateProxyBackendOpts(opts OpenOpts) error {
	switch opts.Backend {
	case BackendLocalServer:
		if opts.ConfigFilePath == "" {
			return fmt.Errorf("OpenOpts.ConfigFilePath is required for backend %q", opts.Backend)
		}
		if opts.LogFilePath == "" {
			return fmt.Errorf("OpenOpts.LogFilePath is required for backend %q", opts.Backend)
		}
		if opts.DoltBinPath == "" {
			return fmt.Errorf("OpenOpts.DoltBinPath is required for backend %q", opts.Backend)
		}
	case BackendExternal:
		if opts.LogFilePath == "" {
			return fmt.Errorf("OpenOpts.LogFilePath is required for backend %q", opts.Backend)
		}
		if err := opts.External.Validate(); err != nil {
			return fmt.Errorf("OpenOpts.External: %w", err)
		}
	}
	return nil
}

type proxyOpenAttempt struct {
	endpoint     Endpoint
	adopted      bool
	markerActive bool
	spawnErr     error
}

func proxyDiscoveryError(rootDir string, discovery adoptionResult, context string) error {
	return fmt.Errorf("discover proxy from %s%s: %w", pidfile.Path(rootDir, PIDFileName), context, discovery.err)
}

func legacyProxyLockError(rootDir string) error {
	recordPath := pidfile.Path(rootDir, PIDFileName)
	return fmt.Errorf(
		"legacy proxy record %s is protected by held lock %s; stop the pre-upgrade proxy with the old bd binary or wait for its idle exit, then quarantine the record manually by renaming %s to %s.stale-<unix-timestamp> before retrying",
		recordPath,
		filepath.Join(rootDir, LockFileName),
		recordPath,
		recordPath,
	)
}

func tryProxyOpen(rootDir string, opts OpenOpts, deadline time.Time, stopEpoch, want string) (proxyOpenAttempt, error) {
	discovery := readAndDial(rootDir)
	switch discovery.status {
	case adoptionAdopted:
		endpoint, err := adoptedEndpoint(rootDir, want, discovery)
		if err != nil {
			return proxyOpenAttempt{}, err
		}
		return proxyOpenAttempt{endpoint: endpoint, adopted: true}, nil
	case adoptionIOErr:
		return proxyOpenAttempt{}, proxyDiscoveryError(rootDir, discovery, "")
	}

	lock, err := util.TryLock(filepath.Join(rootDir, LockFileName))
	if err != nil {
		if !lockfile.IsLocked(err) {
			return proxyOpenAttempt{}, fmt.Errorf("probe proxy lock: %w", err)
		}
		if discovery.status == adoptionLegacy {
			return proxyOpenAttempt{}, legacyProxyLockError(rootDir)
		}
		return proxyOpenAttempt{}, nil
	}

	return tryProxyOpenUnderLock(rootDir, opts, deadline, stopEpoch, want, lock)
}

func tryProxyOpenUnderLock(rootDir string, opts OpenOpts, deadline time.Time, stopEpoch, want string, lock *util.Lock) (proxyOpenAttempt, error) {
	discovery := readAndDial(rootDir)
	if discovery.status == adoptionAdopted {
		lock.Unlock()
		endpoint, err := adoptedEndpoint(rootDir, want, discovery)
		if err != nil {
			return proxyOpenAttempt{}, err
		}
		return proxyOpenAttempt{endpoint: endpoint, adopted: true}, nil
	}
	if discovery.status == adoptionIOErr {
		lock.Unlock()
		return proxyOpenAttempt{}, proxyDiscoveryError(rootDir, discovery, " under lock")
	}

	markerActive, err := inspectSpawnMarkerLocked(rootDir)
	if err != nil {
		lock.Unlock()
		return proxyOpenAttempt{}, err
	}
	if markerActive {
		lock.Unlock()
		return proxyOpenAttempt{markerActive: true}, nil
	}

	endpoint, spawnErr := spawnAndHandoff(rootDir, opts, deadline, stopEpoch, lock, discovery)
	return proxyOpenAttempt{endpoint: endpoint, adopted: spawnErr == nil, spawnErr: spawnErr}, nil
}

func updateLastProxySpawnError(attempt proxyOpenAttempt, lastSpawnErr *error) error {
	if attempt.spawnErr != nil {
		if errors.Is(attempt.spawnErr, errStartInterrupted) {
			return attempt.spawnErr
		}
		*lastSpawnErr = attempt.spawnErr
	}
	if attempt.markerActive {
		*lastSpawnErr = nil
	}
	return nil
}

func waitForProxyEndpoint(rootDir string, opts OpenOpts, deadline time.Time, stopEpoch, want string) (Endpoint, error) {
	timeout := time.NewTimer(openDeadline)
	defer timeout.Stop()
	poll := time.NewTicker(openPollInterval)
	defer poll.Stop()

	var lastSpawnErr error
	for {
		attempt, err := tryProxyOpen(rootDir, opts, deadline, stopEpoch, want)
		if err != nil {
			return Endpoint{}, err
		}
		if attempt.adopted {
			return attempt.endpoint, nil
		}
		if err := updateLastProxySpawnError(attempt, &lastSpawnErr); err != nil {
			return Endpoint{}, err
		}

		select {
		case <-timeout.C:
			if lastSpawnErr != nil {
				return Endpoint{}, lastSpawnErr
			}
			return Endpoint{}, fmt.Errorf("timeout waiting for proxy on %s", rootDir)
		case <-poll.C:
		}
	}
}

func GetCreateDatabaseProxyServerEndpoint(rootDir string, opts OpenOpts) (Endpoint, error) {
	if err := validateProxyOpenOpts(opts); err != nil {
		return Endpoint{}, err
	}
	stopEpoch, err := readStopEpoch(rootDir)
	if err != nil {
		return Endpoint{}, fmt.Errorf("read proxy stop epoch: %w", err)
	}
	deadline := time.Now().Add(openDeadline)
	return waitForProxyEndpoint(rootDir, opts, deadline, stopEpoch, intendedUpstreamID(opts))
}

func adoptedEndpoint(rootDir, want string, discovery adoptionResult) (Endpoint, error) {
	if want != "" && discovery.pidfile.UpstreamID != "" && discovery.pidfile.UpstreamID != want {
		return Endpoint{}, &ErrUpstreamMismatch{
			RootDir: rootDir,
			Want:    want,
			Have:    discovery.pidfile.UpstreamID,
		}
	}
	return discovery.endpoint, nil
}

func prepareProxySpawn(rootDir string, stopEpoch string, discovery adoptionResult) error {
	currentEpoch, err := readStopEpoch(rootDir)
	if err != nil {
		return fmt.Errorf("read proxy stop epoch under lock: %w", err)
	}
	if currentEpoch != stopEpoch {
		return fmt.Errorf("%w for %s", errStartInterrupted, rootDir)
	}
	if err := quarantineForSpawn(rootDir, discovery); err != nil {
		return err
	}
	if err := cleanupOrphanBackend(rootDir); err != nil {
		return err
	}
	return nil
}

func handleSpawnedChildExit(rootDir string, opts OpenOpts, stopEpoch string, childErr error) error {
	if interrupted, ierr := stopEpochChanged(rootDir, stopEpoch); ierr != nil {
		return ierr
	} else if interrupted {
		return fmt.Errorf("%w for %s", errStartInterrupted, rootDir)
	}
	if childErr == nil {
		childErr = errors.New("child exited without reporting an error")
	}
	// A LockHeldExitCode exit is a lost spawn race, not a listen
	// failure; any other exit gets the child's log path so the real
	// error (listen, backend start, ...) is findable.
	var exitErr *exec.ExitError
	if errors.As(childErr, &exitErr) && exitErr.ExitCode() == LockHeldExitCode {
		return fmt.Errorf(
			"proxy child lost the proxy.lock spawn race for %s: %w",
			rootDir, childErr,
		)
	}
	if opts.Port != 0 {
		return fmt.Errorf(
			"proxy child exited before becoming ready on explicitly configured port %d (see %s): %w",
			opts.Port, opts.LogFilePath, childErr,
		)
	}
	return fmt.Errorf(
		"proxy child exited before publishing its OS-assigned port (see %s): %w",
		opts.LogFilePath, childErr,
	)
}

func handleHardSpawnTimeout(child *spawnedProxyChild, opts OpenOpts) error {
	if err := killSpawnedChild(child); err != nil {
		return fmt.Errorf("hard timeout waiting for proxy on %s; safe child kill failed: %w", describeSpawnPort(opts.Port), err)
	}
	return fmt.Errorf("hard timeout (%s) waiting for proxy on %s", spawnReadyHardTimeout, describeSpawnPort(opts.Port))
}

func handleProxyOpenTimeout(child *spawnedProxyChild, opts OpenOpts) error {
	if err := killSpawnedChild(child); err != nil {
		return fmt.Errorf("timeout waiting for proxy on %s; safe child kill failed: %w", describeSpawnPort(opts.Port), err)
	}
	return fmt.Errorf("timeout waiting for proxy to become ready on %s", describeSpawnPort(opts.Port))
}

func waitForSpawnedProxy(rootDir string, opts OpenOpts, deadline time.Time, stopEpoch string, child *spawnedProxyChild) (Endpoint, error) {
	hard := time.NewTimer(spawnReadyHardTimeout)
	defer hard.Stop()
	poll := time.NewTicker(openPollInterval)
	defer poll.Stop()

	for {
		discovered := readAndDial(rootDir)
		if discovered.status == adoptionAdopted {
			if err := sweepOldQuarantines(rootDir, time.Now()); err != nil {
				log.Printf("dbproxy: could not sweep old quarantined records in %s: %v", rootDir, err)
			}
			return discovered.endpoint, nil
		}
		if discovered.status == adoptionIOErr {
			return Endpoint{}, fmt.Errorf("discover spawned proxy: %w", discovered.err)
		}
		select {
		case childErr := <-child.done:
			return Endpoint{}, handleSpawnedChildExit(rootDir, opts, stopEpoch, childErr)
		case <-hard.C:
			return Endpoint{}, handleHardSpawnTimeout(child, opts)
		case <-poll.C:
		}
		if time.Now().After(deadline) {
			return Endpoint{}, handleProxyOpenTimeout(child, opts)
		}
	}
}
