package httpapi

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"syscall"

	"github.com/go-sql-driver/mysql"

	"github.com/jonbaldie/beads/internal/httpapi/apigen"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/issueops"
)

// Result is a problem response ready to be written: the envelope plus the
// transport-level Retry-After the code implies.
type Result struct {
	Problem apigen.Problem
	// RetryAfterSeconds is written as the Retry-After header when positive.
	RetryAfterSeconds int
}

// WithAssignee attaches the `assignee` extension member (the current holder of
// a claimed issue). Populate it from a read in the claim's own transaction —
// never by parsing the sentinel's message fragments.
func (r Result) WithAssignee(assignee string) Result {
	r.Problem.Assignee = &assignee
	return r
}

// WithIssueStatus attaches the `issue_status` extension member (the issue's
// status at the moment of refusal). Same rule as WithAssignee.
func (r Result) WithIssueStatus(status string) Result {
	r.Problem.IssueStatus = &status
	return r
}

// WithOpenChildren attaches the `open_children` extension member (how many open
// children the transaction that refused a close observed). Same rule as
// WithAssignee: it comes from the typed error's own field, read inside that
// transaction, never from parsing the refusal's prose.
//
// Its PRESENCE is load-bearing. It is attached for the open-children refusal
// and withheld for the live-blocker one, which is how a client tells the two
// apart without reading `detail`.
func (r Result) WithOpenChildren(n int) Result {
	r.Problem.OpenChildren = &n
	return r
}

// WithDependencyTypeConflict attaches the two `dependency_exists` extension
// members: the type the pair already carries and the type the request asked
// for. Populate them from *issueops.DependencyTypeConflictError's fields — the
// typed error the role raises inside the transaction that saw the stored edge —
// never by parsing its message.
func (r Result) WithDependencyTypeConflict(existing, requested string) Result {
	r.Problem.ExistingType = &existing
	r.Problem.RequestedType = &requested
	return r
}

// WithHierarchyConflict attaches the three extension members that distinguish
// the HIERARCHY refusal from a plain scheduling cycle inside the one
// `dependency_cycle` code. Their PRESENCE is the discriminator, and the three
// together are enough to rebuild
// *issueops.DependencyHierarchyConflictError whole — BlockerIsAncestor in both
// polarities included, which is why the boolean travels through a pointer and
// is emitted when false.
//
// They can only come from the refusing transaction, for the reason
// ClaimConflictError's do and more so: the conflicting hierarchy may exist only
// inside the batch that was rolled back, so no read after the fact can recover
// it.
func (r Result) WithHierarchyConflict(issueID, blockerID string, blockerIsAncestor bool) Result {
	r.Problem.IssueId = &issueID
	r.Problem.BlockerId = &blockerID
	r.Problem.BlockerIsAncestor = &blockerIsAncestor
	return r
}

// WithBatchItem attaches the four extension members that name WHICH item of a
// batch earned a refusal: its index, its kind, the key it gave itself or the
// key its target ref named, and the id it had resolved when the refusal
// happened.
//
// They come from *issueops.ItemError's own fields — the typed error the role
// raises rather than the prose it formats — for the reason every other typed
// member on this envelope does, and one more that is this operation's alone:
// the request is all or nothing, so there is no per-item result array for a
// client to find the offender in. These members are the only place it exists.
//
// `item_key` and `item_issue_id` are OMITTED WHEN EMPTY, and both absences mean
// something. An item that named nothing symbolically has no key; an item
// refused before its target resolved — a create whose id was never minted, a
// ref that resolved to nothing — has no id.
//
// It is `item_issue_id` rather than `issue_id` deliberately: `issue_id` is
// already a PRESENCE-DISCRIMINATING member of the `dependency_cycle` hierarchy
// refusal, and reusing it would make that discriminator fire on a refusal it
// says nothing about.
func (r Result) WithBatchItem(index int, kind, key, issueID string) Result {
	r.Problem.ItemIndex = &index
	r.Problem.ItemKind = &kind
	if key != "" {
		r.Problem.ItemKey = &key
	}
	if issueID != "" {
		r.Problem.ItemIssueId = &issueID
	}
	return r
}

// WithDeclaredLater attaches the `declared_later` member, which tells an
// unresolvable key that IS declared by a later item from one nothing in the
// request declares at all. The first is an ordering mistake and the second is a
// typo, and a client fixes them differently.
//
// It is emitted in BOTH polarities and never omitted to mean false, for
// WithHierarchyConflict's reason applied to a 400: an absent member on this
// operation's 400s means "this refusal was not about a key" and must not be
// readable as "the key was not declared later".
func (r Result) WithDeclaredLater(declaredLater bool) Result {
	r.Problem.DeclaredLater = &declaredLater
	return r
}

// WithExpectedVersion attaches the `expected_version` member of a
// `precondition_failed`: the row version the request guarded on.
//
// It is the REQUEST's value rather than a read, which is why there is no
// `actual_version` beside it here — see PreconditionFailed.
func (r Result) WithExpectedVersion(expected int64) Result {
	r.Problem.ExpectedVersion = &expected
	return r
}

// WithExpectedStatus attaches the `expected_status` member of a
// `precondition_failed`. Same source and same rule as WithExpectedVersion.
func (r Result) WithExpectedStatus(expected string) Result {
	r.Problem.ExpectedStatus = &expected
	return r
}

// WithExpectedAssignee attaches the `expected_assignee` member of a
// `precondition_failed`. Same source and same rule as WithExpectedVersion.
func (r Result) WithExpectedAssignee(expected string) Result {
	r.Problem.ExpectedAssignee = &expected
	return r
}

// PreconditionFailed builds the 409 for a compare-and-set guard that missed on
// an operation where a miss refuses the whole request.
//
// The detail says what a client does next rather than what the row held,
// because on this contract those are different facts: the transaction that saw
// the mismatch rolled back, so a value read afterwards describes a row the
// refusal never saw. The role's refusals carry the expectation and not the
// observation, so `actual_version`, `actual_status` and `actual_assignee` stay
// absent here — the envelope declares them for an operation whose role can
// report what it found, and inventing one from a later read would be worse than
// omitting it.
func PreconditionFailed() Result {
	return newResult(CodePreconditionFailed,
		"a precondition guard did not match; nothing was written, so re-read the row and recompose the request rather than retrying it")
}

// WithJournalWindow attaches the three `events_journal_truncated` extension
// members: the checkpoint the reported window begins after, the lowest seq
// still retained, and the highest seq ever assigned.
//
// They come from *storage.EventsJournalTruncatedError's own fields — computed
// inside the transaction that saw the gap — for the reason every other typed
// member on this envelope does, and one more: `since` is NOT always the value
// the caller sent. On an interior hole it is the last seq the server could
// serve contiguously from the caller's checkpoint, which is strictly the more
// useful number and cannot be recovered by a client that assumed it was
// echoing its own input back.
//
// All three are emitted together and none is omitted to mean zero: a head of 0
// is a real journal state (nothing has ever been written), and a client
// computing `floor - 1` needs the value rather than an absence to interpret.
func (r Result) WithJournalWindow(since, floor, head int64) Result {
	r.Problem.Since = &since
	r.Problem.Floor = &floor
	r.Problem.Head = &head
	return r
}

// WithRequestID sets the `request_id` member, the correlation id echoed in the
// request log line. It is what makes a 5xx actionable: the body carries a fixed
// static detail by design, so the id is the client's only handle on the one log
// line that has the real error. The document requires it on every problem
// response, which is why this sets it unconditionally — an id this server
// failed to mint travels as an empty string rather than as a missing required
// member.
func (r Result) WithRequestID(id string) Result {
	r.Problem.RequestId = id
	return r
}

func newResult(code Code, detail string) Result {
	status := code.Status()
	if status == 0 {
		// Unreachable unless a code is added without a status; fail closed
		// rather than emitting a 0 status line.
		status = http.StatusInternalServerError
		code = CodeInternal
		detail = staticDetail[CodeInternal]
	}
	if static, ok := staticDetail[code]; ok {
		// 5xx detail is fixed per code, whatever the caller passed.
		detail = static
	}
	p := apigen.Problem{
		ProblemFields1: apigen.ProblemFields1{Code: string(code)},
		ProblemFields3: apigen.ProblemFields3{Status: status, Title: http.StatusText(status)},
	}
	if detail != "" {
		p.Detail = &detail
	}
	return Result{Problem: p}
}

// InvalidArgument builds the 400 for a request the server refuses to
// interpret. param names the offending query parameter, body member or header;
// pass "" only when the input has no nameable part (a body that fails to parse
// at all). detail may quote the caller's own input — it is not server state.
func InvalidArgument(param string, reason Reason, detail string) Result {
	res := newResult(CodeInvalidArgument, detail)
	if param != "" {
		res.Problem.Param = &param
	}
	r := string(reason)
	res.Problem.Reason = &r
	return res
}

// ProjectMismatch builds the 400 for a request whose Bd-Project-Id header names
// a workspace this server does not serve. got is the id the client stamped; own
// is this server's own project id, disclosed in the `server_project_id`
// extension member so a stamped client can tell a wrong-server refusal from a
// malformed one without parsing `detail`.
//
// This is the ONLY refusal on the surface that sets `server_project_id`, and it
// is raised only after the request has cleared the Host gate (and, in a
// deployment that adds one, its authentication layer): a request turned away by
// an earlier gate is answered before the stamp is ever compared, so it never
// discloses the server's identity. Presence of the member is therefore the
// signal that this specific check — and nothing earlier — fired.
func ProjectMismatch(got, own string) Result {
	res := InvalidArgument(ProjectIDHeader, ReasonProjectMismatch,
		"the "+ProjectIDHeader+" header names project "+strconv.Quote(got)+", which this server does not serve")
	res.Problem.ServerProjectId = &own
	return res
}

// InvalidCursor builds the 400 for a cursor this server did not issue, cannot
// decode, or issued under a different internal version.
func InvalidCursor() Result {
	return newResult(CodeInvalidCursor, "cursor is not valid for this server; restart paging without it")
}

// NotFound builds the 404 for an id this server cannot resolve. It is one
// function rather than a literal per site so that a handler which decides a
// miss WITHOUT reading storage — an id no row could hold — is indistinguishable
// on the wire from one that read and missed. A client that could tell them
// apart would be probing which ids are well-formed.
func NotFound() Result {
	return newResult(CodeNotFound, "no issue or wisp with that id")
}

// EventsJournalDisabled builds the 409 for a workspace whose journal is off.
//
// The detail names the setting rather than describing the state, because the
// recovery is entirely on the SERVER side — a client can do nothing about it —
// and the human reading this response is the operator who has to go turn it on.
func EventsJournalDisabled() Result {
	return newResult(CodeEventsJournalDisabled,
		"the durable events journal is not enabled on this workspace; set `events-journal true` and restart the server")
}

// EventsJournalTruncated builds the 410 for a checkpoint below the retained
// window, carrying the window the server can still serve.
//
// The detail is the storage error's OWN sentence, which is the one `bd events
// tail` prints. A consumer that reads both surfaces sees one description of one
// condition, and this is a 4xx, so reflecting the server's own state here is
// within what staticDetail allows.
func EventsJournalTruncated(err *storage.EventsJournalTruncatedError) Result {
	return newResult(CodeEventsJournalTruncated, err.Error()).
		WithJournalWindow(err.Since, err.Floor, err.Head)
}

// EventsWatchSaturated builds the 503 for a stream this server will not hold.
//
// The detail names the alternative rather than the limit, because unlike every
// other 503 here this one has a recovery that is not "wait": the paged read
// answers the same records from the same checkpoint and is not capped this way.
func EventsWatchSaturated() Result {
	res := newResult(CodeEventsWatchSaturated,
		"this server is already holding as many journal streams as it will; retry later, or read the same records from the same checkpoint with GET /v0/beads/events")
	res.RetryAfterSeconds = retryAfterWatchSaturation
	return res
}

// MemoryNotFound builds the 404 for a key this workspace holds no memory under.
//
// A separate constructor rather than a detail argument on NotFound, because the
// two say different things: that one is about the issue id space, and reusing
// its sentence here would tell a client its memory key was an issue id. The
// detail deliberately does NOT distinguish an absent row from one stored as the
// empty string — the role cannot see the difference, so the wire must not claim
// to.
func MemoryNotFound() Result {
	return newResult(CodeNotFound, "this workspace holds no memory under that key")
}

// ClassifyError maps an error from the storage seam onto the wire. The caller
// is responsible for logging err: everything mapped to a 5xx deliberately
// drops the error text on the floor (see staticDetail).
//
// ErrVersionMismatch — AND EVERY OTHER COMPARE-AND-SET SENTINEL — IS
// DELIBERATELY ABSENT FROM THIS FUNCTION, because the 409 it earns cannot be
// built from the error alone: `precondition_failed` echoes the value the
// REQUEST guarded on, and this function is handed an error and nothing else.
// So an operation that publishes `expected_version` (or `expected_status`, or
// `expected_assignee`) MUST match the sentinel TYPED in its own failure path,
// before anything reaches failErr — updateIssue, closeIssue, reopenIssue,
// deleteIssues, releaseIssue and applyBatch each do. A handler that forgets is
// not answering a worse 4xx: neither storage leg wraps these in ErrValidation,
// so the miss falls through the default arm below and every guard failure on
// that operation is a GENERIC 500. Mutation-verified on the single-issue
// handlers.
//
// THE RULE IS WRITTEN HERE BECAUSE SIX HANDLERS FOUND IT SEPARATELY. failUpdate
// worked it out, failRelease worked it out again and called it "failUpdate's
// hazard, in a sharper form", and the guard slice worked it out three more
// times. Six independent discoveries of one fact are a fact nobody recorded
// where the seventh author would look — which is here, since a handler author
// asking "does the shared mapping cover my sentinel?" reads this function and
// finds no row.
func ClassifyError(err error) Result {
	// The one row that carries data out of the error rather than only a code.
	// It lives HERE, in the shared mapping, rather than in the events handler:
	// the journal read is reached from two database sources through two
	// different plumbings, and a mapping a handler applied itself would be one
	// `if` away from a pruned-past checkpoint arriving as a generic 500 on
	// whichever arm forgot it.
	var truncated *storage.EventsJournalTruncatedError

	switch {
	case err == nil:
		return newResult(CodeInternal, "")

	case errors.As(err, &truncated):
		return EventsJournalTruncated(truncated)

	// Not-found normalization belongs in the shared read path, which folds the
	// two miss shapes (a wrapped sql.ErrNoRows, and a nil issue with a nil
	// error) into storage.ErrNotFound. The bare sql.ErrNoRows row below is
	// defense in depth for a path that has not been normalized yet: a missing
	// issue must never surface as a 500. The converse mistake — treating any
	// error as a miss — stays closed, because only these two sentinels reach
	// 404 and everything else falls through to 500.
	case errors.Is(err, storage.ErrNotFound), errors.Is(err, sql.ErrNoRows):
		return NotFound()

	case errors.Is(err, storage.ErrAlreadyClaimed):
		return newResult(CodeAlreadyClaimed, "issue is claimed by another actor")

	case errors.Is(err, storage.ErrNotClaimable):
		return newResult(CodeNotClaimable, "issue is not in a claimable state")

	// The two close-policy refusals share one code. They are the same statement
	// to a client — the close was refused for the state of the graph around
	// this issue, and `force` is the bypass for both — and what distinguishes
	// them on the wire is the `open_children` member failClose attaches, not a
	// second vocabulary entry.
	case errors.Is(err, issueops.ErrCloseOpenChildren):
		return newResult(CodeNotClosable, "issue has open children; close them first or close with force")

	case errors.Is(err, issueops.ErrCloseBlocked):
		return newResult(CodeNotClosable, "issue is blocked; clear the blocker or close with force")

	case errors.Is(err, ErrBusy):
		res := newResult(CodeBusy, "")
		res.RetryAfterSeconds = retryAfterSaturation
		return res
	}
	return classifyRetryableError(err)
}

func classifyRetryableError(err error) Result {
	switch {
	// The retry budget is spent inside uow.RunTxResult; reaching here means it
	// gave up, so the request is retryable at the client's cadence, not the
	// server's.
	case uow.IsSerializationError(err):
		res := newResult(CodeBusy, "")
		res.RetryAfterSeconds = retryAfterContention
		return res

	case isUnavailable(err):
		res := newResult(CodeDBUnavailable, "")
		res.RetryAfterSeconds = retryAfterContention
		return res

	default:
		return newResult(CodeInternal, "")
	}
}

// isUnavailable reports whether err is a failure to reach the database at all
// — the server or proxy being down, idle-stopped, or dropping connections —
// as opposed to a failure while executing a statement.
//
// The list is empirical and safe to extend: it only chooses between two 5xx
// codes that carry identical static detail, so a miss costs a less useful
// `code`, never a disclosure.
func isUnavailable(err error) bool {
	// context.DeadlineExceeded satisfies net.Error, so it must be excluded
	// before the net.Error test below: a tripped per-request deadline is a
	// generic 500, not a claim that the database is unreachable.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, mysql.ErrInvalidConn) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// Write emits res as application/problem+json. Success bodies are written by
// their handlers; every non-2xx byte goes through here.
func Write(w http.ResponseWriter, res Result) {
	h := w.Header()
	h.Set("Content-Type", "application/problem+json; charset=utf-8")
	if res.RetryAfterSeconds > 0 {
		h.Set("Retry-After", strconv.Itoa(res.RetryAfterSeconds))
	}
	// RFC 9110 requires the challenge on a 401, and it is set here rather than
	// at the refusal so a second 401 site could not forget it — the same reason
	// Retry-After is set from the code above.
	if res.Problem.Code == string(CodeUnauthenticated) {
		h.Set("WWW-Authenticate", "Bearer")
	}
	w.WriteHeader(res.Problem.Status)
	_ = json.NewEncoder(w).Encode(res.Problem)
}
