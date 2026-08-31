package doctor

import "sort"

// dependencyCycleSearch holds the mutable state for the iterative Tarjan
// traversal. Keeping the state in a value makes the traversal helpers small
// while preserving the non-recursive walk used for very deep graphs.
type dependencyCycleSearch struct {
	edges      map[string][]string
	index      map[string]int
	low        map[string]int
	onStack    map[string]bool
	sccStack   []string
	next       int
	cycleNodes []string
}

type dependencyCycleFrame struct {
	node string
	succ int
}

// dependencyCycleNodes returns the sorted ids of every node that participates
// in at least one dependency cycle: members of any strongly connected
// component of size >= 2, plus nodes with a self-edge. This matches the set
// the recursive SQL reported (nodes with some simple path back to themselves).
func dependencyCycleNodes(edges map[string][]string) []string {
	search := newDependencyCycleSearch(edges)
	for root := range edges {
		if _, seen := search.index[root]; seen {
			continue
		}
		walkDependencyCycleGraph(search, root)
	}

	sort.Strings(search.cycleNodes)
	return search.cycleNodes
}

func newDependencyCycleSearch(edges map[string][]string) *dependencyCycleSearch {
	return &dependencyCycleSearch{
		edges:   edges,
		index:   make(map[string]int, len(edges)),
		low:     make(map[string]int, len(edges)),
		onStack: make(map[string]bool, len(edges)),
	}
}

func walkDependencyCycleGraph(search *dependencyCycleSearch, root string) {
	frames := []dependencyCycleFrame{{node: root}}
	for dependencyCycleFramesRemain(frames) {
		frame := dependencyCycleCurrentFrame(frames)
		v := frame.node
		if frame.succ == 0 {
			startDependencyCycleNode(search, v)
		}

		if w, descended := nextDependencyCycleSuccessor(search, frame); descended {
			frames = append(frames, dependencyCycleFrame{node: w})
			continue
		}

		finishDependencyCycleNode(search, v)
		frames = frames[:len(frames)-1]
		if dependencyCycleFramesRemain(frames) {
			updateDependencyCycleParent(search, frames, v)
		}
	}
}

func dependencyCycleFramesRemain(frames []dependencyCycleFrame) bool {
	return len(frames) > 0
}

func dependencyCycleCurrentFrame(frames []dependencyCycleFrame) *dependencyCycleFrame {
	return &frames[len(frames)-1]
}

func startDependencyCycleNode(search *dependencyCycleSearch, node string) {
	search.index[node] = search.next
	search.low[node] = search.next
	search.next++
	search.sccStack = append(search.sccStack, node)
	search.onStack[node] = true
}

func nextDependencyCycleSuccessor(search *dependencyCycleSearch, frame *dependencyCycleFrame) (string, bool) {
	successors := search.edges[frame.node]
	successorCount := len(successors)
	for frame.succ < successorCount {
		w := successors[frame.succ]
		frame.succ++
		if _, seen := search.index[w]; !seen {
			return w, true
		}
		if search.onStack[w] && search.index[w] < search.low[frame.node] {
			search.low[frame.node] = search.index[w]
		}
	}
	return "", false
}

func finishDependencyCycleNode(search *dependencyCycleSearch, node string) {
	if search.low[node] != search.index[node] {
		return
	}
	scc := popSCC(&search.sccStack, search.onStack, node)
	if len(scc) > 1 || hasSelfEdge(search.edges, node) {
		search.cycleNodes = append(search.cycleNodes, scc...)
	}
}

func updateDependencyCycleParent(search *dependencyCycleSearch, frames []dependencyCycleFrame, child string) {
	parent := dependencyCycleCurrentFrame(frames).node
	if search.low[child] < search.low[parent] {
		search.low[parent] = search.low[child]
	}
}

func popSCC(sccStack *[]string, onStack map[string]bool, v string) []string {
	var scc []string
	for {
		s := *sccStack
		w := s[len(s)-1]
		*sccStack = s[:len(s)-1]
		onStack[w] = false
		scc = append(scc, w)
		if w == v {
			return scc
		}
	}
}

func hasSelfEdge(edges map[string][]string, v string) bool {
	for _, w := range edges[v] {
		if w == v {
			return true
		}
	}
	return false
}
