package httpapi

import (
	"errors"
	"net/http"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/issueops"
)

// failUpdate answers a failed update, mapping the role's TYPED refusals onto
// the frozen codes and its own validation refusal onto the 400 the document
// promises.
//
// EVERY 409 BRANCH READS TYPED FIELDS, never prose, and every one of them is
// matched BEFORE the ErrValidation and ErrNotFound arms. That order is the
// whole correctness of this function, and the hazard it avoids is worse than a
// disagreement between backends: NEITHER LEG WRAPS THESE FIVE FAMILIES IN
// ErrValidation. The store legs return ExecuteUpdate's error through
// runIssueOperationTx unchanged (internal/storage/dolt/issue_operations.go) and
// the unit of work returns ApplyUpdate's unchanged
// (internal/storage/uow/issue_operations.go), so a precondition miss, a close-
// policy refusal, the assignee fence and both graph refusals reach here as bare
// sentinels. Below the ErrValidation arm they would all fall into `!Is(...)`
// and be swallowed into failErr — a generic 500, on BOTH legs, for five
// conditions this document names by code.
//
// NEITHER BRANCH QUOTES THE ROLE'S MESSAGE. A refusal from the workspace's
// configured vocabulary arrives as prose about statuses and types, and a
// refused edge as a driver error naming tables and constraints; 4xx details on
// this surface reflect the caller's own input back rather than server
// internals. The real error goes to the log with the request id.
//
// The ErrNotFound arm is LAST among the misses and means the PATH id, which is
// the divergence from failBatchCreate: this operation addresses a resource, so
// an id that names nothing is a genuine 404. A missing new PARENT is not that —
// it is an edge endpoint, and it is a 400 on `patch.parent_id`, conforming to
// POST /v0/beads/dependencies:add.
func (s *Server) failUpdate(w http.ResponseWriter, r *http.Request, request issueops.UpdateRequest, err error) {
	if failure, ok := classifyUpdateFailure(request, err); ok {
		if failure.refused {
			recordUpdateRefusal(s, r, err)
		}
		s.fail(w, r, failure.result)
		return
	}
	if errors.Is(err, storage.ErrNotFound) {
		s.fail(w, r, NotFound())
		return
	}
	if !errors.Is(err, storage.ErrValidation) {
		s.failErr(w, r, err)
		return
	}
	recordUpdateRefusal(s, r, err)
	s.fail(w, r, InvalidArgument(updatePatchMember, ReasonInvalidValue,
		"a patch value was refused by this workspace's own validation; nothing was written"))
}

type updateFailure struct {
	result  Result
	refused bool
}

func classifyUpdateFailure(request issueops.UpdateRequest, err error) (updateFailure, bool) {
	if failure, ok := updatePreconditionFailure(request, err); ok {
		return failure, true
	}
	if failure, ok := updateCloseFailure(err); ok {
		return failure, true
	}
	if failure, ok := updateClaimFailure(err); ok {
		return failure, true
	}
	if failure, ok := updateDependencyFailure(err); ok {
		return failure, true
	}
	var endpoint *issueops.DependencyEndpointNotFoundError
	if errors.As(err, &endpoint) {
		return updateFailure{
			result: InvalidArgument(patchParam("parent_id"), ReasonInvalidValue,
				"`parent_id` names no issue this workspace holds; nothing was written"),
			refused: true,
		}, true
	}
	return updateFailure{}, false
}

func updatePreconditionFailure(request issueops.UpdateRequest, err error) (updateFailure, bool) {
	switch {
	case errors.Is(err, issueops.ErrVersionMismatch),
		errors.Is(err, issueops.ErrStatusMismatch),
		errors.Is(err, issueops.ErrAssigneeMismatch):
		return updateFailure{result: updatePreconditionResult(request, err)}, true
	default:
		return updateFailure{}, false
	}
}

func updateCloseFailure(err error) (updateFailure, bool) {
	if errors.Is(err, issueops.ErrCloseOpenChildren) {
		res := namedUpdateResult(newResult(CodeNotClosable,
			"`patch.status` closes an issue with open children; close them first, or send `force_close_policy`"),
			patchParam("status"))
		var openChildren *issueops.CloseOpenChildrenError
		if errors.As(err, &openChildren) {
			res = res.WithOpenChildren(openChildren.OpenChildren)
		}
		return updateFailure{result: res}, true
	}
	if errors.Is(err, issueops.ErrCloseBlocked) {
		return updateFailure{result: namedUpdateResult(newResult(CodeNotClosable,
			"`patch.status` closes a blocked issue; clear the blocker, or send `force_close_policy`"),
			patchParam("status"))}, true
	}
	return updateFailure{}, false
}

func updateClaimFailure(err error) (updateFailure, bool) {
	if !errors.Is(err, storage.ErrAlreadyClaimed) {
		return updateFailure{}, false
	}
	res := namedUpdateResult(newResult(CodeAlreadyClaimed,
		"`patch.assignee` transfers work away from a live foreign owner; send `force_assignee_transfer`, or guard with `expected_assignee`"),
		patchParam("assignee"))
	var claimed *issueops.ClaimConflictError
	if errors.As(err, &claimed) {
		if claimed.Assignee != "" {
			res = res.WithAssignee(claimed.Assignee)
		}
		if claimed.Status != "" {
			res = res.WithIssueStatus(string(claimed.Status))
		}
	}
	return updateFailure{result: res}, true
}

func updateDependencyFailure(err error) (updateFailure, bool) {
	var typeConflict *issueops.DependencyTypeConflictError
	if errors.As(err, &typeConflict) {
		return updateFailure{result: namedUpdateResult(newResult(CodeDependencyExists,
			"this pair already carries an edge of a different type; remove it before reparenting").
			WithDependencyTypeConflict(typeConflict.ExistingType, typeConflict.RequestedType),
			patchParam("parent_id"))}, true
	}
	var hierarchy *issueops.DependencyHierarchyConflictError
	if errors.As(err, &hierarchy) {
		return updateFailure{result: namedUpdateResult(newResult(CodeDependencyCycle,
			"this reparent would put the issue under its own descendant, or gate it on its own ancestor").
			WithHierarchyConflict(hierarchy.IssueID, hierarchy.BlockerID, hierarchy.BlockerIsAncestor),
			patchParam("parent_id"))}, true
	}
	if errors.Is(err, issueops.ErrDependencyCycle) {
		return updateFailure{result: namedUpdateResult(newResult(CodeDependencyCycle,
			"this reparent would create a dependency cycle; nothing was written"),
			patchParam("parent_id"))}, true
	}
	return updateFailure{}, false
}

func namedUpdateResult(res Result, param string) Result {
	res.Problem.Param = &param
	return res
}

func recordUpdateRefusal(s *Server, r *http.Request, err error) {
	s.event("request_refused", "request_id", requestInfo(r.Context()).id, "error", err.Error())
}

// updatePreconditionResult builds the 409 for a guard that missed, naming the
// guard member and echoing what the request asked for.
//
// applyPreconditionResult's rule unchanged: the expected value comes from the
// REQUEST rather than from a read, and the observed value is absent, because
// the refusal rolled its transaction back and a read afterwards would describe
// a row the refusal never saw. See PreconditionFailed.
func updatePreconditionResult(request issueops.UpdateRequest, err error) Result {
	res := PreconditionFailed()
	switch {
	case errors.Is(err, issueops.ErrVersionMismatch):
		res.Problem.Param = updateGuardParam("expected_version")
		if request.ExpectedVersion != nil {
			res = res.WithExpectedVersion(*request.ExpectedVersion)
		}
	case errors.Is(err, issueops.ErrStatusMismatch):
		res.Problem.Param = updateGuardParam("expected_status")
		if request.ExpectedStatus != nil {
			res = res.WithExpectedStatus(string(*request.ExpectedStatus))
		}
	default:
		res.Problem.Param = updateGuardParam("expected_assignee")
		if request.ExpectedAssignee != nil {
			res = res.WithExpectedAssignee(*request.ExpectedAssignee)
		}
	}
	return res
}

// updateGuardParam names a request-level guard member for `param`. The guards
// sit BESIDE `patch` rather than inside it, so they are spelled bare where a
// patch member is qualified by patchParam.
func updateGuardParam(member string) *string { return &member }
