package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/dolthub/dolt/go/libraries/doltcore/servercfg"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	_ "github.com/go-sql-driver/mysql"
	"golang.org/x/sync/errgroup"

	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/procid"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/identity"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/pidfile"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/util"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

const defaultKeepAlivePeriod = 30 * time.Second

const (
	PIDFileName  = "proxy-child.pid"
	LockFileName = "proxy-child.lock"
)

const (
	startReadyTimeout      = 30 * time.Second
	startReadyPollInterval = 50 * time.Millisecond
	startReadyDialTimeout  = 250 * time.Millisecond
)

type DoltServer struct {
	id              string
	doltBinExec     string
	rootDir         string
	configPath      string
	database        string
	config          servercfg.ServerConfig
	keepAlivePeriod time.Duration

	logFile *os.File
	eg      *errgroup.Group
	egCtx   context.Context
	cancel  context.CancelFunc
	pid     int
}

var _ DatabaseServer = (*DoltServer)(nil)

func NewDoltServer(doltBinExec, rootDir, configPath, logFilePath string, keepAlivePeriod time.Duration, database string) (*DoltServer, error) {
	paths, err := resolveDoltServerPaths(doltBinExec, rootDir, configPath)
	if err != nil {
		return nil, err
	}
	cfg, err := loadDoltServerConfig(configPath)
	if err != nil {
		return nil, err
	}
	logFile, err := openDoltServerLog(logFilePath)
	if err != nil {
		return nil, err
	}
	if keepAlivePeriod == 0 {
		keepAlivePeriod = defaultKeepAlivePeriod
	}
	sum := sha256.Sum256([]byte(paths.rootDir))
	return &DoltServer{
		id:              hex.EncodeToString(sum[:]),
		doltBinExec:     paths.doltBinExec,
		rootDir:         paths.rootDir,
		configPath:      paths.configPath,
		database:        database,
		config:          cfg,
		keepAlivePeriod: keepAlivePeriod,
		logFile:         logFile,
	}, nil
}

type doltServerPaths struct {
	doltBinExec string
	rootDir     string
	configPath  string
}

func resolveDoltServerPaths(doltBinExec, rootDir, configPath string) (doltServerPaths, error) {
	if doltBinExec == "" {
		return doltServerPaths{}, errors.New("server: NewDoltServer: doltBinExec is required")
	}
	if rootDir == "" {
		return doltServerPaths{}, errors.New("server: NewDoltServer: rootDir is required")
	}
	if configPath == "" {
		return doltServerPaths{}, errors.New("server: NewDoltServer: configPath is required")
	}
	absDoltBinExec, err := filepath.Abs(doltBinExec)
	if err != nil {
		return doltServerPaths{}, errors.New("server: NewDoltServer: failed to determine absolute path of doltBinExec")
	}
	absRootDir, err := filepath.Abs(rootDir)
	if err != nil {
		return doltServerPaths{}, errors.New("server: NewDoltServer: failed to determine absolute path of rootDir")
	}
	absConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return doltServerPaths{}, errors.New("server: NewDoltServer: failed to determine absolute path of configPath")
	}
	return doltServerPaths{
		doltBinExec: absDoltBinExec,
		rootDir:     absRootDir,
		configPath:  absConfigPath,
	}, nil
}

func loadDoltServerConfig(configPath string) (servercfg.ServerConfig, error) {
	cfg, err := servercfg.YamlConfigFromFile(filesys.LocalFS, configPath)
	if err != nil {
		return nil, fmt.Errorf("server: NewDoltServer: parse config %q: %w", configPath, err)
	}
	return cfg, nil
}

func openDoltServerLog(logFilePath string) (*os.File, error) {
	if logFilePath == "" {
		return nil, nil
	}
	absLogFilePath, err := filepath.Abs(logFilePath)
	if err != nil {
		return nil, errors.New("server: NewDoltServer: failed to determine absolute path of logFilePath")
	}
	logFile, err := os.OpenFile(absLogFilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // logFilePath is caller-derived, not user-request input
	if err != nil {
		return nil, fmt.Errorf("server: NewDoltServer: open log %q: %w", logFilePath, err)
	}
	return logFile, nil
}

func (s *DoltServer) ID(_ context.Context) string {
	return s.id
}

func (s *DoltServer) DSN(_ context.Context, database, user, password string) string {
	dsn := util.DoltServerDSN{
		User:        user,
		Password:    password,
		Database:    database,
		TLSRequired: s.config.RequireSecureTransport(),
		TLSCert:     s.config.TLSCert(),
		TLSKey:      s.config.TLSKey(),
	}
	if sock := s.config.Socket(); sock != "" {
		dsn.Socket = sock
	} else {
		dsn.Host = s.config.Host()
		dsn.Port = s.config.Port()
	}
	return dsn.String()
}

func (s *DoltServer) doltConfigure(ctx context.Context) error {
	probe := exec.CommandContext(ctx, s.doltBinExec, "config", "--global", "--get", "user.name")
	if out, err := probe.Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		return nil
	}
	name, email := "beads", "beads@localhost"
	if out, err := exec.CommandContext(ctx, "git", "config", "user.name").Output(); err == nil {
		if v := strings.TrimSpace(string(out)); v != "" {
			name = v
		}
	}
	if out, err := exec.CommandContext(ctx, "git", "config", "user.email").Output(); err == nil {
		if v := strings.TrimSpace(string(out)); v != "" {
			email = v
		}
	}
	if out, err := exec.CommandContext(ctx, s.doltBinExec, "config", "--global", "--add", "user.name", name).CombinedOutput(); err != nil {
		return fmt.Errorf("server: DoltServer.doltConfigure: set user.name: %w\n%s", err, out)
	}
	if out, err := exec.CommandContext(ctx, s.doltBinExec, "config", "--global", "--add", "user.email", email).CombinedOutput(); err != nil {
		return fmt.Errorf("server: DoltServer.doltConfigure: set user.email: %w\n%s", err, out)
	}
	return nil
}

func (s *DoltServer) doltInit(ctx context.Context) error {
	if err := os.MkdirAll(s.rootDir, 0o755); err != nil {
		return fmt.Errorf("server: DoltServer.doltInit: mkdir %q: %w", s.rootDir, err)
	}

	cmd := exec.CommandContext(ctx, s.doltBinExec, "init")
	cmd.Dir = s.rootDir
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "already been initialized") {
			return doltserver.MarkDoltDirCompatible(s.rootDir)
		}
		return fmt.Errorf("server: DoltServer.doltInit: %w\n%s", err, out)
	}

	return doltserver.MarkDoltDirCompatible(s.rootDir)
}

var retryableDoltInitErrSubstrings = []string{
	"repository state is invalid",
}

func isRetryableDoltInitErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, s := range retryableDoltInitErrSubstrings {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

func (s *DoltServer) doltInitWithRetries(ctx context.Context) error {
	const maxRetries = 4
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 100 * time.Millisecond
	bo.MaxInterval = 1 * time.Second
	bo.MaxElapsedTime = 0

	op := func() error {
		err := s.doltInit(ctx)
		if err == nil {
			return nil
		}
		if !isRetryableDoltInitErr(err) {
			return backoff.Permanent(err)
		}
		return err
	}

	return backoff.Retry(op, backoff.WithMaxRetries(backoff.WithContext(bo, ctx), maxRetries))
}

func (s *DoltServer) Start(ctx context.Context) error {
	if s.eg != nil || s.egCtx != nil {
		return fmt.Errorf("server: DoltServer.Start: server already started")
	}

	lock, err := util.TryLock(filepath.Join(s.rootDir, LockFileName))
	if err != nil {
		return fmt.Errorf("server: DoltServer.Start: acquire %s: %w", LockFileName, err)
	}

	if err := prepareDoltServerStart(s, ctx); err != nil {
		lock.Unlock()
		return err
	}

	if err := startManagedDoltProcess(s, lock); err != nil {
		return err
	}

	if err := s.waitReady(ctx); err != nil {
		abortDoltReadinessWait(s)
		return fmt.Errorf("server: DoltServer.Start: %w", err)
	}
	return nil
}

func prepareDoltServerStart(s *DoltServer, ctx context.Context) error {
	if err := s.doltConfigure(ctx); err != nil {
		return err
	}
	return s.doltInitWithRetries(ctx)
}

func startManagedDoltProcess(s *DoltServer, lock *util.Lock) error {
	managedCtx, cancel := context.WithCancel(context.Background())
	eg, egCtx := errgroup.WithContext(managedCtx)
	s.eg = eg
	s.egCtx = egCtx
	s.cancel = cancel

	cmd := newDoltServerCommand(s, managedCtx)
	if err := cmd.Start(); err != nil {
		clearDoltStartState(s, cancel, lock)
		return fmt.Errorf("server: DoltServer.Start: spawn dolt: %w", err)
	}

	s.pid = cmd.Process.Pid
	birth, rootID, err := captureDoltStartIdentity(s)
	if err != nil {
		abortDoltProcessStart(s, cmd, cancel, lock)
		return err
	}
	if err := writeDoltStartPIDFile(s, birth, rootID); err != nil {
		abortDoltProcessStart(s, cmd, cancel, lock)
		return err
	}

	eg.Go(func() error {
		defer lock.Unlock()
		return cmd.Wait()
	})
	return nil
}

func newDoltServerCommand(s *DoltServer, ctx context.Context) *exec.Cmd {
	cmd := exec.CommandContext(ctx, s.doltBinExec, "sql-server", "--config", s.configPath)
	cmd.Dir = s.rootDir
	cmd.Stdin = nil
	if s.logFile != nil {
		cmd.Stdout = s.logFile
		cmd.Stderr = s.logFile
	}
	// The proxied server runs CALL DOLT_PUSH/FETCH in-process; see
	// doltserver.ServerSpawnEnv for the guards it needs (GH#4272).
	cmd.Env = doltserver.ServerSpawnEnv()
	return cmd
}

func captureDoltStartIdentity(s *DoltServer) (procid.Token, string, error) {
	birth, err := procid.Capture(s.pid)
	if err != nil {
		return "", "", fmt.Errorf("server: DoltServer.Start: capture child birth identity: %w", err)
	}
	rootID, err := identity.RootID(s.rootDir)
	if err != nil {
		return "", "", fmt.Errorf("server: DoltServer.Start: resolve proxy root identity: %w", err)
	}
	return birth, rootID, nil
}

func writeDoltStartPIDFile(s *DoltServer, birth procid.Token, rootID string) error {
	if err := pidfile.Write(s.rootDir, PIDFileName, pidfile.PidFile{
		Pid:    s.pid,
		Port:   s.config.Port(),
		Schema: pidfile.SchemaV2,
		Kind:   pidfile.KindDoltBackend,
		Birth:  string(birth),
		RootID: rootID,
	}); err != nil {
		return fmt.Errorf("server: DoltServer.Start: write pidfile: %w", err)
	}
	return nil
}

func clearDoltStartState(s *DoltServer, cancel context.CancelFunc, lock *util.Lock) {
	s.eg, s.egCtx, s.cancel = nil, nil, nil
	cancel()
	lock.Unlock()
}

func abortDoltProcessStart(s *DoltServer, cmd *exec.Cmd, cancel context.CancelFunc, lock *util.Lock) {
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	s.eg, s.egCtx, s.cancel, s.pid = nil, nil, nil, 0
	cancel()
	lock.Unlock()
}

func abortDoltReadinessWait(s *DoltServer) {
	s.cancel()
	_ = s.eg.Wait()
	s.eg, s.egCtx, s.cancel, s.pid = nil, nil, nil, 0
	_ = pidfile.Remove(s.rootDir, PIDFileName)
}

func (s *DoltServer) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(startReadyTimeout)
	for {
		if s.egCtx.Err() != nil {
			return errors.New("dolt sql-server exited before listener became ready")
		}

		dctx, dcancel := context.WithTimeout(ctx, startReadyDialTimeout)
		conn, err := s.Dial(dctx)
		dcancel()
		if err == nil {
			_ = conn.Close()
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("listener not ready after %s: %w", startReadyTimeout, err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.egCtx.Done():
			return errors.New("dolt sql-server exited before listener became ready")
		case <-time.After(startReadyPollInterval):
		}
	}
}

func (s *DoltServer) Stop(ctx context.Context) error {
	return errors.Join(
		shutdownDoltGCError(s, ctx),
		waitForDoltStop(s),
		closeDoltLogFile(s),
		removeDoltPIDFile(s),
	)
}

func shutdownDoltGCError(s *DoltServer, ctx context.Context) error {
	if err := s.runShutdownGC(ctx); err != nil {
		return fmt.Errorf("server: DoltServer.Stop: %w", err)
	}
	return nil
}

func waitForDoltStop(s *DoltServer) error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.eg == nil {
		return nil
	}
	waitErr := s.eg.Wait()
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) || errors.Is(waitErr, context.Canceled) {
		return nil
	}
	if waitErr != nil {
		return fmt.Errorf("server: DoltServer.Stop: %w", waitErr)
	}
	return nil
}

func closeDoltLogFile(s *DoltServer) error {
	if s.logFile == nil {
		return nil
	}
	err := s.logFile.Close()
	s.logFile = nil
	if err != nil {
		return fmt.Errorf("server: DoltServer.Stop: close log: %w", err)
	}
	return nil
}

func removeDoltPIDFile(s *DoltServer) error {
	if s.pid == 0 {
		return nil
	}
	err := pidfile.Remove(s.rootDir, PIDFileName)
	s.pid = 0
	if err != nil {
		return fmt.Errorf("server: DoltServer.Stop: remove pidfile: %w", err)
	}
	return nil
}

func (s *DoltServer) runShutdownGC(ctx context.Context) (retErr error) {
	if s.database == "" || !s.Running(ctx) {
		return nil
	}
	db, err := sql.Open("mysql", s.DSN(ctx, s.database, "root", ""))
	if err != nil {
		return fmt.Errorf("open gc connection: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, db.Close()) }()

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire gc connection: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, conn.Close()) }()

	if err := versioncontrolops.DoltGC(ctx, conn); err != nil {
		retErr = errors.Join(retErr, err)
	}
	if _, err := conn.ExecContext(ctx, "CALL DOLT_STATS_GC()"); err != nil {
		retErr = errors.Join(retErr, fmt.Errorf("dolt_stats_gc: %w", err))
	}
	return retErr
}

func (s *DoltServer) Running(_ context.Context) bool {
	if s.egCtx == nil {
		return false
	}
	return s.egCtx.Err() == nil
}

func (s *DoltServer) Dial(ctx context.Context) (net.Conn, error) {
	network, addr := "tcp", net.JoinHostPort(s.config.Host(), strconv.Itoa(s.config.Port()))
	if sock := s.config.Socket(); sock != "" {
		network, addr = "unix", sock
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("server: DoltServer.Dial: %w", err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(s.keepAlivePeriod)
	}
	return conn, nil
}
