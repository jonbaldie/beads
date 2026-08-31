package httpapi

import (
	"io"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/issueops"
	"github.com/jonbaldie/beads/memoryops"
)

// APIVersion is the path major this package serves, reported as
// ContextResponse.api_version. It changes only when /v1 is cut.
const APIVersion = "v0"

// The operating envelope. Every one of these is a bound on how much of the
// process a client can occupy, and the two that matter most are the ones with
// no natural limit: without semAcquireTimeout a queue behind a wedged database
// grows without end, and without requestDeadline a request that got a slot
// never gives it back. Both are deliberately generous — this is a loopback
// service for automation clients, not a public endpoint — and all of them can
// become operator flags later without touching the wire.
const (
	// maxInflight bounds handlers that touch the database. Every unit of work
	// pins one SQL connection, so this is also the steady-state connection
	// count.
	maxInflight = 16
	// maxConns bounds ACCEPTED connections. The semaphore does not: Go spawns
	// a goroutine per connection, and one parked on a full semaphore still
	// holds its goroutine, fd and buffers. Excess connections wait in the
	// kernel accept backlog instead of in Go memory.
	maxConns = 64
	// semAcquireTimeout bounds the queue in TIME as well as width. A timed-out
	// acquisition is the already-documented 503 busy, so shedding load
	// introduces no new status vocabulary.
	semAcquireTimeout = 10 * time.Second
	// requestDeadline is the whole-request backstop, needed because
	// WriteTimeout is 0 (below). It covers semaphore wait + unit of work +
	// query, and deliberately not the response write.
	requestDeadline = 60 * time.Second
	// saturationWarn is how long a semaphore wait has to last before it is
	// worth a log line of its own. This is the wedge-detection signal: /healthz
	// stays green while the database is hung, so saturation events are what
	// distinguish "wedged" from "no traffic".
	saturationWarn = time.Second
	// drainTimeout covers a claim inside its serialization-retry budget plus
	// the commit, so a graceful shutdown does not kill a connection whose write
	// may already have landed.
	drainTimeout = 20 * time.Second
	// uowCloseTimeout bounds the DETACHED close described on WithUOW.
	uowCloseTimeout = 5 * time.Second
)

// Pool limits for the provider's *sql.DB. The semaphore bounds handlers, not
// connections: a poisoned connection replaced after a failed ROLLBACK, each
// retry attempt of a committing transaction (a fresh unit of work is a fresh
// pinned connection), and any semaphore-exempt handler that later touches the
// database all escape it.
var servePoolLimits = uow.PoolLimits{
	MaxOpenConns:    maxInflight + 4,
	MaxIdleConns:    maxInflight,
	ConnMaxIdleTime: 5 * time.Minute,
	ConnMaxLifetime: time.Hour,
}

// HTTP-level timeouts. WriteTimeout is deliberately absent: `limit=0` means
// unlimited on both list operations, and a whole-response deadline would
// truncate a large body mid-write.
//
// writeStallTimeout is what replaces it — a deadline rolled forward before
// every write (statusWriter.extendWriteDeadline), which bounds a STALLED write
// without bounding total transfer. That bound is load-bearing, not hygiene:
// route() releases the database slot when the handler returns, and the handler
// returns only after writing the body, so without it a client that opens
// maxInflight requests and then stops reading pins every slot and its pinned
// connection until the process is restarted — while /healthz stays green. A
// context deadline cannot substitute: nothing cancels a blocked socket write.
//
// The read, header and idle timeouts bound request READING and keep-alive idle.
// They say nothing about a response write, and must not be cited as if they did.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	idleTimeout       = 120 * time.Second
	writeStallTimeout = 30 * time.Second
	maxHeaderBytes    = 64 << 10
)

// SourceRoles are the capabilities a roles-backed server answers from. The
// groups keep each capability family together without making Config itself a
// flat bag of unrelated dependencies.
type SourceRoles struct {
	IssueRoles
	GraphRoles
	WorkspaceRoles
	JournalRoles
}

type IssueRoles struct {
	Reader            issueops.Reader
	Claimer           issueops.Claimer
	BatchCloser       issueops.BatchCloser
	ReadyClaimer      issueops.ReadyClaimer
	Releaser          issueops.Releaser
	Lifecycle         issueops.Lifecycle
	Relations         issueops.Relations
	Commenter         issueops.Commenter
	BlockingAnnotator issueops.BlockingAnnotator
	DependencyEditor  issueops.DependencyEditor
	BatchApplier      issueops.BatchApplier
}

type GraphRoles struct {
	CycleDetector issueops.CycleDetector
	EdgeReader    issueops.EdgeReader
	GraphCounter  issueops.GraphCounter
	TreeWalker    issueops.TreeWalker
	ReadyCounter  issueops.ReadyCounter
	Counter       issueops.Counter
	Querier       issueops.Querier
}

type WorkspaceRoles struct {
	Settings     issueops.WorkspaceConfig
	Stats        issueops.StatsReporter
	Sweeper      issueops.Sweeper
	Deleter      issueops.Deleter
	BatchCreator issueops.BatchCreator
	MetadataCAS  issueops.MetadataCAS
	Memories     memoryops.Memories
}

type JournalRoles struct {
	EventsJournal        storage.EventsJournalCursor
	EventsJournalEnabled bool
}

// Config is everything the server needs to answer. It is assembled by the
// caller — the package resolves no workspace state of its own.
type Config struct {
	// Addr is the host:port to bind. The host must be a numeric IP literal;
	// see ValidateBindAddr.
	Addr string
	// AllowNonLoopback permits a bind beyond loopback. There is no TLS, so
	// this is an operator decision that is never taken by default, and it
	// requires either Auth or InsecureNoAuth — see ValidateAuthPosture.
	AllowNonLoopback bool
	// Auth verifies bearer credentials. NIL MEANS NO AUTHENTICATION, which is
	// the pre-existing behavior and stays the default on loopback: a zero
	// Config serves exactly what it served before this field existed.
	Auth *TokenFileAuth
	// InsecureNoAuth is the operator's explicit waiver for serving a
	// non-loopback bind with no credential. It only ever permits; it never
	// disables a configured Auth (that combination is refused).
	InsecureNoAuth bool
	// AllowedHosts are extra Host header values to answer to, beyond the
	// loopback spellings and the bind address. In a cluster the client dials a
	// service DNS name, which the rebinding defense would otherwise refuse;
	// see newHostPolicy. Empty leaves today's policy exactly as it was.
	AllowedHosts []string
	// Provider is where every database-touching handler opens its one unit of
	// work per request.
	Provider uow.UnitOfWorkProvider
	// The fields below are the roles this server answers from, for a backend
	// whose facade is a STORE rather than a unit-of-work provider. sourceRoles
	// is the authoritative list.
	//
	// Set them ALL, and only when Provider is nil: together they are the other
	// complete database source, not an override of one. Listen refuses every
	// other combination, including a partial set — a server missing one role
	// would bind, answer every other route, and fail that one with a nil
	// dereference inside a handler on a live server, which is worse than not
	// starting.
	//
	// A caller with a store takes them off the store's own accessors, and WHICH
	// store value it takes them off is the whole question. Every decorator a
	// store wears is on the value its accessor returns — that is what the
	// accessors are for — and bd's chain is
	// HookFiringStore -> InstrumentedStorage -> raw, so `store.IssueClaimer()`
	// there returns a claimer that runs the workspace's on_update hook script
	// for every claim it lands. That is precisely what this server documents it
	// does not do (cmd/bd/serve.go). Take the roles from BENEATH the hook layer:
	//
	//	src := store
	//	if hooked, ok := src.(*storage.HookFiringStore); ok {
	//		src = hooked.Unwrap() // keeps the telemetry layer, drops the hooks
	//	}
	//	rd, err := src.IssueReader()
	//	cl, err := src.IssueClaimer()
	//	... one per field below, all off the same src ...
	//	httpapi.Listen(httpapi.Config{Reader: rd, Claimer: cl, /* and the rest */})
	//
	// cmd/bd's serveIssueRoles is that loop, written out.
	//
	// Listen refuses a hook-firing role rather than trusting the paragraph
	// above — see checkDatabaseSource.
	//
	// WHAT LISTEN CANNOT CHECK, and the caller therefore owns: each call must
	// commit ON ITS OWN, atomically and durably. That is what this server's
	// contract states per request, and nothing here can observe the commit
	// protocol of the backend behind an interface — every check available would
	// be a self-declaration by the same caller-supplied code being checked.
	// Embedded Dolt is the backend that does not qualify (its commit runs
	// outside the SQL transaction on a separate connection) and it is refused
	// where the workspace is actually known: serveDatabaseSource in
	// cmd/bd/serve.go.
	//
	// Unlike the provider path these are built ONCE, before Listen, rather than
	// per request. The provider path rebuilds its roles per request for exactly
	// one reason — so the units of work they open land in that request's uow_ms
	// (see Server.reader) — and a role reached this way opens none through this
	// server, so a rebuild would buy nothing.
	SourceRoles
	// BatchCloser closes many issues as one transaction, behind
	// POST /v0/beads/issues:batchClose. It is its own field rather than a mode
	// of Lifecycle for the role's reason: the request is the transaction
	// boundary, and a loop over Lifecycle.Close is N transactions.
	// ReadyClaimer is the atomic take of ready work, behind
	// POST /v0/beads/issues:claimNext. It is its own field rather than a second
	// verb on Claimer for the reason the role is its own interface: the caller
	// names a QUESTION and the implementation picks the answer, so selection is
	// part of the operation and not a patch.
	// Releaser is the claim's inverse, behind
	// POST /v0/beads/issues/{id}:release. It is its own field rather than a
	// method on Claimer for the reason the role is its own interface: a caller
	// entitled to give its own work back is very often not entitled to take
	// new work, so a surface carrying both hands out a capability it should not
	// be able to reach.
	// Lifecycle is the guarded-mutation role behind the issue lifecycle
	// operations. Required on the same terms as every field here, and the
	// hook-firing refusal below bites hardest on it: a store's own
	// IssueLifecycle() returns a role that fires on_create, on_update and the
	// close hooks for every mutation it lands.

	// GraphCounter is the edge-count role behind
	// GET /v0/beads/dependencies:count. It is a SEPARATE field from EdgeReader
	// for the reason Counter is separate from ReadyCounter: that role answers
	// with the edge ROWS in one direction, this one with a number in either,
	// and neither can answer the other's question. Required on the same terms
	// as every field here.
	// Relations is the single-anchor neighbor read behind
	// GET /v0/beads/issues/{id}/related. It is a SEPARATE field from EdgeReader
	// for the reason issueops.EdgeReader's own doc gives at length: that role
	// answers with the edge ROWS for many anchors and reports a miss per anchor,
	// this one answers with the hydrated ISSUES on the far end for ONE anchor and
	// answers ErrNotFound. Different answer shape, different miss policy,
	// different arity. Required on the same terms as every field here.
	// Commenter is the append-one-comment role behind
	// POST /v0/beads/issues/{id}/comments. It is its own field rather than a
	// verb on Lifecycle for the role's own reason: a comment is not a patch to
	// an issue — it appends a row to a thread the issue owns and leaves every
	// field of the issue untouched, so an IssuePatch has nothing to carry and an
	// UpdateResult has nowhere to put the comment. Required on the same terms as
	// every field here.
	// Counter is the issue-count role behind GET /v0/beads/issues:count. It is
	// a SEPARATE field from ReadyCounter because it is a separate role: that
	// one sizes the ready predicate, this one sizes a filter, and neither can
	// answer the other's question. Required on the same terms as every field
	// here.
	// Sweeper is the DESTRUCTIVE one, required on the same terms as every other
	// role rather than opt-in: whether this build erases beads is a decision
	// for the operator who chose to run bd serve, not a consequence of whether
	// a caller remembered a field.
	// Deleter is the OTHER destructive one, required for the same reason.
	// DependencyEditor is the graph's write side. Required on the same terms as
	// the two destructive roles above: whether this build can rewire the
	// dependency graph is a decision for the operator who chose to run bd serve,
	// not a consequence of whether a caller remembered a field.
	// MetadataCAS is the conditional single-key metadata write behind
	// POST /v0/beads/issues/{id}:casMetadata. Required on the same terms as
	// every other role: a Config missing it would bind and nil-dereference on
	// the first request that reached that handler.
	// BatchApplier is the ordered, heterogeneous plan behind
	// POST /v0/beads/issues:batchApply. Required on the same terms as every
	// other role: a Config missing it would bind and nil-dereference on the
	// first request that reached that handler.
	//
	// It is the field where the hook-firing refusal below bites HARDEST. A
	// store's own accessor returns an applier that fires on_create, on_update
	// AND the close hooks — once per landed item, plus once per distinct edge
	// source — so a single hundred-item request served unpeeled would run a
	// hundred of the workspace's subprocesses inside one HTTP call.
	// Memories is the workspace's persistent memory plane, and the one field
	// here that is not an issueops role: memories are user data riding in the
	// config table under their own merge class, not settings, so they have
	// their own leaf package. Required on the same terms as every field above —
	// a partial set is refused, so the field and the operations that reach it
	// land together.
	// EventsJournal is the durable mutation journal's READ side, and the ONE
	// role here that is required CONDITIONALLY — which is why it is absent from
	// sourceRoles and checked on its own. Like Memories it is not an issueops
	// role: the journal is a replay feed over engine state on a dolt_ignored
	// table, not a bead query, so it has its own leaf package (journalops,
	// whose doc states why). storage.EventsJournalCursor is an ALIAS of
	// journalops.Journal, so this field names the role whichever spelling
	// reaches it.
	//
	// Required exactly when EventsJournalEnabled. A workspace that records
	// nothing needs no reader, and a storage backend that cannot read the
	// journal at all is a perfectly ordinary backend as long as nobody asked it
	// to journal — the same deal eventsjournal.Apply takes when it binds
	// activation to a store. Demanding it unconditionally would make a
	// capability nothing uses a precondition for running the server.
	//
	// It is the READ role and deliberately NOT the wider
	// storage.EventsJournalAccessor the CLI takes. That interface also prunes,
	// and a server that is documented to publish the journal and never retain
	// it should not be holding a delete it merely promises not to call.
	// EventsJournalEnabled is whether the served workspace actually records
	// mutations, resolved by the caller through eventsjournal.EnabledFor.
	//
	// It is a resolved BOOLEAN rather than something this package works out,
	// for the reason Config gives at the top: activation reads the target
	// workspace's own config.yaml and the BD_EVENTS_JOURNAL environment
	// override, which is workspace state, and this package resolves none.
	//
	// It cannot be inferred from the data either, which is the whole reason the
	// field exists. A disabled journal presents as zero rows and a head of
	// zero — byte-identical to an enabled journal nothing has written yet — so
	// a server without this flag would answer "you are caught up" to a consumer
	// polling a workspace that will never emit a record.
	// Workspace is the startup snapshot GET /v0/beads/context answers from.
	// Only the allowlisted fields are ever serialized — see contextResponse,
	// which names the whole set and the reasons for the exclusions.
	Workspace domain.ContextInfo
	// SchemaVersion is the CLI's stdout JSON envelope version, reported for
	// diagnostics. Clients are told not to branch on it.
	SchemaVersion int
	// Mode names the resolved storage topology ("proxied", "external") for the
	// startup log line. Cosmetic: nothing dispatches on it.
	Mode string
	// Stdout receives exactly one line, the bound address, so a caller that
	// asked for an ephemeral port can discover it. Stderr receives the
	// operational log. Both default to the process streams.
	Stdout io.Writer
	Stderr io.Writer
}
