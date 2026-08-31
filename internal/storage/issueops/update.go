package issueops

import (
	"context"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// IsAllowedUpdateField checks if a field name is valid for issue updates.
func IsAllowedUpdateField(key string) bool {
	allowed := map[string]bool{
		"status": true, "priority": true, "title": true, "assignee": true, "owner": true,
		"description": true, "design": true, "acceptance_criteria": true, "notes": true,
		"issue_type": true, "estimated_minutes": true, "external_ref": true, "spec_id": true,
		"started_at": true,
		"closed_at":  true, "close_reason": true, "closed_by_session": true,
		"source_repo": true,
		"sender":      true, "wisp": true, "wisp_type": true, "no_history": true, "pinned": true,
		"mol_type":       true,
		"event_category": true, "event_actor": true, "event_target": true, "event_payload": true,
		"due_at": true, "defer_until": true, "await_id": true, "waiters": true,
		"metadata": true,
	}
	return allowed[key]
}

// ManageClosedAt brings the close-lifecycle columns along with a status
// transition, so a generic update that crosses into closed lands the same row
// `bd close` would and a reopen leaves nothing of the old close behind.
//
// closeIssueInTx writes closed_at, close_reason and closed_by_session on EVERY
// close, including the empty reason and session a caller that supplied neither
// produces. The update funnels write a column only when the caller names its
// key, so without these defaults a generic close inherits whatever the previous
// close left: a re-close after a generic reopen keeps the old session and
// `bd show` reports the new close as the old session's work (ga-kjkv1). Each
// default is suppressed by its own explicit key, so a caller that names the
// column still wins — the CLI's own closed_by_session pass-through included.
//
// The reopen branch clears all three, mirroring ReopenIssueInTx. Both branches
// stay keyed on the LITERAL closed status; category-aware reopen keying is
// ga-ktn9pe.4.15.
func ManageClosedAt(oldIssue *types.Issue, updates map[string]interface{}, setClauses []string, args []interface{}) ([]string, []interface{}) {
	statusVal, hasStatus := updates["status"]
	if !hasStatus {
		return setClauses, args
	}

	var newStatus string
	switch v := statusVal.(type) {
	case string:
		newStatus = v
	case types.Status:
		newStatus = string(v)
	default:
		return setClauses, args
	}

	// Defaults are SET-clause appends, never inserts into updates: the map
	// drives no-op detection, the event payload, lease management and the
	// blocked recompute, none of which should see a column the caller did not
	// ask for.
	appendDefault := func(column string, value interface{}) {
		if _, explicit := updates[column]; explicit {
			return
		}
		setClauses = append(setClauses, column+" = ?")
		args = append(args, value)
	}

	if newStatus == string(types.StatusClosed) {
		appendDefault("closed_at", time.Now().UTC())
		appendDefault("close_reason", "")
		appendDefault("closed_by_session", "")
	} else if oldIssue.Status == types.StatusClosed {
		appendDefault("closed_at", nil)
		appendDefault("close_reason", "")
		appendDefault("closed_by_session", "")
	}

	return setClauses, args
}

// ValidateClosedAtCoherence refuses an explicit closed_at write that would
// leave the row violating the closed-iff-closed_at invariant
// types.Issue.Validate enforces: a closed issue has a closed_at, a non-closed
// issue has none. It reads the status the update actually lands — the update's
// own status when it carries one, otherwise the row's current status, which
// both funnels read inside the mutation transaction.
//
// The key stays allowlisted, so every coherent standalone write still works:
// stamping closed_at on a row that is already closed is the repair path for
// rows a pre-invariant close left blank, an external-sync or backfill caller
// can land status and closed_at together, and clearing closed_at on a row that
// is or becomes non-closed is the reverse repair. What is refused is the half
// that mints a row no reader can trust — stamping closed_at on a row that stays
// open, or clearing it from a row that stays closed.
//
// This is a coherence invariant rather than close policy, so the force override
// that waives the open-children and blocker refusals does not waive it, exactly
// as force never waives the version CAS.
//
// Both funnels must call this BEFORE DiscardNoopIssueUpdates. The guard judges
// the caller's intent, and a closed_at equal to the stored value is still a
// request to keep the column; the no-op filter drops it as an unchanged value,
// which would hide exactly the incoherent halves this refuses and let
// ManageClosedAt's reopen branch clear the column instead.
func ValidateClosedAtCoherence(oldIssue *types.Issue, updates map[string]interface{}) error {
	rawClosedAt, hasClosedAt := updates["closed_at"]
	if !hasClosedAt {
		return nil
	}

	landedStatus := oldIssue.Status
	if rawStatus, hasStatus := updates["status"]; hasStatus {
		switch value := rawStatus.(type) {
		case string:
			landedStatus = types.Status(value)
		case types.Status:
			landedStatus = value
		default:
			// A status Go type nobody can read is already CrossesIntoDoneCategoryInTx's
			// refusal in both funnels; leave that the single message for it.
			return nil
		}
	}

	clearing := clearsClosedAt(rawClosedAt)
	switch {
	case landedStatus == types.StatusClosed && clearing:
		return fmt.Errorf("%w: refusing to clear closed_at on %s: its status stays %q, and a closed issue must keep a closed_at; reopen it with a status update instead",
			storage.ErrValidation, oldIssue.ID, landedStatus)
	case landedStatus != types.StatusClosed && !clearing:
		return fmt.Errorf("%w: refusing to set closed_at on %s: its status stays %q, and only a closed issue may carry a closed_at; set status=closed in the same update to close it",
			storage.ErrValidation, oldIssue.ID, landedStatus)
	}
	return nil
}

// clearsClosedAt reports whether an allowlisted closed_at value blanks the
// column. It accepts the same nil shapes matchesTimePointer treats as empty, so
// the guard and the no-op filter agree on what "no closed_at" means.
func clearsClosedAt(value interface{}) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case *time.Time:
		return typed == nil
	default:
		return false
	}
}

// ManageStartedAt auto-sets started_at when transitioning to in_progress.
// If the issue already has a started_at, it is preserved (not overwritten).
func ManageStartedAt(oldIssue *types.Issue, updates map[string]interface{}, setClauses []string, args []interface{}) ([]string, []interface{}) {
	statusVal, hasStatus := updates["status"]
	_, hasExplicitStartedAt := updates["started_at"]
	if hasExplicitStartedAt || !hasStatus {
		return setClauses, args
	}

	var newStatus string
	switch v := statusVal.(type) {
	case string:
		newStatus = v
	case types.Status:
		newStatus = string(v)
	default:
		return setClauses, args
	}

	if newStatus == string(types.StatusInProgress) && oldIssue.StartedAt == nil {
		now := time.Now().UTC()
		setClauses = append(setClauses, "started_at = ?")
		args = append(args, now)
	}

	return setClauses, args
}

// ManageLeaseOnUpdate keeps lease ownership coherent when generic updates alter
// status or assignee. Leases are armed ONLY by the lease-aware verbs — claim
// (ClaimIssueInTx, bd update --claim, bd ready --claim) and heartbeat — never by
// a generic update. A bare `bd update -s in_progress -a <who>` is an interactive
// hand-dole claim: nobody is heartbeating it, so arming a lease here just turns
// the claim into reclaim-bait that reverts to open after the TTL (bd-9hpgf,
// GH#4716). This helper therefore only ever CLEARS lease columns:
//
//   - the update moves the row out of the claimed state (not in_progress, or
//     unassigned): any lease is stale — clear it.
//   - the update changes who holds the claim (assignee transfer, or a fresh
//     transition into in_progress): the previous owner's lease must not count
//     down against the new holder — clear it. The new holder gets a lease only
//     via the claim verb; a real worker's next heartbeat re-arms one.
//   - the update leaves the same claim in place (already in_progress, same
//     assignee): leave the lease untouched, so a worker's live lease survives
//     unrelated edits to its issue.
//
// Returns true when the update ends/transfers the claim and the issue's lease
// row must be deleted (DeleteLeaseInTx) after the row update.
func ManageLeaseOnUpdate(oldIssue *types.Issue, updates map[string]interface{}) bool {
	rawStatus, hasStatus := updates["status"]
	rawAssignee, hasAssignee := updates["assignee"]
	if !hasStatus && !hasAssignee {
		return false
	}

	newStatus, ok := leaseStatusValue(rawStatus, hasStatus, oldIssue.Status)
	if !ok {
		return false
	}
	newAssignee := leaseAssigneeValue(rawAssignee, hasAssignee, oldIssue.Assignee)

	// newAssignee == oldIssue.Assignee is deliberately verbatim, not
	// actorMatches (ga-v2k49): a generic update that hand-doles the same
	// identity back under a different layer's spelling genuinely rewrites the
	// assignee column's bytes, so clearing the lease here (as any other
	// transfer would) is defensible rather than a false positive — it costs
	// the holder's live lease, but HeartbeatIssueInTx's fallback re-arms one
	// under the caller's current spelling on the next beat, so it self-heals.
	sameClaim := newStatus == string(types.StatusInProgress) && newAssignee != "" &&
		oldIssue.Status == types.StatusInProgress && newAssignee == oldIssue.Assignee
	return !sameClaim
}

func leaseStatusValue(rawStatus interface{}, present bool, oldStatus types.Status) (string, bool) {
	if !present {
		return string(oldStatus), true
	}
	switch value := rawStatus.(type) {
	case string:
		return value, true
	case types.Status:
		return string(value), true
	default:
		return "", false
	}
}

func leaseAssigneeValue(rawAssignee interface{}, present bool, oldAssignee string) string {
	if !present {
		return oldAssignee
	}
	switch value := rawAssignee.(type) {
	case nil:
		return ""
	case string:
		return value
	default:
		return fmt.Sprint(value)
	}
}

// DetermineEventType returns the appropriate event type for an update.
func DetermineEventType(oldIssue *types.Issue, updates map[string]interface{}) types.EventType {
	statusVal, hasStatus := updates["status"]
	if !hasStatus {
		return types.EventUpdated
	}

	var newStatus string
	switch v := statusVal.(type) {
	case string:
		newStatus = v
	case types.Status:
		newStatus = string(v)
	default:
		return types.EventUpdated
	}

	if newStatus == string(types.StatusClosed) {
		return types.EventClosed
	}
	if oldIssue.Status == types.StatusClosed {
		return types.EventReopened
	}
	return types.EventStatusChanged
}

// CrossesIntoDoneCategoryInTx reports whether updates move oldStatus from
// outside the done category into it. The categories are resolved from the same
// transaction that will perform the write, so a custom status configured as
// done counts exactly as the built-in closed status does.
//
// It is false without a status update, and for a move that starts in the done
// category — a done-to-done restatement is not a crossing, and reopening is the
// operation that leaves.
//
// A status value whose Go type cannot carry a status is a validation error, not
// a false. Both write funnels ask this question to decide whether close policy
// applies, so answering "no crossing" for a value nobody can read would let an
// in-process caller that got the transport wrong land status='closed' with the
// policy gate skipped — on an issue with open children, no less. Refusing here
// gives a mis-typed status the same fail-loud handling the mis-typed override
// key already gets (see PopForceClosePolicy).
func CrossesIntoDoneCategoryInTx(ctx context.Context, tx DBTX, oldStatus types.Status, updates map[string]interface{}) (bool, error) {
	rawStatus, hasStatus := updates["status"]
	if !hasStatus {
		return false, nil
	}
	var newStatus types.Status
	switch value := rawStatus.(type) {
	case string:
		newStatus = types.Status(value)
	case types.Status:
		newStatus = value
	default:
		return false, fmt.Errorf("%w: status value of type %T is neither a string nor a types.Status", storage.ErrValidation, rawStatus)
	}

	newCategory, err := ReopenCategoryInTx(ctx, tx, newStatus)
	if err != nil {
		return false, err
	}
	if newCategory != types.CategoryDone {
		return false, nil
	}
	oldCategory, err := ReopenCategoryInTx(ctx, tx, oldStatus)
	if err != nil {
		return false, err
	}
	return oldCategory != types.CategoryDone, nil
}

// UpdateResult holds the result of an UpdateIssueInTx call.
type UpdateResult struct {
	OldIssue         *types.Issue
	IsWisp           bool
	Changed          bool
	IssueRowsChanged bool
	WispRowsChanged  bool
}
