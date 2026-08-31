//go:build !cgo

package embeddeddolt

import (
	"context"
	"errors"
	"testing"
)

func TestNoCGOOpenVariantsReturnTheirSentinel(t *testing.T) {
	openers := []struct {
		name string
		open func(context.Context, string, string, string) (*EmbeddedDoltStore, error)
	}{
		{"Open", Open},
		{"OpenReadOnly", OpenReadOnly},
		{"OpenForPreviewCommand", OpenForPreviewCommand},
		{"OpenForReadOnlyCommand", OpenForReadOnlyCommand},
		{"OpenForWorkingSetReconcile", OpenForWorkingSetReconcile},
	}
	for _, opener := range openers {
		t.Run(opener.name, func(t *testing.T) {
			store, err := opener.open(context.Background(), "dir", "database", "branch")
			if store != nil {
				t.Fatalf("store = %#v, want nil", store)
			}
			if !errors.Is(err, errNoCGO) {
				t.Fatalf("error = %v, want %v", err, errNoCGO)
			}
		})
	}
}
