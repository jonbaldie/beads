package main

import (
	"slices"

	"github.com/jonbaldie/beads/internal/types"
)

// depGlyph marks a dependency/relationship edge in the --deps tree view. It is
// deliberately distinct from the parent-child connectors (├── └──) so a
// dependency can never be mistaken for hierarchy (the confusion GH#4686 fixed).
const depGlyph = "╌╌▷"

// depEdgeDisplay classifies a dependency edge for the --deps view.
//   - label: how the edge reads from the source node's perspective. A stored
//     "blocks" edge (issue_id depends on depends_on_id) reads as "depends-on",
//     matching `bd show`.
//   - scheduling: true for edges that constrain execution order (depends-on,
//     conditional-blocks, waits-for). Only these drive sibling ordering.
//   - ok: false for parent-child, which is hierarchy and never appears here.
type depEdgeKind struct {
	label      string
	scheduling bool
	ok         bool
}

var depEdgeKinds = map[types.DependencyType]depEdgeKind{
	types.DepParentChild:       {ok: false},
	types.DepBlocks:            {label: "depends-on", scheduling: true, ok: true},
	types.DepConditionalBlocks: {label: "conditionally-depends-on", scheduling: true, ok: true},
	types.DepWaitsFor:          {label: "waits-for", scheduling: true, ok: true},
	types.DepRelated:           {label: "related", ok: true},
	types.DepRelatesTo:         {label: "related", ok: true},
	types.DepDiscoveredFrom:    {label: "discovered-from", ok: true},
	types.DepDuplicates:        {label: "duplicates", ok: true},
	// `bd supersede old --with new` stores (old, new), so the source end of
	// the edge is the issue being replaced.
	types.DepSupersedes: {label: "superseded-by", ok: true},
	types.DepRepliesTo:  {label: "replies-to", ok: true},
}

func depEdgeDisplay(t types.DependencyType) (label string, scheduling, ok bool) {
	if kind, found := depEdgeKinds[t]; found {
		return kind.label, kind.scheduling, kind.ok
	}
	return string(t), false, true
}

// orderSiblingsByDeps reorders a sibling group so that, within the group, an
// issue that depends on another (via a scheduling edge) sorts after it — giving
// a top-to-bottom reading that is a valid execution order. Ties and dependency
// cycles fall back to compareIssuesByPriority (priority, then natural ID), so
// the result is always total and never hangs on a cycle. Ordering is driven by
// scheduling edges only, regardless of --deps mode: knowledge-graph edges
// (related, discovered-from, ...) carry no ordering meaning.
func orderSiblingsByDeps(siblings []*types.Issue, allDeps map[string][]*types.Dependency) []*types.Issue {
	if len(siblings) < 2 || allDeps == nil {
		return siblings
	}
	byID, indeg, dependents := siblingDepGraph(siblings, allDeps)
	out, emitted := kahnOrderSiblings(siblings, byID, indeg, dependents)
	return appendCyclicSiblings(out, siblings, emitted)
}

func siblingDepGraph(siblings []*types.Issue, allDeps map[string][]*types.Dependency) (map[string]*types.Issue, map[string]int, map[string][]string) {
	byID := make(map[string]*types.Issue, len(siblings))
	for _, s := range siblings {
		byID[s.ID] = s
	}
	// indeg[N] = count of in-group scheduling targets N must come after.
	// dependents[T] = in-group issues that depend on T (T unblocks them).
	indeg := make(map[string]int, len(siblings))
	dependents := make(map[string][]string)
	for _, s := range siblings {
		addSiblingSchedulingEdges(s, allDeps[s.ID], byID, indeg, dependents)
	}
	return byID, indeg, dependents
}

func addSiblingSchedulingEdges(s *types.Issue, deps []*types.Dependency, byID map[string]*types.Issue, indeg map[string]int, dependents map[string][]string) {
	seen := make(map[string]bool)
	for _, dep := range deps {
		if _, scheduling, ok := depEdgeDisplay(dep.Type); !ok || !scheduling {
			continue
		}
		t := dep.DependsOnID
		if t == s.ID || byID[t] == nil || seen[t] {
			continue
		}
		seen[t] = true
		indeg[s.ID]++
		dependents[t] = append(dependents[t], s.ID)
	}
}

func kahnOrderSiblings(siblings []*types.Issue, byID map[string]*types.Issue, indeg map[string]int, dependents map[string][]string) ([]*types.Issue, map[string]bool) {
	ready := make([]*types.Issue, 0, len(siblings))
	for _, s := range siblings {
		if indeg[s.ID] == 0 {
			ready = append(ready, s)
		}
	}
	slices.SortFunc(ready, compareIssuesByPriority)
	out := make([]*types.Issue, 0, len(siblings))
	emitted := make(map[string]bool, len(siblings))
	for {
		if len(ready) == 0 {
			break
		}
		n := ready[0]
		ready = ready[1:]
		out = append(out, n)
		emitted[n.ID] = true
		grew := false
		for _, dID := range dependents[n.ID] {
			indeg[dID]--
			if indeg[dID] == 0 {
				ready = append(ready, byID[dID])
				grew = true
			}
		}
		if grew {
			slices.SortFunc(ready, compareIssuesByPriority)
		}
	}
	return out, emitted
}

func appendCyclicSiblings(out, siblings []*types.Issue, emitted map[string]bool) []*types.Issue {
	if len(out) >= len(siblings) {
		return out
	}
	rest := make([]*types.Issue, 0, len(siblings)-len(out))
	for _, s := range siblings {
		if !emitted[s.ID] {
			rest = append(rest, s)
		}
	}
	slices.SortFunc(rest, compareIssuesByPriority)
	return append(out, rest...)
}
