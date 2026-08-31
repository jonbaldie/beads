package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// reqInfo is the per-request record the log line is assembled from. Layers fill
// in what they know; the outermost middleware writes it.
type reqInfo struct {
	id      string
	op      string
	status  int
	code    Code
	semWait time.Duration
	uowWait time.Duration
	// refused is the caller-supplied value a middleware turned down: the Host
	// this server does not answer to, or the unrecognized parameter name. It
	// goes on the request line so a refusal is attributable — a rebinding probe
	// that leaves no server-side trace is a control nobody can investigate.
	// logValue quotes it, which is what makes logging attacker-controlled text
	// safe.
	//
	// A pointer, so that the request line carries the field only when there was
	// a refusal: the empty parameter name in `?=1` is a refusal with an empty
	// value, not the absence of one.
	refused *string
}

// refuse records the value a middleware turned down, for the request line.
func (rec *reqInfo) refuse(value string) { rec.refused = &value }

type reqInfoKey struct{}

// requestInfo returns the record for the request in flight. It never returns
// nil: every request goes through withRequestContext, and handing back a
// detached record rather than nil means a mis-wired caller loses a log line
// instead of panicking mid-response.
func requestInfo(ctx context.Context) *reqInfo {
	if rec, ok := ctx.Value(reqInfoKey{}).(*reqInfo); ok {
		return rec
	}
	return &reqInfo{}
}

// allows reports whether a Host header value is one this server answers to.
func (p hostPolicy) allows(host string) bool {
	h := hostOnly(host)
	if p.names[h] {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	return p.anyIP || containsIP(p.ips, ip)
}

// withRequestContext assigns the correlation id, applies response-wide
// headers, recovers panics, and writes the one log line per request. It is
// outermost so that a request refused by the Host check is logged like any
// other.
func (s *Server) withRequestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &reqInfo{id: s.nextID(), status: http.StatusOK}
		r = r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, rec))

		// No client or intermediary may cache an answer about live work.
		w.Header().Set("Cache-Control", "no-store")

		sw := &statusWriter{
			ResponseWriter: w,
			rc:             http.NewResponseController(w),
			budget:         orDefault(s.limits.writeStall, writeStallTimeout),
		}
		// Arm before the handler runs: a handler that writes nothing still has a
		// response, written by net/http on the way out.
		sw.extendWriteDeadline(sw.budget)

		start := time.Now()

		// Deferred, so that the two things this middleware promises — an answer
		// in the documented shape, and one log line per request — survive the
		// one failure where correlating them matters most.
		defer func() {
			p := recover()
			if p != nil && p != http.ErrAbortHandler {
				s.panicked(sw, r, rec, p)
			}

			// net/http flushes what is left of the buffered response after the
			// handler returns, so extend once more to cover it. The extension has
			// to outlast the idle timeout too: the deadline stays armed while a
			// keep-alive connection waits for its next request, and net/http
			// answers some requests (a malformed request line, oversized headers)
			// without reaching this middleware — an expired deadline would turn
			// those into a dropped connection. Anything that does reach here
			// re-arms above, before a handler writes a byte.
			sw.extendWriteDeadline(sw.budget + idleTimeout)

			if sw.status != 0 {
				// Still zero means the handler returned without writing
				// anything, in which case net/http has sent the 200 rec already
				// carries.
				rec.status = sw.status
			}

			fields := []any{
				"request_id", rec.id,
				"op", rec.op,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"code", string(rec.code),
				"duration_ms", millis(time.Since(start)),
				"sem_wait_ms", millis(rec.semWait),
				"uow_ms", millis(rec.uowWait),
				// The connection gauge belongs on the busiest line in the log:
				// it is the only place an operator watches it climb toward the
				// cap. remote_addr answers "which client", which on loopback
				// means "which local process" via the port.
				"conns", s.network.liveConns.Load(),
				"remote_addr", r.RemoteAddr,
			}
			if rec.refused != nil {
				fields = append(fields, "refused", *rec.refused)
			}
			s.event("request", fields...)

			if p == http.ErrAbortHandler {
				// net/http's documented "abandon this response silently" signal.
				// It gets its log line like every other request, and then it gets
				// the abort it asked for.
				panic(p)
			}
		}()

		next.ServeHTTP(sw, r)
	})
}

// panicked gives a panicking handler the same shape as every other failure: one
// problem+json response and one log line, both carrying the request id.
//
// Without it the panic reaches net/http's per-connection recover, which prints
// an unstructured stack trace to stderr with nothing on it to tie to a client
// report, drops the connection with no body at all, and skips the request line —
// so the one class of failure where correlation matters most is the one class
// that has none. The panic text stays out of the response for the same reason
// every other 5xx detail does.
func (s *Server) panicked(sw *statusWriter, r *http.Request, rec *reqInfo, p any) {
	rec.code = CodeInternal
	s.event("panic",
		"request_id", rec.id,
		"op", rec.op,
		"method", r.Method,
		"path", r.URL.Path,
		"error", fmt.Sprint(p),
		"stack", string(debug.Stack()),
	)
	if sw.status != 0 {
		// The response is already on the wire; a truncated body is all the
		// client can be told, and writing a second header would only add a
		// superfluous-WriteHeader line to the log.
		return
	}
	s.fail(sw, r, newResult(CodeInternal, ""))
}

// checkHost is the DNS-rebinding defense. An unauthenticated service on
// loopback is reachable from any browser on the host; a page that re-resolves
// its own name to 127.0.0.1 issues requests the browser treats as same-origin,
// so no CORS rule stops them. What the browser does preserve is the attacker's
// hostname in Host, which is what this rejects.
func (s *Server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.network.hosts.allows(r.Host) {
			requestInfo(r.Context()).refuse(r.Host)
			s.fail(w, r, InvalidArgument("Host", ReasonInvalidValue,
				"Host header is not one this server answers to"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// dispatchCustomMethod is the one registration the single-resource custom
// methods share. It splits the trailing `:verb` off the matched segment, hands
// the request to the row that claims it, and leaves the id where that row's
// handler reads it.
//
// The split happens BEFORE s.route, which is what makes an unrouted suffix cost
// nothing: it takes no database slot and books no operation on the request
// line, exactly as the catch-all's 404 does. Answering it from inside a row's
// handler — where the claim answered it while it was the only POST here — would
// attribute every probe of this prefix to whichever operation happened to be
// first in the table.
func (s *Server) dispatchCustomMethod(rows []route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt, id, res := customMethodTarget(rows, r.PathValue(customMethodPathValue))
		if res != nil {
			s.fail(w, r, *res)
			return
		}
		r.SetPathValue(customMethodIDValue, id)
		s.route(rt).ServeHTTP(w, r)
	})
}

// route wraps one operation with the limits that apply to it: the per-request
// deadline, the bearer credential unless the operation is exempt, the
// Bd-Project-Id stamp check unless the operation is exempt, and — unless the
// operation is exempt — a database slot.
//
// The credential check runs BEFORE the semaphore, which is the load-bearing
// ordering: a storm of refused requests then costs one SHA-256 each and can
// never occupy the slots, or the SQL connections pinned to them, that
// authenticated clients are waiting for. It runs inside withRequestContext, so
// a 401 gets a request id and a request log line like every other refusal.
//
// The stamp check runs AFTER the credential and before the semaphore: the
// project-mismatch refusal is the one that discloses this server's own project
// id (server_project_id), so it must sit behind the authentication gate — an
// unauthenticated caller is turned away by the 401 before the stamp is ever
// compared, and so learns nothing about the workspace's identity.
func (s *Server) route(rt route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := requestInfo(r.Context())
		rec.op = rt.op

		// A STREAMING OPERATION GETS NO DEADLINE. For every other row this is
		// the backstop that stops a request from holding resources forever; on a
		// held-open response it would do nothing but sever the stream at sixty
		// seconds. What replaces it is per-read: streamEvents holds no slot
		// between passes and bounds each read on its own.
		if !rt.streaming {
			ctx, cancel := context.WithTimeout(r.Context(), requestDeadline)
			defer cancel()
			r = r.WithContext(ctx)
		}

		if !rt.authExempt && s.identity.auth != nil && !s.authorize(w, r, rec) {
			return
		}

		// Identity before resources: a stamp for the wrong workspace is turned
		// away before it can buy a database slot or open a unit of work, so a
		// misdirected read or write costs nothing and mutates nothing. The two
		// exempt routes (liveness, identity handshake) skip it. It runs after
		// the credential check so the server_project_id it discloses stays
		// behind the authentication gate.
		if !rt.projectExempt {
			if res := s.checkProjectStamp(r); res != nil {
				s.fail(w, r, *res)
				return
			}
		}

		if !rt.bypassSemaphore {
			release, err := s.acquire(r.Context(), rec)
			if err != nil {
				s.failErr(w, r, err)
				return
			}
			defer release()
		}

		rt.handler(s, w, r)
	})
}

// authorize verifies the request's bearer credential, writing the 401 itself
// and reporting whether the handler may run.
//
// The presented credential appears in NO log field and NO response byte. It is
// deliberately not recorded through rec.refuse, which is defined as an echoed
// CALLER VALUE and goes on the request line: a Host header or a parameter name
// is attacker-controlled text worth attributing, and a token is a secret. What
// the log gets instead is the reason — which of the three client mistakes it
// was — and the request id that ties it to the response.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, rec *reqInfo) bool {
	token, reason := bearerCredential(r.Header.Get("Authorization"))
	if reason == "" {
		ok, reloadErr := s.identity.auth.Verify(token)
		if reloadErr != nil {
			// A reload that failed leaves the last-good token set in force, so
			// this is not a refusal on its own — but it means the file the
			// operator is rotating is unreadable, and nothing else would say so.
			s.event("auth_reload_error", "request_id", rec.id, "error", reloadErr.Error())
		}
		if ok {
			return true
		}
		reason = "unknown_token"
	}

	s.event("auth_refused", "request_id", rec.id, "op", rec.op,
		"reason", reason, "remote_addr", r.RemoteAddr)
	s.fail(w, r, newResult(CodeUnauthenticated, ""))
	return false
}

// bearerCredential extracts the token from an Authorization header value. It
// returns a non-empty reason instead when there is nothing to verify, so the
// log can separate a misconfigured client from a wrong or stale token.
//
// The scheme is matched case-insensitively, which RFC 9110 requires.
func bearerCredential(header string) (token, reason string) {
	if strings.TrimSpace(header) == "" {
		return "", "missing"
	}
	rest, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		scheme, tail, split := strings.Cut(header, " ")
		if !split || !strings.EqualFold(scheme, "Bearer") {
			return "", "malformed"
		}
		rest = tail
	}
	if token = strings.TrimSpace(rest); token == "" {
		return "", "malformed"
	}
	return token, ""
}

// authLabel describes the credential posture for the startup line. It names the
// token FILE, never a token: the path is configuration an operator already
// knows, and the contents are the secret.
func (s *Server) authLabel() string {
	if s.identity.auth == nil {
		return "none"
	}
	return "bearer (" + s.identity.auth.path + ")"
}

// checkProjectStamp enforces per-request workspace identity. A client that
// stamps a request with its intended workspace's project id in the
// Bd-Project-Id header is asserting "I mean to be talking to THIS workspace"; if
// the id it names is not the one this server serves, the request is refused
// before it can read or write the wrong workspace. It returns nil when the
// request may proceed, or the 400 to write.
//
// The comparison is LITERAL, and a server whose own project id is empty refuses
// any non-empty stamp: it cannot prove it is the workspace the client named, so
// it does not answer as if it were. That mirrors the identity handshake — a
// server advertises the id it can assert, and asserts nothing when it has none.
//
// Two paths return nil. An ABSENT header is the backward-compatible one: an
// older client never sends it, and enforcement triggers only when the header
// arrives, so this adds no precondition to any request already in the field. A
// stamp equal to the server's own project id is a match.
//
// The refusal is recorded on the request line like every other middleware
// refusal, so a client persistently addressing the wrong server is attributable
// on loopback down to the local process.
func (s *Server) checkProjectStamp(r *http.Request) *Result {
	stamp := r.Header.Get(ProjectIDHeader)
	if stamp == "" {
		return nil
	}
	if stamp == s.identity.ctxBody.ProjectId {
		return nil
	}
	requestInfo(r.Context()).refuse(stamp)
	res := ProjectMismatch(stamp, s.identity.ctxBody.ProjectId)
	return &res
}

// fail writes a problem response and records what it was for the log line.
// Every non-2xx byte this server emits goes through here or through
// handleNotImplemented.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, res Result) {
	rec := requestInfo(r.Context())
	rec.code = Code(res.Problem.Code)
	Write(w, res.WithRequestID(rec.id))
}

// failErr maps an error from the storage seam and logs the error text. On a 5xx
// that text goes to the log and NOWHERE else: driver and dial errors routinely
// embed the DSN, and the response detail is a fixed string per code. The
// request_id in both places is what reconnects them.
func (s *Server) failErr(w http.ResponseWriter, r *http.Request, err error) {
	rec := requestInfo(r.Context())
	res := ClassifyError(err)
	s.fail(w, r, res)

	// A client that hung up — while queued for a slot, or mid unit of work — is
	// not a server fault, and this is the moment it would be counted as one:
	// context.Canceled has nowhere better to go than the generic 500, and every
	// >=500 emits request_error. On a saturated server, which is exactly when
	// clients time out and disconnect, that turns impatient callers into a spike
	// in the one signal an operator alerts on. The status stays as classified
	// (it is written to a socket nobody is reading either way); only the
	// accounting changes.
	//
	// An EXPIRED request deadline is a different statement and keeps the 500:
	// nothing about it says the client left.
	if errors.Is(err, context.Canceled) {
		rec.code = codeClientClosed
		return
	}
	if res.Problem.Status >= 500 {
		s.event("request_error", "request_id", rec.id, "error", err.Error())
	}
}

// writeJSON emits a success body. The status is always 200: every 2xx on this
// surface is a 200, and every non-2xx byte goes through Write as problem+json
// instead — so a status parameter here would only ever be a way to write one
// that the document does not describe.
func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}
