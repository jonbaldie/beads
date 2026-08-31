package telemetry

import (
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/jonbaldie/beads/internal/storage"
)

const storageScopeName = "github.com/jonbaldie/beads/storage"

// InstrumentedStorage wraps storage.DoltStorage with OTel tracing and metrics.
// Methods on the core Storage interface are overridden to emit bd.storage.*
// counters, duration histograms, and per-operation spans. Methods on the
// DoltStorage capability sub-interfaces (VersionControl, HistoryViewer,
// SyncStore, etc.) pass through to the embedded inner store unchanged —
// those operations already have their own dolt.* spans inside the dolt
// implementation, so wrapping them here would double-count.
//
// Use WrapStorage to construct one; it returns the original store unchanged
// when telemetry is disabled.
type InstrumentedStorage struct {
	storage.DoltStorage // passthrough for capability methods we don't instrument
	inner               storage.DoltStorage
	tracer              trace.Tracer
	ops                 metric.Int64Counter
	dur                 metric.Float64Histogram
	errs                metric.Int64Counter
	issueGauge          metric.Int64Gauge
}

// WrapStorage returns s decorated with OTel instrumentation.
// When telemetry is disabled, s is returned as-is with zero overhead.
func WrapStorage(s storage.DoltStorage) storage.DoltStorage {
	if !Enabled() {
		return s
	}
	m := Meter(storageScopeName)
	ops, _ := m.Int64Counter("bd.storage.operations",
		metric.WithDescription("Total storage operations executed"),
	)
	dur, _ := m.Float64Histogram("bd.storage.operation.duration",
		metric.WithDescription("Storage operation duration in milliseconds"),
		metric.WithUnit("ms"),
	)
	errs, _ := m.Int64Counter("bd.storage.errors",
		metric.WithDescription("Total storage operation errors"),
	)
	issueGauge, _ := m.Int64Gauge("bd.issue.count",
		metric.WithDescription("Current number of issues by status (snapshot from GetStatistics)"),
	)
	return &InstrumentedStorage{
		DoltStorage: s,
		inner:       s,
		tracer:      Tracer(storageScopeName),
		ops:         ops,
		dur:         dur,
		errs:        errs,
		issueGauge:  issueGauge,
	}
}

// Unwrap satisfies storage.Unwrapper so storage.UnwrapStore can peel the
// instrumentation layer for optional-interface type assertions.

// op starts a span and records a metric for the named storage operation.

// done ends the span, records duration and optional error.

// ── Issue CRUD ──────────────────────────────────────────────────────────────
