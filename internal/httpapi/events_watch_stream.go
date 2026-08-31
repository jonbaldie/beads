package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jonbaldie/beads/internal/eventsjournal"
	"github.com/jonbaldie/beads/internal/storage"
)

type serverStreams struct {
	watchPoll time.Duration
	watchBeat time.Duration

	closing     chan struct{}
	closingOnce sync.Once

	watchStreams    atomic.Int64
	maxWatchStreams int
}

// closeStreams signals every streaming handler that this server is shutting
// down. Idempotent: http.Server calls its registered hooks once per Shutdown,
// and a second Shutdown must not panic on an already-closed channel.
func (s *Server) closeStreams() {
	s.streams.closingOnce.Do(func() { close(s.streams.closing) })
}

// handleWatchEvents answers GET /v0/beads/events:watch.
//
// EVERY REFUSAL IS AN ORDINARY PROBLEM RESPONSE, and the ordering below is what
// makes that true: validation, activation, transport, capacity, and then the
// first read — all of it before a single stream byte. Once the 200 and its
// text/event-stream header are on the wire the status is spent, and a
// truncation discovered after that can only be reported inside the stream (see
// the `truncated` event). The connect-time half of the contract is therefore
// byte-identical to the poll endpoint's: same 400s, same 409, same 410.
func (s *Server) handleWatchEvents(w http.ResponseWriter, r *http.Request) {
	rec := requestInfo(r.Context())
	cursor, ok := s.watchCursor(w, r, rec)
	if !ok {
		return
	}
	if !s.cfg.EventsJournalEnabled {
		s.fail(w, r, EventsJournalDisabled())
		return
	}
	if !s.prepareWatchTransport(w, r, rec) {
		return
	}

	release, admitted := s.admitWatchStream(rec)
	if !admitted {
		s.fail(w, r, EventsWatchSaturated())
		return
	}
	defer release()
	clearRequestReadDeadline(w)
	journal, page, ok := s.watchFirstPage(w, r, rec, cursor)
	if !ok {
		return
	}
	s.streamEvents(w, r, journal, cursor, page)
}

// watchCursor validates the query and resume header before the stream spends
// capacity or touches the journal.
func (s *Server) watchCursor(w http.ResponseWriter, r *http.Request, rec *reqInfo) (int64, bool) {
	q := newQuery(r.URL.Query())
	since := q.integer64("since")
	if !s.acceptQuery(w, r, q) {
		return 0, false
	}
	if since == nil || *since < 0 {
		rec.refuse("since")
		s.fail(w, r, InvalidArgument("since", ReasonInvalidValue,
			"since is required and must be zero or a positive sequence number; use 0 to read from the beginning"))
		return 0, false
	}
	cursor := *since
	if raw := r.Header.Get(lastEventIDHeader); raw != "" {
		resumed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || resumed < 0 {
			rec.refuse(lastEventIDHeader)
			s.fail(w, r, InvalidArgument(lastEventIDHeader, ReasonInvalidValue,
				"Last-Event-ID must be a journal sequence number: zero or a positive 64-bit integer, as this stream emits it"))
			return 0, false
		}
		cursor = resumed
	}
	return cursor, true
}

func (s *Server) prepareWatchTransport(w http.ResponseWriter, r *http.Request, rec *reqInfo) bool {
	if canFlush(w) {
		return true
	}
	s.event("events_watch_unflushable", "request_id", rec.id, "writer", fmt.Sprintf("%T", w))
	s.fail(w, r, newResult(CodeInternal, ""))
	return false
}

func (s *Server) watchFirstPage(w http.ResponseWriter, r *http.Request, rec *reqInfo, cursor int64) (storage.EventsJournalCursor, storage.EventsJournalPage, bool) {
	journal, err := s.eventsJournalCursor(r)
	if err != nil {
		s.failErr(w, r, err)
		return nil, storage.EventsJournalPage{}, false
	}
	page, err := s.readWatchBatch(r.Context(), rec, journal, cursor)
	if err != nil {
		s.failErr(w, r, err)
		return nil, storage.EventsJournalPage{}, false
	}
	return journal, page, true
}

// streamEvents writes the stream and owns its life.
//
// It returns when the client goes away, when the server begins shutting down,
// when a write fails, or when the journal is pruned out from under the cursor —
// and on every one of those paths the caller's deferred release gives the
// stream slot back. There is no other exit.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, journal storage.EventsJournalCursor, since int64, page storage.EventsJournalPage) {
	ctx := r.Context()
	rec := requestInfo(ctx)

	stream := newEventStream(w, func(name string, kv ...any) {
		s.event(name, append([]any{"request_id", rec.id}, kv...)...)
	})
	stream.open(eventsWatchRetry)
	passes := &reqInfo{id: rec.id}
	interval := orDefault(s.streams.watchPoll, watchPollInterval)
	poll := time.NewTimer(watchPollDelay(interval))
	defer poll.Stop()
	beat := orDefault(s.streams.watchBeat, watchHeartbeat)
	quiet := time.Now()

	for {
		if !writeWatchPage(stream, page, &since, &quiet, beat) {
			return
		}
		if !s.waitForWatchPass(ctx, poll, interval, len(page.Rows) == eventsWatchBatch) {
			return
		}

		next, err := s.readWatchBatch(ctx, passes, journal, since)
		if err == nil {
			page = next
			continue
		}
		if !s.handleWatchReadError(stream, rec, since, err) {
			return
		}
	}
}

func writeWatchPage(stream *eventStream, page storage.EventsJournalPage, since *int64, quiet *time.Time, beat time.Duration) bool {
	for _, row := range page.Rows {
		if !stream.record(row) {
			return false
		}
		*since = row.Seq
	}
	if len(page.Rows) > 0 {
		stream.flush()
		*quiet = time.Now()
	} else if time.Since(*quiet) >= beat {
		stream.comment("heartbeat")
		*quiet = time.Now()
	}
	return !stream.failed()
}

func (s *Server) waitForWatchPass(ctx context.Context, poll *time.Timer, interval time.Duration, fullBatch bool) bool {
	if fullBatch {
		select {
		case <-ctx.Done():
			return false
		case <-s.streams.closing:
			return false
		default:
			return true
		}
	}
	select {
	case <-ctx.Done():
		return false
	case <-s.streams.closing:
		return false
	case <-poll.C:
		poll.Reset(watchPollDelay(interval))
		return true
	}
}

func (s *Server) handleWatchReadError(stream *eventStream, rec *reqInfo, since int64, err error) bool {
	var truncated *storage.EventsJournalTruncatedError
	switch {
	case errors.Is(err, ErrBusy):
		return true
	case errors.As(err, &truncated):
		stream.truncate(truncatedFrame(rec, truncated))
		return false
	case errors.Is(err, context.Canceled):
		return false
	default:
		s.event("events_watch_failed", "request_id", rec.id, "since", since, "error", err.Error())
		return false
	}
}

// readWatchBatch performs one pass of the loop's read, holding a database slot
// for exactly as long as the read takes.
//
// A STREAM MUST NOT HOLD A SLOT WHILE IT WAITS. There are sixteen and this
// server admits sixty-four streams; one slot per open stream would let a
// handful of idle consumers starve every other operation on the server for
// hours, with /healthz green throughout. Taking the slot per read keeps the
// invariant the semaphore exists for — nothing touches the database without one
// — while charging a stream only for the moments it actually does.
//
// The read carries its own deadline for the same reason the route's per-request
// one exists. That deadline does not apply to this operation (the response has
// no bounded length), so without one here a wedged database would park a read,
// and its slot, for the life of a connection nobody is watching. It is a
// shorter budget than the route's, and eventsWatchReadDeadline says why.
func (s *Server) readWatchBatch(ctx context.Context, rec *reqInfo, journal storage.EventsJournalCursor, since int64) (storage.EventsJournalPage, error) {
	release, err := s.acquire(ctx, rec)
	if err != nil {
		return storage.EventsJournalPage{}, err
	}
	defer release()

	readCtx, cancel := context.WithTimeout(ctx, eventsWatchReadDeadline)
	defer cancel()
	return journal.ReadEventsJournalPage(readCtx, since, eventsWatchBatch)
}

// watchPollDelay spreads one stream's next read around the poll interval, by up
// to a tenth of it either way.
//
// Streams synchronize by themselves and there is nothing random to break the
// tie: every consumer reconnecting after a restart is handed the same `retry`
// delay, so they come back together and, on a fixed interval, read together
// forever after — one burst of up to maxWatchStreams reads per second against a
// semaphore that admits sixteen. Jittering every wait rather than only the first
// keeps them spread even after a pass that took longer than its interval.
//
// crypto/rand because it is the one source in this process that needs no seed
// and no justification; the cost is a two-byte read once per stream per second.
// A failed read falls back to the flat interval, which is the behavior this
// function is improving on rather than depending on.
func watchPollDelay(interval time.Duration) time.Duration {
	span := interval / 5
	if span <= 0 {
		return interval
	}
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return interval
	}
	offset := time.Duration(int64(span) * int64(binary.BigEndian.Uint16(b[:])) / (1 << 16))
	return interval - span/2 + offset
}

// clearRequestReadDeadline drops http.Server.ReadTimeout for this request.
//
// ReadTimeout is a deadline on the WHOLE request, and net/http keeps a
// background read running on the connection while a handler writes — the read
// that notices a disconnect. When that read hits the deadline, net/http treats
// it as a dead connection and CANCELS the request context, so a stream would end
// itself at ReadTimeout with no client involved and nothing in the log to say
// why. Clearing it is per-request: the next request on a reused connection gets
// its deadlines set fresh.
//
// It does not weaken disconnect detection. A client that goes away still
// produces a read error, which still cancels the context; what is lost is only
// the TIME limit, which is the thing SSE requires — a healthy consumer sends
// nothing for the life of its stream. A peer that vanishes without a FIN or RST
// is reaped by TCP keepalive, which Go's listener enables by default (see the
// note in engdocs/SERVE_RUNBOOK.md for the measured window).
//
// The WRITE deadline needs no such handling and must not be cleared:
// statusWriter rolls it forward immediately before every write, so it bounds one
// stalled write rather than the stream, which is exactly right here.
//
// The error is dropped for the reason extendWriteDeadline drops its own: a
// ResponseWriter with no connection under it (httptest's recorder) has no
// deadline to clear and nothing to time out.
func clearRequestReadDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
}

// truncatedFrame is the body the mid-stream `truncated` event carries: the
// EXACT problem document the connect-time 410 would have carried, request id
// and all.
//
// One shape rather than two. A consumer meets this condition on both surfaces —
// a 410 when it reconnects, this event when a prune races an open stream — and
// a second encoding of the same three numbers is a second contract to keep in
// step. Whatever handles the 410 handles this, unchanged.
func truncatedFrame(rec *reqInfo, err *storage.EventsJournalTruncatedError) []byte {
	res := EventsJournalTruncated(err).WithRequestID(rec.id)
	body, marshalErr := json.Marshal(res.Problem)
	if marshalErr != nil {
		// Unreachable: apigen.Problem is plain data. The stream still has to
		// end, and it ends with something a consumer can parse as this event.
		return []byte(`{"code":"` + string(CodeEventsJournalTruncated) + `"}`)
	}
	return body
}

// admitWatchStream reserves one of the concurrent-stream slots, or reports that
// this server is already holding as many as it will.
//
// The counter is incremented FIRST and rolled back on refusal, rather than
// compared and then incremented: two connects arriving together would both read
// the same count and both be admitted, which is how a cap ends up being
// advisory. The release the caller defers is the only decrement, so it runs on
// every exit path a stream has.
func (s *Server) admitWatchStream(rec *reqInfo) (release func(), admitted bool) {
	limit := int64(maxWatchStreams)
	if s.streams.maxWatchStreams > 0 {
		limit = int64(s.streams.maxWatchStreams)
	}
	live := s.streams.watchStreams.Add(1)
	if live > limit {
		s.streams.watchStreams.Add(-1)
		s.event("events_watch_saturated", "request_id", rec.id, "streams", live-1, "max_streams", limit)
		return nil, false
	}
	// The gauge on ADMISSION, not only on refusal. Streams accumulate over hours
	// and the refusal is the cliff; a line per connect carrying the live count
	// against the limit is what lets an operator watch the approach instead of
	// discovering it from a consumer's 503. One line per stream, not per record.
	s.event("events_watch_admitted", "request_id", rec.id, "streams", live, "max_streams", limit)
	return func() { s.streams.watchStreams.Add(-1) }, true
}

// canFlush reports whether w can push bytes to the client before the handler
// returns, without writing anything to find out.
//
// http.ResponseController answers the same question, but only by attempting a
// Flush — which writes the header and spends the status. This surface has to
// know before it decides between a 200 stream and a 500, so it walks the
// wrapper chain the same way the controller does. The bound is against a writer
// whose Unwrap returns itself; net/http's own walk is unbounded, and a hang in
// a handler is worse than a missed capability.
func canFlush(w http.ResponseWriter) bool {
	for range 8 {
		if _, ok := w.(http.Flusher); ok {
			return true
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = unwrapper.Unwrap()
	}
	return false
}

// eventStream frames one text/event-stream response.
//
// It remembers the FIRST write error and turns every later call into a no-op,
// so the loop above reads like the happy path and still stops at the first
// broken pipe. There is no buffering: each frame is written and flushed on its
// way out, because a record held back is a record the consumer has not seen.
type eventStream struct {
	w  http.ResponseWriter
	rc *http.ResponseController
	// note records a condition the response itself cannot report, because the
	// status is spent. Exactly one thing uses it — see record — and write
	// failures deliberately do not: a client hanging up is ordinary and is not a
	// server event.
	note func(name string, kv ...any)
	err  error
}

func newEventStream(w http.ResponseWriter, note func(name string, kv ...any)) *eventStream {
	return &eventStream{w: w, rc: http.NewResponseController(w), note: note}
}

// open writes the response headers and the reconnection delay, and flushes them
// so a client's connect completes before the first record exists.
func (e *eventStream) open(retryMillis int) {
	h := e.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	// Buffering is the one intermediary behavior that silently defeats a
	// stream: a reverse proxy that accumulates the response delivers every
	// record at once, late. This is the de-facto header for turning it off.
	//
	// Cache-Control is deliberately NOT set here. withRequestContext already
	// sets `no-store` on every response, which forbids storing the body at all
	// — strictly stronger than the `no-cache` an SSE example usually shows —
	// and a second, weaker value would be the only thing this operation
	// disagreed with the rest of the surface about.
	h.Set("X-Accel-Buffering", "no")

	// The request read deadline is already gone — clearRequestReadDeadline runs
	// before the connect read, so that read is bounded by its own deadline and
	// not by the residue of http.Server.ReadTimeout. Nothing to do here.
	e.w.WriteHeader(http.StatusOK)
	e.write(fmt.Sprintf("retry: %d\n\n", retryMillis))
	e.flush()
}

// record emits one journal record as one SSE event, and reports whether the
// stream is still writable.
//
// `id:` IS THE CURSOR. It carries the record's seq, which is what a client
// sends back as Last-Event-ID and what it would have passed as `since` while
// polling — one checkpoint, three spellings, all the same number.
//
// The event is UNNAMED on purpose: the default `message` type is what a bare
// `es.onmessage` receives, so the ordinary case needs no listener registration
// at all. The one named event on this stream is `truncated`, and naming only
// the exception is what lets a consumer treat "an event I do not recognize" as
// "stop, this is not a record".
//
// ONE `data:` LINE, ALWAYS. An SSE frame is line-oriented and a raw newline
// inside a data line would split one record into two, so the guarantee has to
// come from the encoding rather than from hope: this is encoding/json output,
// and the encoder escapes every control character — including U+000A and
// U+000D — inside every string it writes. The payload members travel as
// json.RawMessage, so they are not re-encoded here; they are themselves the
// output of a marshaler for the same reason.
// TestEventsWatchFramesEveryRecordOnOneLine drives a record whose payloads
// carry literal newlines and carriage returns and pins that the frame stays one
// line.
func (e *eventStream) record(row storage.EventsJournalRow) bool {
	if e.err != nil {
		return false
	}
	encoded, err := json.Marshal(eventsjournal.NewRecord(row))
	if err != nil {
		// Unreachable for a well-formed row, and there is no way to tell the
		// CLIENT: the 200 is long since on the wire. So it goes in the log
		// naming the seq, because the alternative is the worst diagnosis on this
		// surface — a stream that ends silently, and ends again at the same
		// record on every reconnect, with nothing anywhere to say which row is
		// unencodable. Ending the stream is still the right behavior; skipping
		// the record would be silent loss.
		e.note("events_watch_failed", "seq", row.Seq, "error", err.Error())
		e.err = err
		return false
	}
	e.write(fmt.Sprintf("id: %d\ndata: %s\n\n", row.Seq, encoded))
	return e.err == nil
}

// comment emits a line no consumer sees: an SSE comment, which exists to put
// bytes on an idle connection.
func (e *eventStream) comment(text string) {
	e.write(": " + text + "\n\n")
	e.flush()
}

// truncate ends the stream with the one named event it can emit.
//
// The reconnection delay is RAISED FIRST, and the order matters: a client that
// reconnects on this event reaches the connect-time 410 and can do nothing
// about it, so the last instruction this server gets to give is "come back
// slowly". A consumer that handles the event properly stops and re-baselines
// and never spends the delay.
func (e *eventStream) truncate(body []byte) {
	e.write(fmt.Sprintf("retry: %d\n\n", eventsWatchTruncatedRetry))
	e.write(fmt.Sprintf("event: truncated\ndata: %s\n\n", body))
	e.flush()
}

func (e *eventStream) write(frame string) {
	if e.err != nil {
		return
	}
	// gosec's taint analysis flags every direct write to a ResponseWriter as a
	// possible XSS sink because it cannot see a Content-Type. It is
	// text/event-stream here, set in open, which no browser parses as a
	// document: EventSource hands the data to script as a string and renders
	// nothing. The only caller-derived value that reaches these frames is a
	// journal record, and it arrives as encoding/json output.
	//nolint:gosec // G705: text/event-stream body, JSON-encoded content, no HTML sink.
	if _, err := fmt.Fprint(e.w, frame); err != nil {
		e.err = err
	}
}

func (e *eventStream) flush() {
	if e.err != nil {
		return
	}
	if err := e.rc.Flush(); err != nil {
		e.err = err
	}
}

// failed reports whether a write has already failed, which is the loop's signal
// that this connection is over.
func (e *eventStream) failed() bool { return e.err != nil }
