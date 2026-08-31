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
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jonbaldie/beads/internal/storage/doltutil"
)

func runReadyScenario(ctx context.Context, cfg config, ws *workspace) []opResult {
	jobs := make([]job, 0, cfg.Ops)
	for i := 0; i < cfg.Ops; i++ {
		if i%2 == 0 {
			jobs = append(jobs, job{Kind: "ready", Argv: []string{"ready", "--assignee=example-org--control-dispatcher", "--json", "--limit=20"}})
		} else {
			jobs = append(jobs, job{Kind: "ready", Argv: []string{"ready", "--metadata-field", "route.routed_to=example-org/control-dispatcher", "--unassigned", "--json", "--limit=20"}})
		}
	}
	return runJobs(ctx, cfg, ws, jobs)
}

func runDepScenario(ctx context.Context, cfg config, ws *workspace) []opResult {
	jobs := make([]job, 0, cfg.Ops)
	for i := 0; i < cfg.Ops; i++ {
		jobs = append(jobs, depAddJob(i, cfg.ChainDepth))
	}
	return runJobs(ctx, cfg, ws, jobs)
}

func runControlQueryScenario(ctx context.Context, cfg config, ws *workspace) []opResult {
	return runJobs(ctx, cfg, ws, controlQueryJobs(cfg.Ops))
}

func runOutageScenario(ctx context.Context, cfg config, ws *workspace) []opResult {
	jobs := make([]job, 0, cfg.Ops*2)
	jobs = append(jobs, controlQueryJobs(cfg.Ops)...)
	for i := 0; i < cfg.Ops; i++ {
		jobs = append(jobs, depAddJob(i, cfg.ChainDepth))
	}
	return runJobs(ctx, cfg, ws, jobs)
}

type cycleCheckFunc func(context.Context, *sql.DB, string, string, int) error

func runCycleCheckScenario(ctx context.Context, cfg config, ws *workspace, check cycleCheckFunc) []opResult {
	dsn := doltutil.ServerDSN{
		Host:     "127.0.0.1",
		Port:     ws.Port,
		User:     "root",
		Database: ws.Database,
		Timeout:  cfg.Timeout,
	}.String()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return []opResult{{Kind: "cycle-open", Err: err.Error()}}
	}
	defer db.Close()
	db.SetMaxOpenConns(cfg.Concurrency)
	db.SetMaxIdleConns(cfg.Concurrency)

	start := time.Now()
	jobCh := make(chan int)
	resCh := make(chan opResult, cfg.Ops)
	var wg sync.WaitGroup
	for w := 0; w < cfg.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobCh {
				opCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
				base := depBase(i, cfg.ChainDepth)
				issueID := depIssueID(base)
				dependsOnID := depIssueID(base + 1)
				opStart := time.Now()
				err := check(opCtx, db, issueID, dependsOnID, cfg.ChainDepth)
				latency := time.Since(opStart)
				res := opResult{Kind: "cycle", Argv: []string{"cycle-check", issueID, dependsOnID}, Latency: latency}
				if opCtx.Err() == context.DeadlineExceeded {
					res.TimedOut = true
				}
				if err != nil {
					res.Err = err.Error()
					res.StderrTail = tail(err.Error(), 300)
				}
				cancel()
				resCh <- res
			}
		}()
	}
	for i := 0; i < cfg.Ops; i++ {
		jobCh <- i
	}
	close(jobCh)
	wg.Wait()
	close(resCh)

	results := make([]opResult, 0, cfg.Ops)
	for res := range resCh {
		results = append(results, res)
	}
	fmt.Printf("scenario wall=%s\n", time.Since(start).Round(time.Millisecond))
	return results
}

func cycleCheckCurrentSQL(ctx context.Context, db *sql.DB, issueID, dependsOnID string, _ int) error {
	var reachable int
	query := fmt.Sprintf(`
		WITH RECURSIVE reachable AS (
			SELECT ? AS node, 0 AS depth
			UNION ALL
			SELECT d.target_id, r.depth + 1
			FROM reachable r
			JOIN (
				SELECT issue_id, %s AS target_id FROM dependencies WHERE type IN ('blocks', 'conditional-blocks')
				UNION ALL
				SELECT issue_id, %s AS target_id FROM wisp_dependencies WHERE type IN ('blocks', 'conditional-blocks')
			) d ON d.issue_id = r.node
			WHERE r.depth < 100
		)
		SELECT COUNT(*) FROM reachable WHERE node = ?
	`, dependencyTargetExpr, dependencyTargetExpr)
	err := db.QueryRowContext(ctx, query, dependsOnID, issueID).Scan(&reachable)
	if err != nil {
		return err
	}
	if reachable > 0 {
		return fmt.Errorf("cycle detected")
	}
	return nil
}

func cycleCheckDependenciesOnlySQL(ctx context.Context, db *sql.DB, issueID, dependsOnID string, _ int) error {
	return cycleCheckOneTableSQL(ctx, db, "dependencies", issueID, dependsOnID)
}

func cycleCheckWispsOnlySQL(ctx context.Context, db *sql.DB, issueID, dependsOnID string, _ int) error {
	return cycleCheckOneTableSQL(ctx, db, "wisp_dependencies", issueID, dependsOnID)
}

func cycleCheckOneTableSQL(ctx context.Context, db *sql.DB, table, issueID, dependsOnID string) error {
	var reachable int
	//nolint:gosec // G201: table is selected by fixed scenario wrappers.
	query := fmt.Sprintf(`
		WITH RECURSIVE reachable AS (
			SELECT ? AS node, 0 AS depth
			UNION ALL
			SELECT %s, r.depth + 1
			FROM reachable r
			JOIN %s d ON d.issue_id = r.node
			WHERE d.type IN ('blocks', 'conditional-blocks') AND r.depth < 100
		)
		SELECT COUNT(*) FROM reachable WHERE node = ?
	`, dependencyTargetExpr, table)
	if err := db.QueryRowContext(ctx, query, dependsOnID, issueID).Scan(&reachable); err != nil {
		return err
	}
	if reachable > 0 {
		return fmt.Errorf("cycle detected")
	}
	return nil
}

func cycleCheckBatchedBFS(ctx context.Context, db *sql.DB, issueID, dependsOnID string, maxDepth int) error {
	if maxDepth <= 0 || maxDepth > 100 {
		maxDepth = 100
	}
	seen := map[string]struct{}{dependsOnID: {}}
	frontier := []string{dependsOnID}
	for depth := 0; depth < maxDepth; depth++ {
		if frontier == nil {
			break
		}
		next, err := fetchBlockingTargets(ctx, db, frontier)
		if err != nil {
			return err
		}
		frontier = nil
		for _, id := range next {
			if id == issueID {
				return fmt.Errorf("cycle detected")
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			frontier = append(frontier, id)
		}
	}
	return nil
}

func fetchBlockingTargets(ctx context.Context, db *sql.DB, issueIDs []string) ([]string, error) {
	if len(issueIDs) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(issueIDs)*2)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(issueIDs)), ",")
	//nolint:gosec // G201: placeholders are generated from ? markers only.
	query := fmt.Sprintf(`
		SELECT %s FROM dependencies
		WHERE issue_id IN (%s) AND type IN ('blocks', 'conditional-blocks')
		UNION ALL
		SELECT %s FROM wisp_dependencies
		WHERE issue_id IN (%s) AND type IN ('blocks', 'conditional-blocks')
	`, dependencyTargetExpr, placeholders, dependencyTargetExpr, placeholders)
	for _, id := range issueIDs {
		args = append(args, id)
	}
	for _, id := range issueIDs {
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func runMixedCityLoadScenario(ctx context.Context, cfg config, ws *workspace) []opResult {
	start := time.Now()
	depCount := cfg.Ops
	backgroundCount := cfg.Ops * cfg.Concurrency

	results := make([]opResult, 0, depCount+backgroundCount)
	resultCh := make(chan opResult, depCount+backgroundCount)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < depCount; i++ {
			job := depAddJob(i, cfg.ChainDepth)
			job.Kind = "dispatcher-dep"
			resultCh <- runJob(ctx, cfg, ws, job)
		}
	}()

	backgroundJobs := mixedBackgroundJobs(backgroundCount)
	jobCh := make(chan job)
	for w := 0; w < cfg.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				resultCh <- runJob(ctx, cfg, ws, job)
			}
		}()
	}
	for _, job := range backgroundJobs {
		jobCh <- job
	}
	close(jobCh)

	wg.Wait()
	close(resultCh)
	for res := range resultCh {
		results = append(results, res)
	}
	fmt.Printf("scenario wall=%s\n", time.Since(start).Round(time.Millisecond))
	return results
}

func mixedBackgroundJobs(count int) []job {
	jobs := make([]job, 0, count)
	for i := 0; i < count; i++ {
		switch i % 6 {
		case 0:
			jobs = append(jobs, job{Kind: "session-ready", Argv: []string{"ready", "--include-ephemeral", "--assignee=" + sessionAssignee(i), "--json", "--limit=1"}})
		case 1:
			jobs = append(jobs, job{Kind: "control-ready", Argv: []string{"--readonly", "--sandbox", "ready", "--include-ephemeral", "--assignee=example-org--control-dispatcher", "--json", "--limit=20"}})
		case 2:
			jobs = append(jobs, job{Kind: "route-ready", Argv: []string{"--readonly", "--sandbox", "ready", "--include-ephemeral", "--metadata-field", "route.routed_to=example-org/control-dispatcher", "--unassigned", "--json", "--limit=20"}})
		case 3:
			jobs = append(jobs, job{Kind: "show", Argv: []string{"show", fmt.Sprintf("perf-%06d", i%350), "--json"}})
		case 4:
			jobs = append(jobs, job{Kind: "list", Argv: []string{"list", "--json", "--status", "in_progress", "--assignee=" + sessionAssignee(i), "--limit=1"}})
		case 5:
			jobs = append(jobs, job{Kind: "claim", Argv: []string{"update", fmt.Sprintf("perf-%06d", i%40), "--claim", "--json"}})
		}
	}
	return jobs
}

func sessionAssignee(i int) string {
	return fmt.Sprintf("mc-%07d", i%64)
}

func depAddJob(i, chainDepth int) job {
	base := depBase(i, chainDepth)
	return job{
		Kind: "dep",
		Argv: []string{"dep", "add", depIssueID(base), depIssueID(base + 1), "--type", "blocks", "--json"},
	}
}

func depBase(i, chainDepth int) int {
	if chainDepth <= 0 {
		return i * 2
	}
	return i * (chainDepth + 3)
}

func controlQueryJobs(count int) []job {
	targets := []struct {
		target  string
		session string
		legacy  string
	}{
		{target: "control-dispatcher", session: "control-dispatcher", legacy: "workflow-control"},
		{target: "example-org/control-dispatcher", session: "example-org--control-dispatcher", legacy: "example-org/workflow-control"},
		{target: "example-gui/control-dispatcher", session: "example-gui--control-dispatcher", legacy: "example-gui/workflow-control"},
		{target: "gtest-rig/control-dispatcher", session: "gtest-rig--control-dispatcher", legacy: "gtest-rig/workflow-control"},
	}

	jobs := make([]job, 0, count)
	script := controlQueryScript()
	for i := 0; i < count; i++ {
		t := targets[i%len(targets)]
		jobs = append(jobs, job{
			Kind: "control",
			Sh:   script,
			Env: []string{
				"BD_EXPORT_AUTO=false",
				"GC_CONTROL_TARGET=" + t.target,
				"GC_CONTROL_SESSION_NAME=" + t.session,
				"GC_CONTROL_LEGACY_TARGET=" + t.legacy,
				"GC_SESSION_NAME=" + t.session,
				"GC_ALIAS=" + t.target,
				"GC_SESSION_ID=" + t.session,
			},
		})
	}
	return jobs
}

func controlQueryScript() string {
	return `BD_EXPORT_AUTO=false
tmp=$(mktemp)
err=$(mktemp)
trap "rm -f \"$tmp\" \"$err\"" EXIT
emit_ready() {
  r=$("$@" 2>"$err")
  status=$?
  if [ "$status" -ne 0 ]; then
    printf "ready probe failed (%s):\n" "$*" >&2
    cat "$err" >&2
    return "$status"
  fi
  [ -n "$r" ] && [ "$r" != "[]" ] && printf "%s\n" "$r" >> "$tmp"
}
for id in "$GC_CONTROL_SESSION_NAME" "$GC_SESSION_NAME" "$GC_ALIAS" "$GC_CONTROL_TARGET" "$GC_SESSION_ID"; do
  [ -z "$id" ] && continue
  legacy=""
  case "$id" in *control-dispatcher) legacy="${id%control-dispatcher}workflow-control";; esac
  for cand in "$id" "$legacy"; do
    [ -z "$cand" ] && continue
    emit_ready "$BD_BIN" --readonly --sandbox ready --assignee="$cand" --json --limit=20 || exit $?
  done
done
emit_ready "$BD_BIN" --readonly --sandbox ready --metadata-field "route.routed_to=$GC_CONTROL_TARGET" --unassigned --json --limit=20 || exit $?
emit_ready "$BD_BIN" --readonly --sandbox ready --metadata-field "route.routed_to=$GC_CONTROL_LEGACY_TARGET" --unassigned --json --limit=20 || exit $?
[ -s "$tmp" ] && jq -s 'reduce add[] as $item ([]; if any(.[]; .id == $item.id) then . else . + [$item] end)' "$tmp" || printf "[]"
`
}

func depIssueID(i int) string {
	return fmt.Sprintf("perf-dep-%06d", i)
}

func runJobs(ctx context.Context, cfg config, ws *workspace, jobs []job) []opResult {
	start := time.Now()
	jobCh := make(chan job)
	resCh := make(chan opResult, len(jobs))
	var wg sync.WaitGroup
	for w := 0; w < cfg.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				resCh <- runJob(ctx, cfg, ws, job)
			}
		}()
	}
	for _, job := range jobs {
		jobCh <- job
	}
	close(jobCh)
	wg.Wait()
	close(resCh)

	results := make([]opResult, 0, len(jobs))
	for res := range resCh {
		results = append(results, res)
	}
	fmt.Printf("scenario wall=%s\n", time.Since(start).Round(time.Millisecond))
	return results
}

func runJob(ctx context.Context, cfg config, ws *workspace, j job) opResult {
	if j.Sh != "" {
		return runShell(ctx, cfg, ws, j)
	}
	return runBD(ctx, cfg, ws, j)
}

func runBD(ctx context.Context, cfg config, ws *workspace, j job) opResult {
	opCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	cmd := newBDCommand(opCtx, cfg, ws, j)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	latency := time.Since(start)
	res := opResult{Kind: j.Kind, Argv: j.Argv, Latency: latency}
	if len(res.Argv) == 0 {
		res.Argv = []string{"sh", "-c", compactShell(j.Sh)}
	}
	if opCtx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	}
	if err != nil {
		res.Err = err.Error()
	}
	res.StderrTail = tail(stderr.String(), 300)
	return res
}

func runShell(ctx context.Context, cfg config, ws *workspace, j job) opResult {
	opCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	cmd := newShellCommand(opCtx, cfg, ws, j)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	latency := time.Since(start)
	res := opResult{Kind: j.Kind, Argv: []string{"sh", "-c", compactShell(j.Sh)}, Latency: latency}
	if opCtx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	}
	if err != nil {
		res.Err = err.Error()
	}
	res.StderrTail = tail(stderr.String(), 300)
	return res
}

func newBDCommand(ctx context.Context, cfg config, ws *workspace, j job) *exec.Cmd {
	cmd := exec.CommandContext(ctx, cfg.BDPath, j.Argv...)
	cmd.Dir = ws.Dir
	cmd.Env = subprocessEnv(append([]string{"BD_NON_INTERACTIVE=1"}, j.Env...)...)
	return cmd
}

func newShellCommand(ctx context.Context, cfg config, ws *workspace, j job) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", j.Sh)
	cmd.Dir = ws.Dir
	cmd.Env = subprocessEnv(append([]string{"BD_NON_INTERACTIVE=1", "BD_BIN=" + cfg.BDPath}, j.Env...)...)
	return cmd
}

func compactShell(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func report(name string, results []opResult) {
	sort.Slice(results, func(i, j int) bool { return results[i].Latency < results[j].Latency })
	var failures, timeouts int
	var driverReadTimeouts int
	for _, r := range results {
		if r.Err != "" {
			failures++
		}
		if r.TimedOut {
			timeouts++
		}
		if isDriverReadTimeout(r) {
			driverReadTimeouts++
		}
	}
	fmt.Printf("\n[%s] ops=%d failures=%d harness_timeouts=%d driver_read_timeouts=%d p50=%s p95=%s max=%s\n",
		name, len(results), failures, timeouts, driverReadTimeouts,
		percentile(results, 50).Round(time.Millisecond),
		percentile(results, 95).Round(time.Millisecond),
		percentile(results, 100).Round(time.Millisecond),
	)
	firstSlow := len(results) - 5
	for i := len(results) - 1; i >= firstSlow; i-- {
		r := results[i]
		fmt.Printf("  slow kind=%s latency=%s timeout=%t err=%q stderr=%q argv=%s\n",
			r.Kind, r.Latency.Round(time.Millisecond), r.TimedOut, r.Err, r.StderrTail, strings.Join(r.Argv, " "))
	}
}

func isDriverReadTimeout(r opResult) bool {
	return strings.Contains(r.StderrTail, "packets.go:58 read tcp") &&
		strings.Contains(r.StderrTail, "i/o timeout")
}

func percentile(results []opResult, p int) time.Duration {
	if len(results) == 0 {
		return 0
	}
	if p >= 100 {
		return results[len(results)-1].Latency
	}
	idx := (len(results)*p + 99) / 100
	if idx <= 0 {
		idx = 1
	}
	return results[idx-1].Latency
}

func readInt(path string) (int, error) {
	//nolint:gosec // G304: benchmark harness reads control files from the selected workspace.
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func subprocessEnv(extra ...string) []string {
	env := cleanEnv(os.Environ(), subprocessEnvDenylist...)
	return append(env, extra...)
}

func cleanEnv(env []string, keys ...string) []string {
	drop := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		drop[environmentKeyIdentity(key)] = struct{}{}
	}
	out := env[:0]
	for _, e := range env {
		key, _, ok := strings.Cut(e, "=")
		if ok {
			if _, skip := drop[environmentKeyIdentity(key)]; skip {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

// environmentKeyIdentity mirrors the key comparison used by os/exec when it
// prepares a child environment. strings.ToLower is intentional: EqualFold
// would collapse Unicode near-collisions that os/exec keeps distinct.
func environmentKeyIdentity(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(key)
	}
	return key
}

func tail(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}
