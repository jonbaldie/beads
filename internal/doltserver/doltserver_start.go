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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	_ "github.com/go-sql-driver/mysql"
	"gopkg.in/yaml.v3"

	"github.com/jonbaldie/beads/internal/githooksenv"
	"github.com/jonbaldie/beads/internal/gittraceenv"
)

// EnsureRunning starts the server if it is not already running.
// This is the main auto-start entry point. Thread-safe via file lock.
// Returns the port the server is listening on.
//
// When metadata.json specifies an explicit dolt_server_port (indicating an
// external/shared server, e.g. managed by systemd), EnsureRunning will NOT
// start a new server. The external server's lifecycle is not bd's
// responsibility — starting a per-project server would conflict with (or
// kill) the shared server. See GH#2554.
func EnsureRunning(beadsDir string) (int, error) {
	port, _, err := EnsureRunningDetailed(beadsDir)
	return port, err
}

// EnsureRunningDetailed is like EnsureRunning but also reports whether a new
// server was started (startedByUs=true) vs. an already-running server was
// adopted (startedByUs=false). Callers that need to clean up auto-started
// servers (e.g. test teardown) should use this variant.
func EnsureRunningDetailed(beadsDir string) (port int, startedByUs bool, err error) {
	serverDir := resolveServerDir(beadsDir)

	announceSharedServerUse()

	state, err := IsRunning(serverDir)
	if err != nil {
		return 0, false, err
	}
	if state.Running {
		_ = EnsurePortFile(serverDir, state.Port)
		return state.Port, false, nil
	}

	// If the server mode is External (explicit port in metadata.json,
	// shared server mode, etc.), do not start a per-project server —
	// it would conflict with the external one.
	mode := ResolveServerMode(beadsDir)
	if mode == ServerModeExternal {
		return reportExternalServerUnavailable(beadsDir)
	}

	// Defense-in-depth: if dolt.auto-start is explicitly disabled in
	// config.yaml or env, never spawn a server even if the caller
	// somehow reached this point (e.g. stale AutoStart=true in config).
	if IsAutoStartDisabled() {
		return reportAutoStartDisabled(beadsDir)
	}

	s, err := Start(serverDir)
	if err != nil {
		return 0, false, err
	}
	return s.Port, true, nil
}

func announceSharedServerUse() {
	if IsSharedServerMode() && os.Getenv("GT_ROOT") != "" {
		fmt.Fprintf(os.Stderr, "Info: Orchestrator detected (GT_ROOT set). Shared server uses port %d to avoid conflict.\n", DefaultSharedServerPort)
	}
}

func reportExternalServerUnavailable(beadsDir string) (int, bool, error) {
	cfg := DefaultConfig(beadsDir)
	if host, ok := externalNonLocalhostHost(beadsDir); ok {
		// "bd dolt start" is not the right advice for a non-localhost server
		// (GH#3518).
		return 0, false, fmt.Errorf("Dolt server at %s:%d is unreachable, and bd will not "+
			"start a local server because an external one is configured.\n\n"+
			"Verify the external server is running and reachable from this host:\n"+
			"  nc -zv %s %d  # or curl %s:%d for an HTTP-style check\n"+
			"  bd dolt status   # detailed external-server check",
			host, cfg.Port, host, cfg.Port, host, cfg.Port)
	}
	return 0, false, fmt.Errorf("Dolt server is not running on port %d, and auto-start is suppressed "+
		"because the server is externally managed (dolt.auto-start: false or explicit port configured).\n\n"+
		"Start the external server, or enable auto-start to allow bd to manage the server.\n"+
		"  To start manually: bd dolt start\n"+
		"  To check status: bd dolt status", cfg.Port)
}

func reportAutoStartDisabled(beadsDir string) (int, bool, error) {
	cfg := DefaultConfig(beadsDir)
	if host, ok := externalNonLocalhostHost(beadsDir); ok {
		return 0, false, fmt.Errorf("Configured Dolt server at %s:%d is unreachable, and auto-start "+
			"is disabled (dolt.auto-start: false in config.yaml or BEADS_DOLT_AUTO_START=0).\n\n"+
			"This is an external server; bd will not start it. Verify it is running:\n"+
			"  nc -zv %s %d  # or curl %s:%d for an HTTP-style check\n"+
			"  bd dolt status   # detailed external-server check",
			host, cfg.Port, host, cfg.Port, host, cfg.Port)
	}
	return 0, false, fmt.Errorf("Dolt server unreachable (port %d) and auto-start is disabled "+
		"(dolt.auto-start: false in config.yaml or BEADS_DOLT_AUTO_START=0).\n\n"+
		"Start the server manually or enable auto-start.\n"+
		"  To start manually: bd dolt start\n"+
		"  To check status: bd dolt status", cfg.Port)
}

// doltServerLogLevel is the --loglevel value passed to `dolt sql-server`.
//
// Dolt's sql-server logs every new connection and connection close at INFO
// level (`msg=NewConnection` / `msg=ConnectionClosed`). Because beads opens
// a fresh MySQL connection for each `bd` invocation, a busy project can
// produce millions of lines of connection churn noise, which in one field
// report filled dolt-server.log with ~380 MB of useless entries, generated
// significant btrfs write pressure, and buried real error signals.
//
// Raising the floor to `warning` silences that chatter while still surfacing
// warnings, errors, and fatal messages. Valid dolt levels are:
// trace, debug, info, warning, error, fatal.
const doltServerLogLevel = "warning"

// ServerSpawnEnv returns the environment for a spawned dolt sql-server (the
// directly-managed spawn here and the proxied one in dbproxy/server). The
// server runs CALL DOLT_PUSH/FETCH itself, so the guards must be in the
// child's environment: templated git hooks disabled (GH#4272; approach from
// PR #4281 by pmgledhill102) and stderr-directed git tracing scrubbed (see
// internal/gittraceenv).
func ServerSpawnEnv() []string {
	return githooksenv.DisabledEnv(gittraceenv.ScrubEnv(os.Environ()))
}

// buildDoltServerArgs returns the argv passed to `dolt` (excluding argv[0]/
// the binary itself). It is factored out of Start so it can be asserted on
// in unit tests without spawning a real server.
//
// The `--loglevel` flag MUST be included here — see doltServerLogLevel for
// the rationale. If you remove or reorder these args, update the tests in
// doltserver_test.go accordingly.
//
// When debug is true, the argv begins with `--prof cpu --prof-path <profDir>`.
// These top-level dolt flags MUST appear before the `sql-server` subcommand:
// dolt's argv loop stops scanning debug flags on the first unknown token
// (see ~/cursor_src/dolt/go/cmd/dolt/dolt.go runMain). The caller must
// ensure profDir already exists — dolt panics if it does not.
//
// Debug mode also raises --loglevel from the default warning to debug;
// the connection-log spam concern that motivated the warning floor is
// the price of opting into debug.
func buildDoltServerArgs(host string, port int, debug bool, profDir string) []string {
	var args []string
	if debug {
		args = append(args, "--prof", "cpu", "--prof-path", profDir)
	}
	args = append(args,
		"sql-server",
		"-H", host,
		"-P", strconv.Itoa(port),
	)
	if debug {
		args = append(args, "--loglevel=debug")
	} else {
		args = append(args, "--loglevel="+doltServerLogLevel)
	}
	return args
}

// doltServerConfigFileName is the YAML config Start() writes when the
// resolved external dolt binary supports auto_gc_behavior.archive_level
// (see SupportsArchiveLevelConfig). It lives alongside the other gitignored
// server state files in beadsDir, not inside the Dolt data directory.
const doltServerConfigFileName = "dolt-server-config.yaml"

func doltServerConfigPath(beadsDir string) string {
	return filepath.Join(beadsDir, doltServerConfigFileName)
}

// doltCfgDirName mirrors commands.DefaultCfgDirName in the pinned dolt
// module (cmd/dolt/commands/sql.go) — the on-disk directory name Dolt looks
// in for privileges.db (users/passwords) and branch_control.db.
const doltCfgDirName = ".doltcfg"

// ErrMultipleDoltCfgDirs is returned by resolveCfgDir when both a parent
// and a data-directory .doltcfg exist. Mirrors Dolt's own ambiguous case
// (commands.ErrMultipleDoltCfgDirs in the pinned module) — guessing which
// one to use risks the same silent user/branch-control loss this function
// exists to prevent, so this is surfaced as a hard Start() failure instead.
var ErrMultipleDoltCfgDirs = errors.New("multiple .doltcfg directories detected")

// resolveCfgDir replicates Dolt's own flag-mode .doltcfg discovery
// (setupDoltConfig in cmd/dolt/commands/sqlserver/sqlserver.go, pinned
// module) for our generated --config YAML. setupDoltConfig returns
// immediately when --config is passed, so a deployment that previously ran
// this launcher in CLI-flag mode — where dolt auto-discovers a parent
// ../.doltcfg holding privileges.db/branch_control.db — would otherwise
// silently fall back to a fresh $data_dir/.doltcfg under --config mode:
// existing users and branch controls abandoned, new passwordless root
// (gastownhall/beads#4986).
//
// doltDir is the server's data directory (== process cwd, since neither
// --data-dir nor --doltcfg-dir are ever passed). Mirrors Dolt exactly:
//   - parent ../.doltcfg (relative to doltDir) if it exists and is a dir
//   - else doltDir/.doltcfg
//   - ErrMultipleDoltCfgDirs if BOTH exist — ambiguous, matches Dolt's own
//     ErrMultipleDoltCfgDirs case rather than guessing.
//
// doltDir is resolved to its physical (symlink-free) location before
// computing the parent. filepath.Join/Clean strip ".." lexically without
// touching the filesystem: if doltDir is itself a symlink, a naive
// filepath.Join(doltDir, "..") resolves to the symlink's own lexical
// parent directory, not the physical parent of whatever it points at. On
// Linux, chdir into a symlink makes the kernel track the process's cwd
// physically, so the actual `dolt` child process's own "../.doltcfg"
// lookup (a bare relative path, resolved against its real, physically
// tracked cwd — see setupDoltConfig) sees the PHYSICAL parent. Using
// filepath.EvalSymlinks here keeps this resolution consistent with what
// the child process itself does; skipping it would let a symlinked data
// directory miss the very parent .doltcfg this function exists to find,
// re-abandoning existing privileges/branch-control (gastownhall/beads#4986
// round 3).
//
// Returns an absolute path.
func resolveCfgDir(doltDir string) (string, error) {
	// Fall back to the lexical doltDir on error (e.g. it does not exist
	// yet) rather than failing outright — resolveCfgDir has no dependency
	// on doltDir already existing beyond this best-effort symlink check,
	// and ensureDoltInit already runs before this is called from Start().
	physicalDoltDir := physicalDoltPath(doltDir)

	parentDirCfg := filepath.Join(physicalDoltDir, "..", doltCfgDirName)
	parentExists := isDirectory(parentDirCfg)
	currDirCfg := filepath.Join(physicalDoltDir, doltCfgDirName)
	currExists := isDirectory(currDirCfg)

	if parentExists && currExists {
		absParent := absolutePathOrOriginal(parentDirCfg)
		absCurr := absolutePathOrOriginal(currDirCfg)
		return "", fmt.Errorf("%w: %q and %q; remove or merge one before starting the managed dolt server "+
			"(matches Dolt's own ambiguous-.doltcfg case; see --doltcfg-dir in `dolt sql-server --help`)",
			ErrMultipleDoltCfgDirs, absParent, absCurr)
	}

	chosen := currDirCfg
	if parentExists {
		chosen = parentDirCfg
	}
	abs, err := filepath.Abs(chosen)
	if err != nil {
		return "", fmt.Errorf("resolving .doltcfg path %q: %w", chosen, err)
	}
	return abs, nil
}

func physicalDoltPath(doltDir string) string {
	if resolved, err := filepath.EvalSymlinks(doltDir); err == nil {
		return resolved
	}
	return doltDir
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func absolutePathOrOriginal(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

type doltServerYAMLConfig struct {
	LogLevel string `yaml:"log_level"`
	Listener struct {
		Host string `yaml:"host"`
		Port int    `yaml:"port"`
	} `yaml:"listener"`
	CfgDir   string `yaml:"cfg_dir"`
	Behavior struct {
		AutoGCBehavior struct {
			ArchiveLevel int `yaml:"archive_level"`
		} `yaml:"auto_gc_behavior"`
	} `yaml:"behavior"`
}

// buildDoltServerYAMLConfig renders a minimal sql-server YAML config
// equivalent to buildDoltServerArgs' CLI flags (host, port, log level), plus
// auto_gc_behavior.archive_level: 0 so this managed server's background
// auto-GC writes classic Snappy table files instead of zstd archives
// (gastownhall/beads#4986). Auto-GC itself is left enabled (archive_level's
// sibling "enable" key is omitted, which Dolt defaults to true). cfgDir must
// be pre-resolved via resolveCfgDir — --config mode skips Dolt's own
// flag-mode .doltcfg discovery entirely, so this is the one place that
// behavior must be replicated.
//
// Callers MUST only use this when SupportsArchiveLevelConfig(doltBin)
// reports true — an older external dolt's own YAMLConfig struct may lack
// this field, and Dolt's YAML loader uses yaml.UnmarshalStrict, so an
// unrecognized key is a hard parse error at server startup, not a
// silently-ignored one.
func buildDoltServerYAMLConfig(host string, port int, debug bool, cfgDir string) ([]byte, error) {
	logLevel := doltServerLogLevel
	if debug {
		logLevel = "debug"
	}
	var cfg doltServerYAMLConfig
	cfg.LogLevel = logLevel
	cfg.Listener.Host = host
	cfg.Listener.Port = port
	cfg.CfgDir = cfgDir
	return yaml.Marshal(cfg)
}

// buildDoltServerArgsWithConfig is the --config counterpart to
// buildDoltServerArgs. Dolt's sql-server subcommand ignores all other
// command-line parameters when --config is present, so host/port/log-level
// must come from the YAML file at configPath (see buildDoltServerYAMLConfig)
// rather than from flags here. The top-level --prof/--prof-path flags are
// unaffected — they are consumed by the outer `dolt` command dispatcher
// before the sql-server subcommand's own arg parser ever sees --config.
func buildDoltServerArgsWithConfig(configPath string, debug bool, profDir string) []string {
	var args []string
	if debug {
		args = append(args, "--prof", "cpu", "--prof-path", profDir)
	}
	args = append(args, "sql-server", "--config", configPath)
	return args
}
