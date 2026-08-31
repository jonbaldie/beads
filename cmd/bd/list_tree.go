package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
)

// depRender carries the state needed to annotate and order a --deps tree.
// A nil *depRender means the feature is off; its methods are nil-safe.
type depRender struct {
	mode    string                         // "scheduling" or "all"
	allDeps map[string][]*types.Dependency // outgoing edges keyed by issue_id
	inView  map[string]*types.Issue        // displayed issues, for titles + in-view test
}

type depAnnotationRow struct{ label, target, title string }

// annotationsFor prints the dependency-edge annotation rows for a node, indented
// to childPrefix so they align with (and sit just above) the node's children.
// No-op when the receiver is nil (--deps off).
func (dr *depRender) annotationsFor(nodeID, childPrefix string) {
	if dr == nil {
		return
	}
	inView, outView := dr.annotationRows(nodeID)
	if len(inView) == 0 && len(outView) == 0 {
		return
	}
	printInViewAnnotations(inView, childPrefix)
	printOutOfViewAnnotations(outView, childPrefix)
}

func (dr *depRender) annotationRows(nodeID string) ([]depAnnotationRow, []string) {
	var inView []depAnnotationRow
	var outView []string
	seen := make(map[string]bool)
	for _, dep := range dr.allDeps[nodeID] {
		label, scheduling, ok := depEdgeDisplay(dep.Type)
		if !ok {
			continue // parent-child: hierarchy, not a dependency
		}
		if dr.mode != "all" && !scheduling {
			continue // scheduling mode hides knowledge-graph edges
		}
		key := string(dep.Type) + "\x00" + dep.DependsOnID
		if seen[key] {
			continue
		}
		seen[key] = true
		if issue := dr.inView[dep.DependsOnID]; issue != nil {
			inView = append(inView, depAnnotationRow{label: label, target: dep.DependsOnID, title: issue.Title})
		} else {
			outView = append(outView, dep.DependsOnID)
		}
	}
	return inView, outView
}

func printInViewAnnotations(rows []depAnnotationRow, childPrefix string) {
	slices.SortFunc(rows, func(a, b depAnnotationRow) int {
		if a.label != b.label {
			return cmp.Compare(a.label, b.label)
		}
		return utils.NaturalCompareIDs(a.target, b.target)
	})
	for _, r := range rows {
		tag := ui.RenderMuted(fmt.Sprintf("%s %-20s", depGlyph, "["+r.label+"]"))
		fmt.Println(childPrefix + tag + " " + r.target + " " + r.title)
	}
}

func printOutOfViewAnnotations(rows []string, childPrefix string) {
	if len(rows) == 0 {
		return
	}
	slices.SortFunc(rows, utils.NaturalCompareIDs)
	const maxNamed = 4
	named, suffix := rows, ""
	if len(rows) > maxNamed {
		named = rows[:maxNamed]
		suffix = fmt.Sprintf(", +%d more", len(rows)-maxNamed)
	}
	summary := fmt.Sprintf("%s ↗ %d outside this view: %s%s",
		depGlyph, len(rows), strings.Join(named, ", "), suffix)
	fmt.Println(childPrefix + ui.RenderMuted(summary))
}

// buildIssueTree builds parent-child tree structure from issues
// Uses actual parent-child dependencies from the database when store is provided
func buildIssueTree(issues []*types.Issue) (roots []*types.Issue, childrenMap map[string][]*types.Issue) {
	return buildIssueTreeWithDeps(issues, nil)
}

// buildIssueTreeWithDeps builds parent-child tree using dependency records
// If allDeps is nil, falls back to dotted ID hierarchy (e.g., "parent.1")
// Only parent-child dependency edges establish nesting; other edge types
// (blocks, waits-for, discovered-from, relates-to, ...) are workflow/graph
// links and are not rendered as hierarchy.
func buildIssueTreeWithDeps(issues []*types.Issue, allDeps map[string][]*types.Dependency) (roots []*types.Issue, childrenMap map[string][]*types.Issue) {
	issueMap := indexIssues(issues)
	childrenMap = make(map[string][]*types.Issue)
	isChild := make(map[string]bool)
	addDependencyChildren(allDeps, issueMap, childrenMap, isChild)
	addDottedIDChildren(issues, issueMap, childrenMap, isChild)
	roots = rootIssues(issues, isChild)
	slices.SortFunc(roots, compareIssuesByPriority)
	for parentID := range childrenMap {
		slices.SortFunc(childrenMap[parentID], compareIssuesByPriority)
	}
	return roots, childrenMap
}

func indexIssues(issues []*types.Issue) map[string]*types.Issue {
	indexed := make(map[string]*types.Issue, len(issues))
	for _, issue := range issues {
		indexed[issue.ID] = issue
	}
	return indexed
}

func addDependencyChildren(allDeps map[string][]*types.Dependency, issues map[string]*types.Issue, children map[string][]*types.Issue, isChild map[string]bool) {
	added := make(map[string]bool)
	for issueID, deps := range allDeps {
		for _, dep := range deps {
			child, childExists := issues[issueID]
			_, parentExists := issues[dep.DependsOnID]
			if dep.Type != types.DepParentChild || !childExists || !parentExists {
				continue
			}
			key := dep.DependsOnID + ":" + issueID
			if !added[key] {
				children[dep.DependsOnID] = append(children[dep.DependsOnID], child)
				added[key] = true
			}
			isChild[issueID] = true
		}
	}
}

func addDottedIDChildren(issues []*types.Issue, indexed map[string]*types.Issue, children map[string][]*types.Issue, isChild map[string]bool) {
	for _, issue := range issues {
		if isChild[issue.ID] || !strings.Contains(issue.ID, ".") {
			continue
		}
		parts := strings.Split(issue.ID, ".")
		parentID := strings.Join(parts[:len(parts)-1], ".")
		if _, exists := indexed[parentID]; exists {
			children[parentID] = append(children[parentID], issue)
			isChild[issue.ID] = true
		}
	}
}

func rootIssues(issues []*types.Issue, isChild map[string]bool) []*types.Issue {
	roots := make([]*types.Issue, 0, len(issues))
	for _, issue := range issues {
		if !isChild[issue.ID] {
			roots = append(roots, issue)
		}
	}
	return roots
}

// compareIssuesByPriority provides stable sorting for tree display
// Primary sort: priority (P0 before P1 before P2...)
// Secondary sort: ID for deterministic ordering when priorities match
func compareIssuesByPriority(a, b *types.Issue) int {
	// Primary: priority (ascending: P0 before P1 before P2...)
	if result := cmp.Compare(a.Priority, b.Priority); result != 0 {
		return result
	}
	// Secondary: ID for deterministic order when priorities match
	return utils.NaturalCompareIDs(a.ID, b.ID)
}

// printPrettyTree recursively prints the issue tree.
// Children are ordered by dependency then priority when dr != nil (--deps), else
// by priority (P0 first) for intuitive reading. When dr is set, each node's
// dependency edges are annotated just beneath it.
func printPrettyTree(childrenMap map[string][]*types.Issue, parentID string, prefix string, dr *depRender) {
	children := childrenMap[parentID]

	if dr != nil {
		children = orderSiblingsByDeps(children, dr.allDeps)
	} else {
		// Sort children by priority using same comparison as roots for consistency
		slices.SortFunc(children, compareIssuesByPriority)
	}

	for i, child := range children {
		isLast := i == len(children)-1
		connector := "├── "
		if isLast {
			connector = "└── "
		}
		fmt.Printf("%s%s%s\n", prefix, connector, formatPrettyIssue(child))

		extension := "│   "
		if isLast {
			extension = "    "
		}
		dr.annotationsFor(child.ID, prefix+extension)
		printPrettyTree(childrenMap, child.ID, prefix+extension, dr)
	}
}

// displayPrettyList displays issues in pretty tree format (GH#654)
// Uses buildIssueTree which only supports dotted ID hierarchy
func displayPrettyList(issues []*types.Issue, showHeader bool) {
	displayPrettyListWithDeps(issues, showHeader, nil, false)
}

// displayPrettyListWithDeps displays issues in tree format using dependency data.
func displayPrettyListWithDeps(issues []*types.Issue, showHeader bool, allDeps map[string][]*types.Dependency, truncated bool) {
	displayPrettyListWithDepsMode(issues, showHeader, allDeps, "", truncated)
}

// displayPrettyListWithDepsMode displays issues in tree format. When depsMode is
// "scheduling" or "all", the tree also annotates each node's dependency edges and
// orders siblings by their scheduling dependencies (see orderSiblingsByDeps). An
// empty depsMode is the plain parent-child tree. truncated means the page was cut
// by --limit; the summary then says "Showing N" instead of "Total: N" (GH#5362).
func displayPrettyListWithDepsMode(issues []*types.Issue, showHeader bool, allDeps map[string][]*types.Dependency, depsMode string, truncated bool) {
	if showHeader {
		printTreeHeader()
	}
	if len(issues) == 0 {
		fmt.Println("No issues found.")
		return
	}
	roots, childrenMap := buildIssueTreeWithDeps(issues, allDeps)
	dr := newDepRender(depsMode, allDeps, issues)
	if dr != nil {
		roots = orderSiblingsByDeps(roots, allDeps)
	}
	for _, issue := range roots {
		fmt.Println(formatPrettyIssue(issue))
		dr.annotationsFor(issue.ID, "")
		printPrettyTree(childrenMap, issue.ID, "", dr)
	}
	printTreeSummary(issues, truncated, dr != nil)
}

func printTreeHeader() {
	fmt.Print("\033[2J\033[H")
	fmt.Println(strings.Repeat("=", 80))
	fmt.Printf("Beads - Open & In Progress (%s)\n", time.Now().Format("15:04:05"))
	fmt.Println(strings.Repeat("=", 80))
	fmt.Println()
}

func newDepRender(mode string, allDeps map[string][]*types.Dependency, issues []*types.Issue) *depRender {
	if mode == "" {
		return nil
	}
	return &depRender{mode: mode, allDeps: allDeps, inView: indexIssues(issues)}
}

func printTreeSummary(issues []*types.Issue, truncated, showDeps bool) {
	openCount, inProgressCount := countTreeStatuses(issues)
	fmt.Println()
	fmt.Println(strings.Repeat("-", 80))
	printTreeCounts(len(issues), openCount, inProgressCount, truncated)
	fmt.Println()
	fmt.Println("Status: ○ open  ◐ in_progress  ● blocked  ✓ closed  ❄ deferred")
	fmt.Println("Priority: P0–P4 (label only; not a status icon)")
	if showDeps {
		fmt.Printf("Deps:   %s = depends-on / relationship (points to target); siblings ordered so dependencies come first; ↗ = target outside current view\n", depGlyph)
	}
}

func countTreeStatuses(issues []*types.Issue) (openCount, inProgressCount int) {
	for _, issue := range issues {
		switch issue.Status {
		case "open":
			openCount++
		case "in_progress":
			inProgressCount++
		}
	}
	return openCount, inProgressCount
}

func printTreeCounts(total, openCount, inProgressCount int, truncated bool) {
	if truncated {
		fmt.Printf("Showing %d issues (%d open, %d in progress); more match (truncated by --limit). Use --limit 0 for all.\n",
			total, openCount, inProgressCount)
	} else {
		fmt.Printf("Total: %d issues (%d open, %d in progress)\n", total, openCount, inProgressCount)
	}
}
