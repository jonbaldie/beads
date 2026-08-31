package httpapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/jonbaldie/beads/internal/storage/uow"
)

// Serve accepts requests until ctx is canceled, then drains. It returns nil
// on a clean shutdown; a listener failure is returned as-is.
//
// The drain budget covers a committing request that is mid-retry, because
// Shutdown does not cancel in-flight handler contexts: killing such a
// connection early would leave the client unable to tell whether its write
// landed.
func (s *Server) Serve(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.network.http.Serve(s.network.listener) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	s.event("shutdown_start", "drain_timeout", drainTimeout.String(), "conns", s.network.liveConns.Load())

	// Detached: ctx is already canceled, and the drain is the point.
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()

	if err := s.network.http.Shutdown(drainCtx); err != nil {
		killed := s.network.liveConns.Load()
		_ = s.network.http.Close()
		s.event("shutdown_forced", "conns_killed", killed, "reason", err.Error())
	} else {
		s.event("shutdown_complete")
	}
	<-errCh
	return nil
}

// connState tracks accepted connections, and says out loud when the cap is
// reached.
//
// It has to: netutil.LimitListener simply stops calling Accept at the cap, so
// further connections wait in the kernel backlog with nothing on stderr — and
// /healthz needs a fresh accept too, so an exhausted cap is indistinguishable
// from no traffic at all. Request lines just stop. This is the connection
// tier's version of the semaphore's saturation event, and the one wedge mode
// this slice can actually exhibit.
//
// The event is edge-triggered: once when the cap is reached, again only after
// it has cleared. The conns gauge on every request line is what shows it
// climbing beforehand.
func (s *Server) connState(_ net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		n := s.network.liveConns.Add(1)
		if s.network.maxConns > 0 && n >= int64(s.network.maxConns) && s.network.connCapWarned.CompareAndSwap(false, true) {
			s.event("conn_cap_saturated", "conns", n, "max_conns", s.network.maxConns)
		}
	case http.StateHijacked, http.StateClosed:
		if s.network.liveConns.Add(-1) < int64(s.network.maxConns) {
			s.network.connCapWarned.Store(false)
		}
	}
}

// WithUOW runs fn inside one unit of work and guarantees the rollback.
//
// The close context is DETACHED on purpose. Close sends ROLLBACK on the pinned
// connection, and the transaction layer POISONS that connection if the send
// fails (internal/storage/uow/doltserver_tx.go) — go-sql-driver's session reset
// does not clear an open transaction, so a session that may still be in one
// must never go back to the pool. Correctness is therefore safe either way, but
// closing with the request's own canceled context would fail the ROLLBACK
// immediately and burn one pinned session on every client disconnect. Reads
// never commit.
//
// It is provider-only, and says so rather than dereferencing nil: a
// roles-backed server has no unit of work to open, and the roles it does hold
// own their own transactions.
func (s *Server) WithUOW(ctx context.Context, rec *reqInfo, fn func(uow.UnitOfWork) error) error {
	if s.provider == nil {
		return errors.New("httpapi: this server has no unit-of-work provider; it answers from configured issue roles")
	}
	start := time.Now()
	uw, err := s.provider.NewUOW(ctx)
	if rec != nil {
		rec.uowWait = time.Since(start)
	}
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), uowCloseTimeout)
		defer cancel()
		uw.Close(closeCtx)
	}()
	return fn(uw)
}

// acquire takes a database slot, or gives up. A timed-out wait is ErrBusy, not
// a request parked for the full deadline and then answered with a
// non-retryable 500.
func (s *Server) acquire(ctx context.Context, rec *reqInfo) (release func(), err error) {
	start := time.Now()
	release = func() { <-s.limits.sem }

	select {
	case s.limits.sem <- struct{}{}:
		rec.semWait = time.Since(start)
		return release, nil
	default:
	}

	timer := time.NewTimer(orDefault(s.limits.semTimeout, semAcquireTimeout))
	defer timer.Stop()
	select {
	case s.limits.sem <- struct{}{}:
		rec.semWait = time.Since(start)
		s.noteSaturation(rec, "acquired")
		return release, nil
	case <-timer.C:
		rec.semWait = time.Since(start)
		s.event("semaphore_timeout", "request_id", rec.id, "wait_ms", millis(rec.semWait),
			"inflight", maxInflight, "conns", s.network.liveConns.Load())
		return nil, ErrBusy
	case <-ctx.Done():
		// The client hung up, or the request deadline expired, while queued.
		// Still a saturation datapoint: it is the same wedge, observed from a
		// request that did not live long enough to be shed.
		rec.semWait = time.Since(start)
		s.noteSaturation(rec, "abandoned")
		return nil, ctx.Err()
	}
}

// noteSaturation logs a wait that lasted long enough to matter. This is the
// signal that separates "wedged" from "no traffic" at 3 a.m., because /healthz
// stays green either way.
func (s *Server) noteSaturation(rec *reqInfo, outcome string) {
	if rec.semWait < orDefault(s.limits.semWarn, saturationWarn) {
		return
	}
	s.event("semaphore_saturated",
		"request_id", rec.id, "wait_ms", millis(rec.semWait),
		"inflight", maxInflight, "conns", s.network.liveConns.Load(), "outcome", outcome)
}

func orDefault(v, fallback time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return fallback
}

// handler builds the whole request path: the route table's registrations, the
// catch-all that keeps unrouted paths on the same error shape, and the
// middleware in front of both.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	// Rows carrying a customMethod SHARE a pattern, so they get one
	// registration between them and a dispatcher in front. Collected in table
	// order, which is the order customMethodTarget tries the suffixes in.
	shared := map[string][]route{}
	var sharedOrder []string
	for _, rt := range routeTable {
		if rt.customMethod == "" {
			mux.Handle(rt.method+" "+rt.pattern, s.route(rt))
			continue
		}
		key := rt.method + " " + rt.pattern
		if _, seen := shared[key]; !seen {
			sharedOrder = append(sharedOrder, key)
		}
		shared[key] = append(shared[key], rt)
	}
	for _, key := range sharedOrder {
		mux.Handle(key, s.dispatchCustomMethod(shared[key]))
	}

	// Not an operation and deliberately not in the route table: it exists so
	// that an unrouted path still answers with problem+json rather than
	// net/http's text/plain default, which the document promises for EVERY
	// non-2xx byte. A method mismatch on a known path lands here too and
	// answers 404 rather than 405, because 405 is not in the v0 vocabulary.
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.fail(w, r, newResult(CodeNotFound, "no such route on this server"))
	}))

	return s.withRequestContext(s.checkHost(mux))
}
