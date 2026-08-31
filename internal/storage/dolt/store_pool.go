// Package dolt implements the storage interface using Dolt (versioned MySQL-compatible database).
//
// Dolt provides native version control for SQL data with cell-level merge, history queries,
// and federation via Dolt remotes. The database itself is version-controlled.
//
// Dolt capabilities:
//   - Native version control (commit, push, pull, branch, merge)
//   - Time-travel queries via AS OF and dolt_history_* tables
//   - Cell-level merge for conflict resolution
//   - Multi-writer via dolt sql-server (federation, pure Go)
//
// All operations require a running dolt sql-server. Connect via MySQL protocol (pure Go).
package dolt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	mysql "github.com/go-sql-driver/mysql"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/jonbaldie/beads/internal/storage/schema"
)

// Defaults for the *sql.DB connection pool. Exported for tests/callers that
// want to reason about the out-of-the-box pool limits without having to read
// openServerConnection.
const (
	defaultMaxOpenConns    = 10
	defaultMaxIdleConns    = 5
	defaultConnMaxLifetime = time.Hour
	// defaultConnMaxIdleTime keeps idle pooled connections shorter-lived than the
	// dolt sql-server wait_timeout (30s) so the pool retires an idle connection
	// before the server reaps it; this prevents the next read from picking up a
	// server-closed connection and failing with "invalid connection".
	defaultConnMaxIdleTime = 20 * time.Second
	// defaultPoolReadTimeout / defaultPoolWriteTimeout are the per-I/O
	// deadlines on shared-pool connections. Overridable via
	// Config.PoolReadTimeout/PoolWriteTimeout (BEADS_DOLT_POOL_READ_TIMEOUT /
	// BEADS_DOLT_POOL_WRITE_TIMEOUT, dolt.pool-read-timeout /
	// dolt.pool-write-timeout); the defaults themselves are deliberately
	// unchanged (bd-vz0y9).
	defaultPoolReadTimeout  = 10 * time.Second
	defaultPoolWriteTimeout = 10 * time.Second
)

// cliExecTimeout is the default maximum time to wait for dolt CLI
// push/pull/fetch operations. SSH transfers can hang indefinitely on network
// issues or SSH key prompts; this prevents the process from blocking forever.
// Large transfers can legitimately run longer (e.g. pushing a big chunk store
// to a cloud remote, or a transfer serialized behind a busy dolt sql-server
// that holds the database directory lock); set BEADS_CLI_TRANSFER_TIMEOUT to
// override.
const cliExecTimeout = 5 * time.Minute

// cliExecTimeoutEnv is the environment variable that overrides cliExecTimeout.
const cliExecTimeoutEnv = "BEADS_CLI_TRANSFER_TIMEOUT"

// cliExecWaitDelay bounds how long Wait/CombinedOutput may keep waiting after
// the transfer context expires. CommandContext kills only the direct dolt
// child; a grandchild (e.g. a cloud credential helper) that inherited the
// output pipes would otherwise keep Wait blocked indefinitely after the kill.
const cliExecWaitDelay = 10 * time.Second

// cliExecTimeoutDuration returns the configured CLI transfer timeout. The env
// var BEADS_CLI_TRANSFER_TIMEOUT overrides the compiled-in cliExecTimeout
// const; valid time.ParseDuration strings (e.g. "20m", "90s") or bare numbers
// treated as seconds (e.g. "90") are accepted. Unset or invalid values fall
// back to cliExecTimeout.
func cliExecTimeoutDuration() time.Duration {
	return timeoutFromEnv(cliExecTimeoutEnv, cliExecTimeout)
}

func withCLIExecTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, cliExecTimeoutDuration())
}

// timeoutFromEnv returns the duration configured in the named env var, falling
// back to fallback when the var is unset, unparsable, or non-positive. Valid
// time.ParseDuration strings (e.g. "2m", "90s") or bare numbers treated as
// seconds (e.g. "90") are accepted.
func timeoutFromEnv(env string, fallback time.Duration) time.Duration {
	return parseTimeout(os.Getenv(env), fallback)
}

// parseTimeout parses a duration setting, falling back to fallback when raw is
// empty, unparsable, or non-positive. Valid time.ParseDuration strings (e.g.
// "2m", "90s") or bare numbers treated as seconds (e.g. "90") are accepted.
func parseTimeout(raw string, fallback time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	if d, err := time.ParseDuration(raw); err == nil {
		if d > 0 {
			return d
		}
		return fallback
	}
	if d, err := time.ParseDuration(raw + "s"); err == nil {
		if d > 0 {
			return d
		}
		return fallback
	}
	return fallback
}

// fsckTimeout is the default maximum time to wait for dolt fsck to verify the
// local chunk store before a push. fsck reads local files only; 30 seconds is
// ample for small stores. Large stores may need more time; set
// BEADS_FSCK_TIMEOUT to override.
const fsckTimeout = 30 * time.Second

// fsckTimeoutEnv is the environment variable that overrides fsckTimeout.
const fsckTimeoutEnv = "BEADS_FSCK_TIMEOUT"

// fsckTimeoutDuration returns the configured fsck timeout. The env var
// BEADS_FSCK_TIMEOUT overrides the compiled-in fsckTimeout const; valid
// time.ParseDuration strings (e.g. "2m", "90s") or bare numbers treated as
// seconds (e.g. "90") are accepted. Unset or invalid values fall back to
// fsckTimeout.
func fsckTimeoutDuration() time.Duration {
	return timeoutFromEnv(fsckTimeoutEnv, fsckTimeout)
}

// Retry configuration for transient connection errors (stale pool connections,
// brief network issues, server restarts).
const serverRetryMaxElapsed = 30 * time.Second

func newServerRetryBackoff() backoff.BackOff {
	bo := backoff.NewExponentialBackOff()
	bo.MaxElapsedTime = serverRetryMaxElapsed
	return bo
}

// isRetryableError returns true if the error is a transient connection error
// that should be retried in server mode.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if schema.IsMigrationLockError(err) {
		return true
	}
	if handled, retry := retryableMySQLServerError(err); handled {
		return retry
	}
	return retryableConnectionMessage(err.Error())
}

func retryableMySQLServerError(err error) (bool, bool) {
	// A decoded 1105 is a definite server response. Preserve the two explicit
	// server-startup recoveries below, but do not let any other 1105 enter the
	// general retry or circuit-breaker path just because its message happens to
	// contain connection-like wording.
	var mysqlErr *mysql.MySQLError
	if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1105 {
		return false, false
	}
	message := strings.ToLower(mysqlErr.Message)
	return true, strings.Contains(message, "no root value found") ||
		strings.Contains(message, "database is read only")
}

func retryableConnectionMessage(message string) bool {
	errStr := strings.ToLower(message)
	return retryableDriverMessage(errStr) ||
		retryableNetworkMessage(errStr) ||
		retryableDoltMessage(errStr)
}

func retryableDriverMessage(message string) bool {
	return strings.Contains(message, "driver: bad connection") ||
		strings.Contains(message, "invalid connection")
}

func retryableNetworkMessage(message string) bool {
	return strings.Contains(message, "broken pipe") ||
		strings.Contains(message, "connection reset") ||
		strings.Contains(message, "connection refused") ||
		strings.Contains(message, "lost connection") ||
		strings.Contains(message, "gone away") ||
		strings.Contains(message, "i/o timeout")
}

func retryableDoltMessage(message string) bool {
	return strings.Contains(message, "database is read only") ||
		strings.Contains(message, "unknown database") ||
		strings.Contains(message, "no root value found")
}

// isLockError returns true if the error indicates a Dolt lock contention problem.
// These can occur when the Dolt server's storage layer is locked by another
// process or a stale LOCK file was left behind by a crashed server.
func isLockError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "database is locked") ||
		strings.Contains(errStr, "lock file") ||
		strings.Contains(errStr, "noms lock") ||
		strings.Contains(errStr, "locked by another dolt process")
}

// wrapLockError wraps lock-related errors with actionable guidance.
// Non-lock errors and nil are returned unchanged.
func wrapLockError(err error) error {
	if !isLockError(err) {
		return err
	}
	hint := lockProcessHint()
	return fmt.Errorf("%w\n\nThe Dolt database is locked.%s\n"+
		"Try: bd doctor --fix (clears stale locks), or kill the holding process.", err, hint)
}

// lockProcessHint tries to identify the process holding the database lock.
// Returns a hint string like " Process 12345 (bd) may be holding the lock."
// Returns empty string if identification fails or on unsupported platforms.
func lockProcessHint() string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		// /proc not available (macOS, Windows, FreeBSD) — skip PID detection
		return ""
	}

	holders := lockProcessHolders(entries, os.Getpid())
	return formatLockProcessHint(holders)
}

func lockProcessHolders(entries []os.DirEntry, myPID int) []string {
	var holders []string
	for _, entry := range entries {
		if pid, ok := lockProcessPID(entry, myPID); ok {
			holders = append(holders, pid)
		}
	}
	return holders
}

func lockProcessPID(entry os.DirEntry, myPID int) (string, bool) {
	if !entry.IsDir() {
		return "", false
	}
	pid, err := strconv.Atoi(entry.Name())
	if err != nil || pid == myPID {
		return "", false
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
	if err != nil {
		return "", false
	}
	cmd := string(cmdline)
	if !strings.Contains(cmd, "bd") && !strings.Contains(cmd, "dolt") {
		return "", false
	}
	return fmt.Sprintf("%d", pid), true
}

func formatLockProcessHint(holders []string) string {
	switch len(holders) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf(" Process %s (bd/dolt) may be holding the lock.", holders[0])
	default:
		return fmt.Sprintf(" Processes %s (bd/dolt) may be holding the lock.", strings.Join(holders, ", "))
	}
}

type circuitWriteContextKey struct{}

func circuitWriteManaged(ctx context.Context) bool {
	_, ok := ctx.Value(circuitWriteContextKey{}).(struct{})
	return ok
}

// SetEventsJournalEnabled activates the journal for this store instance only.
func (s *DoltStore) SetEventsJournalEnabled(enabled bool) {
	s.doltStoreLifecycleState.eventsJournalEnabled.Store(enabled)
}

// doltTracer is the OTel tracer for SQL-level spans.
// It uses the global provider, which is a no-op until telemetry.Init() is called.
var doltTracer = otel.Tracer("github.com/jonbaldie/beads/storage/dolt")

// doltMetrics holds OTel metric instruments for the dolt storage backend.
// Instruments are registered against the global delegating provider at init time,
// so they automatically forward to the real provider once telemetry.Init() runs.
type doltMetricsState struct {
	retryCount           metric.Int64Counter
	lockWaitMs           metric.Float64Histogram
	circuitTrips         metric.Int64Counter
	circuitRejected      metric.Int64Counter
	serializationErrors  metric.Int64Counter
	writeRetries         metric.Int64Counter
	connAcquireMs        metric.Float64Histogram
	poolWaitCount        metric.Int64Counter
	poolWaitMs           metric.Float64Histogram
	claimVerifyLost      metric.Int64Counter
	claimVerifyRecovered metric.Int64Counter
	ignoredTxFreshPool   metric.Int64Counter
}

var doltMetrics = &doltMetricsState{}

func init() {
	m := otel.Meter("github.com/jonbaldie/beads/storage/dolt")
	doltMetrics.initialize(m)
}

func (d *doltMetricsState) initialize(m metric.Meter) {
	d.retryCount, _ = m.Int64Counter("bd.db.retry_count",
		metric.WithDescription("SQL operations retried due to server-mode transient errors"),
		metric.WithUnit("{retry}"),
	)
	d.lockWaitMs, _ = m.Float64Histogram("bd.db.lock_wait_ms",
		metric.WithDescription("Time spent waiting to acquire database locks"),
		metric.WithUnit("ms"),
	)
	d.circuitTrips, _ = m.Int64Counter("bd.db.circuit_trips",
		metric.WithDescription("Number of times the Dolt circuit breaker tripped open"),
		metric.WithUnit("{trip}"),
	)
	d.circuitRejected, _ = m.Int64Counter("bd.db.circuit_rejected",
		metric.WithDescription("Requests rejected by open circuit breaker (fail-fast)"),
		metric.WithUnit("{request}"),
	)
	d.serializationErrors, _ = m.Int64Counter("bd.db.serialization_errors",
		metric.WithDescription("Serialization failures (MySQL 1213/1205) before retry"),
		metric.WithUnit("{error}"),
	)
	d.writeRetries, _ = m.Int64Counter("bd.write_retries_total",
		metric.WithDescription("Write-tx retries in withRetryTx (label: type=serialization|connection)"),
		metric.WithUnit("{retry}"),
	)
	d.connAcquireMs, _ = m.Float64Histogram("bd.db.conn_acquire_ms",
		metric.WithDescription("Time to acquire a pooled connection for a Dolt transaction"),
		metric.WithUnit("ms"),
	)
	d.poolWaitCount, _ = m.Int64Counter("bd.db.pool_wait_count",
		metric.WithDescription("Number of times a connection acquisition had to wait for the pool"),
		metric.WithUnit("{wait}"),
	)
	d.poolWaitMs, _ = m.Float64Histogram("bd.db.pool_wait_ms",
		metric.WithDescription("Total time connections spent waiting due to pool exhaustion"),
		metric.WithUnit("ms"),
	)
	d.claimVerifyLost, _ = m.Int64Counter("bd.claim_verify_lost_total",
		metric.WithDescription("Claim-family writes that reported success but failed verify-by-re-read (label: op=claim|unclaim)"),
		metric.WithUnit("{write}"),
	)
	d.claimVerifyRecovered, _ = m.Int64Counter("bd.claim_verify_recovered_total",
		metric.WithDescription("Indeterminate claim-family commits resolved by re-read (label: op, outcome=applied|replayed)"),
		metric.WithUnit("{write}"),
	)
	d.ignoredTxFreshPool, _ = m.Int64Counter("bd.db.ignored_tx_fresh_pool",
		metric.WithDescription("ignored-tx transactions that fell back to a dedicated single-connection pool instead of borrowing from the main pool"),
		metric.WithUnit("{tx}"),
	)
}

// spanSQL truncates a SQL string to keep spans readable.
func spanSQL(q string) string {
	if len(q) > 300 {
		return q[:300] + "…"
	}
	return q
}

// endSpan records an error (if any) and ends the span.
func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// ErrStoreClosed is returned when an operation is attempted on a closed store.
var ErrStoreClosed = errors.New("store is closed")

// applyConfigDefaults fills in default values for unset Config fields.
