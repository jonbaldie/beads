package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/issueops"
)

// failApplyBatch answers a refused plan, mapping the role's TYPED refusals onto
// the frozen codes and naming the item each one came from.
//
// EVERY BRANCH READS THE ROLE'S TYPED FIELDS, never its prose. That matters more
// here than anywhere else on the surface: the request is all or nothing, so
// there is no per-item result array a client could find the offender in, and
// the `item_*` problem members are the only place it exists. They come from
// *issueops.ItemError, raised inside the transaction that refused.
//
// Nothing here quotes a role message. 4xx details on this surface reflect the
// caller's own input back rather than server internals, and the real error goes
// to the log with the request id.
func (s *Server) failApplyBatch(w http.ResponseWriter, r *http.Request, request issueops.ApplyBatchRequest, err error) {
	failure := classifyApplyBatchFailure(err)
	if s.failApplyBatchDependencyFailure(w, r, request, err, failure) {
		return
	}
	if s.failApplyBatchStateFailure(w, r, request, err, failure) {
		return
	}
	s.failErr(w, r, err)
}

type applyBatchFailure struct {
	itemErr    *issueops.ItemError
	dependency applyBatchDependencyFailure
	state      applyBatchState
}

type applyBatchDependencyFailure struct {
	refErr          *issueops.RefError
	typeConflict    *issueops.DependencyTypeConflictError
	hierarchy       *issueops.DependencyHierarchyConflictError
	endpoint        *issueops.DependencyEndpointNotFoundError
	dependencyCycle bool
	selfDependency  bool
}

type applyBatchState struct {
	openChildren   *issueops.CloseOpenChildrenError
	claimed        *issueops.ClaimConflictError
	alreadyExists  bool
	closeOpen      bool
	closeBlocked   bool
	alreadyClaimed bool
	precondition   bool
	notFound       bool
	validation     bool
}

func classifyApplyBatchFailure(err error) applyBatchFailure {
	var failure applyBatchFailure
	errors.As(err, &failure.itemErr)
	errors.As(err, &failure.dependency.refErr)
	errors.As(err, &failure.dependency.typeConflict)
	errors.As(err, &failure.dependency.hierarchy)
	errors.As(err, &failure.dependency.endpoint)
	errors.As(err, &failure.state.openChildren)
	errors.As(err, &failure.state.claimed)
	failure.dependency.dependencyCycle = errors.Is(err, issueops.ErrDependencyCycle)
	failure.dependency.selfDependency = errors.Is(err, issueops.ErrSelfDependency)
	failure.state.alreadyExists = errors.Is(err, storage.ErrAlreadyExists)
	failure.state.closeOpen = errors.Is(err, issueops.ErrCloseOpenChildren)
	failure.state.closeBlocked = errors.Is(err, issueops.ErrCloseBlocked)
	failure.state.alreadyClaimed = errors.Is(err, storage.ErrAlreadyClaimed)
	failure.state.precondition = errors.Is(err, issueops.ErrVersionMismatch) ||
		errors.Is(err, issueops.ErrStatusMismatch) ||
		errors.Is(err, issueops.ErrAssigneeMismatch)
	failure.state.notFound = errors.Is(err, storage.ErrNotFound)
	failure.state.validation = errors.Is(err, storage.ErrValidation)
	return failure
}

func applyBatchResultAt(res Result, itemErr *issueops.ItemError, member string) Result {
	if itemErr == nil {
		// A refusal the role raised without naming an item — the request's
		// own, or a wrapper that lost the identity. `param` still names the
		// member that carries them all, because the document promises one on
		// every 400 but the body that failed to parse.
		if member != "" && res.Problem.Param == nil {
			param := "items"
			res.Problem.Param = &param
		}
		return res
	}
	res = res.WithBatchItem(itemErr.Index, string(itemErr.Kind), itemErr.Key, itemErr.IssueID)
	if member != "" {
		param := applyItemParam(itemErr.Index, string(itemErr.Kind)+"."+member)
		res.Problem.Param = &param
	}
	return res
}

func (s *Server) failApplyBatchDependencyFailure(w http.ResponseWriter, r *http.Request, request issueops.ApplyBatchRequest, err error, failure applyBatchFailure) bool {
	switch {
	// An unresolvable ref is a *RefError and it is the one 400 that carries a
	// discriminator: `declared_later` tells an ORDERING mistake from a typo, and
	// it is emitted in both polarities so an absent member cannot be misread as
	// false. It is matched before ErrValidation because it unwraps to it.
	case failure.dependency.refErr != nil:
		s.refusedApplyBatch(r, err)
		s.fail(w, r, InvalidArgument(applyRefParam(request, failure.dependency.refErr), ReasonInvalidValue,
			applyRefDetail(failure.dependency.refErr)).
			WithBatchItem(failure.dependency.refErr.Index, applyKindAt(request, failure.dependency.refErr.Index), failure.dependency.refErr.Key, "").
			WithDeclaredLater(failure.dependency.refErr.DeclaredLater))

	case failure.dependency.typeConflict != nil:
		s.fail(w, r, applyBatchResultAt(newResult(CodeDependencyExists,
			"this pair already carries an edge of a different type; remove it before re-adding").
			WithDependencyTypeConflict(failure.dependency.typeConflict.ExistingType, failure.dependency.typeConflict.RequestedType), failure.itemErr, ""))

	case failure.dependency.hierarchy != nil:
		s.fail(w, r, applyBatchResultAt(newResult(CodeDependencyCycle,
			"a blocking edge against the issue's own ancestor or descendant would never clear").
			WithHierarchyConflict(failure.dependency.hierarchy.IssueID, failure.dependency.hierarchy.BlockerID, failure.dependency.hierarchy.BlockerIsAncestor), failure.itemErr, ""))

	case failure.dependency.dependencyCycle:
		// No hierarchy members: this is the plain scheduling cycle, and their
		// ABSENCE is what tells a client which of the two refusals it got. It may
		// come from the per-edge probe or from the END GATE, which is the only
		// place an edge that is legal alone and illegal in the graph this request
		// built is caught.
		s.fail(w, r, applyBatchResultAt(newResult(CodeDependencyCycle,
			"the plan's edges would create a dependency cycle; nothing was written"), failure.itemErr, ""))

	// The edge existence refusals are 400s rather than 404s, conforming to
	// POST /v0/beads/dependencies:add: an edge describes a relation rather than
	// acting on a row, and its target may legitimately be an "external:"
	// reference this database holds nothing for.
	case failure.dependency.endpoint != nil:
		s.refusedApplyBatch(r, err)
		member, detail := "source", "an edge's source names no issue in this workspace; nothing was written"
		if errors.Is(err, issueops.ErrDependencyTargetNotFound) {
			member, detail = "target", "an edge's target names no issue this workspace can see; nothing was written"
		}
		s.fail(w, r, applyBatchResultAt(InvalidArgument("", ReasonInvalidValue, detail), failure.itemErr, member))

	case failure.dependency.selfDependency:
		s.refusedApplyBatch(r, err)
		s.fail(w, r, applyBatchResultAt(InvalidArgument("", ReasonInvalidValue, "an issue cannot depend on itself"), failure.itemErr, "target"))

	default:
		return false
	}
	return true
}

func (s *Server) failApplyBatchStateFailure(w http.ResponseWriter, r *http.Request, request issueops.ApplyBatchRequest, err error, failure applyBatchFailure) bool {
	if s.failApplyBatchStateConflict(w, r, err, failure) {
		return true
	}
	return s.failApplyBatchStateGuardOrLookup(w, r, request, err, failure)
}

func (s *Server) failApplyBatchStateConflict(w http.ResponseWriter, r *http.Request, err error, failure applyBatchFailure) bool {
	switch {
	// An occupied explicit id is a 409: the body is well-formed and stays
	// well-formed, and what refuses it is STATE the client cannot see without
	// reading it — so recovery is to look at that state (adopt the row, pick
	// another id, or stop) rather than to fix a malformed request. The identical
	// body succeeded before the id was taken. Matched before ErrValidation
	// because the create path wraps both.
	case failure.state.alreadyExists:
		s.refusedApplyBatch(r, err)
		s.fail(w, r, applyBatchResultAt(newResult(CodeAlreadyExists,
			"a create item's `id` already names a stored row; nothing was written"), failure.itemErr, "id"))

	case failure.state.closeOpen:
		res := applyBatchResultAt(newResult(CodeNotClosable,
			"an item closes an issue with open children; close them first, or send the item's force flag"), failure.itemErr, "")
		if failure.state.openChildren != nil {
			res = res.WithOpenChildren(failure.state.openChildren.OpenChildren)
		}
		s.fail(w, r, res)

	case failure.state.closeBlocked:
		s.fail(w, r, applyBatchResultAt(newResult(CodeNotClosable,
			"an item closes a blocked issue; clear the blocker, or send the item's force flag"), failure.itemErr, ""))

	case failure.state.alreadyClaimed:
		res := applyBatchResultAt(newResult(CodeAlreadyClaimed,
			"an update transfers work away from a live foreign owner; send `force_assignee_transfer`, or guard with `expected_assignee`"), failure.itemErr, "assignee")
		if failure.state.claimed != nil {
			if failure.state.claimed.Assignee != "" {
				res = res.WithAssignee(failure.state.claimed.Assignee)
			}
			if failure.state.claimed.Status != "" {
				res = res.WithIssueStatus(string(failure.state.claimed.Status))
			}
		}
		s.fail(w, r, res)

	default:
		return false
	}
	return true
}

func (s *Server) failApplyBatchStateGuardOrLookup(w http.ResponseWriter, r *http.Request, request issueops.ApplyBatchRequest, err error, failure applyBatchFailure) bool {
	switch {
	case failure.state.precondition:
		s.fail(w, r, applyPreconditionResult(request, failure.itemErr, err,
			func(res Result, member string) Result {
				return applyBatchResultAt(res, failure.itemErr, member)
			}))

	// A target an update or a close NAMED is a resource this request failed to
	// address, which is POST /v0/beads/issues:delete's 404 rather than the edge
	// refusal's 400 above.
	case failure.state.notFound:
		s.fail(w, r, applyBatchResultAt(NotFound(), failure.itemErr, ""))

	case failure.state.validation:
		s.refusedApplyBatch(r, err)
		s.fail(w, r, applyBatchResultAt(InvalidArgument("items", ReasonInvalidValue,
			"an item was refused by this workspace's own validation; nothing was written"), failure.itemErr, ""))

	default:
		return false
	}
	return true
}

// applyPreconditionResult builds the 409 for a guard that missed, naming the
// guard member and echoing what the request asked for.
//
// The expected value comes from the REQUEST rather than from a read, and the
// observed value is absent, because this operation's refusal rolled its
// transaction back: a read afterwards would describe a row the refusal never
// saw. See PreconditionFailed.
func applyPreconditionResult(request issueops.ApplyBatchRequest, itemErr *issueops.ItemError, err error, at func(Result, string) Result) Result {
	res := PreconditionFailed()
	switch {
	case errors.Is(err, issueops.ErrVersionMismatch):
		res = at(res, "expected_version")
		if expected := applyExpectedVersion(request, itemErr); expected != nil {
			res = res.WithExpectedVersion(*expected)
		}
	case errors.Is(err, issueops.ErrStatusMismatch):
		res = at(res, "expected_status")
		if item := applyUpdateAt(request, itemErr); item != nil && item.ExpectedStatus != nil {
			res = res.WithExpectedStatus(string(*item.ExpectedStatus))
		}
	default:
		res = at(res, "expected_assignee")
		if item := applyUpdateAt(request, itemErr); item != nil && item.ExpectedAssignee != nil {
			res = res.WithExpectedAssignee(*item.ExpectedAssignee)
		}
	}
	return res
}

// applyExpectedVersion reads the version guard off the refused item. Both an
// update and a close carry one, which is why this is not applyUpdateAt's caller.
func applyExpectedVersion(request issueops.ApplyBatchRequest, itemErr *issueops.ItemError) *int64 {
	item := applyItemAt(request, itemErr)
	switch {
	case item == nil:
		return nil
	case item.Update != nil:
		return item.Update.ExpectedVersion
	case item.Close != nil:
		return item.Close.ExpectedVersion
	}
	return nil
}

// applyUpdateAt reads the refused item's update payload, or nil when the
// refusal named no item or named one of another kind.
func applyUpdateAt(request issueops.ApplyBatchRequest, itemErr *issueops.ItemError) *issueops.UpdateItem {
	if item := applyItemAt(request, itemErr); item != nil {
		return item.Update
	}
	return nil
}

// applyItemAt reads the refused item out of the request the handler built.
//
// It is bounds-checked rather than trusted: the index comes from the role, and
// a server that indexed a slice on a number another package computed would turn
// a contract drift into a panic on a live request.
func applyItemAt(request issueops.ApplyBatchRequest, itemErr *issueops.ItemError) *issueops.ApplyItem {
	if itemErr == nil || itemErr.Index < 0 || itemErr.Index >= len(request.Items) {
		return nil
	}
	return &request.Items[itemErr.Index]
}

// applyKindAt names the kind of the item at index, for a refusal that carries
// an index but no kind of its own.
func applyKindAt(request issueops.ApplyBatchRequest, index int) string {
	if index < 0 || index >= len(request.Items) {
		return ""
	}
	return string(request.Items[index].Kind)
}

// applyRefParam spells `param` for an unresolvable ref.
//
// RefError.Member is DIAGNOSTIC PROSE — "target", "source", or "metadata_ref
// <key>" — rather than a vocabulary, so it is mapped onto the document's own
// member names rather than published. Anything that is not one of the two
// addressing refs is a metadata ref, and the whole member is named because the
// key inside it came from the caller's own object.
func applyRefParam(request issueops.ApplyBatchRequest, refErr *issueops.RefError) string {
	kind := applyKindAt(request, refErr.Index)
	if kind == "" {
		return applyItemParam(refErr.Index, "")
	}
	member := "metadata_refs"
	if refErr.Member == "target" || refErr.Member == "source" {
		member = refErr.Member
	}
	return applyItemParam(refErr.Index, kind+"."+member)
}

// applyRefDetail says which of the two key diagnoses this is, in the server's
// own words. The machine-readable half is `declared_later`.
func applyRefDetail(refErr *issueops.RefError) string {
	if refErr.DeclaredLater {
		return "this ref names a key declared LATER in the request; a key reaches backward only, so move the item that declares it earlier"
	}
	return "this ref names a key no item in the request declares"
}

// refusedApplyBatch records the real refusal for the operator. The 4xx path does
// not log by default, and the role's message is the only place the underlying
// reason survives once the response carries the server's own words.
func (s *Server) refusedApplyBatch(r *http.Request, err error) {
	s.event("request_refused", "request_id", requestInfo(r.Context()).id, "error", err.Error())
}

// applyItemParam spells the `param` member for a refusal inside `items`, so a
// client dispatching on it learns WHICH item and WHICH member.
func applyItemParam(index int, member string) string {
	param := fmt.Sprintf("items[%d]", index)
	if member == "" {
		return param
	}
	return param + "." + member
}

// applyParam joins a level's dotted prefix to a member, and names the LEVEL
// itself when the member is empty — a refusal about the whole object rather
// than about one of its members. Without the trim that case would spell a
// `param` ending in a dot, which is a member name no schema declares.
func applyParam(prefix, member string) string {
	if member == "" {
		return strings.TrimSuffix(prefix, ".")
	}
	return prefix + member
}

// applyUnknownMember answers an unknown member below the request level, where
// the offender's name has to be qualified by the path that reached it.
func applyUnknownMember(prefix, offender string, allowed []string) *Result {
	res := InvalidArgument(prefix+offender, ReasonUnknownParameter,
		"this member carries "+strings.Join(allowed, ", ")+" and nothing else")
	return &res
}

// applyKindNames lists the tag vocabulary in the document's order, for a
// refusal that has to spell it.
func applyKindNames() []string {
	return []string{"create", "update", "close", "dep_add"}
}

// applyObjectMember reads a required member that must be a JSON object, as raw
// members so the level below it can be checked by name.
func applyObjectMember(raw map[string]json.RawMessage, prefix, member string) (map[string]json.RawMessage, *Result) {
	encoded, ok := raw[member]
	if !ok {
		res := InvalidArgument(prefix+member, ReasonInvalidValue, "`"+member+"` is required")
		return nil, &res
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &out); err != nil || out == nil {
		res := InvalidArgument(prefix+member, ReasonInvalidValue, "`"+member+"` must be a JSON object")
		return nil, &res
	}
	return out, nil
}

// applyRequiredText reads a required string member, bounded the way every
// stored string on this surface is.
func applyRequiredText(raw map[string]json.RawMessage, prefix, member string) (string, *Result) {
	if _, ok := raw[member]; !ok {
		res := InvalidArgument(prefix+member, ReasonInvalidValue, "`"+member+"` is required")
		return "", &res
	}
	value, res := applyTextMember(raw, prefix, member)
	if res != nil {
		return "", res
	}
	if value == "" {
		res := InvalidArgument(prefix+member, ReasonInvalidValue, "`"+member+"` must not be empty")
		return "", &res
	}
	return value, nil
}

// applyTextMember, applyBoolMember and applyVersionGuardMember are the pure twins of
// Server.storedTextMember and Server.booleanMember, with their rules unchanged:
// an absent member is the zero value the role reads as "not supplied", an
// explicit `null` is a 400 naming the member rather than a third state, and a
// string is bounded by what storage holds and refused for control characters —
// because these values land in columns that renderers print.
//
// They are functions rather than methods because this body nests four levels
// deep, and threading a ResponseWriter through every level so each could fail
// in place would put the response machinery in the middle of the projection.
func applyTextMember(raw map[string]json.RawMessage, prefix, member string) (string, *Result) {
	encoded, ok := raw[member]
	if !ok {
		return "", nil
	}
	var value *string
	if err := json.Unmarshal(encoded, &value); err != nil || value == nil {
		res := InvalidArgument(prefix+member, ReasonInvalidValue, "`"+member+"` must be a string")
		return "", &res
	}
	if res := applyBoundedText(prefix, member, value); res != nil {
		return "", res
	}
	return *value, nil
}

func applyBoolMember(raw map[string]json.RawMessage, prefix, member string) (bool, *Result) {
	encoded, ok := raw[member]
	if !ok {
		return false, nil
	}
	var value *bool
	if err := json.Unmarshal(encoded, &value); err != nil || value == nil {
		res := InvalidArgument(prefix+member, ReasonInvalidValue, "`"+member+"` must be a boolean")
		return false, &res
	}
	return *value, nil
}

// applyVersionGuardMember is the family's int64 reader, and it names the member
// itself rather than taking one.
//
// Its siblings above are generic because they read many members; this one has
// read exactly one on every operation that has ever published a 64-bit member —
// the row-version guard — so the member name lives in the function instead of
// at five call sites that could disagree about how to spell it. A second int64
// member would re-generalize it, which is a two-line change and not a reason to
// carry a parameter nothing varies.
//
// prefix stays, because a batch item's guard is reported qualified by the item
// that carried it where a single operation's is spelled bare.
func applyVersionGuardMember(raw map[string]json.RawMessage, prefix string) (*int64, *Result) {
	encoded, ok := raw[expectedVersionMember]
	if !ok {
		return nil, nil
	}
	var value *int64
	if err := json.Unmarshal(encoded, &value); err != nil || value == nil {
		res := InvalidArgument(prefix+expectedVersionMember, ReasonInvalidValue,
			"`"+expectedVersionMember+"` must be an integer")
		return nil, &res
	}
	return value, nil
}

// applyBoundedText applies the bounds a stored string carries wherever it is
// spelled, so every level of this body refuses the same values. A nil pointer is
// an absent member and passes.
func applyBoundedText(prefix, member string, value *string) *Result {
	if value == nil {
		return nil
	}
	refuse := func(detail string) *Result {
		res := InvalidArgument(prefix+member, ReasonInvalidValue, detail)
		return &res
	}
	switch {
	case types.CheckFieldLen(member, *value) != nil:
		return refuse(fmt.Sprintf("`%s` is %d characters; storage holds at most %d",
			member, utf8.RuneCountInString(*value), types.MaxFieldLen))
	case strings.ContainsFunc(*value, isControlChar):
		return refuse("`" + member + "` must not contain control characters")
	}
	return nil
}

// applyBoundedLabels applies the same bound to every entry of a label list.
func applyBoundedLabels(prefix, member string, labels []string) *Result {
	for i, label := range labels {
		if types.CheckFieldLen("label", label) != nil {
			res := InvalidArgument(prefix+member, ReasonInvalidValue,
				fmt.Sprintf("`%s[%d]` is %d characters; storage holds at most %d",
					member, i, utf8.RuneCountInString(label), types.MaxFieldLen))
			return &res
		}
	}
	return nil
}

// applyRawCopy copies a raw member rather than aliasing the decoded body, so
// nothing downstream can be surprised by the request buffer's lifetime.
func applyRawCopy(raw json.RawMessage) json.RawMessage {
	return json.RawMessage(append([]byte(nil), raw...))
}
