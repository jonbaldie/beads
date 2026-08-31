package httpapi

import (
	"time"
)

// The events journal, PUSHED. GET /v0/beads/events:watch is the poll read's
// sibling, not a mode of it: same cursor, same records, same refusals, held
// open as text/event-stream so a consumer learns about a mutation when it
// happens instead of on its next interval.
//
// THE CURSOR IS STILL THE CONTRACT, and that is the whole design. This server
// keeps no per-consumer state, remembers no subscription and cannot redeliver;
// a stream is a sequence of `since` reads this process performs on the client's
// behalf, and every record carries `id: <seq>` so the client's own checkpoint
// is the same number it would have held while polling. That is what makes
// reconnection free: `Last-Event-ID` — the header a browser's EventSource
// resends by itself — is the same value the `since` parameter takes, so a
// dropped stream resumes exactly where a poller would have.
//
// NOTHING NEW IS READ. The loop below calls the SAME ReadEventsJournalPage the
// poll handler calls, through the same read-only cursor, with the same
// truncation contract arriving as the same typed error. There is no follow mode
// in storage, no notification channel and no new engine surface — a push
// endpoint that invented its own read path would be a second retention contract
// with the same failure mode this feature exists to prevent.
//
// WHEN TO USE WHICH is a real decision and the document states it: a consumer
// that can afford its interval should poll, because a poll holds nothing
// between requests. A stream costs a connection and a goroutine for as long as
// it stays open, plus a database slot for the moment of each read — which is
// why there is a hard cap on how many this process will hold at once, and why
// the refusal past that cap points back at the paged read.

const (
	// eventsWatchBatch is how many records one pass of the loop may carry. It
	// is the poll endpoint's DEFAULT rather than its ceiling, and it is not a
	// parameter: a stream paces itself, so the only thing this number changes
	// is how a backlog is chunked on the way out.
	eventsWatchBatch = defaultEventsLimit

	// eventsWatchRetry is the reconnection delay handed to the client in the
	// stream's first line, in milliseconds. EventSource's own default is
	// implementation-defined and browsers have shipped values from 500ms up, so
	// it is stated rather than inherited: three seconds is long enough that a
	// server restart does not produce a reconnect storm, short enough that a
	// consumer's lag after one is measured in seconds.
	eventsWatchRetry = 3000

	// eventsWatchTruncatedRetry is the delay raised in front of the `truncated`
	// event, in milliseconds. A consumer that respects the event stops and
	// re-baselines and never uses this; it is for the one that does not — a bare
	// EventSource will reconnect with the same dead Last-Event-ID and earn a
	// connect-time 410 forever, and a minute between attempts is the difference
	// between a slow loop and a hot one.
	eventsWatchTruncatedRetry = 60000

	// maxWatchStreams bounds concurrent streams, and it sits BELOW the
	// accepted-connection cap on purpose. Every stream holds a connection and a
	// goroutine for its whole life, so without a bound the stream surface would
	// be the one operation on this server with no limit at all — and it is the
	// one whose requests last hours.
	//
	// SIXTEEN CONNECTIONS OF HEADROOM IS WHAT MAKES THE REFUSAL REAL, and this
	// is the arithmetic. netutil.LimitListener returns a connection slot only
	// when the connection CLOSES — after the handler has returned, which is
	// after this counter has already come back down. A cap equal to maxConns
	// would therefore be unreachable: at sixty-four streams every connection
	// slot is held by one of them, the sixty-fifth connect is never accepted,
	// and the 503 this operation documents could not be delivered to anybody.
	// The paged read, every mutation and a fresh-connection /healthz would park
	// silently in the kernel accept backlog instead, which is precisely the
	// failure the cap exists to convert into a status and a log line.
	//
	// So the stream surface saturates FIRST, sixteen connections early — one
	// full complement of the in-flight database requests this server admits —
	// leaving room for the polls, writes and probes that must keep answering.
	// Connection saturation at maxConns is still reachable by other means and is
	// still the silent cliff; the runbook documents it as the worse one.
	// TestTheStreamCapLeavesConnectionHeadroom pins the relationship.
	maxWatchStreams = 48
)

// The stream's two cadences. Both are Server fields at the point of use
// (orDefault), for the reason semTimeout and writeStall are: a test that had to
// wait real seconds for a heartbeat would either be slow or would not exist.
const (
	// watchPollInterval is how often the loop asks for new records. One second
	// is what `bd events tail --follow` uses, and the stream is deliberately no
	// fresher than the CLI's own follow: a tighter loop would multiply database
	// reads across every open stream to shave a delay nothing here promises.
	watchPollInterval = time.Second

	// watchHeartbeat is how long a stream may go silent before it emits a
	// comment line. It is proxy and NAT defense first — an idle connection
	// through an intermediary that times out silently is the classic way a
	// stream stops delivering without either end noticing — and liveness second:
	// a write to a client that has gone away fails, which is how this loop
	// learns about a disconnect no TCP FIN announced.
	watchHeartbeat = 20 * time.Second

	// eventsWatchReadDeadline bounds ONE pass of the loop's read, and it is
	// deliberately much shorter than the requestDeadline this operation is
	// exempt from.
	//
	// It is not really a database budget: a bounded, indexed page of a thousand
	// journal rows needs nothing like fifteen seconds, and a read that takes
	// them is a wedge rather than a slow query. It is a LIVENESS budget, and it
	// bounds two things a stream cannot otherwise bound. A read parked for sixty
	// seconds suspends this stream's heartbeats for sixty seconds, which reads
	// to every intermediary between here and the consumer as a dead connection.
	// And it delays this loop's next look at the shutdown signal by the same
	// amount, which is three times the drain budget — turning a clean stop into
	// a forced one.
	eventsWatchReadDeadline = 15 * time.Second
)

// lastEventIDHeader is the standard SSE resume header. It is spelled once
// because it is both read from the request and named in a refusal.
const lastEventIDHeader = "Last-Event-ID"
