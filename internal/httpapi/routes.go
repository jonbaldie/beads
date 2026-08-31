package httpapi

import (
	"strings"

	"github.com/jonbaldie/beads/internal/types"
)

const (
	// customMethodPathValue names the ServeMux wildcard the single-resource
	// custom methods share. The route rows build their pattern from it and
	// customMethodTarget splits the custom method back off the segment it
	// matched, so the two halves of the one path exception cannot drift apart.
	customMethodPathValue = "idop"
	// customMethodPattern is that one registration. Every row carrying a
	// customMethod declares it, and s.handler collapses them into a single
	// ServeMux entry.
	customMethodPattern = "/v0/beads/issues/{" + customMethodPathValue + "}"
	// customMethodIDValue is where the dispatcher leaves the id it split out,
	// so a handler reads its target exactly as one on a literal `{id}` pattern
	// does rather than re-deriving the split.
	customMethodIDValue = "id"
)

// ProjectIDHeader names the optional per-request workspace-identity stamp. A
// client that knows which workspace it means to address puts that workspace's
// project id here; checkProjectStamp (server.go) refuses the request when the id
// it names is not the one this server serves, so a misdirected read or write is
// turned away before it can touch the wrong workspace. The header is optional and
// absent by default: an older client that never sends it is served exactly as
// before, which is what keeps enforcement additive rather than a new precondition.
const ProjectIDHeader = "Bd-Project-Id"

// CapProjectEnforce is the behavior capability that advertises this enforcement.
// Unlike the per-operation tokens it names no route: it tells a client that a
// stamped request WILL be checked here, so the client can rely on the refusal
// instead of discovering an older server silently ignored its stamp.
const CapProjectEnforce = "project.enforce"

// customMethodTarget splits the custom method off the segment the router
// matched, and reports the row that claims it.
//
// ServeMux wildcards match a whole path segment, so `{id}:claim` is not
// expressible as a pattern: the rows register one shared wildcard and the parse
// lands here. A segment that ends in no REGISTERED suffix is not an addressable
// resource on this surface — POST on the issue-detail path is documented
// nowhere — so it gets the same 404 the catch-all gives any other unrouted
// path. That is what keeps the wide pattern a routing detail rather than
// undocumented surface.
//
// The id itself is bounded HERE, once for every operation on the pattern, for
// the same reason the actor is bounded at the edge: this is the last point
// before a request buys a concurrency slot and two database round trips.
// `issues.id` is VARCHAR(255) and the document calls the parameter an exact
// canonical id, so a longer one — or one carrying a control character, which a
// percent-escape in the path decodes to — names no row that can exist.
func customMethodTarget(rows []route, segment string) (route, string, *Result) {
	unrouted := func() *Result {
		res := newResult(CodeNotFound, "no such route on this server")
		return &res
	}
	for _, rt := range rows {
		id, ok := strings.CutSuffix(segment, rt.customMethod)
		if !ok || id == "" {
			continue
		}
		if types.CheckFieldLen("id", id) != nil || strings.ContainsFunc(id, isControlChar) {
			// The SAME 404 a real miss gets. A distinct refusal here would let
			// a caller map the server's notion of a well-formed id, and there
			// is nothing to learn from it: no such row exists either way.
			res := NotFound()
			return route{}, "", &res
		}
		return rt, id, nil
	}
	return route{}, "", unrouted()
}
