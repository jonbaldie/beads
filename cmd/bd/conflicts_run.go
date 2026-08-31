package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
	"github.com/jonbaldie/beads/internal/ui"
)

// commitMergeResolution concludes the merge: CommitMergeResolution, not
// Commit, because server-mode Commit excludes the config table (GH#2455), so
// a resolved config conflict would be dropped and the merge would re-wedge on
// the next pull (GH#2474). preHead scopes the is_blocked recompute the
// merged-in writes bypassed, exactly as bd vc merge does (bd-578h9.11).
func commitMergeResolution(ctx context.Context, msg, preHead string) error {
	if err := getStore().CommitMergeResolution(ctx, msg); err != nil {
		return fmt.Errorf("commit failed: %w", err)
	}
	// Same unwrap rule as conflictInspector: RecomputeBlockedAfterMerge lives
	// on the concrete Dolt store, not on DoltStorage. Route through the shared
	// helper (cmd/bd/vc.go) so the package has one declaration of the optional
	// interface, and never skip silently (bd vc merge's else branch, wy-163oy).
	if rs, ok := blockedAfterMergeRecomputerFor(getStore()); ok {
		if err := rs.RecomputeBlockedAfterMerge(ctx, preHead); err != nil {
			return fmt.Errorf("is_blocked recompute failed: %w", err)
		}
	} else {
		fmt.Fprintf(os.Stderr, "Warning: storage backend %T cannot recompute is_blocked after a merge; 'bd ready' may be stale until 'bd recompute-blocked' runs\n", storage.UnwrapStore(getStore()))
	}
	// The store's merge-conclusion path can no-op silently (an unreadable
	// dolt_merge_status degrades to "nothing to commit"), and reporting
	// "Merge committed" over a still-open merge is exactly the wy-36ilm
	// symptom — loudly this time (adversarial review F4).
	if after, err := mergeBlockers(ctx); err == nil && after.Merging {
		return fmt.Errorf("commit did not conclude the merge: dolt still reports a merge in progress; conclude it with `bd dolt commit`")
	}
	return nil
}

// errText renders an error for a JSON payload, nil as an empty string.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// concludeAction is what `--conclude` will do with the merge state it found.
// It exists so the decision is a pure function of that state (planConclude),
// testable without a live merge: a Blocked() inversion or a dropped
// merge-status check is otherwise only visible against real dolt.
type concludeAction int

const (
	// concludeActionCommit: nothing outstanding and a merge to finish.
	concludeActionCommit concludeAction = iota
	// concludeActionConflictsLive: dolt_conflicts still has rows.
	concludeActionConflictsLive
	// concludeActionBlocked: schema conflicts or constraint violations.
	concludeActionBlocked
	// concludeActionNothingToConclude: no merge is open at all.
	concludeActionNothingToConclude
)

// planConclude decides --conclude's outcome from the merge state.
//
// Order is load-bearing: live row conflicts outrank blockers (they are the
// thing the operator can actually resolve with bd), and blockers outrank
// "nothing to conclude" so a wedged merge can never be reported as a no-op.
// "Nothing to conclude" requires the merge status to be BOTH available and
// readable — a backend with no MergeBlockerInspector, or a blocker read that
// errored, reports Merging=false for want of knowing, and must fall through
// to attempting the commit rather than claiming there is no merge.
func planConclude(remaining int, blockers storage.MergeBlockers, blockerErr error, haveStatus bool) concludeAction {
	switch {
	case remaining > 0:
		return concludeActionConflictsLive
	case blockers.Blocked():
		return concludeActionBlocked
	case haveStatus && blockerErr == nil && !blockers.Merging:
		return concludeActionNothingToConclude
	default:
		return concludeActionCommit
	}
}

// shouldCommitResolution is the resolve path's commit-hold gate: the merge is
// committed only when this pass actually resolved something, nothing is left
// in dolt_conflicts, the operator did not ask to hold the commit, and no
// schema conflict or constraint violation would make the commit fail with a
// raw dolt error (wy-36ilm F12).
func shouldCommitResolution(resolved, remaining int, noCommit bool, blockers storage.MergeBlockers) bool {
	return resolved > 0 && remaining == 0 && !noCommit && !blockers.Blocked()
}

// concludeFlagConflict rejects the flag combinations --conclude has no
// meaning for. --conclude commits an ALREADY-resolved merge, so an issue ID,
// a strategy, --all or --table describe a resolution it will not perform, and
// silently ignoring --table would imply a scoped conclude exists (review F8).
func concludeFlagConflict(args []string, opts conflictsResolveOptions) error {
	if len(args) > 0 || opts.all || opts.ours || opts.theirs ||
		opts.strategy != "" || opts.table != "" {
		return fmt.Errorf("--conclude takes no issue IDs, table or strategy: it commits an already-resolved merge")
	}
	if opts.noCommit {
		return fmt.Errorf("--conclude and --no-commit are opposites")
	}
	return nil
}

// concludeResolvedMerge commits a merge that has no live conflicts left —
// `bd conflicts resolve --conclude` (wy-36ilm F4). It refuses while anything
// is still outstanding, so it can only ever finish a resolution someone else
// already made, never paper over one.
func concludeResolvedMerge(ctx context.Context) error {
	remaining, err := totalConflicts(ctx)
	if err != nil {
		return HandleErrorRespectJSON("failed to read conflicts: %v", err)
	}
	blockers, blockerErr := mergeBlockers(ctx)
	if blockerErr != nil {
		// The blocker read is diagnosis, never a gate — but a caller that
		// cannot see the blockers must not read "nothing outstanding" into
		// their absence (adversarial review F3).
		fmt.Fprintf(os.Stderr, "Warning: could not read schema conflicts/constraint violations: %v\n", blockerErr)
	}
	// A backend with no MergeBlockerInspector reports Merging=false, so it
	// keeps the old behavior of just attempting the commit.
	_, haveStatus := storage.UnwrapStore(getStore()).(storage.MergeBlockerInspector)
	switch planConclude(remaining, blockers, blockerErr, haveStatus) {
	case concludeActionConflictsLive:
		return HandleErrorRespectJSON("%d conflict(s) are still live; resolve them first (bd conflicts list)", remaining)
	case concludeActionBlocked:
		if !isJSONOutput() {
			printMergeBlockers(blockers)
		}
		// HandleErrorRespectJSON, not a bare error: it exits non-zero in JSON
		// mode too, so a script cannot read `{"committed":false}` at status 0
		// as success and push over an open merge (adversarial review F2).
		return HandleErrorRespectJSON("merge not concluded: schema conflicts or constraint violations are outstanding")
	case concludeActionNothingToConclude:
		// Say so rather than minting an empty commit.
		return concludeJSON(false, blockers, "No merge is in progress; nothing to conclude.")
	}

	preHead, _ := getStore().GetCurrentCommit(ctx)
	if err := commitMergeResolution(ctx, "Conclude resolved merge", preHead); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	blockers, _ = mergeBlockers(ctx)
	return concludeJSON(true, blockers, "Merge committed. Push when ready: bd sync")
}

// concludeJSON emits --conclude's one payload shape — same keys on every
// outcome, so a consumer can key on `committed` (review F9) — or the human
// line when JSON is off.
func concludeJSON(committed bool, blockers storage.MergeBlockers, human string) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"committed": committed,
			"remaining": 0,
			"merging":   blockers.Merging,
			"blockers":  blockers,
		})
	}
	fmt.Println(human)
	return nil
}

// conflictsMetrics starts a command metrics event and returns its closer, so
// each subcommand can arm it with one deferred call.
func conflictsMetrics(name string) func() {
	evt := metrics.NewCommandEvent(name)
	return func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}
}

// requireConflictSupport refuses the modes where conflict inspection has no
// meaning: a proxied server owns its own working set, and a non-Dolt backend
// has no merge state at all.
func requireConflictSupport() error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("bd conflicts is not supported in proxied-server mode")
	}
	if getStore() == nil {
		return HandleErrorRespectJSON("no database open")
	}
	return nil
}

// conflictedTables returns the tables dolt reports as conflicted.
func conflictedTables(ctx context.Context) ([]storage.Conflict, error) {
	conflicts, err := getStore().GetConflicts(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Field < conflicts[j].Field })
	return conflicts, nil
}

// conflictInspector returns the store's conflict surface. store is ALWAYS
// wrapped (telemetry, and the hook-firing decorator whenever hooks are on),
// and a decorator that embeds the DoltStorage interface promotes only that
// interface's methods — so asserting ConflictInspector on the wrapper is
// always false and every conflict would read as "no conflicts". UnwrapStore
// is the codebase's answer for optional interfaces (hook_decorator.go:67).
func conflictInspector() (storage.ConflictInspector, bool) {
	inspector, ok := storage.UnwrapStore(getStore()).(storage.ConflictInspector)
	return inspector, ok
}

// conflictRows returns the per-field conflicted rows of one table, or an empty
// slice when the backend has no ConflictInspector.
func conflictRows(ctx context.Context, table string) ([]storage.ConflictRow, error) {
	inspector, ok := conflictInspector()
	if !ok {
		return nil, nil
	}
	return inspector.GetConflictRows(ctx, table)
}

// mergeBlockers reports the non-row merge state — schema conflicts and
// constraint violations — that blocks the merge commit while
// dolt_conflicts is empty (wy-36ilm F12). Same UnwrapStore rule as
// conflictInspector. A backend without the interface reports nothing, and a
// read error is the caller's to soften: this is diagnosis, never a gate.
func mergeBlockers(ctx context.Context) (storage.MergeBlockers, error) {
	inspector, ok := storage.UnwrapStore(getStore()).(storage.MergeBlockerInspector)
	if !ok {
		return storage.MergeBlockers{}, nil
	}
	return inspector.GetMergeBlockers(ctx)
}

// printMergeBlockers renders the blockers an operator has to clear by hand.
// dolt offers no ours/theirs for either class, so this is diagnosis plus the
// dolt commands that do resolve them — the guidance the raw commit error
// (wy-36ilm F12) never carried.
func printMergeBlockers(b storage.MergeBlockers) {
	writeMergeBlockers(os.Stdout, b)
}

// writeMergeBlockers is printMergeBlockers' body against an explicit writer,
// so the remedy text is assertable without hijacking stdout. An unblocked
// state writes NOTHING: `bd conflicts list` calls this unconditionally.
func writeMergeBlockers(w io.Writer, b storage.MergeBlockers) {
	if !b.Blocked() {
		return
	}
	fmt.Fprintf(w, "\n%s the merge cannot be committed yet:\n", ui.RenderAccent("!!"))
	for _, t := range b.SchemaConflictTables {
		fmt.Fprintf(w, "  schema conflict: %s\n", t)
	}
	for _, v := range b.ConstraintViolations {
		fmt.Fprintf(w, "  constraint violations: %s (%d)\n", v.Table, v.Count)
	}
	fmt.Fprintln(w, "\nNeither class has an ours/theirs resolution — bd conflicts resolve cannot settle them.")
	if len(b.SchemaConflictTables) > 0 {
		// Not `dolt conflicts resolve`: dolt refuses that outright while a
		// schema conflict is live (dolthub/dolt#6616), so pointing an
		// operator at it sends them to a command that always errors
		// (wy-36ilm adversarial review F1). A schema conflict is aborted and
		// re-merged, not resolved in place.
		fmt.Fprintln(w, "  Schema conflicts:       abort the merge (dolt merge --abort), apply the peer's")
		fmt.Fprintln(w, "                          ALTER TABLE statements locally, then merge again")
	}
	if len(b.ConstraintViolations) > 0 {
		fmt.Fprintln(w, "  Constraint violations:  inspect dolt_constraint_violations_<table>, delete the offending rows,")
		if len(b.SchemaConflictTables) > 0 {
			fmt.Fprintln(w, "                          then re-inspect after aborting and re-merging the schema change")
		} else {
			fmt.Fprintln(w, "                          then conclude with: bd conflicts resolve --conclude")
		}
	}
}

// totalConflicts counts every live conflicted row, the gate on committing the
// merge: dolt refuses a commit while any conflict is live.
func totalConflicts(ctx context.Context) (int, error) {
	tables, err := conflictedTables(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, t := range tables {
		total += t.Count
	}
	return total, nil
}

// resolveStrategy reads the strategy from --ours/--theirs/--strategy.
func resolveStrategy(opts conflictsResolveOptions) (string, error) {
	picked, err := strategyFromSideFlags(opts)
	if err != nil {
		return "", err
	}
	if opts.strategy != "" {
		if picked != "" && picked != opts.strategy {
			return "", fmt.Errorf("--strategy %s contradicts --%s", opts.strategy, picked)
		}
		picked = opts.strategy
	}
	if picked == "" {
		return "", fmt.Errorf("a resolution strategy is required: --ours or --theirs")
	}
	if err := versioncontrolops.ValidateConflictStrategy(picked); err != nil {
		return "", err
	}
	return picked, nil
}

func strategyFromSideFlags(opts conflictsResolveOptions) (string, error) {
	switch {
	case opts.ours && opts.theirs:
		return "", fmt.Errorf("pass --ours or --theirs, not both")
	case opts.ours:
		return versioncontrolops.ConflictStrategyOurs, nil
	case opts.theirs:
		return versioncontrolops.ConflictStrategyTheirs, nil
	}
	return "", nil
}

// filterShownFields drops the agreeing fields from JSON output unless the
// caller asked for all of them, so a conflict reads as its handful of
// diverged fields rather than the whole row.
func filterShownFields(rows []storage.ConflictRow, allFields bool) []storage.ConflictRow {
	if allFields {
		return rows
	}
	out := make([]storage.ConflictRow, 0, len(rows))
	for _, r := range rows {
		trimmed := r
		trimmed.Fields = differingFields(r.Fields)
		out = append(out, trimmed)
	}
	return out
}

func differingFields(fields []storage.ConflictFieldValue) []storage.ConflictFieldValue {
	out := make([]storage.ConflictFieldValue, 0, len(fields))
	for _, f := range fields {
		if f.Differs() {
			out = append(out, f)
		}
	}
	return out
}

// printConflictRow renders one conflicted row for humans.
func printConflictRow(r storage.ConflictRow, allFields bool) {
	key := r.Key
	if key == "" {
		key = "(unkeyed row)"
	}
	fmt.Printf("\n%s %s  [%s]\n", ui.RenderAccent(r.Table), key, conflictKind(r))
	fields := r.Fields
	if !allFields {
		fields = differingFields(fields)
	}
	if len(fields) == 0 {
		fmt.Println("  (no differing fields; the conflict is structural)")
		return
	}
	for _, f := range fields {
		fmt.Printf("  %s\n", f.Name)
		fmt.Printf("    base:   %s\n", conflictValue(f.Base))
		fmt.Printf("    ours:   %s\n", conflictValue(f.Ours))
		fmt.Printf("    theirs: %s\n", conflictValue(f.Theirs))
	}
	fmt.Println()
}

// conflictKind names the conflict class in the vocabulary an operator needs to
// choose a strategy, since ours/theirs mean different things per class.
func conflictKind(r storage.ConflictRow) string {
	switch {
	case !r.OurExists && !r.TheirExists:
		return "both sides deleted"
	case !r.OurExists:
		return "we deleted / they modified"
	case !r.TheirExists:
		return "we modified / they deleted"
	case !r.BaseExists:
		return "both sides added"
	default:
		return "both sides modified"
	}
}

// conflictValue renders a field value, distinguishing SQL NULL from empty.
func conflictValue(v *string) string {
	if v == nil {
		return "(null)"
	}
	s := strings.ReplaceAll(*v, "\n", "\\n")
	if s == "" {
		return `""`
	}
	return s
}

func init() {
	conflictsShowCmd.Flags().Bool("all-fields", false, "Show every column, not just the fields that diverged")
	conflictsShowCmd.Flags().String("table", "", "Restrict to one conflicted table (default: all)")

	conflictsResolveCmd.Flags().Bool("ours", false, "Keep our side")
	conflictsResolveCmd.Flags().Bool("theirs", false, "Take their side")
	conflictsResolveCmd.Flags().String("strategy", "", "Resolution strategy: ours|theirs")
	conflictsResolveCmd.Flags().Bool("all", false, "Resolve whole tables instead of named issues")
	conflictsResolveCmd.Flags().String("table", "", "Table to resolve (default: issues)")
	conflictsResolveCmd.Flags().Bool("no-commit", false, "Resolve without committing the merge")
	conflictsResolveCmd.Flags().Bool("conclude", false, "Commit a merge whose conflicts are already resolved")

	conflictsCmd.AddCommand(conflictsListCmd)
	conflictsCmd.AddCommand(conflictsShowCmd)
	conflictsCmd.AddCommand(conflictsResolveCmd)
	rootCmd.AddCommand(conflictsCmd)
}
