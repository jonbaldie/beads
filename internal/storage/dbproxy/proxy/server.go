package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v4"
	"golang.org/x/sync/errgroup"

	"github.com/jonbaldie/beads/internal/lockfile"
	"github.com/jonbaldie/beads/internal/procid"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/identity"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/pidfile"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/server"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/util"
)

const IdleTimeoutNever time.Duration = -1

type ProxyOpts struct {
	RootDir     string
	Port        int
	IdleTimeout time.Duration
	Server      server.DatabaseServer
	// StopEpoch is the proxy stop epoch the spawning parent observed under
	// proxy.lock before forking this proxy. ListenAndServe re-reads the
	// epoch immediately before publishing proxy.pid and aborts if it has
	// advanced, so a slow backend start cannot outlast a concurrent
	// `bd dolt stop` and publish a running proxy after the stop returned.
	// Empty means "no stop had ever run", which readStopEpoch also reports
	// for a missing epoch file, so the zero value stays correct for direct
	// (non-forked) callers such as tests.
	StopEpoch string
	// Stats is optional. When non-nil, the proxy records per-event counters
	// against it; tests use Snapshot() to assert. Production code should
	// leave this nil.
	Stats *Stats
}

type proxyServer struct {
	rootDir     string
	port        int
	idleTimeout time.Duration
	server      server.DatabaseServer
	stats       *Stats
	stopEpoch   string

	logger      *log.Logger
	listener    net.Listener
	activeConns atomic.Int64
	conns       errgroup.Group

	// shutdown cancels the ListenAndServe run loop from handleConn when it
	// finds the backend dead. It is nil until ListenAndServe installs it, and
	// guarded by shutdownOnce so a burst of connections that all hit the same
	// dead backend triggers exactly one shutdown.
	shutdown     context.CancelFunc
	shutdownOnce sync.Once
}

const (
	PIDFileName  = "proxy.pid"
	LogFileName  = "proxy.log"
	LockFileName = "proxy.lock"
)

// LockHeldExitCode is the exit code a child proxy should use when
// ListenAndServe returns ErrLockHeld. The spawning parent treats this
// (EX_TEMPFAIL) as "lost the spawn race" and retries via readAndDial.
const LockHeldExitCode = 75

// ErrLockHeld is returned from ListenAndServe when another proxy already
// holds proxy.lock for the same rootDir. It is a normal "lost the race"
// outcome, not a failure: callers spawned as children should map it to
// LockHeldExitCode and exit cleanly.
var ErrLockHeld = errors.New("proxy lock held by another proxy on this rootDir")

const (
	serverReadyTimeout     = 30 * time.Second
	readyDialTimeout       = 2 * time.Second
	readyInitialBackoff    = 50 * time.Millisecond
	readyMaxBackoff        = 1 * time.Second
	idleWatcherMinInterval = 1 * time.Second
	backendStopTimeout     = 5 * time.Minute
	tcpKeepAlivePeriod     = 30 * time.Second

	// backendDeathPollBudget/Interval bound a short re-poll of Running()
	// after a Dial failure. A real backend's OS-level socket teardown (which
	// makes Dial fail) happens synchronously with process death, but
	// DatabaseServer.Running() only flips false once cmd.Wait() reaps the
	// dead process — typically microseconds to a few milliseconds later, but
	// not instantaneous. A single unlucky Dial can land in that gap and see
	// Running() still (stale) true; without a re-poll that connection would
	// be the proxy's only chance to notice the backend is gone, and it would
	// run forever with a backend that never dials successfully again. The
	// budget is comfortably larger than the observed reap lag while staying
	// small enough not to meaningfully delay the (rare) genuinely-transient
	// Dial failure case, where Running() stays true for the whole budget.
	backendDeathPollBudget   = 250 * time.Millisecond
	backendDeathPollInterval = 5 * time.Millisecond
)

var errIdleTimeout = errors.New("idle timeout reached")

type heldProxyLock struct {
	lock *util.Lock
	held bool
}

func (l *heldProxyLock) release() {
	if l != nil && l.held {
		l.held = false
		l.lock.Unlock()
	}
}

type proxyRun struct {
	ctx    context.Context
	cancel context.CancelFunc

	logFile *os.File
	signals proxySignals
	epoch   proxyEpochWatch

	listener net.Listener
	control  *controlServer
	identity proxyIdentity
	backend  proxyBackend
}

type proxySignals struct {
	ch       chan os.Signal
	received atomic.Bool
}

type proxyEpochWatch struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

type proxyIdentity struct {
	mu        sync.RWMutex
	reply     identity.IdentReply
	dataPort  int
	published bool
}

type proxyBackend struct {
	started bool
	stopped bool
}

func newProxyRun(parentCtx context.Context, p *proxyServer) (*proxyRun, error) {
	logPath := filepath.Join(p.rootDir, LogFileName)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- logPath is derived from operator-supplied config, not untrusted request input
	if err != nil {
		return nil, fmt.Errorf("open proxy log %q: %w", logPath, err)
	}
	p.logger = log.New(f, "[proxy] ", log.LstdFlags|log.Lmicroseconds)
	ctx, cancel := context.WithCancel(parentCtx)
	run := &proxyRun{ctx: ctx, cancel: cancel, logFile: f}
	p.shutdown = cancel
	run.installSignals(p)
	return run, nil
}

func (r *proxyRun) installSignals(p *proxyServer) {
	r.signals.ch = make(chan os.Signal, 1)
	signal.Notify(r.signals.ch, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case <-r.ctx.Done():
		case <-r.signals.ch:
			r.signals.received.Store(true)
			p.stats.IncSignalReceived()
			r.cancel()
		}
	}()
}

func (r *proxyRun) startEpochWatch(p *proxyServer) {
	watchCtx, watchCancel := context.WithCancel(context.Background())
	r.epoch.cancel = watchCancel
	r.epoch.done = make(chan struct{})
	go func() {
		defer close(r.epoch.done)
		ticker := time.NewTicker(openPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
				changed, err := stopEpochChanged(p.rootDir, p.stopEpoch)
				if err == nil && changed {
					r.cancel()
					return
				}
			}
		}
	}()
}

func (r *proxyRun) stopEpochWatch() {
	r.epoch.once.Do(func() {
		if r.epoch.cancel == nil {
			return
		}
		r.epoch.cancel()
		<-r.epoch.done
	})
}

func (r *proxyRun) stopBackend(p *proxyServer) error {
	if !r.backend.started || r.backend.stopped {
		return nil
	}
	r.backend.stopped = true
	p.stats.IncBackendStop()
	return stopBackendBounded(p.server)
}

func (r *proxyRun) close(p *proxyServer) {
	if r.backend.started && !r.backend.stopped {
		_ = r.stopBackend(p)
	}
	r.stopEpochWatch()
	r.cancel()
	if r.signals.ch != nil {
		signal.Stop(r.signals.ch)
	}
	if r.listener != nil {
		_ = r.listener.Close()
	}
	if r.control != nil {
		_ = r.control.Close()
	}
	if r.identity.published {
		_ = pidfile.Remove(filepath.Dir(r.logFile.Name()), PIDFileName)
	}
	if r.logFile != nil {
		_ = r.logFile.Close()
	}
}

func NewProxyServer(opts ProxyOpts) *proxyServer {
	stats := opts.Stats
	if stats == nil {
		stats = &Stats{}
	}
	return &proxyServer{
		rootDir:     opts.RootDir,
		port:        opts.Port,
		idleTimeout: opts.IdleTimeout,
		server:      opts.Server,
		stats:       stats,
		stopEpoch:   opts.StopEpoch,
	}
}

func (p *proxyServer) handleConn(ctx context.Context, client net.Conn) error {
	addr := client.RemoteAddr()
	p.tracef("handleConn(%s) start", addr)
	p.activeConns.Add(1)
	defer func() {
		p.activeConns.Add(-1)
		p.tracef("handleConn(%s) end (active=%d)", addr, p.activeConns.Load())
	}()

	p.stats.IncBackendDialAttempt()
	backend, err := p.server.Dial(ctx)
	if err != nil {
		p.tracef("handleConn(%s) backend dial error: %v", addr, err)
		p.stats.IncBackendDialError()
		_ = client.Close()
		if p.backendConfirmedDead(ctx) {
			// The backend (e.g. the dolt sql-server child) exited independently
			// of this proxy process (GH#5842): DatabaseServer.Start is single-
			// shot per instance (by design, see doltserver_test.go), so this
			// proxy cannot restart it in place. Instead shut the proxy down
			// cleanly so it removes its pidfile; the existing stale-pidfile
			// self-heal on the client side then transparently spawns a fresh
			// proxy (and a fresh backend instance) on the next command, rather
			// than leaving every future connection failing forever.
			p.shutdownOnce.Do(func() {
				p.tracef("handleConn(%s) backend not running, shutting proxy down to self-heal", addr)
				p.stats.IncBackendDeadShutdown()
				if p.shutdown != nil {
					p.shutdown()
				}
			})
		}
		return err
	}
	p.tracef("handleConn(%s) backend dial ok", addr)
	p.stats.IncBackendDialSuccess()
	p.stats.IncHandledConn()

	done := make(chan struct{})
	var doneOnce sync.Once
	finish := func() { doneOnce.Do(func() { close(done) }) }

	var g errgroup.Group
	g.Go(func() error {
		select {
		case <-ctx.Done():
			p.tracef("handleConn(%s) ctx canceled, force-closing", addr)
			_ = client.Close()
			_ = backend.Close()
		case <-done:
		}
		return nil
	})
	g.Go(func() error {
		defer finish()
		defer func() { _ = backend.Close() }()
		defer func() { _ = client.Close() }()
		n, err := io.Copy(backend, client)
		p.stats.AddBytesClientToBackend(n)
		p.tracef("handleConn(%s) client→backend done (n=%d, err=%v)", addr, n, err)
		return err
	})
	g.Go(func() error {
		defer finish()
		defer func() { _ = backend.Close() }()
		defer func() { _ = client.Close() }()
		n, err := io.Copy(client, backend)
		p.stats.AddBytesBackendToClient(n)
		p.tracef("handleConn(%s) backend→client done (n=%d, err=%v)", addr, n, err)
		return err
	})
	return g.Wait()
}

func (p *proxyServer) acceptLoop(ctx context.Context) error {
	p.tracef("acceptLoop start (addr=%s)", p.listener.Addr())
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				p.tracef("acceptLoop exit (ctx=%v)", ctx.Err())
				return nil
			}
			// Surface non-shutdown accept errors to the errgroup so the
			// proxy fails fast instead of busy-looping. Specific errors that
			// warrant retry (e.g. transient EMFILE under load) can be added
			// here as the need arises.
			p.tracef("acceptLoop error: %v", err)
			p.stats.IncAcceptError()
			return fmt.Errorf("accept: %w", err)
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetKeepAlive(true)
			_ = tc.SetKeepAlivePeriod(tcpKeepAlivePeriod)
		}
		p.tracef("acceptLoop accepted (remote=%s)", conn.RemoteAddr())
		p.stats.IncAccept()
		p.conns.Go(func() error {
			return p.handleConn(ctx, conn)
		})
	}
}

func (p *proxyServer) backendConfirmedDead(ctx context.Context) bool {
	deadline := time.Now().Add(backendDeathPollBudget)
	for {
		if !p.server.Running(ctx) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(backendDeathPollInterval):
		}
	}
}

func acquireProxyLock(rootDir string) (*heldProxyLock, error) {
	lock, err := util.TryLock(filepath.Join(rootDir, LockFileName))
	if err != nil {
		if lockfile.IsLocked(err) {
			return nil, ErrLockHeld
		}
		return nil, fmt.Errorf("acquire %s: %w", LockFileName, err)
	}
	return &heldProxyLock{lock: lock, held: true}, nil
}

func startProxyRun(p *proxyServer, parentCtx context.Context, held *heldProxyLock) (run *proxyRun, err error) {
	run, err = newProxyRun(parentCtx, p)
	if err != nil {
		return nil, err
	}
	started := run
	defer func() {
		if err != nil {
			_ = started.stopBackend(p)
			started.close(p)
		}
	}()

	run.startEpochWatch(p)
	if err := startDataPath(p, run); err != nil {
		return nil, err
	}
	if err := startBackend(p, run, held); err != nil {
		return nil, err
	}
	if err := publishProxy(p, run, held); err != nil {
		return nil, err
	}
	return run, nil
}

func startDataPath(p *proxyServer, run *proxyRun) error {
	addr := fmt.Sprintf("127.0.0.1:%d", p.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	run.listener = ln
	p.listener = ln
	p.stats.IncListenAndServe()
	dataPort, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("proxy: unexpected data listener address %T", ln.Addr())
	}
	run.identity.dataPort = dataPort.Port
	if _, err := identity.WriteSecret(p.rootDir); err != nil {
		return fmt.Errorf("write proxy secret: %w", err)
	}
	run.identity.reply = identity.IdentReply{Schema: pidfile.SchemaV2, Role: pidfile.KindProxy, DataPort: dataPort.Port}
	control, err := startControl(p.rootDir, func() identity.IdentReply {
		run.identity.mu.RLock()
		defer run.identity.mu.RUnlock()
		return run.identity.reply
	})
	if err != nil {
		return fmt.Errorf("start control listener: %w", err)
	}
	run.control = control
	return nil
}

func startBackend(p *proxyServer, run *proxyRun, held *heldProxyLock) error {
	p.stats.IncBackendStart()
	if err := p.server.Start(run.ctx); err != nil {
		if changed, checkErr := stopEpochChanged(p.rootDir, p.stopEpoch); checkErr == nil && changed {
			return fmt.Errorf("%w for %s: stop epoch advanced during backend start (%v)", errStartInterrupted, p.rootDir, err)
		}
		return fmt.Errorf("start database server: %w", err)
	}
	run.backend.started = true
	if err := waitForServerReady(run.ctx, p.server, serverReadyTimeout); err != nil {
		if changed, checkErr := stopEpochChanged(p.rootDir, p.stopEpoch); checkErr == nil && changed {
			return abortInterruptedStart(p, run, held)
		}
		if stopErr := run.stopBackend(p); stopErr != nil {
			return errors.Join(fmt.Errorf("database server not ready: %w", err), fmt.Errorf("stop backend: %w", stopErr))
		}
		return fmt.Errorf("database server not ready: %w", err)
	}
	return nil
}

func publishProxy(p *proxyServer, run *proxyRun, held *heldProxyLock) error {
	birth, err := procid.Capture(os.Getpid())
	if err != nil {
		return fmt.Errorf("capture proxy birth identity: %w", err)
	}
	rootID, err := identity.RootID(p.rootDir)
	if err != nil {
		return fmt.Errorf("resolve proxy root identity: %w", err)
	}
	upstreamID := p.server.ID(run.ctx)
	run.identity.mu.Lock()
	run.identity.reply.RootID = rootID
	run.identity.reply.UpstreamID = upstreamID
	run.identity.reply.PID = os.Getpid()
	run.identity.reply.Birth = string(birth)
	run.identity.reply.ControlPort = run.control.Port()
	run.identity.mu.Unlock()

	run.stopEpochWatch()
	if changed, err := stopEpochChanged(p.rootDir, p.stopEpoch); err != nil {
		return fmt.Errorf("re-check proxy stop epoch before publish: %w", err)
	} else if changed {
		return abortInterruptedStart(p, run, held)
	}

	if err := pidfile.Write(p.rootDir, PIDFileName, pidfile.PidFile{
		Pid: os.Getpid(), Port: run.identity.dataPort, UpstreamID: upstreamID,
		Schema: pidfile.SchemaV2, Kind: pidfile.KindProxy, Birth: string(birth),
		RootID: rootID, ControlPort: run.control.Port(),
	}); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	run.identity.published = true
	return nil
}

func abortInterruptedStart(p *proxyServer, run *proxyRun, held *heldProxyLock) error {
	if held != nil {
		held.release()
	}
	_ = run.stopBackend(p)
	return fmt.Errorf("%w for %s: stop epoch advanced during startup", errStartInterrupted, p.rootDir)
}

func runProxyLoops(p *proxyServer, run *proxyRun) error {
	g, gctx := errgroup.WithContext(run.ctx)
	g.Go(func() error {
		<-gctx.Done()
		_ = run.listener.Close()
		_ = run.control.Close()
		return nil
	})
	g.Go(func() error { return p.idleWatcher(gctx) })
	g.Go(func() error { return p.acceptLoop(gctx) })
	runErr := g.Wait()
	_ = p.conns.Wait()
	stopErr := run.stopBackend(p)
	if stopErr != nil {
		stopErr = fmt.Errorf("stop database server: %w", stopErr)
	}
	return errors.Join(normalizeProxyRunError(runErr, run.signals.received.Load()), stopErr)
}

func normalizeProxyRunError(runErr error, signalReceived bool) error {
	if errors.Is(runErr, errIdleTimeout) || signalReceived {
		runErr = nil
	}
	return runErr
}

func stopBackendBounded(s server.DatabaseServer) error {
	ctx, cancel := context.WithTimeout(context.Background(), backendStopTimeout)
	defer cancel()
	return s.Stop(ctx)
}

func handleIdleTick(p *proxyServer, idleSince *time.Time) (bool, error) {
	if n := p.activeConns.Load(); n > 0 {
		if !idleSince.IsZero() {
			p.tracef("idleWatcher cleared (active=%d)", n)
			*idleSince = time.Time{}
		}
		return false, nil
	}
	if idleSince.IsZero() {
		p.tracef("idleWatcher armed")
		*idleSince = time.Now()
		return false, nil
	}
	if time.Since(*idleSince) >= p.idleTimeout {
		p.tracef("idleWatcher expired after %s, shutting down", p.idleTimeout)
		p.stats.IncIdleTimeout()
		return true, errIdleTimeout
	}
	return false, nil
}

// backendConfirmedDead re-polls Running() for up to backendDeathPollBudget
// after a Dial failure before concluding the backend is actually dead. A
// single Running()==true right after Dial fails is not conclusive: it can
// be stale for up to the real backend's process-reap latency (see
// backendDeathPollBudget's doc comment). Polling briefly here, rather than
// trusting the first read, keeps a connection that lands in that gap from
// being the proxy's only (missed) chance to notice the backend is gone.

func waitForServerReady(ctx context.Context, s server.DatabaseServer, timeout time.Duration) error {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = readyInitialBackoff
	bo.MaxInterval = readyMaxBackoff
	bo.MaxElapsedTime = timeout

	return backoff.Retry(func() error {
		if !s.Running(ctx) {
			return errors.New("database server not running")
		}
		dialCtx, cancel := context.WithTimeout(ctx, readyDialTimeout)
		defer cancel()
		conn, err := s.Dial(dialCtx)
		if err != nil {
			return err
		}
		_ = conn.Close()
		return nil
	}, backoff.WithContext(bo, ctx))
}
