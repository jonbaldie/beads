package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jonbaldie/beads/internal/httpapi/apigen"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/issueops"
)

const (
	// updatePatchMember is the member carrying the fields to write. Nesting them
	// rather than spreading them beside `actor` keeps the patch vocabulary a
	// closed set that can grow without ever colliding with a request-level
	// member.
	updatePatchMember = "patch"
	// maxUpdateBodyBytes bounds the request body. A patch can carry a
	// description, a design and acceptance criteria at once, so the claim's
	// megabyte is too tight and the batch's four is the right order.
	maxUpdateBodyBytes = 4 << 20
)

// updateRequestMembers and issuePatchMembers are the document's member lists at
// each level, refused BY NAME for the reason every other body on this surface
// is: encoding/json's DisallowUnknownFields reports the offender only inside an
// error string.
var (
	updateRequestMembers = []string{
		"actor", "expected_assignee", "expected_status", "expected_version",
		"force_assignee_transfer", "force_close_policy", updatePatchMember,
	}
	issuePatchMembers = []string{
		"title", "description", "design", "acceptance_criteria",
		"notes", "append_notes", "priority", "issue_type", "status",
		"assignee", "parent_id", "labels", "add_labels", "remove_labels", "metadata",
		"estimated_minutes", "external_ref", "due_at", "defer_until",
	}
	// nullablePatchMembers is the closed set on which explicit `null` CLEARS
	// rather than refuses. They are exactly the members the role models as
	// Field[*T], because a pointer is the only thing a clear has to write.
	nullablePatchMembers = map[string]bool{
		"estimated_minutes": true,
		"external_ref":      true,
		"due_at":            true,
		"defer_until":       true,
	}
)

// handleUpdate edits the fields of one issue.
//
// It carries the claim's posture verbatim. The actor is caller-ASSERTED
// provenance for the audit trail and not authenticated identity; hooks do not
// fire and the per-command auto-commit machinery does not run, exactly as for
// POST /v0/beads/issues/{id}:claim. The only durable effect is the single
// storage commit the role makes inside its own transaction.
//
// A PLAIN PATCH on the issue-detail path rather than a custom method, so the id
// arrives from the router's own wildcard and there is no suffix to split. The
// id bound is this handler's, because it is not on the dispatcher's pattern.
//
// PLANES, as for close and reopen: the id resolves across both.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.updateTarget(w, r)
	if !ok {
		return
	}
	if !s.requireNoQuery(w, r) {
		return
	}
	if !s.requireJSONContent(w, r) {
		return
	}
	request, ok := s.updateRequest(w, r, id)
	if !ok {
		return
	}

	lifecycle, err := s.lifecycle(r)
	if err != nil {
		s.failUpdate(w, r, request, err)
		return
	}
	result, err := lifecycle.Update(r.Context(), request)
	if err != nil {
		s.failUpdate(w, r, request, err)
		return
	}
	// `changed` is the role's own verbatim: a same-value patch is a 200 with
	// false, not an error — idempotent, like every replay answer here.
	//
	// `revision` is the row's post-write concurrency token, read off the same
	// snapshot rather than computed. It is on the wire because `expected_version`
	// is: a guard whose token no response carries is a guard a caller cannot
	// fill. types.Issue.RowVersion is `json:"-"`, so the Issue body cannot carry
	// it and this member is where it lives.
	writeJSON(w, apigen.UpdateIssueResponse{
		Issue:    *result.Issue,
		Changed:  result.Changed,
		Revision: result.Issue.RowVersion,
	})
}

// updateTarget reads and bounds the id this operation addresses.
//
// The bound is the dispatcher's, applied here because this route is not on the
// dispatcher's pattern: an id longer than the column, or carrying a control
// character a percent-escape decoded to, names no row that can exist, and it
// gets the SAME 404 a real miss gets so a caller cannot map the server's notion
// of a well-formed id.
func (s *Server) updateTarget(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if id == "" || types.CheckFieldLen("id", id) != nil || strings.ContainsFunc(id, isControlChar) {
		s.fail(w, r, NotFound())
		return "", false
	}
	return id, true
}

// updateRequest decodes and validates the body, and reports whether the request
// may proceed. Every refusal here happens BEFORE any database work.
func (s *Server) updateRequest(w http.ResponseWriter, r *http.Request, id string) (issueops.UpdateRequest, bool) {
	members, res := decodeJSONObject(w, r, maxUpdateBodyBytes)
	if res != nil {
		s.fail(w, r, *res)
		return issueops.UpdateRequest{}, false
	}
	if offender, unknown := unknownMember(members, updateRequestMembers); unknown {
		s.failUnknownMember(w, r, offender, updateRequestMembers)
		return issueops.UpdateRequest{}, false
	}

	actor, ok := s.bodyActor(w, r, members)
	if !ok {
		return issueops.UpdateRequest{}, false
	}
	patch, ok := s.issuePatch(w, r, id, members)
	if !ok {
		return issueops.UpdateRequest{}, false
	}

	guards, ok := s.readUpdateGuards(w, r, members)
	if !ok {
		return issueops.UpdateRequest{}, false
	}
	if !s.validateForceAssigneeTransfer(w, r, patch, guards.expectedAssignee, guards.forceAssigneeTransfer) {
		return issueops.UpdateRequest{}, false
	}

	// Claim stays ZERO — acquiring work is `{id}:claim`, which carries its own
	// eligibility rules — and so does IssuePlaneOnly, because this operation
	// resolves across both planes.
	return issueops.UpdateRequest{
		Actor:                 actor,
		IssueID:               id,
		Patch:                 patch,
		ExpectedVersion:       guards.expectedVersion,
		ExpectedStatus:        guards.expectedStatus,
		ExpectedAssignee:      guards.expectedAssignee,
		ForceClosePolicy:      guards.forceClosePolicy,
		ForceAssigneeTransfer: guards.forceAssigneeTransfer,
		Provenance:            updateProvenance,
	}, true
}

type updateGuards struct {
	expectedVersion       *int64
	expectedStatus        *issueops.Status
	expectedAssignee      *string
	forceClosePolicy      bool
	forceAssigneeTransfer bool
}

func (s *Server) readUpdateGuards(w http.ResponseWriter, r *http.Request, members map[string]json.RawMessage) (updateGuards, bool) {
	expectedVersion, res := applyVersionGuardMember(members, "")
	if res != nil {
		s.fail(w, r, *res)
		return updateGuards{}, false
	}
	expectedStatus, ok := s.updateExpectedStatus(w, r, members)
	if !ok {
		return updateGuards{}, false
	}
	expectedAssignee, ok := s.updateExpectedAssignee(w, r, members)
	if !ok {
		return updateGuards{}, false
	}
	forceClosePolicy, ok := s.booleanMember(w, r, members, "force_close_policy")
	if !ok {
		return updateGuards{}, false
	}
	forceAssigneeTransfer, ok := s.booleanMember(w, r, members, "force_assignee_transfer")
	if !ok {
		return updateGuards{}, false
	}
	return updateGuards{
		expectedVersion:       expectedVersion,
		expectedStatus:        expectedStatus,
		expectedAssignee:      expectedAssignee,
		forceClosePolicy:      forceClosePolicy,
		forceAssigneeTransfer: forceAssigneeTransfer,
	}, true
}

func (s *Server) validateForceAssigneeTransfer(w http.ResponseWriter, r *http.Request, patch issueops.IssuePatch, expectedAssignee *string, force bool) bool {
	if !force {
		return true
	}
	if expectedAssignee != nil {
		s.fail(w, r, InvalidArgument("force_assignee_transfer", ReasonInvalidValue,
			"`expected_assignee` is the compare-and-set that replaces the fence; `force_assignee_transfer` bypasses it. Send one"))
		return false
	}
	if !patch.Assignee.Set {
		s.fail(w, r, InvalidArgument("force_assignee_transfer", ReasonInvalidValue,
			"`force_assignee_transfer` bypasses the fence on an assignee TRANSFER; send `patch.assignee` with it"))
		return false
	}
	return true
}

// updateExpectedStatus reads the status precondition, preserving the difference
// between an absent guard and one that expects the empty status. The role
// models it as a POINTER for exactly that reason, so an absent member must not
// collapse into a guard on "".
func (s *Server) updateExpectedStatus(w http.ResponseWriter, r *http.Request, members map[string]json.RawMessage) (*issueops.Status, bool) {
	if _, present := members["expected_status"]; !present {
		return nil, true
	}
	value, ok := s.storedTextMember(w, r, members, "expected_status")
	if !ok {
		return nil, false
	}
	status := issueops.Status(value)
	return &status, true
}

// updateExpectedAssignee is updateExpectedStatus's twin, and the difference
// between nil and a pointer to "" is load-bearing here too: a guard on the
// EMPTY assignee is how a caller says "only if nobody holds it".
func (s *Server) updateExpectedAssignee(w http.ResponseWriter, r *http.Request, members map[string]json.RawMessage) (*string, bool) {
	if _, present := members["expected_assignee"]; !present {
		return nil, true
	}
	value, ok := s.storedTextMember(w, r, members, "expected_assignee")
	if !ok {
		return nil, false
	}
	return &value, true
}

// updateProvenance labels the history entry an update records, naming this
// surface for reopenProvenance's reason: the role's implementations disagree
// about their own default, so a spelled label is what makes an entry read the
// same whichever backend answered. Not wire-visible.
const updateProvenance = "bd serve: update issue"
