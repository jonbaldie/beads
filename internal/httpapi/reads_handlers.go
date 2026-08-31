package httpapi

import (
	"net/http"
	"strings"

	"github.com/jonbaldie/beads/internal/httpapi/apigen"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/issueops"
)

// handleListIssues answers GET /v0/beads/issues.
func (s *Server) handleListIssues(w http.ResponseWriter, r *http.Request) {
	q := newQuery(r.URL.Query())

	req := issueops.ListRequest{ListIdentityFilters: issueops.ListIdentityFilters{Status: q.csv("status"),
		IssueType: q.str("type"),
		Assignee:  q.str("assignee")}, ListLabelFilters: issueops.ListLabelFilters{Labels: q.list("label"),
		LabelsAny:     q.list("label_any"),
		ExcludeLabels: q.list("exclude_label")}, ListTimeFilters: issueops.ListTimeFilters{CreatedBefore: q.timestamp("created_before"),
		CreatedAfter: q.timestamp("created_after")}, ListProjectionOptions: issueops.ListProjectionOptions{Brief: q.boolean("brief")}, ListVisibilityOptions: issueops.ListVisibilityOptions{IncludeTemplates: q.boolean("include_templates"),
		IncludeGates:     q.boolean("include_gates"),
		IncludeInfra:     q.boolean("include_infra"),
		IncludeEphemeral: q.boolean("include_ephemeral")}, ListRelationFilters: issueops.ListRelationFilters{ParentID: q.str("parent")}, ListStateFilters: issueops.ListStateFilters{MetadataFields: q.metadataFields("metadata_field"),
		HasMetadataKey: q.str("has_metadata_key")}, ListModeOptions: issueops.ListModeOptions{AllFlag: q.boolean("all")}, ListPageOptions: issueops.ListPageOptions{

		// ORDERING IS FIXED AND DIVERGES FROM `bd list` DELIBERATELY, which is
		// why there is no `sort` parameter to decode. The cursor is a keyset
		// position in the created order, so a first page under `bd list`'s
		// priority-first default would make the second page skip and duplicate
		// rows. The order is welded to the cursor contract.
		SortBy: "created",

		Limit: q.limit()},
	}

	token := q.str("cursor")

	if !s.acceptQuery(w, r, q) {
		return
	}
	if token != "" {
		pos, ok := decodeCursor(token)
		if !ok {
			requestInfo(r.Context()).refuse(token)
			s.fail(w, r, InvalidCursor())
			return
		}
		req.AfterCreatedAt = &pos.CreatedAt
		req.AfterID = pos.ID
	}
	if !s.allowUnlimited(w, r, req.Limit) {
		return
	}

	rd, err := s.reader(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	page, err := rd.List(r.Context(), req)
	if err != nil {
		s.failReadErr(w, r, err)
		return
	}

	body := apigen.IssuesPage{
		Items:   wireItems(page.Items),
		HasMore: page.HasMore,
	}
	if page.HasMore {
		// Present if and only if has_more, which the document states as a
		// biconditional: a client that sees one and not the other has no way
		// to know whether paging is finished.
		if next := cursorFor(page.Items); next != "" {
			body.NextCursor = &next
		}
	}
	writeJSON(w, body)
}

// handleQueryIssues answers GET /v0/beads/issues:query.
//
// The EXPRESSION IS NOT PARSED HERE. Parsing, evaluation, the predicate
// decision and the scan bound all live inside the role, so this handler cannot
// make the truncating one: it reads five parameters and hands the sentence
// over.
func (s *Server) handleQueryIssues(w http.ResponseWriter, r *http.Request) {
	q := newQuery(r.URL.Query())

	req := issueops.QueryRequest{
		Expression:    q.str("q"),
		IncludeClosed: q.boolean("all"),
		SortBy:        q.oneOf("sort", "", querySorts...),
		Reverse:       q.boolean("reverse"),
		Limit:         q.limit(),
		// No Offset. The document publishes no parameter for one, because the
		// two database sources this server can be built on disagree about
		// whether they can honor it — see issueops.QueryRequest.Offset.
	}

	if !s.acceptQuery(w, r, q) {
		return
	}
	if !s.allowUnlimited(w, r, req.Limit) {
		return
	}

	qr, err := s.querier(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	page, err := qr.Query(r.Context(), req)
	if err != nil {
		s.failReadErr(w, r, err)
		return
	}
	writeJSON(w, apigen.QueryPage{
		Items:   wireItems(page.Items),
		HasMore: page.HasMore,
	})
}

// querySorts is the display-order vocabulary this operation publishes, in the
// document's order. It is the set workapi.CompareIssuesBy can order by; a value
// outside it is refused here rather than accepted and ignored, because a page
// returned unordered under a sort the caller named is indistinguishable from
// one whose order the caller does not understand.
var querySorts = []string{"priority", "created", "updated", "closed", "status", "id", "title", "type", "assignee"}

// handleGetIssue answers GET /v0/beads/issues/{id}.
func (s *Server) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	q := newQuery(r.URL.Query())

	// Both default off, so a request that names neither builds the request this
	// handler built when the operation had no parameters at all.
	req := issueops.GetRequest{
		IncludeComments:   q.boolean("include_comments"),
		IncludeDependents: q.boolean("include_dependents"),
		BriefDeps:         q.boolean("brief_deps"),
	}

	// Before the id bound, which is the order this operation had when
	// requireNoQuery ran first: a refused query string is a 400 that names what
	// to fix, and deciding the id first would answer it with a 404 instead.
	if !s.acceptQuery(w, r, q) {
		return
	}
	id := r.PathValue("id")

	// The id is bounded HERE, before the request buys a concurrency slot and a
	// database round trip, exactly as the claim route bounds its own. The
	// column is VARCHAR(255) and the document calls this an exact canonical
	// id, so a longer one — or one carrying a control character, which a
	// percent-escape in the path decodes to — names no row that can exist.
	// The refusal is the SAME 404 a real miss gets: a distinct answer would
	// let a caller map this server's notion of a well-formed id, and there is
	// nothing to learn from it.
	if id == "" || types.CheckFieldLen("id", id) != nil || strings.ContainsFunc(id, isControlChar) {
		s.fail(w, r, NotFound())
		return
	}

	req.ID = id

	rd, err := s.reader(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	details, err := rd.Get(r.Context(), req)
	if err != nil {
		s.failReadErr(w, r, err)
		return
	}
	writeJSON(w, *details)
}

// acceptQuery answers a refused query string and reports whether the request
// may proceed.
func (s *Server) acceptQuery(w http.ResponseWriter, r *http.Request, q *query) bool {
	res := q.result()
	if res == nil {
		return true
	}
	requestInfo(r.Context()).refuse(res.offender())
	s.fail(w, r, *res)
	return false
}

// allowUnlimited enforces the one mode-dependent refusal on this surface: an
// unlimited read buffers the whole active set and its JSON encoding inside one
// shared process, which must not be reachable by arbitrary network peers.
//
// The bind mode is deliberately NOT advertised in ContextResponse. A client
// that wants an unlimited read asks for one and, on this 400, re-issues with
// an explicit limit; it is a client-side fix, never a retry.
func (s *Server) allowUnlimited(w http.ResponseWriter, r *http.Request, limit *int) bool {
	if !s.cfg.AllowNonLoopback || limit == nil || *limit != 0 {
		return true
	}
	requestInfo(r.Context()).refuse("0")
	s.fail(w, r, InvalidArgument("limit", ReasonInvalidValue,
		"unlimited reads are loopback-only; pass an explicit limit"))
	return false
}

// failReadErr answers a failed read. It exists so that a filter-construction
// refusal — an unknown status, an issue type outside the workspace vocabulary,
// an invalid metadata key — reaches the client as the 400 it is rather than as
// a 500, while every storage failure keeps going through the one mapping in
// problem.go.
//
// The builders are the authority on what those messages say (the CLI prints
// them verbatim), so the detail is the builder's own text: it reflects the
// caller's own input back, which is what a 4xx detail is for.
func (s *Server) failReadErr(w http.ResponseWriter, r *http.Request, err error) {
	if param, ok := invalidFilterParam(err); ok {
		requestInfo(r.Context()).refuse(param)
		s.fail(w, r, InvalidArgument(param, ReasonInvalidValue, err.Error()))
		return
	}
	s.failErr(w, r, err)
}

// invalidFilterParam maps a filter-construction failure back to the query
// parameter that caused it.
//
// It matches on the builder's message prefixes, and that is a deliberate,
// bounded exception to this package's "never classify by prose" rule: these
// are THIS repository's own error strings, not a foreign library's. The
// alternative — typed errors for each — is the right end state and is a change
// to internal/workapi, not to the wire. A message that stops matching degrades
// to a 500, which is loud, rather than to a wrong 400.
//
// Every row here is driven end to end by a case in
// TestABuilderRefusalIsTheDocumentedBadRequest, so a reworded builder message
// fails a test rather than silently demoting its parameter to a 500. That
// test IS the pin: the builders' golden files record successful filters only,
// and cannot see a message at all.
func invalidFilterParam(err error) (string, bool) {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "invalid status "):
		return "status", true
	case strings.HasPrefix(msg, "invalid issue type "):
		return "type", true
	case strings.HasPrefix(msg, "invalid sort policy "):
		return "sort", true
	case strings.HasPrefix(msg, "invalid metadata field key"):
		return "metadata_field", true
	case strings.HasPrefix(msg, "invalid metadata key filter"):
		return "has_metadata_key", true
	// The query role's refusal. It is the caller's SENTENCE being wrong, which
	// is a 400 on `q` rather than the 500 an unclassified error would give.
	case strings.HasPrefix(msg, "invalid query expression"):
		return "q", true
	}
	return "", false
}

// wireItems projects the reader's page onto the generated envelope's element
// type.
//
// The element type is an ALIAS of types.IssueWithCounts — the same struct the
// CLI's --json marshals — so this copies a header and shares everything under
// it. There is no second wire struct here and there must never be one: that is
// what keeps `bd list --json` and this body one compatibility domain.
func wireItems(items []*types.IssueWithCounts) []apigen.IssueWithCounts {
	out := make([]apigen.IssueWithCounts, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		out = append(out, *item)
	}
	return out
}
