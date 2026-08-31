package domain

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/validation"
	publicops "github.com/jonbaldie/beads/issueops"
)

func (u *issueClaimModule) ClaimIssue(ctx context.Context, id, actor string) (ClaimResult, error) {
	return u.claim(ctx, id, actor, false)
}

func (u *issueClaimModule) ClaimWisp(ctx context.Context, id, actor string) (ClaimResult, error) {
	return u.claim(ctx, id, actor, true)
}

func (u *issueClaimModule) claim(ctx context.Context, id, actor string, useWisp bool) (ClaimResult, error) {
	if id == "" {
		return ClaimResult{}, fmt.Errorf("claim: id must not be empty")
	}
	if actor == "" {
		return ClaimResult{}, fmt.Errorf("claim: actor must not be empty")
	}
	row, err := u.issueRepo.Claim(ctx, id, actor, IssueTableOpts{UseWispsTable: useWisp})
	if err != nil {
		return ClaimResult{}, fmt.Errorf("claim %s: %w", id, err)
	}
	return claimResultFromRow(row, id, actor)
}

func claimResultFromRow(row ClaimRowResult, id, actor string) (ClaimResult, error) {
	if row.Updated {
		return ClaimResult{}, nil
	}
	if validation.ActorMatches(row.CurrentAssignee, actor) && row.CurrentStatus == types.StatusInProgress {
		return ClaimResult{AlreadyClaimed: true, PriorAssignee: actor}, nil
	}
	return ClaimResult{}, newClaimConflict(row, id, actor)
}

func newClaimConflict(row ClaimRowResult, id, actor string) *publicops.ClaimConflictError {
	// The refusal carries the assignee and status the repository read back in
	// THIS transaction, so a caller reports who won without parsing the
	// message. The copy lives on the typed error, shared with the twin
	// (issueops.ClaimIssueInTx) that used to restate it — that is what bd-at6rc
	// asked for. The sentinel stays matchable through Unwrap, which the proxied
	// batch exit code keys on.
	//
	// A pool-assigned issue only loses the CAS for a non-assignee reason
	// (status changed underneath us), so it refuses on the status rather than
	// with a misleading held-by refusal.
	// Composed here rather than on the typed error, for the reason
	// issueops.ClaimIssueInTx gives: ClaimConflictError passes its wrapped
	// refusal through unchanged, so the fragments have to be in the wrapped
	// error or beads.ParseClaimConflict recovers nothing. This is the
	// domain-stack twin of that producer and must answer the same three ways
	// (bd-at6rc).
	refusal := fmt.Errorf("%w%s%s", storage.ErrNotClaimable, storage.NotClaimableStatusFragment, row.CurrentStatus)
	if row.CurrentAssignee != "" && !validation.ActorMatches(row.CurrentAssignee, actor) {
		switch {
		// Pool aliases refuse on the STATUS, checked first so a pool never
		// reaches the holder-steering copy below.
		case row.CurrentAssigneeIsPool:
			// refusal already names the status.
		case row.CurrentStatus == types.StatusOpen:
			// Never name a release command here (wy-yuclk): copy that names
			// one gets pattern-matched by batch agents into an unclaim+claim
			// steamroller of live claims. bd reclaim is safe to name because
			// it only recovers claims whose lease has already expired. The
			// parseable " by <assignee>" tail is deliberately absent, so the
			// holder is recovered from the typed field instead.
			refusal = fmt.Errorf("%w: already assigned to %q — coordinate with the holder; if their claim is abandoned (crashed agent), lease expiry will surface it for bd reclaim", storage.ErrAlreadyClaimed, row.CurrentAssignee)
		default:
			refusal = fmt.Errorf("%w%s%s", storage.ErrAlreadyClaimed, storage.ClaimedByFragment, row.CurrentAssignee)
		}
	}
	return &publicops.ClaimConflictError{
		IssueID:  id,
		Assignee: row.CurrentAssignee,
		Status:   row.CurrentStatus,
		Err:      refusal,
	}
}

// ApplyUpdate applies spec's guarded field updates and returns the resulting
// issue. When ExpectedAssignee is set, the guard compares under
// validation.ActorMatches, not verbatim ==, so two spellings of the same
// identity (ga-wzl83) don't false-mismatch — the unit-of-work twin of
// issueops.CheckExpectedFieldsInTx's SQL-CAS guard; both paths of
// AuthorizeAssigneeTransferWithPools already made this fix, this was the
// third, previously-split verbatim-comparison surface (ga-5ksp5, gate review
