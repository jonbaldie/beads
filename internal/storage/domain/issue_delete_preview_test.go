package domain

import (
	"testing"

	"github.com/jonbaldie/beads/internal/types"
)

func TestPreviewMissingIDsPreservesNilWhenAllIDsExist(t *testing.T) {
	got := previewMissingIDs(
		[]string{"bd-one", "bd-two"},
		map[string]*types.Issue{
			"bd-one": {},
			"bd-two": {},
		},
	)
	if got != nil {
		t.Fatalf("previewMissingIDs() = %#v, want nil", got)
	}
}

func TestPreviewMissingIDsListsMissingIDs(t *testing.T) {
	got := previewMissingIDs(
		[]string{"bd-one", "bd-missing", "bd-two", "bd-missing-again"},
		map[string]*types.Issue{
			"bd-one": {},
			"bd-two": {},
		},
	)
	want := []string{"bd-missing", "bd-missing-again"}
	if len(got) != len(want) {
		t.Fatalf("previewMissingIDs() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("previewMissingIDs() = %#v, want %#v", got, want)
		}
	}
}
