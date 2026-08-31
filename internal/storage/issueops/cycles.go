package issueops

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/types"
)

// DetectCyclesInTx finds dependency cycles across both the dependencies and
// wisp_dependencies tables. Returns slices of issues forming each cycle.
// Only considers "blocks" and "conditional-blocks" dependencies for cycle detection.
func DetectCyclesInTx(ctx context.Context, tx DBTX) ([][]*types.Issue, error) {
	// Build adjacency list from both dependency tables.
	graph := make(map[string][]string)
	if err := AppendBlockingGraphInTx(ctx, tx, []string{"dependencies", "wisp_dependencies"}, graph); err != nil {
		return nil, err
	}
	return findCyclesInGraph(ctx, tx, graph), nil
}

type cycleSearch struct {
	ctx      context.Context
	tx       DBTX
	graph    map[string][]string
	visited  map[string]bool
	recStack map[string]bool
	path     []string
	cycles   [][]*types.Issue
}

func findCyclesInGraph(ctx context.Context, tx DBTX, graph map[string][]string) [][]*types.Issue {
	search := cycleSearch{
		ctx:      ctx,
		tx:       tx,
		graph:    graph,
		visited:  make(map[string]bool),
		recStack: make(map[string]bool),
	}
	for node := range graph {
		if !search.visited[node] {
			search.visit(node)
		}
	}
	return search.cycles
}

func (search *cycleSearch) visit(node string) bool {
	search.visited[node] = true
	search.recStack[node] = true
	search.path = append(search.path, node)

	for _, neighbor := range search.graph[node] {
		if !search.visited[neighbor] {
			if search.visit(neighbor) {
				return true
			}
			continue
		}
		if search.recStack[neighbor] {
			search.recordCycle(neighbor)
		}
	}

	search.path = search.path[:len(search.path)-1]
	search.recStack[node] = false
	return false
}

func (search *cycleSearch) recordCycle(neighbor string) {
	cycleStart := -1
	for i, node := range search.path {
		if node == neighbor {
			cycleStart = i
			break
		}
	}
	if cycleStart < 0 {
		return
	}
	cycleIssues := search.loadCycleIssues(search.path[cycleStart:])
	if len(cycleIssues) > 0 {
		search.cycles = append(search.cycles, cycleIssues)
	}
}

func (search *cycleSearch) loadCycleIssues(cyclePath []string) []*types.Issue {
	var cycleIssues []*types.Issue
	for _, id := range cyclePath {
		issue, _ := GetIssueInTx(search.ctx, search.tx, id)
		if issue != nil {
			cycleIssues = append(cycleIssues, issue)
		}
	}
	return cycleIssues
}

// AppendBlockingGraphInTx adds the blocking-type ("blocks",
// "conditional-blocks") dependency edges from the given tables on tx into
// graph as adjacency lists. The caller may merge tables read from different
// transactions into one graph (dolt server mode keeps wisp writes on a
// separate ignored tx).
//
//nolint:gosec // G201: depTable is hardcoded to "dependencies" or "wisp_dependencies"
func AppendBlockingGraphInTx(ctx context.Context, tx DBTX, depTables []string, graph map[string][]string) error {
	return appendDependencyGraphInTx(ctx, tx, depTables, graph, false)
}

// AppendSchedulingGraphInTx adds blocks, conditional-blocks, and parent-child
// edges to graph for validating mutations against the combined scheduling
// graph. DetectCycles intentionally continues to use AppendBlockingGraphInTx.
func AppendSchedulingGraphInTx(ctx context.Context, tx DBTX, depTables []string, graph map[string][]string) error {
	return appendDependencyGraphInTx(ctx, tx, depTables, graph, true)
}

func appendDependencyGraphInTx(ctx context.Context, tx DBTX, depTables []string, graph map[string][]string, includeParentChild bool) error {
	for _, depTable := range depTables {
		if err := appendDependencyGraphTable(ctx, tx, depTable, graph, includeParentChild); err != nil {
			return err
		}
	}
	return nil
}

func appendDependencyGraphTable(ctx context.Context, tx DBTX, depTable string, graph map[string][]string, includeParentChild bool) error {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT issue_id, %s AS depends_on_id, type
		FROM %s
	`, DepTargetExpr, depTable))
	if err != nil {
		return fmt.Errorf("dependency graph: query %s: %w", depTable, err)
	}
	for rows.Next() {
		var issueID, dependsOnID, depType string
		if err := rows.Scan(&issueID, &dependsOnID, &depType); err != nil {
			_ = rows.Close()
			return fmt.Errorf("dependency graph: scan %s: %w", depTable, err)
		}
		if dependencyGraphEdgeAllowed(types.DependencyType(depType), includeParentChild) {
			graph[issueID] = append(graph[issueID], dependsOnID)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("dependency graph: rows %s: %w", depTable, err)
	}
	return nil
}

func dependencyGraphEdgeAllowed(depType types.DependencyType, includeParentChild bool) bool {
	return depType == types.DepBlocks || depType == types.DepConditionalBlocks ||
		(includeParentChild && depType == types.DepParentChild)
}

// CycleThroughEdgesInGraph reports a rendered cycle that traverses
// one of the new edges (issueID -> dependsOnID pairs), or "" when no new edge
// lies on a cycle. An edge u -> v is on a cycle exactly when u is reachable
// from v, so this is precise where cycle enumeration is not: a DFS-based
// detector records one cycle per back edge and can report a pre-existing
// cycle through the same nodes instead of the one the new edge created
// (bd-578h9.9). The graph must already contain the new edges.
func CycleThroughEdgesInGraph(graph map[string][]string, edges [][2]string) string {
	for _, edge := range edges {
		source, target := edge[0], edge[1]
		if source == "" || target == "" {
			continue
		}
		if source == target {
			return source + " → " + source
		}
		path := reachPath(graph, target, source)
		if path == nil {
			continue
		}
		// path runs target ⇝ source inclusive; the new edge closes the cycle.
		ids := append([]string{source}, path...)
		return strings.Join(ids, " → ")
	}
	return ""
}

// reachPath returns a BFS path from start to goal in graph (inclusive of
// both), or nil when goal is unreachable. start == goal returns [start].
func reachPath(graph map[string][]string, start, goal string) []string {
	if start == goal {
		return []string{start}
	}
	parent := map[string]string{start: ""}
	queue := []string{start}
	for {
		if len(queue) == 0 {
			break
		}
		node := queue[0]
		queue = queue[1:]
		for _, next := range graph[node] {
			if _, seen := parent[next]; seen {
				continue
			}
			parent[next] = node
			if next == goal {
				path := []string{goal}
				for at := node; at != ""; at = parent[at] {
					path = append(path, at)
				}
				// Reverse: built goal-back-to-start.
				for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
					path[i], path[j] = path[j], path[i]
				}
				return path
			}
			queue = append(queue, next)
		}
	}
	return nil
}
