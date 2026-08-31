package main

import (
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

// `bd conflicts` is the operator surface for a merge that halted (federation
// ask #3). Without it, resolving means dropping into the raw dolt CLI inside
// .beads/dolt/<db> — `dolt conflicts cat issues`, `dolt conflicts resolve
// --ours issues`, `dolt add -A && dolt commit` — whose flag surface differs
// from git's just enough to bite. Here the same work is issue-oriented: which
// issues are conflicted, what each side says field by field, and a resolution
// that can name a single issue instead of a whole table.
//
// It reads the LIVE working set. beads' own pull path auto-resolves the safe
// conflict classes and aborts anything else (restoring the working set), so
// what lands here is what a CLI-level pull — the federation bridge's — left
// behind, plus anything a `bd vc merge` left unresolved.

const conflictsResolveHint = "Resolve with: bd conflicts resolve <issue-id> --ours|--theirs"

var conflictsCmd = &cobra.Command{
	Use:     "conflicts",
	GroupID: "sync",
	Short:   "Inspect and resolve live merge conflicts",
	Long: `Inspect and resolve the merge conflicts sitting in the working set.

Conflicts appear when a pull or merge brought in changes that collide with
local ones and could not be settled automatically. These commands present them
per issue and per field, and resolve them without the raw dolt CLI.

Examples:
  bd conflicts list                          # which tables and issues are conflicted
  bd conflicts show                          # every conflicted row, field by field
  bd conflicts show bd-1234                  # one issue
  bd conflicts resolve bd-1234 --ours        # keep our side of one issue
  bd conflicts resolve --all --theirs        # take their side of everything`,
}

type conflictsShowOptions struct {
	allFields bool
	table     string
}

func conflictsShowOptionsFromCommand(cmd *cobra.Command) conflictsShowOptions {
	return conflictsShowOptions{
		allFields: commandBoolFlag(cmd, "all-fields"),
		table:     conflictsStringFlag(cmd, "table"),
	}
}

func conflictsStringFlag(cmd *cobra.Command, name string) string {
	if cmd == nil {
		return ""
	}
	value, _ := cmd.Flags().GetString(name)
	return value
}

type conflictsResolveOptions struct {
	ours     bool
	theirs   bool
	strategy string
	all      bool
	table    string
	noCommit bool
	conclude bool
}

func conflictsResolveOptionsFromCommand(cmd *cobra.Command) conflictsResolveOptions {
	return conflictsResolveOptions{
		ours:     commandBoolFlag(cmd, "ours"),
		theirs:   commandBoolFlag(cmd, "theirs"),
		strategy: conflictsStringFlag(cmd, "strategy"),
		all:      commandBoolFlag(cmd, "all"),
		table:    conflictsStringFlag(cmd, "table"),
		noCommit: commandBoolFlag(cmd, "no-commit"),
		conclude: commandBoolFlag(cmd, "conclude"),
	}
}

var conflictsListCmd = &cobra.Command{
	Use:           "list",
	Short:         "List tables and issues with live merge conflicts",
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		defer conflictsMetrics("conflicts-list")()
		ctx := getRootContext()
		if err := requireConflictSupport(); err != nil {
			return err
		}
		tables, err := conflictedTables(ctx)
		if err != nil {
			return HandleErrorRespectJSON("failed to read conflicts: %v", err)
		}
		type tableOut struct {
			Table string   `json:"table"`
			Count int      `json:"count"`
			Keys  []string `json:"keys,omitempty"`
		}
		out := make([]tableOut, 0, len(tables))
		total := 0
		for _, t := range tables {
			rows, err := conflictRows(ctx, t.Field)
			if err != nil {
				return HandleErrorRespectJSON("failed to read conflicts for %s: %v", t.Field, err)
			}
			keys := make([]string, 0, len(rows))
			for _, r := range rows {
				if r.Key != "" {
					keys = append(keys, r.Key)
				}
			}
			// dolt_conflicts' own count is authoritative for tables whose
			// rows we could not key (keyless tables list no keys).
			count := len(rows)
			if count == 0 {
				count = t.Count
			}
			total += count
			out = append(out, tableOut{Table: t.Field, Count: count, Keys: keys})
		}
		// Schema conflicts and constraint violations are outstanding merge
		// state that dolt_conflicts never lists, so "No merge conflicts."
		// over a wedged merge was a lie (wy-36ilm F12).
		blockers, blockerErr := mergeBlockers(ctx)
		if isJSONOutput() {
			payload := map[string]interface{}{
				"conflicts": total,
				"tables":    out,
				"blockers":  blockers,
			}
			if blockerErr != nil {
				payload["blockers_error"] = blockerErr.Error()
			}
			return outputJSON(payload)
		}
		if total == 0 {
			switch {
			case blockers.Blocked():
				fmt.Println("No conflicted rows.")
				printMergeBlockers(blockers)
			case blockers.Merging:
				fmt.Println("No merge conflicts; a merge is open and resolved.")
				fmt.Println("Conclude it with: bd conflicts resolve --conclude")
			default:
				fmt.Println("No merge conflicts.")
			}
			if blockerErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not read schema conflicts/constraint violations: %v\n", blockerErr)
			}
			return nil
		}
		fmt.Printf("\n%s %d live merge conflict(s):\n\n", ui.RenderAccent("!!"), total)
		for _, t := range out {
			fmt.Printf("  %s (%d)\n", ui.RenderAccent(t.Table), t.Count)
			for _, k := range t.Keys {
				fmt.Printf("    %s\n", k)
			}
		}
		printMergeBlockers(blockers)
		if blockerErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not read schema conflicts/constraint violations: %v\n", blockerErr)
		}
		fmt.Printf("\nInspect with: bd conflicts show [<issue-id>]\n%s\n\n", conflictsResolveHint)
		return nil
	},
}

var conflictsShowCmd = &cobra.Command{
	Use:   "show [<issue-id>]",
	Short: "Show conflicted rows field by field (base/ours/theirs)",
	Long: `Show each conflicted row with its fields side by side.

Only fields where our side and their side disagree are shown; --all-fields
shows every column. Without an issue ID, every conflicted row of every
conflicted table is shown.`,
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		defer conflictsMetrics("conflicts-show")()
		ctx := getRootContext()
		opts := conflictsShowOptionsFromCommand(cmd)
		if err := requireConflictSupport(); err != nil {
			return err
		}
		wantKey := ""
		if len(args) == 1 {
			wantKey = args[0]
		}

		var tables []string
		if opts.table != "" {
			tables = []string{opts.table}
		} else {
			ts, err := conflictedTables(ctx)
			if err != nil {
				return HandleErrorRespectJSON("failed to read conflicts: %v", err)
			}
			for _, t := range ts {
				tables = append(tables, t.Field)
			}
		}

		var matched []storage.ConflictRow
		for _, table := range tables {
			rows, err := conflictRows(ctx, table)
			if err != nil {
				return HandleErrorRespectJSON("failed to read conflicts for %s: %v", table, err)
			}
			for _, r := range rows {
				if wantKey != "" && r.Key != wantKey {
					continue
				}
				matched = append(matched, r)
			}
		}

		if isJSONOutput() {
			return outputJSON(map[string]interface{}{
				"conflicts": len(matched),
				"rows":      filterShownFields(matched, opts.allFields),
			})
		}
		if len(matched) == 0 {
			if wantKey != "" {
				fmt.Printf("No live merge conflict for %s.\n", wantKey)
			} else {
				fmt.Println("No merge conflicts.")
			}
			return nil
		}
		for _, r := range matched {
			printConflictRow(r, opts.allFields)
		}
		fmt.Printf("%s\n\n", conflictsResolveHint)
		return nil
	},
}

var conflictsResolveCmd = &cobra.Command{
	Use:   "resolve [<issue-id>...]",
	Short: "Resolve merge conflicts with --ours or --theirs",
	Long: `Resolve live merge conflicts, then conclude the merge with a commit.

Named issue IDs are resolved row by row, leaving every other conflicted row
alone. --all resolves whole tables at once (dolt's own table-level
resolution). The merge is committed only once NO conflicts remain, so a
partial resolution leaves the merge open for the next pass.

Row-by-row resolution requires the row to exist on both sides: when one side
deleted it, resolve that table wholesale or edit the row directly.

Examples:
  bd conflicts resolve bd-1234 --ours              # keep our side of one issue
  bd conflicts resolve bd-1234 bd-5678 --theirs    # take their side of two
  bd conflicts resolve --all --ours                # every conflicted table
  bd conflicts resolve --all --table config --theirs
  bd conflicts resolve --conclude                  # commit an already-resolved merge`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		defer conflictsMetrics("conflicts-resolve")()
		ctx := getRootContext()
		opts := conflictsResolveOptionsFromCommand(cmd)
		if err := requireConflictSupport(); err != nil {
			return err
		}
		// --conclude commits a merge whose conflicts are ALREADY gone: the
		// state left by --no-commit, or by a partial resolve whose later ID
		// errored out. Before this, that merge could not be concluded through
		// bd at all — with zero live conflicts, --all returns early and a
		// named ID errors "no live conflict" (wy-36ilm F4).
		if opts.conclude {
			if err := concludeFlagConflict(args, opts); err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
			return concludeResolvedMerge(ctx)
		}

		strategy, err := resolveStrategy(opts)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		if opts.all == (len(args) > 0) {
			return HandleErrorRespectJSON("name the issue IDs to resolve, or pass --all (not both)")
		}

		// Pre-resolution HEAD scopes the is_blocked recompute the merged-in
		// writes bypassed, exactly as bd vc merge does (bd-578h9.11).
		preHead, _ := getStore().GetCurrentCommit(ctx)

		resolved := 0
		if opts.all {
			tables := []string{}
			if opts.table != "" {
				tables = append(tables, opts.table)
			} else {
				ts, err := conflictedTables(ctx)
				if err != nil {
					return HandleErrorRespectJSON("failed to read conflicts: %v", err)
				}
				for _, t := range ts {
					tables = append(tables, t.Field)
				}
			}
			if len(tables) == 0 {
				fmt.Println("No merge conflicts.")
				return nil
			}
			for _, table := range tables {
				rows, err := conflictRows(ctx, table)
				if err != nil {
					return HandleErrorRespectJSON("failed to read conflicts for %s: %v", table, err)
				}
				if err := getStore().ResolveConflicts(ctx, table, strategy); err != nil {
					return HandleErrorRespectJSON("failed to resolve conflicts in %s: %v", table, err)
				}
				resolved += len(rows)
			}
		} else {
			table := opts.table
			if table == "" {
				table = "issues"
			}
			inspector, ok := conflictInspector()
			if !ok {
				return HandleErrorRespectJSON("this backend does not support per-issue conflict resolution; use --all")
			}
			if !versioncontrolops.SupportsRowResolve(table) {
				return HandleErrorRespectJSON("per-issue resolution is not supported for table %s; use --all --table %s", table, table)
			}
			n, err := inspector.ResolveConflictRows(ctx, table, args, strategy)
			if err != nil {
				// A partial resolution is real state: report what landed.
				if n > 0 {
					return HandleErrorRespectJSON("resolved %d of %d conflict(s) before failing: %v", n, len(args), err)
				}
				return HandleErrorRespectJSON("failed to resolve conflicts: %v", err)
			}
			resolved = n
		}

		remaining, err := totalConflicts(ctx)
		if err != nil {
			return HandleErrorRespectJSON("resolved %d conflict(s) but failed to re-check conflicts: %v", resolved, err)
		}

		// Schema conflicts and constraint violations survive a clean
		// dolt_conflicts, and CommitMergeResolution would fail on them with a
		// raw dolt error; hold the commit and explain instead (wy-36ilm F12).
		blockers, blockerErr := mergeBlockers(ctx)
		if blockerErr != nil && !isJSONOutput() {
			fmt.Fprintf(os.Stderr, "Warning: could not read schema conflicts/constraint violations: %v\n", blockerErr)
		}

		committed := false
		if shouldCommitResolution(resolved, remaining, opts.noCommit, blockers) {
			msg := fmt.Sprintf("Resolve %d merge conflict(s) using %s strategy", resolved, strategy)
			if err := commitMergeResolution(ctx, msg, preHead); err != nil {
				return HandleErrorRespectJSON("conflicts resolved but %v", err)
			}
			committed = true
		}

		if isJSONOutput() {
			return outputJSON(map[string]interface{}{
				"resolved":       resolved,
				"strategy":       strategy,
				"remaining":      remaining,
				"committed":      committed,
				"blockers":       blockers,
				"blockers_error": errText(blockerErr),
			})
		}
		fmt.Printf("Resolved %d conflict(s) using '%s'.\n", resolved, strategy)
		switch {
		case remaining > 0:
			fmt.Printf("%d conflict(s) remain; the merge is not committed yet.\nRun: bd conflicts list\n", remaining)
		case committed:
			fmt.Println("Merge committed. Push when ready: bd sync")
		case blockers.Blocked():
			printMergeBlockers(blockers)
		case resolved == 0:
			fmt.Println("Nothing was conflicted; no commit made.")
		default:
			fmt.Println("All conflicts resolved; commit withheld. Conclude the merge with: bd conflicts resolve --conclude")
		}
		return nil
	},
}
