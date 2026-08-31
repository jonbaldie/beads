// repro-dolt-prod-timeouts runs production-shaped bd CLI timeout scenarios.
//
// It initializes a real server-mode beads workspace, bulk-loads a graph that
// mirrors a large production deployment's skew (large mostly-closed issue table, large
// dependency table, small active frontier), then forks actual bd commands.
//
// Usage:
//
//	go run ./scripts/repro-dolt-prod-timeouts --bd ./bd --scenario all
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jonbaldie/beads/internal/storage/doltutil"
)

func runWithArgs(ctx context.Context, args []string) error {
	cfg, err := parseFlags(args)
	if err != nil {
		return err
	}
	bdPath, err := resolveExecutablePath(cfg.BDPath)
	if err != nil {
		return err
	}
	cfg.BDPath = bdPath

	ws, err := openOrCreateWorkspace(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanupWorkspace(cfg, ws)

	fmt.Printf("workspace=%s port=%d database=%s\n", ws.Dir, ws.Port, ws.Database)
	if err := loadProductionShape(ctx, ws, cfg); err != nil {
		return err
	}

	scenarios, err := scenarioNames(cfg.Scenario)
	if err != nil {
		return err
	}
	runScenarios(ctx, cfg, ws, scenarios)
	return nil
}

func cleanupWorkspace(cfg config, ws *workspace) {
	if cfg.Workspace == "" {
		stopWorkspace(context.Background(), cfg, ws)
	}
	if cfg.KeepWorkdir {
		fmt.Printf("kept workdir: %s\n", ws.Dir)
		return
	}
	if cfg.Workspace == "" {
		_ = os.RemoveAll(ws.Dir)
	}
}

func runScenarios(ctx context.Context, cfg config, ws *workspace, scenarios []string) {
	for _, scenario := range scenarios {
		report(scenario, runScenario(ctx, cfg, ws, scenario))
	}
}

func parseFlags(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("repro-dolt-prod-timeouts", flag.ContinueOnError)
	fs.StringVar(&cfg.BDPath, "bd", "bd", "bd binary to execute")
	fs.StringVar(&cfg.Workspace, "workspace", "", "existing workspace to test instead of creating a synthetic one")
	fs.StringVar(&cfg.SeedMode, "seed-mode", "full", "fixture seed mode: full, dep-only, none")
	fs.StringVar(&cfg.Scenario, "scenario", "all", "scenario: ready, dep, control, mixed, outage, cycle-current, cycle-deps-only, cycle-wisps-only, cycle-bfs, all")
	fs.IntVar(&cfg.IssueCount, "issues", 100000, "issue rows to seed")
	fs.IntVar(&cfg.DepCount, "deps", 85000, "dependency rows to seed")
	fs.IntVar(&cfg.Concurrency, "concurrency", 20, "concurrent bd processes")
	fs.IntVar(&cfg.Ops, "ops", 80, "total operations per scenario")
	fs.DurationVar(&cfg.Timeout, "timeout", 30*time.Second, "per-command timeout")
	fs.IntVar(&cfg.ChainDepth, "chain-depth", 0, "existing blocking chain depth behind each dep-add target")
	fs.BoolVar(&cfg.KeepWorkdir, "keep-workdir", false, "keep temp workspace")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if cfg.Workspace != "" && !flagPassed(fs, "seed-mode") {
		cfg.SeedMode = "none"
	}
	return cfg, nil
}

func flagPassed(fs *flag.FlagSet, name string) bool {
	passed := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			passed = true
		}
	})
	return passed
}

func resolveExecutablePath(path string) (string, error) {
	bdPath, err := exec.LookPath(path)
	if err != nil {
		return "", fmt.Errorf("find bd binary %q: %w", path, err)
	}
	absPath, err := filepath.Abs(bdPath)
	if err != nil {
		return "", fmt.Errorf("resolve bd binary %q: %w", bdPath, err)
	}
	return absPath, nil
}

var allScenarioNames = []string{
	"ready",
	"dep",
	"control",
	"mixed",
	"outage",
	"cycle-current",
	"cycle-deps-only",
	"cycle-wisps-only",
	"cycle-bfs",
}

func scenarioNames(scenario string) ([]string, error) {
	if scenario == "all" {
		return append([]string(nil), allScenarioNames...), nil
	}
	for _, name := range allScenarioNames {
		if scenario == name {
			return []string{scenario}, nil
		}
	}
	return nil, fmt.Errorf("unknown scenario %q", scenario)
}

func runScenario(ctx context.Context, cfg config, ws *workspace, scenario string) []opResult {
	switch scenario {
	case "ready", "dep", "control", "mixed", "outage":
		return runPrimaryScenario(ctx, cfg, ws, scenario)
	case "cycle-current", "cycle-deps-only", "cycle-wisps-only", "cycle-bfs":
		return runCycleNamedScenario(ctx, cfg, ws, scenario)
	default:
		return []opResult{{Kind: scenario, Err: fmt.Sprintf("unknown scenario %q", scenario)}}
	}
}

func runPrimaryScenario(ctx context.Context, cfg config, ws *workspace, scenario string) []opResult {
	switch scenario {
	case "ready":
		return runReadyScenario(ctx, cfg, ws)
	case "dep":
		return runDepScenario(ctx, cfg, ws)
	case "control":
		return runControlQueryScenario(ctx, cfg, ws)
	case "mixed":
		return runMixedCityLoadScenario(ctx, cfg, ws)
	default:
		return runOutageScenario(ctx, cfg, ws)
	}
}

func runCycleNamedScenario(ctx context.Context, cfg config, ws *workspace, scenario string) []opResult {
	checks := map[string]cycleCheckFunc{
		"cycle-current":    cycleCheckCurrentSQL,
		"cycle-deps-only":  cycleCheckDependenciesOnlySQL,
		"cycle-wisps-only": cycleCheckWispsOnlySQL,
		"cycle-bfs":        cycleCheckBatchedBFS,
	}
	return runCycleCheckScenario(ctx, cfg, ws, checks[scenario])
}

func openOrCreateWorkspace(ctx context.Context, cfg config) (*workspace, error) {
	if cfg.Workspace != "" {
		return openWorkspace(ctx, cfg, cfg.Workspace)
	}
	return createWorkspace(ctx, cfg)
}

func openWorkspace(ctx context.Context, cfg config, dir string) (*workspace, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	beadsDir := filepath.Join(absDir, ".beads")
	if _, err := os.Stat(beadsDir); err != nil {
		return nil, fmt.Errorf("open .beads in %s: %w", absDir, err)
	}
	port, err := readInt(filepath.Join(beadsDir, "dolt-server.port"))
	if err != nil {
		return nil, fmt.Errorf("read dolt-server.port: %w", err)
	}
	if !isPortOpen(port) {
		if err := startWorkspaceDolt(ctx, cfg, absDir); err != nil {
			return nil, err
		}
		port, err = readInt(filepath.Join(beadsDir, "dolt-server.port"))
		if err != nil {
			return nil, fmt.Errorf("read dolt-server.port: %w", err)
		}
	}
	database, err := readDoltDatabase(filepath.Join(beadsDir, "metadata.json"))
	if err != nil {
		return nil, err
	}
	return &workspace{Dir: absDir, BeadsDir: beadsDir, Port: port, Database: database}, nil
}

func isPortOpen(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second) // #nosec G704 -- loopback probe of a locally started dolt server; not attacker-controlled
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func createWorkspace(ctx context.Context, cfg config) (*workspace, error) {
	dir, err := os.MkdirTemp("", "bd-prod-timeout-*")
	if err != nil {
		return nil, err
	}

	initTimeout := cfg.Timeout * 4
	if initTimeout < 2*time.Minute {
		initTimeout = 2 * time.Minute
	}
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()

	fmt.Printf("initializing server workspace timeout=%s\n", initTimeout)
	cmd := exec.CommandContext(initCtx, cfg.BDPath, // #nosec G702 -- fixed subcommand args; cfg.BDPath is an operator-supplied local binary path, not attacker input
		"init",
		"--server",
		"--prefix=perf",
		"--non-interactive",
		"--quiet",
		"--skip-hooks",
		"--skip-agents",
	)
	cmd.Dir = dir
	cmd.Env = subprocessEnv("BD_NON_INTERACTIVE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("bd init after %s: %w\n%s", initTimeout, err, string(out))
	}

	beadsDir := filepath.Join(dir, ".beads")
	port, err := readInt(filepath.Join(beadsDir, "dolt-server.port"))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("read dolt-server.port: %w", err)
	}
	database, err := readDoltDatabase(filepath.Join(beadsDir, "metadata.json"))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &workspace{Dir: dir, BeadsDir: beadsDir, Port: port, Database: database}, nil
}

func startWorkspaceDolt(ctx context.Context, cfg config, dir string) error {
	startTimeout := cfg.Timeout * 2
	if startTimeout < time.Minute {
		startTimeout = time.Minute
	}
	startCtx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	cmd := exec.CommandContext(startCtx, cfg.BDPath, "dolt", "start") // #nosec G702 -- fixed subcommand args; cfg.BDPath is an operator-supplied local binary path, not attacker input
	cmd.Dir = dir
	cmd.Env = subprocessEnv("BD_NON_INTERACTIVE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bd dolt start after %s: %w\n%s", startTimeout, err, string(out))
	}
	return nil
}

func readDoltDatabase(path string) (string, error) {
	//nolint:gosec // G304: benchmark harness reads metadata from the selected workspace.
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read metadata.json: %w", err)
	}
	var meta struct {
		DoltDatabase string `json:"dolt_database"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("parse metadata.json: %w", err)
	}
	if meta.DoltDatabase == "" {
		return "", fmt.Errorf("metadata.json missing dolt_database")
	}
	return meta.DoltDatabase, nil
}

func stopWorkspace(ctx context.Context, cfg config, ws *workspace) {
	cmd := exec.CommandContext(ctx, cfg.BDPath, "dolt", "stop") //nolint:gosec // G702: operator-selected local bd binary with fixed arguments.
	cmd.Dir = ws.Dir
	cmd.Env = subprocessEnv("BD_NON_INTERACTIVE=1")
	_ = cmd.Run()
}

func loadProductionShape(ctx context.Context, ws *workspace, cfg config) error {
	if cfg.SeedMode == "none" {
		fmt.Printf("seed skipped seed_mode=%s\n", cfg.SeedMode)
		return nil
	}

	dsn := doltutil.ServerDSN{
		Host:     "127.0.0.1",
		Port:     ws.Port,
		User:     "root",
		Database: ws.Database,
		Timeout:  cfg.Timeout,
	}.String()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	start := time.Now()
	if err := seedProductionShape(ctx, db, cfg); err != nil {
		return err
	}
	fmt.Printf("seeded mode=%s issues=%d deps=%d in %s\n", cfg.SeedMode, cfg.IssueCount, cfg.DepCount, time.Since(start).Round(time.Millisecond))
	return nil
}

func seedProductionShape(ctx context.Context, db *sql.DB, cfg config) error {
	if err := seedRecords(ctx, db, cfg); err != nil {
		return err
	}
	if cfg.ChainDepth > 0 {
		if err := insertDepAddChains(ctx, db, cfg.Ops, cfg.ChainDepth); err != nil {
			return err
		}
	}
	return commitSeed(ctx, db)
}

func seedRecords(ctx context.Context, db *sql.DB, cfg config) error {
	switch cfg.SeedMode {
	case "full":
		if err := insertIssues(ctx, db, cfg.IssueCount, cfg.Ops, cfg.ChainDepth); err != nil {
			return err
		}
		return insertDependencies(ctx, db, cfg.DepCount, cfg.IssueCount)
	case "dep-only":
		return insertDepIssues(ctx, db, cfg.Ops, cfg.ChainDepth)
	default:
		return fmt.Errorf("unknown seed mode %q", cfg.SeedMode)
	}
}

func commitSeed(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, "CALL DOLT_ADD('-A')"); err != nil {
		return fmt.Errorf("DOLT_ADD fixture: %w", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-m', 'seed production timeout fixture')"); err != nil {
		return fmt.Errorf("DOLT_COMMIT fixture: %w", err)
	}
	return nil
}
