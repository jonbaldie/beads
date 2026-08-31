package httpapi

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"golang.org/x/net/netutil"

	"github.com/jonbaldie/beads/internal/httpapi/apigen"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/uow"
)

type serverNetwork struct {
	listener net.Listener
	http     *http.Server
	hosts    hostPolicy

	maxConns      int
	liveConns     atomic.Int64
	connCapWarned atomic.Bool
}

type serverLimits struct {
	// sem bounds handlers that touch the database. Buffered channel rather
	// than sync.Semaphore so the acquisition can select on a timer.
	sem        chan struct{}
	semTimeout time.Duration
	semWarn    time.Duration
	writeStall time.Duration
}

type serverIdentity struct {
	auth     *TokenFileAuth
	ctxBody  apigen.ContextResponse
	idPrefix string
	idSeq    atomic.Uint64
}

type serverOutput struct {
	log    *log.Logger
	stdout io.Writer
}

// Server is one bound listener and the routes behind it. Build it with Listen,
// which binds before returning so the caller can read Addr, then run Serve.
type Server struct {
	cfg      Config
	provider uow.UnitOfWorkProvider
	roles    serverRoles
	network  serverNetwork
	limits   serverLimits
	streams  serverStreams
	identity serverIdentity
	output   serverOutput
}

// ValidateBindAddr enforces the bind posture, following the policy the managed
// Dolt child already lives under (validateManagedServerConfigPolicy in
// cmd/bd/proxied_server.go): the host must be a NUMERIC IP literal.
//
// Hostnames are refused, "localhost" included. A name is not a listener
// specification — it resolves to whatever the host's resolver says today, so
// the operator cannot tell from the flag which interfaces they just opened.
// Unix sockets are not supported at all; they fail here because they do not
// parse as host:port.
func ValidateBindAddr(addr string, allowNonLoopback bool) (net.IP, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("--addr %q must be HOST:PORT with a numeric IP literal host (unix sockets are not supported): %w", addr, err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return nil, fmt.Errorf("--addr %q: port must be a number from 0 to 65535 (0 picks an ephemeral port)", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("--addr %q: host must be a numeric IP literal, not a name — use 127.0.0.1 rather than localhost", addr)
	}
	if !ip.IsLoopback() && !allowNonLoopback {
		return nil, fmt.Errorf("--addr %q binds beyond loopback, which requires --allow-non-loopback (and, with it, --auth-token-file)", addr)
	}
	return ip, nil
}

// Listen validates the configuration, binds the listener, and reports the
// bound address on stdout and the startup state on stderr. It does not accept
// anything until Serve runs.
//
// There is no lock file, pid file or discovery file: bd serve is
// operator-invoked and the TCP bind IS the mutual exclusion, so a second
// instance on the same fixed port fails here with the operating system's own
// address-in-use error. (Under the ephemeral default that exclusion does not
// exist — N instances simply run on N ports — which is why fixed ports are the
// deployment recommendation.)
func Listen(cfg Config) (*Server, error) {
	ip, err := validateListenConfig(cfg)
	if err != nil {
		return nil, err
	}
	applyListenDefaults(&cfg)
	prefix, err := newIDPrefix()
	if err != nil {
		return nil, err
	}
	s := newServer(cfg, ip, prefix)
	if err := bindServer(s, cfg); err != nil {
		return nil, err
	}
	configureServerPool(s, cfg.Provider)
	reportServerReady(s)
	return s, nil
}

func validateListenConfig(cfg Config) (net.IP, error) {
	if err := checkDatabaseSource(cfg); err != nil {
		return nil, err
	}
	ip, err := ValidateBindAddr(cfg.Addr, cfg.AllowNonLoopback)
	if err != nil {
		return nil, err
	}
	// The same posture and allowlist rules the CLI applies, applied again here
	// so a second caller of this package cannot assemble a Config that serves
	// the whole surface to a network with no credential.
	if err := ValidateAuthPosture(cfg.AllowNonLoopback, cfg.Auth != nil, cfg.InsecureNoAuth); err != nil {
		return nil, err
	}
	for _, host := range cfg.AllowedHosts {
		if err := ValidateAllowedHost(host); err != nil {
			return nil, err
		}
	}
	return ip, nil
}

func applyListenDefaults(cfg *Config) {
	if cfg.Stdout == nil {
		cfg.Stdout = os.Stdout
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
}

func newServer(cfg Config, ip net.IP, prefix string) *Server {
	return &Server{
		cfg:      cfg,
		provider: cfg.Provider,
		roles: serverRoles{
			issue: serverIssueRoles{
				reader:       cfg.Reader,
				claimer:      cfg.Claimer,
				batchCloser:  cfg.BatchCloser,
				readyClaimer: cfg.ReadyClaimer,
				releaser:     cfg.Releaser,
				lifecycle:    cfg.Lifecycle,
				relations:    cfg.Relations,
				commenter:    cfg.Commenter,
				blocking:     cfg.BlockingAnnotator,
				dependencies: cfg.DependencyEditor,
				batchApplier: cfg.BatchApplier,
			},
			graph: serverGraphRoles{
				cycles:       cfg.CycleDetector,
				edges:        cfg.EdgeReader,
				edgeCounter:  cfg.GraphCounter,
				tree:         cfg.TreeWalker,
				readyCounter: cfg.ReadyCounter,
				counter:      cfg.Counter,
				querier:      cfg.Querier,
			},
			workspace: serverWorkspaceRoles{
				settings:     cfg.Settings,
				stats:        cfg.Stats,
				sweeper:      cfg.Sweeper,
				deleter:      cfg.Deleter,
				batchCreator: cfg.BatchCreator,
				metadataCAS:  cfg.MetadataCAS,
				memories:     cfg.Memories,
			},
			journal: serverJournalRoles{eventsJournal: cfg.EventsJournal},
		},
		network: serverNetwork{
			hosts:         newHostPolicy(ip, cfg.AllowedHosts),
			maxConns:      maxConns,
			liveConns:     atomic.Int64{},
			connCapWarned: atomic.Bool{},
		},
		limits: serverLimits{
			sem:        make(chan struct{}, maxInflight),
			semTimeout: semAcquireTimeout,
			semWarn:    saturationWarn,
			writeStall: 0,
		},
		streams: serverStreams{
			closing:         make(chan struct{}),
			maxWatchStreams: maxWatchStreams,
		},
		identity: serverIdentity{
			auth:     cfg.Auth,
			ctxBody:  contextResponse(cfg.Workspace, cfg.SchemaVersion, Capabilities()),
			idPrefix: prefix,
			idSeq:    atomic.Uint64{},
		},
		output: serverOutput{
			log:    log.New(cfg.Stderr, "bd serve: ", log.LstdFlags|log.LUTC),
			stdout: cfg.Stdout,
		},
	}
}

func bindServer(s *Server, cfg Config) error {
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("bind %s: %w", cfg.Addr, err)
	}
	s.network.listener = netutil.LimitListener(ln, s.network.maxConns)

	s.network.http = &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          log.New(cfg.Stderr, "bd serve: http: ", log.LstdFlags|log.LUTC),
		ConnState:         s.connState,
	}
	// Tell the streams to wind up as soon as a drain starts. Without it a
	// graceful shutdown waits out the whole drain timeout on any open stream and
	// then reports itself forced, which is the one shutdown signal an operator
	// is meant to be able to trust.
	s.network.http.RegisterOnShutdown(s.closeStreams)
	return nil
}

func configureServerPool(s *Server, provider uow.UnitOfWorkProvider) {
	// Bound what a burst of requests can open on the database. The knob is
	// optional on the interface, so say so out loud when a provider does not
	// carry it rather than silently running unbounded.
	//
	// Nothing to bound on the roles source: the pool belongs to whatever the
	// backend is, and this server neither owns it nor can reach it. Saying the
	// knob is "unavailable" there would report a missing capability for a
	// provider that was never asked for.
	if provider == nil {
		return
	}
	if tuner, ok := provider.(uow.PoolTuner); ok {
		tuner.SetPoolLimits(servePoolLimits)
		return
	}
	s.event("pool_limits_unavailable", "provider", fmt.Sprintf("%T", provider))
}

func reportServerReady(s *Server) {
	fmt.Fprintf(s.output.stdout, "bd serve: listening on http://%s\n", s.Addr())
	s.logStartup()
}

// checkDatabaseSource enforces exactly one complete database source.
//
// There are two, and a Config carries one or the other: a unit-of-work
// provider, or the roles this surface answers from (sourceRoles). A PARTIAL
// set is refused with the same message as none at all, because it is the same
// mistake and the failure it would otherwise produce is the worst shape
// available — a Config missing one role binds, answers every other route, and
// fails that one with a nil dereference in a handler on a live server.
//
// The set GROWS as this surface grows: every operation added here is an
// operation a roles-backed deployment must be able to answer, so a role added
// to the set turns "this build serves an operation your Config cannot answer"
// into a startup error instead of a 500 on the first client that finds it.
//
// Both together is refused rather than resolved by precedence: a caller that
// set both holds two different opinions about where this server reads from, and
// silently honoring one of them leaves the other as configuration that looks
// live and is not.
//
// The last refusal is the one a caller does not see coming. A store wears
// decorators and its accessors hand them out — that is what the accessors are
// FOR — so the obvious `store.IssueClaimer()` on bd's own storage chain returns
// a claimer that fires the workspace's on_update hook for every claim it lands.
// This server's contract says hooks do not fire (cmd/bd/serve.go), and a
// contract broken by the caller's most natural line is not a contract. Refusing
// at Listen is the difference between a startup error naming the store to take
// roles from and a server that has been quietly running a user's subprocess per
// claim since it booted.

// sourceRoles is the store-shaped source's roles in ONE place, so the three
// questions checkDatabaseSource asks — is any set, is any missing, does any
// fire hooks — cannot drift apart as the set grows. An operation that reaches a
// role this source does not yet carry adds a line here and a line to
// roleSourceNames, and nothing else in this file.
//
// A role is compared against nil as an INTERFACE, which is what the caller
// actually sets; a typed nil stored in one of these fields is a value as far as
// this check is concerned.
//
// It carries only the roles every deployment must have. EventsJournal is
// deliberately NOT here: it is required only when the workspace's journal is
// enabled, and folding a conditional field into a set whose whole value is
// "all or nothing" would turn an honest condition into a special case inside
// three functions. It is checked once, on its own, below.
func sourceRoles(cfg Config) []any {
	return []any{cfg.Reader, cfg.Claimer, cfg.ReadyClaimer, cfg.Releaser, cfg.Lifecycle, cfg.BatchCloser, cfg.Settings, cfg.Stats, cfg.CycleDetector, cfg.EdgeReader, cfg.GraphCounter, cfg.Relations, cfg.Commenter, cfg.BlockingAnnotator, cfg.TreeWalker, cfg.ReadyCounter, cfg.Counter, cfg.Querier, cfg.Sweeper, cfg.Deleter, cfg.BatchCreator, cfg.DependencyEditor, cfg.BatchApplier, cfg.Memories, cfg.MetadataCAS}
}

// roleSourceNames spells sourceRoles for the refusal message, in the same
// order, so a caller reading the error learns the whole set it must pass.
const roleSourceNames = "Reader, Claimer, ReadyClaimer, Releaser, Lifecycle, BatchCloser, Settings, Stats, CycleDetector, EdgeReader, GraphCounter, Relations, Commenter, BlockingAnnotator, TreeWalker, ReadyCounter, Counter, Querier, Sweeper, Deleter, BatchCreator, DependencyEditor, BatchApplier, Memories and MetadataCAS"

func anyRoleSet(cfg Config) bool {
	return slices.ContainsFunc(sourceRoles(cfg), func(r any) bool { return r != nil })
}

func everyRoleSet(cfg Config) bool {
	return !slices.Contains(sourceRoles(cfg), nil)
}

func anyRoleFiresHooks(cfg Config) bool {
	return slices.ContainsFunc(sourceRoles(cfg), storage.RoleFiresHooks)
}

func checkDatabaseSource(cfg Config) error {
	if err := checkDatabaseSourceChoice(cfg); err != nil {
		return err
	}
	if err := checkJournalSource(cfg); err != nil {
		return err
	}
	if err := checkRoleSourceHooks(cfg); err != nil {
		return err
	}
	return checkProviderSourceHooks(cfg)
}

func checkDatabaseSourceChoice(cfg Config) error {
	if cfg.Provider != nil && (anyRoleSet(cfg) || cfg.EventsJournal != nil) {
		return errors.New("httpapi: both a unit-of-work provider and issue roles were set; pass exactly one database source")
	}
	if cfg.Provider == nil && !everyRoleSet(cfg) {
		return errors.New("httpapi: no database source: set Provider, or " + roleSourceNames + " together")
	}
	return nil
}

func checkJournalSource(cfg Config) error {
	if cfg.Provider == nil && cfg.EventsJournalEnabled && cfg.EventsJournal == nil {
		return errors.New("httpapi: this workspace's events journal is enabled but no EventsJournal reader was configured; " +
			"take one off the store (storage.EventsJournalCursor), or serve a workspace with the journal off")
	}
	return nil
}

func checkRoleSourceHooks(cfg Config) error {
	if !anyRoleFiresHooks(cfg) {
		return nil
	}
	return errors.New("httpapi: a configured role fires this workspace's hooks; " +
		"this server does not run hooks, so take the roles from the store beneath the hook decorator " +
		"((*storage.HookFiringStore).Unwrap)")
}

func checkProviderSourceHooks(cfg Config) error {
	if !uow.ProviderFiresHooks(cfg.Provider) {
		return nil
	}
	// The same refusal for the other database source. A provider's roles
	// carry whatever the provider carries, so a hook-firing one would run a
	// user's subprocess per served mutation just as a hook-firing role does.
	return errors.New("httpapi: the configured provider fires this workspace's hooks; " +
		"this server does not run hooks, so pass the provider beneath the hook layer " +
		"(uow.UnwrapProvider)")
}

// Addr is the bound address, which is the only way to discover the port under
// the ephemeral default.
func (s *Server) Addr() string { return s.network.listener.Addr().String() }
