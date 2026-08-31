package main

import "github.com/jonbaldie/beads/internal/workapi"

func registerListQueryFlags() {
	listCmd.Flags().StringP("status", "s", "", "Filter by stored status (open, in_progress, blocked, deferred, closed). Comma-separated for multiple: --status open,in_progress. Note: repeating -s/--status silently overwrites the previous value — always use the comma-separated form for multi-status filters.")
	listCmd.Flags().String("state", "", "Alias for --status")
	_ = listCmd.Flags().MarkHidden("state")
	registerPriorityFlag(listCmd, "")
	listCmd.Flags().StringP("assignee", "a", "", "Filter by assignee")
	listCmd.Flags().StringP("type", "t", "", "Filter by type (bug, feature, task, epic, chore, decision, merge-request, molecule, gate, convoy). Aliases: mr→merge-request, feat→feature, mol→molecule, dec/adr→decision")
	listCmd.Flags().StringSliceP("label", "l", []string{}, "Filter by labels (AND: must have ALL). Can combine with --label-any")
	listCmd.Flags().StringSlice("label-any", []string{}, "Filter by labels (OR: must have AT LEAST ONE). Can combine with --label")
	listCmd.Flags().StringSlice("exclude-label", []string{}, "Exclude issues that have ANY of these labels")
	listCmd.Flags().String("label-pattern", "", "Filter by label glob pattern (e.g., 'tech-*' matches tech-debt, tech-legacy)")
	listCmd.Flags().String("label-regex", "", "Filter by label regex pattern (e.g., 'tech-(debt|legacy)')")
	listCmd.Flags().String("title", "", "Filter by title text (case-insensitive substring match)")
	listCmd.Flags().String("spec", "", "Filter by spec_id prefix")
	listCmd.Flags().String("id", "", "Filter by specific issue IDs (comma-separated, e.g., bd-1,bd-5,bd-10)")
	listCmd.Flags().IntP("limit", "n", workapi.DefaultListLimit, "Limit results (default 50, use 0 for unlimited)")
	listCmd.Flags().Int("offset", 0, "Skip the first N matching results (0-based). Only supported under --proxied-server.")
	listCmd.Flags().String("format", "", "Output format: 'digraph' (for golang.org/x/tools/cmd/digraph), 'dot' (Graphviz), or Go template")
	listCmd.Flags().Bool("all", false, "Show all issues including closed (overrides default filter)")
	listCmd.Flags().Bool("long", false, "Show detailed multi-line output for each issue")
	listCmd.Flags().String("sort", "", "Sort by field: priority, created, updated, closed, status, id, title, type, assignee")
	listCmd.Flags().BoolP("reverse", "r", false, "Reverse sort order")
}

func registerListFilterFlags() {
	listCmd.Flags().String("title-contains", "", "Filter by title substring (case-insensitive)")
	listCmd.Flags().String("desc-contains", "", "Filter by description substring (case-insensitive)")
	listCmd.Flags().String("notes-contains", "", "Filter by notes substring (case-insensitive)")
	listCmd.Flags().String("external-contains", "", "Filter by external ref substring (case-insensitive)")
	listCmd.Flags().String("external-ref", "", "Filter by exact external_ref value")
	listCmd.Flags().String("created-after", "", "Filter issues created after date (YYYY-MM-DD or RFC3339)")
	listCmd.Flags().String("created-before", "", "Filter issues created before date (YYYY-MM-DD or RFC3339)")
	listCmd.Flags().String("updated-after", "", "Filter issues updated after date (YYYY-MM-DD or RFC3339)")
	listCmd.Flags().String("updated-before", "", "Filter issues updated before date (YYYY-MM-DD or RFC3339)")
	listCmd.Flags().String("closed-after", "", "Filter issues closed after date (YYYY-MM-DD or RFC3339)")
	listCmd.Flags().String("closed-before", "", "Filter issues closed before date (YYYY-MM-DD or RFC3339)")
	listCmd.Flags().Bool("empty-description", false, "Filter issues with empty or missing description")
	listCmd.Flags().Bool("no-assignee", false, "Filter issues with no assignee")
	listCmd.Flags().Bool("no-labels", false, "Filter issues with no labels")
	listCmd.Flags().Bool("skip-labels", false,
		"Skip label hydration. The labels field in output will be empty regardless "+
			"of actual labels. Use only when the caller does not depend on label data. "+
			"Cannot combine with --label, --label-any, --label-pattern, --label-regex, "+
			"--exclude-label, or --no-labels.")
	listCmd.Flags().Bool("brief", false,
		"Omit the free-form text (description, design, acceptance criteria, notes, "+
			"payload, waiters) from each row. Filters that read those fields, such as "+
			"--desc-contains, still select on them. An omitted field is"+
			" indistinguishable from an empty one in --json; fetch a whole issue"+
			" with bd show.")
	listCmd.Flags().String("priority-min", "", "Filter by minimum priority (inclusive, 0-4 or P0-P4)")
	listCmd.Flags().String("priority-max", "", "Filter by maximum priority (inclusive, 0-4 or P0-P4)")
	listCmd.Flags().Bool("pinned", false, "Show only pinned issues")
	listCmd.Flags().Bool("no-pinned", false, "Exclude pinned issues")
	listCmd.Flags().Bool("include-templates", false, "Include template molecules in output")
	listCmd.Flags().Bool("include-gates", false, "Include gate issues in output (normally hidden)")
	listCmd.Flags().Bool("include-infra", false, "Include infrastructure beads (agent/role/message) in output")
	listCmd.Flags().StringSlice("exclude-type", nil, "Exclude issue types from results (comma-separated or repeatable, e.g., --exclude-type=convoy,epic)")
}

func registerListScopeFlags() {
	listCmd.Flags().String("parent", "", "Filter by parent issue ID (shows children of specified issue)")
	listCmd.Flags().String("filter-parent", "", "Alias for --parent")
	_ = listCmd.Flags().MarkHidden("filter-parent") // Only fails if flag missing (caught in tests)
	listCmd.Flags().Bool("no-parent", false, "Exclude child issues (show only top-level issues)")
	listCmd.Flags().String("mol-type", "", "Filter by molecule type: swarm, patrol, or work")
	listCmd.Flags().String("wisp-type", "", "Filter by wisp type: heartbeat, ping, patrol, gc_report, recovery, error, escalation")
	listCmd.Flags().Bool("deferred", false, "Show only issues with defer_until set")
	listCmd.Flags().String("defer-after", "", "Filter issues deferred after date (supports relative: +6h, tomorrow)")
	listCmd.Flags().String("defer-before", "", "Filter issues deferred before date (supports relative: +6h, tomorrow)")
	listCmd.Flags().String("due-after", "", "Filter issues due after date (supports relative: +6h, tomorrow)")
	listCmd.Flags().String("due-before", "", "Filter issues due before date (supports relative: +6h, tomorrow)")
	listCmd.Flags().Bool("overdue", false, "Show only issues with due_at in the past (not closed)")
	listCmd.Flags().Bool("pretty", false, "Display issues in a tree format with status/priority symbols")
	listCmd.Flags().Bool("tree", true, "Hierarchical tree format (default: true; use --flat to disable)")
	listCmd.Flags().Bool("flat", false, "Disable tree format and use legacy flat list output")
	listCmd.Flags().BoolP("watch", "w", false, "Watch for changes and auto-update display (implies --pretty)")
	listCmd.Flags().String("deps", "", "Annotate tree with dependency edges and order siblings by them: 'scheduling' (bare --deps) or 'all'")
	if f := listCmd.Flags().Lookup("deps"); f != nil {
		f.NoOptDefVal = "scheduling"
	}
	listCmd.Flags().StringArray("metadata-field", nil, "Filter by metadata field (key=value, repeatable)")
	listCmd.Flags().String("has-metadata-key", "", "Filter issues that have this metadata key set")
	listCmd.Flags().Bool("no-pager", false, "Disable pager output")
	listCmd.Flags().Bool("ready", false, "Show only ready issues (no active blockers, same semantics as bd ready)")
	addRoutedMaxRowsFlag(listCmd)
}
