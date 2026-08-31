package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jonbaldie/beads/internal/storage/depid"
	"github.com/jonbaldie/beads/internal/storage/doltutil"
	"github.com/jonbaldie/beads/internal/storage/schema"
	"github.com/jonbaldie/beads/internal/testutil"
)

func TestIsDriverReadTimeout(t *testing.T) {
	result := opResult{
		StderrTail: "[mysql] 2026/05/13 18:13:48 packets.go:58 read tcp 127.0.0.1:39308->127.0.0.1:21791: i/o timeout",
	}
	if !isDriverReadTimeout(result) {
		t.Fatal("expected MySQL driver read timeout to be classified")
	}
}

func TestIsDriverReadTimeoutIgnoresHarnessTimeout(t *testing.T) {
	result := opResult{
		TimedOut:   true,
		StderrTail: "signal: killed",
	}
	if isDriverReadTimeout(result) {
		t.Fatal("harness timeout should not be classified as driver read timeout")
	}
}

func TestMixedBackgroundJobsIncludesSessionLoadShapes(t *testing.T) {
	jobs := mixedBackgroundJobs(12)
	seen := map[string]bool{}
	for _, job := range jobs {
		seen[job.Kind] = true
	}
	for _, want := range []string{"session-ready", "control-ready", "route-ready", "show", "list", "claim"} {
		if !seen[want] {
			t.Fatalf("mixed background jobs missing %q; seen=%v", want, seen)
		}
	}
}

func TestDepFixtureIssueCountIncludesChainTargets(t *testing.T) {
	if got := depFixtureIssueCount(10, 0); got != 20 {
		t.Fatalf("without chains got %d, want 20", got)
	}
	if got := depFixtureIssueCount(10, 100); got != 1132 {
		t.Fatalf("with chains got %d, want 1132", got)
	}
}

func TestResolveExecutablePathReturnsAbsolutePath(t *testing.T) {
	tmp := t.TempDir()
	bd := writeNativeBDTestExecutable(t, tmp)
	t.Chdir(tmp)

	got, err := resolveExecutablePath("./bd")
	if err != nil {
		t.Fatal(err)
	}
	if got != bd {
		t.Fatalf("got %q, want %q", got, bd)
	}
}

func TestParseFlagsDefaultsSyntheticWorkspaceToFullSeed(t *testing.T) {
	cfg, err := parseFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SeedMode != "full" {
		t.Fatalf("SeedMode = %q, want full", cfg.SeedMode)
	}
}

func TestParseFlagsDefaultsExistingWorkspaceToNoSeed(t *testing.T) {
	cfg, err := parseFlags([]string{"--workspace", "/tmp/existing-beads-workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SeedMode != "none" {
		t.Fatalf("SeedMode = %q, want none", cfg.SeedMode)
	}
}

func TestParseFlagsPreservesExplicitExistingWorkspaceSeedMode(t *testing.T) {
	cfg, err := parseFlags([]string{"--workspace", "/tmp/existing-beads-workspace", "--seed-mode", "full"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SeedMode != "full" {
		t.Fatalf("SeedMode = %q, want full", cfg.SeedMode)
	}
}

func TestParseFlagsReturnsEveryDefaultAndOverride(t *testing.T) {
	defaults, err := parseFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	wantDefaults := config{
		BDPath:      "bd",
		SeedMode:    "full",
		Scenario:    "all",
		IssueCount:  100000,
		DepCount:    85000,
		Concurrency: 20,
		Ops:         80,
		Timeout:     30 * time.Second,
	}
	if !reflect.DeepEqual(defaults, wantDefaults) {
		t.Fatalf("parseFlags(nil) = %#v, want %#v", defaults, wantDefaults)
	}

	got, err := parseFlags([]string{
		"--bd", "custom-bd", "--workspace", "/workspace", "--seed-mode", "dep-only",
		"--scenario", "dep", "--issues", "7", "--deps", "8", "--concurrency", "9",
		"--ops", "10", "--timeout", "11s", "--chain-depth", "12", "--keep-workdir",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := config{
		BDPath:      "custom-bd",
		Workspace:   "/workspace",
		SeedMode:    "dep-only",
		Scenario:    "dep",
		IssueCount:  7,
		DepCount:    8,
		Concurrency: 9,
		Ops:         10,
		Timeout:     11 * time.Second,
		ChainDepth:  12,
		KeepWorkdir: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseFlags(overrides) = %#v, want %#v", got, want)
	}
	if _, err := parseFlags([]string{"--issues", "not-a-number"}); err == nil {
		t.Fatal("parseFlags accepted malformed integer")
	}
}

func TestSeedPureHelpersExact(t *testing.T) {
	for _, tc := range []struct {
		start, batch, total, want int
	}{
		{0, 500, 1200, 500},
		{1000, 500, 1200, 1200},
		{1200, 500, 1200, 1200},
	} {
		if got := minSeedEnd(tc.start, tc.batch, tc.total); got != tc.want {
			t.Fatalf("minSeedEnd(%d, %d, %d) = %d, want %d", tc.start, tc.batch, tc.total, got, tc.want)
		}
	}

	rows := []struct {
		i, count int
		want     issueSeedRowValues
	}{
		{0, 100, issueSeedRowValues{"perf-000000", "open", "example-org--control-dispatcher", "{}", 1}},
		{39, 100, issueSeedRowValues{"perf-000039", "open", "example-org--control-dispatcher", "{}", 4}},
		{40, 100, issueSeedRowValues{"perf-000040", "open", "", `{"route.routed_to":"example-org/control-dispatcher"}`, 1}},
		{79, 100, issueSeedRowValues{"perf-000079", "open", "", `{"route.routed_to":"example-org/control-dispatcher"}`, 4}},
		{80, 100, issueSeedRowValues{"perf-000080", "open", "", "{}", 1}},
		{350, 400, issueSeedRowValues{"perf-000350", "closed", "", "{}", 3}},
		{100, 100, issueSeedRowValues{"perf-dep-000000", "open", "", "{}", 1}},
	}
	for _, tc := range rows {
		if got := issueSeedRow(tc.i, tc.count); got != tc.want {
			t.Fatalf("issueSeedRow(%d, %d) = %#v, want %#v", tc.i, tc.count, got, tc.want)
		}
	}

	for _, tc := range []struct {
		ops, depth, want int
	}{{10, -1, 20}, {10, 0, 20}, {10, 1, 43}, {10, 100, 1132}} {
		if got := depFixtureIssueCount(tc.ops, tc.depth); got != tc.want {
			t.Fatalf("depFixtureIssueCount(%d, %d) = %d, want %d", tc.ops, tc.depth, got, tc.want)
		}
	}

	for _, tc := range []struct {
		i, count                        int
		wantIssue, wantTarget, wantType string
	}{
		{0, 100, "perf-000000", "perf-000002", "blocks"},
		{19, 100, "perf-000019", "perf-000021", "blocks"},
		{20, 100, "perf-000020", "perf-000023", "blocks"},
		{40, 100, "perf-000040", "perf-000042", "blocks"},
		{59, 100, "perf-000059", "perf-000061", "blocks"},
		{60, 100, "perf-000060", "perf-000063", "blocks"},
		{4999, 100, "perf-000099", "perf-000051", "blocks"},
		{5000, 100, "perf-000000", "perf-000053", "parent-child"},
	} {
		issue, target, typ := dependencySeedRow(tc.i, tc.count)
		if issue != tc.wantIssue || target != tc.wantTarget || typ != tc.wantType {
			t.Fatalf("dependencySeedRow(%d, %d) = (%q, %q, %q), want (%q, %q, %q)", tc.i, tc.count, issue, target, typ, tc.wantIssue, tc.wantTarget, tc.wantType)
		}
	}

	for _, tc := range []struct {
		count, want int
		wantErr     bool
	}{{-1, 0, true}, {0, 0, true}, {1, 1, false}, {2, 2, false}, {5, 20, false}} {
		got, err := maxDependencyPairs(tc.count)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Fatalf("maxDependencyPairs(%d) = (%d, %v), want (%d, error=%v)", tc.count, got, err, tc.want, tc.wantErr)
		}
	}

	for _, tc := range []struct {
		i, count, sourceOffset, targetOffset int
		wantSource, wantTarget               string
	}{
		{0, 1, 1000, 300, "perf-000000", "perf-000000"},
		{0, 5, 0, 200, "perf-000000", "perf-000004"},
		{4, 5, 0, 200, "perf-000004", "perf-000003"},
		{5, 5, 0, 200, "perf-000000", "perf-000001"},
		{7, 5, 1000, 300, "perf-000002", "perf-000003"},
	} {
		source, target := dependencyEndpoints(tc.i, tc.count, tc.sourceOffset, tc.targetOffset)
		if source != tc.wantSource || target != tc.wantTarget {
			t.Fatalf("dependencyEndpoints(%d, %d, %d, %d) = (%q, %q), want (%q, %q)", tc.i, tc.count, tc.sourceOffset, tc.targetOffset, source, target, tc.wantSource, tc.wantTarget)
		}
	}
}

func TestBuildIssueInsertBatchExact(t *testing.T) {
	query, args := buildIssueInsertBatch(0, 2, 1)
	wantQuery := `INSERT INTO issues
		(id, title, description, design, acceptance_criteria, notes,
		 status, priority, issue_type, assignee, metadata)
		VALUES (?,?,?,?,?,?,?,?,?,?,?),(?,?,?,?,?,?,?,?,?,?,?)`
	if query.String() != wantQuery {
		t.Fatalf("query = %q, want %q", query.String(), wantQuery)
	}
	wantArgs := []any{
		"perf-000000", "prod timeout issue 0", "fixture", "", "", "", "open", 1, "task", "example-org--control-dispatcher", "{}",
		"perf-dep-000000", "prod timeout issue 1", "fixture", "", "", "", "open", 2, "task", "example-org--control-dispatcher", "{}",
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", args, wantArgs)
	}

	emptyQuery, emptyArgs := buildIssueInsertBatch(4, 4, 10)
	if strings.Contains(emptyQuery.String(), "(?)") || len(emptyArgs) != 0 {
		t.Fatalf("empty batch = (%q, %#v)", emptyQuery.String(), emptyArgs)
	}
}

func TestSeedInsertErrorsRetainBatchAndCause(t *testing.T) {
	wantErr := errors.New("database unavailable")
	tests := []struct {
		name string
		run  func(context.Context, *sql.DB) error
		want string
	}{
		{"issues", func(ctx context.Context, db *sql.DB) error { return insertIssues(ctx, db, 1, 0, 0) }, "insert issues 0-1"},
		{"dep issues", func(ctx context.Context, db *sql.DB) error { return insertDepIssues(ctx, db, 1, 1) }, "insert dep issues 0-7"},
		{"dependencies", func(ctx context.Context, db *sql.DB) error { return insertDependencies(ctx, db, 1, 2) }, "insert dependencies 0-1"},
		{"chains", func(ctx context.Context, db *sql.DB) error { return insertDepAddChains(ctx, db, 1, 1) }, "insert dep-add chains 0-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectExec("INSERT INTO").WillReturnError(wantErr)
			err = tc.run(context.Background(), db)
			if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q wrapping cause", err, tc.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestScenarioJobBuildersExact(t *testing.T) {
	wantMixed := []job{
		{Kind: "session-ready", Argv: []string{"ready", "--include-ephemeral", "--assignee=mc-0000000", "--json", "--limit=1"}},
		{Kind: "control-ready", Argv: []string{"--readonly", "--sandbox", "ready", "--include-ephemeral", "--assignee=example-org--control-dispatcher", "--json", "--limit=20"}},
		{Kind: "route-ready", Argv: []string{"--readonly", "--sandbox", "ready", "--include-ephemeral", "--metadata-field", "route.routed_to=example-org/control-dispatcher", "--unassigned", "--json", "--limit=20"}},
		{Kind: "show", Argv: []string{"show", "perf-000003", "--json"}},
		{Kind: "list", Argv: []string{"list", "--json", "--status", "in_progress", "--assignee=mc-0000004", "--limit=1"}},
		{Kind: "claim", Argv: []string{"update", "perf-000005", "--claim", "--json"}},
	}
	if got := mixedBackgroundJobs(6); !reflect.DeepEqual(got, wantMixed) {
		t.Fatalf("mixedBackgroundJobs(6) = %#v, want %#v", got, wantMixed)
	}
	if got := mixedBackgroundJobs(0); len(got) != 0 {
		t.Fatalf("mixedBackgroundJobs(0) = %#v, want empty", got)
	}
	if got := sessionAssignee(64); got != "mc-0000000" {
		t.Fatalf("sessionAssignee(64) = %q", got)
	}

	for _, tc := range []struct{ i, depth, want int }{{3, -1, 6}, {3, 0, 6}, {3, 1, 12}, {3, 5, 24}} {
		if got := depBase(tc.i, tc.depth); got != tc.want {
			t.Fatalf("depBase(%d, %d) = %d, want %d", tc.i, tc.depth, got, tc.want)
		}
	}
	wantDep := job{Kind: "dep", Argv: []string{"dep", "add", "perf-dep-000008", "perf-dep-000009", "--type", "blocks", "--json"}}
	if got := depAddJob(2, 1); !reflect.DeepEqual(got, wantDep) {
		t.Fatalf("depAddJob(2, 1) = %#v, want %#v", got, wantDep)
	}

	control := controlQueryJobs(5)
	if len(control) != 5 {
		t.Fatalf("controlQueryJobs(5) length = %d", len(control))
	}
	wantEnvs := [][]string{
		{"BD_EXPORT_AUTO=false", "GC_CONTROL_TARGET=control-dispatcher", "GC_CONTROL_SESSION_NAME=control-dispatcher", "GC_CONTROL_LEGACY_TARGET=workflow-control", "GC_SESSION_NAME=control-dispatcher", "GC_ALIAS=control-dispatcher", "GC_SESSION_ID=control-dispatcher"},
		{"BD_EXPORT_AUTO=false", "GC_CONTROL_TARGET=example-org/control-dispatcher", "GC_CONTROL_SESSION_NAME=example-org--control-dispatcher", "GC_CONTROL_LEGACY_TARGET=example-org/workflow-control", "GC_SESSION_NAME=example-org--control-dispatcher", "GC_ALIAS=example-org/control-dispatcher", "GC_SESSION_ID=example-org--control-dispatcher"},
		{"BD_EXPORT_AUTO=false", "GC_CONTROL_TARGET=example-gui/control-dispatcher", "GC_CONTROL_SESSION_NAME=example-gui--control-dispatcher", "GC_CONTROL_LEGACY_TARGET=example-gui/workflow-control", "GC_SESSION_NAME=example-gui--control-dispatcher", "GC_ALIAS=example-gui/control-dispatcher", "GC_SESSION_ID=example-gui--control-dispatcher"},
		{"BD_EXPORT_AUTO=false", "GC_CONTROL_TARGET=gtest-rig/control-dispatcher", "GC_CONTROL_SESSION_NAME=gtest-rig--control-dispatcher", "GC_CONTROL_LEGACY_TARGET=gtest-rig/workflow-control", "GC_SESSION_NAME=gtest-rig--control-dispatcher", "GC_ALIAS=gtest-rig/control-dispatcher", "GC_SESSION_ID=gtest-rig--control-dispatcher"},
	}
	for i, item := range control {
		if item.Kind != "control" || item.Sh != controlQueryScript() || !reflect.DeepEqual(item.Env, wantEnvs[i%4]) {
			t.Fatalf("controlQueryJobs(5)[%d] = %#v", i, item)
		}
	}
}

func TestResultHelpersExact(t *testing.T) {
	results := []opResult{{Latency: time.Millisecond}, {Latency: 2 * time.Millisecond}, {Latency: 10 * time.Millisecond}, {Latency: 20 * time.Millisecond}}
	for _, tc := range []struct {
		p    int
		want time.Duration
	}{{0, time.Millisecond}, {1, time.Millisecond}, {25, time.Millisecond}, {50, 2 * time.Millisecond}, {95, 20 * time.Millisecond}, {100, 20 * time.Millisecond}, {101, 20 * time.Millisecond}} {
		if got := percentile(results, tc.p); got != tc.want {
			t.Fatalf("percentile(results, %d) = %s, want %s", tc.p, got, tc.want)
		}
	}
	if got := percentile(nil, 50); got != 0 {
		t.Fatalf("percentile(nil, 50) = %s", got)
	}
	if got := tail("  abcdef  ", 4); got != "cdef" {
		t.Fatalf("tail = %q, want cdef", got)
	}
	if got := tail("  abc  ", 4); got != "abc" {
		t.Fatalf("tail short = %q, want abc", got)
	}
	if got := compactShell("  one\n  two\tthree "); got != "one two three" {
		t.Fatalf("compactShell = %q", got)
	}
	for _, tc := range []struct {
		stderr string
		want   bool
	}{
		{"packets.go:58 read tcp x: i/o timeout", true},
		{"packets.go:59 read tcp x: i/o timeout", false},
		{"packets.go:58 read tcp x: reset", false},
	} {
		if got := isDriverReadTimeout(opResult{StderrTail: tc.stderr}); got != tc.want {
			t.Fatalf("isDriverReadTimeout(%q) = %v, want %v", tc.stderr, got, tc.want)
		}
	}
}

func TestCleanEnvUsesHostKeySemantics(t *testing.T) {
	env := []string{
		"FIRST=keep-first",
		"beads_dir=drop-on-windows",
		"BEADS_DIR=drop-canonical-first",
		"ALLOWED=keep-duplicate-first",
		"MALFORMED",
		"BeAdS_DoLt_PoRt=drop-on-windows",
		"BEADS_DOLT_PORT=drop-canonical",
		"BEADS_DIR=drop-canonical-second",
		"BEADſ_DIR=keep-unicode-near-collision",
		"ALLOWED=keep-duplicate-second",
		"=C:=keep-windows-drive-entry",
		"LAST=keep-last",
	}

	got := cleanEnv(slices.Clone(env), "BEADS_DIR", "BEADS_DOLT_PORT")
	want := []string{
		"FIRST=keep-first",
		"beads_dir=drop-on-windows",
		"ALLOWED=keep-duplicate-first",
		"MALFORMED",
		"BeAdS_DoLt_PoRt=drop-on-windows",
		"BEADſ_DIR=keep-unicode-near-collision",
		"ALLOWED=keep-duplicate-second",
		"=C:=keep-windows-drive-entry",
		"LAST=keep-last",
	}
	if runtime.GOOS == "windows" {
		want = []string{
			"FIRST=keep-first",
			"ALLOWED=keep-duplicate-first",
			"MALFORMED",
			"BEADſ_DIR=keep-unicode-near-collision",
			"ALLOWED=keep-duplicate-second",
			"=C:=keep-windows-drive-entry",
			"LAST=keep-last",
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanEnv() = %q, want %q on %s", got, want, runtime.GOOS)
	}
}

func TestBenchmarkCommandBuildersStripDoltEnvOverrides(t *testing.T) {
	windowsSpellings := map[string]string{
		"BEADS_DIR":                "beads_dir",
		"BEADS_DOLT_SERVER_PORT":   "BeAdS_DoLt_SeRvEr_PoRt",
		"BEADS_DOLT_PORT":          "beads_dolt_port",
		"BEADS_DOLT_SERVER_HOST":   "BeAdS_DoLt_SeRvEr_HoSt",
		"BEADS_DOLT_SERVER_SOCKET": "beads_dolt_server_socket",
	}
	for _, key := range subprocessEnvDenylist {
		ambientKey := key
		if runtime.GOOS == "windows" {
			var ok bool
			ambientKey, ok = windowsSpellings[key]
			if !ok {
				t.Fatalf("missing mixed-case Windows spelling for denied key %q", key)
			}
		}
		t.Setenv(ambientKey, "ambient-"+strings.ToLower(key))
	}
	const allowedAmbient = "BEADS_REPRO_TIMEOUTS_ALLOWED_AMBIENT=present"
	allowedKey, allowedValue, _ := strings.Cut(allowedAmbient, "=")
	t.Setenv(allowedKey, allowedValue)

	cfg := config{BDPath: filepath.Join(t.TempDir(), "bd")}
	ws := &workspace{Dir: t.TempDir()}
	j := job{
		Kind: "env",
		Argv: []string{"status"},
		Env: []string{
			"BEADS_REPRO_TIMEOUTS_CONTROLLED=one",
			"BEADS_REPRO_TIMEOUTS_CONTROLLED_SECOND=two",
		},
		Sh: "printf test",
	}
	shellPath, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("find shell: %v", err)
	}

	tests := []struct {
		name       string
		build      func(context.Context, config, *workspace, job) *exec.Cmd
		wantPath   string
		wantArgs   []string
		wantSuffix []string
	}{
		{
			name:       "bd",
			build:      newBDCommand,
			wantPath:   cfg.BDPath,
			wantArgs:   []string{cfg.BDPath, "status"},
			wantSuffix: append([]string{"BD_NON_INTERACTIVE=1"}, j.Env...),
		},
		{
			name:     "shell",
			build:    newShellCommand,
			wantPath: shellPath,
			wantArgs: []string{"sh", "-c", j.Sh},
			wantSuffix: append([]string{
				"BD_NON_INTERACTIVE=1",
				"BD_BIN=" + cfg.BDPath,
			}, j.Env...),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := tt.build(context.Background(), cfg, ws, j)
			if cmd.Path != tt.wantPath {
				t.Fatalf("Path = %q, want %q", cmd.Path, tt.wantPath)
			}
			if !slices.Equal(cmd.Args, tt.wantArgs) {
				t.Fatalf("Args = %q, want %q", cmd.Args, tt.wantArgs)
			}
			if cmd.Dir != ws.Dir {
				t.Fatalf("Dir = %q, want %q", cmd.Dir, ws.Dir)
			}
			if len(cmd.Env) < len(tt.wantSuffix) || !slices.Equal(cmd.Env[len(cmd.Env)-len(tt.wantSuffix):], tt.wantSuffix) {
				t.Fatalf("Env suffix = %q, want %q", cmd.Env, tt.wantSuffix)
			}
			if !slices.Contains(cmd.Env, allowedAmbient) {
				t.Fatalf("Env dropped allowed ambient variable %q", allowedAmbient)
			}
			for _, key := range subprocessEnvDenylist {
				for _, entry := range cmd.Env {
					gotKey, _, _ := strings.Cut(entry, "=")
					if environmentKeyIdentity(gotKey) == environmentKeyIdentity(key) {
						t.Fatalf("Env retained ambient denied override %q", entry)
					}
				}
			}
		})
	}
}

func TestControlQueryScriptPreservesReadyProbeFailure(t *testing.T) {
	tmp := t.TempDir()
	bd := filepath.Join(tmp, "bd")
	if err := os.WriteFile(bd, []byte("#!/bin/sh\necho ready failed >&2\nexit 17\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config{BDPath: bd, Timeout: time.Second}
	ws := &workspace{Dir: tmp}
	result := runShell(context.Background(), cfg, ws, controlQueryJobs(1)[0])
	if result.Err == "" {
		t.Fatalf("expected control query shell to fail when bd ready fails; stderr=%q", result.StderrTail)
	}
	if !strings.Contains(result.StderrTail, "ready failed") {
		t.Fatalf("stderr tail = %q, want ready probe stderr", result.StderrTail)
	}
}

func writeNativeBDTestExecutable(t *testing.T, directory string) string {
	t.Helper()

	sourcePath, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve current test executable: %v", err)
	}
	targetPath := filepath.Join(directory, nativeBDExecutableName())

	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatalf("open current test executable: %v", err)
	}
	defer source.Close()

	target, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatalf("create native test executable: %v", err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		t.Fatalf("copy native test executable: %v", err)
	}
	if err := target.Close(); err != nil {
		t.Fatalf("close native test executable: %v", err)
	}
	return targetPath
}

func nativeBDExecutableName() string {
	if runtime.GOOS == "windows" {
		return "bd.exe"
	}
	return "bd"
}

func TestScenarioNamesAllIncludesEveryAdvertisedScenario(t *testing.T) {
	got, err := scenarioNames("all")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
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
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scenarioNames(all) got %v, want %v", got, want)
	}
}

func TestInsertDependenciesWritesTypedIssueTargetColumn(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO dependencies\s+\(id, issue_id, depends_on_issue_id, type, created_by, metadata\)\s+VALUES`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := insertDependencies(context.Background(), db, 1, 100000); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestInsertDependenciesUsesConfiguredIssueRange(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO dependencies\s+\(id, issue_id, depends_on_issue_id, type, created_by, metadata\)\s+VALUES`).
		WithArgs(
			depid.New("perf-000000", "perf-000004"), "perf-000000", "perf-000004", "blocks", "bench", "{}",
			depid.New("perf-000001", "perf-000000"), "perf-000001", "perf-000000", "blocks", "bench", "{}",
			depid.New("perf-000002", "perf-000001"), "perf-000002", "perf-000001", "blocks", "bench", "{}",
			depid.New("perf-000003", "perf-000002"), "perf-000003", "perf-000002", "blocks", "bench", "{}",
			depid.New("perf-000004", "perf-000003"), "perf-000004", "perf-000003", "blocks", "bench", "{}",
		).
		WillReturnResult(sqlmock.NewResult(0, 5))

	if err := insertDependencies(context.Background(), db, 5, 5); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestInsertDependenciesRejectsImpossibleUniquePairCount(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	err = insertDependencies(context.Background(), db, 3, 2)
	if err == nil {
		t.Fatal("expected impossible pair count error")
	}
	if !strings.Contains(err.Error(), "exceeds unique dependency pairs") {
		t.Fatalf("error = %v, want unique dependency pair failure", err)
	}
}

func TestInsertDepAddChainsWritesTypedIssueTargetColumn(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO dependencies\s+\(id, issue_id, depends_on_issue_id, type, created_by, metadata\)\s+VALUES`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := insertDepAddChains(context.Background(), db, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCycleCheckCurrentSQLUsesTypedDependencyTargetProjection(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(dependencyTargetExpr)).
		WithArgs("perf-000002", "perf-000001").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(0))

	if err := cycleCheckCurrentSQL(context.Background(), db, "perf-000001", "perf-000002", 0); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFetchBlockingTargetsUsesTypedDependencyTargetProjection(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(dependencyTargetExpr)).
		WithArgs("perf-000001", "perf-000001").
		WillReturnRows(sqlmock.NewRows([]string{"target_id"}).AddRow("perf-000002"))

	got, err := fetchBlockingTargets(context.Background(), db, []string{"perf-000001"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"perf-000002"}) {
		t.Fatalf("targets = %v, want [perf-000002]", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSeedProductionShapeFullCoversTypedDependencyInserts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO issues\s+\(id, title, description, design, acceptance_criteria, notes,\s+status, priority, issue_type, assignee, metadata\)\s+VALUES`).
		WillReturnResult(sqlmock.NewResult(0, 8))
	mock.ExpectExec(`INSERT INTO dependencies\s+\(id, issue_id, depends_on_issue_id, type, created_by, metadata\)\s+VALUES`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO dependencies\s+\(id, issue_id, depends_on_issue_id, type, created_by, metadata\)\s+VALUES`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("CALL DOLT_ADD('-A')")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("CALL DOLT_COMMIT('-m', 'seed production timeout fixture')")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	cfg := config{SeedMode: "full", IssueCount: 1, DepCount: 1, Ops: 1, ChainDepth: 1}
	if err := seedProductionShape(context.Background(), db, cfg); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSeedProductionShapeFullSmallIssueCountRealSchema(t *testing.T) {
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not installed, skipping real-schema smoke")
	}
	skipOldDoltForCurrentSchema(t)

	ctx := context.Background()
	baseDir := t.TempDir()
	dbName := "testdb"
	dbDir := filepath.Join(baseDir, dbName)
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runTestCmd(t, dbDir, "dolt", "init", "--name", "test", "--email", "test@example.com")

	port, err := testutil.FindFreePort()
	if err != nil {
		t.Fatal(err)
	}
	serverCmd := exec.Command("dolt", "sql-server",
		"-H", "127.0.0.1",
		"-P", fmt.Sprintf("%d", port),
	)
	serverCmd.Dir = baseDir
	if err := serverCmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = serverCmd.Process.Kill()
		_ = serverCmd.Wait()
	})
	if !testutil.WaitForServer(port, 15*time.Second) {
		t.Fatal("dolt sql-server did not become ready")
	}

	dsn := doltutil.ServerDSN{Host: "127.0.0.1", Port: port, User: "root", Database: dbName, Timeout: 10 * time.Second}.String()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	if _, err := schema.MigrateUp(ctx, db); err != nil {
		t.Fatalf("migrate schema: %v", err)
	}

	cfg := config{SeedMode: "full", IssueCount: 50, DepCount: 50, Ops: 5, ChainDepth: 2}
	if err := seedProductionShape(ctx, db, cfg); err != nil {
		t.Fatal(err)
	}

	var depRows int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dependencies").Scan(&depRows); err != nil {
		t.Fatal(err)
	}
	wantDeps := cfg.DepCount + cfg.Ops*cfg.ChainDepth
	if depRows != wantDeps {
		t.Fatalf("dependency rows = %d, want %d", depRows, wantDeps)
	}

	sourceID, targetID := dependencyEndpoints(0, cfg.IssueCount, 0, 200)
	if err := cycleCheckCurrentSQL(ctx, db, perfIssueID(1), sourceID, 0); err != nil {
		t.Fatalf("cycleCheckCurrentSQL: %v", err)
	}
	targets, err := fetchBlockingTargets(ctx, db, []string{sourceID})
	if err != nil {
		t.Fatalf("fetchBlockingTargets: %v", err)
	}
	if !slices.Contains(targets, targetID) {
		t.Fatalf("fetchBlockingTargets(%q) = %v, want %q", sourceID, targets, targetID)
	}
}

func skipOldDoltForCurrentSchema(t *testing.T) {
	t.Helper()
	output, err := exec.Command("dolt", "version").CombinedOutput()
	if err != nil {
		t.Skipf("dolt version unavailable, skipping real-schema smoke: %v", err)
	}
	if regexp.MustCompile(`\bdolt version 1\.`).Match(output) {
		t.Skipf("dolt 1.x cannot initialize the current migration set: %s", strings.TrimSpace(string(output)))
	}
}

func runTestCmd(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v failed in %s: %v\nOutput: %s", name, args, dir, err, output)
	}
}

func runTestDoltSQL(t *testing.T, dir, query string) {
	t.Helper()
	cmd := exec.Command("dolt", "sql", "-q", query)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dolt sql failed in %s: %v\nQuery: %.200s...\nOutput: %s", dir, err, query, output)
	}
}
