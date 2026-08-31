package beads

import (
	"context"
	"errors"
	"testing"

	"github.com/jonbaldie/beads/internal/storage/dolt"
)

func TestOpenFromConfigPassesContextPathAndCreatePolicy(t *testing.T) {
	original := openFromConfigWithOptions
	t.Cleanup(func() { openFromConfigWithOptions = original })

	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "sentinel")
	wantErr := errors.New("stop after capture")
	openFromConfigWithOptions = func(gotCtx context.Context, gotDir string, cfg *dolt.Config) (*dolt.DoltStore, error) {
		if gotCtx == nil || gotCtx.Value(contextKey{}) != "sentinel" {
			t.Fatalf("context = %v, want caller context", gotCtx)
		}
		if gotDir != "/beads" {
			t.Fatalf("beadsDir = %q, want /beads", gotDir)
		}
		if cfg == nil || !cfg.RemoteOptions.CreateIfMissing {
			t.Fatalf("config = %#v, want CreateIfMissing", cfg)
		}
		return nil, wantErr
	}

	_, err := OpenFromConfig(ctx, "/beads")
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}
