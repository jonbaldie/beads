package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/spf13/cobra"
)

// storageExecutor handles operations that need a store connection
type storageExecutor func(store storage.DoltStorage) error

// withStorage executes an operation with either the direct store or a read-only store
func withStorage(ctx context.Context, store storage.DoltStorage, dbPath string, fn storageExecutor) error {
	if store != nil {
		return fn(store)
	} else if dbPath != "" {
		// Open read-only connection using repo metadata when available so
		// helper paths keep the correct Dolt database and server endpoint.
		roStore, err := openReadOnlyStoreForDBPath(ctx, dbPath)
		if err != nil {
			return err
		}
		defer func() { _ = roStore.Close() }() // Best effort cleanup
		return fn(roStore)
	}
	return fmt.Errorf("no storage available")
}

// issueSnapshot builds a comparable string from issue IDs, statuses, and
// update times so we can detect when the result set has changed.
func issueSnapshot(issues []*types.Issue) string {
	var b strings.Builder
	for _, issue := range issues {
		fmt.Fprintf(&b, "%s:%s:%d;", issue.ID, issue.Status, issue.UpdatedAt.UnixNano())
	}
	return b.String()
}

// skipLabelsIssueView wraps IssueWithCounts so the JSON encoder always emits
// `labels: []` regardless of the omitempty tag on Issue.Labels. AD-02 contract:
// with --skip-labels, every issue's labels field is present and empty.
type skipLabelsIssueView struct {
	*types.IssueWithCounts
	Labels []string `json:"labels"`
}

type skipLabelsListJSONResponse struct {
	Issues []skipLabelsIssueView `json:"issues"`
	Meta   skipLabelsListMeta    `json:"meta"`
}

type skipLabelsListMeta struct {
	SkipLabels bool `json:"skip_labels"`
	Count      int  `json:"count"`
}

func newSkipLabelsListJSONResponse(issues []*types.IssueWithCounts) skipLabelsListJSONResponse {
	views := make([]skipLabelsIssueView, len(issues))
	for i, issue := range issues {
		views[i] = skipLabelsIssueView{
			IssueWithCounts: issue,
			Labels:          []string{},
		}
	}
	return skipLabelsListJSONResponse{
		Issues: views,
		Meta: skipLabelsListMeta{
			SkipLabels: true,
			Count:      len(views),
		},
	}
}

// skipLabelsConflicts returns the names of label-filter flags that conflict
// with --skip-labels. Empty result means no conflict. AD-02 Wireframe 5.
func skipLabelsConflicts(labels, labelsAny []string, labelPattern, labelRegex string, excludeLabels []string, noLabels bool) []string {
	var conflicts []string
	if len(labels) > 0 {
		conflicts = append(conflicts, "--label")
	}
	if len(labelsAny) > 0 {
		conflicts = append(conflicts, "--label-any")
	}
	if labelPattern != "" {
		conflicts = append(conflicts, "--label-pattern")
	}
	if labelRegex != "" {
		conflicts = append(conflicts, "--label-regex")
	}
	if len(excludeLabels) > 0 {
		conflicts = append(conflicts, "--exclude-label")
	}
	if noLabels {
		conflicts = append(conflicts, "--no-labels")
	}
	return conflicts
}

// skipLabelsFooterText is the AD-02 Wireframe 2 footer note.
// The leading newline keeps the note visually distinct from the table.
func skipLabelsFooterText() string {
	return "\nnote: --skip-labels in effect — labels suppressed in output.\n"
}

// printSkipLabelsFooter writes the AD-02 footer to stdout when the flag is set
// and --quiet is not. Used by output paths that don't go through the buffered
// pager (pretty/tree mode).
func printSkipLabelsFooter(skipLabels bool) {
	if !skipLabels || isQuiet() {
		return
	}
	fmt.Print(skipLabelsFooterText())
}

// formatSkipLabelsConflictError builds the user-facing error message for AD-02
// Wireframe 5. The got: line echoes the conflicting flags so the user can see
// which input to remove without re-reading their command line.
func formatSkipLabelsConflictError(conflicts []string) string {
	return fmt.Sprintf(
		"error: --skip-labels cannot be combined with --label,\n"+
			"       --label-any, --label-pattern, --label-regex,\n"+
			"       --exclude-label, or --no-labels (the filter).\n"+
			"       (got: --skip-labels %s)\n"+
			"reason: --skip-labels suppresses the labels JOIN that those\n"+
			"        filters depend on.\n\n"+
			"To filter by labels: drop --skip-labels.\n"+
			"To get a label-free result fast: drop --label flags.\n",
		strings.Join(conflicts, " "))
}

// knownListFlags maps bare words that users might pass as positional args
// but are actually flag names. Each maps to a hint for the error message.
var knownListFlags = map[string]string{
	"ready":   "--ready",
	"tree":    "--tree",
	"flat":    "--flat",
	"all":     "--all",
	"long":    "--long",
	"watch":   "--watch",
	"pretty":  "--pretty",
	"pinned":  "--pinned",
	"overdue": "--overdue",
}

var listCmd = &cobra.Command{
	Use:     "list",
	GroupID: "issues",
	Short:   "List issues",
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return nil
		}
		for _, arg := range args {
			if hint, ok := knownListFlags[arg]; ok {
				return fmt.Errorf("unknown argument %q; did you mean %q or 'bd %s'?", arg, hint, arg)
			}
		}
		return fmt.Errorf("bd list does not accept positional arguments; use flags instead (see bd list --help)")
	},
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("list")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		return runListCore(cmd, args)
	},
}

// runListCore runs the list query and rendering without emitting a metrics
// event, so the caller owns emission: `bd list` emits "list" exactly once, and
// the `bd children` alias emits "children" exactly once. children sets listCmd's
// flags and calls this core directly rather than listCmd.RunE, which would emit
// a second "list" event for a single user command.
func runListCore(cmd *cobra.Command, _ []string) error {
	in, err := gatherListInput(cmd)
	if err != nil {
		return err
	}
	if usesProxiedServer() {
		return runListCoreProxied(cmd, in)
	}
	return runListCoreDirect(cmd, in)
}

func init() {
	registerListQueryFlags()
	registerListFilterFlags()
	registerListScopeFlags()
	rootCmd.AddCommand(listCmd)
}
