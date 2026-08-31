package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// statusWriter records the status for the log line and bounds how long any one
// write may stall. It intentionally does not buffer the body: an unlimited read
// must stream.
type statusWriter struct {
	http.ResponseWriter
	rc     *http.ResponseController
	budget time.Duration
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.extendWriteDeadline(w.budget)
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.extendWriteDeadline(w.budget)
	return w.ResponseWriter.Write(b)
}

// extendWriteDeadline rolls the connection's write deadline d into the future.
// Rolling it before every write bounds each write rather than the transfer: a
// client that keeps reading streams a body of any size, while one that stops
// reading fails its handler within d — which is what lets the deferred
// semaphore release and unit-of-work rollback actually run.
//
// SetWriteDeadline is unsupported on a ResponseWriter with no connection under
// it (httptest's recorder), where there is nothing to stall; the error is
// dropped for that reason and no other.
func (w *statusWriter) extendWriteDeadline(d time.Duration) {
	if w.rc == nil || d <= 0 {
		return
	}
	_ = w.rc.SetWriteDeadline(time.Now().Add(d))
}

// Unwrap keeps http.ResponseController working through the wrapper, so a
// handler that needs to flush a large streamed page still can.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// hostPolicy is the set of Host header values this server answers to. It is
// data rather than a closure so the startup line can state the whole policy,
// and so the wildcard case below is a visible rule instead of an absence.
type hostPolicy struct {
	// ips are the numeric addresses allowed, matched with net.IP.Equal so every
	// spelling of one address matches: [0:0:0:0:0:0:0:1] and [::ffff:127.0.0.1]
	// are the same hosts as ::1 and 127.0.0.1, and a client that spells one of
	// them the long way is not an attacker.
	ips []net.IP
	// names are the allowed non-numeric Host values, lowercased. "localhost"
	// is always there; an operator may enumerate more with --allowed-host, and
	// nothing else can add one. Matching is EXACT — no wildcard and no suffix
	// syntax — so the allowlist is precisely what was enumerated.
	names map[string]bool
	// anyIP additionally allows ANY numeric Host literal. Only a wildcard bind
	// sets it; see newHostPolicy for why that is still a rebinding defense.
	anyIP bool
}

// newHostPolicy returns the Host policy implied by a bind address.
//
// The loopback spellings are always allowed, and the bind's own address is too
// — including an alternate loopback bind like 127.0.0.2, whose clients dial
// exactly that address and would otherwise be refused by the defense meant to
// protect them.
//
// A WILDCARD bind (0.0.0.0, ::) has no single configured address to allow, so
// it allows any numeric IP literal instead — and still refuses foreign DNS
// names, which is the whole defense. A rebound page cannot produce an IP-literal
// Host: the browser sends the hostname from the attacker's URL, and fetching an
// IP URL directly is a direct connection, which is the exposure the operator
// accepted when they passed --allow-non-loopback. Disabling the check outright
// would instead surrender the defense on the serving host's own loopback
// interface, which is rebinding's canonical target, and on every LAN browser
// behind a firewall the attacker cannot otherwise reach.
// EXTRA is the operator's enumerated additions (--allowed-host). A deployment
// where clients dial a service DNS name is refused by the policy above on every
// single request, so without this the server is unreachable rather than
// protected. Admitting a name the operator named does not weaken the defense
// the check exists for: a rebound page still cannot make a browser send that
// Host to 127.0.0.1, and the in-cluster clients that do send it are not
// browsers. Numeric values land in ips, so a pod IP can be enumerated too.
func newHostPolicy(bind net.IP, extra []string) hostPolicy {
	p := hostPolicy{
		ips:   []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		names: map[string]bool{"localhost": true},
		anyIP: bind.IsUnspecified(),
	}
	if !p.anyIP && !containsIP(p.ips, bind) {
		p.ips = append(p.ips, bind)
	}
	for _, host := range extra {
		h := hostOnly(host)
		if ip := net.ParseIP(h); ip != nil {
			if !containsIP(p.ips, ip) {
				p.ips = append(p.ips, ip)
			}
			continue
		}
		p.names[h] = true
	}
	return p
}

// ValidateAllowedHost refuses an allowlist entry that is not a bare host.
//
// The Host header's port is stripped before matching (hostOnly), so an entry
// carrying one would silently never match — and an operator who wrote it would
// reasonably read the startup line as proof that it does. A URL, a path or
// embedded whitespace is the same mistake in a louder form.
func ValidateAllowedHost(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("--allowed-host is empty; pass the Host header value clients send, such as bd-myproject.beads.svc.cluster.local")
	}
	if strings.ContainsAny(v, " \t\r\n") {
		return fmt.Errorf("--allowed-host %q contains whitespace; it must be a bare host name or IP", v)
	}
	if strings.ContainsAny(v, "/@") {
		return fmt.Errorf("--allowed-host %q looks like a URL; pass just the host, with no scheme and no path", v)
	}
	// An IPv6 address is spelled in brackets in a Host header, so an operator
	// copying one off the wire types it that way. hostOnly strips them before
	// matching, so the entry works; refusing it here — with a message about a
	// port it does not have — would be the validation lying about the policy.
	if net.ParseIP(strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")) != nil {
		return nil
	}
	if strings.Contains(v, ":") {
		return fmt.Errorf("--allowed-host %q carries a port; the port is stripped from a request's Host before matching, so an entry with one could never match", v)
	}
	return nil
}

// label renders the policy for the startup line, so an operator can read what
// this server will answer to without deducing it from the bind address.
func (p hostPolicy) label() string {
	parts := make([]string, 0, len(p.ips)+len(p.names)+1)
	for _, ip := range p.ips {
		parts = append(parts, ip.String())
	}
	parts = append(parts, slices.Sorted(maps.Keys(p.names))...)
	if p.anyIP {
		parts = append(parts, "any-ip-literal (wildcard bind)")
	}
	return strings.Join(parts, ",")
}

func containsIP(ips []net.IP, want net.IP) bool {
	return slices.ContainsFunc(ips, want.Equal)
}

// hostOnly strips the port and any IPv6 brackets from a Host header value.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return strings.ToLower(host)
}

// newIDPrefix draws one random prefix per process so ids from two servers, or
// from two runs, never collide in a shared log.
func newIDPrefix() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("httpapi: request id seed: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *Server) nextID() string {
	return fmt.Sprintf("%s-%06d", s.identity.idPrefix, s.identity.idSeq.Add(1))
}

func (s *Server) logStartup() {
	s.event("startup",
		"addr", s.Addr(),
		"mode", s.cfg.Mode,
		"db", s.dbSource(),
		"workspace", s.cfg.Workspace.RepoRoot,
		"beads_dir", s.cfg.Workspace.BeadsDir,
		"database", s.cfg.Workspace.Database,
		"host_allowlist", s.network.hosts.label(),
		"capabilities", strings.Join(s.identity.ctxBody.Capabilities, ","),
		// Whether this server requires a credential is the first thing an
		// operator checks after a deploy, and the last thing they should have
		// to infer from the absence of a flag in a process listing.
		"auth", s.authLabel(),
	)

	limits := []any{
		"max_inflight", maxInflight,
		"max_conns", maxConns,
		"sem_wait", semAcquireTimeout.String(),
		"deadline", requestDeadline.String(),
	}
	// The pool bounds are this server's, applied to the provider above. On the
	// roles source there is no pool here to bound, and printing the numbers
	// anyway would report limits nothing enforces.
	if s.provider != nil {
		limits = append(limits,
			"pool_max_open", servePoolLimits.MaxOpenConns,
			"pool_max_idle", servePoolLimits.MaxIdleConns,
			"pool_idle_time", servePoolLimits.ConnMaxIdleTime.String(),
			"pool_lifetime", servePoolLimits.ConnMaxLifetime.String(),
		)
	}
	s.event("limits", limits...)
}

// dbSource names which database source this server was built from, for the
// startup line.
//
// It is there so uow_ms is attributable. That field means "how long this
// request spent OBTAINING units of work", and a roles-backed server obtains
// none — so every one of its request lines reads uow_ms=0.000, which is the
// true value and is indistinguishable, on its own, from instrumentation that
// broke. This is the line that tells them apart.
func (s *Server) dbSource() string {
	if s.provider != nil {
		return "provider"
	}
	return "roles"
}

// event writes one structured stderr line. Values are quoted when they are not
// bare tokens, so a path or an error message can never inject a field — or a
// whole line — into the log.
func (s *Server) event(name string, kv ...any) {
	var b strings.Builder
	b.WriteString("event=")
	b.WriteString(name)
	fieldCount := len(kv)
	for i := 0; i+1 < fieldCount; i += 2 {
		key, _ := kv[i].(string)
		b.WriteByte(' ')
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(logValue(kv[i+1]))
	}
	s.output.log.Print(b.String())
}

// logValue renders one value of the k=v request line, quoting anything that
// would disturb either the line or the terminal reading it.
//
// Two audiences, two rules. The k=v framing needs space, '"' and '=' quoted, or
// a caller-supplied value forges fields and whole lines. The operator's console
// needs every CONTROL character quoted, C1 included: an unquoted U+009B is a
// CSI introducer, so a refusal recorded from a request body member name or a
// Content-Type header would paint the terminal of whoever tails the log. Bytes
// 0x80-0xFF are legal obs-text in an HTTP/1 field value and arrive here as
// invalid UTF-8 rather than as runes, so validity is checked separately —
// ContainsFunc would see only U+FFFD, which is not a control character.
func logValue(v any) string {
	str, ok := v.(string)
	if !ok {
		return fmt.Sprint(v)
	}
	if str == "" {
		return `""`
	}
	if !utf8.ValidString(str) || strings.ContainsFunc(str, func(r rune) bool {
		return r <= ' ' || r == '"' || r == '=' || isControlChar(r)
	}) {
		return strconv.Quote(str)
	}
	return str
}

func millis(d time.Duration) string {
	return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 3, 64)
}
