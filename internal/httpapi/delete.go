package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jonbaldie/beads/internal/httpapi/apigen"
	"github.com/jonbaldie/beads/issueops"
)

// The request body's member vocabulary. The schema is
// additionalProperties: false, so anything else is refused BY NAME: on this
// operation an ignored member is the difference between orphaning a dependent
// and deleting it.
const (
	deleteIDsMember     = "ids"
	deleteActorMember   = "actor"
	deleteCascadeMember = "cascade"
	deleteForceMember   = "force"
	deleteDryRunMember  = "dry_run"
)

// deleteMembers is the whole vocabulary, in one place, so the unknown-member
// refusal and the decoding below cannot disagree about what this operation
// accepts.
var deleteMembers = []string{
	deleteIDsMember,
	deleteActorMember,
	deleteCascadeMember,
	deleteForceMember,
	deleteDryRunMember,
	expectedVersionMember,
}

// maxDeleteIDs bounds the `ids` array, matching the document's maxItems. It
// bounds the REQUEST rather than what a cascade expands to: the whole delete is
// one transaction, so the practical ceiling is the backend's write timeout.
const maxDeleteIDs = 1000

// handleDelete answers POST /v0/beads/issues:delete — the second DESTRUCTIVE
// operation on this surface.
//
// WHAT THIS HANDLER DOES NOT DO is the point of it, as for the sweep. It does
// not resolve ids, does not expand a cascade, does not decide which rows are
// dependents, and — the one that matters — does not implement the guard that
// refuses an unforced delete over an outside dependent. All of that is
// issueops.Deleter, the same library surface `bd delete` calls. Everything
// above the role here is argument validation.
//
// NO ACTOR IS INFERRED, for the reason the claim gives. It is OPTIONAL here as
// it is on the sweep — a deleted bead leaves no row to attribute the deletion
// on — and validated by the same rules when present, because it reaches the
// same commit-message interpolation AND the surviving rows this operation
// rewrites.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireNoQuery(w, r) {
		return
	}
	if !s.requireJSONContent(w, r) {
		return
	}
	request, ok := s.deleteRequest(w, r)
	if !ok {
		return
	}

	deleter, err := s.deleter(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	result, err := deleter.Delete(r.Context(), request)
	if err != nil {
		s.failDeleteErr(w, r, request, err)
		return
	}
	writeJSON(w, deleteResponse(result))
}

// deleteRequest decodes the body into the role's request, member by member, so
// that every refusal can NAME the member it is about.
func (s *Server) deleteRequest(w http.ResponseWriter, r *http.Request) (issueops.DeleteRequest, bool) {
	members, res := decodeJSONObjectBody(w, r)
	if res != nil {
		s.fail(w, r, *res)
		return issueops.DeleteRequest{}, false
	}

	if offender, unknown := unknownMember(members, deleteMembers); unknown {
		// One offender, chosen deterministically so a client dispatching on
		// `param` never sees it depend on map order.
		requestInfo(r.Context()).refuse(offender)
		s.fail(w, r, InvalidArgument(offender, ReasonUnknownParameter,
			"this operation's request body carries "+deleteMemberList()+" and nothing else"))
		return issueops.DeleteRequest{}, false
	}

	request, res := parseDeleteRequest(members)
	if res != nil {
		s.fail(w, r, *res)
		return issueops.DeleteRequest{}, false
	}
	return request, true
}

func parseDeleteRequest(members map[string]json.RawMessage) (issueops.DeleteRequest, *Result) {
	ids, res := decodeDeleteIDs(members)
	if res != nil {
		return issueops.DeleteRequest{}, res
	}
	expectedVersion, res := deleteVersionGuard(members, ids)
	if res != nil {
		return issueops.DeleteRequest{}, res
	}
	actor, res := optionalActorMember(members)
	if res != nil {
		return issueops.DeleteRequest{}, res
	}
	flags, res := decodeDeleteFlags(members)
	if res != nil {
		return issueops.DeleteRequest{}, res
	}
	return issueops.DeleteRequest{
		IDs:             ids,
		ExpectedVersion: expectedVersion,
		Actor:           actor,
		Cascade:         flags.cascade,
		Force:           flags.force,
		DryRun:          flags.dryRun,
	}, nil
}

func decodeDeleteIDs(members map[string]json.RawMessage) ([]string, *Result) {
	raw, ok := members[deleteIDsMember]
	if !ok {
		return nil, deleteRefusal(deleteIDsMember, "`"+deleteIDsMember+"` is required")
	}
	var ids *[]string
	if err := json.Unmarshal(raw, &ids); err != nil || ids == nil {
		return nil, deleteRefusal(deleteIDsMember, "`"+deleteIDsMember+"` must be an array of strings")
	}
	if len(*ids) == 0 {
		return nil, deleteRefusal(deleteIDsMember, "`"+deleteIDsMember+"` must name at least one bead")
	}
	if len(*ids) > maxDeleteIDs {
		return nil, deleteRefusal(deleteIDsMember, "`"+deleteIDsMember+"` carries more ids than this operation accepts in one request")
	}
	for i, id := range *ids {
		if strings.TrimSpace(id) == "" {
			return nil, deleteRefusal(deleteIDsMember,
				fmt.Sprintf("`%s[%d]` is blank; every id must name a bead", deleteIDsMember, i))
		}
	}
	return *ids, nil
}

func deleteVersionGuard(members map[string]json.RawMessage, ids []string) (*int64, *Result) {
	expectedVersion, res := applyVersionGuardMember(members, "")
	if res != nil || expectedVersion == nil {
		return expectedVersion, res
	}
	if distinct := distinctDeleteIDs(ids); distinct > 1 {
		return nil, deleteRefusal(expectedVersionMember, fmt.Sprintf(
			"`%s` guards ONE bead and this request names %d distinct ids; a row version describes one row. Send one guarded request per bead",
			expectedVersionMember, distinct))
	}
	return expectedVersion, nil
}

type deleteFlags struct {
	cascade bool
	force   bool
	dryRun  bool
}

func decodeDeleteFlags(members map[string]json.RawMessage) (deleteFlags, *Result) {
	cascade, res := applyBoolMember(members, "", deleteCascadeMember)
	if res != nil {
		return deleteFlags{}, res
	}
	force, res := applyBoolMember(members, "", deleteForceMember)
	if res != nil {
		return deleteFlags{}, res
	}
	dryRun, res := applyBoolMember(members, "", deleteDryRunMember)
	if res != nil {
		return deleteFlags{}, res
	}
	return deleteFlags{cascade: cascade, force: force, dryRun: dryRun}, nil
}

func deleteRefusal(param, detail string) *Result {
	res := InvalidArgument(param, ReasonInvalidValue, detail)
	return &res
}

// distinctDeleteIDs counts the ids a request really names, collapsing
// duplicates after trimming so that the arity rule above agrees with the
// library's own normalization. See the call site for why it is spelled here.
func distinctDeleteIDs(ids []string) int {
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		seen[strings.TrimSpace(id)] = true
	}
	return len(seen)
}

func deleteMemberList() string {
	quoted := make([]string, len(deleteMembers))
	for i, name := range deleteMembers {
		quoted[i] = "`" + name + "`"
	}
	return strings.Join(quoted, ", ")
}

// failDeleteErr answers a failed delete.
//
// It draws the same ErrValidation-is-a-400 line the sweep draws, and adds two
// more arms: the row-version guard's 409, and the role's dependents refusal.
// That last one is a 400 rather than a 409 because the fix is to change the
// REQUEST — send `cascade` or `force`.
//
// THE PRECONDITION ARM IS FIRST, ABOVE THE ErrValidation LINE, and the order is
// load-bearing rather than cosmetic. ErrVersionMismatch wraps neither
// ErrValidation nor ErrNotFound, so under ClassifyError's rule it falls straight
// through failErr's default into a GENERIC 500 — for the one refusal on this
// operation that reports an irreversible act being stopped because the caller's
// view had moved. Verified by mutation: dropping the arm turns
// TestDeleteRefusesAStaleGuard into a 500, not a 400.
//
// THE ABSENT-ID REFUSAL NEEDS NO BRANCH AT ALL, and does not get one: the
// role's *NotFoundError wraps issueops.ErrNotFound, which ClassifyError already
// maps to a 404 carrying NotFound()'s FIXED detail. The role's own message
// names every id that did not resolve and the wire deliberately does not repeat
// it — NotFound's doc says why. `bd delete` still names them, because it is
// talking to the person who typed them. Its rank ABOVE the guard is the role's:
// a request that named no row has nothing to be stale about, so no branch here
// has to arrange it.
func (s *Server) failDeleteErr(w http.ResponseWriter, r *http.Request, request issueops.DeleteRequest, err error) {
	if errors.Is(err, issueops.ErrVersionMismatch) {
		s.fail(w, r, versionPreconditionResult(request.ExpectedVersion))
		return
	}
	if errors.Is(err, issueops.ErrValidation) || errors.Is(err, issueops.ErrDependentsOutsideRequest) {
		// No `param`: neither refusal is about one member of the request. The
		// dependents one is about the absence of a CHOICE between two of them.
		s.fail(w, r, InvalidArgument("", ReasonInvalidValue, err.Error()))
		return
	}
	s.failErr(w, r, err)
}

// deleteResponse projects the role's result onto the wire type. It is a field
// list rather than an alias for the reason sweepResponse is: DeleteResult is
// deliberately not x-go-type-pinned, and TestDeleteResponseCarriesEveryRoleField
// is what keeps a new result field from being dropped here in silence.
func deleteResponse(result issueops.DeleteResult) apigen.DeleteIssuesResult {
	body := apigen.DeleteIssuesResult{
		DryRun:            result.DryRun,
		Deleted:           result.Deleted,
		Dependencies:      result.Dependencies,
		Labels:            result.Labels,
		Events:            result.Events,
		ReferencesUpdated: result.ReferencesUpdated,
	}
	if len(result.Orphaned) > 0 {
		ids := append([]string(nil), result.Orphaned...)
		body.Orphaned = &ids
	}
	return body
}
