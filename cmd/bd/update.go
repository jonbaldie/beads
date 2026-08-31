package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/issueops"
	"github.com/spf13/cobra"
)

// commandIssueUpdater is the lifecycle capability the update command needs.
// issueops.Lifecycle satisfies it without an adapter.
type commandIssueUpdater interface {
	Update(context.Context, issueops.UpdateRequest) (issueops.UpdateResult, error)
}

// commandUpdateMutation contains the command-derived values for one lifecycle
// update. Both routes of `bd update` fill it in: the direct one below, and the
// proxied-server claim path in update_proxied_server.go.
type commandUpdateMutation struct {
	actor            string
	issueID          string
	patch            issueops.IssuePatch
	claim            bool
	force            bool
	expectedAssignee *string
	expectedStatus   *issueops.Status
	// provenance names the history entry the write records. Empty takes the
	// backend's default, which is what the direct route wants; the proxied
	// route spells the message it has always written.
	provenance string
}

// runCommandUpdateMutation maps a command update into its lifecycle request and
// returns the lifecycle result unchanged. It is the ONE place the command's
// flag semantics become a request — in particular the one --force that means
// two overrides, whose assignee half only applies to an assignee edit.
func runCommandUpdateMutation(ctx context.Context, updater commandIssueUpdater, mutation commandUpdateMutation) (issueops.UpdateResult, error) {
	return updater.Update(ctx, issueops.UpdateRequest{
		Actor:                 mutation.actor,
		IssueID:               mutation.issueID,
		Patch:                 mutation.patch,
		Claim:                 mutation.claim,
		ForceAssigneeTransfer: mutation.force && mutation.patch.Assignee.Set,
		ForceClosePolicy:      mutation.force,
		ExpectedAssignee:      mutation.expectedAssignee,
		ExpectedStatus:        mutation.expectedStatus,
		Provenance:            mutation.provenance,
	})
}

var updateCmd = &cobra.Command{
	Use:               "update [id...]",
	GroupID:           "issues",
	Short:             "Update one or more issues",
	ValidArgsFunction: issueIDCompletion,
	Long: `Update one or more issues.

If no issue ID is provided, updates the last touched issue (from most recent
create, update, show, or close operation). This fallback only applies in
interactive sessions (stdin is a terminal); in scripts and agent sessions a
missing ID is an error, so a command built from an empty variable cannot
silently mutate an unrelated issue. Set BD_LAST_TOUCHED_FALLBACK=1 to allow
the fallback anywhere, or =0 to disable it entirely.

Updates are applied per issue ID, not atomically across IDs: when some IDs
fail, the remaining issues are still updated, every failed ID is reported on
stderr, and the command exits nonzero.

Exit codes: 1 for general failures; 13 when every failure is a stale
--if-assignee/--if-status guard (the precondition no longer held, nothing was
written — another actor won the race, so retrying the same guard is
pointless).`,
	// The non-interactive no-ID refusal lives in argument validation, which
	// cobra runs before root's PersistentPreRunE — so a scripted `bd update
	// $ID ...` with an empty $ID fails fast, before the pre-run hooks can
	// open the store, run a version-bump migration, or auto-import JSONL
	// (bd-m00pb). The interactive fallback itself is resolved in RunE.
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 && !AllowLastTouchedFallback() {
			return HandleErrorRespectJSON("no issue ID provided (the last-touched fallback only applies in interactive sessions; pass an explicit issue ID or set BD_LAST_TOUCHED_FALLBACK=1)")
		}
		return nil
	},
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runUpdate,
}

// setField marks a patch field as supplied, including when the value is a zero
// value the caller meant to write.
func setField[T any](value T) issueops.Field[T] {
	return issueops.Field[T]{Set: true, Value: value}
}

func stringField(value any) (issueops.Field[string], bool) {
	text, ok := value.(string)
	if !ok {
		return issueops.Field[string]{}, false
	}
	return setField(text), true
}

func optionalTimeField(value any) (issueops.Field[*time.Time], bool) {
	if value == nil {
		return setField[*time.Time](nil), true
	}
	at, ok := value.(time.Time)
	if !ok {
		return issueops.Field[*time.Time]{}, false
	}
	return setField(&at), true
}

// parseSetMetadataFlags splits --set-metadata key=value pairs, matching
// storage.ApplyMetadataEdits' parsing so the CLI contract is unchanged. Values
// are always stored as JSON strings (GH#4146).
func parseSetMetadataFlags(flags []string) (map[string]json.RawMessage, error) {
	set := make(map[string]json.RawMessage, len(flags))
	for _, flag := range flags {
		key, value, ok := strings.Cut(flag, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid --set-metadata: expected key=value, got %q", flag)
		}
		set[key] = storage.MetadataEditValue(value)
	}
	return set, nil
}

func replacesExistingNotes(existing string, fields map[string]any) bool {
	newNotes, replacing := fields["notes"].(string)
	return replacing && existing != "" && newNotes != existing
}

func warnNotesReplacement(id string) {
	fmt.Fprintf(os.Stderr, "warning: %s: --notes replaced existing notes (use --append-notes to preserve history)\n", id) //nolint:gosec // G705: stderr, not a browser context
}

// ExitGuardMismatch is the exit code when a `bd update` run failed solely
// because --if-assignee/--if-status guards did not match: the precondition no
// longer held, nothing was written, and retrying is pointless — another actor
// won the race. Scripts branch on it to tell "racer won, skip gracefully"
// (13) from infra failure (1, retry/abort). Mixed batches — any failure that
// is NOT a guard mismatch — exit 1, the conservative "something needs a
// retry" verdict. The stderr line carries the machine-greppable sentinel
// text ("assignee mismatch" / "status mismatch") either way.
const ExitGuardMismatch = 13

// isGuardMismatch reports whether err is a bd-wsqvw conditional-update guard
// refusal (stale --if-assignee/--if-status), the failure class that exits
// ExitGuardMismatch instead of 1.
func isGuardMismatch(err error) bool {
	return errors.Is(err, storage.ErrAssigneeMismatch) || errors.Is(err, storage.ErrStatusMismatch)
}

// updateIDFailure records one issue ID that could not be updated and why.
// GuardMismatch marks a --if-assignee/--if-status refusal so JSON consumers
// can distinguish it without parsing the error text.
type updateIDFailure struct {
	ID            string `json:"id"`
	Error         string `json:"error"`
	GuardMismatch bool   `json:"guard_mismatch,omitempty"`
}

// errStrayFlagValuePositional refuses, before any write, a positional argument
// that contains '='. --set-metadata takes ONE key=value per flag, so
// `--set-metadata a=1 b=2` silently turns `b=2` into a positional issue id;
// no issue id contains '=', so such a positional is unambiguously a mis-typed
// flag value. Rejecting it up front prevents a partial write that would apply
// only the pairs that happened to bind to a flag (bd-5247).
func errStrayFlagValuePositional(args []string) error {
	for _, arg := range args {
		if strings.Contains(arg, "=") {
			return fmt.Errorf("positional argument %q contains '=', which no issue id does — this is a mis-typed flag value; repeat the flag per pair, e.g. --set-metadata a=1 --set-metadata b=2", arg)
		}
	}
	return nil
}

// reportUpdateFailures emits a per-ID failure report on stderr and returns a
// nonzero exit error — ExitGuardMismatch when every failure is a
// --if-assignee/--if-status guard refusal, 1 otherwise. In --json mode the
// report is a single compact JSON line — the last line on stderr — so
// callers can parse which IDs failed while stdout keeps the plain
// array-of-updated-issues success shape. In text mode the individual errors
// were already printed inline; this adds a summary naming every failed ID.
func reportUpdateFailures(failures []updateIDFailure, total int) error {
	msg := fmt.Sprintf("%d of %d issues failed to update", len(failures), total)
	if isJSONOutput() {
		inner := map[string]interface{}{
			"error":  msg,
			"failed": failures,
		}
		var payload interface{}
		if jsonEnvelopeEnabled() {
			payload = map[string]interface{}{
				"schema_version": JSONSchemaVersion,
				"data":           inner,
			}
		} else {
			inner["schema_version"] = JSONSchemaVersion
			payload = inner
		}
		data, err := json.Marshal(payload)
		if err != nil {
			// Marshaling flat strings cannot realistically fail; fall back to
			// the text summary rather than exiting silently.
			fmt.Fprintf(os.Stderr, "Error: %s\n", msg)
		} else {
			fmt.Fprintln(os.Stderr, string(data))
		}
	} else {
		fmt.Fprintf(os.Stderr, "Error: %s\n", msg)
		for _, f := range failures {
			fmt.Fprintf(os.Stderr, "  %s: %s\n", f.ID, f.Error)
		}
	}
	allGuard := len(failures) > 0
	for _, f := range failures {
		if !f.GuardMismatch {
			allGuard = false
			break
		}
	}
	if allGuard {
		return &exitError{Code: ExitGuardMismatch}
	}
	return &exitError{Code: 1}
}

// mergeMetadata merges new metadata JSON into existing metadata.
// Keys from newMeta overwrite keys in existing; keys only in existing are preserved.
// Thin alias over the shared storage helper (also used in-transaction by issueops).
func mergeMetadata(existing, newMeta json.RawMessage) (json.RawMessage, error) {
	return storage.MergeMetadataJSON(existing, newMeta)
}

// applyMetadataEdits applies --set-metadata and --unset-metadata edits to existing metadata.
// Thin alias over the shared storage helper (also used in-transaction by issueops).
func applyMetadataEdits(existing json.RawMessage, setFlags, unsetFlags []string) (json.RawMessage, error) {
	return storage.ApplyMetadataEdits(existing, setFlags, unsetFlags)
}

// toJSONValue stores a CLI metadata value as a JSON string.
// Previous behavior inferred types (numbers, booleans) from content,
// which silently broke map[string]string round-trips (GH#4146).
func toJSONValue(s string) json.RawMessage {
	return storage.MetadataEditValue(s)
}

// updateGuardsFromFlags reads the bd-wsqvw conditional-update guards
// (--if-assignee/--if-status) with presence detected via Changed(), so
// `--if-assignee ""` is a real guard meaning "expected unassigned" rather than
// "no guard" (the unclaim.go idiom). It rejects combining guards with --claim
// (--claim is its own compare-and-set with claim-pool semantics; the guards
// would silently duplicate or contradict it) and guards with no regular field
// update to ride on (the CAS applies to the issues-row UPDATE; label and
// parent edits run outside it and would not be guarded). An --if-status value
// is validated against the same built-in + custom status set as --status, so a
// typo fails fast instead of mismatching forever.
func updateGuardsFromFlags(cmd *cobra.Command, claimFlag bool, updates map[string]interface{}) (ifAssignee, ifStatus *string, err error) {
	ifAssignee = updateIfAssigneeFlag(cmd)
	ifStatus, err = updateIfStatusFlag(cmd)
	if err != nil {
		return nil, nil, err
	}
	if ifAssignee == nil && ifStatus == nil {
		return nil, nil, nil
	}
	if claimFlag {
		return nil, nil, HandleErrorRespectJSON("cannot combine --if-assignee/--if-status with --claim (--claim is already an atomic compare-and-set)")
	}
	if !updateGuardsHaveFieldUpdate(updates) {
		return nil, nil, HandleErrorRespectJSON("--if-assignee/--if-status require at least one field update (e.g. -a, -s); label and parent edits are not covered by the guard")
	}
	return ifAssignee, ifStatus, nil
}

func updateIfAssigneeFlag(cmd *cobra.Command) *string {
	if !cmd.Flags().Changed("if-assignee") {
		return nil
	}
	v, _ := cmd.Flags().GetString("if-assignee")
	return &v
}

func updateIfStatusFlag(cmd *cobra.Command) (*string, error) {
	if !cmd.Flags().Changed("if-status") {
		return nil, nil
	}
	v, _ := cmd.Flags().GetString("if-status")
	var customStatuses []string
	if getStore() != nil {
		if cs, csErr := getStore().GetCustomStatuses(getRootContext()); csErr == nil {
			customStatuses = cs
		}
	}
	if !types.Status(v).IsValidWithCustom(customStatuses) {
		return nil, HandleErrorRespectJSON("invalid --if-status %q (built-in: open, in_progress, blocked, deferred, closed, pinned, hooked; or configure custom statuses via 'bd config set status.custom')", v)
	}
	return &v, nil
}

func updateGuardsHaveFieldUpdate(updates map[string]interface{}) bool {
	for k := range updates {
		switch k {
		case "add_labels", "remove_labels", "set_labels", "parent":
		default:
			return true
		}
	}
	return false
}

func init() {
	updateCmd.Flags().StringP("status", "s", "", "New status")
	registerPriorityFlag(updateCmd, "")
	updateCmd.Flags().String("title", "", "New title")
	updateCmd.Flags().StringP("type", "t", "", "New type (bug|feature|task|epic|chore|decision|spike|story|milestone); custom types require types.custom config; aliases: enhancement/feat→feature, dec/adr→decision")
	registerCommonIssueFlags(updateCmd)
	updateCmd.Flags().Lookup("notes").Usage = "Additional notes (replaces existing notes; use --append-notes to append)"
	updateCmd.Flags().Bool("allow-empty-description", false, "Allow empty description replacement when reading from stdin or file")
	updateCmd.Flags().String("spec-id", "", "Link to specification document")
	updateCmd.Flags().String("acceptance-criteria", "", "DEPRECATED: use --acceptance")
	_ = updateCmd.Flags().MarkHidden("acceptance-criteria") // Only fails if flag missing (caught in tests)
	updateCmd.Flags().IntP("estimate", "e", 0, "Time estimate in minutes (e.g., 60 for 1 hour)")
	updateCmd.Flags().StringSlice("add-label", nil, "Add labels (repeatable)")
	updateCmd.Flags().StringSlice("remove-label", nil, "Remove labels (repeatable)")
	updateCmd.Flags().StringSlice("set-labels", nil, "Set labels, replacing all existing (repeatable)")
	updateCmd.Flags().String("parent", "", "New parent issue ID (reparents the issue, use empty string to remove parent)")
	updateCmd.Flags().Bool("claim", false, "Atomically claim the issue (sets assignee to you, status to in_progress; idempotent if already claimed by you; issues assigned to a pool alias listed in the claim.pools config are claimable too)")
	// Overrides the live-claim reassign fence (bd-98s5c) and close policy.
	updateCmd.Flags().Bool("force", false, "Override two refusals: let -a/--assignee overwrite another actor's live in_progress claim (use only for abandoned claims — crashed agent, expired lease; prefer bd reclaim), and let -s/--status move the issue into closed (or a configured done status) despite open children or a live blocker (same as bd close --force)")
	// Conditional (compare-and-set) update guards (bd-wsqvw)
	updateCmd.Flags().String("if-assignee", "", "Apply the update only if the current assignee equals this value (--if-assignee '' requires unassigned); a mismatch writes nothing and exits 13 (vs 1 for other failures). Requires a field update; cannot combine with --claim")
	updateCmd.Flags().String("if-status", "", "Apply the update only if the current status equals this value; a mismatch writes nothing and exits 13 (vs 1 for other failures). Requires a field update; cannot combine with --claim")
	// --force (unconditional bypass of the reassign fence) and --if-assignee
	// (write only while a specific assignee still holds it) encode
	// contradictory intent — same rationale as unclaim's pairing. Rejecting the
	// combination stops a script that habitually passes --force from silently
	// dropping its --if-assignee guard.
	updateCmd.MarkFlagsMutuallyExclusive("force", "if-assignee")
	updateCmd.Flags().String("session", "", "Claude Code session ID for status=closed (or set CLAUDE_SESSION_ID env var)")
	// Time-based scheduling flags (GH#820)
	// Examples:
	//   --due=+6h           Due in 6 hours
	//   --due=tomorrow      Due tomorrow
	//   --due="next monday" Due next Monday
	//   --due=2025-01-15    Due on specific date
	//   --due=""            Clear due date
	//   --defer=+1h         Hidden from bd ready for 1 hour
	//   --defer=""          Clear defer (show in bd ready immediately)
	updateCmd.Flags().String("due", "", "Due date/time (empty to clear). Formats: +6h, +1d, +2w, tomorrow, next monday, 2025-01-15")
	updateCmd.Flags().String("defer", "", "Defer until date (empty to clear). Issue hidden from bd ready until then, then auto-wakes to open")
	// Gate fields (bd-z6kw)
	updateCmd.Flags().String("await-id", "", "Set gate await_id (e.g., GitHub run ID for gh:run gates)")
	// Ephemeral/persistent flags
	updateCmd.Flags().Bool("ephemeral", false, "Mark issue as ephemeral (wisp) - not exported to JSONL")
	updateCmd.Flags().Bool("persistent", false, "Mark issue as persistent (promote wisp to regular issue)")
	updateCmd.Flags().Bool("no-history", false, "Mark issue as no-history (skip Dolt commits, not GC-eligible)")
	updateCmd.Flags().Bool("history", false, "Clear no-history flag (re-enable Dolt commit history)")
	// Metadata flag (GH#1413)
	updateCmd.Flags().String("metadata", "", "Set custom metadata (JSON string or @file.json to read from file)")
	// Incremental metadata edits (GH#1406)
	updateCmd.Flags().StringArray("set-metadata", nil, "Set metadata key=value (repeatable, e.g., --set-metadata team=platform)")
	updateCmd.Flags().StringArray("unset-metadata", nil, "Remove metadata key (repeatable, e.g., --unset-metadata team)")
	rootCmd.AddCommand(updateCmd)
}
