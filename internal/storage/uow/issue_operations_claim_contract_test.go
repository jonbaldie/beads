package uow

import (
	"testing"

	storageissueops "github.com/jonbaldie/beads/internal/storage/issueops"
	publicops "github.com/jonbaldie/beads/issueops"
)

// Lifecycle and BatchApplier both run ExecuteUpdate, so the claim CAS,
// plane restriction, and Changed rule live in that shared body. What
// remains here is the commit-message helper this adapter still owns.

func TestUpdateHistoryEntryNamesProvenance(t *testing.T) {
	for _, tc := range []struct {
		name       string
		provenance string
		changed    bool
		claim      bool
		want       string
	}{
		{"caller supplies one", "bd serve: claim bd-1 by alice", true, true, "bd serve: claim bd-1 by alice"},
		{"caller supplies none", "", true, true, "update issue"},
		{"idempotent claim records none", "bd serve: claim bd-1 by alice", false, true, ""},
		{"same-value patch still records", "", false, false, "update issue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := publicops.UpdateRequest{Claim: tc.claim, Provenance: tc.provenance}
			if !tc.claim {
				request.Patch = publicops.IssuePatch{Title: publicops.Field[string]{Set: true, Value: "same"}}
			}
			if got := updateHistoryEntry(request, tc.changed); got != tc.want {
				t.Fatalf("updateHistoryEntry() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReopenHistoryEntryNamesProvenance(t *testing.T) {
	for _, tc := range []struct {
		name       string
		provenance string
		want       string
	}{
		{"caller supplies one", "bd: reopen bd-1", "bd: reopen bd-1"},
		{"caller supplies none", "", "reopen issue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := storageissueops.HistoryEntry(tc.provenance, "reopen issue")
			if got != tc.want {
				t.Fatalf("HistoryEntry() = %q, want %q", got, tc.want)
			}
		})
	}
}
