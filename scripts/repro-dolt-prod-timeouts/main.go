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
	"log"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type config struct {
	BDPath        string
	Workspace     string
	SeedMode      string
	Scenario      string
	IssueCount    int
	DepCount      int
	Concurrency   int
	Ops           int
	Timeout       time.Duration
	ChainDepth    int
	KeepWorkdir   bool
	ManagedServer bool
}

type workspace struct {
	Dir      string
	BeadsDir string
	Port     int
	Database string
}

type opResult struct {
	Kind       string        `json:"kind"`
	Argv       []string      `json:"argv"`
	Latency    time.Duration `json:"latency"`
	TimedOut   bool          `json:"timed_out"`
	Err        string        `json:"err,omitempty"`
	StderrTail string        `json:"stderr_tail,omitempty"`
}

type job struct {
	Kind string
	Argv []string
	Env  []string
	Sh   string
}

var subprocessEnvDenylist = []string{
	"BEADS_DIR",
	"BEADS_DOLT_SERVER_PORT",
	"BEADS_DOLT_PORT",
	"BEADS_DOLT_SERVER_HOST",
	"BEADS_DOLT_SERVER_SOCKET",
}

const dependencyTargetExpr = "COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external)"

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	return runWithArgs(ctx, os.Args[1:])
}
