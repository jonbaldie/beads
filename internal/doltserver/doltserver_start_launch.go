// Package doltserver manages the lifecycle of a local dolt sql-server process.
// It provides transparent auto-start so that `bd init` and `bd <command>` work
// without manual server management.
//
// Port assignment uses OS-assigned ephemeral ports by default. When no explicit
// port is configured (env var, config.yaml, metadata.json), Start() asks the OS
// for a free port via net.Listen(":0"), passes it to dolt sql-server, and writes
// the actual port to dolt-server.port. This eliminates the birthday-problem
// collisions that plagued the old hash-derived port scheme (GH#2098, GH#2372).
//
// Users with explicit port config via BEADS_DOLT_SERVER_PORT env var or
// config.yaml always use that port instead, with conflict detection via
// reclaimPort.
//
// Server state files (PID, port, log, lock) live in the .beads/ directory.
package doltserver

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/lockfile"
)

// Start explicitly starts a dolt sql-server for the project.
// Returns the State of the started server, or an error.
func Start(beadsDir string) (*State, error) {
	cfg := DefaultConfig(beadsDir)
	doltDir := ResolveDoltDir(beadsDir)

	lockF, err := os.OpenFile(lockPath(beadsDir), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("creating lock file: %w", err)
	}
	defer lockF.Close()

	state, err := acquireStartLock(lockF, beadsDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lockfile.FlockUnlock(lockF) }()
	if state != nil {
		return state, nil
	}
	return startServerAfterLock(beadsDir, doltDir, cfg)
}

func startServerAfterLock(beadsDir, doltDir string, cfg *Config) (*State, error) {
	// Re-check after acquiring lock (double-check pattern)
	if state, _ := IsRunning(beadsDir); state != nil && state.Running {
		return state, nil
	}

	prep, err := prepareServerStart(beadsDir, doltDir, cfg)
	if err != nil {
		return nil, err
	}
	defer prep.logFile.Close()

	launch, err := launchDoltServer(prep)
	if err != nil {
		return nil, err
	}
	if launch.adopted != nil {
		return launch.adopted, nil
	}

	if err := persistStartedServer(beadsDir, launch.pid, launch.port); err != nil {
		return nil, err
	}
	if err := waitForStartedServer(beadsDir, cfg.Host, launch.pid, launch.port); err != nil {
		return nil, err
	}

	return &State{
		Running: true,
		PID:     launch.pid,
		Port:    launch.port,
		DataDir: doltDir,
	}, nil
}

func acquireStartLock(lockF *os.File, beadsDir string) (*State, error) {
	if err := lockfile.FlockExclusiveNonBlocking(lockF); err == nil {
		return nil, nil
	} else if !lockfile.IsLocked(err) {
		return nil, fmt.Errorf("acquiring start lock: %w", err)
	}
	// Another bd process is starting the server — wait for it.
	if err := lockfile.FlockExclusiveBlocking(lockF); err != nil {
		return nil, fmt.Errorf("waiting for server start lock: %w", err)
	}
	state, err := IsRunning(beadsDir)
	if err != nil {
		return nil, err
	}
	if state.Running {
		return state, nil
	}
	return nil, nil
}

type serverStartPreparation struct {
	beadsDir              string
	cfg                   *Config
	doltDir               string
	doltBin               string
	useArchiveLevelConfig bool
	debug                 bool
	profDir               string
	cfgDir                string
	logFile               *os.File
}

func prepareServerStart(beadsDir, doltDir string, cfg *Config) (*serverStartPreparation, error) {
	doltBin, useArchiveLevelConfig, err := findDoltStartBinary(beadsDir)
	if err != nil {
		return nil, err
	}
	if err := configureDoltStartIdentity(); err != nil {
		return nil, fmt.Errorf("configuring dolt identity: %w", err)
	}

	debug := IsDebugMode()
	profDir, err := prepareDebugProfileDir(beadsDir, debug)
	if err != nil {
		return nil, err
	}
	if err := ensureDoltInit(doltDir); err != nil {
		return nil, fmt.Errorf("initializing dolt database: %w", err)
	}
	maybeRotateLog(beadsDir)

	cfgDir, err := prepareDoltConfigDir(doltDir, useArchiveLevelConfig)
	if err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(logPath(beadsDir), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600) //nolint:gosec // G304: logPath derives from user-configured beadsDir
	if err != nil {
		return nil, fmt.Errorf("opening log file: %w", err)
	}
	return &serverStartPreparation{
		beadsDir:              beadsDir,
		cfg:                   cfg,
		doltDir:               doltDir,
		doltBin:               doltBin,
		useArchiveLevelConfig: useArchiveLevelConfig,
		debug:                 debug,
		profDir:               profDir,
		cfgDir:                cfgDir,
		logFile:               logFile,
	}, nil
}

func findDoltStartBinary(beadsDir string) (string, bool, error) {
	// Clean up orphaned processes INSIDE the start lock. This prevents a race
	// where one process kills a server another is still starting (GH#2430).
	if killed, killErr := KillStaleServers(beadsDir); killErr == nil && len(killed) > 0 {
		fmt.Fprintf(os.Stderr, "Info: cleaned up %d orphaned dolt sql-server process(es)\n", len(killed))
	}
	doltBin, err := exec.LookPath("dolt")
	if err != nil {
		return "", false, fmt.Errorf("dolt is not installed (not found in PATH)\n\nInstall from: https://docs.dolthub.com/introduction/installation")
	}
	useArchiveLevelConfig := SupportsArchiveLevelConfig(doltBin)
	if !useArchiveLevelConfig {
		fmt.Fprintf(os.Stderr,
			"Info: external dolt at %s predates archive_level config support (need >= Dolt %s); "+
				"this managed server's background auto-GC may still produce zstd archives.\n",
			doltBin, MinDoltVersionForArchiveLevelConfig)
	}
	return doltBin, useArchiveLevelConfig, nil
}

func configureDoltStartIdentity() error {
	return ensureDoltIdentity()
}

func prepareDebugProfileDir(beadsDir string, debug bool) (string, error) {
	if !debug {
		return "", nil
	}
	profDir := DebugProfileDir(beadsDir)
	if err := os.MkdirAll(profDir, config.BeadsDirPerm); err != nil {
		return "", fmt.Errorf("creating pprof directory %s: %w", profDir, err)
	}
	return profDir, nil
}

func prepareDoltConfigDir(doltDir string, useArchiveLevelConfig bool) (string, error) {
	if !useArchiveLevelConfig {
		return "", nil
	}
	cfgDir, err := resolveCfgDir(doltDir)
	if err != nil {
		return "", fmt.Errorf("resolving .doltcfg directory: %w", err)
	}
	return cfgDir, nil
}

type serverLaunchResult struct {
	pid     int
	port    int
	adopted *State
}

func launchDoltServer(prep *serverStartPreparation) (*serverLaunchResult, error) {
	actualPort := prep.cfg.Port
	explicitPort := actualPort > 0
	if adopted, err := adoptDoltServer(prep, actualPort, explicitPort); err != nil {
		return nil, err
	} else if adopted != nil {
		return adopted, nil
	}
	return launchDoltServerAttempts(prep, actualPort, explicitPort)
}

func adoptDoltServer(prep *serverStartPreparation, port int, explicitPort bool) (*serverLaunchResult, error) {
	if !explicitPort {
		return nil, nil
	}
	adoptPID, err := reclaimPort(prep.cfg.Host, port, prep.beadsDir)
	if err != nil {
		return nil, fmt.Errorf("cannot start dolt server on port %d: %w", port, err)
	}
	if adoptPID == 0 {
		return nil, nil
	}
	_ = os.WriteFile(pidPath(prep.beadsDir), []byte(strconv.Itoa(adoptPID)), 0600)
	_ = writePortFile(prep.beadsDir, port)
	return &serverLaunchResult{adopted: &State{Running: true, PID: adoptPID, Port: port, DataDir: prep.doltDir}}, nil
}

func launchDoltServerAttempts(prep *serverStartPreparation, initialPort int, explicitPort bool) (*serverLaunchResult, error) {
	actualPort := initialPort
	attempts := 1
	if !explicitPort {
		attempts = maxEphemeralPortAttempts
	}
	var lastErr error
	for i := range attempts {
		port, err := chooseStartPort(prep.cfg.Host, actualPort, explicitPort)
		if err != nil {
			lastErr = err
			continue
		}
		actualPort = port
		pid, err := launchDoltServerAttempt(prep, actualPort, i, attempts)
		if err != nil {
			lastErr = err
			if explicitPort {
				break
			}
			continue
		}
		return &serverLaunchResult{pid: pid, port: actualPort}, nil
	}
	return nil, serverStartFailure(prep.beadsDir, prep.doltDir, attempts, lastErr)
}

func launchDoltServerAttempt(prep *serverStartPreparation, port, attempt, attempts int) (int, error) {
	cmdArgs, err := buildStartCommandArgs(prep, port)
	if err != nil {
		return 0, err
	}
	pid, err := startDoltProcess(prep, cmdArgs)
	if err != nil {
		return 0, err
	}
	time.Sleep(200 * time.Millisecond)
	if !isProcessAlive(pid) {
		return 0, fmt.Errorf("dolt sql-server exited immediately on port %d (attempt %d/%d)", port, attempt+1, attempts)
	}
	return pid, nil
}

func chooseStartPort(host string, configuredPort int, explicitPort bool) (int, error) {
	if explicitPort {
		return configuredPort, nil
	}
	return allocateEphemeralPort(host)
}

func buildStartCommandArgs(prep *serverStartPreparation, port int) ([]string, error) {
	if !prep.useArchiveLevelConfig {
		return buildDoltServerArgs(prep.cfg.Host, port, prep.debug, prep.profDir), nil
	}
	cfgBody, err := buildDoltServerYAMLConfig(prep.cfg.Host, port, prep.debug, prep.cfgDir)
	if err != nil {
		return nil, fmt.Errorf("rendering managed sql-server config: %w", err)
	}
	// The child's cwd is doltDir, so the config path must be absolute.
	absConfigPath, err := filepath.Abs(doltServerConfigPath(prep.beadsDir))
	if err != nil {
		return nil, fmt.Errorf("resolving managed sql-server config path: %w", err)
	}
	if err := os.WriteFile(absConfigPath, cfgBody, 0600); err != nil {
		return nil, fmt.Errorf("writing managed sql-server config: %w", err)
	}
	return buildDoltServerArgsWithConfig(absConfigPath, prep.debug, prep.profDir), nil
}

func startDoltProcess(prep *serverStartPreparation, cmdArgs []string) (int, error) {
	cmd := exec.Command(prep.doltBin, cmdArgs...) //nolint:gosec // doltBin is resolved from PATH, not user input
	cmd.Dir = prep.doltDir
	cmd.Stdout = prep.logFile
	cmd.Stderr = prep.logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = procAttrDetached()
	cmd.Env = ServerSpawnEnv()

	// The server outlives the caller; close inherited descriptors that would
	// otherwise remain pinned until the server restarts (GH#4634).
	sanitizeInheritedFDs()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

func serverStartFailure(beadsDir, doltDir string, attempts int, lastErr error) error {
	// GH#3290 / bd-6dnrw.6: detect corruption but never auto-repair; repair
	// stays behind explicit bd doctor --fix because reinitializing .dolt is
	// destructive.
	if dirs, err := detectCorruptManifest(beadsDir, doltDir); err == nil && len(dirs) > 0 {
		return fmt.Errorf("failed to start dolt server after %d attempts: %w\n"+
			"Corrupt manifest with no recoverable data detected (GH#3290) in:\n  %s\n"+
			"Run 'bd doctor --fix' to back up the corrupt database(s) and reinitialize.\nCheck logs: %s",
			attempts, lastErr, strings.Join(dirs, "\n  "), logPath(beadsDir))
	}
	return fmt.Errorf("failed to start dolt server after %d attempts: %w\nCheck logs: %s",
		attempts, lastErr, logPath(beadsDir))
}

func persistStartedServer(beadsDir string, pid, port int) error {
	if err := os.WriteFile(pidPath(beadsDir), []byte(strconv.Itoa(pid)), 0600); err != nil {
		killProcess(pid)
		return fmt.Errorf("writing PID file: %w", err)
	}
	if err := writePortFile(beadsDir, port); err != nil {
		killProcess(pid)
		_ = os.Remove(pidPath(beadsDir))
		return fmt.Errorf("writing port file: %w", err)
	}
	return nil
}

func killProcess(pid int) {
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Kill()
	}
}

func waitForStartedServer(beadsDir, host string, pid, port int) error {
	if err := waitForReady(host, port, readyTimeout()); err == nil {
		return nil
	} else {
		killProcess(pid)
		_ = os.Remove(pidPath(beadsDir))
		_ = os.Remove(portPath(beadsDir))
		return formatServerReadinessFailure(beadsDir, pid, port, err)
	}
}

func formatServerReadinessFailure(beadsDir string, pid, port int, err error) error {
	if hasJournalCorruption, logErr := logHasCorruptJournalError(logPath(beadsDir)); logErr == nil && hasJournalCorruption {
		return fmt.Errorf("server started (PID %d) but not accepting connections on port %d: %w\n\n%s",
			pid, port, err, corruptJournalRecoveryHint(beadsDir))
	}
	return fmt.Errorf("server started (PID %d) but not accepting connections on port %d: %w\nCheck logs: %s",
		pid, port, err, logPath(beadsDir))
}
