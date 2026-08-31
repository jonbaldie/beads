package main

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// deferredImportEdges carries the dependencies of one row that could not be
// wired inline with its phase-1 write and must be applied in the dependency
// pass.
type deferredImportEdges struct {
	issue *types.Issue
	deps  []*types.Dependency
}

// partitionChunkedImportDeps splits each ordered row's dependencies into edges
// wired inline with the row (target outside the batch, or first written in the
// same or an earlier chunk) and edges deferred to the dependency pass. It
// narrows each issue's dependency slice to the inline subset in place and
// returns the deferred edges. orderImportIssuesForChunking guarantees every
// readiness edge owned by a non-cycle row points at the same or an earlier
// chunk, so only non-readiness edges (related, discovered-from) into a later
// chunk and the readiness edges of force-emitted cycle members (their own cyclic
// edges, plus any acyclic blocker they were emitted ahead of) are ever deferred
// here.
func partitionChunkedImportDeps(ordered []*types.Issue) []deferredImportEdges {
	firstChunkOf := make(map[string]int, len(ordered))
	for pos, issue := range ordered {
		if issue.ID == "" {
			continue // ID assigned by the engine at insert; nothing can reference it yet
		}
		if _, ok := firstChunkOf[issue.ID]; !ok {
			firstChunkOf[issue.ID] = pos / importChunkSize
		}
	}
	var deferred []deferredImportEdges
	for pos, issue := range ordered {
		if len(issue.Dependencies) == 0 {
			continue
		}
		inline, later := splitDepsByChunk(issue.Dependencies, pos/importChunkSize, firstChunkOf)
		if len(later) == 0 {
			continue
		}
		issue.Dependencies = inline
		deferred = append(deferred, deferredImportEdges{issue: issue, deps: later})
	}
	return deferred
}

// splitDepsByChunk partitions one row's dependencies (the row lands in rowChunk)
// into edges that can be wired inline and edges whose target is first written in
// a later chunk and so must be deferred.
func splitDepsByChunk(deps []*types.Dependency, rowChunk int, firstChunkOf map[string]int) (inline, later []*types.Dependency) {
	for _, dep := range deps {
		if targetChunk, inBatch := firstChunkOf[dep.DependsOnID]; inBatch && targetChunk > rowChunk {
			later = append(later, dep)
			continue
		}
		inline = append(inline, dep)
	}
	return inline, later
}

// writeImportRowChunks writes the ordered rows (dependencies already narrowed to
// their inline subset) in bounded transactions, pausing between commits.
func writeImportRowChunks(ctx context.Context, store storage.DoltStorage, ordered []*types.Issue, actor string, rowOpts storage.BatchCreateOptions, pacer *importChunkPacer) error {
	total := len(ordered)
	chunks := (total + importChunkSize - 1) / importChunkSize
	for start, chunk := 0, 1; start < total; start, chunk = start+importChunkSize, chunk+1 {
		end := min(start+importChunkSize, total)
		paceImportTransaction(pacer)
		if err := store.CreateIssuesWithFullOptions(ctx, ordered[start:end], actor, rowOpts); err != nil {
			return fmt.Errorf("import chunk %d/%d failed, %d issues already committed (committed rows are durable; re-run the import to resume — it converges): %w", chunk, chunks, start, err)
		}
		fmt.Fprintf(importProgress, "bd import: %d/%d issues committed\n", end, total) //nolint:gosec // G705: stderr, not a browser context
	}
	return nil
}

// wireDeferredImportDeps applies the deferred edges once every target row exists,
// without rewriting the rows themselves. rowTotal is the count of phase-1 rows
// already committed, used only for the resume message.
func wireDeferredImportDeps(ctx context.Context, store storage.DoltStorage, deferred []deferredImportEdges, phase1Stale map[string]struct{}, rowTotal int, actor string, opts storage.BatchCreateOptions, pacer *importChunkPacer) error {
	depRows := make([]*types.Issue, 0, len(deferred))
	for _, d := range deferred {
		if _, stale := phase1Stale[d.issue.ID]; stale {
			continue // stale snapshot: its deps stay out too (bd-578h9.8)
		}
		cp := *d.issue
		cp.Dependencies = d.deps
		// The row landed in phase 1 and its labels/comments merged there;
		// this pass carries edges only.
		cp.Labels = nil
		cp.Comments = nil
		depRows = append(depRows, &cp)
	}
	if len(depRows) == 0 {
		return nil
	}
	depOpts := opts
	// Never rewrite an existing row here: the import's row write already
	// happened in phase 1, and a concurrent update since then must win. With
	// ConflictSkip the engine leaves the stored row untouched and still wires
	// the batch's dependencies.
	depOpts.ConflictSkip = true
	// No row write can be stale-rejected under ConflictSkip; leaving the
	// callback unset keeps a phase-2 signal from ever misreporting a row
	// whose phase-1 write committed.
	depOpts.OnStaleRejected = nil
	depTotal := len(depRows)
	depChunks := (depTotal + importChunkSize - 1) / importChunkSize
	for start, chunk := 0, 1; start < depTotal; start, chunk = start+importChunkSize, chunk+1 {
		end := min(start+importChunkSize, depTotal)
		paceImportTransaction(pacer)
		if err := store.CreateIssuesWithFullOptions(ctx, depRows[start:end], actor, depOpts); err != nil {
			return fmt.Errorf("import dependency pass chunk %d/%d failed (all %d issue rows are committed; re-run the import to resume — it converges): %w", chunk, depChunks, rowTotal, err)
		}
		fmt.Fprintf(importProgress, "bd import: deferred dependencies wired for %d/%d issues\n", end, depTotal) //nolint:gosec // G705: stderr, not a browser context
	}
	return nil
}

// orderImportIssuesForChunking returns the issues reordered so that every valid
// readiness-affecting edge (blocks, parent-child, conditional-blocks, waits-for;
// the types GetReadyWork consults) points at a row in the same or an earlier
// chunk, which lets the import wire that edge in the same transaction as the
// row. It runs Kahn's algorithm over the intra-batch readiness edges, seeded in
// file order so unconstrained rows keep their relative order; duplicate IDs are
// chained in file order to preserve last-row-wins upsert semantics.
//
// A readiness cycle (invalid for blocking types; only ever present in the
// corrupted or legacy JSONL the import tolerates) cannot be fully ordered.
// Appending the stalled rows in plain file order — as an earlier version did —
// can place a valid dependent of a cycle before the cycle member it blocks on,
// deferring that live readiness edge and briefly exposing the dependent as ready
// without its blocker. Instead each stall is broken by emitting a row that
// genuinely lies on a cycle, so every readiness edge owned by a non-cycle row
// still rides inline; only a force-emitted cycle member may defer a readiness
// edge — its own unsatisfiable cycle edge, or a valid acyclic edge whose blocker
// it was emitted ahead of — and the engine skip-reports the unsatisfiable ones,
// still breaking the cycle. For a cycle of length
// >=3 the edge left to defer (and hence which member is left spuriously ready)
// can differ from the one the single-transaction import drops, which checks all
// cycle edges in file order against the already-persisted set; both break it.
func orderImportIssuesForChunking(issues []*types.Issue) []*types.Issue {
	if len(issues) < 2 {
		return issues
	}
	return buildImportOrderGraph(issues).topologicalFileOrder(issues)
}

// importOrderGraph is the intra-batch readiness-edge graph over import rows,
// indexed by position in the input slice. An edge target->dependent means the
// target (a readiness blocker, or an earlier duplicate of the same ID) must be
// emitted no later than the dependent.
type importOrderGraph struct {
	dependents [][]int // dependents[t]: rows released when row t is emitted
	blockers   [][]int // blockers[u]: rows that must be emitted before row u
	indegree   []int
}

func buildImportOrderGraph(issues []*types.Issue) importOrderGraph {
	n := len(issues)
	indicesByID := indexImportIssuesByID(issues)
	// A waits-for waiter's is_blocked state is gated on whether its spawner has
	// an active parent-child child, not on the spawner row itself, so those
	// child rows are readiness inputs to the waiter (see addRowReadinessEdges).
	// Index them by spawner up front so the edge pass can order them.
	childrenBySpawnerID := parentChildChildrenBySpawner(issues)
	g := importOrderGraph{
		dependents: make([][]int, n),
		blockers:   make([][]int, n),
		indegree:   make([]int, n),
	}
	for i, issue := range issues {
		g.addRowReadinessEdges(i, issue, indicesByID, childrenBySpawnerID)
	}
	// Chain duplicate IDs in file order so the last row wins the upsert.
	chainImportDuplicateIDs(g, indicesByID)
	return g
}

// chainImportDuplicateIDs orders duplicate rows in file order so the final
// row retains the import's last-row-wins upsert semantics.
func chainImportDuplicateIDs(g importOrderGraph, indicesByID map[string][]int) {
	for _, indices := range indicesByID {
		for k := range indices[1:] {
			g.addEdge(indices[k], indices[k+1])
		}
	}
}

// indexImportIssuesByID maps each import row's ID to its positions in the input
// slice. ID-less rows are skipped: their ID is assigned by the engine at insert,
// so nothing in the batch can reference them yet.
func indexImportIssuesByID(issues []*types.Issue) map[string][]int {
	indicesByID := make(map[string][]int, len(issues))
	for i, issue := range issues {
		if issue.ID == "" {
			continue
		}
		indicesByID[issue.ID] = append(indicesByID[issue.ID], i)
	}
	return indicesByID
}

// parentChildChildrenBySpawner maps each spawner ID to the positions of the
// in-batch rows that declare it as their parent-child parent. A parent-child
// dependency row is stored on the child (issue_id = child, depends_on = parent),
// so the child is the row carrying the edge.
func parentChildChildrenBySpawner(issues []*types.Issue) map[string][]int {
	children := make(map[string][]int)
	for i, issue := range issues {
		for _, dep := range issue.Dependencies {
			if dep != nil && dep.Type == types.DepParentChild && dep.DependsOnID != "" {
				children[dep.DependsOnID] = append(children[dep.DependsOnID], i)
			}
		}
	}
	return children
}

// addEdge records that row target must be emitted no later than row dependent.
func (g importOrderGraph) addEdge(target, dependent int) {
	g.dependents[target] = append(g.dependents[target], dependent)
	g.blockers[dependent] = append(g.blockers[dependent], target)
	g.indegree[dependent]++
}

// addEdgesBefore orders every target row no later than dependent, skipping any
// self-reference.
func (g importOrderGraph) addEdgesBefore(dependent int, targets []int) {
	for _, target := range targets {
		if target != dependent {
			g.addEdge(target, dependent)
		}
	}
}

// addRowReadinessEdges records the intra-batch ordering edges that row i imposes:
// every readiness blocker it names must be emitted no later than i, and for a
// waits-for edge the spawner's in-batch parent-child children must be too.
//
// The waiter's is_blocked state keys on whether its spawner has an active child,
// not on the spawner row itself, and the per-chunk is_blocked recompute only
// re-evaluates the rows in its own transaction. So a waiter whose spawner's child
// lands in a later chunk would compute is_blocked=0 against a still-childless
// spawner and never be re-blocked — spuriously ready for the rest of the import
// and after it. Ordering the spawner inline is not enough; the child must be
// committed by the time the waiter's chunk recomputes.
func (g importOrderGraph) addRowReadinessEdges(i int, issue *types.Issue, indicesByID, childrenBySpawnerID map[string][]int) {
	for _, dep := range issue.Dependencies {
		if dep == nil || dep.DependsOnID == "" || !dep.Type.AffectsReadyWork() {
			continue
		}
		g.addEdgesBefore(i, indicesByID[dep.DependsOnID])
		if dep.Type == types.DepWaitsFor {
			g.addEdgesBefore(i, childrenBySpawnerID[dep.DependsOnID])
		}
	}
}

// topologicalFileOrder emits rows in dependency order (Kahn's, file-order
// seeded), breaking each stall by force-emitting a row that lies on a readiness
// cycle, so every non-cycle row's readiness edges ride inline; only a
// force-emitted cycle member can leave a readiness edge (its unsatisfiable cycle
// edge, or a valid acyclic edge it was emitted ahead of) for the deferred
// dependency pass.
func (g importOrderGraph) topologicalFileOrder(issues []*types.Issue) []*types.Issue {
	n := len(issues)
	indegree := append([]int(nil), g.indegree...)
	emitted := make([]bool, n)
	queue := make([]int, 0, n)
	for i := range n {
		if indegree[i] == 0 {
			queue = append(queue, i)
		}
	}
	ordered := make([]*types.Issue, 0, n)
	for importOrderHasRowsRemaining(ordered, n) {
		if len(queue) == 0 {
			queue = append(queue, g.nextCycleParticipant(emitted))
		}
		i := queue[0]
		queue = queue[1:]
		if emitted[i] {
			continue
		}
		emitted[i] = true
		ordered = append(ordered, issues[i])
		queue = g.release(i, indegree, emitted, queue)
	}
	return ordered
}

// release decrements the indegree of every row that row i blocks and enqueues
// any that becomes free and has not already been emitted.
func (g importOrderGraph) release(i int, indegree []int, emitted []bool, queue []int) []int {
	for _, j := range g.dependents[i] {
		indegree[j]--
		if indegree[j] == 0 && !emitted[j] {
			queue = append(queue, j)
		}
	}
	return queue
}

func importOrderHasRowsRemaining(ordered []*types.Issue, total int) bool {
	return len(ordered) < total
}

// nextCycleParticipant returns an un-emitted row that lies on a readiness cycle,
// used to break a Kahn stall.
func (g importOrderGraph) nextCycleParticipant(emitted []bool) int {
	start := 0
	for importOrderRowEmitted(start, emitted) {
		start++
	}
	return g.cycleParticipant(start, emitted)
}

func importOrderRowEmitted(index int, emitted []bool) bool {
	return index < len(emitted) && emitted[index]
}

// cycleParticipant walks blocker edges from an un-emitted row until it revisits
// a row, which therefore lies on a cycle. At a stall every un-emitted row still
// has an un-emitted blocker, so the walk always closes a cycle.
func (g importOrderGraph) cycleParticipant(start int, emitted []bool) int {
	seen := make(map[int]bool)
	v := start
	for !seen[v] {
		seen[v] = true
		next := -1
		for _, b := range g.blockers[v] {
			if !emitted[b] {
				next = b
				break
			}
		}
		if next == -1 {
			return v // defensive: no un-emitted blocker (unexpected at a stall)
		}
		v = next
	}
	return v
}
