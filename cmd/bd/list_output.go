package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"text/template"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
)

// printTruncationHint emits a one-line notice to stderr when the list output
// was truncated by --limit, so users and agents can't mistake a partial view
// for a complete one (GH#3212, GH#788).
func printTruncationHint(truncated bool, effectiveLimit int) {
	if !truncated || effectiveLimit <= 0 || !ui.IsStderrTerminal() {
		return
	}
	msg := fmt.Sprintf("\nShowing %d issues; more results matched but were hidden by --limit. Use --limit 0 for all, or --limit N to raise the cap.\n", effectiveLimit)
	fmt.Fprint(os.Stderr, ui.RenderWarn(msg))
}

func outputDotFormat(out io.Writer, issues []*types.Issue, depsByIssueID map[string][]*types.Dependency) error {
	w := &graphExportWriter{out: out}
	w.println("digraph dependencies {")
	w.println("  rankdir=TB;")
	w.println("  node [shape=box, style=rounded];")
	w.println()

	issueMap := indexDOTIssues(issues)
	writeDOTIssueNodes(w, issues)
	w.println()

	writeDOTIssueEdges(w, issues, depsByIssueID, issueMap)

	w.println("}")
	return w.wrapError("DOT")
}

func indexDOTIssues(issues []*types.Issue) map[string]*types.Issue {
	issueMap := make(map[string]*types.Issue, len(issues))
	for _, issue := range issues {
		issueMap[issue.ID] = issue
	}
	return issueMap
}

func writeDOTIssueNodes(w *graphExportWriter, issues []*types.Issue) {
	// Output nodes with labels including ID, type, priority, and status.
	for _, issue := range issues {
		label := fmt.Sprintf("%s\n[%s P%d]\n%s\n(%s)",
			issue.ID, issue.IssueType, issue.Priority, issue.Title, issue.Status)
		fillColor, fontColor := dotIssueColors(issue.Status)
		w.printf("  %q [label=%q, style=\"rounded,filled\", fillcolor=%q, fontcolor=%q];\n",
			issue.ID, label, fillColor, fontColor)
	}
}

func dotIssueColors(status types.Status) (fillColor, fontColor string) {
	fillColor = "white"
	fontColor = "black"
	switch status {
	case "closed":
		fillColor = "lightgray"
		fontColor = "dimgray"
	case "in_progress":
		fillColor = "lightyellow"
	case "blocked":
		fillColor = "lightcoral"
	}
	return fillColor, fontColor
}

func writeDOTIssueEdges(w *graphExportWriter, issues []*types.Issue, depsByIssueID map[string][]*types.Dependency, issueMap map[string]*types.Issue) {
	// Output edges with labels for dependency type.
	for _, issue := range issues {
		for _, dep := range depsByIssueID[issue.ID] {
			if issueMap[dep.DependsOnID] == nil {
				continue
			}
			color, style := dotDependencyColors(dep.Type)
			w.printf("  %q -> %q [label=%q, color=%s, style=%s];\n",
				issue.ID, dep.DependsOnID, dep.Type, color, style)
		}
	}
}

func dotDependencyColors(depType types.DependencyType) (color, style string) {
	color = "black"
	style = "solid"
	switch depType {
	case "blocks":
		color = "red"
		style = "bold"
	case "parent-child":
		color = "blue"
	case "discovered-from":
		color = "green"
		style = "dashed"
	case "related":
		color = "gray"
		style = "dashed"
	}
	return color, style
}

func outputFormattedList(out io.Writer, issues []*types.Issue, depsByIssueID map[string][]*types.Dependency, formatStr string) error {
	// Handle special 'dot' format (Graphviz output)
	if formatStr == "dot" {
		return outputDotFormat(out, issues, depsByIssueID)
	}
	w := &graphExportWriter{out: out}

	tmpl, err := parseListTemplate(formatStr)
	if err != nil {
		return fmt.Errorf("invalid format template: %w", err)
	}

	issueMap := indexFormattedIssues(issues)
	if err := writeFormattedDependencies(w, tmpl, issues, depsByIssueID, issueMap); err != nil {
		return err
	}
	return w.wrapError("formatted list")
}

func parseListTemplate(formatStr string) (*template.Template, error) {
	if formatStr == "digraph" {
		formatStr = "{{.IssueID}} {{.DependsOnID}}"
	}
	return template.New("format").Parse(formatStr)
}

func indexFormattedIssues(issues []*types.Issue) map[string]bool {
	issueMap := make(map[string]bool, len(issues))
	for _, issue := range issues {
		issueMap[issue.ID] = true
	}
	return issueMap
}

func writeFormattedDependencies(w *graphExportWriter, tmpl *template.Template, issues []*types.Issue, depsByIssueID map[string][]*types.Dependency, issueMap map[string]bool) error {
	// For each issue, output its dependencies using the template.
	for _, issue := range issues {
		for _, dep := range depsByIssueID[issue.ID] {
			if !issueMap[dep.DependsOnID] {
				continue
			}
			data := map[string]interface{}{
				"IssueID": issue.ID, "DependsOnID": dep.DependsOnID,
				"Type": dep.Type, "Issue": issue, "Dependency": dep,
			}
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, data); err != nil {
				return fmt.Errorf("template execution error: %w", err)
			}
			w.println(buf.String())
			if err := w.wrapError("formatted list"); err != nil {
				return err
			}
		}
	}
	return nil
}
