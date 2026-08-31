package httpapi

import (
	"errors"
	"net/http"

	"github.com/jonbaldie/beads/internal/storage"
)

// Every non-2xx byte this server emits is an RFC 9457 problem+json document
// (apigen.Problem, generated from the spec — there is one error shape here).
// This file owns the whole mapping: sentinel error in, status + machine code
// out, matched exclusively with errors.Is/errors.As. Never `err != nil ->
// status`.
//
// SENTINEL NOTE: the mapping keys on the sentinel VALUES, never on where they
// are declared. A parallel refactor relocates the canonical Err* declarations
// into a leaf issueops package and aliases them back from internal/storage
// with the same pointers, so errors.Is keeps matching either spelling with no
// edit here. Do not re-express these rows as package-path or message checks.

// Code is the machine-readable member of the problem envelope, and the only
// member a client may dispatch on.
//
// The vocabulary is a one-way door: renaming or removing a documented
// status+code pair breaks the wire. Adding one does not, which is why clients
// are told to default-branch on unknown codes within a status class. Keep it
// at what the v0 operations actually need.
type Code string

// The v0 code vocabulary. Every value here is documented in the spec, and
// TestSpecStatusCodesMatchHandlerTable fails if the two ever disagree.
const (
	// CodeInvalidArgument covers every request-validation refusal: an unknown
	// query parameter, a malformed value, an invalid actor, an unparseable or
	// oversized body, limit=0 under --allow-non-loopback, and the Host-header
	// middleware. The 400 carries `param` and `reason` so clients never have
	// to tell those cases apart by reading prose.
	CodeInvalidArgument Code = "invalid_argument"
	// CodeInvalidCursor is a separate code because a stale or foreign cursor
	// is a normal client situation with an obvious recovery (restart paging),
	// not a client bug.
	CodeInvalidCursor Code = "invalid_cursor"
	CodeNotFound      Code = "not_found"
	// CodeAlreadyClaimed reports a live foreign holder. Where the `assignee`
	// extension member is present it is the holder, read inside the same
	// transaction and never parsed out of the sentinel's message text — but
	// PRESENCE IS PER PRODUCER, and there are four:
	//
	//   - claimIssue always attaches it: the claim's conflict path reads the
	//     row it lost to, so it has the holder in hand.
	//   - updateIssue and applyBatch attach it CONDITIONALLY, when the refusing
	//     error carried one. The assignee fence
	//     (AuthorizeAssigneeTransferWithPools) refuses without naming the
	//     holder, so an implementation that reported none leaves it absent.
	//   - releaseIssue NEVER attaches it. It is the same fence pointed the
	//     other way and it names nobody.
	//
	// A client therefore treats the member as optional on every operation but
	// the claim, and re-reads the row when it is absent. Absence means "this
	// refusal could not name the holder", never "nobody holds it".
	CodeAlreadyClaimed Code = "already_claimed"
	CodeNotClaimable   Code = "not_claimable"
	// CodeNotReleasable is a row refusing to give up a claim, and it covers
	// the two conditions releaseIssue's role reports for that: the row holds
	// no claim, or its status is neither open nor in_progress.
	//
	// A 409 for CodeNotClosable's reason: the body is well-formed and STATE
	// refuses it, so the same request succeeds or fails on something the client
	// cannot see without reading it.
	//
	// IT IS MINTED RATHER THAN FOLDED INTO CodeNotClaimable, which is the
	// nearest existing code and covers a superset of the same statuses today.
	// The coincidence is not a contract: the release transition is pinned to
	// {open, in_progress} by releasableStatus and the claim's eligibility is a
	// separate predicate, so the two sets are free to diverge and would then be
	// one code meaning two things. Worse, the unheld half would be an outright
	// lie — an open, unassigned row is the MOST claimable row a workspace has,
	// and answering `not_claimable` about it would send a client somewhere
	// there is nothing to find.
	//
	// IT COVERS BOTH CONDITIONS UNDER ONE CODE, and that is a deliberate
	// start-narrow CHOICE rather than a consequence of missing information.
	//
	// THE TWO CONDITIONS ARE FULLY DISTINGUISHABLE HERE. ErrNotClaimed and
	// ErrNotReleasable are two distinct typed sentinels, and failRelease holds
	// both in one case arm — so a future split needs no archeology and no
	// prose-scraping: it is a mapping change in that arm plus a code in this
	// block and a line in the document. What IS unavailable typed is the
	// OBSERVATION either refusal made — the status it saw, the emptiness of the
	// assignee — because both format those into their messages and this surface
	// does not scrape its own prose. That is why the code carries no extension
	// member, and it is a narrower statement than "the refusals are
	// indistinguishable", which they are not.
	//
	// The split is deferred rather than refused because one code is the
	// reversible direction: splitting later is an ADDITION, which the document
	// already tells clients to tolerate, while merging two published codes into
	// one is the removal that breaks the wire. A client that needs the
	// distinction before then reads the row, which the operation description
	// tells it to do for a safety reason rather than a taste one.
	CodeNotReleasable Code = "not_releasable"
	// CodeNotClosable is close policy refusing an unforced close: open
	// children, or a live blocker. The open-children refusal carries the count
	// in the `open_children` extension member, read inside the refusing
	// transaction — never parsed out of the sentinel's message text — and its
	// PRESENCE is how a client tells the two refusals apart without prose.
	//
	// A 409 rather than the delete precedent's 400: this is a statement about
	// the current state of one named resource, so the same request succeeds or
	// fails on state the client cannot see without reading it. That is the
	// not_claimable situation and it gets the not_claimable answer.
	CodeNotClosable Code = "not_closable"
	// CodeDependencyCycle covers BOTH never-makes-progress refusals a requested
	// edge set can earn: a scheduling cycle, and a blocking edge against the
	// issue's own ancestor or descendant. They are one code because they have
	// one client recovery — rethink the edge, with no force bypass for either —
	// and codes are the vocabulary of recovery. The typed distinction is NOT
	// lost: the hierarchy refusal additionally carries `issue_id`, `blocker_id`
	// and `blocker_is_ancestor`, read inside the refusing transaction, and
	// member presence is the discriminator.
	CodeDependencyCycle Code = "dependency_cycle"
	// CodeDependencyExists is the pair that already carries an edge of a
	// DIFFERENT type, with both types in `existing_type`/`requested_type`.
	CodeDependencyExists Code = "dependency_exists"
	// CodeAlreadyExists is a create whose EXPLICIT id already names a stored
	// row. `param` names the offending member and, on a batch operation, the
	// item members name which item carried it.
	//
	// A 409 rather than a 400, and the distinction is the one CodeNotClosable
	// draws: the request is well-formed and stays well-formed: what refuses it
	// is STATE the client cannot see without reading it, and recovery is to
	// look at that state — adopt the row, pick another id, or stop. A 400 says
	// "this body is malformed, fix it and it will work", which is false here:
	// the identical body succeeded before the id was taken and would succeed
	// again against a workspace that never took it.
	//
	// It is minted rather than folded into CodeInvalidArgument because the two
	// are not narrowable later: widening a 400 to a 409 changes the status an
	// existing client dispatches on, while a 409 that turns out to be
	// unreachable retires for free.
	CodeAlreadyExists Code = "already_exists"
	// CodePreconditionFailed is a compare-and-set guard that MISSED on an
	// operation whose contract is that a miss refuses everything. The expected
	// values travel in typed extension members; a batch operation adds the
	// members naming which item carried the guard.
	//
	// A 409 for CodeNotClosable's reason: the request is fine as a request and
	// the STATE refuses it, so the same body succeeds or fails on something the
	// client cannot see without reading it.
	//
	// IT IS NOT THE ANSWER TO EVERY LOST COMPARE-AND-SET on this surface, and
	// the split is the point. Where a miss is the ordinary path of a retry loop
	// — the metadata compare-and-set — it is a 200 carrying the current value,
	// because putting a loop's normal iteration in the error channel would make
	// the value that loop needs next travel as a problem member. This code is
	// for the opposite contract: a guard on one step of a plan the caller meant
	// to land whole, where a miss took the entire request down and there is
	// nothing to report but the refusal.
	//
	// The expected/actual members are SPLIT BY TYPE — expected_version and
	// actual_version, expected_status and actual_status, expected_assignee and
	// actual_assignee — rather than one polymorphic pair, because a generic
	// `expected`/`actual` of "a version or a status or an assignee" is a schema
	// alternation and this document's x-go-type doctrine admits no composition
	// keyword to spell one.
	CodePreconditionFailed Code = "precondition_failed"
	// CodeEventsJournalDisabled is the events journal being OFF on the served
	// workspace. It exists because the alternative answer is a lie: a disabled
	// journal reads as zero rows and a head of zero, which is byte-identical to
	// an enabled journal nothing has written to yet — so a consumer would poll a
	// workspace that will never produce a record and call it "caught up"
	// forever. This is the one refusal on this surface that a client cannot
	// discover any other way.
	//
	// A 409 for the reason CodeNotClosable gives: it is a statement about the
	// current state of the workspace, which the same request stops earning the
	// moment an operator sets `events-journal true` and restarts the server.
	// Not a 404 — the operation and the resource both exist — and not a 501,
	// which this surface reserves for an operation this BUILD does not
	// implement, whereas every build implements this one.
	CodeEventsJournalDisabled Code = "events_journal_disabled"
	// CodeEventsJournalTruncated is a journal read whose checkpoint has fallen
	// below the retained window. The value is storage's own constant, so the
	// code a `bd events tail --json` failure carries and the code this surface
	// emits cannot drift to two spellings of one condition.
	//
	// A 410 Gone, and the only one in the vocabulary: the records the caller
	// asked for existed, were addressable by exactly this request, and have been
	// deliberately deleted. That is what 410 means and what 404 does not — a 404
	// would say the resource never existed and invite a retry, and the whole
	// point of this refusal is that retrying the same `since` can never succeed.
	// The `since`, `floor` and `head` members carry the window the server can
	// still serve, so the recovery (resume from `floor - 1` and accept the gap,
	// or re-baseline) is a decision the client makes from data rather than prose.
	CodeEventsJournalTruncated Code = Code(storage.EventsJournalTruncatedCode)
	// CodeEventsWatchSaturated is this server already holding as many open
	// journal streams as it will. It is not CodeBusy, even though both are a
	// 503 with a Retry-After, because the two carry different recoveries: busy
	// says the database is congested and the same request will work shortly,
	// while this says a bounded resource is fully subscribed by connections that
	// may last hours — and the caller has a second recovery available that busy
	// does not offer, namely the paged read, which holds nothing between
	// requests and is never refused for this reason.
	CodeEventsWatchSaturated Code = "events_watch_saturated"
	// CodeUnauthenticated is a missing, malformed or unrecognized bearer
	// credential on a server that was configured with one. It is a DEPLOYMENT
	// posture, not a property of the operation: a server started with no token
	// file never emits it. The three client mistakes are deliberately one code
	// — telling them apart on the wire would tell an unauthenticated caller
	// which of its guesses was closer.
	CodeUnauthenticated Code = "unauthenticated"
	// CodeBusy is retryable contention: the transaction retry budget was
	// exhausted, or the in-flight request limit was saturated.
	CodeBusy Code = "busy"
	// CodeDBUnavailable is a retryable connectivity failure reaching the
	// database.
	CodeDBUnavailable Code = "db_unavailable"
	CodeInternal      Code = "internal"
)

// codeClientClosed is a LOG-ONLY outcome, not wire vocabulary: it is
// deliberately absent from codeStatus, from operationCodes and from the
// document, and it never reaches a response body — the client it describes has
// already gone. It exists so that the request line does not book a client
// hanging up as a server fault. See failErr.
const codeClientClosed Code = "client_closed"

// codeStatus freezes one HTTP status per code. A code that could arrive with
// two different statuses would defeat the point of dispatching on it.
var codeStatus = map[Code]int{
	CodeInvalidArgument:  http.StatusBadRequest,
	CodeInvalidCursor:    http.StatusBadRequest,
	CodeUnauthenticated:  http.StatusUnauthorized,
	CodeNotFound:         http.StatusNotFound,
	CodeAlreadyClaimed:   http.StatusConflict,
	CodeNotClaimable:     http.StatusConflict,
	CodeNotClosable:      http.StatusConflict,
	CodeNotReleasable:    http.StatusConflict,
	CodeDependencyCycle:  http.StatusConflict,
	CodeDependencyExists: http.StatusConflict,
	CodeAlreadyExists:    http.StatusConflict,

	CodePreconditionFailed: http.StatusConflict,

	CodeEventsJournalDisabled:  http.StatusConflict,
	CodeEventsJournalTruncated: http.StatusGone,
	CodeEventsWatchSaturated:   http.StatusServiceUnavailable,

	CodeBusy:          http.StatusServiceUnavailable,
	CodeDBUnavailable: http.StatusServiceUnavailable,
	CodeInternal:      http.StatusInternalServerError,
}

// Status returns the HTTP status frozen to c, or 0 if c is not in the v0
// vocabulary.
func (c Code) Status() int { return codeStatus[c] }

// Reason distinguishes the two client postures behind a 400
// CodeInvalidArgument, so that telling them apart never requires parsing
// `detail`. The set may grow; clients default-branch on unknown values.
type Reason string

const (
	// ReasonUnknownParameter means this server does not know that parameter:
	// version skew. The client degrades or falls back. It is also a client's
	// only per-parameter capability probe, since `capabilities` is
	// operation-level.
	ReasonUnknownParameter Reason = "unknown_parameter"
	// ReasonInvalidValue means the server will not act on that value:
	// malformed, outside the vocabulary, or legal-but-refused in this
	// server's configuration (limit=0 under --allow-non-loopback). The
	// recovery is always to send something different, never to retry; the
	// detail says which case it was.
	ReasonInvalidValue Reason = "invalid_value"
	// ReasonProjectMismatch means the request stamped a Bd-Project-Id that is
	// not the project this server serves. Like the Host-header refusal it is a
	// document-level 400 reachable on every enforced route rather than
	// per-operation behavior, and it is the one refusal that carries
	// `server_project_id`. The recovery is to stop stamping this server with
	// another workspace's id, never to retry the same request.
	ReasonProjectMismatch Reason = "project_mismatch"
)

// staticDetail is the set of codes whose `detail` is FIXED, whatever the
// caller or the call site passed. newResult overrides the supplied detail for
// every code listed here, which is what makes the guarantee structural rather
// than a rule each call site has to remember.
//
// Every 5xx is on it. The underlying error goes to the server log and nowhere
// else: driver and dial errors routinely embed the DSN — go-sql-driver renders
// connection targets as user@tcp(127.0.0.1:PORT)/db, net dial errors carry the
// same host:port — and query errors can carry SQL fragments. The moment the
// server is bound with --allow-non-loopback, a verbose 5xx detail is an
// information-disclosure channel to network peers.
//
// The 401 is on it for the mirror-image reason, and it is the one row that is
// not a 5xx: the caller's own input here is a CREDENTIAL. Ordinary 4xx details
// stay specific precisely because they reflect the caller's input back, which
// is exactly what must never happen to a presented token — it would land in
// every client log and proxy trace between here and the caller.
var staticDetail = map[Code]string{
	CodeUnauthenticated: "missing or invalid bearer token",
	CodeBusy:            "the server is busy; retry shortly",
	CodeDBUnavailable:   "database temporarily unavailable; retry",
	CodeInternal:        "internal server error",
}

// Retry-After values, in seconds.
const (
	// retryAfterContention follows an exhausted transaction retry budget. The
	// budget spans many seconds of observed write contention, so a one-second
	// comeback would invite a convoy of retries that each hold a slot while
	// they wait — starving reads exactly when the server is busiest.
	retryAfterContention = 5
	// retryAfterSaturation follows an in-flight-limit wait timeout. Slot
	// pressure clears quickly.
	retryAfterSaturation = 1
	// retryAfterWatchSaturation follows a refused journal stream. Streams are
	// held for as long as their consumers stay connected — minutes to hours —
	// so a slot opens on a human timescale rather than a request one, and the
	// one-second comeback the slot limiter offers would be a busy loop against a
	// condition that has not changed.
	retryAfterWatchSaturation = 30
)

// ErrBusy reports that the in-flight request limiter refused to admit the
// request within its bounded wait. The limiter itself lands with the server;
// the sentinel lives here so the mapping owns the whole 503 vocabulary.
var ErrBusy = errors.New("server busy")

// Operation ids, matching the spec's operationId values exactly.
const (
	OpHealth        = "health"
	OpGetContext    = "getContext"
	OpListReadyWork = "listReadyWork"
	OpGetStats      = "getStats"
	OpListIssues    = "listIssues"
	OpGetIssue      = "getIssue"
	OpClaimIssue    = "claimIssue"
	// OpBatchCloseIssues closes many issues as one transaction, behind
	// issueops.BatchCloser. It is the surface's ONLY operation whose 200 body
	// carries refusals: the role is deliberately not all-or-nothing, so an id
	// it turns down is skipped and the survivors commit.
	//
	// Its problem vocabulary is therefore narrow rather than wide — everything
	// an ITEM can earn lives in that item's outcome, and a problem document
	// from this operation means the batch NEVER RAN.
	OpBatchCloseIssues = "batchCloseIssues"
	// OpClaimNextIssue takes ONE ready issue and hands it back claimed, behind
	// issueops.ReadyClaimer. It is the surface's first operation that names no
	// row at all: the caller sends a QUESTION — the ready listing's own filter
	// vocabulary — and the role picks the answer.
	//
	// It exists to retire a RACE rather than a round trip. The listing-then-claim
	// composition it replaces reads a row another agent claims before the second
	// request arrives, so a fleet polling one queue earns 409s for rows it was
	// correctly offered.
	OpClaimNextIssue = "claimNextIssue"
	// OpReleaseIssue gives a claim back — the claim's inverse, and what
	// `bd unclaim` spells. It is a named lifecycle action rather than a status
	// patch for OpCloseIssue's reason: an update spells the release three
	// fields at a time, which puts the transition's definition in the caller,
	// and the lease it drops is the part a patch cannot express at all.
	//
	// It is the one write on this surface that is NOT idempotent, and the
	// asymmetry with the claim is a fact about the two post-states rather than
	// a preference: a claim's post-state names the claimant, so a re-claim is
	// recognizable and can answer 200. A release leaves an anonymous row, so
	// "I released this twice", "a reaper beat me to it" and "nothing ever
	// claimed it" are one row — and one 200 for three situations that want
	// different things from a caller. It answers 409 instead and lets the
	// caller decide which of them it can live with.
	OpReleaseIssue = "releaseIssue"
	// OpCloseIssue is the second half of the agent loop this surface exists to
	// serve: claim, work, close. It is a named lifecycle action rather than a
	// status patch because Close carries semantics a patch has nowhere to put —
	// the reason and session under first-close-wins, the done-status
	// normalization, and the close policy vocabulary.
	OpCloseIssue = "closeIssue"
	// OpReopenIssue is the close's mirror, and it completes the lifecycle pair
	// so a recovery flow works end to end over this surface. It is the one write
	// here with no POLICY conflict code: reopen takes an issue OUT of the done
	// category, so there is no state of the graph that can refuse it. Its one
	// 409 is the caller's own compare-and-set, which is a fact about the
	// request's premise rather than about the graph.
	OpReopenIssue = "reopenIssue"
	// OpUpdateIssue edits the FIELDS of one issue, including the three that
	// carry policy: `status`, `assignee` and `parent_id`.
	//
	// It USED to exclude all three, and to have no 409 because of it. The
	// exclusion did not survive contact with a caller: an edit that moves a
	// status alongside other fields in one transaction is the thing two calls
	// cannot do, `issues:batchApply`'s update item has published all three since
	// it landed, and keeping them off here meant the two operations disagreed
	// about what patching one issue means. They are the SAME refusals here as
	// there — close policy, the assignee fence, the graph's two — and the named
	// lifecycle operations keep the semantics a status write has nowhere to put.
	OpUpdateIssue          = "updateIssue"
	OpListSettings         = "listSettings"
	OpGetSetting           = "getSetting"
	OpListDependencyCycles = "listDependencyCycles"
	// OpSetSetting stores one setting, replacing whatever was there. It is the
	// surface's first PUT, and the method IS the argument: the caller names the
	// resource by path and sends the value that becomes its whole state.
	// rememberMemory posts to a COLLECTION because its key may be derived from
	// the content; here the caller can always name what it is writing.
	OpSetSetting = "setSetting"
	// OpUnsetSetting removes one setting. It is the second DELETE on this
	// surface and the one that does NOT 404 on a key nothing stored: this role
	// reports no affected-row count, so the operation states an intended end
	// state rather than an act performed. See its operationCodes row.
	OpUnsetSetting = "unsetSetting"
	// OpListDependencies reads STORED EDGE ROWS for several issues at once.
	// It is a separate operation from getIssue's embedded `dependencies`
	// member because it answers per named issue, reports the ids that named
	// nothing, and returns edges whose target this database holds no row for.
	OpListDependencies = "listDependencies"
	// OpCountDependencyEdges sizes each anchor's edge set in ONE named
	// direction, behind issueops.GraphCounter. It is NOT listDependencies
	// counted: that operation is outgoing-only and takes no direction, this one
	// REQUIRES one and answers about either end, so the two agree on a number
	// only at direction=out. It is also not a third Counter method — that role
	// answers about a set of ISSUES described by a predicate, and this one about
	// EDGES anchored on ids, per anchor.
	OpCountDependencyEdges = "countDependencyEdges"
	// OpListRelatedIssues reads ONE issue's neighbors in a named direction,
	// behind issueops.Relations. It is NOT listDependencies narrowed to one
	// anchor: that operation answers the stored edge ROWS with their targets
	// spelled as stored, and this one answers the ISSUES on the far end — so an
	// edge whose target this database holds no row for is a row there and no
	// neighbor here, and the two answer different arities of question.
	//
	// It is a SUB-RESOURCE OF THE ISSUE rather than a member of the dependency
	// collection, and the argument is ELEMENT IDENTITY rather than a claim about
	// what that collection answers with — getDependencyTree answers hydrated
	// TreeNodes, so "everything under /dependencies is about edges" would be
	// false. What decides it is narrower and checkable: the rows here are the
	// SAME pinned struct getIssue already carries under `dependencies` and
	// `dependents`, so this operation is that pair, standalone,
	// direction-parameterized and type-filterable — and it belongs on the
	// resource whose members it publishes.
	OpListRelatedIssues = "listRelatedIssues"
	// OpListBlockingAnnotations reads the DERIVED blocking decoration for
	// several issues at once — open blockers, issues blocked, and the parent.
	// It is separate from listDependencies because it answers a summary over
	// two edge types with a status rule applied, where that one returns the
	// stored rows and applies nothing.
	OpListBlockingAnnotations = "listBlockingAnnotations"
	// OpGetDependencyTree walks the dependency graph from ONE root. It is
	// separate from listDependencies because that one answers raw edge rows for
	// many anchors at one hop, and this one recurses from a single anchor with a
	// depth, a cycle policy and a node shape of its own.
	OpGetDependencyTree = "getDependencyTree"
	OpCountReadyWork    = "countReadyWork"
	OpQueryIssues       = "queryIssues"
	// OpCountIssues sizes a set the ISSUE listing describes, behind
	// issueops.Counter. It is a sibling of countReadyWork rather than a mode of
	// it: that one sizes the READY predicate, which is dependency-aware and not
	// expressible as a filter over one table, and this one sizes a predicate.
	//
	// It is also NOT `listIssues` with the page taken off, which is the mistake
	// its own document spends a section on: a listing hides closed, pinned,
	// template and gate rows and a count hides none of them, so the two answer
	// about different sets for the same parameters. That difference is the
	// ROLE's and is the reason Counter is not a counted variant of Reader.
	//
	// One operation carries both of the role's methods. `group_by` selects the
	// bucketed shape, and the grouped response is the scalar response plus one
	// member — the same schema, not a second contract wearing one id.
	OpCountIssues = "countIssues"
	// OpRemoveDependency is the first WRITE to the dependency graph on this
	// surface, behind issueops.DependencyEditor. It names one edge by both its
	// endpoints, because an edge has two and neither alone identifies it.
	OpRemoveDependency = "removeDependency"
	// OpAddDependencies is the graph's other write: a BATCH of edges asserted
	// as one transaction, or none of them. It is the operation that owns both
	// new conflict codes, because both are statements about the graph a
	// requested edge set would produce.
	OpAddDependencies = "addDependencies"
	// OpSweepIssues is one of the two DESTRUCTIVE operations on this surface:
	// bulk clearance of closed beads from one tier, behind issueops.Sweeper.
	OpSweepIssues = "sweepIssues"
	// OpDeleteIssues is the other DESTRUCTIVE operation: erasure of beads the
	// request NAMES, behind issueops.Deleter. It is the one operation here whose
	// refusals include a question about the GRAPH — a named bead with a
	// dependent the request did not name.
	OpDeleteIssues = "deleteIssues"
	// OpCreateIssue creates ONE issue, with its parent, its explicit edges and
	// its waits-for gate, as one transaction. It is the plain collection POST
	// batchCreateIssues left free by spelling itself as a custom method.
	//
	// It publishes the whole create vocabulary rather than that operation's
	// narrow item — `status`, `sender`, `metadata`, `ephemeral`, `no_history`
	// and an explicit `id` included — which is what makes it usable for a
	// caller composing a real row, and which is also where its two conflict
	// codes come from: an occupied id, and the graph refusing the edges the
	// request asked for.
	OpCreateIssue = "createIssue"
	// OpAddComment appends one comment to the thread an issue owns, behind
	// issueops.Commenter. It is the surface's first write on a SUB-RESOURCE
	// COLLECTION, and a plain collection POST for OpCreateIssue's reason:
	// creating one member of the collection a path names is what POST means.
	//
	// The row it creates is the same pinned Comment getIssue already carries
	// under `comments`, which is what puts the operation on the issue rather
	// than on a collection of its own. The collection publishes no GET,
	// deliberately: no role answers a comment PAGE, and inventing one here
	// would be this surface deciding a paging contract the role declined.
	OpAddComment = "addComment"
	// OpBatchCreateIssues creates many issues as one transaction, or none.
	OpBatchCreateIssues = "batchCreateIssues"
	// OpApplyBatch applies an ORDERED, heterogeneous plan — creates, updates,
	// closes and dependency edges together — as one transaction, or none of it.
	// It is the only operation here whose request expresses a graph that
	// references its own items: a create may NAME itself and later items address
	// it by that name.
	//
	// It is therefore the widest refusal vocabulary on this surface, and every
	// entry is inherited rather than invented: it can earn the lifecycle's
	// close-policy conflict, the claim's assignee fence and both of the graph's
	// conflicts, because it performs all three families of write.
	OpApplyBatch = "applyBatch"
	// OpRememberMemory stores one memory, behind memoryops.Memories. It is the
	// first operation on this surface that reaches a role outside issueops: the
	// memory plane is user data riding in the config table, not settings, and
	// the two have different merge semantics and a different miss contract.
	OpRememberMemory = "rememberMemory"
	// OpGetMemory reads one memory by key. It is the ONE operation on this
	// surface that answers a miss with a 404 where its settings counterpart
	// deliberately does not: see its operationCodes row.
	OpGetMemory = "getMemory"
	// OpForgetMemory is the THIRD destructive operation on this surface, and the
	// only one that is a DELETE method: it names one memory by path, carries no
	// body and takes no flags, which is what that method already means. The two
	// destructive issue operations are collection-level custom methods because
	// they act on a set the request describes.
	OpForgetMemory = "forgetMemory"
	// OpListMemories enumerates the memory plane, narrowed by one search term.
	// It is the operation that makes stored memories DISCOVERABLE rather than
	// merely readable by a caller who already knows a key.
	OpListMemories = "listMemories"
	// OpListEvents pages the durable mutation journal from a caller-held
	// checkpoint. It is the first operation on this surface whose paging is not
	// a cursor this server minted: `since` is a sequence number the journal
	// itself assigned, so a consumer's position survives a restart on either
	// side and is meaningful to `bd events tail` as well.
	//
	// It is a READ of a log, not a subscription: the paged form has no follow
	// mode and never will. The retention contract is what makes that safe to
	// publish — a consumer that falls too far behind is told so with
	// `events_journal_truncated` rather than served a silently shortened history.
	OpListEvents = "listEvents"
	// OpWatchEvents pushes the same journal over a held-open text/event-stream
	// response, resuming from the same checkpoint. It is a sibling of the paged
	// read rather than a mode of it: the contracts differ in media type,
	// lifetime, limits and capacity, and one operation carrying both would have
	// documented two of everything under one operationId.
	//
	// It is the surface's ONLY streaming operation, and the only one whose
	// response can report a failure after its status is written — a prune that
	// races an open stream arrives as a named event rather than as the 410 the
	// same condition earns at connect.
	OpWatchEvents = "watchEvents"
	// OpCompareAndSetMetadata conditionally sets one metadata key on an issue.
	// It is the only WRITE on this surface whose ordinary refusal is a 200: a
	// lost race is the answer to the question the caller asked, and the value
	// that refused the swap is what its retry needs next.
	OpCompareAndSetMetadata = "compareAndSetMetadata"
)

// specBearerScheme is the name of the document's securityScheme for the bearer
// token. It is a constant so TestSpecSecurityMatchesRouteTable compares the
// route table's exemption column against the same string the document uses.
const specBearerScheme = "bearerToken"
