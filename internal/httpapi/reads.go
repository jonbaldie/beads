package httpapi

import (
	"net/http"

	"github.com/jonbaldie/beads/internal/httpapi/apigen"
	"github.com/jonbaldie/beads/issueops"
)

// The issue-collection reads. Each one decodes its parameters, hands the whole
// request to a role, and shapes the answer onto the wire. Three are on
// issueops.Reader (ready, list, detail); the count and the query are on
// ReadyCounter and Querier, siblings reached the same way through the same
// provider accessors. What follows is about the Reader three, and holds for
// the other two in every respect but which role they name.
//
// WHAT IS NOT HERE IS THE POINT. No filter is built, no ConfigSource is wired,
// no default limit is applied, no status exclusion is chosen, no wisp fallback
// is arranged: all of that is inside issueops.Reader's implementation, which
// `bd show --json` reaches through the same accessor. A handler that skipped a
// step of that construction — which is exactly how a hosted viewer once shipped
// a bare IssueFilter and silently served in_progress rows as "ready" — is not
// writable from here.
//
// That is a MACHINE claim, not a convention, and it takes two rules because
// there are two ways to build a filter. The depguard rule
// httpapi-transport-boundary denies internal/workapi from this package's
// non-test files, so the builders are not importable here at all. That alone
// would leave the door the hosted viewer actually walked through: it did not
// misuse a builder, it hand-rolled `IssueFilter{}` — and internal/types is
// reachable from here, for CheckFieldLen and IssueWithCounts below. So the
// forbidigo rule in .golangci.yml denies NAMING types.IssueFilter or
// types.WorkFilter in this package at all — every file of it, so a file added
// here tomorrow is covered the moment it exists. Both rules run in
// `make ci-pr-lint`, which the PR workflow runs on every pull request and
// aggregates into its ci-gate job; what that gate is and is not worth as
// enforcement is stated once, in doc.go.
//
// `bd ready` and `bd list` still call the builders directly, for the reasons
// issueops.Reader's doc comment sets out. They are not unguarded for it: they
// build from the same request types through the same builders, and they run
// the same workapi.FinishPage epilogue the reader below runs — `bd list` in
// every mode but the hierarchical --parent tree, `bd ready` on its proxied
// route only. Where both hold, what a CLI listing and one of these bodies can
// differ by is presentation, not the query and not the page. `bd show --json`
// is on the role outright, on both its routes.
//
// What DOES stay here is transport: parameter decoding, the opaque cursor
// codec, the bind-mode refusal of an unlimited read, and the wire envelopes.

// handleReady answers GET /v0/beads/ready.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	q := newQuery(r.URL.Query())

	req := readyFilters(q)

	// EXPLICIT, always. The storage layer maps an EMPTY sort policy to hybrid,
	// and forwarding an absent `sort` as "" would silently adopt that fallback
	// while the document still read `default: priority`. It is not a cosmetic
	// difference: hybrid demotes older high-priority work, so the item SET
	// changes as soon as `limit` truncates — and only for the clients this API
	// exists to migrate off `bd ready`, whose flag registers a concrete default
	// and never sends empty.
	req.Sort = q.oneOf("sort", readySortDefault, "hybrid", "priority", "oldest")
	req.Limit = q.limit()
	// Decoded here and not in readyFilters, which is the vocabulary the count
	// shares: this is a projection of the rows a page returns, in the same
	// class as the two lines above it, and the count returns no rows to project.
	req.Brief = q.boolean("brief")

	if !s.acceptQuery(w, r, q) {
		return
	}
	if !s.allowUnlimited(w, r, req.Limit) {
		return
	}

	rd, err := s.reader(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	page, err := rd.Ready(r.Context(), req)
	if err != nil {
		s.failReadErr(w, r, err)
		return
	}
	writeJSON(w, apigen.ReadyPage{
		Items:   wireItems(page.Items),
		HasMore: page.HasMore,
	})
}

// readyFilters decodes the ready-work filter vocabulary the two ready
// operations share — everything except the PAGE, which only the listing has,
// and the ORDER, which only the listing needs.
//
// It is one function because the two operations must admit exactly the same
// filters: countReadyWork answers with the size of the set listReadyWork
// returns, and a parameter one of them decoded and the other did not would
// make that identity false for any client that sent it.
func readyFilters(q *query) issueops.ReadyRequest {
	return issueops.ReadyRequest{
		IssueType: q.str("type"),
		ReadyRequestFilters: issueops.ReadyRequestFilters{
			Assignee:       q.str("assignee"),
			Unassigned:     q.boolean("unassigned"),
			ExcludeLabels:  q.list("exclude_label"),
			LabelPattern:   q.str("label_pattern"),
			LabelRegex:     q.str("label_regex"),
			ExcludeTypes:   q.list("exclude_type"),
			MetadataFields: q.metadataFields("metadata_field"),
			HasMetadataKey: q.str("has_metadata_key"),
		},

		Labels:    q.list("label"),
		LabelsAny: q.list("label_any"),

		Priority: q.integer("priority"),
		ParentID: q.str("parent"),

		IncludeEphemeral: q.boolean("include_ephemeral"),
		IncludeDeferred:  q.boolean("include_deferred"),
	}
}

// handleCountReady answers GET /v0/beads/ready:count.
//
// THE REQUEST IS THE LISTING'S REQUEST with the page taken off, which is what
// makes the answer the size of the page the listing would return. It is not
// assembled here beyond that: the role refuses a Limit and an Offset itself
// (issueops.ReadyCounter.CountReady), so this handler could not smuggle a
// bounded count past it even if a parameter for one existed.
//
// THE SORT IS SENT ANYWAY, and the operation publishes no parameter for it. A
// count has no order, but the request still has to be one the builder accepts,
// and sending "" would adopt the storage layer's hybrid fallback that no front
// door relies on.
func (s *Server) handleCountReady(w http.ResponseWriter, r *http.Request) {
	q := newQuery(r.URL.Query())

	req := readyFilters(q)
	req.Sort = readySortDefault

	if !s.acceptQuery(w, r, q) {
		return
	}

	counter, err := s.readyCounter(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	result, err := counter.CountReady(r.Context(), req)
	if err != nil {
		s.failReadErr(w, r, err)
		return
	}
	writeJSON(w, apigen.ReadyCount{Total: result.Total})
}

// readySortDefault is the ordering this operation applies when `sort` is
// absent. It is the same value `bd ready --sort` registers.
//
// Three things have to agree: the frozen document, the CLI flag, and this.
// TestDefaultsMatchCLIFlags compares all three against each other and
// TestReadyForwardsAnExplicitSortPolicy asserts the LITERAL "priority" on the
// filter the handler built — deliberately not this constant, because an
// assertion against the value the handler itself read would hold for every
// value it could take.
const readySortDefault = "priority"

// countFilters decodes the count's predicate: every filter the role publishes
// and nothing about a page, an order or a bucket.
//
// It is a function of its own for readyFilters' reason turned inside out. That
// one is shared because two operations must admit the same parameters; this one
// has a single caller, and it is split off so a test can drive it over an empty
// query and read back the EXACT set of names this handler asks for
// (query.read). That is what makes the parameter-parity check mechanical rather
// than a second hand-rolled list beside the document's.
func countFilters(q *query) issueops.CountRequest {
	return issueops.CountRequest{
		CountIdentityFilters: issueops.CountIdentityFilters{
			// ONE status, not the listing's comma-separated OR set. The role says so
			// and the document says so; reading it with q.csv here would publish a
			// set the role would answer 0 for.
			Status: q.str("status"), IssueType: q.str("type"), Assignee: q.str("assignee"),
		},

		CountPriorityFilters: issueops.CountPriorityFilters{
			Priority: q.integer("priority"), PriorityMin: q.integer("priority_min"), PriorityMax: q.integer("priority_max"),
		},

		CountTextFilters: issueops.CountTextFilters{
			Labels: q.list("label"), LabelsAny: q.list("label_any"),

			TitleSearch: q.str("title"),
			// A COMMA-SEPARATED string, handed over as written: the role splits,
			// trims and de-duplicates it, and a handler that pre-split it would be
			// deciding what an id set means.
			IDFilter: q.str("id"),

			TitleContains: q.str("title_contains"), DescContains: q.str("desc_contains"), NotesContains: q.str("notes_contains"),
		},

		CountTimeFilters: issueops.CountTimeFilters{
			CreatedAfter: q.timestamp("created_after"), CreatedBefore: q.timestamp("created_before"),
			UpdatedAfter: q.timestamp("updated_after"), UpdatedBefore: q.timestamp("updated_before"),
			ClosedAfter: q.timestamp("closed_after"), ClosedBefore: q.timestamp("closed_before"),
		},

		CountPresenceFilters: issueops.CountPresenceFilters{
			EmptyDesc: q.boolean("empty_description"), NoAssignee: q.boolean("no_assignee"), NoLabels: q.boolean("no_labels"),

			// The plane switch, forwarded as the boolean the caller sent. What it
			// MEANS — merge the wisps tier, drop templates, drop gates, and route an
			// infra type to the ephemeral tier — is four decisions the role makes
			// from the WORKSPACE's own infra vocabulary, which is a config load this
			// handler must never perform.
			IncludeInfra: q.boolean("include_infra"),
		},
	}
}

// countGroupOf reads the bucketing dimension and reports whether one was asked
// for.
//
// PRESENCE is the signal, which is why this returns a boolean beside the value:
// an absent `group_by` selects the scalar method, and q.oneOf's fallback alone
// would collapse "no bucketing asked for" into a dimension. An unknown value is
// refused HERE rather than at the role, so the 400 names the parameter — the
// role's own rule (an unknown dimension is ErrValidation, never an empty
// answer) with the member name a client dispatches on added.
func countGroupOf(q *query) (issueops.CountGroup, bool) {
	grouped := q.has("group_by")
	return issueops.CountGroup(q.oneOf("group_by", "", countGroupNames()...)), grouped
}

// countGroups is the closed dimension vocabulary, in the document's order, so
// the schema's enum and the values this server accepts are one list read twice
// rather than two lists kept in step by hand.
//
// It is spelled with the ROLE's constants rather than as bare strings: the wire
// names and issueops.CountGroup's values are the same strings today, and
// deriving one from the other is what keeps them the same tomorrow.
var countGroups = []issueops.CountGroup{
	issueops.CountGroupStatus,
	issueops.CountGroupPriority,
	issueops.CountGroupType,
	issueops.CountGroupAssignee,
	issueops.CountGroupLabel,
}

// countGroupNames is countGroups as the strings q.oneOf compares against.
func countGroupNames() []string {
	names := make([]string, len(countGroups))
	for i, g := range countGroups {
		names[i] = string(g)
	}
	return names
}

// handleCountIssues answers GET /v0/beads/issues:count.
//
// ONE HANDLER FOR BOTH OF THE ROLE'S METHODS, because `group_by` chooses
// between two shapes of one answer rather than between two questions: the same
// predicate over the same set, differing only in whether the reply is one
// number or a number per bucket. The grouped result carries the scalar total
// itself, which is why splitting them would have put one role's promise inside
// the other's result.
//
// WHAT IS NOT HERE is this file's whole point, and on a count it is more than
// usual. No filter is built, no ConfigSource is wired, and the workspace's
// INFRA VOCABULARY is never read — that config load is precisely what
// issueops.Counter exists to keep off a front door.
//
// The default answer is the ROLE's too, and it is NOT the listing's: an empty
// request counts every durable row including closed, pinned, template and gate
// ones. A handler that "helpfully" applied the listing's exclusions would be
// answering a different question with the same parameters.
func (s *Server) handleCountIssues(w http.ResponseWriter, r *http.Request) {
	q := newQuery(r.URL.Query())

	req := countFilters(q)
	group, grouped := countGroupOf(q)

	if !s.acceptQuery(w, r, q) {
		return
	}

	counter, err := s.counter(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	if grouped {
		// THE SAME PREDICATE reaches both methods, which is the identity the
		// role promises: a grouped count is a scalar count plus a dimension, so
		// the two cannot be asked of different sets.
		result, err := counter.CountByGroup(r.Context(), issueops.CountByGroupRequest{Filter: req, GroupBy: group})
		if err != nil {
			s.failReadErr(w, r, err)
			return
		}
		// `groups` is PRESENT because the request asked for buckets, even when
		// the answer has none: an empty object means "nothing matched" and an
		// absent member means "you did not ask", and a client must be able to
		// tell those apart without re-reading its own request. The role promises
		// a non-nil map; this does not lean on that promise, because a nil map
		// would marshal as `{}` anyway and leaning on it would make the
		// difference invisible if it ever broke.
		groups := result.Groups
		if groups == nil {
			groups = map[string]int{}
		}
		writeJSON(w, apigen.IssueCount{Total: result.Total, Groups: &groups})
		return
	}
	result, err := counter.Count(r.Context(), req)
	if err != nil {
		s.failReadErr(w, r, err)
		return
	}
	writeJSON(w, apigen.IssueCount{Total: result.Total})
}
