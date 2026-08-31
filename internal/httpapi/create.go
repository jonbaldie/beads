package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jonbaldie/beads/internal/httpapi/apigen"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/issueops"
)

// The wire adapter over issueops.Lifecycle.Create: ONE issue, its parent, its
// explicit edges and its waits-for gate, written as one transaction.
//
// It carries the claim's posture verbatim. The actor is caller-ASSERTED
// provenance for the audit trail and not authenticated identity; hooks do not
// fire and the per-command auto-commit machinery does not run. The only durable
// effect is the single storage commit the role makes inside its own
// transaction.
//
// EVERYTHING ABOVE THE ROLE IS ARGUMENT VALIDATION. The id minting, the prefix
// check, the create-only guard, the workspace's status and type vocabularies,
// the plane routing, the edge write and the graph gates all belong to
// issueops.Lifecycle. This file decodes a body two levels deep, refuses the
// shapes the document refuses, and marshals the row that came back.

const (
	// maxCreateBodyBytes bounds the request body. One issue can carry a
	// description, a design, acceptance criteria, notes and a metadata document
	// at once, so this is the update's bound rather than the claim's megabyte.
	maxCreateBodyBytes = 4 << 20
	// maxCreateDependencies is the document's cap on `dependencies`. It bounds
	// how long one request may hold a write transaction — not create semantics,
	// which have no edge count in them.
	maxCreateDependencies = 100
	// createMetadataMember is the one member whose explicit `null` is not
	// refused at the edge: its bytes are the metadata plane's own, and the role
	// is the single definition of what that plane accepts.
	createMetadataMember = "metadata"
)

// The document's member list at each of this body's levels. Every schema is
// additionalProperties: false, so anything else is refused BY NAME — which is
// why each level is decoded as raw members first: encoding/json's
// DisallowUnknownFields reports the offender only inside an error string.
var (
	createRequestMembers = []string{
		"acceptance_criteria", "actor", "assignee", "defer_until", "dependencies",
		"description", "design", "due_at", "ephemeral", "estimated_minutes",
		"external_ref", "force_id_prefix", "id", "inherit_labels_from_parent",
		"issue_type", "labels", createMetadataMember, "no_history", "notes", "owner",
		"parent_id", "priority", "sender", "status", "title", "waits_for",
	}
	createDependencyMembers = []string{"metadata", "reverse", "target_id", "type"}
	createWaitsForMembers   = []string{"gate", "spawner_id"}
)

// handleCreateIssue creates one issue, or creates nothing.
//
// A PLAIN COLLECTION POST rather than a custom method: this creates one member
// of the collection the path names, which is what POST already means — the
// argument rememberMemory makes on its own collection. The path was left free
// for exactly this operation when issues:batchCreate chose a custom method.
func (s *Server) handleCreateIssue(w http.ResponseWriter, r *http.Request) {
	if !s.requireNoQuery(w, r) {
		return
	}
	if !s.requireJSONContent(w, r) {
		return
	}
	request, ok := s.createIssueRequest(w, r)
	if !ok {
		return
	}

	lifecycle, err := s.lifecycle(r)
	if err != nil {
		s.failCreateIssue(w, r, err)
		return
	}
	result, err := lifecycle.Create(r.Context(), request)
	if err != nil {
		s.failCreateIssue(w, r, err)
		return
	}
	// The row as STORED, never the request reflected back: the role clones its
	// request, so the minted id, the defaulted status and the persisted
	// timestamps exist only on this snapshot. checkedLifecycle.Create is what
	// makes the dereference safe.
	writeJSON(w, *result.Issue)
}

// createIssueRequest decodes and validates the body, and reports whether the
// request may proceed. Every refusal here happens BEFORE any database work,
// which is what lets these 400s reflect the caller's own input back.
func (s *Server) createIssueRequest(w http.ResponseWriter, r *http.Request) (issueops.CreateRequest, bool) {
	refuse := func(res *Result) (issueops.CreateRequest, bool) {
		s.fail(w, r, *res)
		return issueops.CreateRequest{}, false
	}

	members, res := decodeJSONObject(w, r, maxCreateBodyBytes)
	if res != nil {
		return refuse(res)
	}
	if offender, unknown := unknownMember(members, createRequestMembers); unknown {
		s.failUnknownMember(w, r, offender, createRequestMembers)
		return issueops.CreateRequest{}, false
	}
	if res := validateCreateMembers(members); res != nil {
		return refuse(res)
	}
	actor, ok := s.bodyActor(w, r, members)
	if !ok {
		return issueops.CreateRequest{}, false
	}

	body, res := parseCreateBody(members)
	if res != nil {
		return refuse(res)
	}

	request := issueops.CreateRequest{
		Actor:                   actor,
		Issue:                   body.issue,
		ParentID:                derefString(body.wire.ParentId),
		InheritLabelsFromParent: derefBool(body.wire.InheritLabelsFromParent),
		ForceIDPrefix:           derefBool(body.wire.ForceIdPrefix),
	}
	// IDPrefix stays ZERO, and its absence is a decision rather than an
	// omission. It exists because a workspace's own config.yaml prefix wins over
	// the database's and only a local front door can read that file
	// (CreateRequest.IDPrefix). A remote client's config.yaml describes a
	// workspace this server does not serve, so publishing it would let a caller
	// override the served workspace's prefix rule from outside it.

	request.Dependencies = body.dependencies
	request.WaitsFor = body.waitsFor
	return request, true
}

type parsedCreateBody struct {
	wire         apigen.CreateIssueRequest
	issue        *types.Issue
	dependencies []issueops.CreateDependency
	waitsFor     *issueops.WaitsFor
}

func parseCreateBody(members map[string]json.RawMessage) (parsedCreateBody, *Result) {
	rawEdges, rawWaitsFor, res := createRawRelations(members)
	if res != nil {
		return parsedCreateBody{}, res
	}
	wire, res := decodeCreateWire(members)
	if res != nil {
		return parsedCreateBody{}, res
	}
	if res := validateCreateWire(&wire); res != nil {
		return parsedCreateBody{}, res
	}
	dependencies, res := createDependencies(rawEdges, wire.Dependencies)
	if res != nil {
		return parsedCreateBody{}, res
	}
	waitsFor, res := createWaitsFor(rawWaitsFor, wire.WaitsFor)
	if res != nil {
		return parsedCreateBody{}, res
	}
	return parsedCreateBody{
		wire: wire, issue: createIssueFromWire(&wire, members), dependencies: dependencies, waitsFor: waitsFor,
	}, nil
}

func validateCreateMembers(members map[string]json.RawMessage) *Result {
	for name, value := range members {
		if name != createMetadataMember && isJSONNull(value) {
			return createRefusal(name, "`"+name+"` is not nullable; omit it to leave it at the workspace default")
		}
	}
	return nil
}

func createRawRelations(members map[string]json.RawMessage) ([]map[string]json.RawMessage, map[string]json.RawMessage, *Result) {
	rawEdges, res := createRawEdges(members)
	if res != nil {
		return nil, nil, res
	}
	rawWaitsFor, res := createRawWaitsFor(members)
	if res != nil {
		return nil, nil, res
	}
	return rawEdges, rawWaitsFor, nil
}

func decodeCreateWire(members map[string]json.RawMessage) (apigen.CreateIssueRequest, *Result) {
	var wire apigen.CreateIssueRequest
	if err := json.Unmarshal(rawObject(members), &wire); err != nil {
		return apigen.CreateIssueRequest{}, createRefusal("", "a request member carries the wrong JSON type")
	}
	return wire, nil
}

func validateCreateWire(wire *apigen.CreateIssueRequest) *Result {
	if strings.TrimSpace(wire.Title) == "" {
		return createRefusal("title", "`title` is required and must not be blank")
	}
	for _, bounded := range []struct {
		member string
		value  *string
	}{
		{"id", wire.Id}, {"title", &wire.Title}, {"issue_type", wire.IssueType},
		{"status", wire.Status}, {"assignee", wire.Assignee}, {"owner", wire.Owner},
		{"external_ref", wire.ExternalRef}, {"sender", wire.Sender},
		{"parent_id", wire.ParentId},
	} {
		if res := applyBoundedText("", bounded.member, bounded.value); res != nil {
			return res
		}
	}
	if res := validateCreatePriority(wire.Priority); res != nil {
		return res
	}
	if res := validateCreateLabels(wire.Labels); res != nil {
		return res
	}
	if res := validateCreateRetention(wire); res != nil {
		return res
	}
	return nil
}

func validateCreatePriority(priority *int) *Result {
	if priority == nil {
		return nil
	}
	if *priority < 0 || *priority > 4 {
		return createRefusal("priority", fmt.Sprintf("`priority` is %d; the range is 0 to 4", *priority))
	}
	return nil
}

func validateCreateLabels(labels *[]string) *Result {
	if labels == nil {
		return nil
	}
	return applyBoundedLabels("", "labels", *labels)
}

func validateCreateRetention(wire *apigen.CreateIssueRequest) *Result {
	if !derefBool(wire.Ephemeral) {
		return nil
	}
	if derefBool(wire.NoHistory) {
		return createRefusal("no_history", "`ephemeral` and `no_history` select different retention modes; send one")
	}
	return nil
}

func createIssueFromWire(wire *apigen.CreateIssueRequest, members map[string]json.RawMessage) *types.Issue {
	issue := &types.Issue{
		IssueID: types.IssueID{ID: derefString(wire.Id)},
		IssueContent: types.IssueContent{
			Title: wire.Title, Description: derefString(wire.Description), Design: derefString(wire.Design),
			AcceptanceCriteria: derefString(wire.AcceptanceCriteria), Notes: derefString(wire.Notes),
		},
		IssueWorkflow: types.IssueWorkflow{
			Status: types.Status(derefString(wire.Status)), IssueType: types.IssueType(derefString(wire.IssueType)),
			Assignee: derefString(wire.Assignee), Owner: derefString(wire.Owner), EstimatedMinutes: wire.EstimatedMinutes,
		},
		IssueLease: types.IssueLease{DueAt: wire.DueAt, DeferUntil: wire.DeferUntil},
		IssueMeta:  types.IssueMeta{ExternalRef: wire.ExternalRef},
		IssueWisp:  types.IssueWisp{Sender: derefString(wire.Sender), Ephemeral: derefBool(wire.Ephemeral), NoHistory: derefBool(wire.NoHistory)},
	}
	if wire.Priority != nil {
		issue.Priority = *wire.Priority
	}
	if wire.Labels != nil {
		issue.Labels = append([]string(nil), *wire.Labels...)
	}
	if raw, present := members[createMetadataMember]; present {
		issue.Metadata = applyRawCopy(raw)
	}
	return issue
}

// createRawEdges reads `dependencies` as raw members, so an unknown member
// INSIDE an edge is refused by name and a member that is not an array of
// objects is refused against `dependencies` rather than against the body.
//
// It answers nil for an absent member, which is what "no edges" means.
func createRawEdges(members map[string]json.RawMessage) ([]map[string]json.RawMessage, *Result) {
	raw, present := members["dependencies"]
	if !present {
		return nil, nil
	}
	var edges []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &edges); err != nil || edges == nil {
		return nil, createRefusal("dependencies", "`dependencies` must be an array of edges")
	}
	for i, edge := range edges {
		if edge == nil {
			return nil, createRefusal(strings.TrimSuffix(createDependencyParam(i, ""), "."), "an edge must be a JSON object")
		}
	}
	return edges, nil
}

// createRawWaitsFor reads `waits_for` as raw members, for createRawEdges'
// reason. It answers nil for an absent member, which is what "no gate" means.
func createRawWaitsFor(members map[string]json.RawMessage) (map[string]json.RawMessage, *Result) {
	raw, present := members["waits_for"]
	if !present {
		return nil, nil
	}
	var gate map[string]json.RawMessage
	if err := json.Unmarshal(raw, &gate); err != nil || gate == nil {
		return nil, createRefusal("waits_for", "`waits_for` must be a JSON object naming a `spawner_id`")
	}
	return gate, nil
}

// createDependencies validates `dependencies` and projects it onto the role's
// edges. The raw members are re-read beside the generated slice so an unknown
// member INSIDE an edge is refused by name like every other one.
func createDependencies(rawEdges []map[string]json.RawMessage, wire *[]apigen.CreateIssueDependency) ([]issueops.CreateDependency, *Result) {
	if wire == nil {
		return nil, nil
	}
	if len(*wire) > maxCreateDependencies {
		return nil, createRefusal("dependencies",
			fmt.Sprintf("`dependencies` carries %d edges; the limit is %d per request", len(*wire), maxCreateDependencies))
	}
	edges := make([]issueops.CreateDependency, 0, len(*wire))
	for i, edge := range *wire {
		if offender, unknown := unknownMember(rawEdgeAt(rawEdges, i), createDependencyMembers); unknown {
			return nil, applyUnknownMember(createDependencyParam(i, ""), offender, createDependencyMembers)
		}
		if edge.TargetId == "" {
			return nil, createRefusal(createDependencyParam(i, "target_id"), "`target_id` is required and must not be empty")
		}
		if res := applyBoundedText(createDependencyParam(i, ""), "target_id", &edge.TargetId); res != nil {
			return nil, res
		}
		// A value at all, and one the column holds — never membership of a
		// known-types list. The edge vocabulary is OPEN, as EdgeReadRequest.Types
		// says, so a workspace's own type passes and only an unstorable one is
		// refused.
		if !types.DependencyType(edge.Type).IsValid() {
			return nil, createRefusal(createDependencyParam(i, "type"),
				fmt.Sprintf("`type` must be 1 to %d characters", types.MaxDependencyTypeLen))
		}
		created := issueops.CreateDependency{
			TargetID: edge.TargetId,
			Type:     types.DependencyType(edge.Type),
			Reverse:  derefBool(edge.Reverse),
		}
		// The blob travels as the bytes the caller sent, applyDepAddItem's rule
		// unchanged: the role is the single definition of what it will accept.
		if raw := rawEdgeMember(rawEdges, i, "metadata"); raw != nil {
			created.Metadata = string(raw)
		}
		// ThreadID stays zero: CreateDependency carries one, this document
		// publishes it on no operation, and a discussion-thread id nothing on
		// this surface can read back is write-only state.
		edges = append(edges, created)
	}
	return edges, nil
}

// createWaitsFor validates `waits_for` and projects it onto the role's typed
// gate. The gate VOCABULARY is the role's — an unknown one is its ErrValidation
// reaching the wire — so only the schema's own bounds are applied here.
func createWaitsFor(raw map[string]json.RawMessage, wire *apigen.CreateIssueWaitsFor) (*issueops.WaitsFor, *Result) {
	if raw == nil || wire == nil {
		return nil, nil
	}
	if offender, unknown := unknownMember(raw, createWaitsForMembers); unknown {
		return nil, applyUnknownMember("waits_for.", offender, createWaitsForMembers)
	}
	if wire.SpawnerId == "" {
		return nil, createRefusal("waits_for.spawner_id", "`spawner_id` is required and must not be empty")
	}
	for _, bounded := range []struct {
		member string
		value  *string
	}{{"spawner_id", &wire.SpawnerId}, {"gate", wire.Gate}} {
		if res := applyBoundedText("waits_for.", bounded.member, bounded.value); res != nil {
			return nil, res
		}
	}
	return &issueops.WaitsFor{SpawnerID: wire.SpawnerId, Gate: derefString(wire.Gate)}, nil
}

// failCreateIssue answers a refused create, mapping the role's TYPED refusals
// onto the frozen codes.
//
// THERE IS NO 404 BRANCH, and its absence is this operation's posture rather
// than an oversight. A dependency, parent or waits-for target that names
// nothing arrives as ErrValidation WRAPPING ErrNotFound, and it is a statement
// about the request body rather than about a resource this operation was asked
// to address — there is no id in the path to have missed. That is
// batchCreateIssues' argument and addDependencies', applied to a single create.
// The ErrValidation check therefore has to run before any ErrNotFound reading,
// exactly as failBatchCreate's does.
//
// NOTHING HERE QUOTES THE ROLE'S MESSAGE. A refusal from the workspace's own
// vocabulary arrives as prose about statuses and types, and a refused edge
// arrives as a driver error naming tables and constraints; 4xx details on this
// surface reflect the caller's own input back rather than server internals. The
// real error goes to the log with the request id.
func (s *Server) failCreateIssue(w http.ResponseWriter, r *http.Request, err error) {
	var hierarchy *issueops.DependencyHierarchyConflictError
	// The 409 constructors take no param, and the document promises one on
	// every problem but the body that failed to parse, so the conflicts name
	// the member that earned them here.
	named := func(res Result, param string) Result {
		res.Problem.Param = &param
		return res
	}

	switch {
	// An occupied explicit id is a 409: the body is well-formed and stays
	// well-formed, and what refuses it is STATE the client cannot see without
	// reading it — so recovery is to look at that state (adopt the row with
	// PATCH, pick another id, or stop) rather than to fix a malformed request.
	// The identical body succeeded before the id was taken. Matched before
	// ErrValidation because the create path can wrap both.
	case errors.Is(err, storage.ErrAlreadyExists):
		s.refusedCreateIssue(r, err)
		s.fail(w, r, named(newResult(CodeAlreadyExists,
			"`id` already names a stored row; nothing was created"), "id"))

	// The hierarchy refusal and the plain scheduling cycle share one code, and
	// the three extension members are what tells them apart — presence is the
	// discriminator, so the hierarchy arm must be matched first.
	case errors.As(err, &hierarchy):
		s.refusedCreateIssue(r, err)
		s.fail(w, r, named(newResult(CodeDependencyCycle,
			"a blocking edge against the new issue's own ancestor or descendant would never clear").
			WithHierarchyConflict(hierarchy.IssueID, hierarchy.BlockerID, hierarchy.BlockerIsAncestor), "dependencies"))

	case errors.Is(err, issueops.ErrDependencyCycle):
		s.refusedCreateIssue(r, err)
		s.fail(w, r, named(newResult(CodeDependencyCycle,
			"the requested edges would create a dependency cycle; nothing was created"), "dependencies"))

	case !errors.Is(err, storage.ErrValidation):
		s.failErr(w, r, err)

	// The 4xx path does not log by default, so the branches below are the one
	// place the real refusal is recorded for the operator.
	case errors.Is(err, storage.ErrNotFound):
		s.refusedCreateIssue(r, err)
		s.fail(w, r, InvalidArgument("dependencies", ReasonInvalidValue,
			"a dependency, parent or waits-for target names no issue in this workspace; nothing was created"))

	case errors.Is(err, issueops.ErrSelfDependency):
		s.refusedCreateIssue(r, err)
		s.fail(w, r, InvalidArgument("dependencies", ReasonInvalidValue,
			"an issue cannot depend on itself; nothing was created"))

	case errors.Is(err, issueops.ErrPrefixMismatch):
		s.refusedCreateIssue(r, err)
		s.fail(w, r, InvalidArgument("id", ReasonInvalidValue,
			"`id` is outside this workspace's configured issue prefix; send `force_id_prefix` to create it anyway"))

	default:
		s.refusedCreateIssue(r, err)
		// No `param`: the remaining refusals are the workspace's own
		// vocabularies, which can name a member this request does not carry.
		// deleteIssues draws the same line for the same reason.
		s.fail(w, r, InvalidArgument("", ReasonInvalidValue,
			"the request was refused by this workspace's own validation; nothing was created"))
	}
}

// refusedCreateIssue records the real refusal for the operator, since the 4xx
// path does not log by default and the response carries the server's own words.
func (s *Server) refusedCreateIssue(r *http.Request, err error) {
	s.event("request_refused", "request_id", requestInfo(r.Context()).id, "error", err.Error())
}

// createRefusal is the 400 this body's levels raise, with `param` already
// spelled the way the client reads it back.
func createRefusal(param, detail string) *Result {
	res := InvalidArgument(param, ReasonInvalidValue, detail)
	return &res
}

// createDependencyParam spells the `param` member for a refusal inside
// `dependencies`, so a client dispatching on it learns WHICH edge and WHICH
// member. An empty member names the edge itself, and the trailing dot is what
// the nested unknown-member refusal appends its offender to.
func createDependencyParam(index int, member string) string {
	return fmt.Sprintf("dependencies[%d].%s", index, member)
}

// rawEdgeMember reads one member off a decoded edge, or nil when the edge or
// the member is absent.
func rawEdgeMember(edges []map[string]json.RawMessage, index int, member string) json.RawMessage {
	edge := rawEdgeAt(edges, index)
	if edge == nil {
		return nil
	}
	raw, ok := edge[member]
	if !ok {
		return nil
	}
	return raw
}

// rawObject re-encodes a decoded object so the generated struct can be
// unmarshaled from the same bytes the raw member check ran over. The two
// decodes have to see one document, and re-encoding the members is cheaper than
// buffering the body twice.
func rawObject(members map[string]json.RawMessage) []byte {
	encoded, err := json.Marshal(members)
	if err != nil {
		// json.RawMessage values came out of a successful decode, so this
		// cannot fail; `null` keeps the typed decode's own refusal honest.
		return []byte("null")
	}
	return encoded
}

// derefBool reads an optional boolean member, defaulting to false — the
// document's default for every flag on this surface.
func derefBool(value *bool) bool {
	return value != nil && *value
}
