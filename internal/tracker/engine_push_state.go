package tracker

import "github.com/jonbaldie/beads/internal/types"

// ResolveState maps a beads status to a tracker state ID using the push state cache.
// Returns (stateID, ok). Only usable during a push operation after BuildStateCache has run.
func (e *Engine) ResolveState(status types.Status) (string, bool) {
	cache := engineStateCache(e)
	if e.PushHooks == nil || e.PushHooks.ResolveState == nil || cache == nil {
		return "", false
	}
	return e.PushHooks.ResolveState(cache, status)
}
