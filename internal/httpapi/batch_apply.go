package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/jonbaldie/beads/issueops"
)

// The wire adapter over issueops.BatchApplier: an ORDERED, heterogeneous plan
// applied as one transaction or not at all.
//
// It carries the claim's posture verbatim. The actor is caller-ASSERTED
// provenance for the audit trail and not authenticated identity; hooks do not
// fire and the per-command auto-commit machinery does not run. The only durable
// effect is the single storage commit the role makes inside its own
// transaction.
//
// EVERYTHING ABOVE THE ROLE IS ARGUMENT VALIDATION. What the values MEAN — how
// a key resolves and which way it may reach, whether a precondition holds
// as-modified, whether a waits-for gate is one of the two, what the graph the
// whole request built permits — belongs to issueops.BatchApplier. This file
// decodes a body four levels deep, refuses the shapes the document refuses, and
// maps the role's TYPED refusals onto the frozen codes.

const (
	// maxApplyBatchItems is the document's cap on `items`, and the role's own
	// bound restated at the edge so an over-long request costs no database work.
	// It bounds how long one request may hold a write transaction — not batch
	// semantics, which have no size in them.
	maxApplyBatchItems = issueops.MaxApplyBatchItems
	// maxApplyBatchBodyBytes bounds the request body. A hundred items each
	// carrying a description, a design, acceptance criteria and a metadata
	// document is the shape this has to admit, so it refuses the absurd before
	// any of it is parsed. It is the batch create's bound for the same reason.
	maxApplyBatchBodyBytes = 4 << 20
)

// The document's member list at each of this body's levels. Every schema is
// additionalProperties: false, so anything else is refused BY NAME — which is
// why each level is decoded as raw members first, and why the levels below the
// request are checked in the same pass that projects them.
//
// PRESENCE IS THE SIGNAL at three of these levels and that is the second reason
// for the raw decode: an item's payload members carry the tagged union's
// disagreement cases, a patch member present is written where an absent one is
// untouched, and a metadata member present holding `null` is a value.
var (
	applyBatchRequestMembers = []string{"actor", "force_id_prefix", "items", "provenance", "skip_per_edge_cycle_check"}
	applyItemMembers         = []string{"close", "create", "dep_add", "kind", "update"}
	applyRefMembers          = []string{"id", "key"}
	applyCreateItemMembers   = []string{
		"acceptance_criteria", "assignee", "defer_until", "description", "design",
		"due_at", "ephemeral", "estimated_minutes", "external_ref", "id",
		"issue_type", "key", "labels", "metadata", "metadata_refs", "no_history",
		"notes", "owner", "priority", "sender", "status", "title",
	}
	applyUpdateItemMembers = []string{
		"expected_assignee", "expected_status", "expected_version",
		"force_assignee_transfer", "force_close_policy", "patch", "target",
	}
	applyPatchMembers = []string{
		"acceptance_criteria", "append_notes", "assignee", "defer_until",
		"description", "design", "due_at", "estimated_minutes", "external_ref",
		"issue_type", "labels", "metadata", "notes", "owner", "priority",
		"status", "title",
	}
	applyLabelPatchMembers    = []string{"add", "remove", "replace"}
	applyMetadataPatchMembers = []string{"merge", "replace", "set", "unset"}
	applyCloseItemMembers     = []string{"expected_version", "force", "reason", "session", "target"}
	applyDepAddItemMembers    = []string{"metadata", "source", "target", "type"}

	// applyItemKinds is the tag vocabulary, paired with the payload member each
	// value names. It is one map rather than a switch so the enum, the member
	// names and the agreement check cannot drift into three opinions.
	applyItemKinds = map[issueops.ItemKind]string{
		issueops.ItemCreate: "create",
		issueops.ItemUpdate: "update",
		issueops.ItemClose:  "close",
		issueops.ItemDepAdd: "dep_add",
	}

	// applyNullablePatchMembers is the closed set on which explicit `null`
	// CLEARS rather than refuses, exactly as it is for PATCH
	// /v0/beads/issues/{id}: they are the members the role models as Field[*T],
	// because a pointer is the only thing a clear has to write.
	applyNullablePatchMembers = map[string]bool{
		"estimated_minutes": true,
		"external_ref":      true,
		"due_at":            true,
		"defer_until":       true,
	}
)

// handleApplyBatch applies every item in the request body in order, or applies
// none of them.
//
// The transaction boundary, the ref resolution, the as-modified preconditions,
// the close policy, the assignee fence, the metadata splice and the end gate
// all belong to issueops.BatchApplier.
func (s *Server) handleApplyBatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireNoQuery(w, r) {
		return
	}
	if !s.requireJSONContent(w, r) {
		return
	}
	request, ok := s.applyBatchRequest(w, r)
	if !ok {
		return
	}

	applier, err := s.batchApplier(r)
	if err != nil {
		s.failApplyBatch(w, r, request, err)
		return
	}
	result, err := applier.ApplyBatch(r.Context(), request)
	if err != nil {
		s.failApplyBatch(w, r, request, err)
		return
	}
	writeJSON(w, applyBatchResponse(result))
}

// applyBatchRequest decodes and validates the body, and reports whether the
// request may proceed. Every refusal here happens BEFORE any database work,
// which is what lets these 400s reflect the caller's own input back.
func (s *Server) applyBatchRequest(w http.ResponseWriter, r *http.Request) (issueops.ApplyBatchRequest, bool) {
	refuse := func(res *Result) (issueops.ApplyBatchRequest, bool) {
		s.fail(w, r, *res)
		return issueops.ApplyBatchRequest{}, false
	}

	members, res := decodeJSONObject(w, r, maxApplyBatchBodyBytes)
	if res != nil {
		return refuse(res)
	}
	if offender, unknown := unknownMember(members, applyBatchRequestMembers); unknown {
		s.failUnknownMember(w, r, offender, applyBatchRequestMembers)
		return issueops.ApplyBatchRequest{}, false
	}

	actor, ok := s.bodyActor(w, r, members)
	if !ok {
		return issueops.ApplyBatchRequest{}, false
	}
	provenance, res := applyTextMember(members, "", "provenance")
	if res != nil {
		return refuse(res)
	}
	forceIDPrefix, res := applyBoolMember(members, "", "force_id_prefix")
	if res != nil {
		return refuse(res)
	}
	skipPerEdge, res := applyBoolMember(members, "", "skip_per_edge_cycle_check")
	if res != nil {
		return refuse(res)
	}
	items, res := applyItems(members)
	if res != nil {
		return refuse(res)
	}
	return issueops.ApplyBatchRequest{
		Actor:                 actor,
		Items:                 items,
		Provenance:            provenance,
		ForceIDPrefix:         forceIDPrefix,
		SkipPerEdgeCycleCheck: skipPerEdge,
	}, true
}

// applyItems validates `items` and projects it onto the role's items, in
// request order — which this operation never changes.
func applyItems(members map[string]json.RawMessage) ([]issueops.ApplyItem, *Result) {
	refuse := func(detail string) *Result {
		res := InvalidArgument("items", ReasonInvalidValue, detail)
		return &res
	}
	raw, ok := members["items"]
	if !ok {
		return nil, refuse("`items` is required")
	}
	var rawItems []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawItems); err != nil || rawItems == nil {
		return nil, refuse("`items` must be an array of objects")
	}
	switch {
	case len(rawItems) == 0:
		return nil, refuse("`items` must carry at least one item; a request that writes nothing is refused rather than answered")
	case len(rawItems) > maxApplyBatchItems:
		return nil, refuse(fmt.Sprintf("`items` carries %d items; the limit is %d per request", len(rawItems), maxApplyBatchItems))
	}

	items := make([]issueops.ApplyItem, 0, len(rawItems))
	for i, rawItem := range rawItems {
		item, res := applyItem(i, rawItem)
		if res != nil {
			return nil, res
		}
		items = append(items, item)
	}
	return items, nil
}

// applyItem projects one tagged item onto the role's item.
//
// IT ENFORCES THE TAG BY HAND, and that is the cost of the document's own
// doctrine rather than an oversight here. The item is a single-shape object
// with a required `kind` and four optional payload members, because a schema
// alternation would need a composition keyword this document does not use — so
// nothing in the generated type stops a client sending two payloads, or a
// payload the kind does not name. Both are answered here, before the role sees
// a request it would have to refuse for the same reason.
func applyItem(index int, raw map[string]json.RawMessage) (issueops.ApplyItem, *Result) {
	prefix := applyItemParam(index, "")
	if raw == nil {
		res := InvalidArgument(prefix, ReasonInvalidValue, "an item must be a JSON object")
		return issueops.ApplyItem{}, &res
	}
	if offender, unknown := unknownMember(raw, applyItemMembers); unknown {
		return issueops.ApplyItem{}, applyUnknownMember(prefix+".", offender, applyItemMembers)
	}

	kindText, res := applyRequiredText(raw, prefix+".", "kind")
	if res != nil {
		return issueops.ApplyItem{}, res
	}
	kind := issueops.ItemKind(kindText)
	named, known := applyItemKinds[kind]
	if !known {
		res := InvalidArgument(prefix+".kind", ReasonInvalidValue,
			"`kind` must be one of "+strings.Join(applyKindNames(), ", "))
		return issueops.ApplyItem{}, &res
	}

	present := applyItemPayloadMembers(raw)
	if res := validateApplyItemPayload(prefix, kindText, named, present); res != nil {
		return issueops.ApplyItem{}, res
	}

	payload, res := applyObjectMember(raw, prefix+".", named)
	if res != nil {
		return issueops.ApplyItem{}, res
	}
	return projectApplyItemPayload(issueops.ApplyItem{Kind: kind}, kind, prefix, named, raw, payload)
}

func applyItemPayloadMembers(raw map[string]json.RawMessage) []string {
	var present []string
	for _, member := range []string{"create", "update", "close", "dep_add"} {
		if _, ok := raw[member]; ok {
			present = append(present, member)
		}
	}
	return present
}

func validateApplyItemPayload(prefix string, kindText string, named string, present []string) *Result {
	// The tag and the payloads have to agree in both directions: a kind with no
	// payload is an item that does nothing, and a payload the kind does not name
	// is an item whose two halves disagree.
	switch {
	case len(present) == 0:
		res := InvalidArgument(prefix+"."+named, ReasonInvalidValue,
			"an item of kind `"+kindText+"` must carry its `"+named+"` payload")
		return &res
	case len(present) > 1:
		res := InvalidArgument(prefix+"."+present[1], ReasonInvalidValue,
			"an item carries exactly one payload; this one carries "+strings.Join(present, " and "))
		return &res
	case present[0] != named:
		res := InvalidArgument(prefix+"."+present[0], ReasonInvalidValue,
			"this item is kind `"+kindText+"` but carries the `"+present[0]+"` payload; the two must name the same verb")
		return &res
	}
	return nil
}

func projectApplyItemPayload(item issueops.ApplyItem, kind issueops.ItemKind, prefix string, named string, raw map[string]json.RawMessage, payload map[string]json.RawMessage) (issueops.ApplyItem, *Result) {
	payloadPrefix := prefix + "." + named + "."
	switch kind {
	case issueops.ItemCreate:
		create, res := applyCreateItem(payloadPrefix, raw[named], payload)
		if res != nil {
			return issueops.ApplyItem{}, res
		}
		item.Create = create
	case issueops.ItemUpdate:
		update, res := applyUpdateItem(payloadPrefix, raw[named], payload)
		if res != nil {
			return issueops.ApplyItem{}, res
		}
		item.Update = update
	case issueops.ItemClose:
		closeItem, res := applyCloseItem(payloadPrefix, payload)
		if res != nil {
			return issueops.ApplyItem{}, res
		}
		item.Close = closeItem
	case issueops.ItemDepAdd:
		depAdd, res := applyDepAddItem(payloadPrefix, payload)
		if res != nil {
			return issueops.ApplyItem{}, res
		}
		item.DepAdd = depAdd
	}
	return item, nil
}
