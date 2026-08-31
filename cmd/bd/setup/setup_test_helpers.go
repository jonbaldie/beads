package setup

import (
	"testing"

	"github.com/jonbaldie/beads/internal/templates/agents"
)

// stubDetectRenderOpts overrides detectRenderOptsImpl to return
// DefaultRenderOpts (HasRemote=true), matching what agents.RenderSection()
// produces. This prevents hash mismatches in tests where no beads config exists.
func stubDetectRenderOpts(t *testing.T) {
	t.Helper()
	orig := detectRenderOptsImpl.Load()
	detectRenderOptsImpl.Store(func() agents.RenderOpts { return agents.DefaultRenderOpts() })
	t.Cleanup(func() { detectRenderOptsImpl.Store(orig) })
}
