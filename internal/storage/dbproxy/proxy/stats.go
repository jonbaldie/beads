package proxy

import "sync"

type Counters struct {
	ListenAndServeCalls  int64
	BackendStartCalls    int64
	BackendStopCalls     int64
	IdleTimeouts         int64
	SignalsReceived      int64
	AcceptCalls          int64
	AcceptErrors         int64
	BackendDialAttempts  int64
	BackendDialSuccess   int64
	BackendDialErrors    int64
	BackendDeadShutdowns int64
	HandledConns         int64
	BytesClientToBackend int64
	BytesBackendToClient int64
}

// Stats groups related counters into small state objects. The groups remain
// embedded so the historical Stats method set and zero-value behavior remain
// unchanged while each state object has a focused public surface.
type Stats struct {
	StatsLifecycle
	StatsBackend
	StatsConnections
	StatsBytes
}

type StatsLifecycle struct {
	mu       sync.Mutex
	counters Counters
}

type StatsBackend struct {
	mu       sync.Mutex
	counters Counters
}

type StatsConnections struct {
	mu       sync.Mutex
	counters Counters
}

type StatsBytes struct {
	mu       sync.Mutex
	counters Counters
}

func (s *Stats) Snapshot() Counters {
	if s == nil {
		return Counters{}
	}
	lifecycle := s.StatsLifecycle.snapshot()
	backend := s.StatsBackend.snapshot()
	connections := s.StatsConnections.snapshot()
	bytes := s.StatsBytes.snapshot()
	return Counters{
		ListenAndServeCalls:  lifecycle.ListenAndServeCalls,
		BackendStartCalls:    backend.BackendStartCalls,
		BackendStopCalls:     backend.BackendStopCalls,
		IdleTimeouts:         lifecycle.IdleTimeouts,
		SignalsReceived:      lifecycle.SignalsReceived,
		AcceptCalls:          connections.AcceptCalls,
		AcceptErrors:         connections.AcceptErrors,
		BackendDialAttempts:  backend.BackendDialAttempts,
		BackendDialSuccess:   backend.BackendDialSuccess,
		BackendDialErrors:    backend.BackendDialErrors,
		BackendDeadShutdowns: backend.BackendDeadShutdowns,
		HandledConns:         connections.HandledConns,
		BytesClientToBackend: bytes.BytesClientToBackend,
		BytesBackendToClient: bytes.BytesBackendToClient,
	}
}

func snapshotStats(mu *sync.Mutex, counters *Counters) Counters {
	mu.Lock()
	defer mu.Unlock()
	return *counters
}

func (s *StatsLifecycle) snapshot() Counters {
	return snapshotStats(&s.mu, &s.counters)
}

func (s *StatsBackend) snapshot() Counters {
	return snapshotStats(&s.mu, &s.counters)
}

func (s *StatsConnections) snapshot() Counters {
	return snapshotStats(&s.mu, &s.counters)
}

func (s *StatsBytes) snapshot() Counters {
	return snapshotStats(&s.mu, &s.counters)
}

func updateStats(mu *sync.Mutex, counters *Counters, fn func(*Counters)) {
	mu.Lock()
	fn(counters)
	mu.Unlock()
}

func (s *StatsLifecycle) IncListenAndServe() {
	updateStats(&s.mu, &s.counters, func(c *Counters) { c.ListenAndServeCalls++ })
}

func (s *StatsLifecycle) IncIdleTimeout() {
	updateStats(&s.mu, &s.counters, func(c *Counters) { c.IdleTimeouts++ })
}

func (s *StatsLifecycle) IncSignalReceived() {
	updateStats(&s.mu, &s.counters, func(c *Counters) { c.SignalsReceived++ })
}

func (s *StatsBackend) IncBackendStart() {
	updateStats(&s.mu, &s.counters, func(c *Counters) { c.BackendStartCalls++ })
}

func (s *StatsBackend) IncBackendStop() {
	updateStats(&s.mu, &s.counters, func(c *Counters) { c.BackendStopCalls++ })
}

func (s *StatsBackend) IncBackendDialAttempt() {
	updateStats(&s.mu, &s.counters, func(c *Counters) { c.BackendDialAttempts++ })
}

func (s *StatsBackend) IncBackendDialSuccess() {
	updateStats(&s.mu, &s.counters, func(c *Counters) { c.BackendDialSuccess++ })
}

func (s *StatsBackend) IncBackendDialError() {
	updateStats(&s.mu, &s.counters, func(c *Counters) { c.BackendDialErrors++ })
}

func (s *StatsBackend) IncBackendDeadShutdown() {
	updateStats(&s.mu, &s.counters, func(c *Counters) { c.BackendDeadShutdowns++ })
}

func (s *StatsConnections) IncAccept() {
	updateStats(&s.mu, &s.counters, func(c *Counters) { c.AcceptCalls++ })
}

func (s *StatsConnections) IncAcceptError() {
	updateStats(&s.mu, &s.counters, func(c *Counters) { c.AcceptErrors++ })
}

func (s *StatsConnections) IncHandledConn() {
	updateStats(&s.mu, &s.counters, func(c *Counters) { c.HandledConns++ })
}

func (s *StatsBytes) AddBytesClientToBackend(n int64) {
	updateStats(&s.mu, &s.counters, func(c *Counters) { c.BytesClientToBackend += n })
}

func (s *StatsBytes) AddBytesBackendToClient(n int64) {
	updateStats(&s.mu, &s.counters, func(c *Counters) { c.BytesBackendToClient += n })
}
