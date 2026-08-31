package httpapi

// operationCodes is the per-operation problem vocabulary: exactly the codes
// that operation's handler can produce, and therefore exactly what the spec
// documents for it. TestSpecStatusCodesMatchHandlerTable asserts set-equality
// in both directions, so an undocumented emission and an unemittable
// documented status both fail CI.
//
// Two 400 invalid_argument paths are reachable on every route including
// /healthz — the Host-header middleware, and the unknown-query-parameter
// refusal every decoder performs — and both are deliberately absent from every
// row here. They are uniform rules, not per-operation behavior, and the spec
// documents them once at the document level rather than repeating them on
// every operation; these rows carry what an operation produces beyond them.
// Keep the two documents in step: a row here and the document-level prose are
// the only two places that carve-out exists.
//
// The 401 is NOT one of those uniform rules and does appear in the rows below.
// It is per-operation surface, and the policy is every operation except
// liveness: auth is enforced in route() for every row whose authExempt column
// is false, and OpHealth is the only row that sets it, so a probe can answer
// with no credential. Stating it per-operation rather than once at the document
// level is what makes TestSpecStatusCodesMatchHandlerTable require the 401s to
// be documented in the same change as the check that emits them — including for
// an operation added later, which inherits enforcement the moment it is routed
// and so must carry CodeUnauthenticated here from its first commit.
var operationCodes = map[string][]Code{
	// Liveness answers from the process, touches nothing that can fail, and
	// carries no credential: it is the one auth-exempt row of the route table.
	OpHealth: nil,
	// v0 serves a startup snapshot without touching the database, so there is
	// no 503 here. If a later slice makes this a DB-probing readiness
	// endpoint, db_unavailable joins this row and the spec in the same change.
	OpGetContext:    {CodeUnauthenticated, CodeInternal},
	OpListReadyWork: {CodeInvalidArgument, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	OpListIssues:    {CodeInvalidArgument, CodeInvalidCursor, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// The 400 is this operation's own, the getStats precedent: a malformed
	// `include_comments` or `include_dependents` is a bad value on a parameter
	// this server knows, not the document-level unknown-key rule this table
	// omits.
	OpGetIssue: {CodeInvalidArgument, CodeUnauthenticated, CodeNotFound, CodeBusy, CodeDBUnavailable, CodeInternal},
	// No 400 of its own: the operation takes no parameters, so the only
	// invalid_argument it can raise is the document-level unknown-query-key
	// rule this table deliberately omits.
	OpListSettings: {CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// No 404: a key nothing stored and a key stored as the empty string are one
	// answer on this surface, so the only refusal a key can earn is the 400
	// that says it was not a key.
	OpGetSetting: {CodeInvalidArgument, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// getSetting's row PLUS the ROLE's refusals, which is the whole difference
	// between the read half and the write half. Two of the role's three are
	// reachable — `issue_prefix` in either spelling, and a `status.custom` that
	// does not parse — and both arrive as the 400 they are, on the sentinel,
	// through the shared ErrValidation line every role-backed handler here draws.
	//
	// NO 404 and no conflict code, both inherited from the read beside it. A key
	// nothing stored and a key stored empty are one answer on this plane, so
	// there is no resource this write can fail to address; and the write is an
	// unconditional replace, so there is no state for it to lose a race against.
	// A `revision` guard would need a row version this plane does not hold.
	OpSetSetting: {CodeInvalidArgument, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// getSetting's row EXACTLY, and that is this operation's whole error story:
	// it takes the same parameter, judges it the same way, and reaches a role
	// whose only refusal — an empty key — the path bound has already made
	// unreachable. Its 400 is therefore entirely the transport's.
	//
	// THE ABSENT 404 IS THE DIVERGENCE FROM forgetMemory, which addresses the
	// same shape of resource with the same method and answers 404 for a key it
	// held nothing under. That role reports Found; this one cannot — the storage
	// seam discards the affected-row count on all three legs — so a 404 here
	// would publish a distinction this server would have to invent.
	OpUnsetSetting: {CodeInvalidArgument, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// The 400 here is this operation's own, not the document-level
	// unknown-parameter rule: a malformed `skip_blocked`, and the EMPTY
	// `assignee` the document refuses rather than answering with the rows that
	// have no assignee.
	OpGetStats: {CodeInvalidArgument, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// The cycle sweep takes no parameters at all, so it has no 400 of its own:
	// the two uniform ones above are the whole of its invalid-argument story.
	OpListDependencyCycles: {CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// No not_found here, deliberately: an id that names nothing is reported in
	// the response's `missing` member, so a batch keeps the answers for the ids
	// that were found. A 404 would discard them.
	OpListDependencies: {CodeInvalidArgument, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// The stored-edge read's vocabulary exactly, and no not_found for its
	// reason: an id that names nothing is reported on its own anchor, so a
	// batch keeps the answers for the ids that were found. The role has no
	// ErrNotFound at all, which its doc states.
	//
	// Its 400 is BOTH the transport's and the ROLE's, which is what separates
	// this row from GET /v0/beads/issues:count beside it. That operation
	// refuses its one enum at the edge and reaches no role refusal; here
	// ValidateEdgeCountRequest runs inside the single shared body — the role
	// has one body on all three legs, so the check could not belong to an
	// accessor — and four of its refusals are reachable over the wire: a
	// missing or unrecognized direction, a status beside direction=out, an
	// empty id, and a dependency type no edge could carry. Each reaches the
	// client as the 400 it is, on the sentinel, with the parameter named in the
	// validator's own order.
	OpCountDependencyEdges: {CodeInvalidArgument, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// The same vocabulary as the stored-edge read beside it, and no not_found
	// for a stronger version of the same reason: this operation probes no id's
	// existence at all, so there is nothing it could 404 on.
	OpListBlockingAnnotations: {CodeInvalidArgument, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// getDependencyTree's row exactly, and for its reasons: ONE anchor, so a
	// miss is the 404 it is rather than a per-anchor flag — there is no other
	// answer to preserve by reporting it in the body, and an empty neighbor
	// list is the common case, so a typo answered with one would never surface.
	//
	// Its 400 is BOTH the transport's and the ROLE's. The transport owns the
	// unknown key and the repeated single-valued parameter; ValidateRelatedRequest
	// owns the two that are about this request's MEANING — a missing or
	// unrecognized direction, and a dependency type no edge could carry — and each
	// reaches the client as the 400 it is, on the sentinel, with the parameter
	// named. The validator's third refusal, an empty anchor id, is unreachable
	// here: the id is a PATH segment this handler bounds before the role is
	// asked, and an id that fails that bound is the 404 a real miss gets.
	OpListRelatedIssues: {CodeInvalidArgument, CodeUnauthenticated, CodeNotFound, CodeBusy, CodeDBUnavailable, CodeInternal},
	// The 404 is the difference from the row above: this operation has ONE
	// anchor, so there is no other answer to preserve by reporting the miss in
	// the body. Its 400 is its own — an empty root, a direction outside the
	// closed set, a non-positive max_depth — all three the ROLE's ErrValidation
	// reaching the wire.
	OpGetDependencyTree: {CodeInvalidArgument, CodeUnauthenticated, CodeNotFound, CodeBusy, CodeDBUnavailable, CodeInternal},
	// The same vocabulary as the listing it sizes: it takes the same filters
	// and can refuse them the same way. limit=0's mode-dependent refusal has no
	// analog here because there is no limit to pass.
	OpCountReadyWork: {CodeInvalidArgument, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// The ready count's vocabulary exactly, and for the same reasons: a
	// cardinality has no page, so there is no cursor to invalidate and no
	// unlimited-read refusal to make; and no 404, because a predicate matching
	// nothing is 0 — the role has no ErrNotFound at all, which its own doc
	// states, since a question about a set has an answer even when the set is
	// empty.
	//
	// Its 400 is ENTIRELY THE TRANSPORT'S, which is the one way this row differs
	// from the listings' beside it: a malformed boolean, integer or timestamp, a
	// repeated single-valued parameter, and a `group_by` outside the closed set.
	//
	// No ROLE refusal is reachable. issueops.Counter has exactly one
	// ErrValidation — ValidateCountGroup's unknown dimension, since
	// BuildCountFilter cannot fail — and countGroupOf refuses that dimension at
	// the edge, so the shared read failure path never classifies a count. An
	// unrecognized status or type is not a refusal at all here; the role
	// promises it matches nothing and answers 0.
	// TestCountGroupEnumMatchesTheRolesVocabulary is what keeps that true.
	OpCountIssues: {CodeInvalidArgument, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// The listing's vocabulary minus the cursor: this operation has none, so
	// invalid_cursor cannot arise. An unparseable EXPRESSION is an
	// invalid_argument on `q` rather than a code of its own — a client's
	// recovery is the same as for any other malformed parameter value.
	OpQueryIssues: {CodeInvalidArgument, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// The 400 here is the widest on this surface, and most of it is not this
	// handler's: the unfiltered durable sweep, the unrecognized tier and the
	// malformed glob are all the ROLE's ErrValidation reaching the wire through
	// failSweepErr. No 404 — this operation names no id — and no 409: a bead
	// another sweep already took is simply not in the set.
	OpSweepIssues: {CodeInvalidArgument, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// CodeNotFound is the one this operation has and the sweep does not: a
	// sweep describes a set that can legitimately be empty, while a delete
	// names beads and an id that resolves to nothing is a caller mistake.
	//
	// precondition_failed is `expected_version`'s, and it is the 409 form for
	// the reason that code documents: a miss refuses the whole request and
	// leaves nothing to report but the refusal. It ranks BELOW the 404 — a
	// request that named no row has nothing to be stale about — and ABOVE the
	// dependents refusal, which is the role's own order and is what makes the
	// wire's answer the same one `bd delete` gives.
	//
	// The ARITY refusal is a 400 rather than a second conflict: a token beside
	// two distinct ids is a malformed request, not a statement about state.
	OpDeleteIssues: {
		CodeInvalidArgument, CodeUnauthenticated, CodeNotFound, CodePreconditionFailed,
		CodeBusy, CodeDBUnavailable, CodeInternal,
	},
	OpClaimIssue: {
		CodeInvalidArgument, CodeUnauthenticated, CodeNotFound, CodeAlreadyClaimed, CodeNotClaimable,
		CodeBusy, CodeDBUnavailable, CodeInternal,
	},
	// The NARROWEST write vocabulary on this surface, and the narrowness is the
	// contract rather than an oversight. A problem document from this operation
	// means the batch never ran; every refusal an ITEM can earn — not_found for
	// an id naming no row, not_closable for close policy — travels in that
	// item's outcome inside a 200. A 404 here would say the operation went to
	// the wrong place, and a 409 would say the whole batch was refused, and
	// neither is ever true of a per-item refusal.
	OpBatchCloseIssues: {CodeInvalidArgument, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// NO 409 AND NO 404, and both absences are this operation's contract rather
	// than an omission. There is no id to have missed, and a row a racing agent
	// took is simply not in the set this claim scanned — the role walks past it
	// inside the transaction, which is the whole reason the operation exists.
	// An empty ready front is a 200 with the row absent, not a refusal.
	//
	// The 400 is the ready listing's filter vocabulary plus this operation's own
	// `limit` refusal plus the body rules, and the ROLE's ErrValidation behind
	// them, which is defensively unreachable.
	OpClaimNextIssue: {CodeInvalidArgument, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// THREE conflict codes, and only one of them is new. `already_claimed` is
	// the ownership fence, inherited from updateIssue's assignee arm: the same
	// situation — a live foreign owner refusing a write — with the same two
	// bypasses, spelled `force` and `expected_assignee` here. `precondition_failed`
	// is the `expected_assignee` guard, inherited from the same operation and
	// carrying the same members for the same reason: the request's expectation,
	// never an observation.
	//
	// `not_releasable` is the mint, and its own doc carries the analysis.
	//
	// The 404 is the path id's, on the terms updateIssue states. There is no
	// `not_claimable` here even though the claim's status refusal is the nearest
	// neighbor — see CodeNotReleasable for why that reuse was refused.
	OpReleaseIssue: {
		CodeInvalidArgument, CodeUnauthenticated, CodeNotFound,
		CodeAlreadyClaimed, CodeNotReleasable, CodePreconditionFailed,
		CodeBusy, CodeDBUnavailable, CodeInternal,
	},
	// TWO 409s, and they answer different questions with `code` as the only
	// discriminator a client needs.
	//
	// not_closable is close POLICY: an unforced close refused for open children
	// or for a live blocker. There is no already_claimed here — closing work
	// somebody else holds is not a refusal on this surface — and the idempotent
	// re-close is a 200 carrying `already_closed`, the claim's answer to the
	// same question.
	//
	// precondition_failed is `expected_version`'s, in the 409 form for the
	// reason that code documents, and it is CHECKED FIRST — before policy and
	// before the idempotent re-close, which is the role's own order
	// (issueops.Lifecycle.Close). `force` bypasses policy and never it: the two
	// members make unrelated claims.
	OpCloseIssue: {
		CodeInvalidArgument, CodeUnauthenticated, CodeNotFound, CodeNotClosable, CodePreconditionFailed,
		CodeBusy, CodeDBUnavailable, CodeInternal,
	},
	// ONE 409, and it is a PRECONDITION rather than a policy. Close has a policy
	// guard — open children, a live blocker — and reopen is the direction that
	// takes an issue OUT of the done category rather than putting it in, so
	// there is no state of the graph that can refuse it: not_closable is absent
	// here and always will be. What `expected_version` refuses is the request's
	// own premise, which every write can have wrong. The idempotent case is
	// still a 200 carrying `already_open`, unless a guard came with it.
	OpReopenIssue: {
		CodeInvalidArgument, CodeUnauthenticated, CodeNotFound, CodePreconditionFailed,
		CodeBusy, CodeDBUnavailable, CodeInternal,
	},
	// FOUR conflict codes, and the three members that publish them are exactly
	// the ones this row used to say would drag them in: `status` brought close
	// policy, `assignee` the fence, `parent_id` the graph vocabulary. Publishing
	// the members is what made the codes reachable, and every one of them is
	// inherited from an operation that already has it — this is applyBatch's row
	// for a single update item, minus `already_exists`, which needs a create.
	//
	// precondition_failed is the guard trio's, and it is the 409 form rather
	// than compareAndSetMetadata's 200 for the reason that code documents: a
	// miss here refuses the whole write and leaves nothing to report but the
	// refusal, where a lost metadata swap is a retry loop's ordinary iteration
	// carrying the value it needs next.
	//
	// The 404 is still the PATH id only. A `patch.parent_id` that names nothing
	// is an edge endpoint and stays a 400, conforming to addDependencies.
	//
	// The 400 is the body vocabulary plus the ROLE's ErrValidation — a
	// workspace-vocabulary issue_type or status, a metadata key the query layer
	// could not spell, a field-length refusal that slipped the edge check —
	// through failUpdate.
	OpUpdateIssue: {
		CodeInvalidArgument, CodeUnauthenticated, CodeNotFound,
		CodePreconditionFailed, CodeNotClosable, CodeAlreadyClaimed,
		CodeDependencyCycle, CodeDependencyExists,
		CodeBusy, CodeDBUnavailable, CodeInternal,
	},
	// No not_found. The role refuses an edge whose target names nothing, and
	// that refusal is about the REQUEST BODY the client sent, not about a
	// resource this operation was asked to address — there is no id in the path
	// to have missed. A 404 here would tell a client its request went to the
	// wrong place.
	//
	// No conflict code either: this operation publishes no `id` member, so no
	// item can collide with a stored row and the role's ErrAlreadyExists is
	// unreachable from the wire.
	OpBatchCreateIssues: {CodeInvalidArgument, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// The batch's row plus the two codes its narrower vocabulary cannot earn,
	// and both additions are members rather than judgement calls.
	//
	// already_exists arrives with `id`: that operation publishes none, so its
	// items can never collide with a stored row and the role's ErrAlreadyExists
	// is unreachable from its wire. Here it is reachable and it is a 409 for
	// CodeNotClosable's reason — the body is well-formed and STATE refuses it.
	//
	// dependency_cycle arrives with `parent_id` and `dependencies[].reverse`:
	// the first places the new row inside a hierarchy the caller cannot see, so
	// a blocking edge against its own ancestor is refusable, and the second
	// writes an edge INTO the id being minted, which is the only way a create
	// can close a scheduling cycle at all. It is addDependencies' 409 unchanged,
	// including the hierarchy discriminator.
	//
	// NO 404 and no dependency_exists. A target that names nothing is a
	// statement about the request body rather than a resource this operation was
	// asked to address — batchCreateIssues' argument, and there is no id in this
	// path to have missed. And the only type conflict a create can raise is
	// between two edges of the SAME request, which is a malformed body: no
	// stored edge can name a pair whose endpoint is an id this request is
	// minting.
	OpCreateIssue: {
		CodeInvalidArgument, CodeUnauthenticated, CodeAlreadyExists, CodeDependencyCycle,
		CodeBusy, CodeDBUnavailable, CodeInternal,
	},
	// The widest row here, and every code on it is inherited from an operation
	// that already has it: this one performs the lifecycle's writes, the claim's
	// assignee transfer and the graph's edge assertions inside one transaction,
	// so it can earn any refusal any of them can.
	//
	// THERE IS A CONFLICT CODE HERE, unlike the metadata compare-and-set, and
	// that is the whole difference between the two contracts rather than a
	// difference of taste. A lost compare-and-set is that operation's ORDINARY
	// path — a retry loop is its designed caller — so a 409 there would put the
	// normal iteration in the error channel and force the value the loop needs
	// next into a problem member. Here a precondition miss refuses every item in
	// the request, so there is no partial outcome to report on a 200 and nothing
	// for a client to do with one: the refusal IS the answer, and it belongs
	// where every other "the state says no" on this surface lives.
	//
	// The 404 is the delete's, not the batch create's: `update` and `close`
	// items NAME rows this request acts on, so a target that resolves to nothing
	// is a resource the request failed to address. An EDGE endpoint stays a 400,
	// conforming to addDependencies — nothing in that refusal is about a
	// resource this operation was asked to address.
	OpApplyBatch: {
		CodeInvalidArgument, CodeUnauthenticated, CodeNotFound,
		CodePreconditionFailed, CodeNotClosable, CodeAlreadyClaimed, CodeAlreadyExists,
		CodeDependencyCycle, CodeDependencyExists,
		CodeBusy, CodeDBUnavailable, CodeInternal,
	},
	// getDependencyTree's row, and it is the same shape for the same two
	// reasons: ONE anchor, so an id that names nothing is the 404 it is rather
	// than a per-item flag, and a 400 that is BOTH the transport's and the
	// ROLE's.
	//
	// NO CONFLICT CODE, and the absence is the operation's contract. A thread is
	// append-only and this write touches no field of the issue, so there is no
	// row state a guard could be stale about and no concurrent comment for this
	// one to collide with — which is also why there is no `expected_version`
	// member to earn a precondition_failed with.
	//
	// Of the role's three ErrValidation refusals exactly ONE is reachable here.
	// An empty author is refused at the edge under `actor`'s rules, which are
	// strictly stronger, and an empty issue id cannot arrive at all — a ServeMux
	// wildcard does not match an empty segment, and an id that fails the path
	// bound is the 404 a real miss gets. So the blank body is the whole of what
	// the role can refuse over this wire, which is why failAddComment names one
	// parameter rather than re-asking the validator's questions.
	OpAddComment: {CodeInvalidArgument, CodeUnauthenticated, CodeNotFound, CodeBusy, CodeDBUnavailable, CodeInternal},
	// No 404 and no conflict code: this is an UPSERT with a server-derivable
	// key, so there is no resource it can fail to address and no row it can
	// collide with. Its 400 is the body vocabulary plus the ROLE's two
	// refusals — empty content, and content no key can be derived from.
	OpRememberMemory: {CodeInvalidArgument, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// THE 404 IS THE DIVERGENCE, and it is deliberate. OpGetSetting has none
	// because on that plane an absent key and a key stored empty are one answer
	// the CLI itself prints identically, so a 404 would publish an invented
	// distinction. Here the CLI already distinguishes a miss — `bd recall` has
	// an exit-code contract for it — and the role answers Found rather than a
	// value, so the 404 reports a distinction that exists. The stored-empty row
	// falls on the miss side of it, which the document states.
	OpGetMemory: {CodeInvalidArgument, CodeUnauthenticated, CodeNotFound, CodeBusy, CodeDBUnavailable, CodeInternal},
	// The read's vocabulary exactly, because the two operations address the same
	// resource the same way and this one's Found false is the same answer: a key
	// nothing stored is a 404 and nothing was removed. No 409 — a memory another
	// caller already forgot is simply not there, which is what the 404 says.
	OpForgetMemory: {CodeInvalidArgument, CodeUnauthenticated, CodeNotFound, CodeBusy, CodeDBUnavailable, CodeInternal},
	// The 400 here IS this operation's own, unlike listSettings' absent row: it
	// has one parameter, and a repeated `q` is refused rather than resolved to
	// one of its values. No 404 — a search matching nothing is an empty page,
	// because a question about a set has an answer even when the set is empty.
	OpListMemories: {CodeInvalidArgument, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// The two journal codes are this operation's alone and neither has a
	// precedent elsewhere on this surface, because no other operation reads a
	// LOG. The 400 is its own too: `since` is required, and a negative or
	// unparseable checkpoint is refused rather than treated as zero — `seq > -5`
	// would quietly serve the whole journal as if it were a legitimate resume,
	// which is the same refusal `bd events tail --since` makes.
	//
	// No 404. A checkpoint at or past the head is a caught-up 200 with an empty
	// list, because "nothing new yet" is an answer about a log rather than a
	// missing resource, and a poller that got a 404 for being up to date would
	// have to treat the surface's miss vocabulary as a normal steady state.
	OpListEvents: {
		CodeInvalidArgument, CodeUnauthenticated, CodeEventsJournalDisabled, CodeEventsJournalTruncated,
		CodeBusy, CodeDBUnavailable, CodeInternal,
	},
	// THE PAGED READ'S VOCABULARY PLUS ONE, and the shared part is the point:
	// every connect-time refusal a poller can earn, a stream earns identically,
	// because the stream only opens once the same first read has succeeded. The
	// 410 in particular is a real 410 here and not an in-band event — that
	// mapping applies to a prune that races an ALREADY OPEN stream, where no
	// status is left to send.
	//
	// events_watch_saturated is the addition, and it is the only code on this
	// surface that describes a limit on connections rather than on data. Its 503
	// therefore documents three codes where every other operation's documents
	// two.
	OpWatchEvents: {
		CodeInvalidArgument, CodeUnauthenticated, CodeEventsJournalDisabled, CodeEventsJournalTruncated,
		CodeEventsWatchSaturated, CodeBusy, CodeDBUnavailable, CodeInternal,
	},
	// NO 404, for a stronger version of the batch create's reason: an edge that
	// is not there is `removed: false`, and an endpoint id that names nothing
	// holds no edge either, so this operation probes no id's existence and has
	// nothing it could report a miss on — listBlockingAnnotations' argument,
	// applied to a write. No conflict code either: the removal is idempotent, so
	// another caller having got there first is a success rather than a collision.
	// NO CONFLICT CODE, and the absence is this operation's whole posture. A
	// lost compare-and-set is a 200 carrying `swapped: false` and the current
	// value, because a retry loop is the DESIGNED caller and a 409 would put its
	// ordinary path in the error channel — and would have to smuggle the value
	// that loop needs next into a problem extension member. The 404 is here for
	// the id, which is the one refusal a caller cannot converge on.
	OpCompareAndSetMetadata: {
		CodeInvalidArgument, CodeUnauthenticated, CodeNotFound,
		CodeBusy, CodeDBUnavailable, CodeInternal,
	},
	OpRemoveDependency: {CodeInvalidArgument, CodeUnauthenticated, CodeBusy, CodeDBUnavailable, CodeInternal},
	// The two conflict codes are this operation's alone, and both say the same
	// kind of thing: the request is fine as a request and the GRAPH refuses it,
	// which the caller cannot know without reading state it does not have. That
	// is the claim's not_claimable situation and it gets the claim's answer, a
	// typed 409 whose extension members are read inside the refusing
	// transaction. The delete's 400-for-a-graph-refusal precedent does not
	// apply: that one is about request COMPLETENESS — send cascade or force —
	// and neither of these has a force to send.
	//
	// No 404: an endpoint that names nothing is a refusal of the request BODY,
	// so it joins the 400 with every other body refusal (batchCreateIssues'
	// argument). Nothing was written in any of these cases.
	OpAddDependencies: {
		CodeInvalidArgument, CodeUnauthenticated, CodeDependencyCycle, CodeDependencyExists,
		CodeBusy, CodeDBUnavailable, CodeInternal,
	},
}
